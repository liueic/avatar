package server

import (
	"os"
	"path/filepath"
	"strconv"
)

// scaledDiskCache persists rendered avatar size variants on disk so a repeat
// (hash, size) never re-decodes the original (cold renders cost tens to
// hundreds of ms of CPU; a disk hit costs ~0.1ms). Files are pruned by the
// cleaner under cache.scaled_disk_max_bytes.
type scaledDiskCache struct {
	root string
}

func newScaledDiskCache(cacheDir string) (*scaledDiskCache, error) {
	root := filepath.Join(cacheDir, "scaled")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &scaledDiskCache{root: root}, nil
}

// key derives the shard path. hash is pre-validated hex and rev/size are
// integers, so no user input reaches the filesystem unfiltered.
func (c *scaledDiskCache) key(hash string, size int64, rev int64, ext string) string {
	return filepath.Join(hash[:2], hash+"."+ext+"."+strconv.FormatInt(size, 10)+".r"+strconv.FormatInt(rev, 10))
}

func (c *scaledDiskCache) get(key string) ([]byte, bool) {
	b, err := os.ReadFile(filepath.Join(c.root, key))
	if err != nil {
		return nil, false
	}
	return b, true
}

func (c *scaledDiskCache) put(key string, data []byte) {
	path := filepath.Join(c.root, key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	_ = os.Rename(tmpName, path)
}
