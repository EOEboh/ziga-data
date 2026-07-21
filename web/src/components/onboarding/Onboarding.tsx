import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { Api, ApiError, googleStartURL } from "../../api";
import { openSheetPicker } from "../../picker";
import { Me } from "../../types";
import { AuthCard, FormError } from "../auth/AuthShell";
import { Button } from "../Button";

const isMock = () => new URLSearchParams(location.search).has("mock");

// Onboarding gates the app until the user has connected Google and chosen a
// destination sheet. Step 1: connect Google. Step 2: create a new sheet or
// attach an existing one via the Picker.
export function Onboarding({ api, me, reload }: { api: Api; me: Me; reload: () => void }) {
  const nav = useNavigate();
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (!me.google_connected) {
    return (
      <AuthCard title="Connect your Google account" subtitle="Ziga writes each confirmed lead to your own Google Sheet.">
        <a
          href={googleStartURL}
          className="block text-center rounded-ctl border border-green bg-green text-white font-semibold px-4 py-2 hover:bg-green-deep"
        >
          Connect Google
        </a>
        <p className="text-sm text-text-2 mt-4">
          Ziga only requests access to sheets you create or choose here — never your whole Drive.
        </p>
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
        id = await openSheetPicker(me.config.google_client_id, me.config.google_picker_api_key);
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
    </AuthCard>
  );
}
