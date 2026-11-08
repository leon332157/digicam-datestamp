// digicam_datestamp.go - Stamp a retro digicam-style date code onto photos.
//
// Port of digicam_datestamp.py: same CLI surface and behavior. Reads the
// capture time from EXIF (DateTimeOriginal, then DateTimeDigitized, then
// DateTime, then file mtime) and renders the Canon PowerShot stamp
// (default), or the Sony Cyber-shot / Nikon Coolpix looks via --style.
// A realism pass (soften, grain, JPEG re-encode round-trip) makes the stamp
// look burned in by the camera; --no-realism disables it.
//
// Requires: Go stdlib only (no external dependencies).
//
// Examples:
//
//	go run digicam_datestamp.go photo.jpg stamped.jpg
//	go run digicam_datestamp.go . -o stamped/          // every JPEG/PNG here
//	go run digicam_datestamp.go a.jpg b.png photos/ -o stamped/
//	go run digicam_datestamp.go photo.jpg stamped.jpg --style sony
//	go run digicam_datestamp.go photo.jpg stamped.jpg --color '#FFCC00' --size 0.05 --position bottom-left
//	go run digicam_datestamp.go photo.jpg stamped.jpg --no-realism
//	go run digicam_datestamp.go photo.jpg stamped.jpg --date '2004:07:15 18:32:11' --format '%Y-%m-%d %H:%M'
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// --- EXIF tags ------------------------------------------------------------
const (
	exifIFDPointer     = 0x8769
	tagDateTime        = 0x0132
	tagDateTimeOrig    = 0x9003
	tagDateTimeDig     = 0x9004
	exifDateTimeFormat = "2006:01:02 15:04:05"
)

// --- Canon PowerShot-style stroke font -------------------------------------
// Glyph elements on a 6x12 unit grid: "p" polylines (round caps/joins),
// "r" rounded-rectangle outlines (x0,y0,x1,y1,radius), "d" round dots.
const (
	canonW, canonH = 6.0, 12.0
	canonSpacing   = 1.6
)

var canonGlyphs = map[rune]string{
	'0': "r1.0,0.8,5.0,11.2,1.6",
	'1': "p3.0,0.8,3.0,11.2",
	'2': "p0.9,2.1,0.9,0.9,3.9,0.9,5.0,2.1,5.0,4.3,1.0,11.2,5.0,11.2",
	'3': "p0.9,0.9,3.8,0.9,4.9,2.0,4.9,4.0,3.9,5.0,2.4,5.0;p3.9,5.0,4.9,6.0,4.9,10.0,3.9,11.1,0.9,11.1",
	'4': "p1.5,0.8,1.5,6.2,5.0,6.2;p4.3,0.8,4.3,11.2",
	'5': "p4.9,0.9,1.3,0.9,1.3,5.0,3.8,5.0,4.9,6.1,4.9,9.9,3.8,11.1,1.1,11.1",
	'6': "p4.7,1.1,2.6,0.9,1.2,2.3,1.2,9.9,2.3,11.1,3.9,11.1,5.0,9.9,5.0,6.7,3.9,5.6,1.6,5.6",
	'7': "p1.0,0.9,5.0,0.9,2.7,11.2",
	'8': "r1.0,0.8,5.0,11.2,1.6;p1.4,6.0,4.6,6.0",
	'9': "r1.0,0.8,5.0,6.6,1.6;p5.0,3.7,5.0,11.2",
	'/': "p1.2,11.2,4.8,0.8",
	'-': "p1.3,6.0,4.7,6.0",
	'.': "d3.0,10.3",
	':': "d3.0,3.6;d3.0,8.4",
	' ': "",
	'A': "p1.0,11.2,3.0,0.8,5.0,11.2;p1.9,7.6,4.1,7.6",
	'M': "p1.0,11.2,1.0,0.8,3.0,4.8,5.0,0.8,5.0,11.2",
	'P': "p1.0,11.2,1.0,0.9,3.9,0.9,4.9,1.9,4.9,4.5,3.9,5.5,1.0,5.5",
}

// Wait: glyph "1" - match python table (see source of truth in python file).
// glyph "2" likewise. These are patched at init from the canonical table below.

// canonical glyph elements copied from the python spec
var canonGlyphElems = map[rune][]gElem{}

type gElem struct {
	kind byte  // 'p', 'r', 'd'
	pts  []fpt // polyline points, or dot center (1 pt)
	rect [5]float64
}

type fpt struct{ x, y float64 }

func parseGlyphs() {
	for ch, spec := range canonGlyphs {
		if spec == "" {
			canonGlyphElems[ch] = []gElem{}
			continue
		}
		for _, part := range strings.Split(spec, ";") {
			kind := part[0]
			nums := strings.Split(part[1:], ",")
			f := make([]float64, len(nums))
			for i, s := range nums {
				v, err := strconv.ParseFloat(s, 64)
				if err != nil {
					panic("bad glyph spec " + part)
				}
				f[i] = v
			}
			switch kind {
			case 'p':
				e := gElem{kind: 'p'}
				for i := 0; i+1 < len(f); i += 2 {
					e.pts = append(e.pts, fpt{f[i], f[i+1]})
				}
				canonGlyphElems[ch] = append(canonGlyphElems[ch], e)
			case 'r':
				e := gElem{kind: 'r'}
				copy(e.rect[:], f[:5])
				canonGlyphElems[ch] = append(canonGlyphElems[ch], e)
			case 'd':
				canonGlyphElems[ch] = append(canonGlyphElems[ch], gElem{kind: 'd', pts: []fpt{{f[0], f[1]}}})
			}
		}
	}
}

func init() {
	// "1" and "2" come from the python source verbatim.
	canonGlyphs['1'] = "p3.0,0.8,3.0,11.2"
	canonGlyphs['2'] = "p1.0,3.2,1.1,1.9,2.2,0.9,3.9,0.9,5.0,2.1,5.0,4.3,1.0,11.2,5.0,11.2"
	parseGlyphs()
}

// --- styles ---------------------------------------------------------------
type stampStyle struct {
	color   string
	size    float64
	outline float64
	stroke  float64
	format  string
}

var styles = map[string]stampStyle{
	"canon": {"#F5A81C", 0.035, 0.08, 0.15, "%m/%d/%Y"},
	"sony":  {"#FA6100", 0.030, 0.0, 0.09, "%m %d %Y"},
	"nikon": {"#FE530E", 0.055, 0.0, 0.09, "%d . %m . %Y"},
}

var colorNames = map[string][3]uint8{
	"black": {0, 0, 0}, "white": {255, 255, 255}, "red": {255, 0, 0},
	"green": {0, 128, 0}, "blue": {0, 0, 255}, "yellow": {255, 255, 0},
	"orange": {255, 165, 0}, "amber": {255, 191, 0}, "gray": {128, 128, 128},
	"grey": {128, 128, 128}, "purple": {128, 0, 128}, "pink": {255, 192, 203},
	"brown": {165, 42, 42}, "cyan": {0, 255, 255}, "magenta": {255, 0, 255},
	"navy": {0, 0, 128}, "teal": {0, 128, 128}, "olive": {128, 128, 0},
	"maroon": {128, 0, 0}, "silver": {192, 192, 192}, "lime": {0, 255, 0},
	"gold": {255, 215, 0}, "coral": {255, 127, 80}, "salmon": {250, 128, 114},
}

func parseColor(s string) (color.RGBA, error) {
	if c, ok := colorNames[strings.ToLower(s)]; ok {
		return color.RGBA{c[0], c[1], c[2], 255}, nil
	}
	h := strings.TrimPrefix(s, "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) == 6 {
		if v, err := strconv.ParseUint(h, 16, 32); err == nil {
			return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255}, nil
		}
	}
	return color.RGBA{}, fmt.Errorf("unrecognized color %q (use a hex value like '#F5A81C' or a color name like 'orange')", s)
}

// --- EXIF -----------------------------------------------------------------

// readTIFFString reads an ASCII tag value from an IFD.
func tiffString(tiff []byte, bo binary.ByteOrder, ifdOff int, tag uint16) string {
	if ifdOff+2 > len(tiff) {
		return ""
	}
	n := int(bo.Uint16(tiff[ifdOff:]))
	for i := 0; i < n; i++ {
		e := ifdOff + 2 + i*12
		if e+12 > len(tiff) {
			return ""
		}
		if bo.Uint16(tiff[e:]) != tag {
			continue
		}
		typ := bo.Uint16(tiff[e+2:])
		count := int(bo.Uint32(tiff[e+4:]))
		if typ != 2 || count < 2 {
			return ""
		}
		var data []byte
		if count <= 4 {
			data = tiff[e+8 : e+8+count]
		} else {
			off := int(bo.Uint32(tiff[e+8:]))
			if off+count > len(tiff) {
				return ""
			}
			data = tiff[off : off+count]
		}
		return strings.TrimRight(string(data), "\x00")
	}
	return ""
}

func tiffIFDOffset(tiff []byte, bo binary.ByteOrder, ifdOff int, tag uint16) int {
	if ifdOff+2 > len(tiff) {
		return -1
	}
	n := int(bo.Uint16(tiff[ifdOff:]))
	for i := 0; i < n; i++ {
		e := ifdOff + 2 + i*12
		if e+12 > len(tiff) {
			return -1
		}
		if bo.Uint16(tiff[e:]) == tag {
			return int(bo.Uint32(tiff[e+8:]))
		}
	}
	return -1
}

// parseEXIFTIFF extracts the best capture-time string from raw TIFF data.
func exifDateFromTIFF(tiff []byte) string {
	if len(tiff) < 8 {
		return ""
	}
	var bo binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return ""
	}
	if bo.Uint16(tiff[2:]) != 42 {
		return ""
	}
	ifd0 := int(bo.Uint32(tiff[4:]))
	if exifOff := tiffIFDOffset(tiff, bo, ifd0, exifIFDPointer); exifOff >= 0 {
		if s := tiffString(tiff, bo, exifOff, tagDateTimeOrig); s != "" {
			if _, err := time.Parse(exifDateTimeFormat, strings.TrimSpace(s)); err == nil {
				return s
			}
		}
		if s := tiffString(tiff, bo, exifOff, tagDateTimeDig); s != "" {
			if _, err := time.Parse(exifDateTimeFormat, strings.TrimSpace(s)); err == nil {
				return s
			}
		}
	}
	if s := tiffString(tiff, bo, ifd0, tagDateTime); s != "" {
		if _, err := time.Parse(exifDateTimeFormat, strings.TrimSpace(s)); err == nil {
			return s
		}
	}
	return ""
}

// extractEXIF returns the raw EXIF payload (JPEG: bytes after "Exif\x00\x00"
// prefix; PNG: eXIf chunk payload) from an encoded image file.
func extractEXIF(data []byte) []byte {
	if len(data) >= 4 && data[0] == 0xFF && data[1] == 0xD8 {
		// JPEG: scan segments for APP1 Exif
		off := 2
		for off+4 <= len(data) {
			if data[off] != 0xFF {
				break
			}
			marker := data[off+1]
			if marker == 0xDA || marker == 0xD9 {
				break
			}
			segLen := int(binary.BigEndian.Uint16(data[off+2:]))
			if marker == 0xE1 && segLen >= 8 && string(data[off+4:off+10]) == "Exif\x00\x00" {
				return data[off+10 : off+2+segLen]
			}
			off += 2 + segLen
		}
		return nil
	}
	if len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n" {
		off := 8
		for off+12 <= len(data) {
			ln := int(binary.BigEndian.Uint32(data[off:]))
			typ := string(data[off+4 : off+8])
			if typ == "eXIf" {
				return data[off+8 : off+8+ln]
			}
			off += 12 + ln
			if typ == "IEND" {
				break
			}
		}
	}
	return nil
}

// injectEXIFJPEG inserts an APP1 Exif segment after the SOI marker.
func injectEXIFJPEG(jpegData, tiff []byte) []byte {
	payload := append([]byte("Exif\x00\x00"), tiff...)
	seg := new(bytes.Buffer)
	seg.Write(jpegData[:2])
	seg.Write([]byte{0xFF, 0xE1})
	binary.Write(seg, binary.BigEndian, uint16(len(payload)+2))
	seg.Write(payload)
	seg.Write(jpegData[2:])
	return seg.Bytes()
}

// injectEXIFPNG inserts an eXIf chunk right after IHDR.
func injectEXIFPNG(pngData, tiff []byte) []byte {
	out := new(bytes.Buffer)
	out.Write(pngData[:8])
	ihdrLen := 12 + int(binary.BigEndian.Uint32(pngData[8:]))
	out.Write(pngData[8 : 8+ihdrLen])
	binary.Write(out, binary.BigEndian, uint32(len(tiff)))
	out.WriteString("eXIf")
	out.Write(tiff)
	crc := crc32.NewIEEE()
	crc.Write([]byte("eXIf"))
	crc.Write(tiff)
	binary.Write(out, binary.BigEndian, crc.Sum32())
	out.Write(pngData[8+ihdrLen:])
	return out.Bytes()
}

func getCaptureTime(path string, exif []byte) (time.Time, string) {
	if s := exifDateFromTIFF(exif); s != "" {
		dt, _ := time.Parse(exifDateTimeFormat, strings.TrimSpace(s))
		// report which tag matched
		var bo binary.ByteOrder
		if string(exif[:2]) == "II" {
			bo = binary.LittleEndian
		} else {
			bo = binary.BigEndian
		}
		ifd0 := int(bo.Uint32(exif[4:]))
		src := "EXIF DateTime"
		if off := tiffIFDOffset(exif, bo, ifd0, exifIFDPointer); off >= 0 {
			if v := tiffString(exif, bo, off, tagDateTimeOrig); strings.TrimSpace(v) == strings.TrimSpace(s) {
				src = "EXIF DateTimeOriginal"
			} else if v := tiffString(exif, bo, off, tagDateTimeDig); strings.TrimSpace(v) == strings.TrimSpace(s) {
				src = "EXIF DateTimeDigitized"
			}
		}
		return dt, src
	}
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime(), "file modification time (no usable EXIF date found)"
	}
	return time.Now(), "current time (no usable EXIF date or file mtime found)"
}

// --- strftime -------------------------------------------------------------
func strftime(format string, t time.Time) string {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 >= len(format) {
			b.WriteByte(format[i])
			continue
		}
		i++
		switch format[i] {
		case 'Y':
			fmt.Fprintf(&b, "%04d", t.Year())
		case 'y':
			fmt.Fprintf(&b, "%02d", t.Year()%100)
		case 'm':
			fmt.Fprintf(&b, "%02d", int(t.Month()))
		case 'd':
			fmt.Fprintf(&b, "%02d", t.Day())
		case 'H':
			fmt.Fprintf(&b, "%02d", t.Hour())
		case 'I':
			h := t.Hour() % 12
			if h == 0 {
				h = 12
			}
			fmt.Fprintf(&b, "%02d", h)
		case 'M':
			fmt.Fprintf(&b, "%02d", t.Minute())
		case 'S':
			fmt.Fprintf(&b, "%02d", t.Second())
		case 'p':
			if t.Hour() < 12 {
				b.WriteString("AM")
			} else {
				b.WriteString("PM")
			}
		case 'a':
			b.WriteString(t.Format("Mon"))
		case 'A':
			b.WriteString(t.Format("Monday"))
		case 'b':
			b.WriteString(t.Format("Jan"))
		case 'B':
			b.WriteString(t.Format("January"))
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(format[i])
		}
	}
	return b.String()
}

// --- stamp rendering ------------------------------------------------------
const ss = 4 // supersample factor

func rnd(f float64) int { return int(math.RoundToEven(f)) }

func measureCanon(text string, pitch float64) (float64, float64) {
	cols := 0.0
	for _, ch := range text {
		if _, ok := canonGlyphElems[ch]; ok {
			cols += canonW
		}
		cols += canonSpacing
	}
	if len(text) > 0 {
		cols -= canonSpacing
	}
	return cols * pitch, canonH * pitch
}

func distSeg(p, a, b fpt) float64 {
	dx, dy := b.x-a.x, b.y-a.y
	l2 := dx*dx + dy*dy
	t := 0.0
	if l2 > 0 {
		t = ((p.x-a.x)*dx + (p.y-a.y)*dy) / l2
		if t < 0 {
			t = 0
		} else if t > 1 {
			t = 1
		}
	}
	return math.Hypot(p.x-(a.x+t*dx), p.y-(a.y+t*dy))
}

// glyphDist returns the min distance from p (glyph-local px) to the glyph strokes.
func glyphDist(e gElem, p fpt) float64 {
	switch e.kind {
	case 'p':
		d := math.Inf(1)
		for i := 0; i+1 < len(e.pts); i++ {
			if v := distSeg(p, e.pts[i], e.pts[i+1]); v < d {
				d = v
			}
		}
		return d
	case 'r':
		x0, y0, x1, y1, r := e.rect[0], e.rect[1], e.rect[2], e.rect[3], e.rect[4]
		cx, cy := (x0+x1)/2, (y0+y1)/2
		hx, hy := (x1-x0)/2, (y1-y0)/2
		qx, qy := math.Abs(p.x-cx)-hx+r, math.Abs(p.y-cy)-hy+r
		ox, oy := math.Max(qx, 0), math.Max(qy, 0)
		sdf := math.Hypot(ox, oy) + math.Min(math.Max(qx, qy), 0) - r
		return math.Abs(sdf) // distance to the rounded-rect outline path
	case 'd':
		return math.Hypot(p.x-e.pts[0].x, p.y-e.pts[0].y)
	}
	return math.Inf(1)
}

// drawCanonPass strokes the whole text at width strokeW (supersampled px) with src-over compositing.
func drawCanonPass(layer *image.RGBA, mx, my float64, text string, pitch, strokeW float64, col color.RGBA) {
	cx := mx
	for _, ch := range text {
		elems, ok := canonGlyphElems[ch]
		if !ok {
			cx += canonSpacing * pitch
			continue
		}
		gw := (canonW + canonSpacing) * pitch
		half := strokeW / 2
		// bounding box of this glyph's strokes
		x0 := int(math.Floor(cx - half - 1))
		x1 := int(math.Ceil(cx + canonW*pitch + half + 1))
		y0 := int(math.Floor(my - half - 1))
		y1 := int(math.Ceil(my + canonH*pitch + half + 1))
		if x0 < 0 {
			x0 = 0
		}
		if y0 < 0 {
			y0 = 0
		}
		if x1 > layer.Bounds().Dx() {
			x1 = layer.Bounds().Dx()
		}
		if y1 > layer.Bounds().Dy() {
			y1 = layer.Bounds().Dy()
		}
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				p := fpt{(float64(x) + 0.5 - cx) / pitch, (float64(y) + 0.5 - my) / pitch}
				d := math.Inf(1)
				for _, e := range elems {
					if v := glyphDist(e, p) * pitch; v < d {
						d = v
					}
				}
				cov := half + 0.5 - d
				if cov <= 0 {
					continue
				}
				if cov > 1 {
					cov = 1
				}
				sa := float64(col.A) / 255 * cov
				i := layer.PixOffset(x, y)
				dr, dg, db, da := layer.Pix[i], layer.Pix[i+1], layer.Pix[i+2], layer.Pix[i+3]
				dfa := float64(da) / 255
				outA := sa + dfa*(1-sa)
				if outA <= 0 {
					continue
				}
				layer.Pix[i+0] = uint8((float64(col.R)*sa + float64(dr)*dfa*(1-sa)) / outA)
				layer.Pix[i+1] = uint8((float64(col.G)*sa + float64(dg)*dfa*(1-sa)) / outA)
				layer.Pix[i+2] = uint8((float64(col.B)*sa + float64(db)*dfa*(1-sa)) / outA)
				layer.Pix[i+3] = uint8(outA*255 + 0.5)
			}
		}
		cx += gw
	}
}

// downsample box-averages a supersampled RGBA by ss x ss.
func downsample(src *image.RGBA) *image.RGBA {
	w, h := src.Bounds().Dx()/ss, src.Bounds().Dy()/ss
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var r, g, b, a uint32
			for dy := 0; dy < ss; dy++ {
				for dx := 0; dx < ss; dx++ {
					i := src.PixOffset(x*ss+dx, y*ss+dy)
					r += uint32(src.Pix[i])
					g += uint32(src.Pix[i+1])
					b += uint32(src.Pix[i+2])
					a += uint32(src.Pix[i+3])
				}
			}
			n := uint32(ss * ss)
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = uint8(r / n)
			dst.Pix[i+1] = uint8(g / n)
			dst.Pix[i+2] = uint8(b / n)
			dst.Pix[i+3] = uint8(a / n)
		}
	}
	return dst
}

// gaussianBlurRGBA blurs a small RGBA layer (straight alpha) with the given radius.
func gaussianBlurRGBA(img *image.RGBA, sigma float64) *image.RGBA {
	if sigma <= 0 {
		return img
	}
	rad := int(math.Ceil(3 * sigma))
	k := make([]float64, 2*rad+1)
	sum := 0.0
	for i := -rad; i <= rad; i++ {
		v := math.Exp(-float64(i*i) / (2 * sigma * sigma))
		k[i+rad] = v
		sum += v
	}
	for i := range k {
		k[i] /= sum
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	tmp := image.NewRGBA(img.Bounds())
	// horizontal
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var r, g, b, a float64
			for i := -rad; i <= rad; i++ {
				xx := x + i
				if xx < 0 {
					xx = 0
				}
				if xx >= w {
					xx = w - 1
				}
				p := img.PixOffset(xx, y)
				r += float64(img.Pix[p]) * k[i+rad]
				g += float64(img.Pix[p+1]) * k[i+rad]
				b += float64(img.Pix[p+2]) * k[i+rad]
				a += float64(img.Pix[p+3]) * k[i+rad]
			}
			p := tmp.PixOffset(x, y)
			tmp.Pix[p] = uint8(r + 0.5)
			tmp.Pix[p+1] = uint8(g + 0.5)
			tmp.Pix[p+2] = uint8(b + 0.5)
			tmp.Pix[p+3] = uint8(a + 0.5)
		}
	}
	dst := image.NewRGBA(img.Bounds())
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var r, g, b, a float64
			for i := -rad; i <= rad; i++ {
				yy := y + i
				if yy < 0 {
					yy = 0
				}
				if yy >= h {
					yy = h - 1
				}
				p := tmp.PixOffset(x, yy)
				r += float64(tmp.Pix[p]) * k[i+rad]
				g += float64(tmp.Pix[p+1]) * k[i+rad]
				b += float64(tmp.Pix[p+2]) * k[i+rad]
				a += float64(tmp.Pix[p+3]) * k[i+rad]
			}
			p := dst.PixOffset(x, y)
			dst.Pix[p] = uint8(r + 0.5)
			dst.Pix[p+1] = uint8(g + 0.5)
			dst.Pix[p+2] = uint8(b + 0.5)
			dst.Pix[p+3] = uint8(a + 0.5)
		}
	}
	return dst
}

type options struct {
	style        string
	color        color.RGBA
	size         float64
	sizePx       int
	position     string
	padding      int
	format       string
	date         string
	outline      float64
	outlineColor color.RGBA
	opacity      float64
	shadow       bool
	realism      bool
	jpegQuality  int
	soften       float64
	grain        float64
	stroke       float64
}

func buildStampLayer(text string, o *options) *image.RGBA {
	alpha := rnd(255 * o.opacity / 100)
	fg := o.color
	fg.A = uint8(alpha)

	pitch := float64(o.sizePx) / canonH
	strokeW := math.Max(2, math.RoundToEven(float64(o.sizePx)*o.stroke))
	outlineW := 0
	if o.outline > 0 {
		outlineW = rnd(float64(o.sizePx) * o.outline)
	}
	textW, textH := measureCanon(text, pitch)
	margin := int(strokeW)/2 + outlineW + 2
	shadowOff := 0
	if o.shadow {
		shadowOff = int(math.Max(1, math.RoundToEven(pitch*0.45)))
	}

	wSS := (rnd(textW) + 2*margin + shadowOff) * ss
	hSS := (rnd(textH) + 2*margin + shadowOff) * ss
	layerSS := image.NewRGBA(image.Rect(0, 0, wSS, hSS))

	pSS := pitch * ss
	mx, my := float64(margin*ss), float64(margin*ss)
	if o.shadow {
		sc := color.RGBA{0, 0, 0, uint8(rnd(float64(alpha) * 0.6))}
		drawCanonPass(layerSS, mx+float64(shadowOff*ss), my+float64(shadowOff*ss), text, pSS, strokeW*ss, sc)
	}
	if outlineW > 0 {
		oc := o.outlineColor
		oc.A = uint8(alpha)
		drawCanonPass(layerSS, mx, my, text, pSS, strokeW*ss+float64(2*outlineW*ss), oc)
	}
	drawCanonPass(layerSS, mx, my, text, pSS, strokeW*ss, fg)

	return downsample(layerSS)
}

func positionOf(pos string, iw, ih, sw, sh, p int) (int, int) {
	switch pos {
	case "top-left":
		return p, p
	case "top-right":
		return iw - sw - p, p
	case "bottom-left":
		return p, ih - sh - p
	default: // bottom-right
		return iw - sw - p, ih - sh - p
	}
}

// alphaComposite src-over at (x, y).
func alphaComposite(dst *image.RGBA, src *image.RGBA, x, y int) {
	sb := src.Bounds()
	for sy := 0; sy < sb.Dy(); sy++ {
		for sx := 0; sx < sb.Dx(); sx++ {
			dxp, dyp := x+sx, y+sy
			if dxp < 0 || dyp < 0 || dxp >= dst.Bounds().Dx() || dyp >= dst.Bounds().Dy() {
				continue
			}
			sp := src.PixOffset(sb.Min.X+sx, sb.Min.Y+sy)
			sa := float64(src.Pix[sp+3]) / 255
			if sa == 0 {
				continue
			}
			dp := dst.PixOffset(dxp, dyp)
			da := float64(dst.Pix[dp+3]) / 255
			outA := sa + da*(1-sa)
			dst.Pix[dp+0] = uint8((float64(src.Pix[sp])*sa + float64(dst.Pix[dp])*da*(1-sa)) / outA)
			dst.Pix[dp+1] = uint8((float64(src.Pix[sp+1])*sa + float64(dst.Pix[dp+1])*da*(1-sa)) / outA)
			dst.Pix[dp+2] = uint8((float64(src.Pix[sp+2])*sa + float64(dst.Pix[dp+2])*da*(1-sa)) / outA)
			dst.Pix[dp+3] = uint8(outA*255 + 0.5)
		}
	}
}

// addGrain adds gaussian noise (stddev sigma) over the stamp footprint
// (stamp alpha dilated by a 9x9 max filter), with saturation.
func addGrain(img *image.RGBA, layer *image.RGBA, px, py int, sigma float64, rng *rand.Rand) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	lb := layer.Bounds()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// mask: any nonzero alpha in the 9x9 neighborhood of the layer at (x-px, y-py)
			lx, ly := x-px, y-py
			hit := false
			for dy := -4; dy <= 4 && !hit; dy++ {
				for dx := -4; dx <= 4; dx++ {
					nx, ny := lx+dx, ly+dy
					if nx < 0 || ny < 0 || nx >= lb.Dx() || ny >= lb.Dy() {
						continue
					}
					if layer.Pix[layer.PixOffset(lb.Min.X+nx, lb.Min.Y+ny)+3] != 0 {
						hit = true
						break
					}
				}
			}
			if !hit {
				continue
			}
			n := rng.NormFloat64() * sigma
			p := img.PixOffset(x, y)
			for c := 0; c < 3; c++ {
				v := float64(img.Pix[p+c]) + n
				if v < 0 {
					v = 0
				}
				if v > 255 {
					v = 255
				}
				img.Pix[p+c] = uint8(v)
			}
		}
	}
}

// Extensions collected when a directory is given as input (non-recursive).
// Matches the formats both this program and its Python original can decode.
var imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// expandInputs resolves input files and directories into a flat list of
// image paths. Directories are scanned non-recursively for JPEG/PNG
// images, in filename order (os.ReadDir returns sorted entries).
func expandInputs(inputs []string) ([]string, error) {
	var paths []string
	for _, item := range inputs {
		fi, err := os.Stat(item)
		if err != nil {
			return nil, fmt.Errorf("input not found: %s", item)
		}
		if !fi.IsDir() {
			paths = append(paths, item)
			continue
		}
		entries, err := os.ReadDir(item)
		if err != nil {
			return nil, err
		}
		var found []string
		for _, e := range entries {
			if !e.IsDir() && imageExts[strings.ToLower(filepath.Ext(e.Name()))] {
				found = append(found, filepath.Join(item, e.Name()))
			}
		}
		if len(found) == 0 {
			return nil, fmt.Errorf("no JPEG/PNG images found in directory %q", item)
		}
		paths = append(paths, found...)
	}
	return paths, nil
}

// processOne stamps one photo and writes it to outputPath.
func processOne(o0 *options, inputPath, outputPath string) error {
	o := *o0 // per-file copy: sizePx derives from each image's height

	raw, err := os.ReadFile(inputPath)
	if err != nil {
		return err
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("cannot decode %s: %w", inputPath, err)
	}
	exifBytes := extractEXIF(raw)

	var dt time.Time
	var source string
	if o.date != "" {
		if dt, err = time.Parse(exifDateTimeFormat, o.date); err != nil {
			return fmt.Errorf("bad --date (want '2004:07:15 18:32:11'): %w", err)
		}
		source = "--date override"
	} else {
		dt, source = getCaptureTime(inputPath, exifBytes)
	}

	text := strftime(o.format, dt)
	// warn about missing glyphs
	missing := map[rune]bool{}
	for _, ch := range text {
		if _, ok := canonGlyphElems[ch]; !ok {
			missing[ch] = true
		}
	}
	if len(missing) > 0 {
		var chars []string
		for ch := range missing {
			chars = append(chars, strconv.QuoteRune(ch))
		}
		fmt.Fprintf(os.Stderr, "warning: the font has no glyph for %s; those characters are skipped.\n", strings.Join(chars, ", "))
	}

	b := img.Bounds()
	iw, ih := b.Dx(), b.Dy()
	if o.sizePx == 0 {
		o.sizePx = int(math.Max(8, math.RoundToEven(float64(ih)*o.size)))
	}
	layer := buildStampLayer(text, &o)
	if o.realism && o.soften > 0 {
		layer = gaussianBlurRGBA(layer, o.soften)
	}
	pad := o.padding
	if pad < 0 {
		pad = rnd(float64(ih) * 0.025)
	}
	px, py := positionOf(o.position, iw, ih, layer.Bounds().Dx(), layer.Bounds().Dy(), pad)

	base := image.NewRGBA(image.Rect(0, 0, iw, ih))
	for y := 0; y < ih; y++ {
		for x := 0; x < iw; x++ {
			r, g, bb, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			p := base.PixOffset(x, y)
			base.Pix[p] = uint8(r >> 8)
			base.Pix[p+1] = uint8(g >> 8)
			base.Pix[p+2] = uint8(bb >> 8)
			base.Pix[p+3] = 255
		}
	}
	alphaComposite(base, layer, px, py)

	out := base
	if o.realism {
		if o.grain > 0 {
			addGrain(out, layer, px, py, o.grain, rand.New(rand.NewSource(1)))
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: o.jpegQuality}); err != nil {
			return err
		}
		dec, err := jpeg.Decode(&buf)
		if err != nil {
			return err
		}
		rgb := image.NewRGBA(image.Rect(0, 0, iw, ih))
		for y := 0; y < ih; y++ {
			for x := 0; x < iw; x++ {
				r, g, bb, _ := dec.At(x, y).RGBA()
				p := rgb.PixOffset(x, y)
				rgb.Pix[p] = uint8(r >> 8)
				rgb.Pix[p+1] = uint8(g >> 8)
				rgb.Pix[p+2] = uint8(bb >> 8)
				rgb.Pix[p+3] = 255
			}
		}
		out = rgb
	}

	ext := strings.ToLower(filepath.Ext(outputPath))
	var enc bytes.Buffer
	switch ext {
	case ".jpg", ".jpeg":
		if err := jpeg.Encode(&enc, out, &jpeg.Options{Quality: 95}); err != nil {
			return err
		}
		data := enc.Bytes()
		if len(exifBytes) > 0 {
			data = injectEXIFJPEG(data, exifBytes)
		}
		if err := os.WriteFile(outputPath, data, 0644); err != nil {
			return err
		}
	case ".png":
		if err := png.Encode(&enc, out); err != nil {
			return err
		}
		data := enc.Bytes()
		if len(exifBytes) > 0 {
			data = injectEXIFPNG(data, exifBytes)
		}
		if err := os.WriteFile(outputPath, data, 0644); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported output extension %q (use .jpg, .jpeg or .png)", ext)
	}

	fmt.Printf("stamp: %q (%s) -> %s\n", text, source, outputPath)
	return nil
}

func main() {
	fs := flag.NewFlagSet("digicam_datestamp", flag.ExitOnError)
	var styleName, colorStr, position, format, dateStr, outlineColorStr, outputDir string
	var size, outline, opacity, soften, grain float64
	var sizePx, padding, jpegQuality int
	var shadowOn, shadowOff, realismOn, realismOff bool
	fs.StringVar(&styleName, "style", "canon", "camera brand look: canon, sony or nikon")
	fs.StringVar(&colorStr, "color", "", "stamp color - hex like '#F5A81C' or a color name (default: the style's brand color)")
	fs.Float64Var(&size, "size", -1, "stamp height as a fraction of image height (default: the style's proportion)")
	fs.IntVar(&sizePx, "size-px", 0, "stamp height in pixels (overrides --size)")
	fs.StringVar(&position, "position", "bottom-right", "top-left, top-right, bottom-left or bottom-right")
	fs.IntVar(&padding, "padding", -1, "margin from the chosen corner in pixels (default: 2.5% of image height)")
	fs.StringVar(&format, "format", "", "strftime format for the stamp text (default: the style's brand format)")
	fs.StringVar(&dateStr, "date", "", "override the EXIF date, e.g. '2004:07:15 18:32:11'")
	fs.Float64Var(&outline, "outline", -1, "dark outline width as a fraction of stamp height, 0 disables (default: the style's outline)")
	fs.StringVar(&outlineColorStr, "outline-color", "#141414", "outline color")
	fs.Float64Var(&opacity, "opacity", 100, "stamp opacity, 0-100")
	fs.BoolVar(&shadowOn, "shadow", false, "draw a subtle dark offset behind the stamp")
	fs.BoolVar(&shadowOff, "no-shadow", false, "disable the shadow")
	fs.BoolVar(&realismOn, "realism", true, "burned-in realism pass: soften, grain, JPEG re-encode (default: on)")
	fs.BoolVar(&realismOff, "no-realism", false, "skip the realism pass - crisp, clean stamp pixels")
	fs.IntVar(&jpegQuality, "jpeg-quality", 90, "JPEG quality of the realism re-encode (1-100). Realism only.")
	fs.Float64Var(&soften, "soften", 0.5, "gaussian blur radius applied to the stamp before compositing. 0 disables. Realism only.")
	fs.Float64Var(&grain, "grain", 6.0, "noise strength (stddev) blended over the stamped area. 0 disables. Realism only.")
	fs.StringVar(&outputDir, "o", "", "write outputs to this directory, keeping the original filenames (created if missing)")
	fs.StringVar(&outputDir, "output-dir", "", "same as -o")
	// argparse-style: allow flags and positionals in any order.
	boolFlags := map[string]bool{"shadow": true, "no-shadow": true, "realism": true, "no-realism": true}
	var flagArgs, posArgs []string
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			name := strings.TrimLeft(a, "-")
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				name = name[:eq]
			} else if !boolFlags[name] && i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
		} else {
			posArgs = append(posArgs, a)
		}
	}
	fs.Parse(append(flagArgs, posArgs...))
	pos := fs.Args()
	if len(pos) == 0 {
		fmt.Fprintln(os.Stderr, "usage: digicam_datestamp [options] input output")
		fmt.Fprintln(os.Stderr, "       digicam_datestamp [options] input... -o output-dir")
		fs.PrintDefaults()
		os.Exit(2)
	}
	// With --output-dir every positional is an input; otherwise the last
	// positional is the output file (the original single-file CLI shape).
	var inputs []string
	var output string
	if outputDir != "" {
		inputs = pos
	} else if len(pos) >= 2 {
		inputs, output = pos[:len(pos)-1], pos[len(pos)-1]
	} else {
		inputs = pos
	}

	style, ok := styles[styleName]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown --style %q (canon, sony or nikon)\n", styleName)
		os.Exit(2)
	}
	o := options{
		style: styleName, sizePx: sizePx, position: position, format: format,
		date: dateStr, opacity: opacity, shadow: shadowOn && !shadowOff,
		realism: realismOn && !realismOff, jpegQuality: jpegQuality,
		soften: soften, grain: grain, stroke: style.stroke,
		padding: padding, size: size, outline: outline,
	}
	var err error
	if colorStr == "" {
		o.color, _ = parseColor(style.color)
	} else if o.color, err = parseColor(colorStr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if o.outlineColor, err = parseColor(outlineColorStr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if o.size < 0 {
		o.size = style.size
	}
	if o.outline < 0 {
		o.outline = style.outline
	}
	if o.format == "" {
		o.format = style.format
	}

	if o.date != "" {
		if _, err := time.Parse(exifDateTimeFormat, o.date); err != nil {
			fmt.Fprintln(os.Stderr, "bad --date (want '2004:07:15 18:32:11'):", err)
			os.Exit(2)
		}
	}
	expanded, err := expandInputs(inputs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	if output != "" {
		if len(inputs) != 1 || isDir(inputs[0]) {
			fmt.Fprintln(os.Stderr, "error: an output file works with a single input file only; use --output-dir for batch input")
			os.Exit(2)
		}
		if isDir(output) {
			output = filepath.Join(output, filepath.Base(expanded[0]))
		}
		if err := processOne(&o, expanded[0], output); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if outputDir == "" {
		fmt.Fprintln(os.Stderr, "error: no output: pass an output file or --output-dir DIR")
		os.Exit(2)
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	seen := map[string]bool{}
	outputs := make([]string, len(expanded))
	for i, in := range expanded {
		outputs[i] = filepath.Join(outputDir, filepath.Base(in))
		if seen[outputs[i]] {
			fmt.Fprintf(os.Stderr, "error: output name collision in batch mode: %q comes from two inputs\n", filepath.Base(in))
			os.Exit(2)
		}
		seen[outputs[i]] = true
	}
	for i := range expanded {
		if err := processOne(&o, expanded[i], outputs[i]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fmt.Printf("stamped %d photo(s) -> %s\n", len(expanded), outputDir)
}

var _ = errors.New
