# Regenerates assets/cli.svg. Usage: python3 assets/make-terminals.py assets
# The output is the CLI's real format; the targets are generic examples.
import sys, html
C = dict(t="#C9D1D9", d="#8B949E", g="#3FB950", y="#E3B341", r="#FF7B72", c="#79C0FF", m="#D2A8FF", p="#2DD4BF", w="#F0F6FC")
FONT = 'ui-monospace, SFMono-Regular, Menlo, Consolas, "DejaVu Sans Mono", monospace'

def render(path, title, lines, width=1000, desc=""):
    lh, top = 23, 70
    h = top + lh * len(lines) + 26
    out = [f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {width} {h}" width="{width}" height="{h}" role="img" aria-labelledby="t d">',
           f'<title id="t">{html.escape(title)}</title><desc id="d">{html.escape(desc)}</desc>',
           f'<rect width="{width}" height="{h}" rx="14" fill="#0D1117" stroke="#30363D"/>',
           f'<path d="M0 14 a14 14 0 0 1 14 -14 h{width-28} a14 14 0 0 1 14 14 v26 h-{width} z" fill="#161B22"/>',
           '<circle cx="24" cy="20" r="6.5" fill="#FF5F57"/><circle cx="46" cy="20" r="6.5" fill="#FEBC2E"/><circle cx="68" cy="20" r="6.5" fill="#28C840"/>',
           f'<text x="{width/2}" y="25" text-anchor="middle" fill="#8B949E" font-family="{html.escape(FONT)}" font-size="13">{html.escape(title)}</text>']
    for i, segs in enumerate(lines):
        y = top + i * lh
        # Columns line up by padding with spaces, so they follow the font's
        # real advance instead of an assumed glyph width.
        parts = []
        for c, s in segs:
            fw = ' font-weight="700"' if c in "rgyp" else ''
            parts.append(f'<tspan fill="{C[c]}"{fw}>{html.escape(s)}</tspan>')
        out.append(f'<text x="22" y="{y}" xml:space="preserve" font-family="{html.escape(FONT)}" font-size="14.5">{"".join(parts)}</text>')
    out.append('</svg>')
    open(path, 'w').write('\n'.join(out) + '\n')

def cmd(s, note=""):
    return [("p", "$ "), ("t", s + ("  " if note else ""))] + ([("d", "# " + note)] if note else [])

def tgt(id, kind, label):
    return [("w", f"{id:<41}"), ("c", f"{kind:<11}"), ("t", label)]

A = sys.argv[1]
render(f"{A}/cli.svg", "loupe", [
    cmd("brew install bmcreations/tap/loupe && brew services start loupe"),
    [],
    cmd("loupe targets | jq -r '.[] | [.id, .kind, .label] | @tsv' | column -ts $'\\t'"),
    tgt("39021FDH2000QZ", "device", "Pixel 9"),
    tgt("avd:Pixel_10", "emulator", "Pixel 10 (emulator)"),
    tgt("avd:Tablet_API_36", "emulator", "Tablet API 36 (emulator)"),
    tgt("sim:5C0E6A1B-2F7D-4E8C-9B3A-1D2E3F4A5B6C", "simulator", "iPhone 17"),
    [],
    cmd("loupe ui avd:Pixel_10 | jq -c '.. | objects | select(.text == \"Sign in\") | .center'"),
    [("t", "[540,1200]")],
    [],
    cmd("loupe tap avd:Pixel_10 540 1200", "claims the device, then taps"),
    [("g", '{"ok":true}')],
    cmd("loupe screenshot avd:Pixel_10 -o signed-in.png"),
    [],
    cmd("loupe tap avd:Pixel_10 540 1200", "while you use it in the browser"),
    [("r", "409: a person is using this device; retry in 2.4s (code humanHeld, retry after 2400ms)")],
], width=1080, desc="Example session: install with Homebrew, list targets, find a button in the accessibility tree, tap its center, take a screenshot, and get refused while a person is using the device.")
print("ok")
