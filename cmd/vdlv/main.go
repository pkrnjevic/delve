package main

import (
	"errors"
	"fmt"
	"github.com/derekparker/delve/config"
	"github.com/derekparker/delve/service"
	"github.com/derekparker/delve/service/rpc"
	"github.com/derekparker/delve/terminal"
	"github.com/derekparker/delve/version"
	"github.com/spf13/cobra"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

var (
	// Build is the git sha of this binaries build.
	Build string
	// Addr is the debugging server listen address.
	Addr string = "localhost:0"
	// BuildFlags is the flags passed during compiler invocation.
	BuildFlags string
	Client     service.Client
	Term       *terminal.Term
	Listener   net.Listener
	conf       *config.Config
)

const (
	debugname     = "debug"
	testdebugname = "debug.test"
)

const dlvCommandLongDesc = `Delve is a source level debugger for Go programs.

Delve enables you to interact with your program by controlling the execution of the process,
evaluating variables, and providing information of thread / goroutine state, CPU register state and more.

The goal of this tool is to provide a simple yet powerful interface for debugging Go programs.
`

func main() {
	version.DelveVersion.Build = Build

	// Config setup and load.
	conf = config.LoadConfig()
	if runtime.GOOS == "windows" {
		// Work-around for https://github.com/golang/go/issues/13154
		BuildFlags = "-ldflags=-linkmode internal"
	}

	// Main dlv root command.
	RootCommand := &cobra.Command{
		Use:   "vdlv",
		Short: "Delve is a debugger for the Go programming language.",
		Long:  dlvCommandLongDesc,
	}

	// 'version' subcommand.
	versionCommand := &cobra.Command{
		Use:   "version",
		Short: "Prints version.",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("Delve Debugger\n%s\n", version.DelveVersion)
		},
	}
	RootCommand.AddCommand(versionCommand)

	// 'debug' subcommand.
	debugCommand := &cobra.Command{
		Use:   "debug [package]",
		Short: "Compile and begin debugging program.",
		Long: `Compiles your program with optimizations disabled,
starts and attaches to it, and enables you to immediately begin debugging your program.`,
		Run: debugCmd,
	}
	RootCommand.AddCommand(debugCommand)

	// 'exec' subcommand.
	execCommand := &cobra.Command{
		Use:   "exec [./path/to/binary]",
		Short: "Runs precompiled binary, attaches and begins debug session.",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("you must provide a path to a binary")
			}
			return nil
		},
		Run: func(cmd *cobra.Command, args []string) {
			os.Exit(execute(0, args, conf))
		},
	}
	RootCommand.AddCommand(execCommand)

	// 'test' subcommand.
	testCommand := &cobra.Command{
		Use:   "test [package]",
		Short: "Compile test binary and begin debugging program.",
		Long:  `Compiles a test binary with optimizations disabled, starts and attaches to it, and enable you to immediately begin debugging your program.`,
		Run:   testCmd,
	}
	RootCommand.AddCommand(testCommand)

	// 'attach' subcommand.
	attachCommand := &cobra.Command{
		Use:   "attach pid",
		Short: "Attach to running process and begin debugging.",
		Long:  "Attach to running process and begin debugging.",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("you must provide a PID")
			}
			return nil
		},
		Run: attachCmd,
	}
	RootCommand.AddCommand(attachCommand)

	RootCommand.Execute()

}

func debugCmd(cmd *cobra.Command, args []string) {
	status := func() int {
		var pkg string
		dlvArgs, targetArgs := splitArgs(cmd, args)

		if len(dlvArgs) > 0 {
			pkg = args[0]
		}
		err := gobuild(debugname, pkg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		fp, err := filepath.Abs("./" + debugname)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		defer os.Remove(fp)

		processArgs := append([]string{"./" + debugname}, targetArgs...)
		return execute(0, processArgs, conf)
	}()
	os.Exit(status)
}

func testCmd(cmd *cobra.Command, args []string) {
	status := func() int {
		var pkg string
		dlvArgs, targetArgs := splitArgs(cmd, args)

		if len(dlvArgs) > 0 {
			pkg = args[0]
		}
		err := gotestbuild(pkg)
		if err != nil {
			return 1
		}
		defer os.Remove("./" + testdebugname)
		processArgs := append([]string{"./" + testdebugname}, targetArgs...)

		return execute(0, processArgs, conf)
	}()
	os.Exit(status)
}

func attachCmd(cmd *cobra.Command, args []string) {
	pid, err := strconv.Atoi(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid pid: %s\n", args[0])
		os.Exit(1)
	}
	os.Exit(execute(pid, nil, conf))
}

func splitArgs(cmd *cobra.Command, args []string) ([]string, []string) {
	if cmd.ArgsLenAtDash() >= 0 {
		return args[:cmd.ArgsLenAtDash()], args[cmd.ArgsLenAtDash():]
	}
	return args, []string{}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func execute(attachPid int, processArgs []string, conf *config.Config) int {
	// Make a TCP listener
	listener, err := net.Listen("tcp", Addr)
	if err != nil {
		fmt.Printf("couldn't start listener: %s\n", err)
		return 1
	}
	defer listener.Close()

	// Create and start a debugger server
	server := rpc.NewServer(&service.Config{
		Listener:    listener,
		ProcessArgs: processArgs,
		AttachPid:   attachPid,
		AcceptMulti: false,
	}, false)
	if err := server.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	Client = rpc.NewClient(listener.Addr().String())
	Term = terminal.NewSimple(Client, conf, nil)
	Term.PrintFile = nil

	restoreBreakpoints()

	http.HandleFunc("/static/", handlerWrapper(staticHandler))
	http.HandleFunc("/list", handlerWrapper(listHandler))
	http.HandleFunc("/bp", handlerWrapper(bpHandler))
	http.HandleFunc("/cmd", handlerWrapper(cmdHandler))
	http.HandleFunc("/interrupt", handlerWrapper(interruptHandler))

	Listener, _ = net.Listen("tcp", "127.0.0.1:8180")

	s := &http.Server{
		Handler:        http.DefaultServeMux,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	s.Serve(Listener)

	return 0
}

func gobuild(debugname, pkg string) error {
	args := []string{"-gcflags", "-N -l", "-o", debugname}
	if BuildFlags != "" {
		args = append(args, BuildFlags)
	}
	args = append(args, pkg)
	return gocommand("build", args...)
}

func gotestbuild(pkg string) error {
	args := []string{"-gcflags", "-N -l", "-c", "-o", testdebugname}
	if BuildFlags != "" {
		args = append(args, BuildFlags)
	}
	args = append(args, pkg)
	return gocommand("test", args...)
}

func gocommand(command string, args ...string) error {
	allargs := []string{command}
	allargs = append(allargs, args...)
	goBuild := exec.Command("go", allargs...)
	goBuild.Stderr = os.Stderr
	return goBuild.Run()
}
