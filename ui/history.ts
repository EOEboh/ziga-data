import { $, clone, relativeTime } from "./dom";
import { HistoryResponse } from "./types";

export function renderHistory(history: HistoryResponse): void {
  const list = $("history-list");
  list.textContent = "";
  if (history.items.length === 0) {
    const note = document.createElement("div");
    note.className = "empty-note";
    note.textContent = "Nothing added yet. Confirmed leads show up here.";
    list.appendChild(note);
    return;
  }
  for (const item of history.items) {
    const row = clone("tpl-history-item");
    const who = item.result?.name || item.result?.contact || "Unnamed lead";
    row.querySelector<HTMLElement>(".who")!.textContent = who;
    row.querySelector<HTMLElement>(".what")!.textContent = item.result?.need || item.excerpt;
    row.querySelector<HTMLElement>(".when")!.textContent =
      item.result?.date || relativeTime(item.created_at);
    list.appendChild(row);
  }
}
