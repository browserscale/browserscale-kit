package configurator

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/browserscale/browserscale-kit/form"
)

type serveSettings struct {
	Threads   int    `json:"threads"`
	Mailbox   bool   `json:"mailbox"`
	Host      string `json:"host"`
	Password  string `json:"password"`
	Names     string `json:"names"`
	Countries []string
}

type served struct {
	in    *io.PipeWriter
	lines *bufio.Scanner
	done  chan error
}

func serve(t *testing.T, dir string) (*served, *serveSettings) {
	t.Helper()
	s := &serveSettings{}
	f := form.New("t", "Test", s)
	f.Int("threads", &s.Threads).Label("Threads").Default(2).Min(1).Max(50)
	f.Section("Mailbox")
	f.Bool("mailbox", &s.Mailbox).Label("Mailbox")
	f.String("host", &s.Host).Label("Host").Required().ShowWhen(func() bool { return s.Mailbox }).
		Validate(func(v string) error {
			if strings.Contains(v, " ") {
				return errors.New("no spaces")
			}
			return nil
		})
	f.String("password", &s.Password).Secret().ShowWhenTruthy("mailbox")
	f.File("names", &s.Names).Tag(form.TagLines).ValidateFile(func(path string) error {
		data, _ := os.ReadFile(path)
		if !strings.Contains(string(data), ",") {
			return errors.New("needs a comma")
		}
		return nil
	})
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Serve(f.Build(), filepath.Join(dir, "config.json"), dir, inR, outW)
		outW.Close()
	}()
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	t.Cleanup(func() { inW.Close() })
	return &served{in: inW, lines: sc, done: done}, s
}

func (s *served) next(t *testing.T) State {
	t.Helper()
	if !s.lines.Scan() {
		t.Fatalf("no state: %v", s.lines.Err())
	}
	var st State
	if err := json.Unmarshal(s.lines.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func (s *served) send(t *testing.T, id int, set map[string]any) State {
	t.Helper()
	return s.command(t, id, "set", set)
}

func (s *served) command(t *testing.T, id int, name string, values map[string]any) State {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"id": id, name: values})
	if _, err := s.in.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	for {
		st := s.next(t)
		if st.ID == id {
			return st
		}
	}
}

func field(st State, name string) FieldState {
	for _, f := range st.Fields {
		if f.Name == name {
			return f
		}
	}
	return FieldState{}
}

func TestServeEditsThroughTheBindings(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "names.txt"), []byte("a\nb\n\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "store.db"), nil, 0o644)
	srv, s := serve(t, dir)

	st := srv.next(t)
	threads := field(st, "threads")
	if st.Title != "Test" || threads.Value != float64(2) || *threads.Min != 1 || *threads.Max != 50 || threads.Section != "" {
		t.Fatalf("start: %+v", threads)
	}
	if h := field(st, "host"); h.Visible || !h.Conditional || h.Section != "Mailbox" || h.Error != "" {
		t.Fatalf("host before the mailbox: %+v", h)
	}
	if p := field(st, "password"); !p.Secret || p.DependsOn != "mailbox" {
		t.Fatalf("password: %+v", p)
	}
	if len(st.Files) != 1 || st.Files[0].Name != "names.txt" || st.Files[0].Lines != 2 || st.Problems != 0 {
		t.Fatalf("files: %+v, problems %d", st.Files, st.Problems)
	}

	st = srv.send(t, 1, map[string]any{"mailbox": true})
	if h := field(st, "host"); !h.Visible || h.Error != "required" || st.Problems != 1 {
		t.Fatalf("host with the mailbox: %+v, problems %d", h, st.Problems)
	}
	st = srv.send(t, 2, map[string]any{"host": "a b", "threads": 0})
	if field(st, "host").Error != "no spaces" || field(st, "threads").Error != "must be >= 1" || st.Problems != 2 {
		t.Fatalf("validators: %+v", st.Fields)
	}
	st = srv.send(t, 3, map[string]any{"host": "imap.example", "threads": 1.5, "names": "names.txt"})
	if field(st, "threads").Error == "" || field(st, "threads").Value != float64(0) || field(st, "names").Error != "needs a comma" {
		t.Fatalf("refused and file check: %+v", st.Fields)
	}
	st = srv.send(t, 4, map[string]any{"threads": 3, "nope": 1})
	if field(st, "threads").Error != "" || !strings.Contains(st.Error, "nope") {
		t.Fatalf("unknown field: %q", st.Error)
	}
	if s.Threads != 3 || s.Host != "imap.example" {
		t.Fatalf("bindings: %+v", s)
	}
	var saved map[string]any
	data, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if json.Unmarshal(data, &saved) != nil || saved["threads"] != float64(3) || saved["host"] != "imap.example" {
		t.Fatalf("saved: %s", data)
	}

	_ = os.WriteFile(filepath.Join(dir, "names.txt"), []byte("a,b\n"), 0o644)
	if st = srv.next(t); field(st, "names").Error != "" || st.Problems != 0 {
		t.Fatalf("after the file changed: %+v", field(st, "names"))
	}
	time.Sleep(10 * time.Millisecond)
	saved["threads"] = 9
	data, _ = json.Marshal(saved)
	_ = os.WriteFile(filepath.Join(dir, "config.json"), data, 0o644)
	if st = srv.next(t); field(st, "threads").Value != float64(9) {
		t.Fatalf("after the config changed: %+v", field(st, "threads"))
	}

	st = srv.command(t, 5, "try", map[string]any{"threads": 0, "mailbox": false})
	if !st.Try || field(st, "threads").Error != "must be >= 1" || field(st, "host").Visible || st.Problems != 1 {
		t.Fatalf("try: %+v", st)
	}
	st = srv.send(t, 6, map[string]any{})
	if st.Try || field(st, "threads").Value != float64(9) || !field(st, "host").Visible || s.Threads != 9 || !s.Mailbox {
		t.Fatalf("after the try: %+v", st.Fields)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "config.json"))
	if json.Unmarshal(data, &saved) != nil || saved["threads"] != float64(9) || saved["mailbox"] != true {
		t.Fatalf("saved after the try: %s", data)
	}

	srv.in.Close()
	if err := <-srv.done; err != nil {
		t.Fatal(err)
	}
}

// A bot serves its form with paths relative to the project, and the config
// file sits in the files directory.
func TestServeReloadsARelativeConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	_ = os.MkdirAll("data", 0o755)
	_ = os.WriteFile(filepath.Join("data", "config.json"), []byte(`{"threads":1}`), 0o644)
	s := &serveSettings{}
	f := form.New("t", "Test", s)
	f.Int("threads", &s.Threads)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Serve(f.Build(), filepath.Join("data", "config.json"), "data", inR, outW)
		outW.Close()
	}()
	sc := bufio.NewScanner(outR)
	srv := &served{in: inW, lines: sc, done: done}
	if st := srv.next(t); field(st, "threads").Value != float64(1) {
		t.Fatalf("start: %+v", field(st, "threads"))
	}
	time.Sleep(10 * time.Millisecond)
	_ = os.WriteFile(filepath.Join("data", "config.json"), []byte(`{"threads":7}`), 0o644)
	if st := srv.next(t); field(st, "threads").Value != float64(7) {
		t.Fatalf("after the config changed: %+v", field(st, "threads"))
	}
	inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
