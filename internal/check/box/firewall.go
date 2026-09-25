package box

// The firewall checks Hestia's own view misses: IPv6 (Hestia's firewall is
// IPv4-only), who can reach SSH and the panel, and outbound traffic.

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	. "github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/fwgen"
)

func firewallChecks() []Check {
	return []Check{
		{ID: "firewall.ipv6", Section: "security", Title: "IPv6 firewall", Run: checkIPv6},
		{ID: "panel.exposure", Section: "security", Title: "SSH and panel exposure", Run: checkPanelExposure},
		{ID: "egress", Section: "security", Title: "Outbound traffic", Timeout: 20 * time.Second, Run: checkEgress},
	}
}

// listening6 lists [::]/* listeners that are not loopback: port → process.
func listening6(ctx context.Context, env *Env) map[string]string {
	out, _ := env.Sys.Run(ctx, "ss", "-Hltnp")
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		local := f[3]
		if !(strings.HasPrefix(local, "[::]:") || strings.HasPrefix(local, "*:") || (strings.HasPrefix(local, "[") && !strings.HasPrefix(local, "[::1]"))) {
			continue
		}
		port := local[strings.LastIndex(local, ":")+1:]
		proc := ""
		if len(f) > 5 {
			if i := strings.Index(f[5], `(("`); i >= 0 {
				proc = strings.SplitN(f[5][i+3:], `"`, 2)[0]
			}
		}
		m[port] = proc
	}
	return m
}

func rulesConf(env *Env) []fwgen.Rule {
	return fwgen.ParseRules(readString(env.Sys, fwgen.RulesConf))
}

func firewallConf(env *Env) fwgen.Config {
	return fwgen.ParseConfig(readString(env.Sys, fwgen.ConfPath))
}

func checkIPv6(ctx context.Context, env *Env) []Result {
	s := env.Sys
	addr, _ := s.Run(ctx, "ip", "-6", "addr", "show", "scope", "global")
	if !strings.Contains(addr, "inet6") {
		return []Result{New(NA, "no global IPv6 address")}
	}
	in6, _ := s.Run(ctx, "ip6tables", "-S", "INPUT")
	if strings.Contains(in6, "-j HS_IN6") {
		return []Result{New(OK, "IPv6 inbound mirrors Hestia's public rules (hs firewall)").Ev(strings.TrimSpace(in6))}
	}
	if strings.Contains(in6, "-P INPUT DROP") {
		return []Result{New(Configured, "IPv6 INPUT drops by default (not managed by hs)").Ev(strings.TrimSpace(in6))}
	}
	public := fwgen.PublicPorts(rulesConf(env))["tcp"]
	pub := map[string]bool{}
	for _, p := range public {
		pub[p] = true
	}
	var exposed []string
	for port, proc := range listening6(ctx, env) {
		if !pub[port] {
			exposed = append(exposed, port+" ("+proc+")")
		}
	}
	sort.Strings(exposed)
	r := New(Fail, "IPv6 bypasses the firewall").Ev("ip6tables INPUT: policy ACCEPT, no rules")
	if len(exposed) > 0 {
		r = r.Ev("reachable over IPv6 although Hestia's IPv4 rules do not open them: " + strings.Join(exposed, ", "))
	}
	return []Result{r.
		Because("Hestia's firewall only writes IPv4 rules. Everything listening on [::] — panel, SSH, Netdata, the streamer — answers to the whole IPv6 internet whatever the IPv4 rules say (hzdemolink 2026-09-25: panel and Netdata reachable).").
		Fixed("hs op firewall-ipv6")}
}

func checkPanelExposure(ctx context.Context, env *Env) []Result {
	var rs []Result
	for _, r := range rulesConf(env) {
		if !r.Public() {
			continue
		}
		for _, p := range strings.Split(r.Port, ",") {
			switch strings.TrimSpace(p) {
			case "8083":
				rs = append(rs, New(Warn, "panel (:8083) open to every address").For("panel").Ev(fmt.Sprintf("rule %s %s", r.ID, r.Comment)).
					Because("The next panel bug is the web-terminal story again: an unauthenticated flaw reachable by the whole internet.").
					Fixed("hs op panel-restrict"))
			case "22":
				rs = append(rs, New(Warn, "SSH open to every address").For("ssh").Ev(fmt.Sprintf("rule %s %s", r.ID, r.Comment)).
					Because("Continuous brute force in every auth log; one weak key or password away from root.").
					Fixed("hs op panel-restrict"))
			}
		}
	}
	if len(rs) == 0 {
		return []Result{New(OK, "SSH and panel limited to listed addresses")}
	}
	return rs
}

// egressLog summarises the hs-egress log lines of the last days: what the
// filter logged (log mode: what it would drop).
func egressLog(ctx context.Context, env *Env) (n int, top []string) {
	out, _ := env.Sys.Run(ctx, "journalctl", "-k", "--since=-7d", "--no-pager", "-o", "cat", "--grep", strings.TrimSpace(fwgen.LogPrefix))
	count := map[string]int{}
	for _, l := range strings.Split(out, "\n") {
		if !strings.Contains(l, strings.TrimSpace(fwgen.LogPrefix)) {
			continue
		}
		n++
		var dst, dpt, uid string
		for _, f := range strings.Fields(l) {
			switch {
			case strings.HasPrefix(f, "DST="):
				dst = f[4:]
			case strings.HasPrefix(f, "DPT="):
				dpt = f[4:]
			case strings.HasPrefix(f, "UID="):
				uid = f[4:]
			}
		}
		if ip := net.ParseIP(dst); ip != nil && ip.IsPrivate() {
			dst += " (private)"
		}
		count[fmt.Sprintf("port %s → %s (uid %s)", dpt, dst, uid)]++
	}
	for k, c := range count {
		top = append(top, fmt.Sprintf("%5d× %s", c, k))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(top)))
	if len(top) > 12 {
		top = append(top[:12], fmt.Sprintf("… %d more destinations", len(top)-12))
	}
	return n, top
}

func checkEgress(ctx context.Context, env *Env) []Result {
	c := firewallConf(env)
	out, _ := env.Sys.Run(ctx, "iptables", "-S", "HS_OUT")
	var rs []Result
	if !strings.Contains(out, "-j DROP") {
		rs = append(rs, New(Warn, "known C2 hosts are not blocked outbound").For("block list").
			Because("The block list (templates/ioc/block-hosts) is the only thing stopping a leftover implant from calling home.").
			Fixed("hs op egress   # applies the block list in any mode"))
	}
	switch c.Egress {
	case "log", "drop":
		n, top := egressLog(ctx, env)
		verb := "would have been rejected"
		if c.Egress == "drop" {
			verb = "rejected"
		}
		r := New(Configured, fmt.Sprintf("outbound filter %s: %d connection(s) %s in 7 days", c.Egress, n, verb)).Ev(top...)
		if c.Egress == "drop" && n == 0 {
			r = New(OK, "outbound filter on, nothing rejected in 7 days")
		}
		if c.Egress == "log" {
			r = r.Because("Log mode proves the allowlist before it enforces: every line here breaks when the filter rejects. Allow what is legitimate (extra ports), then switch to drop.").
				Fixed("hs op egress mode=drop")
		}
		rs = append(rs, r)
	default:
		rs = append(rs, New(Warn, "outbound traffic unrestricted").
			Because("Every connection the May 2026 implants needed went to non-standard ports (C2 :8000/:50051, mining pool :8444); a port allowlist would have cut all of them.").
			Fixed("hs op egress mode=log   # a week of logging first"))
	}
	return rs
}
