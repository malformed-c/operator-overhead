# Operator overhead measurements

**What does one Kubernetes operator cost, and what does the hundredth cost?**

Five arms do the identical job — copy `data.v` from ConfigMap `src-<i>` to ConfigMap `dst-<i>` — at N ∈ {1, 8, 32, 64}, on one host, under one protocol:

| arm | what it is | processes at N |
|---|---|---|
| **A1** `a1-cr-leader` | N controller-runtime managers, **leader election on** — an operator as shipped | N |
| **A2** `a2-cr-noleader` | N controller-runtime managers, **leader election off** | N |
| **A3** `a3-cr-shared` | **one** manager, N controllers over one informer and one connection | 1 |
| **B** `b-perseid` | N Perseids — a wasm step, parked between passes, on a shared host | N |
| **B2** `b2-perseid-fused` | **one** Perseid relaying all N pairs | 1 |

**They are three pairs, and each pair exists to kill one objection.**

A1 against A2, because leader election is the single largest thing an idle operator does, and running only one of them invites the objection that the result was arranged. They are the **same binary with one flag different**.

A1/A2 against A3, because "N managers" is a reading and not the only one. N vendors with N release cycles is one deployment story; one operator hosting N controllers is another, and it is the steelman a sceptic asks for first.

A3 against B2, because comparing N Perseids to one shared manager would measure **consolidation and call it runtime**. One process on each side is the only version of the memory question that is about the runtime rather than about how many times you started something.

### What is actually being compared

**controller-runtime is a library compiled into every operator. Perseid is a runtime into which operator programs are loaded.** That is the difference the columns below are measuring, and it is why the two sides do not have the same knobs: an operator amortises its cost by *containing more code*, and a Perseid amortises its cost by *being loaded into something already running*.

It also sets what consolidation costs on each side. **Radiant shares the machinery without making every program share one implicit authority.** Arm A3 gets its 37× by putting 64 relays in one process — one ServiceAccount, one Role, one grant spanning every pair, and any of the 64 controllers able to write any other's objects, because they are one program to the apiserver. Arm B keeps N identities while sharing the host: each program gets a Role derived from its own `spec.writes`, scoped by `resourceNames` to the objects it declared. The memory saving and the authority pooling are the same act on the A side and separable on the B side, and no column in this file prices that.

---

## The rule this benchmark is built under

**A number is reported at the scope its instrument supports, or it is not reported.** Everything below follows from that.

| we want to know | instrument | why it can answer |
|---|---|---|
| memory | `VmRSS` from `/proc/<pid>/status`, and `Pss` from `smaps_rollup` | per-process, and named. PSS because at N=64, summing RSS counts shared text pages 64 times |
| CPU | `utime+stime` delta from `/proc/<pid>/stat` | per-process. Cross-checked against `getrusage` in a test, because a column-offset bug reads `cmajflt` as CPU and looks plausible |
| requests made | `rest_client_requests_total`, scraped from **each worker's own** `/metrics` | client-go increments it *inside the calling process*. **Attribution by construction** — there is no bucket to divide |
| what is **held open** | `ESTABLISHED` sockets in `/proc/<pid>/net/tcp` | a request counter cannot see a watch. See below |
| convergence | origin timestamp carried **inside** the relayed value | the field the relay copied is the field that dates it |
| **reaction** | the operator stamps its **own** clock into `data.t` as it writes | separates the arm's reflex from the round trip, which is not the arm's cost |
| size | `stat` on the artifact, `apsis images --json` for the library, a line classifier for the source | three different questions; see below |

### A request counter cannot see a watch

`rest_client_requests_total` is labelled by **HTTP method**, not Kubernetes verb. A LIST and a WATCH are both `GET`, and a WATCH is a GET *that does not complete* — so it never increments while it is held. Measured, one manager, a 45-second window with its informer watching throughout:

```
GET=3  PATCH=74
```

The watch contributed nothing to either number and was open the whole time. So "N operators cost N caches" is a claim about what is **held**, not about a request rate, and it needs a different instrument. Sockets are what is actually held:

```
/proc/<pid>/net/tcp
AA98000A:A414 0100600A:01BB 01     10.0.152.170:42004 -> 10.96.0.1:443 ESTABLISHED
```

**One** connection per manager, not several: client-go speaks HTTP/2, so the list, the watch and every patch are multiplexed over a single socket. That is the opposite of what "one watch per informer" suggests, and it is in the report because it was measured rather than assumed.

### Reaction is not convergence

Convergence — harness writes `src`, harness observes `dst` — is four legs, and only two of them belong to the arm:

```
notice    the change reaching the operator (an informer, or a wake index)  ─┐ the
decide    the reconcile or the step running                                ─┘ arm
apply     the operator's write reaching the apiserver                      ─┐ not
deliver   the apiserver telling the HARNESS                                ─┘ the arm
```

So each arm stamps its **own** clock into `data.t` at the moment it decides to write — `time.Now().UnixMilli()` in Go, `observe.now()` in the Perseid, which is epoch milliseconds *by contract* because reconcile.wit pins the unit. Measured, arm A2, N=1, 39 changes:

| | react p50 | react p99 | conv p50 | conv p99 |
|---|---|---|---|---|
| A2 | **9.9 ms** | 13.3 ms | 16.4 ms | 19.2 ms |

**~9.9 ms is the arm noticing and deciding.** The remaining ~6.5 ms is the write's round trip plus this harness's own watch delivery. That tail is a constant added to every arm and cancels in the comparison; it does *not* cancel in the absolute figure, which is why both columns are printed rather than one.

Two guards on the column, because a clock comparison across processes is exactly where a plausible wrong number comes from:

- **Both clocks are one clock.** Every pawn in a run is pinned to a single machine (`peri.apsis/host`), so the skew term is absent rather than merely small. On a multi-host run this would be measuring NTP.
- **A negative minimum voids the column, loudly.** An operator cannot stamp a clock earlier than the harness wrote unless the two are not on one clock, so the report prints `⚠ NEGATIVE: the arm and the harness are not on one clock` rather than a tidy quantile over an invalid assumption. Measured minimum on the run above: **+3.9 ms**.

Arm A stamps in the **same PATCH** as the value, so it stays at one apiserver request per change (`api req = 39` over 39 ticks) — a second write for a timestamp would show up in the API column as if the relay were amplifying. Arm B emits both fields as a single `ensure-all` obligation, applied as one MergePatch, so the same holds there; the value carries a `<value>@<clock>` encoding so that a torn pair is detectable rather than silently averaged into the reaction column.

---

## Size — three questions, and the arms do not win the same one

`bench size` measures rather than asserts. Every byte below came from `stat` or from the node library's own accounting; the line counts came from reading the files.

| arm | artifact | in the node library | per instance |
|---|---|---|---|
| controller-runtime (A1, A2) | **31.29 MiB** | 41.06 MiB | 1 image, shared by all N — the instance is a `-id` flag |
| perseid (B) | **1.02 MiB** | 1.47 MiB | 1 image, shared by all N — the instance is `spec.config` |

**The artifact an author hands over is 31× smaller.** A relay Perseid is 1.02 MiB of wasm against 31.29 MiB of static Go — and the Go figure is already `-trimpath -ldflags="-s -w"`; unstripped it is 45.29 MiB.

**The program a person writes and reviews is smaller still:**

`cloc 2.08`, over the two programs — `cmd/crrelay/main.go` and `perseid/relay/src/main.ts`:

```
-------------------------------------------------
Language                     files           code
-------------------------------------------------
Go                (arm A)        1            157
TypeScript        (arm B)        1             43
-------------------------------------------------
```

`code` is cloc's, which counts neither comments nor blank lines. `bench size` carries its own classifier and agrees with cloc to the line, which is why the number in a record and the number in this table can be quoted interchangeably.

**3.7× the code, for identical behaviour.** The difference is not cleverness — it is a manager, a scheme, a cache with a label selector, a predicate, flag parsing, a leader-election block and a shared-mode branch. Arm B's author writes none of it **because Radiant already ran it**. That is ADR-0075's claim, stated as a diff.

**Written against the raw WIT, not the [Periapsis SDK][sdk].** The SDK is how a Perseid is normally authored — it exports `LANGUAGE_VERSION`, wraps the resume vocabulary, and documents the trade this step makes by hand (`untilDrift`). These components import `radiant:reconcile` directly, which is why the 43 lines are the whole program and why `spec.language` here is a claim rather than a derived stamp. An SDK-authored relay would be shorter to write and would carry the SDK's own code behind it.

**Neither arm is charged for its platform**, which is the only way the two numbers are comparable. Arm A's 157 lines do not include controller-runtime, most of that 31 MiB binary. Arm B's 43 do not include `perseid/relay/wit/`, which is `radiant:reconcile` **vendored by dwarf** rather than written — a toolchain artifact, the same as arm A's `go.sum`. Counting either platform against the program that uses it would be the same error, made twice.

---

## Controls

Every run takes these **before** the first window opens, and refuses to proceed if any fails. A run whose controls failed is not a run with bad numbers; it is not a run.

- **Positive** — the process selector matches exactly N processes. Not N+1, not N−1.
- **Negative** — every *other* arm's selector matches zero. Proof a stale pod from a previous arm is not being counted into this one.
- **Instrument** — all N metrics endpoints serve the counter. An absent metric family and a family at zero are different facts, and the record keeps them apart (`Found`).
- **Population** — a window whose process count moved is marked, not averaged.
- **Denominators** — every quantile is printed beside its sample count.
- **Environment** — every `perseid-relay-*` pod carries `trail.apsis/env=all`. The value is asserted, not merely the key: trail forwards the environment for exactly `"all"` and strips it for anything else, so a pod annotated `""` is byte-identical in behaviour to one with no annotation at all.

## Results — five arms, four rungs

`engix99` · 45 s windows · PSS under privilege · every record in `results/raw/` carrying its runtime commits and node placement.

### Idle — nothing happening at all, apiserver requests per 45 s

| N | A1 | A2 | A3 | B |
|---|---|---|---|---|
| 1 | 22 | 0 | 0 | 0 |
| 8 | 179 | 0 | 1 | 0 |
| 32 | **711** | 0 | 0 | 0 |
| 64 | **1419** | 3 | 0 | 0 |

**Leader election is the operator tax and it is linear.** 1419 writes in 45 s at N=64 — ~32 writes/second from 64 operators doing nothing. A1 and A2 are the same binary with one flag different, so that column *is* the flag.

### Under change — RSS (MiB) / apiserver writes / reaction p50

| N | A1 | A2 | A3 | B |
|---|---|---|---|---|
| 1 | 37.7 / 67 / 8.9 ms | 45.8 / 44 / 10.0 ms | 43.6 / 44 / 9.6 ms | 24.3 / **3** / 30.8 ms |
| 8 | 346 / 533 / 40.1 ms | 338 / 352 / 37.9 ms | **38.6** / 352 / 41.0 ms | 192 / **24** / 617.6 ms |
| 32 | 1317 / 2122 / 140 ms | 1315 / 1408 / 135 ms | **39.8** / 1408 / 132 ms | 789 / **96** / 134 ms |
| 64 | 2672 / 4231 / 312 ms | 2580 / 2830 / 260 ms | **42.1** / 2810 / 268 ms | 1572 / **192** / 303 ms |

---

## The two things this measured

### 1. Writes: Perseid is 14.6× lower, and consolidation does not touch it

**192 writes against 2810** at N=64, for the same 2816 source changes. It does **not** come from waking on change: under 1 Hz churn the program never parks. Every pass finds `src` already ahead of `dst`, relays the latest value and yields, and a yielded program's next pass is the next poll tick — `-perseid-poll`, **15 s by default** (`reconcilehost.waitYield`: *paced by Poll, not immediate; a yield carries no wake condition*). Measured at every rung, `applied == runs == 3N` per 45 s window: one pass per program per 15 s, each relaying whatever the source holds at that instant. An idle parked Perseid does **zero writes and one pass** per 45 s, measured per-program from `radiant_perseid_applied_total{perseid=…}`.

***THE RATIO IS THE POLL INTERVAL OVER THE CHANGE PERIOD, AND IT IS A KNOB.*** 15 s ÷ 1 s ≈ 44 ticks ÷ 3 passes ≈ 14.6. It is a batching interval, not an architecture: `-perseid-poll=1s` would collapse it to ~1×, and a source that changes less often than once per 15 s gets one write per change, the same as arm A. **A yielding step cannot be woken at all, and that is by construction rather than by omission.** `Watch` is subscribed only when the watch set derived from a program's *resume* is non-empty; a yield carries no resume, so the set is empty, the event channel stays nil and the timer is the only arm. Wake-on-change is therefore available exclusively to programs that **park** — and a relay under continuous churn is precisely the program that never does. Closing ADR-0097's gap 2 would not change this arm's number.

***AND THE 15 s IS BORROWED, NOT CHOSEN.*** `Poll` was picked for how often a *parked* program's resume is re-evaluated; a yield reuses it because, in the driver's own words, it is "the nearest existing meaning of *look again shortly*". So one number answers two different questions — how stale a parked condition may be, and how fast a yielding step may spin — and nothing has ever decided that 15 s is right for the second. Confirmed by radiant's author against the source, not inferred here.

**What the destination sees, which no column above measures.** 2624 of 2816 source changes at N=64 never reached `dst`; between passes `dst` is up to 15 s behind `src`. The reaction column cannot see this — it dates only the values that *were* relayed, from their own origin, so it reports ~300 ms for a destination that is stale most of the time. For a level-triggered relay, where the only consumer is whoever reads `dst` next, that is the intended behaviour, and it is how every Kubernetes controller reconciles: to the latest state, not through every intermediate one. For a workload that needs every version to land, this is the wrong arm, and the writes column would be reporting a loss as a win.

**The honest statement of this result has no headline in it.** Arm B batches at the poll interval because a yielded step has no wake path; at a change period longer than the poll it is 1× by construction; and a relay that *parked* after each write would take the wake index (~30 ms) and write per change, trading the batching away for freshness. The 14.6× is a property of **yield-after-write**, which is a choice this step makes — not a property of Perseid.

Arm A3 consolidates 64 managers into one process and still issues 2810 writes, because every change still becomes a PATCH. That column is the one consolidation cannot fix; it is also the one that would survive a parking relay, since per-change writes on both sides is the comparison that no longer favours either.

### 2. Memory: the Perseid case holds against N vendors, and only against N vendors

Arm B is 1572 MiB against A2's 2580 — a real win, **and** it is beaten 37× by one consolidated manager at 42.1 MiB doing identical work.

Arm A's per-instance cost is **flat at ~40 MiB across N=1…64** while its informer holds two ConfigMaps. So that cost was never the cache: it is a Go process. One process serving 64 pairs costs 42 MiB; 64 processes serving one pair each cost 2.5 GiB.

> ***THE MEMORY ARGUMENT FOR PERSEID HOLDS AGAINST N SEPARATE OPERATORS — N vendors, N deployments, N release cycles, which is ADR-0075's motivating case — AND DOES NOT HOLD AGAINST ONE OPERATOR THAT CONSOLIDATED.*** That is the only case it holds in, and this sentence is next to the number on purpose.

What A3 pays for that 37× is not in this table: 64 relays in one process are **one subject to the apiserver**, holding one grant over every pair. Arm B pools the machinery and not the authority — see *What is actually being compared* above, and *Its identity is derived* below.

---

## Fusing by hand: one program, every pair

Arm **B2** is one Perseid relaying every pair, and it is the symmetric counterpart to arm A3: one process on each side, which is the only version of the memory question that is about the runtime rather than about how many times you started something.

It relays every pair in a single pass, and parks on one expression covering all of them:

```
Applied  performed 3 write(s): EnsureAll(dst-000, {"data":{"t":…,"v":…}});
         EnsureAll(dst-003, …); EnsureAll(dst-007, …)
Yielded  pass yielded; declared 3 obligation(s)
```

---

## What would change these numbers

- **The writes column survives consolidation and not much else.** Arm A3 shows that merging operators does not reproduce it; `-perseid-poll` and the change rate set it, and either can erase it — see *1. Writes*.
- **The memory column is the contingent one.** It is a comparison of process counts, so it moves with any change to how either side is deployed.

### The shared host is part of arm B, and leaving it out flatters the arm

N managers are the *whole* of arm A. Arm B is **Radiant + N steps**, and Radiant is about the size of one manager, so sampling only the step pods would report a fraction of the arm.

Measured, `engix99`, per 35 s window with N=1:

| | step (`perseid-relay-000`) | shared host (`radiant`) |
|---|---|---|
| RSS | 24.4 MiB | **46.5 MiB** (peak 52.6) |
| CPU | 10 ms | **280 ms** (~0.8 % of a core) |
| apiserver connections | **0** | 1 |
| threads | — | 32 |

**⚠ It is a shared cost and this benchmark does not own all of it.** The same Radiant serves `gazer-governance`, `scaler-v4`, `dag-demo` and `podmaker`, and is the Trail Operator besides.
Charging all 46.5 MiB to arm B overstates; charging none understates. Both readings are in the record and the report prints the caveat on the same line as the number.

### PSS is the column that decides the ladder, and it already disagrees with RSS

`/proc/<pid>/smaps_rollup` needs `PTRACE_MODE_READ` and the workers run as root, so this column prints `0.0*` — *marked unavailable, never zero* — unless the run is given privilege. Measured at N=1:

| | RSS | PSS | shared |
|---|---|---|---|
| **A2** controller-runtime | 39.9 MiB | **39.9 MiB** | nothing |
| **B** perseid step | 24.2 MiB | **20.0 MiB** | 4.2 MiB |

**A controller-runtime manager at N=1 shares nothing** — PSS and RSS are equal to the tenth of a MiB, because no other process on the box is running that binary. **A Perseid step already shares 17 %**, because `trail` is running for four other Perseids that have nothing to do with this benchmark.

***THIS IS WHY THE LADDER IS THE EXPERIMENT AND N=1 IS THE SETUP.*** Summing RSS across N copies of one binary counts its text pages N times. At N=64 both arms share their own binary's text with themselves, so both curves bend — and which bends further is the memory result this benchmark exists to produce. At N=1 the question is invisible, and the RSS column above is the *most* flattering reading arm A can get.

The harness reports PSS and RSS separately and marks PSS incomplete rather than substituting one for the other, because they are different quantities.

### Arm B is attributable by construction, not by subtraction

Radiant emits `radiant_perseid_{applied,runs}_total{perseid="ns/name"}`, so filtering to this benchmark's own programs is the same class of instrument arm A gets from client-go: the label is applied at the emit site by the thing doing the work, and there is no bucket left to divide. That is ADR-0098's protocol step 1 — *identify callers* — satisfied rather than worked around.

Both are scraped, because they answer different questions:

| 45 s window | **mine** (`perseid="overhead/relay-*"`) | shared bucket (every Perseid) |
|---|---|---|
| idle | `applied=0  runs=1` | `applied=3  runs=4` |
| change | `applied=3  runs=3` | `applied=6  runs=6` |

**An idle parked Perseid performs zero writes and one pass in 45 seconds**, and that is a measurement of *this program* rather than a delta against a background that was most of the bucket.
Under change it performed **3 writes for 44 source changes**, against 44 for arm A2 and 67 for arm A1 — 15× and 22× fewer, and every one of the 3 was a poll-tick pass rather than a wake (see *1. Writes*).

The bucket stays in the report as the neighbour control: a window where the bucket moved and the per-program series did not is a window with somebody else's program in it.

### What the Perseid arm wins

**It holds nothing open.** Zero ESTABLISHED connections to the apiserver against one per controller-runtime manager, and zero objects cached against two. `status.waitingFor` shows what a parked program is waiting on, as data:

```
Parked   (Get(".../src-000", "data.v") != Get(".../dst-000", "data.v")) || Now() >= 1788285851835
```

**It batches**, at one write per poll tick however often the source moves — 1 write per ~15 changes at 1 Hz, set by `-perseid-poll`. See *1. Writes* for what that costs the destination.

### Its identity is derived, not authored — and that is a real asymmetry

Radiant provisions a ServiceAccount, a Role and a RoleBinding **per Perseid**, with the rules computed from the program's declared `spec.writes` rather than written by anyone. Verified on this cluster against `default/dag-demo`, which declares two deployment paths:

```
spec.writes  /apis/apps/v1/namespaces/default/deployments/{dag-a,dag-b}

Role perseid-dag-demo
  apps  deployments        [dag-a dag-b]  get patch
  apps  deployments/scale  [dag-a dag-b]  get update
  radiant.apsis  perseids        [dag-demo]  get
  radiant.apsis  perseids/status [dag-demo]  get patch update

ServiceAccount perseid-dag-demo   ownerReferences: Perseid/dag-demo
```

`resourceNames` is the *exact* set from `spec.writes`, and the ownerReference means the grant cannot outlive the program that justified it.

***COMPARE WHAT ARM A REQUIRED.*** `deploy/overhead.yaml` grants `patch` on **every ConfigMap in the namespace**, and the comment there says why: scoping it to `src-<id>`/`dst-<id>` by name would mean sixty-four Roles and sixty-four bindings to author and keep in step. So arm A's grant is loose because tightening it does not scale *by hand* — and arm B gets the tight version because no hand is involved. That is the same claim the memory columns make, in a place the memory columns cannot see.

The derivation is bounded by the apiserver, not by good intentions: Radiant is deliberately **not** granted `escalate`, so Kubernetes' privilege-escalation check refuses any derived Role exceeding Radiant's own ClusterRole. The ceiling is enforced by the thing being asked, for free.

**The counterweight, which no column here charges arm B for.** Three objects per Perseid is `3N` pieces of cluster state that arm A does not create — precisely the growth-with-N avoided for arm A by taking the looser Role. Provisioning them is also Radiant apiserver traffic, and it lands in the shared-host row at admission rather than in arm B's per-window writes. Both facts are outside the measurement, so this section is an observation about the two arms' operational shape and **not** a result: nothing in the ladder was re-run to support it.

### What it loses

**Reaction: 37 ms p50 against 9.9 ms**, and convergence 47 ms against 16 ms. Waking a parked program through a shared wake index costs more than an informer callback in the same process. That is architecture, not tuning, and it is the clearest loss in the table.

**Freshness, under churn.** A source that moves faster than the poll tick leaves `dst` up to 15 s behind it, and the reaction column does not see that, because it only dates the values that were relayed. The two losses are one design seen twice: waking a parked program costs more than a callback, and a yielded program does not wake at all until the tick.

### A step's memory is tight, and it is set by the component

Freshly launched and parked, identical component and trail, n=5:

```
23.5   24.3   24.2   24.3   24.2 MiB      VmHWM within 0.6 MiB of RSS every time
```

**It is tight, not variable**, and load does not move it either: 25 changes moved RSS from 24.2 to 24.3, with peak unchanged. `VmHWM` within 0.6 MiB of RSS on every launch also rules out a process that grew and released.

For scale, the same runtime across four unrelated Perseids live on this cluster spans **17.1 MiB (`governance:v6`) to 70.9 MiB (`dag:v3`)** — 4×, with trail version ruled out, since `gazer-governance` and `scaler-v4` run the *same* binary and differ by that much. Whatever sets a step's footprint, it is the component, and `spec.resources` is what bounds it.

---

## The control that asserts the effect

Every other control here asserts a **marker**: pods Ready, N processes matched, the metrics family present, an annotation set. Each is necessary and none of them is the question. The question is whether a value written into `src-<i>` arrives in `dst-<i>`, and the only thing that answers it is writing one and looking.

So `ProveConvergence` is the last gate before any window opens, for every arm: it writes one sentinel into every source and waits until every destination carries it. Mutation-verified in both directions, because a control nobody has seen fail is one nobody knows works.

```
# arm down
bench: convergence control FAILED: 1/1 destinations never took the value in 25s:
dst-000. The arm is up and is not doing its job — every other control here asserts
a marker (pods Ready, processes matched, metrics present) and none of them can see
this. Measuring now would report an arm that does nothing as an arm that is cheap

# arm up
effect:   relayed a value end-to-end in 1.0s before any window opened
```

***THE REASON THIS EXISTS IS THAT ARM B'S FAILURE MODE LOOKS LIKE ARM B'S BEST RESULT.*** A Perseid whose config never arrived yields forever: no writes, no obligations, minimal CPU, a flat idle window. It would post the **lowest** apiserver traffic and the **smallest** CPU of any arm here — because it does nothing — and every marker-shaped control would be green. Publishing that as a result, in a benchmark whose whole premise is surviving scrutiny, is the failure this is built to make impossible.

It costs a write and up to 90 seconds per run, and it is not behind a flag: a control that is optional is a control that is off on the day it matters.

---

## Running it

```bash
kubectl apply -f deploy/overhead.yaml

CGO_ENABLED=0 go build -o crrelay ./cmd/crrelay
apsis ingest ./crrelay --name crrelay:v1

go run ./cmd/bench fixtures -n 8

go run ./cmd/bench up   -arm a1-cr-leader -n 8
go run ./cmd/bench run  -arm a1-cr-leader -n 8      # settle → idle → change
go run ./cmd/bench down -arm a1-cr-leader

# -arm takes any of the five. A1/A2/A3 all run the crrelay ingested above;
# A3 is one process whatever -n says.
#   a1-cr-leader  a2-cr-noleader  a3-cr-shared  b-perseid  b2-perseid-fused

# arm B builds and ingests its component first. ONE artifact serves every
# instance — each Perseid carries its own pair in spec.config, the same way arm A
# carries it in -id — so this does not take -n.
hack/perseid-build.sh perseid/relay relay:v1
go run ./cmd/bench up -arm b-perseid -n 8

# arm B2 is a different component, ingested under its own name.
hack/perseid-build.sh perseid/fused relay-fused:v1
go run ./cmd/bench up -arm b2-perseid-fused -n 8

# what each arm costs to SHIP — artifact, node library, source lines
go run ./cmd/bench size
```

Setup, measurement and teardown are **separate commands on purpose**: a run measures a population that is already standing, so a control that fails can be investigated against the live thing and the window re-taken without paying for another startup. Folding setup in would also put pod creation inside the settle period — which answers the startup question while claiming to answer the idle one.

Each run writes a full record to `results/raw/<arm>-n<N>-<ts>.json`, **including when it failed**: a failed control is the most useful artifact this produces, because it names the population that was actually standing.

### Two windows, always

`idle` runs with no writes at all; `change` writes one value per source per second. ADR-0098's protocol step 7: *idle measurements answer idle questions.* The leader-election result above is invisible in a change window, where it is swamped by real work, and it is the entire finding.

---

## Layout

```
cmd/crrelay        arm A: one controller-runtime manager, one relay pair
cmd/bench          fixtures, arms, windows, controls, records
perseid/relay      the step, its world, and the build
perseid/fused      arm B2: one program, every pair
internal/relay     the workload's vocabulary — ONE spelling, three programs
internal/procsample  /proc: RSS, PSS, CPU, sockets
internal/apiload   client-side request counters
internal/bench/size.go  artifact / library / source-line comparison
deploy/            namespace, ServiceAccount, the smallest Role that works
hack/              the experiment: build the component, run the ladder
validation/        live checks against PERIAPSIS behaviour, not the benchmark's
```

`internal/relay` is one package because three programs must spell the workload identically. A namespace, a label or a field name spelled twice is how two arms of a comparison quietly measure different things — the failure ADR-0098 spends its length on.

---

## The control that does not exist

Every control above is falsifiable by machine. `ProveConvergence` goes red, the annotation check goes red, the process-count control goes red. There is one more that matters at least as much and **has no red state**:

> An effect-level control protects against a broken arm. It does not protect against *wanting* an arm to win — and the arm you most want to win is precisely the one whose cheap result you are least qualified to accept.

Nothing fires when that is violated. The only remedy is numbers reproducible by somebody who does not care. Concretely, and checkable rather than a sentiment:

1. **The losing columns get the same prominence as the winning ones.** Arm B loses reaction outright and loses memory to a consolidated manager by 37×, and both sit in the results table rather than in a footnote.
2. **Every run writes a raw record to `results/raw/`, including runs that FAILED their controls** — a failed control is the most useful artifact here, because it names the population that was actually standing.
3. **The protocol was written down before the numbers, and it is ADR-0098's rather than this benchmark's.**

If an arm wins on memory and CPU and those three cannot be produced, the result should not be believed.

---

## What this does not claim

- It does not measure a mature cluster's third-party operator population.
- The client-side counters see what an arm's **own** client did. Traffic an arm causes *indirectly* — a write that wakes another controller — is a different question and is not answered here.
- Arm B's API figure is Radiant's counter as a **delta against an N=0 baseline**. Radiant is a shared host serving other programs, so that is the arm's *marginal* cost and not an isolated total. The record says so in the row.
- The `-perseid-poll=1s` rung was **not run**. It would collapse the writes ratio toward 1×, and it would also make every program on the cluster re-run 15× more often — ~64 passes/second at N=64 against ~4.3 today, each a wasm instantiation plus the step's reads. Radiant's CPU at that rate is unmeasured, so the trade is named and not quantified.
- Neither the destination's **staleness** nor the **fraction of source changes that reached it** is a column. Under 1 Hz churn arm B relays about 1 change in 15 and the reaction quantiles cover only those; arm A relays every one. A rung with a change period longer than `-perseid-poll` would make the writes columns equal, and it was not run.
- `CachedObjects` is **derived, not measured**: controller-runtime exports no cache-size metric, and each manager's informer is label-scoped to its own pair. The field carries its own basis string so nobody promotes it.

[sdk]: https://github.com/apsis-io/sdk
