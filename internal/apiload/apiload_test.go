package apiload

import "testing"

// A trimmed but otherwise verbatim controller-runtime exposition. The
// neighbouring families are here on purpose: a parser that summed every counter
// on the endpoint would pass a single-family fixture and over-count in
// production.
const exposition = `# HELP go_goroutines Number of goroutines that currently exist.
# TYPE go_goroutines gauge
go_goroutines 41
# HELP controller_runtime_reconcile_total Total number of reconciliations per controller
# TYPE controller_runtime_reconcile_total counter
controller_runtime_reconcile_total{controller="relay",result="success"} 812
# HELP rest_client_requests_total Number of HTTP requests, partitioned by status code, method, and host.
# TYPE rest_client_requests_total counter
rest_client_requests_total{code="200",host="192.168.50.2:6443",method="GET"} 14
rest_client_requests_total{code="200",host="192.168.50.2:6443",method="PATCH"} 97
rest_client_requests_total{code="409",host="192.168.50.2:6443",method="PATCH"} 3
rest_client_requests_total{code="200",host="192.168.50.2:6443",method="PUT"} 226
`

func TestParseSumsOnlyTheClientCounter(t *testing.T) {
	r, err := Parse(exposition)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !r.Found {
		t.Fatal("Found is false on an exposition that carries the family")
	}
	if want := 14.0 + 97 + 3 + 226; r.Requests != want {
		t.Errorf("Requests = %v, want %v (the reconcile counter must not be summed in)", r.Requests, want)
	}
	// The code split is what tells a busy run from a conflicting one.
	if r.ByCode["409"] != 3 {
		t.Errorf("ByCode[409] = %v, want 3", r.ByCode["409"])
	}
	if r.ByCode["200"] != 337 {
		t.Errorf("ByCode[200] = %v, want 337", r.ByCode["200"])
	}
}

// ADR-0098 protocol step 4: "a zero means either no events or a broken
// instrument until the controls distinguish those cases". Found is that
// distinction, so an endpoint with no client counter must not read as an arm
// that made no requests.
func TestParseAbsentFamilyIsNotZeroTraffic(t *testing.T) {
	r, err := Parse("# TYPE go_goroutines gauge\ngo_goroutines 41\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.Found {
		t.Error("Found is true for an exposition with no client counter")
	}
	if _, err := Delta(r, r); err == nil {
		t.Error("Delta over an absent family must refuse rather than return 0")
	}
}

func TestDeltaRefusesACounterReset(t *testing.T) {
	before := Reading{Found: true, Requests: 500, Endpoint: "http://10.0.0.1:8080/metrics"}
	after := Reading{Found: true, Requests: 12, Endpoint: "http://10.0.0.1:8080/metrics"}

	// A restarted pod's counter starts over. Clamping to zero would turn a void
	// window into "this arm made no requests" — the exact shape of an
	// unattributable number the whole package exists to avoid.
	if _, err := Delta(before, after); err == nil {
		t.Fatal("Delta accepted a counter that went backwards")
	}

	got, err := Delta(before, Reading{Found: true, Requests: 560})
	if err != nil {
		t.Fatalf("Delta on a normal window: %v", err)
	}
	if got != 60 {
		t.Errorf("Delta = %v, want 60", got)
	}
}
