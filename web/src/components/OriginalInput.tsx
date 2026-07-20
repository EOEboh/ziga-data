import { relativeTime } from "../format";
import { Button } from "./Button";

// Read-only left panel: the raw pasted text / uploaded screenshot, when it
// was pasted, the detected source, and Edit and re-run.
export function OriginalInput({
  text,
  imageUrl,
  createdAt,
  detectedSource,
  showEditRerun,
  onEditRerun,
}: {
  text: string;
  imageUrl: string | null;
  createdAt: string;
  detectedSource: string | null;
  showEditRerun: boolean;
  onEditRerun: () => void;
}) {
  return (
    <div className="bg-surface border border-line rounded-card p-4">
      <div className="flex items-baseline gap-2 mb-2 font-semibold">
        Original input{" "}
        <span className="font-normal text-text-2 text-[13px]">pasted {relativeTime(createdAt)}</span>
      </div>
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
