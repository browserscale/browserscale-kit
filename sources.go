package kit

import (
	"fmt"
	"iter"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/browserscale/browserscale-kit/input"
)

// Times yields n jobs. item makes the i-th one, counting from 0; nil gives
// zero values.
func Times[J any](n int, item func(i int) J) iter.Seq[J] {
	return func(yield func(J) bool) {
		for i := range n {
			if !yield(make1(item, i)) {
				return
			}
		}
	}
}

// Forever yields jobs until the run is stopped.
func Forever[J any](item func(i int) J) iter.Seq[J] {
	return func(yield func(J) bool) {
		for i := 0; ; i++ {
			if !yield(make1(item, i)) {
				return
			}
		}
	}
}

// Until yields jobs until n of them ended with one of ends, or with a good
// end when none are given. It holds back while the jobs still running could
// make up the rest.
func Until[J any](r *Run, n int, item func(i int) J, ends ...string) iter.Seq[J] {
	reached := func() int {
		if len(ends) == 0 {
			return r.Ended(ToneGood)
		}
		return r.Count(ends...)
	}
	return func(yield func(J) bool) {
		for i := 0; ; {
			if r.Stopped() || reached() >= n {
				return
			}
			if reached()+r.Running() >= n {
				select {
				case <-r.c.changed():
				case <-r.c.ctx.Done():
					return
				}
				continue
			}
			if !yield(make1(item, i)) {
				return
			}
			i++
		}
	}
}

// Every yields a job, then the next one d later, until the run is stopped.
func Every[J any](r *Run, d time.Duration, item func(i int) J) iter.Seq[J] {
	return func(yield func(J) bool) {
		for i := 0; ; i++ {
			if i > 0 && !r.sleep(d) {
				return
			}
			if !yield(make1(item, i)) {
				return
			}
		}
	}
}

// At yields a job every day at each of the clock times, such as "09:00" and
// "18:30" in local time, until the run is stopped. A time that isn't hh:mm
// fails the run.
func At[J any](r *Run, times []string, item func(i int) J) iter.Seq[J] {
	var minutes []int
	for _, s := range times {
		t, err := time.Parse("15:04", strings.TrimSpace(s))
		if err != nil {
			r.Fail(fmt.Errorf("kit: %q isn't a time of day like 09:30", s))
			return func(func(J) bool) {}
		}
		minutes = append(minutes, t.Hour()*60+t.Minute())
	}
	if len(minutes) == 0 {
		r.Fail(fmt.Errorf("kit: At needs a time of day"))
		return func(func(J) bool) {}
	}
	return func(yield func(J) bool) {
		for i := 0; ; i++ {
			if !r.sleep(time.Until(nextAt(time.Now(), minutes))) {
				return
			}
			if !yield(make1(item, i)) {
				return
			}
		}
	}
}

// nextAt is the first of the day's minutes after now, today or tomorrow.
func nextAt(now time.Time, minutes []int) time.Time {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var best time.Time
	for _, m := range minutes {
		t := day.Add(time.Duration(m) * time.Minute)
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		if best.IsZero() || t.Before(best) {
			best = t
		}
	}
	return best
}

// Queue yields what the run's tasks Push under name, first the items given
// here, e.g. the first page of a crawl whose jobs push the pages they find.
// It ends when the run stops, or when nothing can come any more: the queue
// is empty, no job runs, and every other task is done or waits on an empty
// queue too. A task fed by a queue has as many threads as it says; the
// queue hands each item to one job.
func Queue[J any](r *Run, name string, first ...J) iter.Seq[J] {
	for _, item := range first {
		r.Push(name, item)
	}
	return func(yield func(J) bool) {
		for {
			v, ok := r.c.pop(name, r.t)
			if !ok {
				return
			}
			item, ok := v.(J)
			if !ok {
				r.Fail(fmt.Errorf("kit: the queue %q got a %T, not a %T", name, v, item))
				return
			}
			if !yield(item) {
				return
			}
		}
	}
}

// Lines yields the non-empty lines of a file in the data folder. A file that
// can't be read fails the run.
func Lines(r *Run, name string) iter.Seq[string] {
	lines, err := input.NonEmptyLines(r.File(name))
	if err != nil {
		r.Fail(fmt.Errorf("read %s: %w", name, err))
	}
	return func(yield func(string) bool) {
		for _, l := range lines {
			if !yield(l) {
				return
			}
		}
	}
}

// Rows yields the rows of a CSV file in the data folder. The first row names
// the columns; they fill a struct's fields by csv tag or field name, ignoring
// case, spaces and underscores, or a map[string]string. A file that can't be
// read fails the run.
func Rows[J any](r *Run, name string) iter.Seq[J] {
	rows, err := input.CSV(r.File(name))
	if err != nil {
		r.Fail(fmt.Errorf("read %s: %w", name, err))
	}
	return func(yield func(J) bool) {
		if len(rows) < 2 {
			return
		}
		head := rows[0]
		for n, row := range rows[1:] {
			var item J
			if err := fill(&item, head, row); err != nil {
				r.Fail(fmt.Errorf("%s line %d: %w", name, n+2, err))
				return
			}
			if !yield(item) {
				return
			}
		}
	}
}

func make1[J any](item func(int) J, i int) J {
	if item == nil {
		var zero J
		return zero
	}
	return item(i)
}

func columnKey(s string) string {
	return strings.ToLower(strings.NewReplacer(" ", "", "_", "", "-", "").Replace(strings.TrimSpace(s)))
}

func fill(dst any, head, row []string) error {
	v := reflect.ValueOf(dst).Elem()
	switch v.Kind() {
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String || v.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("rows fill structs or map[string]string, not %s", v.Type())
		}
		v.Set(reflect.MakeMap(v.Type()))
		for i, h := range head {
			if i < len(row) {
				v.SetMapIndex(reflect.ValueOf(h).Convert(v.Type().Key()), reflect.ValueOf(row[i]).Convert(v.Type().Elem()))
			}
		}
		return nil
	case reflect.Struct:
	default:
		return fmt.Errorf("rows fill structs or map[string]string, not %s", v.Type())
	}
	t := v.Type()
	fields := map[string]int{}
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
		fields[columnKey(name)] = i
	}
	for i, h := range head {
		idx, ok := fields[columnKey(h)]
		if !ok || i >= len(row) {
			continue
		}
		if err := setCell(v.Field(idx), row[i]); err != nil {
			return fmt.Errorf("column %s: %w", h, err)
		}
	}
	return nil
}

func setCell(f reflect.Value, s string) error {
	s = strings.TrimSpace(s)
	switch f.Kind() {
	case reflect.String:
		f.SetString(s)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if s == "" {
			return nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		f.SetInt(n)
	case reflect.Float32, reflect.Float64:
		if s == "" {
			return nil
		}
		x, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		f.SetFloat(x)
	case reflect.Bool:
		if s == "" {
			return nil
		}
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		f.SetBool(b)
	default:
		return fmt.Errorf("can't fill a %s", f.Type())
	}
	return nil
}
