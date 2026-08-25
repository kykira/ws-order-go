package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTaskSlotGroupKeyExplicitGroup(t *testing.T) {
	a := TaskConfig{ID: "a", Name: "小张btc", Group: "xiao-zhang"}
	b := TaskConfig{ID: "b", Name: "小张eth", Group: "xiao-zhang"}
	if a.SlotGroupKey() != b.SlotGroupKey() {
		t.Fatalf("same group should share slot key: %q vs %q", a.SlotGroupKey(), b.SlotGroupKey())
	}
	if got := a.SlotGroupKey(); got != "group:xiao-zhang" {
		t.Fatalf("unexpected slot group key: %q", got)
	}
}

func TestTaskSlotGroupKeyFallbackToTaskID(t *testing.T) {
	a := TaskConfig{ID: "a"}
	if got := a.SlotGroupKey(); got != "task:a" {
		t.Fatalf("expected fallback to task id, got %q", got)
	}
}

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
