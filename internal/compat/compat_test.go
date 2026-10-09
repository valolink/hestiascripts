package compat

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// The key sets EngineLink parses today, copied from the printf format strings
// in v-server-health.sh and v-server-setup-status.sh. If a field is renamed
// here, EngineLink silently reads undefined — so this test is the contract.
var wantHealth = map[string][]string{
	"":     {"backups", "disk", "mailQueue", "services"},
	"disk": {"info", "usedPct"},
}

var wantSetup = map[string][]string{
	"":               {"disk", "fail2ban", "hestia", "maldet", "mariadb", "netdata", "nginxTemplates", "opcache", "phpFpmProfiles", "redis", "restic", "security", "serviceDrift", "smtp", "streamer", "wpcli"},
	"streamer":       {"running", "vScripts"},
	"wpcli":          {"installed", "version"},
	"redis":          {"installed", "maxmemory", "phpExt", "policy", "running"},
	"fail2ban":       {"installed", "running", "wpJail"},
	"maldet":         {"installed", "lastScan"},
	"netdata":        {"installed", "running", "tuned"},
	"security":       {"sshKeyOnly", "swap", "unattendedUpgrades"},
	"smtp":           {"relay"},
	"phpFpmProfiles": {"installed", "missing"},
	"opcache":        {"needsAttention", "ok"},
	"mariadb":        {"bufferPool"},
	"nginxTemplates": {"wpRocket", "wpSecure"},
	"hestia":         {"installed", "latest"},
	"restic":         {"systemRepo", "usersTotal", "usersWithKeys"},
	"disk":           {"info", "usedPct"},
}

func keysOf(t *testing.T, v any) map[string][]string {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	out := map[string][]string{}
	for k, sub := range m {
		out[""] = append(out[""], k)
		if obj, ok := sub.(map[string]any); ok && k != "services" && k != "phpExt" {
			for kk := range obj {
				out[k] = append(out[k], kk)
			}
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func TestHealthShape(t *testing.T) {
	got := keysOf(t, Health{Backups: []BackupEntry{}, Services: map[string]string{}})
	if !reflect.DeepEqual(got, wantHealth) {
		t.Errorf("health keys\n got %v\nwant %v", got, wantHealth)
	}
	three := 3
	b, _ := json.Marshal(BackupEntry{User: "u", AgeHours: &three, Newest: "2026-09-24 03:00", Source: "restic"})
	if string(b) != `{"user":"u","ageHours":3,"newest":"2026-09-24 03:00","source":"restic"}` {
		t.Errorf("backup entry = %s", b)
	}
	// No snapshot: ageHours is null (EngineLink treats it as stale), the reason rides along.
	b, _ = json.Marshal(BackupEntry{User: "u", Source: "restic", Error: "restic is not set up on this box"})
	if string(b) != `{"user":"u","ageHours":null,"newest":"","source":"restic","error":"restic is not set up on this box"}` {
		t.Errorf("missing entry = %s", b)
	}
}

func TestSetupStatusShape(t *testing.T) {
	var st SetupStatus
	st.Redis.PHPExt = map[string]bool{}
	got := keysOf(t, st)
	if !reflect.DeepEqual(got, wantSetup) {
		t.Errorf("setup-status keys\n got %v\nwant %v", got, wantSetup)
	}
}
