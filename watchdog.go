package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	watchdogRetryFor   = 10 * time.Minute
	watchdogRetryPause = 5 * time.Second
)

// spawnWatchdog starts this binary again in its own session, so closing the
// terminal or freezing the shell that applied the overclock cannot stop it.
func (a *app) spawnWatchdog() (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(exe, "-i", strconv.Itoa(a.gpu), "watchdog")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}

// isOurWatchdog guards against a recycled pid: only a process started as
// "<this binary> ... watchdog" counts.
func isOurWatchdog(pid int) bool {
	if pid <= 0 {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(data) == 0 {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	return len(args) >= 2 && filepath.Base(args[0]) == filepath.Base(exe) && args[len(args)-1] == "watchdog"
}

func stopWatchdog(pid int) error {
	if !isOurWatchdog(pid) {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("cannot stop the watchdog (pid %d, maybe started by root): %w", pid, err)
	}
	for i := 0; i < 30; i++ {
		if !isOurWatchdog(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("watchdog pid %d did not exit", pid)
}

// arm records the session and makes sure a watchdog is running for it. It is
// called before anything is applied, so even a hung apply gets undone.
func (a *app) arm(p Params, deadline time.Time) error {
	s, err := a.loadSession()
	if err != nil {
		s = nil // an unreadable file is replaced
	}
	if s != nil && s.GPU != a.gpu && isOurWatchdog(s.PID) {
		return fmt.Errorf("a session is already active on GPU %d: run keplergo otval first", s.GPU)
	}
	if s != nil && s.GPU == a.gpu && isOurWatchdog(s.PID) {
		s.Method, s.Level, s.Gfx, s.Mem, s.MaxTemp = p.Method, p.Level, p.Gfx, p.Mem, p.MaxTemp
		s.Deadline = deadline
		return a.saveSession(s)
	}
	s = &Session{
		Method: p.Method, GPU: a.gpu, Level: p.Level, Gfx: p.Gfx, Mem: p.Mem,
		MaxTemp: p.MaxTemp, Started: time.Now(), Deadline: deadline,
		Display: a.xenv.Display, XAuthority: a.xenv.XAuthority,
	}
	if err := a.saveSession(s); err != nil {
		return err
	}
	pid, err := a.spawnWatchdog()
	if err != nil {
		_ = a.clearSession()
		return fmt.Errorf("cannot start the watchdog: %w", err)
	}
	s.PID = pid
	return a.saveSession(s)
}

// cmdWatchdog is the hidden background guard. It reverts on a missed
// confirmation, on heat or throttling, and when nvidia-smi stops answering.
func (a *app) cmdWatchdog(ctx context.Context) int {
	fails := 0
	for {
		s, err := a.loadSession()
		if err != nil {
			return a.watchdogRevert(nil, "session file unreadable: "+err.Error())
		}
		if s == nil {
			return exitOK
		}
		a.adoptSessionEnv(s)
		if !s.confirmed() && time.Now().After(s.Deadline) {
			return a.watchdogRevert(s, "no confirmation before the deadline")
		}
		smp, err := a.sample(ctx)
		if ctx.Err() != nil {
			a.logEvent("watchdog stopped by signal")
			return exitOK
		}
		if err != nil {
			fails++
			if fails >= maxSampleFailures {
				return a.watchdogRevert(s, fmt.Sprintf("nvidia-smi failed %d times in a row: %v", fails, err))
			}
		} else {
			fails = 0
			if reason := verdict(smp, s.MaxTemp); reason != "" {
				return a.watchdogRevert(s, reason+"; last sample: "+smp.String())
			}
		}
		interval := time.Second
		if s.confirmed() {
			interval = 2 * time.Second
		}
		select {
		case <-ctx.Done():
			a.logEvent("watchdog stopped by signal")
			return exitOK
		case <-time.After(interval):
		}
	}
}

// watchdogRevert keeps trying until the defaults are back or the retry
// window ends. The session is kept if it gives up, so otval can finish later.
func (a *app) watchdogRevert(s *Session, reason string) int {
	a.logEvent("watchdog restoring defaults: %s", reason)
	giveUp := time.Now().Add(watchdogRetryFor)
	for {
		steps := a.restoreDefaults(context.Background(), s)
		a.logEvent("watchdog restore: %s", summarize(steps))
		if countFailed(steps) == 0 {
			_ = a.clearSession()
			return exitRollback
		}
		if time.Now().After(giveUp) {
			a.logEvent("watchdog gave up, run: keplergo otval")
			if s != nil {
				s.PID = 0
				_ = a.saveSession(s)
			}
			return exitError
		}
		time.Sleep(watchdogRetryPause)
	}
}
