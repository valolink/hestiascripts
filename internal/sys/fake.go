package sys

import (
	"context"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// Fake is an in-memory box for tests. Files are keyed by absolute path;
// directories exist implicitly as prefixes of file paths (or via Dirs).
// Commands are keyed by "name arg1 arg2 ...".
type Fake struct {
	Files    map[string]string
	Dirs     []string
	Links    map[string]string // path → resolved target
	Cmds     map[string]FakeCmd
	Commands map[string]bool // Have()
	HTTP     map[string]FakeHTTP
	Clock    time.Time
	ModTimes map[string]time.Time
}

type FakeCmd struct {
	Out   string
	Code  int           // non-zero → *ExitError
	Err   error         // e.g. not found
	Delay time.Duration // honours ctx
}

type FakeHTTP struct {
	Status int
	Body   string
	Err    error
}

func (f *Fake) Run(ctx context.Context, name string, args ...string) (string, error) {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	c, ok := f.Cmds[key]
	if !ok {
		return "", &ExitError{Code: 127, Stderr: "fake: no such command: " + key}
	}
	if c.Delay > 0 {
		select {
		case <-time.After(c.Delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if c.Err != nil {
		return c.Out, c.Err
	}
	if c.Code != 0 {
		return c.Out, &ExitError{Code: c.Code}
	}
	return c.Out, nil
}

func (f *Fake) ReadFile(p string) ([]byte, error) {
	if v, ok := f.Files[p]; ok {
		return []byte(v), nil
	}
	return nil, fs.ErrNotExist
}

func (f *Fake) isDir(p string) bool {
	p = strings.TrimSuffix(p, "/")
	for _, d := range f.Dirs {
		if d == p || strings.HasPrefix(d, p+"/") {
			return true
		}
	}
	for k := range f.Files {
		if strings.HasPrefix(k, p+"/") {
			return true
		}
	}
	return false
}

func (f *Fake) ReadDir(p string) ([]fs.DirEntry, error) {
	p = strings.TrimSuffix(p, "/")
	seen := map[string]bool{}
	var out []fs.DirEntry
	add := func(full string) {
		rest := strings.TrimPrefix(full, p+"/")
		if rest == full || rest == "" {
			return
		}
		name, _, isDir := strings.Cut(rest, "/")
		if seen[name] {
			return
		}
		seen[name] = true
		out = append(out, fakeEntry{name: name, dir: isDir || f.isDir(p+"/"+name)})
	}
	for k := range f.Files {
		add(k)
	}
	for _, d := range f.Dirs {
		add(d)
	}
	if len(out) == 0 && !f.isDir(p) {
		return nil, fs.ErrNotExist
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

func (f *Fake) Glob(pattern string) ([]string, error) {
	var out []string
	cand := map[string]bool{}
	for k := range f.Files {
		cand[k] = true
		for d := path.Dir(k); d != "/" && d != "."; d = path.Dir(d) {
			cand[d] = true
		}
	}
	for _, d := range f.Dirs {
		cand[d] = true
	}
	for k := range f.Links {
		cand[k] = true
	}
	for k := range cand {
		if ok, _ := path.Match(pattern, k); ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *Fake) Stat(p string) (fs.FileInfo, error) {
	if t, ok := f.Links[p]; ok {
		p = t
	}
	if v, ok := f.Files[p]; ok {
		return fakeInfo{name: path.Base(p), size: int64(len(v)), mod: f.mod(p)}, nil
	}
	if f.isDir(p) {
		return fakeInfo{name: path.Base(p), dir: true, mod: f.mod(p)}, nil
	}
	return nil, fs.ErrNotExist
}

func (f *Fake) Lstat(p string) (fs.FileInfo, error) {
	if _, ok := f.Links[p]; ok {
		return fakeInfo{name: path.Base(p), link: true}, nil
	}
	return f.Stat(p)
}

func (f *Fake) EvalSymlinks(p string) (string, error) {
	if t, ok := f.Links[p]; ok {
		return t, nil
	}
	return p, nil
}

func (f *Fake) mod(p string) time.Time {
	if t, ok := f.ModTimes[p]; ok {
		return t
	}
	return f.Clock
}

func (f *Fake) Have(cmd string) bool { return f.Commands[cmd] }
func (f *Fake) Now() time.Time       { return f.Clock }

func (f *Fake) HTTPGet(ctx context.Context, url string, _ map[string]string) (int, []byte, error) {
	h, ok := f.HTTP[url]
	if !ok {
		return 0, nil, fs.ErrNotExist
	}
	return h.Status, []byte(h.Body), h.Err
}

type fakeEntry struct {
	name string
	dir  bool
}

func (e fakeEntry) Name() string               { return e.name }
func (e fakeEntry) IsDir() bool                { return e.dir }
func (e fakeEntry) Type() fs.FileMode          { return e.Info2().Mode().Type() }
func (e fakeEntry) Info() (fs.FileInfo, error) { return e.Info2(), nil }
func (e fakeEntry) Info2() fakeInfo            { return fakeInfo{name: e.name, dir: e.dir} }

type fakeInfo struct {
	name string
	size int64
	dir  bool
	link bool
	mod  time.Time
}

func (i fakeInfo) Name() string { return i.name }
func (i fakeInfo) Size() int64  { return i.size }
func (i fakeInfo) Mode() fs.FileMode {
	switch {
	case i.link:
		return fs.ModeSymlink | 0o777
	case i.dir:
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (i fakeInfo) ModTime() time.Time { return i.mod }
func (i fakeInfo) IsDir() bool        { return i.dir }
func (i fakeInfo) Sys() any           { return nil }
