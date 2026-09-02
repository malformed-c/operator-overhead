// ARM B2: ONE Perseid relaying EVERY pair — the symmetric counterpart to arm A3.
//
// ═══════════════════════════════════════════════════════════════════════════
// ***A3 IS ONE MANAGER FOR N PAIRS, SO THE FAIR COMPARISON IS ONE PERSEID FOR N
// PAIRS.*** Comparing N Perseids against ONE consolidated manager measures
// consolidation, not runtime — and consolidation won that by 61x. This arm takes
// it away as a variable: same fixtures, same field, same window, one process on
// each side.
//
// It is also the shape a Perseid author would actually reach for. A step is a
// total function of its observations; nothing about the contract says one
// program watches one object, and `spec.writes` takes a LIST.
// ═══════════════════════════════════════════════════════════════════════════
//
// Read config inside run(): Wizer pre-init evaluates module scope at BUILD time,
// so a top-level process.env captures the build host's environment.

import { get, now } from 'radiant:reconcile/observe@0.1.0'
import { ensureAll } from 'radiant:reconcile/ensure@0.1.0'

type Obs = { tag: 'known'; val: string } | { tag: 'absent' } | { tag: 'unknown' }

const yieldStep = JSON.stringify({ o: 'yield' })

// `data.<key>` — the field PATH from the object root, which is what `ensure`
// and the aperture expression language take. The bare key writes a top-level
// field the apiserver prunes, and radiant reports that write as Applied.
const fieldPath = (f: string) => 'data.' + f

const id = (i: number) => String(i).padStart(3, '0')

function fieldOf(raw: string, field: string): string | null {
  try {
    const v = (JSON.parse(raw) as { data?: Record<string, unknown> })?.data?.[field]

    return typeof v === 'string' ? v : null
  } catch {
    return null
  }
}

export const step = {
  run(): string {
    const e = process.env
    const ns = e.RELAY_NS, field = e.RELAY_FIELD, count = Number(e.RELAY_COUNT)
    // A missing or unusable config yields rather than guessing a path: a
    // fabricated one reads a DIFFERENT object successfully, or is refused by the
    // grant and reported `absent` — which a step reads as "gone, terminate".
    if (!ns || !field || !Number.isFinite(count) || count <= 0) return yieldStep

    const base = '/api/v1/namespaces/' + ns + '/configmaps/'
    const stamp = String(now())
    let wrote = 0

    for (let i = 0; i < count; i++) {
      const src = base + 'src-' + id(i)
      const dst = base + 'dst-' + id(i)
      const s = get(src) as Obs
      if (s.tag !== 'known') continue
      const want = fieldOf(s.val, field)
      if (want === null) continue


      const d = get(dst) as Obs
      if (d.tag === 'unknown') continue
      const have = d.tag === 'known' ? fieldOf(d.val, field) : null
      if (have === want) continue

      // ONE obligation per object, two fields, one apiserver write — so a reader
      // never sees the new value beside the previous stamp.
      ensureAll(dst, [
        { path: fieldPath(field), value: { tag: 'text', val: want } },
        { path: fieldPath('t'), value: { tag: 'text', val: want + '@' + stamp } },
      ])
      wrote++
    }

    // ***ANY WRITE MEANS YIELD, NOT PARK.*** Parking now would sleep on a
    // condition the obligations this pass declared are about to falsify.
    if (wrote > 0) return yieldStep

    // ═══════════════════════════════════════════════════════════════════════
    // ***ONE CALL OVER EVERY PAIR, AT ANY N — periapsis ADR-0101.***
    // This was a disjunction of per-pair comparisons, and it cost 2N+1 calls
    // against DefaultLimits.MaxCalls=16, so a fused relay could not park past
    // SEVEN pairs. Halving it to `Get(src) != "<seen>"` reached fifteen and gave
    // up detecting destination drift to get there.
    //
    // `Fields` is one call over a collection, ordered by object name, compared
    // element-wise — so BOTH sides are one call and the expression is 3 calls
    // (two, plus the host's backstop) at N=8 and at N=64 alike. The ceiling on
    // how many objects one parked program may watch is gone, and it did not go
    // by raising the budget: reads come from radiant's informer cache, so a wake
    // check costs ZERO apiserver requests however many pairs it covers.
    //
    // Ordering is what makes the comparison correct: `src-000…src-N` and
    // `dst-000…dst-N` sort into the same positions, so element i of one list is
    // the counterpart of element i of the other. A workload whose two sides did
    // not sort into correspondence could not use this form.
    //
    // Authority is BY KIND: this needs `observe-configmaps`, the same grant the
    // per-object reads needed. A collection is not a wider capability, which is
    // why it did not need a new one.
    // ═══════════════════════════════════════════════════════════════════════
    const coll = '/api/v1/namespaces/' + ns + '/configmaps'
    const resume =
      `Fields("${coll}", "overhead.apsis/side=src", "${fieldPath(field)}")` +
      ` != Fields("${coll}", "overhead.apsis/side=dst", "${fieldPath(field)}")`

    return JSON.stringify({ o: 'quiesce', resume })
  },
}
