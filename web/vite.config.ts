import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';

// The coordinator serves this build from its own binary, so the dev server
// proxies the API to a locally running `loadwave serve` instead of the
// frontend needing to know an absolute base URL.
const COORDINATOR = process.env.LOADWAVE_API ?? 'http://127.0.0.1:8088';

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    // Embedded in the Go binary, so keep the output small and predictable.
    // Fingerprinted asset names let the server cache them immutably.
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
    chunkSizeWarningLimit: 700,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: COORDINATOR,
        changeOrigin: true,
        // The live stream is a WebSocket upgrade under the same prefix.
        ws: true,
      },
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    include: ['src/**/*.test.{ts,tsx}'],
  },
});
