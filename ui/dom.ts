// Tiny DOM helpers. All extraction-derived content is set via textContent —
// never innerHTML — because pasted lead text is attacker-influenced.

export function $(id: string): HTMLElement {
  const el = document.getElementById(id);
  if (!el) throw new Error(`missing element #${id}`);
  return el;
}

export function clone(tplId: string): HTMLElement {
  const tpl = $(tplId) as HTMLTemplateElement;
  return tpl.content.firstElementChild!.cloneNode(true) as HTMLElement;
}

export function show(el: HTMLElement): void {
  el.hidden = false;
}

export function hide(el: HTMLElement): void {
  el.hidden = true;
}

// relativeTime renders "pasted just now" style timestamps.
export function relativeTime(iso: string): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "";
  const mins = Math.floor((Date.now() - then) / 60_000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins} min ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours} h ago`;
  const days = Math.floor(hours / 24);
  return days === 1 ? "yesterday" : `${days} days ago`;
}

// sentenceCase turns a schema field name into a label: "name" → "Name".
export function sentenceCase(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}
