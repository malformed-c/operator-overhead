// Package procsample reads memory and CPU for a set of processes out of /proc.
//
// ***IT READS /proc AND NOT A CGROUP, AND THAT IS THE WHOLE REASON THE PACKAGE
// EXISTS.*** systemd's `MemoryCurrent` is the cgroup's charged memory — page
// cache, kernel allocations and all — and it is not RSS. The two are different
// quantities and substituting one for the other is not a rounding error. So when
// this benchmark says RSS it means `VmRSS` from `/proc/<pid>/status`, and it says
// so at the point of measurement rather than in a footnote.
//
// PSS IS REPORTED BESIDE IT, BECAUSE AT N=64 RSS IS THE WRONG SUM. Sixty-four
// copies of one binary share their text pages; adding sixty-four VmRSS values
// counts those pages sixty-four times and overstates the arm. `Pss` from
// `/proc/<pid>/smaps_rollup` divides each shared page by its sharer count, so
// the PSS sum is the honest fleet figure and the RSS sum is the honest
// per-process one. Reporting only one of them would flatter whichever arm the
// author preferred.
package procsample

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Sample is one process, at one instant.
type Sample struct {
	PID int `json:"pid"`
	// Cmdline is kept so a sample can be audited after the fact. A row nobody
	// can attribute to a process is the shape ADR-0098 refuses.
	Cmdline string `json:"cmdline"`
	// RSSBytes is VmRSS. NAMED, not "memory".
	RSSBytes uint64 `json:"rssBytes"`
	// PeakRSSBytes is VmHWM — the high-water mark since the process started.
	//
	// ***IT IS WHAT DISTINGUISHES "SMALL NOW" FROM "NEVER BIG".*** A point RSS
	// sample cannot tell a process that has always been 24 MiB from one that
	// touched 68 MiB and released it, and those are different things to budget
	// for. Measured on this benchmark: five launches of one component all showed
	// VmHWM within 0.6 MiB of RSS, which is what ruled out "it grew and shrank"
	// as the explanation for two much larger readings elsewhere.
	PeakRSSBytes uint64 `json:"peakRssBytes"`
	// PSSBytes is Pss from smaps_rollup, or 0 when the kernel would not serve
	// it (a kernel without smaps_rollup, or a process that exited mid-read).
	PSSBytes uint64 `json:"pssBytes"`
	// PSSKnown distinguishes "shared-page accounting says zero" from "the read
	// did not happen". A zero that means both is the shape ADR-0098's protocol
	// step 4 names: "a zero means either no events or a broken instrument".
	PSSKnown bool `json:"pssKnown"`
	// CPUTicks is utime+stime in clock ticks. A DELTA between two samples is
	// the CPU figure; an absolute value here is meaningless on its own and is
	// kept only so the delta can be computed by the caller.
	CPUTicks uint64 `json:"cpuTicks"`
}

// Set is every process matching one selector at one instant.
type Set struct {
	Match   string   `json:"match"`
	Samples []Sample `json:"samples"`
}

// Totals sums a set. RSS and PSS are summed separately for the reason in the
// package doc; CPU ticks sum because CPU time is genuinely additive.
func (s Set) Totals() (rss, pss, ticks uint64, pssComplete bool) {
	pssComplete = true
	for _, p := range s.Samples {
		rss += p.RSSBytes
		ticks += p.CPUTicks
		if p.PSSKnown {
			pss += p.PSSBytes
		} else {
			pssComplete = false
		}
	}

	return rss, pss, ticks, pssComplete
}

// ClockTicks is the kernel's USER_HZ. Hard-coded at 100 because that is what
// every Linux/x86_64 kernel this benchmark can run on uses, and because reading
// it properly needs cgo (`sysconf(_SC_CLK_TCK)`) which would make this binary
// dynamically linked — and `apsis ingest` packs a FROM-scratch image with no
// loader in it.
//
// A wrong value here scales every CPU number by a constant, identically in both
// arms, so it cannot change which arm wins. It would still make the absolute
// milliseconds wrong, which is why it is named rather than inlined.
const ClockTicks = 100

// Collect samples every process whose /proc/<pid>/cmdline contains match and
// none of reject.
//
// SUBSTRING RATHER THAN A PID LIST, because the processes being measured are
// pods: they are created by a controller, they get recycled, and a PID captured
// at setup is stale by the time a window closes. The selector is the stable
// thing, so `bench` passes the same string to every sample in a run and a
// restarted pod stays inside the population.
//
// ═══════════════════════════════════════════════════════════════════════════
// ***`reject` IS NOT A CONVENIENCE. A POD'S SUPERVISOR CARRIES THE GUEST'S OWN
// ARGV, SO EVERY POD MATCHES TWICE.*** Measured on engix99 at N=1:
//
//	2348627  systemd-nspawn … --setenv=… -- /usr/local/bin/meteor /crrelay -id=000 …
//	2348632  /crrelay -id=000 -namespace=overhead -leader-election=false …
//
// The supervisor's command line ENDS with the exec target, so any substring
// that identifies the workload identifies its supervisor too. Without the
// rejection a sampler reports two processes per instance and adds ~2 MB of
// `systemd-nspawn` to every arm's RSS — a constant per-instance overstatement
// that grows linearly with N and looks exactly like a real result.
//
// The population control (`Found != N`) is what makes that visible, which is the
// argument for having the control at all.
// ═══════════════════════════════════════════════════════════════════════════
func Collect(match string, reject ...string) (Set, error) {
	if match == "" {
		return Set{}, errors.New("procsample: an empty match selects every process on the host")
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return Set{}, fmt.Errorf("procsample: read /proc: %w", err)
	}

	out := Set{Match: match}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a process directory
		}
		cmd, err := cmdline(pid)
		if err != nil || !strings.Contains(cmd, match) {
			// A process that exited between ReadDir and here is not an error:
			// it is a process that is not in the population any more.
			continue
		}
		if rejected(cmd, reject) {
			continue
		}
		s, err := sample(pid, cmd)
		if err != nil {
			continue
		}
		out.Samples = append(out.Samples, s)
	}

	return out, nil
}

func rejected(cmd string, reject []string) bool {
	for _, r := range reject {
		if r != "" && strings.Contains(cmd, r) {
			return true
		}
	}

	return false
}

func cmdline(pid int) (string, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return "", err
	}

	return strings.ReplaceAll(string(raw), "\x00", " "), nil
}

func sample(pid int, cmd string) (Sample, error) {
	s := Sample{PID: pid, Cmdline: strings.TrimSpace(cmd)}

	status, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return s, err
	}
	s.RSSBytes, err = ParseStatusVmRSS(string(status))
	if err != nil {
		return s, err
	}
	// VmHWM is absent for a kernel thread and for a process that has not faulted
	// anything in; zero there is honest, so this does not fail the sample.
	s.PeakRSSBytes, _ = parseKBLine(string(status), "VmHWM:")

	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return s, err
	}
	s.CPUTicks, err = ParseStatCPUTicks(string(stat))
	if err != nil {
		return s, err
	}

	// smaps_rollup is optional and its absence is recorded rather than
	// substituted. See Sample.PSSKnown.
	if rollup, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "smaps_rollup")); err == nil {
		if pss, err := ParseSmapsRollupPSS(string(rollup)); err == nil {
			s.PSSBytes, s.PSSKnown = pss, true
		}
	}

	return s, nil
}

// ParseStatusVmRSS pulls VmRSS out of /proc/<pid>/status and returns BYTES.
//
// The file reports kB. Converting here rather than at the call site is what
// keeps a unit from being a comment: every field on Sample is bytes.
func ParseStatusVmRSS(status string) (uint64, error) {
	return parseKBLine(status, "VmRSS:")
}

// ParseSmapsRollupPSS pulls Pss out of /proc/<pid>/smaps_rollup, in BYTES.
func ParseSmapsRollupPSS(rollup string) (uint64, error) {
	return parseKBLine(rollup, "Pss:")
}

func parseKBLine(text, prefix string) (uint64, error) {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("procsample: %q line has no value: %q", prefix, line)
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("procsample: %q value %q: %w", prefix, fields[1], err)
		}

		return kb * 1024, nil
	}

	return 0, fmt.Errorf("procsample: no %q line", prefix)
}

// ParseStatCPUTicks returns utime+stime from /proc/<pid>/stat.
//
// ***THE COMM FIELD IS PARENTHESISED AND MAY CONTAIN SPACES AND PARENTHESES,
// WHICH IS WHY THIS DOES NOT strings.Fields THE WHOLE LINE.*** A process named
// `(foo bar)` shifts every subsequent index, so a naive split reads the wrong
// two numbers and reports a plausible CPU time for the wrong fields. The last
// ')' is the documented delimiter — proc(5) says so — so the scan is from the
// right.
func ParseStatCPUTicks(stat string) (uint64, error) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, errors.New("procsample: /proc/<pid>/stat has no comm field")
	}
	// After ") " the fields are state(3), ppid(4), ... utime(14), stime(15),
	// 1-indexed as in proc(5). In the remainder they are 0-indexed from state,
	// so utime is index 11 and stime index 12.
	rest := strings.Fields(stat[end+1:])
	const utimeIdx, stimeIdx = 11, 12
	if len(rest) <= stimeIdx {
		return 0, fmt.Errorf("procsample: /proc/<pid>/stat has %d fields after comm, need %d",
			len(rest), stimeIdx+1)
	}
	utime, err := strconv.ParseUint(rest[utimeIdx], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("procsample: utime %q: %w", rest[utimeIdx], err)
	}
	stime, err := strconv.ParseUint(rest[stimeIdx], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("procsample: stime %q: %w", rest[stimeIdx], err)
	}

	return utime + stime, nil
}

// TicksToMillis converts a CPU-tick delta to milliseconds.
func TicksToMillis(ticks uint64) float64 {
	return float64(ticks) * 1000 / ClockTicks
}
