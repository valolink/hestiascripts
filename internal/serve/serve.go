// Package serve is the streamer EngineLink talks to (port 8091), ported from
// main.go's hestia-streamer. The HTTP contract is frozen — EngineLink's
// Operate, Setup and health flows depend on every endpoint, status code and
// SSE frame here:
//
//	/execute        SSE: one "data: <line>" per output line, then
//	                "event: exit\ndata: <code>". Allowlisted v-* scripts only.
//	/download       one-shot handoff of a dump from $BACKUP (.zip/.sql.gz
//	                deleted after serving; .tar kept)
//	/upload         .tar into $BACKUP via temp file + rename
//	/netdata/alarms passthrough to box-local Netdata
//	/netdata/data   rebuilt, validated query to Netdata /api/v1/data
//
// All share the optional X-Streamer-Token gate (constant-time compare).
package serve

import (
	"bufio"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Server struct {
	Token      string        // "" disables the gate (pre-token installs)
	BinDir     string        // where allowlisted scripts live
	BackupDir  func() string // Hestia's $BACKUP, resolved per request
	NetdataURL string        // box-local Netdata base
}

// Default returns the production configuration.
func Default(token string) *Server {
	return &Server{
		Token:      token,
		BinDir:     "/usr/local/hestia/bin/",
		BackupDir:  HestiaBackupDir,
		NetdataURL: "http://127.0.0.1:19999",
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/execute", s.execute)
	mux.HandleFunc("/download", s.download)
	mux.HandleFunc("/upload", s.upload)
	mux.HandleFunc("/netdata/alarms", s.alarms)
	mux.HandleFunc("/netdata/data", s.data)
	return mux
}

// checkToken enforces the shared secret when configured; writes the 403.
func (s *Server) checkToken(w http.ResponseWriter, r *http.Request) bool {
	if s.Token == "" {
		return true
	}
	got := r.Header.Get("X-Streamer-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	return true
}

var safeScriptRegex = regexp.MustCompile(`^v-[a-zA-Z0-9-]+$`)

// The name regex alone would admit every stock Hestia binary in BinDir,
// making the token root-equivalent (v-delete-user, v-add-user-ssh-key, …).
// Our own families pass by prefix — they only exist in BinDir because
// install-scripts.sh symlinked them from this repo; stock commands must be
// named exactly. Root-shell-only tools (v-restore-restic, safe-reboot,
// make-restore-script) are deliberately absent.
var allowedPrefixes = []string{"v-wp-", "v-server-"}

var allowedScripts = map[string]bool{
	"v-hestiascripts-update": true,
	// Site dumps (files + DB) for downloads / local dev pulls.
	"v-dump-site":     true,
	"v-dump-database": true,
	// Inventory & reconciliation (read-only listings).
	"v-list-users":                 true,
	"v-list-users-stats":           true,
	"v-list-web-domains":           true,
	"v-list-web-domain":            true,
	"v-list-web-domain-ssl":        true,
	"v-list-databases":             true,
	"v-list-user-backups":          true,
	"v-list-sys-services":          true,
	"v-list-sys-php":               true,
	"v-list-web-templates-backend": true,
	"v-search-domain-owner":        true,
	// Full-user backup for cross-server migration (source side).
	"v-backup-user": true,
	// Per-site operations.
	"v-purge-nginx-cache":             true,
	"v-rebuild-web-domain":            true,
	"v-add-letsencrypt-domain":        true,
	"v-add-web-domain-ssl-force":      true,
	"v-suspend-web-domain":            true,
	"v-unsuspend-web-domain":          true,
	"v-change-web-domain-backend-tpl": true,
	"v-schedule-user-backup":          true,
	// Privilege-dropped runner (wp / composer as the site's user).
	"v-run-cli-cmd": true,
	// Server-level maintenance.
	"v-restart-service":        true,
	"v-restart-web-backend":    true,
	"v-update-letsencrypt-ssl": true,
	"v-update-sys-hestia-all":  true,
}

// v-restore-restic matches the v- shape but is TTY-only and must never be
// reachable over HTTP; the v-server- prefix does not cover it, and this
// denylist makes that explicit should a prefix ever widen.
var deniedScripts = map[string]bool{"v-restore-restic": true}

func IsAllowedScript(name string) bool {
	if !safeScriptRegex.MatchString(name) || deniedScripts[name] {
		return false
	}
	if allowedScripts[name] {
		return true
	}
	for _, p := range allowedPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Longest single output line /execute will relay whole. The old streamer
// used bufio's 64 KiB default; one longer line stopped the scanner, nobody
// drained the pipe, the script blocked on write and the exit event never came.
const maxLine = 4 << 20

func (s *Server) execute(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	q := r.URL.Query()
	name := q.Get("script")
	if !IsAllowedScript(name) {
		fmt.Fprintf(w, "data: ERROR: Invalid or unauthorized script name.\n\n")
		fmt.Fprintf(w, "event: exit\ndata: 1\n\n")
		return
	}

	// filepath.Join + the name regex rule out traversal; exec without a shell
	// rules out injection through args.
	cmd := exec.Command(filepath.Join(s.BinDir, name), q["arg"]...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(w, "data: ERROR: Failed to create stdout pipe: %v\n\n", err)
		return
	}
	// After StdoutPipe(), which is what sets cmd.Stdout — before it, stderr
	// went to /dev/null and every script's real error was lost (to 2026-07-06).
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(w, "data: ERROR: Failed to start script: %v\n\n", err)
		return
	}

	// The script keeps running if the client goes away: an update or a
	// clone must not die half-done because a browser tab closed.
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		fmt.Fprintf(w, "data: %s\n\n", sc.Text())
		flusher.Flush()
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(w, "data: WARNING: output line over %d bytes; the rest of the output is discarded\n\n", maxLine)
		flusher.Flush()
		io.Copy(io.Discard, stdout)
	}

	code := 0
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	fmt.Fprintf(w, "event: exit\ndata: %d\n\n", code)
	flusher.Flush()
}

// Basenames only: site zips (v-dump-site), gzipped SQL (v-dump-database),
// full-user tars (v-backup-user).
var safeDumpRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.(zip|sql\.gz|tar)$`)

// HestiaBackupDir resolves $BACKUP from hestia.conf, default /backup. Read
// per request: one small file, and a Hestia reconfigure can change it.
func HestiaBackupDir() string {
	data, err := os.ReadFile("/usr/local/hestia/conf/hestia.conf")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "BACKUP=") {
				if v := strings.Trim(strings.TrimPrefix(line, "BACKUP="), `'" `); v != "" {
					return v
				}
			}
		}
	}
	return "/backup"
}

// download hands a finished dump to EngineLink. Ephemeral dumps are deleted
// after serving (one-shot handoff, not a stored backup); a .tar is a real
// Hestia user backup and stays.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	name := r.URL.Query().Get("file")
	if !safeDumpRegex.MatchString(name) || strings.Contains(name, "..") {
		http.Error(w, "Invalid file", http.StatusBadRequest)
		return
	}
	path := filepath.Join(s.BackupDir(), name)
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	ct := "application/zip"
	switch {
	case strings.HasSuffix(name, ".sql.gz"):
		ct = "application/gzip"
	case strings.HasSuffix(name, ".tar"):
		ct = "application/x-tar"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Content-Disposition", "attachment; filename="+name)
	io.Copy(w, f)
	if !strings.HasSuffix(name, ".tar") {
		os.Remove(path)
	}
}

// upload is the destination half of cross-server user migration: a .tar
// lands in $BACKUP via temp file + rename, so v-restore-user never sees a
// half-transferred archive.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := r.URL.Query().Get("file")
	if !safeDumpRegex.MatchString(name) || !strings.HasSuffix(name, ".tar") || strings.Contains(name, "..") {
		http.Error(w, "Invalid file", http.StatusBadRequest)
		return
	}
	dir := s.BackupDir()
	tmp, err := os.CreateTemp(dir, ".upload-*.tmp")
	if err != nil {
		http.Error(w, "Cannot create temp file", http.StatusInternalServerError)
		return
	}
	tmpPath := tmp.Name()
	n, err := io.Copy(tmp, r.Body)
	tmp.Close()
	if err != nil {
		os.Remove(tmpPath)
		http.Error(w, "Write failed", http.StatusInternalServerError)
		return
	}
	dst := filepath.Join(dir, name)
	if err := os.Rename(tmpPath, dst); err != nil {
		os.Remove(tmpPath)
		http.Error(w, "Rename failed", http.StatusInternalServerError)
		return
	}
	_ = os.Chmod(dst, 0o644)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"file":%q,"bytes":%d}`, name, n)
}

var netdataClient = &http.Client{Timeout: 8 * time.Second}

func (s *Server) proxyNetdata(w http.ResponseWriter, u string) {
	resp, err := netdataClient.Get(u)
	if err != nil {
		http.Error(w, fmt.Sprintf("Netdata unreachable: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (s *Server) alarms(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	s.proxyNetdata(w, s.NetdataURL+"/api/v1/alarms")
}

var (
	// Slashes for disk-space chart ids ("disk_space./home"); ".." is refused.
	safeChartRegex = regexp.MustCompile(`^[a-zA-Z0-9_./]+$`)
	safeDimsRegex  = regexp.MustCompile(`^[a-zA-Z0-9_.,| -]+$`)
	safeGroups     = map[string]bool{"average": true, "max": true, "min": true, "sum": true}
)

// data rebuilds the upstream query from validated params — never a
// forwarded raw path — so a caller can only reach /api/v1/data.
func (s *Server) data(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	q := r.URL.Query()
	chart := q.Get("chart")
	if !safeChartRegex.MatchString(chart) || strings.Contains(chart, "..") {
		http.Error(w, "Invalid chart", http.StatusBadRequest)
		return
	}
	out := url.Values{}
	out.Set("chart", chart)
	out.Set("format", "json")

	after := -600
	if v := q.Get("after"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n >= 0 || n < -31_536_000 {
			http.Error(w, "Invalid after", http.StatusBadRequest)
			return
		}
		after = n
	}
	out.Set("after", strconv.Itoa(after))

	if v := q.Get("points"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 2000 {
			http.Error(w, "Invalid points", http.StatusBadRequest)
			return
		}
		out.Set("points", strconv.Itoa(n))
	}
	if v := q.Get("group"); v != "" {
		if !safeGroups[v] {
			http.Error(w, "Invalid group", http.StatusBadRequest)
			return
		}
		out.Set("group", v)
	}
	if v := q.Get("dimensions"); v != "" {
		if !safeDimsRegex.MatchString(v) {
			http.Error(w, "Invalid dimensions", http.StatusBadRequest)
			return
		}
		out.Set("dimensions", v)
	}
	s.proxyNetdata(w, s.NetdataURL+"/api/v1/data?"+out.Encode())
}
