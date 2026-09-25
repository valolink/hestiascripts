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

func TestRemoveServiceClearsOnlyItsOwnValue(t *testing.T) {
	f := box8G()
	f.Files[hestia.ConfPath] = "FTP_SYSTEM='proftpd'\nANTIVIRUS_SYSTEM='clamav-daemon'\n"
	f.Files["/etc/exim4/exim4.conf.template"] = "av_scanner = clamd:/run/clamav/clamd.ctl\n"
	o, _ := ByID("remove-service")
	cs := o.Fields[0].Choices(context.Background(), &check.Env{Sys: f}, Target{})
	if len(cs) != 1 || cs[0][0] != "ANTIVIRUS_SYSTEM" {
		t.Fatalf("a proftpd box must not offer the vsftpd cleanup: %v", cs)
	}
	p := text(planOf(t, "remove-service", f, Values{"service": "ANTIVIRUS_SYSTEM"}))
	for _, want := range []string{"v-change-sys-config-value ANTIVIRUS_SYSTEM ''", "exim4 -bV", "sed -i.hs-clamav-daemon"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in\n%s", want, p)
		}
	}
	if strings.Contains(p, "apt-get remove") {
		t.Errorf("the package is already gone:\n%s", p)
	}
}

func TestUpgradePlanCarriesTheClassifiedList(t *testing.T) {
	f := box8G()
	f.Cmds["apt-get -s upgrade"] = sys.FakeCmd{Out: "Inst mariadb-server [1:10.11.11-0+deb12u1] (1:10.11.13-0+deb12u1 Debian:12.11/stable [amd64])\n"}
	p := text(planOf(t, "updates-apply", f, nil))
	if !strings.Contains(p, "mariadb-server") || !strings.Contains(p, "restarts the database") || !strings.Contains(p, "--force-confold") {
		t.Errorf("plan:\n%s", p)
	}
	f.Cmds["apt-get -s upgrade"] = sys.FakeCmd{Out: "0 upgraded\n"}
	if st := planOf(t, "updates-apply", f, nil); len(st) != 0 {
		t.Errorf("nothing pending, planned %v", st)
	}
}

func TestLogTrimWritesInPlace(t *testing.T) {
	f := box8G()
	log := "/home/u/web/a.fi/public_html/wp-content/debug.log"
	f.Files[log] = "x\n"
	f.Cmds["tail -n 3 "+log] = sys.FakeCmd{Out: "PHP Warning\n"}
	for how, want := range map[string]string{"truncate": "truncate -s 0 " + log, "keep500": `cat "$t" > "$1"`, "cap": "logcap add a.fi_debug.log " + log + " u 50M"} {
		o, _ := ByID("log-trim") // validation limits file to the listed logs; plan directly
		st, err := o.Plan(context.Background(), &check.Env{Sys: f}, Target{}, Values{"file": log, "how": how})
		if err != nil {
			t.Fatal(err)
		}
		p := text(st)
		if !strings.Contains(p, want) || strings.Contains(p, "rm -f "+log) {
			t.Errorf("%s:\n%s", how, p)
		}
	}
}

func sshBox(keys, journal string) *sys.Fake {
	f := box8G()
	f.Files["/root/.ssh/authorized_keys"] = keys
	f.Files["/etc/ssh/sshd_config"] = "Include /etc/ssh/sshd_config.d/*.conf\n"
	f.Cmds["journalctl --since=-30d --no-pager -o cat -u ssh -u sshd --grep Accepted (publickey|password) for root"] = sys.FakeCmd{Out: journal}
	return f
}

func TestSSHKeysOnlyRefusesWhatWouldLockRootOut(t *testing.T) {
	o, _ := ByID("ssh-keys-only")
	plan := func(f *sys.Fake) error {
		_, err := o.Plan(context.Background(), &check.Env{Sys: f}, Target{}, Values{})
		return err
	}
	if err := plan(sshBox("", "Accepted publickey for root from 1.2.3.4 port 5 ssh2")); err == nil || !strings.Contains(err.Error(), "no key") {
		t.Errorf("no authorized key: %v", err)
	}
	if err := plan(sshBox("ssh-ed25519 AAAA reima", "Accepted password for root from 1.2.3.4 port 5 ssh2")); err == nil {
		t.Error("no key login seen, yet allowed")
	}
	t.Setenv("SSH_CONNECTION", "1.2.3.4 5555 10.0.0.1 22")
	if err := plan(sshBox("ssh-ed25519 AAAA reima", "Accepted publickey for root from 9.9.9.9 port 1 ssh2\nAccepted password for root from 1.2.3.4 port 5555 ssh2")); err == nil {
		t.Error("this session used a password, yet allowed")
	}
	st := planOf(t, "ssh-keys-only", sshBox("ssh-ed25519 AAAA reima", "Accepted publickey for root from 1.2.3.4 port 5555 ssh2"), nil)
	p := text(st)
	if !strings.Contains(p, "sshd -t") || strings.Index(p, "sshd -t") > strings.Index(p, "systemctl reload ssh") {
		t.Errorf("config must be tested before reload:\n%s", p)
	}
}

func TestResendKeyNeverInThePlan(t *testing.T) {
	f := box8G()
	f.Commands["postconf"] = true
	f.Files["/etc/postfix/main.cf"] = "relayhost =\n"
	f.Cmds["dpkg-query -W -f=${Status} libsasl2-modules"] = sys.FakeCmd{Out: "install ok installed"}
	o, _ := ByID("smtp-relay")
	v := Values{"api-key": "re_TOPSECRET", "sender": "noreply@valolink.fi"}
	if err := o.Validate(context.Background(), &check.Env{Sys: f}, Target{}, v); err != nil {
		t.Fatal(err)
	}
	st, err := o.Plan(context.Background(), &check.Env{Sys: f}, Target{}, v)
	if err != nil {
		t.Fatal(err)
	}
	if p := text(st); strings.Contains(p, "TOPSECRET") || !strings.Contains(p, "$HS_SECRET_API_KEY") {
		t.Errorf("plan:\n%s", p)
	}
	if strings.Contains(strings.Join(v.Args(o), " "), "TOPSECRET") {
		t.Error("key in argv")
	}
}

func TestPostfixRefusedWhereEximRuns(t *testing.T) {
	f := box8G()
	f.Cmds["dpkg-query -W -f=${Status} exim4-daemon-heavy"] = sys.FakeCmd{Out: "install ok installed"}
	o, _ := ByID("smtp-install")
	if _, err := o.Plan(context.Background(), &check.Env{Sys: f}, Target{}, nil); err == nil {
		t.Error("installing postfix would remove exim4")
	}
}
