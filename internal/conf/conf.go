// Package conf edits configuration files the way operations need to: set a
// key, in a section if the format has sections, keeping the file's own
// layout. It replaces run.sh's `sed -i 's/^#*key.*/key value/'`, which could
// not add a key that was missing and silently did nothing then.
//
// `hs conf set` and `hs conf install` print each changed line (before →
// after) and keep the previous file under /var/lib/hs/backups/<path>.<time>
// — never next to the original, where fail2ban's jail.d or cron.d would read
// a backup as live configuration.
package conf

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/valolink/hestiascripts/internal/state"
)

// Style is how a new line is written. An existing line keeps its own form.
type Style string

const (
	Space Style = "space" // key value          (redis.conf, sshd_config)
	INI   Style = "ini"   // key = value        (my.cnf, fail2ban jails)
	Eq    Style = "eq"    // key=value          (php.ini)
	Shell Style = "shell" // key="value"        (conf.maldet)
)

type Opts struct {
	Section string // "" = the whole file
	Style   Style
	Comment string // comment prefix for superseded duplicates ("#" default)
	// After: a missing key with no commented example goes after this key's
	// line (keeps pm.* together in a pool template) instead of at the end.
	After string
}

// Change is one key's outcome.
type Change struct {
	Key, Old, New string // Old "" = added
}

func (c Change) String() string {
	switch {
	case c.Old == c.New:
		return "  = " + c.New
	case c.Old == "":
		return "  + " + c.New
	}
	return "  - " + c.Old + "\n  + " + c.New
}

func render(st Style, key, val string) string {
	switch st {
	case INI:
		return key + " = " + val
	case Eq:
		return key + "=" + val
	case Shell:
		return key + `="` + val + `"`
	}
	return key + " " + val
}

// restyle writes val in the form an existing line uses.
func restyle(line, key, val string, st Style) string {
	rest := strings.TrimPrefix(strings.TrimLeft(line, " \t"), key)
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	if m := regexp.MustCompile(`^(\s*=\s*)`).FindString(rest); m != "" {
		v := val
		if strings.HasPrefix(strings.TrimSpace(rest[len(m):]), `"`) {
			v = `"` + val + `"`
		}
		return indent + key + m + v
	}
	return indent + render(st, key, val)
}

var header = regexp.MustCompile(`^\s*\[([^\]]+)\]\s*$`)

func keyRe(key string) *regexp.Regexp {
	return regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `(\s*=|\s+|$)`)
}

func commentedRe(key string) *regexp.Regexp {
	return regexp.MustCompile(`^\s*[#;]+\s*` + regexp.QuoteMeta(key) + `(\s*=|\s+|$)`)
}

// scope returns the [start, end) line range the keys live in, or -1 when
// the section does not exist.
func scope(lines []string, section string) (int, int) {
	if section == "" {
		return 0, len(lines)
	}
	start := -1
	for i, l := range lines {
		if m := header.FindStringSubmatch(l); m != nil {
			if start >= 0 {
				return start, i
			}
			if strings.TrimSpace(m[1]) == section {
				start = i + 1
			}
		}
	}
	if start < 0 {
		return -1, -1
	}
	return start, len(lines)
}

// Set returns content with each key set. kv is key, value, key, value, …
func Set(content string, o Opts, kv ...string) (string, []Change) {
	if o.Comment == "" {
		o.Comment = "#"
	}
	if o.Style == "" {
		o.Style = Space
	}
	trailingNL := strings.HasSuffix(content, "\n") || content == ""
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if content == "" {
		lines = nil
	}
	var changes []Change
	for i := 0; i+1 < len(kv); i += 2 {
		key, val := kv[i], kv[i+1]
		start, end := scope(lines, o.Section)
		if start < 0 {
			if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
				lines = append(lines, "")
			}
			lines = append(lines, "["+o.Section+"]")
			start, end = len(lines), len(lines)
		}
		re, cre := keyRe(key), commentedRe(key)
		first, commented := -1, -1
		for j := start; j < end; j++ {
			switch {
			case re.MatchString(lines[j]):
				if first < 0 {
					first = j
				} else {
					lines[j] = o.Comment + " " + strings.TrimSpace(lines[j]) + "   # superseded by hs (duplicate)"
				}
			case commented < 0 && cre.MatchString(lines[j]):
				commented = j
			}
		}
		switch {
		case first >= 0:
			old := lines[first]
			lines[first] = restyle(old, key, val, o.Style)
			changes = append(changes, Change{key, strings.TrimSpace(old), strings.TrimSpace(lines[first])})
		case commented >= 0:
			nl := render(o.Style, key, val)
			lines = append(lines[:commented+1], append([]string{nl}, lines[commented+1:]...)...)
			changes = append(changes, Change{Key: key, New: nl})
		default:
			at := end
			for at > start && strings.TrimSpace(lines[at-1]) == "" {
				at--
			}
			if o.After != "" {
				are := keyRe(o.After)
				for j := start; j < end; j++ {
					if are.MatchString(lines[j]) {
						at = j + 1
					}
				}
			}
			nl := render(o.Style, key, val)
			lines = append(lines[:at], append([]string{nl}, lines[at:]...)...)
			changes = append(changes, Change{Key: key, New: nl})
			if o.After != "" {
				o.After = key // the next missing key follows this one
			}
		}
	}
	out := strings.Join(lines, "\n")
	if trailingNL {
		out += "\n"
	}
	return out, changes
}

// Unset comments out every active line of each key in the scope.
func Unset(content string, o Opts, keys ...string) (string, []Change) {
	if o.Comment == "" {
		o.Comment = "#"
	}
	lines := strings.Split(content, "\n")
	start, end := scope(lines, o.Section)
	var changes []Change
	if start < 0 {
		return content, nil
	}
	for _, key := range keys {
		re := keyRe(key)
		for j := start; j < end; j++ {
			if re.MatchString(lines[j]) {
				old := strings.TrimSpace(lines[j])
				lines[j] = o.Comment + old
				changes = append(changes, Change{key, old, lines[j]})
			}
		}
	}
	return strings.Join(lines, "\n"), changes
}

// UnsetFile applies Unset to a file.
func UnsetFile(path string, o Opts, keys ...string) ([]Change, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	out, changes := Unset(string(b), o, keys...)
	if out == string(b) {
		return changes, "", nil
	}
	bak, err := write(path, out, 0o644)
	return changes, bak, err
}

// Get returns a key's active value in the scope ("" if unset).
func Get(content string, o Opts, key string) (string, bool) {
	lines := strings.Split(content, "\n")
	start, end := scope(lines, o.Section)
	if start < 0 {
		return "", false
	}
	re := keyRe(key)
	for j := start; j < end; j++ {
		if re.MatchString(lines[j]) {
			v := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[j]), key))
			v = strings.TrimSpace(strings.TrimPrefix(v, "="))
			return strings.Trim(v, `"`), true
		}
	}
	return "", false
}

// BackupPath is where the previous version of path is kept.
func BackupPath(path string, at time.Time) string {
	abs, _ := filepath.Abs(path)
	return filepath.Join(state.Dir(), "backups", abs+"."+at.Format("20060102-150405"))
}

// write replaces path with content, keeping mode and owner, after copying
// the old file to its backup. Returns the backup path ("" = new file).
func write(path, content string, mode os.FileMode) (string, error) {
	old, err := os.ReadFile(path)
	var bak string
	uid, gid := -1, -1
	if err == nil {
		st, _ := os.Stat(path)
		mode = st.Mode().Perm()
		if s, ok := st.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(s.Uid), int(s.Gid)
		}
		bak = BackupPath(path, time.Now())
		// Several steps can edit one file within a second: never overwrite
		// an earlier backup — it is the state before the first of them.
		for n := 2; ; n++ {
			if _, err := os.Stat(bak); os.IsNotExist(err) {
				break
			}
			bak = BackupPath(path, time.Now()) + "-" + strconv.Itoa(n)
		}
		if err := os.MkdirAll(filepath.Dir(bak), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(bak, old, 0o600); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	tmp := filepath.Join(filepath.Dir(path), ".hs-tmp-"+filepath.Base(path))
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
		return "", err
	}
	_ = os.Chmod(tmp, mode)
	if uid >= 0 {
		_ = os.Chown(tmp, uid, gid)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return bak, nil
}

// Write replaces path with content (backup kept, mode and owner preserved;
// a new file gets mode). Returns the backup path, "" when unchanged or new.
func Write(path, content string, mode os.FileMode) (string, error) {
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		return "", nil
	}
	return write(path, content, mode)
}

// SetFile applies Set to a file and reports what changed. Nothing is
// written (and no backup made) when every key already has its value.
func SetFile(path string, o Opts, kv ...string) ([]Change, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	out, changes := Set(string(b), o, kv...)
	if out == string(b) {
		return changes, "", nil
	}
	bak, err := write(path, out, 0o644)
	return changes, bak, err
}

// InstallFile writes src's content to dst (backing up a different dst).
// Returns the lines removed and added, and the backup path.
func InstallFile(src, dst string, mode os.FileMode) (removed, added []string, bak string, err error) {
	nb, err := os.ReadFile(src)
	if err != nil {
		return nil, nil, "", err
	}
	ob, _ := os.ReadFile(dst)
	if string(nb) == string(ob) {
		return nil, nil, "", nil
	}
	removed, added = LineDiff(string(ob), string(nb))
	bak, err = write(dst, string(nb), mode)
	return removed, added, bak, err
}

// LineDiff lists lines only in a and lines only in b (order kept).
func LineDiff(a, b string) (onlyA, onlyB []string) {
	count := func(s string) map[string]int {
		m := map[string]int{}
		for _, l := range strings.Split(s, "\n") {
			m[l]++
		}
		return m
	}
	ca, cb := count(a), count(b)
	for _, l := range strings.Split(a, "\n") {
		if cb[l] > 0 {
			cb[l]--
		} else if strings.TrimSpace(l) != "" {
			onlyA = append(onlyA, l)
		}
	}
	for _, l := range strings.Split(b, "\n") {
		if ca[l] > 0 {
			ca[l]--
		} else if strings.TrimSpace(l) != "" {
			onlyB = append(onlyB, l)
		}
	}
	return
}

// Report prints what SetFile/InstallFile did.
func Report(path string, changes []Change, bak string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s:\n", path)
	for _, c := range changes {
		b.WriteString(c.String() + "\n")
	}
	if bak != "" {
		fmt.Fprintf(&b, "  previous file kept: %s\n  undo: cp -p %s %s\n", bak, bak, path)
	} else {
		b.WriteString("  unchanged — nothing written\n")
	}
	return b.String()
}
