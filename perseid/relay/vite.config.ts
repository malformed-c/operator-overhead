import { defineConfig } from 'vite'

// The host interfaces are EXTERNAL: they are supplied by the linker at
// componentization, not bundled. Marking them so is what lets `import { get }
// from 'radiant:reconcile/observe@0.1.0'` survive the bundle as an import that
// dwarf can resolve into a WIT import — bundling would turn a capability into a
// missing module.
export default defineConfig({
  build: {
    target: 'esnext', minify: true, emptyOutDir: true, outDir: 'dist',
    lib: { entry: 'src/main.ts', formats: ['es'], fileName: () => 'main.js' },
    rollupOptions: {
      external: (id) => id.startsWith('radiant:') || id.startsWith('periapsis:') || id.startsWith('wasi:'),
    },
  },
})
