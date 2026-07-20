// App shell and review-flow orchestration, ported from ui/main.ts. All async
// flows read post-await state through stateRef — the React equivalent of the
// vanilla module-state reads — so in-flight requests never stomp state the
// user has since moved past (most importantly the composing guard).

import { useEffect, useReducer, useRef } from "react";
import { ApiError, createApi } from "./api";
import { ComposeBox } from "./components/ComposeBox";
import { HistoryView } from "./components/HistoryView";
import { PreviewStrip } from "./components/PreviewStrip";
import { ReviewPane } from "./components/ReviewPane";
import { TopBar } from "./components/TopBar";
import { initialState, reducer } from "./state";
import { Submission } from "./types";

const api = createApi();

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

const MAX_IMAGE_BYTES = 5 << 20; // mirrors the server-side limit

export function App() {
  const [state, dispatch] = useReducer(reducer, initialState);
  const stateRef = useRef(state);
  stateRef.current = state;

  // ---- async flows ----------------------------------------------------------

  async function refreshBadge(): Promise<void> {
    try {
      const q = await api.queue();
      dispatch({ type: "BADGE", count: q.count });
    } catch {
      dispatch({ type: "BADGE", count: 0 });
    }
  }

  // advance loads the next queued item, or returns to the empty state.
  async function advance(): Promise<void> {
    let next: Submission | null = null;
    try {
      const q = await api.queue();
      dispatch({ type: "BADGE", count: q.count });
      next = q.items[0] ?? null;
    } catch {
      dispatch({ type: "BADGE", count: 0 });
    }
    if (next) {
      dispatch({ type: "ENTER_REVIEW", submission: next });
    } else {
      dispatch({ type: "ENTER_EMPTY" });
    }
  }

  // startComposing shows the paste box on demand ("New lead") without
  // touching the queue — pending items stay behind the Review badge.
  function startComposing(): void {
    dispatch({ type: "START_COMPOSING" });
    if (location.hash === "#/history") location.hash = "#/";
    refreshBadge();
  }

  // openQueue returns from the compose box (or history) to the review queue.
  // A plain hash link is not enough: clicking "#/" while already there fires
  // no hashchange.
  async function openQueue(): Promise<void> {
    const s = stateRef.current;
    if (!s.composing && s.phase !== "empty") return; // already on the queue
    dispatch({ type: "COMPOSING_ENDED" });
    await advance();
  }

  // editRerun copies the original input back into the compose box; the next
  // successful submit creates a replacement and discards this submission.
  async function editRerun(): Promise<void> {
    const sub = stateRef.current.submission;
    if (!sub) return;
    startComposing();
    dispatch({ type: "RERUN_STARTED", id: sub.id, text: sub.input.text ?? "" });
    if (sub.input.image_url) {
      try {
        const resp = await fetch(sub.input.image_url);
        if (!resp.ok) throw new Error(`image fetch: ${resp.status}`);
        const blob = await resp.blob();
        const file = new File([blob], "original." + (blob.type.split("/")[1] ?? "png"), { type: blob.type });
        dispatch({ type: "SET_COMPOSE_FILE", file });
      } catch {
        dispatch({ type: "SUBMIT_ERROR", message: "Could not load the original image — re-attach it to include it" });
      }
    }
  }

  async function startExtraction(): Promise<void> {
    const s = stateRef.current;
    const text = s.composeText.trim();
    const file = s.composeFile;
    if (!text && !file) {
      dispatch({ type: "SUBMIT_ERROR", message: "Add some text or an image first" });
      return;
    }
    // Client-side pre-check mirroring the server's 5 MB cap: reject before
    // any network call, keeping the file attached so the user can swap it.
    if (file && file.size > MAX_IMAGE_BYTES) {
      dispatch({ type: "SUBMIT_ERROR", message: "image exceeds the 5 MB limit" });
      return;
    }

    const form = new FormData();
    if (text) form.set("text", text);
    let localImageUrl: string | null = null;
    if (file) {
      form.set("image", file);
      localImageUrl = URL.createObjectURL(file);
    }

    // Captured before the await: a mid-flight "New lead" click resets
    // rerunOf, but this submission still replaces the original.
    const rerunOf = s.rerunOf;

    dispatch({
      type: "EXTRACTION_STARTED",
      text,
      localImageUrl,
      startedAt: new Date().toISOString(),
    });

    let sub: Submission;
    try {
      sub = await api.submit(form);
    } catch (err) {
      dispatch({
        type: "EXTRACTION_FAILED",
        message: err instanceof ApiError ? err.message : "Extraction failed. Try again",
      });
      return;
    }

    // The re-run replaced the original: discard it, unless the server deduped
    // us onto an existing submission (unchanged content returns the old row —
    // discarding it would destroy the very submission we are now showing).
    if (rerunOf !== null) {
      dispatch({ type: "RERUN_CLEARED" });
      if (!sub.duplicate && sub.id !== rerunOf) {
        await api.discard(rerunOf).catch(() => {});
      }
    }

    // If the user hit "New lead" while this extraction was in flight, leave
    // their fresh compose box alone — the result waits in the queue.
    if (stateRef.current.composing) {
      refreshBadge();
      return;
    }

    dispatch({ type: "COMPOSE_CLEARED" });

    if (sub.duplicate && sub.status === "written") {
      dispatch({ type: "DUPLICATE_SETTLED" });
      return;
    }
    dispatch({ type: "ENTER_REVIEW", submission: sub });
    refreshBadge();
  }

  async function confirm(): Promise<void> {
    const s = stateRef.current;
    const sub = s.submission;
    if (!sub || s.phase === "confirming") return;
    dispatch({ type: "CONFIRM_STARTED" });

    try {
      await api.confirm(sub.id, stateRef.current.fields);
    } catch (err) {
      if (err instanceof ApiError && err.status === 422 && err.fieldStates) {
        dispatch({ type: "CONFIRM_INVALID", fieldStates: err.fieldStates });
        return;
      }
      if (err instanceof ApiError && err.status === 409) {
        // Already written (double click, second tab): treat as settled.
        await settle();
        return;
      }
      dispatch({
        type: "WRITE_FAILED",
        message:
          err instanceof ApiError && err.status !== 0
            ? "Could not write to your sheet."
            : "Could not reach the server.",
      });
      return;
    }
    await settle();
  }

  // settle re-fetches the preview so the pending row visibly becomes a normal
  // row (green tint fading out), then advances to the next queued item.
  async function settle(): Promise<void> {
    dispatch({ type: "SETTLE_BEGIN" });
    try {
      const preview = await api.preview();
      dispatch({ type: "SETTLE_FLASH", preview });
    } catch {
      // strip stays as-is; not worth blocking the flow
    }
    await sleep(600); // let the fade finish before repainting the strip
    await advance();
  }

  async function discard(): Promise<void> {
    const sub = stateRef.current.submission;
    if (!sub) return;
    dispatch({ type: "DISCARD_STARTED" });
    try {
      await api.discard(sub.id);
    } catch {
      // fall through: advance re-syncs with the server either way
    }
    dispatch({ type: "SETTLE_BEGIN" });
    await advance();
  }

  // ---- effects --------------------------------------------------------------

  // Boot: seed the badge and preview strip, then land on the first queued
  // item or the empty compose box.
  useEffect(() => {
    (async () => {
      const [queueRes, previewRes] = await Promise.allSettled([api.queue(), api.preview()]);
      dispatch({
        type: "PREVIEW_LOADED",
        preview:
          previewRes.status === "fulfilled"
            ? previewRes.value
            : { columns: [], rows: [], error: "preview unavailable" },
      });
      if (queueRes.status === "fulfilled" && queueRes.value.items.length > 0) {
        dispatch({ type: "BADGE", count: queueRes.value.count });
        dispatch({ type: "ENTER_REVIEW", submission: queueRes.value.items[0] });
      } else {
        dispatch({ type: "BADGE", count: queueRes.status === "fulfilled" ? queueRes.value.count : 0 });
        dispatch({ type: "ENTER_EMPTY" });
      }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Hash routing: #/ (review) and #/history, no router library.
  useEffect(() => {
    const apply = () =>
      dispatch({ type: "ROUTE", route: location.hash === "#/history" ? "history" : "review" });
    apply();
    window.addEventListener("hashchange", apply);
    return () => window.removeEventListener("hashchange", apply);
  }, []);

  // Object-URL lifecycle: revoke the previous local image URL whenever it is
  // replaced or cleared (the reducer never touches the URL API).
  const prevUrl = useRef<string | null>(null);
  useEffect(() => {
    if (prevUrl.current && prevUrl.current !== state.localImageUrl) {
      URL.revokeObjectURL(prevUrl.current);
    }
    prevUrl.current = state.localImageUrl;
  }, [state.localImageUrl]);

  // ---- render ---------------------------------------------------------------

  // The "New lead" button shows whenever the paste box itself is not on screen.
  const newLeadVisible = state.booted && (state.route === "history" || state.phase !== "empty");

  const pending =
    state.submission !== null
      ? { values: state.fields, kind: state.phase === "write_failed" ? ("error" as const) : ("pending" as const) }
      : null;

  return (
    <>
      <TopBar
        api={api}
        route={state.route}
        queueCount={state.queueCount}
        newLeadVisible={newLeadVisible}
        onNewLead={startComposing}
        onOpenQueue={openQueue}
      />
      <main className="max-w-[1040px] mx-auto p-6">
        {state.route === "history" ? (
          <section>
            <HistoryView api={api} />
          </section>
        ) : (
          <section>
            {state.booted && state.phase === "empty" && (
              <ComposeBox
                text={state.composeText}
                file={state.composeFile}
                submitError={state.submitError}
                onTextChange={(text) => dispatch({ type: "SET_COMPOSE_TEXT", text })}
                onFileChange={(file) => dispatch({ type: "SET_COMPOSE_FILE", file })}
                onSubmit={startExtraction}
              />
            )}
            {state.booted && state.phase !== "empty" && (
              <ReviewPane
                phase={state.phase}
                submission={state.submission}
                extractingText={state.extractingText}
                extractStartedAt={state.extractStartedAt}
                localImageUrl={state.localImageUrl}
                fields={state.fields}
                fieldStates={state.fieldStates}
                edited={state.edited}
                writeError={state.writeError}
                busy={state.busy}
                onFieldChange={(name, value) => dispatch({ type: "FIELD_EDITED", name, value })}
                onConfirm={confirm}
                onDiscard={discard}
                onRetry={confirm}
                onEditRerun={editRerun}
              />
            )}
            {state.booted && state.preview !== null && (
              <PreviewStrip preview={state.preview} pending={pending} settleToken={state.settleToken} />
            )}
          </section>
        )}
      </main>
    </>
  );
}
