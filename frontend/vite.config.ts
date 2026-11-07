/// <reference types="vitest/config" />
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// The API's CORS rules and the Cognito callback URL allow exactly http://localhost:5173,
// so refuse to fall back to another port.
export default defineConfig({
  plugins: [react()],
  server: { host: "localhost", port: 5173, strictPort: true },
  test: {
    environment: "jsdom",
    environmentOptions: { jsdom: { url: "http://localhost:5173/" } },
    globals: true,
    setupFiles: ["./src/test-setup.ts"],
  },
});
