package terminal

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/derekparker/delve/service"
	"github.com/derekparker/delve/service/api"
)

type persistentBp struct {
	Name         string
	Function     string
	Filename     string
	Line         int
	LineContents string

	ID      int
	Enabled bool

	Tracepoint bool
	Commands   string
}

type persistentBps []*persistentBp

var breakpointFilePath = ""
var persistentBreakpoints persistentBps

func findBreakpointFileLocation(c service.Client) {
	locs, err := c.FindLocation(api.EvalScope{-1, 0}, "main.main")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not find main.main: %v\n", err)
		return
	}

	p := fmt.Sprintf("%s_%s_%032x", filepath.Base(locs[0].File), filepath.Base(filepath.Dir(locs[0].File)), md5.Sum([]byte(locs[0].File)))

	var d string
	switch runtime.GOOS {
	case "windows":
		d = os.Getenv("HOME")
	case "darwin":
		d = filepath.Join(os.Getenv("HOME"), "Library")
	default:
		d = filepath.Join(os.Getenv("HOME"), ".config")
	}
	d = filepath.Join(d, "vdlv")
	os.Mkdir(d, 0770)
	breakpointFilePath = filepath.Join(d, p)
}

func restoreBreakpoints(t *Term) {
	if breakpointFilePath == "" {
		return
	}
	fh, err := os.Open(breakpointFilePath)
	if err != nil {
		return
	}
	defer fh.Close()
	json.NewDecoder(fh).Decode(&persistentBreakpoints)
	count := 0

	for i, sbp := range persistentBreakpoints {
		if sbp == nil {
			continue
		}
		if sbp.adjust(t.client) {
			if sbp.Enabled {
				sbp.Enabled = false
				sbp.Enable(t)
			}
			count++
		} else {
			persistentBreakpoints[i] = nil
		}
	}
	updateBreakpoints(t.client)

	if len(persistentBreakpoints) > 0 {
		fmt.Printf("Restored %d of %d saved breakpoints\n", count, len(persistentBreakpoints))
	}
}

func saveBreakpoints() {
	if breakpointFilePath == "" {
		return
	}
	fh, err := os.Create(breakpointFilePath)
	if err != nil {
		return
	}
	defer fh.Close()
	json.NewEncoder(fh).Encode(persistentBreakpoints)
}

func updateBreakpoints(c service.Client) {
	for _, sbp := range persistentBreakpoints {
		if sbp == nil {
			continue
		}
		sbp.ID = 0
		sbp.Enabled = false
	}
	bps, err := c.ListBreakpoints()
	if err != nil {
		panic(err)
	}
	for _, bp := range bps {
		if bp.ID < 0 {
			continue
		}
		i := persistentBreakpoints.Index(bp.File, bp.Line)
		var sbp *persistentBp
		if i < 0 {
			sbp = &persistentBp{}
			sbp.LineContents = getLineContents(bp.File, bp.Line)
			found := false
			for i := range persistentBreakpoints {
				if persistentBreakpoints[i] == nil {
					persistentBreakpoints[i] = sbp
					found = true
					break
				}
			}
			if !found {
				persistentBreakpoints = append(persistentBreakpoints, sbp)
			}
		} else {
			sbp = persistentBreakpoints[i]
		}
		sbp.ID = bp.ID
		sbp.Name = bp.Name
		sbp.Function = bp.FunctionName
		sbp.Filename = bp.File
		sbp.Line = bp.Line

		var buf bytes.Buffer

		if bp.Tracepoint {
			sbp.Tracepoint = true
		}

		if bp.Cond != "" {
			fmt.Fprintf(&buf, "\tcond %s\n", bp.Cond)
		}

		if bp.Goroutine {
			fmt.Fprintf(&buf, "\tgoroutine\n")
		}

		if bp.Stacktrace > 0 {
			fmt.Fprintf(&buf, "\tstack %d\n", bp.Stacktrace)
		}

		if bp.LoadArgs != nil {
			if *bp.LoadArgs == ShortLoadConfig {
				fmt.Fprintf(&buf, "\targs\n")
			} else {
				fmt.Fprintf(&buf, "\targs -v\n")
			}
		}

		if bp.LoadLocals != nil {
			if *bp.LoadLocals == ShortLoadConfig {
				fmt.Fprintf(&buf, "\tlocals\n")
			} else {
				fmt.Fprintf(&buf, "\tlocals -v\n")
			}
		}

		for i := range bp.Variables {
			fmt.Fprintf(&buf, "\tprint %v\n", bp.Variables[i])
		}

		sbp.Commands = buf.String()
		sbp.Enabled = true
	}
}

func getLineContents(filename string, lineno int) string {
	fh, err := os.Open(filename)
	if err != nil {
		return ""
	}
	defer fh.Close()

	s := bufio.NewScanner(fh)
	curln := 0
	for s.Scan() {
		curln++
		if curln == lineno {
			return s.Text()
		}
	}
	return ""
}

func (pbps persistentBps) Index(filename string, lineno int) int {
	for i, bp := range pbps {
		if bp == nil {
			continue
		}
		if bp.Filename == filename && bp.Line == lineno {
			return i
		}
	}
	return -1
}

func (sbp *persistentBp) adjust(c service.Client) bool {
	locs, err := c.FindLocation(api.EvalScope{-1, 0}, sbp.Function)
	if err != nil {
		fmt.Fprintf(os.Stderr, "removing: could not find function %s: %v\n", sbp.Function, err)
		return false
	}
	if len(locs) != 1 {
		fmt.Fprintf(os.Stderr, "removing: could not find function %s: %v\n", sbp.Function, err)
		return false
	}

	disass, err := c.DisassemblePC(api.EvalScope{-1, 0}, locs[0].PC, api.IntelFlavour)
	if err != nil {
		fmt.Fprintf(os.Stderr, "removing: could not disassemble function %s: %v\n", sbp.Function, err)
		return false
	}
	lines := []int{}

	for _, instr := range disass {
		if instr.Loc.File != sbp.Filename {
			continue
		}
		if len(lines) == 0 {
			lines = append(lines, instr.Loc.Line)
		} else {
			if lines[len(lines)-1] != instr.Loc.Line {
				lines = append(lines, instr.Loc.Line)
			}
		}
	}

	if len(lines) == 0 {
		fmt.Fprintf(os.Stderr, "removing: function %s empty or in a different file.\n")
		return false
	}

	sort.Ints(lines)

	fh, err := os.Open(sbp.Filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "removing %s:%d could not open source: %v\n", sbp.Filename, sbp.Line, err)
		return false
	}
	defer fh.Close()
	scan := bufio.NewScanner(fh)
	fileLines := []string{""}
	for scan.Scan() {
		fileLines = append(fileLines, scan.Text())
	}

	if sbp.Line < len(fileLines) && fileLines[sbp.Line] == sbp.LineContents {
		return true
	}

	for i := range lines {
		if fileLines[lines[i]] == sbp.LineContents {
			fmt.Fprintf(os.Stderr, "adjusted position of %s:%d to %d\n", sbp.Filename, sbp.Line, lines[i])
			sbp.Line = lines[i]
			return true
		}
	}

	fmt.Fprintf(os.Stderr, "removed: %s:%d could not find position\n", sbp.Filename, sbp.Line)
	return false
}

func (sbp *persistentBp) Enable(t *Term) {
	if !sbp.Enabled {
		bp, err := t.client.CreateBreakpoint(sbp.ApiBreakpoint(t))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Could not create breakpoint at %s:%d: %v", sbp.Filename, sbp.Line, err)
		}
		sbp.ID = bp.ID
		sbp.Enabled = true
	} else {
		err := t.client.AmendBreakpoint(sbp.ApiBreakpoint(t))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Could not amend breakpoint at %s:%d: %v", sbp.Filename, sbp.Line, err)
		}
	}
}

func (sbp *persistentBp) Disable(t *Term) {
	if sbp.Enabled {
		_, err := t.client.ClearBreakpoint(sbp.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Could not disable breakpoint: %v\n", err)
		} else {
			sbp.ID = 0
			sbp.Enabled = false
		}
	}
}

func (sbp *persistentBp) ApiBreakpoint(t *Term) *api.Breakpoint {
	scan := bufio.NewScanner(bytes.NewReader([]byte(sbp.Commands)))
	bp := api.Breakpoint{ID: sbp.ID, Name: sbp.Name, File: sbp.Filename, Line: sbp.Line, Tracepoint: sbp.Tracepoint}
	ctx := callContext{onPrefix, api.EvalScope{-1, 0}, &bp}
	for scan.Scan() {
		cmd := strings.TrimSpace(scan.Text())
		v := strings.SplitN(cmd, " ", 2)
		var args string
		if len(v) != 1 {
			args = v[1]
		}
		t.cmds.CallWithContext(v[0], args, t, ctx)
	}
	return &bp
}

func editBreakpoints() ([]*persistentBp, error) {
	edcmd := os.Getenv("EDITOR")
	if edcmd == "" {
		return nil, errors.New("environment variable $EDITOR not set")
	}

	f, err := ioutil.TempFile("", "bpedit")
	if err != nil {
		return nil, err
	}
	fpath := f.Name()
	func() { // serialize for edit
		defer f.Close()
		w := bufio.NewWriter(f)
		defer w.Flush()
		for _, sbp := range persistentBreakpoints {
			if sbp == nil {
				continue
			}
			if !sbp.Enabled {
				io.WriteString(w, "disabled ")
			}
			if sbp.Tracepoint {
				io.WriteString(w, "trace ")
			} else {
				io.WriteString(w, "break ")
			}
			fmt.Fprintf(w, "%s %s:%d\n\tin function %s\n%s\n", sbp.Name, sbp.Filename, sbp.Line, sbp.Function, sbp.Commands)
		}
	}()

	// call editor
	err = exec.Command(edcmd, fpath).Run()
	if err != nil {
		return nil, err
	}

	fh, err := os.Open(fpath)
	if err != nil {
		return nil, fmt.Errorf("could not read temp file: %v", err)
	}
	defer fh.Close()
	s := bufio.NewScanner(fh)

	r := make(persistentBps, 0, len(persistentBreakpoints)+1)
	var bp *persistentBp = nil
	commands := []string{}

	flush := func() {
		if bp != nil {
			bp.Commands = strings.Join(commands, "\n")
			commands = []string{}
			r = append(r, bp)
		}
	}

	lineno := 0
	for s.Scan() {
		line := s.Text()
		lineno++
		if len(line) <= 0 {
			continue
		}

		if line[0] == '\t' {
			if bp == nil {
				return nil, fmt.Errorf("malformed line %d: no header", lineno)
			}
			if !strings.HasPrefix(line, "\tin function") {
				commands = append(commands, line)
			}
		} else {
			flush()
			bp = &persistentBp{}
			v := strings.SplitN(line, " ", 2)
			if len(v) != 2 {
				return nil, fmt.Errorf("malformed line %d: bad header, expected 'disabled', 'break' or 'trace' followed by position", lineno)
			}
			bp.Enabled = v[0] != "disabled"
			if !bp.Enabled {
				v = strings.SplitN(v[1], " ", 2)
				if len(v) != 2 {
					return nil, fmt.Errorf("malformed line %d: bad header, expected 'break' or 'trace' followed by position", lineno)
				}
			}
			switch v[0] {
			case "break":
				bp.Tracepoint = false
			case "trace":
				bp.Tracepoint = true
			default:
				return nil, fmt.Errorf("malformed line %d: bad header, expected 'break' or 'trace' followed by position", lineno)
			}
			if v[1][0] != '/' {
				v = strings.SplitN(v[1], " ", 2)
				if len(v) != 2 {
					return nil, fmt.Errorf("malformed line %d: breakpoint name should be followed by position information", lineno)
				}
				bp.Name = v[0]
			}
			if v[1][0] != '/' {
				return nil, fmt.Errorf("malformed line %d: breakpoint name should be followed by position information", lineno)
			}
			v = strings.SplitN(v[1], ":", 2)
			if len(v) != 2 {
				return nil, fmt.Errorf("malformed line %d: malformed position", lineno)
			}
			bp.Filename = v[0]
			n, _ := strconv.Atoi(v[1])
			bp.Line = n
		}
	}

	flush()

	return r, nil
}
