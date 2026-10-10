package avatar

import (
	"bytes"
	"image/png"
	"testing"
)

const testHash32 = "0123abcd0123abcd0123abcd0123abcd"

func newGen(t *testing.T) *Generator {
	t.Helper()
	g, err := NewGenerator(t.TempDir(), "thumbs", "lorelei", 64, 512)
	if err != nil {
		t.Fatalf("generator: %v", err)
	}
	return g
}

// TestDeterministic is SPEC §18: identical hashes must render byte-identical
// output across calls (same process) — DiceBear derives everything from seed.
func TestDeterministic(t *testing.T) {
	g := newGen(t)
	a, err := g.SVG("identicon", testHash32, 80)
	if err != nil {
		t.Fatal(err)
	}
	b, err := g.SVG("identicon", testHash32, 80)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("same hash+style+size must render identical SVG")
	}

	// Through the disk-cache path too (fresh generator, same dir semantics).
	g2, err := NewGenerator(t.TempDir(), "identicon", "pixel-art", 64, 512)
	if err != nil {
		t.Fatal(err)
	}
	c, err := g2.SVG("identicon", testHash32, 80)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, c) {
		t.Fatal("SVG must be stable across generator instances")
	}
}

func TestDifferentHashesDiffer(t *testing.T) {
	g := newGen(t)
	a, _ := g.SVG("identicon", testHash32, 80)
	b, _ := g.SVG("identicon", "ffffffffffffffffffffffffffffffff", 80)
	if bytes.Equal(a, b) {
		t.Fatal("different hashes should (virtually always) differ")
	}
}

func TestPNGRasterization(t *testing.T) {
	g := newGen(t)
	data, err := g.PNG("identicon", testHash32, 80)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("output is not a valid PNG: %v", err)
	}
	if img.Bounds().Dx() != 80 || img.Bounds().Dy() != 80 {
		t.Fatalf("png size = %v", img.Bounds())
	}
}

func TestStyleForDMapping(t *testing.T) {
	g := newGen(t) // defaultStyle=thumbs, retroStyle=lorelei
	cases := map[string]string{
		"identicon":                  "identicon", // protocol meaning preserved
		"retro":                      "lorelei",   // configured retro mapping
		"mp":                         "thumbs",    // unsupported named style falls back
		"https://evil.example/a.png": "thumbs",    // custom URL falls back (SPEC §4.2)
		"":                           "thumbs",
	}
	for in, want := range cases {
		if got := g.StyleForD(in); got != want {
			t.Errorf("StyleForD(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnknownStyleRejected(t *testing.T) {
	if _, err := NewGenerator(t.TempDir(), "nope", "lorelei", 8, 512); err == nil {
		t.Fatal("unknown default style must be rejected at startup")
	}
}

// TestAllBuiltinStylesRender guards every embedded style: SVG + PNG must
// render without error (catches oksvg-incompatible styles at test time).
func TestAllBuiltinStylesRender(t *testing.T) {
	g := newGen(t)
	for _, style := range []string{"identicon", "pixel-art", "thumbs", "lorelei", "open-peeps"} {
		if _, err := g.SVG(style, testHash32, 64); err != nil {
			t.Errorf("style %s: svg: %v", style, err)
			continue
		}
		if _, err := g.PNG(style, testHash32, 64); err != nil {
			t.Errorf("style %s: png: %v", style, err)
		}
	}
}

func TestLRUEviction(t *testing.T) {
	c := NewLRU(2)
	c.Put("a", []byte{1})
	c.Put("b", []byte{2})
	c.Get("a") // a becomes most recent
	c.Put("c", []byte{3})
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted (least recent)")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should survive")
	}
	if c.Len() != 2 {
		t.Fatalf("len = %d, want 2", c.Len())
	}
}

// TestPNGRasterClamped guards the CPU/ disk-exhaustion mitigation: oversized
// PNG requests render at maxRaster and share one cache entry.
func TestPNGRasterClamped(t *testing.T) {
	g, err := NewGenerator(t.TempDir(), "thumbs", "lorelei", 64, 64)
	if err != nil {
		t.Fatal(err)
	}
	data, err := g.PNG("thumbs", testHash32, 2048)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 64 || b.Dy() != 64 {
		t.Fatalf("clamped raster = %v, want 64x64", b)
	}
}
