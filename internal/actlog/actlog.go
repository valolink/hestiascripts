// Package actlog records every action hs runs: an append-only index
// (who, when, what, the exact command, exit code) plus a full transcript of
// what the terminal printed. Root-only — transcripts can hold hostnames,
// paths, repository names and whatever a script chose to print.
//
// Layout (HS_LOG_DIR, default /var/log/hs):
//
//	actions.jsonl                one JSON event per line: "start", then "end"
//	runs/2026-09-24/<id>.log     the transcript
//
// A start event is written before the command runs, so a killed or crashed
// run still appears — with no end, it reads as "interrupted".
package actlog

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

func Dir() string {
	if d := os.Getenv("HS_LOG_DIR"); d != "" {
		return d
	}
	return "/var/log/hs"
}

func indexPath() string { return filepath.Join(Dir(), "actions.jsonl") }

type Event struct {
	Kind     string    `json:"kind"` // start | end
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	Action   string    `json:"action,omitempty"`
	Title    string    `json:"title,omitempty"`
	Target   string    `json:"target,omitempty"`
	Plan     string    `json:"plan,omitempty"`
	Mode     string    `json:"mode,omitempty"`
	Operator string    `json:"operator,omitempty"`
	Log      string    `json:"log,omitempty"` // transcript path
	Exit     *int      `json:"exit,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// Run is a start event merged with its end (if any).
type Run struct {
	Event
	Ended    time.Time
	Exit     *int
	EndError string
}

func (r Run) Interrupted() bool { return r.Ended.IsZero() }

var mu sync.Mutex

func appendEvent(e Event) error {
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(indexPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(e)
	_, err = f.Write(append(b, '\n'))
	return err
}

// operator names the person behind root: SUDO_USER, else the login name,
// else the SSH client address.
func operator() string {
	if u := os.Getenv("SUDO_USER"); u != "" {
		return u
	}
	name := "root"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if c := os.Getenv("SSH_CLIENT"); c != "" {
		if f := splitFirst(c); f != "" {
			return name + "@" + f
		}
	}
	return name
}

func splitFirst(s string) string {
	for i, c := range s {
		if c == ' ' {
			return s[:i]
		}
	}
	return s
}

// Start records the run and returns it with the transcript path to write to.
func Start(actionID, title, target, plan, mode string) (Event, error) {
	b := make([]byte, 4)
	rand.Read(b)
	now := time.Now()
	id := now.Format("20060102-150405") + "-" + hex.EncodeToString(b)
	dir := filepath.Join(Dir(), "runs", now.Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Event{}, err
	}
	e := Event{Kind: "start", ID: id, At: now, Action: actionID, Title: title, Target: target, Plan: plan,
		Mode: mode, Operator: operator(), Log: filepath.Join(dir, id+".log")}
	return e, appendEvent(e)
}

func End(start Event, exit int, errText string) error {
	code := exit
	return appendEvent(Event{Kind: "end", ID: start.ID, At: time.Now(), Exit: &code, Error: errText})
}

// Transcript opens the run's transcript for writing (root-only).
func Transcript(e Event) (*os.File, error) {
	return os.OpenFile(e.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// Runs returns every run, newest first.
func Runs() ([]Run, error) {
	f, err := os.Open(indexPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	byID := map[string]*Run{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Kind {
		case "start":
			byID[e.ID] = &Run{Event: e}
			order = append(order, e.ID)
		case "end":
			if r := byID[e.ID]; r != nil {
				r.Ended, r.Exit, r.EndError = e.At, e.Exit, e.Error
			}
		}
	}
	out := make([]Run, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, sc.Err()
}
