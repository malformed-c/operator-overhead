// Package apiload counts apiserver requests BY THE CALLER THAT MADE THEM.
package apiload

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// Counter is the metric family client-go increments once per apiserver request.
const Counter = "rest_client_requests_total"

// Reading is one scrape of one process.
type Reading struct {
	// Endpoint is the URL scraped, kept so a row can be audited back to a pod.
	Endpoint string `json:"endpoint"`
	// Requests is the sum over every label combination of Counter.
	Requests float64 `json:"requests"`
	// ByCode is the same total split by HTTP status. A run whose requests are
	// mostly 409s is a run whose reconcilers are fighting, not a run that did
	// more work, and the split is what makes those distinguishable.
	ByCode map[string]float64 `json:"byCode,omitempty"`

	// ByMethod splits the total by HTTP method.
	//
	// ═══════════════════════════════════════════════════════════════════════
	// ***HTTP METHOD, NOT KUBERNETES VERB.*** `LIST` and `WATCH` are not values
	// this map can carry: client-go labels the counter with the HTTP method, a
	// LIST and a WATCH are both `GET`, and a WATCH is a GET that DOES NOT
	// COMPLETE — so it never increments while it is held.
	//
	// Measured at N=1, one controller-runtime manager, a 45-second window with
	// its informer watching throughout: `GET=3 PATCH=74`. The watch contributed
	// nothing to either number and was open the whole time.
	//
	// What this map DOES answer, cleanly:
	//
	//	PATCH   the relay writing. One per change, or the arm is amplifying
	//	PUT     a Lease renewal — leader election, and nothing else here
	//	GET     reads that reached the apiserver, plus completed LISTs
	//
	// PUT is what makes A1 and A2 legible: with leader election on, each
	// instance renews its Lease every ~10s forever, whether or not anything
	// happened. That traffic cannot be attributed from a server-side bucket, and
	// a per-process counter is the instrument that can.
	//
	// What is held open rather than requested is a different quantity and needs
	// a different instrument — see procsample.ConnsTo, which counts sockets.
	// ═══════════════════════════════════════════════════════════════════════
	ByMethod map[string]float64 `json:"byMethod,omitempty"`
	// Found records whether the family was present at all. A missing family and
	// a family at zero are different facts: the first is a broken instrument
	// and the second is a measurement (ADR-0098, protocol step 4).
	Found bool `json:"found"`
}

// Scrape reads Counter from one process's metrics endpoint.
func Scrape(ctx context.Context, endpoint string, timeout time.Duration) (Reading, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Reading{Endpoint: endpoint}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Reading{Endpoint: endpoint}, fmt.Errorf("apiload: scrape %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Reading{Endpoint: endpoint}, fmt.Errorf("apiload: scrape %s: HTTP %d", endpoint, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Reading{Endpoint: endpoint}, fmt.Errorf("apiload: read %s: %w", endpoint, err)
	}

	r, err := Parse(string(body))
	r.Endpoint = endpoint

	return r, err
}

// Parse sums Counter out of a Prometheus text exposition.
//
// Uses expfmt rather than a hand-rolled line scanner: client-go's counter
// carries `code`, `host` and `method` labels, the exposition escapes label
// values, and a scanner that got either wrong would silently under-count.
func Parse(text string) (Reading, error) {
	// ***THE VALIDATION SCHEME IS PASSED, NOT DEFAULTED.*** A zero-valued
	// TextParser PANICS in prometheus/common v0.67 — `ValidationScheme` has no
	// meaningful zero since UTF-8 metric names landed, so `var p expfmt.TextParser`
	// compiles and dies at the first `# HELP`. Legacy validation is the right
	// one here: client-go's counter name is plain ASCII and legacy is what the
	// exporters this scrapes actually emit.
	p := expfmt.NewTextParser(model.LegacyValidation)
	families, err := p.TextToMetricFamilies(strings.NewReader(text))
	if err != nil {
		return Reading{}, fmt.Errorf("apiload: parse exposition: %w", err)
	}
	fam, ok := families[Counter]
	if !ok {
		// NOT AN ERROR AND NOT A ZERO. See Reading.Found.
		return Reading{}, nil
	}

	out := Reading{Found: true, ByCode: map[string]float64{}, ByMethod: map[string]float64{}}
	for _, m := range fam.GetMetric() {
		c := m.GetCounter()
		if c == nil {
			continue
		}
		out.Requests += c.GetValue()
		out.ByCode[labelOf(m, "code")] += c.GetValue()
		out.ByMethod[labelOf(m, "method")] += c.GetValue()
	}

	return out, nil
}

func labelOf(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}

	return "" // an unlabelled sample still belongs in the total
}

// Delta is the difference between two scrapes of the same endpoint.
//
// ***A COUNTER RESET IS REPORTED, NOT CLAMPED.*** A process that restarted
// mid-window starts its counter at zero, so `after - before` goes negative;
// returning 0 there would silently convert a restarted pod into an arm that
// made no requests. The window is invalid and the caller has to know.
func Delta(before, after Reading) (float64, error) {
	if !before.Found || !after.Found {
		return 0, fmt.Errorf("apiload: %s absent from one end of the window (before=%v after=%v) — "+
			"a delta over a missing series is not zero traffic", Counter, before.Found, after.Found)
	}
	if after.Requests < before.Requests {
		return 0, fmt.Errorf("apiload: %s went backwards (%.0f -> %.0f) at %s: the process restarted "+
			"inside the window, so this sample is void", Counter, before.Requests, after.Requests, after.Endpoint)
	}

	return after.Requests - before.Requests, nil
}

// DeltaByMethod is Delta, split by HTTP method.
//
// ***A METHOD THAT APPEARS ONLY IN `after` IS A REAL DELTA, NOT A MISSING
// SERIES.*** client-go registers a label combination the first time it is used,
// so an arm that had not yet issued a PATCH has no PATCH series at all at the
// start of the window — treating that as absent would drop every first write of
// a run from the count.
func DeltaByMethod(before, after Reading) (map[string]float64, error) {
	if !before.Found || !after.Found {
		return nil, fmt.Errorf("apiload: %s absent from one end of the window", Counter)
	}
	out := make(map[string]float64, len(after.ByMethod))
	for method, a := range after.ByMethod {
		b := before.ByMethod[method] // zero when the series did not exist yet
		if a < b {
			return nil, fmt.Errorf("apiload: %s{method=%q} went backwards (%.0f -> %.0f): "+
				"the process restarted inside the window", Counter, method, b, a)
		}
		if d := a - b; d != 0 {
			out[method] = d
		}
	}

	return out, nil
}

// HostCounters are radiant's OWN counters about the work it does for programs.
//
// ═══════════════════════════════════════════════════════════════════════════
// ***THESE ARE A SHARED BUCKET AND MUST NEVER BE QUOTED AS ONE ARM'S COST.***
// radiant exposes them on :9600/metrics UNLABELLED — no program, no namespace,
// no kind:
//
//	radiant_reconcile_applied_total 70
//	radiant_reconcile_runs_total 58
//
// One radiant serves every Perseid on the cluster, so during any window these
// also count `gazer-governance` (which yields on every tick and has passed
// 15,000+ times), `scaler-v4`, `dag-demo` and `podmaker`. A delta over them is a
// statement about a RESOURCE BUCKET, not about the benchmark's programs — the
// precise scope error ADR-0098's protocol step 1 exists to prevent.
//
// ***AND radiant DOES NOT REGISTER `rest_client_requests_total` AT ALL*** —
// measured, zero series on that endpoint — so the by-construction attribution
// arm A gets is simply not available here. There is no version of this that
// makes the number attributable by itself.
//
// The only honest use is a PAIRED DELTA: run the same window with the benchmark's
// Perseids absent (`-n 0`) to get the background rate, then with them present.
// The difference is the arm's MARGINAL cost, and the background is reported
// beside it so a reader can see how much of the bucket was never ours.
// ═══════════════════════════════════════════════════════════════════════════
var HostCounters = []string{
	"radiant_reconcile_applied_total",             // writes performed for programs
	"radiant_reconcile_runs_total",                // step passes driven
	"radiant_reconcile_runs_failed_total",         // passes that returned an error
	"radiant_reconcile_obligations_dropped_total", // an obligation nobody serviced
	"radiant_reconcile_write_failed_total",
}

// ScrapeCounters reads named unlabelled counter families from one endpoint.
//
// A family that is ABSENT is omitted from the result rather than recorded as
// zero, for the same reason Reading.Found exists: a missing series and a series
// at zero are different facts, and only one of them is a measurement.
func ScrapeCounters(ctx context.Context, endpoint string, names []string, timeout time.Duration) (map[string]float64, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("apiload: scrape %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("apiload: scrape %s: HTTP %d", endpoint, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	p := expfmt.NewTextParser(model.LegacyValidation)
	families, err := p.TextToMetricFamilies(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("apiload: parse %s: %w", endpoint, err)
	}
	out := make(map[string]float64, len(names))
	for _, n := range names {
		fam, ok := families[n]
		if !ok {
			continue // absent, not zero
		}
		var sum float64
		for _, m := range fam.GetMetric() {
			if c := m.GetCounter(); c != nil {
				sum += c.GetValue()
			}
		}
		out[n] = sum
	}

	return out, nil
}

// PerseidApplied and PerseidRuns are radiant's PER-PROGRAM counters.
//
// ═══════════════════════════════════════════════════════════════════════════
// ***THESE MAKE ARM B ATTRIBUTABLE BY CONSTRUCTION, WHICH IT WAS NOT.*** The
// unlabelled `radiant_reconcile_*` families are one bucket for every Perseid on
// the cluster, so arm B's cost could only be reached as a paired delta — one run
// with the programs absent, one with them present — and the background was most
// of the bucket at these magnitudes.
//
// radiant now emits `perseid="ns/name"`, so a filter on this benchmark's own
// programs is the same class of instrument arm A gets from client-go: the label
// is applied at the emit site by the thing doing the work, and there is no
// bucket left to divide. That is ADR-0098's protocol step 1 — "identify
// callers" — satisfied rather than worked around.
//
// The unlabelled families are still scraped beside these, because the two answer
// different questions: this one is "what did MY programs cost", the bucket is
// "what was radiant doing at the time", and a run where the second moved and the
// first did not is a run with a neighbour in it.
// ═══════════════════════════════════════════════════════════════════════════
const (
	PerseidApplied = "radiant_perseid_applied_total"
	PerseidRuns    = "radiant_perseid_runs_total"
	PerseidLabel   = "perseid"
)

// ScrapeByLabelPrefix sums named counter families over only those series whose
// `label` value starts with prefix.
//
// A PREFIX RATHER THAN AN EXACT SET, so one scrape covers N instances without
// the caller enumerating them — and a program that is not this benchmark's
// cannot match, which is the negative control the unlabelled bucket could not
// offer.
func ScrapeByLabelPrefix(
	ctx context.Context, endpoint string, names []string, label, prefix string, timeout time.Duration,
) (map[string]float64, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("apiload: scrape %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("apiload: scrape %s: HTTP %d", endpoint, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	p := expfmt.NewTextParser(model.LegacyValidation)
	families, err := p.TextToMetricFamilies(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("apiload: parse %s: %w", endpoint, err)
	}

	out := make(map[string]float64, len(names))
	for _, n := range names {
		fam, ok := families[n]
		if !ok {
			continue // absent, not zero — see Reading.Found
		}
		var sum float64
		for _, m := range fam.GetMetric() {
			if !strings.HasPrefix(labelOf(m, label), prefix) {
				continue
			}
			if c := m.GetCounter(); c != nil {
				sum += c.GetValue()
			}
		}
		out[n] = sum
	}

	return out, nil
}
