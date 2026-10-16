package kit

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
)

// saver appends records to files in the run's folder. A file's columns are
// fixed by the first record written to it.
type saver struct {
	mu    sync.Mutex
	dir   string
	heads map[string][]string
}

func newSaver(dir string) *saver { return &saver{dir: dir, heads: map[string][]string{}} }

func (sv *saver) save(name string, rec any) (string, error) {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." {
		return "", fmt.Errorf("kit: Save needs a file name")
	}
	if filepath.Ext(name) == "" {
		name += ".csv"
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	path := filepath.Join(sv.dir, name)
	info, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if !strings.EqualFold(filepath.Ext(name), ".csv") {
		_, err = fmt.Fprintln(f, rec)
		return name, err
	}
	w := csv.NewWriter(f)
	head, row := columns(rec, sv.heads[name])
	if head != nil {
		if _, seen := sv.heads[name]; !seen {
			if statErr != nil || info.Size() == 0 {
				if err := w.Write(head); err != nil {
					return "", err
				}
			}
			sv.heads[name] = head
		}
	}
	if err := w.Write(row); err != nil {
		return "", err
	}
	w.Flush()
	return name, w.Error()
}

// columns turns a record into a CSV row. Structs give their exported fields,
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
	case reflect.Slice:
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
