package f2b

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepair(t *testing.T) {
	d := t.TempDir()
	Dir = d
	t.Setenv("HS_STATE_DIR", filepath.Join(d, "state"))
	os.MkdirAll(d+"/jail.d", 0o755)
	os.MkdirAll(d+"/filter.d", 0o755)
	os.WriteFile(d+"/jail.local", []byte("[sshd]\naction = %(action_mwl)s\n[recidive]\nenabled = true\n"), 0o644)
	os.WriteFile(d+"/"+Filter, []byte("[Definition]\nfailregex = ^<HOST> .* \"POST .*wp-login\\.php\n"), 0o644)
	os.WriteFile(d+"/"+Jail, []byte("[wordpress]\nenabled = true\n"), 0o644)

	tests := []string{"ERROR  Have not found any log file for recidive jail", "ERROR  Have not found any log file for dovecot jail", ""}
	run := func(context.Context, string, ...string) (string, error) {
		out := tests[0]
		tests = tests[1:]
		if out == "" {
			return "OK", nil
		}
		return out, errors.New("exit 255")
	}
	var w bytes.Buffer
	if err := Repair(context.Background(), &w, run, "10.0.0.5 2a01::1"); err != nil {
		t.Fatalf("%v\n%s", err, w.String())
	}
	read := func(p string) string { b, _ := os.ReadFile(d + "/" + p); return string(b) }
	if !strings.Contains(read("jail.local"), "action = %(action_)s") {
		t.Error("ban mails not silenced")
	}
	if !strings.Contains(read("jail.local"), "[recidive]\nenabled = false") {
		t.Errorf("recidive lives in jail.local and must be disabled there:\n%s", read("jail.local"))
	}
	if read("jail.d/zzz-disable-dovecot.conf") != "[dovecot]\nenabled = false\n" {
		t.Error("dovecot override missing")
	}
	if !strings.Contains(read(Filter), "xmlrpc") {
		t.Error("filter not patched")
	}
	if !strings.Contains(read(Jail), "ignoreip = 127.0.0.1/8 ::1 10.0.0.5 2a01::1") {
		t.Errorf("self-ban guard missing:\n%s", read(Jail))
	}
	if !strings.Contains(w.String(), "configuration OK") {
		t.Error(w.String())
	}
}
