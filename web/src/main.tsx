import { createRoot } from "react-dom/client";
import { App } from "./App";
import "./styles.css";

// No <StrictMode>: the boot and history effects are ports of one-shot vanilla
// calls; StrictMode's dev double-invoke would double-fire them.
createRoot(document.getElementById("root")!).render(<App />);
