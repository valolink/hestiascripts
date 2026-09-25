package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/valolink/hestiascripts/internal/conf"
)

const confUsage = `hs conf — edit configuration files the way operations do

  hs conf set [--section S] [--style space|ini|eq|shell] [--comment ';'] [--after KEY] FILE KEY VALUE [KEY VALUE ...]
      Set keys (inside [S] when given), keeping each existing line's own form;
      a missing key goes after its commented example or at the end of the
      section. Prints before → after; the previous file is kept under
      /var/lib/hs/backups. A VALUE of @env:NAME is read from that environment
      variable (secrets never appear in argv or the log).
  hs conf unset [--section S] [--comment ';'] FILE KEY [KEY ...]
      Comment the keys out.
  hs conf install [--mode 0644] SRC DST
      Write SRC's content to DST, printing the lines that change.
  hs conf get [--section S] FILE KEY
`

func cmdConf(args []string) int {
	if len(args) == 0 {
		fmt.Print(confUsage)
		return 2
	}
	sub, args := args[0], args[1:]
	var o conf.Opts
	mode := os.FileMode(0o644)
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if i+1 < len(args) {
			switch a {
			case "--section":
				o.Section = args[i+1]
				i++
				continue
			case "--style":
				o.Style = conf.Style(args[i+1])
				i++
				continue
			case "--comment":
				o.Comment = args[i+1]
				i++
				continue
			case "--after":
				o.After = args[i+1]
				i++
				continue
			case "--mode":
				n, err := strconv.ParseUint(args[i+1], 8, 32)
				if err != nil {
					fmt.Fprintln(os.Stderr, "hs conf: --mode is octal, e.g. 0644")
					return 2
				}
				mode = os.FileMode(n)
				i++
				continue
			}
		}
		rest = append(rest, a)
	}
	switch sub {
	case "set":
		if len(rest) < 3 || len(rest)%2 == 0 {
			fmt.Fprint(os.Stderr, confUsage)
			return 2
		}
		kv := append([]string{}, rest[1:]...)
		shown := append([]string{}, kv...)
		for i := 1; i < len(kv); i += 2 {
			if name, ok := strings.CutPrefix(kv[i], "@env:"); ok {
				v, set := os.LookupEnv(name)
				if !set {
					fmt.Fprintf(os.Stderr, "hs conf: %s is not set\n", name)
					return 2
				}
				kv[i], shown[i] = v, "(secret)"
			}
		}
		changes, bak, err := conf.SetFile(rest[0], o, kv...)
		if err != nil {
			fmt.Fprintln(os.Stderr, "hs conf:", err)
			return 1
		}
		// Never print a secret: report with the placeholder.
		for i := range changes {
			for j := 1; j < len(kv); j += 2 {
				if shown[j] != kv[j] {
					changes[i].Old = strings.ReplaceAll(changes[i].Old, kv[j], "(secret)")
					changes[i].New = strings.ReplaceAll(changes[i].New, kv[j], "(secret)")
				}
			}
		}
		fmt.Print(conf.Report(rest[0], changes, bak))
		return 0
	case "unset":
		if len(rest) < 2 {
			fmt.Fprint(os.Stderr, confUsage)
			return 2
		}
		changes, bak, err := conf.UnsetFile(rest[0], o, rest[1:]...)
		if err != nil {
			fmt.Fprintln(os.Stderr, "hs conf:", err)
			return 1
		}
		fmt.Print(conf.Report(rest[0], changes, bak))
		return 0
	case "install":
		if len(rest) != 2 {
			fmt.Fprint(os.Stderr, confUsage)
			return 2
		}
		removed, added, bak, err := conf.InstallFile(rest[0], rest[1], mode)
		if err != nil {
			fmt.Fprintln(os.Stderr, "hs conf:", err)
			return 1
		}
		fmt.Printf("%s:\n", rest[1])
		if len(removed)+len(added) == 0 {
			fmt.Println("  identical to " + rest[0] + " — nothing written")
			return 0
		}
		for _, l := range removed {
			fmt.Println("  - " + l)
		}
		for _, l := range added {
			fmt.Println("  + " + l)
		}
		if bak != "" {
			fmt.Printf("  previous file kept: %s\n  undo: cp -p %s %s\n", bak, bak, rest[1])
		} else {
			fmt.Println("  (new file)")
		}
		return 0
	case "get":
		if len(rest) != 2 {
			fmt.Fprint(os.Stderr, confUsage)
			return 2
		}
		b, err := os.ReadFile(rest[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, "hs conf:", err)
			return 1
		}
		v, ok := conf.Get(string(b), o, rest[1])
		if !ok {
			return 1
		}
		fmt.Println(v)
		return 0
	}
	fmt.Print(confUsage)
	return 2
}
