package media

import (
	"strings"
	"testing"
)

func TestScanProgressCallsOncePerBlock(t *testing.T) {
	stream := strings.NewReader(
		"frame=1\nout_time_us=1000000\nspeed=0.5x\nprogress=continue\n" +
			"frame=2\nout_time_us=2000000\nspeed=1.2x\nprogress=continue\n" +
			"frame=3\nout_time_us=3000000\nspeed=1.0x\nprogress=end\n",
	)

	type update struct {
		us    int64
		speed float64
	}
	var updates []update
	scanProgress(stream, func(us int64, speed float64) {
		updates = append(updates, update{us, speed})
	})

	if len(updates) != 3 {
		t.Fatalf("got %d updates, want 3: %+v", len(updates), updates)
	}
	if updates[0].us != 1000000 || updates[0].speed != 0.5 {
		t.Errorf("update[0] = %+v, want {1000000 0.5}", updates[0])
	}
	if updates[2].us != 3000000 || updates[2].speed != 1.0 {
		t.Errorf("update[2] = %+v, want {3000000 1.0}", updates[2])
	}
}

func TestScanProgressStopsAtEnd(t *testing.T) {
	stream := strings.NewReader(
		"out_time_us=1000000\nprogress=end\n" +
			"out_time_us=99999999\nprogress=continue\n", // must not be seen
	)
	var calls int
	var lastUS int64
	scanProgress(stream, func(us int64, speed float64) {
		calls++
		lastUS = us
	})
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (scanProgress should return after progress=end)", calls)
	}
	if lastUS != 1000000 {
		t.Errorf("lastUS = %d, want 1000000", lastUS)
	}
}

func TestScanProgressDefaultsBeforeFirstReport(t *testing.T) {
	stream := strings.NewReader("progress=continue\n")
	var us int64 = -99
	var speed float64 = -99
	scanProgress(stream, func(u int64, s float64) { us, speed = u, s })
	if us != -1 {
		t.Errorf("us = %d, want -1 (no out_time_us reported yet)", us)
	}
	if speed != 0 {
		t.Errorf("speed = %v, want 0", speed)
	}
}

func TestLimitedBufferKeepsOnlyTail(t *testing.T) {
	var b limitedBuffer
	b.buf.Grow(0)
	long := strings.Repeat("a", stderrTailBytes+500)
	if _, err := b.Write([]byte(long)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := b.String(); len(got) > stderrTailBytes {
		t.Errorf("limitedBuffer kept %d bytes, want <= %d", len(got), stderrTailBytes)
	}
}

func TestParseFrameRate(t *testing.T) {
	cases := map[string]float64{
		"30/1":       30,
		"30000/1001": 29.97002997002997,
		"0/0":        0,
		"":           0,
		"25":         25,
	}
	for in, want := range cases {
		got := parseFrameRate(in)
		if diff := got - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("parseFrameRate(%q) = %v, want %v", in, got, want)
		}
	}
}
