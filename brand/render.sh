#!/usr/bin/env bash
#
# Cuts every brand asset from brand/mark.svg.
#
# Rasterising is done by headless Chrome so the repo needs no Node, ImageMagick, or
# librsvg. Chrome screenshots each wrapper at exactly the target pixel size, so the
# PNGs are vector-sharp rather than resampled.
#
# Usage:  ./brand/render.sh
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

if [ ! -x "$CHROME" ]; then
  echo "error: Google Chrome not found at $CHROME" >&2
  echo "Install Chrome, or point CHROME at any Chromium binary." >&2
  exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# The single source of truth: everything below is composed from this fragment.
GLYPH="$(awk '/glyph:start/{f=1;next} /glyph:end/{f=0} f' "$ROOT/brand/mark.svg")"

GREEN="#1D9E75"
INK="#1A1A18"
GROUND="#FAFAF8"
LINE="#E4E4DF"
MUTED="#6B6B66"

# shot <out.png> <width> <height> <transparent:yes|no> — body markup on stdin
shot() {
  local out="$1" w="$2" h="$3" transparent="$4"
  local page="$TMP/page.html"
  {
    echo '<!doctype html><html><head><meta charset="utf-8"><style>'
    echo "  html,body{margin:0;padding:0;width:${w}px;height:${h}px;overflow:hidden}"
    if [ "$transparent" = "yes" ]; then
      echo '  body{background:transparent}'
    fi
    echo '  body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;-webkit-font-smoothing:antialiased}'
    echo '  .fill{width:100%;height:100%;display:flex;align-items:center;justify-content:center}'
    echo '</style></head><body>'
    cat
    echo '</body></html>'
  } > "$page"

  "$CHROME" --headless --disable-gpu --hide-scrollbars \
    --force-device-scale-factor=1 \
    --default-background-color=00000000 \
    --screenshot="$out" --window-size="${w},${h}" \
    "file://$page" >/dev/null 2>&1

  mkdir -p "$(dirname "$out")"
  echo "  $(basename "$out")  ${w}x${h}"
}

# svg_standalone <px> <color>  — tight crop, no container
svg_standalone() {
  printf '<svg width="%s" height="%s" viewBox="4 4 24 24" xmlns="http://www.w3.org/2000/svg"><g fill="%s">%s</g></svg>' "$1" "$1" "$2" "$GLYPH"
}

# svg_container <px> <rx> — white glyph knocked out of the green square
#
# rx=7 keeps the app-icon rounding, for the SVG favicon that sits on a page.
# rx=0 is full-bleed, which is what every PNG icon wants: iOS, Android, and the
# Google consent screen all apply their own mask (a squircle or a circle), and a
# pre-rounded corner shows through it as a notch.
svg_container() {
  printf '<svg width="%s" height="%s" viewBox="0 0 32 32" xmlns="http://www.w3.org/2000/svg"><rect width="32" height="32" rx="%s" fill="%s"/><g fill="#FFFFFF">%s</g></svg>' "$1" "$1" "$2" "$GREEN" "$GLYPH"
}

mkdir -p "$ROOT/brand/png" "$ROOT/site/assets" "$ROOT/web/public"

# ---------------------------------------------------------------- SVG favicons
echo "SVG"
write_favicon() {
  cat > "$1" <<EOF
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" role="img" aria-label="Ziga Data">
  <rect width="32" height="32" rx="7" fill="$GREEN"/>
  <g fill="#FFFFFF">
$GLYPH
  </g>
</svg>
EOF
  echo "  $(basename "$1")  32x32"
}
write_favicon "$ROOT/site/assets/favicon.svg"
write_favicon "$ROOT/web/public/favicon.svg"

# ------------------------------------------------------- Google OAuth console
echo "PNG — Google OAuth consent screen"
shot "$ROOT/brand/png/oauth-logo-120.png" 120 120 no <<EOF
<div class="fill">$(svg_container 120 0)</div>
EOF
shot "$ROOT/brand/png/oauth-logo-512.png" 512 512 no <<EOF
<div class="fill">$(svg_container 512 0)</div>
EOF

# ------------------------------------------------------------- standalone PNGs
echo "PNG — standalone mark"
shot "$ROOT/brand/png/mark-green-512.png" 512 512 yes <<EOF
<div class="fill">$(svg_standalone 512 "$GREEN")</div>
EOF
shot "$ROOT/brand/png/mark-white-512.png" 512 512 yes <<EOF
<div class="fill">$(svg_standalone 512 "#FFFFFF")</div>
EOF

# ------------------------------------------------------------------- site icons
echo "PNG — site icons"
shot "$ROOT/site/assets/apple-touch-icon.png" 180 180 no <<EOF
<div class="fill">$(svg_container 180 0)</div>
EOF
shot "$ROOT/site/assets/icon-192.png" 192 192 no <<EOF
<div class="fill">$(svg_container 192 0)</div>
EOF
shot "$ROOT/site/assets/icon-512.png" 512 512 no <<EOF
<div class="fill">$(svg_container 512 0)</div>
EOF

# ----------------------------------------------------------------------- lockup
echo "PNG — lockup"
shot "$ROOT/brand/png/lockup-light-1600.png" 1600 400 yes <<EOF
<div class="fill" style="gap:40px">
  $(svg_standalone 170 "$GREEN")
  <div style="font-size:150px;font-weight:600;letter-spacing:-0.025em;color:$INK;line-height:1">Ziga Data</div>
</div>
EOF
# The mark keeps its green on both grounds — only the wordmark flips, so the pair
# stays recognisably the same lockup. Used by the README's <picture> switch.
shot "$ROOT/brand/png/lockup-dark-1600.png" 1600 400 yes <<EOF
<div class="fill" style="gap:40px">
  $(svg_standalone 170 "$GREEN")
  <div style="font-size:150px;font-weight:600;letter-spacing:-0.025em;color:#ECECE6;line-height:1">Ziga Data</div>
</div>
EOF

# --------------------------------------------------------------------- OG image
echo "PNG — Open Graph card"
shot "$ROOT/site/assets/og.png" 1200 630 no <<EOF
<div style="width:1200px;height:630px;background:$GROUND;padding:92px 96px;box-sizing:border-box;display:flex;flex-direction:column;justify-content:center;gap:40px;color:$INK">
  <div style="display:flex;align-items:center;gap:22px">
    $(svg_standalone 56 "$GREEN")
    <div style="font-size:44px;font-weight:600;letter-spacing:-0.025em">Ziga Data</div>
  </div>
  <div style="font-size:82px;font-weight:600;letter-spacing:-0.035em;line-height:1.06">
    Messy leads in.<br>Clean spreadsheet out.
  </div>
  <div style="height:1px;background:$LINE"></div>
  <div style="font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:24px;color:$MUTED;letter-spacing:0.02em">zigadata.com</div>
</div>
EOF

echo
echo "Done. Upload brand/png/oauth-logo-120.png to the Google OAuth consent screen."
