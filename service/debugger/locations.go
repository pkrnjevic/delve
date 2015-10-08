package debugger

import (
	"debug/gosym"
	"fmt"
	"go/constant"
	"path/filepath"
	"reflect"
	"runtime"
	"io/ioutil"
	"regexp"
	"strconv"
	"strings"

	"github.com/derekparker/delve/proc"
	"github.com/derekparker/delve/service/api"
)

const maxFindLocationCandidates = 5

type LocationSpec interface {
	Find(d *Debugger, scope *proc.EvalScope, locStr string) ([]api.Location, error)
}

type NormalLocationSpec struct {
	Base       string
	FuncBase   *FuncLocationSpec
	LineOffset LineOffsetExprs
}

type RegexLocationSpec struct {
	FuncRegex string
}

type AddrLocationSpec struct {
	AddrExpr string
}

type OffsetLocationSpec struct {
	Offset int
}

type LineLocationSpec struct {
	Line LineOffsetExprs
}

type FuncLocationSpec struct {
	PackageName           string
	AbsolutePackage       bool
	ReceiverName          string
	PackageOrReceiverName string
	BaseName              string
}

type LineOffsetOp int

const (
	LOO_PLUS  = LineOffsetOp(+1)
	LOO_MINUS = LineOffsetOp(-1)
)

type LineOffsetExpr struct {
	Op LineOffsetOp
	Rx *regexp.Regexp
	N  int
}
type LineOffsetExprs []LineOffsetExpr

type LineOffsetExprErr struct {
	Offset int
	Reason string
}

func (err LineOffsetExprErr) Error() string {
	return fmt.Sprintf("%d: %s", err.Offset, err.Reason)
}

func parseLocationSpec(locStr string) (LocationSpec, error) {
	rest := locStr

	malformed := func(reason string) error {
		return fmt.Errorf("Malformed breakpoint location \"%s\" at %d: %s", locStr, len(locStr)-len(rest), reason)
	}

	if len(rest) <= 0 {
		return nil, malformed("empty string")
	}

	switch rest[0] {
	case '+', '-':
		offset, err := strconv.Atoi(rest)
		if err != nil {
			return nil, malformed(err.Error())
		}
		return &OffsetLocationSpec{offset}, nil

	case '/':
		if rest[len(rest)-1] == '/' {
			rx, rest := readRegex(rest[1:])
			if len(rest) < 0 {
				return nil, malformed("non-terminated regular expression")
			}
			if len(rest) > 1 {
				return nil, malformed("no line offset can be specified for regular expression locations")
			}
			return &RegexLocationSpec{rx}, nil
		} else {
			return parseLocationSpecDefault(locStr, rest)
		}

	case '*':
		return &AddrLocationSpec{rest[1:]}, nil

	default:
		return parseLocationSpecDefault(locStr, rest)
	}
}

func parseLocationSpecDefault(locStr, rest string) (LocationSpec, error) {
	v := strings.Split(rest, ":")
	if len(v) > 2 {
		// On Windows, path may contain ":", so split only on last ":"
		v = []string{strings.Join(v[0:len(v)-1], ":"), v[len(v)-1]}
	}

	if len(v) == 1 {
		n, err := strconv.ParseInt(v[0], 0, 64)
		if err == nil {
			return &LineLocationSpec{[]LineOffsetExpr{{Op: LOO_PLUS, N: int(n)}}}, nil
		}
	}

	spec := &NormalLocationSpec{}

	spec.Base = v[0]
	spec.FuncBase = parseFuncLocationSpec(spec.Base)

	if len(v) < 2 {
		spec.LineOffset = nil
		return spec, nil
	}

	rest = v[1]

	var err error
	spec.LineOffset, err = parseLineOffsetExpr(rest)
	if err != nil {
		e := err.(LineOffsetExprErr)
		return nil, fmt.Errorf("Malformed breakpoint location \"%s\" at %d: %s", locStr, len(locStr)-(len(rest)-e.Offset), e.Reason)
	}

	return spec, nil
}

func readRegex(in string) (rx string, rest string) {
	out := make([]rune, 0, len(in))
	escaped := false
	for i, ch := range in {
		if escaped {
			if ch == '/' {
				out = append(out, '/')
			} else {
				out = append(out, '\\')
				out = append(out, ch)
			}
			escaped = false
		} else {
			switch ch {
			case '\\':
				escaped = true
			case '/':
				return string(out), in[i:]
			default:
				out = append(out, ch)
			}
		}
	}
	return string(out), ""
}

func readInt(in string) (n int, rest string) {
	for i, ch := range in {
		if ch < '0' || ch > '9' {
			r, _ := strconv.Atoi(in[:i])
			return int(r), in[i:]
		}
	}
	r, _ := strconv.Atoi(in)
	return int(r), ""
}

func parseFuncLocationSpec(in string) *FuncLocationSpec {
	v := strings.Split(in, ".")
	var spec FuncLocationSpec
	switch len(v) {
	case 1:
		spec.BaseName = v[0]

	case 2:
		spec.BaseName = v[1]
		r := stripReceiverDecoration(v[0])
		if r != v[0] {
			spec.ReceiverName = r
		} else if strings.Index(r, "/") >= 0 {
			spec.PackageName = r
		} else {
			spec.PackageOrReceiverName = r
		}

	case 3:
		spec.BaseName = v[2]
		spec.ReceiverName = stripReceiverDecoration(v[1])
		spec.PackageName = v[0]

	default:
		return nil
	}

	if strings.HasPrefix(spec.PackageName, "/") {
		spec.PackageName = spec.PackageName[1:]
		spec.AbsolutePackage = true
	}

	if strings.Index(spec.BaseName, "/") >= 0 || strings.Index(spec.ReceiverName, "/") >= 0 {
		return nil
	}

	return &spec
}

func stripReceiverDecoration(in string) string {
	if len(in) < 3 {
		return in
	}
	if (in[0] != '(') || (in[1] != '*') || (in[len(in)-1] != ')') {
		return in
	}

	return in[2 : len(in)-1]
}

func (spec *FuncLocationSpec) Match(sym *gosym.Sym) bool {
	if spec.BaseName != sym.BaseName() {
		return false
	}

	recv := stripReceiverDecoration(sym.ReceiverName())
	if spec.ReceiverName != "" && spec.ReceiverName != recv {
		return false
	}
	if spec.PackageName != "" {
		if spec.AbsolutePackage {
			if spec.PackageName != sym.PackageName() {
				return false
			}
		} else {
			if !partialPathMatch(spec.PackageName, sym.PackageName()) {
				return false
			}
		}
	}
	if spec.PackageOrReceiverName != "" && !partialPathMatch(spec.PackageOrReceiverName, sym.PackageName()) && spec.PackageOrReceiverName != recv {
		return false
	}
	return true
}

func (loc *RegexLocationSpec) Find(d *Debugger, scope *proc.EvalScope, locStr string) ([]api.Location, error) {
	funcs := d.process.Funcs()
	matches, err := regexFilterFuncs(loc.FuncRegex, funcs)
	if err != nil {
		return nil, err
	}
	r := make([]api.Location, 0, len(matches))
	for i := range matches {
		addr, err := d.process.FindFunctionLocation(matches[i], true, 0)
		if err == nil {
			r = append(r, api.Location{PC: addr})
		}
	}
	return r, nil
}

func (loc *AddrLocationSpec) Find(d *Debugger, scope *proc.EvalScope, locStr string) ([]api.Location, error) {
	if scope == nil {
		addr, err := strconv.ParseInt(loc.AddrExpr, 0, 64)
		if err != nil {
			return nil, fmt.Errorf("could not determine current location (scope is nil)")
		}
		return []api.Location{{PC: uint64(addr)}}, nil
	} else {
		v, err := scope.EvalExpression(loc.AddrExpr, proc.LoadConfig{true, 0, 0, 0, 0})
		if err != nil {
			return nil, err
		}
		if v.Unreadable != nil {
			return nil, v.Unreadable
		}
		switch v.Kind {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			addr, _ := constant.Uint64Val(v.Value)
			return []api.Location{{PC: addr}}, nil
		case reflect.Func:
			_, _, fn := d.process.PCToLine(uint64(v.Base))
			pc, err := d.process.FirstPCAfterPrologue(fn, false)
			if err != nil {
				return nil, err
			}
			return []api.Location{{PC: uint64(pc)}}, nil
		default:
			return nil, fmt.Errorf("wrong expression kind: %v", v.Kind)
		}
	}
}

func (loc *NormalLocationSpec) FileMatch(path string) bool {
	return partialPathMatch(loc.Base, path)
}

func partialPathMatch(expr, path string) bool {
	if runtime.GOOS == "windows" {
		// Accept `expr` which is case-insensitive and slash-insensitive match to `path`
		expr = strings.ToLower(filepath.ToSlash(expr))
		path = strings.ToLower(filepath.ToSlash(path))
	}
	if len(expr) < len(path)-1 {
		return strings.HasSuffix(path, expr) && (path[len(path)-len(expr)-1] == '/')
	} else {
		return expr == path
	}
}

type AmbiguousLocationError struct {
	Location           string
	CandidatesString   []string
	CandidatesLocation []api.Location
}

func (ale AmbiguousLocationError) Error() string {
	var candidates []string
	if ale.CandidatesLocation != nil {
		for i := range ale.CandidatesLocation {
			candidates = append(candidates, ale.CandidatesLocation[i].Function.Name)
		}

	} else {
		candidates = ale.CandidatesString
	}
	return fmt.Sprintf("Location \"%s\" ambiguous: %s…", ale.Location, strings.Join(candidates, ", "))
}

func (loc *NormalLocationSpec) Find(d *Debugger, scope *proc.EvalScope, locStr string) ([]api.Location, error) {
	funcs := d.process.Funcs()
	files := d.process.Sources()

	candidates := []string{}
	for file := range files {
		if loc.FileMatch(file) {
			candidates = append(candidates, file)
			if len(candidates) >= maxFindLocationCandidates {
				break
			}
		}
	}

	if loc.FuncBase != nil {
		for _, f := range funcs {
			if f.Sym == nil {
				continue
			}
			if loc.FuncBase.Match(f.Sym) {
				candidates = append(candidates, f.Name)
				if len(candidates) >= maxFindLocationCandidates {
					break
				}
			}
		}
	}

	switch len(candidates) {
	case 1:
		var addr uint64
		var err error
		if filepath.IsAbs(candidates[0]) {
			if loc.LineOffset == nil {
				return nil, fmt.Errorf("Malformed breakpoint location, no line offset specified")
			}
			var lineOffset int
			lineOffset, err = loc.LineOffset.run(candidates[0], 0)
			if err != nil {
				return nil, err
			}
			addr, err = d.process.FindFileLocation(candidates[0], lineOffset)
		} else {
			addr, err = d.process.FindFunctionLocation(candidates[0], loc.LineOffset == nil, 0)
			if loc.LineOffset != nil && !loc.LineOffset.isZero(){
				file, start, _ := d.process.PCToLine(addr)
				var lineOffset int
				lineOffset, err = loc.LineOffset.run(file, start)
				if err != nil {
					return nil, err
				}
				addr, err = d.process.FindFileLocation(file, lineOffset)
			}
		}
		if err != nil {
			return nil, err
		}
		return []api.Location{{PC: addr}}, nil

	case 0:
		return nil, fmt.Errorf("Location \"%s\" not found", locStr)
	default:
		return nil, AmbiguousLocationError{Location: locStr, CandidatesString: candidates}
	}
}

func (loc *OffsetLocationSpec) Find(d *Debugger, scope *proc.EvalScope, locStr string) ([]api.Location, error) {
	if scope == nil {
		return nil, fmt.Errorf("could not determine current location (scope is nil)")
	}
	file, line, fn := d.process.PCToLine(scope.PC)
	if fn == nil {
		return nil, fmt.Errorf("could not determine current location")
	}
	addr, err := d.process.FindFileLocation(file, line+loc.Offset)
	return []api.Location{{PC: addr}}, err
}

func (loc *LineLocationSpec) Find(d *Debugger, scope *proc.EvalScope, locStr string) ([]api.Location, error) {
	if scope == nil {
		return nil, fmt.Errorf("could not determine current location (scope is nil)")
	}
	file, _, fn := d.process.PCToLine(scope.PC)
	if fn == nil {
		return nil, fmt.Errorf("could not determine current location")
	}
	line, err := loc.Line.run(file, 0)
	if err != nil {
		return nil, err
	}
	addr, err := d.process.FindFileLocation(file, line)
	return []api.Location{{PC: addr}}, err
}

func parseLineOffsetExpr(in string) ([]LineOffsetExpr, error) {
	s := in
	var r []LineOffsetExpr = nil
	op := LOO_PLUS

	parseRegex := func(n int) error {
		startoff := len(in) - len(s)
		var rxstr string
		rxstr, s = readRegex(s[1:])
		if len(s) <= 0 || s[0] != '/' {
			return LineOffsetExprErr{startoff, "non-terminated regular expression"}
		}
		s = s[1:]
		rx, err := regexp.Compile(rxstr)
		if err != nil {
			return LineOffsetExprErr{startoff, fmt.Sprintf("malformed regular expression: %s", err)}
		}
		r = append(r, LineOffsetExpr{Op: op, N: n, Rx: rx})
		return nil
	}

	canend := false

	for len(s) > 0 {
		if s[0] >= '0' && s[0] <= '9' {
			var n int
			n, s = readInt(s)
			if len(s) > 0 && s[0] == '/' {
				if err := parseRegex(n); err != nil {
					return nil, err
				}
			} else {
				r = append(r, LineOffsetExpr{Op: op, N: n})
			}
		} else if s[0] == '/' {
			if err := parseRegex(1); err != nil {
				return nil, err
			}
		} else {
			return nil, LineOffsetExprErr{len(in) - len(s), fmt.Sprintf("unexpected character %c (expected number or regular expression)", s[0])}
		}

		canend = true
		if len(s) > 0 {
			canend = false
			switch s[0] {
			case '+':
				op = LOO_PLUS
			case '-':
				op = LOO_MINUS
			default:
				return nil, LineOffsetExprErr{len(in) - len(s), fmt.Sprintf("unexpected character %c (expecting operator)", s[0])}
			}
			s = s[1:]
		}
	}

	if !canend {
		return nil, LineOffsetExprErr{len(in) - len(s), "unexpected end of expression"}
	}

	return r, nil
}

func (exprs *LineOffsetExprs) isZero() bool {
	return len(*exprs) == 1 && (*exprs)[0].Rx == nil && (*exprs)[0].Op == 1 && (*exprs)[0].N == 0
}

func (expr *LineOffsetExprs) run(filename string, line int) (int, error) {
	for i := range *expr {
		var err error
		line, err = (*expr)[i].run(filename, line)
		if err != nil {
			return 0, err
		}
	}
	return line, nil
}

func (expr LineOffsetExpr) run(filename string, line int) (int, error) {
	if expr.Rx == nil {
		return line + int(expr.Op)*expr.N, nil
	} else {
		buf, err := ioutil.ReadFile(filename)
		if err != nil {
			return 0, err
		}
		lines := strings.Split(string(buf), "\n")
		mcount := 0
		// Note that line numbers start at 1, but the lines array starts at 0
		for ; (line <= len(lines)) && (line >= 1); line += int(expr.Op) {
			if expr.Rx.MatchString(lines[line-1]) {
				mcount++
			}
			if mcount >= expr.N {
				return line, nil
			}
		}
		return 0, fmt.Errorf("could not find match for regular expression")
	}
}
