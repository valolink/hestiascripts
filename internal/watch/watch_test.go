package watch

import (
	"strings"
	"testing"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
)

func res(chk, subj, sum string, st check.State) check.Result {
	return check.Result{Check: chk, Subject: subj, Summary: sum, State: st, CheckedAt: time.Now()}
}

func TestBaselineNewAndDailyRealert(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	first := Findings([]check.Result{
		res("ssh.private-keys", "/root/.ssh/id_valolink", "private key on the server", check.Fail),
		res("persist.cron", "", "3 cron entries, none reviewed yet", check.Warn),
		res("persist.iocs", "", "none of 29 indicators present", check.OK),
	}, now)
	if len(first) != 2 {
		t.Fatalf("OK results are not findings: %v", first)
	}
	st := Load(dir)
	st.Accept(first)
	st.Save(dir)

	st = Load(dir)
	second := Findings([]check.Result{
		res("ssh.private-keys", "/root/.ssh/id_valolink", "private key on the server", check.Fail),
		res("persist.cron", "", "4 cron entries, none reviewed yet", check.Warn), // same finding, new count
		res("persist.iocs", "/usr/lib/__hesti", "compromise indicator present: /usr/lib/__hesti", check.Fail),
	}, now)
	nw := st.New(second)
	if len(nw) != 1 || nw[0].Check != "persist.iocs" {
		t.Fatalf("new: %v", nw)
	}
	if len(st.Due(nw, now)) != 1 {
		t.Fatal("a new finding is due")
	}
	st.Alerted[nw[0].Key] = now
	if len(st.Due(nw, now.Add(time.Hour))) != 0 || len(st.Due(nw, now.Add(25*time.Hour))) != 1 {
		t.Error("re-alert at most once a day")
	}
	st.Forget(nil)
	if len(st.Alerted) != 0 {
		t.Error("a finding that is gone is forgotten, so a recurrence alerts at once")
	}
	subj, body := Mail("hzweb1", nw)
	if !strings.Contains(subj, "1 CRITICAL") || !strings.Contains(body, "/usr/lib/__hesti") {
		t.Errorf("%s\n%s", subj, body)
	}
	s := Summarise(nw, 2, "", now)
	if s.Critical != 1 || s.Warn != 0 || s.Accepted != 2 {
		t.Errorf("%+v", s)
	}
}
