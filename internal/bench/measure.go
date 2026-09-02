package bench

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"

	"github.com/malformed-c/operator-overhead/internal/relay"
)

// EncodeValue renders the relayed field: a sequence number and the instant the
// harness wrote it.
//
// ***THE TIMESTAMP TRAVELS INSIDE THE VALUE, AND THAT IS WHAT MAKES THE LATENCY
// END-TO-END.*** The alternative — noting the write time locally and matching
// it to a destination event by sequence — needs the harness to hold a map keyed
// on something the relay never sees, and it silently mis-attributes whenever a
// destination event arrives out of order or a write is retried. Carrying the
// origin instant in the payload means a destination observation IS its own
// measurement: the field the relay copied is the field that dates it.
//
// The arms are unaffected. Both copy `data.v` opaquely; neither parses it.
func EncodeValue(seq int, at time.Time) string {
	return strconv.Itoa(seq) + "|" + strconv.FormatInt(at.UnixNano(), 10)
}

// DecodeValue reads back what EncodeValue wrote.
func DecodeValue(v string) (seq int, at time.Time, err error) {
	head, tail, ok := strings.Cut(v, "|")
	if !ok {
		return 0, time.Time{}, fmt.Errorf("bench: value %q carries no origin instant", v)
	}
	seq, err = strconv.Atoi(head)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("bench: value %q: sequence: %w", v, err)
	}
	ns, err := strconv.ParseInt(tail, 10, 64)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("bench: value %q: instant: %w", v, err)
	}

	return seq, time.Unix(0, ns), nil
}

// Latencies collects one convergence sample per destination write, and the
// operator's own reaction time when the destination carries a stamp.
type Latencies struct {
	mu       sync.Mutex
	samples  []time.Duration
	reaction []time.Duration
	// dropped counts destination values this harness could not decode. A
	// benchmark that silently ignored them would report a clean p99 over
	// whichever writes happened to parse.
	dropped int
	// stale counts pairs whose stamp describes a DIFFERENT value than the one
	// observed — a torn read of an arm that writes value and stamp separately.
	// Its own counter, because "the arm is slow" and "the harness caught it
	// mid-update" are different facts and only one is about the arm.
	stale int
	// unstamped counts destinations that converged without an operator stamp.
	// SEPARATE FROM dropped, because it is a different fact: the relay worked
	// and this harness cannot see how fast it reacted. Folding them together
	// would let a stamp that never lands read as a clean convergence sample.
	unstamped int
}

// Observe records a destination value, dating it from the value itself, and the
// operator's stamp when one is present.
//
// `stamp` is `data.t` in epoch milliseconds — empty when the writer did not set
// it. See relay.FieldT for what the split buys.
func (l *Latencies) Observe(value, stamp string, now time.Time) {
	_, at, err := DecodeValue(value)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		l.dropped++

		return
	}
	l.samples = append(l.samples, now.Sub(at))

	// ***THE STAMP MUST NAME THE VALUE IT DATES, OR IT IS DISCARDED.*** An arm
	// that writes the value and the stamp as two obligations can be observed
	// mid-pair: new `data.v`, previous `data.t`. Accepting that produces a
	// reaction sample that is negative by however long ago the last change was —
	// measured at -72 SECONDS before the format carried the value.
	stampedValue, stampedAt, ok := strings.Cut(stamp, "@")
	ms, perr := strconv.ParseInt(stampedAt, 10, 64)
	if !ok || perr != nil {
		l.unstamped++

		return
	}
	if stampedValue != value {
		// Not an error and not a slow reaction: a torn read of a two-write
		// update. Counted so the denominator stays honest.
		l.stale++

		return
	}
	// ***A NEGATIVE REACTION IS RECORDED, NOT CLAMPED.*** It would mean the
	// operator stamped a clock earlier than the harness's write instant, which on
	// a single host cannot happen and therefore means the two are NOT on one
	// clock — the assumption relay.FieldT rests on. Clamping would hide exactly
	// the condition that invalidates the column.
	l.reaction = append(l.reaction, time.UnixMilli(ms).Sub(at))
}

// Summary is the latency column of one window.
type Summary struct {
	// Count is the DENOMINATOR, and it is reported beside every quantile
	// because ADR-0098's protocol step 4 requires it: a p99 over eleven samples
	// is a different claim from a p99 over four thousand.
	Count   int     `json:"count"`
	Dropped int     `json:"dropped"`
	P50MS   float64 `json:"p50Ms"`
	P90MS   float64 `json:"p90Ms"`
	P99MS   float64 `json:"p99Ms"`
	MaxMS   float64 `json:"maxMs"`

	// Reaction is NOTICE + DECIDE, from the operator's own clock: how long the
	// arm took to react to a change, excluding the write's round trip and the
	// harness's own delivery. This is the arm's reflex; the fields above are
	// what an observer of the cluster sees.
	Reaction Quantiles `json:"reaction"`
	// Stale counts torn reads — a stamp naming a different value than the one
	// observed. High stale with low reaction Count means the arm writes its
	// value and its stamp separately, which is a fact about the arm.
	Stale int `json:"stale"`
	// Unstamped counts convergences whose writer set no `data.t`. A nonzero
	// value with a populated Reaction means the quantiles cover only part of the
	// population, which is a different claim from covering all of it.
	Unstamped int `json:"unstamped"`
}

// Quantiles is a distribution with its denominator attached, because a quantile
// without one is not a measurement.
type Quantiles struct {
	Count int     `json:"count"`
	P50MS float64 `json:"p50Ms"`
	P90MS float64 `json:"p90Ms"`
	P99MS float64 `json:"p99Ms"`
	MaxMS float64 `json:"maxMs"`
	// MinMS matters for reaction and not for convergence: a NEGATIVE minimum is
	// the tell that the two clocks are not one clock, which invalidates the
	// column rather than merely widening it.
	MinMS float64 `json:"minMs"`
}

// Summarize computes the quantiles over everything observed so far.
func (l *Latencies) Summarize() Summary {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := Summary{Count: len(l.samples), Dropped: l.dropped, Unstamped: l.unstamped, Stale: l.stale}
	out.Reaction = quantilesOf(l.reaction)
	if len(l.samples) == 0 {
		return out
	}
	s := slices.Sorted(slices.Values(l.samples))
	out.P50MS = millis(quantile(s, 0.50))
	out.P90MS = millis(quantile(s, 0.90))
	out.P99MS = millis(quantile(s, 0.99))
	out.MaxMS = millis(s[len(s)-1])

	return out
}

func millis(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func quantilesOf(ds []time.Duration) Quantiles {
	out := Quantiles{Count: len(ds)}
	if len(ds) == 0 {
		return out
	}
	s := slices.Sorted(slices.Values(ds))
	out.P50MS = millis(quantile(s, 0.50))
	out.P90MS = millis(quantile(s, 0.90))
	out.P99MS = millis(quantile(s, 0.99))
	out.MaxMS = millis(s[len(s)-1])
	out.MinMS = millis(s[0])

	return out
}

// quantile is nearest-rank on a sorted slice.
//
// NEAREST-RANK RATHER THAN INTERPOLATED, because an interpolated p99 over a
// small sample invents a value between two real observations and then gets
// quoted as if something took that long. Every number this returns is a
// latency that was actually measured.
func quantile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := min(max(int(math.Ceil(q*float64(len(sorted))))-1, 0), len(sorted)-1)

	return sorted[rank]
}

// WatchDestinations follows every destination ConfigMap and feeds l until ctx
// ends.
//
// ***A WATCH, NOT A POLL, AND THE HARNESS'S OWN LATENCY IS THE FLOOR THIS
// REPORTS.*** Every measured number includes the time for the apiserver to
// deliver the destination event to this process, which is a constant added to
// both arms and cancels in the comparison. It does NOT cancel in the absolute
// figures, so those are reported as convergence-as-observed rather than as the
// relay's own service time.
func WatchDestinations(ctx context.Context, cs *kubernetes.Clientset, l *Latencies) error {
	api := cs.CoreV1().ConfigMaps(relay.Namespace)
	sel := relay.LabelSide + "=" + relay.SideDst

	// A resourceVersion from a List, so the watch starts from a known point and
	// the initial state is not replayed as N spurious convergences.
	list, err := api.List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return fmt.Errorf("bench: list destinations: %w", err)
	}
	rv := list.ResourceVersion

	for ctx.Err() == nil {
		w, err := api.Watch(ctx, metav1.ListOptions{LabelSelector: sel, ResourceVersion: rv})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			return fmt.Errorf("bench: watch destinations: %w", err)
		}
		rv = drain(ctx, w, l, rv)
	}

	return nil
}

func drain(ctx context.Context, w watch.Interface, l *Latencies, rv string) string {
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return rv
		case ev, ok := <-w.ResultChan():
			if !ok {
				// The watch expired. Resuming from the last seen version rather
				// than from scratch is what keeps a re-List from replaying every
				// destination as a fresh convergence.
				return rv
			}
			cm, ok := ev.Object.(*corev1.ConfigMap)
			if !ok {
				continue
			}
			rv = cm.ResourceVersion
			if ev.Type != watch.Modified && ev.Type != watch.Added {
				continue
			}
			if v := cm.Data[relay.FieldV]; v != "" {
				l.Observe(v, cm.Data[relay.FieldT], time.Now())
			}
		}
	}
}

// Load writes a fresh value to every source ConfigMap on each tick, until ctx
// ends, and reports how many ticks completed.
//
// ***THE TICK IS SKIPPED RATHER THAN QUEUED WHEN A ROUND OVERRUNS.*** At N=64 a
// round is 64 sequential updates; if that takes longer than the period, queuing
// would let the harness fall progressively behind and the "rate" in the report
// would describe an intention rather than the run. A skipped tick is visible in
// the completed count, which is what the report prints.
func Load(ctx context.Context, cs *kubernetes.Clientset, n int, period time.Duration) (ticks int, err error) {
	t := time.NewTicker(period)
	defer t.Stop()

	for seq := 0; ; seq++ {
		select {
		case <-ctx.Done():
			return ticks, nil
		case <-t.C:
		}
		if err := SetSource(ctx, cs, n, EncodeValue(seq, time.Now())); err != nil {
			// ***THE WINDOW CLOSING IS NOT A FAILURE, AND `errors.Is` ALONE DOES
			// NOT SEE IT.*** A round in flight when the deadline lands surfaces
			// as whatever the layer that noticed first produced — measured:
			// `client rate limiter Wait returned an error: rate: Wait(n=1) would
			// exceed context deadline`, a plain error wrapping nothing. Asking
			// the CONTEXT rather than the error is what makes the check total:
			// if the window is over, the last partial round is expected.
			if ctx.Err() != nil {
				return ticks, nil
			}

			return ticks, err
		}
		ticks++
	}
}
