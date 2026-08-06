// MockApi (?mock=1) serves fixtures covering every review state, for demos
// and frontend work with no backend running. Confirm fails when any field
// value contains "fail", exercising the write-failed → retry path.

import { Api, ApiError } from "./api";
import {
  ConfirmResponse,
  Destination,
  DestinationResponse,
  HistoryResponse,
  Me,
  NotionConnection,
  NotionMapping,
  NotionMappingResponse,
  NotionResources,
  PreviewResponse,
  QueueResponse,
  SheetConnection,
  Submission,
} from "./types";

const delay = (ms: number) => new Promise((r) => setTimeout(r, ms));

const MOCK_NOTION = new URLSearchParams(location.search).get("notion") ?? "";

const COLUMNS = ["date", "name", "contact", "source", "need", "notes", "flags"];

type Fixture = Pick<Submission, "result" | "field_states" | "flags">;

const fixtures: Fixture[] = [
  {
    result: {
      name: "Ada Okafor",
      contact: "ada@lumen.studio",
      source: "X direct message",
      need: "Wants a landing page for a product launch",
      date: "2026-07-15",
      notes: "Launch is mid-August, budget around $1,200",
      confidence: "high",
      missing_fields: [],
      multiple_leads_detected: false,
    },
    field_states: { name: "ok", contact: "ok", source: "ok", date: "ok", need: "ok", notes: "ok" },
  },
  {
    result: {
      name: "M. Diallo",
      contact: "+221 77 5.. (partly cut off)",
      source: "WhatsApp screenshot",
      need: "Monthly bookkeeping for a small shop",
      date: "2026-07-16",
      notes: "",
      confidence: "medium",
      field_confidence: { name: "medium", contact: "low", source: "high", date: "high", need: "high", notes: "high" },
      missing_fields: [],
      multiple_leads_detected: false,
    },
    field_states: { name: "ok", contact: "low_confidence", source: "ok", date: "ok", need: "ok", notes: "ok" },
  },
  {
    result: {
      name: "Kofi Mensah",
      contact: null,
      source: "forwarded email",
      need: "Redesign of a Shopify store",
      date: "2026-07-17",
      notes: "Second person (Ama) mentioned in the same thread",
      confidence: "medium",
      missing_fields: ["contact"],
      multiple_leads_detected: true,
    },
    field_states: { name: "ok", contact: "missing", source: "ok", date: "ok", need: "ok", notes: "ok" },
    flags: ["multiple leads detected — only the primary lead was extracted"],
  },
];

/**
 * mockConnectNotion simulates a completed Notion consent when running against
 * fixtures, and reports whether it applied — the real client is untouched.
 */
export function mockConnectNotion(api: Api): boolean {
  if (api instanceof MockApi) {
    api.connectNotionForMock();
    return true;
  }
  return false;
}

export class MockApi implements Api {
  private nextId = 100;
  private fixtureIdx = 0;
  private pending = new Map<number, Submission>();
  private rows: string[][] = [
    ["2026-07-14", "Lena Fischer", "lena@fischer.dev", "referral", "API integration help", "", ""],
    ["2026-07-15", "Sam Torres", "@samtorres", "LinkedIn", "Brand identity refresh", "urgent", ""],
    ["2026-07-16", "Priya Nair", "priya@nair.co", "cold email", "Quarterly tax filing", "", ""],
  ];

  async submit(form: FormData): Promise<Submission> {
    await delay(900);
    const fixture = fixtures[this.fixtureIdx++ % fixtures.length];
    const sub: Submission = {
      id: this.nextId++,
      status: "pending",
      result: JSON.parse(JSON.stringify(fixture.result)),
      field_states: { ...fixture.field_states },
      flags: fixture.flags ? [...fixture.flags] : undefined,
      input: {
        text: String(form.get("text") ?? ""),
        has_image: form.get("image") instanceof File,
      },
      created_at: new Date().toISOString(),
    };
    this.pending.set(sub.id, sub);
    return sub;
  }

  async confirm(id: number, fields: Record<string, string>): Promise<ConfirmResponse> {
    await delay(500);
    const sub = this.pending.get(id);
    if (!sub) throw new ApiError("submission not found", 404);
    if (Object.values(fields).some((v) => v.includes("fail"))) {
      throw new ApiError("Could not write to your sheet. Retry", 502);
    }
    if (this.destinationType === "notion" && this.notionBroken) {
      throw new ApiError(
        "Ziga has lost access to your Notion database. Reconnect Notion to write this lead",
        409,
      );
    }
    const row = COLUMNS.map((col) => (col === "flags" ? (sub.flags ?? []).join("; ") : fields[col] ?? ""));
    this.rows.push(row);
    this.pending.delete(id);

    const res: ConfirmResponse = { id, status: "written" };
    if (this.destinationType === "notion") {
      res.url = "https://www.notion.so/mock-page";
      // Fields the chosen database has no home for come back named, so the
      // review pane can say so rather than losing them quietly.
      const dropped = COLUMNS.filter((col) => !this.notionMappingSaved[col] && (fields[col] ?? "") !== "");
      if (dropped.length) res.dropped_fields = dropped;
    }
    return res;
  }

  async discard(id: number): Promise<void> {
    await delay(200);
    this.pending.delete(id);
  }

  async queue(): Promise<QueueResponse> {
    const items = [...this.pending.values()].reverse();
    return { count: items.length, items };
  }

  async preview(): Promise<PreviewResponse> {
    await delay(200);
    return { columns: COLUMNS, rows: this.rows.slice(-3) };
  }

  async destinations(): Promise<DestinationResponse> {
    const active: Destination =
      this.destinationType === "notion"
        ? {
            id: "notion",
            type: "notion",
            label: `${this.notionDatabaseTitle} (Notion)`,
            active: true,
            connected: !this.notionBroken,
            needs_reconnect: this.notionBroken,
            database_id: this.notionDatabaseId,
            database_title: this.notionDatabaseTitle,
          }
        : {
            id: "google_sheet",
            type: "google_sheet",
            label: (this.sheetConnected ? "Ziga Leads" : "No destination connected") + " (Google Sheet)",
            active: true,
            connected: this.sheetConnected,
            needs_setup: !this.sheetConnected,
          };

    const other: Destination =
      this.destinationType === "notion"
        ? { id: "google_sheet", type: "google_sheet", label: "Google Sheets" }
        : { id: "notion", type: "notion", label: "Notion" };

    return { destinations: [active, other] };
  }

  // --- Auth / onboarding (mock: a pre-connected logged-in user so ?mock=1
  // walks the whole authed experience without a backend) ---

  private authed = true;
  private googleConnected = true;
  private sheetConnected = MOCK_NOTION === "";

  async me(): Promise<Me> {
    return {
      authenticated: this.authed,
      user: this.authed ? { id: 1, email: "you@example.com", email_verified: true } : null,
      google_connected: this.googleConnected,
      notion_connected: this.notionConnected && !this.notionBroken,
      destination_connected:
        this.destinationType === "notion" ? this.notionConnected && !this.notionBroken : this.sheetConnected,
      destination_configured:
        this.destinationType === "notion" ? this.notionConnected : this.sheetConnected,
      config: {
        google_oauth: true,
        google_client_id: "mock-client",
        google_picker_api_key: "mock-key",
        google_project_number: "575697153359",
        notion_oauth: true,
      },
    };
  }
  async signup(): Promise<void> {
    await delay(300);
  }
  async login(): Promise<void> {
    await delay(300);
    this.authed = true;
  }
  async logout(): Promise<void> {
    this.authed = false;
  }
  async forgotPassword(): Promise<void> {
    await delay(300);
  }
  async resetPassword(): Promise<void> {
    await delay(300);
  }
  async disconnectGoogle(): Promise<void> {
    this.googleConnected = false;
    // Only a Google-backed destination loses access with the Google grant.
    if (this.destinationType === "google_sheet") this.sheetConnected = false;
  }
  async createSheet(): Promise<SheetConnection> {
    await delay(400);
    this.googleConnected = true;
    this.sheetConnected = true;
    this.destinationType = "google_sheet";
    return { spreadsheet_id: "mock-sheet", sheet_tab: "Leads", created_by_app: true };
  }
  async attachSheet(spreadsheetId: string): Promise<SheetConnection> {
    await delay(400);
    this.googleConnected = true;
    this.sheetConnected = true;
    this.destinationType = "google_sheet";
    return { spreadsheet_id: spreadsheetId, sheet_tab: "Leads", created_by_app: false };
  }

  // --- Notion (mock: a workspace with one granted page and two granted
  // databases — one a clean match, one deliberately missing a "notes" home so
  // the dropped-fields notice is walkable) ---

  // ?mock=1&notion=connected starts with a Notion workspace already linked;
  // &notion=broken additionally simulates access having been revoked, so the
  // reconnect prompt is walkable without a backend.
  private notionConnected = MOCK_NOTION !== "";
  private notionBroken = MOCK_NOTION === "broken";
  private destinationType: "google_sheet" | "notion" = MOCK_NOTION ? "notion" : "google_sheet";
  private notionDatabaseId = MOCK_NOTION ? "db-crm" : "";
  private notionDatabaseTitle = MOCK_NOTION ? "Client CRM" : "";
  // The pre-seeded destination deliberately maps a database with no home for
  // "notes", so a confirmed lead demonstrates the dropped-fields notice.
  private notionMappingSaved: NotionMapping = MOCK_NOTION
    ? { ...MockApi.NOTION_MAPPINGS["db-crm"]! }
    : {};

  private static readonly NOTION_SCHEMAS: Record<string, NotionMappingResponse["properties"]> = {
    "db-clean": [
      { name: "Name", type: "title", writable: true },
      { name: "Contact", type: "email", writable: true },
      { name: "Date", type: "date", writable: true },
      { name: "Flags", type: "rich_text", writable: true },
      { name: "Need", type: "rich_text", writable: true },
      { name: "Notes", type: "rich_text", writable: true },
      { name: "Source", type: "select", writable: true },
    ],
    // A real-world database that was not built for Ziga: differently named,
    // no home for notes, and a status property Ziga cannot write to.
    "db-crm": [
      { name: "Person", type: "title", writable: true },
      { name: "Channel", type: "select", writable: true },
      { name: "Email address", type: "email", writable: true },
      { name: "Reached out", type: "date", writable: true },
      { name: "Stage", type: "status", writable: false },
      { name: "Summary", type: "rich_text", writable: true },
    ],
  };

  private static readonly NOTION_MAPPINGS: Record<string, NotionMapping> = {
    "db-clean": {
      date: { name: "Date", type: "date" },
      name: { name: "Name", type: "title" },
      contact: { name: "Contact", type: "email" },
      source: { name: "Source", type: "select" },
      need: { name: "Need", type: "rich_text" },
      notes: { name: "Notes", type: "rich_text" },
      flags: { name: "Flags", type: "rich_text" },
    },
    "db-crm": {
      date: { name: "Reached out", type: "date" },
      name: { name: "Person", type: "title" },
      contact: { name: "Email address", type: "email" },
      source: { name: "Channel", type: "select" },
      need: { name: "Summary", type: "rich_text" },
    },
  };

  async notionResources(): Promise<NotionResources> {
    await delay(400);
    this.requireNotion();
    return {
      databases: [
        { id: "db-clean", title: "Ziga Leads", data_source_id: "ds-clean" },
        { id: "db-crm", title: "Client CRM", data_source_id: "ds-crm" },
      ],
      pages: [{ id: "page-home", title: "Workspace Home" }],
      can_create: true,
    };
  }

  async notionMapping(databaseId: string): Promise<NotionMappingResponse> {
    await delay(400);
    this.requireNotion();
    const properties = MockApi.NOTION_SCHEMAS[databaseId];
    if (!properties) throw new ApiError("database not found", 404);
    const mapping = MockApi.NOTION_MAPPINGS[databaseId] ?? {};
    return {
      database_id: databaseId,
      data_source_id: "ds-" + databaseId,
      database_title: databaseId === "db-clean" ? "Ziga Leads" : "Client CRM",
      fields: COLUMNS,
      properties,
      mapping,
      unmapped: COLUMNS.filter((f) => !mapping[f]),
    };
  }

  async createNotionDatabase(parentPageId: string): Promise<NotionConnection> {
    await delay(600);
    this.requireNotion();
    if (!parentPageId) throw new ApiError("parent_page_id is required", 400);
    this.useNotion("db-created", "Ziga Leads", MockApi.NOTION_MAPPINGS["db-clean"]!);
    return {
      database_id: "db-created",
      database_title: "Ziga Leads",
      created_by_app: true,
      mapping: this.notionMappingSaved,
    };
  }

  async setNotionDestination(databaseId: string, mapping: NotionMapping): Promise<NotionConnection> {
    await delay(500);
    this.requireNotion();
    const schema = MockApi.NOTION_SCHEMAS[databaseId];
    if (!schema) throw new ApiError("database not found", 404);
    const chosen = Object.keys(mapping).length ? mapping : (MockApi.NOTION_MAPPINGS[databaseId] ?? {});

    // Mirror the server's re-validation against the live schema.
    for (const [field, target] of Object.entries(chosen)) {
      const prop = schema.find((p) => p.name === target.name);
      if (!prop) {
        throw new ApiError(
          `property "${target.name}" does not exist in this database (property names are case-sensitive)`,
          422,
        );
      }
      if (!prop.writable) {
        throw new ApiError(`property "${target.name}" is a ${prop.type}, which Ziga cannot write to`, 422);
      }
      if (prop.type !== target.type) {
        throw new ApiError(`property "${target.name}" is a ${prop.type}, not a ${target.type}`, 422);
      }
      void field;
    }

    const title = databaseId === "db-clean" ? "Ziga Leads" : "Client CRM";
    this.useNotion(databaseId, title, chosen);
    return {
      database_id: databaseId,
      database_title: title,
      created_by_app: false,
      mapping: chosen,
      unmapped: COLUMNS.filter((f) => !chosen[f]),
    };
  }

  async disconnectNotion(): Promise<void> {
    await delay(200);
    this.notionConnected = false;
    this.notionBroken = false;
    if (this.destinationType === "notion") {
      this.destinationType = "google_sheet";
      this.sheetConnected = false;
    }
  }

  private requireNotion() {
    if (!this.notionConnected) throw new ApiError("Connect your Notion workspace", 409);
    if (this.notionBroken) {
      throw new ApiError("Ziga has lost access to that Notion page. Reconnect Notion and grant it again", 409);
    }
  }

  private useNotion(databaseId: string, title: string, mapping: NotionMapping) {
    this.destinationType = "notion";
    this.notionDatabaseId = databaseId;
    this.notionDatabaseTitle = title;
    this.notionMappingSaved = mapping;
    this.notionBroken = false;
  }

  /**
   * connectNotionForMock stands in for returning from Notion's consent screen;
   * the real flow is a top-level navigation to /api/notion/start, which a
   * backend-less ?mock=1 session cannot follow.
   */
  connectNotionForMock() {
    this.notionConnected = true;
    this.notionBroken = false;
  }

  async history(): Promise<HistoryResponse> {
    return {
      items: this.rows
        .slice()
        .reverse()
        .map((row, i) => ({
          id: i + 1,
          excerpt: row[4],
          result: {
            name: row[1], contact: row[2], source: row[3], need: row[4],
            date: row[0], notes: row[5], confidence: "high",
            missing_fields: [], multiple_leads_detected: false,
          },
          created_at: `${row[0]}T10:00:00Z`,
        })),
    };
  }
}
