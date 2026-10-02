// Package copies knows the WordPress site copies on a box and whether a
// staging copy still has live's media mounted read-only — the Go side of
// v-wp-copies and of v-wp-staging-create's media_binds / setup_copy_uploads.
//
// Layout since 2026-10-02: a copy's uploads are its own folder; live's media
// folders (past years, the current year's past months, ShortpixelBackups) are
// bound in read-only, one private bind each. Before it the whole of live's
// uploads was one read-only bind, so a copy could not save the font and CSS
// files plugins generate (kuumalahde dev1, 2026-10-01).
//
// Why the mount check exists (2026-09-30): staging copies made before the
// 2026-09-25 fstab persistence lost their bind mount at the next reboot
// (soutuveneet, delicatessen — rebooted 12:54 that day). Staging's
// wp-content/uploads was then its own 28 KB writable folder: no images on
// staging, and anything staging wrote there went nowhere near live.
package copies

import (
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/sys"
)

const (
	RegistryDir = "/root/.hestia-site-copies"
	URLStoreDir = "/root/.hestia-staging-urls"
	FstabTag    = "# hestia-staging-uploads"
)

// Copy is one copy of a live site. SourceDomain is "" when only the copy's
// wp-config says it is a staging site.
type Copy struct {
	Kind         string // "staging" or "clone"
	Domain       string
	User         string
	SourceDomain string
	SourceUser   string
	Origin       string // "registry", "url-store", "wp-config"
}

func docroot(user, domain string) string { return "/home/" + user + "/web/" + domain + "/public_html" }

// Uploads paths: live's (the mount source) and the copy's (the mount point).
func (c Copy) SourceUploads() string {
	return docroot(c.SourceUser, c.SourceDomain) + "/wp-content/uploads"
}
func (c Copy) Uploads() string { return docroot(c.User, c.Domain) + "/wp-content/uploads" }

var envStaging = regexp.MustCompile(`(?m)^\s*define\s*\(\s*['"]WP_ENVIRONMENT_TYPE['"]\s*,\s*['"](staging|development)['"]`)

// List every copy on the box: the copy registry first, then the older
// staged-URL store, then sites whose wp-config says staging/development.
func List(s sys.Sys) []Copy {
	domains := hestia.WebDomains(s)
	owner := map[string]string{}
	for _, d := range domains {
		owner[d.Name] = d.User
	}
	seen := map[string]bool{}
	var out []Copy
	files, _ := s.Glob(RegistryDir + "/*.conf")
	for _, f := range files {
		b, err := s.ReadFile(f)
		if err != nil {
			continue
		}
		kv := map[string]string{}
		for _, l := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
				kv[k] = strings.Trim(v, `"'`)
			}
		}
		c := Copy{Kind: kv["KIND"], Domain: kv["DOMAIN"], User: kv["USER"], SourceDomain: kv["SOURCE_DOMAIN"], SourceUser: kv["SOURCE_USER"], Origin: "registry"}
		if c.Domain == "" || seen[c.Domain] {
			continue
		}
		if c.User == "" {
			c.User = owner[c.Domain]
		}
		if c.SourceUser == "" {
			c.SourceUser = owner[c.SourceDomain]
		}
		seen[c.Domain] = true
		out = append(out, c)
	}
	// URL store: <src-user>_<src-domain> holding the staging URL. Domains
	// never contain "_", users may — split at the last one.
	store, _ := s.Glob(URLStoreDir + "/*")
	for _, f := range store {
		base := path.Base(f)
		i := strings.LastIndex(base, "_")
		if i <= 0 {
			continue
		}
		b, err := s.ReadFile(f)
		if err != nil {
			continue
		}
		u := strings.TrimSpace(string(b))
		if j := strings.Index(u, "://"); j >= 0 {
			u = u[j+3:]
		}
		dom, _, _ := strings.Cut(u, "/")
		if dom == "" || seen[dom] || owner[dom] == "" {
			continue
		}
		seen[dom] = true
		out = append(out, Copy{Kind: "staging", Domain: dom, User: owner[dom], SourceUser: base[:i], SourceDomain: base[i+1:], Origin: "url-store"})
	}
	for _, d := range domains {
		if seen[d.Name] {
			continue
		}
		b, err := s.ReadFile(d.DocRoot() + "/wp-config.php")
		if err != nil || !envStaging.Match(b) {
			continue
		}
		seen[d.Name] = true
		out = append(out, Copy{Kind: "staging", Domain: d.Name, User: d.User, Origin: "wp-config"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}

// Mount is what /proc/self/mountinfo says about a mount point.
type Mount struct {
	Mounted  bool
	ReadOnly bool
	Shared   bool   // in a peer group: mounts made beneath it propagate to the peers (live's uploads)
	Root     string // the mounted directory within its filesystem (the bind source)
}

// MountAt reads /proc/self/mountinfo (the last mount on a point wins).
func MountAt(s sys.Sys, point string) Mount {
	b, _ := s.ReadFile("/proc/self/mountinfo")
	var m Mount
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 6 || unescape(f[4]) != point {
			continue
		}
		m = Mount{Mounted: true, Root: unescape(f[3])}
		for _, o := range strings.Split(f[5], ",") {
			if o == "ro" {
				m.ReadOnly = true
			}
		}
		for _, o := range f[6:] { // optional fields up to the "-" separator
			if o == "-" {
				break
			}
			if strings.HasPrefix(o, "shared:") {
				m.Shared = true
			}
		}
	}
	return m
}

func unescape(p string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(p)
}

// FstabLine is the tagged line persist_uploads_mount writes for a mount
// point ("" when there is none).
func FstabLine(s sys.Sys, point string) string {
	b, _ := s.ReadFile("/etc/fstab")
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && f[1] == point && strings.Contains(l, FstabTag) {
			return l
		}
	}
	return ""
}

// MediaBinds is the list v-wp-staging-create's media_binds makes: live's
// uploads folders bound read-only into a copy, relative to uploads — the past
// years, the current year's past months, ShortpixelBackups.
func MediaBinds(s sys.Sys, live string, now time.Time) []string {
	var out []string
	cy, cm := now.Year(), int(now.Month())
	years, _ := s.ReadDir(live)
	for _, y := range years {
		n, err := strconv.Atoi(y.Name())
		if !y.IsDir() || len(y.Name()) != 4 || err != nil {
			continue
		}
		switch {
		case n < cy:
			out = append(out, y.Name())
		case n == cy:
			months, _ := s.ReadDir(live + "/" + y.Name())
			for _, m := range months {
				k, err := strconv.Atoi(m.Name())
				if m.IsDir() && len(m.Name()) == 2 && err == nil && k < cm {
					out = append(out, y.Name()+"/"+m.Name())
				}
			}
		}
	}
	if fi, err := s.Stat(live + "/ShortpixelBackups"); err == nil && fi.IsDir() {
		out = append(out, "ShortpixelBackups")
	}
	sort.Strings(out)
	return out
}

// WantBindFstab is the tagged line setup_copy_uploads writes for one bind.
func WantBindFstab(c Copy, rel string) string {
	dst := c.Uploads() + "/" + rel
	return c.SourceUploads() + "/" + rel + " " + dst + " none bind,ro,private,nofail 0 0 " + FstabTag + " " + dst
}
