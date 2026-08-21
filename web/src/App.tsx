// App shell and review-flow orchestration, ported from ui/main.ts. All async
// flows read post-await state through stateRef — the React equivalent of the
// vanilla module-state reads — so in-flight requests never stomp state the
// user has since moved past (most importantly the composing guard).

import { useEffect, useReducer, useRef } from "react";
import { ApiError, api } from "./api";
import { AccountMenu } from "./components/AccountMenu";
import { ComposeBox } from "./components/ComposeBox";
import { HistoryView } from "./components/HistoryView";
import { AwayBanner } from "./components/AwayBanner";
import { QuarantineView } from "./components/QuarantineView";
import { ForwardingSetup } from "./components/ForwardingSetup";
import { DroppedFieldsNotice } from "./components/DroppedFieldsNotice";
import { PreviewStrip } from "./components/PreviewStrip";
import { ReviewPane } from "./components/ReviewPane";
import { TopBar } from "./components/TopBar";
import { initialState, reducer, type Route } from "./state";
import { Me, Submission } from "./types";

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

const MAX_IMAGE_BYTES = 5 << 20; // mirrors the server-side limit

export function App({ me, reload }: { me: Me; reload: () => void }) {
  const [state, dispatch] = useReducer(reducer, initialState);
  const stateRef = useRef(state);
  stateRef.current = state;

  // ---- async flows ----------------------------------------------------------

  const emailIngest = me.config.email_ingest === true;

  async function refreshBadge(): Promise<void> {
    try {
      const q = await api.queue();
      dispatch({ type: "BADGE", count: q.count });
      dispatch({ type: "AWAY_COUNT", count: q.captured_while_away ?? 0 });
    } catch {
      dispatch({ type: "BADGE", count: 0 });
    }
    if (!emailIngest) return;
    try {
      const filtered = await api.quarantine();
      dispatch({ type: "QUARANTINE_BADGE", count: filtered.items.length });
    } catch {
      // A failed quarantine count must not break the review queue: it is a
      // secondary surface, and the badge simply stays as it was.
    }
  }

  // dismissAway clears the "while you were away" banner by marking the queue
  // seen. It affects only the count — the leads themselves are untouched.
  async function dismissAway(): Promise<void> {
    dispatch({ type: "AWAY_DISMISSED" });
    try {
      await api.markQueueSeen();
    } catch {
      // Cosmetic; the banner reappears next load at worst.
    }
  }

  async function reviewAway(): Promise<void> {
    await dismissAway();
    await openQueue();
  }

  // advance loads the next queued item, or returns to the empty state.
  async function advance(): Promise<void> {
    let next: Submission | null = null;
    try {
      const q = await api.queue();
      dispatch({ type: "BADGE", count: q.count });
      dispatch({ type: "AWAY_COUNT", count: q.captured_while_away ?? 0 });
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
      const res = await api.confirm(sub.id, stateRef.current.fields);
      // A destination that could not accept every field still wrote the rest;
      // name what it dropped rather than losing it quietly.
      dispatch({ type: "FIELDS_DROPPED", fields: res.dropped_fields ?? null });
    } catch (err) {
      if (err instanceof ApiError && err.status === 422 && err.fieldStates) {
        dispatch({ type: "CONFIRM_INVALID", fieldStates: err.fieldStates });
        return;
      }
      if (err instanceof ApiError && err.status === 409) {
        // A lost-access 409 is not "already written": the lead is still
        // pending and the user has to reconnect before a retry can work.
        if (/reconnect/i.test(err.message)) {
          dispatch({ type: "WRITE_FAILED", message: err.message });
          return;
        }
        // Already written (double click, second tab): treat as settled.
        await settle();
        return;
      }
      dispatch({
        type: "WRITE_FAILED",
        message:
          err instanceof ApiError && err.status !== 0
            ? "Could not write to your destination."
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
      if (queueRes.status === "fulfilled") {
        dispatch({ type: "AWAY_COUNT", count: queueRes.value.captured_while_away ?? 0 });
      }
      if (queueRes.status === "fulfilled" && queueRes.value.items.length > 0) {
        dispatch({ type: "BADGE", count: queueRes.value.count });
        dispatch({ type: "ENTER_REVIEW", submission: queueRes.value.items[0] });
      } else {
        dispatch({ type: "BADGE", count: queueRes.status === "fulfilled" ? queueRes.value.count : 0 });
        dispatch({ type: "ENTER_EMPTY" });
      }
      if (emailIngest) {
        try {
          const filtered = await api.quarantine();
          dispatch({ type: "QUARANTINE_BADGE", count: filtered.items.length });
        } catch {
          // Secondary surface; the review queue must boot regardless.
        }
      }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Hash routing: #/ (review), #/history, #/quarantine, #/email. No router
  // library — react-router handles auth/onboarding, this handles the app's
  // own tabs.
  useEffect(() => {
    const apply = () => {
      const hash = location.hash;
      const route: Route =
        hash === "#/history"
          ? "history"
          : hash === "#/quarantine"
            ? "quarantine"
            : hash === "#/email"
              ? "email"
              : "review";
      dispatch({ type: "ROUTE", route });
    };
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
  const newLeadVisible = state.booted && (state.route !== "review" || state.phase !== "empty");

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
        quarantineCount={state.quarantineCount}
        emailIngest={emailIngest}
        newLeadVisible={newLeadVisible}
        onNewLead={startComposing}
        onOpenQueue={openQueue}
        accountMenu={<AccountMenu api={api} me={me} reload={reload} />}
      />
      <main className="max-w-[1040px] mx-auto p-6">
        {state.route === "history" ? (
          <section>
            <HistoryView api={api} />
          </section>
        ) : state.route === "quarantine" ? (
          <section>
            <QuarantineView api={api} onChanged={refreshBadge} />
          </section>
        ) : state.route === "email" ? (
          <section>
            <ForwardingSetup api={api} />
          </section>
        ) : (
          <section className="space-y-4">
            {state.booted && emailIngest && (
              <AwayBanner count={state.awayCount} onReview={reviewAway} onDismiss={dismissAway} />
            )}
            {state.booted && state.phase === "empty" && (
              <ComposeBox
                text={state.composeText}
                file={state.composeFile}
                submitError={state.submitError}
                onTextChange={(text) => dispatch({ type: "SET_COMPOSE_TEXT", text })}
                onFileChange={(file) => dispatch({ type: "SET_COMPOSE_FILE", file })}
                onSubmit={startExtraction}
                emailIngest={emailIngest}
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
            {state.droppedFields && state.droppedFields.length > 0 && (
              <DroppedFieldsNotice
                fields={state.droppedFields}
                onDismiss={() => dispatch({ type: "FIELDS_DROPPED", fields: null })}
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
