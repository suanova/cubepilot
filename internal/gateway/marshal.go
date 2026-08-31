package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
)

// MarshalSorted serializes v as JSON with object keys sorted recursively, so
// the same semantic value always yields the same bytes. Go's encoding/json
// orders map keys randomly, which makes a downstream change-detection hash
// (the supervisor's gateway-config hash guard) fire on every poll even when
// nothing changed -- triggering a gateway reload each time.
//
// v must be the shape produced by json.Unmarshal into any (map[string]any /
// []any / scalars); typed maps/slices are not recursed into.
func MarshalSorted(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeSortedJSON(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeSortedJSON(w io.Writer, v any) error {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		io.WriteString(w, "{")
		for i, k := range keys {
			if i > 0 {
				io.WriteString(w, ",")
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return err
			}
			w.Write(kb)
			io.WriteString(w, ":")
			if err := writeSortedJSON(w, t[k]); err != nil {
				return err
			}
		}
		io.WriteString(w, "}")
	case []any:
		io.WriteString(w, "[")
		for i, item := range t {
			if i > 0 {
				io.WriteString(w, ",")
			}
			if err := writeSortedJSON(w, item); err != nil {
				return err
			}
		}
		io.WriteString(w, "]")
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		w.Write(b)
	}
	return nil
}
