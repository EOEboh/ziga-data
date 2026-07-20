import { KeyboardEvent, useEffect, useRef } from "react";
import { Button } from "./Button";

// The paste box: textarea plus image upload. The selected File lives in app
// state (not the input element), so edit-and-re-run can re-attach the
// original image; the input is only a picker.
export function ComposeBox({
  text,
  file,
  submitError,
  onTextChange,
  onFileChange,
  onSubmit,
}: {
  text: string;
  file: File | null;
  submitError: string | null;
  onTextChange: (text: string) => void;
  onFileChange: (file: File | null) => void;
  onSubmit: () => void;
}) {
  const fileInputRef = useRef<HTMLInputElement>(null);

  // When the file is cleared in app state, reset the picker so re-choosing
  // the same file fires change again.
  useEffect(() => {
    if (file === null && fileInputRef.current) fileInputRef.current.value = "";
  }, [file]);

  // Cmd/Ctrl+Enter in the textarea submits.
  function onKeyDown(e: KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) onSubmit();
  }

  return (
    <div className="bg-surface border border-line rounded-card p-4 max-w-[560px] my-12 mx-auto text-center">
      <p className="text-text-2 m-0 mb-4">Paste a lead, forward an email, or drop a screenshot.</p>
      {submitError !== null && <p className="text-red-text text-[13px] m-0 mb-4">{submitError}</p>}
      <textarea
        className="w-full min-h-[140px] resize-y font-mono text-[13px] text-text bg-surface border border-line rounded-ctl p-3 focus:outline-none focus:border-green"
        spellCheck={false}
        value={text}
        onChange={(e) => onTextChange(e.target.value)}
        onKeyDown={onKeyDown}
      />
      <div className="flex items-center justify-center gap-2 mt-4">
        <Button onClick={() => fileInputRef.current?.click()}>Upload image</Button>
        <input
          ref={fileInputRef}
          type="file"
          accept="image/png,image/jpeg,image/webp,image/gif"
          hidden
          onChange={(e) => onFileChange(e.target.files?.[0] ?? null)}
        />
        <span className="text-text-2 text-xs">{file?.name ?? ""}</span>
        <Button variant="primary" onClick={onSubmit}>
          Extract fields
        </Button>
      </div>
    </div>
  );
}
