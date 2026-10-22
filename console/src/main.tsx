import React from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { start } from "./theme";
import "./styles.css";

// The person's theme, from this browser's cache, before React mounts —
// so the first paint is the one they chose rather than the default
// followed by a correction. The server's answer arrives a moment later
// and overwrites it. See theme.ts.
start();

createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
