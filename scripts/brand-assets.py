"""Draw CloudGround's README banners and social card as SVG.

    python3 scripts/brand-assets.py <repository> <font folder>

The font folder holds three fonts from github.com/google/fonts (SIL Open Font
Licence), saved under these names:

    Archivo.ttf                 ofl/archivo/Archivo[wdth,wght].ttf
    InstrumentSerif-Italic.ttf  ofl/instrumentserif/InstrumentSerif-Italic.ttf
    GeistMono.ttf               ofl/geistmono/GeistMono[wght].ttf

Output, relative to the repository: docs/media/banner-light.svg,
docs/media/banner-dark.svg and docs/media/og.svg. The logo (docs/media/logo.svg,
logo-dark.svg) and the favicon (ui/public/favicon.svg) are the redrawn v2 mark
(no tile, the cloud in ink over a blue and an orange bar; the favicon keeps an
ink tile) and are no longer drawn here.
Every letter becomes a path, so the files need no font to look right.

The mark is a 48x48 tile with corners of radius 11. On it, scaled to 0.84 and
moved by (4, 3): a cloud made of three circles (radius 8 at 14,18; 10 at 24,16;
6 at 36,20) cut straight at y=26, and below it two bars 4.6 high, the blue one
8 units right of the orange one.

Needs fontTools (pip install fonttools).
"""
import sys
from pathlib import Path

from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.ttLib import TTFont
from fontTools.varLib.instancer import instantiateVariableFont

# The brand colours.
COLOURS = {
    "paper": "#F2F1EC", "grid": "#E2E0D8", "grid-dark": "#1E1E24",
    "ink": "#0F0F12", "muted": "#55555D", "muted-dark": "#B7B6AE", "footer": "#3B3B42",
    "blue": "#2F5BFF", "blue-dark": "#7C9BFF", "orange": "#E0612B",
}
CLOUD = "M14 26A8 8 0 1 1 15.843 10.215A10 10 0 0 1 33.87 14.391A6 6 0 1 1 36 26Z"
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


def tile(fill, cloud):
    """The mark on a 48-unit grid: a tile of colour fill with the cloud in cloud."""
    return "".join([
        f'<rect width="48" height="48" rx="11" fill="{fill}"/>',
        '<g transform="translate(4 3) scale(.84)">',
        f'<path fill="{cloud}" d="{CLOUD}"/>',
        f'<rect x="14" y="30" width="28" height="4.6" rx="1" fill="{COLOURS["blue"]}"/>',
        f'<rect x="6" y="38" width="28" height="4.6" rx="1" fill="{COLOURS["orange"]}"/>',
        "</g>",
    ])


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


def banner(faces, dark):
    width, height = 1280, 320
    c = COLOURS
    background, text = (c["ink"], c["paper"]) if dark else (c["paper"], c["ink"])
    mark, left, gap = 150, 64, 40
    x = left + mark + gap
    lead = "Hosting che si "
    lead_width = faces["sans"].width(lead, 34, tracking=-0.04)
    tag = "Pannello di hosting source-available".upper()
    tag_width = faces["mono"].width(tag, 11, tracking=0.15)
    parts = [
        f'<rect width="{width}" height="{height}" fill="{background}"/>',
        squares(width, height, c["grid-dark"] if dark else c["grid"]),
        f'<g transform="translate({left} {(height - mark) / 2:.2f}) scale({mark / 48:.4f})">'
        f'{tile(text, background)}</g>',
        path(text, faces["sans"].outline("cloudground", 92, x, 168, tracking=-0.045)[0]),
        path(text, faces["sans"].outline(lead, 34, x + 4, 222, tracking=-0.04)[0]),
        path(c["blue-dark"] if dark else c["blue"],
             faces["serif"].outline("misura.", 38, x + 4 + lead_width, 222, tracking=-0.02)[0]),
        path(c["muted-dark"] if dark else c["muted"],
             faces["mono"].outline(tag, 11, width - 28 - tag_width, height - 22, tracking=0.15)[0]),
    ]
    return svg(width, height, "".join(parts), LABEL)


def social_card(faces):
    width, height, margin = 1200, 630, 64
    c = COLOURS
    sans, serif, mono = faces["sans"], faces["serif"], faces["mono"]
    site = "CLOUDGROUND.IT"
    first_width = sans.width("che si ", 132, tracking=-0.045)
    badge = "SOURCE-AVAILABLE"
    badge_width = mono.width(badge, 12, tracking=0.15)
    badge_x = width - margin - badge_width - 20
    parts = [
        f'<rect width="{width}" height="{height}" fill="{c["paper"]}"/>',
        f'<g transform="translate({margin} {margin}) scale({56 / 48:.4f})">{tile(c["ink"], c["paper"])}</g>',
        path(c["ink"], sans.outline("cloudground", 30, margin + 70, margin + 38, tracking=-0.045)[0]),
        path(c["muted"], mono.outline(site, 13, width - margin - mono.width(site, 13, tracking=0.15),
                                      margin + 34, tracking=0.15)[0]),
        path(c["ink"], sans.outline("Hosting", 132, margin - 6, 330, tracking=-0.045)[0]),
        path(c["ink"], sans.outline("che si ", 132, margin - 6, 444, tracking=-0.045)[0]),
        path(c["blue"], serif.outline("misura.", 150, margin - 6 + first_width, 444, tracking=-0.02)[0]),
        f'<rect x="{margin}" y="{height - margin - 30}" width="{width - 2 * margin}" height="1" fill="{c["ink"]}"/>',
        path(c["footer"], mono.outline("UN COMANDO · UN CLIC PER SITO · BACKUP RIPRISTINATI PER PROVA",
                                       12, margin, height - margin, tracking=0.15)[0]),
        f'<rect x="{badge_x}" y="{height - margin - 17}" width="{badge_width + 20}" height="26" fill="{c["ink"]}"/>',
        path(c["paper"], mono.outline(badge, 12, badge_x + 10, height - margin, tracking=0.15)[0]),
    ]
    return svg(width, height, "".join(parts), LABEL)


def main(repository, font_folder):
    faces = {
        "sans": Face(font_folder / "Archivo.ttf", wght=560, wdth=118),
        "serif": Face(font_folder / "InstrumentSerif-Italic.ttf"),
        "mono": Face(font_folder / "GeistMono.ttf", wght=500),
    }
    outputs = {
        "docs/media/banner-light.svg": banner(faces, dark=False),
        "docs/media/banner-dark.svg": banner(faces, dark=True),
        "docs/media/og.svg": social_card(faces),
    }
    for name, content in outputs.items():
        target = repository / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content)
        print("wrote", name)


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    main(Path(sys.argv[1]), Path(sys.argv[2]))
