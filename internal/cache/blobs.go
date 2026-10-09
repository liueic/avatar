package cache

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Blobs stores image bytes on the filesystem, sharded by the first two hash
// characters: <root>/<hash[0:2]>/<hash>.<ext> (SPEC §10.1).
type Blobs struct {
	root string
}

// NewBlobs creates the blob root (which must live inside the cache dir).
func NewBlobs(cacheDir string) (*Blobs, error) {
	root := filepath.Join(cacheDir, "blobs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("cache: create blob dir: %w", err)
	}
	return &Blobs{root: root}, nil
}

// Path returns the relative blob path for a hash. The hash must have been
// validated with ValidateHash first; the path is derived purely from the hash
// so no user input ever reaches a filesystem path (SPEC §17).
func (b *Blobs) Path(hash, ext string) string {
	return filepath.Join(hash[:2], hash+"."+ext)
}

func (b *Blobs) abs(rel string) (string, error) {
	clean := filepath.Clean(rel)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("cache: suspicious blob path %q", rel)
	}
	return filepath.Join(b.root, clean), nil
}

// Write stores data atomically (temp file + rename) and returns the relative
// path to record in the DB.
func (b *Blobs) Write(hash, ext string, data []byte) (string, error) {
	rel := b.Path(hash, ext)
	abs, err := b.abs(rel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("cache: blob mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("cache: blob temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", fmt.Errorf("cache: blob write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", fmt.Errorf("cache: blob sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, abs); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("cache: blob rename: %w", err)
	}
	return rel, nil
}

// Open opens the blob for reading and returns the file and its size.
func (b *Blobs) Open(rel string) (*os.File, int64, error) {
	abs, err := b.abs(rel)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, 0, fmt.Errorf("cache: blob open: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("cache: blob stat: %w", err)
	}
	return f, fi.Size(), nil
}

// Read reads the whole blob into memory (bounded by upstream max_bytes at
// write time, so this cannot exhaust memory).
func (b *Blobs) Read(rel string) ([]byte, error) {
	abs, err := b.abs(rel)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("cache: blob read: %w", err)
	}
	return data, nil
}

// Delete removes a blob; a missing file is not an error.
func (b *Blobs) Delete(rel string) error {
	if rel == "" {
		return nil
	}
	abs, err := b.abs(rel)
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cache: blob delete: %w", err)
	}
	return nil
}

// DeleteAll removes every blob file (admin purge?status=all).
func (b *Blobs) DeleteAll() error {
	entries, err := os.ReadDir(b.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cache: blob delete all: %w", err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(b.root, e.Name())); err != nil {
			return fmt.Errorf("cache: blob delete all: %w", err)
		}
	}
	return nil
}

// Scan returns relPath → size for every blob on disk (orphan detection).
func (b *Blobs) Scan() (map[string]int64, error) {
	out := make(map[string]int64)
	err := filepath.WalkDir(b.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(b.root, path)
		if rerr != nil {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		out[rel] = info.Size()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cache: blob scan: %w", err)
	}
	return out, nil
}
