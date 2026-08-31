package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
)

// MarshalSorted serializes v as indented JSON with object keys sorted
// recursively, matching json.MarshalIndent(v, "", "  ") for readability but
// yielding deterministic bytes. encoding/json orders map keys randomly, which
// makes a downstream change-detection hash (the supervisor's gateway-config
// hash guard) fire on every poll even when nothing changed -- triggering a
// gateway reload each time.
//
// v must be the shape produced by json.Unmarshal into any (map[string]any /
// []any / scalars); typed maps/slices are not recursed into.
func MarshalSorted(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeSortedJSON(&buf, v, "", "  "); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeSortedJSON writes v with the given indentation; map keys are sorted.
func writeSortedJSON(w io.Writer, v any, indent, step string) error {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			io.WriteString(w, "{}")
			return nil
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		child := indent + step
		io.WriteString(w, "{\n")
		for i, k := range keys {
			if i > 0 {
				io.WriteString(w, ",\n")
			}
			io.WriteString(w, child)
			kb, err := json.Marshal(k)
			if err != nil {
				return err
			}
			w.Write(kb)
			io.WriteString(w, ": ")
			if err := writeSortedJSON(w, t[k], child, step); err != nil {
				return err
			}
		}
		io.WriteString(w, "\n"+indent+"}")
	case []any:
		if len(t) == 0 {
			io.WriteString(w, "[]")
			return nil
		}
		child := indent + step
		io.WriteString(w, "[\n")
		for i, item := range t {
			if i > 0 {
				io.WriteString(w, ",\n")
			}
			io.WriteString(w, child)
			if err := writeSortedJSON(w, item, child, step); err != nil {
				return err
			}
		}
		io.WriteString(w, "\n"+indent+"]")
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		w.Write(b)
	}
	return nil
}
