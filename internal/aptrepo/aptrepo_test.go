package aptrepo

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/valolink/hestiascripts/internal/sys"
)

var now = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// viona's sources, 2026-09-30 (trimmed).
func vionaFake() *sys.Fake {
	return &sys.Fake{Clock: now, Files: map[string]string{
		"/etc/apt/sources.list.d/hestia.list": "deb [arch=amd64 signed-by=/usr/share/keyrings/hestia-keyring.gpg] https://apt.hestiacp.com/ bookworm main\n",
		"/etc/apt/sources.list.d/php.list":    "deb [arch=amd64 signed-by=/usr/share/keyrings/sury-keyring.gpg] https://packages.sury.org/php/ bookworm main\n# deb-src https://packages.sury.org/php/ bookworm main\n",
		"/etc/apt/sources.list.d/debian.sources": "Types: deb\nURIs: mirror+file:///etc/apt/mirrors/debian.list\nSuites: bookworm bookworm-updates\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n\n" +
			"Types: deb\nURIs: https://deb.debian.org/debian-security\nSuites: bookworm-security\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n\n" +
			"Types: deb-src\nURIs: https://deb.debian.org/debian\nSuites: bookworm\nComponents: main\n",
		"/etc/apt/sources.list.d/flat.list":              "deb [signed-by=/usr/share/keyrings/flat.gpg] https://example.org/repo ./\n",
		"/usr/share/keyrings/hestia-keyring.gpg":         "",
		"/usr/share/keyrings/sury-keyring.gpg":           "binary",
		"/usr/share/keyrings/debian-archive-keyring.gpg": "binary",
		"/usr/share/keyrings/flat.gpg":                   "binary",
	}, Cmds: map[string]sys.FakeCmd{}, HTTP: map[string]sys.FakeHTTP{}}
}

func TestSources(t *testing.T) {
	rs := Sources(vionaFake())
	got := map[string]Repo{}
	for _, r := range rs {
		got[r.URI+" "+r.Suite] = r
	}
	if len(rs) != 6 {
		t.Fatalf("want 6 repos (deb-src skipped, two suites split), got %d: %+v", len(rs), rs)
	}
	h := got["https://apt.hestiacp.com bookworm"]
	if len(h.SignedBy) != 1 || h.InReleaseURL() != "https://apt.hestiacp.com/dists/bookworm/InRelease" {
		t.Errorf("hestia: %+v %s", h, h.InReleaseURL())
	}
	if f := got["https://example.org/repo ./"]; !f.Flat() || f.InReleaseURL() != "https://example.org/repo/InRelease" {
		t.Errorf("flat: %+v %s", f, f.InReleaseURL())
	}
	if _, ok := got["https://deb.debian.org/debian-security bookworm-security"]; !ok {
		t.Error("deb822 stanza missing")
	}
}

func TestCheck(t *testing.T) {
	f := vionaFake()
	dir := t.TempDir()
	gnupg := "GNUPGHOME=" + dir + "/gnupg"
	// sury: key expired 2026-02-04, and the network probe is never reached.
	f.Cmds["env "+gnupg+" gpg --batch --no-options --with-colons --show-keys /usr/share/keyrings/sury-keyring.gpg"] = sys.FakeCmd{Out: "pub:e:4096:1:B188E2B695BD4743:1580000000:1770200746::-:::sc::::::23::0:\n"}
	f.Cmds["env "+gnupg+" gpg --batch --no-options --with-colons --show-keys /usr/share/keyrings/debian-archive-keyring.gpg"] = sys.FakeCmd{Out: "pub:-:4096:1:AAAA:1600000000:::-:::sc:\n"}
	f.Cmds["env "+gnupg+" gpg --batch --no-options --with-colons --show-keys /usr/share/keyrings/flat.gpg"] = sys.FakeCmd{Out: "pub:-:4096:1:BBBB:1600000000:1900000000::-:::sc:\n"}
	f.HTTP["https://deb.debian.org/debian-security/dists/bookworm-security/InRelease"] = sys.FakeHTTP{Status: 200, Body: "Origin: Debian\nValid-Until: Tue, 07 Oct 2026 08:00:00 UTC\n\n..."}
	f.HTTP["https://example.org/repo/InRelease"] = sys.FakeHTTP{Status: 200, Body: "Origin: x\nValid-Until: Mon, 01 Jun 2026 00:00:00 UTC\n\n"}
	sec := dir + "/https_deb.debian.org_debian-security_bookworm-security.InRelease"
	f.Cmds["gpgv --homedir "+dir+"/gnupg --status-fd 1 --keyring /usr/share/keyrings/debian-archive-keyring.gpg "+sec] = sys.FakeCmd{Out: "[GNUPG:] GOODSIG 1234 Debian\n[GNUPG:] VALIDSIG ABCD\n"}
	flat := dir + "/https_example.org_repo_._.InRelease"
	f.Cmds["gpgv --homedir "+dir+"/gnupg --status-fd 1 --keyring /usr/share/keyrings/flat.gpg "+flat] = sys.FakeCmd{Out: "[GNUPG:] GOODSIG 1 x\n[GNUPG:] VALIDSIG 2\n"}

	sts := Check(context.Background(), f, dir)
	by := map[string]Status{}
	for _, s := range sts {
		by[s.Repo.Host()+" "+s.Repo.Suite] = s
	}
	if s := by["apt.hestiacp.com bookworm"]; s.OK || !strings.Contains(s.Reason, "is empty") {
		t.Errorf("empty hestia keyring: %+v", s)
	}
	if s := by["packages.sury.org bookworm"]; s.OK || !strings.Contains(s.Reason, "expired on 2026-02-04") {
		t.Errorf("expired sury key: %+v", s)
	}
	if s := by["deb.debian.org bookworm-security"]; !s.OK {
		t.Errorf("good repo: %+v", s)
	}
	if s := by["example.org ./"]; s.OK || !strings.Contains(s.Reason, "Valid-Until") {
		t.Errorf("stale mirror: %+v", s)
	}
	if s := by["file: bookworm"]; !s.Skipped {
		// mirror+file: host parse is odd; find it by URI instead
		for _, st := range sts {
			if strings.HasPrefix(st.Repo.URI, "mirror+file") && !st.Skipped {
				t.Errorf("mirror+file should be skipped: %+v", st)
			}
		}
	}
	total, ok, failing := Summary(sts)
	if total != 4 || ok != 1 || len(failing) != 3 {
		t.Errorf("summary %d/%d, failing %d", ok, total, len(failing))
	}
}

func TestSigVerdict(t *testing.T) {
	cases := map[string]string{
		"[GNUPG:] KEYEXPIRED 1770200746\n[GNUPG:] EXPKEYSIG B188E2B695BD4743 DEB.SURY.ORG\n":  "expired on 2026-02-04",
		"[GNUPG:] NO_PUBKEY A189E93654F0B0E5\n[GNUPG:] ERRSIG A189E93654F0B0E5 1 10 01 1 9\n": "NO_PUBKEY",
		"[GNUPG:] BADSIG 1 x\n": "BADSIG",
		"":                      "gpgv gave no verdict",
	}
	for in, want := range cases {
		if got := sigVerdict(in); !strings.Contains(got, want) {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	if sigVerdict("[GNUPG:] GOODSIG 1 x\n[GNUPG:] VALIDSIG 2\n") != "" {
		t.Error("good signature reported as failing")
	}
}

func TestDearmor(t *testing.T) {
	bin := []byte{0x99, 0x01, 0x0d, 0xff, 0x00}
	armored := "-----BEGIN PGP PUBLIC KEY BLOCK-----\nComment: test\n\n" + base64.StdEncoding.EncodeToString(bin) + "\n=abcd\n-----END PGP PUBLIC KEY BLOCK-----\n"
	got, err := dearmor(armored)
	if err != nil || string(got) != string(bin) {
		t.Errorf("dearmor: %v %x", err, got)
	}
	if _, err := dearmor("not a key"); err == nil {
		t.Error("garbage dearmored")
	}
}

func TestInlineSignedBy(t *testing.T) {
	src := "Types: deb\nURIs: https://repo.netdata.cloud/repos/edge/debian/\nSuites: bookworm/\nSigned-By:\n -----BEGIN PGP PUBLIC KEY BLOCK-----\n .\n " + base64.StdEncoding.EncodeToString([]byte{1, 2, 3}) + "\n -----END PGP PUBLIC KEY BLOCK-----\n"
	rs := parseDeb822("/etc/apt/sources.list.d/netdata-edge.sources", src)
	if len(rs) != 1 || rs[0].InlineKey == "" || !rs[0].Flat() || rs[0].InReleaseURL() != "https://repo.netdata.cloud/repos/edge/debian/bookworm/InRelease" {
		t.Fatalf("netdata inline key: %+v", rs)
	}
	if b, err := dearmor(rs[0].InlineKey); err != nil || len(b) != 3 {
		t.Errorf("inline dearmor: %v %v", err, b)
	}
}
