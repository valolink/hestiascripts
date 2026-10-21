package fwgen

import (
	"strings"
	"testing"
)

const rules = `RULE='1' ACTION='ACCEPT' PROTOCOL='TCP' PORT='80,443' IP='0.0.0.0/0' COMMENT='WEB' SUSPENDED='no'
RULE='2' ACTION='ACCEPT' PROTOCOL='TCP' PORT='8083' IP='0.0.0.0/0' COMMENT='HESTIA' SUSPENDED='no'
RULE='3' ACTION='ACCEPT' PROTOCOL='ICMP' PORT='0' IP='0.0.0.0/0' COMMENT='PING' SUSPENDED='no'
RULE='10' ACTION='ACCEPT' PROTOCOL='TCP' PORT='22' IP='0.0.0.0/0' COMMENT='SSH' SUSPENDED='no'
RULE='11' ACTION='ACCEPT' PROTOCOL='TCP' PORT='19999' IP='0.0.0.0/0' COMMENT='Netdata' SUSPENDED='yes'
RULE='12' ACTION='ACCEPT' PROTOCOL='TCP' PORT='8091' IP='135.181.26.201' COMMENT='Nuxt_Streamer' SUSPENDED='no'
RULE='13' ACTION='ACCEPT' PROTOCOL='UDP' PORT='53' IP='0.0.0.0/0' COMMENT='DNS' SUSPENDED='no'
`

func TestIPv6MirrorsOnlyThePublicRules(t *testing.T) {
	s := Script(Config{IPv6Input: "mirror", Egress: "off", AdminV6: []string{"fd7a:115c:a1e0::/48"}, BlockHosts: []string{"195.72.61.165"}}, ParseRules(rules), "")
	for _, want := range []string{
		"ip6tables -w -A HS_IN6 -p tcp --dport 443 -j ACCEPT",
		"ip6tables -w -A HS_IN6 -p tcp --dport 8083 -j ACCEPT",
		"ip6tables -w -A HS_IN6 -p udp --dport 53 -j ACCEPT",
		"ip6tables -w -A HS_IN6 -p ipv6-icmp -j ACCEPT",
		"ip6tables -w -A HS_IN6 -s fd7a:115c:a1e0::/48 -j ACCEPT",
		"ip6tables -w -A HS_IN6 -j DROP",
		"iptables -w -A HS_OUT -d 195.72.61.165 -j DROP",
		"iptables -w -A HS_BLOCK -s 195.72.61.165 -j DROP",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q", want)
		}
	}
	// The drop chain is inserted after HS_IN6, so it lands first in INPUT.
	if strings.Index(s, "chain ip6tables HS_BLOCK INPUT") < strings.Index(s, "chain ip6tables HS_IN6 INPUT") {
		t.Error("HS_BLOCK must be inserted after HS_IN6 to be evaluated first")
	}
	// The streamer (IPv4-limited) and Netdata (suspended) must not open on IPv6.
	for _, bad := range []string{"--dport 8091", "--dport 19999", "LOG"} {
		if strings.Contains(s, bad) {
			t.Errorf("must not contain %q:\n%s", bad, s)
		}
	}
}

func TestEgressModes(t *testing.T) {
	log := Script(Config{IPv6Input: "off", Egress: "log"}, nil, "")
	if !strings.Contains(log, "--log-prefix 'hs-egress: ' --log-uid") || strings.Contains(log, "REJECT") {
		t.Errorf("log mode:\n%s", log)
	}
	if !strings.Contains(log, "unchain ip6tables HS_IN6 INPUT") {
		t.Error("IPv6 off must remove the chain")
	}
	drop := Script(Config{Egress: "drop", EgressExtra: []string{"3306"}}, nil, "")
	if !strings.Contains(drop, "-j REJECT") || !strings.Contains(drop, "--dports 22,23,53,80,443,465,587,3306") {
		t.Errorf("drop mode:\n%s", drop)
	}
}

func TestCustomBlockKeepsTheRest(t *testing.T) {
	orig := "#!/bin/bash\niptables -I OUTPUT -d 195.72.61.165 -j DROP\n"
	once := WithCustomBlock(orig)
	if !strings.HasPrefix(once, orig) || strings.Count(once, CustomBegin) != 1 {
		t.Fatalf("%s", once)
	}
	if WithCustomBlock(once) != once {
		t.Error("not idempotent")
	}
}

func TestConfig(t *testing.T) {
	c := ParseConfig("IPV6_INPUT='mirror'\nEGRESS=\"log\"\nEGRESS_EXTRA_PORTS=\"3306 43\"\nADMIN_V6='2a01::/32'\n# EGRESS=drop\n")
	if c.IPv6Input != "mirror" || c.Egress != "log" || strings.Join(c.EgressExtra, " ") != "3306 43" || c.AdminV6[0] != "2a01::/32" {
		t.Errorf("%+v", c)
	}
}

func TestPanelGate(t *testing.T) {
	c := ParseConfig("IPV6_INPUT='mirror'\nPANEL_GATE='on'\nPANEL_TTL='3600'\n")
	if c.PanelGate != "on" || c.PanelTTL != 3600 {
		t.Fatalf("%+v", c)
	}
	s := Script(c, ParseRules(rules), "8083")
	for _, want := range []string{
		"ipset -exist create hs-panel hash:ip family inet timeout 3600",
		"iptables -w -A HS_PANEL -i lo -j RETURN",
		"iptables -w -A HS_PANEL -p tcp --dport 8083 -m set --match-set hs-panel src -j RETURN",
		"iptables -w -A HS_PANEL -p tcp --dport 8083 -j DROP",
		"  ipset -exist create hs-panel6 hash:ip family inet6 timeout 3600",
		"  ip6tables -w -A HS_PANEL6 -p tcp --dport 8083 -j DROP",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q", want)
		}
	}
	// INPUT order after the run: HS_BLOCK, HS_PANEL6, HS_IN6 (each chain() inserts at 1).
	if !(strings.Index(s, "chain ip6tables HS_IN6 INPUT") < strings.Index(s, "chain ip6tables HS_PANEL6 INPUT") &&
		strings.Index(s, "chain ip6tables HS_PANEL6 INPUT") < strings.Index(s, "chain ip6tables HS_BLOCK INPUT")) {
		t.Error("HS_PANEL6 must be inserted after HS_IN6 (evaluated before it) and before HS_BLOCK")
	}
	off := Script(ParseConfig(""), ParseRules(rules), "")
	if !strings.Contains(off, "unchain iptables HS_PANEL INPUT") || strings.Contains(off, "ipset") {
		t.Errorf("gate off must remove the chains and touch no sets:\n%s", off)
	}
}

func TestPMAGate(t *testing.T) {
	if g := PMAGate(nil); !strings.Contains(g, "Require all denied") || !strings.Contains(g, "<Directory /usr/share/phpmyadmin>") {
		t.Errorf("empty: %s", g)
	}
	if g := PMAGate([]string{"203.0.113.7", "2001:db8::1"}); !strings.Contains(g, "Require ip 203.0.113.7 2001:db8::1") || strings.Contains(g, "denied") {
		t.Errorf("two: %s", g)
	}
}
