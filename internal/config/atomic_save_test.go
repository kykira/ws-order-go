package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A Docker deployment bind-mounts a SINGLE file (./config.json:/app/config.json).
// A mount point cannot be replaced by rename (EBUSY), so the atomic path fails
// and the save must fall back to an in-place write; otherwise every config save
// from the panel would return an error.
func TestSaveFallsBackToInPlaceWhenRenameFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	mgr, err := LoadManager(path)
	if err != nil {
		t.Fatal(err)
	}

	orig := writeAtomic
	writeAtomic = func(string, []byte) error { return errors.New("simulated EBUSY") }
	defer func() { writeAtomic = orig }()

	if err := mgr.Update(func(c *Config) { c.Dispatch = "random" }); err != nil {
		t.Fatalf("Update should fall back to an in-place write, got: %v", err)
	}

	bs, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got Config
	if err := json.Unmarshal(bs, &got); err != nil {
		t.Fatal(err)
	}
	if got.Dispatch != "random" {
		t.Fatalf("dispatch = %q, want %q (the fallback did not persist the config)", got.Dispatch, "random")
	}
}
