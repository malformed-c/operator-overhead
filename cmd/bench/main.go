// Command bench builds, measures and tears down the operator-overhead
// comparison.
//
//	bench fixtures -n 8                 # N ConfigMap pairs, idempotent
//	bench up   -arm a1-cr-leader -n 8   # N workers, waited to Ready
//	bench run  -arm a1-cr-leader -n 8   # idle window, then change window
//	bench down -arm a1-cr-leader
//
// ***THE PHASES ARE SEPARATE COMMANDS BECAUSE A FAILED RUN MUST NOT COST A
// REBUILD.*** `run` measures a population that is already standing, so a control
// that fails can be investigated against the live thing and the window re-taken.
// Folding setup into the measurement would also put pod creation inside the
// settle period, which answers the startup question while claiming to answer the
// idle one.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/malformed-c/operator-overhead/internal/bench"
	"github.com/malformed-c/operator-overhead/internal/relay"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "fixtures":
		err = cmdFixtures(ctx, os.Args[2:])
	case "up":
		err = cmdUp(ctx, os.Args[2:])
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "down":
		err = cmdDown(ctx, os.Args[2:])
	case "size":
		err = cmdSize(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: bench <fixtures|up|run|down|size> [flags]

arms: %s, %s, %s
`, relay.ArmLeader, relay.ArmNoLeader, relay.ArmPerseid)
	os.Exit(2)
}

type common struct {
	kubeconfig string
	arm        string
	n          int
	image      string
	host       string
}

func (c *common) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig path (default: the usual resolution order)")
	fs.StringVar(&c.arm, "arm", "", "arm: "+relay.ArmLeader+" | "+relay.ArmNoLeader+" | "+relay.ArmPerseid)
	fs.IntVar(&c.n, "n", 1, "instances")
	fs.StringVar(&c.image, "image", "crrelay:v1", "ingested image for the controller-runtime arms")
	fs.StringVar(&c.host, "host", relay.Host, "physical host every worker is pinned to; the sampler must run there")
}

func cmdFixtures(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fixtures", flag.ExitOnError)
	var c common
	c.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cs, err := bench.Client(c.kubeconfig)
	if err != nil {
		return err
	}
	if err := bench.Fixtures(ctx, cs, c.n); err != nil {
		return err
	}
	fmt.Printf("fixtures: %d ConfigMap pairs in %s\n", c.n, relay.Namespace)

	return nil
}

func cmdUp(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	var c common
	c.bind(fs)
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for the arm to come up")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if c.arm == "" {
		return fmt.Errorf("-arm is required")
	}
	relay.Host = c.host

	cs, err := bench.Client(c.kubeconfig)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if c.arm == relay.ArmPerseid || c.arm == relay.ArmFused {
		dyn, err := dynClient(c.kubeconfig)
		if err != nil {
			return err
		}
		if c.arm == relay.ArmFused {
			if err := bench.UpFused(wctx, dyn, c.n); err != nil {
				return err
			}
		} else if err := bench.UpPerseids(wctx, dyn, c.n); err != nil {
			return err
		}
		if err := bench.WaitPerseidsRunning(wctx, dyn, c.arm, c.n); err != nil {
			return err
		}
	} else {
		if err := bench.UpArm(wctx, cs, c.arm, c.n, c.image); err != nil {
			return err
		}
		if err := bench.WaitPodsReady(wctx, cs, c.arm, c.n); err != nil {
			return err
		}
	}
	fmt.Printf("up: %d %s workers on %s\n", c.n, c.arm, c.host)

	return nil
}

func cmdDown(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	var c common
	c.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if c.arm == "" {
		return fmt.Errorf("-arm is required")
	}
	cs, err := bench.Client(c.kubeconfig)
	if err != nil {
		return err
	}
	if c.arm == relay.ArmPerseid || c.arm == relay.ArmFused {
		dyn, err := dynClient(c.kubeconfig)
		if err != nil {
			return err
		}

		return bench.DownPerseids(ctx, dyn)
	}

	return bench.Down(ctx, cs, c.arm)
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var c common
	c.bind(fs)
	spec := bench.Spec{}
	fs.DurationVar(&spec.Settle, "settle", 60*time.Second, "quiet period after readiness, before the first window")
	fs.DurationVar(&spec.Idle, "idle", 120*time.Second, "idle window: no writes at all")
	fs.DurationVar(&spec.Change, "change", 120*time.Second, "change window: one write per source per period")
	fs.DurationVar(&spec.Period, "period", time.Second, "load period")
	fs.DurationVar(&spec.ConvergeTimeout, "converge-timeout", 90*time.Second,
		"how long the effect-level control waits for every destination to take one written value")
	fs.StringVar(&spec.RadiantMetrics, "radiant-metrics", os.Getenv("RADIANT_METRICS"),
		"radiant's /metrics URL (arm B). A SHARED bucket: subtract a -n 0 baseline run for the marginal figure")
	fs.StringVar(&spec.APIServer, "apiserver", os.Getenv("BENCH_APISERVER"),
		"apiserver host:port, beside the ClusterIP — radiant dials it directly, and matching only the ClusterIP reports it as holding nothing")
	out := fs.String("out", "results/raw", "directory for the run record")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if c.arm == "" {
		return fmt.Errorf("-arm is required")
	}
	relay.Host = c.host
	spec.Arm, spec.N, spec.Host, spec.Image, spec.Kubecfg = c.arm, c.n, c.host, c.image, c.kubeconfig

	cs, err := bench.Client(c.kubeconfig)
	if err != nil {
		return err
	}

	// The dynamic client is only used for arm B's phase control; building it for
	// every arm keeps the call site from branching on something the harness
	// should not care about.
	dyn, err := dynClient(c.kubeconfig)
	if err != nil {
		return err
	}

	res, runErr := bench.Run(ctx, cs, dyn, spec)

	// ***THE RECORD IS WRITTEN EVEN WHEN THE RUN FAILED.*** A failed control is
	// the most useful artifact this produces — it names which population was
	// actually standing — and discarding it on error would leave the operator
	// with only the message.
	if err := writeRecord(*out, res); err != nil {
		return err
	}
	if runErr != nil {
		return runErr
	}
	printRun(res)

	return nil
}

// cmdSize reports what each arm costs to SHIP, which is a different question
// from what it costs to run and is answered by different instruments.
func cmdSize(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("size", flag.ExitOnError)
	crrelay := fs.String("crrelay", "build/crrelay", "the built controller-runtime binary")
	component := fs.String("component", "build/perseid/relay-000.wasm", "one built Perseid component")
	apsis := fs.String("apsis", "apsis", "the apsis CLI, which reports the node library's accounting")
	out := fs.String("out", "results/raw", "directory for the record")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// The ladder the benchmark actually runs, so the crossover is reported at a
	// point somebody will have data for rather than at an interpolated one.
	rep, err := bench.MeasureSizes(ctx, *apsis, *crrelay, *component, []int{1, 8, 32, 64})
	if err != nil {
		return err
	}
	if err := writeJSON(*out, "size", rep); err != nil {
		return err
	}
	printSizes(rep)

	return nil
}

func printSizes(rep bench.SizeReport) {
	mib := func(b int64) string { return fmt.Sprintf("%.2f MiB", float64(b)/(1<<20)) }

	fmt.Printf("\n%-28s %12s %12s  %s\n", "arm", "artifact", "in library", "per instance")
	for _, s := range rep.Sizes {
		fmt.Printf("%-28s %12s %12s  %s\n", s.Arm, mib(s.ArtifactBytes), mib(s.ImageBytes), s.PerInstance)
	}

	fmt.Printf("\n%-28s %10s %10s %10s  %s\n", "source (the program)", "code", "comment", "blank", "file")
	for _, s := range rep.Sizes {
		fmt.Printf("%-28s %10d %10d %10d  %s\n",
			s.Arm, s.Source.Code, s.Source.Comment, s.Source.Blank, s.Source.Path)
	}

	fmt.Printf("\nbytes in the node library at N:\n")
	fmt.Printf("%6s", "N")
	for _, s := range rep.Sizes {
		fmt.Printf(" %20s", s.Arm)
	}
	fmt.Println()
	for _, at := range rep.Shipped {
		fmt.Printf("%6d", at.N)
		for _, s := range rep.Sizes {
			fmt.Printf(" %20s", mib(at.Bytes[s.Arm]))
		}
		fmt.Println()
	}
	if rep.Crossover > 0 {
		fmt.Printf("\ncrossover: at N=%d the Perseid arm has put more bytes in the library than\n"+
			"the controller-runtime arm, which ships one image at every N.\n", rep.Crossover)
	} else {
		fmt.Printf("\ncrossover: none within the ladder.\n")
	}
}

func writeJSON(dir, kind string, v any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("bench: results dir: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.json", kind, time.Now().UTC().Format("20060102T150405Z")))
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("bench: write %s: %w", path, err)
	}
	fmt.Println("record:", path)

	return nil
}

func orNone(s string) string {
	if s == "" {
		return "(unknown)"
	}

	return s
}

func writeRecord(dir string, res bench.Result) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("bench: results dir: %w", err)
	}
	name := fmt.Sprintf("%s-n%03d-%s.json", res.Spec.Arm, res.Spec.N,
		res.StartedAt.UTC().Format("20060102T150405Z"))
	path := filepath.Join(dir, name)
	raw, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("bench: write %s: %w", path, err)
	}
	fmt.Println("record:", path)

	return nil
}

func printRun(res bench.Result) {
	fmt.Printf("\n%s  N=%d  host=%s  commit=%s\n",
		res.Spec.Arm, res.Spec.N, res.Spec.Host, res.Preflight.Commit)
	if res.Preflight.TrailVersion != "" || res.Preflight.RadiantVersion != "" {
		// Protocol step 0. Read from the artifact and the running pod, because a
		// deploy log was wrong about trail by sixteen days on this cluster.
		fmt.Printf("runtime:  trail %s · radiant %s\n",
			orNone(res.Preflight.TrailVersion), orNone(res.Preflight.RadiantVersion))
	}
	fmt.Printf("controls: %d processes matched %q, %d from other arms, %d metrics endpoints\n",
		res.Preflight.Found, res.Preflight.Selector, res.Preflight.Excluded, res.Preflight.MetricsEndpoints)
	fmt.Printf("effect:   relayed a value end-to-end in %.1fs before any window opened\n",
		res.Preflight.ConvergeSeconds)
	if len(res.Preflight.APITotals) > 0 {
		// Cumulative, since the workers started — the only place a long-lived
		// WATCH is visible at all. See Preflight.APITotals.
		methods := slices.Sorted(maps.Keys(res.Preflight.APITotals))
		parts := make([]string, 0, len(methods))
		for _, m := range methods {
			parts = append(parts, fmt.Sprintf("%s=%.0f", m, res.Preflight.APITotals[m]))
		}
		fmt.Printf("requested: %s (cumulative since worker start; HTTP methods, so a held WATCH is invisible here)\n",
			strings.Join(parts, " "))
	}
	last := res.Windows[len(res.Windows)-1]
	conns := strconv.Itoa(last.APIServerConns)
	if !last.APIServerConnsFull {
		conns += "*"
	}
	// The LABEL names the set, not one member. Counting only the ClusterIP would
	// report radiant — which dials the apiserver directly — as holding nothing.
	fmt.Printf("held:      %s ESTABLISHED connections to the apiserver — this IS the watch\n", conns)
	fmt.Printf("cached:    %d objects — %s\n", last.CachedObjects, last.CachedBasis)
	fmt.Printf("%-8s %5s %7s %9s %8s %8s %9s %9s %9s %9s %7s\n",
		"window", "procs", "secs", "rss MiB", "cpu ms", "api req",
		"react p50", "react p99", "conv p50", "conv p99", "samples")
	for _, w := range res.Windows {
		pss := fmt.Sprintf("%.1f", float64(w.PSSBytes)/(1<<20))
		if !w.PSSComplete {
			// An incomplete PSS is marked rather than printed as a number
			// somebody would compare across arms.
			pss += "*"
		}
		// ***REACTION IS PRINTED BEFORE CONVERGENCE, AND THAT IS THE POINT OF
		// HAVING BOTH.*** Reaction is notice+decide from the operator's own
		// clock — the arm's reflex. Convergence adds the write's round trip and
		// this harness's own watch delivery, neither of which is the arm's cost.
		react := "     n/a"
		if w.Latency.Reaction.Count > 0 {
			react = fmt.Sprintf("%9.1f", w.Latency.Reaction.P50MS)
		}
		react99 := "     n/a"
		if w.Latency.Reaction.Count > 0 {
			react99 = fmt.Sprintf("%9.1f", w.Latency.Reaction.P99MS)
		}
		// `n/m` — not measured — rather than 0. See Window.APIMeasured.
		api := "     n/m"
		if w.APIMeasured {
			api = fmt.Sprintf("%8.0f", w.APIRequests)
		}
		fmt.Printf("%-8s %5d %7.0f %9.1f %8.0f %s %s %s %9.1f %9.1f %7d\n",
			w.Name, w.Processes, w.Seconds, float64(w.RSSBytes)/(1<<20),
			w.CPUms, api, react, react99, w.Latency.P50MS, w.Latency.P99MS, w.Latency.Count)
		fmt.Printf("         pss %s MiB", pss)
		if w.Latency.Reaction.Count > 0 {
			fmt.Printf(" · reaction over %d samples, min %.1f ms",
				w.Latency.Reaction.Count, w.Latency.Reaction.MinMS)
			// A negative minimum means the operator stamped a clock EARLIER than
			// the harness wrote, which on one host cannot happen — so the two are
			// not on one clock and the whole reaction column is void.
			if w.Latency.Reaction.MinMS < 0 {
				fmt.Printf("  ⚠ NEGATIVE: the arm and the harness are not on one clock; reaction is void")
			}
		}
		if w.HostRSSBytes > 0 {
			// ***THE SHARED HOST IS PART OF THIS ARM AND IS PRINTED AS PART OF
			// IT.*** N managers are the whole of arm A; arm B is radiant + N
			// steps, and radiant is about the size of one manager.
			fmt.Printf("         shared host (radiant): rss %.1f MiB · peak %.1f MiB · cpu %.0f ms · %d conn — "+
				"NOT all this arm's: it also serves every other Perseid on the cluster\n",
				float64(w.HostRSSBytes)/(1<<20), float64(w.HostPeakBytes)/(1<<20), w.HostCPUms, w.HostConns)
		}
		if len(w.HostCounters) > 0 {
			keys := slices.Sorted(maps.Keys(w.HostCounters))
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%s=%.0f", strings.TrimPrefix(k, "radiant_reconcile_"), w.HostCounters[k]))
			}
			fmt.Printf("         radiant (SHARED bucket, all perseids on the cluster): %s\n", strings.Join(parts, " "))
		}
		if w.Latency.Stale > 0 {
			// A torn read is about the ARM's write shape, not about its speed:
			// it writes value and stamp as separate obligations.
			fmt.Printf(" · %d torn pairs discarded (value and stamp written separately)", w.Latency.Stale)
		}
		if w.Latency.Unstamped > 0 {
			fmt.Printf(" · %d convergences carried no %s stamp", w.Latency.Unstamped, "data.t")
		}
		fmt.Println()
		if len(w.APIByMethod) > 0 {
			methods := slices.Sorted(maps.Keys(w.APIByMethod))
			parts := make([]string, 0, len(methods))
			for _, m := range methods {
				parts = append(parts, fmt.Sprintf("%s=%.0f", m, w.APIByMethod[m]))
			}
			fmt.Printf("         verbs: %s\n", strings.Join(parts, " "))
		}
		if w.APIErr != "" {
			fmt.Printf("         note: %s\n", w.APIErr)
		}
	}
}

func dynClient(kubeconfig string) (dynamic.Interface, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("bench: kubeconfig: %w", err)
	}
	cfg.UserAgent = "operator-overhead-bench"

	return dynamic.NewForConfig(cfg)
}
