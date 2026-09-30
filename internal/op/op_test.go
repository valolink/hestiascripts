package op

import (
	"context"
	"strconv"
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
	for _, c := range cs {
		if c[0] == "vsftpd" {
			t.Fatalf("a proftpd box must not offer the vsftpd cleanup: %v", cs)
		}
	}
	if len(cs) == 0 || cs[0][0] != "clamav-daemon" {
		t.Fatalf("choices %v", cs)
	}
	p := text(planOf(t, "remove-service", f, Values{"service": "clamav-daemon"}))
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

func siteBox() (*sys.Fake, Target) {
	f := box8G()
	f.Files[hestia.UsersDir+"/renea/web.conf"] = "DOMAIN='renea.fi' IP='1.2.3.4' PROXY='default'\nDOMAIN='copy.renea.fi' IP='1.2.3.4'\n"
	f.Files["/home/renea/web/renea.fi/public_html/wp-config.php"] = "<?php\ndefine( 'WP_DEBUG', 1 );\ndefine('WP_AUTO_UPDATE_CORE', 'minor');\ndefine('WP_MEMORY_LIMIT', '128M');\n"
	d := hestia.Domain{User: "renea", Name: "renea.fi"}
	return f, Target{Domain: &d}
}

func sitePlan(t *testing.T, id string, f *sys.Fake, tg Target, v Values) ([]Step, error) {
	t.Helper()
	o, _ := ByID(id)
	env := &check.Env{Sys: f}
	vals := o.Defaults(context.Background(), env, tg)
	for k, x := range v {
		vals[k] = x
	}
	if err := o.Validate(context.Background(), env, tg, vals); err != nil {
		return nil, err
	}
	return o.Plan(context.Background(), env, tg, vals)
}

func TestWPConfigWritesOnlyChangesWithTheRightLiteral(t *testing.T) {
	f, tg := siteBox()
	st, err := sitePlan(t, "wp-config", f, tg, Values{"WP_DEBUG": "false", "WP_MEMORY_LIMIT": "", "DISALLOW_FILE_EDIT": "true", "WP_AUTO_UPDATE_CORE": "minor"})
	if err != nil {
		t.Fatal(err)
	}
	p := text(st)
	for _, want := range []string{
		"config set WP_DEBUG false --type=constant --raw",
		"config set DISALLOW_FILE_EDIT true --type=constant --raw",
		"config delete WP_MEMORY_LIMIT --type=constant",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in\n%s", want, p)
		}
	}
	if strings.Contains(p, "WP_AUTO_UPDATE_CORE") || len(st) != 3 {
		t.Errorf("unchanged defines must not be written:\n%s", p)
	}
	// A hand-written 1 stays a valid choice: opening and saving changes nothing.
	if st, err := sitePlan(t, "wp-config", f, tg, nil); err != nil || len(st) != 0 {
		t.Errorf("untouched form planned %v (%v)", st, err)
	}
}

func TestCloneOverwriteIsOptInAndTyped(t *testing.T) {
	f, tg := siteBox()
	if _, err := sitePlan(t, "clone", f, tg, Values{"new-domain": "copy.renea.fi"}); err == nil {
		t.Error("an existing domain was overwritten without asking")
	}
	st, err := sitePlan(t, "clone", f, tg, Values{"new-domain": "copy.renea.fi", "overwrite": "yes"})
	if err != nil || !strings.Contains(text(st), "--force") {
		t.Errorf("%v\n%s", err, text(st))
	}
	o, _ := ByID("clone")
	if o.RiskFor(Values{"overwrite": "yes"}) != Destructive || o.RiskFor(Values{"overwrite": "no"}) != Change {
		t.Error("overwrite must require the typed confirmation")
	}
}

// kuumalahde 2026-09-30 in miniature: a 1 GB /swapfile nearly full of parked
// pages on an 8 GB box with 3 GB available.
func swapBox(availKB string) *sys.Fake {
	f := box8G()
	f.Files["/proc/meminfo"] = "MemTotal:        8155000 kB\nMemAvailable:    " + availKB + " kB\nSwapTotal:       1048572 kB\nSwapFree:          60000 kB\n"
	f.Files["/proc/swaps"] = "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n/swapfile                               file\t\t1048572\t\t988572\t\t-2\n"
	f.Files["/swapfile"] = "x"
	return f
}

func TestSwapResizeKeepsTheOldFileAndNeverLeavesTheBoxWithoutRAM(t *testing.T) {
	p := text(planOf(t, "swap", swapBox("3300000"), Values{"size": "2G"}))
	for _, want := range []string{"fallocate -l 2G /swapfile.new", "swapon /swapfile.new", "swapoff /swapfile\n", "mv -n -v -- /swapfile \"$1/swapfile\"", "/root/hs-moved/", "mv -n -v -- /swapfile.new /swapfile", "swapon /swapfile\n"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in\n%s", want, p)
		}
	}
	if strings.Contains(p, "$ rm") {
		t.Errorf("the old swap file must be moved, not deleted:\n%s", p)
	}
	if i, j := strings.Index(p, "fallocate"), strings.Index(p, "swapoff /swapfile\n"); i > j {
		t.Errorf("the new file must exist before the old one is switched off:\n%s", p)
	}
	// The same size again: nothing to do.
	if st := planOf(t, "swap", swapBox("3300000"), Values{"size": "1G"}); len(st) != 0 {
		t.Errorf("same size planned %v", st)
	}
	// 965 MB in swap, 976 MB available: swapoff would leave nothing.
	o, _ := ByID("swap")
	if _, err := o.Plan(context.Background(), &check.Env{Sys: swapBox("1000000")}, Target{}, Values{"size": "2G"}); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("swapoff into a full box was allowed: %v", err)
	}
	// Swap on a partition is not ours to resize.
	f := swapBox("3300000")
	f.Files["/proc/swaps"] = "Filename\tType\tSize\tUsed\tPriority\n/dev/vda3 partition 2097148 0 -2\n"
	if _, err := o.Plan(context.Background(), &check.Env{Sys: f}, Target{}, Values{"size": "2G"}); err == nil {
		t.Error("a swap partition was accepted")
	}
	// No swap at all: create it, fstab line once.
	f = box8G()
	f.Files["/proc/meminfo"] = "MemTotal:        8155000 kB\nMemAvailable:    3300000 kB\nSwapTotal:             0 kB\nSwapFree:              0 kB\n"
	if p := text(planOf(t, "swap", f, Values{"size": "2G"})); !strings.Contains(p, "fallocate -l 2G /swapfile\n") || !strings.Contains(p, "mkswap /swapfile") || strings.Contains(p, "swapoff") {
		t.Errorf("create plan:\n%s", p)
	}
}

func TestVMSysctlWritesTheDropInAndAppliesLive(t *testing.T) {
	f := box8G()
	f.Files["/proc/sys/vm/swappiness"] = "60\n"
	f.Files["/proc/sys/vm/vfs_cache_pressure"] = "100\n"
	f.Files["/proc/sys/vm/overcommit_memory"] = "0\n"
	f.Files["/etc/sysctl.conf"] = "# vm.swappiness = 60\nnet.ipv4.ip_forward = 0\n"
	o, _ := ByID("vm-sysctl")
	d := o.Defaults(context.Background(), &check.Env{Sys: f}, Target{})
	if d["vm.swappiness"] != "10" || d["vm.vfs_cache_pressure"] != "50" || d["vm.overcommit_memory"] != "1" {
		t.Errorf("suggestions for a Debian-default box with Redis: %v", d)
	}
	p := text(planOf(t, "vm-sysctl", f, nil))
	for _, want := range []string{
		"# hs: kernel memory settings (hs op vm-sysctl)", "> /etc/sysctl.d/90-hs-memory.conf",
		"conf set --style ini /etc/sysctl.d/90-hs-memory.conf vm.swappiness 10 vm.vfs_cache_pressure 50 vm.overcommit_memory 1",
		"sysctl -p /etc/sysctl.d/90-hs-memory.conf",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in\n%s", want, p)
		}
	}
	if strings.Contains(p, "systemctl") || strings.Contains(p, "also set in") {
		t.Errorf("no restart, and a commented-out line elsewhere is not a competitor:\n%s", p)
	}
	// Another file setting the key is named — it wins at boot if it sorts later.
	f.Files["/etc/sysctl.d/99-cloud.conf"] = "vm.swappiness = 30\n"
	if p := text(planOf(t, "vm-sysctl", f, nil)); !strings.Contains(p, "also set in /etc/sysctl.d/99-cloud.conf (30)") {
		t.Errorf("competing file not named:\n%s", p)
	}
	// Already applied and written: nothing to do.
	f.Files["/proc/sys/vm/swappiness"], f.Files["/proc/sys/vm/vfs_cache_pressure"], f.Files["/proc/sys/vm/overcommit_memory"] = "10", "50", "1"
	f.Files["/etc/sysctl.d/90-hs-memory.conf"] = "vm.swappiness = 10\nvm.vfs_cache_pressure = 50\nvm.overcommit_memory = 1\n"
	if st := planOf(t, "vm-sysctl", f, Values{"vm.swappiness": "10", "vm.vfs_cache_pressure": "50", "vm.overcommit_memory": "1"}); len(st) != 0 {
		t.Errorf("nothing changed, planned %v", st)
	}
}

// Two domains on a 50-worker dynamic template, workers ~150 MB private on
// an 8 GB box: 100 allowed, about 40 fit.
func fpmBox() *sys.Fake {
	f := box8G()
	f.Files[hestia.UsersDir+"/u/web.conf"] = "DOMAIN='a.fi' BACKEND='production-PHP-8_2'\nDOMAIN='b.fi' BACKEND='production-PHP-8_2'\n"
	f.Files[hestia.FpmTpl+"/production-PHP-8_2.tpl"] = "[%domain%]\npm = dynamic\npm.max_children = 50\npm.start_servers = 10\npm.min_spare_servers = 10\npm.max_spare_servers = 20\npm.max_requests = 1000\n"
	for _, d := range []string{"a.fi", "b.fi"} {
		f.Files["/etc/php/8.2/fpm/pool.d/"+d+".conf"] = "[" + d + "]\npm = dynamic\npm.max_children = 50\n"
	}
	delete(f.Files, "/etc/php/8.2/fpm/pool.d/renea.fi.conf")
	for i, pid := range []string{"100", "101", "102"} {
		f.Files["/proc/"+pid+"/comm"] = "php-fpm8.2\n"
		// shared 100 MB + private 150 MB each: RSS 250 MB, PSS shares the 100 MB three ways
		f.Files["/proc/"+pid+"/smaps_rollup"] = "Rss:              256000 kB\nPss:              " + []string{"187733", "187733", "187734"}[i] + " kB\n"
	}
	return f
}

func TestFPMPoolSizeEditsTheTemplateThenRebuildsEachDomain(t *testing.T) {
	f := fpmBox()
	o, _ := ByID("fpm-pool-size")
	d := o.Defaults(context.Background(), &check.Env{Sys: f}, Target{})
	if d["template"] != "production-PHP-8_2" || d["mode"] != "dynamic" || d["min_spare"] != "10" {
		t.Errorf("defaults from the biggest template: %v", d)
	}
	if n, _ := strconv.Atoi(d["max_children"]); n >= 50 || n < 2 {
		t.Errorf("over budget, so a smaller size should be suggested: %v", d)
	}
	p := text(planOf(t, "fpm-pool-size", f, Values{"max_children": "20", "start": "", "min_spare": "", "max_spare": ""}))
	for _, want := range []string{
		"conf set --style ini --comment ';' --after pm.max_children " + hestia.FpmTpl + "/production-PHP-8_2.tpl pm dynamic pm.max_children 20 pm.start_servers 7 pm.min_spare_servers 4 pm.max_spare_servers 10",
		"conf unset --comment ';' " + hestia.FpmTpl + "/production-PHP-8_2.tpl pm.process_idle_timeout",
		"v-rebuild-web-domain u a.fi no", "v-rebuild-web-domain u b.fi no",
		"php-fpm8.2 -t", "systemctl try-reload-or-restart php8.2-fpm",
		"grep -H ^pm /etc/php/8.2/fpm/pool.d/a.fi.conf /etc/php/8.2/fpm/pool.d/b.fi.conf",
		"100 → 40", "÷ 150 MB private per worker",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in\n%s", want, p)
		}
	}
	if strings.Contains(p, "conf install") || strings.Contains(p, "systemctl restart") || strings.Contains(p, "pool.d/a.fi.conf pm") {
		t.Errorf("only the template is edited, reload is graceful:\n%s", p)
	}
	if i, j := strings.Index(p, "php-fpm8.2 -t"), strings.Index(p, "try-reload-or-restart"); i > j {
		t.Errorf("the config test comes before the reload:\n%s", p)
	}
	// Unchanged: nothing to do.
	if st := planOf(t, "fpm-pool-size", f, Values{"max_children": "50", "start": "10", "min_spare": "10", "max_spare": "20"}); len(st) != 0 {
		t.Errorf("same values planned %v", st)
	}
	// php-fpm's own rule: min ≤ start ≤ max ≤ children.
	if _, err := o.Plan(context.Background(), &check.Env{Sys: f}, Target{}, Values{"template": "production-PHP-8_2", "mode": "dynamic", "max_children": "20", "start": "5", "min_spare": "8", "max_spare": "30"}); err == nil {
		t.Error("spare servers above max_children accepted")
	}
	// static: the spare keys are commented out instead.
	if p := text(planOf(t, "fpm-pool-size", f, Values{"mode": "static", "max_children": "12"})); !strings.Contains(p, "pm static pm.max_children 12\n") || !strings.Contains(p, "pm.start_servers pm.min_spare_servers pm.max_spare_servers pm.process_idle_timeout") {
		t.Errorf("static plan:\n%s", p)
	}
}

func TestNetdataAlarmsInstallThenReloadWithoutRestart(t *testing.T) {
	f := box8G()
	f.Commands["netdata"], f.Commands["netdatacli"] = true, true
	f.Files["/repo/templates/netdata/health.d/swap.conf"] = "alarm: hs_swap_io\n"
	f.Files["/repo/templates/netdata/health.d/hs-pressure.conf"] = "alarm: hs_ram_pressure\n"
	p := text(planOf(t, "netdata-alarms", f, nil))
	for _, want := range []string{
		"install -d -m 755 /etc/netdata/health.d",
		"conf install /repo/templates/netdata/health.d/swap.conf /etc/netdata/health.d/swap.conf",
		"conf install /repo/templates/netdata/health.d/hs-pressure.conf /etc/netdata/health.d/hs-pressure.conf",
		"netdatacli reload-health", "used_swap",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in\n%s", want, p)
		}
	}
	if strings.Contains(p, "systemctl restart netdata") {
		t.Errorf("netdatacli is here, no restart needed:\n%s", p)
	}
	delete(f.Commands, "netdatacli")
	if p := text(planOf(t, "netdata-alarms", f, nil)); !strings.Contains(p, "systemctl restart netdata") {
		t.Errorf("without netdatacli a restart is the way:\n%s", p)
	}
}
