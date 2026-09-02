package bench

import (
	"strings"
	"testing"

	"github.com/malformed-c/operator-overhead/internal/procsample"
	"github.com/malformed-c/operator-overhead/internal/relay"
)

// The two command lines below are verbatim from engix99 on 2026-09-01, trimmed
// only where the argument list is irrelevant. The supervisor's line ENDS with
// the workload's, which is why a substring selector matches a pod twice — and
// why every sample rejects Supervisors.
const (
	realGuest = "/crrelay -arm=a2-cr-noleader -id=000 -namespace=overhead " +
		"-leader-election=false -metrics-addr=:8080"

	// ***THE SHARED ARM ALSO RUNS WITH LEADER ELECTION OFF***, which is why the
	// selector cannot key on that flag. Before `-arm=` existed, A2's selector
	// (`-leader-election=false`) matched this line and one arm's memory would
	// have been counted into the other's column.
	realShared = "/crrelay -arm=a3-cr-shared -shared -namespace=overhead " +
		"-leader-election=false -metrics-addr=:8080"

	realSupervisor = "systemd-nspawn --console=read-only --keep-unit --register=yes " +
		"--machine=pod-b3bcee90-ce8b-4600-95b0-71698cb695b2-relay --hostname=a2-cr-noleader-000 " +
		"-- /usr/local/bin/meteor " + realGuest

	// A Perseid step, and one of the three unrelated Perseids already running on
	// this cluster. They differ ONLY in --pod-name.
	realPerseidStep = "/usr/local/bin/trail --p3 --component /module.wasm --rootfs / " +
		"--component-name step --pod-name perseid-relay-000 --namespace overhead --pawn engix99-e2e-2"
	realForeignPerseid = "/usr/local/bin/trail --p3 --component /module.wasm --rootfs / " +
		"--component-name step --pod-name perseid-scaler-v4 --namespace default --pawn engix99-e2e-2"
)

// matched is Collect's predicate, applied to one command line.
func matched(cmd, match string, reject []string) bool {
	if !strings.Contains(cmd, match) {
		return false
	}
	for _, r := range reject {
		if strings.Contains(cmd, r) {
			return false
		}
	}

	return true
}

func TestSupervisorIsNotCountedAsAnInstance(t *testing.T) {
	m := procMatch(relay.ArmNoLeader)

	if !matched(realGuest, m, Supervisors) {
		t.Errorf("the workload does not match its own arm's selector %q", m)
	}
	// The whole point: the supervisor's line contains the guest's, so it matches
	// the selector and MUST still be excluded. Without the rejection this
	// reported two processes per instance and added systemd-nspawn's memory to
	// every arm — a per-instance overstatement growing linearly with N.
	if matched(realSupervisor, m, Supervisors) {
		t.Error("systemd-nspawn matched as an instance; every pod would be counted twice")
	}
	if !strings.Contains(realSupervisor, m) {
		t.Fatal("fixture is wrong: the supervisor line no longer carries the guest argv, " +
			"which is the hazard this test exists for")
	}
}

// A1 and A2 run the same binary. If the selector did not carry the flag, a pod
// left standing from one arm would land in the other's totals — and nothing in
// the numbers would look wrong.
func TestTheTwoControllerRuntimeArmsAreDistinguishable(t *testing.T) {
	leaderCmd := strings.Replace(realGuest, "-arm=a2-cr-noleader", "-arm=a1-cr-leader", 1)
	leaderCmd = strings.Replace(leaderCmd, "-leader-election=false", "-leader-election=true", 1)

	if matched(leaderCmd, procMatch(relay.ArmNoLeader), Supervisors) {
		t.Error("a leader-electing worker matched the no-leader arm")
	}
	if matched(realGuest, procMatch(relay.ArmLeader), Supervisors) {
		t.Error("a non-leader-electing worker matched the leader arm")
	}
	if !matched(leaderCmd, procMatch(relay.ArmLeader), Supervisors) {
		t.Error("a leader-electing worker did not match its own arm")
	}
}

// This cluster already runs Perseids that are character-for-character identical
// apart from --pod-name. Counting them would attribute three strangers' memory
// to arm B — the exact unattributed-population failure ADR-0098 is about.
func TestForeignPerseidsAreExcluded(t *testing.T) {
	m := procMatch(relay.ArmPerseid)

	if !matched(realPerseidStep, m, Supervisors) {
		t.Errorf("this benchmark's own Perseid did not match %q", m)
	}
	if matched(realForeignPerseid, m, Supervisors) {
		t.Error("scaler-v4 matched arm B's selector; an unrelated program would be measured")
	}
}

// Every arm's selector must reject every other arm's workload, or the negative
// control in preflight can pass while the populations overlap.
func TestArmSelectorsArePairwiseDisjoint(t *testing.T) {
	cmds := map[string]string{
		relay.ArmNoLeader: realGuest,
		relay.ArmLeader: strings.Replace(
			strings.Replace(realGuest, "-arm=a2-cr-noleader", "-arm=a1-cr-leader", 1),
			"-leader-election=false", "-leader-election=true", 1),
		relay.ArmShared:  realShared,
		relay.ArmPerseid: realPerseidStep,
	}
	for arm, cmd := range cmds {
		for _, other := range relay.Arms {
			if other == arm {
				continue
			}
			if matched(cmd, procMatch(other), Supervisors) {
				t.Errorf("a %s workload matches the %s selector %q", arm, other, procMatch(other))
			}
		}
	}
}

// ***THE COLLISION `-arm=` EXISTS TO PREVENT.*** A2 and A3 both run with leader
// election off, so a selector keyed on that flag matches both — and the shared
// arm is ONE process against A2's N, so at N=64 the mistake is 63 processes of
// memory landing in the wrong column, in whichever direction the teardown
// happened to leave stale.
func TestTheSharedArmDoesNotCollideWithA2(t *testing.T) {
	if matched(realShared, procMatch(relay.ArmNoLeader), Supervisors) {
		t.Error("the shared arm matched A2's selector")
	}
	if matched(realGuest, procMatch(relay.ArmShared), Supervisors) {
		t.Error("an A2 worker matched the shared arm's selector")
	}
	if !matched(realShared, procMatch(relay.ArmShared), Supervisors) {
		t.Error("the shared arm did not match its own selector")
	}
	// The discriminator that CANNOT work — pinned as the mutation this guards
	// against, so a future selector cannot quietly key on it.
	if !strings.Contains(realShared, "-leader-election=false") ||
		!strings.Contains(realGuest, "-leader-election=false") {
		t.Fatal("fixture is wrong: both arms must carry -leader-election=false, " +
			"which is exactly why the selector cannot key on it")
	}
}

// An arm whose selector is empty would select every process on the host.
// procsample refuses that, and this pins that no arm can produce one.
func TestNoArmHasAnEmptySelector(t *testing.T) {
	for _, arm := range relay.Arms {
		if procMatch(arm) == "" {
			t.Errorf("arm %q has an empty selector", arm)
		}
		if _, err := procsample.Collect(procMatch(arm), Supervisors...); err != nil {
			t.Errorf("Collect(%q): %v", procMatch(arm), err)
		}
	}
}
