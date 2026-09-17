package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheRoundTrip(t *testing.T) {
	c := NewCache(t.TempDir(), time.Hour)
	now := time.Now().UTC().Truncate(time.Second)
	body := []byte("xlsx-bytes")

	meta := cacheMeta{Hash: HashBytes(body), ImportedAt: now, RowCount: 7}
	if err := c.Save(defaultAppID, body, meta); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, gotMeta, ok := c.Load(defaultAppID, now)
	if !ok {
		t.Fatalf("Load reported no entry")
	}
	if string(got) != string(body) {
		t.Errorf("body = %q, want %q", got, body)
	}
	if gotMeta.Hash != meta.Hash || gotMeta.RowCount != 7 || !gotMeta.ImportedAt.Equal(now) {
		t.Errorf("meta = %+v, want %+v", gotMeta, meta)
	}
}

func TestCacheRejectsUnusableEntries(t *testing.T) {
	now := time.Now()
	body := []byte("xlsx-bytes")

	tests := []struct {
		name   string
		maxAge time.Duration
		// corrupt runs after a good Save and breaks the entry somehow.
		corrupt func(t *testing.T, dir string)
		readAt  time.Time
	}{
		{
			name:    "missing entry",
			maxAge:  time.Hour,
			corrupt: func(t *testing.T, dir string) { os.RemoveAll(dir) },
			readAt:  now,
		},
		{
			// A crash between the two renames leaves metadata describing a
			// file that is no longer there; the hash check must catch it.
			name:   "body does not match recorded hash",
			maxAge: time.Hour,
			corrupt: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, defaultAppID+".xlsx"), []byte("different"), 0o640); err != nil {
					t.Fatalf("overwrite: %v", err)
				}
			},
			readAt: now,
		},
		{
			name:   "truncated metadata",
			maxAge: time.Hour,
			corrupt: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, defaultAppID+".json"), []byte("{"), 0o640); err != nil {
					t.Fatalf("truncate: %v", err)
				}
			},
			readAt: now,
		},
		{
			// Serving a day-old file would quietly reintroduce content the
			// rest of the fleet has moved past.
			name:    "older than max age",
			maxAge:  time.Minute,
			corrupt: func(t *testing.T, dir string) {},
			readAt:  now.Add(2 * time.Minute),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c := NewCache(dir, tc.maxAge)

			if err := c.Save(defaultAppID, body, cacheMeta{Hash: HashBytes(body), ImportedAt: now, RowCount: 1}); err != nil {
				t.Fatalf("Save: %v", err)
			}
			tc.corrupt(t, dir)

			if _, _, ok := c.Load(defaultAppID, tc.readAt); ok {
				t.Errorf("Load accepted an unusable entry")
			}
		})
	}
}

func TestCacheSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir, time.Hour)
	body := []byte("xlsx-bytes")

	for range 3 {
		if err := c.Save(defaultAppID, body, cacheMeta{Hash: HashBytes(body), ImportedAt: time.Now()}); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("cache dir holds %v, want exactly the xlsx and json pair", names)
	}
}

func TestNilCacheIsANoOp(t *testing.T) {
	// An empty CACHE_DIR disables the cache rather than failing startup.
	var c *Cache = NewCache("", time.Hour)

	if err := c.Save(defaultAppID, []byte("x"), cacheMeta{}); err != nil {
		t.Errorf("Save on disabled cache: %v", err)
	}
	if _, _, ok := c.Load(defaultAppID, time.Now()); ok {
		t.Errorf("Load on disabled cache reported an entry")
	}
}

func TestValidateAppID(t *testing.T) {
	tests := []struct {
		id      string
		wantErr bool
	}{
		{"default", false},
		{"ifrc-go", false},
		{"app_2", false},
		{"", true},
		{"../escape", true},
		{"Upper", true},
		{"has space", true},
	}

	for _, tc := range tests {
		t.Run(tc.id, func(t *testing.T) {
			if err := validateAppID(tc.id); (err != nil) != tc.wantErr {
				t.Errorf("validateAppID(%q) error = %v, wantErr %v", tc.id, err, tc.wantErr)
			}
		})
	}
}
