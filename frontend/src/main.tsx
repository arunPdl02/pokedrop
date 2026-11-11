import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { loadConfig } from "./config";
import "./index.css";

const root = createRoot(document.getElementById("root")!);

// Nothing reads `config` before this resolves, so the app only renders once it is filled. If the
// config cannot be loaded the page says so instead of staying blank.
loadConfig().then(
  () =>
    root.render(
      <StrictMode>
        <App />
      </StrictMode>,
    ),
  (error: unknown) =>
    root.render(
      <main>
        <h1>PokéDrop</h1>
        <p role="alert">
          The app could not start. {error instanceof Error ? error.message : "Its configuration could not be loaded."}
        </p>
      </main>,
    ),
);
