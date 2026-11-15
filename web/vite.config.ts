// Vite build and dev server, and Vitest (ADR-0057, ADR-0061).
import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react()],
  build: {
    // Embedded into the control plane by go:embed under the webui tag;
    // the directory is gitignored (UI-09).
    outDir: "../internal/controlplane/webui/dist",
    emptyOutDir: true,
    assetsInlineLimit: 0, // no data: URIs, so the CSP's img-src stays 'self'
    modulePreload: { polyfill: false }, // the polyfill is an inline script
    sourcemap: false,
  },
  server: {
    host: "127.0.0.1",
    port: 5173,
    strictPort: true,
    proxy: { "/api": "http://127.0.0.1:8080" },
  },
  test: {
    environment: "jsdom",
    setupFiles: ["src/test/setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
    restoreMocks: true,
    coverage: {
      provider: "v8",
      include: ["src/**/*.{ts,tsx}"],
      exclude: ["src/**/*.gen.ts", "src/**/*.test.{ts,tsx}", "src/test/**"],
      // The logic a bug in which leaks tokens or corrupts data (ADR-0061);
      // screens are held to their state tests instead.
      reporter: ["text"],
      thresholds: {
        "src/api/**": { lines: 80, branches: 80 },
        "src/auth/**": { lines: 80, branches: 80 },
        "src/stream/**": { lines: 80, branches: 80 },
      },
    },
  },
});
