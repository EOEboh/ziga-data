import { sentenceCase } from "../format";
import { FieldState } from "../types";

// One editable input per schema field with its confidence state. Flagged
// fields are ordinary inputs — fixing one is a click into it, never a
// separate flow. Editing a flagged field clears its caution styling — the
// user has addressed it.
export function FieldEditor({
  name,
  value,
  state,
  edited,
  onChange,
}: {
  name: string;
  value: string;
  state: FieldState;
  edited: boolean;
  onChange: (name: string, value: string) => void;
}) {
  const showAmber = state === "low_confidence" && !edited;
  const showRed = state === "missing" && !edited;
  const inputCls = [
    "w-full text-text rounded-ctl px-2.5 py-2 border",
    "focus:outline-none focus:border-green placeholder:text-text-2",
    showAmber ? "bg-amber-tint border-amber" : showRed ? "bg-surface border-red" : "bg-surface border-line",
  ].join(" ");

  return (
    <div className="mb-4 last:mb-0">
      <label className="flex items-center gap-2 mb-1 text-[13px] text-text-2">
        <span>{sentenceCase(name)}</span>
        {showAmber && (
          <span className="text-[11px] font-semibold rounded-full px-2 py-px text-amber-text bg-amber-tint">
            low confidence
          </span>
        )}
        {showRed && (
          <span className="text-[11px] font-semibold rounded-full px-2 py-px text-red-text bg-red-tint">
            not found
          </span>
        )}
      </label>
      <input
        className={inputCls}
        type="text"
        spellCheck={false}
        name={name}
        value={value}
        placeholder={state === "missing" ? "Not stated. Add if known" : ""}
        onChange={(e) => onChange(name, e.target.value)}
      />
    </div>
  );
}
