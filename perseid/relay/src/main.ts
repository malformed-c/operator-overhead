// ARM B: the same relay as `cmd/crrelay`, as a Perseid.
//
// ═══════════════════════════════════════════════════════════════════════════
// ***ONE ARTIFACT FOR EVERY INSTANCE, AS OF PERIAPSIS 9c0f37178.***
//
// This was a TEMPLATE. `spec.config` was in the Perseid CRD, described as
// "opaque per-program configuration, passed to the step", and read by nothing —
// and `PodForPerseid` set no environment — so an artifact had no runtime channel
// by which to learn which pair it served. The only way to vary one was to
// compile the paths in and build a component per instance: at N=64 that was 64
// builds, 64 ingests and 93.53 MiB in the node library, against 40.42 MiB for
// one controller-runtime image.
//
// `spec.config` now flattens to the pod's environment, and dwarf's `process.env`
// is backed by `wasi:cli/environment` — which every perseid world already
// imports, so this needs no new capability and no new WIT interface. The 64
// artifacts collapse to one.
// ═══════════════════════════════════════════════════════════════════════════
//
// Everything runs inside `run()` and never at module scope: dwarf's Wizer
// pre-init evaluates top-level code at BUILD time. That constraint is
// LOAD-BEARING here rather than incidental — a module-scope `process.env` read
// would capture the BUILD host's environment into the artifact, and every
// instance would then relay whatever pair the build machine happened to imply.
// dwarf re-fetches `process.env` from the host on every access, so reading it
// inside `run` is both correct and cheap.

import { get, now } from 'radiant:reconcile/observe@0.1.0'
import { ensureAll } from 'radiant:reconcile/ensure@0.1.0'

// The three-valued observation, as dwarf binds WIT's `variant obs`.
//
// ***`absent` AND `unknown` ARE DIFFERENT ANSWERS AND THE VARIANT IS WHY.***
// `absent` is a positive claim that the object is not there; `unknown` is "we
// could not tell". A host returning `option<...>` would collapse them and a
// step would conclude "gone, tear down" from an apiserver blip — which
// reconcile.wit names as the most-repeated defect in that codebase's history.
type Obs = { tag: 'known'; val: string } | { tag: 'absent' } | { tag: 'unknown' }

// The keys `spec.config` must carry. Neither may start with `PERSEID_` — that
// prefix is reserved to the host, so a program cannot squat on a name the
// runtime may want later (`validConfigKey`, internal/trailop/perseidpod.go).
const KEY_SRC = 'RELAY_SRC'
const KEY_DST = 'RELAY_DST'
const KEY_FIELD = 'RELAY_FIELD'

// The operator's own clock, stamped beside the relayed value. See
// internal/relay's FieldT: it splits convergence into the arm's REFLEX
// (notice + decide) and the write's round trip, which is not the arm's cost.
//
// `observe.now()` is EPOCH MILLISECONDS by contract — the unit is written into
// reconcile.wit precisely because `-> u64` is equally satisfied by seconds,
// millis, micros and nanos, and two implementations that pick differently do not
// fail, they are silently off by a factor of a thousand. Arm A stamps
// `time.Now().UnixMilli()`, so the two arms are the same unit from the same host
// clock.
const FIELD_T = 't'

// ═══════════════════════════════════════════════════════════════════════════
// ***THE DATA KEY AND THE FIELD PATH ARE NOT THE SAME STRING, AND USING ONE FOR
// BOTH FAILS SILENTLY IN THE WORST DIRECTION.***
//
// `RELAY_FIELD` is `v` — the key inside a ConfigMap's `data` map, which is what
// this step extracts from the JSON `observe.get` returns. But `ensure` and the
// aperture expression language take a field PATH FROM THE OBJECT ROOT:
// reconcile.wit's own example is `ensure(configMap(...), 'data.mode', 'fast')`.
//
// Passing the bare key wrote a TOP-LEVEL `v` on the ConfigMap. Measured: radiant
// reported `Applied: performed 1 write(s): Ensure(.../dst-000, "v", "...")` and
// the object did not change — the apiserver prunes an unknown field on a typed
// resource, so the write was issued, accepted, and dropped, with a success event
// naming it. Nothing anywhere said the field went nowhere.
//
// One derivation, at the two places that need a path, so the two spellings
// cannot drift.
// ═══════════════════════════════════════════════════════════════════════════
const fieldPath = (field: string) => 'data.' + field

const yieldStep = JSON.stringify({ o: 'yield' })
const terminate = JSON.stringify({ o: 'terminate' })

// fieldRef is an observe path narrowed to one field: `<path>#data.<field>`.
//
// ***THE BLOB NEVER ENTERS WASM.*** The host splits on `#`, runs the same parse,
// confinement and routing the bare path always did, and projects the field on the
// way out — so only the value crosses the boundary. `#` is unambiguous by
// construction: a DNS-1123 name cannot contain one, which a last-dot split cannot
// say (`configmaps/kube-root-ca.crt` is a real object on this cluster).
//
// It matters here beyond tidiness: this arm measures what a wasm step costs, and
// parsing a whole ConfigMap in QuickJS to read one key was the benchmark's own
// overhead rather than the workload's.
const fieldRef = (path: string, field: string) => path + '#' + fieldPath(field)

export const step = {
  run(): string {
    const env = process.env
    const src = env[KEY_SRC]
    const dst = env[KEY_DST]
    const field = env[KEY_FIELD]

    // ***A MISSING CONFIG KEY YIELDS RATHER THAN GUESSING A PATH.*** There is
    // no sensible default: a fabricated path would be read successfully against
    // the WRONG object, or refused by the grant's namespace check and reported
    // as `absent` — which this step reads as "the source is gone, terminate",
    // a terminal state, from a typo in a ConfigMap. Yielding leaves the program
    // visibly running and doing nothing, which is the recoverable failure.
    if (!src || !dst || !field) return yieldStep

    const s = get(fieldRef(src, field)) as Obs
    if (s.tag === 'unknown') return yieldStep

    // ***`absent` ON A NARROWED READ IS TWO DIFFERENT FACTS AND ONLY ONE OF THEM
    // IS TERMINAL.*** The host passes `absent` through unchanged — a missing
    // FIELD of an object it read, and an object that is not there, arrive as the
    // same answer. Terminating on both would kill every instance at startup,
    // because the harness creates each pair empty and writes into it afterwards:
    // the ordinary pre-first-write state is a source that exists with no `data.v`.
    //
    // So the object is re-read ONLY here, to tell the two apart. The common path
    // stays one narrowed read; this costs a second one in the rare case, which is
    // the right way round.
    if (s.tag === 'absent') {
      const obj = get(src) as Obs
      // The source is gone. Nothing left to reconcile toward, ever — and
      // `Terminated` is genuinely terminal, enforced in radiant's runner.
      if (obj.tag === 'absent') return terminate

      return yieldStep
    }

    const want = s.val

    const d = get(fieldRef(dst, field)) as Obs
    if (d.tag === 'unknown') return yieldStep
    // `absent` needs no disambiguation on this side: a destination that does not
    // exist and one that carries no value are both "not converged yet", and the
    // ensure below is what creates or fills it.
    const have = d.tag === 'known' ? d.val : null

    if (have !== want) {
      // ***DECLARING, NOT DOING.*** `ensure` emits an obligation; radiant
      // services it, deduplicates it by identity and owns the back-off. The
      // step never records that it asked — it re-derives the same conclusion
      // from the same observation next pass, which is why no in-flight
      // bookkeeping can leak out of the program.
      //
      // It returns nothing on purpose. A step that could observe its own
      // obligation failing would have to HANDLE it, handling means remembering,
      // and a step that remembers is no longer total. The level-triggered
      // property IS the error channel.
      // ═══════════════════════════════════════════════════════════════════
      // ***ONE OBLIGATION, TWO FIELDS, ONE APISERVER WRITE.*** These were two
      // `ensure` calls until periapsis 7ad3e7f43, and radiant applied them as
      // two writes — so a reader could observe the new `data.v` beside the
      // PREVIOUS `data.t`, durably, until the next pass. Measured before
      // `ensure-all` existed: reaction samples with min -72064 ms and max +71 ms.
      // Mixed signs, which is the signature of a torn pair rather than of clock
      // skew, because an offset shifts every sample the same way.
      //
      // ⚠ ***THE `<value>@<clock>` ENCODING STAYS, AND REMOVING IT WOULD MAKE
      // THE FIX UNVERIFIABLE.*** It looks like a workaround that `ensure-all`
      // retires. It is not: it is the DETECTOR. The harness counts a pair whose
      // stamp names a different value as `stale`, and the acceptance test for
      // this change is `stale == 0` against a recorded red of 3-of-6. Drop the
      // encoding and `stale` becomes 0 trivially — for every build, fixed or
      // not — which is a control that cannot fail, the exact shape this project
      // has spent its length deleting.
      // ═══════════════════════════════════════════════════════════════════
      ensureAll(dst, [
        { path: fieldPath(field), value: { tag: 'text', val: want } },
        { path: fieldPath(FIELD_T), value: { tag: 'text', val: want + '@' + String(now()) } },
      ])

      return yieldStep
    }

    // Converged. Park on the condition that would change the answer.
    //
    // ***THIS IS THE ONE LINE THE WHOLE ARM EXISTS TO MEASURE, AND IT ONLY
    // BECAME EXPRESSIBLE IN PERIAPSIS 4f263170c.*** A resume is DATA: radiant
    // evaluates it WITHOUT invoking the step, wakes the program when it stops
    // holding, and renders it into `status.waitingFor` so an operator can see
    // what a sleeping program is waiting on. Before that commit a resume could
    // not name a ConfigMap — `configmaps:read` was excluded from `resumeCaps` —
    // so this relay could only have parked on a deadline, and the latency column
    // would have compared a capability list rather than an architecture.
    //
    // Comparing two Gets rather than a Get against a literal is deliberate:
    // parking on `!= <the value I saw>` goes stale the moment the destination is
    // edited by something else, and the program would then be asleep on a
    // condition that no longer describes convergence.
    //
    // `now()` is read so the pass is dated even though the host folds in its own
    // bounded backstop deadline.
    void now()

    return JSON.stringify({
      o: 'quiesce',
      resume: `Get("${src}", "${fieldPath(field)}") != Get("${dst}", "${fieldPath(field)}")`,
    })
  },
}
