// Package validate implements upstream image validation (SPEC §7): magic-byte
// sniffing, decode-bomb limits, full decode, and optional re-encoding that
// strips EXIF and hostile chunks.
package validate

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"

	"golang.org/x/image/webp"
)

// Reason codes recorded on negative entries (SPEC §7.6).
const (
	ReasonBadMagic   = "bad_magic"
	ReasonTooLarge   = "too_large"
	ReasonDecodeFail = "decode_failed"
)

// Options bounds validation.
type Options struct {
	MaxDimension int
	MaxPixels    int64
	Reencode     bool
	JPEGQuality  int
}

// Result describes a validated image.
type Result struct {
	ContentType string // sniffed + normalized MIME type
	Width       int
	Height      int

	// Data is the bytes to persist: the re-encoded payload when re-encoding
	// is enabled, otherwise the original input. Type mirrors ContentType.
	Data []byte
	// Reencoded reports whether Data differs from the input.
	Reencoded bool
}

// Sentinel validation failures; wrap via fmt.Errorf("%w") when context helps.
var (
	ErrBadMagic   = errors.New("unrecognized image magic bytes")
	ErrTooLarge   = errors.New("image exceeds size limits")
	ErrDecodeFail = errors.New("image failed to decode")
)

// sniff identifies the image format from leading magic bytes, ignoring any
// upstream Content-Type header (SPEC §7.1).
func sniff(d []byte) (string, bool) {
	switch {
	case len(d) >= 3 && d[0] == 0xFF && d[1] == 0xD8 && d[2] == 0xFF:
		return "image/jpeg", true
	case len(d) >= 8 && bytes.Equal(d[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "image/png", true
	case len(d) >= 6 && (bytes.Equal(d[:6], []byte("GIF87a")) || bytes.Equal(d[:6], []byte("GIF89a"))):
		return "image/gif", true
	case len(d) >= 12 && bytes.Equal(d[:4], []byte("RIFF")) && bytes.Equal(d[8:12], []byte("WEBP")):
		return "image/webp", true
	}
	return "", false
}

// Validate runs the full pipeline on data. On success it returns the
// normalized content type, dimensions, and the bytes to persist.
func Validate(data []byte, opts Options) (*Result, error) {
	ct, ok := sniff(data)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrBadMagic, ReasonBadMagic)
	}

	// Header-only decode: cheap defense against decode bombs before we ever
	// allocate a full pixel buffer (SPEC §7.2).
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrDecodeFail, ReasonDecodeFail, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 ||
		cfg.Width > opts.MaxDimension || cfg.Height > opts.MaxDimension ||
		int64(cfg.Width)*int64(cfg.Height) > opts.MaxPixels {
		return nil, fmt.Errorf("%w: %s: %dx%d", ErrTooLarge, ReasonTooLarge, cfg.Width, cfg.Height)
	}

	// Full decode confirms the file is a complete, decodable image (SPEC §7.3).
	img, err := decode(ct, data)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrDecodeFail, ReasonDecodeFail, err)
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > opts.MaxDimension || h > opts.MaxDimension || int64(w)*int64(h) > opts.MaxPixels {
		return nil, fmt.Errorf("%w: %s: decoded %dx%d", ErrTooLarge, ReasonTooLarge, w, h)
	}

	res := &Result{ContentType: ct, Width: w, Height: h, Data: data}

	if opts.Reencode {
		out, outCT, err := reencode(img, opts.JPEGQuality)
		if err != nil {
			return nil, fmt.Errorf("%w: reencode: %v", ErrDecodeFail, err)
		}
		res.Data = out
		res.ContentType = outCT
		res.Reencoded = true
	}
	return res, nil
}

func decode(ct string, data []byte) (image.Image, error) {
	switch ct {
	case "image/jpeg":
		return jpeg.Decode(bytes.NewReader(data))
	case "image/png":
		return png.Decode(bytes.NewReader(data))
	case "image/gif":
		// image/gif returns the first frame of an animation (SPEC §7.4).
		return gif.Decode(bytes.NewReader(data))
	case "image/webp":
		// x/image/webp decodes the first frame; animated WebP with the
		// extended header is rejected by the decoder, which we treat as a
		// validation failure.
		return webp.Decode(bytes.NewReader(data))
	}
	return nil, ErrBadMagic
}

// reencode normalizes any accepted input to JPEG (opaque) or PNG (has alpha),
// stripping EXIF and any exotic container metadata (SPEC §7.5).
func reencode(img image.Image, quality int) ([]byte, string, error) {
	if img.Bounds().Empty() {
		return nil, "", errors.New("empty image")
	}
	if isOpaque(img) {
		var buf bytes.Buffer
		if quality <= 0 || quality > 100 {
			quality = 85
		}
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), "image/jpeg", nil
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "image/png", nil
}

func isOpaque(img image.Image) bool {
	type opaque interface{ Opaque() bool }
	if o, ok := img.(opaque); ok {
		return o.Opaque()
	}
	return false
}
