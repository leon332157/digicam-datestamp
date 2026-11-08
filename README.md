# digicam-datestamp

Burn a retro digicam date stamp onto photos, sourced from each photo's EXIF
capture time (`DateTimeOriginal`, with fallbacks). Two implementations, same
flags:

- `digicam_datestamp.py` - Python, needs Pillow.
- `digicam_datestamp.go` - single file, stdlib only, `go build`.

## Styles

`--style` picks the camera look:

| style | look |
| --- | --- |
| `canon` (default) | PowerShot amber digits, thin dark outline, slash-separated |
| `sony` | Cyber-shot Write Date, small thin red-orange, space-separated |
| `nikon` | Coolpix Print Date, thin orange, dot-separated |
| `classic` | bright red date+time stamp (`YYYY/MM/DD HH:MM:SS`), no outline - Python only |

| canon | sony |
| --- | --- |
| ![canon](samples/sample_canon.jpg) | ![sony](samples/sample_sony.jpg) |

| nikon | classic |
| --- | --- |
| ![nikon](samples/sample_nikon.jpg) | ![classic](samples/sample_classic.jpg) |

The Go version has `canon`, `sony` and `nikon` only; `classic` is Python-only.

## Usage

Single file:

    python3 digicam_datestamp.py photo.jpg stamped.jpg
    python3 digicam_datestamp.py photo.jpg stamped.jpg --style sony

Batch (files and/or directories, original filenames kept, JPEG/PNG,
non-recursive):

    python3 digicam_datestamp.py photos/ extra.jpg -o stamped/

Go:

    go build digicam_datestamp.go
    ./digicam_datestamp photos/ -o stamped/

## Options

    -o, --output-dir DIR   batch output directory (keeps original filenames)
    --style STYLE          canon | sony | nikon | classic (Python only)
    --color COLOR          hex like '#F5A81C' or a color name
    --size FRACTION        stamp height as a fraction of image height
    --size-px PX           stamp height in pixels (overrides --size)
    --position CORNER      top-left | top-right | bottom-left | bottom-right
    --padding PX           margin from the corner (default 2.5% of height)
    --format FMT           strftime format for the stamp text
    --date 'YYYY:MM:DD HH:MM:SS'   override the EXIF date
    --outline FRACTION     dark outline width, 0 disables
    --opacity 0-100
    --shadow / --no-shadow subtle dark offset behind the stamp
    --output-format FMT    force output format (default: same as input)

## Realism pass

On by default: the stamp is softened, blended with sensor-like grain, and the
frame is JPEG re-encoded so the stamp looks burned in by the camera rather
than composited on top. Disable with `--no-realism`; tune with
`--jpeg-quality`, `--soften`, `--grain`.

## Credits

Built by [@leon332157](https://github.com/leon332157) with
Instinct as a contributor.
