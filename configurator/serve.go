package configurator

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/browserscale/browserscale-kit/form"
	"github.com/browserscale/browserscale-kit/proxy"
)

// State is the form as Serve writes it: every field with its value and
// whether it shows and passes, and the files there are to pick from.
type State struct {
	// ID is the command's this state answers, 0 when none does.
	ID     int          `json:"id,omitempty"`
	Title  string       `json:"title"`
	Module string       `json:"module"`
	Fields []FieldState `json:"fields"`
	Files  []FileState  `json:"files"`
	// Problems counts the visible fields that don't pass, what Validate
	// would report.
	Problems int `json:"problems"`
	// Error is what went wrong with the command or with reading or saving
	// the config file.
	Error string `json:"error,omitempty"`
	// Try marks the answer to a try command: the form with its values,
	// which it doesn't keep.
	Try bool `json:"try,omitempty"`
}

// FieldState is one field of State.
type FieldState struct {
	Name        string    `json:"name"`
	Kind        form.Kind `json:"kind"`
	Label       string    `json:"label"`
	Help        string    `json:"help,omitempty"`
	Section     string    `json:"section,omitempty"`
	Placeholder string    `json:"placeholder,omitempty"`
	Required    bool      `json:"required,omitempty"`
	Secret      bool      `json:"secret,omitempty"`
	// Visible is whether the field shows with the values as they are.
	Visible bool `json:"visible"`
	// Conditional fields show only while other fields are set a certain
	// way; DependsOn names the field when the condition is declarative.
	Conditional bool                `json:"conditional,omitempty"`
	DependsOn   string              `json:"dependsOn,omitempty"`
	Value       any                 `json:"value"`
	Default     any                 `json:"default,omitempty"`
	Options     []form.SelectOption `json:"options,omitempty"`
	FileTag     string              `json:"fileTag,omitempty"`
	// Proxies is what a proxies field needs of its proxies.
	Proxies *form.ProxyRule `json:"proxies,omitempty"`
	Min     *float64        `json:"min,omitempty"`
	Max     *float64        `json:"max,omitempty"`
	// Error is why the visible field doesn't pass, or why the last value
	// set on it was refused.
	Error string `json:"error,omitempty"`
	// Note is what a proxies field's value doesn't quite give the bot,
	// though it passes.
	Note string `json:"note,omitempty"`
}

// FileState is a file in the files directory.
type FileState struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// Lines counts the non-empty lines, -1 for a file too large to count.
	Lines    int   `json:"lines"`
	Modified int64 `json:"modified"`
}

type command struct {
	ID  int                        `json:"id"`
	Set map[string]json.RawMessage `json:"set"`
	Try map[string]json.RawMessage `json:"try"`
	bad bool
}

// countLimit is the largest file whose lines State counts.
const countLimit = 64 << 20

// Serve lets another program edit the settings, such as a settings screen:
// it reads commands from in and writes the form's State to out, one JSON
// object per line, until in closes.
//
// The state is written at the start, after every command, and whenever the
// files in filesDirectory or configPath change on disk; a changed config
// file is read again. Every change is saved to configPath at once, valid or
// not, so a run reads what the user sees; Problems says whether it passes.
//
// A command is {"id": 1, "set": {"field": value, ...}}. The values are
// written through the schema's bindings, so ShowWhen predicates and
// validators see them as they do in the terminal; the state written in
// reply carries the command's id. A file field's check runs again when its
// value or its file changes.
//
// {"id": 2, "try": {...}} answers with the form as it would be with those
// values over the current ones, such as one run's settings, and then puts
// the current ones back without saving.
func Serve(schema *form.Schema, configPath, filesDirectory string, in io.Reader, out io.Writer) error {
	schema.ApplyDefaults()
	s := &server{
		schema:  schema,
		config:  configPath,
		files:   filesDirectory,
		out:     json.NewEncoder(out),
		refused: map[string]string{},
		checks:  map[string]fileCheck{},
		counts:  map[string]fileCount{},
	}
	msg := s.load()

	cmds := make(chan command)
	read := make(chan error, 1)
	go func() {
		defer close(cmds)
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			var c command
			if err := json.Unmarshal(line, &c); err != nil {
				c = command{bad: true}
			}
			cmds <- c
		}
		read <- sc.Err()
	}()

	disk := s.disk()
	if err := s.write(0, msg, false); err != nil {
		return err
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case c, ok := <-cmds:
			if !ok {
				return <-read
			}
			if !c.bad && c.Try != nil {
				if err := s.try(c.ID, c.Try); err != nil {
					return err
				}
				continue
			}
			msg := "The command isn't valid JSON."
			if !c.bad {
				msg = s.set(c.Set)
			}
			disk = s.disk()
			if err := s.write(c.ID, msg, false); err != nil {
				return err
			}
		case <-tick.C:
			now := s.disk()
			if now == disk {
				continue
			}
			msg := ""
			if now.config != disk.config {
				msg = s.load()
			}
			disk = now
			if err := s.write(0, msg, false); err != nil {
				return err
			}
		}
	}
}

type server struct {
	schema *form.Schema
	config string
	files  string
	out    *json.Encoder
	// refused are the fields whose last value set couldn't be taken, with why.
	refused map[string]string
	checks  map[string]fileCheck
	counts  map[string]fileCount
}

type fileCheck struct {
	value string
	stamp stamp
	msg   string
	note  string
}

type fileCount struct {
	stamp stamp
	lines int
}

type stamp struct {
	size int64
	mod  time.Time
}

type disk struct{ config, files string }

func (s *server) load() string {
	err := s.schema.LoadJSON(s.config)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	return fmt.Sprintf("%s couldn't be read: %v", filepath.Base(s.config), err)
}

func (s *server) set(values map[string]json.RawMessage) string {
	msg := s.apply(values)
	if len(values) > 0 {
		if err := s.schema.SaveJSON(s.config); err != nil {
			return fmt.Sprintf("%s couldn't be saved: %v", filepath.Base(s.config), err)
		}
	}
	return msg
}

func (s *server) try(id int, values map[string]json.RawMessage) error {
	kept, refused := s.schema.ToMap(), maps.Clone(s.refused)
	msg := s.apply(values)
	err := s.write(id, msg, true)
	_ = s.schema.LoadMap(kept)
	s.refused = refused
	return err
}

func (s *server) apply(values map[string]json.RawMessage) string {
	var unknown []string
	for name, raw := range values {
		f := s.schema.Field(name)
		if f == nil {
			unknown = append(unknown, name)
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			s.refused[name] = "not a valid value"
			continue
		}
		if err := f.Set(v); err != nil {
			s.refused[name] = err.Error()
			continue
		}
		delete(s.refused, name)
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return "The form has no field " + strings.Join(unknown, ", ") + "."
	}
	return ""
}

func (s *server) write(id int, msg string, try bool) error {
	st := State{ID: id, Title: s.schema.DisplayName, Module: s.schema.Module, Error: msg, Fields: []FieldState{}, Files: s.list(), Try: try}
	for _, f := range s.schema.Fields {
		fs := FieldState{
			Name: f.Name, Kind: f.Kind, Label: f.Label, Help: f.Help, Section: f.Section, Placeholder: f.Placeholder,
			Required: f.Required, Secret: f.Secret, Visible: f.Visible(), Conditional: f.HasPredicate(),
			Value: f.Get(), Default: f.Default, Options: f.Options, FileTag: f.FileTag, Proxies: f.Proxies, Min: f.Min, Max: f.Max,
		}
		if w := f.VisibleWhenSpec(); w != nil {
			fs.DependsOn = w.Field
		}
		if msg, ok := s.refused[f.Name]; ok {
			fs.Error = msg
		} else if fs.Visible {
			fs.Error, fs.Note = s.check(f)
		}
		if fs.Error != "" && fs.Visible {
			st.Problems++
		}
		st.Fields = append(st.Fields, fs)
	}
	return s.out.Encode(st)
}

// check is f.Check and f.Note, kept for a file field until its value or file
// changes.
func (s *server) check(f *form.Field) (msg, note string) {
	if f.Kind != form.KindFile {
		return f.Check(s.files), ""
	}
	name, _ := f.Get().(string)
	st := s.stamp(name)
	if c, ok := s.checks[f.Name]; ok && c.value == name && c.stamp == st {
		return c.msg, c.note
	}
	msg = f.Check(s.files)
	if msg == "" {
		note = f.Note(s.files)
	}
	s.checks[f.Name] = fileCheck{value: name, stamp: st, msg: msg, note: note}
	return msg, note
}

func (s *server) path(name string) string {
	if proxy.IsRef(name) {
		p, _, _ := proxy.Path(s.files, name)
		return p
	}
	if name == "" || filepath.IsAbs(name) || s.files == "" {
		return name
	}
	return filepath.Join(s.files, name)
}

func (s *server) stamp(name string) stamp {
	if name == "" {
		return stamp{}
	}
	info, err := os.Stat(s.path(name))
	if err != nil {
		return stamp{size: -1}
	}
	return stamp{size: info.Size(), mod: info.ModTime()}
}

// list is the files to pick from: those directly in the files directory,
// without the config file, the kit's store and hidden ones.
func (s *server) list() []FileState {
	out := []FileState{}
	entries, err := os.ReadDir(s.files)
	if err != nil {
		return out
	}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "store.db") || samePath(s.path(name), s.config) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		st := stamp{size: info.Size(), mod: info.ModTime()}
		c, ok := s.counts[name]
		if !ok || c.stamp != st {
			c = fileCount{stamp: st, lines: countLines(s.path(name), st.size)}
			s.counts[name] = c
		}
		out = append(out, FileState{Name: name, Size: st.size, Lines: c.lines, Modified: st.mod.UnixMilli()})
	}
	return out
}

func (s *server) disk() disk {
	var b strings.Builder
	if entries, err := os.ReadDir(s.files); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil {
				fmt.Fprintf(&b, "%s %d %d\n", e.Name(), info.Size(), info.ModTime().UnixNano())
			}
		}
	}
	st := stamp{size: -1}
	if info, err := os.Stat(s.config); err == nil {
		st = stamp{size: info.Size(), mod: info.ModTime()}
	}
	return disk{config: fmt.Sprintf("%d %d", st.size, st.mod.UnixNano()), files: b.String()}
}

func samePath(a, b string) bool {
	x, err1 := filepath.Abs(a)
	y, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && strings.EqualFold(x, y)
}

func countLines(path string, size int64) int {
	if size > countLimit {
		return -1
	}
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) > 0 {
			n++
		}
	}
	return n
}
