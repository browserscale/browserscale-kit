package input

import (
	"encoding/csv"
	"os"
	"strings"
)

// Columns splits each non-empty line of path on sep into trimmed fields.
// It's the generic delimited-list loader: the kit does not decide what the
// columns mean — credentials, tokens, coordinates, proxy+label pairs — that
// is entirely the module's call.
//
//	rows, _ := input.Columns("list.txt", ":")
//	// rows[0] == []string{"a@b.com", "hunter2"}  // module decides the meaning
func Columns(path, sep string) ([][]string, error) {
	lines, err := NonEmptyLines(path)
	if err != nil {
		return nil, err
	}
	if sep == "" {
		sep = ":"
	}
	out := make([][]string, 0, len(lines))
	for _, l := range lines {
		parts := strings.Split(l, sep)
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		out = append(out, parts)
	}
	return out, nil
}

// CSV parses path as RFC-4180 CSV and returns every row. Ragged rows are
// allowed (FieldsPerRecord = -1) so partially-filled files still load, and
// leading whitespace is trimmed. Header handling, if any, is left to the
// module.
func CSV(path string) ([][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	return r.ReadAll()
}
