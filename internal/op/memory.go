package op

// Memory: the swap file (create or resize), the kernel's memory knobs, and
// the PHP-FPM ceiling — the three levers behind the kuumalahde swap alert of
// 2026-09-30 (a 1 GB swap file 94 % full of parked pages on a 7.9 GB box,
// nothing moving, and pools allowed to fork far past RAM).

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/box"
	"github.com/valolink/hestiascripts/internal/conf"
)

const (
	swapFile = "/swapfile"
	// swapMovedDir keeps a replaced swap file (moved, never deleted): stale
	// memory pages, kept only so the change can be put back.
	swapMovedDir = "/root/hs-moved"
	// swapOffMargin: swapoff brings every page in swap back into RAM; refuse
	// unless that leaves this much MemAvailable for the running services.
	swapOffMargin = 512
	sysctlFile    = "/etc/sysctl.d/90-hs-memory.conf"
	// fpmReserveMB is what the box needs besides PHP, MariaDB and Redis: the
	// OS, nginx, Netdata, Redis's fork for a save, page cache for the sites.
	fpmReserveMB = 1024
)

func meminfoMB(env *check.Env, key string) int {
	for _, line := range strings.Split(readFile(env, "/proc/meminfo"), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == key+":" {
			n, _ := strconv.Atoi(f[1])
			return n / 1024
		}
	}
	return 0
}

// swapSummary is the swap field's "now" line.
func swapSummary(env *check.Env) string {
	areas := box.SwapAreas(env.Sys)
	ram := fmt.Sprintf(" · RAM %s, %s available", mb(box.RAMMB(env.Sys)), mb(meminfoMB(env, "MemAvailable")))
	if len(areas) == 0 {
		return "no swap" + ram
	}
	var parts []string
	for _, a := range areas {
		parts = append(parts, fmt.Sprintf("%s %s, %s used", a.Path, mb(int(a.SizeKB/1024)), mb(int(a.UsedKB/1024))))
	}
	return strings.Join(parts, "; ") + ram
}

// swapSizeMB rounds a size like 2G / 1536M to MB.
func swapSizeMB(size string) int { return int(sizeBytes(size) >> 20) }

// swapCreateSteps make swapFile from nothing and register it in fstab.
func swapCreateSteps(size string) []Step {
	return []Step{
		{Why: "reserve " + size, Argv: []string{"fallocate", "-l", size, swapFile}},
		{Argv: []string{"chmod", "600", swapFile}},
		{Argv: []string{"mkswap", swapFile}},
		{Why: "use it now", Argv: []string{"swapon", swapFile}},
		{Why: "and at boot (the fstab line is added only if absent)", Argv: []string{"sh", "-c", "grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab; grep '^/swapfile ' /etc/fstab"}},
		{Argv: []string{"swapon", "--show"}},
	}
}

// swapMoveStep keeps the old file under swapMovedDir.
func swapMoveStep(env *check.Env, why string) Step {
	dest := swapMovedDir + "/" + env.Sys.Now().Format("20060102")
	return Step{Why: why, Argv: []string{"sh", "-c", `install -d -m 700 "$1" && mv -n -v -- ` + swapFile + ` "$1/swapfile"`, "sh", dest}}
}

// --- kernel knobs ------------------------------------------------------------

type vmKnob struct {
	Key, Label, Help string
	Min, Max         int
	// Suggest: what to pre-fill given today's value ("" = keep it).
	Suggest func(ctx context.Context, env *check.Env, cur string) string
}

var vmKnobs = []vmKnob{
	{Key: "vm.swappiness", Label: "vm.swappiness", Min: 0, Max: 200,
		Help: "How eagerly idle anonymous memory (PHP heap, MariaDB, Redis) is parked in swap to make room for page cache. 60 is Debian's default; 10 suits a box whose services keep their own caches. Not 0 — that is no swapping until the box is out of memory.",
		Suggest: func(_ context.Context, _ *check.Env, cur string) string {
			if n, _ := strconv.Atoi(cur); n > 30 {
				return "10"
			}
			return cur
		}},
	{Key: "vm.vfs_cache_pressure", Label: "vm.vfs_cache_pressure", Min: 1, Max: 1000,
		Help: "How readily the kernel drops directory and inode entries against other cache. 100 is the default; 50 keeps them longer — WordPress stats thousands of files per request and OpCache re-checks them every 2 s.",
		Suggest: func(_ context.Context, _ *check.Env, cur string) string {
			if cur == "100" {
				return "50"
			}
			return cur
		}},
	{Key: "vm.overcommit_memory", Label: "vm.overcommit_memory", Min: 0, Max: 2,
		Help: "0 = the kernel refuses allocations it judges too large, 1 = always allow (copy-on-write means little of it is used), 2 = strict. Redis forks for every RDB save and its startup log asks for 1 — with 0 the save's fork can fail on a box with little free RAM.",
		Suggest: func(ctx context.Context, env *check.Env, cur string) string {
			if cur == "0" && box.Redis(ctx, env).Installed {
				return "1"
			}
			return cur
		}},
}

func vmFields() []Field {
	var fs []Field
	for _, k := range vmKnobs {
		k := k
		fs = append(fs, Field{Key: k.Key, Label: k.Label, Kind: Number, Min: k.Min, Max: k.Max, Help: k.Help,
			Current: func(_ context.Context, env *check.Env, _ Target) string {
				cur := box.Sysctl(env.Sys, k.Key)
				if k.Key == "vm.swappiness" {
					return cur + " · " + swapSummary(env)
				}
				return cur
			},
			Default: func(ctx context.Context, env *check.Env, _ Target) string {
				cur := box.Sysctl(env.Sys, k.Key)
				if cur == "" {
					return ""
				}
				return k.Suggest(ctx, env, cur)
			}})
	}
	return fs
}

// sysctlElsewhere lists other sysctl files setting a key — they win when
// they sort after ours at boot.
func sysctlElsewhere(env *check.Env, key string) []string {
	files, _ := env.Sys.Glob("/etc/sysctl.d/*.conf")
	files = append(files, "/etc/sysctl.conf")
	var out []string
	for _, f := range files {
		if f == sysctlFile {
			continue
		}
		if v, ok := conf.Get(readFile(env, f), conf.Opts{}, key); ok {
			out = append(out, f+" ("+v+")")
		}
	}
	return out
}

// --- PHP-FPM ceiling ---------------------------------------------------------

// fpmBudget: how many workers this box can afford in total, and the numbers
// behind that, for the form's hint and the plan's explanation.
type fpmBudget struct {
	RAMMB, ReserveMB, DBMB, RedisMB int
	PrivateMB, SharedMB             int
	Workers                         int // measured now
	FitTotal                        int // workers that fit, across every pool
}

func (b fpmBudget) explain() string {
	if b.PrivateMB == 0 {
		return "no PHP-FPM worker running to measure — start with a request to a site, then reopen"
	}
	return fmt.Sprintf("RAM %s − %s system − MariaDB pool %s − Redis cap %s = %s for PHP ÷ %d MB private per worker (measured over %d workers, %d MB shared once) → about %d workers in total across every pool",
		mb(b.RAMMB), mb(b.ReserveMB), mb(b.DBMB), mb(b.RedisMB), mb(max(b.RAMMB-b.ReserveMB-b.DBMB-b.RedisMB, 0)), b.PrivateMB, b.Workers, b.SharedMB, b.FitTotal)
}

func budget(ctx context.Context, env *check.Env) fpmBudget {
	b := fpmBudget{RAMMB: box.RAMMB(env.Sys), ReserveMB: fpmReserveMB}
	private, shared, n := box.FPMWorkerMemory(env.Sys)
	b.PrivateMB, b.SharedMB, b.Workers = int(private/1024), int(shared/1024), n
	if out, err := env.Sys.Run(ctx, dbClient(env), "-N", "-B", "-e", "SELECT @@innodb_buffer_pool_size"); err == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		b.DBMB = int(v >> 20)
	}
	if st := box.Redis(ctx, env); st.MaxMemory > 0 {
		b.RedisMB = int(st.MaxMemory >> 20)
	}
	if b.PrivateMB > 0 {
		b.FitTotal = max((b.RAMMB-b.ReserveMB-b.DBMB-b.RedisMB-b.SharedMB)/b.PrivateMB, 2)
	}
	return b
}

// biggest is the template with the largest ceiling — the form's default.
func biggest(env *check.Env) (box.FPMTemplate, bool) {
	ts := box.FPMTemplates(env.Sys)
	if len(ts) == 0 {
		return box.FPMTemplate{}, false
	}
	return ts[0], true
}

func templateByName(env *check.Env, name string) (box.FPMTemplate, bool) {
	for _, t := range box.FPMTemplates(env.Sys) {
		if t.Name == name {
			return t, true
		}
	}
	return box.FPMTemplate{}, false
}

func fpmSummary(t box.FPMTemplate) string {
	s := fmt.Sprintf("pm %s, max_children %d", orDash(t.Mode), t.MaxChildren)
	switch t.Mode {
	case "dynamic":
		s += fmt.Sprintf(", start %d, spare %d–%d", t.Start, t.MinSpare, t.MaxSpare)
	case "ondemand":
		s += ", idle timeout " + orDash(t.IdleTimeout)
	}
	return s
}

// spareField is one of the dynamic-mode counts, shown from the biggest
// template (the form's default) and left empty otherwise.
func spareField(key, label, help string, pick func(t box.FPMTemplate) int) Field {
	return Field{Key: key, Label: label, Kind: Number, Min: 1, Max: 1000, Optional: true, Help: help,
		Current: func(_ context.Context, env *check.Env, _ Target) string {
			if t, ok := biggest(env); ok && t.Mode == "dynamic" {
				return fmt.Sprintf("%d in %s", pick(t), t.Name)
			}
			return "not used by the biggest template's pm mode"
		},
		Default: func(_ context.Context, env *check.Env, _ Target) string {
			if t, ok := biggest(env); ok && t.Mode == "dynamic" && pick(t) > 0 {
				return strconv.Itoa(pick(t))
			}
			return ""
		}}
}

func init() {
	register(Op{
		ID: "vm-sysctl", Title: "Kernel memory settings (swappiness, cache pressure, overcommit)", Section: "system", Risk: Change,
		Resolves: "memory.swap", Applies: summaryHas("parked"),
		Note: "vm.swappiness, vm.vfs_cache_pressure and vm.overcommit_memory: applied to the running kernel and kept in " + sysctlFile + ". Nothing restarts.",
		How: "`hs conf set` writes the three keys to " + sysctlFile + " (created if missing; before → after printed, previous file kept), `sysctl -p` on that file applies them now and proves the file parses, and the last step reads the running values back. " +
			"swappiness: at Debian's 60 the kernel parks idle anonymous memory early even with RAM to spare — how a 1 GB swap file reads 94 % used with nothing moving (kuumalahde 2026-09-30); 10 keeps PHP workers, MariaDB and Redis in RAM and drops page cache first, which is the right trade on a box whose services hold their own caches (OpCache, buffer pool, Redis). " +
			"vfs_cache_pressure 50: directory and inode entries stay cached longer; WordPress stats thousands of files per request. " +
			"overcommit_memory 1: Redis forks for every RDB save and needs the fork to be allowed; its own startup log asks for this. The plan names any other sysctl file that sets the same key — a file sorting after ours wins at boot.",
		Undo:    "Run it again with the previous values (shown as “now”), or rm " + sysctlFile + " and `sysctl -w` the old values.",
		Fields:  vmFields(),
		Recheck: []string{"memory.swap"},
		Plan: func(_ context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			var kv, notes []string
			same := true
			for _, k := range vmKnobs {
				if v[k.Key] == "" {
					return nil, fmt.Errorf("%s: no value (is /proc/sys readable here?)", k.Key)
				}
				kv = append(kv, k.Key, v[k.Key])
				if box.Sysctl(env.Sys, k.Key) != v[k.Key] {
					same = false
				}
				if others := sysctlElsewhere(env, k.Key); len(others) > 0 {
					notes = append(notes, k.Key+" is also set in "+strings.Join(others, ", ")+" — a file sorting after 90-hs-memory.conf wins at boot")
				}
			}
			fileHas := true
			for i := 0; i < len(kv); i += 2 {
				if cur, _ := conf.Get(readFile(env, sysctlFile), conf.Opts{}, kv[i]); cur != kv[i+1] {
					fileHas = false
				}
			}
			if same && fileHas {
				return nil, nil
			}
			var steps []Step
			if !exists(env, sysctlFile) {
				steps = append(steps, Step{Why: "the hs-owned sysctl file", Argv: []string{"sh", "-c", "printf '# hs: kernel memory settings (hs op vm-sysctl)\\n' > " + sysctlFile}})
			}
			why := "the values"
			if len(notes) > 0 {
				why += "\n" + strings.Join(notes, "\n")
			}
			return append(steps,
				confSet(why, sysctlFile, conf.Opts{Style: conf.INI}, kv...),
				Step{Why: "apply now (and the file parses)", Argv: []string{"sysctl", "-p", sysctlFile}},
				Step{Why: "running values", Argv: []string{"sysctl", "vm.swappiness", "vm.vfs_cache_pressure", "vm.overcommit_memory"}},
			), nil
		},
	})
	register(Op{
		ID: "swap", Title: "Swap file size", Section: "system", Risk: Change,
		Resolves: "memory.swap",
		Note:     "Creates /swapfile, or resizes it in place of the old one. Without swap the kernel OOM-kills at once instead of degrading; with one that is full of parked pages nothing more can be parked. The old file is moved, never deleted.",
		How: "New: fallocate reserves /swapfile, chmod 600, mkswap, swapon, one /swapfile line in /etc/fstab (added only if absent). " +
			"Resize: the new file is made first as /swapfile.new and switched on beside the old one, so a full disk fails before anything changes and the pages leaving the old file have somewhere to go; `swapoff /swapfile` then brings the old file's pages back — refused at planning time unless MemAvailable covers them with " + strconv.Itoa(swapOffMargin) + " MB to spare; the old file moves to " + swapMovedDir + "/<date>/swapfile (an active swap file cannot be renamed, so the new one is switched off for a moment, renamed to /swapfile and switched on again). The fstab line does not change. The moved file holds stale memory pages and keeps its size on disk: rm it once the new swap has proven itself.",
		Undo: "swapoff /swapfile, mv " + swapMovedDir + "/<date>/swapfile back to /swapfile, swapon /swapfile (the new file is then the one to remove).",
		Fields: []Field{{Key: "size", Label: "Size", Kind: Text, Pattern: sizeRe, Help: "e.g. 2G or 1024M. A file the size of RAM is pointless on a box that must not swap under load; 1–2 GB gives the kernel room to park idle pages and the OOM killer a margin.",
			Current: func(_ context.Context, env *check.Env, _ Target) string { return swapSummary(env) },
			Default: func(_ context.Context, env *check.Env, _ Target) string {
				for _, a := range box.SwapAreas(env.Sys) {
					if a.Path == swapFile {
						return strconv.Itoa(int((a.SizeKB+4)/1024)) + "M"
					}
				}
				if box.RAMMB(env.Sys) <= 4096 {
					return "2G"
				}
				return "1G"
			}}},
		Recheck: []string{"memory.swap"},
		Plan: func(_ context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			areas := box.SwapAreas(env.Sys)
			var cur *box.SwapArea
			for i := range areas {
				if areas[i].Path == swapFile {
					cur = &areas[i]
				} else {
					return nil, fmt.Errorf("swap is on %s (%s), not on /swapfile — not automated; swapoff it first if /swapfile is to replace it", areas[i].Path, areas[i].Type)
				}
			}
			if exists(env, swapFile+".new") {
				return nil, fmt.Errorf("%s.new exists — a previous run stopped half way; swapoff and remove it first", swapFile)
			}
			wantMB := swapSizeMB(v["size"])
			switch {
			case cur == nil && !exists(env, swapFile):
				return swapCreateSteps(v["size"]), nil
			case cur == nil:
				// A file nothing uses (swapoff by hand): out of the way, then new.
				return append([]Step{swapMoveStep(env, swapFile+" exists but is not in use — kept aside")}, swapCreateSteps(v["size"])...), nil
			}
			haveMB := int((cur.SizeKB + 4) / 1024) // /proc/swaps omits the header page
			if haveMB == wantMB {
				return nil, nil
			}
			usedMB, availMB := int(cur.UsedKB/1024), meminfoMB(env, "MemAvailable")
			if availMB-usedMB < swapOffMargin {
				return nil, fmt.Errorf("refused: swapoff must bring %d MB back into RAM and only %d MB is available — that leaves under %d MB for the running services; lower the PHP-FPM ceiling or wait for a quiet moment", usedMB, availMB, swapOffMargin)
			}
			return []Step{
				{Why: fmt.Sprintf("%d MB → %d MB; %d MB in use, %d MB of RAM available to take it back\nthe new file first, so a full disk fails here", haveMB, wantMB, usedMB, availMB),
					Argv: []string{"fallocate", "-l", v["size"], swapFile + ".new"}},
				{Argv: []string{"chmod", "600", swapFile + ".new"}},
				{Argv: []string{"mkswap", swapFile + ".new"}},
				{Why: "on beside the old one: pages leaving it can land here", Argv: []string{"swapon", swapFile + ".new"}},
				{Why: fmt.Sprintf("empty the old file (%d MB back into RAM)", usedMB), Argv: []string{"swapoff", swapFile}},
				swapMoveStep(env, "keep the old file (stale pages; rm it once the new swap has proven itself)"),
				{Why: "an active swap file cannot be renamed: off for a moment", Argv: []string{"swapoff", swapFile + ".new"}},
				{Argv: []string{"mv", "-n", "-v", "--", swapFile + ".new", swapFile}},
				{Why: "on again under its fstab name", Argv: []string{"swapon", swapFile}},
				{Why: "the fstab line is unchanged", Argv: []string{"sh", "-c", "grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab; grep '^/swapfile ' /etc/fstab"}},
				{Argv: []string{"swapon", "--show"}},
			}, nil
		},
	})
	register(Op{
		ID: "fpm-pool-size", Title: "PHP-FPM pool size (max_children, spare workers)", Section: "performance", Risk: Change,
		Resolves: "memory.fpm-ceiling",
		Note:     "Lowers (or raises) what the domains on one pool template may fork, in the template itself — the place Hestia regenerates every pool from, so it lasts. Then each domain's pool is rebuilt and PHP-FPM reloads gracefully.",
		How: "`hs conf set` writes pm, pm.max_children and the mode's spare-worker keys into the backend template in " + "/usr/local/hestia/data/templates/web/php-fpm" + " (before → after printed, previous file kept); `hs conf unset` comments out the keys the mode does not use. " +
			"`v-rebuild-web-domain USER DOMAIN no` re-renders each domain's pool file from the template (a pool file edited by hand is overwritten on the next rebuild, which is why the template is edited instead); the vhost files are re-rendered from unchanged templates, so nginx needs no reload. " +
			"`php-fpmX.Y -t` checks the pools, then `systemctl try-reload-or-restart` reloads gracefully — running requests finish, new workers use the new limits. The last step shows the pm lines of the rebuilt pools. " +
			"The number that fits: RAM minus " + mb(fpmReserveMB) + " for the system, minus MariaDB's buffer pool and Redis's cap, divided by one worker's private memory (PSS-based, so the shared OpCache is counted once) — shown beside the field and in the plan.",
		Undo: "Run it again with the previous values (shown as “now”); the template's previous file is kept under /var/lib/hs/backups.",
		Fields: []Field{
			{Key: "template", Label: "Pool template", Kind: Choice, Help: "Templates in use, biggest ceiling (domains × max_children) first.",
				Choices: func(_ context.Context, env *check.Env, _ Target) [][2]string {
					var out [][2]string
					for _, t := range box.FPMTemplates(env.Sys) {
						var names []string
						for _, d := range t.Domains {
							names = append(names, d.Name)
						}
						out = append(out, [2]string{t.Name, fmt.Sprintf("%d workers: %s × %s (%s)", t.Ceiling(), plural(len(t.Domains), "domain", "domains"), fpmSummary(t), joinMax(names, 3))})
					}
					return out
				}},
			{Key: "mode", Label: "pm", Kind: Choice, Help: "dynamic keeps min–max spare workers ready; static forks max_children at once and keeps them; ondemand forks per request and idles them out.",
				Current: func(_ context.Context, env *check.Env, _ Target) string {
					if t, ok := biggest(env); ok {
						return orDash(t.Mode) + " in " + t.Name
					}
					return ""
				},
				Choices: func(context.Context, *check.Env, Target) [][2]string {
					return [][2]string{{"dynamic", "spare workers between min and max"}, {"static", "max_children workers, always"}, {"ondemand", "forked per request, idle ones exit"}}
				},
				Default: func(_ context.Context, env *check.Env, _ Target) string {
					if t, ok := biggest(env); ok && t.Mode != "" {
						return t.Mode
					}
					return "dynamic"
				}},
			{Key: "max_children", Label: "pm.max_children", Kind: Number, Min: 1, Max: 1000,
				Help: "Per domain on the template. The hint is the whole box's budget: what every pool may fork together should stay under it, with the busiest sites getting most of it.",
				Current: func(ctx context.Context, env *check.Env, _ Target) string {
					b := budget(ctx, env)
					t, ok := biggest(env)
					if !ok {
						return "no domains"
					}
					children, pools := box.FPMPoolChildren(env.Sys)
					return fmt.Sprintf("%d in %s · fits about %d in total (now %d across %d pools): %s for PHP ÷ %d MB per worker",
						t.MaxChildren, t.Name, b.FitTotal, children, pools, mb(max(b.RAMMB-b.ReserveMB-b.DBMB-b.RedisMB, 0)), b.PrivateMB)
				},
				Default: func(ctx context.Context, env *check.Env, _ Target) string {
					t, ok := biggest(env)
					if !ok {
						return ""
					}
					// Pre-fill a size that fits when the box is over its budget:
					// this template's share of the total, per domain.
					b := budget(ctx, env)
					children, _ := box.FPMPoolChildren(env.Sys)
					if b.FitTotal > 0 && children > b.FitTotal && t.Ceiling() > 0 {
						fit := max(b.FitTotal*t.Ceiling()/children/len(t.Domains), 2)
						if fit < t.MaxChildren {
							return strconv.Itoa(fit)
						}
					}
					return strconv.Itoa(t.MaxChildren)
				}},
			spareField("start", "pm.start_servers", "dynamic only: workers at start (between min and max spare). Empty = derived.", func(t box.FPMTemplate) int { return t.Start }),
			spareField("min_spare", "pm.min_spare_servers", "dynamic only: idle workers always kept ready. Empty = derived (a fifth of max_children).", func(t box.FPMTemplate) int { return t.MinSpare }),
			spareField("max_spare", "pm.max_spare_servers", "dynamic only: idle workers above this are stopped. Empty = derived (half of max_children).", func(t box.FPMTemplate) int { return t.MaxSpare }),
		},
		Recheck: []string{"memory.fpm-ceiling", "services.core"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			t, ok := templateByName(env, v["template"])
			if !ok {
				return nil, fmt.Errorf("no domain uses a template %q", v["template"])
			}
			if !exists(env, t.Path) {
				return nil, fmt.Errorf("%s is missing — the domains on it cannot be rebuilt until it exists (hs op fpm-profile makes the profile templates)", t.Path)
			}
			children := v.Int("max_children")
			p := fpmProfile{Name: t.Name, Mode: v["mode"], MaxChildren: strconv.Itoa(children), IdleTimeout: t.IdleTimeout}
			if p.IdleTimeout == "" {
				p.IdleTimeout = "10s"
			}
			if p.Mode == "dynamic" {
				minS, maxS, start := v.Int("min_spare"), v.Int("max_spare"), v.Int("start")
				if minS == 0 {
					minS = max(children/5, 1)
				}
				if maxS == 0 {
					maxS = max(children/2, minS)
				}
				if start == 0 {
					start = minS + (maxS-minS)/2
				}
				if minS > maxS || maxS > children || start < minS || start > maxS {
					return nil, fmt.Errorf("php-fpm requires min_spare (%d) ≤ start (%d) ≤ max_spare (%d) ≤ max_children (%d)", minS, start, maxS, children)
				}
				p.Start, p.MinSpare, p.MaxSpare = strconv.Itoa(start), strconv.Itoa(minS), strconv.Itoa(maxS)
			}
			if t.Mode == p.Mode && t.MaxChildren == children &&
				(p.Mode != "dynamic" || (strconv.Itoa(t.Start) == p.Start && strconv.Itoa(t.MinSpare) == p.MinSpare && strconv.Itoa(t.MaxSpare) == p.MaxSpare)) {
				return nil, nil
			}
			b := budget(ctx, env)
			total, _ := box.FPMPoolChildren(env.Sys)
			after := total - t.Ceiling() + children*len(t.Domains)
			why := fmt.Sprintf("%s: %s → pm %s, max_children %d\nceiling for its %s: %d → %d workers; box-wide %d → %d (%s)\n%s",
				t.Name, fpmSummary(t), p.Mode, children, plural(len(t.Domains), "domain", "domains"), t.Ceiling(), children*len(t.Domains), total, after,
				map[bool]string{true: "fits", false: "still over the budget"}[after <= b.FitTotal || b.FitTotal == 0], b.explain())
			if t.Name == "default" || strings.HasPrefix(t.Name, "PHP-") {
				why += "\nNOTE: " + t.Name + " is Hestia's own template — a Hestia upgrade may rewrite it; hs op fpm-profile makes templates of ours to switch the domains to"
			}
			steps := poolSteps(why, p, t.Path)
			var pools []string
			userPool := readFile(env, "/usr/local/hestia/conf/hestia.conf")
			for _, d := range t.Domains {
				steps = append(steps, Step{Why: "re-render " + d.Name + "'s pool from the template", Argv: []string{bin + "v-rebuild-web-domain", d.User, d.Name, "no"}})
				name := d.Name
				if strings.Contains(userPool, "WEB_BACKEND_POOL='user'") {
					name = d.User
				}
				pools = append(pools, name)
			}
			versions := []string{t.Version}
			if t.Version == "" {
				versions = fpmVersions(env) // Hestia's default PHP: reload every version rather than guess
			}
			for _, ver := range versions {
				var files []string
				for _, p := range pools {
					files = append(files, "/etc/php/"+ver+"/fpm/pool.d/"+p+".conf")
				}
				steps = append(steps,
					Step{Why: "php-fpm " + ver + " accepts the pools", Argv: []string{"php-fpm" + ver, "-t"}},
					Step{Why: "graceful reload: running requests finish", Argv: []string{"systemctl", "try-reload-or-restart", "php" + ver + "-fpm"}},
					Step{Why: "the rebuilt pools", Argv: append([]string{"grep", "-H", "^pm"}, files...)})
			}
			return steps, nil
		},
	})
}

// plural mirrors box.plural for form labels.
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
