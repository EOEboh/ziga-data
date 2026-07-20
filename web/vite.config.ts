import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// base "./" keeps asset URLs relative so the bundle works served from the
// Go binary's embedded FS at any mount point.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  base: "./",
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
  server: {
    // Dev server proxies API calls to a locally running Go server; use
    // ?mock=1 to work with no backend at all.
    proxy: {
      "/api": "http://localhost:8080",
    },
  },
});
