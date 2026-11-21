package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const (
	restoreAttempts = 2
	recoveryHint    = `Defaults are not fully restored yet. A watchdog that is still running keeps retrying. You can also:
  - press Ctrl+Alt+F3, log in and run: keplergo otval
  - ssh in from another machine and run: keplergo otval
  - reboot: offsets and application clocks are never persistent`
)

type stepStatus int

const (
	stepOK stepStatus = iota
	stepSkipped
	stepFailed
)

// Step is one thing otval tried to put back to default.
type Step struct {
	Name   string
	Status stepStatus
	Detail string
}

func countFailed(steps []Step) int {
	n := 0
	for _, st := range steps {
		if st.Status == stepFailed {
			n++
		}
	}
	return n
}

func (st stepStatus) label() string {
	switch st {
	case stepOK:
		return "ok"
	case stepSkipped:
		return "skipped"
	}
	return "FAILED"
}

func printSteps(w io.Writer, steps []Step) {
	for _, st := range steps {
		fmt.Fprintf(w, "  [%s] %s: %s\n", st.Status.label(), st.Name, st.Detail)
	}
}

func summarize(steps []Step) string {
	parts := make([]string, 0, len(steps))
	for _, st := range steps {
		parts = append(parts, fmt.Sprintf("%s=%s", st.Name, st.Status.label()))
	}
	return strings.Join(parts, "; ")
}

// restoreDefaults puts every clock back to stock. It is idempotent and safe to
// run at any time, with or without a session.
func (a *app) restoreDefaults(ctx context.Context, s *Session) []Step {
	steps := a.restoreOffsets(ctx, s)
	return append(steps, a.restoreAppClocks(ctx, s))
}

func (a *app) restoreOffsets(ctx context.Context, s *Session) []Step {
	const name = "clock offsets"
	if err := a.xReachable(ctx); err != nil {
		st := Step{Name: name, Status: stepFailed, Detail: "cannot reach the X server: " + clip(err.Error(), 160)}
		if s != nil && s.Method == methodAppClocks {
			st.Status = stepSkipped
			st.Detail = "no X server, offsets were not used in this session"
		}
		return []Step{st}
	}
	levels := a.detectLevels(ctx)
	if len(levels) == 0 {
		return []Step{{Name: name, Status: stepSkipped, Detail: "no offset controls (Coolbits is not active), nothing to reset"}}
	}
	steps := make([]Step, 0, len(levels))
	for _, l := range levels {
		steps = append(steps, a.resetLevel(ctx, l))
	}
	return steps
}

func (a *app) resetLevel(ctx context.Context, level int) Step {
	st := Step{Name: fmt.Sprintf("offsets level %d", level), Status: stepOK}
	var notes []string
	for _, attr := range []struct{ label, name string }{{"gfx", attrGfx}, {"mem", attrMem}} {
		before, err := a.getOffset(ctx, attr.name, level)
		if err != nil {
			if attr.name == attrGfx {
				st.Status = stepFailed
				notes = append(notes, "gfx: cannot read")
			}
			continue
		}
		if before == 0 {
			notes = append(notes, attr.label+" 0")
			continue
		}
		if err := a.setOffset(ctx, attr.name, level, 0); err != nil {
			st.Status = stepFailed
			notes = append(notes, attr.label+": "+clip(err.Error(), 120))
			continue
		}
		after, err := a.getOffset(ctx, attr.name, level)
		if err != nil || after != 0 {
			st.Status = stepFailed
			notes = append(notes, fmt.Sprintf("%s still %+d", attr.label, after))
			continue
		}
		notes = append(notes, fmt.Sprintf("%s %+d -> 0", attr.label, before))
	}
	st.Detail = strings.Join(notes, ", ")
	return st
}

func (a *app) restoreAppClocks(ctx context.Context, s *Session) Step {
	const name = "application clocks"
	if os.Geteuid() != 0 {
		if s != nil && s.Method == methodAppClocks {
			return Step{Name: name, Status: stepFailed, Detail: "needs root, run: sudo keplergo otval"}
		}
		return Step{Name: name, Status: stepSkipped, Detail: "not root, nothing to reset for this session"}
	}
	if _, err := a.smi(ctx, "-rac"); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not supported") {
			return Step{Name: name, Status: stepSkipped, Detail: "not supported by this GPU"}
		}
		return Step{Name: name, Status: stepFailed, Detail: clip(err.Error(), 160)}
	}
	return Step{Name: name, Status: stepOK, Detail: "reset"}
}

func (a *app) restoreWithRetry(ctx context.Context, s *Session) []Step {
	var steps []Step
	for attempt := 1; attempt <= restoreAttempts; attempt++ {
		steps = a.restoreDefaults(ctx, s)
		if countFailed(steps) == 0 {
			return steps
		}
		if attempt < restoreAttempts {
			time.Sleep(a.pause)
		}
	}
	return steps
}

// rollback restores defaults, stops the watchdog and drops the session. When
// something could not be restored the session stays, with an expired
// deadline, so the watchdog keeps retrying. Reports whether all is restored.
func (a *app) rollback(reason string) bool {
	s, err := a.loadSession()
	if err != nil {
		fmt.Fprintln(a.errw, "warning:", err)
		s = nil
	}
	if s != nil {
		a.adoptSessionEnv(s)
	}
	steps := a.restoreWithRetry(context.Background(), s)
	printSteps(a.out, steps)
	a.logEvent("rollback reason=%q steps: %s", reason, summarize(steps))
	if countFailed(steps) == 0 {
		if s != nil {
			if err := stopWatchdog(s.PID); err != nil {
				fmt.Fprintln(a.errw, "warning:", err)
			}
		}
		_ = a.clearSession()
		return true
	}
	if s != nil {
		s.Deadline = time.Now()
		_ = a.saveSession(s)
	}
	fmt.Fprintln(a.out, recoveryHint)
	return false
}
