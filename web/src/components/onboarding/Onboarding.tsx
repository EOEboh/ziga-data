import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { Api, ApiError, googleStartURL } from "../../api";
import { openSheetPicker } from "../../picker";
import { Me } from "../../types";
import { AuthCard, FormError } from "../auth/AuthShell";
import { Button } from "../Button";

const isMock = () => new URLSearchParams(location.search).has("mock");

// Onboarding gates the app until the user has connected a lead destination.
// Step 1 is the choice of destination — Google Sheets or Notion — since the
// two need different connections. Google Sheets then continues here (connect
// Google, then create or pick a sheet); Notion hands off to /onboarding-notion.
export function Onboarding({ api, me, reload }: { api: Api; me: Me; reload: () => void }) {
  const nav = useNavigate();
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // Which destination the user picked. Null until they choose; a user who has
  // already connected Google is past the choice for the Sheets path.
  const [kind, setKind] = useState<"google_sheet" | "notion" | null>(null);

  const notionOffered = me.config.notion_oauth;

  // The choice is always the first step, whether this is first-run onboarding
  // or a deliberate switch from the account menu.
  if (kind === null) {
    return (
      <AuthCard title="Where should your leads go?" subtitle="Ziga writes each confirmed lead to one destination.">
        <div className="flex flex-col gap-3">
          <Button variant="primary" onClick={() => setKind("google_sheet")}>
            Google Sheets
          </Button>
          <Button
            disabled={!notionOffered}
            onClick={() => nav("/onboarding-notion")}
            title={notionOffered ? undefined : "Notion is not configured on this server"}
          >
            Notion
          </Button>
        </div>
        <p className="text-sm text-text-2 mt-4">
          Only one destination is active at a time. Switching replaces the current one; nothing
          already written is affected.
        </p>
      </AuthCard>
    );
  }

  if (!me.google_connected) {
    return (
      <AuthCard title="Connect your Google account" subtitle="Ziga Data writes each confirmed lead to your own Google Sheet.">
        <a
          href={googleStartURL}
          className="block text-center rounded-ctl border border-green bg-green text-white font-semibold px-4 py-2 hover:bg-green-deep"
        >
          Connect Google
        </a>
        <p className="text-sm text-text-2 mt-4">
          Ziga Data only requests access to sheets you create or choose here — never your whole Drive.
        </p>
        <div className="mt-4 pt-4 border-t border-line">
          <Button variant="ghost" className="w-full" onClick={() => setKind(null)}>
            Choose a different destination
          </Button>
        </div>
      </AuthCard>
    );
  }

  async function done() {
    reload();
    nav("/");
  }

  async function createNew() {
    setErr(null);
    setBusy(true);
    try {
      await api.createSheet();
      await done();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "Could not create your sheet");
    } finally {
      setBusy(false);
    }
  }

  async function chooseExisting() {
    setErr(null);
    try {
      let id: string | null;
      if (isMock()) {
        id = "mock-existing-sheet";
      } else {
        id = await openSheetPicker(me.config.google_client_id, me.config.google_picker_api_key, me.config.google_project_number);
      }
      if (!id) return; // cancelled
      setBusy(true);
      await api.attachSheet(id);
      await done();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "Could not attach that sheet");
    } finally {
      setBusy(false);
    }
  }

  return (
    <AuthCard title="Choose your leads sheet" subtitle="Where should confirmed leads be written?">
      <FormError message={err} />
      <div className="flex flex-col gap-3">
        <Button variant="primary" onClick={createNew} disabled={busy}>
          {busy ? "Working…" : "Create a new sheet"}
        </Button>
        <Button variant="default" onClick={chooseExisting} disabled={busy}>
          Choose an existing sheet
        </Button>
      </div>
      {notionOffered && (
        <div className="mt-4 pt-4 border-t border-line">
          <Button variant="ghost" className="w-full" disabled={busy} onClick={() => nav("/onboarding-notion")}>
            Use Notion instead
          </Button>
        </div>
      )}
    </AuthCard>
  );
}
