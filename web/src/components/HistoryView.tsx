import { useEffect, useState } from "react";
import { Api } from "../api";
import { relativeTime } from "../format";
import { HistoryItem } from "../types";

// The last 50 written leads: name / need / date. Fetches on every visit
// (the component remounts per navigation, like the old hash handler).
export function HistoryView({ api }: { api: Api }) {
  const [items, setItems] = useState<HistoryItem[] | null>(null);

  useEffect(() => {
    let alive = true;
    api
      .history()
      .then((h) => {
        if (alive) setItems(h.items);
      })
      .catch(() => {
        if (alive) setItems([]);
      });
    return () => {
      alive = false;
    };
  }, [api]);

  return (
    <div className="bg-surface border border-line rounded-card p-0">
      {items && items.length === 0 && (
        <div className="text-text-2 text-center p-8">Nothing added yet. Confirmed leads show up here.</div>
      )}
      {items?.map((item) => (
        <div
          key={item.id}
          className="flex items-baseline gap-4 px-4 py-3 border-b border-line last:border-b-0"
        >
          <span className="font-medium min-w-[160px]">
            {item.result?.name || item.result?.contact || "Unnamed lead"}
          </span>
          <span className="flex-1 text-text-2 overflow-hidden text-ellipsis whitespace-nowrap">
            {item.result?.need || item.excerpt}
          </span>
          <span className="text-text-2 text-xs whitespace-nowrap">
            {item.result?.date || relativeTime(item.created_at)}
          </span>
        </div>
      ))}
    </div>
  );
}
