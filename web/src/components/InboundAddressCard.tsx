import { useEffect, useState } from "react";
import type { Api } from "../api";
import type { InboundAddress } from "../types";
import { Button } from "./Button";

/**
 * The user's private capture address, with copy and rotate.
 *
 * The address renders in a read-only input rather than plain text on purpose:
 * navigator.clipboard needs a secure context and can fail, and an address the
 * user cannot select is an address they cannot use.
 */
export function InboundAddressCard({ api, compact = false }: { api: Api; compact?: boolean }) {
  const [state, setState] = useState<InboundAddress | null>(null);
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    let live = true;
    api
      .inbound()
      .then((r) => live && setState(r))
      .catch(() => live && setState(null));
    return () => {
      live = false;
    };
  }, [api]);

  async function enable() {
    setBusy(true);
    setError("");
    try {
      setState(await api.enableInbound());
    } catch (e: any) {
      // Provisioning talks to the mail provider, so it can genuinely fail.
      // Say so and let them retry rather than showing a dead address.
      setError(e?.message ?? "Could not set up your address. Try again");
    } finally {
      setBusy(false);
    }
  }

  async function rotate() {
    if (!confirm("Issue a new address? Your current one keeps working for two weeks so nothing in flight is lost.")) {
      return;
    }
    setBusy(true);
    setError("");
    try {
      setState(await api.rotateInbound());
    } catch (e: any) {
      setError(e?.message ?? "Could not issue a new address. Try again");
    } finally {
      setBusy(false);
    }
  }

  async function copy() {
    if (!state?.address) return;
    try {
      await navigator.clipboard.writeText(state.address);
      setCopied(true);
      setTimeout(() => setCopied(false), 1600);
    } catch {
      // Clipboard access needs a secure context and can be denied. The input
      // below is selectable, so this is a degraded path, not a dead end.
      setError("Couldn't copy — select the address and copy it manually");
    }
  }

  if (!state) return null;

  if (!state.enabled) {
    return (
      <div className={compact ? "" : "bg-surface border border-line rounded-card p-4"}>
        <div className="font-semibold mb-1">Capture leads from email</div>
        <p className="text-[13px] text-text-2 mb-3">
          Get a private address at <span className="font-mono">{state.domain}</span>. Forward a lead to it and it lands
          in your review queue — you still confirm every one.
        </p>
        <Button variant="primary" onClick={enable} disabled={busy}>
          {busy ? "Setting up…" : "Turn on email capture"}
        </Button>
        {error && <div className="mt-2 text-[13px] text-red-text">{error}</div>}
      </div>
    );
  }

  return (
    <div className={compact ? "" : "bg-surface border border-line rounded-card p-4"}>
      <div className="font-semibold mb-1">Your capture address</div>
      <p className="text-[13px] text-text-2 mb-2">
        Forward a lead here and it appears in your review queue. Nothing is written until you confirm it.
      </p>
      <div className="flex gap-2 items-center flex-wrap">
        <input
          readOnly
          value={state.address ?? ""}
          onFocus={(e) => e.currentTarget.select()}
          aria-label="Your capture address"
          className="flex-1 min-w-[240px] bg-bg border border-line rounded-ctl px-3 py-2 font-mono text-[13px]"
        />
        <Button onClick={copy}>{copied ? "Copied" : "Copy"}</Button>
      </div>
      <div className="mt-2 flex items-center gap-3">
        <button
          type="button"
          onClick={rotate}
          disabled={busy}
          className="text-[13px] text-text-2 hover:text-text underline cursor-pointer disabled:opacity-50"
        >
          Issue a new address
        </button>
        <span className="text-[12px] text-text-2">
          Anyone who knows this address can send leads to your queue — keep it private.
        </span>
      </div>
      {error && <div className="mt-2 text-[13px] text-red-text">{error}</div>}
    </div>
  );
}
