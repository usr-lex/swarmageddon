/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { fileURLToPath } from "node:url";

// The Go backend (control-center, or a port-forward to it) listens on 8443.
const backend = process.env.SWARM_BACKEND ?? "https://localhost:8443";
const proxy = { target: backend, changeOrigin: true, secure: false };

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  build: {
    outDir: "dist",
    target: "es2022",
    chunkSizeWarningLimit: 1500,
  },
  server: {
    proxy: {
      "/api": proxy,
      "/auth": proxy,
      "/ws": { ...proxy, ws: true },
      // Connect services are mounted at /swarm.<package>.<Service>/<Method>.
      "^/swarm\\.": proxy,
    },
  },
  preview: { port: 4173, strictPort: true },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
  },
});
