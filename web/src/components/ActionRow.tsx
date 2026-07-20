import { Button } from "./Button";

// Confirm / discard plus the trust microcopy; when a write has failed, the
// inline error with Retry replaces the confirm button — Retry is the one
// action while the write error is up, keeping a single primary action.
export function ActionRow({
  writeError,
  busy,
  onConfirm,
  onDiscard,
  onRetry,
}: {
  writeError: string | null;
  busy: boolean;
  onConfirm: () => void;
  onDiscard: () => void;
  onRetry: () => void;
}) {
  return (
    <>
      <div className="flex items-center gap-2 mt-6">
        {writeError === null && (
          <Button variant="primary" disabled={busy} onClick={onConfirm}>
            Confirm and add row
          </Button>
        )}
        <Button variant="ghost" disabled={busy} onClick={onDiscard}>
          Discard
        </Button>
        <span className="ml-auto text-text-2 text-xs text-right">Nothing written until you confirm</span>
      </div>
      {writeError !== null && (
        <div className="flex items-center gap-2 text-red-text text-[13px] mt-2">
          <span>{writeError}</span>
          <Button disabled={busy} onClick={onRetry}>
            Retry
          </Button>
        </div>
      )}
    </>
  );
}
