package box

// Persistence and intrusion checks (hardening plan §8, §9): what found the
// May 2026 implants by hand, repeated by the machine. Read-only and bounded;
// the heavy sweeps have a MinInterval so the 15-minute cron stays cheap.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/ioc"
)

func persistChecks() []Check {
	return []Check{
		{ID: "persist.iocs", Section: "security", Title: "Known compromise indicators", Timeout: 30 * time.Second, Run: checkIOCs},
		{ID: "persist.processes", Section: "security", Title: "Suspicious processes", Timeout: 30 * time.Second, Run: checkProcesses},
		{ID: "persist.outbound", Section: "security", Title: "Unusual outbound connections", Run: checkOutbound},
		{ID: "persist.accounts", Section: "security", Title: "Root-equivalent accounts", Run: checkAccounts},
		{ID: "persist.cron", Section: "security", Title: "Cron entries", Run: checkCron},
		{ID: "persist.unowned", Section: "security", Title: "Unpackaged binaries", Heavy: true, MinInterval: time.Hour, Timeout: 3 * time.Minute, Run: checkUnowned},
		{ID: "persist.units", Section: "security", Title: "Unpackaged systemd units", Run: checkUnits},
		{ID: "web.webshell", Section: "security", Title: "Webshells in web roots", Heavy: true, MinInterval: time.Hour, Timeout: 5 * time.Minute, Run: checkWebshells},
		{ID: "web.root-owned", Section: "security", Title: "Root-owned PHP in web roots", Heavy: true, MinInterval: time.Hour, Timeout: 2 * time.Minute, Run: checkRootOwned},
		{ID: "pkg.integrity", Section: "security", Title: "Package file integrity", Heavy: true, MinInterval: 7 * 24 * time.Hour, Timeout: 20 * time.Minute, Run: checkPkgIntegrity},
	}
}

func indicators(env *Env) ioc.List { return ioc.Load(readString(env.Sys, ioc.LocalPath)) }

// --- package ownership ------------------------------------------------------

var dpkgCache struct {
	sync.Mutex
	stamp time.Time
	set   map[string]bool
}

// aliases: merged-/usr means dpkg may list /bin/x for a file found at /usr/bin/x.
func aliases(p string) []string {
	out := []string{p}
	for _, pair := range [][2]string{{"/bin/", "/usr/bin/"}, {"/sbin/", "/usr/sbin/"}, {"/lib/", "/usr/lib/"}, {"/lib64/", "/usr/lib64/"}} {
		if strings.HasPrefix(p, pair[0]) {
			out = append(out, pair[1]+strings.TrimPrefix(p, pair[0]))
		} else if strings.HasPrefix(p, pair[1]) {
			out = append(out, pair[0]+strings.TrimPrefix(p, pair[1]))
		}
	}
	return out
}

// dpkgOwned is every path some package owns (plus diversion targets),
// rebuilt when the dpkg database changes.
func dpkgOwned(env *Env) map[string]bool {
	s := env.Sys
	st, err := s.Stat("/var/lib/dpkg/status")
	var stamp time.Time
	if err == nil {
		stamp = st.ModTime()
	}
	dpkgCache.Lock()
	defer dpkgCache.Unlock()
	if dpkgCache.set != nil && stamp.Equal(dpkgCache.stamp) {
		return dpkgCache.set
	}
	set := map[string]bool{}
	lists, _ := s.Glob("/var/lib/dpkg/info/*.list")
	for _, l := range lists {
		for _, p := range strings.Split(readString(s, l), "\n") {
			if p != "" {
				for _, a := range aliases(p) {
					set[a] = true
				}
			}
		}
	}
	div := strings.Split(readString(s, "/var/lib/dpkg/diversions"), "\n")
	for i := 0; i+1 < len(div); i += 3 {
		for _, a := range aliases(div[i+1]) {
			set[a] = true
		}
	}
	dpkgCache.stamp, dpkgCache.set = stamp, set
	return set
}

// allowPrefixes: known-good unpackaged paths (hs itself, rclone, Postfix's
// chroot copies) plus /etc/hs/unowned.allowed.
func allowPrefixes(env *Env) []string {
	out := []string{"/usr/bin/rclone", "/usr/local/bin/hs", "/root/hs-test", "/root/hs-v2-test", "/var/spool/postfix/", "/var/lib/hs/",
		// installed by maldet's own install.sh
		"/usr/local/maldetect/", "/usr/lib/systemd/system/maldet.service",
		// Hestia's and cloud-init's own sudoers files
		"/etc/sudoers.d/hestiaweb", "/etc/sudoers.d/admin", "/etc/sudoers.d/90-cloud-init-users"}
	if env.RepoDir != "" {
		out = append(out, env.RepoDir+"/")
	}
	// The deployed hestiascripts checkout: where its v-scripts link to.
	if t, err := env.Sys.EvalSymlinks("/usr/local/hestia/bin/v-wp-info"); err == nil {
		out = append(out, filepath.Dir(t)+"/")
	}
	for _, l := range strings.Split(readString(env.Sys, "/etc/hs/unowned.allowed"), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

func allowed(p string, prefixes []string) bool {
	for _, a := range prefixes {
		if p == a || strings.HasPrefix(p, a) {
			return true
		}
	}
	return false
}

func sha256File(env *Env, p string) string {
	b, err := env.Sys.ReadFile(p)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// --- indicators -------------------------------------------------------------

func hostNets(list ioc.List) []*net.IPNet {
	var nets []*net.IPNet
	for _, h := range list.Values("host") {
		if !strings.Contains(h, "/") {
			if strings.Contains(h, ":") {
				h += "/128"
			} else {
				h += "/32"
			}
		}
		if _, n, err := net.ParseCIDR(h); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}

// connections: ss -Htnp output as (local, remote, state, process).
type conn struct{ State, Local, Remote, Proc string }

func connections(ctx context.Context, env *Env) []conn {
	out, _ := env.Sys.Run(ctx, "ss", "-Htanp")
	var cs []conn
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 5 {
			continue
		}
		c := conn{State: f[0], Local: f[3], Remote: f[4]}
		if len(f) > 5 {
			c.Proc = f[5]
		}
		cs = append(cs, c)
	}
	return cs
}

func splitHostPort(a string) (net.IP, int) {
	h, p, err := net.SplitHostPort(a)
	if err != nil {
		return nil, 0
	}
	if i := strings.Index(h, "%"); i >= 0 {
		h = h[:i]
	}
	n, _ := strconv.Atoi(p)
	return net.ParseIP(h), n
}

func checkIOCs(ctx context.Context, env *Env) []Result {
	s := env.Sys
	list := indicators(env)
	owned := dpkgOwned(env)
	var rs []Result
	crit := func(subject, summary string, ev ...string) {
		rs = append(rs, New(Fail, summary).For(subject).Ev(ev...).
			Because("This matches an indicator from a real compromise (internal/ioc/indicators.txt). Treat the box as compromised until proven otherwise: do not log in with a forwarded agent, do not copy keys to it, preserve evidence before removing anything.").
			Fixed("see docs/security-hardening.md — quarantine (move to /root/quarantine-<date>, keep hashes), block, then find the entry point"))
	}
	for _, i := range list.Of("path") {
		matches, _ := s.Glob(i.Value)
		for _, m := range matches {
			if owned[m] && m != "/etc/ld.so.preload" {
				continue
			}
			ev := []string{i.Note}
			if sum := sha256File(env, m); sum != "" {
				ev = append(ev, "sha256 "+sum)
			}
			crit(m, "compromise indicator present: "+m, ev...)
		}
	}
	names := map[string]string{}
	for _, i := range list.Of("process") {
		names[i.Value] = i.Note
	}
	if out, err := s.Run(ctx, "ps", "-eo", "pid=,comm=,args="); err == nil {
		for _, l := range strings.Split(out, "\n") {
			f := strings.Fields(l)
			if len(f) >= 2 {
				if note, bad := names[f[1]]; bad {
					crit("pid "+f[0], "known implant process running: "+f[1], note, strings.Join(f[2:], " "))
				}
			}
		}
	}
	nets := hostNets(list)
	for _, c := range connections(ctx, env) {
		for _, a := range []string{c.Remote, c.Local} {
			ip, _ := splitHostPort(a)
			if ip != nil && covers(nets, ip) {
				crit(c.Remote, "connection to a known-bad host", c.State+" "+c.Local+" → "+c.Remote+" "+c.Proc)
			}
		}
	}
	if len(rs) == 0 {
		return []Result{New(OK, fmt.Sprintf("none of %d indicators present", len(list))).Valid(24 * time.Hour)}
	}
	return rs
}

func covers(nets []*net.IPNet, ip net.IP) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// --- processes ----------------------------------------------------------------

func checkProcesses(ctx context.Context, env *Env) []Result {
	out, _ := env.Sys.Run(ctx, "find", "/proc", "-mindepth", "2", "-maxdepth", "2", "-name", "exe", "-printf", `%h %l\n`)
	owned := dpkgOwned(env)
	allow := allowPrefixes(env)
	known := indicators(env)
	seen := map[string]bool{}
	var rs []Result
	for _, l := range strings.Split(out, "\n") {
		dir, exe, ok := strings.Cut(l, " ")
		if !ok || exe == "" {
			continue
		}
		pid := strings.TrimPrefix(dir, "/proc/")
		deleted := strings.HasSuffix(exe, " (deleted)")
		path := strings.TrimSuffix(exe, " (deleted)")
		if seen[exe] {
			continue
		}
		seen[exe] = true
		inTmp := strings.HasPrefix(path, "/tmp/") || strings.HasPrefix(path, "/var/tmp/") || strings.HasPrefix(path, "/dev/shm/") || strings.HasPrefix(path, "/memfd:")
		switch {
		case inTmp:
			rs = append(rs, New(Fail, "process running from a temporary directory").For(path).Ev("pid "+pid).
				Because("Nothing legitimate on a web server runs binaries out of /tmp, /var/tmp or /dev/shm; the May 2026 tooling ran /tmp/a.elf.").
				Fixed("ls -l /proc/"+pid+"/exe; cat /proc/"+pid+"/cmdline | tr '\\\\0' ' '; do not kill before capturing it"))
		case owned[path]:
			// packaged; a deleted packaged binary is just stale after an upgrade
		case allowed(path, allow):
		case deleted:
			rs = append(rs, New(Warn, "process whose unpackaged binary was deleted").For(path).Ev("pid "+pid).
				Because("Malware deletes its binary after starting; a packaged program replaced by an upgrade shows here only if dpkg never owned the path.").
				Fixed("cp /proc/"+pid+"/exe /root/evidence-"+pid+" before anything else"))
		default:
			ev := []string{"pid " + pid}
			st := Warn
			if sum := sha256File(env, path); sum != "" {
				if note := known.Hash(sum); note != "" {
					st, ev = Fail, append(ev, "sha256 matches "+note)
				}
			}
			rs = append(rs, New(st, "process running an unpackaged binary").For(path).Ev(ev...).
				Because("Every implant in May 2026 ran as an unpackaged binary. Legitimate ones go in /etc/hs/unowned.allowed.").
				Fixed("dpkg -S "+path+"; sha256sum "+path))
		}
	}
	if len(rs) == 0 {
		return []Result{New(OK, fmt.Sprintf("%d running binaries, all packaged or allowed", len(seen)))}
	}
	return rs
}

// --- outbound ------------------------------------------------------------------

var outboundPorts = map[int]bool{22: true, 23: true, 25: true, 53: true, 80: true, 123: true, 443: true, 465: true, 587: true, 993: true, 995: true}

func checkOutbound(ctx context.Context, env *Env) []Result {
	listen := map[int]bool{}
	if out, err := env.Sys.Run(ctx, "ss", "-Htanl"); err == nil {
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) >= 4 {
				_, p := splitHostPort(f[3])
				listen[p] = true
			}
		}
	}
	var rs []Result
	for _, c := range connections(ctx, env) {
		if c.State != "ESTAB" && c.State != "SYN-SENT" {
			continue
		}
		_, lport := splitHostPort(c.Local)
		rip, rport := splitHostPort(c.Remote)
		if rip == nil || listen[lport] || rip.IsLoopback() || rip.IsPrivate() || rip.IsLinkLocalUnicast() || outboundPorts[rport] {
			continue // inbound to a service, local, or an expected port
		}
		rs = append(rs, New(Warn, fmt.Sprintf("outbound to port %d", rport)).For(c.Remote).Ev(c.State+" "+c.Local+" → "+c.Remote+" "+c.Proc).
			Because("The May 2026 implants called out on :8000 and :50051 and mined on :8444; web servers rarely need anything beyond web, mail and DNS ports.").
			Fixed("identify the process; allow the port (hs op egress) if it is legitimate"))
	}
	if len(rs) == 0 {
		return []Result{New(OK, "no outbound connections to unusual ports")}
	}
	return rs
}

// --- accounts and cron ------------------------------------------------------------

func checkAccounts(ctx context.Context, env *Env) []Result {
	var rs []Result
	for _, l := range strings.Split(readString(env.Sys, "/etc/passwd"), "\n") {
		f := strings.Split(l, ":")
		if len(f) > 3 && f[2] == "0" && f[0] != "root" {
			rs = append(rs, New(Fail, "second account with uid 0").For(f[0]).Ev(l).
				Because("Any account with uid 0 is root under another name — a classic backdoor.").Fixed("vipw; find out who added it"))
		}
	}
	owned := dpkgOwned(env)
	files, _ := env.Sys.Glob("/etc/sudoers.d/*")
	for _, p := range files {
		if !owned[p] && filepath.Base(p) != "README" {
			rs = append(rs, New(Warn, "sudoers drop-in no package installed").For(p).
				Because("A sudoers file grants root; Hestia manages its own (admin), anything else should be known.").
				Fixed("cat "+p+"; add it to /etc/hs/unowned.allowed if it is yours"))
		}
	}
	allow := allowPrefixes(env)
	var out []Result
	for _, r := range rs {
		if !allowed(r.Subject, allow) {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return []Result{New(OK, "root is the only uid 0; sudoers drop-ins are packaged or allowed")}
	}
	return out
}

// Cron: files in /etc/cron.d that no package owns and root/user crontab lines
// that are not Hestia's own v-* commands, compared with the accepted list.
const AcceptedCron = "/etc/hs/cron.accepted"

var cronEnvLine = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*[ \t]*=`)

type CronEntry struct{ Where, Line string }

func (c CronEntry) ID() string {
	h := sha256.Sum256([]byte(c.Where + "\x00" + c.Line))
	return hex.EncodeToString(h[:8])
}

// CronEntries lists the cron entries a person has to vouch for.
func CronEntries(env *Env) []CronEntry {
	s := env.Sys
	owned := dpkgOwned(env)
	ours := map[string]bool{"/etc/cron.d/hs": true, "/etc/cron.d/hs-logcap": true, "/etc/cron.d/hs-watch": true, "/etc/cron.d/hestia-restic": true}
	var out []CronEntry
	files, _ := s.Glob("/etc/cron.d/*")
	tabs, _ := s.Glob("/var/spool/cron/crontabs/*")
	for _, f := range append(files, tabs...) {
		if owned[f] || ours[f] || strings.HasSuffix(f, "/.placeholder") {
			continue
		}
		for _, l := range strings.Split(readString(s, f), "\n") {
			t := strings.TrimSpace(l)
			if t == "" || strings.HasPrefix(t, "#") || cronEnvLine.MatchString(t) {
				continue
			}
			if strings.Contains(t, "/usr/local/hestia/bin/v-") || strings.Contains(t, "sudo /usr/local/hestia/bin/") {
				continue // Hestia's own jobs
			}
			out = append(out, CronEntry{Where: f, Line: t})
		}
	}
	return out
}

func checkCron(ctx context.Context, env *Env) []Result {
	entries := CronEntries(env)
	acc := map[string]bool{}
	for _, l := range strings.Split(readString(env.Sys, AcceptedCron), "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			acc[f[0]] = true
		}
	}
	if len(acc) == 0 && len(entries) > 0 {
		var ev []string
		for _, e := range entries {
			ev = append(ev, e.Where+": "+e.Line)
		}
		return []Result{New(Warn, fmt.Sprintf("%d cron entries, none reviewed yet", len(entries))).Ev(ev...).
			Because("A cron line is the simplest way to bring an implant back after a cleanup. Once the current ones are accepted, any new one is a finding.").
			Fixed("review the list, then hs op cron-accept")}
	}
	var rs []Result
	for _, e := range entries {
		if !acc[e.ID()] {
			rs = append(rs, New(Fail, "cron entry nobody accepted").For(e.Where+" "+e.ID()).Ev(e.Line).
				Because("A scheduled command appeared since cron was last reviewed.").
				Fixed("if it is known: hs op cron-accept; if not: capture it, remove it, find how it got there"))
		}
	}
	if len(rs) == 0 {
		return []Result{New(OK, fmt.Sprintf("%d cron entries, all accepted (Hestia's own v-* jobs not counted)", len(entries)))}
	}
	return rs
}

// --- sweeps ---------------------------------------------------------------------

func isELF(env *Env, p string) bool {
	b, err := env.Sys.ReadFile(p)
	return err == nil && len(b) >= 4 && string(b[:4]) == "\x7fELF"
}

func checkUnowned(ctx context.Context, env *Env) []Result {
	owned := dpkgOwned(env)
	allow := allowPrefixes(env)
	known := indicators(env)
	var roots []string
	for _, d := range []string{"/usr", "/etc", "/opt", "/var", "/root", "/boot", "/srv", "/tmp", "/dev/shm"} {
		if st, err := env.Sys.Stat(d); err == nil && st.IsDir() {
			roots = append(roots, d)
		}
	}
	args := append(roots, "-xdev", "(", "-path", "/var/lib/docker", "-o", "-path", "/var/lib/mysql", "-o", "-path", "/proc", ")", "-prune", "-o",
		"-type", "f", "(", "-perm", "/111", "-o", "-name", "*.so", "-o", "-name", "*.so.*", ")", "-size", "-200M", "-print")
	out, _ := env.Sys.Run(ctx, "find", args...)
	var rs []Result
	n := 0
	for _, p := range strings.Split(out, "\n") {
		if p == "" || owned[p] || allowed(p, allow) {
			continue
		}
		n++
		if !isELF(env, p) {
			continue
		}
		sum := sha256File(env, p)
		if note := known.Hash(sum); note != "" {
			rs = append(rs, New(Fail, "known malware binary: "+note).For(p).Ev("sha256 "+sum).
				Because("This file's hash matches an implant from a real compromise.").
				Fixed("preserve it (move to a root-only evidence directory with its hash), then investigate how it got here"))
			continue
		}
		rs = append(rs, New(Warn, "unpackaged executable").For(p).Ev("sha256 "+sum).
			Because("No package installed it. The May 2026 implants were all unpackaged ELF files; legitimate ones (hand-installed tools) go in /etc/hs/unowned.allowed.").
			Fixed("find out what it is; allow it in /etc/hs/unowned.allowed or remove it"))
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].State > rs[j].State })
	if len(rs) == 0 {
		return []Result{New(OK, "every executable and library is packaged or allowed").Valid(2 * time.Hour)}
	}
	return rs
}

func checkUnits(ctx context.Context, env *Env) []Result {
	owned := dpkgOwned(env)
	allow := append(allowPrefixes(env), "/etc/systemd/system/hestia-streamer.service", "/etc/systemd/system/hs-firewall.service",
		"/lib/systemd/system/hestia-iptables.service", "/usr/lib/systemd/system/hestia-iptables.service", "/etc/systemd/system/hestia-streamer.service.d/")
	var rs []Result
	for _, dir := range []string{"/etc/systemd/system", "/usr/lib/systemd/system"} {
		ents, _ := env.Sys.ReadDir(dir)
		for _, e := range ents {
			p := filepath.Join(dir, e.Name())
			if e.IsDir() || !(strings.HasSuffix(p, ".service") || strings.HasSuffix(p, ".timer")) {
				continue
			}
			if fi, err := env.Sys.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				continue // a symlink: enabling/aliasing a unit, or masking
			}
			if owned[p] || allowed(p, allow) {
				continue
			}
			rs = append(rs, New(Warn, "systemd unit no package installed").For(p).Ev(firstExecStart(readString(env.Sys, p))).
				Because("The May 2026 agents installed themselves as fake systemd units (ulibd.service, systemd-journal-upload.service).").
				Fixed("systemctl cat "+e.Name()+"; allow it in /etc/hs/unowned.allowed if it is yours"))
		}
	}
	if len(rs) == 0 {
		return []Result{New(OK, "every systemd unit is packaged or known")}
	}
	return rs
}

func firstExecStart(unit string) string {
	for _, l := range strings.Split(unit, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "ExecStart=") {
			return strings.TrimSpace(l)
		}
	}
	return "no ExecStart"
}

func webRootsAll(env *Env) []string {
	roots, _ := env.Sys.Glob("/home/*/web/*/public_html")
	if st, err := env.Sys.Stat("/var/www"); err == nil && st.IsDir() {
		roots = append(roots, "/var/www")
	}
	return roots
}

func checkWebshells(ctx context.Context, env *Env) []Result {
	roots := webRootsAll(env)
	if len(roots) == 0 {
		return []Result{New(NA, "no web roots")}
	}
	known := indicators(env)
	var rs []Result
	seen := map[string]bool{}
	for _, m := range known.Of("marker") {
		out, _ := env.Sys.Run(ctx, "grep", append([]string{"-rlF", "--include=*.php", "-e", m.Value}, roots...)...)
		for _, p := range strings.Split(out, "\n") {
			if p != "" && !seen[p] {
				seen[p] = true
				rs = append(rs, New(Fail, "webshell: "+m.Note).For(p).
					Because("This file carries the password gate of the webshell dropped into every web root in May 2026 (maldet did not detect it).").
					Fixed("hs fix uploads-php style: quarantine it (root-only, keep the hash), then check the site's other files and users"))
			}
		}
	}
	// Code that evaluates request data directly: rare in legitimate plugins.
	pat := `(eval|assert|create_function)[[:space:]]*\([[:space:]]*(base64_decode|gzinflate|str_rot13|gzuncompress)?[[:space:]]*\(?[[:space:]]*\$_(POST|GET|REQUEST|COOKIE|SERVER\[.HTTP_)`
	out, _ := env.Sys.Run(ctx, "grep", append([]string{"-rlE", "--include=*.php", "--exclude-dir=wp-includes", "--exclude-dir=wp-admin", "-e", pat}, roots...)...)
	for _, p := range strings.Split(out, "\n") {
		if p != "" && !seen[p] {
			seen[p] = true
			rs = append(rs, New(Warn, "PHP evaluating request data").For(p).
				Because("eval/assert on $_POST/$_GET/$_COOKIE is how webshells take commands; a legitimate plugin almost never does it.").
				Fixed("read the file; quarantine it if it is not a plugin you can name"))
		}
	}
	if len(rs) == 0 {
		return []Result{New(OK, fmt.Sprintf("no webshell markers or request-eval code in %d web roots", len(roots)))}
	}
	return rs
}

func checkRootOwned(ctx context.Context, env *Env) []Result {
	roots := webRootsAll(env)
	var sites []string
	for _, r := range roots {
		if strings.HasPrefix(r, "/home/") {
			sites = append(sites, r)
		}
	}
	if len(sites) == 0 {
		return []Result{New(NA, "no site web roots")}
	}
	var rs []Result
	for _, site := range sites {
		out, _ := env.Sys.Run(ctx, "find", site, "-xdev", "-name", "*.php", "-user", "root", "-print")
		files := nonEmpty(out)
		if len(files) == 0 {
			continue
		}
		total, _ := env.Sys.Run(ctx, "sh", "-c", `find "$1" -xdev -name '*.php' | wc -l`, "sh", site)
		n, _ := strconv.Atoi(strings.TrimSpace(total))
		sample := files
		if len(sample) > 8 {
			sample = append(sample[:8], fmt.Sprintf("… %d more", len(files)-8))
		}
		if n > 0 && len(files)*2 > n {
			// Most of the site: a copy made as root, a permissions problem.
			rs = append(rs, New(Warn, fmt.Sprintf("%d of %d PHP files owned by root", len(files), n)).For(site).Ev(sample...).
				Because("The site was copied in as root (a migration or restore). It works, but it hides the one signal that matters here: a webshell written with root is root-owned too.").
				Fixed("Sites → Fix file ownership and permissions"))
			continue
		}
		for _, p := range files {
			rs = append(rs, New(Warn, "PHP file owned by root in a site").For(p).
				Because("Sites write their files as their own user; every webshell copy in May 2026 was root-owned, written by an attacker with root.").
				Fixed("read it; if it is legitimate, chown it to the site user"))
		}
	}
	if len(rs) == 0 {
		return []Result{New(OK, "no root-owned PHP in site web roots")}
	}
	return rs
}

// dpkg --verify: modified files that belong to packages, conffiles excluded
// (editing configuration is normal). What a rootkit that replaces a packaged
// binary looks like.
func checkPkgIntegrity(ctx context.Context, env *Env) []Result {
	out, err := env.Sys.Run(ctx, "dpkg", "--verify")
	if err != nil && strings.TrimSpace(out) == "" {
		return []Result{New(Unknown, "dpkg --verify failed").Ev(err.Error())}
	}
	var bad []string
	allow := allowPrefixes(env)
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 3 && f[1] == "c" {
			continue // conffile
		}
		if len(f) >= 2 && allowed(f[len(f)-1], allow) {
			continue
		}
		if len(f) >= 2 && strings.Contains(f[0], "5") {
			bad = append(bad, strings.Join(f, " "))
		}
	}
	if len(bad) == 0 {
		return []Result{New(OK, "no packaged file differs from its package").Valid(8 * 24 * time.Hour)}
	}
	sort.Strings(bad)
	return []Result{New(Warn, fmt.Sprintf("%d packaged files differ from their package", len(bad))).Ev(bad...).
		Because("A modified binary or library outside a package upgrade is how a rootkit hides. Some are expected (files Hestia patches in place); anything in /usr/bin, /usr/sbin or /usr/lib is not.").
		Fixed("dpkg -S FILE; apt-get install --reinstall PACKAGE restores it")}
}
