// THE GUEST-SIDE PROBE. It answers questions that can only be answered from
// inside a component, by writing what it observed into a ConfigMap.
//
// ═══════════════════════════════════════════════════════════════════════════
// ***THIS IS THE ONLY EFFECT TEST FOR `spec.config` DELIVERY THAT EXISTS, AND
// THAT IS WHY IT IS A COMMITTED ARTIFACT RATHER THAN SOMETHING IN /tmp.***
//
// wasmtime's `WasiCtx` exposes no reader, so nothing in periapsis' own tree can
// assert that a guest received its environment — the best available in-tree check
// is a `contains("builder.envs")` grep over the source, which is an assertion on a
// MARKER rather than on the effect the marker is supposed to produce. That is the
// exact shape this benchmark spent a day deleting.
//
// The chain it has already walked, each stage looking correct while the next was
// broken (2026-09-01):
//
//	spec.config validated and persisted        ... and radiant read nothing
//	radiant rendered it onto the pod           ... and the isolated profile stripped it
//	the annotation was written                 ... as "", which strips
//	the annotation said "all", argv was right  ... and a step's WasiCtx was built
//	                                               fresh, with no envs, EVER
//
// Four consecutive fixes, each verified from the pod spec, each believed to
// unblock the work, none of which could. Every one of them was caught by this
// file reporting `keys=0` — a number no marker can produce.
//
// ***`keys` IS THE TOTAL, NOT A LOOKUP, AND THAT IS LOAD-BEARING.*** Reporting
// only whether RELAY_SRC was present would have read as a filtering or naming
// problem. Zero TOTAL — with seventeen service-link variables also missing —
// is what pointed at construction rather than at forwarding, and it is what
// located the defect.
// ═══════════════════════════════════════════════════════════════════════════
//
// Nothing runs at module scope: dwarf's Wizer pre-init evaluates top-level code
// at BUILD time, so a module-scope `process.env` read would capture the build
// host's environment and this probe would report the wrong machine's answer.

import { get, now } from 'radiant:reconcile/observe@0.1.0'
import { ensure } from 'radiant:reconcile/ensure@0.1.0'

type Obs = { tag: 'known'; val: string } | { tag: 'absent' } | { tag: 'unknown' }

// HARDCODED ON PURPOSE. The probe must not depend on the mechanism it tests: if
// it read its own paths from `spec.config`, a config-delivery failure would make
// it report nothing at all instead of reporting `keys=0`.
const NS = '/api/v1/namespaces/overhead'
const REPORT_TO = NS + '/configmaps/dst-000'
const REPORT_FIELD = 'data.v'

export const step = {
  run(): string {
    const e = process.env
    // Named after WHERE each value is read, never after what it would prove —
    // a probe called PROBE_ENV_REACHES_GUEST read from a pod spec is a name
    // doing the asserting, which is how a marker gets mistaken for an effect.
    const flags =
      (e.RELAY_SRC ? 'S' : '-') + (e.RELAY_DST ? 'D' : '-') + (e.RELAY_FIELD ? 'F' : '-')

    let m = 'env=' + flags + ' keys=' + Object.keys(e).length
    const podMissing = get(NS + '/pods/does-not-exist') as Obs
    m += ' podAbsent=' + podMissing.tag
    const cmMissing = get(NS + '/configmaps/does-not-exist') as Obs
    m += ' cmAbsent=' + cmMissing.tag
    const cmReal = get(NS + '/configmaps/src-000') as Obs
    m += ' cmReal=' + cmReal.tag
    if (cmReal.tag === 'known') m += ' len=' + cmReal.val.length
    m += ' at=' + String(now())

    ensure(REPORT_TO, REPORT_FIELD, { tag: 'text', val: m })

    // ***YIELD, NEVER PARK.*** A parked probe stops re-reporting, so a fix that
    // lands later is invisible until somebody recreates it. Yielding means the
    // next pass re-answers every question against the current runtime.
    return JSON.stringify({ o: 'yield' })
  },
}
