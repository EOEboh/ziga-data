// Review pane rendering: original-input panel (left), extracted fields with
// per-field confidence states (right), and the action row.

import { $, clone, hide, relativeTime, sentenceCase, show } from "./dom";
import { FIELD_ORDER, FieldName, FieldState, Submission, fieldValue } from "./types";

// renderLeft paints the original-input panel. Called once per submission:
// immediately on submit (before extraction returns) or when a queued item
// is restored. localImageUrl is the object URL of a just-uploaded file.
export function renderLeft(text: string, imageUrl: string | null, createdAt: string): void {
  const raw = $("raw-input");
  raw.textContent = "";
  if (text) {
    raw.textContent = text;
  }
  if (imageUrl) {
    const img = document.createElement("img");
    img.src = imageUrl;
    img.alt = "submitted screenshot";
    raw.appendChild(img);
  }
  $("input-when").textContent = `pasted ${relativeTime(createdAt)}`;
  hide($("detected-source"));
}

export function renderDetectedSource(source: string): void {
  const el = $("detected-source");
  el.textContent = `Detected source: ${source || "unknown"}`;
  show(el);
}

// renderSkeleton fills the right column with placeholder rows while the
// extraction request is in flight.
export function renderSkeleton(): void {
  const fields = $("fields");
  fields.textContent = "";
  for (let i = 0; i < FIELD_ORDER.length; i++) {
    fields.appendChild(clone("tpl-skeleton-row"));
  }
  hide($("action-row"));
  hide($("multi-lead-banner"));
  hide($("write-error"));
}

// renderFields paints one editable input per schema field with its
// confidence state. Flagged fields are ordinary inputs — fixing one is a
// click into it, never a separate flow.
export function renderFields(sub: Submission): void {
  const fields = $("fields");
  fields.textContent = "";
  for (const name of FIELD_ORDER) {
    const row = clone("tpl-field-row");
    const label = row.querySelector<HTMLElement>(".name")!;
    const pill = row.querySelector<HTMLElement>(".pill")!;
    const input = row.querySelector<HTMLInputElement>(".field")!;

    label.textContent = sentenceCase(name);
    input.name = name;
    input.value = fieldValue(sub.result, name);
    applyFieldState(input, pill, sub.field_states?.[name] ?? "ok");

    // Editing a flagged field clears its caution styling — the user has
    // addressed it.
    input.addEventListener("input", () => {
      if (input.classList.contains("field--low") || input.classList.contains("field--missing")) {
        input.classList.add("field--edited");
        hide(pill);
      }
    }, { once: false });

    fields.appendChild(row);
  }

  if (sub.result?.multiple_leads_detected) {
    show($("multi-lead-banner"));
  } else {
    hide($("multi-lead-banner"));
  }
  show($("action-row"));
}

export function applyFieldStates(states: Record<string, FieldState>): void {
  for (const input of fieldInputs()) {
    const row = input.closest(".field-row")!;
    const pill = row.querySelector<HTMLElement>(".pill")!;
    input.classList.remove("field--edited");
    applyFieldState(input, pill, states[input.name] ?? "ok");
  }
}

function applyFieldState(input: HTMLInputElement, pill: HTMLElement, state: FieldState): void {
  input.classList.remove("field--low", "field--missing");
  input.placeholder = "";
  hide(pill);
  pill.classList.remove("pill--amber", "pill--red");
  if (state === "low_confidence") {
    input.classList.add("field--low");
    pill.classList.add("pill--amber");
    pill.textContent = "low confidence";
    show(pill);
  } else if (state === "missing") {
    input.classList.add("field--missing");
    input.placeholder = "Not stated. Add if known";
    pill.classList.add("pill--red");
    pill.textContent = "not found";
    show(pill);
  }
}

export function fieldInputs(): HTMLInputElement[] {
  return [...$("fields").querySelectorAll<HTMLInputElement>("input.field")];
}

// collectFields reads the current (possibly edited) values for confirm.
export function collectFields(): Record<string, string> {
  const out: Record<string, string> = {};
  for (const input of fieldInputs()) {
    out[input.name as FieldName] = input.value;
  }
  return out;
}

export function setConfirmBusy(busy: boolean): void {
  ($("confirm-button") as HTMLButtonElement).disabled = busy;
  ($("discard-button") as HTMLButtonElement).disabled = busy;
  ($("retry-button") as HTMLButtonElement).disabled = busy;
}

export function showWriteError(message: string): void {
  const el = $("write-error");
  el.querySelector("span")!.textContent = message;
  show(el);
  // Retry is the one action while the write error is up; keeping the confirm
  // button too would mean two competing primary actions.
  hide($("confirm-button"));
}

export function hideWriteError(): void {
  hide($("write-error"));
  show($("confirm-button"));
}
