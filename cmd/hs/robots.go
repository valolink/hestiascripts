package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/site"
	"github.com/valolink/hestiascripts/internal/conf"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/robots"
	"github.com/valolink/hestiascripts/internal/sys"
)

const robotsUsage = `hs robots — the hs-managed block in a WordPress site's robots.txt

  hs robots show DOMAIN              print the file hs would write (reads the live site; writes nothing)
  hs robots write DOMAIN [--sum S]   write it: previous file kept in the site's private/hs-backup-<date>/
                                     and /var/lib/hs/backups; --sum refuses content other than the planned one
`

// cmdRobots: every read and write inside the docroot runs as the site's user
// (runuser), never as root — robots.txt is the user's file, and a symlink
// planted in its place must not turn a root write into a root-file overwrite.
func cmdRobots(args []string) int {
	if len(args) < 2 || (args[0] != "show" && args[0] != "write") {
		fmt.Print(robotsUsage)
		return 2
	}
	verb, name := args[0], args[1]
	want := ""
	if len(args) >= 4 && args[2] == "--sum" {
		want = args[3]
	}
	s := sys.Real{}
	var d hestia.Domain
	found := false
	for _, x := range hestia.WebDomains(s) {
		if x.Name == name {
			d, found = x, true
		}
	}
	if !found {
		fmt.Fprintf(os.Stderr, "hs robots: no web domain %q on this box\n", name)
		return 1
	}
	env := &check.Env{Sys: s}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	path := robots.Path(d)
	existing, exists, err := readAsUser(d, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hs robots:", err)
		return 1
	}
	f, err := robots.Gather(ctx, s, d, func(ctx context.Context, a ...string) (string, error) { return site.WP(ctx, env, d, a...) })
	if err != nil {
		fmt.Fprintln(os.Stderr, "hs robots: WP-CLI:", err)
		return 1
	}
	st := robots.Assess(existing, exists, f)
	content := robots.Render(existing, f)
	if verb == "show" {
		fmt.Printf("# %s — %s\n# sum %s\n", path, f.String(), robots.Sum(content))
		if st.Route == "plugin" || st.Route == "unshadow" {
			fmt.Printf("# NOTE: %s manages robots.txt rules on this site; a physical file overrides them\n", f.SEOPlugin)
		}
		fmt.Print(content)
		return 0
	}
	switch st.Route {
	case "file":
	case "plugin", "unshadow":
		fmt.Fprintf(os.Stderr, "hs robots: %s serves robots.txt rules on %s; a physical file would override it — nothing written (hs check names the route)\n", f.SEOPlugin, d.Name)
		return 1
	default:
		fmt.Fprintf(os.Stderr, "hs robots: %s needs nothing (%s) — nothing written\n", d.Name, st.Kind)
		return 0
	}
	if want != "" && robots.Sum(content) != want {
		fmt.Fprintf(os.Stderr, "hs robots: the file would now be %s, not the planned %s — the site changed since the plan; plan again\n", robots.Sum(content), want)
		return 1
	}
	if exists && content == existing {
		fmt.Println(path + ": unchanged — nothing written")
		return 0
	}
	if exists {
		bak := conf.BackupPath(path, time.Now())
		if err := os.MkdirAll(filepath.Dir(bak), 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "hs robots:", err)
			return 1
		}
		if err := os.WriteFile(bak, []byte(existing), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "hs robots:", err)
			return 1
		}
		priv := "/home/" + d.User + "/web/" + d.Name + "/private/hs-backup-" + time.Now().Format("20060102")
		if err := asUser(d, []byte(existing), `umask 027; mkdir -p "$1" && cat > "$1/robots.txt.$2"`, priv, time.Now().Format("150405")); err != nil {
			fmt.Fprintln(os.Stderr, "hs robots: backup to private/:", err)
			return 1
		}
		fmt.Printf("previous file kept: %s/robots.txt.%s and %s\n", priv, time.Now().Format("150405"), bak)
	}
	if err := asUser(d, []byte(content), `umask 022; t=$(mktemp "$1/.robots.XXXXXX") && cat > "$t" && chmod 644 "$t" && mv -f "$t" "$1/robots.txt"`, d.DocRoot()); err != nil {
		fmt.Fprintln(os.Stderr, "hs robots: write:", err)
		return 1
	}
	removed, added := conf.LineDiff(existing, content)
	fmt.Printf("%s:\n", path)
	for _, l := range removed {
		fmt.Println("  - " + l)
	}
	for _, l := range added {
		fmt.Println("  + " + l)
	}
	return 0
}

// readAsUser reads a docroot file as the site's user; exists=false when
// there is none. A symlink or other non-regular file is refused.
func readAsUser(d hestia.Domain, path string) (string, bool, error) {
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !fi.Mode().IsRegular() {
		return "", false, fmt.Errorf("%s is not a regular file — not touching it", path)
	}
	out, err := exec.Command("runuser", "-u", d.User, "--", "cat", "--", path).Output()
	if err != nil {
		return "", false, fmt.Errorf("read %s as %s: %v", path, d.User, err)
	}
	return string(out), true, nil
}

func asUser(d hestia.Domain, stdin []byte, script string, args ...string) error {
	cmd := exec.Command("runuser", append([]string{"-u", d.User, "--", "sh", "-c", script, "sh"}, args...)...)
	cmd.Stdin = bytes.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
