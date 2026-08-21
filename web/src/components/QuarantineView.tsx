import { useCallback, useEffect, useState } from "react";
import type { Api } from "../api";
import { relativeTime } from "../format";
import type { QuarantineItem } from "../types";
import { Button } from "./Button";

/**
 * User-facing wording for each filter reason.
 *
 * These are explanations, not apologies. The user needs enough to decide
 * whether the filter was right, and the exact rule that fired is one click
 * further in — see `detail`.
 */
const REASONS: Record<string, string> = {
  blocked_sender: "You blocked this sender",
  machine_mail: "Looks automated — a newsletter, notification or auto-reply",
  calendar_invite: "A calendar invite, not an enquiry",
  no_text: "No readable text — attachments aren't read yet",
  too_short: "Too short to contain a lead",
  size_rejected: "Too large to process",
  parse_failed: "We couldn't read this message's format",
  rate_limited: "Past your daily capture limit",
};

function reasonLabel(reason: string): string {
  return REASONS[reason] ?? "Filtered";
}

/**
 * Filtered mail, with the reason and a way back.
 *
 * This view is what turns "a lead is never silently lost" from a claim into
 * something the user can check. Every filter decision is a judgement call, and
 * some will be wrong — a "support@" address that is really a person, a
 * three-word message that really is an enquiry — so every one is reversible.
 */
export function QuarantineView({ api, onChanged }: { api: Api; onChanged: () => void }) {
  const [items, setItems] = useState<QuarantineItem[] | null>(null);
  const [busy, setBusy] = useState<number | null>(null);
  const [error, setError] = useState("");

  const load = useCallback(() => {
    api
      .quarantine()
      .then((r) => setItems(r.items ?? []))
      .catch((e: any) => setError(e?.message ?? "Could not load filtered mail"));
  }, [api]);

  useEffect(load, [load]);

  async function act(id: number, fn: () => Promise<unknown>) {
    setBusy(id);
    setError("");
    try {
      await fn();
      load();
      onChanged();
    } catch (e: any) {
      setError(e?.message ?? "That didn't work. Try again");
    } finally {
      setBusy(null);
    }
  }

  async function block(item: QuarantineItem) {
    if (!item.from_address) return;
    await act(item.id, async () => {
      await api.blockSender(item.from_address!);
      await api.dismiss(item.id);
    });
  }

  if (items === null) return <div className="text-text-2 text-[13px]">Loading…</div>;

  return (
    <div>
      <div className="mb-3">
        <h2 className="font-semibold">Filtered mail</h2>
        <p className="text-[13px] text-text-2 mt-1">
          Email that reached your capture address but didn't look like a lead. Nothing is deleted — if we got it wrong,
          send it to your review queue.
        </p>
      </div>

      {error && <div className="mb-3 text-[13px] text-red-text">{error}</div>}

      {items.length === 0 ? (
        <div className="bg-surface border border-line rounded-card p-4 text-[13px] text-text-2">
          Nothing filtered. Anything that arrives and doesn't look like a lead will show up here.
        </div>
      ) : (
        <ul className="space-y-2">
          {items.map((item) => (
            <li key={item.id} className="bg-surface border border-line rounded-card p-4">
              <div className="flex items-baseline justify-between gap-2 flex-wrap">
                <div className="font-medium [word-break:break-word]">{item.subject || "(no subject)"}</div>
                <div className="text-[12px] text-text-2 whitespace-nowrap">{relativeTime(item.received_at)}</div>
              </div>
              <div className="text-[13px] text-text-2 mt-0.5 [word-break:break-word]">
                {item.from_name ? `${item.from_name} · ` : ""}
                {item.from_address}
              </div>
              <div className="text-[13px] text-text-2 mt-2 [word-break:break-word]">{item.excerpt}</div>

              <div className="mt-3 flex items-center gap-2 flex-wrap">
                <span
                  className="text-[12px] text-text-2 border border-line rounded-full px-2 py-0.5"
                  title={item.detail || undefined}
                >
                  {reasonLabel(item.reason)}
                </span>
                <div className="flex-1" />
                {item.rescuable ? (
                  <Button onClick={() => act(item.id, () => api.rescue(item.id))} disabled={busy === item.id}>
                    Send to review
                  </Button>
                ) : (
                  // Retention has cleared the body, so there is nothing left to
                  // extract. Say so rather than offering a button that fails.
                  <span className="text-[12px] text-text-2">Too old to recover</span>
                )}
                <Button variant="ghost" onClick={() => act(item.id, () => api.dismiss(item.id))} disabled={busy === item.id}>
                  Dismiss
                </Button>
                {item.from_address && (
                  <Button variant="ghost" onClick={() => block(item)} disabled={busy === item.id}>
                    Block sender
                  </Button>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
