import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

// BASE_PATH mounts the SPA under a URL prefix (GitLab nginx routes /-/banyan/
// here with full-path passthrough); vite bakes it into every asset URL.
const base = (process.env.BASE_PATH || "").replace(/\/+$/, "");
const bff = process.env.BFF_URL || "http://127.0.0.1:3000";

export default defineConfig({
  base: base === "" ? "/" : base + "/",
  plugins: [react()],
  build: { outDir: "dist" },
  server: {
    proxy: {
      "/api": { target: bff, changeOrigin: true },
      "/auth": { target: bff, changeOrigin: true },
      "/avatar.png": { target: bff, changeOrigin: true },
    },
  },
  test: { environment: "jsdom", include: ["src/**/*.test.{ts,tsx}"] },
});
