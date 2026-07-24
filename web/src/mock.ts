// MockApi (?mock=1) serves fixtures covering every review state, for demos
// and frontend work with no backend running. Confirm fails when any field
// value contains "fail", exercising the write-failed → retry path.

import { Api, ApiError } from "./api";
import {
  ConfirmResponse,
  DestinationResponse,
  HistoryResponse,
  Me,
  PreviewResponse,
  QueueResponse,
  SheetConnection,
  Submission,
} from "./types";

const delay = (ms: number) => new Promise((r) => setTimeout(r, ms));

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
    const row = COLUMNS.map((col) => (col === "flags" ? (sub.flags ?? []).join("; ") : fields[col] ?? ""));
    this.rows.push(row);
    this.pending.delete(id);
    return { id, status: "written" };
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
    return {
      destinations: [
        {
          id: "sheet",
          label: (this.sheetConnected ? "Ziga Leads" : "No sheet connected") + " (Google Sheet)",
          type: "google_sheet",
          active: true,
        },
        { id: "notion", label: "Notion", type: "notion", disabled: true, coming_soon: true },
      ],
    };
  }

  // --- Auth / onboarding (mock: a pre-connected logged-in user so ?mock=1
  // walks the whole authed experience without a backend) ---

  private authed = true;
  private googleConnected = true;
  private sheetConnected = true;

  async me(): Promise<Me> {
    return {
      authenticated: this.authed,
      user: this.authed ? { id: 1, email: "you@example.com", email_verified: true } : null,
      google_connected: this.googleConnected,
      sheet_connected: this.sheetConnected,
      config: { google_oauth: true, google_client_id: "mock-client", google_picker_api_key: "mock-key", google_project_number: "575697153359" },
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
    this.sheetConnected = false;
  }
  async createSheet(): Promise<SheetConnection> {
    await delay(400);
    this.googleConnected = true;
    this.sheetConnected = true;
    return { spreadsheet_id: "mock-sheet", sheet_tab: "Leads", created_by_app: true };
  }
  async attachSheet(spreadsheetId: string): Promise<SheetConnection> {
    await delay(400);
    this.googleConnected = true;
    this.sheetConnected = true;
    return { spreadsheet_id: spreadsheetId, sheet_tab: "Leads", created_by_app: false };
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
