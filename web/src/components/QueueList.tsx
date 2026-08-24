import { relativeTime } from "../format";
import type { Submission } from "../types";
import { SourceBadge } from "./SourceBadge";

/**
 * Everything waiting for review, oldest first.
 *
 * Before email ingestion the queue was almost always one item — you pasted a
 * lead and reviewed it on the spot — so showing a single pane and a count was
 * enough. Leads that arrive unattended pile up, and a count is not a queue: it
 * tells you three things are waiting without letting you look at any of them
 * but the first.
 *
 * Rendered only when more than one is waiting. A one-row list is noise.
 */
export function QueueList({
  items,
  selectedId,
  onSelect,
}: {
  items: Submission[];
  selectedId: number | null;
  onSelect: (sub: Submission) => void;
}) {
  if (items.length < 2) return null;

  return (
    <div className="bg-surface border border-line rounded-card overflow-hidden">
      <div className="flex items-baseline justify-between px-4 py-2.5 border-b border-line">
        <span className="font-semibold text-[13px]">Waiting for review</span>
        <span className="text-[12px] text-text-2">{items.length} leads, oldest first</span>
      </div>
      <ul>
        {items.map((sub) => {
          const selected = sub.id === selectedId;
          return (
            <li key={sub.id}>
              <button
                type="button"
                onClick={() => onSelect(sub)}
                aria-current={selected ? "true" : undefined}
                className={[
                  "w-full text-left px-4 py-2.5 flex items-baseline gap-3 cursor-pointer",
                  "border-b border-line last:border-b-0",
                  selected ? "bg-green-tint" : "hover:bg-bg",
                ].join(" ")}
              >
                <SourceBadge source={sub.source} />
                <span className="font-medium min-w-[140px] max-w-[200px] truncate">{primaryLabel(sub)}</span>
                <span className="flex-1 text-text-2 truncate">{secondaryLabel(sub)}</span>
                {sub.status === "failed_write" && (
                  // A failed write is still waiting on the user, and is the one
                  // item in here that needs action rather than just review.
                  <span className="text-[11px] text-red-text whitespace-nowrap">write failed</span>
                )}
                <span className="text-[12px] text-text-2 whitespace-nowrap">{relativeTime(sub.created_at)}</span>
              </button>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

/** Who the lead is, falling back through what we actually know about it. */
function primaryLabel(sub: Submission): string {
  return (
    sub.result?.name ||
    sub.result?.contact ||
    sub.from_address ||
    (sub.source === "email" ? "Email lead" : "Pasted lead")
  );
}

/** What they want, or failing that what the message was about. */
function secondaryLabel(sub: Submission): string {
  return sub.result?.need || sub.subject || sub.input.text?.slice(0, 80) || "";
}
