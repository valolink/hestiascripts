package fix

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/conf"
	"github.com/valolink/hestiascripts/internal/copies"
)

func init() {
	register(Fix{
		ID: "staging-uploads", Title: "Mount live's uploads read-only into staging", Check: "site.staging-uploads", Scope: "site", Risk: Change,
		Applies: func(r check.Result) bool {
			return r.State == check.Fail || r.State == check.Warn && strings.Contains(r.Summary, "reboot")
		},
		Note: "soutuveneet and delicatessen 2026-09-30: staging copies from before the 2026-09-25 fstab line lost the mount at a reboot — staging had its own empty uploads.",
		How: "The same two steps v-wp-staging-create runs (bind_mount_uploads_ro, persist_uploads_mount): `mount --bind` live's wp-content/uploads onto staging's, `mount -o remount,ro,bind`, and a write probe as root that unmounts again if the mount is writable; " +
			"then the tagged `bind,ro,nofail` line in /etc/fstab (an earlier line for the same mount point replaced, the previous fstab kept under /var/lib/hs/backups), `systemctl daemon-reload` and `findmnt --verify`. " +
			"Whatever staging's own uploads folder holds stays underneath the mount, hidden, not deleted. Every path on the way is refused if it is a symlink — root must not mount over a place a site user points it at.",
		Undo: "umount the staging uploads path and delete its `# hestia-staging-uploads` line from /etc/fstab (or restore the fstab backup shown in the plan).",
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			var c copies.Copy
			for _, x := range copies.List(env.Sys) {
				if x.Domain == subject && x.Kind == "staging" {
					c = x
				}
			}
			switch {
			case c.Domain == "":
				return nil, fmt.Errorf("%s is not a staging copy on this box (v-wp-copies lists them)", subject)
			case c.SourceDomain == "" || c.SourceUser == "":
				return nil, fmt.Errorf("%s: the live site is unknown — register it first: v-wp-copies --mark --domain=%s --source-domain=LIVE", subject, subject)
			}
			src, dst := c.SourceUploads(), c.Uploads()
			if fi, err := env.Sys.Stat(src); err != nil || !fi.IsDir() {
				return nil, fmt.Errorf("live's uploads %s is not a directory", src)
			}
			// Every component a site user controls, live's and staging's.
			guard := []string{}
			for _, p := range []string{src, dst} {
				root := strings.TrimSuffix(p, "/public_html/wp-content/uploads")
				for _, q := range []string{root + "/public_html", root + "/public_html/wp-content", p} {
					if fi, err := env.Sys.Lstat(q); err == nil && !fi.Mode().IsDir() {
						return nil, fmt.Errorf("%s is not a plain directory (symlink?) — not mounting there", q)
					}
					guard = append(guard, q)
				}
			}
			noLinks := Step{Why: "refuse if any path on the way became a symlink",
				Argv: append([]string{"sh", "-c", `for p in "$@"; do if [ -L "$p" ]; then echo "$p is a symlink — refusing"; exit 1; fi; done; echo "no symlinks on the way"`, "sh"}, guard...)}
			var steps []Step
			m := copies.MountAt(env.Sys, dst)
			if !m.Mounted || !m.ReadOnly || (m.Root != "" && m.Root != src) {
				steps = append(steps, noLinks)
				if m.Mounted {
					steps = append(steps, Step{Why: "drop the wrong mount first (" + m.Root + ")", Argv: []string{"umount", dst}})
				}
				if _, err := env.Sys.Stat(dst); err != nil {
					steps = append(steps, Step{Why: "staging's uploads directory is missing", Argv: []string{"install", "-d", "-o", c.User, "-g", c.User, "-m", "755", dst}})
				}
				steps = append(steps,
					Step{Why: "bind live's uploads onto staging's (its own files stay underneath, hidden)", Argv: []string{"mount", "--bind", src, dst}},
					Step{Why: "make the bind read-only", Argv: []string{"mount", "-o", "remount,ro,bind", dst}},
					Step{Why: "prove it: a root write must fail (else unmount and stop)",
						Argv: []string{"sh", "-c", `p="$1/.hestia-ro-probe-$$"; if touch "$p" 2>/dev/null; then rm -f "$p"; umount -l "$1"; echo "writable — unmounted again"; exit 1; fi; echo "read-only: a root write was refused"`, "sh", dst}},
				)
			}
			if copies.FstabLine(env.Sys, dst) != copies.WantFstab(c) {
				bak := conf.BackupPath("/etc/fstab", time.Now())
				steps = append(steps,
					Step{Why: "keep the current /etc/fstab", Argv: []string{"install", "-D", "-m", "600", "/etc/fstab", bak}},
					Step{Why: "the tagged line, replacing any earlier one for this mount point:\n    " + copies.WantFstab(c),
						Argv: []string{"sh", "-c", `sed -i "\#[[:space:]]$2[[:space:]]#d" /etc/fstab && printf '%s %s none bind,ro,nofail 0 0 %s %s\n' "$1" "$2" "` + copies.FstabTag + `" "$2" >> /etc/fstab`, "sh", src, dst}},
					Step{Why: "let systemd read the new fstab", Argv: []string{"systemctl", "daemon-reload"}},
					Step{Why: "check /etc/fstab before the next reboot", Argv: []string{"findmnt", "--verify", "--tab-file", "/etc/fstab"}},
				)
			}
			if len(steps) == 0 {
				return nil, fmt.Errorf("%s: live's uploads are already mounted read-only and persisted", subject)
			}
			return append(steps, Step{Why: "the mount now", Argv: []string{"findmnt", "--target", dst}}), nil
		},
	})
}
