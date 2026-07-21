import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { Root } from "./Root";
import "./styles.css";

// No <StrictMode>: the app's boot/history effects are ports of one-shot vanilla
// calls; StrictMode's dev double-invoke would double-fire them. BrowserRouter
// carries the auth / onboarding / reset routes; the app keeps its own in-page
// hash routing for Review vs History.
createRoot(document.getElementById("root")!).render(
  <BrowserRouter>
    <Root />
  </BrowserRouter>,
);
