// App state machine, ported from the vanilla module state in ui/main.ts:
//   empty → extracting → reviewing → confirming → (settled → next/empty)
//                                       └→ write_failed → (retry = confirming)
//
// Field values are held here (controlled inputs) because three consumers need
// them outside the field editor: the live-bound pending preview row, the
// confirm payload, and edit preservation across 422 / failed-write retries.

import { FIELD_ORDER, FieldState, PreviewResponse, Submission, fieldValue } from "./types";

export type Phase = "empty" | "extracting" | "reviewing" | "confirming" | "write_failed";
export type Route = "review" | "history";

export interface AppState {
  phase: Phase;
  route: Route;
  // booted: boot() has decided between review and empty; nothing below the
  // top bar renders before that (the old HTML shipped both views hidden).
  booted: boolean;
  submission: Submission | null;
  // Controlled field-input values; user edits live here and survive 422s and
  // failed writes.
  fields: Record<string, string>;
  // Per-field display state from the server; replaced wholesale on 422.
  fieldStates: Record<string, FieldState>;
  // Editing a flagged field clears its caution styling — the user has
  // addressed it.
  edited: Record<string, boolean>;
  // composing: the user opened the paste box via "New lead" while queued
  // items still exist — queue-driven routing must not stomp the compose box.
  composing: boolean;
  // rerunOf: id of the submission being edited-and-re-run; discarded once the
  // replacement submission is created.
  rerunOf: number | null;
  composeText: string;
  composeFile: File | null;
  // Object URL of a just-uploaded file; revoked by an App effect when it
  // changes, so the reducer stays pure.
  localImageUrl: string | null;
  // Left-panel content while extraction is in flight (the submission does not
  // exist yet).
  extractingText: string;
  extractStartedAt: string;
  preview: PreviewResponse | null;
  // Monotonic token; each increment tells the preview strip to run one
  // green-tint settle fade on its last row.
  settleToken: number;
  queueCount: number;
  submitError: string | null;
  // Non-null while the write-error block (with Retry) is shown; the confirm
  // button hides so there is one primary action.
  writeError: string | null;
  // Confirm/discard/retry disabled while a confirm or discard is in flight.
  busy: boolean;
}

export const initialState: AppState = {
  phase: "empty",
  route: "review",
  booted: false,
  submission: null,
  fields: {},
  fieldStates: {},
  edited: {},
  composing: false,
  rerunOf: null,
  composeText: "",
  composeFile: null,
  localImageUrl: null,
  extractingText: "",
  extractStartedAt: "",
  preview: null,
  settleToken: 0,
  queueCount: 0,
  submitError: null,
  writeError: null,
  busy: false,
};

export type Action =
  | { type: "ROUTE"; route: Route }
  | { type: "BADGE"; count: number }
  | { type: "PREVIEW_LOADED"; preview: PreviewResponse }
  | { type: "SET_COMPOSE_TEXT"; text: string }
  | { type: "SET_COMPOSE_FILE"; file: File | null }
  | { type: "SUBMIT_ERROR"; message: string | null }
  | { type: "START_COMPOSING" }
  | { type: "COMPOSING_ENDED" }
  | { type: "RERUN_STARTED"; id: number; text: string }
  | { type: "RERUN_CLEARED" }
  | { type: "EXTRACTION_STARTED"; text: string; localImageUrl: string | null; startedAt: string }
  | { type: "EXTRACTION_FAILED"; message: string }
  | { type: "COMPOSE_CLEARED" }
  | { type: "DUPLICATE_SETTLED" }
  | { type: "ENTER_REVIEW"; submission: Submission }
  | { type: "FIELD_EDITED"; name: string; value: string }
  | { type: "CONFIRM_STARTED" }
  | { type: "CONFIRM_INVALID"; fieldStates: Record<string, FieldState> }
  | { type: "WRITE_FAILED"; message: string }
  | { type: "DISCARD_STARTED" }
  | { type: "SETTLE_BEGIN" }
  | { type: "SETTLE_FLASH"; preview: PreviewResponse }
  | { type: "ENTER_EMPTY" };

export function reducer(state: AppState, action: Action): AppState {
  switch (action.type) {
    case "ROUTE":
      return { ...state, route: action.route };

    case "BADGE":
      return { ...state, queueCount: action.count };

    case "PREVIEW_LOADED":
      return { ...state, preview: action.preview };

    case "SET_COMPOSE_TEXT":
      return { ...state, composeText: action.text };

    case "SET_COMPOSE_FILE":
      return { ...state, composeFile: action.file };

    case "SUBMIT_ERROR":
      return { ...state, submitError: action.message };

    // startComposing shows the paste box on demand ("New lead") without
    // touching the queue — pending items stay behind the Review badge.
    case "START_COMPOSING":
      return {
        ...state,
        composing: true,
        phase: "empty",
        submission: null,
        rerunOf: null,
        composeText: "",
        composeFile: null,
        localImageUrl: null,
        submitError: null,
      };

    // Returning to the queue ends composing immediately, before the queue
    // fetch resolves — an extraction landing mid-fetch must see it ended.
    case "COMPOSING_ENDED":
      return { ...state, composing: false };

    case "RERUN_STARTED":
      return { ...state, rerunOf: action.id, composeText: action.text };

    case "RERUN_CLEARED":
      return { ...state, rerunOf: null };

    case "EXTRACTION_STARTED":
      return {
        ...state,
        phase: "extracting",
        composing: false,
        submission: null,
        localImageUrl: action.localImageUrl,
        extractingText: action.text,
        extractStartedAt: action.startedAt,
        submitError: null,
      };

    // Back to the input with the text intact; nothing was stored.
    case "EXTRACTION_FAILED":
      return { ...state, phase: "empty", localImageUrl: null, submitError: action.message };

    case "COMPOSE_CLEARED":
      return { ...state, composeText: "", composeFile: null };

    case "DUPLICATE_SETTLED":
      return {
        ...state,
        phase: "empty",
        submission: null,
        localImageUrl: null,
        submitError: "This content was already added today. No new row was created",
      };

    case "ENTER_REVIEW": {
      const sub = action.submission;
      const fields: Record<string, string> = {};
      for (const name of FIELD_ORDER) fields[name] = fieldValue(sub.result, name);
      const failed = sub.status === "failed_write";
      return {
        ...state,
        phase: failed ? "write_failed" : "reviewing",
        booted: true,
        submission: sub,
        composing: false,
        fields,
        fieldStates: sub.field_states ?? {},
        edited: {},
        writeError: failed ? sub.error || "Could not write to your sheet." : null,
        busy: false,
      };
    }

    case "FIELD_EDITED": {
      const prior = state.fieldStates[action.name] ?? "ok";
      const edited =
        prior === "low_confidence" || prior === "missing"
          ? { ...state.edited, [action.name]: true }
          : state.edited;
      return { ...state, fields: { ...state.fields, [action.name]: action.value }, edited };
    }

    case "CONFIRM_STARTED":
      return { ...state, phase: "confirming", busy: true, writeError: null };

    // 422: the server re-flags fields; user edits stay untouched.
    case "CONFIRM_INVALID":
      return { ...state, phase: "reviewing", busy: false, fieldStates: action.fieldStates, edited: {} };

    case "WRITE_FAILED":
      return { ...state, phase: "write_failed", busy: false, writeError: action.message };

    case "DISCARD_STARTED":
      return { ...state, busy: true };

    case "SETTLE_BEGIN":
      return { ...state, submission: null, localImageUrl: null };

    case "SETTLE_FLASH":
      return { ...state, preview: action.preview, settleToken: state.settleToken + 1 };

    case "ENTER_EMPTY":
      return {
        ...state,
        phase: "empty",
        booted: true,
        submission: null,
        composing: false,
        localImageUrl: null,
        writeError: null,
        busy: false,
      };
  }
}
