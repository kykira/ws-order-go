package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadManagerEnvPortOverridesConfigFile(t *testing.T) {
	t.Setenv("WSORDER_PORT", "9101")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	content := []byte(`{
  "server": {
    "port": 9000
  }
}`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	mgr, err := LoadManager(path)
	if err != nil {
		t.Fatalf("load manager: %v", err)
	}

	if got := mgr.Get().Server.Port; got != 9101 {
		t.Fatalf("expected env port to override config file, got %d", got)
	}
}
