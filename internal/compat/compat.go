// Package compat reproduces the JSON that EngineLink already parses from
// v-server-health and v-server-setup-status, built from the same probes the
// checks use. The shapes are frozen: EngineLink reads the final stdout line
// as JSON (server/utils/serverMonitor.ts), so field names, types and nesting
// must match the bash scripts exactly. Richer data belongs in hs check --json.
package compat

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/box"
	"github.com/valolink/hestiascripts/internal/hestia"
)

type BackupEntry struct {
	User     string `json:"user"`
	AgeHours int    `json:"ageHours"`
	Newest   string `json:"newest"`
}

type Disk struct {
	UsedPct int    `json:"usedPct"`
	Info    string `json:"info"`
}

type Health struct {
	Backups   []BackupEntry     `json:"backups"`
	Services  map[string]string `json:"services"`
	MailQueue int               `json:"mailQueue"`
	Disk      Disk              `json:"disk"`
}

func HealthReport(ctx context.Context, env *check.Env) Health {
	now := env.Sys.Now()
	h := Health{Backups: []BackupEntry{}, Services: map[string]string{}}
	for _, bi := range box.BackupAges(ctx, env) {
		if bi.Nightly.IsZero() {
			continue // the bash script omits users with no artifact
		}
		h.Backups = append(h.Backups, BackupEntry{
			User:     bi.User,
			AgeHours: int(now.Sub(bi.Nightly) / time.Hour),
			Newest:   bi.Nightly.Local().Format("2006-01-02 15:04"),
		})
	}
	for _, svc := range box.CoreServices(ctx, env) {
		out, _ := env.Sys.Run(ctx, "systemctl", "is-active", svc)
		state := strings.TrimSpace(out)
		if state == "" {
			state = "unknown"
		}
		h.Services[svc] = state
	}
	h.MailQueue, _ = box.MailQueue(ctx, env)
	h.Disk = rootDisk(ctx, env)
	return h
}

func rootDisk(ctx context.Context, env *check.Env) Disk {
	var d Disk
	if out, err := env.Sys.Run(ctx, "df", "-P", "/"); err == nil {
		if l := lines(out); len(l) >= 2 {
			if f := strings.Fields(l[1]); len(f) >= 5 {
				d.UsedPct, _ = strconv.Atoi(strings.TrimSuffix(f[4], "%"))
			}
		}
	}
	if out, err := env.Sys.Run(ctx, "df", "-h", "/"); err == nil {
		if l := lines(out); len(l) >= 2 {
			if f := strings.Fields(l[1]); len(f) >= 3 {
				d.Info = f[2] + " / " + f[1]
			}
		}
	}
	return d
}

type SetupStatus struct {
	Streamer struct {
		Running  bool `json:"running"`
		VScripts int  `json:"vScripts"`
	} `json:"streamer"`
	WPCLI struct {
		Installed bool   `json:"installed"`
		Version   string `json:"version"`
	} `json:"wpcli"`
	Redis struct {
		Installed bool            `json:"installed"`
		Running   bool            `json:"running"`
		MaxMemory int64           `json:"maxmemory"`
		Policy    string          `json:"policy"`
		PHPExt    map[string]bool `json:"phpExt"`
	} `json:"redis"`
	Fail2ban struct {
		Installed bool   `json:"installed"`
		Running   bool   `json:"running"`
		WPJail    string `json:"wpJail"`
	} `json:"fail2ban"`
	Maldet struct {
		Installed bool   `json:"installed"`
		LastScan  string `json:"lastScan"`
	} `json:"maldet"`
	Netdata struct {
		Installed bool `json:"installed"`
		Running   bool `json:"running"`
		Tuned     bool `json:"tuned"`
	} `json:"netdata"`
	Security struct {
		Swap               bool   `json:"swap"`
		SSHKeyOnly         bool   `json:"sshKeyOnly"`
		UnattendedUpgrades string `json:"unattendedUpgrades"`
	} `json:"security"`
	SMTP struct {
		Relay string `json:"relay"`
	} `json:"smtp"`
	PHPFpmProfiles struct {
		Installed int `json:"installed"`
		Missing   int `json:"missing"`
	} `json:"phpFpmProfiles"`
	Opcache struct {
		OK             int `json:"ok"`
		NeedsAttention int `json:"needsAttention"`
	} `json:"opcache"`
	MariaDB struct {
		BufferPool string `json:"bufferPool"`
	} `json:"mariadb"`
	NginxTemplates struct {
		WPSecure bool `json:"wpSecure"`
		WPRocket bool `json:"wpRocket"`
	} `json:"nginxTemplates"`
	Hestia struct {
		Installed string `json:"installed"`
		Latest    string `json:"latest"`
	} `json:"hestia"`
	ServiceDrift []box.Drift `json:"serviceDrift"`
	Restic       struct {
		SystemRepo    bool `json:"systemRepo"`
		UsersWithKeys int  `json:"usersWithKeys"`
		UsersTotal    int  `json:"usersTotal"`
	} `json:"restic"`
	Disk Disk `json:"disk"`
}

func SetupStatusReport(ctx context.Context, env *check.Env) SetupStatus {
	s := env.Sys
	var st SetupStatus
	active := func(unit string) bool {
		_, err := s.Run(ctx, "systemctl", "is-active", "--quiet", unit)
		return err == nil
	}

	st.Streamer.Running = active("hestia-streamer")
	_, st.Streamer.VScripts = box.LinkedScripts(env)

	st.WPCLI.Version = box.WPCLIVersion(ctx, env)
	st.WPCLI.Installed = s.Have("wp")

	r := box.Redis(ctx, env)
	st.Redis.Installed, st.Redis.Running, st.Redis.MaxMemory, st.Redis.Policy = r.Installed, r.Running, r.MaxMemory, r.Policy
	st.Redis.PHPExt = map[string]bool{}
	for _, v := range box.PHPVersions(s) {
		st.Redis.PHPExt[v] = box.PHPHasRedis(ctx, env, v)
	}

	st.Fail2ban.WPJail = "missing"
	if s.Have("fail2ban-client") {
		st.Fail2ban.Installed = true
		if active("fail2ban") {
			st.Fail2ban.Running = true
			jctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_, err := s.Run(jctx, "fail2ban-client", "status", "wordpress")
			switch {
			case jctx.Err() == context.DeadlineExceeded:
				st.Fail2ban.WPJail = "unresponsive"
			case err == nil:
				st.Fail2ban.WPJail = "active"
			}
			cancel()
		}
	}

	if _, err := s.Stat("/usr/local/maldetect/maldet"); err == nil {
		st.Maldet.Installed = true
		st.Maldet.LastScan, _ = box.MaldetLastScan(env)
	}

	if s.Have("netdata") {
		st.Netdata.Installed = true
		st.Netdata.Running = active("netdata")
		st.Netdata.Tuned = box.NetdataTuned(env)
	}

	if out, err := s.Run(ctx, "swapon", "--show", "--noheadings"); err == nil && strings.TrimSpace(out) != "" {
		st.Security.Swap = true
	}
	if out, err := s.Run(ctx, "sshd", "-T"); err == nil {
		st.Security.SSHKeyOnly = strings.Contains(out, "\npasswordauthentication no") || strings.HasPrefix(out, "passwordauthentication no")
	}
	st.Security.UnattendedUpgrades = box.UnattendedScope(ctx, env)

	st.SMTP.Relay = box.Relay(ctx, env)
	st.PHPFpmProfiles.Installed, st.PHPFpmProfiles.Missing = box.FPMProfiles(env)
	for _, v := range box.PHPVersions(s) {
		ok, present := box.OpcacheOK(env, v)
		switch {
		case ok:
			st.Opcache.OK++
		case present:
			st.Opcache.NeedsAttention++
		}
	}
	st.MariaDB.BufferPool = box.MariaDBBufferConf(env)
	st.NginxTemplates.WPSecure = box.WPSecureBuilt(env)
	st.NginxTemplates.WPRocket = box.WPRocketInstalled(env)

	st.Hestia.Installed = hestia.Conf(s)["VERSION"]
	st.Hestia.Latest = box.HestiaLatest(ctx, env)
	st.ServiceDrift = box.ServiceDrift(ctx, env)
	if st.ServiceDrift == nil {
		st.ServiceDrift = []box.Drift{}
	}

	st.Restic.SystemRepo = hestia.ResticRepo(s) != ""
	for _, u := range hestia.Users(s) {
		st.Restic.UsersTotal++
		if _, err := s.Stat(hestia.UserResticKey(u)); err == nil {
			st.Restic.UsersWithKeys++
		}
	}
	st.Disk = rootDisk(ctx, env)
	return st
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
