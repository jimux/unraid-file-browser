import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The SPA is installed at /usr/local/emhttp/plugins/filebrowser/app/ and loaded
// in an iframe, so every asset URL has to be relative to the document.
export default defineConfig({
  plugins: [react()],
  base: "./",
  build: {
    outDir: "dist",
    emptyOutDir: true,
    target: "es2020",
  },
  server: {
    port: 5173,
    strictPort: false,
    proxy: {
      // `filebrowserd -dev -listen 127.0.0.1:8384`.
      // SSE (/api/v1/index/events) passes through unbuffered by default.
      "/api": {
        target: "http://127.0.0.1:8384",
        changeOrigin: false,
      },
    },
  },
});
