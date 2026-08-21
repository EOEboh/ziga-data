import { Button } from "./Button";

/**
 * "Here's what came in while you were away."
 *
 * Ingestion changes the emotional contract: before, a lead only existed
 * because the user had just pasted it. Now they arrive unattended, and the
 * review queue becomes somewhere to come back to rather than a screen you pass
 * through.
 *
 * Deliberately calm — neutral colours, a count, one action. This is not an
 * alert. Nothing has gone wrong, and nothing is urgent: the leads are safe in
 * the queue whether they look now or tomorrow.
 */
export function AwayBanner({ count, onReview, onDismiss }: { count: number; onReview: () => void; onDismiss: () => void }) {
  if (count <= 0) return null;
  return (
    <div className="bg-surface border border-line rounded-card p-4 flex items-center gap-3 flex-wrap">
      <div className="flex-1 min-w-[200px]">
        <div className="font-semibold">
          {count === 1 ? "1 lead came in while you were away" : `${count} leads came in while you were away`}
        </div>
        <div className="text-[13px] text-text-2 mt-0.5">Captured from your email. Nothing has been written yet.</div>
      </div>
      <Button onClick={onReview}>Review {count === 1 ? "it" : "them"}</Button>
      <Button variant="ghost" onClick={onDismiss}>
        Later
      </Button>
    </div>
  );
}
