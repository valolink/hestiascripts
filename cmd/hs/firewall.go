package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/valolink/hestiascripts/internal/conf"
	"github.com/valolink/hestiascripts/internal/fwgen"
	"github.com/valolink/hestiascripts/internal/ioc"
)

// blockHosts: the indicator list's hosts (built in + /etc/hs/indicators.local)
// plus any in firewall.conf.
func blockHosts(c fwgen.Config) []string {
	local, _ := os.ReadFile(ioc.LocalPath)
	seen := map[string]bool{}
	var out []string
	for _, h := range append(ioc.Load(string(local)).Values("host"), c.BlockHosts...) {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

func firewallScript(repo string) string {
	cb, _ := os.ReadFile(fwgen.ConfPath)
	rb, _ := os.ReadFile(fwgen.RulesConf)
	c := fwgen.ParseConfig(string(cb))
	c.BlockHosts = blockHosts(c)
	return fwgen.Script(c, fwgen.ParseRules(string(rb)))
}

// cmdFirewall: `hs firewall show` prints the generated rules; `apply` writes
// them, hooks them into Hestia's custom.sh and the boot unit, and runs them.
func cmdFirewall(repo string, args []string) int {
	if len(args) != 1 || (args[0] != "show" && args[0] != "apply") {
		fmt.Fprintln(os.Stderr, "usage: hs firewall show | apply")
		return 2
	}
	script := firewallScript(repo)
	if args[0] == "show" {
		fmt.Print(script)
		return 0
	}
	run := func(name string, a ...string) error {
		c := exec.Command(name, a...)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		return c.Run()
	}
	report := func(path, bak string, err error) bool {
		switch {
		case err != nil:
			fmt.Printf("✗ %s: %v\n", path, err)
			return false
		case bak != "":
			fmt.Printf("✓ %s (previous kept: %s)\n", path, bak)
		default:
			fmt.Printf("✓ %s\n", path)
		}
		return true
	}
	os.MkdirAll(filepath.Dir(fwgen.ScriptPath), 0o700)
	bak, err := conf.Write(fwgen.ScriptPath, script, 0o700)
	if !report(fwgen.ScriptPath, bak, err) {
		return 1
	}
	os.Chmod(fwgen.ScriptPath, 0o700)

	old, _ := os.ReadFile(fwgen.CustomSh)
	bak, err = conf.Write(fwgen.CustomSh, fwgen.WithCustomBlock(string(old)), 0o700)
	if !report(fwgen.CustomSh+" (runs it on every v-update-firewall)", bak, err) {
		return 1
	}
	// Hestia runs custom.sh only when executable: add the owner's x bit,
	// never widen what is there (it is 700 on boxes where someone set it).
	if st, err := os.Stat(fwgen.CustomSh); err == nil && st.Mode().Perm()&0o100 == 0 {
		os.Chmod(fwgen.CustomSh, st.Mode().Perm()|0o100)
	}

	bak, err = conf.Write(fwgen.UnitPath, fwgen.Unit(), 0o644)
	if !report(fwgen.UnitPath+" (runs it at boot, after Hestia restores IPv4)", bak, err) {
		return 1
	}
	if run("systemctl", "daemon-reload") != nil || run("systemctl", "enable", "--quiet", "hs-firewall.service") != nil {
		fmt.Println("✗ could not enable hs-firewall.service")
		return 1
	}
	fmt.Println("→ applying now")
	if err := run(fwgen.ScriptPath); err != nil {
		fmt.Println("✗ the rules did not apply:", err)
		return 1
	}
	fmt.Println("\nnow in force:")
	run("sh", "-c", "iptables -S HS_OUT 2>/dev/null; ip6tables -S HS_IN6 2>/dev/null; ip6tables -S HS_OUT 2>/dev/null")
	return 0
}
