package bench

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/malformed-c/operator-overhead/internal/relay"
)

// ***THIS GUARD HAD NEVER FAILED WHEN IT WAS WRITTEN, AND A GUARD NOBODY HAS
// SEEN FAIL IS ONE NOBODY KNOWS WORKS.*** Proving it needs a pod on a second
// physical host, which cannot be arranged on the real cluster without placing
// one there — so the population is faked and the LOGIC is what is under test.
func nodes() []*corev1.Node {
	return []*corev1.Node{
		node("engix99-trail-1", "engix99"),
		node("engix99-e2e-2", "engix99"),
		node("engifire", "engifire"),
	}
}

func node(name, host string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{relay.NodeHostLabel: host},
	}}
}

func pod(name, nodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: relay.Namespace},
		Spec:       corev1.PodSpec{NodeName: nodeName},
	}
}

func TestPodPlacementRefusesAnOffHostPod(t *testing.T) {
	cs := fake.NewSimpleClientset(
		nodes()[0], nodes()[1], nodes()[2],
		pod(PerseidPodPrefix+PerseidPrefix+"000", "engix99-trail-1"),
		pod(PerseidPodPrefix+PerseidPrefix+"001", "engifire"), // the hazard
	)

	placement, err := PodPlacement(context.Background(), cs, "engix99", relay.ArmPerseid)
	if err == nil {
		t.Fatal("a pod on engifire was accepted; the sampler cannot see it and the arm " +
			"would read as CHEAPER rather than as incomplete")
	}
	// The message has to name the pod AND the node, because "Found != N" is the
	// uninformative version this exists to replace.
	for _, want := range []string{"perseid-relay-001", "engifire", "CHEAPER"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
	// Placement is returned EVEN ON REFUSAL, so the record shows where things
	// landed rather than only that something was wrong.
	if placement[PerseidPodPrefix+PerseidPrefix+"001"] != "engifire" {
		t.Errorf("placement not reported on refusal: %v", placement)
	}
}

func TestPodPlacementAcceptsAnOnHostPopulation(t *testing.T) {
	cs := fake.NewSimpleClientset(
		nodes()[0], nodes()[1], nodes()[2],
		pod(PerseidPodPrefix+PerseidPrefix+"000", "engix99-trail-1"),
		pod(PerseidPodPrefix+PerseidPrefix+"001", "engix99-e2e-2"),
	)

	placement, err := PodPlacement(context.Background(), cs, "engix99", relay.ArmPerseid)
	if err != nil {
		t.Fatalf("two pods on two engix99 PAWNS must pass — the host is what matters, "+
			"not the node: %v", err)
	}
	if len(placement) != 2 {
		t.Errorf("placement = %v, want both pods recorded", placement)
	}
}

// A pod that is not this benchmark's must not be judged: radiant's other
// programs land where they land and are none of this run's business.
func TestPodPlacementIgnoresForeignPods(t *testing.T) {
	cs := fake.NewSimpleClientset(
		nodes()[0], nodes()[2],
		pod(PerseidPodPrefix+PerseidPrefix+"000", "engix99-trail-1"),
		pod(PerseidPodPrefix+"scaler-v4", "engifire"), // someone else's, off-host
		pod("a2-cr-noleader-000", "engifire"),         // not a perseid pod at all
	)

	if _, err := PodPlacement(context.Background(), cs, "engix99", relay.ArmPerseid); err != nil {
		t.Fatalf("a foreign Perseid off-host must not fail this run: %v", err)
	}
}

// An unscheduled pod has no node yet. That is a pod to WAIT for, not a pod on
// the wrong host — reporting it as off-host would send the operator hunting a
// placement problem during normal startup.
func TestPodPlacementSkipsUnscheduled(t *testing.T) {
	cs := fake.NewSimpleClientset(
		nodes()[0],
		pod(PerseidPodPrefix+PerseidPrefix+"000", ""),
	)

	placement, err := PodPlacement(context.Background(), cs, "engix99", relay.ArmPerseid)
	if err != nil {
		t.Fatalf("an unscheduled pod must not read as off-host: %v", err)
	}
	if len(placement) != 0 {
		t.Errorf("placement = %v, want empty for an unscheduled pod", placement)
	}
}
