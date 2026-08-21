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

/**
 * How a lead arrived.
 *
 * Not to be confused with `ExtractionResult.source`, which is what the lead
 * says about where THEY came from ("X DM", "referral") and is rendered as
 * "Detected source". This one is the channel: did a human paste it, or did it
 * arrive by email while nobody was looking.
 */
export type LeadSource = "paste" | "email";

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
  /** See LeadSource — the channel, not result.source. */
  source?: LeadSource;
  /** Set for email-sourced leads: who it came from, and what it was about. */
  from_address?: string;
  subject?: string;
  received_at?: string;
}

export interface QueueResponse {
  count: number;
  items: Submission[];
  /**
   * Leads captured by email since the user last opened the queue. Ingestion
   * changes the contract: leads now arrive while they are away, so the queue
   * has to say what turned up.
   */
  captured_while_away?: number;
}

// --- Email ingestion ---

/** The user's private capture address. */
export interface InboundAddress {
  /** Empty until the user turns email capture on. */
  address?: string;
  enabled: boolean;
  /** Shown before opting in, so the feature can be explained. */
  domain: string;
  created_at?: string;
}

/** Why a message was filtered. The UI maps these to wording. */
export type QuarantineReason =
  | "blocked_sender"
  | "machine_mail"
  | "calendar_invite"
  | "no_text"
  | "too_short"
  | "size_rejected"
  | "parse_failed"
  | "rate_limited"
  | "forwarding_confirmation";

export interface QuarantineItem {
  id: number;
  status: "quarantined" | "rescued" | "dismissed" | "verification";
  reason: QuarantineReason | string;
  /** The exact rule that fired, so "why was this filtered?" is answerable. */
  detail?: string;
  from_address?: string;
  from_name?: string;
  subject?: string;
  excerpt: string;
  received_at: string;
  /** False once retention has cleared the body: dismissable, not rescuable. */
  rescuable: boolean;
  /** Set on verification items — the forwarding handshake. */
  verify_code?: string;
  verify_url?: string;
}

export interface QuarantineResponse {
  items: QuarantineItem[];
}

export interface PreviewResponse {
  columns: string[];
  rows: string[][];
  error?: string;
}

export type DestinationType = "google_sheet" | "notion";

export interface Destination {
  id: string;
  label: string;
  type: DestinationType | string;
  active?: boolean;
  disabled?: boolean;
  /** The provider is not configured on this server, so it cannot be chosen. */
  unavailable?: boolean;
  dry_run?: boolean;
  connected?: boolean;
  needs_setup?: boolean;
  needs_reconnect?: boolean;
  // google_sheet
  spreadsheet_id?: string;
  // notion
  database_id?: string;
  database_title?: string;
  created_by_app?: boolean;
}

export interface DestinationResponse {
  destinations: Destination[];
}

export interface ConfirmResponse {
  id: number;
  status: Status;
  /** Schema fields the destination had no home for. Never silently dropped. */
  dropped_fields?: string[];
  /** Link to the written row/page, when the destination exposes one. */
  url?: string;
}

export interface HistoryItem {
  id: number;
  excerpt: string;
  result?: ExtractionResult;
  created_at: string;
  /** See LeadSource — the channel, not result.source. */
  source?: LeadSource;
  from_address?: string;
}

export interface HistoryResponse {
  items: HistoryItem[];
}

export function fieldValue(result: ExtractionResult | undefined, name: FieldName): string {
  if (!result) return "";
  const v = result[name];
  return v ?? "";
}

// --- Auth / multi-tenant ---

export interface User {
  id: number;
  email: string;
  email_verified: boolean;
}

// Me is the bootstrap payload from GET /api/me: who the current user is, their
// connection status, and the public config the frontend needs.
export interface Me {
  authenticated: boolean;
  user: User | null;
  google_connected?: boolean;
  notion_connected?: boolean;
  /** A destination is chosen and writable right now. */
  destination_connected?: boolean;
  /**
   * A destination has been chosen at all, healthy or not. This is what gates
   * onboarding: a user whose access was revoked is past setup and needs a
   * reconnect prompt, not the setup flow again.
   */
  destination_configured?: boolean;
  config: {
    google_oauth: boolean;
    google_client_id: string;
    google_picker_api_key: string;
    google_project_number: string;
    notion_oauth: boolean;
    /** Email capture is configured on this server and can be offered. */
    email_ingest: boolean;
  };
}

export interface SheetConnection {
  spreadsheet_id: string;
  sheet_tab: string;
  created_by_app: boolean;
}

// --- Notion ---

/** A database or page the user granted Ziga access to during Notion consent. */
export interface NotionResource {
  id: string;
  title: string;
  data_source_id?: string;
}

export interface NotionResources {
  databases: NotionResource[];
  pages: NotionResource[];
  /** Creating a database needs a granted parent page; false when none was granted. */
  can_create: boolean;
}

/** One property of a Notion database, offered as a mapping target. */
export interface NotionProperty {
  name: string;
  type: string;
  /** False for types Ziga cannot write (status, formula, rollup...). */
  writable: boolean;
}

/** A Ziga field's target property. `name` keeps Notion's exact casing. */
export interface MappedProperty {
  name: string;
  type: string;
}

export type NotionMapping = Record<string, MappedProperty>;

export interface NotionMappingResponse {
  database_id: string;
  data_source_id: string;
  database_title: string;
  fields: string[];
  properties: NotionProperty[];
  mapping: NotionMapping;
  unmapped: string[];
}

export interface NotionConnection {
  database_id: string;
  database_title: string;
  created_by_app: boolean;
  mapping: NotionMapping;
  unmapped?: string[];
}
