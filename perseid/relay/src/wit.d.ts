// Ambient declarations for the two host interfaces this component imports and
// for dwarf's `process` global.
//
// THE HOST SPELLING, NOT A CONVENIENCE ALIAS. These are the shapes that actually
// cross the boundary: `obs` arrives as `{ tag, val }` (dwarf's binding of WIT's
// `variant obs`) and `ensure`'s value is the same tagged shape. Declaring them
// here is what makes a mismatched payload a compile error rather than something
// that crosses the boundary structurally intact and semantically wrong — the
// failure mode reconcile.wit records twice, each time with no error on either
// side.

declare module 'radiant:reconcile/observe@0.1.0' {
  export type Obs =
    | { tag: 'known'; val: string }
    | { tag: 'absent' }
    | { tag: 'unknown' }
  /** Read one object by path. NEVER throws: failure is `unknown`. */
  export function get(path: string): Obs
  /** EPOCH MILLISECONDS, UTC. The unit is in the contract on purpose. */
  export function now(): bigint
}

declare module 'radiant:reconcile/ensure@0.1.0' {
  export type Value =
    | { tag: 'text'; val: string }
    | { tag: 'num'; val: bigint }
    | { tag: 'flag'; val: boolean }
  /** Declare that a field should hold a value. Returns nothing, by design. */
  export function ensure(path: string, field: string, value: Value): void
  /** One object, several fields, ONE apiserver write — so a reader can never
   *  observe the new value of one field beside the old value of another. */
  export function ensureAll(path: string, fields: { path: string; value: Value }[]): void
}

// dwarf's built-in `process`, backed by `wasi:cli/environment@0.2.x`.
// `env` is freshly re-fetched from the host on every access, never cached —
// which is why reading it inside `run()` works and reading it at module scope
// would capture the BUILD host's environment via Wizer pre-init.
declare const process: {
  readonly env: Record<string, string | undefined>
}
