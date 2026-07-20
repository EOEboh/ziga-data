// Interfaces mirroring the Go JSON shapes in internal/httpapi.

export type Status = "pending" | "written" | "failed_write";
export type FieldState = "ok" | "low_confidence" | "missing";
export type Confidence = "high" | "medium" | "low";

// The schema is config-driven server-side; the UI renders this fixed v1
// field set (matching config/schema.json).
export const FIELD_ORDER = ["name", "contact", "source", "date", "need", "notes"] as const;
export type FieldName = (typeof FIELD_ORDER)[number];

export interface ExtractionResult {
  name: string | null;
  contact: string | null;
  source: string;
  need: string;
  date: string;
  notes: string;
  confidence: Confidence;
  field_confidence?: Record<string, Confidence>;
  missing_fields: string[];
  multiple_leads_detected: boolean;
}

export interface SubmissionInput {
  text?: string;
  has_image: boolean;
  image_url?: string;
}

export interface Submission {
  id: number;
  status: Status;
  duplicate?: boolean;
  result?: ExtractionResult;
  field_states?: Record<string, FieldState>;
  flags?: string[];
  error?: string;
  input: SubmissionInput;
  created_at: string;
}

export interface QueueResponse {
  count: number;
  items: Submission[];
}

export interface PreviewResponse {
  columns: string[];
  rows: string[][];
  error?: string;
}

export interface Destination {
  id: string;
  label: string;
  type: string;
  active?: boolean;
  disabled?: boolean;
  coming_soon?: boolean;
  dry_run?: boolean;
}

export interface DestinationResponse {
  destinations: Destination[];
}

export interface ConfirmResponse {
  id: number;
  status: Status;
}

export interface HistoryItem {
  id: number;
  excerpt: string;
  result?: ExtractionResult;
  created_at: string;
}

export interface HistoryResponse {
  items: HistoryItem[];
}

export function fieldValue(result: ExtractionResult | undefined, name: FieldName): string {
  if (!result) return "";
  const v = result[name];
  return v ?? "";
}
