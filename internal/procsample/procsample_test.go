package procsample

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The comm field is the parser's whole hazard: it is parenthesised, it may
// contain spaces AND parentheses, and a naive strings.Fields over the line reads
// two plausible numbers out of the wrong columns. Every case here is one that
// shifts the index.
func TestParseStatCPUTicks(t *testing.T) {
	// The fields FOLLOWING state, so this slice starts at proc(5)'s field 4
	// (ppid). utime is 14 and stime 15, which land at offsets 10 and 11 here —
	// one less than their offsets in the parser, which counts from state.
	after := func(utime, stime string) string {
		f := make([]string, 22)
		for i := range f {
			f[i] = "0"
		}
		f[10], f[11] = utime, stime

		return strings.Join(f, " ")
	}

	cases := []struct {
		name string
		stat string
		want uint64
	}{
		{"plain comm", "42 (crrelay) S " + after("100", "23"), 123},
		{"comm with a space", "42 (crrelay -id 7) S " + after("100", "23"), 123},
		{"comm with parentheses", "42 (weird)name) S " + after("1", "1"), 2},
		{"zero cpu is a measurement", "42 (idle) S " + after("0", "0"), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseStatCPUTicks(c.stat)
			if err != nil {
				t.Fatalf("ParseStatCPUTicks: %v", err)
			}
			if got != c.want {
				t.Errorf("ticks = %d, want %d", got, c.want)
			}
		})
	}
}

// ***THE FIXTURES ABOVE CANNOT CATCH AN OFF-BY-ONE, BECAUSE THEY WERE BUILT TO
// MATCH THE PARSER.*** They pin the comm hazard and nothing else: shift both the
// parser and the helper by one column and every case above still passes while
// the benchmark reports `cmajflt` as CPU time. So this reads the REAL
// /proc/self/stat and checks it against getrusage, which reaches the same two
// kernel counters by an unrelated path.
func TestParseStatCPUTicksAgreesWithGetrusage(t *testing.T) {
	// getrusage has microsecond resolution and stat has 10ms ticks, so the
	// comparison needs enough CPU burned that a tick is not the whole signal.
	deadline := time.Now().Add(150 * time.Millisecond)
	for x := 0; time.Now().Before(deadline); x++ {
		_ = x * x
	}

	raw, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Skipf("no /proc/self/stat: %v", err)
	}
	ticks, err := ParseStatCPUTicks(string(raw))
	if err != nil {
		t.Fatalf("ParseStatCPUTicks on real /proc/self/stat: %v", err)
	}

	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Skipf("getrusage: %v", err)
	}
	oracle := float64(ru.Utime.Sec)*1000 + float64(ru.Utime.Usec)/1000 +
		float64(ru.Stime.Sec)*1000 + float64(ru.Stime.Usec)/1000
	got := TicksToMillis(ticks)

	// One tick of slack each way for the rounding, plus a tick for the time
	// between the two reads. Wide enough not to flake, far narrower than any
	// neighbouring field: `cmajflt` is 0 and `vsize` is ~10^9.
	const slackMS = 30
	if got < oracle-slackMS || got > oracle+slackMS {
		t.Errorf("stat says %.0f ms of CPU, getrusage says %.0f ms — the utime/stime "+
			"columns are wrong, not merely imprecise", got, oracle)
	}
	if got == 0 {
		t.Error("burned 150ms of CPU and stat reported none")
	}
}

func TestParseStatCPUTicksRefusesTruncated(t *testing.T) {
	// A short line must be an ERROR and not zero. Zero would read as "this
	// process used no CPU", which is a measurement; a truncated read is not.
	if _, err := ParseStatCPUTicks("42 (crrelay) S 1 2 3"); err == nil {
		t.Fatal("a stat line with too few fields must not parse as zero CPU")
	}
	if _, err := ParseStatCPUTicks("no comm field here"); err == nil {
		t.Fatal("a line with no comm must not parse")
	}
}

func TestParseStatusVmRSSIsBytes(t *testing.T) {
	const status = "Name:\tcrrelay\nVmPeak:\t 1234 kB\nVmRSS:\t   28160 kB\nThreads:\t9\n"
	got, err := ParseStatusVmRSS(status)
	if err != nil {
		t.Fatalf("ParseStatusVmRSS: %v", err)
	}
	// The file says kB and every field on Sample is bytes; a unit that survives
	// only as a comment is the defect this asserts against.
	if want := uint64(28160 * 1024); got != want {
		t.Errorf("VmRSS = %d bytes, want %d", got, want)
	}
}

// VmRSS is absent for a kernel thread. That must be an error rather than zero,
// for the same reason a truncated stat line is.
func TestParseStatusVmRSSAbsent(t *testing.T) {
	if _, err := ParseStatusVmRSS("Name:\tkthreadd\nThreads:\t1\n"); err == nil {
		t.Fatal("a status with no VmRSS must not parse as zero")
	}
}

func TestParseSmapsRollupPSS(t *testing.T) {
	const rollup = "55a1b2c00000-7ffd0 ---p 00000000 00:00 0 [rollup]\n" +
		"Rss:               28160 kB\n" +
		"Pss:                9216 kB\n" +
		"Pss_Anon:           8000 kB\n"
	got, err := ParseSmapsRollupPSS(rollup)
	if err != nil {
		t.Fatalf("ParseSmapsRollupPSS: %v", err)
	}
	// `Pss_Anon` also starts with "Pss" — the prefix match must be on "Pss:"
	// and hit the first line, not the anon breakdown.
	if want := uint64(9216 * 1024); got != want {
		t.Errorf("Pss = %d bytes, want %d", got, want)
	}
}

func TestTotalsFlagsIncompletePSS(t *testing.T) {
	s := Set{Samples: []Sample{
		{RSSBytes: 100, PSSBytes: 40, PSSKnown: true, CPUTicks: 3},
		{RSSBytes: 100, CPUTicks: 4}, // no smaps_rollup
	}}
	rss, pss, ticks, complete := s.Totals()
	if rss != 200 || ticks != 7 {
		t.Errorf("rss=%d ticks=%d, want 200 and 7", rss, ticks)
	}
	if pss != 40 {
		t.Errorf("pss=%d, want 40 — an unknown must not contribute zero silently", pss)
	}
	// The flag is the point: a PSS sum missing a process is not a smaller PSS,
	// it is a number nobody may compare across arms.
	if complete {
		t.Error("Totals reported a complete PSS over a sample that had none")
	}
}

// Collect must find this test binary by its own cmdline, and must refuse an
// empty selector. The first is the positive control ADR-0098 step 3 asks for —
// proof the instrument sees a series it should see — and without it a zero from
// Collect is indistinguishable from a broken scan.
func TestCollectSeesItself(t *testing.T) {
	if _, err := Collect(""); err == nil {
		t.Fatal("an empty match selects every process on the host and must be refused")
	}

	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot name this executable: %v", err)
	}
	set, err := Collect(self)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	me := strconv.Itoa(os.Getpid())
	found := false
	for _, s := range set.Samples {
		if strconv.Itoa(s.PID) == me {
			found = true
			if s.RSSBytes == 0 {
				t.Error("this process reported zero RSS")
			}
		}
	}
	if !found {
		t.Fatalf("Collect(%q) did not find this process (pid %s)", self, me)
	}

	// The negative control: a selector nothing can match must return an empty
	// set and no error, so an empty result is readable as "nothing matched"
	// rather than "the scan failed".
	none, err := Collect("operator-overhead-selector-that-matches-nothing")
	if err != nil {
		t.Fatalf("Collect on an unmatched selector: %v", err)
	}
	if len(none.Samples) != 0 {
		t.Errorf("unmatched selector returned %d samples", len(none.Samples))
	}
}
