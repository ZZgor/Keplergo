package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const maxLogSize = 1 << 20

type app struct {
	run   Runner
	gpu   int
	stdin io.Reader
	in    *lineReader
	out   io.Writer
	errw  io.Writer
	xenv  xenv
	state string
	poll  time.Duration // sampling interval while holding
	pause time.Duration // pause between restore attempts
}

var (
	errNoSMI      = errors.New("nvidia-smi not found: install the NVIDIA 470.xx legacy driver")
	errNoSettings = errors.New("nvidia-settings not found: install the nvidia-settings package that matches your driver")
)

func (a *app) fail(code int, format string, args ...any) int {
	fmt.Fprintf(a.errw, "error: "+format+"\n", args...)
	return code
}

// smi runs nvidia-smi against the selected GPU.
func (a *app) smi(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, smiTimeout)
	defer cancel()
	full := append([]string{"-i", strconv.Itoa(a.gpu)}, args...)
	out, err := a.run(ctx, nil, "nvidia-smi", full...)
	if errors.Is(err, exec.ErrNotFound) {
		return nil, errNoSMI
	}
	return out, err
}

// settings runs nvidia-settings against the X server recorded in a.xenv.
func (a *app) settings(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, settingsTimeout)
	defer cancel()
	out, err := a.run(ctx, a.xenv.vars(), "nvidia-settings", args...)
	if errors.Is(err, exec.ErrNotFound) {
		return nil, errNoSettings
	}
	return out, err
}

// adoptSessionEnv lets a text console or ssh login reuse the X display of the
// session that applied the overclock.
func (a *app) adoptSessionEnv(s *Session) {
	if os.Getenv("DISPLAY") == "" && s.Display != "" {
		a.xenv.Display = s.Display
	}
	if os.Getenv("XAUTHORITY") == "" && s.XAuthority != "" {
		a.xenv.XAuthority = s.XAuthority
	}
}

// stateDir is the same for the normal user and for sudo, so otval finds the
// session whichever way it is started.
func stateDir() (string, error) {
	if d := os.Getenv("KEPLERGO_STATE_DIR"); d != "" {
		return d, nil
	}
	if os.Geteuid() == 0 {
		if name := os.Getenv("SUDO_USER"); name != "" {
			if u, err := user.Lookup(name); err == nil {
				return filepath.Join(u.HomeDir, ".local", "state", "keplergo"), nil
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "keplergo"), nil
}

func (a *app) ensureState() error {
	if err := os.MkdirAll(a.state, 0o755); err != nil {
		return err
	}
	fixOwner(a.state)
	return nil
}

func (a *app) logEvent(format string, args ...any) {
	if a.ensureState() != nil {
		return
	}
	path := filepath.Join(a.state, "events.log")
	if st, err := os.Stat(path); err == nil && st.Size() > maxLogSize {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
	fixOwner(path)
}

func (a *app) lastEvents(n int) []string {
	data, err := os.ReadFile(filepath.Join(a.state, "events.log"))
	if err != nil || strings.TrimSpace(string(data)) == "" {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// lineReader turns stdin into a channel of lines; the channel closes on EOF.
type lineReader struct {
	lines chan string
}

func newLineReader(r io.Reader) *lineReader {
	lr := &lineReader{lines: make(chan string)}
	go func() {
		defer close(lr.lines)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			lr.lines <- sc.Text()
		}
	}()
	return lr
}

// drain drops one pending line so earlier keystrokes cannot confirm a new step.
func (lr *lineReader) drain() {
	select {
	case <-lr.lines:
	default:
	}
}

func (a *app) startInput() {
	if a.in == nil {
		a.in = newLineReader(a.stdin)
	}
}
