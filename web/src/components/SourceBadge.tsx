import type { LeadSource } from "../types";

/**
 * Where a lead came from: pasted by hand, or captured from email.
 *
 * Neutral colours on purpose. styles.css reserves amber and red for extraction
 * confidence states, and a lead arriving by email is not a caution — it is just
 * a fact about how it got here.
 */
export function SourceBadge({ source }: { source?: LeadSource }) {
  const email = source === "email";
  return (
    <span
      className="inline-flex items-center gap-1 rounded-full border border-line bg-bg px-2 py-0.5 text-[11px] text-text-2"
      title={email ? "Captured from an email forwarded to your capture address" : "Pasted into Ziga"}
    >
      <span aria-hidden="true">{email ? "✉" : "▦"}</span>
      {email ? "Email" : "Pasted"}
    </span>
  );
}
