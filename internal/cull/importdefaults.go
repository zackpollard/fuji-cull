package cull

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Import destination/album persist across sessions: whatever an import last
// ran with prefills the import panel next launch. Explicit --dest /
// --immich-album flags still win over the remembered values.
type importDefaults struct {
	Dest  string `json:"dest"`
	Album string `json:"album"`
}

func importDefaultsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "fuji-cull", "import-defaults.json")
}

func loadImportDefaults() importDefaults {
	var d importDefaults
	raw, err := os.ReadFile(importDefaultsPath())
	if err == nil {
		_ = json.Unmarshal(raw, &d)
	}
	return d
}

func saveImportDefaults(dest, album string) {
	raw, _ := json.Marshal(importDefaults{Dest: dest, Album: album})
	path := importDefaultsPath()
	if os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}
