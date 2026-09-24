// Package hestia reads HestiaCP's own state files. It parses the files
// directly rather than shelling out to v-list-* so a box-wide check costs
// microseconds, not a process per user.
package hestia

import (
	"sort"
	"strings"

	"github.com/valolink/hestiascripts/internal/sys"
)

const (
	Root      = "/usr/local/hestia"
	ConfPath  = Root + "/conf/hestia.conf"
	UsersDir  = Root + "/data/users"
	ResticSys = Root + "/conf/restic.conf"
	NginxTpl  = Root + "/data/templates/web/nginx"
	FpmTpl    = Root + "/data/templates/web/php-fpm"
)

// ParseKV parses Hestia's KEY='value' format: one record per line in *.conf
// lists (web.conf, db.conf), or one key per line in hestia.conf.
func ParseKV(line string) map[string]string {
	m := map[string]string{}
	for len(line) > 0 {
		line = strings.TrimLeft(line, " \t")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			break
		}
		key := line[:eq]
		rest := line[eq+1:]
		if !strings.HasPrefix(rest, "'") {
			// Unquoted value (hestia.conf allows it): up to the next space.
			sp := strings.IndexAny(rest, " \t")
			if sp < 0 {
				m[key] = rest
				break
			}
			m[key] = rest[:sp]
			line = rest[sp:]
			continue
		}
		end := strings.IndexByte(rest[1:], '\'')
		if end < 0 {
			m[key] = rest[1:]
			break
		}
		m[key] = rest[1 : 1+end]
		line = rest[end+2:]
	}
	return m
}

// Conf returns hestia.conf as a map. Missing file → empty map.
func Conf(s sys.Sys) map[string]string {
	b, err := s.ReadFile(ConfPath)
	if err != nil {
		return map[string]string{}
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		for k, v := range ParseKV(line) {
			m[k] = v
		}
	}
	return m
}

// Users lists Hestia users by their data directory, sorted.
func Users(s sys.Sys) []string {
	ents, err := s.ReadDir(UsersDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

type Domain struct {
	User    string
	Name    string
	Aliases []string
	Proxy   string // nginx proxy template (PROXY)
	Backend string // php-fpm pool template (BACKEND)
	Tpl     string // apache template (TPL)
	SSL     bool
	Suspend bool
}

func (d Domain) DocRoot() string {
	return "/home/" + d.User + "/web/" + d.Name + "/public_html"
}

// WebDomains lists every web domain of every user.
func WebDomains(s sys.Sys) []Domain {
	var out []Domain
	for _, u := range Users(s) {
		b, err := s.ReadFile(UsersDir + "/" + u + "/web.conf")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			kv := ParseKV(line)
			if kv["DOMAIN"] == "" {
				continue
			}
			d := Domain{
				User: u, Name: kv["DOMAIN"], Proxy: kv["PROXY"], Backend: kv["BACKEND"], Tpl: kv["TPL"],
				SSL: kv["SSL"] == "yes", Suspend: kv["SUSPENDED"] == "yes",
			}
			if kv["ALIAS"] != "" {
				d.Aliases = strings.Split(kv["ALIAS"], ",")
			}
			out = append(out, d)
		}
	}
	return out
}

// UsersWithDatabases lists users that own at least one database.
func UsersWithDatabases(s sys.Sys) []string {
	var out []string
	for _, u := range Users(s) {
		b, err := s.ReadFile(UsersDir + "/" + u + "/db.conf")
		if err != nil {
			continue
		}
		if strings.Contains(string(b), "DB='") {
			out = append(out, u)
		}
	}
	return out
}

// ResticRepo returns the system restic repo base (REPO= in conf/restic.conf),
// or "" when restic is not registered on this box.
func ResticRepo(s sys.Sys) string {
	b, err := s.ReadFile(ResticSys)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := ParseKV(line)["REPO"]; ok {
			return v
		}
	}
	return ""
}

// UserResticKey is the per-user restic password file; present = user keyed.
func UserResticKey(user string) string { return UsersDir + "/" + user + "/restic.conf" }
