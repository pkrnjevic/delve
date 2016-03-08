package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/derekparker/delve/terminal"
	"github.com/gorilla/websocket"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

//go:generate go-bindata -o assets.go static

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

type HandleFunc func(w http.ResponseWriter, r *http.Request)

func handlerWrapper(hf HandleFunc) HandleFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rerr := recover(); rerr != nil {
				WriteStackTrace(rerr, os.Stderr)
				w.WriteHeader(500)
				fmt.Fprintf(w, "Internal Server Error %v", rerr)
			}
		}()

		from, err := hostPortToIP(r.RemoteAddr, nil)
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(w, "Internal server error: %v", err)
			return
		}
		if !from.IP.IsLoopback() {
			w.WriteHeader(403)
			fmt.Fprintf(w, "Forbidden")
			return
		}

		hf(w, r)
	}
}

func hostPortToIP(hostport string, ctx *net.TCPAddr) (hostaddr *net.TCPAddr, err error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	iport, err := strconv.Atoi(port)
	if err != nil || iport < 0 || iport > 0xFFFF {
		return nil, fmt.Errorf("invalid port %d", iport)
	}
	var addr net.IP
	if ctx != nil && host == "localhost" {
		if ctx.IP.To4() != nil {
			addr = net.IPv4(127, 0, 0, 1)
		} else {
			addr = net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
		}
	} else if addr = net.ParseIP(host); addr == nil {
		return nil, fmt.Errorf("could not parse IP %s", host)
	}

	return &net.TCPAddr{IP: addr, Port: iport}, nil
}

func WriteStackTrace(rerr interface{}, out io.Writer) {
	fmt.Fprintf(out, "Stack trace for: %s\n", rerr)
	for i := 1; ; i++ {
		_, file, line, ok := runtime.Caller(i)
		if !ok {
			break
		}
		fmt.Fprintf(out, "    %s:%d\n", file, line)
	}
}

var validNameRe = regexp.MustCompile(`^[-a-zA-Z0-9_.]+$`)

func staticHandler(w http.ResponseWriter, r *http.Request) {
	vz := strings.Split(r.URL.Path, "/")
	if len(vz) < 2 {
		fmt.Fprintf(os.Stderr, "Can't decode path: %s\n", r.URL.Path)
		return
	}

	name := vz[len(vz)-1]

	if !validNameRe.MatchString(name) {
		fmt.Fprintf(os.Stderr, "Invalid name: %s\n", r.URL.Path)
		return
	}

	h := w.Header()
	if strings.HasSuffix(name, ".js") {
		h.Add("Content-Type", "application/javascript")
	} else if strings.HasSuffix(name, ".css") {
		h.Add("Content-Type", "text/css")
	}

	b, err := Asset("static/" + name)
	if err == nil {
		w.WriteHeader(http.StatusOK)
		w.Write(b)
	} else {
		w.WriteHeader(404)
		fmt.Fprintf(os.Stderr, "Couldn't find static file %s: %v\n", r.URL.Path, err)
	}
}

type eachWriter struct {
	conn *websocket.Conn
}

type webSocketMsg struct {
	List *webSocketList
	Out  string
	Quit bool
}

type webSocketList struct {
	Filename  string
	Line      int
	ShowArrow bool
}

func (w *eachWriter) Write(out []byte) (int, error) {
	uw, err := w.conn.NextWriter(websocket.TextMessage)
	if err != nil {
		return 0, err
	}
	defer uw.Close()
	return len(out), json.NewEncoder(uw).Encode(webSocketMsg{Out: string(out)})
}

func cmdHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	must(err)
	defer conn.Close()
	Term.Out = &eachWriter{conn}
	Term.PrintFile = func(filename string, line int, showArrow bool) error {
		uw, err := conn.NextWriter(websocket.TextMessage)
		if err != nil {
			return err
		}
		defer uw.Close()
		return json.NewEncoder(uw).Encode(webSocketMsg{List: &webSocketList{Filename: filename, Line: line, ShowArrow: showArrow}})
	}

	err = Term.Call(r.URL.Query().Get("cmd"))

	if _, exitreq := err.(terminal.ExitRequestError); exitreq {
		s, err := Client.GetState()
		if err == nil {
			if !s.Exited {
				err = Client.Detach(false)
				if err != nil {
					return
				}
			}
		}
		uw, err := conn.NextWriter(websocket.TextMessage)
		json.NewEncoder(uw).Encode(webSocketMsg{Quit: true, Out: "done\n"})
		uw.Close()
		Listener.Close()
		return
	}

	if err != nil {
		fmt.Fprintf(Term.Out, "%v", err)
	}
}

var listTemplate = template.Must(template.New("").Parse(`<div id='L{{.LineNo}}' class='line{{if .Current}} selectedline{{end}}'>
	<div class='linebp' onclick='togglebreakpoint("{{.Filename}}", {{.LineNo}}, "{{.Line}}", event)'></div>
	<div class='lineno'>{{.LineNo}}</div>
	<div class='linearr'>{{if .Arrow}}=>{{end}}</div>
	<div class='lineline'>{{.Line}}</div>
</div>`))

type listMsg struct {
	Out         string
	Breakpoints []breakpoint
}

func listHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filename := q.Get("filename")
	line, _ := strconv.Atoi(q.Get("line"))
	showArrow := q.Get("showArrow") == "true"

	_, _ = line, showArrow

	file, err := os.Open(filename)
	if err != nil {
		must(json.NewEncoder(w).Encode(listMsg{Out: err.Error()}))
		return
	}
	defer file.Close()

	var buf bytes.Buffer

	scan := bufio.NewScanner(file)
	ln := 0
	for scan.Scan() {
		ln++
		must(listTemplate.Execute(&buf, struct {
			Filename       string
			LineNo         int
			Arrow, Current bool
			Line           string
		}{Filename: filename, LineNo: ln, Arrow: showArrow && (ln == line), Current: ln == line, Line: scan.Text()}))
	}

	updateBreakpoints()
	breakpoints := make([]breakpoint, 0, len(savedBreakpoints))
	for _, bp := range savedBreakpoints {
		if bp.Filename == filename {
			breakpoints = append(breakpoints, *bp)
		}
	}

	must(json.NewEncoder(w).Encode(listMsg{Out: buf.String(), Breakpoints: breakpoints}))
}

func interruptHandler(w http.ResponseWriter, r *http.Request) {
	Client.Halt()
}
