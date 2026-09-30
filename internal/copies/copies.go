// Package copies knows the WordPress site copies on a box and whether a
// staging copy still has live's uploads mounted read-only — the Go side of
// v-wp-copies and of v-wp-staging-create's bind_mount_uploads_ro /
// persist_uploads_mount.
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
	"strings"

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

// WantFstab is the line persist_uploads_mount writes.
func WantFstab(c Copy) string {
	return c.SourceUploads() + " " + c.Uploads() + " none bind,ro,nofail 0 0 " + FstabTag + " " + c.Uploads()
}
