package aptinfo

import (
	"strings"
	"testing"
)

const sim = `Reading package lists...
The following packages have been kept back:
  linux-image-amd64 php8.3-fpm
The following packages will be upgraded:
Inst libssl3 [3.0.15-1~deb12u1] (3.0.17-1~deb12u2 Debian-Security:12/stable-security [amd64])
Inst tzdata [2025a-0+deb12u1] (2025b-0+deb12u1 Debian:12.11/stable [all])
Inst mariadb-server [1:10.11.11-0+deb12u1] (1:10.11.13-0+deb12u1 Debian:12.11/stable [amd64])
Inst nginx [1.26.3-1~bookworm] (1.28.0-1~bookworm nginx:stable [amd64])
Inst curl [7.88.1-10+deb12u12] (7.88.1-10+deb12u14 Debian-Security:12/stable-security [amd64])
Inst libfoo2 (2.1-1 Debian:12.11/stable [amd64])
Conf libssl3 (3.0.17-1~deb12u2 Debian-Security:12/stable-security [amd64])
`

func TestClassify(t *testing.T) {
	for _, c := range [][3]string{
		{"7.88.1-10+deb12u12", "7.88.1-10+deb12u14", "rebuild"},
		{"1:10.11.11-0+deb12u1", "1:10.11.13-0+deb12u1", "patch"},
		{"1.26.3-1~bookworm", "1.28.0-1~bookworm", "minor"},
		{"2.9-1", "3.0-1", "major"},
		{"2025a-0+deb12u1", "2025b-0+deb12u1", "data"},
		{"20230311", "20240203", "data"},
	} {
		if got := Classify(c[0], c[1]); got != c[2] {
			t.Errorf("%s → %s: %s, want %s", c[0], c[1], got, c[2])
		}
	}
}

func TestParseAndRender(t *testing.T) {
	p := Parse(sim)
	if len(p.Upgrades) != 5 || len(p.New) != 1 || p.New[0] != "libfoo2" {
		t.Fatalf("%+v", p)
	}
	if strings.Join(p.Held, " ") != "linux-image-amd64 php8.3-fpm" {
		t.Errorf("held %v", p.Held)
	}
	if p.Security() != 2 {
		t.Errorf("security %d", p.Security())
	}
	out := strings.Join(p.Render(8), "\n")
	for _, want := range []string{
		"patch    mariadb-server             10.11.11 → 10.11.13  (restarts the database)",
		"rebuild  curl                       7.88.1-10+deb12u12 → 7.88.1-10+deb12u14",
		"minor    nginx",
		"Routine (3)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if !strings.Contains(out[strings.Index(out, "Worth a look"):strings.Index(out, "Routine")], "mariadb-server") {
		t.Error("mariadb must be worth a look")
	}
}

func TestParseUpdate(t *testing.T) {
	ok, total, errs := ParseUpdate("Hit:1 http://deb.debian.org/debian bookworm InRelease\nErr:2 https://dlm.mariadb.com/repo/mariadb-server/11.4/repo/debian bookworm InRelease\n  403  Forbidden [IP: 1.2.3.4 443]\nGet:3 http://x y InRelease\n")
	if ok != 2 || total != 3 || len(errs) != 1 || errs[0].Suite != "bookworm" || !strings.HasPrefix(errs[0].Reason, "403") {
		t.Fatalf("%d %d %+v", ok, total, errs)
	}
	if m, _ := Advice(errs[0].Reason); !strings.Contains(m, "stopped serving") {
		t.Error(m)
	}
}
