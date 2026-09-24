// Package sys is the only place hs touches the machine. Checks go through the
// Sys interface so they can be tested against a fake box, and so every command
// they run is bounded by the caller's context.
package sys

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

type Sys interface {
	// Run executes name with args and returns stdout. A non-zero exit is an
	// error of type *ExitError carrying the code; stdout is still returned.
	Run(ctx context.Context, name string, args ...string) (string, error)
	ReadFile(path string) ([]byte, error)
	ReadDir(path string) ([]fs.DirEntry, error)
	Glob(pattern string) ([]string, error)
	Stat(path string) (fs.FileInfo, error)
	Lstat(path string) (fs.FileInfo, error)
	EvalSymlinks(path string) (string, error)
	Have(cmd string) bool
	Now() time.Time
	// HTTPGet fetches url with optional headers. Bounded by ctx.
	HTTPGet(ctx context.Context, url string, headers map[string]string) (status int, body []byte, err error)
	// HTTPLocal requests url from ip on this box with the URL's host as
	// Host/SNI, no redirects followed, any certificate accepted — "does the
	// site answer here", independent of DNS and CDN. Hestia's nginx listens
	// on the domain's configured IP, not on 127.0.0.1.
	HTTPLocal(ctx context.Context, url, ip string) (status int, location string, err error)
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// ExitError is a command that ran and exited non-zero.
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string { return "exit status " + strconv.Itoa(e.Code) }

// ExitCode returns the command's exit code: 0 on success, the code for an
// *ExitError, -1 for anything else (not found, killed by timeout).
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return -1
}

type Real struct{}

func (Real) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	// Nothing hs runs should ever wait on a terminal (git, ssh, apt prompts).
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "DEBIAN_FRONTEND=noninteractive", "LC_ALL=C")
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return out.String(), ctx.Err()
	}
	var xe *exec.ExitError
	if errors.As(err, &xe) {
		return out.String(), &ExitError{Code: xe.ExitCode(), Stderr: errb.String()}
	}
	return out.String(), err
}

func (Real) ReadFile(path string) ([]byte, error)       { return os.ReadFile(path) }
func (Real) ReadDir(path string) ([]fs.DirEntry, error) { return os.ReadDir(path) }
func (Real) Glob(pattern string) ([]string, error)      { return filepath.Glob(pattern) }
func (Real) Stat(path string) (fs.FileInfo, error)      { return os.Stat(path) }
func (Real) Lstat(path string) (fs.FileInfo, error)     { return os.Lstat(path) }
func (Real) EvalSymlinks(path string) (string, error)   { return filepath.EvalSymlinks(path) }
func (Real) Now() time.Time                             { return time.Now() }

func (Real) Have(cmd string) bool {
	_, err := exec.LookPath(cmd)
	if err == nil {
		return true
	}
	// Hestia's own binaries are not on a non-login PATH.
	_, err = os.Stat(filepath.Join("/usr/local/hestia/bin", cmd))
	return err == nil
}

func (Real) HTTPGet(ctx context.Context, url string, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", "hs (hestiascripts)")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, body, err
}

func (Real) HTTPLocal(ctx context.Context, url, ip string) (int, string, error) {
	if ip == "" {
		ip = "127.0.0.1"
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, _ := net.SplitHostPort(addr)
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
		},
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, // cert validity is its own check
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("User-Agent", "hs (hestiascripts) site probe")
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header.Get("Location"), nil
}

func (Real) LookupHost(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}
