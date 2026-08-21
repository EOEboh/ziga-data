import { useEffect, useState } from "react";
import type { Api } from "../api";
import type { QuarantineItem } from "../types";

/**
 * Surfaces a pending forwarding-confirmation handshake.
 *
 * When a user sets up automatic forwarding, their provider emails a code and a
 * link to the destination — which is us. Without showing it, they simply
 * cannot finish setup, and the failure is silent: they wait for a code that
 * arrived somewhere they cannot see.
 *
 * `active` gates the poll, so it runs only while the setup instructions are on
 * screen and stops as soon as they are not.
 */
export function VerificationBanner({ api, active }: { api: Api; active: boolean }) {
  const [items, setItems] = useState<QuarantineItem[]>([]);

  useEffect(() => {
    if (!active) return;
    let live = true;
    const load = () =>
      api
        .quarantine("verification")
        .then((r) => live && setItems(r.items ?? []))
        .catch(() => {});
    load();
    const t = setInterval(load, 5000);
    return () => {
      live = false;
      clearInterval(t);
    };
  }, [api, active]);

  if (items.length === 0) return null;

  return (
    <div className="mt-4 space-y-3">
      {items.map((item) => (
        <div key={item.id} className="bg-green-tint border border-green rounded-card p-4">
          <div className="font-semibold mb-1">Your confirmation code arrived</div>
          <p className="text-[13px] text-text-2 mb-3">
            {item.from_name || item.from_address} sent this to confirm forwarding. Paste the code back into your email
            settings, or open the link.
          </p>
          {item.verify_code && (
            <div className="mb-3">
              <div className="text-[12px] text-text-2 mb-1">Confirmation code</div>
              <input
                readOnly
                value={item.verify_code}
                onFocus={(e) => e.currentTarget.select()}
                aria-label="Confirmation code"
                className="bg-surface border border-line rounded-ctl px-3 py-2 font-mono text-[18px] tracking-wider w-full max-w-[240px]"
              />
            </div>
          )}
          {item.verify_url && (
            <a
              href={item.verify_url}
              target="_blank"
              rel="noreferrer noopener"
              className="inline-block rounded-ctl px-4 py-2 border font-semibold border-green bg-green text-white hover:bg-green-deep hover:border-green-deep"
            >
              Open the confirmation link
            </a>
          )}
        </div>
      ))}
    </div>
  );
}
