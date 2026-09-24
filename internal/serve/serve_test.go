package serve

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, token string) (*Server, string, string) {
	t.Helper()
	bin, backup := t.TempDir(), t.TempDir()
	script := func(name, body string) {
		p := filepath.Join(bin, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script("v-wp-echo", `echo "one"; echo "two" >&2; echo "arg=$1"; exit 3`)
	script("v-wp-longline", `head -c 200000 /dev/zero | tr '\0' 'x'; echo; echo after`)
	script("v-delete-user", `echo "must never run"`)
	script("v-restore-restic", `echo "must never run"`)
	netdata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"path":"` + r.URL.Path + `","q":"` + r.URL.RawQuery + `"}`))
	}))
	t.Cleanup(netdata.Close)
	return &Server{Token: token, BinDir: bin, BackupDir: func() string { return backup }, NetdataURL: netdata.URL}, bin, backup
}

func get(t *testing.T, h http.Handler, method, target, token string, body io.Reader) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	if token != "" {
		req.Header.Set("X-Streamer-Token", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestTokenGate(t *testing.T) {
	s, _, _ := newTestServer(t, "sekrit")
	h := s.Handler()
	for _, path := range []string{"/execute?script=v-wp-echo", "/download?file=a.zip", "/netdata/alarms", "/netdata/data?chart=system.cpu"} {
		if code, _ := get(t, h, "GET", path, "", nil); code != 403 {
			t.Errorf("%s without token: %d", path, code)
		}
		if code, _ := get(t, h, "GET", path, "wrong", nil); code != 403 {
			t.Errorf("%s wrong token: %d", path, code)
		}
	}
}

func TestExecuteStreamsAndReportsExit(t *testing.T) {
	s, _, _ := newTestServer(t, "k")
	code, body := get(t, s.Handler(), "GET", "/execute?script=v-wp-echo&arg=hello", "k", nil)
	if code != 200 {
		t.Fatalf("code %d", code)
	}
	for _, want := range []string{"data: one\n\n", "data: two\n\n", "data: arg=hello\n\n", "event: exit\ndata: 3\n\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %q", want, body)
		}
	}
	if !strings.HasSuffix(body, "event: exit\ndata: 3\n\n") {
		t.Errorf("exit event must be last: %q", body)
	}
}

func TestExecuteAllowlist(t *testing.T) {
	s, _, _ := newTestServer(t, "")
	for _, name := range []string{"v-delete-user", "v-restore-restic", "../v-wp-echo", "v-wp-echo;id", "bash", ""} {
		_, body := get(t, s.Handler(), "GET", "/execute?script="+name, "", nil)
		if strings.Contains(body, "must never run") || !strings.Contains(body, "unauthorized") || !strings.HasSuffix(body, "event: exit\ndata: 1\n\n") {
			t.Errorf("%q: %q", name, body)
		}
	}
	for name, want := range map[string]bool{"v-wp-update": true, "v-server-health": true, "v-dump-site": true, "v-hestiascripts-update": true, "v-add-user": false, "v-restore-restic": false} {
		if IsAllowedScript(name) != want {
			t.Errorf("IsAllowedScript(%q) != %v", name, want)
		}
	}
}

// The old streamer's scanner stopped at 64 KiB and the exit event never came.
func TestExecuteSurvivesLongLine(t *testing.T) {
	s, _, _ := newTestServer(t, "")
	_, body := get(t, s.Handler(), "GET", "/execute?script=v-wp-longline", "", nil)
	if !strings.Contains(body, "data: after\n\n") || !strings.HasSuffix(body, "event: exit\ndata: 0\n\n") {
		t.Errorf("long line broke the stream (len %d), tail %q", len(body), body[max(0, len(body)-80):])
	}
}

func TestDownloadOneShotExceptTar(t *testing.T) {
	s, _, backup := newTestServer(t, "")
	os.WriteFile(filepath.Join(backup, "site_1.zip"), []byte("zipbytes"), 0o644)
	os.WriteFile(filepath.Join(backup, "u.2026-09-24.tar"), []byte("tarbytes"), 0o644)
	h := s.Handler()
	if code, body := get(t, h, "GET", "/download?file=site_1.zip", "", nil); code != 200 || body != "zipbytes" {
		t.Fatalf("zip: %d %q", code, body)
	}
	if _, err := os.Stat(filepath.Join(backup, "site_1.zip")); !os.IsNotExist(err) {
		t.Error("zip should be deleted after download")
	}
	get(t, h, "GET", "/download?file=u.2026-09-24.tar", "", nil)
	if _, err := os.Stat(filepath.Join(backup, "u.2026-09-24.tar")); err != nil {
		t.Error("tar must stay after download")
	}
	for _, bad := range []string{"../etc/passwd", "x.php", ".hidden.zip", "a/b.zip"} {
		if code, _ := get(t, h, "GET", "/download?file="+bad, "", nil); code != 400 {
			t.Errorf("%q: %d", bad, code)
		}
	}
}

func TestUpload(t *testing.T) {
	s, _, backup := newTestServer(t, "")
	h := s.Handler()
	if code, _ := get(t, h, "GET", "/upload?file=u.tar", "", nil); code != 405 {
		t.Errorf("GET upload: %d", code)
	}
	if code, _ := get(t, h, "POST", "/upload?file=u.zip", "", strings.NewReader("x")); code != 400 {
		t.Errorf("non-tar upload: %d", code)
	}
	code, body := get(t, h, "POST", "/upload?file=u.2026.tar", "", strings.NewReader("archive"))
	if code != 200 || body != `{"ok":true,"file":"u.2026.tar","bytes":7}` {
		t.Fatalf("upload: %d %s", code, body)
	}
	if b, _ := os.ReadFile(filepath.Join(backup, "u.2026.tar")); string(b) != "archive" {
		t.Errorf("content %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(backup, ".upload-*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestNetdataDataRebuildsQuery(t *testing.T) {
	s, _, _ := newTestServer(t, "")
	h := s.Handler()
	code, body := get(t, h, "GET", "/netdata/data?chart=disk_space./home&after=-3600&points=60&group=max&evil=1", "", nil)
	if code != 200 || !strings.Contains(body, `"path":"/api/v1/data"`) || strings.Contains(body, "evil") {
		t.Errorf("data: %d %s", code, body)
	}
	for _, bad := range []string{"chart=../x", "chart=a;b", "chart=x&after=5", "chart=x&points=9999", "chart=x&group=median", "chart=x&dimensions=a%3Cb"} {
		if code, _ := get(t, h, "GET", "/netdata/data?"+bad, "", nil); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if code, body := get(t, h, "GET", "/netdata/alarms", "", nil); code != 200 || !strings.Contains(body, "/api/v1/alarms") {
		t.Errorf("alarms: %d %s", code, body)
	}
}
