// Package bench builds the benchmark's cluster state and measures a run.
//
// The population is a FUNCTION OF N and nothing else: `Fixtures` creates N
// ConfigMap pairs, `UpArm` creates N workers for one arm, and `Down` removes
// them. ADR-0098's protocol step 2 — "hold variables fixed, vary one intended
// factor at a time" — is why those are three calls rather than a script: the
// fixtures do not change when the arm does, so the arms see identical objects.
package bench

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"

	"github.com/malformed-c/operator-overhead/internal/relay"
)

// Client is the harness's own connection to the apiserver.
//
// ***ITS TRAFFIC IS NOT IN ANY ARM'S COLUMN, AND THAT IS WHY THE ARMS ARE
// MEASURED CLIENT-SIDE.*** The harness writes every source ConfigMap and
// watches every destination one, which at N=64 and 1 Hz is more apiserver
// traffic than the arms make between them. A server-side aggregate would count
// all of it as "the benchmark"; `rest_client_requests_total` read from each
// arm's own process cannot see the harness at all.
func Client(kubeconfig string) (*kubernetes.Clientset, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("bench: kubeconfig: %w", err)
	}
	// The harness is the load generator. Client-go's default 5 QPS would make
	// the RATE a property of the client's throttle rather than of the flag, and
	// a run whose writes were throttled would report latency for a workload
	// nobody asked for.
	cfg.QPS, cfg.Burst = 400, 800
	cfg.UserAgent = "operator-overhead-bench"

	return kubernetes.NewForConfig(cfg)
}

// Fixtures reconciles the namespace's ConfigMap pairs to exactly n.
//
// IDEMPOTENT AND SUBTRACTIVE. Running it at 8 after a run at 64 deletes the 56
// extra pairs, because a leftover fixture is an object the arms' caches would
// or would not hold depending on a previous run — which is precisely a variable
// that moved between samples without being named.
func Fixtures(ctx context.Context, cs *kubernetes.Clientset, n int) error {
	api := cs.CoreV1().ConfigMaps(relay.Namespace)

	for i := range n {
		id := relay.ID(i)
		for _, side := range []struct{ name, kind string }{
			{relay.SrcName(id), relay.SideSrc},
			{relay.DstName(id), relay.SideDst},
		} {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      side.name,
					Namespace: relay.Namespace,
					Labels: map[string]string{
						relay.LabelID:   id,
						relay.LabelSide: side.kind,
					},
				},
				// ***THE SOURCE STARTS EMPTY AND SO DOES THE DESTINATION.***
				// Seeding the source with a value would have every arm converge
				// during the settle window, so the first measured write would be
				// the only one that could be slow. An empty pair converges on the
				// first load tick, in every arm, from the same state.
				Data: map[string]string{},
			}
			if _, err := api.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
				if !apierrors.IsAlreadyExists(err) {
					return fmt.Errorf("bench: create %s: %w", side.name, err)
				}
			}
		}
	}

	// Remove pairs above n. Listed by the benchmark's own label so a ConfigMap
	// somebody else put in this namespace is never deleted.
	list, err := api.List(ctx, metav1.ListOptions{LabelSelector: relay.LabelSide})
	if err != nil {
		return fmt.Errorf("bench: list fixtures: %w", err)
	}
	for _, cm := range list.Items {
		id := cm.Labels[relay.LabelID]
		if idx, ok := indexOf(id); ok && idx < n {
			continue
		}
		if err := api.Delete(ctx, cm.Name, metav1.DeleteOptions{}); err != nil &&
			!apierrors.IsNotFound(err) {
			return fmt.Errorf("bench: delete stale fixture %s: %w", cm.Name, err)
		}
	}

	return nil
}

func indexOf(id string) (int, bool) {
	var n int
	if id == "" {
		return 0, false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}

	return n, true
}

// SetSource writes one value into every source ConfigMap, and returns when the
// last write is acknowledged.
//
// UPDATE, NOT PATCH, and read-modify-write through client-go's retry helper: a
// conflict here is the harness racing itself across ticks, and swallowing it
// would drop a load tick from a window whose denominator has already been
// written down.
func SetSource(ctx context.Context, cs *kubernetes.Clientset, n int, value string) error {
	api := cs.CoreV1().ConfigMaps(relay.Namespace)
	for i := range n {
		name := relay.SrcName(relay.ID(i))
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cm, err := api.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if cm.Data == nil {
				cm.Data = map[string]string{}
			}
			cm.Data[relay.FieldV] = value
			_, err = api.Update(ctx, cm, metav1.UpdateOptions{})

			return err
		})
		if err != nil {
			return fmt.Errorf("bench: set %s: %w", name, err)
		}
	}

	return nil
}

// UpArm creates n workers for one arm and waits for them to be Ready.
func UpArm(ctx context.Context, cs *kubernetes.Clientset, arm string, n int, image string) error {
	if !relay.IsCR(arm) {
		return fmt.Errorf("bench: UpArm: %q is not a controller-runtime arm; "+
			"the Perseid arm is created by UpPerseids", arm)
	}
	api := cs.CoreV1().Pods(relay.Namespace)
	leader := arm == relay.ArmLeader

	// ***ONE POD FOR THE SHARED ARM, WHATEVER N IS.*** That is the arm: one
	// manager, one informer, one client, one connection, N pairs.
	if arm == relay.ArmShared {
		pod := crPod(arm, "shared", image, false)
		pod.Spec.Containers[0].Args = []string{
			"-arm=" + arm,
			"-shared",
			"-namespace=" + relay.Namespace,
			"-leader-election=false",
			"-metrics-addr=:8080",
		}
		if _, err := api.Create(ctx, pod, metav1.CreateOptions{}); err != nil &&
			!apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("bench: create pod %s: %w", pod.Name, err)
		}

		return nil
	}

	for i := range n {
		id := relay.ID(i)
		pod := crPod(arm, id, image, leader)
		if _, err := api.Create(ctx, pod, metav1.CreateOptions{}); err != nil &&
			!apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("bench: create pod %s: %w", pod.Name, err)
		}
	}

	return nil
}

// PodName is the worker for instance id in arm.
func PodName(arm, id string) string { return arm + "-" + id }

func crPod(arm, id, image string, leader bool) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PodName(arm, id),
			Namespace: relay.Namespace,
			Labels: map[string]string{
				relay.LabelArm: arm,
				relay.LabelID:  id,
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: "relay",
			// See relay.NodeHostLabel: a pod scheduled to another machine is a
			// process the sampler cannot see, and its absence would read as an
			// arm that costs less.
			NodeSelector:  map[string]string{relay.NodeHostLabel: relay.Host},
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name:            "relay",
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				// ***THE LEADER-ELECTION FLAG IS ALWAYS SPELLED OUT, INCLUDING
				// WHEN IT IS FALSE.*** The host-side sampler selects processes
				// by cmdline substring, and A1 and A2 run the same binary — so
				// an omitted `=false` would make the two arms indistinguishable
				// in /proc and a stray pod from one run would be counted into
				// the other's memory total.
				Args: []string{
					"-arm=" + arm,
					"-id=" + id,
					"-namespace=" + relay.Namespace,
					fmt.Sprintf("-leader-election=%t", leader),
					"-metrics-addr=:8080",
				},
				Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8080}},
				// NO RESOURCE LIMITS. A limit would cap the quantity being
				// measured, and a memory limit in particular changes Go's GC
				// behaviour — the benchmark would then report the limit.
			}},
		},
	}
}

// WaitPodsReady blocks until every pod carrying the arm's label is Ready, or
// the context expires.
//
// ***READINESS IS THE START LINE, NOT POD CREATION.*** A manager that has not
// finished its initial List/Watch has neither its cache nor its steady-state
// memory, and a window opened at creation time measures startup — which is a
// real question, and a DIFFERENT one from the idle question this benchmark
// opens with (ADR-0098 protocol step 7).
func WaitPodsReady(ctx context.Context, cs *kubernetes.Clientset, arm string, want int) error {
	want = relay.Instances(arm, want)
	sel := relay.LabelArm + "=" + arm
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	for {
		pods, err := cs.CoreV1().Pods(relay.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return fmt.Errorf("bench: list %s pods: %w", arm, err)
		}
		ready := 0
		var notReady []string
		for _, p := range pods.Items {
			if podReady(&p) {
				ready++
			} else {
				notReady = append(notReady, p.Name+"="+string(p.Status.Phase))
			}
		}
		if ready == want {
			return nil
		}
		select {
		case <-ctx.Done():
			// ***"NOT READY" AND "NOT THERE" ARE DIFFERENT FAILURES AND THE FIRST
			// VERSION OF THIS COULD NOT TELL THEM APART.*** At N=64 arm A1 timed
			// out with `60/64 ready ... not ready:` — an EMPTY list, because the
			// four missing pods did not exist to be unready. That message sends
			// you looking at container startup when the object was never created,
			// or was created and deleted.
			missing := want - len(pods.Items)
			detail := fmt.Sprintf("%d exist, %d of those are not Ready (%s)",
				len(pods.Items), len(notReady), strings.Join(notReady, " "))
			if missing > 0 {
				detail = fmt.Sprintf("%d of %d pods DO NOT EXIST; of the %d that do, %d are not "+
					"Ready (%s)", missing, want, len(pods.Items), len(notReady), strings.Join(notReady, " "))
			}

			return fmt.Errorf("bench: %d/%d %s pods ready after waiting: %s", ready, want, arm, detail)
		case <-tick.C:
		}
	}
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}

	return false
}

// PodIPs maps instance id to pod IP for one arm, skipping pods without one.
func PodIPs(ctx context.Context, cs *kubernetes.Clientset, arm string) (map[string]string, error) {
	pods, err := cs.CoreV1().Pods(relay.Namespace).List(ctx,
		metav1.ListOptions{LabelSelector: relay.LabelArm + "=" + arm})
	if err != nil {
		return nil, fmt.Errorf("bench: list %s pods: %w", arm, err)
	}
	out := make(map[string]string, len(pods.Items))
	for _, p := range pods.Items {
		if p.Status.PodIP != "" {
			out[p.Labels[relay.LabelID]] = p.Status.PodIP
		}
	}

	return out, nil
}

// Down removes one arm's workers and WAITS for them to be gone. Fixtures are
// left alone: the next arm needs the identical ones, and recreating them between
// arms is a variable moving.
//
// ═══════════════════════════════════════════════════════════════════════════
// ***THE WAIT IS NOT POLITENESS. A TERMINATING POD IS STILL `Ready`, SO THE
// NEXT `up` RETURNS IMMEDIATELY AND MEASURES A PROCESS THAT IS DYING.***
// Measured: `down` then `up` for the shared arm at N=1 — the pod name is the
// same, so Create hit AlreadyExists and was ignored, `WaitPodsReady` counted the
// OLD pod as ready, and the run reached its convergence control against a
// process that was already exiting. The control caught it —
//
//	convergence control FAILED: 1/1 destinations never took the value in 1m3s
//
// — which is the control doing exactly its job, on a fault that was the
// harness's rather than the arm's. The identical hazard was fixed for Perseids
// (DownPerseids waits on the finalizer) and left here, because arm A's pods have
// no finalizer and appeared to delete instantly. They do not.
// ═══════════════════════════════════════════════════════════════════════════
func Down(ctx context.Context, cs *kubernetes.Clientset, arm string) error {
	api := cs.CoreV1().Pods(relay.Namespace)
	sel := metav1.ListOptions{LabelSelector: relay.LabelArm + "=" + arm}

	if err := api.DeleteCollection(ctx, metav1.DeleteOptions{}, sel); err != nil &&
		!apierrors.IsNotFound(err) {
		return fmt.Errorf("bench: delete %s pods: %w", arm, err)
	}

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	deadline := time.Now().Add(3 * time.Minute)

	for {
		pods, err := api.List(ctx, sel)
		if err != nil {
			return fmt.Errorf("bench: list %s pods while deleting: %w", arm, err)
		}
		if len(pods.Items) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bench: %d %s pods still terminating after 3m; the next arm would "+
				"see them as Ready and measure a dying process", len(pods.Items), arm)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
