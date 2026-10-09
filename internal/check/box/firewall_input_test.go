package box

import (
	"context"
	"strings"
	"testing"

	. "github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/fwgen"
	"github.com/valolink/hestiascripts/internal/sys"
)

const hestiaRulesConf = `RULE='2' ACTION='ACCEPT' PROTOCOL='TCP' PORT='8083' IP='0.0.0.0/0' COMMENT='HESTIA' SUSPENDED='no'
RULE='9' ACTION='ACCEPT' PROTOCOL='TCP' PORT='80,443' IP='0.0.0.0/0' COMMENT='WEB' SUSPENDED='no'
RULE='10' ACTION='ACCEPT' PROTOCOL='TCP' PORT='22' IP='0.0.0.0/0' COMMENT='SSH' SUSPENDED='no'
`

// kuumalahde on 2026-10-09: 45 lines of fail2ban and hs chains, INPUT ACCEPT,
// an empty saved file. The old check said "45 rules loaded".
const kuumalahdeOpen = `-P INPUT ACCEPT
-P FORWARD ACCEPT
-P OUTPUT ACCEPT
-N HS_BLOCK
-N f2b-wordpress
-A INPUT -j HS_BLOCK
-A INPUT -p tcp -m multiport --dports 0:65535 -j f2b-wordpress
-A HS_BLOCK -s 195.72.61.165/32 -j DROP
-A f2b-wordpress -j RETURN
`

const healthy = `-P INPUT DROP
-P FORWARD ACCEPT
-P OUTPUT ACCEPT
-A INPUT -i lo -j ACCEPT
-A INPUT -m state --state RELATED,ESTABLISHED -j ACCEPT
-A INPUT -p tcp -m tcp --dport 22 -j ACCEPT
-A INPUT -p tcp -m multiport --dports 80,443 -j ACCEPT
`

func fwFake(live, saved, rules string) *sys.Fake {
	return &sys.Fake{
		Commands: map[string]bool{"iptables": true},
		Files:    map[string]string{savedRules: saved, fwgen.RulesConf: rules},
		Cmds: map[string]sys.FakeCmd{
			"iptables -S": {Out: live},
			"systemctl list-unit-files --no-legend hestia-iptables.service": {Out: "hestia-iptables.service enabled enabled\n"},
			"systemctl is-active --quiet hestia-iptables":                   {},
			"systemctl is-active hestia-iptables":                           {Out: "active\n"},
			"sshd -T":                                                       {Out: "port 22\npasswordauthentication no\n"},
		},
	}
}

func summaries(rs []Result) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.State.String()+": "+r.Summary)
	}
	return strings.Join(s, "\n")
}

func TestFirewallOpenPolicyIsNotOK(t *testing.T) {
	rs := checkFirewall(context.Background(), env(fwFake(kuumalahdeOpen, "", hestiaRulesConf)))
	got := summaries(rs)
	if !strings.Contains(got, "fail: INPUT policy is ACCEPT") {
		t.Errorf("open INPUT not reported:\n%s", got)
	}
	if !strings.Contains(got, "fail: the saved firewall opens every port at the next reboot") {
		t.Errorf("empty saved file not reported:\n%s", got)
	}
}

func TestFirewallHealthy(t *testing.T) {
	saved := "*filter\n:INPUT DROP [0:0]\n:FORWARD ACCEPT [0:0]\nCOMMIT\n"
	rs := checkFirewall(context.Background(), env(fwFake(healthy, saved, hestiaRulesConf)))
	if len(rs) != 1 || rs[0].State != OK {
		t.Fatalf("healthy box:\n%s", summaries(rs))
	}
}

func TestFirewallFinalDropCountsAsClosed(t *testing.T) {
	live := strings.Replace(healthy, "-P INPUT DROP", "-P INPUT ACCEPT", 1) + "-A INPUT -j DROP\n"
	if _, closed := inputClosed(live); !closed {
		t.Error("an unconditional final DROP closes INPUT")
	}
}

// The lockout guard: rules.conf without SSH means v-update-firewall (every
// fix this check suggests) would close port 22.
func TestFirewallWarnsBeforeALockout(t *testing.T) {
	noSSH := strings.Replace(hestiaRulesConf, "PORT='22'", "PORT='2222'", 1)
	saved := "*filter\n:INPUT DROP [0:0]\nCOMMIT\n"
	rs := checkFirewall(context.Background(), env(fwFake(healthy, saved, noSSH)))
	if got := summaries(rs); !strings.Contains(got, "would lock everyone out") {
		t.Errorf("missing SSH rule not reported:\n%s", got)
	}
	// Ranges and lists count.
	ranged := strings.Replace(hestiaRulesConf, "PORT='22'", "PORT='20-23,8080'", 1)
	if !rulesAccept(fwgen.ParseRules(ranged), "22") {
		t.Error("a 20-23 range covers 22")
	}
}
