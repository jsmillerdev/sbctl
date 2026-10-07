# Supavise brand kit

Source of truth for the mark, the lockup, and color. Every asset is generated from one geometry by `build.py`; edit the script, not the SVGs.

```bash
python3 brand/build.py
```

Open `variants.html` to see everything on light, dark, and brand surfaces. It is self-contained, so it renders from a file or an email attachment.

## Files

```
brand/
  BRAND.md                              this file
  build.py                              generates everything below from one geometry
  variants.html                         contact sheet
  favicon.svg                           ink mark on a rounded teal tile
  mark/supavise-mark.svg                signal teal, for black and dark surfaces
  mark/supavise-mark-black.svg          ink, the mark on light surfaces and in print
  mark/supavise-mark-white.svg          for dark and brand surfaces
  lockups/supavise-horizontal.svg       teal mark, white word, for black grounds
  lockups/supavise-horizontal-black.svg
  lockups/supavise-horizontal-white.svg
  social/avatar-{teal,black}            800x800 square avatars, .svg + .png
  social/x-header-{slice,black,teal}    1500x500 X headers, .svg + .png
  social/og-image                       1200x630 link preview, .svg + .png
  social/github-social-preview          1280x640 repository social preview, .svg + .png
  social/apple-touch-icon               180x180 iOS home-screen icon, .svg + .png
```

PNGs are rendered from the SVGs with macOS `sips`:

```bash
cd brand/social && for f in *.svg; do sips -s format png "$f" --out "${f%.svg}.png"; done
```

## Mark

Two interlocking hooks that together read as an S. A top bar carries a stub down its left side, a bottom bar carries a stub up its right side, and each hook is the 180-degree rotation of the other, so the mark is balanced by construction. The whole shape leans 20 degrees. The space between the hooks is a zigzag, two wedges joined by a channel, which is the structure of a bolt.

Construction, in a 1000-unit box (see `build.py` for the parameters):

- Height 680, width 800 including the lean (ratio 1.18, squat on purpose), bar 160.
- Drawn upright, then leaned by a shear of tan(20 degrees) about the middle. Stubs are widened by sqrt(1 + tan^2) so every stroke measures the same 160 across, not just the bars.
- Stubs reach 40 units past the middle. Each stub's far end is cut on a slant so the pockets narrow to points a hairline gap (80) from the opposite bar.
- One flat fill, no strokes, no rounded joins, no gradients. Two subpaths.
- The viewBox is cropped to the glyph with 10 units of bleed, so a CSS height is the glyph height.

Rules:

- Never stroke it, round it, or add a shadow or gradient.
- Clear space: one bar width (about 24% of the mark height) on every side.
- Minimum 16px tall for the master. At 16px the hairline gap is about 1px; below that use the favicon tile.

## Lockup

The mark is the initial S. "upavise" is Manrope ExtraBold (OFL), outlined to paths and tracked -3.5%, the face the Allocait lockup uses so the family shares one voice. There is no separate wordmark and no stacked version: the mark is the S, so stacking it over the word would repeat the letter.

- Mark height equals Manrope's cap height. Its edges are flat, so no overshoot is needed.
- Gap from the mark's right ink edge to the u stem is 215 font units.
- The lockup includes the descender of the p and the dot of the i, so its viewBox is taller than the cap height.
- Never re-space, re-set in live type, or add a tagline inside the lockup.

## Color

```
signal         #00e0c4   the mark on black and dark grounds; the ground that carries an ink mark
signal-ink     #0b0b0b   brand black: the mark on light, grounds, text on teal
white          #ffffff   the word on black and dark grounds
```

- Color goes on black; ink goes on light. On white the brand speaks black. Teal is about 1.6:1 on white, so it is never text or a thin mark on a light surface.
- On black, signal teal is about 12:1. Ink on signal teal is the same, which is why the favicon, avatar and app icon use an ink mark on a teal tile.
- Teal is not Supabase green (`#3ecf8e`), on purpose: the project must not look like an official Supabase product. `#00e0c4` replaces the first pick `#00c9b0`, which was already at full saturation in sRGB, so it is lighter and about 9% higher in chroma at the same hue.

## Social

- Avatar: `avatar-teal.png` (ink mark on teal) everywhere: X, LinkedIn, GitHub, Bluesky. Use `avatar-black.png` only on light-only surfaces. The mark is 56% of the side wide, so it survives a circle crop.
- X header: `x-header-slice.png` is the default, with the oversized mark off the right edge and the zigzag in frame. The lockup stays centered in the middle band because X covers the bottom-left and trims the top and bottom on some screens.
- GitHub: upload `github-social-preview.png` under the repository settings, Social preview.

## Typography

Manrope appears only inside the lockup, outlined, and is not loaded as a webfont. Docs and any site use the system font stack or Inter.

## Not affiliated

Supavise is not affiliated with or endorsed by Supabase Inc. Never use the Supabase logo, its green, or its bolt in Supavise assets, and never write the name in a way that implies an official product. Say "runs Supabase's open-source services" in plain text.

## Open

- The mark was chosen to echo the structure of a bolt without copying one. If Supabase ever objects, the first dial to turn is the lean and the wedge angle in `build.py`.
- The lockup's "s" and "e" are stock Manrope. Squaring their terminals to match the mark's flat cuts would need a path-boolean library such as `skia-pathops`, which is not installed.
- The favicon is the mark scaled onto a tile, not a pixel-snapped small-size drawing as in the Allocait kit. If the hairline gap blurs at 16px in a real tab strip, draw a dedicated small version in `build.py`.
