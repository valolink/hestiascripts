// Package state persists the last results so the TUI opens instantly and
// slow probes are not repeated inside their MinInterval.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
)

// Dir is /var/lib/hs, or HS_STATE_DIR (testing without touching the box).
func Dir() string {
	if d := os.Getenv("HS_STATE_DIR"); d != "" {
		return d
	}
	return "/var/lib/hs"
}

func path() string { return filepath.Join(Dir(), "results.json") }

// Load returns the saved results, or nil if there are none.
func Load() ([]check.Result, time.Time) {
	b, err := os.ReadFile(path())
	if err != nil {
		return nil, time.Time{}
	}
	var r check.Report
	if json.Unmarshal(b, &r) != nil {
		return nil, time.Time{}
	}
	return r.Results, r.At
}

// Save writes atomically (temp file + rename), root-only.
func Save(host string, rs []check.Result, now time.Time) error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	tmp := path() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := check.JSON(f, host, rs, now); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path())
}
