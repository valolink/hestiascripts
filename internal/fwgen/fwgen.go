// Package fwgen writes /etc/hs/firewall.sh: the rules Hestia's firewall does
// not make.
//
//   - IPv6 inbound. Hestia's firewall is IPv4-only (rules take an IPV4_CIDR)
//     and leaves ip6tables at policy ACCEPT with no rules, so on a box with a
//     global IPv6 address everything listening on [::] — the panel, SSH,
//     Netdata, the streamer — is reachable from anywhere over IPv6,
//     whatever the IPv4 rules say (hzdemolink 2026-09-25: panel 302, Netdata
//     200 over IPv6). HS_IN6 mirrors the public part of Hestia's rules:
//     ports Hestia opens to 0.0.0.0/0 stay open on IPv6, everything it limits
//     to addresses is closed unless an admin IPv6 range is configured.
//   - The Hestia panel. With PANEL_GATE=on its port answers only addresses
//     in the hs-panel/hs-panel6 ipsets, which EngineLink fills through the
//     streamer (`hs panel allow`, v-server-panel-allow) for PANEL_TTL seconds
//     at a time: an "Open panel" button, so the panel needs no fixed admin
//     address and is closed to the internet. Hestia's own rule (8083 open to
//     0.0.0.0/0) stays; HS_PANEL runs before it. The sets live in memory, so
//     after a reboot the panel is closed until the button is used again.
//   - Outbound. Known command-and-control hosts are always dropped; the
//     optional port filter (hardening plan §4) logs, then rejects, traffic to
//     ports outside a short allowlist — every connection the May 2026
//     implants made went to non-standard ports.
//
// The script is idempotent (own chains, flushed and rebuilt each run). It is
// run by Hestia's custom.sh — so every v-update-firewall re-applies it — and
// by hs-firewall.service at boot, because Hestia restores only the saved IPv4
// rules then.
package fwgen

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/valolink/hestiascripts/internal/hestia"
)

const (
	ScriptPath  = "/etc/hs/firewall.sh"
	ConfPath    = "/etc/hs/firewall.conf"
	CustomSh    = hestia.Root + "/data/firewall/custom.sh"
	RulesConf   = hestia.Root + "/data/firewall/rules.conf"
	UnitPath    = "/etc/systemd/system/hs-firewall.service"
	LogPrefix   = "hs-egress: "
	CustomBegin = "# hs:firewall — generated rules (hs op firewall-*); do not edit between these lines"
	CustomEnd   = "# hs:end firewall"
)

// Config is /etc/hs/firewall.conf.
type Config struct {
	IPv6Input   string   // "mirror" | "off"
	AdminV6     []string // IPv6 ranges allowed to reach the limited ports
	Egress      string   // "off" | "log" | "drop"
	EgressExtra []string // extra allowed outbound TCP ports
	BlockHosts  []string // always dropped outbound (and inbound)
	PanelGate   string   // "on" | "off"
	PanelTTL    int      // seconds an allowed address stays allowed
}

// The panel gate's ipsets and defaults. phpMyAdmin is served on every site's
// own ports (Apache's /phpmyadmin alias), so a port rule cannot close it:
// `hs panel sync` writes the same addresses into PMAGateConf as Require ip.
const (
	PMAGateConf = "/etc/apache2/conf.d/hs-phpmyadmin-gate.conf"
	PMADir      = "/usr/share/phpmyadmin"
	SyncUnit    = "/etc/systemd/system/hs-panel-sync.service"
	SyncTimer   = "/etc/systemd/system/hs-panel-sync.timer"
	PanelSet4   = "hs-panel"
	PanelSet6   = "hs-panel6"
	PanelTTL    = 8 * 3600
	PanelPortD  = "8083"
)

// Default outbound allowlist: DNS, NTP, HTTP(S), mail submission to the
// relay, the Storage Box (23), SSH/git (22).
var (
	EgressTCP = []string{"22", "23", "53", "80", "443", "465", "587"}
	EgressUDP = []string{"53", "123"}
)

func ParseConfig(text string) Config {
	c := Config{IPv6Input: "off", Egress: "off", PanelGate: "off", PanelTTL: PanelTTL}
	for _, line := range strings.Split(text, "\n") {
		// KEY='v' or KEY="v" (hs conf set writes double quotes), one per line.
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		if len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		{
			switch k {
			case "IPV6_INPUT":
				c.IPv6Input = v
			case "ADMIN_V6":
				c.AdminV6 = strings.Fields(v)
			case "EGRESS":
				c.Egress = v
			case "EGRESS_EXTRA_PORTS":
				c.EgressExtra = strings.Fields(strings.ReplaceAll(v, ",", " "))
			case "BLOCK_HOSTS":
				c.BlockHosts = strings.Fields(v)
			case "PANEL_GATE":
				c.PanelGate = v
			case "PANEL_TTL":
				if n, err := strconv.Atoi(v); err == nil && n >= 60 {
					c.PanelTTL = n
				}
			}
		}
	}
	return c
}

// Rule is one line of Hestia's rules.conf.
type Rule struct {
	ID, Action, Protocol, Port, IP, Comment string
	Suspended                               bool
}

func ParseRules(text string) []Rule {
	var out []Rule
	for _, line := range strings.Split(text, "\n") {
		kv := hestia.ParseKV(line)
		if kv["RULE"] == "" {
			continue
		}
		out = append(out, Rule{ID: kv["RULE"], Action: strings.ToUpper(kv["ACTION"]), Protocol: strings.ToUpper(kv["PROTOCOL"]),
			Port: kv["PORT"], IP: kv["IP"], Comment: kv["COMMENT"], Suspended: kv["SUSPENDED"] == "yes"})
	}
	return out
}

// Public: Hestia rules that accept from anywhere.
func (r Rule) Public() bool {
	return r.Action == "ACCEPT" && !r.Suspended && (r.IP == "0.0.0.0/0" || r.IP == "0.0.0.0")
}

// PublicPorts: protocol → ports Hestia opens to everyone (sorted).
func PublicPorts(rules []Rule) map[string][]string {
	set := map[string]map[string]bool{}
	for _, r := range rules {
		if !r.Public() || r.Protocol == "ICMP" {
			continue
		}
		p := strings.ToLower(r.Protocol)
		if set[p] == nil {
			set[p] = map[string]bool{}
		}
		for _, port := range strings.Split(r.Port, ",") {
			set[p][strings.ReplaceAll(strings.TrimSpace(port), "-", ":")] = true
		}
	}
	out := map[string][]string{}
	for p, ports := range set {
		for port := range ports {
			out[p] = append(out[p], port)
		}
		sort.Strings(out[p])
	}
	return out
}

func isV6(ip string) bool { return strings.Contains(ip, ":") }

// Script renders /etc/hs/firewall.sh. panelPort is Hestia's BACKEND_PORT
// ("" = 8083).
func Script(c Config, rules []Rule, panelPort string) string {
	if panelPort == "" {
		panelPort = PanelPortD
	}
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("#!/bin/sh")
	w("# Generated by hs (hs firewall apply) from %s and Hestia's rules.conf. Do not edit:", ConfPath)
	w("# change the settings with the hs operations and re-apply. Idempotent: own chains, rebuilt each run.")
	w("# Run by Hestia's custom.sh (every v-update-firewall) and by hs-firewall.service at boot.")
	w("")
	w("chain() { # TOOL CHAIN PARENT — create/flush CHAIN and put PARENT's one jump to it at the top")
	w("  $1 -w -N $2 2>/dev/null || true")
	w("  $1 -w -F $2")
	w("  while $1 -w -D $3 -j $2 2>/dev/null; do :; done")
	w("  $1 -w -I $3 1 -j $2")
	w("}")
	w("unchain() { # TOOL CHAIN PARENT")
	w("  while $1 -w -D $3 -j $2 2>/dev/null; do :; done")
	w("  $1 -w -F $2 2>/dev/null; $1 -w -X $2 2>/dev/null; true")
	w("}")
	w("HAVE6=; command -v ip6tables >/dev/null 2>&1 && HAVE6=1")
	w("")

	// --- IPv6 inbound
	w("# --- IPv6 inbound: %s", c.IPv6Input)
	if c.IPv6Input == "mirror" {
		w("if [ -n \"$HAVE6\" ]; then")
		w("  chain ip6tables HS_IN6 INPUT")
		w("  ip6tables -w -A HS_IN6 -i lo -j ACCEPT")
		w("  ip6tables -w -A HS_IN6 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT")
		w("  ip6tables -w -A HS_IN6 -p ipv6-icmp -j ACCEPT   # neighbour discovery, path MTU: IPv6 breaks without it")
		w("  ip6tables -w -A HS_IN6 -p udp --dport 546 -j ACCEPT   # DHCPv6 client")
		pp := PublicPorts(rules)
		protos := make([]string, 0, len(pp))
		for p := range pp {
			protos = append(protos, p)
		}
		sort.Strings(protos)
		for _, p := range protos {
			w("  # public in Hestia's IPv4 rules (%s): %s", p, strings.Join(pp[p], ","))
			for _, port := range pp[p] {
				if port == "0" {
					w("  ip6tables -w -A HS_IN6 -p %s -j ACCEPT", p)
				} else {
					w("  ip6tables -w -A HS_IN6 -p %s --dport %s -j ACCEPT", p, port)
				}
			}
		}
		for _, cidr := range c.AdminV6 {
			w("  ip6tables -w -A HS_IN6 -s %s -j ACCEPT   # admin range", cidr)
		}
		w("  ip6tables -w -A HS_IN6 -j DROP")
		w("fi")
	} else {
		w("[ -n \"$HAVE6\" ] && unchain ip6tables HS_IN6 INPUT")
	}
	w("")

	// --- the panel gate: after HS_IN6 so it lands before it in INPUT; before
	// HS_BLOCK so the block list stays first.
	w("# --- Hestia panel (:%s): %s", panelPort, c.PanelGate)
	if c.PanelGate == "on" {
		w("# Only addresses in the timed ipsets (hs panel allow, from EngineLink's Open panel); the box itself via lo.")
		w("# ipset -exist create never empties a set that is there, so re-running this keeps who is allowed.")
		for _, fam := range []struct{ tool, set, family, chain, ind string }{
			{"iptables", PanelSet4, "inet", "HS_PANEL", ""},
			{"ip6tables", PanelSet6, "inet6", "HS_PANEL6", "  "},
		} {
			if fam.ind != "" {
				w("if [ -n \"$HAVE6\" ]; then")
			}
			w("%sipset -exist create %s hash:ip family %s timeout %d", fam.ind, fam.set, fam.family, c.PanelTTL)
			w("%schain %s %s INPUT", fam.ind, fam.tool, fam.chain)
			w("%s%s -w -A %s -i lo -j RETURN", fam.ind, fam.tool, fam.chain)
			w("%s%s -w -A %s -p tcp --dport %s -m set --match-set %s src -j RETURN", fam.ind, fam.tool, fam.chain, panelPort, fam.set)
			w("%s%s -w -A %s -p tcp --dport %s -j DROP", fam.ind, fam.tool, fam.chain, panelPort)
			if fam.ind != "" {
				w("fi")
			}
		}
	} else {
		w("unchain iptables HS_PANEL INPUT")
		w("[ -n \"$HAVE6\" ] && unchain ip6tables HS_PANEL6 INPUT")
	}
	w("")

	// --- known-bad hosts inbound (both families), inserted last so it is first in INPUT, ahead of HS_IN6 and Hestia's rules
	w("# --- known-bad hosts: dropped inbound, before Hestia's own rules")
	w("chain iptables HS_BLOCK INPUT")
	for _, h := range c.BlockHosts {
		if !isV6(h) {
			w("iptables -w -A HS_BLOCK -s %s -j DROP", h)
		}
	}
	w("if [ -n \"$HAVE6\" ]; then")
	w("  chain ip6tables HS_BLOCK INPUT")
	for _, h := range c.BlockHosts {
		if isV6(h) {
			w("  ip6tables -w -A HS_BLOCK -s %s -j DROP", h)
		}
	}
	w("fi")
	w("")

	// --- outbound
	w("# --- outbound: known C2 hosts always; port filter %s", c.Egress)
	for _, tool := range []string{"iptables", "ip6tables"} {
		v6 := tool == "ip6tables"
		ind := ""
		if v6 {
			w("if [ -n \"$HAVE6\" ]; then")
			ind = "  "
		}
		w("%schain %s HS_OUT OUTPUT", ind, tool)
		for _, h := range c.BlockHosts {
			if isV6(h) == v6 {
				w("%s%s -w -A HS_OUT -d %s -j DROP", ind, tool, h)
			}
		}
		if c.Egress == "log" || c.Egress == "drop" {
			icmp := "icmp"
			if v6 {
				icmp = "ipv6-icmp"
			}
			tcp := append(append([]string{}, EgressTCP...), c.EgressExtra...)
			w("%s%s -w -A HS_OUT -o lo -j RETURN", ind, tool)
			w("%s%s -w -A HS_OUT -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN", ind, tool)
			w("%s%s -w -A HS_OUT -p %s -j RETURN", ind, tool, icmp)
			w("%s%s -w -A HS_OUT -p udp -m multiport --dports %s -j RETURN", ind, tool, strings.Join(EgressUDP, ","))
			w("%s%s -w -A HS_OUT -p tcp -m multiport --dports %s -j RETURN", ind, tool, strings.Join(tcp, ","))
			w("%s%s -w -A HS_OUT -m limit --limit 20/min --limit-burst 40 -j LOG --log-prefix '%s' --log-uid", ind, tool, LogPrefix)
			if c.Egress == "drop" {
				w("%s%s -w -A HS_OUT -j REJECT", ind, tool)
			}
		}
		if v6 {
			w("fi")
		}
	}
	w("exit 0")
	return b.String()
}

// CustomBlock is what hs keeps in Hestia's custom.sh.
func CustomBlock() string {
	return CustomBegin + "\n[ -x " + ScriptPath + " ] && " + ScriptPath + "\n" + CustomEnd + "\n"
}

// WithCustomBlock returns custom.sh content with the hs block present once,
// everything else untouched.
func WithCustomBlock(existing string) string {
	if i := strings.Index(existing, CustomBegin); i >= 0 {
		if j := strings.Index(existing[i:], CustomEnd); j >= 0 {
			end := i + j + len(CustomEnd)
			if end < len(existing) && existing[end] == '\n' {
				end++
			}
			return existing[:i] + CustomBlock() + existing[end:]
		}
	}
	if existing == "" {
		existing = "#!/bin/sh\n"
	}
	if !strings.HasSuffix(existing, "\n") {
		existing += "\n"
	}
	return existing + CustomBlock()
}

// Unit is hs-firewall.service: re-applies after Hestia's boot-time restore.
func Unit() string {
	return `[Unit]
Description=hs firewall rules Hestia does not make (IPv6 inbound, outbound filter)
After=hestia-iptables.service network-pre.target
Wants=hestia-iptables.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=` + ScriptPath + `

[Install]
WantedBy=multi-user.target
`
}

// PMAGate renders PMAGateConf for the allowed addresses (none: denied to all).
func PMAGate(ips []string) string {
	req := "Require all denied"
	if len(ips) > 0 {
		req = "Require ip " + strings.Join(ips, " ")
	}
	return "# hs panel sync: phpMyAdmin only from the addresses EngineLink's Open panel allowed\n" +
		"# (the hs-panel/hs-panel6 ipsets). Generated every 5 minutes and on every allow; do not edit.\n" +
		"<Directory " + PMADir + ">\n\t" + req + "\n</Directory>\n"
}

// SyncUnits: the service and timer that keep PMAGateConf in step with the
// ipsets as entries expire, and empty it after a reboot (the sets are gone).
func SyncUnits() (service, timer string) {
	return `[Unit]
Description=hs panel gate: phpMyAdmin allow list from the panel ipsets
After=hs-firewall.service apache2.service

[Service]
Type=oneshot
ExecStart=/usr/local/bin/hs panel sync
`, `[Unit]
Description=hs panel gate: keep phpMyAdmin's allow list in step with expiring entries

[Timer]
OnBootSec=1min
OnUnitActiveSec=5min

[Install]
WantedBy=timers.target
`
}
