package box

// SSH keys on the box (hardening plan §3): private keys that should not be
// here, and authorized keys nobody accepted. The May 2026 attackers found the
// operator's personal key on all three boxes, and it opened root on most of
// the fleet and push access to this repository.

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	. "github.com/valolink/hestiascripts/internal/check"
)

const (
	// AcceptedKeys: fingerprints of authorized_keys lines someone reviewed
	// (hs op ssh-keys-accept). One "SHA256:… file comment" per line.
	AcceptedKeys = "/etc/hs/ssh-keys.accepted"
	// AllowedPrivate: private key files that belong here (automation keys).
	AllowedPrivate = "/etc/hs/ssh-private-keys.allowed"
)

// DefaultAllowedPrivate are automation keys hs itself expects.
var DefaultAllowedPrivate = []string{"/root/.ssh/storagebox"}

func sshDirs(env *Env) []string {
	dirs := []string{"/root/.ssh"}
	homes, _ := env.Sys.Glob("/home/*/.ssh")
	return append(dirs, homes...)
}

// PrivateKeys lists private key files under the .ssh directories.
func PrivateKeys(env *Env) []string {
	var out []string
	for _, d := range sshDirs(env) {
		ents, _ := env.Sys.ReadDir(d)
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			p := filepath.Join(d, e.Name())
			head := readString(env.Sys, p)
			if len(head) > 200 {
				head = head[:200]
			}
			if strings.Contains(head, "PRIVATE KEY-----") {
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

func allowedPrivate(env *Env) map[string]bool {
	m := map[string]bool{}
	for _, p := range DefaultAllowedPrivate {
		m[p] = true
	}
	for _, l := range strings.Split(readString(env.Sys, AllowedPrivate), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			m[l] = true
		}
	}
	return m
}

func checkPrivateKeys(ctx context.Context, env *Env) []Result {
	allow := allowedPrivate(env)
	var rs []Result
	for _, p := range PrivateKeys(env) {
		if allow[p] {
			continue
		}
		fp, _ := env.Sys.Run(ctx, "ssh-keygen", "-lf", p)
		rs = append(rs, New(Fail, "private key on the server").For(p).Ev(strings.TrimSpace(fp)).
			Because("Root on this box is root wherever this key logs in: the May 2026 attackers used such a key to reach the rest of the fleet and GitHub. For server-to-server copies use a throwaway key made on the destination, removed afterwards.").
			Fixed("rotate it where it is authorized, then remove it here (fix below); an automation key that belongs here goes in "+AllowedPrivate))
	}
	if len(rs) == 0 {
		return []Result{New(OK, "no private keys besides the allowed automation keys")}
	}
	return rs
}

// AuthorizedKey is one line of an authorized_keys file.
type AuthorizedKey struct {
	File, Fingerprint, Comment string
}

// AuthorizedKeys fingerprints every authorized_keys line.
func AuthorizedKeys(ctx context.Context, env *Env) []AuthorizedKey {
	var out []AuthorizedKey
	for _, d := range sshDirs(env) {
		files, _ := env.Sys.Glob(d + "/authorized_keys*")
		for _, f := range files {
			res, _ := env.Sys.Run(ctx, "ssh-keygen", "-lf", f)
			for _, l := range strings.Split(res, "\n") {
				// "256 SHA256:abc comment (ED25519)"
				fs := strings.Fields(l)
				if len(fs) < 3 || !strings.HasPrefix(fs[1], "SHA256:") {
					continue
				}
				comment := strings.Join(fs[2:len(fs)-1], " ")
				out = append(out, AuthorizedKey{File: f, Fingerprint: fs[1], Comment: comment})
			}
		}
	}
	return out
}

func acceptedKeys(env *Env) map[string]bool {
	m := map[string]bool{}
	for _, l := range strings.Split(readString(env.Sys, AcceptedKeys), "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 {
			m[f[0]+" "+f[1]] = true
		}
	}
	return m
}

func checkAuthorizedKeys(ctx context.Context, env *Env) []Result {
	keys := AuthorizedKeys(ctx, env)
	acc := acceptedKeys(env)
	if len(acc) == 0 {
		var ev []string
		for _, k := range keys {
			ev = append(ev, k.File+"  "+k.Fingerprint+"  "+k.Comment)
		}
		return []Result{New(Warn, fmt.Sprintf("%d authorized keys, none reviewed yet", len(keys))).Ev(ev...).
			Because("A key someone adds is the quietest way back in. Once the current keys are reviewed and accepted, any new one is a finding.").
			Fixed("review the list, then hs op ssh-keys-accept")}
	}
	var rs []Result
	for _, k := range keys {
		if !acc[k.Fingerprint+" "+k.File] {
			rs = append(rs, New(Fail, "authorized key nobody accepted").For(k.File+" "+k.Fingerprint).Ev(k.Comment).
				Because("A login key appeared since the keys were last reviewed — the obvious way to keep access to a box.").
				Fixed("if it is yours: hs op ssh-keys-accept; if not: remove the line, rotate what it could reach, look for how it got there"))
		}
	}
	if len(rs) == 0 {
		return []Result{New(OK, fmt.Sprintf("%d authorized keys, all accepted", len(keys)))}
	}
	return rs
}
