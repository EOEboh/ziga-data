import { Phase } from "../state";
import { FIELD_ORDER, FieldState, Submission } from "../types";
import { ActionRow } from "./ActionRow";
import { FieldEditor } from "./FieldEditor";
import { MultiLeadBanner } from "./MultiLeadBanner";
import { OriginalInput } from "./OriginalInput";
import { SkeletonRow } from "./Skeleton";

// Two-column review split: original input (left), extracted fields with
// per-field confidence states and the action row (right). Collapses to a
// single column below 760px.
export function ReviewPane({
  phase,
  submission,
  extractingText,
  extractStartedAt,
  localImageUrl,
  fields,
  fieldStates,
  edited,
  writeError,
  busy,
  onFieldChange,
  onConfirm,
  onDiscard,
  onRetry,
  onEditRerun,
}: {
  phase: Phase;
  submission: Submission | null;
  extractingText: string;
  extractStartedAt: string;
  localImageUrl: string | null;
  fields: Record<string, string>;
  fieldStates: Record<string, FieldState>;
  edited: Record<string, boolean>;
  writeError: string | null;
  busy: boolean;
  onFieldChange: (name: string, value: string) => void;
  onConfirm: () => void;
  onDiscard: () => void;
  onRetry: () => void;
  onEditRerun: () => void;
}) {
  const extracting = phase === "extracting";
  const sub = submission;

  return (
    <div className="grid grid-cols-2 max-[760px]:grid-cols-1 gap-6 items-start">
      {extracting || !sub ? (
        <OriginalInput
          text={extractingText}
          imageUrl={localImageUrl}
          createdAt={extractStartedAt}
          detectedSource={null}
          showEditRerun={false}
          onEditRerun={onEditRerun}
        />
      ) : (
        <OriginalInput
          text={sub.input.text ?? ""}
          imageUrl={localImageUrl ?? sub.input.image_url ?? null}
          createdAt={sub.created_at}
          detectedSource={sub.result ? sub.result.source : null}
          showEditRerun
          onEditRerun={onEditRerun}
        />
      )}

      <div className="bg-surface border border-line rounded-card p-4">
        {!extracting && sub?.result?.multiple_leads_detected && <MultiLeadBanner />}
        <div className="flex items-baseline gap-2 mb-2 font-semibold">
          Extracted fields{" "}
          <span className="font-normal text-text-2 text-[13px]">Edit before confirming</span>
        </div>
        {extracting ? (
          <div>
            {FIELD_ORDER.map((name) => (
              <SkeletonRow key={name} />
            ))}
          </div>
        ) : (
          <>
            <div>
              {FIELD_ORDER.map((name) => (
                <FieldEditor
                  key={name}
                  name={name}
                  value={fields[name] ?? ""}
                  state={fieldStates[name] ?? "ok"}
                  edited={!!edited[name]}
                  onChange={onFieldChange}
                />
              ))}
            </div>
            <ActionRow
              writeError={writeError}
              busy={busy}
              onConfirm={onConfirm}
              onDiscard={onDiscard}
              onRetry={onRetry}
            />
          </>
        )}
      </div>
    </div>
  );
}
