// Package logcap keeps chosen log files from growing without bound: one
// logrotate block per log in /etc/hs/logcap.conf, run hourly from
// /etc/cron.d/hs-logcap with its own state file. Used for WordPress debug
// logs that are kept on deliberately (moved to the site's private/).
//
// The file is hs-managed: blocks are delimited by "# hs:logcap <key>" lines,
// added and removed whole, and the rest of the file is left alone.
package logcap

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func etcDir() string {
	if d := os.Getenv("HS_ETC_DIR"); d != "" {
		return d
	}
	return "/etc/hs"
}

func cronDir() string {
	if d := os.Getenv("HS_CRON_DIR"); d != "" {
		return d
	}
	return "/etc/cron.d"
}

func ConfPath() string  { return filepath.Join(etcDir(), "logcap.conf") }
func CronPath() string  { return filepath.Join(cronDir(), "hs-logcap") }
func statePath() string { return "/var/lib/hs/logcap.state" }

const DefaultSize = "50M"

// Block is the logrotate stanza for one log: rotate at size, keep one
// compressed previous file, truncate in place so the writer (PHP-FPM) keeps
// its handle, and run as the file's owner so nothing becomes root-owned.
func Block(key, path, owner, size string) string {
	return fmt.Sprintf(`# hs:logcap %s
%s {
    su %s %s
    size %s
    rotate 1
    compress
    missingok
    notifempty
    copytruncate
}
# hs:end %s
`, key, path, owner, owner, size, key)
}

// CronLine runs logrotate hourly on our file only, with our own state file,
// so the system's daily logrotate is untouched.
func CronLine() string {
	return fmt.Sprintf("# hs: cap hs-managed logs (hs logcap)\n17 * * * * root /usr/sbin/logrotate -s %s %s\n", statePath(), ConfPath())
}

func read() string {
	b, _ := os.ReadFile(ConfPath())
	return string(b)
}

// Entries lists managed keys and their paths.
func Entries() map[string]string {
	out := map[string]string{}
	lines := strings.Split(read(), "\n")
	for i, l := range lines {
		if key, ok := strings.CutPrefix(l, "# hs:logcap "); ok && i+1 < len(lines) {
			out[key] = strings.TrimSuffix(strings.TrimSpace(lines[i+1]), " {")
		}
	}
	return out
}

// Managed reports whether path is capped, and at what size.
func Managed(path string) (size string, ok bool) {
	conf := read()
	i := strings.Index(conf, "\n"+path+" {")
	if i < 0 && !strings.HasPrefix(conf, path+" {") {
		return "", false
	}
	rest := conf[max(i, 0):]
	for _, l := range strings.Split(rest, "\n")[1:] {
		t := strings.TrimSpace(l)
		if s, ok := strings.CutPrefix(t, "size "); ok {
			return s, true
		}
		if t == "}" {
			break
		}
	}
	return DefaultSize, true
}

func remove(conf, key string) string {
	start := "# hs:logcap " + key + "\n"
	end := "# hs:end " + key + "\n"
	i := strings.Index(conf, start)
	if i < 0 {
		return conf
	}
	j := strings.Index(conf[i:], end)
	if j < 0 {
		return conf
	}
	return conf[:i] + conf[i+j+len(end):]
}

func write(conf string) error {
	if err := os.MkdirAll(etcDir(), 0o755); err != nil {
		return err
	}
	// logrotate refuses a config writable by group/other.
	tmp := ConfPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(conf), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, ConfPath())
}

// Add (or replace) the block for key, and make sure the cron entry exists.
func Add(key, path, owner, size string) error {
	for _, v := range []string{key, path, owner, size} {
		if v == "" || strings.ContainsAny(v, "\n\t {}\"'") {
			return fmt.Errorf("refusing unsafe value %q", v)
		}
	}
	if !filepath.IsAbs(path) || strings.Contains(path, "..") {
		return fmt.Errorf("path must be absolute: %q", path)
	}
	conf := remove(read(), key)
	if conf != "" && !strings.HasSuffix(conf, "\n") {
		conf += "\n"
	}
	if err := write(conf + Block(key, path, owner, size)); err != nil {
		return err
	}
	return ensureCron()
}

// Remove drops key's block; the cron entry goes when nothing is left.
func Remove(key string) error {
	conf := remove(read(), key)
	if err := write(conf); err != nil {
		return err
	}
	if len(Entries()) == 0 {
		os.Remove(CronPath())
	}
	return nil
}

func ensureCron() error {
	if b, err := os.ReadFile(CronPath()); err == nil && string(b) == CronLine() {
		return nil
	}
	if err := os.MkdirAll(cronDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(CronPath(), []byte(CronLine()), 0o644)
}

// Keys returns managed keys sorted.
func Keys() []string {
	var ks []string
	for k := range Entries() {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
