package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	attrGfx       = "GPUGraphicsClockOffset"
	attrMem       = "GPUMemoryTransferRateOffset"
	maxPerfLevels = 5
)

const coolbitsHelp = `Coolbits is not active, so the driver exposes no clock offsets. Enable it once, then reboot into an Xorg session:
  sudo mkdir -p /etc/X11/xorg.conf.d
  printf 'Section "Device"\n    Identifier "nvidia"\n    Driver "nvidia"\n    Option "Coolbits" "8"\nEndSection\n' | sudo tee /etc/X11/xorg.conf.d/20-coolbits.conf`

var errWayland = errors.New("clock offsets need an Xorg session: nvidia-settings cannot change clocks under Wayland, pick Xorg on the login screen")

// xenv is the X display nvidia-settings talks to.
type xenv struct {
	Display    string
	XAuthority string
}

func resolveXEnv() xenv {
	e := xenv{Display: os.Getenv("DISPLAY"), XAuthority: os.Getenv("XAUTHORITY")}
	if e.Display == "" {
		e.Display = ":0"
	}
	if e.XAuthority == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path := filepath.Join(home, ".Xauthority")
			if _, err := os.Stat(path); err == nil {
				e.XAuthority = path
			}
		}
	}
	return e
}

func (e xenv) vars() []string {
	v := []string{"DISPLAY=" + e.Display}
	if e.XAuthority != "" {
		v = append(v, "XAUTHORITY="+e.XAuthority)
	}
	return v
}

// sessionType is "x11", "wayland", "tty" or "unknown".
func sessionType() string {
	t := strings.ToLower(os.Getenv("XDG_SESSION_TYPE"))
	switch {
	case t != "":
		return t
	case os.Getenv("WAYLAND_DISPLAY") != "":
		return "wayland"
	case os.Getenv("DISPLAY") != "":
		return "x11"
	}
	return "unknown"
}

func (a *app) attr(name string, level int) string {
	return fmt.Sprintf("[gpu:%d]/%s[%d]", a.gpu, name, level)
}

func (a *app) xReachable(ctx context.Context) error {
	_, err := a.settings(ctx, "-q", "gpus")
	return err
}

func (a *app) getOffset(ctx context.Context, name string, level int) (int, error) {
	out, err := a.settings(ctx, "-t", "-q", a.attr(name, level))
	if err != nil {
		return 0, err
	}
	text := firstLine(string(out))
	v, convErr := strconv.Atoi(text)
	if convErr != nil {
		return 0, fmt.Errorf("unexpected nvidia-settings output %q", clip(text, 60))
	}
	return v, nil
}

func (a *app) setOffset(ctx context.Context, name string, level, v int) error {
	_, err := a.settings(ctx, "-a", fmt.Sprintf("%s=%d", a.attr(name, level), v))
	return err
}

// setOffsets writes both offsets and reads them back. A value that stays at
// zero means the driver or VBIOS ignored the request. A different non-zero
// value is only reported: drivers may round or clamp.
func (a *app) setOffsets(ctx context.Context, level, gfx, mem int) error {
	if err := a.setOffset(ctx, attrGfx, level, gfx); err != nil {
		return err
	}
	if err := a.setOffset(ctx, attrMem, level, mem); err != nil {
		return err
	}
	for _, c := range []struct {
		label, name string
		want        int
	}{{"gfx", attrGfx, gfx}, {"mem", attrMem, mem}} {
		got, err := a.getOffset(ctx, c.name, level)
		if err != nil {
			return err
		}
		switch {
		case got == c.want:
		case c.want != 0 && got == 0:
			return fmt.Errorf("the driver ignored the %s offset %+d on level %d (VBIOS may block overclocking)", c.label, c.want, level)
		default:
			fmt.Fprintf(a.errw, "note: %s offset reads back %+d instead of %+d\n", c.label, got, c.want)
		}
	}
	return nil
}

// detectLevels lists the performance levels that accept a core offset.
func (a *app) detectLevels(ctx context.Context) []int {
	var levels []int
	for l := 0; l < maxPerfLevels; l++ {
		if _, err := a.getOffset(ctx, attrGfx, l); err == nil {
			levels = append(levels, l)
		}
	}
	return levels
}

// prepareOffsets checks that offsets can work here and picks the level to change.
func (a *app) prepareOffsets(ctx context.Context, level int) (int, error) {
	if sessionType() == "wayland" {
		return 0, errWayland
	}
	if err := a.xReachable(ctx); err != nil {
		return 0, fmt.Errorf("nvidia-settings cannot reach the X server (DISPLAY=%s): %w", a.xenv.Display, err)
	}
	levels := a.detectLevels(ctx)
	if len(levels) == 0 {
		return 0, errors.New(coolbitsHelp)
	}
	if level < 0 {
		return levels[len(levels)-1], nil
	}
	for _, l := range levels {
		if l == level {
			return level, nil
		}
	}
	return 0, fmt.Errorf("performance level %d has no offset control (available: %v)", level, levels)
}

func (a *app) prepareAppClocks(ctx context.Context, mem, gfx int) error {
	if os.Geteuid() != 0 {
		return errors.New("application clocks need root: run with sudo")
	}
	out, err := a.smi(ctx, "-q", "-d", "SUPPORTED_CLOCKS")
	if err != nil {
		return err
	}
	clocks := parseSupportedClocks(string(out))
	if len(clocks) == 0 {
		return errors.New("this GPU does not support application clocks (typical for mobile Quadro): use oc instead")
	}
	if !supportsPair(clocks, mem, gfx) {
		return fmt.Errorf("mem %d MHz with gfx %d MHz is not a supported pair, supported:\n%s", mem, gfx, formatClocks(clocks))
	}
	return nil
}
