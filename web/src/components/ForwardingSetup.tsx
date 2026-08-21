import { useState } from "react";
import type { Api } from "../api";
import { InboundAddressCard } from "./InboundAddressCard";
import { VerificationBanner } from "./VerificationBanner";

type Provider = "gmail" | "outlook" | "other";

const TABS: { id: Provider; label: string }[] = [
  { id: "gmail", label: "Gmail" },
  { id: "outlook", label: "Outlook" },
  { id: "other", label: "Other" },
];

/**
 * How to point a mailbox at the capture address.
 *
 * Two pieces of this copy are functional requirements rather than decoration:
 *
 * 1. Forward a FILTERED label, not the whole inbox. A blanket forward pipes
 *    every newsletter and receipt into extraction. Most is filtered, but the
 *    daily cap is finite, and a user who spends it on marketing mail has their
 *    real leads quarantined.
 * 2. The confirmation-code step is explained BEFORE they hit it. Providers
 *    email a code to the destination, and a user who does not expect that
 *    concludes the feature is broken and stops.
 */
export function ForwardingSetup({ api }: { api: Api }) {
  const [tab, setTab] = useState<Provider>("gmail");

  return (
    <div className="bg-surface border border-line rounded-card p-4">
      <div className="font-semibold mb-3">Forward leads automatically</div>

      <div className="mb-4 pb-4 border-b border-line">
        <InboundAddressCard api={api} compact />
      </div>

      <div className="mb-3 rounded-ctl border border-line bg-bg p-3 text-[13px] text-text-2">
        <span className="text-text font-medium">Forward a filtered label, not your whole inbox.</span> A rule that
        forwards everything sends newsletters and receipts here too. They get filtered out, but they use up your daily
        capture limit — so narrow the rule to the mail that actually contains leads.
      </div>

      <div className="mb-3 rounded-ctl border border-line bg-bg p-3 text-[13px] text-text-2">
        <span className="text-text font-medium">You'll be asked for a confirmation code.</span> Your email provider
        sends one to the address above to prove you own it. It arrives here, not in your inbox — we'll show it to you on
        this page as soon as it lands.
      </div>

      <div className="flex gap-1 border-b border-line mb-3">
        {TABS.map((t) => (
          <button
            key={t.id}
            type="button"
            onClick={() => setTab(t.id)}
            className={[
              "px-3 py-2 text-[13px] cursor-pointer border-b-2 -mb-px",
              tab === t.id ? "border-green text-text font-medium" : "border-transparent text-text-2 hover:text-text",
            ].join(" ")}
          >
            {t.label}
          </button>
        ))}
      </div>

      {tab === "gmail" && (
        <ol className="text-[13px] text-text-2 space-y-2 list-decimal pl-5">
          <li>
            In Gmail, open <span className="text-text">Settings → See all settings → Forwarding and POP/IMAP</span>.
          </li>
          <li>
            Click <span className="text-text">Add a forwarding address</span> and paste your capture address.
          </li>
          <li>Gmail sends a confirmation code here. It will appear on this page within a few seconds.</li>
          <li>
            Paste the code back into Gmail (or open the link we show) to confirm.
          </li>
          <li>
            Now go to <span className="text-text">Filters and Blocked Addresses → Create a new filter</span>. Filter on
            what identifies your leads — a label, a sender, words like "enquiry" or "quote".
          </li>
          <li>
            Choose <span className="text-text">Forward it to</span> and pick your capture address.
          </li>
        </ol>
      )}

      {tab === "outlook" && (
        <ol className="text-[13px] text-text-2 space-y-2 list-decimal pl-5">
          <li>
            In Outlook on the web, open <span className="text-text">Settings → Mail → Rules</span>.
          </li>
          <li>
            Choose <span className="text-text">Add new rule</span> and name it something like "Leads to Ziga".
          </li>
          <li>Set a condition that matches your leads — a sender, a subject word, or a category.</li>
          <li>
            Set the action to <span className="text-text">Forward to</span> and enter your capture address.
          </li>
          <li>
            Tick <span className="text-text">Keep a copy</span> so the original stays in your mailbox.
          </li>
          <li>Outlook usually forwards without a confirmation code. If it asks, the code appears on this page.</li>
        </ol>
      )}

      {tab === "other" && (
        <div className="text-[13px] text-text-2 space-y-2">
          <p>
            Most providers have a forwarding or rules setting. Look for "Forwarding", "Filters" or "Rules", and forward
            only the mail that contains leads.
          </p>
          <p>
            If your provider sends a confirmation code to prove you own the address, it will show up on this page.
          </p>
          <p>You can also just forward a lead by hand any time — no setup needed.</p>
        </div>
      )}

      <VerificationBanner api={api} active />
    </div>
  );
}
