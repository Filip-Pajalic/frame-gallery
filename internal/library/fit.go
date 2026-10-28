package library

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/jpeg"
	"io"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	_ "image/gif"
	_ "image/png"
)

const (
	FitW, FitH     = 3840, 2160
	ThumbW, ThumbH = 480, 270
)

// Refused is an import that the full-screen rule turns away.
type Refused struct{ Reason string }

func (r *Refused) Error() string { return r.Reason }

// Loss is the share of the picture a centre crop to 16:9 removes.
func Loss(w, h int) float64 {
	ar, t := float64(w)/float64(h), float64(FitW)/float64(FitH)
	if ar < t {
		return 1 - ar/t
	}
	return 1 - t/ar
}

// Fitted is the result of the full-screen rule.
type Fitted struct {
	Width, Height int     // the original, after EXIF rotation
	Loss          float64 // share cropped away
	CropW, CropH  int     // the region kept, before scaling to 3840x2160
	JPEG          []byte  // 3840x2160, quality 92
	Thumb         []byte  // 480x270
}

// Fit applies the rule: centre-crop to 16:9 when that removes at most maxLoss
// of the picture, scale to 3840x2160; otherwise refuse with the reason.
func Fit(data []byte, maxLoss float64) (*Fitted, error) {
	src, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, &Refused{Reason: "not a picture this app can read (JPEG, PNG, WebP, TIFF or GIF)"}
	}
	if format == "jpeg" {
		src = orient(src, exifOrientation(data))
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	loss := Loss(w, h)
	if loss > maxLoss {
		shape := "taller"
		if float64(w)/float64(h) > float64(FitW)/float64(FitH) {
			shape = "wider"
		}
		return nil, &Refused{Reason: fmt.Sprintf("%dx%d is too %s for the screen: filling it would crop away %.0f %% (the limit is %.0f %%)",
			w, h, shape, loss*100, maxLoss*100)}
	}
	crop := b
	if float64(w)/float64(h) > float64(FitW)/float64(FitH) {
		nw := int(float64(h)*FitW/FitH + .5)
		crop = image.Rect(b.Min.X+(w-nw)/2, b.Min.Y, b.Min.X+(w-nw)/2+nw, b.Max.Y)
	} else {
		nh := int(float64(w)*FitH/FitW + .5)
		crop = image.Rect(b.Min.X, b.Min.Y+(h-nh)/2, b.Max.X, b.Min.Y+(h-nh)/2+nh)
	}
	// The kept region is 16:9 to within a pixel of rounding, and inside the picture.
	if skew := crop.Dx()*FitH - crop.Dy()*FitW; skew > FitW || skew < -FitW || !crop.In(b) || crop.Empty() {
		return nil, fmt.Errorf("internal: crop %v of %v is not 16:9", crop, b)
	}
	full := image.NewRGBA(image.Rect(0, 0, FitW, FitH))
	draw.CatmullRom.Scale(full, full.Bounds(), src, crop, draw.Src, nil)
	thumb := image.NewRGBA(image.Rect(0, 0, ThumbW, ThumbH))
	draw.CatmullRom.Scale(thumb, thumb.Bounds(), full, full.Bounds(), draw.Src, nil)

	f := &Fitted{Width: w, Height: h, Loss: loss, CropW: crop.Dx(), CropH: crop.Dy()}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, full, &jpeg.Options{Quality: 92}); err != nil {
		return nil, err
	}
	f.JPEG = append([]byte(nil), buf.Bytes()...)
	buf.Reset()
	if err := jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}
	f.Thumb = buf.Bytes()
	return f, nil
}

// exifOrientation reads the EXIF orientation tag of a JPEG (1 when absent).
func exifOrientation(data []byte) int {
	r := bytes.NewReader(data)
	var marker [2]byte
	if _, err := io.ReadFull(r, marker[:]); err != nil || marker != [2]byte{0xFF, 0xD8} {
		return 1
	}
	for {
		if _, err := io.ReadFull(r, marker[:]); err != nil || marker[0] != 0xFF {
			return 1
		}
		var n uint16
		if binary.Read(r, binary.BigEndian, &n) != nil || n < 2 {
			return 1
		}
		seg := make([]byte, n-2)
		if _, err := io.ReadFull(r, seg); err != nil {
			return 1
		}
		if marker[1] == 0xDA { // start of scan: no EXIF before the image data
			return 1
		}
		if marker[1] != 0xE1 || len(seg) < 14 || string(seg[:6]) != "Exif\x00\x00" {
			continue
		}
		t := seg[6:]
		var bo binary.ByteOrder = binary.BigEndian
		if string(t[:2]) == "II" {
			bo = binary.LittleEndian
		}
		off := int(bo.Uint32(t[4:8]))
		if off+2 > len(t) {
			return 1
		}
		count := int(bo.Uint16(t[off:]))
		for i := 0; i < count; i++ {
			e := off + 2 + i*12
			if e+12 > len(t) {
				return 1
			}
			if bo.Uint16(t[e:]) == 0x0112 {
				return int(bo.Uint16(t[e+8:]))
			}
		}
		return 1
	}
}

// orient turns a decoded image upright for EXIF orientations 2–8.
func orient(src image.Image, o int) image.Image {
	if o < 2 || o > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
