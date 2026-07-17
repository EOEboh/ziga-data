// Sheet preview strip: the last rows of the connected sheet plus, while a
// review is open, the pending row. On confirm the pending highlight settles
// into a normal row via a ~400ms background fade — this transition is the
// moment the product feels alive; don't remove it.

import { $, hide, sentenceCase, show } from "./dom";
import { PreviewResponse } from "./types";

export type PendingKind = "pending" | "error";

export interface PendingRow {
  values: Record<string, string>; // live field values, keyed by field name
  kind: PendingKind;
}

let lastPreview: PreviewResponse | null = null;

export function renderPreview(preview: PreviewResponse, pending: PendingRow | null): void {
  lastPreview = preview;
  const strip = $("sheet-preview");
  const head = $("preview-head");
  const body = $("preview-body");
  const note = $("preview-note");

  show(strip);
  head.textContent = "";
  body.textContent = "";

  if (preview.error) {
    note.textContent = "Sheet preview unavailable";
    show(note);
    return;
  }
  hide(note);

  for (const col of preview.columns) {
    const th = document.createElement("th");
    th.textContent = sentenceCase(col);
    head.appendChild(th);
  }

  for (const row of preview.rows) {
    body.appendChild(makeRow(preview.columns, (_col, i) => row[i] ?? "", null));
  }
  if (pending) {
    body.appendChild(makeRow(
      preview.columns,
      (col) => (col === "date" ? "pending" : col === "flags" ? "" : pending.values[col] ?? ""),
      pending.kind === "error" ? "row--error" : "row--pending",
    ));
  }
  if (preview.rows.length === 0 && !pending) {
    note.textContent = "No rows in the sheet yet";
    show(note);
  }
}

function makeRow(
  columns: string[],
  value: (col: string, i: number) => string,
  cls: string | null,
): HTMLTableRowElement {
  const tr = document.createElement("tr");
  if (cls) tr.className = cls;
  columns.forEach((col, i) => {
    const td = document.createElement("td");
    td.textContent = value(col, i);
    tr.appendChild(td);
  });
  return tr;
}

// updatePendingValues live-binds the pending row to the field inputs.
export function updatePendingValues(values: Record<string, string>): void {
  if (!lastPreview) return;
  const rows = $("preview-body").querySelectorAll("tr.row--pending, tr.row--error");
  const tr = rows[rows.length - 1];
  if (!tr) return;
  lastPreview.columns.forEach((col, i) => {
    if (col === "date" || col === "flags") return;
    const td = tr.children[i] as HTMLElement;
    if (col in values) td.textContent = values[col];
  });
}

// flashSettle renders the fresh preview (which now contains the confirmed
// row) with the last row tinted, then releases the tint on the next frame so
// it fades out over the CSS 400ms background transition.
export function flashSettle(preview: PreviewResponse): void {
  renderPreview(preview, null);
  const rows = $("preview-body").querySelectorAll("tr");
  const last = rows[rows.length - 1];
  if (!last) return;
  last.classList.add("row--flash");
  // Double rAF: guarantee the tinted frame paints before the class is
  // removed, or the fade never shows.
  requestAnimationFrame(() => {
    requestAnimationFrame(() => last.classList.remove("row--flash"));
  });
}
