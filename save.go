package kit

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
)

// Format is how Save writes records to a file. CSV, Delimited, JSONL and
// Text are the kit's; a bot can bring its own, e.g. with FormatFunc.
type Format interface {
	// Writer is what writes a file's records, one after another. It's asked
	// once per file and run; fresh says the file is new or empty, e.g. for a
	// header line.
	Writer(fresh bool) RecordWriter
}

// RecordWriter writes one record to w.
type RecordWriter func(w io.Writer, record any) error

// FormatFunc is a Format that writes each record the same way, without a
// header.
type FormatFunc func(w io.Writer, record any) error

func (f FormatFunc) Writer(bool) RecordWriter { return RecordWriter(f) }

var (
	// CSV writes a row per record with a header line in a new file. Structs
	// give one column per exported field, named by a csv tag when there is
	// one; maps one per key, fixed by the first record; slices their
	// elements, without a header.
	CSV Format = Delimited(',', true)
	// JSONL writes each record as one line of JSON.
	JSONL Format = FormatFunc(writeJSON)
	// Text writes each record as a line of text as fmt prints it.
	Text Format = FormatFunc(writeText)
)

// Delimited writes records like CSV with another separator, such as '\t'
// or ':' for lines like email:password, and a header line only with header.
func Delimited(sep rune, header bool) Format { return delimited{sep: sep, header: header} }

type delimited struct {
	sep    rune
	header bool
}

func (d delimited) Writer(fresh bool) RecordWriter {
	var head []string
	known := false
	return func(w io.Writer, rec any) error {
		cw := csv.NewWriter(w)
		cw.Comma = d.sep
		h, row := columns(rec, head)
		if !known && h != nil {
			head, known = h, true
			if fresh && d.header {
				if err := cw.Write(h); err != nil {
					return err
				}
			}
		}
		if err := cw.Write(row); err != nil {
			return err
		}
		cw.Flush()
		return cw.Error()
	}
}

func writeJSON(w io.Writer, rec any) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = w.Write(append(line, '\n'))
	return err
}

func writeText(w io.Writer, rec any) error {
	if b, ok := rec.([]byte); ok {
		rec = string(b)
	}
	_, err := fmt.Fprintln(w, rec)
	return err
}

// formatOf is the format a file is written in when Save isn't told: by its
// extension, Text for one it doesn't know.
func formatOf(name string) Format {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".csv":
		return CSV
	case ".tsv":
		return Delimited('\t', true)
	case ".jsonl", ".ndjson":
		return JSONL
	}
	return Text
}

// saver writes the files in the run's folder. A file's format is fixed by
// the first record written to it, and its columns too until it's rewritten.
type saver struct {
	mu    sync.Mutex
	dir   string
	files map[string]*outFile
}

type outFile struct {
	format Format
	// w writes the file's next record; nil until one is written.
	w RecordWriter
}

func newSaver(dir string) *saver { return &saver{dir: dir, files: map[string]*outFile{}} }

// path is where a file of the run goes: name is relative to the run's
// folder and may have folders of its own. A name for records without an
// extension gets .csv.
func (sv *saver) path(name string, records bool) (string, string, error) {
	name = filepath.Clean(filepath.FromSlash(strings.TrimSpace(name)))
	if name == "." || name == "" {
		return "", "", errors.New("kit: a file needs a name")
	}
	if filepath.IsAbs(name) || filepath.VolumeName(name) != "" || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("kit: %s isn't in the run's folder", name)
	}
	if records && filepath.Ext(name) == "" {
		name += ".csv"
	}
	path := filepath.Join(sv.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", "", err
	}
	return filepath.ToSlash(name), path, nil
}

// file is what the saver knows of a file: its format is the one it was
// first written in, else the one given, else its extension's.
func (sv *saver) file(name string, format []Format) *outFile {
	of := sv.files[name]
	if of == nil {
		of = &outFile{format: formatOf(name)}
		if len(format) > 0 && format[0] != nil {
			of.format = format[0]
		}
		sv.files[name] = of
	}
	return of
}

func (sv *saver) save(name string, rec any, format ...Format) (string, error) {
	name, path, err := sv.path(name, true)
	if err != nil {
		return "", err
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	of := sv.file(name, format)
	if of.w == nil {
		info, err := os.Stat(path)
		of.w = of.format.Writer(err != nil || info.Size() == 0)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return name, of.w(f, rec)
}

// rewrite replaces a file with records, a slice, in a format as save would
// write them; later saves go on after them.
func (sv *saver) rewrite(name string, records any, format ...Format) (string, error) {
	name, path, err := sv.path(name, true)
	if err != nil {
		return "", err
	}
	v := reflect.ValueOf(records)
	if records != nil && v.Kind() != reflect.Slice && v.Kind() != reflect.Array {
		return "", fmt.Errorf("kit: Rewrite takes a slice of records, not a %T", records)
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	of := sv.file(name, format)
	w := of.format.Writer(true)
	err = replace(path, func(f io.Writer) error {
		if records == nil {
			return nil
		}
		for i := range v.Len() {
			if err := w(f, v.Index(i).Interface()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	of.w = w
	return name, nil
}

// write replaces a file with data; a later save goes on in the file's
// format as with a file it finds.
func (sv *saver) write(name string, data []byte) (string, error) {
	name, path, err := sv.path(name, false)
	if err != nil {
		return "", err
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	err = replace(path, func(f io.Writer) error {
		_, err := f.Write(data)
		return err
	})
	if err != nil {
		return "", err
	}
	if of := sv.files[name]; of != nil {
		of.w = nil
	}
	return name, nil
}

// replace writes a file whole: to a temporary file next to it that then
// takes its place, so a reader never sees half of it. Where the file can't
// be replaced, such as one another program holds open on Windows, it's
// written over in place.
func replace(path string, fill func(io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	err = fill(tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if os.Rename(tmp.Name(), path) == nil {
		return nil
	}
	_ = os.Remove(tmp.Name())
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	err = fill(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// columns turns a record into a row. Structs give their exported fields,
// named by a csv tag when there is one; maps give their keys. head, when
// known, fixes the order of a map's columns.
func columns(rec any, head []string) ([]string, []string) {
	v := reflect.ValueOf(rec)
	for v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		var names, row []string
		t := v.Type()
		for i := range t.NumField() {
			sf := t.Field(i)
			if !sf.IsExported() {
				continue
			}
			name := sf.Name
			if tag, ok := sf.Tag.Lookup("csv"); ok {
				if tag == "-" {
					continue
				}
				if tag != "" {
					name = tag
				}
			}
			names = append(names, name)
			row = append(row, cell(v.Field(i)))
		}
		return names, row
	case reflect.Map:
		if head == nil {
			for _, k := range v.MapKeys() {
				head = append(head, fmt.Sprint(k.Interface()))
			}
			slices.Sort(head)
		}
		row := make([]string, len(head))
		for _, k := range v.MapKeys() {
			if i := slices.Index(head, fmt.Sprint(k.Interface())); i >= 0 {
				row[i] = cell(v.MapIndex(k))
			}
		}
		return head, row
	case reflect.Slice, reflect.Array:
		row := make([]string, v.Len())
		for i := range row {
			row[i] = cell(v.Index(i))
		}
		return nil, row
	}
	return nil, []string{fmt.Sprint(rec)}
}

func cell(v reflect.Value) string {
	if !v.IsValid() {
		return ""
	}
	if s, ok := v.Interface().(fmt.Stringer); ok {
		return s.String()
	}
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	return fmt.Sprint(v.Interface())
}
