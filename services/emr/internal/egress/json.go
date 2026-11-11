package egress

import (
	"encoding/json"
	"io"
)

// DecodeJSON reads one JSON document of at most MaxBody bytes and nesting
// at most maxDepth into v. Size and depth are checked before anything is
// decoded, so a refused document leaves v as it was.
func DecodeJSON(r io.Reader, v any, maxDepth int) error {
	b, err := io.ReadAll(io.LimitReader(r, MaxBody+1))
	if len(b) > MaxBody {
		return ErrTooLarge
	}
	if err != nil {
		return ErrUnreadable
	}
	if depth(b) > maxDepth {
		return ErrTooDeep
	}
	if !json.Valid(b) {
		return ErrUnreadable
	}
	if err := json.Unmarshal(b, v); err != nil {
		return ErrUnreadable
	}
	return nil
}

// depth is the deepest nesting of objects and arrays in b, ignoring
// brackets inside strings.
func depth(b []byte) int {
	d, most := 0, 0
	inString, escaped := false, false
	for _, c := range b {
		switch {
		case inString && escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case inString && c == '"':
			inString = false
		case inString:
		case c == '"':
			inString = true
		case c == '{' || c == '[':
			d++
			if d > most {
				most = d
			}
		case c == '}' || c == ']':
			d--
		}
	}
	return most
}
