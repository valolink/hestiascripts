package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/valolink/hestiascripts/internal/fwgen"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/sys"
)

const panelLog = "/var/log/hs/panel.log"

// panelResult is the final stdout line of `hs panel allow`, which EngineLink
// reads through v-server-panel-allow.
type panelResult struct {
	Gate         string `json:"gate"` // "on" | "off" (off: the panel is open to all anyway)
	IP           string `json:"ip,omitempty"`
	TTL          int    `json:"ttl,omitempty"`
	Host         string `json:"host"`
	HostResolves bool   `json:"hostResolves"`
	Port         string `json:"port"`
	Error        string `json:"error,omitempty"`
}

// cmdPanel: the panel gate's allow list (docs/security-hardening.md, "The
// panel gate"). allow adds an address for PANEL_TTL seconds (again: resets
// the clock), list shows who is in, revoke removes one.
func cmdPanel(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: hs panel allow IP | list | revoke IP")
		return 2
	}
	cb, _ := os.ReadFile(fwgen.ConfPath)
	c := fwgen.ParseConfig(string(cb))
	port := hestia.Conf(sys.Real{})["BACKEND_PORT"]
	if port == "" {
		port = fwgen.PanelPortD
	}
	switch args[0] {
	case "allow":
		fs := flag.NewFlagSet("panel allow", flag.ExitOnError)
		fs.Parse(args[1:])
		res := panelResult{Gate: c.PanelGate, Port: port}
		res.Host, res.HostResolves = panelHost(ctx)
		fail := func(msg string) int {
			res.Error = msg
			fmt.Println("✗", msg)
			b, _ := json.Marshal(res)
			fmt.Println(string(b))
			return 1
		}
		if fs.NArg() != 1 {
			return fail("one address expected")
		}
		ip := net.ParseIP(strings.TrimSpace(fs.Arg(0)))
		if ip == nil {
			return fail("not an IP address: " + fs.Arg(0))
		}
		// A private or local address means a proxy in front of EngineLink
		// handed over its own address instead of the visitor's.
		if !ip.IsGlobalUnicast() || ip.IsPrivate() {
			return fail(ip.String() + " is not a public address")
		}
		res.IP = ip.String()
		if c.PanelGate != "on" {
			fmt.Printf("panel gate is off here: :%s is open to every address\n", port)
			b, _ := json.Marshal(res)
			fmt.Println(string(b))
			return 0
		}
		set := fwgen.PanelSet4
		if ip.To4() == nil {
			set = fwgen.PanelSet6
		}
		res.TTL = c.PanelTTL
		out, err := exec.CommandContext(ctx, "ipset", "-exist", "add", set, res.IP, "timeout", strconv.Itoa(c.PanelTTL)).CombinedOutput()
		if err != nil {
			return fail(fmt.Sprintf("ipset add %s %s: %v %s", set, res.IP, err, strings.TrimSpace(string(out))))
		}
		appendPanelLog(fmt.Sprintf("allow %s %ds", res.IP, c.PanelTTL))
		pmaSync(ctx, c)
		fmt.Printf("✓ %s may reach the panel (:%s) for %s\n", res.IP, port, time.Duration(c.PanelTTL)*time.Second)
		b, _ := json.Marshal(res)
		fmt.Println(string(b))
		return 0
	case "sync":
		if !pmaSync(ctx, c) {
			return 1
		}
		return 0
	case "list":
		for _, set := range []string{fwgen.PanelSet4, fwgen.PanelSet6} {
			out, err := exec.CommandContext(ctx, "ipset", "list", set).Output()
			if err != nil {
				fmt.Printf("%s: not present\n", set)
				continue
			}
			members := false
			for _, l := range strings.Split(string(out), "\n") {
				if f := strings.Fields(l); len(f) >= 3 && f[1] == "timeout" {
					if !members {
						fmt.Printf("%s:\n", set)
						members = true
					}
					s, _ := strconv.Atoi(f[2])
					fmt.Printf("  %-40s %s left\n", f[0], time.Duration(s)*time.Second)
				}
			}
			if !members {
				fmt.Printf("%s: empty\n", set)
			}
		}
		return 0
	case "revoke":
		if len(args) != 2 || net.ParseIP(args[1]) == nil {
			fmt.Fprintln(os.Stderr, "usage: hs panel revoke IP")
			return 2
		}
		ip := net.ParseIP(args[1])
		set := fwgen.PanelSet4
		if ip.To4() == nil {
			set = fwgen.PanelSet6
		}
		if out, err := exec.CommandContext(ctx, "ipset", "-exist", "del", set, ip.String()).CombinedOutput(); err != nil {
			fmt.Printf("✗ ipset del: %v %s\n", err, out)
			return 1
		}
		appendPanelLog("revoke " + ip.String())
		pmaSync(ctx, c)
		fmt.Printf("✓ %s removed (open connections from it are cut)\n", ip)
		return 0
	}
	fmt.Fprintln(os.Stderr, "usage: hs panel allow IP | list | revoke IP | sync")
	return 2
}

// panelMembers: the addresses in both ipsets now (expired ones are gone).
func panelMembers(ctx context.Context) []string {
	var ips []string
	for _, set := range []string{fwgen.PanelSet4, fwgen.PanelSet6} {
		out, err := exec.CommandContext(ctx, "ipset", "list", set).Output()
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(out), "\n") {
			if f := strings.Fields(l); len(f) >= 3 && f[1] == "timeout" && net.ParseIP(f[0]) != nil {
				ips = append(ips, f[0])
			}
		}
	}
	return ips
}

// pmaSync writes phpMyAdmin's allow list from the ipsets (gate on) or
// removes it (gate off), and reloads Apache gracefully only when it changed.
// A config Apache rejects is put back. No Apache or no phpMyAdmin: nothing.
func pmaSync(ctx context.Context, c fwgen.Config) bool {
	if _, err := exec.LookPath("apache2ctl"); err != nil {
		return true
	}
	if _, err := os.Stat(fwgen.PMADir); err != nil {
		return true
	}
	old, oldErr := os.ReadFile(fwgen.PMAGateConf)
	want := ""
	if c.PanelGate == "on" {
		want = fwgen.PMAGate(panelMembers(ctx))
	}
	if string(old) == want && (want != "" || os.IsNotExist(oldErr)) {
		return true
	}
	if want == "" {
		os.Remove(fwgen.PMAGateConf)
	} else if err := os.WriteFile(fwgen.PMAGateConf, []byte(want), 0o644); err != nil {
		fmt.Println("✗ phpMyAdmin gate:", err)
		return false
	}
	if out, err := exec.CommandContext(ctx, "apache2ctl", "configtest").CombinedOutput(); err != nil {
		if oldErr == nil {
			os.WriteFile(fwgen.PMAGateConf, old, 0o644)
		} else {
			os.Remove(fwgen.PMAGateConf)
		}
		fmt.Printf("✗ Apache refused the phpMyAdmin gate, previous state kept: %s\n", strings.TrimSpace(string(out)))
		return false
	}
	if out, err := exec.CommandContext(ctx, "apache2ctl", "graceful").CombinedOutput(); err != nil {
		fmt.Printf("✗ apache2ctl graceful: %v %s\n", err, out)
		return false
	}
	fmt.Println("✓ phpMyAdmin allow list updated (Apache reloaded gracefully)")
	return true
}

// panelHost: the box's FQDN, which Hestia's panel certificate is issued for,
// and whether public DNS knows it (alavus's hestia.alavusikkunat.fi does not;
// EngineLink then uses the server's IP).
func panelHost(ctx context.Context) (string, bool) {
	out, err := exec.CommandContext(ctx, "hostname", "-f").Output()
	h := strings.TrimSpace(string(out))
	if err != nil || h == "" {
		h, _ = os.Hostname()
	}
	lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(lctx, h)
	return h, err == nil && len(addrs) > 0
}

func appendPanelLog(line string) {
	os.MkdirAll("/var/log/hs", 0o700)
	f, err := os.OpenFile(panelLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), line)
}
