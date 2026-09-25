// Package aptinfo reads `apt-get -s upgrade` — a true dry run: apt resolves
// the whole transaction and prints it without touching anything — and says
// what each upgrade is: a rebuild (same upstream version, Debian applied a
// patch — usually security), a patch/minor/major version move, or a data
// refresh (tzdata, CA certificates), and what it does to a running box. A
// Go port of setup/maintenance.sh's _maintenance_update_preview.
package aptinfo

import (
	"fmt"
	"strings"
)

type Pkg struct {
	Name, Cur, New, Origin string
	Class                  string // rebuild | patch | minor | major | data
	Impact                 string
	Attention              bool
}

type Preview struct {
	Upgrades []Pkg
	New      []string // new dependencies
	Remove   []string
	Held     []string
	Reboot   bool
}

// Upstream strips the epoch and Debian revision: 1:11.4.5-1~deb12u1 → 11.4.5
func Upstream(v string) string {
	if _, after, ok := strings.Cut(v, ":"); ok {
		v = after
	}
	if i := strings.LastIndex(v, "-"); i > 0 {
		v = v[:i]
	}
	return v
}

func leadingNumeric(s string) string {
	for i, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return s[:i]
		}
	}
	return s
}

// Classify says how far an upgrade moves.
func Classify(cur, new string) string {
	cu, nu := Upstream(cur), Upstream(new)
	if cu == nu {
		return "rebuild"
	}
	cu, nu = leadingNumeric(cu), leadingNumeric(nu)
	if !strings.Contains(nu, ".") {
		return "data" // date/serial versioned: tzdata 2025b, ca-certificates 20240203
	}
	c, n := strings.Split(cu, "."), strings.Split(nu, ".")
	part := func(p []string, i int) string {
		if i < len(p) && p[i] != "" {
			return p[i]
		}
		return "0"
	}
	switch {
	case part(n, 0) != part(c, 0):
		return "major"
	case part(n, 1) != part(c, 1):
		return "minor"
	case part(n, 2) != part(c, 2):
		return "patch"
	}
	return "rebuild"
}

func hasPrefix(s string, ps ...string) bool {
	for _, p := range ps {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// Critical: upgrading it interrupts a running site or may want a config look.
func Critical(pkg string) bool {
	return hasPrefix(pkg, "mariadb-", "mysql-", "php", "nginx", "hestia", "redis", "systemd", "linux-image-", "exim4", "postfix") ||
		pkg == "openssh-server" || pkg == "libc6"
}

// Impact is what happens to the box when pkg upgrades.
func Impact(pkg string) string {
	switch {
	case pkg == "mariadb-server" || pkg == "mariadb-server-core" || hasPrefix(pkg, "mysql-server"):
		return "restarts the database"
	case strings.HasPrefix(pkg, "php") && strings.HasSuffix(pkg, "-fpm"):
		return "restarts PHP, sites blip"
	case pkg == "nginx" || pkg == "apache2":
		return "restarts the web server"
	case pkg == "hestia" || pkg == "hestia-nginx" || pkg == "hestia-php":
		return "restarts the control panel"
	case pkg == "redis-server":
		return "restarts Redis: the object cache starts empty"
	case pkg == "openssh-server":
		return "restarts SSH (open sessions survive)"
	case strings.HasPrefix(pkg, "linux-image-"):
		return "needs a reboot to take effect"
	case pkg == "libc6" || pkg == "systemd":
		return "core system library"
	case strings.HasPrefix(pkg, "exim4-daemon") || pkg == "postfix":
		return "restarts mail"
	}
	return ""
}

// Parse reads apt-get -s upgrade output. The format is fixed:
//
//	Inst PKG [CURRENT] (NEW ORIGIN [ARCH])   — upgrade
//	Inst PKG (NEW ORIGIN [ARCH])             — new dependency
//	Remv PKG [CURRENT]
func Parse(sim string) Preview {
	var p Preview
	held := false
	for _, l := range strings.Split(sim, "\n") {
		f := strings.Fields(l)
		switch {
		case strings.Contains(l, "have been kept back") || strings.Contains(l, "held back"):
			held = true
			continue
		case held && strings.HasPrefix(l, " "):
			p.Held = append(p.Held, f...)
			continue
		default:
			held = false
		}
		if len(f) < 3 {
			continue
		}
		switch f[0] {
		case "Remv":
			p.Remove = append(p.Remove, f[1])
		case "Inst":
			i, cur := 2, ""
			if strings.HasPrefix(f[2], "[") {
				cur, i = strings.Trim(f[2], "[]"), 3
			}
			if i >= len(f) {
				continue
			}
			nv := strings.TrimPrefix(f[i], "(")
			origin := strings.TrimSuffix(strings.Join(f[i+1:], " "), ")")
			if cur == "" {
				p.New = append(p.New, f[1])
				continue
			}
			pk := Pkg{Name: f[1], Cur: cur, New: nv, Origin: origin, Class: Classify(cur, nv), Impact: Impact(f[1])}
			pk.Attention = pk.Class == "major" || pk.Class == "minor" || Critical(pk.Name)
			if strings.HasPrefix(pk.Name, "linux-image") {
				p.Reboot = true
			}
			p.Upgrades = append(p.Upgrades, pk)
		}
	}
	return p
}

func (p Preview) Empty() bool { return len(p.Upgrades)+len(p.New)+len(p.Remove) == 0 }

func (p Preview) Security() int {
	n := 0
	for _, u := range p.Upgrades {
		if strings.Contains(strings.ToLower(u.Origin), "security") {
			n++
		}
	}
	return n
}

func noEpoch(v string) string {
	if _, after, ok := strings.Cut(v, ":"); ok {
		return after
	}
	return v
}

func (u Pkg) line() string {
	cv, nv := Upstream(u.Cur), Upstream(u.New)
	if u.Class == "rebuild" {
		// Upstream is the same on both sides: show the Debian revision that moved.
		cv, nv = noEpoch(u.Cur), noEpoch(u.New)
	}
	s := fmt.Sprintf("%-8s %-26s %s → %s", u.Class, u.Name, cv, nv)
	if u.Impact != "" {
		s += "  (" + u.Impact + ")"
	}
	return s
}

// Render is the human summary; routine is truncated to maxRoutine (0 = all).
func (p Preview) Render(maxRoutine int) []string {
	if p.Empty() {
		return []string{"Everything is up to date."}
	}
	out := []string{fmt.Sprintf("To upgrade: %d package(s)", len(p.Upgrades))}
	if len(p.New) > 0 {
		out = append(out, fmt.Sprintf("New dependencies: %d (%s)", len(p.New), strings.Join(p.New, " ")))
	}
	if n := p.Security(); n > 0 {
		out = append(out, fmt.Sprintf("From the security archive: %d", n))
	}
	if p.Reboot {
		out = append(out, "Reboot afterwards: yes (new kernel)")
	}
	if len(p.Held) > 0 {
		out = append(out, "Held back (not upgraded): "+strings.Join(p.Held, " "))
	}
	if len(p.Remove) > 0 {
		out = append(out, fmt.Sprintf("WILL REMOVE %d package(s): %s — unusual for an upgrade, read this list", len(p.Remove), strings.Join(p.Remove, " ")))
	}
	var att, rout []string
	for _, u := range p.Upgrades {
		if u.Attention {
			att = append(att, "  "+u.line())
		} else {
			rout = append(rout, "  "+u.line())
		}
	}
	if len(att) > 0 {
		out = append(append(out, "", "Worth a look"), att...)
	}
	if len(rout) > 0 {
		out = append(out, "", fmt.Sprintf("Routine (%d)", len(rout)))
		if maxRoutine > 0 && len(rout) > maxRoutine {
			out = append(out, rout[:maxRoutine]...)
			out = append(out, fmt.Sprintf("  … and %d more", len(rout)-maxRoutine))
		} else {
			out = append(out, rout...)
		}
	}
	return append(out, "",
		"rebuild = same software, the vendor applied a patch (usually security)",
		"data    = date-versioned data refresh (time zones, CA certificates)",
		"patch / minor / major = the upstream version moved")
}

// RepoError is one failing source from `apt-get update`.
type RepoError struct {
	URL, Suite, Reason string
}

// ParseUpdate reads apt-get update output: counts and the Err: entries
// (the reason is the line after each Err:).
func ParseUpdate(out string) (ok, total int, errs []RepoError) {
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "Hit:") || strings.HasPrefix(l, "Get:"):
			ok++
			total++
		case strings.HasPrefix(l, "Err:"):
			total++
			f := strings.Fields(l)
			e := RepoError{}
			if len(f) > 1 {
				e.URL = f[1]
			}
			if len(f) > 2 {
				e.Suite = f[2]
			}
			if i+1 < len(lines) {
				e.Reason = strings.TrimSpace(lines[i+1])
			}
			errs = append(errs, e)
		}
	}
	return
}

// Advice turns an apt error into what it means and what to do.
func Advice(reason string) (meaning, fix string) {
	has := func(s ...string) bool {
		for _, x := range s {
			if strings.Contains(reason, x) {
				return true
			}
		}
		return false
	}
	switch {
	case has("403", "404"):
		return "the host stopped serving this suite (moved or blocked)", "point the line at another mirror of the same version"
	case has("NO_PUBKEY", "EXPKEYSIG", "KEYEXPIRED"):
		return "the signing key is missing or expired, not the URL", "re-import the vendor's key; leave the URL alone"
	case has("Could not resolve", "Temporary failure", "Connection failed", "Connection timed out"):
		return "DNS or network, likely transient", "retry; if it persists the host may be gone for good"
	case has("expired", "Release file"):
		return "the mirror is stale — its Release file aged out", "switch mirrors; a stale mirror is an abandoned mirror"
	}
	return "", "see the primer below"
}

// Primer is printed after any failing repository: the reasoning on screen
// at the moment the decision has to be made.
var Primer = []string{
	"What a broken repository actually costs you",
	"",
	"Nothing is broken right now. Each repository supplies updates for its own packages",
	"only, so one failing means no more updates for that vendor's software while",
	"everything else carries on. The danger is quiet: missed security updates, no warning.",
	"",
	"Why editing the URL is safe: every package is GPG-signed and apt refuses anything",
	"not signed by a key already trusted here, so a wrong URL fails loudly.",
	"",
	"The one rule: change the HOST, never the VERSION in the path.",
	"  https://dlm.mariadb.com/repo/mariadb-server/11.4/repo/debian",
	"          ^^^^^^^^^^^^^^^ safe to swap         ^^^^ leave alone",
	"The version segment decides which MariaDB you get; changing it makes the next",
	"upgrade migrate your databases to a new major version.",
	"",
	"Back up the file first (cp /etc/apt/sources.list.d/NAME.list{,.bak}), then re-run the check.",
}
