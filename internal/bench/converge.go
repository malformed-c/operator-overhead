package bench

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/malformed-c/operator-overhead/internal/relay"
)

// ProveConvergence writes one value into every source and waits until every
// destination carries it. It is the LAST control before a window opens.
func ProveConvergence(ctx context.Context, cs *kubernetes.Clientset, n int, timeout time.Duration) (time.Duration, error) {
	// A sentinel that cannot collide with a load value: `EncodeValue` renders
	// `<seq>|<nanos>`, and nothing in a measured window writes this shape.
	want := fmt.Sprintf("preflight|%d", time.Now().UnixNano())
	if err := SetSource(ctx, cs, n, want); err != nil {
		return 0, fmt.Errorf("bench: convergence control could not write the sources: %w", err)
	}

	api := cs.CoreV1().ConfigMaps(relay.Namespace)
	started := time.Now()
	deadline := started.Add(timeout)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for {
		list, err := api.List(ctx, metav1.ListOptions{
			LabelSelector: relay.LabelSide + "=" + relay.SideDst,
		})
		if err != nil {
			return 0, fmt.Errorf("bench: convergence control: list destinations: %w", err)
		}
		var behind []string
		for _, cm := range list.Items {
			if idx, ok := indexOf(cm.Labels[relay.LabelID]); !ok || idx >= n {
				continue // a fixture above N; not this run's population
			}
			if cm.Data[relay.FieldV] != want {
				behind = append(behind, cm.Name)
			}
		}
		if len(behind) == 0 {
			return time.Since(started), nil
		}
		if time.Now().After(deadline) {
			// ***NAMED, AND CAPPED, SO THE MESSAGE IS READABLE AT N=64.*** A
			// list of sixty-four names is not a diagnosis; the count plus a
			// sample is.
			shown := behind
			if len(shown) > 5 {
				shown = append(shown[:5:5], fmt.Sprintf("… and %d more", len(behind)-5))
			}

			return 0, fmt.Errorf("bench: convergence control FAILED: %d/%d destinations never "+
				"took the value in %s: %s. The arm is up and is not doing its job — every other "+
				"control here asserts a marker (pods Ready, processes matched, metrics present) "+
				"and none of them can see this. Measuring now would report an arm that does "+
				"nothing as an arm that is cheap",
				len(behind), n, timeout, strings.Join(shown, " "))
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-tick.C:
		}
	}
}
