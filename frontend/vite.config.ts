import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
  plugins: [react(), tailwindcss()],
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
