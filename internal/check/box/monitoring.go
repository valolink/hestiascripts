package box

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	. "github.com/valolink/hestiascripts/internal/check"
)

func monitoringChecks() []Check {
	return []Check{
		{ID: "netdata", Section: "monitoring", Title: "Netdata", Run: checkNetdata},
		{ID: "netdata.alarms", Section: "monitoring", Title: "Netdata alarms", Run: checkNetdataAlarms},
		{ID: "streamer", Section: "monitoring", Title: "hestia-streamer", Run: checkStreamer},
		{ID: "wpcli", Section: "monitoring", Title: "WP-CLI", Run: checkWPCLI},
		{ID: "updates.signing", Section: "security", Title: "Signed hestiascripts updates", Run: checkSigning},
	}
}

func NetdataTuned(env *Env) bool {
	return strings.Contains(readString(env.Sys, "/etc/netdata/netdata.conf"), "hestiascripts-tuned")
}

func checkNetdata(ctx context.Context, env *Env) []Result {
	s := env.Sys
	if !s.Have("netdata") {
		return []Result{New(Warn, "not installed").
			Because("EngineLink's server alarms and charts read Netdata through the streamer; without it the box is unmonitored.").
			Fixed("hs op netdata-install")}
	}
	if !active(ctx, s, "netdata") {
		return []Result{New(Fail, "installed but not running").Fixed("systemctl start netdata")}
	}
	var rs []Result
	if !NetdataTuned(env) {
		rs = append(rs, New(Warn, "running on the stock profile").
			Because("Stock Netdata's memory footprint triggered the web1 OOM on 2026-07-06.").
			Fixed("hs op netdata-tune"))
	}
	// :19999 should only ever answer on localhost — EngineLink reaches it
	// through the streamer's token-gated /netdata proxy.
	if out, err := s.Run(ctx, "iptables", "-S", "INPUT"); err == nil {
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "--dport 19999") && strings.Contains(l, "ACCEPT") {
				rs = append(rs, New(Warn, "port 19999 is open in the firewall").Ev(l).
					Because("The dashboard exposes process lists and system internals; the streamer proxy makes this rule unnecessary.").
					Fixed("remove the 19999 rule in Hestia → Server → Firewall"))
			}
		}
	}
	// Proof it works: the alarms API answers.
	hctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	code, _, err := s.HTTPGet(hctx, "http://127.0.0.1:19999/api/v1/alarms", nil)
	cancel()
	if err != nil || code != 200 {
		rs = append(rs, New(Fail, fmt.Sprintf("alarms API not answering (%v)", errOrCode(err, code))).
			Because("EngineLink's alarm polling returns nothing for this box.").Fixed("systemctl status netdata"))
	}
	if len(rs) > 0 {
		return rs
	}
	return []Result{New(OK, "running, low-footprint profile, alarms API answering on localhost")}
}

// NetdataHealthDir is where Netdata reads the operator's health files; a
// file there with a stock file's name takes the stock file's place.
const NetdataHealthDir = "/etc/netdata/health.d"

// HSAlarmFiles are the health files hs installs from templates/netdata/health.d/:
// swap.conf replaces the stock one (its used_swap fired on how full swap is —
// kuumalahde 2026-09-30, 94 % parked, nothing moving), hs-pressure.conf adds
// alarms on PSI. The names are what EngineLink's alarm list shows.
var (
	HSAlarmFiles = []string{"swap.conf", "hs-pressure.conf"}
	HSAlarmNames = []string{"hs_ram_pressure", "hs_ram_stall", "hs_swap_io", "hs_cpu_pressure"}
)

// NetdataAlarmNames lists every alarm the running Netdata has loaded
// (raised or not), from /api/v1/alarms?all.
func NetdataAlarmNames(ctx context.Context, env *Env) ([]string, error) {
	hctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	code, body, err := env.Sys.HTTPGet(hctx, "http://127.0.0.1:19999/api/v1/alarms?all", nil)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("HTTP %d", code)
	}
	var v struct {
		Alarms map[string]struct{ Name string } `json:"alarms"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	var names []string
	for _, a := range v.Alarms {
		names = append(names, a.Name)
	}
	sort.Strings(names)
	return names, nil
}

// Green needs the alarms loaded in the running Netdata, not files on disk.
func checkNetdataAlarms(ctx context.Context, env *Env) []Result {
	s := env.Sys
	if !s.Have("netdata") {
		return []Result{New(NA, "Netdata is not installed")}
	}
	var missing []string
	for _, f := range HSAlarmFiles {
		if same, ok := sameFile(s, env.RepoDir+"/templates/netdata/health.d/"+f, NetdataHealthDir+"/"+f); !ok || !same {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return []Result{New(Warn, "alarms fire on swap fill level, not on pressure").
			Ev("not installed or differs from the repo: " + NetdataHealthDir + "/" + strings.Join(missing, ", ")).
			Because("Stock Netdata warns when the swap file is nearly full — pages parked during an old peak, harmless while nothing moves (kuumalahde 2026-09-30) — and says nothing when tasks actually stall on memory or the box swaps under load.").
			Fixed("hs op netdata-alarms")}
	}
	if !active(ctx, s, "netdata") {
		return []Result{New(Configured, "alarm files installed; Netdata is not running to load them")}
	}
	names, err := NetdataAlarmNames(ctx, env)
	if err != nil {
		return []Result{New(Configured, "alarm files installed; the alarms API did not answer to confirm them").Ev(err.Error())}
	}
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	var loaded, absent []string
	for _, n := range HSAlarmNames {
		if have[n] {
			loaded = append(loaded, n)
		} else {
			absent = append(absent, n)
		}
	}
	ev := []string{fmt.Sprintf("%d alarms loaded; hs: %s", len(names), strings.Join(loaded, ", "))}
	if have["used_swap"] {
		return []Result{New(Warn, "the stock used_swap alarm is still loaded").Ev(ev...).
			Because("The installed health.d/swap.conf should have taken the stock file's place; Netdata has not re-read its health configuration.").
			Fixed("netdatacli reload-health (or hs op netdata-alarms)")}
	}
	if len(loaded) == 0 {
		return []Result{New(Warn, "hs alarm files installed but none of their alarms is loaded").Ev(ev...).
			Because("Netdata has not re-read its health configuration since the files were written, or the charts they watch do not exist here (PSI needs a kernel with /proc/pressure).").
			Fixed("netdatacli reload-health (or hs op netdata-alarms)")}
	}
	if len(absent) > 0 {
		// The charts PSI alarms watch exist only on kernels with PSI on.
		return []Result{New(Configured, "some hs alarms are not loaded: "+strings.Join(absent, ", ")).Ev(ev...).
			Ev("their charts are missing on this box (system.*_pressure needs /proc/pressure) — swap and the rest are covered")}
	}
	return []Result{New(OK, "pressure alarms loaded, stock used_swap off").Ev(ev...).Valid(24 * time.Hour)}
}

func errOrCode(err error, code int) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("HTTP %d", code)
}

// StreamerToken reads HESTIA_STREAMER_TOKEN from the unit's env file.
func StreamerToken(env *Env) string {
	for _, l := range strings.Split(readString(env.Sys, "/etc/hestia-streamer.env"), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "HESTIA_STREAMER_TOKEN="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

// Green needs the streamer to answer an authenticated request, not just a
// running unit. /netdata/alarms is the cheapest endpoint that runs nothing.
func checkStreamer(ctx context.Context, env *Env) []Result {
	s := env.Sys
	if !active(ctx, s, "hestia-streamer") {
		return []Result{New(Fail, "not running").
			Because("EngineLink cannot run anything on this box: no health data, updates or operate actions.").
			Fixed("systemctl status hestia-streamer ; bash install-scripts.sh")}
	}
	tok := StreamerToken(env)
	if tok == "" {
		return []Result{New(Fail, "no token configured").
			Because("The streamer refuses every request without one (it fails closed since the 2026-09 hardening), so EngineLink cannot reach this box.").
			Fixed("bash install-scripts.sh   # generates /etc/hestia-streamer.env, then restarts the streamer")}
	}
	h := map[string]string{"X-Streamer-Token": tok}
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	code, _, err := s.HTTPGet(hctx, "http://127.0.0.1:8091/netdata/alarms", h)
	cancel()
	switch {
	case err != nil:
		return []Result{New(Fail, "unit active but :8091 does not answer").Ev(err.Error()).Fixed("journalctl -u hestia-streamer -n 50")}
	case code == 403:
		return []Result{New(Fail, "rejects its own token").Ev("/etc/hestia-streamer.env token → HTTP 403").
			Fixed("systemctl restart hestia-streamer   # env file changed since start")}
	}
	ours, _ := LinkedScripts(env)
	return []Result{New(OK, fmt.Sprintf("answering with token (HTTP %d via /netdata/alarms)", code)).
		Ev(fmt.Sprintf("%d v-scripts linked from the repo", ours))}
}

// LinkedScripts counts v-* symlinks in Hestia's bin: those resolving into
// this repo, and all symlinks (the number setup-status has always reported).
func LinkedScripts(env *Env) (ours, links int) {
	scripts, _ := env.Sys.Glob("/usr/local/hestia/bin/v-*")
	for _, p := range scripts {
		fi, err := env.Sys.Lstat(p)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		links++
		if t, err := env.Sys.EvalSymlinks(p); err == nil && env.RepoDir != "" && strings.HasPrefix(t, env.RepoDir+"/") {
			ours++
		}
	}
	return ours, links
}

// WPCLIVersion returns wp --version ("" when missing).
func WPCLIVersion(ctx context.Context, env *Env) string {
	if !env.Sys.Have("wp") {
		return ""
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := env.Sys.Run(wctx, "wp", "--version", "--allow-root")
	if err != nil {
		return ""
	}
	f := strings.Fields(out)
	if len(f) >= 2 {
		return f[1]
	}
	return ""
}

func checkWPCLI(ctx context.Context, env *Env) []Result {
	if !env.Sys.Have("wp") {
		return []Result{New(Fail, "not installed").
			Because("Every v-wp-* script and EngineLink's site actions depend on it.").Fixed("hs op wpcli")}
	}
	v := WPCLIVersion(ctx, env)
	if v == "" {
		return []Result{New(Fail, "installed but wp --version fails").
			Because("Usually disabled PHP CLI functions after a PHP update.").Fixed("hs op php-cli-functions")}
	}
	return []Result{New(OK, "WP-CLI "+v+" runs")}
}

const allowedSigners = "/etc/hs/allowed_signers"

// v-hestiascripts-update refuses a commit that is not signed by a key in
// allowedSigners (a file on the box, outside the repo), so push access to the
// repository is not root on every box (hardening plan §13).
func checkSigning(ctx context.Context, env *Env) []Result {
	s := env.Sys
	if strings.TrimSpace(readString(s, allowedSigners)) == "" {
		return []Result{New(Warn, "signed updates are not set up").
			Because("Without " + allowedSigners + ", v-hestiascripts-update (EngineLink's update button) refuses to install anything; updating needs a root terminal and --allow-unsigned.").
			Fixed("put the signing key(s) in " + allowedSigners + " (\"email ssh-ed25519 AAAA…\" per line) and sign commits")}
	}
	if env.RepoDir == "" {
		return []Result{New(Configured, "trusted signers configured; checkout not found to verify")}
	}
	vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := s.Run(vctx, "git", "-C", env.RepoDir, "-c", "gpg.format=ssh", "-c", "gpg.ssh.allowedSignersFile="+allowedSigners, "verify-commit", "HEAD")
	if err != nil {
		return []Result{New(Warn, "the installed checkout's HEAD is not signed by a trusted key").Ev(strings.TrimSpace(out)).
			Because("Code running as root here was not verified — pulled by hand, or signed by an unknown key.").
			Fixed("git -C " + env.RepoDir + " log --show-signature -1")}
	}
	return []Result{New(OK, "checkout HEAD signed by a trusted key").Ev(strings.TrimSpace(out))}
}
