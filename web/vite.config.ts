import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const BACKEND = "http://localhost:9494";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": { target: BACKEND },
      "/auth": { target: BACKEND },
      "/healthz": { target: BACKEND },
    },
  },
});
