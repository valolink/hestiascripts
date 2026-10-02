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
		ID: "staging-uploads", Title: "Mount live's media read-only into staging", Check: "site.staging-uploads", Scope: "site", Risk: Change,
		Applies: func(r check.Result) bool {
			return (r.State == check.Fail || r.State == check.Warn) && !strings.Contains(r.Summary, "old uploads layout")
		},
		Note: "soutuveneet and delicatessen 2026-09-30: staging copies from before the 2026-09-25 fstab line lost the mount at a reboot — staging had no images. The layout of 2026-10-02 binds live's media folders one by one into the copy's own uploads; a copy in the older layout (all of uploads one bind) is rebuilt with v-wp-staging-create --uploads-only.",
		How: "For each of live's media folders (past years, the current year's past months, ShortpixelBackups) that is not mounted as v-wp-staging-create leaves it — and that the copy holds no files of its own in: " +
			"`mount --bind` live's folder onto the copy's, `mount -o remount,ro,bind`, `mount --make-private`, and a write probe as root that unmounts again if the mount is writable; " +
			"then its tagged `bind,ro,private,nofail` line in /etc/fstab (an earlier line for the same mount point replaced, the previous fstab kept under /var/lib/hs/backups), `systemctl daemon-reload` and `findmnt --verify`. " +
			"Every path on the way is refused if it is a symlink — root must not mount over a place a site user points it at.",
		Undo: "umount the folder in the copy's uploads and delete its `# hestia-staging-uploads` line from /etc/fstab (or restore the fstab backup shown in the plan).",
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
			live, uploads := c.SourceUploads(), c.Uploads()
			if fi, err := env.Sys.Stat(live); err != nil || !fi.IsDir() {
				return nil, fmt.Errorf("live's uploads %s is not a directory", live)
			}
			if copies.MountAt(env.Sys, uploads).Mounted {
				return nil, fmt.Errorf("%s is in the old layout (all of live's uploads one bind): rebuild it with v-wp-staging-create --uploads-only --src-user=%s --src-domain=%s --dest-user=%s --new-domain=%s", subject, c.SourceUser, c.SourceDomain, c.User, c.Domain)
			}
			root := strings.TrimSuffix(uploads, "/public_html/wp-content/uploads")
			guard := []string{root + "/public_html", root + "/public_html/wp-content", uploads}
			for _, q := range guard {
				if fi, err := env.Sys.Lstat(q); err == nil && !fi.Mode().IsDir() {
					return nil, fmt.Errorf("%s is not a plain directory (symlink?) — not mounting there", q)
				}
			}
			var steps, fstab []Step
			for _, rel := range copies.MediaBinds(env.Sys, live, env.Sys.Now()) {
				src, dst := live+"/"+rel, uploads+"/"+rel
				m := copies.MountAt(env.Sys, dst)
				if !m.Mounted {
					if es, _ := env.Sys.ReadDir(dst); len(es) > 0 {
						continue // the copy's own files: copied while it was the current month
					}
				}
				if m.Mounted && (!m.ReadOnly || m.Root != src) {
					steps = append(steps, Step{Why: "drop the wrong mount on " + rel + " (" + m.Root + ")", Argv: []string{"umount", dst}})
				}
				if !m.Mounted || !m.ReadOnly || m.Root != src {
					if strings.Contains(rel, "/") {
						guard = append(guard, uploads+"/"+strings.SplitN(rel, "/", 2)[0])
					}
					guard = append(guard, dst)
					steps = append(steps,
						Step{Why: rel + ": the mount point, the copy's", Argv: []string{"install", "-d", "-o", c.User, "-g", c.User, dst}},
						Step{Why: rel + ": bind live's folder", Argv: []string{"mount", "--bind", src, dst}},
						Step{Why: rel + ": read-only", Argv: []string{"mount", "-o", "remount,ro,bind", dst}},
						Step{Why: rel + ": private", Argv: []string{"mount", "--make-private", dst}},
						Step{Why: rel + ": prove it — a root write must fail (else unmount and stop)",
							Argv: []string{"sh", "-c", `p="$1/.hestia-ro-probe-$$"; if touch "$p" 2>/dev/null; then unlink "$p"; umount -l "$1"; echo "writable — unmounted again"; exit 1; fi; echo "read-only: a root write was refused"`, "sh", dst}},
					)
				} else if m.Shared {
					steps = append(steps, Step{Why: rel + ": private, so mounts made inside it cannot reach live's uploads", Argv: []string{"mount", "--make-private", dst}})
				}
				if copies.FstabLine(env.Sys, dst) != copies.WantBindFstab(c, rel) {
					fstab = append(fstab, Step{Why: rel + ": its tagged line, replacing any earlier one for this mount point:\n    " + copies.WantBindFstab(c, rel),
						Argv: []string{"sh", "-c", `sed -i "\#[[:space:]]$2[[:space:]]#d" /etc/fstab && printf '%s %s none bind,ro,private,nofail 0 0 %s %s\n' "$1" "$2" "` + copies.FstabTag + `" "$2" >> /etc/fstab`, "sh", src, dst}})
				}
			}
			if len(steps) == 0 && len(fstab) == 0 {
				return nil, fmt.Errorf("%s: live's media is already mounted read-only and persisted", subject)
			}
			noLinks := Step{Why: "refuse if any path on the way became a symlink",
				Argv: append([]string{"sh", "-c", `for p in "$@"; do if [ -L "$p" ]; then echo "$p is a symlink — refusing"; exit 1; fi; done; echo "no symlinks on the way"`, "sh"}, guard...)}
			steps = append([]Step{noLinks}, steps...)
			if len(fstab) > 0 {
				steps = append(steps, Step{Why: "keep the current /etc/fstab", Argv: []string{"install", "-D", "-m", "600", "/etc/fstab", conf.BackupPath("/etc/fstab", time.Now())}})
				steps = append(steps, fstab...)
				steps = append(steps,
					Step{Why: "let systemd read the new fstab", Argv: []string{"systemctl", "daemon-reload"}},
					Step{Why: "check /etc/fstab before the next reboot", Argv: []string{"findmnt", "--verify", "--tab-file", "/etc/fstab"}},
				)
			}
			return append(steps, Step{Why: "the mounts now", Argv: []string{"findmnt", "-R", uploads}}), nil
		},
	})
}
