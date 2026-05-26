import { defineConfig } from 'vite'
import { nodePolyfills } from 'vite-plugin-node-polyfills'

export default defineConfig({
  // Relative asset paths so the bundle works at any URL root.
  base: './',

  plugins: [
    nodePolyfills({
      globals: { Buffer: true, global: true, process: true },
    }),
  ],

  build: {
    // Build locally; `make browser` copies dist/ → /var/www/html via sudo cp.
    outDir: './dist',
    emptyOutDir: true,
    target: 'es2022',  // WebTransport requires a modern baseline
    sourcemap: true,
    chunkSizeWarningLimit: 600,
    rollupOptions: {
      output: {
        // Split vendor (libp2p stack) from app code for better cache reuse.
        manualChunks(id) {
          if (id.includes('node_modules')) return 'vendor'
        },
      },
    },
  },

  optimizeDeps: {
    esbuildOptions: { target: 'es2022' },
  },
})
