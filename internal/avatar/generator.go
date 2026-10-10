// Package avatar generates deterministic default avatars with DiceBear
// (geometric styles, no generative AI — SPEC §8) and serves them from a
// two-level cache: in-memory LRU plus a disk cache keyed
// {style}/{hash}/{size}.{fmt}.
package avatar

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sync"

	"github.com/dicebear/dicebear-go/v11"
	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
)

//go:embed styles/identicon.json
var identiconDef []byte

//go:embed styles/pixel-art.json
var pixelArtDef []byte

//go:embed styles/thumbs.json
var thumbsDef []byte

//go:embed styles/lorelei.json
var loreleiDef []byte

//go:embed styles/open-peeps.json
var openPeepsDef []byte

// builtins are the style definitions compiled into the binary. All are CC0
// (public domain); we still credit DiceBear in the README and LICENSE.
var builtins = map[string][]byte{
	"identicon":  identiconDef,
	"pixel-art":  pixelArtDef,
	"thumbs":     thumbsDef,
	"lorelei":    loreleiDef,
	"open-peeps": openPeepsDef,
}

// Generator renders default avatars for a fixed set of DiceBear styles.
type Generator struct {
	styles map[string]*dicebear.Style

	defaultStyle string
	retroStyle   string

	disk string // defaults cache dir
	mem  *LRU

	// maxRaster caps PNG rasterization: SVG is resolution-independent, so
	// oversized renders only serve CPU/disk exhaustion attacks (README 限流).
	maxRaster int

	renderMu sync.Mutex // dicebear Style reuse is not documented as goroutine-safe
}

// NewGenerator parses the built-in styles and prepares the disk cache under
// cacheDir/defaults.
func NewGenerator(cacheDir, defaultStyle, retroStyle string, lruSize int, maxRaster int) (*Generator, error) {
	if maxRaster <= 0 {
		maxRaster = 512
	}
	g := &Generator{
		styles:       make(map[string]*dicebear.Style, len(builtins)),
		defaultStyle: defaultStyle,
		retroStyle:   retroStyle,
		disk:         filepath.Join(cacheDir, "defaults"),
		mem:          NewLRU(lruSize),
		maxRaster:    maxRaster,
	}
	for name, def := range builtins {
		st, err := dicebear.NewStyle(def)
		if err != nil {
			return nil, fmt.Errorf("avatar: parse style %s: %w", name, err)
		}
		g.styles[name] = st
	}
	for _, want := range []string{defaultStyle, retroStyle} {
		if _, ok := g.styles[want]; !ok {
			return nil, fmt.Errorf("avatar: style %q is not built in (available: identicon, pixel-art, thumbs, lorelei, open-peeps)", want)
		}
	}
	if err := os.MkdirAll(g.disk, 0o755); err != nil {
		return nil, fmt.Errorf("avatar: create defaults dir: %w", err)
	}
	return g, nil
}

// StyleForD maps a Gravatar d= parameter onto a built-in style (SPEC §4.2).
// d=identicon keeps its protocol meaning (the classic geometric pattern);
// d=retro maps to the configured retro style; unknown values and custom URLs
// fall back to the configured default style.
func (g *Generator) StyleForD(d string) string {
	switch d {
	case "identicon":
		return "identicon"
	case "retro":
		return g.retroStyle
	default:
		return g.defaultStyle
	}
}

// SVG returns the cached (or freshly rendered) SVG avatar for hash at size.
func (g *Generator) SVG(styleName, hash string, size int) ([]byte, error) {
	return g.get(styleName, hash, size, "svg")
}

// PNG returns the cached (or freshly rasterized) PNG avatar for hash at size.
func (g *Generator) PNG(styleName, hash string, size int) ([]byte, error) {
	return g.get(styleName, hash, size, "png")
}

func (g *Generator) get(styleName, hash string, size int, format string) ([]byte, error) {
	// Clamp oversized PNG renders before the cache key so every >max size
	// shares one cached bitmap.
	if format == "png" && size > g.maxRaster {
		size = g.maxRaster
	}
	key := styleName + "/" + hash + "/" + format + "/" + itoa(size)
	if b, ok := g.mem.Get(key); ok {
		return b, nil
	}

	path := filepath.Join(g.disk, styleName, hash, fmt.Sprintf("%d.%s", size, format))
	if b, err := os.ReadFile(path); err == nil {
		g.mem.Put(key, b)
		return b, nil
	}

	b, err := g.render(styleName, hash, size, format)
	if err != nil {
		return nil, err
	}
	g.mem.Put(key, b)
	if err := writeAtomic(path, b); err != nil {
		// Disk cache failures are non-fatal: memory cache still serves.
		return b, nil
	}
	return b, nil
}

func (g *Generator) render(styleName, hash string, size int, format string) ([]byte, error) {
	st, ok := g.styles[styleName]
	if !ok {
		return nil, fmt.Errorf("avatar: unknown style %q", styleName)
	}

	g.renderMu.Lock()
	av, err := dicebear.NewAvatar(st, map[string]any{
		"seed": hash,
		"size": size,
	})
	g.renderMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("avatar: render: %w", err)
	}

	svg := []byte(av.SVG())
	if format == "svg" {
		return svg, nil
	}
	out, err := rasterize(svg, size)
	if err != nil {
		return nil, fmt.Errorf("avatar: rasterize: %w", err)
	}
	return out, nil
}

// rasterize converts an SVG to a PNG of the given square size using
// oksvg/rasterx (pure Go, SPEC §8.2).
func rasterize(svg []byte, size int) ([]byte, error) {
	icon, err := oksvg.ReadIconStream(bytes.NewReader(svg))
	if err != nil {
		return nil, err
	}
	icon.SetTarget(0, 0, float64(size), float64(size))
	rgba := image.NewRGBA(image.Rect(0, 0, size, size))
	scannerGV := rasterx.NewScannerGV(size, size, rgba, rgba.Bounds())
	raster := rasterx.NewDasher(size, size, scannerGV)
	icon.Draw(raster, 1.0)

	var buf bytes.Buffer
	if err := png.Encode(&buf, rgba); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
