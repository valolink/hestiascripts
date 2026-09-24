package box

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
)

func performanceChecks() []Check {
	return []Check{
		{ID: "redis", Section: "performance", Title: "Redis", Run: checkRedis},
		{ID: "redis.sites", Section: "performance", Title: "Redis object cache per site", Run: checkRedisSites},
		{ID: "php.opcache", Section: "performance", Title: "OpCache", Run: checkOpcache},
		{ID: "php.fpm-profiles", Section: "performance", Title: "PHP-FPM profiles", Run: checkFPMProfiles},
		{ID: "mariadb.buffer", Section: "performance", Title: "MariaDB buffer pool", Run: checkMariaDB},
	}
}

func redisCLI(ctx context.Context, env *Env, args ...string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return env.Sys.Run(rctx, "redis-cli", args...)
}

func infoField(info, key string) string {
	for _, l := range strings.Split(info, "\n") {
		if strings.HasPrefix(l, key+":") {
			return strings.TrimSpace(strings.TrimPrefix(l, key+":"))
		}
	}
	return ""
}

// RedisState is what setup-status reports.
type RedisState struct {
	Installed, Running bool
	MaxMemory          int64
	Policy             string
}

func Redis(ctx context.Context, env *Env) RedisState {
	var r RedisState
	if !env.Sys.Have("redis-server") {
		return r
	}
	r.Installed = true
	if !active(ctx, env.Sys, "redis-server") {
		return r
	}
	r.Running = true
	if out, err := redisCLI(ctx, env, "config", "get", "maxmemory"); err == nil {
		l := nonEmpty(out)
		if len(l) > 0 {
			r.MaxMemory, _ = strconv.ParseInt(l[len(l)-1], 10, 64)
		}
	}
	if out, err := redisCLI(ctx, env, "config", "get", "maxmemory-policy"); err == nil {
		if l := nonEmpty(out); len(l) > 0 {
			r.Policy = l[len(l)-1]
		}
	}
	return r
}

// kuumalahde 2026-08-30: nightly 504s. redis-cache's flush_group() SCANs the
// whole keyspace on every call (406k keys, 173 ms each, 41 % of a core) and
// Redis is single-threaded. Measure the symptom — EVAL's share of uptime.
func checkRedis(ctx context.Context, env *Env) []Result {
	st := Redis(ctx, env)
	switch {
	case !st.Installed:
		if len(hestia.WebDomains(env.Sys)) == 0 {
			return []Result{New(NA, "not installed, no sites")}
		}
		return []Result{New(Warn, "not installed").
			Because("WordPress sites run without an object cache, so every page rebuilds from MariaDB.").
			Fixed("run.sh → 3 (Redis) → 1")}
	case !st.Running:
		return []Result{New(Fail, "installed but not running").
			Because("Sites with the object-cache drop-in fall back to MariaDB for every lookup, or error.").
			Fixed("systemctl start redis-server")}
	}
	if out, err := redisCLI(ctx, env, "ping"); err != nil || strings.TrimSpace(out) != "PONG" {
		return []Result{New(Fail, "running but not answering PING")}
	}
	var rs []Result
	if st.MaxMemory == 0 {
		rs = append(rs, New(Warn, "maxmemory is not set").
			Because("Redis grows until the OOM killer picks something.").Fixed("run.sh → 3 (Redis) → 2"))
	}
	server, _ := redisCLI(ctx, env, "info", "server")
	stats, _ := redisCLI(ctx, env, "info", "commandstats")
	keyspace, _ := redisCLI(ctx, env, "info", "keyspace")
	uptime, _ := strconv.ParseFloat(infoField(server, "uptime_in_seconds"), 64)
	evalUsec := 0.0
	if m := regexp.MustCompile(`cmdstat_eval:calls=\d+,usec=(\d+)`).FindStringSubmatch(stats); m != nil {
		evalUsec, _ = strconv.ParseFloat(m[1], 64)
	}
	evalPct := 0.0
	if uptime > 3600 {
		evalPct = evalUsec / 1e6 * 100 / uptime
		if evalPct >= 10 {
			rs = append(rs, New(Warn, fmt.Sprintf("%.0f%% of uptime spent in object-cache flush scans", evalPct)).
				Because("flush_group() scans the entire keyspace on every call and Redis is single-threaded, so each scan stalls every other read — 504s under load.").
				Fixed("wp-config: define('WP_REDIS_DISABLE_GROUP_FLUSH', true); define('WP_REDIS_MAXTTL', 86400);"))
		}
	}
	var keys, expires int64
	for _, m := range regexp.MustCompile(`keys=(\d+),expires=(\d+)`).FindAllStringSubmatch(keyspace, -1) {
		k, _ := strconv.ParseInt(m[1], 10, 64)
		e, _ := strconv.ParseInt(m[2], 10, 64)
		keys += k
		expires += e
	}
	if keys > 50000 && expires*100/keys < 25 {
		rs = append(rs, New(Warn, fmt.Sprintf("%d keys, only %d%% with a TTL", keys, expires*100/keys)).
			Because("Nothing bounds the keyspace, so it grows until every flush scan is slow.").
			Fixed("define('WP_REDIS_MAXTTL', 86400); in wp-config.php, then flush that site's database"))
	}
	if len(rs) > 0 {
		return rs
	}
	return []Result{New(OK, fmt.Sprintf("answering, %d MB cap %s, %d keys, flush scans %.1f%% of uptime",
		st.MaxMemory>>20, st.Policy, keys, evalPct))}
}

func checkRedisSites(ctx context.Context, env *Env) []Result {
	s := env.Sys
	var rs []Result
	prefixes := map[string][]string{}
	withDropin := 0
	for _, d := range hestia.WebDomains(s) {
		root := d.DocRoot()
		if !exists(s, root+"/wp-content/object-cache.php") {
			continue
		}
		withDropin++
		conf := readString(s, root+"/wp-config.php")
		if !strings.Contains(conf, "WP_REDIS_DISABLE_GROUP_FLUSH") {
			rs = append(rs, New(Warn, "object cache without WP_REDIS_DISABLE_GROUP_FLUSH").For(d.Name).
				Because("Harmless until the keyspace grows — then every group flush scans all of it.").
				Fixed(fmt.Sprintf("wp config set WP_REDIS_DISABLE_GROUP_FLUSH true --raw --type=constant --path=%s", root)))
		}
		// Sites collide only when both the Redis database and the key prefix
		// match — one database per site (the clone/staging default) is enough.
		db := "0"
		if m := regexp.MustCompile(`WP_REDIS_DATABASE['"]\s*,\s*['"]?(\d+)`).FindStringSubmatch(conf); m != nil {
			db = m[1]
		}
		prefix := ""
		if m := regexp.MustCompile(`WP_REDIS_PREFIX['"]\s*,\s*['"]([^'"]+)`).FindStringSubmatch(conf); m != nil {
			prefix = m[1]
		} else if m := regexp.MustCompile(`WP_CACHE_KEY_SALT['"]\s*,\s*['"]([^'"]+)`).FindStringSubmatch(conf); m != nil {
			prefix = m[1]
		}
		key := "db " + db + ", prefix " + orDash(prefix)
		prefixes[key] = append(prefixes[key], d.Name)
	}
	for key, doms := range prefixes {
		if len(doms) > 1 {
			rs = append(rs, New(Fail, fmt.Sprintf("%d sites share one Redis keyspace (%s)", len(doms), key)).For(key).Ev(doms...).
				Because("Sites sharing a keyspace read each other's cached options and posts — a staging copy can serve live's data or the reverse.").
				Fixed("give each site its own WP_REDIS_DATABASE or a unique WP_REDIS_PREFIX (v-wp-redis-install does)"))
		}
	}
	// PHP redis extension on every version that serves a pool.
	for _, v := range PHPVersions(s) {
		pools, _ := s.Glob("/etc/php/" + v + "/fpm/pool.d/*.conf")
		if len(pools) == 0 {
			continue
		}
		if !PHPHasRedis(ctx, env, v) {
			rs = append(rs, New(Warn, "PHP "+v+" has no redis extension").For("php"+v).
				Because("Sites on this version cannot use the object cache.").
				Fixed("run.sh → 3 (Redis) → 3"))
		}
	}
	if len(rs) > 0 {
		return rs
	}
	if withDropin == 0 {
		return []Result{New(NA, "no site uses the Redis object cache")}
	}
	return []Result{New(OK, fmt.Sprintf("%d sites on the object cache, unique prefixes, guard constant set", withDropin))}
}

func PHPHasRedis(ctx context.Context, env *Env, ver string) bool {
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := env.Sys.Run(pctx, "php"+ver, "-r", "echo extension_loaded('redis') ? 1 : 0;")
	return err == nil && strings.TrimSpace(out) == "1"
}

// OpcacheOK reports whether php.ini for ver enables OpCache with ≥ 256 MB,
// and whether the ini exists at all. Shared with setup-status.
func OpcacheOK(env *Env, ver string) (ok, present bool) {
	ini := readString(env.Sys, "/etc/php/"+ver+"/fpm/php.ini")
	if ini == "" {
		return false, false
	}
	en := regexp.MustCompile(`(?m)^\s*opcache\.enable=(\d)`).FindStringSubmatch(ini)
	mem := regexp.MustCompile(`(?m)^\s*opcache\.memory_consumption=(\d+)`).FindStringSubmatch(ini)
	if en == nil || en[1] != "1" || mem == nil {
		return false, true
	}
	n, _ := strconv.Atoi(mem[1])
	return n >= 256, true
}

func checkOpcache(ctx context.Context, env *Env) []Result {
	var rs []Result
	var good []string
	for _, v := range PHPVersions(env.Sys) {
		pools, _ := env.Sys.Glob("/etc/php/" + v + "/fpm/pool.d/*.conf")
		ok, present := OpcacheOK(env, v)
		if !present || len(pools) == 0 {
			continue
		}
		if !ok {
			rs = append(rs, New(Warn, "PHP "+v+": OpCache off or under 256 MB").For("php"+v).
				Because("WordPress recompiles every PHP file on every request.").Fixed("run.sh → 10 (OpCache)"))
		} else {
			good = append(good, v)
		}
	}
	if len(rs) > 0 {
		return rs
	}
	if len(good) == 0 {
		return []Result{New(NA, "no PHP-FPM versions with pools")}
	}
	// Configured, not OK: the ini says so; hit rate would need a request
	// through FPM to prove it, which a read-only check does not make.
	return []Result{New(Configured, "enabled with ≥ 256 MB on PHP "+strings.Join(good, ", "))}
}

var fpmProfiles = []string{"production", "standard", "staging", "small"}

// FPMProfiles counts installed/missing profile templates across PHP versions.
func FPMProfiles(env *Env) (installed, missing int) {
	for _, v := range PHPVersions(env.Sys) {
		tag := strings.ReplaceAll(v, ".", "_")
		for _, p := range fpmProfiles {
			if exists(env.Sys, hestia.FpmTpl+"/"+p+"-PHP-"+tag+".tpl") {
				installed++
			} else {
				missing++
			}
		}
	}
	return
}

func checkFPMProfiles(ctx context.Context, env *Env) []Result {
	inst, miss := FPMProfiles(env)
	switch {
	case inst+miss == 0:
		return []Result{New(Warn, "no PHP versions found")}
	case miss > 0:
		return []Result{New(Warn, fmt.Sprintf("%d profile templates installed, %d missing", inst, miss)).
			Because("A domain cannot be moved to a sized pool profile on the versions that lack it.").
			Fixed("run.sh → 9 (PHP-FPM) → 1")}
	}
	return []Result{New(Configured, fmt.Sprintf("%d profile templates installed", inst))}
}

// MariaDBBufferConf is innodb_buffer_pool_size as written in 50-server.cnf.
func MariaDBBufferConf(env *Env) string {
	m := regexp.MustCompile(`(?m)^\s*innodb_buffer_pool_size\s*=\s*(\S+)`).
		FindStringSubmatch(readString(env.Sys, "/etc/mysql/mariadb.conf.d/50-server.cnf"))
	if m == nil {
		return ""
	}
	return m[1]
}

// The number that decides is the hit rate, not dataset vs pool size: a
// dataset bigger than the pool is fine while the working set fits.
func checkMariaDB(ctx context.Context, env *Env) []Result {
	q := `SELECT VARIABLE_NAME, VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS WHERE VARIABLE_NAME IN ('UPTIME','INNODB_BUFFER_POOL_READS','INNODB_BUFFER_POOL_READ_REQUESTS')`
	out, err := env.Sys.Run(ctx, "mysql", "-N", "-B", "-e", q)
	if err != nil {
		return []Result{New(Unknown, "MariaDB not reachable as root")}
	}
	v := map[string]float64{}
	for _, l := range nonEmpty(out) {
		f := strings.Fields(l)
		if len(f) == 2 {
			v[f[0]], _ = strconv.ParseFloat(f[1], 64)
		}
	}
	reqs, reads, up := v["INNODB_BUFFER_POOL_READ_REQUESTS"], v["INNODB_BUFFER_POOL_READS"], v["UPTIME"]
	if reqs == 0 {
		return []Result{New(Unknown, "no buffer pool reads recorded yet")}
	}
	hit := 100 * (1 - reads/reqs)
	ev := fmt.Sprintf("hit rate %.3f%% over %.1f h, pool %s", hit, up/3600, orDash(MariaDBBufferConf(env)))
	if up < 86400 {
		return []Result{New(Configured, fmt.Sprintf("hit rate %.2f%% (uptime under a day, provisional)", hit)).Ev(ev)}
	}
	if hit < 99 {
		return []Result{New(Warn, fmt.Sprintf("buffer pool hit rate %.2f%%", hit)).Ev(ev).
			Because("Below 99% the pool is too small for the working set, so reads go to disk.").
			Fixed("run.sh → 11 (MariaDB)")}
	}
	return []Result{New(OK, fmt.Sprintf("buffer pool hit rate %.2f%%", hit)).Ev(ev)}
}

func orDash(s string) string {
	if s == "" {
		return "not set"
	}
	return s
}
