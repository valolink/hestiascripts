package actlog

import (
	"os"
	"testing"
)

func TestStartEndRuns(t *testing.T) {
	t.Setenv("HS_LOG_DIR", t.TempDir())
	a, err := Start("site.info", "Site report", "a.fi", "/usr/local/hestia/bin/v-wp-info --user=u --domain=a.fi", "stream")
	if err != nil {
		t.Fatal(err)
	}
	f, _ := Transcript(a)
	f.WriteString("line one\n")
	f.Close()
	End(a, 0, "")
	b, _ := Start("site.update", "Update", "a.fi", "v-wp-update", "stream") // never ends: interrupted

	runs, err := Runs()
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs %v %v", runs, err)
	}
	if runs[0].ID != b.ID || !runs[0].Interrupted() {
		t.Errorf("newest first, interrupted: %+v", runs[0])
	}
	if runs[1].Exit == nil || *runs[1].Exit != 0 || runs[1].Operator == "" {
		t.Errorf("ended run: %+v", runs[1])
	}
	if st, err := os.Stat(a.Log); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("transcript perms: %v %v", st, err)
	}
	if st, _ := os.Stat(indexPath()); st.Mode().Perm() != 0o600 {
		t.Errorf("index perms %v", st.Mode())
	}
}
