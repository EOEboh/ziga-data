import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Api, ApiError, notionStartURL } from "../../api";
import { mockConnectNotion } from "../../mock";
import {
  Me,
  NotionMapping,
  NotionMappingResponse,
  NotionResources,
} from "../../types";
import { AuthCard, FormError } from "../auth/AuthShell";
import { Button } from "../Button";

const isMock = () => new URLSearchParams(location.search).has("mock");

// Human labels for the Ziga schema fields shown in the mapping table.
const FIELD_LABELS: Record<string, string> = {
  date: "Date",
  name: "Name",
  contact: "Contact",
  source: "Source",
  need: "Need",
  notes: "Notes",
  flags: "Flags",
};

const fieldLabel = (f: string) => FIELD_LABELS[f] ?? f;

// DONT_WRITE is the sentinel for "leave this field out" in the property
// dropdown. Every other option's value is the property's index in the writable
// list rather than its name, because Notion property names may contain spaces
// and may differ only by case — packing a name and type into one string could
// not be split back apart reliably.
const DONT_WRITE = "none";

/**
 * NotionSetup is the destination flow for Notion: connect the workspace, then
 * either create a database Ziga owns or pick an existing one and review how
 * Ziga's fields map onto its properties.
 *
 * The mapping step exists because Notion is not a spreadsheet. A sheet has a
 * column for every field by construction; a Notion database has typed
 * properties that may be named differently, typed differently, or missing
 * entirely — so the automatic mapping is shown for review rather than applied
 * silently.
 */
export function NotionSetup({ api, me, reload }: { api: Api; me: Me; reload: () => void }) {
  const nav = useNavigate();
  const [resources, setResources] = useState<NotionResources | null>(null);
  const [chosen, setChosen] = useState<NotionMappingResponse | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // Errors from the OAuth callback come back as a query param, since that leg
  // is a top-level navigation rather than a fetch.
  const callbackError = new URLSearchParams(location.search).get("notion_error");

  const connected = !!me.notion_connected;

  useEffect(() => {
    if (!connected) return;
    let alive = true;
    api
      .notionResources()
      .then((r) => alive && setResources(r))
      .catch((e) => alive && setErr(e instanceof ApiError ? e.message : "Could not reach Notion"));
    return () => {
      alive = false;
    };
  }, [api, connected]);

  async function done() {
    reload();
    nav("/");
  }

  if (!connected) {
    return (
      <AuthCard
        title="Connect your Notion workspace"
        subtitle="Ziga Data writes each confirmed lead into a Notion database you choose."
      >
        <FormError message={callbackError ? connectErrorMessage(callbackError) : null} />
        {isMock() ? (
          <Button
            variant="primary"
            className="w-full"
            onClick={() => {
              // ?mock=1 has no backend to redirect to, so returning from
              // Notion's consent screen is simulated in place.
              mockConnectNotion(api);
              reload();
            }}
          >
            Connect Notion
          </Button>
        ) : (
          <a
            href={notionStartURL}
            className="block text-center rounded-ctl border border-green bg-green text-white font-semibold px-4 py-2 hover:bg-green-deep"
          >
            Connect Notion
          </a>
        )}
        <p className="text-sm text-text-2 mt-4">
          Notion asks you to pick exactly which pages and databases to share. Ziga only ever sees
          what you choose there — never your whole workspace.
        </p>
      </AuthCard>
    );
  }

  if (chosen) {
    return (
      <MappingReview
        api={api}
        mapping={chosen}
        onBack={() => setChosen(null)}
        onSaved={done}
      />
    );
  }

  async function createNew(parentPageID: string) {
    setErr(null);
    setBusy(true);
    try {
      await api.createNotionDatabase(parentPageID);
      await done();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "Could not create the database");
    } finally {
      setBusy(false);
    }
  }

  async function chooseExisting(databaseID: string) {
    setErr(null);
    setBusy(true);
    try {
      setChosen(await api.notionMapping(databaseID));
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "Could not read that database");
    } finally {
      setBusy(false);
    }
  }

  if (!resources) {
    return (
      <AuthCard title="Reading your Notion workspace" subtitle="One moment…">
        <FormError message={err} />
      </AuthCard>
    );
  }

  const parentPage = resources.pages[0];

  return (
    <AuthCard title="Choose where leads go" subtitle="Ziga writes each confirmed lead as a new row.">
      <FormError message={err} />

      <div className="mb-5">
        <p className="text-sm text-text mb-1 font-medium">Create a new database</p>
        <p className="text-sm text-text-2 mb-2">
          {resources.can_create
            ? `Ziga creates a "Ziga Leads" database with the right properties${
                parentPage ? ` inside ${parentPage.title}` : ""
              }. Recommended — nothing can be dropped.`
            : "You did not share a page for Ziga to create a database in. Reconnect Notion and tick a page, or pick an existing database below."}
        </p>
        <Button
          variant="primary"
          className="w-full"
          disabled={busy || !resources.can_create || !parentPage}
          onClick={() => parentPage && createNew(parentPage.id)}
        >
          {busy ? "Working…" : "Create a Ziga Leads database"}
        </Button>
      </div>

      <div className="border-t border-line pt-5">
        <p className="text-sm text-text mb-1 font-medium">Use an existing database</p>
        {resources.databases.length === 0 ? (
          <p className="text-sm text-text-2">
            You did not share any databases. Reconnect Notion to pick one.
          </p>
        ) : (
          <>
            <p className="text-sm text-text-2 mb-2">
              You will review how Ziga's fields map onto its properties before anything is saved.
            </p>
            <div className="flex flex-col gap-2">
              {resources.databases.map((db) => (
                <Button key={db.id} disabled={busy} onClick={() => chooseExisting(db.id)}>
                  {db.title}
                </Button>
              ))}
            </div>
          </>
        )}
      </div>
    </AuthCard>
  );
}

function connectErrorMessage(code: string): string {
  switch (code) {
    case "denied":
      return "Notion access was not granted. Try again and approve at least one page or database.";
    case "state":
      return "That connection link expired. Try again.";
    default:
      return "Could not connect Notion. Try again.";
  }
}

/**
 * MappingReview shows the proposed field→property mapping and lets the user
 * change any row before saving. Fields with no target are called out up front,
 * so it is clear what this database will not receive.
 */
function MappingReview({
  api,
  mapping,
  onBack,
  onSaved,
}: {
  api: Api;
  mapping: NotionMappingResponse;
  onBack: () => void;
  onSaved: () => void;
}) {
  const [choices, setChoices] = useState<NotionMapping>(mapping.mapping);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const writable = mapping.properties.filter((p) => p.writable);
  const unmapped = mapping.fields.filter((f) => !choices[f]);

  function choose(field: string, value: string) {
    setChoices((prev) => {
      const next = { ...prev };
      const prop = value === DONT_WRITE ? undefined : writable[Number(value)];
      if (!prop) {
        delete next[field];
        return next;
      }
      // The exact name is carried through untouched: Notion property names are
      // case-sensitive, so "Name" and "name" are different targets.
      next[field] = { name: prop.name, type: prop.type };
      return next;
    });
  }

  async function save() {
    setErr(null);
    setBusy(true);
    try {
      await api.setNotionDestination(mapping.database_id, choices);
      onSaved();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "Could not save that mapping");
    } finally {
      setBusy(false);
    }
  }

  return (
    <AuthCard
      title={`Map your fields to ${mapping.database_title}`}
      subtitle="Check where each field will be written. You can change any of these."
    >
      <FormError message={err} />

      <div className="flex flex-col gap-3 mb-4">
        {mapping.fields.map((field) => {
          const current = choices[field];
          const currentIdx = current
            ? writable.findIndex((p) => p.name === current.name && p.type === current.type)
            : -1;
          return (
            <label key={field} className="block">
              <span className="block text-sm text-text-2 mb-1">{fieldLabel(field)}</span>
              <select
                value={currentIdx >= 0 ? String(currentIdx) : DONT_WRITE}
                onChange={(e) => choose(field, e.target.value)}
                className="w-full rounded-ctl border border-line bg-bg px-3 py-2 text-text outline-none focus:border-text-2"
              >
                <option value={DONT_WRITE}>Don't write this field</option>
                {writable.map((p, i) => (
                  <option key={`${p.type}:${p.name}`} value={String(i)}>
                    {p.name} ({p.type})
                  </option>
                ))}
              </select>
            </label>
          );
        })}
      </div>

      {unmapped.length > 0 && (
        <p className="text-sm text-text-2 mb-4">
          Not written to this database: {unmapped.map(fieldLabel).join(", ")}. Those values stay in
          Ziga's history — they just won't appear in Notion.
        </p>
      )}

      <div className="flex gap-2">
        <Button variant="primary" className="flex-1" disabled={busy} onClick={save}>
          {busy ? "Saving…" : "Save destination"}
        </Button>
        <Button variant="ghost" disabled={busy} onClick={onBack}>
          Back
        </Button>
      </div>
    </AuthCard>
  );
}
