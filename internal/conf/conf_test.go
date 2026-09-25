package conf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetReplacesKeepsFormAndCommentsDuplicates(t *testing.T) {
	in := "bind 127.0.0.1\n# maxmemory <bytes>\nmaxmemory 128mb\nmaxmemory 64mb\n"
	out, ch := Set(in, Opts{Style: Space}, "maxmemory", "512mb")
	want := "bind 127.0.0.1\n# maxmemory <bytes>\nmaxmemory 512mb\n# maxmemory 64mb   # superseded by hs (duplicate)\n"
	if out != want {
		t.Fatalf("got\n%s", out)
	}
	if ch[0].Old != "maxmemory 128mb" || ch[0].New != "maxmemory 512mb" {
		t.Errorf("change %+v", ch[0])
	}
}

func TestSetInsertsAfterCommentedExample(t *testing.T) {
	in := "[opcache]\n;opcache.enable=1\n;opcache.memory_consumption=128\n\n[curl]\n"
	out, _ := Set(in, Opts{Style: Eq}, "opcache.memory_consumption", "256")
	if !strings.Contains(out, ";opcache.memory_consumption=128\nopcache.memory_consumption=256\n") {
		t.Fatalf("got\n%s", out)
	}
}

func TestSetAppendsInsideTheSection(t *testing.T) {
	in := "[client]\nport = 3306\n\n[mysqld]\nuser = mysql\n\n[mysqldump]\nquick\n"
	out, _ := Set(in, Opts{Section: "mysqld", Style: INI}, "innodb_buffer_pool_size", "2G")
	if !strings.Contains(out, "[mysqld]\nuser = mysql\ninnodb_buffer_pool_size = 2G\n\n[mysqldump]") {
		t.Fatalf("got\n%s", out)
	}
	// A key of the same name in another section is not touched.
	out, _ = Set("[a]\nx = 1\n[b]\nx = 2\n", Opts{Section: "b", Style: INI}, "x", "3")
	if out != "[a]\nx = 1\n[b]\nx = 3\n" {
		t.Fatalf("got\n%s", out)
	}
}

func TestSetCreatesMissingSectionAndKeepsEqualsForm(t *testing.T) {
	out, _ := Set("[DEFAULT]\nbantime=600\n", Opts{Section: "wordpress", Style: INI}, "maxretry", "10")
	if out != "[DEFAULT]\nbantime=600\n\n[wordpress]\nmaxretry = 10\n" {
		t.Fatalf("got\n%s", out)
	}
	out, _ = Set("[DEFAULT]\nbantime=600\n", Opts{Section: "DEFAULT", Style: INI}, "bantime", "3600")
	if out != "[DEFAULT]\nbantime=3600\n" {
		t.Fatalf("got\n%s", out)
	}
	out, _ = Set(`email_addr="you@domain.com"`+"\n", Opts{Style: Shell}, "email_addr", "ops@valolink.fi")
	if out != `email_addr="ops@valolink.fi"`+"\n" {
		t.Fatalf("got\n%s", out)
	}
}

func TestGet(t *testing.T) {
	v, ok := Get("[mysqld]\ninnodb_buffer_pool_size = 2G\n", Opts{Section: "mysqld"}, "innodb_buffer_pool_size")
	if !ok || v != "2G" {
		t.Fatalf("%q %v", v, ok)
	}
	if _, ok := Get("# maxmemory 1gb\n", Opts{}, "maxmemory"); ok {
		t.Error("a commented line is not a value")
	}
}

func TestSetFileBacksUpOutsideTheDirectoryAndSkipsNoops(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HS_STATE_DIR", filepath.Join(dir, "state"))
	p := filepath.Join(dir, "jail.d", "wordpress.conf")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("[wordpress]\nmaxretry = 10\n"), 0o640)

	_, bak, err := SetFile(p, Opts{Section: "wordpress", Style: INI}, "maxretry", "10")
	if err != nil || bak != "" {
		t.Fatalf("no-op wrote: %q %v", bak, err)
	}
	_, bak, err = SetFile(p, Opts{Section: "wordpress", Style: INI}, "maxretry", "5")
	if err != nil || bak == "" || strings.HasPrefix(bak, filepath.Dir(p)+"/") {
		t.Fatalf("backup %q %v", bak, err)
	}
	if b, _ := os.ReadFile(bak); string(b) != "[wordpress]\nmaxretry = 10\n" {
		t.Errorf("backup holds %q", b)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", st.Mode())
	}
	if ents, _ := os.ReadDir(filepath.Dir(p)); len(ents) != 1 {
		t.Errorf("stray files next to the config: %v", ents)
	}
}

func TestPoolTemplateKeysStayTogether(t *testing.T) {
	in := "[%domain%]\npm = ondemand\npm.max_children = 8\npm.max_requests = 4000\npm.process_idle_timeout = 10s\n\nenv[TMP] = /tmp\n"
	out, _ := Set(in, Opts{Style: INI, After: "pm.max_children"}, "pm", "dynamic", "pm.start_servers", "4")
	out, _ = Unset(out, Opts{Comment: ";"}, "pm.process_idle_timeout")
	want := "[%domain%]\npm = dynamic\npm.max_children = 8\npm.start_servers = 4\npm.max_requests = 4000\n;pm.process_idle_timeout = 10s\n\nenv[TMP] = /tmp\n"
	if out != want {
		t.Fatalf("got\n%s", out)
	}
}
