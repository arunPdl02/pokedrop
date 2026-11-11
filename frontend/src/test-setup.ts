import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach } from "vitest";
import { applyConfig } from "./config";

// The app fills `config` from /config.json before it renders; tests use fixed values instead.
applyConfig(
  { apiUrl: "https://api.test.example", clientId: "test-client-id", hostedUiDomain: "auth.test.example" },
  "http://localhost:5173",
);

afterEach(() => {
  cleanup();
  sessionStorage.clear();
  localStorage.clear();
  window.history.replaceState({}, "", "/");
});
