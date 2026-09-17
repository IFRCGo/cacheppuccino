package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Cache persists the last adopted XLSX per application on local disk so a
// restarted container serves immediately instead of waiting on a peer or on
// upstream. It lives on an emptyDir, so it survives container restarts
// within a pod and nothing more; peer hydration covers everything else.
type Cache struct {
	dir    string
	maxAge time.Duration
}

// cacheMeta accompanies each XLSX. The hash is verified against the file's
// contents on load, so a half-written or truncated pair is discarded rather
// than served.
type cacheMeta struct {
	Hash       string    `json:"hash"`
	ImportedAt time.Time `json:"imported_at"`
	RowCount   int       `json:"row_count"`
}

func NewCache(dir string, maxAge time.Duration) *Cache {
	if dir == "" {
		return nil
	}
	return &Cache{dir: dir, maxAge: maxAge}
}

func (c *Cache) paths(appID string) (xlsx, meta string) {
	return filepath.Join(c.dir, appID+".xlsx"), filepath.Join(c.dir, appID+".json")
}

// Load returns the cached XLSX and its metadata. A missing, corrupt, or
// expired entry reports ok=false rather than an error: the caller's next
// move is the same either way, and a bad cache file must never keep the pod
// from starting.
func (c *Cache) Load(appID string, now time.Time) (xlsx []byte, meta cacheMeta, ok bool) {
	if c == nil {
		return nil, cacheMeta{}, false
	}
	xlsxPath, metaPath := c.paths(appID)

	metaRaw, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, cacheMeta{}, false
	}
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return nil, cacheMeta{}, false
	}

	xlsx, err = os.ReadFile(xlsxPath)
	if err != nil {
		return nil, cacheMeta{}, false
	}
	if HashBytes(xlsx) != meta.Hash {
		return nil, cacheMeta{}, false
	}
	if c.maxAge > 0 && now.Sub(meta.ImportedAt) > c.maxAge {
		return nil, cacheMeta{}, false
	}
	return xlsx, meta, true
}

// Save writes the pair atomically: each file is written to a temp name and
// renamed, so a crash mid-write cannot leave a truncated file in place. The
// XLSX lands before the metadata, so the worst interleaving leaves metadata
// describing an older file, which Load rejects on the hash check.
func (c *Cache) Save(appID string, xlsx []byte, meta cacheMeta) error {
	if c == nil {
		return nil
	}
	if err := os.MkdirAll(c.dir, 0o750); err != nil {
		return err
	}
	xlsxPath, metaPath := c.paths(appID)

	if err := writeFileAtomic(xlsxPath, xlsx); err != nil {
		return err
	}
	metaRaw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return writeFileAtomic(metaPath, metaRaw)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)

	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		// No-op once the rename succeeded; removes the temp file otherwise.
		_ = os.Remove(tmp)
	}()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	// fsync before rename: rename is atomic, but without the sync the
	// rename can land while the contents are still only in page cache.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// validateAppID keeps application ids usable as path components, since they
// name cache files.
func validateAppID(id string) error {
	if id == "" {
		return errors.New("application id must not be empty")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return fmt.Errorf("invalid application id %q: use lowercase letters, digits, '-' and '_'", id)
		}
	}
	return nil
}
