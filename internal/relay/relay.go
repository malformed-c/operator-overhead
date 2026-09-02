// Package relay is the workload's vocabulary, in one package because three
// programs have to spell it identically: `cmd/crrelay` reconciles it, `cmd/bench`
// creates the fixtures and measures against it, and arm B's component is
// configured from it. A label or a field name spelled twice is how two arms of a
// comparison quietly measure different things.
package relay

import "strconv"

const (
	Namespace = "overhead"

	// LabelID is on both members of a pair, and scopes arm A's cache — so it is
	// also what makes an instance's watch traffic attributable to that instance.
	LabelID  = "overhead.apsis/id"
	LabelArm = "overhead.apsis/arm"

	// LabelSide is for the HARNESS's own watch; the arms ignore it. A label
	// rather than a name prefix because the apiserver can filter on one, and a
	// harness watching every ConfigMap in the namespace would add its own traffic
	// to the apiserver it is measuring.
	LabelSide = "overhead.apsis/side"
	SideSrc   = "src"
	SideDst   = "dst"

	// NodeHostLabel pins every arm pod to one machine.
	//
	// ***WITHOUT IT THIS BENCHMARK SILENTLY MEASURES A FRACTION OF ITSELF.*** The
	// cluster spans three hosts, the scheduler will place a pod on any of them,
	// and the sampler reads the LOCAL /proc — so a pod elsewhere is missing from
	// its arm's total with nothing saying so.
	NodeHostLabel = "peri.apsis/host"
)

// Host is the machine every arm pod is pinned to. A var so `bench -host` can move
// a run without a rebuild; the sampler must then run there too.
var Host = "engix99"

const (
	// FieldV is the one relayed field.
	FieldV = "v"

	// FieldT is the operator's OWN clock at the moment it decided to write, in
	// epoch milliseconds. It splits convergence into the arm's part and the rest:
	//
	//	reaction = t - origin      notice + decide. THE ARM'S REFLEX.
	//	tail     = seen - t        the write's round trip, and the harness's watch
	//
	// A resident manager is woken by its own informer and a parked Perseid by a
	// wake index re-evaluating a resume — different mechanisms, and convergence
	// alone cannot separate the wake from the work.
	//
	// Both arms read the same physical clock, because every pawn is pinned to one
	// machine (see NodeHostLabel). On a multi-host run this measures NTP.
	FieldT = "t"
)

// The arms. Named constants rather than strings at call sites, because a mistyped
// arm produces an empty sample — a zero reading as "this arm costs nothing"
// instead of "nothing was measured".
const (
	ArmLeader   = "a1-cr-leader"   // controller-runtime, leader election ON
	ArmNoLeader = "a2-cr-noleader" // controller-runtime, leader election OFF
	ArmPerseid  = "b-perseid"      // a Perseid, parked between passes

	// ArmShared is ONE manager hosting N controllers — the steelman a sceptic
	// asks for first, since A1 and A2 model N separate vendors and that is one
	// reading rather than the only one.
	ArmShared = "a3-cr-shared"

	// ArmFused is ONE Perseid relaying every pair, so the memory question can be
	// asked with one process on each side. Without it, "N Perseids against one
	// manager" measures consolidation and calls it runtime.
	ArmFused = "b2-perseid-fused"
)

// Arms is the full set, in report order.
var Arms = []string{ArmLeader, ArmNoLeader, ArmShared, ArmPerseid, ArmFused}

// IsCR reports whether an arm has a /metrics endpoint of its own to scrape.
func IsCR(arm string) bool {
	return arm == ArmLeader || arm == ArmNoLeader || arm == ArmShared
}

// Instances is how many PROCESSES an arm runs at population n — one for the
// consolidated arms, whatever n is. The population control asserts
// `processes == Instances(arm, n)`.
func Instances(arm string, n int) int {
	if (arm == ArmShared || arm == ArmFused) && n > 0 {
		return 1
	}

	return n
}

// SrcName and DstName name the two halves of instance i's pair, derived from one
// int so a fixture, an arm-A flag and an arm-B config cannot disagree.
func SrcName(id string) string { return "src-" + id }

// DstName is SrcName's counterpart.
func DstName(id string) string { return "dst-" + id }

// ID is the canonical rendering of an instance index, zero-padded so names sort
// as a reader expects at N=64 and are a fixed width in every table.
func ID(i int) string {
	s := strconv.Itoa(i)
	for len(s) < 3 {
		s = "0" + s
	}

	return s
}

// CMPath is the canonical apiserver path for a ConfigMap in Namespace.
//
// ***THE PERSEID ARM'S `spec.writes` AND ITS `ensure` TARGET ARE THIS SAME
// STRING***, so rendering it in one place keeps an admission refusal from being a
// typo in a generator.
func CMPath(name string) string {
	return "/api/v1/namespaces/" + Namespace + "/configmaps/" + name
}
