// Sheet preview strip: the last rows of the connected sheet plus, while a
// review is open, the pending row. On confirm the pending highlight settles
// into a normal row via a ~400ms background fade — this transition is the
// moment the product feels alive; don't remove it.

import { useEffect, useState } from "react";
import { sentenceCase } from "../format";
import { PreviewResponse } from "../types";

export interface PendingRow {
  values: Record<string, string>; // live field values, keyed by field name
  kind: "pending" | "error";
}

const TD =
  "px-2.5 py-1.5 border-b border-line whitespace-nowrap max-w-[220px] overflow-hidden text-ellipsis " +
  "[transition:background-color_400ms_ease] group-last:border-b-0";

export function PreviewStrip({
  preview,
  pending,
  settleToken,
}: {
  preview: PreviewResponse;
  pending: PendingRow | null;
  settleToken: number;
}) {
  // The settle flash: each settleToken increment renders the (fresh) last row
  // tinted, then releases the tint after a double rAF so the tinted frame is
  // guaranteed to paint before the 400ms background fade starts.
  const [clearedToken, setClearedToken] = useState(settleToken);
  useEffect(() => {
    if (settleToken === clearedToken) return;
    let raf2 = 0;
    const raf1 = requestAnimationFrame(() => {
      raf2 = requestAnimationFrame(() => setClearedToken(settleToken));
    });
    return () => {
      cancelAnimationFrame(raf1);
      cancelAnimationFrame(raf2);
    };
  }, [settleToken, clearedToken]);
  const flashLast = settleToken !== clearedToken;

  return (
    <div className="mt-6">
      <div className="bg-surface border border-line rounded-card py-2 px-1.5">
        {preview.error ? (
          <div className="text-text-2 text-xs px-2.5 py-3">Sheet preview unavailable</div>
        ) : (
          <>
            <div className="overflow-x-auto">
              <table className="w-full border-collapse font-mono text-xs">
                <thead>
                  <tr>
                    {preview.columns.map((col) => (
                      <th
                        key={col}
                        className="text-left font-sans font-semibold text-xs text-text-2 px-2.5 py-1.5 border-b border-line whitespace-nowrap"
                      >
                        {sentenceCase(col)}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {preview.rows.map((row, ri) => {
                    const last = ri === preview.rows.length - 1 && !pending;
                    // The settled row is keyed to the token so it mounts as a
                    // new node with the tint already applied (no fade-in);
                    // removing the tint then fades on that same node.
                    return (
                      <tr key={last ? `r${ri}-t${settleToken}` : `r${ri}`} className="group">
                        {preview.columns.map((_, ci) => (
                          <td key={ci} className={`${TD}${last && flashLast ? " bg-green-tint" : ""}`}>
                            {row[ci] ?? ""}
                          </td>
                        ))}
                      </tr>
                    );
                  })}
                  {pending && (
                    <tr className="group">
                      {preview.columns.map((col, ci) => (
                        <td
                          key={ci}
                          className={`${TD} ${pending.kind === "error" ? "bg-red-tint" : "bg-green-tint"}`}
                        >
                          {col === "date" ? "pending" : col === "flags" ? "" : pending.values[col] ?? ""}
                        </td>
                      ))}
                    </tr>
                  )}
                </tbody>
              </table>
            </div>
            {preview.rows.length === 0 && !pending && (
              <div className="text-text-2 text-xs px-2.5 py-3">No rows in the sheet yet</div>
            )}
          </>
        )}
      </div>
    </div>
  );
}
