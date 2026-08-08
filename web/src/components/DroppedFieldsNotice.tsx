import { FIELD_ORDER } from "../types";

const LABELS: Record<string, string> = {
  date: "Date",
  name: "Name",
  contact: "Contact",
  source: "Source",
  need: "Need",
  notes: "Notes",
  flags: "Flags",
};

const label = (f: string) => LABELS[f] ?? f;

/**
 * DroppedFieldsNotice reports fields the destination had no home for.
 *
 * The lead itself was written — a Notion database simply may not have a
 * property for every Ziga field, or a value may not fit the property's type
 * (a phone number in an email property). Saying so is the point: a value that
 * quietly failed to travel would be worse than a visible write failure.
 */
export function DroppedFieldsNotice({
  fields,
  onDismiss,
}: {
  fields: string[];
  onDismiss: () => void;
}) {
  // Render in schema order rather than whatever order the server reported.
  const ordered = FIELD_ORDER.filter((f) => fields.includes(f)) as string[];
  const rest = fields.filter((f) => !ordered.includes(f));
  const names = [...ordered, ...rest].map(label).join(", ");

  return (
    <div className="flex items-start gap-3 bg-surface border border-line rounded-card px-4 py-3 mt-4">
      <div className="flex-1 text-sm text-text-2">
        <span className="text-text">Written, but {names} did not fit your destination.</span>{" "}
        Those values are still in Ziga's history. To capture them, add a matching property to your
        database and change the destination mapping.
      </div>
      <button
        type="button"
        onClick={onDismiss}
        aria-label="Dismiss"
        className="text-text-2 bg-transparent border-0 cursor-pointer hover:text-text"
      >
        ✕
      </button>
    </div>
  );
}
