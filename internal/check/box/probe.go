// Package box holds the box-level checks. Each area has its own file; All()
// is the registry.
package box

import (
	"context"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/sys"
)

// All returns every box-level check in display order.
func All() []check.Check {
	var cs []check.Check
	cs = append(cs, securityChecks()...)
	cs = append(cs, backupChecks()...)
	cs = append(cs, systemChecks()...)
	cs = append(cs, performanceChecks()...)
	cs = append(cs, webChecks()...)
	cs = append(cs, mailChecks()...)
	cs = append(cs, monitoringChecks()...)
	return cs
}

// --- shared probes -----------------------------------------------------------

func active(ctx context.Context, s sys.Sys, unit string) bool {
	_, err := s.Run(ctx, "systemctl", "is-active", "--quiet", unit)
	return err == nil
}

func unitExists(ctx context.Context, s sys.Sys, unit string) bool {
	out, _ := s.Run(ctx, "systemctl", "list-unit-files", "--no-legend", unit+".service")
	return strings.TrimSpace(out) != ""
}

// ActiveSince is when a unit last entered the active state (zero if unknown).
func activeSince(ctx context.Context, s sys.Sys, unit string) time.Time {
	out, err := s.Run(ctx, "systemctl", "show", unit, "-p", "ActiveEnterTimestamp", "--value", "--timestamp=unix")
	if err != nil {
		return time.Time{}
	}
	v := strings.TrimPrefix(strings.TrimSpace(out), "@")
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func pkgInstalled(ctx context.Context, s sys.Sys, pkgs ...string) bool {
	for _, p := range pkgs {
		out, err := s.Run(ctx, "dpkg-query", "-W", "-f=${Status}", p)
		if err == nil && strings.Contains(out, "install ok installed") {
			return true
		}
	}
	return false
}

// PHPVersions lists /etc/php/<ver> directories, oldest first.
func PHPVersions(s sys.Sys) []string {
	ents, err := s.ReadDir("/etc/php")
	if err != nil {
		return nil
	}
	var vs []string
	for _, e := range ents {
		if e.IsDir() {
			vs = append(vs, e.Name())
		}
	}
	sort.Slice(vs, func(i, j int) bool { return versionLess(vs[i], vs[j]) })
	return vs
}

func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return len(pa) < len(pb)
}

func readString(s sys.Sys, path string) string {
	b, err := s.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func exists(s sys.Sys, path string) bool {
	_, err := s.Stat(path)
	return err == nil
}

func sameFile(s sys.Sys, a, b string) (same bool, ok bool) {
	x, err1 := s.ReadFile(a)
	y, err2 := s.ReadFile(b)
	if err1 != nil || err2 != nil {
		return false, false
	}
	return string(x) == string(y), true
}

// Meminfo returns /proc/meminfo values in kB.
func meminfo(s sys.Sys) map[string]int64 {
	m := map[string]int64{}
	for _, line := range strings.Split(readString(s, "/proc/meminfo"), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			m[strings.TrimSuffix(f[0], ":")] = n
		}
	}
	return m
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func joinMax(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + ", +" + strconv.Itoa(len(items)-max) + " more"
}

func hours(d time.Duration) int { return int(d.Hours()) }

var _ = hostname

// RAMMB is installed memory in MB.
func RAMMB(s sys.Sys) int { return int(meminfo(s)["MemTotal"] / 1024) }

// PkgInstalled reports whether any of the packages is installed (dpkg).
func PkgInstalled(ctx context.Context, s sys.Sys, pkgs ...string) bool {
	return pkgInstalled(ctx, s, pkgs...)
}

// RedisCLI runs redis-cli with a short timeout.
func RedisCLI(ctx context.Context, env *check.Env, args ...string) (string, error) {
	return redisCLI(ctx, env, args...)
}
