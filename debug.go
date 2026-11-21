package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
)

const (
	debugEvents     = 20
	debugSmiLines   = 70
	debugKernelLine = 20
)

func (a *app) section(title string) {
	fmt.Fprintf(a.out, "\n== %s ==\n", title)
}

func (a *app) runTimeout(ctx context.Context, d time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return a.run(ctx, nil, name, args...)
}

// kernelNvidiaLines returns the newest NVRM / Xid kernel messages, which is
// where an unstable overclock leaves its trace.
func (a *app) kernelNvidiaLines(ctx context.Context) ([]string, error) {
	out, err := a.runTimeout(ctx, 10*time.Second, "journalctl", "-k", "-b", "--no-pager", "-q")
	if err != nil || len(out) == 0 {
		var derr error
		out, derr = a.runTimeout(ctx, 10*time.Second, "dmesg")
		if derr != nil {
			return nil, derr
		}
	}
	var hits []string
	for _, line := range strings.Split(string(out), "\n") {
		low := strings.ToLower(line)
		if strings.Contains(low, "nvrm") || strings.Contains(low, "xid") {
			hits = append(hits, line)
		}
	}
	if len(hits) > debugKernelLine {
		hits = hits[len(hits)-debugKernelLine:]
	}
	return hits, nil
}

func (a *app) cmdDebug(ctx context.Context, args []string) int {
	fs := newFlagSet("debug", a.errw)
	if code, done := parseFlags(fs, args); done {
		return code
	}
	fmt.Fprintf(a.out, "keplergo %s, %s, %s/%s, %s\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH, time.Now().Format(time.RFC3339))

	a.section("environment")
	fmt.Fprintf(a.out, "uid %d, session type %s\n", os.Geteuid(), sessionType())
	fmt.Fprintf(a.out, "DISPLAY=%s XAUTHORITY=%s\n", a.xenv.Display, a.xenv.XAuthority)
	fmt.Fprintf(a.out, "state dir %s\n", a.state)

	a.section("current sample")
	if smp, err := a.sample(ctx); err != nil {
		fmt.Fprintln(a.out, "nvidia-smi failed:", err)
	} else {
		fmt.Fprintf(a.out, "%s | %s\n", smp.Name, smp)
	}

	a.section("session")
	switch s, err := a.loadSession(); {
	case err != nil:
		fmt.Fprintln(a.out, "unreadable:", err)
	case s == nil:
		fmt.Fprintln(a.out, "none")
	default:
		fmt.Fprintf(a.out, "%s, max-temp %d C, watchdog pid %d (running: %t), confirmed: %t\n",
			Params{Method: s.Method, Level: s.Level, Gfx: s.Gfx, Mem: s.Mem}.describe(), s.MaxTemp, s.PID, isOurWatchdog(s.PID), s.confirmed())
	}

	a.section(fmt.Sprintf("last %d events (newest last)", debugEvents))
	if events := a.lastEvents(debugEvents); len(events) == 0 {
		fmt.Fprintln(a.out, "none")
	} else {
		fmt.Fprintln(a.out, strings.Join(events, "\n"))
	}

	a.section("offsets")
	a.printOffsetCaps(ctx)

	a.section("nvidia-smi -q (performance, clock, temperature)")
	if out, err := a.smi(ctx, "-q", "-d", "PERFORMANCE,CLOCK,TEMPERATURE"); err != nil {
		fmt.Fprintln(a.out, "failed:", err)
	} else {
		lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
		if len(lines) > debugSmiLines {
			lines = lines[:debugSmiLines]
		}
		fmt.Fprintln(a.out, strings.Join(lines, "\n"))
	}

	a.section("kernel messages (NVRM / Xid)")
	hits, err := a.kernelNvidiaLines(ctx)
	switch {
	case err != nil:
		fmt.Fprintln(a.out, "unavailable:", err)
		fmt.Fprintln(a.out, "try: sudo dmesg | grep -i -E 'nvrm|xid'")
	case len(hits) == 0:
		fmt.Fprintln(a.out, "none found")
	default:
		fmt.Fprintln(a.out, strings.Join(hits, "\n"))
	}
	return exitOK
}
