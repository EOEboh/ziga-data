// The Ziga Data mark: a Z built from spreadsheet rows. This is the only vector
// in the app — everything else uses Unicode glyphs — so it stays a single small
// component rather than the start of an icon set.
//
// Geometry is generated from brand/mark.svg. If you change it there, change it
// here too and re-run brand/render.sh. See brand/README.md for the construction
// rules (32-unit grid, 4.0 bars, 3.6 diagonal, square corners at the joins).
//
// Renders in currentColor, so callers set the colour with a text class.
export function Mark({ size = 18, className }: { size?: number; className?: string }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="4 4 24 24"
      fill="currentColor"
      className={className}
      aria-hidden="true"
      focusable="false"
    >
      <path d="M6 6 H26 A1 1 0 0 1 27 7 V10 H6 A1 1 0 0 1 5 9 V7 A1 1 0 0 1 6 6 Z" />
      <polygon points="27,10 21,10 5,22 11,22" />
      <path d="M5 22 H13.5 A1 1 0 0 1 14.5 23 V25 A1 1 0 0 1 13.5 26 H6 A1 1 0 0 1 5 25 V22 Z" />
      <rect x="17.5" y="22" width="9.5" height="4" rx="1" />
    </svg>
  );
}
