package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"net/netip"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/malformed-c/operator-overhead/internal/apiload"
	"github.com/malformed-c/operator-overhead/internal/procsample"
	"github.com/malformed-c/operator-overhead/internal/relay"
)

// Spec is one measured run: one arm, one N, one pair of windows.
type Spec struct {
	Arm    string        `json:"arm"`
	N      int           `json:"n"`
	Settle time.Duration `json:"settle"`
	Idle   time.Duration `json:"idle"`
	Change time.Duration `json:"change"`
	Period time.Duration `json:"period"`
	// ConvergeTimeout bounds the effect-level control. Generous on purpose: a
	// Perseid's first pass waits on radiant's tick, so a bound tight enough to
	// be snappy for arm A would fail arm B for being asynchronous rather than
	// for being broken.
	ConvergeTimeout time.Duration `json:"convergeTimeout"`
	// RadiantMetrics is the shared host's /metrics URL, for arm B only. Empty
	// means the column is NOT MEASURED and says so rather than printing zero.
	RadiantMetrics string `json:"radiantMetrics,omitempty"`
	// APIServer is the apiserver's real host:port, beside the ClusterIP. Some
	// clients (radiant) dial it directly.
	APIServer string `json:"apiServer,omitempty"`
	Host      string `json:"host"`
	Image     string `json:"image,omitempty"`
	Kubecfg   string `json:"-"`
}

// apiEndpoints is every address a client in this cluster may reach the apiserver
// by. See the block comment in procsample/conns.go: matching only the ClusterIP
// reported radiant, which holds watches, as holding nothing.
func (s Spec) apiEndpoints() []netip.AddrPort {
	out := []netip.AddrPort{procsample.APIServerService}
	if s.APIServer != "" {
		if ap, err := netip.ParseAddrPort(s.APIServer); err == nil {
			out = append(out, ap)
		}
	}

	return out
}

// Preflight is the set of controls ADR-0098's protocol step 3 requires, taken
// BEFORE the first window opens.
//
// ***A RUN WHOSE CONTROLS FAILED IS NOT A RUN WITH BAD NUMBERS, IT IS NOT A
// RUN.*** Every field here exists so a zero later in the record can be read as
// a measurement instead of as a broken instrument, and `Preflight.Err` is
// non-nil whenever that reading would not be safe.
type Preflight struct {
	// Selector is the /proc cmdline substring this run's population is defined
	// by. Recorded verbatim so a sample can be re-taken later against the same
	// definition.
	Selector string `json:"selector"`
	// Found is how many processes matched. POSITIVE CONTROL: it must equal N,
	// and a run where it does not is measuring a population nobody described.
	Found int `json:"found"`
	// Excluded is how many processes matched the OTHER arms' selectors. NEGATIVE
	// CONTROL: proof the filter excludes an unrelated subject. A benchmark whose
	// selector caught the neighbouring arm would report one arm's memory twice
	// and nothing in the numbers would look wrong.
	Excluded int `json:"excluded"`
	// Converged records that the arm was OBSERVED relaying a value before any
	// window opened, and how long it took. THE ONLY EFFECT-LEVEL CONTROL here;
	// every other one asserts a marker. See ProveConvergence.
	Converged       bool    `json:"converged"`
	ConvergeSeconds float64 `json:"convergeSeconds"`
	// Placement is each arm-B pod's node. Recorded because arm B cannot be
	// pinned — see PodPlacement — so WHICH node an instance ran on is the column
	// that separates a per-node effect from version skew.
	Placement map[string]string `json:"placement,omitempty"`
	// ConfigDelivered records whether arm B's pods were built by a runtime that
	// passes `spec.config` through to the guest. See bench.EnvAnnotation — an
	// empty guest environment and a pod predating the fix are indistinguishable
	// without it.
	ConfigDelivered bool `json:"configDelivered"`
	// MetricsEndpoints is how many of the arm's own /metrics endpoints answered
	// with the client counter present. For a controller-runtime arm this must
	// equal N; see apiload.Reading.Found for why an absent family is not zero.
	MetricsEndpoints int `json:"metricsEndpoints"`
	// APITotals is the arm's CUMULATIVE request count by method, since its
	// processes started — not a delta.
	//
	// ***A STEADY-STATE WINDOW CANNOT SEE A WATCH.*** The LIST that primes an
	// informer and the WATCH that keeps it primed both happen at startup, so a
	// delta over any later window reports them as ZERO — while the connection
	// they opened stays held for the life of the process. Measured at N=1, a
	// 45 s change window showed `PATCH=44` and nothing else.
	//
	// So a cache's per-instance cost is CUMULATIVE, not a rate. Reporting only
	// the window says an idle controller-runtime arm is free, which is true of
	// its request rate and false of what it holds open.
	APITotals map[string]float64 `json:"apiTotals,omitempty"`

	// Commit is the benchmark's own source revision, and Periapsis' — protocol
	// step 0, "write down the population, binary and commit".
	Commit         string `json:"commit,omitempty"`
	PeriapsisImage string `json:"periapsisImage,omitempty"`

	// TrailVersion and RadiantVersion are the RUNTIME's commits, read from the
	// binary and from the running pod's log.
	//
	// ***WITHOUT THESE A RECORD CANNOT SAY WHAT IT RAN ON.*** The runtimes under
	// this benchmark roll several times a day, so an unexplained number cannot be
	// correlated against them afterwards — "write down the binary and the commit"
	// is ADR-0098's first protocol step.
	//
	// Read from the ARTIFACT (`trail --capabilities`) and from the running pod,
	// never from a deploy log: a deploy record can be wrong by weeks when a binary
	// was installed outside the pipeline.
	TrailVersion   string `json:"trailVersion,omitempty"`
	RadiantVersion string `json:"radiantVersion,omitempty"`
	Err            string `json:"error,omitempty"`
}

// Window is one measured interval.
type Window struct {
	Name    string  `json:"name"`
	Seconds float64 `json:"seconds"`

	// Processes is the population at the CLOSE of the window. It is reported
	// beside every total because a total over a population that shrank
	// mid-window is not comparable to one that did not.
	Processes int `json:"processes"`

	// RSSBytes is the sum of VmRSS at the close. NAMED, not "memory", because a
	// cgroup's charged figure is a different quantity wearing the same word.
	RSSBytes uint64 `json:"rssBytes"`
	// PSSBytes divides shared pages by their sharer count, which is the honest
	// sum when N copies of one binary share their text. Zero and PSSComplete
	// false means the kernel would not serve it, NOT that pages are unshared.
	PSSBytes    uint64 `json:"pssBytes"`
	PSSComplete bool   `json:"pssComplete"`

	// CPUms is a DELTA across the window, from utime+stime.
	CPUms float64 `json:"cpuMs"`

	// APIRequests is the sum over the arm's own client-side counters, as a
	// delta. Attribution is by construction; see package apiload.
	APIRequests float64 `json:"apiRequests"`
	// APIMeasured distinguishes "this arm made no requests" from "nothing was
	// scraped". Without it the two print identically, as 0.
	APIMeasured bool `json:"apiMeasured"`
	// APIByMethod is the split that carries the argument: LIST and WATCH are
	// what a cache costs, PUT is what leader election costs, PATCH is the work.
	// See the block comment on apiload.Reading.ByMethod.
	APIByMethod  map[string]float64 `json:"apiByMethod,omitempty"`
	APIAttribute string             `json:"apiAttribution"`
	APIErr       string             `json:"apiError,omitempty"`

	// APIServerConns is how many ESTABLISHED TCP connections to the apiserver
	// the arm holds, summed over its processes.
	//
	// ***MEASURED, AND IT IS THE ONLY COLUMN THAT SEES A WATCH.*** The request
	// counter cannot: a held WATCH is a GET that has not completed. A socket is
	// what an instance actually keeps, and it is what the apiserver actually
	// spends — a connection plus a watch-cache subscriber, for as long as the
	// process lives. See procsample/conns.go.
	APIServerConns     int  `json:"apiserverConns"`
	APIServerConnsFull bool `json:"apiserverConnsComplete"`

	// CachedObjects is how many apiserver objects the arm holds in memory
	// across every instance.
	//
	// ***BY CONSTRUCTION, NOT BY MEASUREMENT, AND THE RECORD SAYS WHICH.***
	// controller-runtime exports no cache-size metric, so this is derived: each
	// arm-A manager's informer is label-scoped to its own pair, which is two
	// ConfigMaps, so the arm holds 2N. Arm B holds ZERO per instance — a step
	// has no cache at all; radiant's shared wake index is the counterpart and it
	// is one index for the cluster rather than one per program.
	//
	// A derived number is reported as derived. Promoting it to a measurement is
	// the move ADR-0098 spends its length refusing.
	CachedObjects int    `json:"cachedObjects"`
	CachedBasis   string `json:"cachedBasis"`

	// HostRSSBytes, HostPeakBytes, HostCPUms and HostConns are the SHARED HOST's
	// own footprint — radiant's process, not the step pods.
	//
	// ***ARM B IS NOT JUST ITS STEPS, AND OMITTING THE HOST FLATTERS IT.*** N
	// managers are the whole of arm A; arm B is `radiant + N steps`, and radiant
	// is about the size of one manager — measured 42.9 MiB RSS, 52.6 peak, 32
	// threads, one apiserver connection. ADR-0098 lists "the eventual cost of a
	// Perseid host" under NOT MEASURED, and sampling only the steps reproduces
	// that omission.
	//
	// ⚠ ***IT IS A SHARED COST AND THIS BENCHMARK DOES NOT OWN ALL OF IT.*** The
	// same radiant serves other programs and is the Trail Operator besides.
	// Charging all of it to arm B overstates; charging none understates. Both
	// readings are reported rather than one being picked.
	HostRSSBytes  uint64  `json:"hostRssBytes,omitempty"`
	HostPeakBytes uint64  `json:"hostPeakBytes,omitempty"`
	HostCPUms     float64 `json:"hostCpuMs,omitempty"`
	HostConns     int     `json:"hostConns,omitempty"`

	// HostCounters is radiant's own work counters, as a window DELTA.
	//
	// ***A SHARED BUCKET, NOT THIS ARM'S COST.*** One radiant serves every
	// Perseid on the cluster, and these families carry no program label — so a
	// window delta includes gazer-governance, scaler-v4, dag-demo and podmaker.
	// Subtract a `-n 0` baseline run to get the marginal figure; the raw delta is
	// recorded here so the background is visible rather than assumed away.
	HostCounters map[string]float64 `json:"hostCounters,omitempty"`
	HostEndpoint string             `json:"hostEndpoint,omitempty"`

	// LoadTicks is how many load rounds completed inside the window. Zero for
	// the idle window BY DESIGN, and that is the point of having two: an idle
	// measurement answers an idle question and says nothing about reconciliation
	// (ADR-0098 protocol step 7).
	LoadTicks int     `json:"loadTicks"`
	Latency   Summary `json:"latency"`
}

// Result is the whole record of one run, and it is what gets written to disk.
type Result struct {
	Spec      Spec      `json:"spec"`
	StartedAt time.Time `json:"startedAt"`
	Preflight Preflight `json:"preflight"`
	Windows   []Window  `json:"windows"`
}

// Supervisors are command lines that WRAP a workload rather than being one.
//
// ***A POD'S SUPERVISOR CARRIES THE GUEST'S ARGV, SO WITHOUT THIS EVERY
// INSTANCE MATCHES TWICE.*** perigeos launches a pod as a `systemd-nspawn`
// transient unit whose command line ends with the exec target, so a selector
// that identifies the workload identifies its supervisor too. See the block
// comment on procsample.Collect for the measured pair.
//
// `meteor` is perigeos' in-pod exec shim. It execs rather than forks, so it
// leaves no second process — listed anyway, because that is another repository's
// implementation detail and this benchmark should not start double-counting when
// it changes.
var Supervisors = []string{"systemd-nspawn", "/usr/local/bin/meteor"}

// HostSelector matches the SHARED HOST process — radiant. Arm B's cost is
// `radiant + N steps`; see Window.HostRSSBytes for why leaving it out flatters
// the arm.
var HostSelector = "/radiant"

// PerseidSeriesPrefix selects THIS benchmark's programs out of radiant's
// per-program series. A prefix, so one scrape covers N instances — and so a
// Perseid that is not this benchmark's cannot match, which is the negative
// control the unlabelled bucket could not offer.
var PerseidSeriesPrefix = relay.Namespace + "/"

// Selector is the /proc cmdline substring that defines an arm's population.
//
// ***THE CONTROLLER-RUNTIME ARMS RUN THE SAME BINARY, SO THE ARM IS PART OF THE
// SELECTOR.*** `crrelay` alone matches all three alike, and a previous arm's pods
// can outlive a botched teardown — so one arm's memory would land in another's
// column with nothing looking wrong.
//
// The Perseid arm is selected by `--pod-name perseid-relay-`, which trail puts on
// its own command line. That excludes the unrelated Perseids on this cluster,
// which are otherwise identical `trail --p3 --component /module.wasm` processes.
func Selector(arm string) string { return procMatch(arm) }

// procMatch is the cmdline substring that defines an arm's population.
//
// ***AN EXPLICIT `-arm=` TAG, NOT A CLEVER COMBINATION OF BEHAVIOURAL FLAGS.***
// It keyed on `-leader-election=true|false` until A3 arrived — and A3 also runs
// with leader election off, so A2's selector would have matched A3's process and
// one arm's memory would have landed in the other's column with nothing in the
// numbers looking wrong. A selector that has to stay clever is one that breaks
// the next time an arm is added; the binary now carries its own identity and
// ignores it.
func procMatch(arm string) string {
	switch arm {
	case relay.ArmLeader, relay.ArmNoLeader, relay.ArmShared:
		return "-arm=" + arm
	case relay.ArmPerseid:
		return "--pod-name " + PerseidPodPrefix + PerseidPrefix
	case relay.ArmFused:
		return "--pod-name " + PerseidPodPrefix + FusedName
	default:
		return ""
	}
}

// Run executes one measured run against an arm that is already up and Ready.
//
// It does NOT create or delete the arm: the caller does that, so a failed run
// leaves the population standing and can be re-taken without paying for another
// startup. Bringing an arm up inside the measurement would also put pod creation
// inside the settle window, which is the "repeat under change" question and not
// the idle one.
func Run(ctx context.Context, cs *kubernetes.Clientset, dyn dynamic.Interface, spec Spec) (Result, error) {
	res := Result{Spec: spec, StartedAt: time.Now()}

	pre, err := preflight(ctx, cs, dyn, spec)
	res.Preflight = pre
	if err != nil {
		res.Preflight.Err = err.Error()

		return res, err
	}

	// SETTLE FIRST. Caches are synced by the readiness gate, but a manager's
	// first minutes include list backfill and initial reconciles; a window
	// opened on top of that measures startup and calls it idle.
	if spec.Settle > 0 {
		if err := sleepCtx(ctx, spec.Settle); err != nil {
			return res, err
		}
	}

	idle, err := measure(ctx, cs, spec, "idle", spec.Idle, false)
	if err != nil {
		return res, err
	}
	res.Windows = append(res.Windows, idle)
	// ***CHECKED AT EVERY WINDOW BOUNDARY, NOT ONLY UP FRONT.*** The failure this
	// guards against happens AFTER a successful pass, so a preflight-only check
	// would pass and the window it opened would be void.
	if err := healthy(ctx, dyn, spec, "the close of the idle window"); err != nil {
		return res, err
	}

	change, err := measure(ctx, cs, spec, "change", spec.Change, true)
	if err != nil {
		return res, err
	}
	res.Windows = append(res.Windows, change)
	if err := healthy(ctx, dyn, spec, "the close of the change window"); err != nil {
		return res, err
	}

	return res, nil
}

// healthy is PerseidsHealthy for arm B and a no-op elsewhere. Arm A has no
// counterpart: a controller-runtime manager has no phase, which is exactly why
// the equivalent failure there would be invisible.
func healthy(ctx context.Context, dyn dynamic.Interface, spec Spec, when string) error {
	if (spec.Arm != relay.ArmPerseid && spec.Arm != relay.ArmFused) || dyn == nil {
		return nil
	}

	return PerseidsHealthy(ctx, dyn, when)
}

func preflight(ctx context.Context, cs *kubernetes.Clientset, dyn dynamic.Interface, spec Spec) (Preflight, error) {
	match := procMatch(spec.Arm)
	pre := Preflight{Selector: match, Commit: commit(), PeriapsisImage: spec.Image}
	pre.TrailVersion, pre.RadiantVersion = runtimeVersions(ctx)

	set, err := procsample.Collect(match, Supervisors...)
	if err != nil {
		return pre, err
	}
	pre.Found = len(set.Samples)

	// The negative control: every OTHER arm's selector must find nothing, so an
	// empty result later is readable as "that arm is down" and a full one is
	// readable as "the teardown did not happen".
	for _, other := range relay.Arms {
		if other == spec.Arm {
			continue
		}
		s, err := procsample.Collect(procMatch(other), Supervisors...)
		if err != nil {
			return pre, err
		}
		pre.Excluded += len(s.Samples)
	}

	// ***N=0 IS THE BASELINE RUN AND HAS NO PROGRAMS BY DESIGN.*** The
	// config-delivery control asserts something about pods that exist; with none,
	// "could not be checked" is the correct answer and refusing would make the
	// paired baseline — the only thing that turns radiant's shared bucket into a
	// marginal figure — impossible to take. Narrowed to N>0 rather than softened,
	// so a real run still cannot skip it.
	if (spec.Arm == relay.ArmPerseid || spec.Arm == relay.ArmFused) && spec.N > 0 {
		placement, err := PodPlacement(ctx, cs, spec.Host)
		pre.Placement = placement
		if err != nil {
			return pre, err
		}
		if err := ConfigReachesGuest(ctx, cs); err != nil {
			return pre, err
		}
		pre.ConfigDelivered = true
	}

	if relay.IsCR(spec.Arm) {
		eps, err := endpoints(ctx, cs, spec.Arm)
		if err != nil {
			return pre, err
		}
		pre.APITotals = map[string]float64{}
		for _, ep := range eps {
			r, err := apiload.Scrape(ctx, ep, 5*time.Second)
			if err == nil && r.Found {
				pre.MetricsEndpoints++
				for method, n := range r.ByMethod {
					pre.APITotals[method] += n
				}
			}
		}
	}

	// LAST, AND AFTER EVERY MARKER CHECK. The markers are cheap and their
	// failures are specific; this one is slow and its failure is general, so it
	// runs when the cheap ones have already ruled out the things they can name.
	took, convErr := ProveConvergence(ctx, cs, spec.N, spec.ConvergeTimeout)
	if convErr == nil {
		pre.Converged, pre.ConvergeSeconds = true, took.Seconds()
		// AFTER convergence, because the park is what fails and the park comes
		// after the write. Checking before would ask the question too early.
		convErr = healthy(ctx, dyn, spec, "preflight, after convergence")
	}

	switch {
	case convErr != nil:
		return pre, convErr
	case spec.N == 0 && pre.Found != 0:
		return pre, fmt.Errorf("bench: baseline run asked for N=0 but %d %s processes are still "+
			"running; the background rate would include them", pre.Found, spec.Arm)
	case pre.Found != relay.Instances(spec.Arm, spec.N):
		return pre, fmt.Errorf("bench: positive control failed: selector %q matched %d processes, "+
			"expected %d — the population is not the one this run describes",
			match, pre.Found, relay.Instances(spec.Arm, spec.N))
	case pre.Excluded != 0:
		return pre, fmt.Errorf("bench: negative control failed: %d processes from another arm are "+
			"still running; their memory would be counted into %s", pre.Excluded, spec.Arm)
	case relay.IsCR(spec.Arm) && pre.MetricsEndpoints != relay.Instances(spec.Arm, spec.N):
		return pre, fmt.Errorf("bench: %d/%d %s endpoints served %s; a delta over a missing "+
			"series is not zero traffic", pre.MetricsEndpoints,
			relay.Instances(spec.Arm, spec.N), spec.Arm, apiload.Counter)
	}

	return pre, nil
}

// measure opens one window, optionally driving load through it.
func measure(ctx context.Context, cs *kubernetes.Clientset, spec Spec, name string,
	dur time.Duration, load bool,
) (Window, error) {
	w := Window{Name: name, Seconds: dur.Seconds()}
	match := procMatch(spec.Arm)

	before, err := procsample.Collect(match, Supervisors...)
	if err != nil {
		return w, err
	}
	apiBefore, apiErr := scrapeArm(ctx, cs, spec.Arm)

	var hostBefore, mineBefore map[string]float64
	var hostProcBefore procsample.Set
	if spec.Arm == relay.ArmPerseid || spec.Arm == relay.ArmFused {
		if spec.RadiantMetrics != "" {
			hostBefore, _ = apiload.ScrapeCounters(ctx, spec.RadiantMetrics, apiload.HostCounters, 5*time.Second)
			mineBefore, _ = apiload.ScrapeByLabelPrefix(ctx, spec.RadiantMetrics,
				[]string{apiload.PerseidApplied, apiload.PerseidRuns},
				apiload.PerseidLabel, PerseidSeriesPrefix, 5*time.Second)
		}
		hostProcBefore, _ = procsample.Collect(HostSelector, Supervisors...)
	}

	wctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()

	var lat Latencies
	watchDone := make(chan error, 1)
	go func() { watchDone <- WatchDestinations(wctx, cs, &lat) }()

	if load {
		ticks, err := Load(wctx, cs, spec.N, spec.Period)
		w.LoadTicks = ticks
		if err != nil {
			return w, err
		}
	}
	<-wctx.Done()
	if err := <-watchDone; err != nil && !errors.Is(err, context.Canceled) {
		return w, err
	}

	after, err := procsample.Collect(match, Supervisors...)
	if err != nil {
		return w, err
	}
	rss, pss, ticksAfter, complete := after.Totals()
	_, _, ticksBefore, _ := before.Totals()

	w.Processes = len(after.Samples)
	w.RSSBytes, w.PSSBytes, w.PSSComplete = rss, pss, complete
	// ***THE CPU DELTA IS ONLY VALID IF THE POPULATION HELD.*** A pod that
	// restarted mid-window resets its counter, so the difference understates by
	// whatever it had accumulated. Reported as zero-with-a-note rather than as a
	// small number nobody can question.
	if len(after.Samples) == len(before.Samples) && ticksAfter >= ticksBefore {
		w.CPUms = procsample.TicksToMillis(ticksAfter - ticksBefore)
	} else {
		w.APIErr = fmt.Sprintf("population moved inside the window (%d -> %d processes); "+
			"the CPU delta is not comparable", len(before.Samples), len(after.Samples))
	}

	// Radiant's counters bracket the SAME window as everything else, so the
	// delta is over one interval rather than two that nearly overlap.
	if spec.Arm == relay.ArmPerseid && spec.RadiantMetrics != "" {
		if after, err := apiload.ScrapeCounters(ctx, spec.RadiantMetrics, apiload.HostCounters, 5*time.Second); err == nil {
			w.HostEndpoint = spec.RadiantMetrics
			w.HostCounters = map[string]float64{}
			for k, v := range after {
				if b, ok := hostBefore[k]; ok && v >= b {
					w.HostCounters[k] = v - b
				}
			}
		}
	}

	if spec.Arm == relay.ArmPerseid || spec.Arm == relay.ArmFused {
		if hostAfter, err := procsample.Collect(HostSelector, Supervisors...); err == nil && len(hostAfter.Samples) > 0 {
			rss, _, ticksAfter, _ := hostAfter.Totals()
			w.HostRSSBytes = rss
			for _, sm := range hostAfter.Samples {
				w.HostPeakBytes += sm.PeakRSSBytes
			}
			if _, _, ticksBefore, _ := hostProcBefore.Totals(); len(hostProcBefore.Samples) == len(hostAfter.Samples) && ticksAfter >= ticksBefore {
				w.HostCPUms = procsample.TicksToMillis(ticksAfter - ticksBefore)
			}
			w.HostConns, _ = hostAfter.CountConnsAnyOf(spec.apiEndpoints())
		}
	}

	// ***PER-PROGRAM, SO ARM B IS ATTRIBUTABLE BY CONSTRUCTION RATHER THAN BY
	// SUBTRACTION.*** radiant labels these `perseid="ns/name"`, so filtering to
	// this benchmark's own programs is the same class of instrument arm A gets
	// from client-go. Before the label existed the only route was a paired
	// baseline, and the background was most of the bucket.
	if (spec.Arm == relay.ArmPerseid || spec.Arm == relay.ArmFused) && spec.RadiantMetrics != "" && mineBefore != nil {
		if mineAfter, err := apiload.ScrapeByLabelPrefix(ctx, spec.RadiantMetrics,
			[]string{apiload.PerseidApplied, apiload.PerseidRuns},
			apiload.PerseidLabel, PerseidSeriesPrefix, 5*time.Second); err == nil {
			applied := mineAfter[apiload.PerseidApplied] - mineBefore[apiload.PerseidApplied]
			runs := mineAfter[apiload.PerseidRuns] - mineBefore[apiload.PerseidRuns]
			if applied >= 0 && runs >= 0 {
				w.APIRequests, w.APIMeasured = applied, true
				w.APIByMethod = map[string]float64{"applied": applied, "runs": runs}
			}
		}
	}

	apiAfter, apiErr2 := scrapeArm(ctx, cs, spec.Arm)
	w.APIAttribute = attribution(spec.Arm)
	switch {
	case (spec.Arm == relay.ArmPerseid || spec.Arm == relay.ArmFused) && w.APIMeasured:
		// Already measured from the per-program series; the client-counter path
		// below does not apply to an arm that makes no calls of its own.
	case len(apiBefore) == 0:
		// ═══════════════════════════════════════════════════════════════════
		// ***A ZERO WITH NO INSTRUMENT BEHIND IT IS THE FLATTERING KIND, AND
		// THIS BENCHMARK PRODUCED ONE FOR THE ARM ITS AUTHOR WANTS TO WIN.***
		// Arm B has no per-instance metrics endpoint — a Perseid makes no
		// apiserver calls of its own, so there is nothing to scrape — and with
		// RADIANT_METRICS unset there is no shared endpoint either. The
		// subtraction over an empty set is 0, and it printed as `api req 0`
		// beside arm A's real counts.
		//
		// That is exactly ADR-0098 protocol step 4 — "a zero means either no
		// events or a broken instrument until the controls distinguish those
		// cases" — failing inside the harness written to enforce it, in the
		// direction of the preferred result. Set RADIANT_METRICS to measure it;
		// until then the column says so instead of saying zero.
		// ═══════════════════════════════════════════════════════════════════
		w.APIMeasured = false
		w.APIErr = "NOT MEASURED: no client-side counter was scraped for this arm. " +
			"A Perseid makes no apiserver calls of its own, so the marginal cost is " +
			"radiant's — set RADIANT_METRICS to its /metrics endpoint to measure it as a " +
			"delta against the N=0 baseline. This is not a measurement of zero traffic"
	case apiErr != nil:
		w.APIErr = apiErr.Error()
	case apiErr2 != nil:
		w.APIErr = apiErr2.Error()
	default:
		delta, byMethod, err := sumDeltas(apiBefore, apiAfter)
		if err != nil {
			w.APIErr = err.Error()
		} else {
			w.APIRequests, w.APIByMethod, w.APIMeasured = delta, byMethod, true
		}
	}

	w.APIServerConns, w.APIServerConnsFull = after.CountConnsAnyOf(spec.apiEndpoints())
	w.CachedObjects, w.CachedBasis = cached(spec)
	w.Latency = lat.Summarize()

	return w, nil
}

// cached reports the arm's in-memory object population and how it was arrived
// at. See Window.CachedObjects.
func cached(spec Spec) (int, string) {
	if spec.Arm == relay.ArmShared {
		return 2 * spec.N, "DERIVED, not measured: ONE informer scoped to every relay fixture, so " +
			"the arm holds 2N objects in ONE process rather than 2 in each of N."
	}
	if relay.IsCR(spec.Arm) {
		return 2 * spec.N, "DERIVED, not measured: each manager's informer is label-scoped to its " +
			"own pair (2 ConfigMaps), so the arm holds 2N. controller-runtime exports no cache-size metric."
	}

	return 0, "DERIVED, not measured: a Perseid step holds no cache. radiant's wake index is the " +
		"counterpart and is ONE index for the cluster, not one per program."
}

func attribution(arm string) string {
	if relay.IsCR(arm) {
		return "per-instance client-go counters, scraped from each worker's own /metrics — " +
			"attribution by construction"
	}

	return "radiant's PER-PROGRAM counters, filtered to perseid=\"" + PerseidSeriesPrefix +
		"*\" — attribution by construction, the same class as arm A's client-side counters. " +
		"`applied` is writes performed on the program's behalf; `runs` is step passes"
}

// scrapeArm reads the client counter for every process in the arm.
func scrapeArm(ctx context.Context, cs *kubernetes.Clientset, arm string) (map[string]apiload.Reading, error) {
	eps, err := endpoints(ctx, cs, arm)
	if err != nil {
		return nil, err
	}
	out := make(map[string]apiload.Reading, len(eps))
	for _, ep := range eps {
		r, err := apiload.Scrape(ctx, ep, 5*time.Second)
		if err != nil {
			return nil, err
		}
		out[ep] = r
	}

	return out, nil
}

func sumDeltas(before, after map[string]apiload.Reading) (float64, map[string]float64, error) {
	var total float64
	byMethod := map[string]float64{}
	for ep, b := range before {
		a, ok := after[ep]
		if !ok {
			return 0, nil, fmt.Errorf("bench: %s vanished between the ends of the window", ep)
		}
		d, err := apiload.Delta(b, a)
		if err != nil {
			return 0, nil, err
		}
		total += d

		split, err := apiload.DeltaByMethod(b, a)
		if err != nil {
			return 0, nil, err
		}
		for method, n := range split {
			byMethod[method] += n
		}
	}

	return total, byMethod, nil
}

// endpoints lists the /metrics URLs for an arm.
//
// For a Perseid run there is ONE: radiant's. A Perseid makes no apiserver calls
// of its own — it observes and emits through the host — so there is no
// per-instance counter to read, and saying so in the record is more useful than
// an empty map.
func endpoints(ctx context.Context, cs *kubernetes.Clientset, arm string) ([]string, error) {
	if arm == relay.ArmPerseid {
		ep := os.Getenv("RADIANT_METRICS")
		if ep == "" {
			return nil, nil
		}

		return []string{ep}, nil
	}
	ips, err := PodIPs(ctx, cs, arm)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, "http://"+ip+":8080/metrics")
	}

	return out, nil
}

// commit is this repository's revision, for protocol step 0. Empty when the
// tree is not a git checkout, which is a fact worth recording rather than an
// error worth failing on.
func commit() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	rev := strings.TrimSpace(string(out))
	if dirty, err := exec.Command("git", "status", "--porcelain").Output(); err == nil &&
		len(strings.TrimSpace(string(dirty))) > 0 {
		// A dirty tree is not the commit it claims to be, and a result filed
		// under a clean hash cannot be reproduced from it.
		rev += "-dirty"
	}

	return rev
}

// runtimeVersions reads what the RUNTIME actually is, from the artifact and the
// running pod. Empty on failure rather than guessed — an unknown version is a
// fact, and a wrong one is worse than none.
func runtimeVersions(ctx context.Context) (trail, radiant string) {
	if out, err := exec.CommandContext(ctx, "trail", "--capabilities").Output(); err == nil {
		var v struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(out, &v) == nil {
			trail = v.Version
		}
	}
	// radiant prints its own commit at startup; the deployment is tagged
	// `radiant:latest` and stamps no sha, so the log is the only place the
	// running binary names itself.
	out, err := exec.CommandContext(ctx, "kubectl", "logs", "-n", "apsis", "deploy/radiant").Output()
	if err == nil {
		if i := bytes.LastIndex(out, []byte("commit=")); i >= 0 {
			rest := out[i+len("commit="):]
			if j := bytes.IndexAny(rest, " \n\""); j > 0 {
				radiant = string(rest[:j])
			}
		}
	}

	return trail, radiant
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
