// Package op is what replaces run.sh's menus: an operation asks for a few
// values in a form (each showing the current state and a suggestion), then
// plans plain commands from them — shown in full before anything runs, run
// by `hs op`, logged like every action. Same shape as a fix, plus inputs.
package op

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/plan"
)

type Kind int

const (
	Text Kind = iota
	Number
	Choice
	Bool
	// Secret: typed masked, never put in argv, the log or a transcript. It
	// reaches `hs op` as the environment variable SecretEnv(key); plans refer
	// to it as "@env:NAME" (hs conf set) or "$NAME" inside sh -c.
	Secret
)

// SecretEnv is the environment variable a secret field travels in.
func SecretEnv(key string) string {
	return "HS_SECRET_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(key))
}

// Field is one input on the form.
type Field struct {
	Key   string
	Label string
	Kind  Kind
	Help  string // one line under the field: what it means, units
	// Current describes today's value on the box ("maxmemory 512M").
	Current func(ctx context.Context, env *check.Env, t Target) string
	// Default is the suggested value the form starts with.
	Default func(ctx context.Context, env *check.Env, t Target) string
	// Choices for Kind Choice (value, label).
	Choices  func(ctx context.Context, env *check.Env, t Target) [][2]string
	Min, Max int                  // Number bounds (0,0 = none)
	Pattern  *regexp.Regexp       // Text validation
	Validate func(v string) error // extra validation
	Optional bool
}

type Risk int

const (
	ReadOnly Risk = iota
	Change
	Destructive
)

type Target struct {
	Domain *hestia.Domain // nil = the box
}

type Op struct {
	ID      string
	Title   string
	Section string // tab it lives in; "sites" for per-site operations
	Site    bool
	WPOnly  bool // site operation that needs WordPress
	Risk    Risk
	// RiskOf, when set, decides the risk from the values (an overwrite
	// switch makes a clone destructive).
	RiskOf func(v Values) Risk
	// Interactive: the steps get the terminal (ncdu); the TUI hands it over.
	Interactive bool
	Note        string
	How         string
	Undo        string
	Fields      []Field
	Recheck     []string
	// Resolves: the check whose findings this operation addresses; enter
	// on such a finding opens the form. Applies narrows it to some findings.
	Resolves string
	Applies  func(r check.Result) bool
	Plan     func(ctx context.Context, env *check.Env, t Target, v Values) ([]plan.Step, error)
}

type Values map[string]string

func (v Values) Int(k string) int   { n, _ := strconv.Atoi(v[k]); return n }
func (v Values) Bool(k string) bool { return v[k] == "yes" || v[k] == "true" }

var all []Op

func register(o Op) { all = append(all, o) }

// Register adds an operation (tests; the catalogue uses register).
func Register(o Op) { register(o) }

func All() []Op { return all }

func ByID(id string) (Op, bool) {
	for _, o := range all {
		if o.ID == id {
			return o, true
		}
	}
	return Op{}, false
}

func ForSection(section string) []Op {
	var out []Op
	for _, o := range all {
		if o.Section == section && !o.Site {
			out = append(out, o)
		}
	}
	return out
}

// ForResult: the operations that address a finding.
func ForResult(r check.Result) []Op {
	var out []Op
	for _, o := range all {
		if o.Resolves == r.Check && (o.Applies == nil || o.Applies(r)) {
			out = append(out, o)
		}
	}
	return out
}

// summaryHas is an Applies helper.
func summaryHas(sub string) func(check.Result) bool {
	return func(r check.Result) bool { return strings.Contains(r.Summary, sub) }
}

// RiskFor is the risk of running o with v.
func (o Op) RiskFor(v Values) Risk {
	if o.RiskOf != nil {
		return o.RiskOf(v)
	}
	return o.Risk
}

func ForSite(wp bool) []Op {
	var out []Op
	for _, o := range all {
		if o.Site && (!o.WPOnly || wp) {
			out = append(out, o)
		}
	}
	return out
}

// Check validates one value against its field.
func (f Field) Check(v string) error {
	if v == "" {
		if f.Optional {
			return nil
		}
		return fmt.Errorf("%s is required", f.Label)
	}
	switch f.Kind {
	case Number:
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s must be a whole number", f.Label)
		}
		if (f.Min != 0 || f.Max != 0) && (n < f.Min || n > f.Max) {
			return fmt.Errorf("%s must be %d–%d", f.Label, f.Min, f.Max)
		}
	case Bool:
		if v != "yes" && v != "no" {
			return fmt.Errorf("%s: yes or no", f.Label)
		}
	}
	if f.Pattern != nil && !f.Pattern.MatchString(v) {
		return fmt.Errorf("%s: not a valid value", f.Label)
	}
	if f.Validate != nil {
		return f.Validate(v)
	}
	return nil
}

// Validate checks every field; Choice values must be one of the choices.
func (o Op) Validate(ctx context.Context, env *check.Env, t Target, v Values) error {
	for _, f := range o.Fields {
		if err := f.Check(v[f.Key]); err != nil {
			return err
		}
		if f.Kind == Choice && f.Choices != nil && v[f.Key] != "" {
			ok := false
			for _, c := range f.Choices(ctx, env, t) {
				if c[0] == v[f.Key] {
					ok = true
				}
			}
			if !ok {
				return fmt.Errorf("%s: %q is not one of the choices", f.Label, v[f.Key])
			}
		}
	}
	return nil
}

// Defaults fills a Values with every field's suggestion.
func (o Op) Defaults(ctx context.Context, env *check.Env, t Target) Values {
	v := Values{}
	for _, f := range o.Fields {
		if f.Default != nil {
			v[f.Key] = f.Default(ctx, env, t)
		}
		switch {
		case v[f.Key] != "":
		case f.Kind == Choice && f.Choices != nil:
			if cs := f.Choices(ctx, env, t); len(cs) > 0 {
				v[f.Key] = cs[0][0]
			}
		case f.Kind == Bool:
			v[f.Key] = "no"
		}
	}
	return v
}

// Args renders values as `key=value` arguments for `hs op`.
func (v Values) Args(o Op) []string {
	var out []string
	for _, f := range o.Fields {
		if f.Kind != Secret {
			out = append(out, f.Key+"="+v[f.Key])
		}
	}
	return out
}

// Env is the environment carrying o's secret fields.
func (v Values) Env(o Op) []string {
	var out []string
	for _, f := range o.Fields {
		if f.Kind == Secret && v[f.Key] != "" {
			out = append(out, SecretEnv(f.Key)+"="+v[f.Key])
		}
	}
	return out
}

// ParseArgs reads `key=value` arguments.
func ParseArgs(args []string) (Values, error) {
	v := Values{}
	for _, a := range args {
		k, val, ok := strings.Cut(a, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("expected key=value, got %q", a)
		}
		v[k] = val
	}
	return v, nil
}

// IDs is for help output.
func IDs() []string {
	var ids []string
	for _, o := range all {
		ids = append(ids, o.ID)
	}
	sort.Strings(ids)
	return ids
}
