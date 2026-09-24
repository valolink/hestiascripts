// Package logsrc is the catalogue of logs worth reading on a Hestia box, and
// a bounded tail reader for them. Only sources that exist are listed, and a
// read never loads more than the tail — alavus had a 7.3 GB debug.log.
package logsrc

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/sys"
)

type Source struct {
	Group string // System, Hestia, Web server, Security, Mail, PHP, Database, Backups, Site
	Name  string
	Path  string // file, or "" for a journal unit
	Unit  string // journalctl -u
	Size  int64
}

func (s Source) Label() string {
	if s.Unit != "" {
		return "journal: " + s.Unit
	}
	return s.Path
}

// fixed candidates; globs expand per box.
var candidates = []Source{
	{Group: "System", Name: "syslog", Path: "/var/log/syslog"},
	{Group: "System", Name: "kernel", Path: "/var/log/kern.log"},
	{Group: "System", Name: "apt history", Path: "/var/log/apt/history.log"},
	{Group: "System", Name: "unattended-upgrades", Path: "/var/log/unattended-upgrades/unattended-upgrades.log"},
	{Group: "Security", Name: "auth (SSH, sudo)", Path: "/var/log/auth.log"},
	{Group: "Security", Name: "fail2ban", Path: "/var/log/fail2ban.log"},
	{Group: "Security", Name: "maldet events", Path: "/usr/local/maldetect/logs/event_log"},
	{Group: "Hestia", Name: "panel system log", Path: "/usr/local/hestia/log/system.log"},
	{Group: "Hestia", Name: "panel errors", Path: "/usr/local/hestia/log/error.log"},
	{Group: "Hestia", Name: "backups", Path: "/usr/local/hestia/log/backup.log"},
	{Group: "Hestia", Name: "auth", Path: "/usr/local/hestia/log/auth.log"},
	{Group: "Hestia", Name: "nginx (panel)", Path: "/var/log/hestia/nginx-error.log"},
	{Group: "Web server", Name: "nginx errors", Path: "/var/log/nginx/error.log"},
	{Group: "Web server", Name: "apache errors", Path: "/var/log/apache2/error.log"},
	{Group: "Mail", Name: "mail", Path: "/var/log/mail.log"},
	{Group: "Database", Name: "MariaDB errors", Path: "/var/log/mysql/error.log"},
	{Group: "Database", Name: "Redis", Path: "/var/log/redis/redis-server.log"},
	{Group: "hs", Name: "hestia-streamer", Unit: "hestia-streamer"},
	{Group: "Backups", Name: "restic cron (syslog tag)", Unit: "cron"},
	{Group: "System", Name: "netdata", Unit: "netdata"},
	{Group: "Mail", Name: "postfix", Unit: "postfix@-"},
}

// List returns every readable source on the box: the fixed ones that exist,
// PHP-FPM logs per version, and per-domain access/error/debug logs.
func List(s sys.Sys) []Source {
	var out []Source
	add := func(src Source) {
		if src.Unit != "" {
			out = append(out, src)
			return
		}
		if fi, err := s.Stat(src.Path); err == nil && !fi.IsDir() {
			src.Size = fi.Size()
			out = append(out, src)
		}
	}
	for _, c := range candidates {
		add(c)
	}
	if fpm, _ := s.Glob("/var/log/php*-fpm.log"); len(fpm) > 0 {
		for _, p := range fpm {
			add(Source{Group: "PHP", Name: filepath.Base(p), Path: p})
		}
	}
	for _, d := range hestia.WebDomains(s) {
		for _, f := range []struct{ name, path string }{
			{"nginx access", "/var/log/nginx/domains/" + d.Name + ".log"},
			{"nginx errors", "/var/log/nginx/domains/" + d.Name + ".error.log"},
			{"apache access", "/var/log/apache2/domains/" + d.Name + ".log"},
			{"apache errors", "/var/log/apache2/domains/" + d.Name + ".error.log"},
			{"WordPress debug.log", d.DocRoot() + "/wp-content/debug.log"},
		} {
			add(Source{Group: "Site " + d.Name, Name: f.name, Path: f.path})
		}
		// kept-on debug logs and anything else logged into private/
		if priv, _ := s.Glob("/home/" + d.User + "/web/" + d.Name + "/private/*.log"); len(priv) > 0 {
			for _, p := range priv {
				add(Source{Group: "Site " + d.Name, Name: "private/" + filepath.Base(p), Path: p})
			}
		}
		// WP_DEBUG_LOG pointed elsewhere (debug-log-config-tool: debug-<hash>.log)
		if extra, _ := s.Glob(d.DocRoot() + "/wp-content/debug-*.log"); len(extra) > 0 {
			for _, p := range extra {
				add(Source{Group: "Site " + d.Name, Name: filepath.Base(p), Path: p})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		gi, gj := groupRank(out[i].Group), groupRank(out[j].Group)
		if gi != gj {
			return gi < gj
		}
		return out[i].Group < out[j].Group
	})
	return out
}

func groupRank(g string) int {
	order := []string{"System", "Security", "Hestia", "Backups", "Web server", "Mail", "PHP", "Database", "hs"}
	for i, o := range order {
		if g == o {
			return i
		}
	}
	return len(order) // sites last
}

// Tail returns up to n last lines. Files are read from the end in blocks,
// so the cost is bounded by n, not by the file size.
func Tail(ctx context.Context, s sys.Sys, src Source, n int) ([]string, error) {
	if src.Unit != "" {
		out, err := s.Run(ctx, "journalctl", "-u", src.Unit, "-n", itoa(n), "--no-pager", "-o", "short-iso")
		if err != nil && out == "" {
			return nil, err
		}
		return split(out), nil
	}
	return tailFile(src.Path, n)
}

func tailFile(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	const block = 64 << 10
	const maxBytes = 32 << 20 // at most 32 MiB however long the lines are
	var buf []byte
	pos := size
	for pos > 0 && bytes.Count(buf, []byte{'\n'}) <= n && int64(len(buf)) < maxBytes {
		step := int64(block)
		if pos < step {
			step = pos
		}
		pos -= step
		chunk := make([]byte, step)
		if _, err := f.ReadAt(chunk, pos); err != nil && err != io.EOF {
			return nil, err
		}
		buf = append(chunk, buf...)
	}
	lines := split(string(buf))
	if pos > 0 && len(lines) > 0 {
		lines = lines[1:] // first line is partial
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

func split(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func itoa(n int) string { return strconv.Itoa(n) }
