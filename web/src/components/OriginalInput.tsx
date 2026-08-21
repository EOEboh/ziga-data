import { relativeTime } from "../format";
import type { LeadSource } from "../types";
import { Button } from "./Button";
import { SourceBadge } from "./SourceBadge";

// Read-only left panel: the raw pasted text / uploaded screenshot, when it
// arrived, the detected source, and Edit and re-run.
//
// For an email-captured lead the header also names the sender and subject. The
// user was not present when it arrived, so "where did this come from?" is a
// question they will actually have — and with forwarding involved, the sender
// shown here is the original correspondent, not whoever forwarded it.
export function OriginalInput({
  text,
  imageUrl,
  createdAt,
  detectedSource,
  showEditRerun,
  onEditRerun,
  source,
  fromAddress,
  subject,
}: {
  text: string;
  imageUrl: string | null;
  createdAt: string;
  detectedSource: string | null;
  showEditRerun: boolean;
  onEditRerun: () => void;
  source?: LeadSource;
  fromAddress?: string;
  subject?: string;
}) {
  const fromEmail = source === "email";
  return (
    <div className="bg-surface border border-line rounded-card p-4">
      <div className="flex items-baseline gap-2 mb-2 font-semibold flex-wrap">
        Original input <SourceBadge source={source} />
        <span className="font-normal text-text-2 text-[13px]">
          {fromEmail ? "captured" : "pasted"} {relativeTime(createdAt)}
        </span>
      </div>
      {fromEmail && (fromAddress || subject) && (
        <div className="mb-2 text-[13px] text-text-2 border-l-2 border-line pl-2">
          {fromAddress && (
            <div>
              From <span className="text-text">{fromAddress}</span>
            </div>
          )}
          {subject && (
            <div className="[word-break:break-word]">
              Subject <span className="text-text">{subject}</span>
            </div>
          )}
        </div>
      )}
      <div className="bg-bg border border-line rounded-ctl p-3 font-mono text-[13px] whitespace-pre-wrap [word-break:break-word] max-h-[360px] overflow-y-auto">
        {text}
        {imageUrl && <img src={imageUrl} alt="submitted screenshot" className="max-w-full rounded-[4px] block" />}
      </div>
      {detectedSource !== null && (
        <div className="mt-2 text-text-2 text-[13px]">Detected source: {detectedSource || "unknown"}</div>
      )}
      {showEditRerun && (
        <div className="mt-3">
          <Button variant="ghost" onClick={onEditRerun}>
            Edit and re-run
          </Button>
        </div>
      )}
    </div>
  );
}
