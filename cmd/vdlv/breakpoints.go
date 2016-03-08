package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/derekparker/delve/service/api"
	"github.com/derekparker/delve/terminal"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
)

type breakpoint struct {
	Name         string
	ID           int
	Function     string
	Filename     string
	Line         int
	LineContents string
	Config       string
	Enabled      bool
}

var savedBreakpoints = map[string]*breakpoint{}

func updateBreakpoints() {
	for _, sbp := range savedBreakpoints {
		sbp.Enabled = false
	}
	bps, err := Client.ListBreakpoints()
	must(err)
	for _, bp := range bps {
		name := bp.Name
		if name == "" {
			name = fmt.Sprintf("B%d", bp.ID)
		}
		sbp, ok := savedBreakpoints[name]
		if !ok {
			sbp = &breakpoint{Name: name}
			savedBreakpoints[name] = sbp
		}
		sbp.ID = bp.ID
		sbp.Function = bp.FunctionName
		sbp.Filename = bp.File
		sbp.Line = bp.Line

		var buf bytes.Buffer

		if bp.Tracepoint {
			fmt.Fprintf(&buf, "continue\n")
		}

		if bp.Goroutine {
			fmt.Fprintf(&buf, "goroutine\n")
		}

		if bp.Stacktrace > 0 {
			fmt.Fprintf(&buf, "stack %d\n", bp.Stacktrace)
		}

		if bp.LoadArgs != nil {
			if *bp.LoadArgs == terminal.ShortLoadConfig {
				fmt.Fprintf(&buf, "args\n")
			} else {
				fmt.Fprintf(&buf, "args -v\n")
			}
		}

		if bp.LoadLocals != nil {
			if *bp.LoadLocals == terminal.ShortLoadConfig {
				fmt.Fprintf(&buf, "locals\n")
			} else {
				fmt.Fprintf(&buf, "locals -v\n")
			}
		}

		for i := range bp.Variables {
			fmt.Fprintf(&buf, "print %v\n", bp.Variables[i])
		}

		sbp.Enabled = true
	}
}

func findSavedBreakpoint(filename string, lineno int) *breakpoint {
	for _, bp := range savedBreakpoints {
		if bp.Filename == filename && bp.Line == lineno {
			return bp
		}
	}
	return nil
}

type bpUpdateOut struct {
	Ok bool
}

func bpHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	action := q.Get("action")

	switch action {
	case "delete":
		filename := q.Get("filename")
		line, _ := strconv.Atoi(q.Get("line"))
		updateBreakpoints()
		bp := findSavedBreakpoint(filename, line)

		if bp != nil {
			Client.ClearBreakpoint(bp.ID)
			delete(savedBreakpoints, bp.Name)
			saveBreakpoints()
			json.NewEncoder(w).Encode(bp)
		}

	case "update":
		name := q.Get("name")
		config := q.Get("config")

		if savedBreakpoints[name].amendBreakpoint(config) {
			savedBreakpoints[name].Config = config
			json.NewEncoder(w).Encode(bpUpdateOut{Ok: true})
			saveBreakpoints()
		} else {
			json.NewEncoder(w).Encode(bpUpdateOut{Ok: false})
		}

	default:
		filename := q.Get("filename")
		line, _ := strconv.Atoi(q.Get("line"))
		contents := q.Get("contents")
		updateBreakpoints()
		bp := findSavedBreakpoint(filename, line)

		if bp == nil {
			Client.CreateBreakpoint(&api.Breakpoint{File: filename, Line: line})
			updateBreakpoints()
			sbp := findSavedBreakpoint(filename, line)
			if sbp != nil {
				sbp.LineContents = contents
				bp, err := Client.GetBreakpoint(sbp.ID)
				must(err)
				bp.Name = sbp.Name
				Client.AmendBreakpoint(bp)
			}
		} else {
			if bp.Enabled {
				Client.ClearBreakpoint(bp.ID)
			} else {
				Client.CreateBreakpoint(&api.Breakpoint{Name: bp.Name, File: bp.Filename, Line: bp.Line})
			}
		}
		updateBreakpoints()
		bp = findSavedBreakpoint(filename, line)
		json.NewEncoder(w).Encode(bp)
		saveBreakpoints()
	}
}

func saveBreakpoints() {
	fh, err := os.Create("vdlvrc")
	if err != nil {
		return
	}
	defer fh.Close()
	json.NewEncoder(fh).Encode(savedBreakpoints)
}

func restoreBreakpoints() {
	fmt.Fprintf(os.Stderr, "restoring breakpoints\n")
	defer fmt.Fprintf(os.Stderr, "restoring breakpoints done\n")
	fh, err := os.Open("vdlvrc")
	if err != nil {
		return
	}
	defer fh.Close()
	json.NewDecoder(fh).Decode(&savedBreakpoints)

	for _, sbp := range savedBreakpoints {
		if !sbp.Enabled {
			continue
		}

		if sbp.adjust() {
			fmt.Fprintf(os.Stderr, "setting %s at %s:%d\n", sbp.Name, sbp.Filename, sbp.Line)
			Client.CreateBreakpoint(&api.Breakpoint{Name: sbp.Name, File: sbp.Filename, Line: sbp.Line})
			sbp.amendBreakpoint(sbp.Config)
		}
	}
	updateBreakpoints()
}

func (sbp *breakpoint) adjust() bool {
	locs, err := Client.FindLocation(api.EvalScope{-1, 0}, sbp.Function)
	if err != nil {
		fmt.Fprintf(os.Stderr, "removing %s could not find function: %v\n", sbp.Name, err)
		delete(savedBreakpoints, sbp.Name)
		return false
	}
	if len(locs) != 1 {
		fmt.Fprintf(os.Stderr, "removing %s could not find function: %v\n", sbp.Name, err)
		delete(savedBreakpoints, sbp.Name)
		return false
	}

	disass, err := Client.DisassemblePC(api.EvalScope{-1, 0}, locs[0].PC, api.IntelFlavour)
	if err != nil {
		fmt.Fprintf(os.Stderr, "removing %s could not disassemble function: %v\n", sbp.Name, err)
		delete(savedBreakpoints, sbp.Name)
		return false
	}
	lines := []int{}

	for _, instr := range disass {
		if len(lines) == 0 {
			lines = append(lines, instr.Loc.Line)
		} else {
			if lines[len(lines)-1] != instr.Loc.Line {
				lines = append(lines, instr.Loc.Line)
			}
		}
		if instr.Loc.File != sbp.Filename {
			fmt.Fprintf(os.Stderr, "removing %s function not in correct file: %s %s\n", sbp.Name, instr.Loc.File, sbp.Filename)
			delete(savedBreakpoints, sbp.Name)
			return false
		}
	}

	sort.Ints(lines)

	fh, err := os.Open(sbp.Filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "removing %s could not open source: %v\n", sbp.Name, err)
		delete(savedBreakpoints, sbp.Name)
		return false
	}
	defer fh.Close()
	scan := bufio.NewScanner(fh)
	fileLines := []string{""}
	for scan.Scan() {
		fileLines = append(fileLines, scan.Text())
	}

	if fileLines[sbp.Line] == sbp.LineContents {
		return true
	}

	for i := range lines {
		if fileLines[lines[i]] == sbp.LineContents {
			fmt.Fprintf(os.Stderr, "adjusted position of %s to %s:%d\n", sbp.Name, sbp.Filename, lines[i])
			sbp.Line = lines[i]
			return true
		}
	}

	fmt.Fprintf(os.Stderr, "removed %s could not find position\n", sbp.Name)
	delete(savedBreakpoints, sbp.Name)
	return true
}

func (sbp *breakpoint) amendBreakpoint(config string) bool {
	bp := &api.Breakpoint{}
	v := strings.Split(config, "\n")
	for i := range v {
		v[i] = strings.TrimSpace(v[i])
		if v[i] == "" {
			continue
		}
		if err := Term.CallWithContext(v[i], terminal.CallContext{terminal.OnPrefix, api.EvalScope{}, bp}); err != nil {
			return false
		}
	}

	if sbp.Enabled {
		obp, err := Client.GetBreakpointByName(sbp.Name)
		must(err)
		obp.Variables = bp.Variables
		obp.Stacktrace = bp.Stacktrace
		obp.Goroutine = bp.Goroutine
		obp.LoadLocals = bp.LoadLocals
		obp.LoadArgs = bp.LoadArgs
		obp.Tracepoint = bp.Tracepoint
		if err := Client.AmendBreakpoint(obp); err != nil {
			return false
		}
	}

	return true
}
