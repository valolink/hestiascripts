package box

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	. "github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/sys"
)

func backupChecks() []Check {
	return []Check{
		{ID: "backups.setup", Section: "backups", Title: "Backup setup", Run: checkBackupSetup},
		{ID: "backups.nightly", Section: "backups", Title: "Nightly backup", Timeout: 25 * time.Second, Run: checkNightly},
	}
}

const resticCron = "/etc/cron.d/hestia-restic"

// ResticRepoFor joins the system repo base and a user the way upstream does
// ("${REPO%/}/$user", HestiaCP ≥ 1.10.4). Plain concatenation breaks when the
// stored REPO has no trailing slash — which is how ≥ 1.10.4 stores it.
func ResticRepoFor(base, user string) string {
	return strings.TrimSuffix(base, "/") + "/" + user
}

// A repo base with an empty path ("rclone:storagebox:") resolves to an
// absolute path the Storage Box chroot does not map to the same place as the
// relative one — every user fails "Unable to access restic repo" (hzweb1
// 2026-09-07).
func repoPathEmpty(base string) bool {
	i := strings.LastIndex(base, ":")
	if i < 0 {
		return false
	}
	p := strings.Trim(base[i+1:], "/")
	return p == ""
}

type BackupInfo struct {
	User    string
	Nightly time.Time // newest non-hourly snapshot or tarball
	Hourly  time.Time // newest db-hourly snapshot
	Source  string    // "restic", "tarball", "home", ""
	Keyed   bool      // the user is enrolled in restic
	Err     string    // restic error, when it fell back
}

type snapshot struct {
	Time  time.Time `json:"time"`
	Tags  []string  `json:"tags"`
	Paths []string  `json:"paths"`
}

// BackupAges finds the newest backup per user: restic first (one
// `snapshots --latest 1` call returns the newest per group, so nightly and
// db-hourly come back together), then /backup/<user>.*.tar, then
// /home/<user>/backup. Restic calls are network round-trips, so they run in
// parallel, each bounded — EngineLink gives the whole health fetch 30 s.
func BackupAges(ctx context.Context, env *Env) []BackupInfo {
	s := env.Sys
	repo := hestia.ResticRepo(s)
	users := hestia.Users(s)
	out := make([]BackupInfo, len(users))
	var wg sync.WaitGroup
	for i, u := range users {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			bi := BackupInfo{User: u}
			key := hestia.UserResticKey(u)
			if repo != "" && exists(s, key) && s.Have("restic") {
				rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				raw, err := s.Run(rctx, "restic", "--repo", ResticRepoFor(repo, u), "--password-file", key,
					"-o", rcloneArgs, "--no-lock", "--json", "snapshots", "--latest", "1")
				cancel()
				var snaps []snapshot
				if err == nil && json.Unmarshal([]byte(raw), &snaps) == nil {
					for _, sn := range snaps {
						if contains(sn.Tags, "db-hourly") {
							if sn.Time.After(bi.Hourly) {
								bi.Hourly = sn.Time
							}
						} else if sn.Time.After(bi.Nightly) {
							bi.Nightly = sn.Time
						}
					}
					if !bi.Nightly.IsZero() {
						bi.Source = "restic"
					}
				} else if err != nil {
					bi.Err = resticError(raw, err)
				}
				bi.Keyed = true
			}
			if bi.Nightly.IsZero() {
				tars, _ := s.Glob("/backup/" + u + ".*.tar")
				for _, t := range tars {
					if fi, err := s.Stat(t); err == nil && fi.ModTime().After(bi.Nightly) {
						bi.Nightly, bi.Source = fi.ModTime(), "tarball"
					}
				}
			}
			if bi.Nightly.IsZero() {
				out, _ := s.Run(ctx, "find", "/home/"+u+"/backup", "-type", "f", "-printf", "%T@\n")
				for _, l := range nonEmpty(out) {
					var sec float64
					fmt.Sscanf(l, "%f", &sec)
					if t := time.Unix(int64(sec), 0); t.After(bi.Nightly) {
						bi.Nightly, bi.Source = t, "home"
					}
				}
			}
			out[i] = bi
		}(i, u)
	}
	wg.Wait()
	return out
}

// Storage Boxes ban an IP after repeated failed logins, and rclone's default
// pacer retries an auth failure ~10 times — one bad probe reads as ten failed
// logins upstream (hzdemolink, 2026-09-07). A check must never add to that.
const rcloneArgs = "rclone.args=serve restic --stdio --b2-hard-delete --retries 1 --low-level-retries 1"

// resticError keeps restic's own fatal message (JSON exit_error on stdout)
// rather than a bare "exit status 1".
func resticError(raw string, err error) string {
	lines := nonEmpty(raw)
	if ee, ok := err.(*sys.ExitError); ok {
		lines = append(lines, nonEmpty(ee.Stderr)...) // restic writes exit_error to stderr
	}
	// rclone's CRITICAL line names the real cause ("connection refused" is a
	// Storage Box login ban); restic's exit_error only says it could not open.
	var cause, fatal string
	for _, l := range lines {
		var m struct {
			Type    string `json:"message_type"`
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(l), &m) == nil && m.Type == "exit_error" {
			fatal = m.Message
		} else if i := strings.Index(l, "CRITICAL: "); i >= 0 {
			cause = l[i+len("CRITICAL: "):]
		}
	}
	if fatal != "" && cause != "" {
		return fatal + " — " + cause
	}
	if fatal != "" {
		return fatal
	}
	if len(lines) > 0 {
		return lines[len(lines)-1]
	}
	return err.Error()
}

func checkBackupSetup(ctx context.Context, env *Env) []Result {
	s := env.Sys
	repo := hestia.ResticRepo(s)
	if repo == "" {
		return []Result{New(Warn, "restic is not set up — tarball backups only").
			Because("Tarballs are full copies: slower, larger, kept on the same provider, and no hourly database snapshots.").
			Fixed("setup-restic-backup.sh --host <storagebox> --user <u> --password   # SSH only")}
	}
	var rs []Result
	if repoPathEmpty(repo) {
		rs = append(rs, New(Fail, "restic repo is registered with an empty path: "+repo).
			Because("The per-user path resolves outside the Storage Box chroot, so every user's backup fails to open the repo.").
			Fixed("re-run setup-restic-backup.sh with a real --path (the default does)"))
	}
	var unkeyed []string
	users := hestia.Users(s)
	for _, u := range users {
		if !exists(s, hestia.UserResticKey(u)) {
			unkeyed = append(unkeyed, u)
		}
	}
	if len(unkeyed) > 0 {
		rs = append(rs, New(Warn, fmt.Sprintf("%d of %d users have no restic key", len(unkeyed), len(users))).
			Ev(joinMax(unkeyed, 10)).
			Because("Users without a key are not in the restic backup.").
			Fixed("v-server-backup-guard   # re-asserts per-user incremental backups"))
	}
	cron := readString(s, resticCron)
	switch {
	case cron == "":
		rs = append(rs, New(Fail, "restic is registered but nothing schedules it").
			Ev(resticCron+" is missing").
			Because("Upstream ships v-backup-users-restic but no cron entry — without ours, every snapshot is a hand-run.").
			Fixed("setup-restic-backup.sh (re-run installs the schedule)"))
	case !strings.Contains(cron, "v-server-backup-db-hourly") && len(hestia.UsersWithDatabases(s)) > 0:
		rs = append(rs, New(Warn, "hourly database snapshots are not scheduled").
			Because("An order-taking site can lose up to a day of orders between nightlies.").
			Fixed("setup-restic-backup.sh (without --no-hourly)"))
	}
	if !strings.Contains(cron, "v-server-backup-guard") && cron != "" {
		rs = append(rs, New(Warn, "backup guard is not scheduled").
			Because("New users under a package with incremental backups off would silently never back up.").
			Fixed("setup-restic-backup.sh (re-run installs the schedule)"))
	}
	if len(rs) > 0 {
		return rs
	}
	return []Result{New(Configured, fmt.Sprintf("restic registered, %d users keyed, nightly + hourly scheduled", len(users))).
		Ev("repo " + repo)}
}

func checkNightly(ctx context.Context, env *Env) []Result {
	now := env.Sys.Now()
	hourlyExpected := strings.Contains(readString(env.Sys, resticCron), "v-server-backup-db-hourly")
	dbUsers := map[string]bool{}
	for _, u := range hestia.UsersWithDatabases(env.Sys) {
		dbUsers[u] = true
	}
	var rs []Result
	infos := BackupAges(ctx, env)
	sort.Slice(infos, func(i, j int) bool { return infos[i].User < infos[j].User })
	for _, bi := range infos {
		var r Result
		if bi.Nightly.IsZero() {
			r = New(Fail, "no backup found").
				Because("A restore request for this user would have nothing to restore from.").
				Fixed("v-backup-user " + bi.User + "   # then check the schedule")
			if bi.Err != "" {
				r = r.Ev("restic: " + bi.Err)
			}
		} else {
			age := now.Sub(bi.Nightly)
			ev := fmt.Sprintf("%s, %s (%dh ago)", bi.Source, bi.Nightly.Local().Format("2006-01-02 15:04"), hours(age))
			switch {
			case age > 48*time.Hour:
				r = New(Fail, fmt.Sprintf("newest backup is %dh old", hours(age))).Ev(ev).
					Because("A restore today would lose everything since then.").
					Fixed("journalctl -t hestia-restic ; v-backup-user-restic " + bi.User)
			case age > 26*time.Hour:
				r = New(Warn, fmt.Sprintf("newest backup is %dh old — last night's run missed it", hours(age))).Ev(ev).
					Fixed("check /var/log/hestia/backup.log and the nightly cron")
			default:
				r = New(OK, fmt.Sprintf("%s snapshot %dh ago", bi.Source, hours(age))).Ev(ev).Valid(26 * time.Hour)
			}
			if bi.Keyed && bi.Source != "restic" {
				// A fresh tarball must not hide a broken restic enrolment.
				r.Evidence = append(r.Evidence, "restic: "+orDash(bi.Err))
				if r.State < Warn {
					r.State = Warn
					r.Summary = fmt.Sprintf("restic unreachable — only a %s from %dh ago", bi.Source, hours(age))
					r.Why = "The user is enrolled in restic but the repository cannot be read, so off-site incremental backups are not happening."
					r.Fix = "check Storage Box reachability from this box (nc -vz <host> 23); refused from one IP only = a login ban"
				}
			}
		}
		rs = append(rs, r.For(bi.User))

		if hourlyExpected && dbUsers[bi.User] && bi.Source == "restic" {
			age := now.Sub(bi.Hourly)
			switch {
			case bi.Hourly.IsZero():
				rs = append(rs, New(Warn, "no hourly database snapshot yet").For(bi.User+" db-hourly").
					Because("Orders placed since the nightly exist only in the live database.").
					Fixed("v-server-backup-db-hourly --user "+bi.User+" --dry-run"))
			case age > 2*time.Hour:
				rs = append(rs, New(Warn, fmt.Sprintf("last hourly database snapshot %dh ago", hours(age))).For(bi.User+" db-hourly").
					Fixed("tail /var/log/syslog | grep db-hourly ; v-server-backup-db-hourly --user "+bi.User))
			default:
				rs = append(rs, New(OK, fmt.Sprintf("hourly database snapshot %dm ago", int(age.Minutes()))).
					For(bi.User+" db-hourly").Valid(2*time.Hour))
			}
		}
	}
	if len(rs) == 0 {
		return []Result{New(NA, "no Hestia users")}
	}
	return rs
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
