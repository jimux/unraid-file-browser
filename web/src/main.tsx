import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import App from "./App";
import { ErrorBoundary } from "./components/ErrorBoundary";
import { applyTheme, readThemeFromUrl } from "./hooks/useTheme";
import "./styles.css";

// Paint the right palette before React mounts so there is no white flash inside
// a dark Unraid webGUI.
applyTheme(readThemeFromUrl());

const el = document.getElementById("root");
if (!el) throw new Error("#root missing");

createRoot(el).render(
  <StrictMode>
    <ErrorBoundary>
      <App />
    </ErrorBoundary>
  </StrictMode>,
);
