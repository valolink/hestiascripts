package logsrc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTailFileReadsOnlyTheEnd(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.log")
	var b strings.Builder
	for i := 0; i < 200000; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	os.WriteFile(p, []byte(b.String()), 0o600)
	lines, err := tailFile(p, 3)
	if err != nil || len(lines) != 3 || lines[2] != "line 199999" || lines[0] != "line 199997" {
		t.Fatalf("%v %v", lines, err)
	}
	short := filepath.Join(t.TempDir(), "short.log")
	os.WriteFile(short, []byte("a\nb\n"), 0o600)
	if lines, _ := tailFile(short, 10); len(lines) != 2 || lines[0] != "a" {
		t.Errorf("short file: %v", lines)
	}
}

func TestItoa(t *testing.T) {
	if itoa(5000) != "5000" {
		t.Errorf("%q", itoa(5000))
	}
}
