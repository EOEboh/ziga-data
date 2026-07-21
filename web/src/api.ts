import { MockApi } from "./mock";
import {
  ConfirmResponse,
  DestinationResponse,
  FieldState,
  HistoryResponse,
  Me,
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
}

function postJSON<T = unknown>(url: string, body: unknown): Promise<T> {
  return request<T>(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

// googleStartURL is the top-level navigation that begins Google OAuth (not a
// fetch — the browser is redirected to Google and back).
export const googleStartURL = "/api/auth/google/start";

export function createApi(): Api {
  return new URLSearchParams(location.search).has("mock") ? new MockApi() : new HttpApi();
}

// api is the single shared client instance used across the app (so ?mock=1
// keeps one consistent in-memory state).
export const api = createApi();
