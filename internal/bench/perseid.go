package bench

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/malformed-c/operator-overhead/internal/relay"
)

// PerseidPrefix names this benchmark's Perseids: `relay-<id>`.
const PerseidPrefix = "relay-"

// PerseidPodPrefix is what radiant names a Perseid's pod. It is radiant's
// constant, not this benchmark's; spelled here because the process selector is
// built from the two together.
const PerseidPodPrefix = "perseid-"

// PodPrefixFor is the pod-name prefix radiant gives an arm's programs.
//
// ***THE FUSED ARM'S POD IS NOT NAMED perseid-relay-ANYTHING.*** It is ONE
// program called `fused`, so every check keyed on the relay prefix looked at an
// empty population — and one of them then refused the run for a reason that was
// true of the check rather than of the arm.
func PodPrefixFor(arm string) string {
	if arm == relay.ArmFused {
		return PerseidPodPrefix + FusedName
	}

	return PerseidPodPrefix + PerseidPrefix
}

// PerseidGVR is the Perseid custom resource.
var PerseidGVR = schema.GroupVersionResource{
	Group:    "radiant.apsis",
	Version:  "v1",
	Resource: "perseids",
}

// PerseidCapabilities is arm B's GRANT: what the operator PERMITS, against the
// artifact's imports, which are what it DEMANDS. Admission refuses the
// difference.
//
// ***AUTHORED, NEVER DERIVED.*** A grant computed from the component would be
// self-certifying — the artifact permitting itself, both gates collapsing — so a
// new import means a REFUSAL rather than a quiet widening.
//
//	observe             `get` and `now`
//	ensure              the one write
//	types               the shared `obs` variant. Types-only, so it confers
//	                    nothing; the component model materialises a TYPE
//	                    dependency as an INTERFACE IMPORT, and an ungranted
//	                    import does not instantiate
//	observe-configmaps  permission to point `observe.get` at a ConfigMap
//
// The last has no matching import, deliberately: it is empty, a capability
// rather than a call, and naming it here is what confers it. So the grant is a
// strict superset of the demand — the only shape a per-kind read permission can
// take. `workloads` and `observe-secrets` are absent because nothing in the code
// justifies them.
var PerseidCapabilities = []string{
	"radiant:reconcile/ensure@0.1.0",
	"radiant:reconcile/observe-configmaps@0.1.0",
	"radiant:reconcile/observe@0.1.0",
	"radiant:reconcile/types@0.1.0",
}

// EnvAnnotation is what a Perseid pod carries when its wasm profile passes
// `spec.config` through to the guest.
//
// ***A POD WITHOUT IT LOOKS EXACTLY LIKE A POD WHOSE CONFIG IS EMPTY.*** The env
// block can be present on the container and on trail's argv while the guest sees
// `Object.keys(process.env).length == 0` — both halves true, the seam between
// them the defect. The pod spec carries the forwarding decision, so a pod keeps
// the shape it was born with and must be recreated rather than waited on.
//
// Checking this is what separates "the host is broken" from "this pod is older
// than the host", so a run refuses with the remedy instead of producing a
// plausible zero.
const EnvAnnotation = "trail.apsis/env"

// EnvForward is the ONLY value of EnvAnnotation that forwards the environment.
//
// ***PRESENCE IS NOT THE SIGNAL.*** trail's `normStrip` forwards for exactly
// "all" and strips for everything else, including "":
//
//	no annotation      --no-env     nothing forwarded
//	annotation ""      --no-env     IDENTICAL
//	annotation "all"   forwards
//
// A control asserting the KEY EXISTS would find the annotation, open the window,
// and report an empty guest environment as a real measurement. So
// ConfigReachesGuest asserts the VALUE.
const EnvForward = "all"

// ConfigReachesGuest reports whether every Perseid pod was built by a runtime
// that passes spec.config through, and names what to do when one was not.
func ConfigReachesGuest(ctx context.Context, cs *kubernetes.Clientset, arm string) error {
	prefix := PodPrefixFor(arm)
	pods, err := cs.CoreV1().Pods(relay.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("bench: list perseid pods: %w", err)
	}
	var stale []string
	seen := 0
	for _, p := range pods.Items {
		if !strings.HasPrefix(p.Name, prefix) {
			continue
		}
		seen++
		// THE VALUE, NOT THE KEY. See EnvForward.
		if p.Annotations[EnvAnnotation] != EnvForward {
			stale = append(stale, fmt.Sprintf("%s(%s=%q)", p.Name, EnvAnnotation, p.Annotations[EnvAnnotation]))
		}
	}
	if seen == 0 {
		return fmt.Errorf("bench: no %s* pods exist, so whether spec.config reaches the "+
			"guest could not be checked at all", prefix)
	}
	if len(stale) > 0 {
		return fmt.Errorf("bench: %d/%d perseid pods do not carry %s=%q: %s. An absent "+
			"annotation and an EMPTY one are the same pod — trail forwards for exactly %q and "+
			"strips for everything else. Either the node predates periapsis 0db09eebc, or the "+
			"pod was created before the roll and kept the old spec: delete the pod (or the "+
			"Perseid) and let it be recreated. Measuring now would report an empty guest "+
			"environment as a broken fix",
			len(stale), seen, EnvAnnotation, EnvForward, strings.Join(stale, " "), EnvForward)
	}

	return nil
}

// PerseidsHealthy refuses if any of this benchmark's Perseids is in a phase that
// means the PROGRAM failed, whatever its writes did.
//
// ***THE STEP CAN SUCCEED AND THE PROGRAM STILL FAIL, WITH EVERY OTHER CONTROL
// GREEN.*** A pass can run, the write be Applied, and the program then die on the
// PARK — a resume is evaluated with the PROGRAM's own read capabilities, so a
// step parking on an object its manifest grants no read for fails after a fully
// successful pass:
//
//	PerseidFailed: resume expression could not be evaluated:
//	  aperture: unresolved identifier: "Get"
//
// ***NO COST METRIC CAN SEE THAT.*** Convergence passes, processes match, the pod
// is Ready. A benchmark reading only its own instruments reports a program that
// terminated in failure as an arm that relayed and then went quiet — which in a
// cost comparison reads as EFFICIENT. The phase is a STATE rather than a metric,
// and states are the only things that can contradict a flattering number.
func PerseidsHealthy(ctx context.Context, dyn dynamic.Interface, when string) error {
	api := dyn.Resource(PerseidGVR).Namespace(relay.Namespace)
	list, err := api.List(ctx, metav1.ListOptions{
		LabelSelector: relay.LabelArm + " in (" + relay.ArmPerseid + "," + relay.ArmFused + ")",
	})
	if err != nil {
		return fmt.Errorf("bench: list perseids at %s: %w", when, err)
	}

	var bad []string
	for _, obj := range list.Items {
		phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
		switch phase {
		case "Running", "Parked":
			continue
		}
		// `admissionReason` is the ONE status field written unconditionally, so
		// for a Failed program it carries the failure text — the object has no
		// other message field.
		reason, _, _ := unstructured.NestedString(obj.Object, "status", "admissionReason")
		bad = append(bad, fmt.Sprintf("%s=%s(%s)", obj.GetName(), phase, reason))
	}
	if len(bad) == 0 {
		return nil
	}
	shown := bad
	if len(shown) > 4 {
		shown = append(shown[:4:4], fmt.Sprintf("… and %d more", len(bad)-4))
	}

	return fmt.Errorf("bench: %d/%d perseids are not Running or Parked at %s: %s. A step can "+
		"succeed and the program still fail — a park whose resume names an object the manifest "+
		"grants no read for dies AFTER a fully successful pass, with writes applied and the pod "+
		"Ready. Every other control here is green on that run",
		len(bad), len(list.Items), when, strings.Join(shown, " "))
}

// PodPlacement maps each of this benchmark's Perseid pods to the node it landed
// on, and refuses if any landed off the sampler's host.
//
// ***A POD ON ANOTHER HOST IS INVISIBLE TO THE SAMPLER, NOT SMALLER.***
// procsample reads the LOCAL /proc, so an instance elsewhere contributes no
// memory, no CPU and no connections, and the arm reads as cheaper. Both arms
// declare a nodeSelector, but a declaration is not a placement: this asserts
// where the pods actually landed.
//
// It also separates a per-node effect from VERSION SKEW. Nodes can carry
// different trail builds, so a pod on the wrong host may behave differently for
// reasons that have nothing to do with the arm — which is why the node is a
// column rather than an assumption.
// Takes kubernetes.Interface rather than *Clientset SO IT CAN BE FAKED: proving
// this guard refuses an off-host pod needs a pod on a second physical host, and
// arranging one on the real cluster means placing an instance where the sampler
// cannot see it — the exact condition the guard exists to prevent.
func PodPlacement(ctx context.Context, cs kubernetes.Interface, host, arm string) (map[string]string, error) {
	prefix := PodPrefixFor(arm)
	pods, err := cs.CoreV1().Pods(relay.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("bench: list perseid pods: %w", err)
	}
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("bench: list nodes: %w", err)
	}
	nodeHost := make(map[string]string, len(nodes.Items))
	for _, n := range nodes.Items {
		nodeHost[n.Name] = n.Labels[relay.NodeHostLabel]
	}

	placement := map[string]string{}
	var offHost []string
	for _, p := range pods.Items {
		if !strings.HasPrefix(p.Name, prefix) || p.Spec.NodeName == "" {
			continue
		}
		placement[p.Name] = p.Spec.NodeName
		if h := nodeHost[p.Spec.NodeName]; h != host {
			offHost = append(offHost, fmt.Sprintf("%s on %s (host %q)", p.Name, p.Spec.NodeName, h))
		}
	}
	if len(offHost) > 0 {
		shown := offHost
		if len(shown) > 4 {
			shown = append(shown[:4:4], fmt.Sprintf("… and %d more", len(offHost)-4))
		}

		return placement, fmt.Errorf("bench: %d perseid pods landed off %s: %s. The sampler reads "+
			"the LOCAL /proc, so those instances contribute no memory, CPU or connections and the "+
			"arm reads as CHEAPER rather than as incomplete. They may also be running a different "+
			"trail: rolls are per-host, so an off-host pod can predate the step-env fix and "+
			"ensure-all. A Perseid's pod is placed by the scheduler — radiant sets no nodeSelector "+
			"and the CRD has no placement field — so this cannot be pinned from here",
			len(offHost), host, strings.Join(shown, " "))
	}

	return placement, nil
}

// LanguageVersion is what `spec.language` declares: the aperture expression
// language this program's resume was written against. Admission refuses a
// declaration newer than the radiant evaluating it, before a pod and before a
// pass — a resume is assembled at RUNTIME, so no artifact carries the symbols it
// will emit and "SDK version <= radiant version" is the only honest shape.
//
// ⚠ ***ASSERTED HERE, NOT DERIVED.*** The SDKs export `LANGUAGE_VERSION` and a
// build refuses a manifest that disagrees with it. These components import the
// raw WIT rather than an SDK, so there is no constant to cross-check and the `1`
// below is a claim about what the component can emit. Safe today because the
// resume uses `Get`, `Fields` and `Now`, all version 1, and because getting it
// wrong is a refusal with a reason rather than a silent mismatch. If this adopts
// an SDK, take the number from the SDK.
const LanguageVersion = int64(1)

// PerseidName is the CR for instance id.
func PerseidName(id string) string { return PerseidPrefix + id }

// ComponentRef is the ingested artifact. ONE, for every instance: `spec.config`
// flattens to the pod's environment, so an artifact learns which pair it serves
// at runtime instead of having the paths compiled in. That is the same shape arm
// A has, where the instance is a `-id` flag — and it is what keeps size out of
// the scaling question for both arms.
func ComponentRef() string { return "relay:v1" }

// FusedComponentRef is arm B2's artifact: one program, every pair.
func FusedComponentRef() string { return "relay-fused:v1" }

// FusedName is the single CR arm B2 creates.
const FusedName = "fused"

// UpFused creates ONE Perseid that relays every pair.
//
// ***`spec.writes` LISTS EVERY DESTINATION, AND THAT IS THE POINT OF THE
// FIELD.*** The write boundary is per-path, so a program touching N objects
// declares N paths and admission refuses anything outside them — the same
// vocabulary `ensure` targets. A fused program is therefore not a loophole: it
// is a wider, explicitly stated grant.
func UpFused(ctx context.Context, dyn dynamic.Interface, n int) error {
	writes := make([]any, 0, n)
	for i := range n {
		writes = append(writes, relay.CMPath(relay.DstName(relay.ID(i))))
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "radiant.apsis/v1",
		"kind":       "Perseid",
		"metadata": map[string]any{
			"name":      FusedName,
			"namespace": relay.Namespace,
			"labels": map[string]any{
				relay.LabelArm: relay.ArmFused,
				relay.LabelID:  FusedName,
			},
		},
		"spec": map[string]any{
			"component":    FusedComponentRef(),
			"language":     LanguageVersion,
			"capabilities": toAny(PerseidCapabilities),
			"nodeSelector": map[string]any{relay.NodeHostLabel: relay.Host},
			"config": map[string]any{
				"RELAY_NS":    relay.Namespace,
				"RELAY_FIELD": relay.FieldV,
				"RELAY_COUNT": strconv.Itoa(n),
			},
			"writes":     writes,
			"maxSleepMs": int64(300000),
		},
	}}
	if _, err := dyn.Resource(PerseidGVR).Namespace(relay.Namespace).
		Create(ctx, obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("bench: create fused perseid: %w", err)
	}

	return nil
}

// Config keys the component reads out of its environment.
//
// ***NONE MAY BEGIN WITH `PERSEID_`.*** That prefix is reserved to the host
// (`validConfigKey`, internal/trailop/perseidpod.go) so the runtime can add its
// own variables later without a program having squatted on the name — a key
// that violates it is silently DROPPED from the pod's environment, and the step
// then sees an undefined variable rather than a rejected config.
const (
	ConfigSrc   = "RELAY_SRC"
	ConfigDst   = "RELAY_DST"
	ConfigField = "RELAY_FIELD"
)

// UpPerseids creates n Perseid CRs. Radiant launches their pods; this does not.
//
// ***THE POD IS RADIANT'S TO CREATE AND THAT IS THE ARM'S POINT.*** Arm A's
// workers are pods this harness creates directly, because a controller-runtime
// operator IS a deployment. An arm-B instance is a program handed to a cluster
// that already runs a host — so the harness applies a CR and waits, and the
// launch latency that follows is part of what the arm costs.
func UpPerseids(ctx context.Context, dyn dynamic.Interface, n int) error {
	api := dyn.Resource(PerseidGVR).Namespace(relay.Namespace)

	for i := range n {
		id := relay.ID(i)
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "radiant.apsis/v1",
			"kind":       "Perseid",
			"metadata": map[string]any{
				"name":      PerseidName(id),
				"namespace": relay.Namespace,
				"labels": map[string]any{
					relay.LabelArm: relay.ArmPerseid,
					relay.LabelID:  id,
				},
			},
			"spec": map[string]any{
				"component": ComponentRef(),
				"language":  LanguageVersion,
				// THE INSTANCE, AS DATA RATHER THAN AS A BUILD. Every value here
				// comes from internal/relay, the same package cmd/crrelay reads,
				// so the two arms cannot disagree about which objects they
				// operate on.
				"config": map[string]any{
					ConfigSrc:   relay.CMPath(relay.SrcName(id)),
					ConfigDst:   relay.CMPath(relay.DstName(id)),
					ConfigField: relay.FieldV,
				},
				"capabilities": toAny(PerseidCapabilities),
				// ***`spec.imports` IS DELIBERATELY ABSENT, AND ABSENT IS NOT EMPTY.***
				// The field is the submitter's CLAIM; perigeos publishes a
				// ComponentManifest from its own inspection at ingest, so admission
				// rests on evidence (`status.importSourcing: artifact-verified`).
				// Declaring here too would be a second inspection presented as a
				// promise. The cost is an ordering dependency — no declaration and no
				// manifest REFUSES (ADR-0076) — which `hack/perseid-build.sh` asserts.
				// THE WRITE BOUNDARY. One path, named in the same vocabulary
				// `ensure` targets — so the boundary and the effect are one
				// language rather than two dialects agreeing by accident. A
				// second Perseid claiming this path would withdraw this one's
				// admission, which is exactly the conflict detection the design
				// advertises and a reason the ids must not collide.
				"writes": []any{relay.CMPath(relay.DstName(id))},
				// A Perseid's pod is built by radiant, so without this the scheduler
				// places arm B while arm A is pinned — and an instance on a host whose
				// /proc the sampler cannot read is not smaller, it is INVISIBLE, so the
				// arm reads as cheaper. Same key arm A uses, so the two are pinned by one
				// vocabulary rather than two that agree by accident.
				"nodeSelector": map[string]any{relay.NodeHostLabel: relay.Host},
				// The backstop on a park. Left at the CRD default: a resume
				// condition that is too narrow, or a dropped watch, must degrade
				// to a slow poll rather than to a lost wakeup — and a benchmark
				// that shortened it would be measuring a poll it configured.
				"maxSleepMs": int64(300000),
			},
		}}
		// ***AlreadyExists IS AN ERROR HERE, NOT A SUCCESS.*** The pods in arm A
		// are level-triggered and idempotent; a Perseid is a SPEC, and an
		// existing object carries the previous run's. Accepting it silently is
		// what let two runs in a row measure a stale declaration. `Down` waits,
		// so reaching this means something really is left over.
		if _, err := api.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("bench: perseid %s already exists; it carries the previous "+
					"run's spec. Run `bench down -arm %s` first", PerseidName(id), relay.ArmPerseid)
			}

			return fmt.Errorf("bench: create perseid %s: %w", PerseidName(id), err)
		}
	}

	return nil
}

// DownPerseids deletes this benchmark's Perseids and WAITS for them to be gone.
//
// Selected by label rather than by name prefix, so a Perseid somebody else
// created called `relay-something` survives.
//
// ═══════════════════════════════════════════════════════════════════════════
// ***THE WAIT IS NOT POLITENESS. A DELETE THAT HAS NOT COMPLETED MAKES THE NEXT
// `up` A NO-OP, AND THE RUN THEN MEASURES THE PREVIOUS SPEC.*** Perseids carry
// `radiant.apsis/finalize`, so a delete sets `deletionTimestamp` and the object
// stays. `Create` then returns AlreadyExists, and treating that as "the object
// exists, which is the goal" leaves the next run's controls passing against an
// object built from the LAST run's spec — a terminating Perseid reported as the
// freshly-created one, carrying the previous spec and its refusal with it.
//
// ***A STUCK FINALIZER IS REPORTED WITH ITS CAUSE RATHER THAN WAITED OUT.***
// Found on this cluster 2026-09-01 and it is a radiant defect, not a benchmark
// one: a REFUSED Perseid never gets a pod (`launch` returns early on
// `PerseidRefused`, correctly), and its finalizer then blocks forever on
//
//	Finalizing: waiting for cleanup: perseidrun: pod has no address yet:
//	overhead/perseid-relay-000
//
// — a pod that will never exist, re-emitted every five seconds. The object is
// undeletable without stripping the finalizer by hand. This function names that
// state instead of hanging, because the fix is one `kubectl patch` and the
// message is the only thing that leads anyone to it.
// ═══════════════════════════════════════════════════════════════════════════
func DownPerseids(ctx context.Context, dyn dynamic.Interface) error {
	api := dyn.Resource(PerseidGVR).Namespace(relay.Namespace)
	sel := metav1.ListOptions{LabelSelector: relay.LabelArm + " in (" + relay.ArmPerseid + "," + relay.ArmFused + ")"}

	if err := api.DeleteCollection(ctx, metav1.DeleteOptions{}, sel); err != nil &&
		!apierrors.IsNotFound(err) {
		return fmt.Errorf("bench: delete perseids: %w", err)
	}

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	deadline := time.Now().Add(2 * time.Minute)

	for {
		list, err := api.List(ctx, sel)
		if err != nil {
			return fmt.Errorf("bench: list perseids while deleting: %w", err)
		}
		if len(list.Items) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			stuck := make([]string, 0, len(list.Items))
			for _, obj := range list.Items {
				phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
				stuck = append(stuck, obj.GetName()+"("+phase+")")
			}

			return fmt.Errorf("bench: %d perseids are still terminating after 2m: %s — "+
				"a Refused Perseid never gets a pod, and radiant's finalizer waits for one "+
				"forever (\"pod has no address yet\"). Clear it with:\n"+
				"  kubectl patch perseid -n %s <name> --type json "+
				"-p '[{\"op\":\"remove\",\"path\":\"/metadata/finalizers\"}]'",
				len(stuck), strings.Join(stuck, " "), relay.Namespace)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// WaitPerseidsRunning blocks until n Perseids have reached a phase that means a
// program is actually executing.
//
// ═══════════════════════════════════════════════════════════════════════════
// ***`Running` AND `Parked` BOTH COUNT, AND `Pending` DOES NOT.*** The phases
// are not a progress bar. `Parked` is a program that ran, concluded, and is
// asleep on a stated condition — the steady state this arm exists to
// demonstrate, so waiting for `Running` alone would wait forever on a converged
// relay. `Pending` means admitted-but-nothing-has-reported-running-it, and `""`
// means nobody has judged the object at all; opening a window on either measures
// a program that has not started.
//
// `Refused` and `Failed` are reported by name rather than waited out, because
// both are terminal until someone changes something — a benchmark that waited
// would hang for its whole timeout and then say "not ready".
// ═══════════════════════════════════════════════════════════════════════════
// ***THE ARM IS A PARAMETER BECAUSE THE FUSED ARM IS ONE OBJECT, NOT N.*** This
// selected `b-perseid` unconditionally and compared against n, so for
// `b2-perseid-fused` it listed nothing and waited for eight — a guaranteed
// timeout against a program that was already Parked and correct. The population
// a control counts must come from the same place the arm's population does.
func WaitPerseidsRunning(ctx context.Context, dyn dynamic.Interface, arm string, n int) error {
	want := relay.Instances(arm, n)
	api := dyn.Resource(PerseidGVR).Namespace(relay.Namespace)
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()

	for {
		list, err := api.List(ctx, metav1.ListOptions{
			LabelSelector: relay.LabelArm + "=" + arm,
		})
		if err != nil {
			return fmt.Errorf("bench: list perseids: %w", err)
		}
		live, bad := 0, map[string]string{}
		for _, obj := range list.Items {
			phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
			switch phase {
			case "Running", "Parked":
				live++
			case "Refused", "Failed":
				reason, _, _ := unstructured.NestedString(obj.Object, "status", "admissionReason")
				bad[obj.GetName()] = phase + ": " + reason
			}
		}
		if live == want {
			return nil
		}
		if len(bad) > 0 {
			return fmt.Errorf("bench: %d perseids will not start: %v", len(bad), bad)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("bench: %d/%d %s perseids running or parked when the wait expired", live, want, arm)
		case <-tick.C:
		}
	}
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}

	return out
}
