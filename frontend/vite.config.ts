import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "../internal/query/ui/dist",
    emptyOutDir: true,
  },
  server: {
    port: 3001,
    proxy: {
      "/v1": "http://localhost:18080",
      "/api": "http://localhost:18080",
      "/healthz": "http://localhost:18080",
    },
  },
  base: "/",
});
