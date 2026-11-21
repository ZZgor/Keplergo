package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

const (
	sampleFields  = "name,temperature.gpu,clocks.sm,clocks.mem,power.draw,utilization.gpu,fan.speed,clocks_throttle_reasons.active"
	sampleColumns = 8
	maskIdle      = 0x1
)

// Num is a reading that may be missing: Kepler mobile cards report many
// fields as [N/A] or [Not Supported], and that must never look like zero.
type Num struct {
	V  float64
	OK bool
}

type Sample struct {
	Name        string
	Temp        Num
	SM          Num
	Mem         Num
	Power       Num
	Util        Num
	Fan         Num
	Throttle    uint64
	HasThrottle bool
}

type throttleBit struct {
	mask uint64
	name string
	trip bool // a reason that makes the guard restore defaults
}

var throttleBits = []throttleBit{
	{mask: 0x1, name: "idle"},
	{mask: 0x2, name: "app-clocks"},
	{mask: 0x4, name: "sw-power-cap"},
	{mask: 0x8, name: "hw-slowdown", trip: true},
	{mask: 0x10, name: "sync-boost"},
	{mask: 0x20, name: "sw-thermal", trip: true},
	{mask: 0x40, name: "hw-thermal", trip: true},
	{mask: 0x80, name: "hw-power-brake", trip: true},
	{mask: 0x100, name: "display-clocks"},
}

func throttleNames(mask uint64, tripOnly bool) []string {
	var names []string
	for _, b := range throttleBits {
		if mask&b.mask != 0 && (!tripOnly || b.trip) {
			names = append(names, b.name)
		}
	}
	return names
}

func parseNum(s string) Num {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return Num{}
	}
	return Num{V: v, OK: true}
}

func parseSample(text string) (Sample, error) {
	line := firstLine(text)
	if line == "" {
		return Sample{}, fmt.Errorf("empty nvidia-smi output")
	}
	f := strings.Split(line, ",")
	if len(f) != sampleColumns {
		return Sample{}, fmt.Errorf("expected %d fields, got %d in %q", sampleColumns, len(f), clip(line, 120))
	}
	s := Sample{
		Name:  strings.TrimSpace(f[0]),
		Temp:  parseNum(f[1]),
		SM:    parseNum(f[2]),
		Mem:   parseNum(f[3]),
		Power: parseNum(f[4]),
		Util:  parseNum(f[5]),
		Fan:   parseNum(f[6]),
	}
	if mask, err := strconv.ParseUint(strings.TrimSpace(f[7]), 0, 64); err == nil {
		s.Throttle, s.HasThrottle = mask, true
	}
	return s, nil
}

func fmtNum(n Num, format string) string {
	if !n.OK {
		return "n/a"
	}
	return fmt.Sprintf(format, n.V)
}

func (s Sample) String() string {
	line := fmt.Sprintf("%s | sm %s | mem %s | %s | util %s | fan %s",
		fmtNum(s.Temp, "%.0f C"), fmtNum(s.SM, "%.0f MHz"), fmtNum(s.Mem, "%.0f MHz"),
		fmtNum(s.Power, "%.1f W"), fmtNum(s.Util, "%.0f%%"), fmtNum(s.Fan, "%.0f%%"))
	if s.HasThrottle {
		if names := throttleNames(s.Throttle&^maskIdle, false); len(names) > 0 {
			line += " | " + strings.Join(names, ",")
		}
	}
	return line
}

// verdict returns why the overclock must be undone, or "" while the GPU is healthy.
// Power cap and the idle/app-clock flags are normal and never trip the guard.
func verdict(s Sample, maxTemp int) string {
	if s.Temp.OK && s.Temp.V >= float64(maxTemp) {
		return fmt.Sprintf("temperature %.0f C reached the limit of %d C", s.Temp.V, maxTemp)
	}
	if s.HasThrottle {
		if names := throttleNames(s.Throttle, true); len(names) > 0 {
			return "throttling: " + strings.Join(names, ", ")
		}
	}
	return ""
}

func (a *app) sample(ctx context.Context) (Sample, error) {
	out, err := a.smi(ctx, "--query-gpu="+sampleFields, "--format=csv,noheader,nounits")
	if err != nil {
		return Sample{}, err
	}
	return parseSample(string(out))
}

// queryField reads one field on its own, so a field missing in this driver
// version only blanks that row instead of failing the whole query.
func (a *app) queryField(ctx context.Context, field string) (string, error) {
	out, err := a.smi(ctx, "--query-gpu="+field, "--format=csv,noheader,nounits")
	if err != nil {
		return "", err
	}
	v := firstLine(string(out))
	if v == "" || strings.HasPrefix(v, "[") || strings.EqualFold(v, "n/a") {
		return "n/a", nil
	}
	return v, nil
}

// parseSupportedClocks reads "nvidia-smi -q -d SUPPORTED_CLOCKS" into memory -> core clocks.
// A GPU without application clocks prints N/A and yields an empty map.
func parseSupportedClocks(out string) map[int][]int {
	res := map[int][]int{}
	mem := 0
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(val)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Memory":
			mem = n
			res[mem] = nil
		case "Graphics":
			if mem != 0 {
				res[mem] = append(res[mem], n)
			}
		}
	}
	return res
}

func supportsPair(m map[int][]int, mem, gfx int) bool {
	for _, g := range m[mem] {
		if g == gfx {
			return true
		}
	}
	return false
}

func formatClocks(m map[int][]int) string {
	mems := make([]int, 0, len(m))
	for k := range m {
		mems = append(mems, k)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(mems)))
	var b strings.Builder
	for _, k := range mems {
		fmt.Fprintf(&b, "  mem %d MHz: gfx", k)
		for _, g := range m[k] {
			fmt.Fprintf(&b, " %d", g)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
