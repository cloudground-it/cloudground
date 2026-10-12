"""Draw CloudGround's logo, README banners, social card and favicon as SVG.

    python3 scripts/brand-assets.py <repository>

The type is IBM Plex Sans (600) and IBM Plex Mono (500), the panel's own
fonts in ui/app/assets/fonts (SIL Open Font Licence). Every letter becomes a
path, so the files need no font to look right.

Output, relative to the repository: docs/media/logo.svg, logo-dark.svg,
banner-light.svg, banner-dark.svg, og.svg, and the panel's icons:
ui/public/favicon.svg and ui/public/app-tile.svg (the tile the PNG icons
apple-touch-icon.png, icon-192.png and icon-512.png are rendered from, with
any SVG rasteriser at those sizes).

The mark, "Messa a terra", is the electrical ground symbol with a cloud at
the top of its wire, on a 64-unit grid in one stroke 6 wide with round ends:
a cloud of three circles (radius 6.5 at 20.5,22; 10 at 32,17.5; 7 at
43.5,21.5) cut straight at y=28.5, a wire down to y=38, and bars 40, 24 and 8
long at y=38, 47.5 and 57. Its colour is #2F5BFF, #7D97FF on dark grounds.
At 20 pixels and under it is drawn filled on a 16-pixel grid (FAVICON).
The app tile is a #2F5BFF square with corners of radius 14 and the mark in
white, scaled to 0.61 with a 6.6 stroke.

Needs fontTools and brotli (pip install fonttools brotli).
"""
import sys
from pathlib import Path

from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.ttLib import TTFont
from fontTools.varLib.instancer import instantiateVariableFont

# Palette v3 (ui/app/assets/css/main.css).
COLOURS = {
    "paper": "#F9FAFC", "grid": "#E6E9F1", "ink": "#11141C", "muted": "#586074", "blue": "#2549E0",
    "night": "#0E1117", "grid-dark": "#1E2330", "ink-dark": "#EEF1F7", "muted-dark": "#9AA3B8",
    "blue-dark": "#8AA2FF", "brand": "#2F5BFF", "mark-dark": "#7D97FF", "white": "#FFFFFF",
}
MARK = ("M20.5 28.5 H43.5 A7 7 0 1 0 41.62 14.76 A10 10 0 0 0 22.16 15.72 A6.5 6.5 0 1 0 20.5 28.5 Z "
        "M32 28.5 V38 M12 38 H52 M20 47.5 H44 M28 57 H36")
FAVICON = ('<circle cx="5" cy="5.6" r="2.4"/><circle cx="8.2" cy="4.1" r="3.1"/><circle cx="11.1" cy="5.9" r="2.1"/>'
           '<rect x="5" y="5.5" width="6.2" height="2.5"/><rect x="7" y="7" width="2" height="3"/>'
           '<rect x="1" y="10" width="14" height="1" rx=".5"/><rect x="4" y="12" width="8" height="1" rx=".5"/>'
           '<rect x="6" y="14" width="4" height="1" rx=".5"/>')
LABEL = "CloudGround — Hosting che si misura"


class Face:
    """One font at fixed axis values, able to turn a string into an outline."""

    def __init__(self, path, **axes):
        font = TTFont(path)
        self.font = instantiateVariableFont(font, axes) if axes else font
        self.em = self.font["head"].unitsPerEm
        self.cmap = self.font.getBestCmap()
        self.glyphs = self.font.getGlyphSet()
        self.metrics = self.font["hmtx"]

    def outline(self, text, size, x=0.0, y=0.0, tracking=0.0):
        """The SVG path of text set at size with its baseline starting at (x, y),
        and how wide it is. tracking is extra space per letter, in ems."""
        k = size / self.em
        pen = SVGPathPen(self.glyphs)
        pos = x
        for char in text:
            name = self.cmap[ord(char)]
            self.glyphs[name].draw(TransformPen(pen, (k, 0, 0, -k, pos, y)))
            pos += self.metrics[name][0] * k + tracking * size
        return pen.getCommands(), pos - x

    def width(self, text, size, tracking=0.0):
        return self.outline(text, size, tracking=tracking)[1]


def mark(colour, x, y, size, stroke=6):
    """The mark, size units wide, its 64-unit box's corner at (x, y)."""
    return (f'<g transform="translate({x:.2f} {y:.2f}) scale({size / 64:.4f})">'
            f'<path d="{MARK}" fill="none" stroke="{colour}" stroke-width="{stroke}" '
            'stroke-linecap="round" stroke-linejoin="round"/></g>')


def tile(x, y, size):
    """The app tile: the mark in white on a rounded #2F5BFF square."""
    return (f'<g transform="translate({x:.2f} {y:.2f}) scale({size / 64:.4f})">'
            f'<rect width="64" height="64" rx="14" fill="{COLOURS["brand"]}"/>'
            f'{mark(COLOURS["white"], 12.5, 12.4, 64 * 0.61, stroke=6.6)}</g>')


def svg(width, height, body, label=None):
    """A complete SVG document around body."""
    attrs = f'viewBox="0 0 {width} {height}" width="{width}" height="{height}"'
    if label:
        attrs += f' role="img" aria-label="{label}"'
    return f'<svg xmlns="http://www.w3.org/2000/svg" {attrs}>{body}</svg>\n'


def path(fill, d):
    return f'<path fill="{fill}" d="{d}"/>'


def squares(width, height, colour, step=40):
    """A background of 40-unit squares drawn in colour."""
    return (f'<defs><pattern id="g" width="{step}" height="{step}" patternUnits="userSpaceOnUse">'
            f'<path d="M{step} 0H0V{step}" fill="none" stroke="{colour}" stroke-width="1"/></pattern></defs>'
            f'<rect width="{width}" height="{height}" fill="url(#g)"/>')


def lockup(faces, dark):
    """The horizontal logo: the mark and the name, 150 high, no ground."""
    c = COLOURS
    height, pad, size, gap, type_size = 150, 18, 114, 22, 84
    x = pad + size + gap
    name, width = faces["sans"].outline("CloudGround", type_size, x, 106, tracking=-0.015)
    total = round(x + width + pad)
    body = mark(c["mark-dark"] if dark else c["brand"], pad, (height - size) / 2, size) + \
        path(c["ink-dark"] if dark else c["ink"], name)
    return svg(total, height, body, "CloudGround")


def banner(faces, dark):
    width, height = 1280, 320
    c = COLOURS
    background, text = (c["night"], c["ink-dark"]) if dark else (c["paper"], c["ink"])
    size, left, gap = 150, 64, 40
    x = left + size + gap
    lead = "Hosting che si "
    lead_width = faces["sans"].width(lead, 34, tracking=-0.01)
    tag = "Pannello di hosting source-available".upper()
    tag_width = faces["mono"].width(tag, 11, tracking=0.15)
    parts = [
        f'<rect width="{width}" height="{height}" fill="{background}"/>',
        squares(width, height, c["grid-dark"] if dark else c["grid"]),
        tile(left, (height - size) / 2, size),
        path(text, faces["sans"].outline("CloudGround", 92, x, 168, tracking=-0.025)[0]),
        path(text, faces["sans"].outline(lead, 34, x + 4, 222, tracking=-0.01)[0]),
        path(c["blue-dark"] if dark else c["blue"],
             faces["sans"].outline("misura.", 34, x + 4 + lead_width, 222, tracking=-0.01)[0]),
        path(c["muted-dark"] if dark else c["muted"],
             faces["mono"].outline(tag, 11, width - 28 - tag_width, height - 22, tracking=0.15)[0]),
    ]
    return svg(width, height, "".join(parts), LABEL)


def social_card(faces):
    width, height, margin = 1200, 630, 64
    c = COLOURS
    sans, mono = faces["sans"], faces["mono"]
    site = "CLOUDGROUND.IT"
    first_width = sans.width("che si ", 132, tracking=-0.03)
    badge = "SOURCE-AVAILABLE"
    badge_width = mono.width(badge, 12, tracking=0.15)
    badge_x = width - margin - badge_width - 20
    parts = [
        f'<rect width="{width}" height="{height}" fill="{c["paper"]}"/>',
        tile(margin, margin, 56),
        path(c["ink"], sans.outline("CloudGround", 30, margin + 70, margin + 38, tracking=-0.015)[0]),
        path(c["muted"], mono.outline(site, 13, width - margin - mono.width(site, 13, tracking=0.15),
                                      margin + 34, tracking=0.15)[0]),
        path(c["ink"], sans.outline("Hosting", 132, margin - 6, 330, tracking=-0.03)[0]),
        path(c["ink"], sans.outline("che si ", 132, margin - 6, 444, tracking=-0.03)[0]),
        path(c["blue"], sans.outline("misura.", 132, margin - 6 + first_width, 444, tracking=-0.03)[0]),
        f'<rect x="{margin}" y="{height - margin - 30}" width="{width - 2 * margin}" height="1" fill="{c["ink"]}"/>',
        path(c["muted"], mono.outline("UN COMANDO · UN CLIC PER SITO · BACKUP RIPRISTINATI PER PROVA",
                                      12, margin, height - margin, tracking=0.15)[0]),
        f'<rect x="{badge_x}" y="{height - margin - 17}" width="{badge_width + 20}" height="26" fill="{c["ink"]}"/>',
        path(c["paper"], mono.outline(badge, 12, badge_x + 10, height - margin, tracking=0.15)[0]),
    ]
    return svg(width, height, "".join(parts), LABEL)


def favicon():
    """The browser tab's icon: the filled 16-pixel mark, lighter on a dark tab strip."""
    c = COLOURS
    return ('<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" width="16" height="16">'
            f'<style>g{{fill:{c["brand"]}}}@media (prefers-color-scheme:dark){{g{{fill:{c["mark-dark"]}}}}}</style>'
            f'<g>{FAVICON}</g></svg>\n')


def app_tile():
    return svg(64, 64, tile(0, 0, 64))


def main(repository):
    fonts = repository / "ui/app/assets/fonts"
    faces = {
        "sans": Face(fonts / "plex-sans-latin.woff2", wght=600),
        "mono": Face(fonts / "plex-mono-500.woff2"),
    }
    outputs = {
        "docs/media/logo.svg": lockup(faces, dark=False),
        "docs/media/logo-dark.svg": lockup(faces, dark=True),
        "docs/media/banner-light.svg": banner(faces, dark=False),
        "docs/media/banner-dark.svg": banner(faces, dark=True),
        "docs/media/og.svg": social_card(faces),
        "ui/public/favicon.svg": favicon(),
        "ui/public/app-tile.svg": app_tile(),
    }
    for name, content in outputs.items():
        target = repository / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content)
        print("wrote", name)


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    main(Path(sys.argv[1]))
