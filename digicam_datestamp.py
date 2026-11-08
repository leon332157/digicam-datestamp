#!/usr/bin/env python3
"""
digicam_datestamp.py - Stamp a retro digicam-style date code onto photos.

Reads the capture time from EXIF (DateTimeOriginal, falling back to
DateTimeDigitized, then DateTime, then the file's mtime) and renders the
traditional Canon PowerShot stamp (default), or the Sony Cyber-shot /
Nikon Coolpix looks via --style: tall, narrow, rounded-stroke digits,
solid amber fill with a thin dark outline, bottom right - the look late
2000s compact cameras burned into the corner of every shot. --style
classic is the other common digicam stamp: solid bright red, no
outline, full date AND time to the second (YYYY/MM/DD HH:MM:SS).

A realism pass is on by default and is what makes the stamp look burned
in by the camera instead of composited in post. On a real digicam the
stamp is drawn onto the sensor image *before* JPEG encoding, so the
glyphs carry the same artifacts as the rest of the photo: DCT ringing
("mosquito noise") along the edges, chroma bleed from 4:2:0 subsampling
on the amber/outline boundary, the imaging pipeline's slight softness,
and sensor noise texture. The pass reproduces that with three tunable
steps: --soften (gaussian blur on the stamp), --grain (noise over the
stamped region), and a final JPEG round-trip of the whole frame at
--jpeg-quality with 4:2:0 subsampling. --no-realism disables all of it.

Requires: Pillow (pip install pillow)

Examples:
    python3 digicam_datestamp.py photo.jpg stamped.jpg
    python3 digicam_datestamp.py . -o stamped/          # every JPEG/PNG here
    python3 digicam_datestamp.py a.jpg b.png photos/ -o stamped/
    python3 digicam_datestamp.py photo.jpg stamped.jpg \
        --color '#FFCC00' --size 0.05 --position bottom-left
    python3 digicam_datestamp.py photo.jpg stamped.jpg --style sony
    python3 digicam_datestamp.py photo.jpg stamped.jpg --style classic
    python3 digicam_datestamp.py photo.jpg stamped.jpg --no-realism
    python3 digicam_datestamp.py photo.jpg stamped.jpg \
        --date '2004:07:15 18:32:11' --format '%Y-%m-%d %H:%M'
"""

import argparse
import io
import os
import sys
from datetime import datetime

from PIL import Image, ImageChops, ImageColor, ImageDraw, ImageFilter

# --- EXIF tags ----------------------------------------------------------
EXIF_IFD_POINTER = 0x8769
DATETIME_ORIGINAL = 0x9003   # capture time, lives in the EXIF sub-IFD
DATETIME_DIGITIZED = 0x9004  # digitization time, EXIF sub-IFD
DATETIME_TAG = 0x0132        # file-change time, main IFD


# --- Canon PowerShot-style stroke font -----------------------------------
# Canon's on-camera stamp is a tall, condensed, rounded-stroke design
# (measured off real PowerShot stamps: glyph aspect ~0.5, stroke ~15% of
# glyph height, thin dark outline). Each glyph is drawn as strokes on a
# 6x12 unit grid, scaled up, and rendered supersampled so edges stay
# smooth. Glyph elements are ("poly", [(x, y), ...]) polylines with round
# caps/joins, ("rrect", (x0, y0, x1, y1, r)) rounded-rectangle outlines,
# and ("dot", (x, y)) round dots.
CANON_W, CANON_H = 6, 12
CANON_SPACING = 1.6  # letter spacing in glyph cells
CANON_GLYPHS = {
    "0": [("rrect", (1.0, 0.8, 5.0, 11.2, 1.6))],
    "1": [("poly", [(3.0, 0.8), (3.0, 11.2)])],
    "2": [("poly", [(1.0, 3.2), (1.1, 1.9), (2.2, 0.9), (3.9, 0.9),
                    (5.0, 2.1), (5.0, 4.3), (1.0, 11.2), (5.0, 11.2)])],
    "3": [("poly", [(0.9, 0.9), (3.8, 0.9), (4.9, 2.0), (4.9, 4.0),
                    (3.9, 5.0), (2.4, 5.0)]),
          ("poly", [(3.9, 5.0), (4.9, 6.0), (4.9, 10.0), (3.9, 11.1),
                    (0.9, 11.1)])],
    "4": [("poly", [(1.5, 0.8), (1.5, 6.2), (5.0, 6.2)]),
          ("poly", [(4.3, 0.8), (4.3, 11.2)])],
    "5": [("poly", [(4.9, 0.9), (1.3, 0.9), (1.3, 5.0), (3.8, 5.0),
                    (4.9, 6.1), (4.9, 9.9), (3.8, 11.1), (1.1, 11.1)])],
    "6": [("poly", [(4.7, 1.1), (2.6, 0.9), (1.2, 2.3), (1.2, 9.9),
                    (2.3, 11.1), (3.9, 11.1), (5.0, 9.9), (5.0, 6.7),
                    (3.9, 5.6), (1.6, 5.6)])],
    "7": [("poly", [(1.0, 0.9), (5.0, 0.9), (2.7, 11.2)])],
    "8": [("rrect", (1.0, 0.8, 5.0, 11.2, 1.6)),
          ("poly", [(1.4, 6.0), (4.6, 6.0)])],
    "9": [("rrect", (1.0, 0.8, 5.0, 6.6, 1.6)),
          ("poly", [(5.0, 3.7), (5.0, 11.2)])],
    "/": [("poly", [(1.2, 11.2), (4.8, 0.8)])],
    "-": [("poly", [(1.3, 6.0), (4.7, 6.0)])],
    ".": [("dot", (3.0, 10.3))],
    ":": [("dot", (3.0, 3.6)), ("dot", (3.0, 8.4))],
    " ": [],
    "A": [("poly", [(1.0, 11.2), (3.0, 0.8), (5.0, 11.2)]),
          ("poly", [(1.9, 7.6), (4.1, 7.6)])],
    "M": [("poly", [(1.0, 11.2), (1.0, 0.8), (3.0, 4.8), (5.0, 0.8),
                    (5.0, 11.2)])],
    "P": [("poly", [(1.0, 11.2), (1.0, 0.9), (3.9, 0.9), (4.9, 1.9),
                    (4.9, 4.5), (3.9, 5.5), (1.0, 5.5)])],
}


def get_capture_time(path, img):
    """Return (datetime, source_description) for the photo's capture time."""
    exif = img.getexif()
    try:
        exif_ifd = exif.get_ifd(EXIF_IFD_POINTER)
    except Exception:
        exif_ifd = {}
    candidates = [
        (exif_ifd.get(DATETIME_ORIGINAL), "EXIF DateTimeOriginal"),
        (exif_ifd.get(DATETIME_DIGITIZED), "EXIF DateTimeDigitized"),
        (exif.get(DATETIME_TAG), "EXIF DateTime"),
    ]
    for value, source in candidates:
        if not value:
            continue
        try:
            return datetime.strptime(str(value).strip(), "%Y:%m:%d %H:%M:%S"), source
        except ValueError:
            pass  # malformed tag; fall through to the next source
    mtime = datetime.fromtimestamp(os.path.getmtime(path))
    return mtime, "file modification time (no usable EXIF date found)"


def parse_color(value):
    try:
        return ImageColor.getrgb(value)
    except ValueError:
        raise argparse.ArgumentTypeError(
            f"unrecognized color {value!r} (use a hex value like '#F5A81C' "
            "or a Pillow color name like 'orange')"
        )


# --- Camera stamp styles ----------------------------------------------------
# Each style sets the color, size, outline, stroke weight and date format of
# one manufacturer's digicam stamp (grounded in real stamped photos and
# manuals). Explicit --color/--size/--outline/--format flags override the
# selected style's defaults.
STYLES = {
    # Canon PowerShot: tall condensed digits, amber with a thin dark
    # outline, slash-separated.
    "canon": dict(color="#F5A81C", size=0.035, outline=0.08, stroke=0.15,
                  format="%m/%d/%Y"),
    # Sony Cyber-shot "Write Date" (2011+): thin clean sans digits, solid
    # red-orange, no outline, space-separated, noticeably small.
    "sony": dict(color="#FA6100", size=0.030, outline=0.0, stroke=0.09,
                 format="%m %d %Y"),
    # Nikon Coolpix "Print Date": thin sans digits, solid orange-red, no
    # outline, dot-separated.
    "nikon": dict(color="#FE530E", size=0.055, outline=0.0, stroke=0.09,
                  format="%d . %m . %Y"),
    # Classic red digicam timestamp (measured off a real stamped photo,
    # 4608x3456): pure red rounded-stroke digits, no outline, full date
    # and time to the second, slash-separated date. Sits further off the
    # corner than Canon's and spaces its glyphs a little wider.
    "classic": dict(color="#FF0000", size=0.0335, outline=0.0, stroke=0.095,
                    format="%Y/%m/%d %H:%M:%S", padding=0.040, spacing=2.0),
}


# --- Canon stroke-font rendering -----------------------------------------

SS = 4  # supersample factor for smooth edges


def check_canon_coverage(text):
    missing = sorted({ch for ch in text if ch not in CANON_GLYPHS})
    if missing:
        print(
            "warning: the built-in font has no glyph for "
            + ", ".join(repr(c) for c in missing)
            + "; those characters are skipped.",
            file=sys.stderr,
        )


def measure_canon(text, pitch, spacing=CANON_SPACING):
    """Pixel size of the stamp. Spacing is in glyph cells (1 cell = pitch)."""
    cols = 0
    for ch in text:
        cols += (CANON_W if ch in CANON_GLYPHS else 0) + spacing
    if text:
        cols -= spacing
    return cols * pitch, CANON_H * pitch


def _draw_canon_pass(draw, x, y, text, pitch, stroke_w, color,
                     spacing=CANON_SPACING):
    """One pass of the canon font: every stroke at width stroke_w."""
    cap_r = stroke_w / 2.0
    cx = x
    for ch in text:
        elements = CANON_GLYPHS.get(ch)
        if elements is None:
            cx += spacing * pitch
            continue
        for kind, spec in elements:
            if kind == "poly":
                pts = [(cx + px * pitch, y + py * pitch) for px, py in spec]
                if len(pts) > 1:
                    draw.line(pts, fill=color, width=stroke_w, joint="curve")
                for ppx, ppy in (pts[0], pts[-1]):
                    draw.ellipse(
                        [ppx - cap_r, ppy - cap_r, ppx + cap_r, ppy + cap_r],
                        fill=color,
                    )
            elif kind == "rrect":
                x0, y0, x1, y1, r = spec
                half = stroke_w / 2.0
                draw.rounded_rectangle(
                    [cx + x0 * pitch - half, y + y0 * pitch - half,
                     cx + x1 * pitch + half, y + y1 * pitch + half],
                    radius=r * pitch,
                    outline=color,
                    width=stroke_w,
                )
            else:  # dot
                dpx, dpy = spec
                r = stroke_w * 0.62
                draw.ellipse(
                    [cx + dpx * pitch - r, y + dpy * pitch - r,
                     cx + dpx * pitch + r, y + dpy * pitch + r],
                    fill=color,
                )
        cx += (CANON_W + spacing) * pitch


def build_stamp_layer(text, args, size_px):
    """Render the stamp (fill + outline) on its own RGBA layer."""
    check_canon_coverage(text)
    rgb = args.color
    alpha = round(255 * args.opacity / 100)
    fg = rgb + (alpha,)
    outline_rgb = args.outline_color

    spacing = getattr(args, "spacing", CANON_SPACING)
    pitch = size_px / CANON_H                      # final px per grid unit
    stroke_w = max(2, round(size_px * args.stroke))  # style stroke weight
    outline_w = round(size_px * args.outline) if args.outline > 0 else 0
    text_w, text_h = measure_canon(text, pitch, spacing)
    margin = stroke_w // 2 + outline_w + 2
    shadow_off = max(1, round(pitch * 0.45)) if args.shadow else 0

    w_ss = round((round(text_w) + 2 * margin + shadow_off) * SS)
    h_ss = round((round(text_h) + 2 * margin + shadow_off) * SS)
    layer_ss = Image.new("RGBA", (w_ss, h_ss), (0, 0, 0, 0))
    d = ImageDraw.Draw(layer_ss)

    p_ss = pitch * SS
    sw_ss = stroke_w * SS
    mx, my = margin * SS, margin * SS
    if args.shadow:
        _draw_canon_pass(d, mx + shadow_off * SS, my + shadow_off * SS,
                         text, p_ss, sw_ss, (0, 0, 0, round(alpha * 0.6)),
                         spacing)
    if outline_w > 0:
        _draw_canon_pass(d, mx, my, text, p_ss, sw_ss + 2 * outline_w * SS,
                         outline_rgb + (alpha,), spacing)
    _draw_canon_pass(d, mx, my, text, p_ss, sw_ss, fg, spacing)

    return layer_ss.resize(
        (round(text_w) + 2 * margin + shadow_off,
         round(text_h) + 2 * margin + shadow_off),
        Image.LANCZOS,
    )


POSITIONS = {
    "top-left": lambda iw, ih, sw, sh, p: (p, p),
    "top-right": lambda iw, ih, sw, sh, p: (iw - sw - p, p),
    "bottom-left": lambda iw, ih, sw, sh, p: (p, ih - sh - p),
    "bottom-right": lambda iw, ih, sw, sh, p: (iw - sw - p, ih - sh - p),
}


def stamp_image(img, dt, args):
    text = dt.strftime(args.format)
    size_px = args.size_px
    if size_px is None:
        size_px = max(8, round(img.height * args.size))
    layer = build_stamp_layer(text, args, size_px)
    if args.realism and args.soften > 0:
        # The camera's imaging pipeline never produces mathematically crisp
        # edges; a whisper of blur keeps the stamp from looking composited.
        layer = layer.filter(ImageFilter.GaussianBlur(args.soften))
    style_padding = getattr(args, "style_padding", 0.025)
    padding = args.padding if args.padding is not None else round(img.height * style_padding)
    x, y = POSITIONS[args.position](img.width, img.height, layer.width, layer.height, padding)

    base = img.convert("RGBA")
    base.alpha_composite(layer, (x, y))
    return base, text, layer, (x, y)


# --- Realism pass ----------------------------------------------------------
# A crisp stamp on a JPEG photo reads as "added in post" because a real
# digicam burns the stamp in *before* encoding: the glyphs end up with the
# same DCT ringing, 4:2:0 chroma bleed, softness and noise as the scene.
# These steps reproduce that pipeline.


def add_grain(img, layer, pos, sigma):
    """Blend sensor-style noise over the stamped area.

    Uses Pillow's gaussian noise generator; the noise is masked to the
    stamp footprint (dilated a little, so the JPEG ringing zone around the
    glyphs gets textured too) and added to the image with saturation.
    """
    alpha = layer.getchannel("A")
    footprint = alpha.filter(ImageFilter.MaxFilter(9)).point(lambda a: 255 if a else 0)
    mask = Image.new("L", img.size, 0)
    mask.paste(footprint, pos)
    noise = Image.effect_noise(img.size, sigma).convert("RGB")  # mean 128
    noisy = ImageChops.add(img, noise, scale=1.0, offset=-128)
    return Image.composite(noisy, img, mask)


def jpeg_roundtrip(img, quality):
    """Re-encode the frame the way the camera would have.

    Encoding the composited image as JPEG (4:2:0 chroma subsampling, the
    digicam default) and decoding it gives the stamp genuine DCT ringing
    and chroma bleed instead of simulated artifacts.
    """
    buf = io.BytesIO()
    img.convert("RGB").save(buf, "JPEG", quality=quality, subsampling=2)
    buf.seek(0)
    out = Image.open(buf)
    out.load()
    return out


# Extensions collected when a directory is given as input (non-recursive).
# Matches the formats both this script and its Go port can decode.
IMAGE_EXTS = {".jpg", ".jpeg", ".png"}
OUTPUT_FORMATS = {
    "jpg": (".jpg", "JPEG"),
    "png": (".png", "PNG"),
    "webp": (".webp", "WEBP"),
}


def output_path_for_format(path, output_format):
    """Apply an explicit output format to a file path, if requested."""
    if output_format == "same":
        return path
    suffix, _ = OUTPUT_FORMATS[output_format]
    root, _ = os.path.splitext(path)
    return root + suffix


def stamp_one(args, input_path, output_path):
    """Stamp one photo and write it to output_path."""
    img = Image.open(input_path)
    img.load()
    exif_bytes = img.info.get("exif")

    if args.date:
        dt = datetime.strptime(args.date, "%Y:%m:%d %H:%M:%S")
        source = "--date override"
    else:
        dt, source = get_capture_time(input_path, img)

    stamped, text, layer, pos = stamp_image(img, dt, args)

    if args.realism:
        out = stamped.convert("RGB")
        if args.grain > 0:
            out = add_grain(out, layer, pos, args.grain)
        out = jpeg_roundtrip(out, args.jpeg_quality)
    else:
        out = stamped

    ext = os.path.splitext(output_path)[1].lower()
    if not ext:
        raise ValueError(
            f"cannot determine output format from {output_path!r}; "
            "add a file extension or use --output-format"
        )
    save_kwargs = {}
    if ext in (".jpg", ".jpeg"):
        out = out.convert("RGB")
        # High quality so the final save doesn't smear the realism artifacts
        # a second time - they should come from the round-trip alone.
        save_kwargs["quality"] = 95
    if exif_bytes and ext in (".jpg", ".jpeg", ".png", ".webp"):
        save_kwargs["exif"] = exif_bytes  # keep the original metadata
    out.save(output_path, **save_kwargs)

    print(f"stamp: {text!r} ({source}) -> {output_path}")


def expand_inputs(p, inputs):
    """Resolve input files and directories into a flat list of image paths.

    Directories are scanned non-recursively for JPEG/PNG images, in
    filename order.
    """
    paths = []
    for item in inputs:
        if os.path.isdir(item):
            found = [
                os.path.join(item, name)
                for name in sorted(os.listdir(item))
                if os.path.splitext(name)[1].lower() in IMAGE_EXTS
                and os.path.isfile(os.path.join(item, name))
            ]
            if not found:
                p.error(f"no JPEG/PNG images found in directory {item!r}")
            paths.extend(found)
        elif os.path.isfile(item):
            paths.append(item)
        else:
            p.error(f"input not found: {item!r}")
    return paths


def main(argv=None):
    p = argparse.ArgumentParser(
        description="Stamp a digicam-style date code (Canon PowerShot by "
                    "default, Sony/Nikon via --style) onto photos, sourced "
                    "from each photo's EXIF capture time. Single file: pass "
                    "an input and an output. Batch: pass files and/or "
                    "directories with --output-dir; original filenames are "
                    "kept.",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    p.add_argument("paths", nargs="+", metavar="path",
                   help="input photo(s) followed by an output file, or just "
                        "input photo(s) when --output-dir is given. Inputs "
                        "may be image files and/or directories (directories "
                        "are scanned for JPEG/PNG images, non-recursive)")
    p.add_argument("-o", "--output-dir", default=None,
                   help="write outputs to this directory, keeping the "
                        "original filenames (created if missing)")
    p.add_argument("--output-format", choices=["same"] + sorted(OUTPUT_FORMATS),
                   default="same", metavar="FORMAT",
                   help="output image format; replaces the output filename "
                        "extension (default: same as the output path/input)")
    p.add_argument("--style", choices=sorted(STYLES), default="canon",
                   help="camera look: canon (PowerShot amber), sony "
                        "(Cyber-shot Write Date, red-orange), nikon "
                        "(Coolpix Print Date, dot-separated orange) or "
                        "classic (bright red date+time timestamp)")
    p.add_argument("--color", type=parse_color, default=None,
                   help="stamp color - hex like '#F5A81C' or a Pillow color "
                        "name (default: the style's brand color)")
    p.add_argument("--size", type=float, default=None,
                   help="stamp height as a fraction of image height "
                        "(default: the style's proportion)")
    p.add_argument("--size-px", type=int, default=None,
                   help="stamp height in pixels (overrides --size)")
    p.add_argument("--position", choices=sorted(POSITIONS), default="bottom-right")
    p.add_argument("--padding", type=int, default=None,
                   help="margin from the chosen corner in pixels "
                        "(default: 2.5%% of image height)")
    p.add_argument("--format", default=None,
                   help="strftime format for the stamp text (default: the "
                        "style's brand format)")
    p.add_argument("--date", default=None,
                   help="override the EXIF date, e.g. '2004:07:15 18:32:11'")
    p.add_argument("--outline", type=float, default=None,
                   help="dark outline width as a fraction of stamp height, "
                        "0 disables (default: the style's outline)")
    p.add_argument("--outline-color", type=parse_color,
                   default=parse_color("#141414"),
                   help="outline color")
    p.add_argument("--opacity", type=float, default=100.0,
                   help="stamp opacity, 0-100")
    p.add_argument("--shadow", dest="shadow", action="store_true",
                   help="draw a subtle dark offset behind the stamp for "
                        "legibility")
    p.add_argument("--no-shadow", dest="shadow", action="store_false")
    p.set_defaults(shadow=False)
    p.add_argument("--realism", dest="realism", action="store_true", default=True,
                   help="make the stamp look burned in by the camera: soften, "
                        "grain, and a JPEG re-encode of the frame (default: on)")
    p.add_argument("--no-realism", dest="realism", action="store_false",
                   help="skip the realism pass - crisp, clean stamp pixels")
    p.add_argument("--jpeg-quality", type=int, default=90, metavar="Q",
                   help="JPEG quality of the realism re-encode (1-100). Real "
                        "digicams shot around 85-95; lower = more visible "
                        "compression artifacts. Realism only.")
    p.add_argument("--soften", type=float, default=0.5,
                   help="gaussian blur radius in pixels applied to the stamp "
                        "before compositing, matching pipeline softness. "
                        "0 disables. Realism only.")
    p.add_argument("--grain", type=float, default=6.0,
                   help="noise strength (stddev) blended over the stamped "
                        "area to match sensor noise. 0 disables. Realism only.")
    args = p.parse_args(argv)

    style = STYLES[args.style]
    if args.color is None:
        args.color = parse_color(style["color"])
    if args.size is None:
        args.size = style["size"]
    if args.outline is None:
        args.outline = style["outline"]
    if args.format is None:
        args.format = style["format"]
    args.stroke = style["stroke"]
    args.spacing = style.get("spacing", CANON_SPACING)
    args.style_padding = style.get("padding", 0.025)

    # With --output-dir every positional is an input; otherwise the last
    # positional is the output file (the original single-file CLI shape).
    if args.output_dir:
        inputs_raw, output = args.paths, None
    else:
        if len(args.paths) < 2:
            p.error("no output: pass an output file or --output-dir DIR")
        inputs_raw, output = args.paths[:-1], args.paths[-1]

    inputs = expand_inputs(p, inputs_raw)

    if output:
        if len(inputs_raw) != 1 or os.path.isdir(inputs_raw[0]):
            p.error("an output file works with a single input file only; "
                    "use --output-dir for batch input")
        if os.path.isdir(output):
            output = os.path.join(output, os.path.basename(inputs[0]))
        output = output_path_for_format(output, args.output_format)
        stamp_one(args, inputs[0], output)
        return

    if not args.output_dir:
        p.error("no output: pass an output file or --output-dir DIR")

    os.makedirs(args.output_dir, exist_ok=True)
    outputs = [output_path_for_format(
                   os.path.join(args.output_dir, os.path.basename(f)),
                   args.output_format)
               for f in inputs]
    seen = {}
    for out_path in outputs:
        if out_path in seen:
            p.error(f"output name collision in batch mode: "
                    f"{os.path.basename(out_path)!r} comes from two inputs")
        seen[out_path] = True
    for in_path, out_path in zip(inputs, outputs):
        stamp_one(args, in_path, out_path)
    print(f"stamped {len(inputs)} photo(s) -> {args.output_dir}")


if __name__ == "__main__":
    main()
