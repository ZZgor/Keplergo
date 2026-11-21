package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const version = "0.1.0"

const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitRollback = 3
)

const (
	defaultMaxTemp    = 85
	defaultWindow     = 20 * time.Second
	minWindow         = 5 * time.Second
	maxWindow         = 5 * time.Minute
	maxHold           = 10 * time.Minute
	watchdogGrace     = 2 * time.Second
	applySlack        = 30 * time.Second
	maxSampleFailures = 3
)

const usageText = `keplerGO %s - monitor and overclock Nvidia Kepler GPUs (legacy 470.xx driver)

usage: keplergo [-i gpu] <command> [flags]

commands:
  info     GPU details and what this machine supports
  status   active session, clocks and offsets
  watch    live monitor (-interval 1s, -n 0)
  oc       clock offsets through nvidia-settings (Xorg + Coolbits, run as your user)
  apply    application clocks through nvidia-smi (Tesla and desktop Kepler, run with sudo)
  tune     raise the core offset step by step under load and find a stable value
  otval    restore everything to defaults (works from a text console or over ssh)
  debug    print diagnostics for a failed overclock (redirect it to a file)
  version  print the version

Every change is armed with a watchdog. Unless you press ENTER in time, or if
the GPU gets too hot, throttles or stops answering, the defaults are restored
on their own. Nothing is written to the system or started at boot, so a
reboot always returns the card to stock.

exit codes: 0 ok, 1 error, 2 usage, 3 defaults were restored
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-ctx.Done()
		stop() // a second signal gets the default behaviour and ends the process
	}()
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func newFlagSet(name string, w io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(w)
	return fs
}

// parseFlags returns done=true when the command must stop with the given code.
func parseFlags(fs *flag.FlagSet, args []string) (code int, done bool) {
	err := fs.Parse(args)
	switch {
	case err == nil && fs.NArg() == 0:
		return exitOK, false
	case errors.Is(err, flag.ErrHelp):
		return exitOK, true
	case err == nil:
		fmt.Fprintf(fs.Output(), "unexpected argument %q\n", fs.Arg(0))
		return exitUsage, true
	}
	return exitUsage, true
}

func run(ctx context.Context, args []string, stdin io.Reader, out, errw io.Writer) int {
	gfs := newFlagSet("keplergo", errw)
	gpu := gfs.Int("i", 0, "GPU index")
	gfs.Usage = func() { fmt.Fprintf(errw, usageText, version) }
	if err := gfs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	rest := gfs.Args()
	if len(rest) == 0 {
		gfs.Usage()
		return exitUsage
	}
	if rest[0] == "version" {
		fmt.Fprintln(out, "keplergo", version)
		return exitOK
	}
	if rest[0] == "help" {
		gfs.Usage()
		return exitOK
	}
	dir, err := stateDir()
	if err != nil {
		fmt.Fprintln(errw, "error:", err)
		return exitError
	}
	a := &app{
		run: execRunner, gpu: *gpu, stdin: stdin, out: out, errw: errw,
		xenv: resolveXEnv(), state: dir, poll: time.Second, pause: 2 * time.Second,
	}
	cmd, cmdArgs := rest[0], rest[1:]
	switch cmd {
	case "info":
		return a.cmdInfo(ctx, cmdArgs)
	case "status":
		return a.cmdStatus(ctx, cmdArgs)
	case "watch":
		return a.cmdWatch(ctx, cmdArgs)
	case "oc":
		return a.cmdOC(ctx, cmdArgs)
	case "apply":
		return a.cmdApply(ctx, cmdArgs)
	case "tune":
		return a.cmdTune(ctx, cmdArgs)
	case "otval":
		return a.cmdOtval(cmdArgs)
	case "debug":
		return a.cmdDebug(ctx, cmdArgs)
	case "watchdog":
		return a.cmdWatchdog(ctx)
	}
	fmt.Fprintf(errw, "unknown command %q\n\n", cmd)
	gfs.Usage()
	return exitUsage
}
