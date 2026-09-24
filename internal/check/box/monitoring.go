package box

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/valolink/hestiascripts/internal/check"
)

func monitoringChecks() []Check {
	return []Check{
		{ID: "netdata", Section: "monitoring", Title: "Netdata", Run: checkNetdata},
		{ID: "streamer", Section: "monitoring", Title: "hestia-streamer", Run: checkStreamer},
		{ID: "wpcli", Section: "monitoring", Title: "WP-CLI", Run: checkWPCLI},
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
			Fixed("run.sh → 6 (Netdata) → 1")}
	}
	if !active(ctx, s, "netdata") {
		return []Result{New(Fail, "installed but not running").Fixed("systemctl start netdata")}
	}
	var rs []Result
	if !NetdataTuned(env) {
		rs = append(rs, New(Warn, "running on the stock profile").
			Because("Stock Netdata's memory footprint triggered the web1 OOM on 2026-07-06.").
			Fixed("run.sh → 6 (Netdata) → 3"))
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
	h := map[string]string{}
	if tok != "" {
		h["X-Streamer-Token"] = tok
	}
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	code, _, err := s.HTTPGet(hctx, "http://127.0.0.1:8091/netdata/alarms", h)
	cancel()
	switch {
	case err != nil:
		return []Result{New(Fail, "unit active but :8091 does not answer").Ev(err.Error()).Fixed("journalctl -u hestia-streamer -n 50")}
	case code == 403:
		return []Result{New(Fail, "rejects its own token").Ev("/etc/hestia-streamer.env token → HTTP 403").
			Fixed("systemctl restart hestia-streamer   # env file changed since start")}
	case tok == "":
		return []Result{New(Warn, "answering, but no token is configured").
			Because("Anyone the firewall lets through to :8091 can run the allowlisted scripts.").
			Fixed("bash install-scripts.sh   # generates /etc/hestia-streamer.env")}
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
			Because("Every v-wp-* script and EngineLink's site actions depend on it.").Fixed("run.sh → 2 (WP-CLI) → 1")}
	}
	v := WPCLIVersion(ctx, env)
	if v == "" {
		return []Result{New(Fail, "installed but wp --version fails").
			Because("Usually disabled PHP CLI functions after a PHP update.").Fixed("run.sh → 2 (WP-CLI) → 3")}
	}
	return []Result{New(OK, "WP-CLI "+v+" runs")}
}
