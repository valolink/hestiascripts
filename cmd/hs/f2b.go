package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/valolink/hestiascripts/internal/f2b"
)

func cmdF2B(ctx context.Context, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: hs f2b repair | reload | restart")
		return 2
	}
	var err error
	switch args[0] {
	case "repair":
		ips, _ := exec.Command("hostname", "-I").Output()
		err = f2b.Repair(ctx, os.Stdout, f2b.Exec, strings.Join(strings.Fields(string(ips)), " "))
	case "reload":
		err = f2b.Reload(ctx, os.Stdout, false)
	case "restart":
		err = f2b.Reload(ctx, os.Stdout, true)
	default:
		fmt.Fprintln(os.Stderr, "usage: hs f2b repair | reload | restart")
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hs f2b:", err)
		return 1
	}
	return 0
}
