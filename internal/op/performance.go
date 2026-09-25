package op

// Performance: what run.sh's Redis (3), PHP-FPM (9), OpCache (10) and
// MariaDB (11) menus did — with the running value beside each field, and
// without restarts where the service can take the change live.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/box"
	"github.com/valolink/hestiascripts/internal/conf"
	"github.com/valolink/hestiascripts/internal/hestia"
)

const redisConf = "/etc/redis/redis.conf"

func redisSuggestMB(env *check.Env) int {
	n := box.RAMMB(env.Sys) * 15 / 100 / 64 * 64
	if n < 256 {
		n = 256
	}
	return n
}

func redisMaxField() Field {
	return Field{Key: "maxmemory", Label: "Memory cap (MB)", Kind: Number, Min: 64, Max: 65536,
		Help: "Redis evicts least-recently-used keys at this size instead of growing until the OOM killer picks something. ~15 % of RAM suits a box of shops; 256 MB is plenty for one site.",
		Current: func(ctx context.Context, env *check.Env, _ Target) string {
			st := box.Redis(ctx, env)
			if !st.Installed {
				return "not installed"
			}
			ram := fmt.Sprintf(" · RAM %s", mb(box.RAMMB(env.Sys)))
			if st.MaxMemory == 0 {
				return "unlimited" + ram
			}
			return mb(int(st.MaxMemory/1024/1024)) + ", " + st.Policy + ram
		},
		Default: func(_ context.Context, env *check.Env, _ Target) string {
			return strconv.Itoa(redisSuggestMB(env))
		}}
}

func redisPolicyField() Field {
	return Field{Key: "policy", Label: "When full", Kind: Choice,
		Help: "allkeys-lru is right for an object cache: every key is a cache entry, the least recently used goes first.",
		Choices: func(context.Context, *check.Env, Target) [][2]string {
			return [][2]string{{"allkeys-lru", "evict least recently used (object cache)"},
				{"allkeys-lfu", "evict least frequently used"},
				{"volatile-lru", "evict only keys with a TTL"}}
		}}
}

// redisLiveSteps set the cap on the running Redis and in redis.conf — no
// restart, so no site loses its cache (run.sh restarted and emptied it).
func redisLiveSteps(maxMB int, policy string) []Step {
	m := strconv.Itoa(maxMB) + "mb"
	return []Step{
		{Why: "apply to the running Redis (no restart, cache kept)", Argv: []string{"redis-cli", "config", "set", "maxmemory", m}},
		{Argv: []string{"redis-cli", "config", "set", "maxmemory-policy", policy}},
		confSet("keep it across restarts", redisConf, conf.Opts{Style: conf.Space}, "maxmemory", m, "maxmemory-policy", policy),
		{Why: "running values", Argv: []string{"redis-cli", "config", "get", "maxmemory*"}},
	}
}

func phpRedisMissing(ctx context.Context, env *check.Env) []string {
	var out []string
	for _, v := range fpmVersions(env) {
		if servesPools(env, v) && !box.PHPHasRedis(ctx, env, v) {
			out = append(out, v)
		}
	}
	return out
}

func phpExtSteps(vers []string) []Step {
	var steps []Step
	for _, v := range vers {
		steps = append(steps,
			aptInstall("PHP "+v+" redis extension", "php"+v+"-redis"),
			Step{Why: "PHP " + v + " workers load it (graceful reload; running requests finish)", Argv: []string{"systemctl", "try-reload-or-restart", "php" + v + "-fpm"}},
			Step{Why: "check", Argv: []string{"php" + v, "-r", "echo 'redis extension: ', extension_loaded('redis') ? 'loaded' : 'MISSING', PHP_EOL;"}},
		)
	}
	return steps
}

const fpmCLIDisabled = "pcntl_alarm,pcntl_fork,pcntl_waitpid,pcntl_wait,pcntl_wifexited,pcntl_wifstopped,pcntl_wifsignaled,pcntl_wifcontinued,pcntl_wexitstatus,pcntl_wtermsig,pcntl_wstopsig,pcntl_signal,pcntl_signal_dispatch,pcntl_get_last_error,pcntl_strerror,pcntl_sigprocmask,pcntl_sigwaitinfo,pcntl_sigtimedwait,pcntl_exec,pcntl_getpriority,pcntl_setpriority"

// --- OpCache ---------------------------------------------------------------

var iniInt = func(ini, key string) int {
	m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `\s*=\s*(\d+)`).FindStringSubmatch(ini)
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func versionField(label, help string, all bool, pick func(ctx context.Context, env *check.Env) string, label2 func(ctx context.Context, env *check.Env, v string) string) Field {
	return Field{Key: "version", Label: label, Kind: Choice, Help: help,
		Choices: func(ctx context.Context, env *check.Env, _ Target) [][2]string {
			var out [][2]string
			for _, v := range fpmVersions(env) {
				out = append(out, [2]string{v, label2(ctx, env, v)})
			}
			if all && len(out) > 1 {
				out = append(out, [2]string{"all", "every version above"})
			}
			return out
		},
		Default: func(ctx context.Context, env *check.Env, _ Target) string { return pick(ctx, env) }}
}

func pickVersions(env *check.Env, v string) []string {
	if v == "all" {
		return fpmVersions(env)
	}
	return []string{v}
}

// --- MariaDB ---------------------------------------------------------------

const mariadbCnf = "/etc/mysql/mariadb.conf.d/50-server.cnf"

var sizeRe = regexp.MustCompile(`^[0-9]+[MG]$`)

func sizeBytes(s string) int64 {
	n, _ := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if strings.HasSuffix(s, "G") {
		return n << 30
	}
	return n << 20
}

func dbClient(env *check.Env) string {
	if have(env, "mariadb") {
		return "mariadb"
	}
	return "mysql"
}

// --- PHP-FPM profiles ------------------------------------------------------

var fpmProfileNames = []string{"production", "standard", "staging", "small"}

type fpmProfile struct {
	Name, Desc, Mode                       string
	MaxChildren, MaxRequests               string
	Start, MinSpare, MaxSpare, IdleTimeout string
}

func loadProfile(env *check.Env, name string) (fpmProfile, error) {
	p := fpmProfile{Name: name}
	b, err := env.Sys.ReadFile(env.RepoDir + "/templates/php-fpm/" + name + ".conf")
	if err != nil {
		return p, fmt.Errorf("profile %s: %v (is the hestiascripts checkout found?)", name, err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		switch k {
		case "PROFILE_DESC":
			p.Desc = v
		case "PM_MODE":
			p.Mode = v
		case "PM_MAX_CHILDREN":
			p.MaxChildren = v
		case "PM_MAX_REQUESTS":
			p.MaxRequests = v
		case "PM_START_SERVERS":
			p.Start = v
		case "PM_MIN_SPARE":
			p.MinSpare = v
		case "PM_MAX_SPARE":
			p.MaxSpare = v
		case "PM_IDLE_TIMEOUT":
			p.IdleTimeout = v
		}
	}
	if p.Mode == "" || p.MaxChildren == "" {
		return p, fmt.Errorf("profile %s: PM_MODE / PM_MAX_CHILDREN missing", name)
	}
	return p, nil
}

func (p fpmProfile) summary() string {
	s := fmt.Sprintf("pm %s, max_children %s, max_requests %s", p.Mode, p.MaxChildren, p.MaxRequests)
	switch p.Mode {
	case "dynamic":
		s += fmt.Sprintf(", start %s, spare %s–%s", p.Start, p.MinSpare, p.MaxSpare)
	case "ondemand":
		s += ", idle timeout " + p.IdleTimeout
	}
	return s
}

// profileSteps: copy Hestia's base pool template for the version and set the
// profile's pm keys, commenting out the ones its mode does not use.
func profileSteps(p fpmProfile, ver string) []Step {
	tag := strings.ReplaceAll(ver, ".", "_")
	src := hestia.FpmTpl + "/PHP-" + tag + ".tpl"
	dst := hestia.FpmTpl + "/" + p.Name + "-PHP-" + tag + ".tpl"
	o := conf.Opts{Style: conf.INI, Comment: ";", After: "pm.max_children"}
	kv := []string{"pm", p.Mode, "pm.max_children", p.MaxChildren}
	if p.MaxRequests != "" {
		kv = append(kv, "pm.max_requests", p.MaxRequests)
	}
	var off []string
	switch p.Mode {
	case "dynamic":
		kv = append(kv, "pm.start_servers", p.Start, "pm.min_spare_servers", p.MinSpare, "pm.max_spare_servers", p.MaxSpare)
		off = []string{"pm.process_idle_timeout"}
	case "ondemand":
		kv = append(kv, "pm.process_idle_timeout", p.IdleTimeout)
		off = []string{"pm.start_servers", "pm.min_spare_servers", "pm.max_spare_servers"}
	default:
		off = []string{"pm.start_servers", "pm.min_spare_servers", "pm.max_spare_servers", "pm.process_idle_timeout"}
	}
	return []Step{
		confInstall(p.Name+" for PHP "+ver+": start from Hestia's base pool template", src, dst),
		confSet("the profile's worker settings", dst, o, kv...),
		confUnset("keys pm = "+p.Mode+" does not use", dst, conf.Opts{Comment: ";"}, off...),
	}
}

func multiPHP(env *check.Env) []string {
	m := regexp.MustCompile(`multiphp_v=\(([^)]*)\)`).FindStringSubmatch(readFile(env, hestia.Root+"/install/upgrade/upgrade.conf"))
	if m == nil {
		return []string{"7.4", "8.0", "8.1", "8.2", "8.3", "8.4"}
	}
	var out []string
	for _, v := range strings.Fields(strings.ReplaceAll(m[1], `"`, " ")) {
		if n, _ := strconv.ParseFloat(v, 64); n >= 7.4 {
			out = append(out, v)
		}
	}
	return out
}

func init() {
	register(Op{
		ID: "redis-install", Resolves: "redis", Applies: summaryHas("not installed"), Title: "Install Redis", Section: "performance", Risk: Change,
		Note: "Installs redis-server with a memory cap, the least-recently-used eviction an object cache needs, background flushes, and the PHP extension for every version that serves sites.",
		How:  "apt installs redis-server; `systemctl enable --now` starts it at boot. The cap, eviction policy and lazyfree-lazy-user-flush (the plugin FAQ's recommendation: a site's `wp cache flush` never stalls the others) are set live with `redis-cli config set` and written to /etc/redis/redis.conf by `hs conf set`, which prints each changed line and keeps the previous file. Sites use it once their object-cache drop-in is enabled (per site: Install Redis object cache).",
		Undo: "systemctl disable --now redis-server; apt purge redis-server. Sites with the drop-in fall back to the database.",
		Fields: []Field{redisMaxField(), redisPolicyField(),
			{Key: "php-ext", Label: "PHP redis extension", Kind: Bool, Help: "Install php<ver>-redis for every PHP version that serves pools and lacks it.",
				Default: func(context.Context, *check.Env, Target) string { return "yes" }}},
		Recheck: []string{"redis", "redis.sites"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			var steps []Step
			if !box.Redis(ctx, env).Installed {
				steps = append(steps, aptUpdate(), aptInstall("the server", "redis-server"),
					Step{Why: "start now and at boot", Argv: []string{"systemctl", "enable", "--now", "redis-server"}},
					Step{Why: "answers?", Argv: []string{"redis-cli", "ping"}})
			}
			steps = append(steps, Step{Why: "flush in the background (plugin FAQ)", Argv: []string{"redis-cli", "config", "set", "lazyfree-lazy-user-flush", "yes"}})
			live := redisLiveSteps(v.Int("maxmemory"), v["policy"])
			live[2].Argv = append(live[2].Argv, "lazyfree-lazy-user-flush", "yes")
			steps = append(steps, live...)
			if v.Bool("php-ext") {
				steps = append(steps, phpExtSteps(phpRedisMissing(ctx, env))...)
			}
			return steps, nil
		},
	})
	register(Op{
		ID: "redis-memory", Resolves: "redis", Applies: summaryHas("maxmemory"), Title: "Redis memory cap", Section: "performance", Risk: Change,
		Note:    "Sets maxmemory and the eviction policy on the running Redis and in redis.conf. No restart — run.sh restarted Redis here, which emptied every site's cache.",
		How:     "`redis-cli config set` changes the running server at once; `hs conf set` writes the same two lines to /etc/redis/redis.conf (existing lines replaced in place, missing ones added after their commented example), printing before → after and keeping the previous file under /var/lib/hs/backups. Lowering the cap below current use makes Redis evict until it fits — sites keep working, with more cache misses for a while.",
		Undo:    "Run it again with the previous values (shown as “now”), or copy the kept redis.conf back and `redis-cli config set` the old values.",
		Fields:  []Field{redisMaxField(), redisPolicyField()},
		Recheck: []string{"redis"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			st := box.Redis(ctx, env)
			if !st.Installed {
				return nil, fmt.Errorf("Redis is not installed — use Install Redis")
			}
			if !st.Running {
				return nil, fmt.Errorf("Redis is not running — systemctl start redis-server first")
			}
			if max := box.RAMMB(env.Sys) / 2; v.Int("maxmemory") > max {
				return nil, fmt.Errorf("%d MB is more than half the RAM (%s) — MariaDB and PHP need the rest", v.Int("maxmemory"), mb(box.RAMMB(env.Sys)))
			}
			if st.MaxMemory == int64(v.Int("maxmemory"))<<20 && st.Policy == v["policy"] {
				return nil, nil
			}
			return redisLiveSteps(v.Int("maxmemory"), v["policy"]), nil
		},
	})
	register(Op{
		ID: "redis-php-ext", Resolves: "redis.sites", Applies: summaryHas("no redis extension"), Title: "PHP redis extension", Section: "performance", Risk: Change,
		Note: "Installs php<ver>-redis so sites on that PHP version can use the object cache. PHP-FPM reloads gracefully — requests in flight finish.",
		How:  "apt installs the extension package (it drops an ini into /etc/php/<ver>/mods-available and enables it); `systemctl try-reload-or-restart php<ver>-fpm` starts new workers with it; the last step asks PHP whether it is loaded.",
		Undo: "apt purge php<ver>-redis and reload php<ver>-fpm — only once no site on that version has the object-cache drop-in.",
		Fields: []Field{versionField("PHP version", "Versions that serve pools but lack the extension are listed first.", true,
			func(ctx context.Context, env *check.Env) string {
				if m := phpRedisMissing(ctx, env); len(m) > 0 {
					return m[0]
				}
				return ""
			},
			func(ctx context.Context, env *check.Env, v string) string {
				switch {
				case box.PHPHasRedis(ctx, env, v):
					return "has it"
				case servesPools(env, v):
					return "MISSING — serves sites"
				}
				return "missing, no sites on it"
			})},
		Recheck: []string{"redis.sites"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			var vers []string
			for _, ver := range pickVersions(env, v["version"]) {
				if !box.PHPHasRedis(ctx, env, ver) {
					vers = append(vers, ver)
				}
			}
			return phpExtSteps(vers), nil
		},
	})

	register(Op{
		ID: "opcache", Resolves: "php.opcache", Title: "OpCache settings", Section: "performance", Risk: Change,
		Note: "Turns OpCache on with enough memory for WordPress and its plugins, keeps timestamp validation (updated plugins take effect), and reloads PHP-FPM gracefully.",
		How:  "`hs conf set` edits /etc/php/<ver>/fpm/php.ini in its [opcache] section: opcache.enable=1, opcache.memory_consumption, opcache.max_accelerated_files raised to 50000 when lower (Hestia's own installer uses 100000 — never lowered), opcache.validate_timestamps=1. Each changed line is printed; the previous php.ini is kept. `systemctl try-reload-or-restart` re-reads php.ini with a graceful reload — running requests finish, the cache starts cold.",
		Undo: "cp -p the kept php.ini back (the path is printed) and reload php<ver>-fpm.",
		Fields: []Field{
			versionField("PHP version", "", true,
				func(ctx context.Context, env *check.Env) string {
					for _, v := range fpmVersions(env) {
						if ok, _ := box.OpcacheOK(env, v); !ok && servesPools(env, v) {
							return v
						}
					}
					if vs := fpmVersions(env); len(vs) > 0 {
						return vs[len(vs)-1]
					}
					return ""
				},
				func(_ context.Context, env *check.Env, v string) string {
					ini := readFile(env, "/etc/php/"+v+"/fpm/php.ini")
					en, m := iniInt(ini, "opcache.enable"), iniInt(ini, "opcache.memory_consumption")
					s := "off"
					if en == 1 {
						s = "on"
					}
					if m > 0 {
						s += fmt.Sprintf(", %d MB", m)
					}
					if !servesPools(env, v) {
						s += ", no sites"
					}
					return s
				}),
			{Key: "memory", Label: "Memory (MB)", Kind: Number, Min: 64, Max: 4096,
				Help: "Compiled scripts for every site on the version share it. 256 MB fits a handful of WooCommerce sites; 512 MB when the box has 8 GB or more.",
				Default: func(_ context.Context, env *check.Env, _ Target) string {
					if box.RAMMB(env.Sys) >= 8192 {
						return "512"
					}
					return "256"
				}},
		},
		Recheck: []string{"php.opcache"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			var steps []Step
			for _, ver := range pickVersions(env, v["version"]) {
				ini := "/etc/php/" + ver + "/fpm/php.ini"
				cur := readFile(env, ini)
				if cur == "" {
					return nil, fmt.Errorf("%s not found", ini)
				}
				kv := []string{"opcache.enable", "1", "opcache.memory_consumption", v["memory"], "opcache.validate_timestamps", "1"}
				if iniInt(cur, "opcache.max_accelerated_files") < 50000 {
					kv = append(kv, "opcache.max_accelerated_files", "50000")
				}
				if iniInt(cur, "opcache.enable") == 1 && iniInt(cur, "opcache.memory_consumption") == v.Int("memory") &&
					iniInt(cur, "opcache.validate_timestamps") == 1 && len(kv) == 6 {
					continue
				}
				steps = append(steps,
					confSet("PHP "+ver, ini, conf.Opts{Section: "opcache", Style: conf.Eq, Comment: ";"}, kv...),
					Step{Why: "graceful reload re-reads php.ini", Argv: []string{"systemctl", "try-reload-or-restart", "php" + ver + "-fpm"}})
			}
			return steps, nil
		},
	})

	register(Op{
		ID: "mariadb-buffer", Resolves: "mariadb.buffer", Title: "MariaDB buffer pool size", Section: "performance", Risk: Change,
		Note: "Sets innodb_buffer_pool_size — resized on the running server, no restart (run.sh restarted MariaDB, taking every site down for the moment).",
		How:  "`SET GLOBAL innodb_buffer_pool_size` resizes the pool online (MariaDB 10.2+; done in chunks, queries keep running). `hs conf set` writes the same value under [mysqld] in " + mariadbCnf + " so it survives a restart, printing before → after and keeping the previous file. The last step reads the running size back.",
		Undo: "Run it again with the previous size (shown as “now”).",
		Fields: []Field{{Key: "size", Label: "Buffer pool", Kind: Text, Pattern: sizeRe,
			Help: "e.g. 2G or 768M. The pool holds hot table and index pages; the hit rate (Performance → MariaDB buffer pool) says whether it is big enough. Leave room for PHP workers and Redis — the Memory audit shows the PHP-FPM ceiling.",
			Current: func(ctx context.Context, env *check.Env, _ Target) string {
				s := "config " + orDash(box.MariaDBBufferConf(env))
				if out, err := env.Sys.Run(ctx, dbClient(env), "-N", "-B", "-e", "SELECT @@innodb_buffer_pool_size"); err == nil {
					n, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
					s += " · running " + mb(int(n>>20))
				}
				st := box.Redis(ctx, env)
				s += " · RAM " + mb(box.RAMMB(env.Sys))
				if st.MaxMemory > 0 {
					s += " · Redis cap " + mb(int(st.MaxMemory>>20))
				}
				return s
			},
			Default: func(_ context.Context, env *check.Env, _ Target) string {
				if c := box.MariaDBBufferConf(env); c != "" && sizeRe.MatchString(c) {
					return c
				}
				g := box.RAMMB(env.Sys) / 2 / 1024
				if g < 1 {
					return "512M"
				}
				return strconv.Itoa(g) + "G"
			}}},
		Recheck: []string{"mariadb.buffer"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			if !exists(env, mariadbCnf) {
				return nil, fmt.Errorf("%s not found", mariadbCnf)
			}
			if n := sizeBytes(v["size"]); n > int64(box.RAMMB(env.Sys))<<20*3/4 {
				return nil, fmt.Errorf("%s is over 75 %% of RAM (%s)", v["size"], mb(box.RAMMB(env.Sys)))
			} else if n < 128<<20 {
				return nil, fmt.Errorf("under 128M is too small to be useful")
			}
			c := dbClient(env)
			return []Step{
				{Why: "resize the running pool (online, no restart)", Argv: []string{c, "-e", fmt.Sprintf("SET GLOBAL innodb_buffer_pool_size = %d", sizeBytes(v["size"]))}},
				confSet("keep it across restarts", mariadbCnf, conf.Opts{Section: "mysqld", Style: conf.INI}, "innodb_buffer_pool_size", v["size"]),
				{Why: "running size (rounded to the chunk size)", Argv: []string{c, "-e", "SELECT ROUND(@@innodb_buffer_pool_size/1048576) AS buffer_pool_MB"}},
			}, nil
		},
	})

	register(Op{
		ID: "fpm-profile", Resolves: "php.fpm-profiles", Title: "PHP-FPM pool profile templates", Section: "performance", Risk: Change,
		Note: "Creates the sized pool templates (production / standard / staging / small) for a PHP version. Nothing changes for a site until it is switched to the template (Sites → Switch PHP version / pool template).",
		How:  "`hs conf install` copies Hestia's base template PHP-<ver>.tpl to <profile>-PHP-<ver>.tpl in " + hestia.FpmTpl + "; `hs conf set` writes the profile's pm keys (from templates/php-fpm/<profile>.conf in the repo), `hs conf unset` comments out the keys its pm mode ignores. Every changed line is printed; an existing template is kept under /var/lib/hs/backups before it is replaced. Hestia renders the template into a pool only when a domain using it is rebuilt.",
		Undo: "Delete the <profile>-PHP-<ver>.tpl files no domain uses, or copy the kept previous version back.",
		Fields: []Field{
			versionField("PHP version", "", true,
				func(_ context.Context, env *check.Env) string {
					if vs := fpmVersions(env); len(vs) > 1 {
						return "all"
					} else if len(vs) == 1 {
						return vs[0]
					}
					return ""
				},
				func(_ context.Context, env *check.Env, v string) string {
					tag := strings.ReplaceAll(v, ".", "_")
					var have []string
					for _, p := range fpmProfileNames {
						if exists(env, hestia.FpmTpl+"/"+p+"-PHP-"+tag+".tpl") {
							have = append(have, p)
						}
					}
					if len(have) == 0 {
						return "no profiles"
					}
					return "has " + strings.Join(have, ", ")
				}),
			{Key: "profile", Label: "Profile", Kind: Choice,
				Help: "all = the four templates; the pool itself is chosen per site.",
				Choices: func(_ context.Context, env *check.Env, _ Target) [][2]string {
					out := [][2]string{{"all", "all four"}}
					for _, n := range fpmProfileNames {
						if p, err := loadProfile(env, n); err == nil {
							out = append(out, [2]string{n, p.Desc + " — " + p.summary()})
						}
					}
					return out
				}},
		},
		Recheck: []string{"php.fpm-profiles"},
		Plan: func(_ context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			names := []string{v["profile"]}
			if v["profile"] == "all" {
				names = fpmProfileNames
			}
			var steps []Step
			for _, ver := range pickVersions(env, v["version"]) {
				if !exists(env, hestia.FpmTpl+"/PHP-"+strings.ReplaceAll(ver, ".", "_")+".tpl") {
					return nil, fmt.Errorf("no Hestia base template for PHP %s (%s/PHP-%s.tpl) — add the version first", ver, hestia.FpmTpl, strings.ReplaceAll(ver, ".", "_"))
				}
				for _, n := range names {
					p, err := loadProfile(env, n)
					if err != nil {
						return nil, err
					}
					steps = append(steps, profileSteps(p, ver)...)
				}
			}
			return steps, nil
		},
	})

	register(Op{
		ID: "php-add-version", Title: "Add a PHP version", Section: "performance", Risk: Change,
		Note: "Installs another PHP version for sites to switch to, with Hestia's own v-add-web-php (its package list, php.ini defaults and pool template), plus the redis extension when Redis runs here.",
		How:  "v-add-web-php installs the php<ver>-* packages Hestia uses for WordPress-capable pools (from the repository Hestia already configured), writes its php.ini defaults (upload size, OpCache on, exec disabled in FPM) and the PHP-<ver>.tpl pool template; the panel lists the version immediately. No site changes version until switched.",
		Undo: "v-delete-web-php <ver> once no domain uses it.",
		Fields: []Field{{Key: "version", Label: "Version", Kind: Choice,
			Choices: func(_ context.Context, env *check.Env, _ Target) [][2]string {
				var out [][2]string
				for _, v := range multiPHP(env) {
					if !exists(env, "/etc/php/"+v+"/fpm") {
						l := ""
						if n, _ := strconv.ParseFloat(v, 64); n < 8.1 {
							l = "end of life — only for a site that cannot move yet"
						}
						out = append(out, [2]string{v, l})
					}
				}
				sort.SliceStable(out, func(i, j int) bool { return out[i][0] > out[j][0] })
				return out
			}}},
		Recheck: []string{"php.fpm-profiles", "php.opcache"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			ver := v["version"]
			if exists(env, "/etc/php/"+ver+"/fpm") {
				return nil, nil
			}
			steps := []Step{{Why: "Hestia's installer for PHP " + ver, Argv: []string{bin + "v-add-web-php", ver}}}
			if box.Redis(ctx, env).Installed {
				steps = append(steps, phpExtSteps([]string{ver})...)
			}
			return append(steps, Step{Why: "check", Argv: []string{"php" + ver, "-v"}}), nil
		},
	})
}

func orDash(s string) string {
	if s == "" {
		return "not set"
	}
	return s
}
