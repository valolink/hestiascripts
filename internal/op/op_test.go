package op

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/plan"
	"github.com/valolink/hestiascripts/internal/sys"
)

func box8G() *sys.Fake {
	return &sys.Fake{Clock: time.Now(), Files: map[string]string{
		"/proc/meminfo":                           "MemTotal:        8155000 kB\n",
		"/etc/php/8.2/fpm/php.ini":                "[opcache]\n;opcache.enable=1\nopcache.memory_consumption=128\n;opcache.max_accelerated_files=10000\n",
		"/etc/php/8.2/fpm/pool.d/renea.fi.conf":   "[renea.fi]\n",
		"/etc/php/8.2/cli/php.ini":                "disable_functions = pcntl_alarm,exec,proc_open\n",
		"/etc/php/8.3/fpm/php.ini":                "[opcache]\nopcache.enable=1\nopcache.memory_consumption=512\nopcache.max_accelerated_files=100000\nopcache.validate_timestamps=1\n",
		"/etc/php/8.3/cli/php.ini":                "disable_functions = pcntl_alarm\n",
		"/etc/mysql/mariadb.conf.d/50-server.cnf": "[mysqld]\nuser = mysql\n",
		hestia.FpmTpl + "/PHP-8_2.tpl":            "[%domain%]\npm = ondemand\npm.max_children = 8\n",
		"/repo/templates/php-fpm/standard.conf":   "PROFILE_DESC=\"Standard\"\nPM_MODE=\"dynamic\"\nPM_MAX_CHILDREN=20\nPM_MAX_REQUESTS=500\nPM_START_SERVERS=4\nPM_MIN_SPARE=2\nPM_MAX_SPARE=6\n",
	}, Dirs: []string{"/etc/php/8.2", "/etc/php/8.3"},
		Commands: map[string]bool{"redis-server": true, "redis-cli": true, "mariadb": true, "wp": true},
		Cmds: map[string]sys.FakeCmd{
			"systemctl is-active --quiet redis-server":          {},
			"redis-cli config get maxmemory":                    {Out: "maxmemory\n268435456\n"},
			"redis-cli config get maxmemory-policy":             {Out: "maxmemory-policy\nallkeys-lru\n"},
			"php8.2 -r echo extension_loaded('redis') ? 1 : 0;": {Out: "0"},
			"php8.3 -r echo extension_loaded('redis') ? 1 : 0;": {Out: "1"},
		}}
}

func planOf(t *testing.T, id string, f *sys.Fake, v Values) []Step {
	t.Helper()
	o, ok := ByID(id)
	if !ok {
		t.Fatalf("no op %s", id)
	}
	env := &check.Env{Sys: f, RepoDir: "/repo"}
	ctx := context.Background()
	vals := o.Defaults(ctx, env, Target{})
	for k, x := range v {
		vals[k] = x
	}
	if err := o.Validate(ctx, env, Target{}, vals); err != nil {
		t.Fatalf("%s: %v (values %v)", id, err, vals)
	}
	st, err := o.Plan(ctx, env, Target{}, vals)
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	return st
}

func text(st []Step) string { return strings.Join(plan.Preview(st), "\n") }

func TestEveryOperationExplainsItself(t *testing.T) {
	for _, o := range All() {
		if o.Title == "" || o.Section == "" || o.How == "" || o.Plan == nil {
			t.Errorf("%s: title, section, how and plan are required", o.ID)
		}
		if o.Risk != ReadOnly && o.Undo == "" {
			t.Errorf("%s changes the box but says nothing about undoing it", o.ID)
		}
		seen := map[string]bool{}
		for _, f := range o.Fields {
			if seen[f.Key] {
				t.Errorf("%s: duplicate field %s", o.ID, f.Key)
			}
			seen[f.Key] = true
		}
	}
}

func TestRedisCapIsLiveAndNeverRestarts(t *testing.T) {
	p := text(planOf(t, "redis-memory", box8G(), Values{"maxmemory": "1216", "policy": "allkeys-lru"}))
	if strings.Contains(p, "systemctl restart") {
		t.Errorf("restarting Redis empties every site's cache:\n%s", p)
	}
	for _, want := range []string{"redis-cli config set maxmemory 1216mb", "conf set --style space /etc/redis/redis.conf maxmemory 1216mb maxmemory-policy allkeys-lru"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in\n%s", want, p)
		}
	}
	// Unchanged values: nothing to do.
	if st := planOf(t, "redis-memory", box8G(), Values{"maxmemory": "256", "policy": "allkeys-lru"}); len(st) != 0 {
		t.Errorf("no-op planned %v", st)
	}
}

func TestRedisCapRefusesMoreThanHalfTheRAM(t *testing.T) {
	o, _ := ByID("redis-memory")
	_, err := o.Plan(context.Background(), &check.Env{Sys: box8G()}, Target{}, Values{"maxmemory": "6000", "policy": "allkeys-lru"})
	if err == nil {
		t.Error("6000 MB of 8 GB accepted")
	}
}

func TestOpcacheSkipsAGoodVersionAndNeverLowersFiles(t *testing.T) {
	p := text(planOf(t, "opcache", box8G(), Values{"version": "all", "memory": "512"}))
	if !strings.Contains(p, "/etc/php/8.2/fpm/php.ini opcache.enable 1 opcache.memory_consumption 512 opcache.validate_timestamps 1 opcache.max_accelerated_files 50000") {
		t.Errorf("8.2 not configured:\n%s", p)
	}
	if strings.Contains(p, "8.3") {
		t.Errorf("8.3 is already right (and has 100000 files):\n%s", p)
	}
	if !strings.Contains(p, "try-reload-or-restart php8.2-fpm") {
		t.Errorf("no graceful reload:\n%s", p)
	}
}

func TestMariaDBResizesOnline(t *testing.T) {
	p := text(planOf(t, "mariadb-buffer", box8G(), Values{"size": "3G"}))
	if strings.Contains(p, "systemctl restart") || !strings.Contains(p, "SET GLOBAL innodb_buffer_pool_size = 3221225472") ||
		!strings.Contains(p, "--section mysqld --style ini /etc/mysql/mariadb.conf.d/50-server.cnf innodb_buffer_pool_size 3G") {
		t.Errorf("plan:\n%s", p)
	}
	o, _ := ByID("mariadb-buffer")
	if _, err := o.Plan(context.Background(), &check.Env{Sys: box8G()}, Target{}, Values{"size": "7G"}); err == nil {
		t.Error("7G of 8G accepted")
	}
}

func TestFPMProfileCopiesBaseAndSetsModeKeys(t *testing.T) {
	p := text(planOf(t, "fpm-profile", box8G(), Values{"version": "8.2", "profile": "standard"}))
	for _, want := range []string{
		"conf install " + hestia.FpmTpl + "/PHP-8_2.tpl " + hestia.FpmTpl + "/standard-PHP-8_2.tpl",
		"pm dynamic pm.max_children 20 pm.max_requests 500 pm.start_servers 4 pm.min_spare_servers 2 pm.max_spare_servers 6",
		"conf unset --comment ';' " + hestia.FpmTpl + "/standard-PHP-8_2.tpl pm.process_idle_timeout",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in\n%s", want, p)
		}
	}
}

func TestPHPExtOnlyWhereMissing(t *testing.T) {
	p := text(planOf(t, "redis-php-ext", box8G(), Values{"version": "all"}))
	if !strings.Contains(p, "php8.2-redis") || strings.Contains(p, "php8.3-redis") {
		t.Errorf("plan:\n%s", p)
	}
}

func TestCLIFunctionsTouchOnlyTheBlockingCLI(t *testing.T) {
	p := text(planOf(t, "php-cli-functions", box8G(), nil))
	if !strings.Contains(p, "/etc/php/8.2/cli/php.ini disable_functions") || strings.Contains(p, "8.3") || strings.Contains(p, "fpm/php.ini") {
		t.Errorf("plan:\n%s", p)
	}
}

func TestSecretsStayOutOfArgv(t *testing.T) {
	o := Op{ID: "t", Fields: []Field{{Key: "user"}, {Key: "api-key", Kind: Secret}}}
	v := Values{"user": "resend", "api-key": "re_123"}
	if a := strings.Join(v.Args(o), " "); strings.Contains(a, "re_123") {
		t.Errorf("secret in argv: %s", a)
	}
	if e := v.Env(o); len(e) != 1 || e[0] != "HS_SECRET_API_KEY=re_123" {
		t.Errorf("env %v", e)
	}
}
