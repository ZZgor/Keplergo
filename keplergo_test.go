package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

func newTestApp(t *testing.T, run Runner) (*app, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	return &app{
		run: run, out: &out, errw: &out, state: t.TempDir(),
		xenv: xenv{Display: ":0"}, poll: time.Millisecond,
	}, &out
}

// fakeSettings imitates nvidia-settings with per-level offsets.
type fakeSettings struct {
	down    bool // X server unreachable
	ignore  bool // accepts writes but keeps reporting zero
	levels  int  // levels 0..levels-1 exist
	offsets map[string]int
}

var attrRe = regexp.MustCompile(`^\[gpu:0\]/(\w+)\[(\d+)\](?:=(-?\d+))?$`)

func (f *fakeSettings) run(_ context.Context, _ []string, name string, args ...string) ([]byte, error) {
	if name != "nvidia-settings" {
		return nil, fmt.Errorf("unexpected command %s", name)
	}
	if f.down {
		return nil, errors.New("nvidia-settings: Unable to init server")
	}
	if strings.Join(args, " ") == "-q gpus" {
		return []byte("1 GPU on fake:0\n"), nil
	}
	m := attrRe.FindStringSubmatch(args[len(args)-1])
	if m == nil {
		return nil, fmt.Errorf("bad attribute %q", args[len(args)-1])
	}
	var level int
	fmt.Sscan(m[2], &level)
	if level >= f.levels {
		return nil, errors.New("attribute not available")
	}
	key := m[1] + m[2]
	if m[3] != "" {
		if !f.ignore {
			var v int
			fmt.Sscan(m[3], &v)
			f.offsets[key] = v
		}
		return []byte("assigned\n"), nil
	}
	return []byte(fmt.Sprintf("%d\n", f.offsets[key])), nil
}

func TestParseSample(t *testing.T) {
	s, err := parseSample("Quadro K5100M, 62, 705, 1500, [N/A], 12, [Not Supported], 0x0000000000000004\n")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Temp.OK || s.Temp.V != 62 {
		t.Errorf("temp = %+v", s.Temp)
	}
	if s.Power.OK || s.Fan.OK {
		t.Errorf("N/A must stay unavailable, got power %+v fan %+v", s.Power, s.Fan)
	}
	if !s.HasThrottle || s.Throttle != 0x4 {
		t.Errorf("throttle = %#x ok=%v", s.Throttle, s.HasThrottle)
	}
	if !strings.Contains(s.String(), "sw-power-cap") || !strings.Contains(s.String(), "fan n/a") {
		t.Errorf("String() = %q", s.String())
	}
}

func TestParseSampleErrors(t *testing.T) {
	for _, in := range []string{"", "a, b, c"} {
		if _, err := parseSample(in); err == nil {
			t.Errorf("parseSample(%q) must fail", in)
		}
	}
	s, err := parseSample("GPU, 60, 1, 2, 3, 4, 5, [Not Supported]")
	if err != nil || s.HasThrottle {
		t.Errorf("unsupported throttle field: %+v, %v", s, err)
	}
}

func TestVerdict(t *testing.T) {
	cases := []struct {
		name string
		line string
		trip bool
	}{
		{"cool", "G, 60, 1, 2, 3, 4, 5, 0x0", false},
		{"too hot", "G, 85, 1, 2, 3, 4, 5, 0x0", true},
		{"unknown temp", "G, [N/A], 1, 2, 3, 4, 5, 0x0", false},
		{"idle and app clocks are normal", "G, 60, 1, 2, 3, 4, 5, 0x3", false},
		{"power cap is normal", "G, 60, 1, 2, 3, 4, 5, 0x4", false},
		{"hw slowdown", "G, 60, 1, 2, 3, 4, 5, 0x8", true},
		{"hw thermal", "G, 60, 1, 2, 3, 4, 5, 0x40", true},
		{"power brake", "G, 60, 1, 2, 3, 4, 5, 0x80", true},
	}
	for _, c := range cases {
		s, err := parseSample(c.line)
		if err != nil {
			t.Fatal(err)
		}
		if got := verdict(s, 85) != ""; got != c.trip {
			t.Errorf("%s: trip = %v, want %v", c.name, got, c.trip)
		}
	}
}

func TestParseSupportedClocks(t *testing.T) {
	out := "GPU 00000000:01:00.0\n    Supported Clocks\n        Memory                      : 3004 MHz\n            Graphics                : 875 MHz\n            Graphics                : 810 MHz\n        Memory                      : 324 MHz\n            Graphics                : 324 MHz\n"
	m := parseSupportedClocks(out)
	if !supportsPair(m, 3004, 810) || supportsPair(m, 3004, 324) || !supportsPair(m, 324, 324) {
		t.Errorf("unexpected map %v", m)
	}
	if got := parseSupportedClocks("    Supported Clocks                  : N/A\n"); len(got) != 0 {
		t.Errorf("N/A must give no clocks, got %v", got)
	}
}

func TestCheckLimits(t *testing.T) {
	if err := checkLimits(50, 100, 85, false); err != nil {
		t.Errorf("valid values rejected: %v", err)
	}
	if err := checkLimits(151, 0, 85, false); err == nil {
		t.Error("core offset above the limit must fail")
	}
	if err := checkLimits(151, 0, 85, true); err != nil {
		t.Errorf("-unsafe must lift the offset limit: %v", err)
	}
	if err := checkLimits(0, 0, 99, true); err == nil {
		t.Error("the temperature limit can never be lifted")
	}
	if err := checkTimes(0, time.Second); err == nil {
		t.Error("a window below the minimum must fail")
	}
}

func TestSessionRoundTrip(t *testing.T) {
	a, _ := newTestApp(t, nil)
	if s, err := a.loadSession(); s != nil || err != nil {
		t.Fatalf("empty state: %v %v", s, err)
	}
	in := &Session{PID: 42, Method: methodOffsets, Level: 3, Gfx: 50, Mem: 100, MaxTemp: 85, Display: ":1"}
	if err := a.saveSession(in); err != nil {
		t.Fatal(err)
	}
	got, err := a.loadSession()
	if err != nil || got.Gfx != 50 || got.Display != ":1" || !got.confirmed() {
		t.Fatalf("got %+v, %v", got, err)
	}
	if err := a.setDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, _ = a.loadSession(); got.confirmed() {
		t.Error("a deadline in the future means unconfirmed")
	}
	if err := a.clearSession(); err != nil {
		t.Fatal(err)
	}
	if err := a.setDeadline(time.Time{}); err == nil {
		t.Error("setDeadline without a session must fail")
	}
}

func TestSetOffsetsDetectsIgnoredRequest(t *testing.T) {
	f := &fakeSettings{levels: 4, ignore: true, offsets: map[string]int{}}
	a, _ := newTestApp(t, f.run)
	err := a.setOffsets(context.Background(), 3, 50, 100)
	if err == nil || !strings.Contains(err.Error(), "ignored") {
		t.Fatalf("want an ignored-request error, got %v", err)
	}
}

func TestRestoreOffsetsResetsEverything(t *testing.T) {
	f := &fakeSettings{levels: 4, offsets: map[string]int{attrGfx + "3": 50, attrMem + "3": 100, attrGfx + "1": -20}}
	a, _ := newTestApp(t, f.run)
	steps := a.restoreOffsets(context.Background(), &Session{Method: methodOffsets})
	if countFailed(steps) != 0 {
		t.Fatalf("failed steps: %+v", steps)
	}
	for k, v := range f.offsets {
		if v != 0 {
			t.Errorf("%s still %d", k, v)
		}
	}
	if len(steps) != 4 {
		t.Errorf("want one step per level, got %d", len(steps))
	}
}

func TestRestoreOffsetsWhenXIsDown(t *testing.T) {
	f := &fakeSettings{down: true}
	a, _ := newTestApp(t, f.run)
	if steps := a.restoreOffsets(context.Background(), nil); countFailed(steps) != 1 {
		t.Errorf("without a session an unreachable X must count as failed: %+v", steps)
	}
	if steps := a.restoreOffsets(context.Background(), &Session{Method: methodAppClocks}); countFailed(steps) != 0 {
		t.Errorf("an ac session needs no X: %+v", steps)
	}
}

func TestRestoreWithoutCoolbits(t *testing.T) {
	f := &fakeSettings{levels: 0}
	a, _ := newTestApp(t, f.run)
	steps := a.restoreOffsets(context.Background(), nil)
	if len(steps) != 1 || steps[0].Status != stepSkipped {
		t.Errorf("want one skipped step, got %+v", steps)
	}
}

// answerLater delivers lines after the prompt is on screen, like a person typing.
func answerLater(lines ...string) *lineReader {
	ch := make(chan string)
	go func() {
		time.Sleep(30 * time.Millisecond)
		for _, l := range lines {
			ch <- l
		}
		close(ch)
	}()
	return &lineReader{lines: ch}
}

func TestConfirmAnswers(t *testing.T) {
	a, _ := newTestApp(t, nil)
	for _, c := range []struct {
		name string
		in   *lineReader
		want bool
	}{
		{"enter", answerLater(""), true},
		{"yes", answerLater("YES"), true},
		{"no", answerLater("n"), false},
		{"closed input", answerLater(), false},
	} {
		a.in = c.in
		got, err := a.confirm(context.Background(), 2*time.Second)
		if err != nil || got != c.want {
			t.Errorf("%s: got %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}

func TestConfirmIgnoresEarlyEnter(t *testing.T) {
	a, _ := newTestApp(t, nil)
	ch := make(chan string)
	go func() { ch <- "" }() // ENTER typed before the prompt appeared
	time.Sleep(20 * time.Millisecond)
	a.in = &lineReader{lines: ch}
	if got, _ := a.confirm(context.Background(), 60*time.Millisecond); got {
		t.Error("an ENTER typed before the prompt must not confirm")
	}
}

func TestConfirmTimeoutAndCancel(t *testing.T) {
	a, _ := newTestApp(t, nil)
	a.in = &lineReader{lines: make(chan string)}
	if got, _ := a.confirm(context.Background(), 30*time.Millisecond); got {
		t.Error("a timeout must not count as confirmation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.confirm(ctx, time.Second); err == nil {
		t.Error("a cancelled context must be reported")
	}
}

func smiRunner(lines ...string) Runner {
	i := 0
	return func(_ context.Context, _ []string, name string, args ...string) ([]byte, error) {
		l := lines[minInt(i, len(lines)-1)]
		i++
		if l == "ERR" {
			return nil, errors.New("nvidia-smi: Unable to determine the device handle")
		}
		return []byte(l + "\n"), nil
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestMonitorTripsOnTemperature(t *testing.T) {
	a, _ := newTestApp(t, smiRunner("G, 60, 1, 2, 3, 4, 5, 0x0", "G, 90, 1, 2, 3, 4, 5, 0x0"))
	reason, err := a.monitor(context.Background(), 85, time.Minute, false)
	if err != nil || !strings.Contains(reason, "temperature 90 C") {
		t.Fatalf("reason %q, err %v", reason, err)
	}
}

func TestMonitorHealthy(t *testing.T) {
	a, _ := newTestApp(t, smiRunner("G, 60, 1, 2, 3, 4, 5, 0x0"))
	reason, err := a.monitor(context.Background(), 85, 5*time.Millisecond, false)
	if err != nil || reason != "" {
		t.Fatalf("reason %q, err %v", reason, err)
	}
}

func TestMonitorTripsWhenSmiStopsAnswering(t *testing.T) {
	a, _ := newTestApp(t, smiRunner("ERR"))
	reason, err := a.monitor(context.Background(), 85, time.Minute, false)
	if err != nil || !strings.Contains(reason, "failed 3 times") {
		t.Fatalf("reason %q, err %v", reason, err)
	}
}

func TestMonitorStopsOnCancel(t *testing.T) {
	a, _ := newTestApp(t, smiRunner("G, 60, 1, 2, 3, 4, 5, 0x0"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.monitor(ctx, 85, time.Minute, false); err == nil {
		t.Error("a cancelled context must stop the monitor")
	}
}

func TestIsOurWatchdogRejectsStrangers(t *testing.T) {
	for _, pid := range []int{0, -1, 1 << 30} {
		if isOurWatchdog(pid) {
			t.Errorf("pid %d must not look like our watchdog", pid)
		}
	}
}
