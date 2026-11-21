package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Params is one overclock request.
type Params struct {
	Method  string
	Level   int
	Gfx     int
	Mem     int
	MaxTemp int
}

func (p Params) describe() string {
	if p.Method == methodAppClocks {
		return fmt.Sprintf("application clocks mem %d MHz, gfx %d MHz", p.Mem, p.Gfx)
	}
	return fmt.Sprintf("offsets level %d: gfx %+d MHz, mem %+d MT/s", p.Level, p.Gfx, p.Mem)
}

type stepResult struct {
	Kept        bool
	Interrupted bool
	Unrestored  bool // defaults could not be restored, see the printed steps
	Reason      string
}

func (a *app) applyParams(ctx context.Context, p Params) error {
	switch p.Method {
	case methodOffsets:
		return a.setOffsets(ctx, p.Level, p.Gfx, p.Mem)
	case methodAppClocks:
		_, err := a.smi(ctx, "-ac", fmt.Sprintf("%d,%d", p.Mem, p.Gfx))
		return err
	}
	return fmt.Errorf("unknown method %q", p.Method)
}

// runStep arms the watchdog, applies the request, optionally holds it under
// the guard and then waits for the user to confirm. Anything but a timely
// confirmation ends with the defaults restored.
func (a *app) runStep(ctx context.Context, p Params, hold, window time.Duration) (stepResult, error) {
	deadline := time.Now().Add(applySlack + hold + window + watchdogGrace)
	if err := a.arm(p, deadline); err != nil {
		return stepResult{}, err
	}
	fmt.Fprintf(a.out, "applying %s\n", p.describe())
	a.logEvent("apply %s max-temp=%d", p.describe(), p.MaxTemp)
	if err := a.applyParams(ctx, p); err != nil {
		return a.abort("apply failed: "+err.Error(), ctx.Err() != nil), nil
	}
	if hold > 0 {
		fmt.Fprintf(a.out, "holding for %s, keep a GPU load running in another terminal\n", hold)
		reason, err := a.monitor(ctx, p.MaxTemp, hold, true)
		if err != nil {
			return a.abort("interrupted", true), nil
		}
		if reason != "" {
			return a.abort(reason, false), nil
		}
	}
	if err := a.setDeadline(time.Now().Add(window + watchdogGrace)); err != nil {
		return a.abort(err.Error(), false), nil
	}
	ok, err := a.confirm(ctx, window)
	switch {
	case err != nil:
		return a.abort("interrupted", true), nil
	case !ok:
		return a.abort("not confirmed in time", false), nil
	}
	if err := a.setDeadline(time.Time{}); err != nil {
		return a.abort(err.Error(), false), nil
	}
	a.logEvent("kept %s", p.describe())
	return stepResult{Kept: true}, nil
}

func (a *app) abort(reason string, interrupted bool) stepResult {
	fmt.Fprintf(a.out, "\nrestoring defaults: %s\n", reason)
	restored := a.rollback(reason)
	return stepResult{Reason: reason, Interrupted: interrupted, Unrestored: !restored}
}

// confirm waits for ENTER. A timeout, EOF or any other answer counts as "no".
func (a *app) confirm(ctx context.Context, window time.Duration) (bool, error) {
	a.in.drain()
	fmt.Fprintf(a.out, "Screen OK and system responsive? Press ENTER within %s to keep it, anything else restores defaults: ", window.Round(time.Second))
	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case line, open := <-a.in.lines:
		if !open {
			fmt.Fprintln(a.out, "\nno answer (input closed)")
			return false, nil
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		return answer == "" || answer == "y" || answer == "yes", nil
	case <-timer.C:
		fmt.Fprintln(a.out, "\nno answer")
		return false, nil
	case <-ctx.Done():
		fmt.Fprintln(a.out)
		return false, ctx.Err()
	}
}

// monitor samples once per a.poll for d and returns the first unhealthy
// reason. Repeated nvidia-smi failures count as unhealthy: that is what a
// hung GPU looks like.
func (a *app) monitor(ctx context.Context, maxTemp int, d time.Duration, show bool) (string, error) {
	end := time.Now().Add(d)
	fails := 0
	for {
		smp, err := a.sample(ctx)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err != nil {
			fails++
			if fails >= maxSampleFailures {
				return fmt.Sprintf("nvidia-smi failed %d times in a row: %v", fails, err), nil
			}
		} else {
			fails = 0
			if show {
				fmt.Fprintf(a.out, "  %s  %s\n", time.Now().Format("15:04:05"), smp)
			}
			if reason := verdict(smp, maxTemp); reason != "" {
				return reason + "; last sample: " + smp.String(), nil
			}
		}
		if !time.Now().Before(end) {
			return "", nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(a.poll):
		}
	}
}

var errNothingToApply = errors.New("nothing to apply: pass -gfx and/or -mem (use otval to restore defaults)")
