import { ReactNode, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Api } from "../api";
import { Destination } from "../types";
import { Button } from "./Button";
import { Mark } from "./Mark";

// Brand, Review/History/Quarantine nav with badges, the New lead button, the
// destination dropdown, and the account menu.
export function TopBar({
  api,
  route,
  queueCount,
  quarantineCount,
  emailIngest,
  newLeadVisible,
  onNewLead,
  onOpenQueue,
  accountMenu,
}: {
  api: Api;
  route: "review" | "history" | "quarantine" | "email";
  queueCount: number;
  quarantineCount: number;
  /** Email capture is configured on this server; hides the nav item when not. */
  emailIngest: boolean;
  newLeadVisible: boolean;
  onNewLead: () => void;
  onOpenQueue: () => void;
  accountMenu?: ReactNode;
}) {
  const navLink = (active: boolean) =>
    `no-underline px-3 py-1 rounded-ctl inline-flex items-center gap-1.5 ${
      active ? "text-text bg-bg" : "text-text-2 hover:text-text"
    }`;

  return (
    <header className="flex items-center gap-6 px-6 py-3 bg-surface border-b border-line">
      <div className="flex items-center gap-2">
        <Mark size={18} className="text-green" />
        <div className="font-semibold text-[15px] tracking-[-0.01em]">Ziga Data</div>
      </div>
      <nav className="flex gap-1">
        <a href="#/" className={navLink(route === "review")} onClick={onOpenQueue}>
          Review
          {queueCount > 0 && (
            <span className="bg-green text-white rounded-full text-[11px] font-semibold min-w-[18px] h-[18px] px-[5px] inline-flex items-center justify-center">
              {queueCount}
            </span>
          )}
        </a>
        <a href="#/history" className={navLink(route === "history")}>
          History
        </a>
        {emailIngest && (
          <a href="#/quarantine" className={navLink(route === "quarantine")}>
            Filtered
            {quarantineCount > 0 && (
              // Neutral, not green: filtered mail is worth a look, not a
              // call to action. The green badge is reserved for leads
              // actually waiting on the user.
              <span className="bg-line text-text-2 rounded-full text-[11px] font-semibold min-w-[18px] h-[18px] px-[5px] inline-flex items-center justify-center">
                {quarantineCount}
              </span>
            )}
          </a>
        )}
      </nav>
      {newLeadVisible && <Button onClick={onNewLead}>New lead</Button>}
      <div className="flex-1" />
      <DestinationDropdown api={api} />
      {accountMenu}
    </header>
  );
}

function DestinationDropdown({ api }: { api: Api }) {
  const nav = useNavigate();
  const [label, setLabel] = useState("Loading…");
  const [needsReconnect, setNeedsReconnect] = useState(false);
  const [destinations, setDestinations] = useState<Destination[]>([]);
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    let alive = true;
    api
      .destinations()
      .then(({ destinations }) => {
        if (!alive) return;
        const active = destinations.find((d) => d.active);
        setLabel(active ? active.label + (active.dry_run ? " — dry run" : "") : "No destination");
        setNeedsReconnect(!!active?.needs_reconnect);
        setDestinations(destinations);
      })
      .catch(() => {
        if (alive) setLabel("Destination unavailable");
      });
    return () => {
      alive = false;
    };
  }, [api]);

  // Switching destination means walking that provider's connect flow; the
  // switch only takes effect once it completes.
  function switchTo(dest: Destination) {
    setOpen(false);
    nav(dest.type === "notion" ? "/onboarding-notion" : "/onboarding");
  }

  useEffect(() => {
    function onDocClick(e: MouseEvent) {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false);
    }
    document.addEventListener("click", onDocClick);
    return () => document.removeEventListener("click", onDocClick);
  }, []);

  return (
    <div className="relative" ref={rootRef}>
      <button
        type="button"
        className="inline-flex items-center gap-2 text-text bg-surface border border-line rounded-ctl px-3 py-[6px] cursor-pointer hover:border-text-2"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
      >
        <span className="text-green-deep" aria-hidden="true">
          ▦
        </span>
        <span>{label}</span>
        {needsReconnect && (
          <span className="text-red-text text-xs" title="Reconnect needed">
            •
          </span>
        )}
        <span className="text-text-2 text-[10px]" aria-hidden="true">
          ▾
        </span>
      </button>
      {open && (
        <div
          className="absolute right-0 top-[calc(100%+4px)] min-w-[240px] bg-surface border border-line rounded-ctl shadow-popover p-1 z-20"
          role="menu"
        >
          {destinations.map((dest) => (
            <button
              key={dest.id}
              type="button"
              disabled={!!dest.disabled}
              onClick={() => (dest.active ? setOpen(false) : switchTo(dest))}
              className="flex items-center gap-2 w-full text-left text-text bg-transparent border-0 rounded-[6px] px-2.5 py-2 cursor-pointer enabled:hover:bg-bg disabled:text-text-2 disabled:cursor-default"
            >
              <span className="text-green-deep">{dest.type === "google_sheet" ? "▦" : "◆"}</span>
              <span>{dest.label}</span>
              {dest.active && dest.needs_reconnect && (
                <span className="ml-auto text-red-text text-xs">reconnect</span>
              )}
              {dest.active && !dest.needs_reconnect && (
                <span className="ml-auto text-text-2 text-xs">active</span>
              )}
              {!dest.active && !dest.unavailable && (
                <span className="ml-auto text-text-2 text-xs">switch</span>
              )}
              {dest.unavailable && <span className="ml-auto text-text-2 text-xs">unavailable</span>}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
