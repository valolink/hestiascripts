// Package ioc is the list of indicators of compromise hs looks for: built in
// (indicators.txt, embedded) plus box-local additions in
// /etc/hs/indicators.local.
package ioc

import (
	_ "embed"
	"strings"
)

//go:embed indicators.txt
var builtin string

const LocalPath = "/etc/hs/indicators.local"

type Indicator struct {
	Type, Value, Note string
}

type List []Indicator

func Parse(text string) List {
	var out List
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		out = append(out, Indicator{Type: f[0], Value: f[1], Note: strings.Join(f[2:], " ")})
	}
	return out
}

// Load returns the built-in list plus local additions (local may be "").
func Load(local string) List { return append(Parse(builtin), Parse(local)...) }

func (l List) Of(t string) List {
	var out List
	for _, i := range l {
		if i.Type == t {
			out = append(out, i)
		}
	}
	return out
}

func (l List) Values(t string) []string {
	var out []string
	for _, i := range l.Of(t) {
		out = append(out, i.Value)
	}
	return out
}

// Hash returns the note for a known sha256, "" if unknown.
func (l List) Hash(sum string) string {
	for _, i := range l.Of("sha256") {
		if strings.EqualFold(i.Value, sum) {
			if i.Note == "" {
				return "known malware"
			}
			return i.Note
		}
	}
	return ""
}
