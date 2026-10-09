package validate

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
)

func mkJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mkPNG(t *testing.T, w, h int, alpha bool) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a := uint8(255)
			if alpha {
				a = uint8(x % 256)
			}
			img.SetNRGBA(x, y, color.NRGBA{200, 10, 10, a})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mkGIF(t *testing.T, frames int) []byte {
	t.Helper()
	var buf bytes.Buffer
	g := &gif.GIF{}
	for i := 0; i < frames; i++ {
		img := image.NewPaletted(image.Rect(0, 0, 8, 8), color.Palette{color.RGBA{uint8(i * 40), 0, 0, 255}, color.Black})
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				img.SetColorIndex(x, y, uint8((x+y+i)%2))
			}
		}
		g.Image = append(g.Image, img)
		g.Delay = append(g.Delay, 10)
	}
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSniffAndValidateJPEG(t *testing.T) {
	res, err := Validate(mkJPEG(t, 64, 64), Options{MaxDimension: 2048, MaxPixels: 2048 * 2048, Reencode: true, JPEGQuality: 85})
	if err != nil {
		t.Fatalf("valid jpeg rejected: %v", err)
	}
	if res.ContentType != "image/jpeg" || res.Width != 64 || res.Height != 64 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestBadMagicRejected(t *testing.T) {
	data := append([]byte("GARBAGE-NOT-AN-IMAGE---"), mkJPEG(t, 8, 8)...)
	_, err := Validate(data, Options{MaxDimension: 2048, MaxPixels: 2048 * 2048, Reencode: true})
	if err == nil {
		t.Fatal("garbage bytes must be rejected")
	}
}

// TestDecodeBombHeaderRejectsSpec exercises the SPEC §18 requirement: a
// header claiming huge dimensions must be rejected by DecodeConfig before any
// pixel buffer is allocated.
func TestDecodeBombHeaderRejectsSpec(t *testing.T) {
	// PNG header forging 40000x40000 (1.6 GP) with a truncated body.
	bomb := []byte{
		0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A,
		0, 0, 0, 13, 'I', 'H', 'D', 'R',
		0, 0, 0x9C, 0x40, // width 40000
		0, 0, 0x9C, 0x40, // height 40000
		8, 2, 0, 0, 0,
	}
	_, err := Validate(bomb, Options{MaxDimension: 2048, MaxPixels: 2048 * 2048})
	if err == nil {
		t.Fatal("decode bomb header must be rejected")
	}
}

func TestDimensionLimits(t *testing.T) {
	big := mkJPEG(t, 300, 300)
	if _, err := Validate(big, Options{MaxDimension: 200, MaxPixels: 2048 * 2048}); err == nil {
		t.Fatal("over-dimension image must be rejected")
	}
	// Pixel-count cap: 2048x1 fits dims but many small frames would not; use
	// dims under limit but pixels over a tiny cap.
	if _, err := Validate(big, Options{MaxDimension: 2048, MaxPixels: 100}); err == nil {
		t.Fatal("over-pixel-count image must be rejected")
	}
}

func TestReencodeNormalizesAlpha(t *testing.T) {
	pngAlpha := mkPNG(t, 32, 32, true)
	res, err := Validate(pngAlpha, Options{MaxDimension: 2048, MaxPixels: 2048 * 2048, Reencode: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.ContentType != "image/png" {
		t.Fatalf("alpha image must stay PNG, got %s", res.ContentType)
	}

	pngOpaque := mkPNG(t, 32, 32, false)
	res, err = Validate(pngOpaque, Options{MaxDimension: 2048, MaxPixels: 2048 * 2048, Reencode: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.ContentType != "image/jpeg" {
		t.Fatalf("opaque image must normalize to JPEG, got %s", res.ContentType)
	}
	if !res.Reencoded {
		t.Fatal("reencode flag must be set")
	}
}

func TestReencodeOffKeepsOriginal(t *testing.T) {
	src := mkJPEG(t, 16, 16)
	res, err := Validate(src, Options{MaxDimension: 2048, MaxPixels: 2048 * 2048, Reencode: false})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reencoded || !bytes.Equal(res.Data, src) {
		t.Fatal("with reencode=false the original bytes must be kept")
	}
}

func TestAnimatedGIFFirstFrame(t *testing.T) {
	data := mkGIF(t, 3)
	res, err := Validate(data, Options{MaxDimension: 2048, MaxPixels: 2048 * 2048, Reencode: true})
	if err != nil {
		t.Fatalf("gif rejected: %v", err)
	}
	if res.Width != 8 || res.Height != 8 {
		t.Fatalf("gif dims = %dx%d", res.Width, res.Height)
	}
}

func TestWebPAccepted(t *testing.T) {
	// Known-good 1x1 lossy WebP (VP8).
	webp, err := base64.StdEncoding.DecodeString(
		"UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Validate(webp, Options{MaxDimension: 2048, MaxPixels: 2048 * 2048, Reencode: true})
	if err != nil {
		t.Fatalf("webp rejected: %v", err)
	}
	if res.Width != 1 || res.Height != 1 {
		t.Fatalf("webp dims = %dx%d", res.Width, res.Height)
	}
}
