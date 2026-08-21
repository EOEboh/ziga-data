import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Api } from "../api";
import { Destination, Me } from "../types";

// AccountMenu shows the signed-in email, the connected lead destination (with
// a reconnect prompt when it has lost access), and the provider connections
// that can be dropped.
export function AccountMenu({ api, me, reload }: { api: Api; me: Me; reload: () => void }) {
  const nav = useNavigate();
  const [open, setOpen] = useState(false);
  const [destination, setDestination] = useState<Destination | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);
  const email = me.user?.email ?? "";

  useEffect(() => {
    function onDocClick(e: MouseEvent) {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false);
    }
    document.addEventListener("click", onDocClick);
    return () => document.removeEventListener("click", onDocClick);
  }, []);

  // The destination is loaded when the menu opens, so it reflects the current
  // state rather than whatever it was at page load.
  useEffect(() => {
    if (!open) return;
    let alive = true;
    api
      .destinations()
      .then(({ destinations }) => alive && setDestination(destinations.find((d) => d.active) ?? null))
      .catch(() => alive && setDestination(null));
    return () => {
      alive = false;
    };
  }, [api, open]);

  async function logout() {
    await api.logout().catch(() => {});
    reload();
  }
  async function disconnectGoogle() {
    await api.disconnectGoogle().catch(() => {});
    reload();
  }
  async function disconnectNotion() {
    await api.disconnectNotion().catch(() => {});
    reload();
  }

  const initial = email ? email[0]!.toUpperCase() : "?";
  const item =
    "block w-full text-left text-text bg-transparent border-0 rounded-[6px] px-2.5 py-2 cursor-pointer hover:bg-bg";

  // Reconnecting means re-running the destination's own connect flow.
  const reconnectPath = destination?.type === "notion" ? "/onboarding-notion" : "/onboarding";

  return (
    <div className="relative" ref={rootRef}>
      <button
        type="button"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
        className="w-8 h-8 rounded-full bg-bg border border-line text-text-2 font-semibold cursor-pointer hover:border-text-2"
      >
        {initial}
      </button>
      {open && (
        <div
          className="absolute right-0 top-[calc(100%+4px)] min-w-[240px] bg-surface border border-line rounded-ctl shadow-popover p-1 z-20"
          role="menu"
        >
          <div className="px-2.5 py-2 text-text-2 text-sm truncate border-b border-line mb-1">{email}</div>

          {destination && (
            <div className="px-2.5 py-2 border-b border-line mb-1">
              <div className="text-[11px] uppercase tracking-wide text-text-2 mb-0.5">Destination</div>
              <div className="text-sm text-text truncate">{destination.label}</div>
              {destination.needs_reconnect && (
                <button type="button" onClick={() => nav(reconnectPath)} className="text-sm text-red-text cursor-pointer bg-transparent border-0 p-0 mt-1">
                  Lost access — reconnect
                </button>
              )}
            </div>
          )}

          <button type="button" onClick={() => nav("/onboarding")} className={item}>
            Change destination
          </button>
          {me.config.email_ingest && (
            // The compose box carries the discovery hint; this is where a user
            // comes back to it once they know it exists.
            <button
              type="button"
              onClick={() => {
                setOpen(false);
                location.hash = "#/email";
              }}
              className={item}
            >
              Email capture
            </button>
          )}
          {me.google_connected && (
            <button type="button" onClick={disconnectGoogle} className={item}>
              Disconnect Google
            </button>
          )}
          {/* Also offered when the link is broken — which is exactly when a
              user may want to drop it — so this checks the active destination
              too, not only a healthy link. */}
          {(me.notion_connected || destination?.type === "notion") && (
            <button type="button" onClick={disconnectNotion} className={item}>
              Disconnect Notion
            </button>
          )}
          <button type="button" onClick={logout} className={item}>
            Log out
          </button>
        </div>
      )}
    </div>
  );
}
