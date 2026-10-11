#!/usr/bin/env python3
"""Draw the Zex app icon as SVG: the isometric Z (same geometry as the in-app 3D mark)
with holographic faces, on a black dot-grid tile with a pink stippled sphere behind it.

    python3 assets/make_icons.py        # writes assets/icon.svg and assets/icon-small.svg

assets/build_icons.sh turns those into PNG, .icns (macOS) and .ico (Windows).
"""

import math
import pathlib
import random

HERE = pathlib.Path(__file__).resolve().parent

# The Z outline, extruded to a slab and turned to the classic isometric angle.
Z2D = [(0, 0), (2.4, 0), (2.4, 0.8), (0.95, 2.2), (2.4, 2.2), (2.4, 3), (0, 3), (0, 2.2), (1.45, 0.8), (0, 0.8)]
RX, RY = -0.6155, -0.7854


def rot(p):
    x, y, z = p
    c, s = math.cos(RY), math.sin(RY)
    x, z = x * c + z * s, -x * s + z * c
    c, s = math.cos(RX), math.sin(RX)
    y, z = y * c - z * s, y * s + z * c
    return x, y, z


def faces():
    front = [(x - 1.2, y - 1.5, 0.5) for x, y in Z2D]
    back = [(x - 1.2, y - 1.5, -0.5) for x, y in Z2D]
    out = [("front", front), ("back", back[::-1])]
    for i in range(len(Z2D)):
        j = (i + 1) % len(Z2D)
        out.append(("side", [front[i], front[j], back[j], back[i]]))
    res = []
    for kind, f in out:
        v = [rot(p) for p in f]
        a, b, c = v[0], v[1], v[2]
        n = ((b[1] - a[1]) * (c[2] - a[2]) - (b[2] - a[2]) * (c[1] - a[1]),
             (b[2] - a[2]) * (c[0] - a[0]) - (b[0] - a[0]) * (c[2] - a[2]),
             (b[0] - a[0]) * (c[1] - a[1]) - (b[1] - a[1]) * (c[0] - a[0]))
        ln = math.hypot(*n) or 1
        res.append((kind, v, tuple(x / ln for x in n), sum(p[2] for p in v) / len(v)))
    return sorted(res, key=lambda f: f[3])


def mark(cx, cy, size, stroke):
    fs = faces()
    pts = [p for _, v, _, _ in fs for p in v]
    minx, maxx = min(p[0] for p in pts), max(p[0] for p in pts)
    miny, maxy = min(p[1] for p in pts), max(p[1] for p in pts)
    k = size / max(maxx - minx, maxy - miny)
    ox, oy = cx - (minx + maxx) / 2 * k, cy - (miny + maxy) / 2 * k
    out = []
    for kind, v, n, _ in fs:
        if kind == "front":
            fill = "url(#holo)"
        elif n[1] < -0.3:
            fill = "url(#top)"
        elif n[0] > 0.3:
            fill = "url(#right)"
        else:
            fill = "url(#left)"
        d = "M" + "L".join(f"{ox + p[0] * k:.1f} {oy + p[1] * k:.1f}" for p in v) + "Z"
        out.append(f'<path d="{d}" fill="{fill}" stroke="#140a0e" stroke-width="{stroke}" stroke-linejoin="round"/>')
    return "\n  ".join(out)


def stipple(cx, cy, r, seed=3):
    """Dots on a sphere lit from the upper left, denser on the lit side (light dots on a dark tile)."""
    rnd = random.Random(seed)
    lx, ly, lz = -0.52, -0.62, 0.59
    dots = []
    step = r / 46
    y = -r
    while y <= r:
        x = -r
        while x <= r:
            jx, jy = x + (rnd.random() - 0.5) * step, y + (rnd.random() - 0.5) * step
            d2 = (jx * jx + jy * jy) / (r * r)
            if d2 <= 1:
                nz = math.sqrt(1 - d2)
                lit = min(1.0, max(0.0, jx / r * lx + jy / r * ly + nz * lz))
                if rnd.random() < lit ** 1.4 * 0.95 + 0.03:
                    dots.append(f'<rect x="{cx + jx:.1f}" y="{cy + jy:.1f}" width="{step * 0.62:.1f}" height="{step * 0.62:.1f}"/>')
            x += step
        y += step
    return "".join(dots)


DEFS = """<defs>
  <linearGradient id="holo" x1="0" y1="0" x2="1" y2="1">
    <stop offset="0" stop-color="#9be7ff"/><stop offset=".3" stop-color="#f5a8ff"/><stop offset=".6" stop-color="#fff2a1"/><stop offset="1" stop-color="#a8f5c8"/>
  </linearGradient>
  <linearGradient id="top" x1="0" y1="0" x2="1" y2="0"><stop offset="0" stop-color="#fff6c4"/><stop offset="1" stop-color="#ffd2f2"/></linearGradient>
  <linearGradient id="right" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#d7c2ff"/><stop offset="1" stop-color="#9fd8ff"/></linearGradient>
  <linearGradient id="left" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#b9f7d9"/><stop offset="1" stop-color="#8fe3f0"/></linearGradient>
  <linearGradient id="tile" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#1c1c1e"/><stop offset="1" stop-color="#050505"/></linearGradient>
  <pattern id="dots" width="16" height="16" patternUnits="userSpaceOnUse"><circle cx="8" cy="8" r="1.9" fill="#ffffff" opacity=".13"/></pattern>
  <filter id="shadow" x="-20%" y="-20%" width="140%" height="140%"><feDropShadow dx="0" dy="14" stdDeviation="18" flood-color="#000" flood-opacity=".28"/></filter>
</defs>"""


def icon(small=False):
    # macOS layout: an 824px tile centred on a 1024px canvas, with its own shadow.
    x0, size, radius = 100, 824, 186
    tile = f'<rect x="{x0}" y="{x0}" width="{size}" height="{size}" rx="{radius}" fill="url(#tile)"/>'
    clip = f'<clipPath id="clip"><rect x="{x0}" y="{x0}" width="{size}" height="{size}" rx="{radius}"/></clipPath>'
    inner = [tile]
    if not small:
        inner.append(f'<rect x="{x0}" y="{x0}" width="{size}" height="{size}" fill="url(#dots)"/>')
        inner.append(f'<g fill="#f38aa2" opacity=".9">{stipple(735, 760, 250)}</g>')
        inner.append(f'<rect x="{x0 + 22}" y="{x0 + 22}" width="{size - 44}" height="{size - 44}" rx="{radius - 20}" fill="none" stroke="#ffffff" stroke-width="4" opacity=".16"/>')
    body = "\n  ".join(inner)
    z = mark(512, 500, 560 if not small else 640, 14 if not small else 22)
    return f"""<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1024 1024" width="1024" height="1024">
{DEFS}
<defs>{clip}</defs>
<g filter="url(#shadow)"><rect x="{x0}" y="{x0}" width="{size}" height="{size}" rx="{radius}" fill="#0a0a0a"/></g>
<g clip-path="url(#clip)">
  {body}
  {z}
</g>
</svg>
"""


if __name__ == "__main__":
    (HERE / "icon.svg").write_text(icon())
    (HERE / "icon-small.svg").write_text(icon(small=True))
    print("wrote assets/icon.svg and assets/icon-small.svg")
