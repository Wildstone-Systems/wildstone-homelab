import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Dev proxy: the Console is an API client of the ONE public plane
// (ADR-W016 A3); during `vite dev` requests to /api are forwarded to a
// local keystone so no CORS surface is ever added to the API.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": "http://127.0.0.1:9441",
    },
  },
});
