import {
  ConfirmResponse,
  DestinationResponse,
  FieldState,
  HistoryResponse,
  PreviewResponse,
  QueueResponse,
  Submission,
} from "./types";

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly fieldStates?: Record<string, FieldState>,
  ) {
    super(message);
  }
}

// Api is the seam between the UI and the backend. MockApi (?mock=1) serves
// fixtures covering every review state, for demos and frontend work with no
// backend running; nothing outside createApi knows which one is live.
export interface Api {
  submit(form: FormData): Promise<Submission>;
  confirm(id: number, fields: Record<string, string>): Promise<ConfirmResponse>;
  discard(id: number): Promise<void>;
  queue(): Promise<QueueResponse>;
  preview(): Promise<PreviewResponse>;
  destinations(): Promise<DestinationResponse>;
  history(): Promise<HistoryResponse>;
}

async function request<T>(url: string, init?: RequestInit): Promise<T> {
  let res: Response;
  try {
    res = await fetch(url, init);
  } catch {
    throw new ApiError("Could not reach the server. Check your connection and retry", 0);
  }
  let body: any = null;
  try {
    body = await res.json();
  } catch {
    // non-JSON error body; fall through to the status check
  }
  if (!res.ok) {
    throw new ApiError(
      body?.error ?? `Request failed (${res.status})`,
      res.status,
      body?.field_states,
    );
  }
  return body as T;
}

class HttpApi implements Api {
  submit(form: FormData): Promise<Submission> {
    return request<Submission>("/api/submit", { method: "POST", body: form });
  }
  confirm(id: number, fields: Record<string, string>): Promise<ConfirmResponse> {
    return request<ConfirmResponse>(`/api/submissions/${id}/confirm`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ fields }),
    });
  }
  async discard(id: number): Promise<void> {
    await request(`/api/submissions/${id}/discard`, { method: "POST" });
  }
  queue(): Promise<QueueResponse> {
    return request<QueueResponse>("/api/queue");
  }
  preview(): Promise<PreviewResponse> {
    return request<PreviewResponse>("/api/preview");
  }
  destinations(): Promise<DestinationResponse> {
    return request<DestinationResponse>("/api/destination");
  }
  history(): Promise<HistoryResponse> {
    return request<HistoryResponse>("/api/history");
  }
}

// ---- mock -----------------------------------------------------------------

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

class MockApi implements Api {
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
        { id: "sheet", label: "Leads 2026 (Google Sheet)", type: "google_sheet", active: true },
        { id: "notion", label: "Notion", type: "notion", disabled: true, coming_soon: true },
      ],
    };
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

export function createApi(): Api {
  return new URLSearchParams(location.search).has("mock") ? new MockApi() : new HttpApi();
}
