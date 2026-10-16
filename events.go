package kit

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event is one thing that happened in a run. Runs write them to
// runs/<run>/events.jsonl, and with -events to stdout as well, one JSON
// object per line.
//
// Every event of a job carries the Task it belongs to, in a program with
// named tasks.
//
// Types:
//
//	run      the run started; Text is its name, Data has threads (in all;
//	         also as workers), dir and the ends, each with name, tone and
//	         task; for a program with named tasks tasks, each with name,
//	         threads and ends; browsers when the program limits them
//	job      a job (or a retry of it) started on Worker, its task's thread
//	browser  the job got a browser; Data has session and proxy (host:port)
//	step     the job entered Step
//	watch    a watched condition matched during Step; Text is where it goes
//	log      the job logged Text
//	save     the job saved a record to the file in Text
//	wait     the job waits, Text says for what
//	retry    the job is tried again, Text says why
//	end      the job ended as End, of Tone; Text is the detail, Data has
//	         seconds and, for errors, code and shot
//	held     the browser of a job keeps running; Data has session and grpc
//	stop     the run stops early, Text says why
//	done     the run is over; Data has ends, the count of each, and for
//	         named tasks tasks, each one's counts by name
type Event struct {
	At     int64          `json:"at"`
	Type   string         `json:"type"`
	Task   string         `json:"task,omitempty"`
	Job    int            `json:"job,omitempty"`
	Worker int            `json:"worker,omitempty"`
	Try    int            `json:"try,omitempty"`
	Step   string         `json:"step,omitempty"`
	End    string         `json:"end,omitempty"`
	Tone   Tone           `json:"tone,omitempty"`
	Text   string         `json:"text,omitempty"`
	Data   map[string]any `json:"data,omitempty"`
}

type events struct {
	mu     sync.Mutex
	file   *os.File
	status *csv.Writer
	sfile  *os.File
	stream io.Writer
	human  io.Writer
}

func openEvents(runDir string, stream, human io.Writer) (*events, error) {
	f, err := os.OpenFile(filepath.Join(runDir, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	sp := filepath.Join(runDir, "status.csv")
	_, statErr := os.Stat(sp)
	sf, err := os.OpenFile(sp, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		f.Close()
		return nil, err
	}
	ev := &events{file: f, sfile: sf, status: csv.NewWriter(sf), stream: stream, human: human}
	if statErr != nil {
		_ = ev.status.Write([]string{"job", "end", "tone", "step", "detail", "tries", "seconds", "at", "task"})
		ev.status.Flush()
	}
	return ev, nil
}

func (ev *events) emit(e Event) {
	if e.At == 0 {
		e.At = time.Now().UnixMilli()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	line = append(line, '\n')
	ev.mu.Lock()
	defer ev.mu.Unlock()
	_, _ = ev.file.Write(line)
	if ev.stream != nil {
		_, _ = ev.stream.Write(line)
	}
	if ev.human != nil {
		if s := humanLine(e); s != "" {
			fmt.Fprintln(ev.human, s)
		}
	}
}

func (ev *events) row(e Event, tries int, seconds float64) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	_ = ev.status.Write([]string{
		strconv.Itoa(e.Job), e.End, string(e.Tone), e.Step, e.Text,
		strconv.Itoa(tries), strconv.FormatFloat(seconds, 'f', 1, 64),
		time.UnixMilli(e.At).Format(time.RFC3339), e.Task,
	})
	ev.status.Flush()
}

func (ev *events) close() {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.status.Flush()
	_ = ev.sfile.Close()
	_ = ev.file.Close()
}

func humanLine(e Event) string {
	at := time.UnixMilli(e.At).Format("15:04:05")
	job := ""
	if e.Task != "" {
		job = " " + e.Task
	}
	if e.Job > 0 {
		job += fmt.Sprintf(" #%d", e.Job)
		if e.Try > 1 {
			job += fmt.Sprintf(" (try %d)", e.Try)
		}
	}
	var s string
	switch e.Type {
	case "run":
		s = "run " + e.Text
	case "job":
		return ""
	case "browser":
		s = fmt.Sprintf("browser %v", e.Data["session"])
		if p, ok := e.Data["proxy"]; ok {
			s += fmt.Sprintf(" via %v", p)
		}
	case "step":
		s = "→ " + e.Step
	case "watch":
		s = fmt.Sprintf("watch matched in %s → %s", e.Step, e.Text)
	case "end":
		s = "end " + e.End
		if e.Step != "" {
			s += " at " + e.Step
		}
		if e.Text != "" {
			s += ": " + e.Text
		}
	case "done":
		counts, _ := e.Data["ends"].(map[string]int)
		names := slices.Sorted(maps.Keys(counts))
		slices.SortStableFunc(names, func(a, b string) int { return counts[b] - counts[a] })
		var parts []string
		for _, n := range names {
			parts = append(parts, fmt.Sprintf("%d %s", counts[n], n))
		}
		s = "done"
		if len(parts) > 0 {
			s += ": " + strings.Join(parts, ", ")
		}
	default:
		s = e.Type
		if e.Text != "" {
			s += " " + e.Text
		}
	}
	return at + job + " " + s
}
