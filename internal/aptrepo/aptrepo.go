// Package aptrepo answers "is every apt repository still delivering updates?"
// without running apt-get update: it reads the configured sources, checks
// each one's signing keyring, fetches the repository's InRelease into hs's own
// state directory and verifies it with gpgv against that keyring — the same
// test apt makes, minus touching /var/lib/apt.
//
// Why it exists (viona, 2026-09-30): the Hestia keyring had been an empty file
// since the box was installed, so Hestia never updated past 1.10.2; the sury
// keyring held a key that expired on 2026-02-04, so PHP and Apache quietly
// ran on stale lists. apt-daily reported "Finished" every day. A failing
// repository costs no error anywhere — only the updates that never arrive.
package aptrepo

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valolink/hestiascripts/internal/sys"
)

// Repo is one (URI, suite) pair apt fetches an InRelease for.
type Repo struct {
	File       string   // where it is defined
	URI        string   // as written, trailing slash trimmed
	Suite      string   // "bookworm", or a flat-repo path ending in "/"
	Components []string // empty for flat repos
	SignedBy   []string // keyring paths; empty = apt's global trusted keyrings
	InlineKey  string   // deb822 Signed-By holding the key block itself
}

// Flat repositories name a directory instead of a suite ("deb URI dir/").
func (r Repo) Flat() bool { return strings.HasSuffix(r.Suite, "/") }

// InReleaseURL follows apt's layout: URI/dists/SUITE/InRelease, or for a flat
// repository URI/DIR/InRelease.
func (r Repo) InReleaseURL() string {
	if r.Flat() {
		dir := strings.TrimPrefix(r.Suite, "./")
		return r.URI + "/" + dir + "InRelease"
	}
	return r.URI + "/dists/" + r.Suite + "/InRelease"
}

// Host is the URI's host, for one-line summaries.
func (r Repo) Host() string {
	h := r.URI
	if _, after, ok := strings.Cut(h, "://"); ok {
		h = after
	}
	h, _, _ = strings.Cut(h, "/")
	return h
}

func (r Repo) key() string { return r.URI + " " + r.Suite }

// Sources lists every enabled deb repository in sources.list,
// sources.list.d/*.list and sources.list.d/*.sources (deb822).
func Sources(s sys.Sys) []Repo {
	var out []Repo
	files := []string{"/etc/apt/sources.list"}
	lists, _ := s.Glob("/etc/apt/sources.list.d/*.list")
	files = append(files, lists...)
	for _, f := range files {
		if b, err := s.ReadFile(f); err == nil {
			out = append(out, parseOneLine(f, string(b))...)
		}
	}
	d822, _ := s.Glob("/etc/apt/sources.list.d/*.sources")
	for _, f := range d822 {
		if b, err := s.ReadFile(f); err == nil {
			out = append(out, parseDeb822(f, string(b))...)
		}
	}
	// The same repository listed twice is fetched once.
	seen := map[string]bool{}
	var uniq []Repo
	for _, r := range out {
		if !seen[r.key()] {
			seen[r.key()] = true
			uniq = append(uniq, r)
		}
	}
	return uniq
}

func parseOneLine(file, text string) []Repo {
	var out []Repo
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != "deb" {
			continue // deb-src fetches no InRelease of its own worth checking twice
		}
		var opts []string
		rest := f[1:]
		if strings.HasPrefix(rest[0], "[") {
			var inOpts []string
			j := 0
			for ; j < len(rest); j++ {
				inOpts = append(inOpts, rest[j])
				if strings.HasSuffix(rest[j], "]") {
					break
				}
			}
			opts = strings.Fields(strings.Trim(strings.Join(inOpts, " "), "[]"))
			rest = rest[min(j+1, len(rest)):]
		}
		if len(rest) < 2 {
			continue
		}
		r := Repo{File: file, URI: strings.TrimRight(rest[0], "/"), Suite: rest[1], Components: rest[2:]}
		for _, o := range opts {
			if k, v, ok := strings.Cut(o, "="); ok && k == "signed-by" {
				r.SignedBy = append(r.SignedBy, strings.Split(v, ",")...)
			}
		}
		out = append(out, r)
	}
	return out
}

func parseDeb822(file, text string) []Repo {
	var out []Repo
	for _, stanza := range regexp.MustCompile(`\n\s*\n`).Split(strings.ReplaceAll(text, "\r", ""), -1) {
		fields := map[string]string{}
		var last string
		for _, line := range strings.Split(stanza, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && last != "" {
				v := strings.TrimSpace(line)
				if v == "." {
					v = ""
				}
				fields[last] += "\n" + v
				continue
			}
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			last = strings.ToLower(strings.TrimSpace(k))
			fields[last] = strings.TrimSpace(v)
		}
		if strings.EqualFold(strings.TrimSpace(fields["enabled"]), "no") || !contains(strings.Fields(fields["types"]), "deb") {
			continue
		}
		var signed []string
		inline := ""
		if sb := strings.TrimSpace(fields["signed-by"]); strings.Contains(sb, "BEGIN PGP PUBLIC KEY BLOCK") {
			inline = sb
		} else if sb != "" {
			signed = strings.Fields(strings.ReplaceAll(sb, ",", " "))
		}
		for _, uri := range strings.Fields(fields["uris"]) {
			for _, suite := range strings.Fields(fields["suites"]) {
				out = append(out, Repo{File: file, URI: strings.TrimRight(uri, "/"), Suite: suite,
					Components: strings.Fields(fields["components"]), SignedBy: signed, InlineKey: inline})
			}
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Status is one repository's verdict.
type Status struct {
	Repo   Repo
	OK     bool
	Reason string // why it is failing, plain language
	Fix    string
	// Skipped: not probed (not http/https — cdrom:, file:, mirror+file:).
	Skipped bool
}

// Keyrings: the repository's own signed-by files, or apt's global keyrings.
func keyrings(s sys.Sys, r Repo) []string {
	if len(r.SignedBy) > 0 {
		return r.SignedBy
	}
	var out []string
	if _, err := s.Stat("/etc/apt/trusted.gpg"); err == nil {
		out = append(out, "/etc/apt/trusted.gpg")
	}
	for _, pat := range []string{"/etc/apt/trusted.gpg.d/*.gpg", "/etc/apt/trusted.gpg.d/*.asc"} {
		g, _ := s.Glob(pat)
		out = append(out, g...)
	}
	return out
}

const (
	fixKey = "re-import the vendor's signing key into the keyring the source names (signed-by), leave the URL alone; hs apt repos shows apt's own view"
	fixNet = "hs apt repos   # apt's own view; if the host is gone, point the line at another mirror of the same version"
)

// Check verifies every repository. dir is hs's probe directory (downloaded
// InRelease files, dearmored keyrings, a private GNUPGHOME) — nothing outside
// it is written.
func Check(ctx context.Context, s sys.Sys, dir string) []Status {
	repos := Sources(s)
	out := make([]Status, len(repos))
	if len(repos) == 0 {
		return out
	}
	if err := os.MkdirAll(filepath.Join(dir, "gnupg"), 0o700); err != nil {
		for i, r := range repos {
			out[i] = Status{Repo: r, Skipped: true, Reason: "probe directory: " + err.Error()}
		}
		return out
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i, r := range repos {
		wg.Add(1)
		go func(i int, r Repo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = checkOne(ctx, s, dir, r)
		}(i, r)
	}
	wg.Wait()
	return out
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func checkOne(ctx context.Context, s sys.Sys, dir string, r Repo) Status {
	st := Status{Repo: r}
	if !strings.HasPrefix(r.URI, "http://") && !strings.HasPrefix(r.URI, "https://") {
		st.OK, st.Skipped = true, true
		return st
	}
	name := unsafeName.ReplaceAllString(r.key(), "_")

	// 1. Offline: a keyring that is missing or empty verifies nothing.
	var keys []string
	var bad []string
	if r.InlineKey != "" {
		p := filepath.Join(dir, name+".inline.gpg")
		if bin, err := dearmor(r.InlineKey); err == nil && os.WriteFile(p, bin, 0o600) == nil {
			keys = append(keys, p)
		} else {
			bad = append(bad, "the inline Signed-By key in "+r.File+" does not decode")
		}
	}
	for _, k := range keyrings(s, r) {
		fi, err := s.Stat(k)
		switch {
		case err != nil:
			bad = append(bad, k+" does not exist")
			continue
		case fi.Size() == 0:
			bad = append(bad, k+" is empty")
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		st.Reason = "no signing key: " + strings.Join(bad, "; ") + " — this repository delivers no updates"
		if len(bad) == 0 {
			st.Reason = "no signing key configured — this repository delivers no updates"
		}
		st.Fix = fixKey
		return st
	}

	// 2. Offline: every primary key in the keyrings expired.
	if exp, ok := allExpired(ctx, s, dir, keys); ok {
		st.Reason = "its signing key expired on " + exp.UTC().Format("2006-01-02") + " — apt refuses new package lists and keeps using stale ones"
		st.Fix = fixKey
		return st
	}

	// 3. The network probe: fetch InRelease, verify it like apt does.
	pctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	code, body, err := s.HTTPGet(pctx, r.InReleaseURL(), nil)
	cancel()
	switch {
	case err != nil:
		st.Reason = "InRelease not reachable: " + shortErr(err)
		st.Fix = fixNet
		return st
	case code != 200:
		st.Reason = fmt.Sprintf("InRelease answered HTTP %d", code)
		st.Fix = fixNet
		return st
	}
	file := filepath.Join(dir, name+".InRelease")
	if err := os.WriteFile(file, body, 0o600); err != nil {
		st.Skipped, st.OK, st.Reason = true, true, "could not store the probe: "+err.Error()
		return st
	}
	var gkeys []string
	for _, k := range keys {
		if strings.HasSuffix(k, ".asc") {
			b, err := s.ReadFile(k)
			if err != nil {
				continue
			}
			bin, err := dearmor(string(b))
			if err != nil {
				continue
			}
			p := filepath.Join(dir, name+"."+filepath.Base(k)+".gpg")
			if os.WriteFile(p, bin, 0o600) != nil {
				continue
			}
			k = p
		}
		gkeys = append(gkeys, "--keyring", k)
	}
	args := append([]string{"--homedir", filepath.Join(dir, "gnupg"), "--status-fd", "1"}, gkeys...)
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	outp, _ := s.Run(vctx, "gpgv", append(args, file)...)
	cancel()
	if reason := sigVerdict(outp); reason != "" {
		st.Reason = reason
		st.Fix = fixKey
		return st
	}
	if vu, ok := validUntil(string(body)); ok && vu.Before(s.Now()) {
		st.Reason = "the repository's Release file expired on " + vu.UTC().Format("2006-01-02") + " (Valid-Until) — a stale mirror"
		st.Fix = "switch to another mirror of the same suite; a stale mirror is an abandoned one"
		return st
	}
	st.OK = true
	return st
}

func shortErr(err error) string {
	m := err.Error()
	if i := strings.LastIndex(m, ": "); i >= 0 && len(m) > 120 {
		m = m[i+2:]
	}
	return m
}

// sigVerdict reads gpgv's --status-fd lines: "" for a good signature,
// else the reason in words.
func sigVerdict(status string) string {
	var expired string
	for _, l := range strings.Split(status, "\n") {
		f := strings.Fields(strings.TrimPrefix(l, "[GNUPG:] "))
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "KEYEXPIRED":
			if len(f) > 1 {
				if n, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					expired = time.Unix(n, 0).UTC().Format("2006-01-02")
				}
			}
		}
	}
	has := func(tok string) (string, bool) {
		for _, l := range strings.Split(status, "\n") {
			f := strings.Fields(strings.TrimPrefix(l, "[GNUPG:] "))
			if len(f) > 0 && f[0] == tok {
				if len(f) > 1 {
					return f[1], true
				}
				return "", true
			}
		}
		return "", false
	}
	if _, ok := has("GOODSIG"); ok {
		if _, ok := has("VALIDSIG"); ok {
			return ""
		}
	}
	if id, ok := has("EXPKEYSIG"); ok {
		if expired != "" {
			return "signed with key " + id + ", which expired on " + expired + " in this keyring (EXPKEYSIG) — the vendor has usually extended it; re-import"
		}
		return "signed with key " + id + ", which has expired in this keyring (EXPKEYSIG)"
	}
	if id, ok := has("NO_PUBKEY"); ok {
		return "signed with key " + id + ", which is not in the keyring (NO_PUBKEY) — the vendor rotated keys or the keyring is wrong"
	}
	if _, ok := has("BADSIG"); ok {
		return "the InRelease signature does not verify (BADSIG)"
	}
	if _, ok := has("ERRSIG"); ok {
		return "the InRelease signature could not be checked (ERRSIG)"
	}
	if strings.TrimSpace(status) == "" {
		return "gpgv gave no verdict (not installed?)"
	}
	return "no valid signature on InRelease"
}

func validUntil(inrelease string) (time.Time, bool) {
	for _, l := range strings.Split(inrelease, "\n") {
		if v, ok := strings.CutPrefix(l, "Valid-Until:"); ok {
			v = strings.TrimSpace(v)
			for _, layout := range []string{time.RFC1123, time.RFC1123Z, "Mon, 2 Jan 2006 15:04:05 MST"} {
				if t, err := time.Parse(layout, v); err == nil {
					return t, true
				}
			}
		}
		if l == "" || strings.HasPrefix(l, "-----BEGIN PGP SIGNATURE") {
			break // headers end at the first blank line
		}
	}
	return time.Time{}, false
}

// allExpired: every primary key in the keyrings has passed its expiry date.
// ok=false when gpg is missing or reports a key without expiry.
func allExpired(ctx context.Context, s sys.Sys, dir string, keys []string) (time.Time, bool) {
	var latest time.Time
	n := 0
	for _, k := range keys {
		gctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		out, err := s.Run(gctx, "env", "GNUPGHOME="+filepath.Join(dir, "gnupg"), "gpg", "--batch", "--no-options", "--with-colons", "--show-keys", k)
		cancel()
		if err != nil && out == "" {
			return time.Time{}, false
		}
		for _, l := range strings.Split(out, "\n") {
			f := strings.Split(l, ":")
			if len(f) < 7 || f[0] != "pub" {
				continue
			}
			n++
			if f[6] == "" {
				return time.Time{}, false // never expires
			}
			e, err := strconv.ParseInt(f[6], 10, 64)
			if err != nil {
				return time.Time{}, false
			}
			t := time.Unix(e, 0)
			if !t.Before(s.Now()) {
				return time.Time{}, false
			}
			if t.After(latest) {
				latest = t
			}
		}
	}
	return latest, n > 0
}

// dearmor decodes ASCII-armoured OpenPGP key blocks to the binary form gpgv
// reads. Several blocks are concatenated.
func dearmor(text string) ([]byte, error) {
	var out []byte
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	in, body := false, false
	var b64 strings.Builder
	flush := func() error {
		if b64.Len() == 0 {
			return nil
		}
		bin, err := base64.StdEncoding.DecodeString(b64.String())
		if err != nil {
			return err
		}
		out = append(out, bin...)
		b64.Reset()
		return nil
	}
	for _, l := range lines {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "-----BEGIN PGP PUBLIC KEY BLOCK"):
			in, body = true, false
		case strings.HasPrefix(l, "-----END PGP PUBLIC KEY BLOCK"):
			if err := flush(); err != nil {
				return nil, err
			}
			in = false
		case !in:
		case !body:
			if l == "" {
				body = true
			} else if !strings.Contains(l, ":") {
				body = true // no armor headers
				b64.WriteString(l)
			}
		case strings.HasPrefix(l, "="):
			// CRC24 checksum line
		default:
			b64.WriteString(l)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no key block")
	}
	return out, nil
}

// Summary counts: repositories checked and the failing ones, sorted.
func Summary(sts []Status) (total, ok int, failing []Status) {
	for _, st := range sts {
		if st.Skipped && st.OK {
			continue
		}
		total++
		if st.OK {
			ok++
		} else {
			failing = append(failing, st)
		}
	}
	sort.Slice(failing, func(i, j int) bool { return failing[i].Repo.key() < failing[j].Repo.key() })
	return
}
