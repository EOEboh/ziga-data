import { MockApi } from "./mock";
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

export function createApi(): Api {
  return new URLSearchParams(location.search).has("mock") ? new MockApi() : new HttpApi();
}
