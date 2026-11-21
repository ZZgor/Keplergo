package main

import (
	"context"
	"fmt"
	"time"
)

const (
	minGfxOffset = -300
	maxGfxOffset = 150
	minMemOffset = -500
	maxMemOffset = 400
	minMaxTemp   = 40
	maxMaxTemp   = 95
)

func checkLimits(gfx, mem, maxTemp int, unsafe bool) error {
	if maxTemp < minMaxTemp || maxTemp > maxMaxTemp {
		return fmt.Errorf("-max-temp must be between %d and %d", minMaxTemp, maxMaxTemp)
	}
	if unsafe {
		return nil
	}
	if gfx < minGfxOffset || gfx > maxGfxOffset {
		return fmt.Errorf("core offset %d is outside %d..%d MHz (-unsafe lifts the limit)", gfx, minGfxOffset, maxGfxOffset)
	}
	if mem < minMemOffset || mem > maxMemOffset {
		return fmt.Errorf("memory offset %d is outside %d..%d MT/s (-unsafe lifts the limit)", mem, minMemOffset, maxMemOffset)
	}
	return nil
}

func checkTimes(hold, window time.Duration) error {
	if window < minWindow || window > maxWindow {
		return fmt.Errorf("-window must be between %s and %s", minWindow, maxWindow)
	}
	if hold < 0 || hold > maxHold {
		return fmt.Errorf("-hold must be between 0 and %s", maxHold)
	}
	return nil
}

func (a *app) cmdInfo(ctx context.Context, args []string) int {
	fs := newFlagSet("info", a.errw)
	if code, done := parseFlags(fs, args); done {
		return code
	}
	name, err := a.queryField(ctx, "name")
	if err != nil {
		return a.fail(exitError, "%v", err)
	}
	fmt.Fprintf(a.out, "%-22s %s\n", "name:", name)
	rows := []struct{ label, field string }{
		{"driver", "driver_version"},
		{"vbios", "vbios_version"},
		{"pci bus", "pci.bus_id"},
		{"memory (MiB)", "memory.total"},
		{"pstate", "pstate"},
		{"max core clock (MHz)", "clocks.max.sm"},
		{"max mem clock (MHz)", "clocks.max.mem"},
		{"power limit (W)", "power.limit"},
	}
	for _, r := range rows {
		v, err := a.queryField(ctx, r.field)
		if err != nil {
			v = "n/a"
		}
		fmt.Fprintf(a.out, "%-22s %s\n", r.label+":", v)
	}

	fmt.Fprintln(a.out, "\ncapabilities:")
	fmt.Fprintf(a.out, "  %-20s %s\n", "session type:", sessionType())
	out, err := a.smi(ctx, "-q", "-d", "SUPPORTED_CLOCKS")
	if clocks := parseSupportedClocks(string(out)); err == nil && len(clocks) > 0 {
		fmt.Fprintf(a.out, "  %-20s supported (use apply)\n%s\n", "application clocks:", formatClocks(clocks))
	} else {
		fmt.Fprintf(a.out, "  %-20s not supported (normal for mobile Quadro, use oc)\n", "application clocks:")
	}
	a.printOffsetCaps(ctx)
	return exitOK
}

func (a *app) printOffsetCaps(ctx context.Context) {
	const label = "clock offsets:"
	if sessionType() == "wayland" {
		fmt.Fprintf(a.out, "  %-20s unavailable under Wayland, log in to an Xorg session\n", label)
		return
	}
	if err := a.xReachable(ctx); err != nil {
		fmt.Fprintf(a.out, "  %-20s nvidia-settings cannot reach X (DISPLAY=%s): %s\n", label, a.xenv.Display, clip(err.Error(), 140))
		return
	}
	levels := a.detectLevels(ctx)
	if len(levels) == 0 {
		fmt.Fprintf(a.out, "  %-20s not available\n%s\n", label, coolbitsHelp)
		return
	}
	fmt.Fprintf(a.out, "  %-20s available\n", label)
	for _, l := range levels {
		g, gerr := a.getOffset(ctx, attrGfx, l)
		m, merr := a.getOffset(ctx, attrMem, l)
		switch {
		case gerr != nil:
			fmt.Fprintf(a.out, "    level %d: unreadable\n", l)
		case merr != nil:
			fmt.Fprintf(a.out, "    level %d: gfx %+d MHz\n", l, g)
		default:
			fmt.Fprintf(a.out, "    level %d: gfx %+d MHz, mem %+d MT/s\n", l, g, m)
		}
	}
}

func (a *app) cmdStatus(ctx context.Context, args []string) int {
	fs := newFlagSet("status", a.errw)
	if code, done := parseFlags(fs, args); done {
		return code
	}
	s, err := a.loadSession()
	switch {
	case err != nil:
		fmt.Fprintln(a.out, "session: unreadable:", err)
	case s == nil:
		fmt.Fprintln(a.out, "session: none")
	default:
		alive := isOurWatchdog(s.PID)
		fmt.Fprintf(a.out, "session: %s, max-temp %d C\n", Params{Method: s.Method, Level: s.Level, Gfx: s.Gfx, Mem: s.Mem}.describe(), s.MaxTemp)
		switch {
		case !alive:
			fmt.Fprintln(a.out, "         watchdog is not running (stale session), run: keplergo otval")
		case s.confirmed():
			fmt.Fprintf(a.out, "         confirmed, watchdog pid %d guards the temperature\n", s.PID)
		default:
			fmt.Fprintf(a.out, "         waiting for confirmation until %s, watchdog pid %d\n", s.Deadline.Format("15:04:05"), s.PID)
		}
	}
	smp, err := a.sample(ctx)
	if err != nil {
		return a.fail(exitError, "%v", err)
	}
	fmt.Fprintf(a.out, "gpu:     %s | %s\n", smp.Name, smp)
	a.printOffsetCaps(ctx)
	return exitOK
}

func (a *app) cmdWatch(ctx context.Context, args []string) int {
	fs := newFlagSet("watch", a.errw)
	interval := fs.Duration("interval", time.Second, "time between samples")
	count := fs.Int("n", 0, "stop after this many samples (0 = until Ctrl-C)")
	if code, done := parseFlags(fs, args); done {
		return code
	}
	if *interval < 200*time.Millisecond || *count < 0 {
		return a.fail(exitUsage, "-interval must be at least 200ms and -n must not be negative")
	}
	named := false
	for i := 0; *count == 0 || i < *count; i++ {
		smp, err := a.sample(ctx)
		switch {
		case ctx.Err() != nil:
			return exitOK
		case err != nil:
			fmt.Fprintln(a.errw, "sample failed:", err)
		default:
			if !named {
				fmt.Fprintf(a.out, "GPU %d: %s\n", a.gpu, smp.Name)
				named = true
			}
			fmt.Fprintf(a.out, "%s  %s\n", time.Now().Format("15:04:05"), smp)
		}
		if *count != 0 && i == *count-1 {
			break
		}
		select {
		case <-ctx.Done():
			return exitOK
		case <-time.After(*interval):
		}
	}
	return exitOK
}

// cmdOtval restores every clock to default. It needs no session and no
// working terminal beyond a text console or an ssh login.
func (a *app) cmdOtval(args []string) int {
	fs := newFlagSet("otval", a.errw)
	if code, done := parseFlags(fs, args); done {
		return code
	}
	fmt.Fprintln(a.out, "restoring defaults")
	if !a.rollback("otval") {
		return exitError
	}
	fmt.Fprintln(a.out, "defaults restored")
	ctx, cancel := context.WithTimeout(context.Background(), smiTimeout)
	defer cancel()
	if smp, err := a.sample(ctx); err == nil {
		fmt.Fprintf(a.out, "now: %s\n", smp)
	}
	return exitOK
}

func (a *app) offsetsResult(res stepResult, err error) int {
	switch {
	case err != nil:
		return a.fail(exitError, "%v", err)
	case res.Kept:
		return exitOK
	case res.Unrestored:
		return exitError
	}
	fmt.Fprintf(a.out, "defaults restored (%s). Details: keplergo debug\n", res.Reason)
	return exitRollback
}

func (a *app) cmdOC(ctx context.Context, args []string) int {
	fs := newFlagSet("oc", a.errw)
	level := fs.Int("level", -1, "performance level to adjust (-1 = highest available)")
	gfx := fs.Int("gfx", 0, "core clock offset in MHz")
	mem := fs.Int("mem", 0, "memory transfer rate offset in MT/s")
	maxTemp := fs.Int("max-temp", defaultMaxTemp, "restore defaults at this temperature in C")
	hold := fs.Duration("hold", 0, "watch the GPU for this long before asking for confirmation")
	window := fs.Duration("window", defaultWindow, "time you have to confirm")
	unsafe := fs.Bool("unsafe", false, "lift the offset limits")
	if code, done := parseFlags(fs, args); done {
		return code
	}
	if *gfx == 0 && *mem == 0 {
		return a.fail(exitUsage, "%v", errNothingToApply)
	}
	if err := checkLimits(*gfx, *mem, *maxTemp, *unsafe); err != nil {
		return a.fail(exitUsage, "%v", err)
	}
	if err := checkTimes(*hold, *window); err != nil {
		return a.fail(exitUsage, "%v", err)
	}
	lvl, err := a.prepareOffsets(ctx, *level)
	if err != nil {
		return a.fail(exitError, "%v", err)
	}
	a.startInput()
	res, err := a.runStep(ctx, Params{Method: methodOffsets, Level: lvl, Gfx: *gfx, Mem: *mem, MaxTemp: *maxTemp}, *hold, *window)
	code := a.offsetsResult(res, err)
	if code == exitOK {
		fmt.Fprintf(a.out, "kept. The watchdog keeps guarding the temperature (limit %d C). Restore defaults any time: keplergo otval\n", *maxTemp)
	}
	return code
}

func (a *app) cmdApply(ctx context.Context, args []string) int {
	fs := newFlagSet("apply", a.errw)
	mem := fs.Int("mem", 0, "memory clock in MHz (from info)")
	gfx := fs.Int("gfx", 0, "graphics clock in MHz (from info)")
	maxTemp := fs.Int("max-temp", defaultMaxTemp, "restore defaults at this temperature in C")
	hold := fs.Duration("hold", 0, "watch the GPU for this long before asking for confirmation")
	window := fs.Duration("window", defaultWindow, "time you have to confirm")
	if code, done := parseFlags(fs, args); done {
		return code
	}
	if *mem <= 0 || *gfx <= 0 {
		return a.fail(exitUsage, "pass -mem and -gfx, the supported pairs are listed by: keplergo info")
	}
	if err := checkLimits(0, 0, *maxTemp, false); err != nil {
		return a.fail(exitUsage, "%v", err)
	}
	if err := checkTimes(*hold, *window); err != nil {
		return a.fail(exitUsage, "%v", err)
	}
	if err := a.prepareAppClocks(ctx, *mem, *gfx); err != nil {
		return a.fail(exitError, "%v", err)
	}
	a.startInput()
	res, err := a.runStep(ctx, Params{Method: methodAppClocks, Gfx: *gfx, Mem: *mem, MaxTemp: *maxTemp}, *hold, *window)
	code := a.offsetsResult(res, err)
	if code == exitOK {
		fmt.Fprintf(a.out, "kept. The watchdog keeps guarding the temperature (limit %d C). Restore defaults any time: sudo keplergo otval\n", *maxTemp)
	}
	return code
}

func (a *app) cmdTune(ctx context.Context, args []string) int {
	fs := newFlagSet("tune", a.errw)
	level := fs.Int("level", -1, "performance level to adjust (-1 = highest available)")
	step := fs.Int("step", 10, "core offset increase per step in MHz")
	top := fs.Int("max", 100, "highest core offset to try in MHz")
	mem := fs.Int("mem", 0, "fixed memory offset while tuning in MT/s")
	maxTemp := fs.Int("max-temp", defaultMaxTemp, "restore defaults at this temperature in C")
	hold := fs.Duration("hold", 30*time.Second, "how long each step is held under load")
	window := fs.Duration("window", defaultWindow, "time you have to confirm each step")
	unsafe := fs.Bool("unsafe", false, "lift the offset limits")
	if code, done := parseFlags(fs, args); done {
		return code
	}
	if *step < 1 || *step > 50 || *top < *step {
		return a.fail(exitUsage, "-step must be 1..50 and -max must not be below -step")
	}
	if err := checkLimits(*top, *mem, *maxTemp, *unsafe); err != nil {
		return a.fail(exitUsage, "%v", err)
	}
	if err := checkTimes(*hold, *window); err != nil {
		return a.fail(exitUsage, "%v", err)
	}
	lvl, err := a.prepareOffsets(ctx, *level)
	if err != nil {
		return a.fail(exitError, "%v", err)
	}
	a.startInput()
	fmt.Fprintln(a.out, "start a GPU load in another terminal now (glmark2, clpeak or a game)")
	stable, restored := 0, false
	for gfx := *step; gfx <= *top; gfx += *step {
		res, err := a.runStep(ctx, Params{Method: methodOffsets, Level: lvl, Gfx: gfx, Mem: *mem, MaxTemp: *maxTemp}, *hold, *window)
		switch {
		case err != nil:
			return a.fail(exitError, "%v", err)
		case res.Unrestored:
			return exitError
		case res.Interrupted:
			return exitRollback
		case !res.Kept:
			fmt.Fprintf(a.out, "stopped at gfx %+d MHz: %s\n", gfx, res.Reason)
			restored = true
		default:
			stable = gfx
		}
		if restored {
			break
		}
	}
	if !restored && !a.rollback("tune finished") {
		return exitError
	}
	if stable == 0 {
		fmt.Fprintln(a.out, "no stable core offset at this step size")
		return exitOK
	}
	fmt.Fprintf(a.out, "last stable core offset: %+d MHz (mem %+d MT/s), defaults are restored now\n", stable, *mem)
	fmt.Fprintf(a.out, "apply it with: keplergo oc -level %d -gfx %d -mem %d\n", lvl, stable, *mem)
	return exitOK
}
