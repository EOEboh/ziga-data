import { MockApi } from "./mock";
import {
  ConfirmResponse,
  DestinationResponse,
  FieldState,
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

  // Auth / onboarding.
  me(): Promise<Me>;
  signup(email: string, password: string): Promise<void>;
  login(email: string, password: string): Promise<void>;
  logout(): Promise<void>;
  forgotPassword(email: string): Promise<void>;
  resetPassword(token: string, password: string): Promise<void>;
  disconnectGoogle(): Promise<void>;
  createSheet(): Promise<SheetConnection>;
  attachSheet(spreadsheetId: string): Promise<SheetConnection>;

  // Notion destination.
  notionResources(): Promise<NotionResources>;
  notionMapping(databaseId: string): Promise<NotionMappingResponse>;
  createNotionDatabase(parentPageId: string): Promise<NotionConnection>;
  setNotionDestination(databaseId: string, mapping: NotionMapping): Promise<NotionConnection>;
  disconnectNotion(): Promise<void>;
}

// readCookie returns a document cookie value by name, or "".
function readCookie(name: string): string {
  const m = document.cookie.match("(?:^|; )" + name + "=([^;]*)");
  return m ? decodeURIComponent(m[1]) : "";
}

const UNSAFE = /^(POST|PUT|PATCH|DELETE)$/i;

async function request<T>(url: string, init: RequestInit = {}): Promise<T> {
  // Same-origin cookies carry the session; unsafe methods must echo the CSRF
  // cookie in a header (signed double-submit — see internal/httpapi/middleware).
  init.credentials = "same-origin";
  if (UNSAFE.test(init.method ?? "GET")) {
    init.headers = { ...(init.headers ?? {}), "X-CSRF-Token": readCookie("ziga_csrf") };
  }
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

  me(): Promise<Me> {
    return request<Me>("/api/me");
  }
  async signup(email: string, password: string): Promise<void> {
    await postJSON("/api/auth/signup", { email, password });
  }
  async login(email: string, password: string): Promise<void> {
    await postJSON("/api/auth/login", { email, password });
  }
  async logout(): Promise<void> {
    await postJSON("/api/auth/logout", {});
  }
  async forgotPassword(email: string): Promise<void> {
    await postJSON("/api/auth/password/forgot", { email });
  }
  async resetPassword(token: string, password: string): Promise<void> {
    await postJSON("/api/auth/password/reset", { token, password });
  }
  async disconnectGoogle(): Promise<void> {
    await postJSON("/api/auth/google/disconnect", {});
  }
  createSheet(): Promise<SheetConnection> {
    return postJSON<SheetConnection>("/api/sheets/create", {});
  }
  attachSheet(spreadsheetId: string): Promise<SheetConnection> {
    return postJSON<SheetConnection>("/api/sheets/attach", { spreadsheet_id: spreadsheetId });
  }

  notionResources(): Promise<NotionResources> {
    return request<NotionResources>("/api/notion/resources");
  }
  notionMapping(databaseId: string): Promise<NotionMappingResponse> {
    return request<NotionMappingResponse>(`/api/notion/databases/${encodeURIComponent(databaseId)}/mapping`);
  }
  createNotionDatabase(parentPageId: string): Promise<NotionConnection> {
    return postJSON<NotionConnection>("/api/notion/databases/create", { parent_page_id: parentPageId });
  }
  setNotionDestination(databaseId: string, mapping: NotionMapping): Promise<NotionConnection> {
    return postJSON<NotionConnection>("/api/notion/destination", { database_id: databaseId, mapping });
  }
  async disconnectNotion(): Promise<void> {
    await postJSON("/api/notion/disconnect", {});
  }
}

function postJSON<T = unknown>(url: string, body: unknown): Promise<T> {
  return request<T>(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

// googleStartURL / notionStartURL are top-level navigations that begin OAuth
// (not fetches — the browser is redirected to the provider and back).
export const googleStartURL = "/api/auth/google/start";
export const notionStartURL = "/api/notion/start";

export function createApi(): Api {
  return new URLSearchParams(location.search).has("mock") ? new MockApi() : new HttpApi();
}

// api is the single shared client instance used across the app (so ?mock=1
// keeps one consistent in-memory state).
export const api = createApi();
