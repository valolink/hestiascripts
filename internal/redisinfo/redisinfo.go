// Package redisinfo reads how WordPress sites on a box use Redis: which
// database and prefix each site's object cache writes to, and what flush
// behaviour its wp-config selects. Shared by the checks and the fixes so
// both always agree on who shares what.
//
// What the redis-cache plugin (rhubarbgroup, drop-in object-cache.php)
// does, read from its 2.8.0 source and documented in its README/FAQ:
//
//   - flush() — `wp cache flush`, Settings → Redis — is FLUSHDB of the site's
//     database, unless WP_REDIS_SELECTIVE_FLUSH is set, in which case it runs
//     a Lua script that SCANs the database and deletes the prefix's keys.
//     The README lists SELECTIVE_FLUSH as unsupported: "terribly slow".
//   - flush_group() runs a Lua SCAN over the whole database for the group,
//     unless WP_REDIS_DISABLE_GROUP_FLUSH is set, in which case it calls
//     flush() — FLUSHDB (or the selective scan, if that is also set).
//   - WP_REDIS_MAXTTL bounds every key's lifetime; without it keys never
//     expire. The FAQ's answer to a growing Redis is MAXTTL plus one flush.
//   - The FAQ requires a separate WP_REDIS_DATABASE and WP_REDIS_PREFIX per
//     site ("my site is redirected to another domain" is two sites sharing).
//
// So the healthy layout is: one database per site, a prefix, MAXTTL, no
// SELECTIVE_FLUSH. With a database of its own, FLUSHDB only ever touches
// that one site, which is what makes DISABLE_GROUP_FLUSH safe.
package redisinfo

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/sys"
)

type Site struct {
	Domain        hestia.Domain
	DB            int
	DBSet         bool
	Prefix        string
	Selective     bool
	GroupFlushOff bool
	MaxTTL        string
	DropinVersion string   // "" = not the redis-cache drop-in (or unversioned)
	PluginVersion string   // installed redis-cache plugin, "" if absent
	SharesWith    []string // other object-cache sites in the same database
	SamePrefix    []string // … of those, the ones with the same prefix: they read each other's keys
}

var defineRe = regexp.MustCompile(`define\s*\(\s*['"](WP_REDIS_[A-Z_]+)['"]\s*,\s*([^)]*?)\s*\)`)

// defines reads every WP_REDIS_* define, several per line if need be, and
// skips commented-out lines (//, #, and lines inside /* */ blocks).
func defines(conf string) [][]string {
	var out [][]string
	inBlock := false
	for _, l := range strings.Split(conf, "\n") {
		t := strings.TrimSpace(l)
		if inBlock {
			if strings.Contains(t, "*/") {
				inBlock = false
			}
			continue
		}
		if strings.HasPrefix(t, "/*") && !strings.Contains(t, "*/") {
			inBlock = true
			continue
		}
		if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "*") {
			continue
		}
		out = append(out, defineRe.FindAllStringSubmatch(l, -1)...)
	}
	return out
}

var versionRe = regexp.MustCompile(`(?m)^\s*\*?\s*Version:\s*(\S+)`)

func truthy(v string) bool {
	v = strings.Trim(strings.ToLower(v), `'" `)
	return v == "true" || v == "1"
}

// Sites lists every site with an object-cache drop-in, and who shares its
// database with it.
func Sites(s sys.Sys) []Site {
	var out []Site
	for _, d := range hestia.WebDomains(s) {
		root := d.DocRoot()
		dropin, err := s.ReadFile(root + "/wp-content/object-cache.php")
		if err != nil {
			continue
		}
		conf, _ := s.ReadFile(root + "/wp-config.php")
		st := Site{Domain: d}
		for _, m := range defines(string(conf)) {
			v := strings.Trim(m[2], `'" `)
			switch m[1] {
			case "WP_REDIS_DATABASE":
				if n, err := strconv.Atoi(v); err == nil {
					st.DB, st.DBSet = n, true
				}
			case "WP_REDIS_PREFIX":
				st.Prefix = v
			case "WP_REDIS_SELECTIVE_FLUSH":
				st.Selective = truthy(m[2])
			case "WP_REDIS_DISABLE_GROUP_FLUSH":
				st.GroupFlushOff = truthy(m[2])
			case "WP_REDIS_MAXTTL":
				st.MaxTTL = v
			}
		}
		if strings.Contains(string(dropin), "Redis Object Cache") || strings.Contains(string(dropin), "redis-cache") {
			if m := versionRe.FindSubmatch(dropin); m != nil {
				st.DropinVersion = string(m[1])
			}
		}
		if p, err := s.ReadFile(root + "/wp-content/plugins/redis-cache/redis-cache.php"); err == nil {
			if m := versionRe.FindSubmatch(p); m != nil {
				st.PluginVersion = string(m[1])
			}
		}
		out = append(out, st)
	}
	byDB := map[int][]string{}
	for _, st := range out {
		byDB[st.DB] = append(byDB[st.DB], st.Domain.Name)
	}
	prefixOf := map[string]string{}
	for _, st := range out {
		prefixOf[st.Domain.Name] = st.Prefix
	}
	for i := range out {
		for _, n := range byDB[out[i].DB] {
			if n != out[i].Domain.Name {
				out[i].SharesWith = append(out[i].SharesWith, n)
				if prefixOf[n] == out[i].Prefix {
					out[i].SamePrefix = append(out[i].SamePrefix, n)
				}
			}
		}
	}
	return out
}

// Databases is Redis's `databases` setting (16 when unknown).
func Databases(ctx context.Context, s sys.Sys) int {
	out, err := s.Run(ctx, "redis-cli", "config", "get", "databases")
	if err == nil {
		if f := strings.Fields(out); len(f) > 0 {
			if n, err := strconv.Atoi(f[len(f)-1]); err == nil && n > 0 {
				return n
			}
		}
	}
	return 16
}

// KeysPerDB reads INFO keyspace.
func KeysPerDB(ctx context.Context, s sys.Sys) map[int]int {
	out, _ := s.Run(ctx, "redis-cli", "info", "keyspace")
	m := map[int]int{}
	for _, l := range strings.Split(out, "\n") {
		if mm := regexp.MustCompile(`^db(\d+):keys=(\d+)`).FindStringSubmatch(strings.TrimSpace(l)); mm != nil {
			db, _ := strconv.Atoi(mm[1])
			n, _ := strconv.Atoi(mm[2])
			m[db] = n
		}
	}
	return m
}

// FreeDB picks the lowest database from 1 up that no wp-config on the box
// names (any site, cached or not — including half-built staging copies) and
// that holds no keys (something else may own it). Database 0 is never
// handed out: it is the default everything unconfigured falls into.
func FreeDB(ctx context.Context, s sys.Sys) (int, bool) {
	used := map[int]bool{}
	confs, _ := s.Glob("/home/*/web/*/public_html/wp-config.php")
	setup, _ := s.Glob("/home/*/web/*/public_html.setup/wp-config.php")
	for _, c := range append(confs, setup...) {
		b, _ := s.ReadFile(c)
		for _, m := range defines(string(b)) {
			if m[1] == "WP_REDIS_DATABASE" {
				if n, err := strconv.Atoi(strings.Trim(m[2], `'" `)); err == nil {
					used[n] = true
				}
			}
		}
	}
	keys := KeysPerDB(ctx, s)
	max := Databases(ctx, s)
	for i := 1; i < max; i++ {
		if !used[i] && keys[i] == 0 {
			return i, true
		}
	}
	return 0, false
}

// NewPrefix follows v-wp-redis-install: domain (a-z0-9_, 20 chars) + 8 hex.
func NewPrefix(domain, hex8 string) string {
	b := regexp.MustCompile(`[^a-z0-9]`).ReplaceAllString(strings.ToLower(domain), "_")
	if len(b) > 20 {
		b = b[:20]
	}
	return b + "_" + hex8
}

// SortedNames is a helper for stable output.
func SortedNames(ns []string) []string {
	out := append([]string{}, ns...)
	sort.Strings(out)
	return out
}
