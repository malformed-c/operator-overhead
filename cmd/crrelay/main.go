// Command crrelay is ARM A of the operator-overhead benchmark: one
// controller-runtime manager that relays a single ConfigMap field.
//
// It reconciles exactly one pair of ConfigMaps — `src-<id>` and `dst-<id>` in
// one namespace — toward `dst.data.v == src.data.v`. That is the whole program.
// N of these processes are what "N operators" means here: N managers, N caches,
// N watch connections, and (with -leader-election) N Leases.
//
// ***THE CACHE IS SCOPED TO THIS INSTANCE'S OWN PAIR, WHICH IS CONSERVATIVE TO
// CONTROLLER-RUNTIME AND DELIBERATELY SO.*** An unscoped manager caches every
// ConfigMap in the namespace, so its resident memory would grow with the
// BENCHMARK'S OWN fixture population rather than with anything the operator
// does — at N=64 that is 128 objects in each of 64 caches, and the resulting
// curve would measure the harness. A label selector gives each manager the
// smallest cache that can serve its reconciler, which is the best case for this
// arm. Any overhead that survives that is real.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/malformed-c/operator-overhead/internal/relay"
)

func main() {
	var (
		id        string
		namespace string
		leaderEl  bool
		metrics   string
		resync    time.Duration
		shared    bool
		armTag    string
	)
	// ***`-arm` IS FOR THE SAMPLER AND THE PROGRAM IGNORES IT.*** The host-side
	// sampler selects processes by cmdline substring, and every controller-runtime
	// arm runs THIS binary — so before this flag existed the selector keyed on
	// `-leader-election=false`, which A2 and A3 both carry. One arm's memory would
	// have landed in the other's column with nothing in the numbers looking wrong.
	// An explicit identity is cheaper than a selector that has to stay clever.
	flag.StringVar(&armTag, "arm", "", "identity for the host-side sampler; unused by the program")
	flag.BoolVar(&shared, "shared", false,
		"ONE manager for every pair in the namespace, instead of one per pair (arm A3)")
	flag.StringVar(&id, "id", "", "instance id; reconciles src-<id> -> dst-<id> (required)")
	flag.StringVar(&namespace, "namespace", relay.Namespace, "namespace holding the relay pair")
	flag.BoolVar(&leaderEl, "leader-election", false, "acquire a Lease before serving (arm A1)")
	flag.StringVar(&metrics, "metrics-addr", ":8080", "address for /metrics; the client-side request counters live here")
	flag.DurationVar(&resync, "resync", 10*time.Minute, "informer resync period")
	flag.Parse()

	if id == "" && !shared {
		exit("-id is required unless -shared")
	}
	_ = armTag

	log.SetLogger(zap.New(zap.UseDevMode(false)))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})).With("id", id)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		exit("scheme: %v", err)
	}

	// ***THE SELECTOR BOUNDS WHAT THE INFORMER HOLDS, THE NAMESPACE BOUNDS WHERE
	// IT LOOKS, AND BOTH ARE NEEDED.*** A namespace-scoped cache alone still
	// lists every ConfigMap in it. Set only one and the memory curve stops being
	// about the operator and starts being about the fixture population.
	// ***THE SHARED ARM WATCHES EVERY PAIR THROUGH ONE INFORMER, WHICH IS THE
	// WHOLE POINT OF IT.*** A per-instance arm scopes its cache to its own two
	// objects; this one scopes to "is a relay fixture at all" — so the cache holds
	// 2N objects in ONE process rather than 2 objects in each of N.
	sel := labels.SelectorFromSet(labels.Set{relay.LabelID: id})
	if shared {
		req, err := labels.NewRequirement(relay.LabelSide, selection.Exists, nil)
		if err != nil {
			exit("selector: %v", err)
		}
		sel = labels.NewSelector().Add(*req)
	}
	opts := ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			SyncPeriod:        &resync,
			DefaultNamespaces: map[string]cache.Config{namespace: {LabelSelector: sel}},
			// ConfigMap is the only kind this manager ever reads. Declaring it
			// keeps a stray Get on another kind from silently starting a second
			// informer — an unwatched cost in the arm being priced.
			ByObject: map[client.Object]cache.ByObject{
				&corev1.ConfigMap{}: {Label: sel},
			},
		},
		Metrics: metricsserver.Options{BindAddress: metrics},
		// HEALTH PROBES ARE OFF. They are a second HTTP listener per instance
		// and nothing here reads them; leaving them on would charge arm A for a
		// server the Perseid arm has no counterpart to.
		LeaderElection:          leaderEl,
		LeaderElectionID:        "overhead-relay-" + id,
		LeaderElectionNamespace: namespace,
		// controller-runtime's Lease defaults — 15s duration, 10s renew
		// deadline, 2s retry — are left alone ON PURPOSE. Arm A1 prices an
		// operator AS SHIPPED, not as tuned for a benchmark.
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), opts)
	if err != nil {
		exit("manager: %v", err)
	}

	r := &reconciler{Client: mgr.GetClient(), ns: namespace, log: logger, shared: shared}
	var pred predicate.Predicate = predicate.NewPredicateFuncs(func(client.Object) bool { return true })
	if !shared {
		r.src, r.dst = relay.SrcName(id), relay.DstName(id)
		pred = onlyThePair(r.src, r.dst)
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}, builder.WithPredicates(pred)).
		Named("relay").
		Complete(r); err != nil {
		exit("controller: %v", err)
	}

	logger.Info("relay starting", "shared", shared, "leaderElection", leaderEl)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		exit("manager exited: %v", err)
	}
}

// reconciler copies one field. It re-derives the answer from a fresh read every
// pass and remembers nothing between them — the same level-triggered contract
// the Perseid arm is held to, so the two arms differ in RUNTIME and not in
// semantics.
type reconciler struct {
	client.Client
	ns, src, dst string
	log          *slog.Logger
	// shared makes one reconciler serve every pair: the request names the object
	// that changed and the pair is derived from its id label, rather than being
	// fixed at construction.
	shared bool
}

func (r *reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	srcName, dstName := r.src, r.dst
	if r.shared {
		// ***THE PAIR COMES FROM THE REQUEST, NOT FROM CONSTRUCTION.*** Either
		// member waking is a reason to reconcile the pair, so the id is taken
		// from the name rather than assuming the source changed.
		name := req.Name
		id, ok := strings.CutPrefix(name, "src-")
		if !ok {
			if id, ok = strings.CutPrefix(name, "dst-"); !ok {
				return reconcile.Result{}, nil // not a relay fixture
			}
		}
		srcName, dstName = relay.SrcName(id), relay.DstName(id)
	}

	var src corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.ns, Name: srcName}, &src); err != nil {
		if apierrors.IsNotFound(err) {
			// ABSENT IS AN ANSWER, NOT A FAILURE. Requeuing here would spin
			// against a fixture that has not been created yet and charge this
			// arm for the harness's setup order.
			return reconcile.Result{}, nil
		}

		return reconcile.Result{}, err
	}
	want, ok := src.Data[relay.FieldV]
	if !ok {
		return reconcile.Result{}, nil
	}

	var dst corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.ns, Name: dstName}, &dst); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if dst.Data[relay.FieldV] == want {
		// ***THE IDEMPOTENCE GUARD IS THE POINT OF THE LINE.*** Writing
		// unconditionally is what made radiant's own Perseid controller the
		// third-largest consumer of idle apiserver traffic on this cluster
		// (ADR-0098). An arm that wrote every pass would lose the API-volume
		// column to its own defect rather than to its architecture, and the
		// result would be worthless.
		return reconcile.Result{}, nil
	}

	patched := dst.DeepCopy()
	if patched.Data == nil {
		patched.Data = map[string]string{}
	}
	patched.Data[relay.FieldV] = want
	// ***STAMPED AS LATE AS POSSIBLE, AND IN THE SAME PATCH.*** The clock is read
	// on the line before the write is issued, so `reaction` covers notice and
	// decide and excludes the round trip. Putting it in the same patch is what
	// keeps this arm at ONE apiserver request per change — a second write for the
	// timestamp would show up in the API column as if the relay were amplifying.
	// SAME ENCODING AS THE PERSEID ARM, `<value>@<clock>`, though this arm cannot
	// desynchronise — the stamp and the value are in one PATCH. One format means
	// one parser in the harness rather than two that must agree.
	patched.Data[relay.FieldT] = want + "@" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	if err := r.Patch(ctx, patched, client.MergeFrom(&dst)); err != nil {
		return reconcile.Result{}, err
	}
	r.log.Debug("relayed", "v", want)

	return reconcile.Result{}, nil
}

// onlyThePair drops events for anything but this instance's two ConfigMaps.
//
// REDUNDANT WITH THE LABEL SELECTOR AND KEPT ANYWAY: the selector bounds what
// the informer holds, this bounds what the workqueue does, and only the second
// survives a mislabelled fixture. A stray object carrying the label would
// otherwise enqueue a reconcile whose cost lands in this arm's CPU column.
func onlyThePair(src, dst string) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == src || o.GetName() == dst
	})
}

func exit(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "crrelay: "+format+"\n", args...)
	os.Exit(1)
}
