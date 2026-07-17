// App shell and review-flow orchestration. State machine:
//   empty → extracting → reviewing → confirming → (settled → next/empty)
//                                       └→ write_failed → (retry = confirming)

import { Api, ApiError, createApi } from "./api";
import { $, hide, show } from "./dom";
import { renderHistory } from "./history";
import { PendingRow, flashSettle, renderPreview, updatePendingValues } from "./preview";
import {
  applyFieldStates,
  collectFields,
  hideWriteError,
  renderDetectedSource,
  renderFields,
  renderLeft,
  renderSkeleton,
  setConfirmBusy,
  showWriteError,
} from "./review";
import { PreviewResponse, Submission } from "./types";

type Phase = "empty" | "extracting" | "reviewing" | "confirming" | "write_failed";

const api: Api = createApi();

const state: {
  phase: Phase;
  submission: Submission | null;
  localImageUrl: string | null;
  preview: PreviewResponse | null;
} = { phase: "empty", submission: null, localImageUrl: null, preview: null };

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

// ---- review flow ------------------------------------------------------------

function enterEmpty(): void {
  state.phase = "empty";
  state.submission = null;
  releaseLocalImage();
  show($("empty-state"));
  hide($("review-body"));
  refreshPreviewStrip();
}

function enterReview(sub: Submission): void {
  state.phase = sub.status === "failed_write" ? "write_failed" : "reviewing";
  state.submission = sub;
  hide($("empty-state"));
  show($("review-body"));

  renderLeft(sub.input.text ?? "", state.localImageUrl ?? sub.input.image_url ?? null, sub.created_at);
  if (sub.result) renderDetectedSource(sub.result.source);
  renderFields(sub);
  if (state.phase === "write_failed") {
    showWriteError(sub.error || "Could not write to your sheet.");
  } else {
    hideWriteError();
  }
  setConfirmBusy(false);
  refreshPreviewStrip();
}

async function startExtraction(): Promise<void> {
  const text = ($("lead-text") as HTMLTextAreaElement).value.trim();
  const fileInput = $("lead-image") as HTMLInputElement;
  const file = fileInput.files?.[0] ?? null;
  if (!text && !file) {
    submitError("Add some text or an image first");
    return;
  }
  submitError(null);

  const form = new FormData();
  if (text) form.set("text", text);
  if (file) {
    form.set("image", file);
    state.localImageUrl = URL.createObjectURL(file);
  }

  state.phase = "extracting";
  hide($("empty-state"));
  show($("review-body"));
  renderLeft(text, state.localImageUrl, new Date().toISOString());
  renderSkeleton();

  let sub: Submission;
  try {
    sub = await api.submit(form);
  } catch (err) {
    // Back to the input with the text intact; nothing was stored.
    state.phase = "empty";
    releaseLocalImage();
    show($("empty-state"));
    hide($("review-body"));
    submitError(err instanceof ApiError ? err.message : "Extraction failed. Try again");
    return;
  }

  ($("lead-text") as HTMLTextAreaElement).value = "";
  fileInput.value = "";
  $("file-name").textContent = "";

  if (sub.duplicate && sub.status === "written") {
    state.phase = "empty";
    releaseLocalImage();
    show($("empty-state"));
    hide($("review-body"));
    submitError("This content was already added today. No new row was created");
    return;
  }
  enterReview(sub);
  refreshBadge();
}

async function confirm(): Promise<void> {
  const sub = state.submission;
  if (!sub || state.phase === "confirming") return;
  state.phase = "confirming";
  setConfirmBusy(true);
  hideWriteError();

  try {
    await api.confirm(sub.id, collectFields());
  } catch (err) {
    setConfirmBusy(false);
    if (err instanceof ApiError && err.status === 422 && err.fieldStates) {
      state.phase = "reviewing";
      applyFieldStates(err.fieldStates);
      refreshPreviewStrip();
      return;
    }
    if (err instanceof ApiError && err.status === 409) {
      // Already written (double click, second tab): treat as settled.
      await settle();
      return;
    }
    state.phase = "write_failed";
    showWriteError(err instanceof ApiError && err.status !== 0 ? "Could not write to your sheet." : "Could not reach the server.");
    paintPending("error");
    return;
  }
  await settle();
}

// settle re-fetches the preview so the pending row visibly becomes a normal
// row (green tint fading out), then advances to the next queued item.
async function settle(): Promise<void> {
  state.submission = null;
  releaseLocalImage();
  try {
    state.preview = await api.preview();
    flashSettle(state.preview);
  } catch {
    // strip stays as-is; not worth blocking the flow
  }
  await sleep(600); // let the fade finish before repainting the strip
  await advance();
}

async function discard(): Promise<void> {
  const sub = state.submission;
  if (!sub) return;
  setConfirmBusy(true);
  try {
    await api.discard(sub.id);
  } catch {
    // fall through: advance re-syncs with the server either way
  }
  state.submission = null;
  await advance();
}

// advance loads the next queued item, or returns to the empty state.
async function advance(): Promise<void> {
  let next: Submission | null = null;
  try {
    const q = await api.queue();
    badge(q.count);
    next = q.items[0] ?? null;
  } catch {
    badge(0);
  }
  if (next) {
    enterReview(next);
  } else {
    enterEmpty();
  }
}

function submitError(msg: string | null): void {
  const el = $("submit-error");
  if (msg) {
    el.textContent = msg;
    show(el);
  } else {
    hide(el);
  }
}

function releaseLocalImage(): void {
  if (state.localImageUrl) {
    URL.revokeObjectURL(state.localImageUrl);
    state.localImageUrl = null;
  }
}

// ---- preview strip -----------------------------------------------------------

function pendingRow(): PendingRow | null {
  if (!state.submission) return null;
  return {
    values: collectFields(),
    kind: state.phase === "write_failed" ? "error" : "pending",
  };
}

function refreshPreviewStrip(): void {
  if (state.preview) renderPreview(state.preview, pendingRow());
}

function paintPending(kind: "pending" | "error"): void {
  if (!state.preview || !state.submission) return;
  renderPreview(state.preview, { values: collectFields(), kind });
}

// ---- badge --------------------------------------------------------------------

function badge(count: number): void {
  const el = $("queue-badge");
  if (count > 0) {
    el.textContent = String(count);
    show(el);
  } else {
    hide(el);
  }
}

async function refreshBadge(): Promise<void> {
  try {
    const q = await api.queue();
    badge(q.count);
  } catch {
    badge(0);
  }
}

// ---- destination picker ---------------------------------------------------------

async function initDestination(): Promise<void> {
  const toggle = $("destination-toggle") as HTMLButtonElement;
  const menu = $("destination-menu");
  const label = $("destination-label");

  try {
    const { destinations } = await api.destinations();
    const active = destinations.find((d) => d.active);
    label.textContent = active ? active.label + (active.dry_run ? " — dry run" : "") : "No destination";
    menu.textContent = "";
    for (const dest of destinations) {
      const item = document.createElement("button");
      item.type = "button";
      item.className = "dropdown-item";
      item.disabled = !!dest.disabled;
      const icon = document.createElement("span");
      icon.className = "dest-icon";
      icon.textContent = dest.type === "google_sheet" ? "▦" : "◆";
      const name = document.createElement("span");
      name.textContent = dest.label;
      item.append(icon, name);
      if (dest.coming_soon) {
        const hint = document.createElement("span");
        hint.className = "hint";
        hint.textContent = "coming soon";
        item.appendChild(hint);
      }
      menu.appendChild(item);
    }
  } catch {
    label.textContent = "Destination unavailable";
  }

  toggle.addEventListener("click", () => {
    const open = menu.hidden;
    menu.hidden = !open;
    toggle.setAttribute("aria-expanded", String(open));
  });
  document.addEventListener("click", (e) => {
    if (!$("destination").contains(e.target as Node)) {
      menu.hidden = true;
      toggle.setAttribute("aria-expanded", "false");
    }
  });
}

// ---- views ----------------------------------------------------------------------

function applyHash(): void {
  const isHistory = location.hash === "#/history";
  $("view-review").hidden = isHistory;
  $("view-history").hidden = !isHistory;
  document.querySelectorAll<HTMLAnchorElement>(".views a").forEach((a) => {
    a.classList.toggle("active", (a.dataset.view === "history") === isHistory);
  });
  if (isHistory) {
    api.history().then(renderHistory).catch(() => {
      renderHistory({ items: [] });
    });
  }
}

// ---- boot ------------------------------------------------------------------------

async function boot(): Promise<void> {
  initDestination();
  window.addEventListener("hashchange", applyHash);
  applyHash();

  $("extract-button").addEventListener("click", startExtraction);
  $("confirm-button").addEventListener("click", confirm);
  $("retry-button").addEventListener("click", confirm);
  $("discard-button").addEventListener("click", discard);

  const fileInput = $("lead-image") as HTMLInputElement;
  $("image-button").addEventListener("click", () => fileInput.click());
  fileInput.addEventListener("change", () => {
    $("file-name").textContent = fileInput.files?.[0]?.name ?? "";
  });

  // Cmd/Ctrl+Enter in the textarea submits.
  $("lead-text").addEventListener("keydown", (e) => {
    const ke = e as KeyboardEvent;
    if (ke.key === "Enter" && (ke.metaKey || ke.ctrlKey)) startExtraction();
  });

  // Live-bind the pending preview row to the field inputs.
  $("fields").addEventListener("input", () => {
    if (state.submission) updatePendingValues(collectFields());
  });

  const [queueRes, previewRes] = await Promise.allSettled([api.queue(), api.preview()]);
  if (previewRes.status === "fulfilled") {
    state.preview = previewRes.value;
  } else {
    state.preview = { columns: [], rows: [], error: "preview unavailable" };
  }
  if (queueRes.status === "fulfilled" && queueRes.value.items.length > 0) {
    badge(queueRes.value.count);
    enterReview(queueRes.value.items[0]);
  } else {
    badge(queueRes.status === "fulfilled" ? queueRes.value.count : 0);
    enterEmpty();
  }
}

boot();
