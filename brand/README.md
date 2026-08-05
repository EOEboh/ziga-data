# Ziga Data — brand assets

Everything here derives from one file. `mark.svg` is the master; `render.sh` cuts every
other asset from it. Never edit a generated PNG by hand — change the master and re-render.

## The mark

The mark is a **Z built from spreadsheet rows**: a full-width header row, a diagonal, and a
data row split into two cells. It reads as the initial and as the product in the same shape.

It is drawn on a 32-unit grid, optically centred on (16, 16), with the drawn area spanning
`x 5→27, y 6→26`. That 5-unit margin is why the glyph drops straight into a rounded-square
container without rescaling.

Construction rules — keep these if you edit the geometry:

| Rule | Value |
|---|---|
| Grid | 32 units, integers and halves only |
| Bar height | 4.0 |
| Diagonal thickness | 3.6 perpendicular (0.90 × bar) |
| Corner radius | `rx="1"` on every bar |
| Cell gap, data row | 3.0 |
| Container radius | `rx="7"` on 32 (matches `--radius-ctl`) |
| Color | `#1D9E75` only, or white knocked out of it |

The diagonal is cut **thinner** than the bars on purpose. A diagonal at the same geometric
thickness as a horizontal bar reads heavy; 0.90× is the correction that makes them look
equal. Nothing here uses a gradient, shadow, bevel, or second color.

**Known behaviour:** the 3.0-unit gap in the data row closes up under ~20px, so at favicon
sizes the bottom row reads as one bar. This is expected and was accepted when the mark was
chosen — the silhouette still reads as a Z.

## Two viewBoxes

- **Standalone** — `viewBox="4 4 24 24"`, tight crop, `fill="currentColor"`. Use next to the
  wordmark, in the TopBar, anywhere the mark sits on the page ground.
- **Container** — `viewBox="0 0 32 32"` with the green `rx="7"` rect behind a white glyph.
  Use for favicons, app icons, and the OAuth consent logo.

## Rendering

```sh
./brand/render.sh
```

Requires Google Chrome at the standard macOS path — no Node, ImageMagick, or librsvg. Chrome
runs headless and screenshots each wrapper at exact pixel dimensions. The script is
idempotent; it rewrites every output every time.

## Outputs

| File | Size | Use |
|---|---|---|
| `brand/png/oauth-logo-120.png` | 120×120 | **Google Cloud Console → OAuth consent screen.** The one to upload. |
| `brand/png/oauth-logo-512.png` | 512×512 | Same artwork larger, for Play/Workspace listings |
| `brand/png/mark-green-512.png` | 512×512 | Green mark, transparent background |
| `brand/png/mark-white-512.png` | 512×512 | White mark, transparent background, for dark grounds |
| `brand/png/lockup-light-1600.png` | 1600×400 | Mark + wordmark on light grounds; the README header |
| `brand/png/lockup-dark-1600.png` | 1600×400 | Same lockup, wordmark flipped for dark grounds |
| `site/assets/favicon.svg` | 32×32 | Site favicon |
| `site/assets/apple-touch-icon.png` | 180×180 | iOS home screen, opaque |
| `site/assets/icon-192.png` | 192×192 | Web app manifest |
| `site/assets/icon-512.png` | 512×512 | Web app manifest |
| `site/assets/og.png` | 1200×630 | Open Graph / Twitter card |
| `web/public/favicon.svg` | 32×32 | App favicon, copied into `web/dist` by Vite |

## Uploading to the Google OAuth consent screen

Use `brand/png/oauth-logo-120.png`. Google requires a square image of at least 120×120 under
1 MB, and the product name shown beside it must match the wordmark used in the app — which is
why every surface reads **Ziga Data**, never bare "Ziga". Changing the logo there re-triggers
brand verification, so upload it once and leave it.

## Colors

Taken from `web/src/styles.css`, mirrored in `site/styles.css`. Change one, change both.

- `#1D9E75` brand green — the only color the mark uses
- `#0F6E56` deep green — supporting UI only, never in the mark
- `#FAFAF8` page ground · `#1A1A18` ink · `#6B6B66` secondary · `#E4E4DF` hairline

The amber and red tokens are reserved for semantic state and must never appear in brand
artwork.
