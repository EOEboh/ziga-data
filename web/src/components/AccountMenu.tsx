import { useEffect, useRef, useState } from "react";
import { Api } from "../api";
import { Me } from "../types";

// AccountMenu shows the signed-in email with log out and (when linked) a
// disconnect-Google action.
export function AccountMenu({ api, me, reload }: { api: Api; me: Me; reload: () => void }) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  const email = me.user?.email ?? "";

  useEffect(() => {
    function onDocClick(e: MouseEvent) {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false);
    }
    document.addEventListener("click", onDocClick);
    return () => document.removeEventListener("click", onDocClick);
  }, []);

  async function logout() {
    await api.logout().catch(() => {});
    reload();
  }
  async function disconnect() {
    await api.disconnectGoogle().catch(() => {});
    reload();
  }

  const initial = email ? email[0]!.toUpperCase() : "?";

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
          className="absolute right-0 top-[calc(100%+4px)] min-w-[220px] bg-surface border border-line rounded-ctl shadow-popover p-1 z-20"
          role="menu"
        >
          <div className="px-2.5 py-2 text-text-2 text-sm truncate border-b border-line mb-1">{email}</div>
          {me.google_connected && (
            <button
              type="button"
              onClick={disconnect}
              className="block w-full text-left text-text bg-transparent border-0 rounded-[6px] px-2.5 py-2 cursor-pointer hover:bg-bg"
            >
              Disconnect Google
            </button>
          )}
          <button
            type="button"
            onClick={logout}
            className="block w-full text-left text-text bg-transparent border-0 rounded-[6px] px-2.5 py-2 cursor-pointer hover:bg-bg"
          >
            Log out
          </button>
        </div>
      )}
    </div>
  );
}
