package terminal

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"image/color"
	"os"
	"strings"
	"sync"

	"github.com/derekparker/delve/service"
	"github.com/derekparker/delve/service/api"
	"github.com/derekparker/delve/terminal/textwnd"
	"golang.org/x/mobile/event/key"
)

type updatefn func() ([]string, int)

type printCmd struct {
	scope api.EvalScope
	expr  string
}

type window struct {
	sw       *textwnd.Wnd
	fn       updatefn
	printCmd []printCmd
}

var fontSize int
var windowsMutex sync.Mutex
var windows []*window
var defaultTheme = textwnd.Theme{
	Fg: c(0x33E5F6), Bg: c(0x270E0D),
	ScrollColor: c(0xDC160C),
	HlFg:        c(0x32FA4B),
	SearchFg:    c(0x33E5F6), SearchBg: c(0x097B86),
}

func c(co uint32) color.Color {
	return color.RGBA{uint8(co >> 16), uint8(co >> 8 & 0xff), uint8(co & 0xff), 0xff}
}

func (c *Commands) windowCommand(t *Term, ctx callContext, argstr string) error {
	ctx.Prefix = ctx.Prefix | windowPrefix
	args := strings.SplitN(argstr, " ", 2)
	var cmd, rest string
	switch len(args) {
	case 0:
		return errors.New("not enough arguments")
	case 1:
		cmd = args[0]
	case 2:
		cmd = args[0]
		rest = args[1]
	}

	return c.CallWithContext(cmd, rest, t, ctx)
}

func callContextWithoutWindowPrefix(ctx callContext) (r callContext) {
	r = ctx
	r.Prefix = r.Prefix & ^windowPrefix
	return
}

func termForWindow(t *Term) (*Term, *bytes.Buffer) {
	buf := bytes.NewBuffer(make([]byte, 0))
	r := &Term{client: t.client, stdout: buf}
	return r, buf
}

const normalHelp = `ALL WINDOWS:
    ?                 This help screen
    Ctrl-F, Ctrl-G    Interactive Search
    arrow keys        Scroll
    Q                 Close window
`

const listHelp = `ALL WINDOWS:
    ?                 This help screen
    Ctrl-F, Ctrl-G    Interactive Search
    arrow keys        Scroll
    Q                 Close window
    escape            Refresh window, close help

THIS WINDOW
    n                 Next
    s                 Step
    o                 Step-Out
    c                 Continue
`

func createWindow(fn updatefn, listwnd bool) *window {
	lines, pos := fn()
	if pos < 0 {
		pos = 0
	}
	defaultTheme.FontSize = fontSize
	sw := textwnd.NewWindow(defaultTheme, lines, pos)

	w := &window{sw, fn, nil}
	i := addWindow(w)

	go func() {
		events := sw.Events()
		for ei := range events {
			e, ok := ei.(key.Event)
			if !ok {
				continue
			}
			if e.Direction != key.DirPress {
				continue
			}
			if e.Rune == '?' {
				helpstr := normalHelp
				if listwnd {
					helpstr = listHelp
				}
				w.sw.Redraw(strings.Split(helpstr, "\n"), 0, false)
				continue
			}
			if e.Modifiers != 0 {
				continue
			}
			switch e.Code {
			case key.CodeEscape:
				w.update()
			}
			if !listwnd {
				continue
			}
			switch e.Code {
			case key.CodeN:
				//TODO: next
			case key.CodeS:
				//TODO: step
			case key.CodeO:
				//TODO: stepout
			case key.CodeC:
				//TODO: continue
			}
		}
		removeWindow(i)
	}()

	return w
}

func addWindow(w *window) int {
	windowsMutex.Lock()
	defer windowsMutex.Unlock()
	for i := range windows {
		if windows[i] == nil {
			windows[i] = w
			return i
		}
	}
	windows = append(windows, w)
	return len(windows) - 1
}

func removeWindow(idx int) {
	windowsMutex.Lock()
	defer windowsMutex.Unlock()
	windows[idx] = nil
}

func (w *window) update() {
	lines, pos := w.fn()
	w.sw.Redraw(lines, pos, false)
}

func errorOutput(err error) ([]string, int) {
	return []string{"error: " + err.Error()}, -1
}

func createWindowForCall(fn cmdfunc, t *Term, ctx callContext, argstr string) {
	createWindow(func() ([]string, int) {
		wt, buf := termForWindow(t)
		err := fn(wt, callContextWithoutWindowPrefix(ctx), argstr)
		if err != nil {
			return errorOutput(err)
		}
		return strings.Split(buf.String(), "\n"), -1
	}, false)
}

func updateWindows() {
	windowsMutex.Lock()
	defer windowsMutex.Unlock()
	for i := range windows {
		if windows[i] != nil {
			windows[i].update()
		}
	}
}

func makePrintWndUpdatefn(client service.Client, printCmd []printCmd) updatefn {
	return func() ([]string, int) {
		var buf bytes.Buffer
		for _, pcmd := range printCmd {
			if pcmd.scope.GoroutineID != -1 {
				fmt.Fprintf(&buf, "goroutine %d ", pcmd.scope.GoroutineID)
			}
			if pcmd.scope.Frame != 0 {
				fmt.Fprintf(&buf, "frame %d ", pcmd.scope.Frame)
			}
			fmt.Fprintf(&buf, "%s = ", pcmd.expr)
			val, err := client.EvalVariable(pcmd.scope, pcmd.expr, LongLoadConfig)
			if err != nil {
				fmt.Fprintf(&buf, "error: %v\n", err)
			} else {
				fmt.Fprintln(&buf, val.MultilineString(""))
			}
		}
		return strings.Split("\n", buf.String()), -1
	}
}

func addToPrintWindow(client service.Client, scope api.EvalScope, args string) {
	for i := range windows {
		if windows[i].printCmd != nil {
			windows[i].printCmd = append(windows[i].printCmd, printCmd{scope: scope, expr: args})
			windows[i].fn = makePrintWndUpdatefn(client, windows[i].printCmd)
			windows[i].update()
			return
		}
	}

	printCmd := []printCmd{{scope: scope, expr: args}}

	w := createWindow(makePrintWndUpdatefn(client, printCmd), false)
	w.printCmd = printCmd
}

func createListWindow(client service.Client, ctx callContext, args string) {
	ctx = callContextWithoutWindowPrefix(ctx)
	createWindow(func() ([]string, int) {

		file, line, _, showArrow, err := listCommandToLocation(client, ctx, args)
		if err != nil {
			return errorOutput(err)
		}

		breakpoints, err := client.ListBreakpoints()
		if err != nil {
			return errorOutput(err)
		}
		bpmap := map[int]*api.Breakpoint{}
		for _, bp := range breakpoints {
			if bp.File == file {
				bpmap[bp.Line] = bp
			}
		}

		fh, err := os.Open(file)
		if err != nil {
			return errorOutput(err)
		}
		defer fh.Close()

		buf := bufio.NewScanner(fh)
		lines := []string{}
		for buf.Scan() {
			lines = append(lines, buf.Text())
		}

		d := digits(len(lines))
		if d < 3 {
			d = 3
		}

		for i := range lines {
			prefix := ""
			if showArrow {
				prefix = "  "
				if i+1 == line {
					prefix = "=>"
				}
			}
			breakpoint := " "
			if _, ok := bpmap[i+1]; ok {
				breakpoint = "*"
			}
			lines[i] = fmt.Sprintf("%s %s %*d\t%s", prefix, breakpoint, d, i+1, lines[i])
		}

		if err := buf.Err(); err != nil {
			return errorOutput(err)
		}

		return lines, line
	}, true)
}

func createDisassWindow(client service.Client, scope api.EvalScope, args string) {
	createWindow(func() ([]string, int) {
		disass, err := disassCommandToInstr(client, scope, args)
		if err != nil {
			return errorOutput(err)
		}
		var buf bytes.Buffer
		DisasmPrint(disass, &buf)
		lines := strings.Split(buf.String(), "\n")
		pcinst := -1
		for i, inst := range disass {
			if inst.AtPC {
				pcinst = i
				break
			}
		}

		return lines, pcinst
	}, true)
}
