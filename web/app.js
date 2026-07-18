"use strict";
(() => {
  // ui/api.ts
  var ApiError = class extends Error {
    constructor(message, status, fieldStates) {
      super(message);
      this.status = status;
      this.fieldStates = fieldStates;
    }
  };
  async function request(url, init) {
    let res;
    try {
      res = await fetch(url, init);
    } catch {
      throw new ApiError("Could not reach the server. Check your connection and retry", 0);
    }
    let body = null;
    try {
      body = await res.json();
    } catch {
    }
    if (!res.ok) {
      throw new ApiError(
        body?.error ?? `Request failed (${res.status})`,
        res.status,
        body?.field_states
      );
    }
    return body;
  }
  var HttpApi = class {
    submit(form) {
      return request("/api/submit", { method: "POST", body: form });
    }
    confirm(id, fields) {
      return request(`/api/submissions/${id}/confirm`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ fields })
      });
    }
    async discard(id) {
      await request(`/api/submissions/${id}/discard`, { method: "POST" });
    }
    queue() {
      return request("/api/queue");
    }
    preview() {
      return request("/api/preview");
    }
    destinations() {
      return request("/api/destination");
    }
    history() {
      return request("/api/history");
    }
  };
  var delay = (ms) => new Promise((r) => setTimeout(r, ms));
  var COLUMNS = ["date", "name", "contact", "source", "need", "notes", "flags"];
  var fixtures = [
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
        multiple_leads_detected: false
      },
      field_states: { name: "ok", contact: "ok", source: "ok", date: "ok", need: "ok", notes: "ok" }
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
        multiple_leads_detected: false
      },
      field_states: { name: "ok", contact: "low_confidence", source: "ok", date: "ok", need: "ok", notes: "ok" }
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
        multiple_leads_detected: true
      },
      field_states: { name: "ok", contact: "missing", source: "ok", date: "ok", need: "ok", notes: "ok" },
      flags: ["multiple leads detected \u2014 only the primary lead was extracted"]
    }
  ];
  var MockApi = class {
    constructor() {
      this.nextId = 100;
      this.fixtureIdx = 0;
      this.pending = /* @__PURE__ */ new Map();
      this.rows = [
        ["2026-07-14", "Lena Fischer", "lena@fischer.dev", "referral", "API integration help", "", ""],
        ["2026-07-15", "Sam Torres", "@samtorres", "LinkedIn", "Brand identity refresh", "urgent", ""],
        ["2026-07-16", "Priya Nair", "priya@nair.co", "cold email", "Quarterly tax filing", "", ""]
      ];
    }
    async submit(form) {
      await delay(900);
      const fixture = fixtures[this.fixtureIdx++ % fixtures.length];
      const sub = {
        id: this.nextId++,
        status: "pending",
        result: JSON.parse(JSON.stringify(fixture.result)),
        field_states: { ...fixture.field_states },
        flags: fixture.flags ? [...fixture.flags] : void 0,
        input: {
          text: String(form.get("text") ?? ""),
          has_image: form.get("image") instanceof File
        },
        created_at: (/* @__PURE__ */ new Date()).toISOString()
      };
      this.pending.set(sub.id, sub);
      return sub;
    }
    async confirm(id, fields) {
      await delay(500);
      const sub = this.pending.get(id);
      if (!sub) throw new ApiError("submission not found", 404);
      if (Object.values(fields).some((v) => v.includes("fail"))) {
        throw new ApiError("Could not write to your sheet. Retry", 502);
      }
      const row = COLUMNS.map((col) => col === "flags" ? (sub.flags ?? []).join("; ") : fields[col] ?? "");
      this.rows.push(row);
      this.pending.delete(id);
      return { id, status: "written" };
    }
    async discard(id) {
      await delay(200);
      this.pending.delete(id);
    }
    async queue() {
      const items = [...this.pending.values()].reverse();
      return { count: items.length, items };
    }
    async preview() {
      await delay(200);
      return { columns: COLUMNS, rows: this.rows.slice(-3) };
    }
    async destinations() {
      return {
        destinations: [
          { id: "sheet", label: "Leads 2026 (Google Sheet)", type: "google_sheet", active: true },
          { id: "notion", label: "Notion", type: "notion", disabled: true, coming_soon: true }
        ]
      };
    }
    async history() {
      return {
        items: this.rows.slice().reverse().map((row, i) => ({
          id: i + 1,
          excerpt: row[4],
          result: {
            name: row[1],
            contact: row[2],
            source: row[3],
            need: row[4],
            date: row[0],
            notes: row[5],
            confidence: "high",
            missing_fields: [],
            multiple_leads_detected: false
          },
          created_at: `${row[0]}T10:00:00Z`
        }))
      };
    }
  };
  function createApi() {
    return new URLSearchParams(location.search).has("mock") ? new MockApi() : new HttpApi();
  }

  // ui/dom.ts
  function $(id) {
    const el = document.getElementById(id);
    if (!el) throw new Error(`missing element #${id}`);
    return el;
  }
  function clone(tplId) {
    const tpl = $(tplId);
    return tpl.content.firstElementChild.cloneNode(true);
  }
  function show(el) {
    el.hidden = false;
  }
  function hide(el) {
    el.hidden = true;
  }
  function relativeTime(iso) {
    const then = new Date(iso).getTime();
    if (Number.isNaN(then)) return "";
    const mins = Math.floor((Date.now() - then) / 6e4);
    if (mins < 1) return "just now";
    if (mins < 60) return `${mins} min ago`;
    const hours = Math.floor(mins / 60);
    if (hours < 24) return `${hours} h ago`;
    const days = Math.floor(hours / 24);
    return days === 1 ? "yesterday" : `${days} days ago`;
  }
  function sentenceCase(s) {
    return s.charAt(0).toUpperCase() + s.slice(1);
  }

  // ui/history.ts
  function renderHistory(history) {
    const list = $("history-list");
    list.textContent = "";
    if (history.items.length === 0) {
      const note = document.createElement("div");
      note.className = "empty-note";
      note.textContent = "Nothing added yet. Confirmed leads show up here.";
      list.appendChild(note);
      return;
    }
    for (const item of history.items) {
      const row = clone("tpl-history-item");
      const who = item.result?.name || item.result?.contact || "Unnamed lead";
      row.querySelector(".who").textContent = who;
      row.querySelector(".what").textContent = item.result?.need || item.excerpt;
      row.querySelector(".when").textContent = item.result?.date || relativeTime(item.created_at);
      list.appendChild(row);
    }
  }

  // ui/preview.ts
  var lastPreview = null;
  function renderPreview(preview, pending) {
    lastPreview = preview;
    const strip = $("sheet-preview");
    const head = $("preview-head");
    const body = $("preview-body");
    const note = $("preview-note");
    show(strip);
    head.textContent = "";
    body.textContent = "";
    if (preview.error) {
      note.textContent = "Sheet preview unavailable";
      show(note);
      return;
    }
    hide(note);
    for (const col of preview.columns) {
      const th = document.createElement("th");
      th.textContent = sentenceCase(col);
      head.appendChild(th);
    }
    for (const row of preview.rows) {
      body.appendChild(makeRow(preview.columns, (_col, i) => row[i] ?? "", null));
    }
    if (pending) {
      body.appendChild(makeRow(
        preview.columns,
        (col) => col === "date" ? "pending" : col === "flags" ? "" : pending.values[col] ?? "",
        pending.kind === "error" ? "row--error" : "row--pending"
      ));
    }
    if (preview.rows.length === 0 && !pending) {
      note.textContent = "No rows in the sheet yet";
      show(note);
    }
  }
  function makeRow(columns, value, cls) {
    const tr = document.createElement("tr");
    if (cls) tr.className = cls;
    columns.forEach((col, i) => {
      const td = document.createElement("td");
      td.textContent = value(col, i);
      tr.appendChild(td);
    });
    return tr;
  }
  function updatePendingValues(values) {
    if (!lastPreview) return;
    const rows = $("preview-body").querySelectorAll("tr.row--pending, tr.row--error");
    const tr = rows[rows.length - 1];
    if (!tr) return;
    lastPreview.columns.forEach((col, i) => {
      if (col === "date" || col === "flags") return;
      const td = tr.children[i];
      if (col in values) td.textContent = values[col];
    });
  }
  function flashSettle(preview) {
    renderPreview(preview, null);
    const rows = $("preview-body").querySelectorAll("tr");
    const last = rows[rows.length - 1];
    if (!last) return;
    last.classList.add("row--flash");
    requestAnimationFrame(() => {
      requestAnimationFrame(() => last.classList.remove("row--flash"));
    });
  }

  // ui/types.ts
  var FIELD_ORDER = ["name", "contact", "source", "date", "need", "notes"];
  function fieldValue(result, name) {
    if (!result) return "";
    const v = result[name];
    return v ?? "";
  }

  // ui/review.ts
  function renderLeft(text, imageUrl, createdAt) {
    const raw = $("raw-input");
    raw.textContent = "";
    if (text) {
      raw.textContent = text;
    }
    if (imageUrl) {
      const img = document.createElement("img");
      img.src = imageUrl;
      img.alt = "submitted screenshot";
      raw.appendChild(img);
    }
    $("input-when").textContent = `pasted ${relativeTime(createdAt)}`;
    hide($("detected-source"));
  }
  function renderDetectedSource(source) {
    const el = $("detected-source");
    el.textContent = `Detected source: ${source || "unknown"}`;
    show(el);
  }
  function renderSkeleton() {
    const fields = $("fields");
    fields.textContent = "";
    for (let i = 0; i < FIELD_ORDER.length; i++) {
      fields.appendChild(clone("tpl-skeleton-row"));
    }
    hide($("action-row"));
    hide($("multi-lead-banner"));
    hide($("write-error"));
  }
  function renderFields(sub) {
    const fields = $("fields");
    fields.textContent = "";
    for (const name of FIELD_ORDER) {
      const row = clone("tpl-field-row");
      const label = row.querySelector(".name");
      const pill = row.querySelector(".pill");
      const input = row.querySelector(".field");
      label.textContent = sentenceCase(name);
      input.name = name;
      input.value = fieldValue(sub.result, name);
      applyFieldState(input, pill, sub.field_states?.[name] ?? "ok");
      input.addEventListener("input", () => {
        if (input.classList.contains("field--low") || input.classList.contains("field--missing")) {
          input.classList.add("field--edited");
          hide(pill);
        }
      }, { once: false });
      fields.appendChild(row);
    }
    if (sub.result?.multiple_leads_detected) {
      show($("multi-lead-banner"));
    } else {
      hide($("multi-lead-banner"));
    }
    show($("action-row"));
  }
  function applyFieldStates(states) {
    for (const input of fieldInputs()) {
      const row = input.closest(".field-row");
      const pill = row.querySelector(".pill");
      input.classList.remove("field--edited");
      applyFieldState(input, pill, states[input.name] ?? "ok");
    }
  }
  function applyFieldState(input, pill, state2) {
    input.classList.remove("field--low", "field--missing");
    input.placeholder = "";
    hide(pill);
    pill.classList.remove("pill--amber", "pill--red");
    if (state2 === "low_confidence") {
      input.classList.add("field--low");
      pill.classList.add("pill--amber");
      pill.textContent = "low confidence";
      show(pill);
    } else if (state2 === "missing") {
      input.classList.add("field--missing");
      input.placeholder = "Not stated. Add if known";
      pill.classList.add("pill--red");
      pill.textContent = "not found";
      show(pill);
    }
  }
  function fieldInputs() {
    return [...$("fields").querySelectorAll("input.field")];
  }
  function collectFields() {
    const out = {};
    for (const input of fieldInputs()) {
      out[input.name] = input.value;
    }
    return out;
  }
  function setConfirmBusy(busy) {
    $("confirm-button").disabled = busy;
    $("discard-button").disabled = busy;
    $("retry-button").disabled = busy;
  }
  function showWriteError(message) {
    const el = $("write-error");
    el.querySelector("span").textContent = message;
    show(el);
    hide($("confirm-button"));
  }
  function hideWriteError() {
    hide($("write-error"));
    show($("confirm-button"));
  }

  // ui/main.ts
  var api = createApi();
  var state = { phase: "empty", submission: null, localImageUrl: null, preview: null, composing: false };
  var sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  function enterEmpty() {
    state.phase = "empty";
    state.submission = null;
    state.composing = false;
    releaseLocalImage();
    show($("empty-state"));
    hide($("review-body"));
    updateNewLeadButton();
    refreshPreviewStrip();
  }
  function enterReview(sub) {
    state.phase = sub.status === "failed_write" ? "write_failed" : "reviewing";
    state.submission = sub;
    state.composing = false;
    hide($("empty-state"));
    show($("review-body"));
    updateNewLeadButton();
    renderLeft(sub.input.text ?? "", state.localImageUrl ?? sub.input.image_url ?? null, sub.created_at);
    if (sub.result) renderDetectedSource(sub.result.source);
    renderFields(sub);
    if (state.phase === "write_failed") {
      showWriteError(sub.error || "Could not write to your sheet.");
    } else {
      hideWriteError();
    }
    setConfirmBusy(false);
    refreshPreviewStrip();
  }
  function startComposing() {
    state.composing = true;
    state.phase = "empty";
    state.submission = null;
    releaseLocalImage();
    $("lead-text").value = "";
    const fileInput = $("lead-image");
    fileInput.value = "";
    $("file-name").textContent = "";
    if (location.hash === "#/history") location.hash = "#/";
    show($("empty-state"));
    hide($("review-body"));
    submitError(null);
    updateNewLeadButton();
    refreshBadge();
  }
  async function openQueue() {
    if (!state.composing && !$("review-body").hidden) return;
    state.composing = false;
    await advance();
  }
  function updateNewLeadButton() {
    const onReview = location.hash !== "#/history";
    const pasteVisible = onReview && !$("empty-state").hidden;
    $("new-lead-button").hidden = pasteVisible;
  }
  async function startExtraction() {
    const text = $("lead-text").value.trim();
    const fileInput = $("lead-image");
    const file = fileInput.files?.[0] ?? null;
    if (!text && !file) {
      submitError("Add some text or an image first");
      return;
    }
    submitError(null);
    const form = new FormData();
    if (text) form.set("text", text);
    if (file) {
      form.set("image", file);
      state.localImageUrl = URL.createObjectURL(file);
    }
    state.phase = "extracting";
    state.composing = false;
    hide($("empty-state"));
    show($("review-body"));
    updateNewLeadButton();
    renderLeft(text, state.localImageUrl, (/* @__PURE__ */ new Date()).toISOString());
    renderSkeleton();
    let sub;
    try {
      sub = await api.submit(form);
    } catch (err) {
      state.phase = "empty";
      releaseLocalImage();
      show($("empty-state"));
      hide($("review-body"));
      updateNewLeadButton();
      submitError(err instanceof ApiError ? err.message : "Extraction failed. Try again");
      return;
    }
    if (state.composing) {
      refreshBadge();
      return;
    }
    $("lead-text").value = "";
    fileInput.value = "";
    $("file-name").textContent = "";
    if (sub.duplicate && sub.status === "written") {
      state.phase = "empty";
      releaseLocalImage();
      show($("empty-state"));
      hide($("review-body"));
      updateNewLeadButton();
      submitError("This content was already added today. No new row was created");
      return;
    }
    enterReview(sub);
    refreshBadge();
  }
  async function confirm() {
    const sub = state.submission;
    if (!sub || state.phase === "confirming") return;
    state.phase = "confirming";
    setConfirmBusy(true);
    hideWriteError();
    try {
      await api.confirm(sub.id, collectFields());
    } catch (err) {
      setConfirmBusy(false);
      if (err instanceof ApiError && err.status === 422 && err.fieldStates) {
        state.phase = "reviewing";
        applyFieldStates(err.fieldStates);
        refreshPreviewStrip();
        return;
      }
      if (err instanceof ApiError && err.status === 409) {
        await settle();
        return;
      }
      state.phase = "write_failed";
      showWriteError(err instanceof ApiError && err.status !== 0 ? "Could not write to your sheet." : "Could not reach the server.");
      paintPending("error");
      return;
    }
    await settle();
  }
  async function settle() {
    state.submission = null;
    releaseLocalImage();
    try {
      state.preview = await api.preview();
      flashSettle(state.preview);
    } catch {
    }
    await sleep(600);
    await advance();
  }
  async function discard() {
    const sub = state.submission;
    if (!sub) return;
    setConfirmBusy(true);
    try {
      await api.discard(sub.id);
    } catch {
    }
    state.submission = null;
    await advance();
  }
  async function advance() {
    let next = null;
    try {
      const q = await api.queue();
      badge(q.count);
      next = q.items[0] ?? null;
    } catch {
      badge(0);
    }
    if (next) {
      enterReview(next);
    } else {
      enterEmpty();
    }
  }
  function submitError(msg) {
    const el = $("submit-error");
    if (msg) {
      el.textContent = msg;
      show(el);
    } else {
      hide(el);
    }
  }
  function releaseLocalImage() {
    if (state.localImageUrl) {
      URL.revokeObjectURL(state.localImageUrl);
      state.localImageUrl = null;
    }
  }
  function pendingRow() {
    if (!state.submission) return null;
    return {
      values: collectFields(),
      kind: state.phase === "write_failed" ? "error" : "pending"
    };
  }
  function refreshPreviewStrip() {
    if (state.preview) renderPreview(state.preview, pendingRow());
  }
  function paintPending(kind) {
    if (!state.preview || !state.submission) return;
    renderPreview(state.preview, { values: collectFields(), kind });
  }
  function badge(count) {
    const el = $("queue-badge");
    if (count > 0) {
      el.textContent = String(count);
      show(el);
    } else {
      hide(el);
    }
  }
  async function refreshBadge() {
    try {
      const q = await api.queue();
      badge(q.count);
    } catch {
      badge(0);
    }
  }
  async function initDestination() {
    const toggle = $("destination-toggle");
    const menu = $("destination-menu");
    const label = $("destination-label");
    try {
      const { destinations } = await api.destinations();
      const active = destinations.find((d) => d.active);
      label.textContent = active ? active.label + (active.dry_run ? " \u2014 dry run" : "") : "No destination";
      menu.textContent = "";
      for (const dest of destinations) {
        const item = document.createElement("button");
        item.type = "button";
        item.className = "dropdown-item";
        item.disabled = !!dest.disabled;
        const icon = document.createElement("span");
        icon.className = "dest-icon";
        icon.textContent = dest.type === "google_sheet" ? "\u25A6" : "\u25C6";
        const name = document.createElement("span");
        name.textContent = dest.label;
        item.append(icon, name);
        if (dest.coming_soon) {
          const hint = document.createElement("span");
          hint.className = "hint";
          hint.textContent = "coming soon";
          item.appendChild(hint);
        }
        menu.appendChild(item);
      }
    } catch {
      label.textContent = "Destination unavailable";
    }
    toggle.addEventListener("click", () => {
      const open = menu.hidden;
      menu.hidden = !open;
      toggle.setAttribute("aria-expanded", String(open));
    });
    document.addEventListener("click", (e) => {
      if (!$("destination").contains(e.target)) {
        menu.hidden = true;
        toggle.setAttribute("aria-expanded", "false");
      }
    });
  }
  function applyHash() {
    const isHistory = location.hash === "#/history";
    $("view-review").hidden = isHistory;
    $("view-history").hidden = !isHistory;
    document.querySelectorAll(".views a").forEach((a) => {
      a.classList.toggle("active", a.dataset.view === "history" === isHistory);
    });
    if (isHistory) {
      api.history().then(renderHistory).catch(() => {
        renderHistory({ items: [] });
      });
    }
    updateNewLeadButton();
  }
  async function boot() {
    initDestination();
    window.addEventListener("hashchange", applyHash);
    applyHash();
    $("extract-button").addEventListener("click", startExtraction);
    $("confirm-button").addEventListener("click", confirm);
    $("retry-button").addEventListener("click", confirm);
    $("discard-button").addEventListener("click", discard);
    $("new-lead-button").addEventListener("click", startComposing);
    $("nav-review").addEventListener("click", openQueue);
    const fileInput = $("lead-image");
    $("image-button").addEventListener("click", () => fileInput.click());
    fileInput.addEventListener("change", () => {
      $("file-name").textContent = fileInput.files?.[0]?.name ?? "";
    });
    $("lead-text").addEventListener("keydown", (e) => {
      const ke = e;
      if (ke.key === "Enter" && (ke.metaKey || ke.ctrlKey)) startExtraction();
    });
    $("fields").addEventListener("input", () => {
      if (state.submission) updatePendingValues(collectFields());
    });
    const [queueRes, previewRes] = await Promise.allSettled([api.queue(), api.preview()]);
    if (previewRes.status === "fulfilled") {
      state.preview = previewRes.value;
    } else {
      state.preview = { columns: [], rows: [], error: "preview unavailable" };
    }
    if (queueRes.status === "fulfilled" && queueRes.value.items.length > 0) {
      badge(queueRes.value.count);
      enterReview(queueRes.value.items[0]);
    } else {
      badge(queueRes.status === "fulfilled" ? queueRes.value.count : 0);
      enterEmpty();
    }
  }
  boot();
})();
