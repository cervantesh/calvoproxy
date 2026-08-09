package compress

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// JSONTable rewrites a homogeneous JSON array of flat objects as a table.
//
// A 500-row array repeats every key 500 times. The keys carry no information
// after the first row, so hoisting them into a header is a pure restructuring:
// nothing is dropped, and the same values come back in the same order.
//
// It is the only engine here that changes the SHAPE of a result rather than its
// size, which is a different risk: the model has to read the new form. Two
// things address that — the output is a plain delimited table, which models read
// fluently, and it is introduced by a one-line header saying what it is.
//
// The conditions are deliberately strict. Anything that would need a judgement
// call is skipped instead: nested values, ragged keys, values containing the
// delimiter or a newline. An engine that is unsure must do nothing.
type JSONTable struct {
	// MinRows is the array length below which the header costs more than the
	// repeated keys save.
	MinRows int
}

func (JSONTable) Name() string { return "json-table" }

// tableSeparator is a character that does not occur in normal prose or code as
// a field separator, so a value containing it is a signal to skip rather than to
// escape. Escaping would make the output ambiguous to read, which defeats the
// point of a form the model is supposed to understand at a glance.
const tableSeparator = " | "

func (j JSONTable) Apply(messages []Message) ([]Message, int) {
	minRows := j.MinRows
	if minRows < 3 {
		minRows = 3
	}

	out := clone(messages)
	saved := 0
	segments := make([]segment, 0, 4)
	for i, m := range out {
		segments = appendToolResultSegments(segments[:0], m, MaxSegmentBytes)
		for _, seg := range segments {
			table, converted := tabulate(seg.Text, minRows)
			if !converted || len(table) >= len(seg.Text) {
				continue
			}
			m = seg.replace(m, table)
			saved += len(seg.Text) - len(table)
		}
		out[i] = m
	}
	if saved == 0 {
		return messages, 0
	}
	return out, saved
}

// tabulate converts a JSON array of flat, uniform objects into a table. The
// second return is false whenever anything at all is unusual.
//
// It is ONE pass over the text. The previous version decoded the document into
// []map[string]json.RawMessage and then re-read the first object with a
// json.Decoder purely to recover its key order — Go map iteration order is
// random, and a table whose columns shuffle between runs is not deterministic.
// The map was doing nothing else: every value came straight back out as a cell.
// So the map is gone, and the column order is simply the order the keys are
// read in, which is what the source says.
//
// The scanner accepts exactly the shape this engine converts:
//
//	array  := '[' ( object ( ',' object )* )? ']'
//	object := '{' ( string ':' scalar ( ',' string ':' scalar )* )? '}'
//	scalar := string | number | 'true' | 'false' | 'null'
//
// Anything else — a nested object or array, a trailing comma, a byte after the
// closing bracket, a malformed escape — is refused rather than interpreted. It
// is deliberately no more permissive than encoding/json: this engine's whole
// claim is that it does not guess, and a scanner that accepted documents the
// standard library rejects would be guessing about what they meant.
//
// Cells are substrings of the input wherever possible, so a table of plain
// ASCII values allocates only its own output.
func tabulate(text string, minRows int) (string, bool) {
	src := strings.TrimSpace(text)
	if !strings.HasPrefix(src, "[") {
		return "", false
	}

	var (
		columns []string
		// column maps a decoded key to its position, so a key the header does
		// not have is caught without building a map per row.
		column = map[string]int{}
		// writtenIn[c] is the row that last wrote column c: comparing it with
		// the current row detects the same key twice in one object without
		// having to clear anything between rows.
		writtenIn []int
		cells     []string
		body      strings.Builder
		rows      int
	)

	i := skipSpace(src, 1)
	if i < len(src) && src[i] == ']' {
		return "", false // an empty array is never long enough to be a table
	}
	for {
		if i >= len(src) || src[i] != '{' {
			return "", false // not an object: this is not a table
		}
		i = skipSpace(src, i+1)
		written := 0
		for i < len(src) && src[i] != '}' {
			if written > 0 {
				if src[i] != ',' {
					return "", false
				}
				i = skipSpace(src, i+1)
			}
			if i >= len(src) || src[i] != '"' {
				return "", false
			}
			keyEnd, keyEscaped, keyWide, ok := scanString(src, i)
			if !ok {
				return "", false
			}
			key, ok := stringValue(src[i:keyEnd], keyEscaped, keyWide)
			if !ok {
				return "", false
			}
			i = skipSpace(src, keyEnd)
			if i >= len(src) || src[i] != ':' {
				return "", false
			}
			i = skipSpace(src, i+1)

			cell, next, ok := scanCell(src, i)
			if !ok {
				return "", false
			}
			i = skipSpace(src, next)

			at, known := column[key]
			switch {
			case rows == 0 && known:
				return "", false // duplicate key: which value would be the cell?
			case rows == 0:
				at = len(columns)
				column[key] = at
				columns = append(columns, key)
				writtenIn = append(writtenIn, -1)
				cells = append(cells, "")
			case !known:
				return "", false // a key the header does not have
			case writtenIn[at] == rows:
				return "", false // the same key twice in one object
			}
			writtenIn[at] = rows
			cells[at] = cell
			written++
		}
		if i >= len(src) {
			return "", false
		}
		i = skipSpace(src, i+1) // past '}'
		if written != len(columns) {
			return "", false // ragged: a missing key would become an empty cell
		}
		for c, cell := range cells {
			if c > 0 {
				body.WriteString(tableSeparator)
			}
			body.WriteString(cell)
		}
		body.WriteString("\n")
		rows++

		if i < len(src) && src[i] == ',' {
			i = skipSpace(src, i+1)
			continue
		}
		break
	}
	// The closing bracket, and then nothing: a byte after it means the text was
	// never a lone JSON array, and encoding/json would have refused it too.
	if i >= len(src) || src[i] != ']' || skipSpace(src, i+1) != len(src) {
		return "", false
	}
	if rows < minRows || len(columns) == 0 {
		return "", false
	}

	header := fmt.Sprintf("[table of %d rows · columns: %s]\n", rows, strings.Join(columns, ", "))
	names := strings.Join(columns, tableSeparator)

	var b strings.Builder
	b.Grow(len(header) + len(names) + 1 + body.Len())
	b.WriteString(header)
	b.WriteString(names)
	b.WriteString("\n")
	b.WriteString(body.String())
	return b.String(), true
}

func skipSpace(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// scanCell reads one value and renders it as a cell. It refuses anything that
// is not a scalar, and anything a scalar cell could not carry unambiguously.
func scanCell(s string, i int) (cell string, next int, ok bool) {
	if i >= len(s) {
		return "", 0, false
	}
	switch s[i] {
	case '{', '[':
		return "", 0, false // nested value: flattening it would lose structure
	case '"':
		end, escaped, wide, ok := scanString(s, i)
		if !ok {
			return "", 0, false
		}
		text, ok := stringValue(s[i:end], escaped, wide)
		if !ok {
			return "", 0, false
		}
		// Only a string can carry either of these, and either one would make
		// the table ambiguous to read.
		if strings.Contains(text, "\n") || strings.Contains(text, tableSeparator) {
			return "", 0, false
		}
		return text, end, true
	case 't':
		return literal(s, i, "true")
	case 'f':
		return literal(s, i, "false")
	case 'n':
		return literal(s, i, "null")
	}
	end, ok := scanNumber(s, i)
	if !ok {
		return "", 0, false
	}
	// A JSON number the platform cannot represent is refused rather than
	// rounded: a cell reading 1e+308 where the source said 1e400 is a wrong
	// answer, and the contract here is to skip what cannot be carried.
	if _, err := strconv.ParseFloat(s[i:end], 64); err != nil {
		return "", 0, false
	}
	return s[i:end], end, true
}

func literal(s string, i int, want string) (string, int, bool) {
	if !strings.HasPrefix(s[i:], want) {
		return "", 0, false
	}
	return want, i + len(want), true
}

// scanString finds the end of a JSON string, applying encoding/json's rules: a
// raw byte below 0x20 and an unknown escape are both errors. It reports whether
// the string needs real decoding — an escape, or a byte that might be invalid
// UTF-8 — so that the common case can stay a substring of the input.
func scanString(s string, i int) (end int, escaped, wide, ok bool) {
	for j := i + 1; j < len(s); {
		c := s[j]
		switch {
		case c == '"':
			return j + 1, escaped, wide, true
		case c == '\\':
			escaped = true
			j++
			if j >= len(s) {
				return 0, false, false, false
			}
			switch s[j] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				j++
			case 'u':
				if j+4 >= len(s) {
					return 0, false, false, false
				}
				for k := 1; k <= 4; k++ {
					if !isHex(s[j+k]) {
						return 0, false, false, false
					}
				}
				j += 5
			default:
				return 0, false, false, false
			}
		case c < 0x20:
			return 0, false, false, false
		default:
			if c >= 0x80 {
				wide = true
			}
			j++
		}
	}
	return 0, false, false, false
}

// stringValue is the text of a scanned JSON string, quotes included in raw.
// Pure-ASCII strings with no escape are returned as a substring of the input;
// anything else goes through encoding/json, which is also what replaces invalid
// UTF-8 with the replacement character — a substitution a substring would
// silently skip, changing the bytes the model ends up seeing.
func stringValue(raw string, escaped, wide bool) (string, bool) {
	if !escaped && (!wide || utf8.ValidString(raw)) {
		return raw[1 : len(raw)-1], true
	}
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return "", false
	}
	return s, true
}

// scanNumber applies the JSON number grammar exactly: no leading plus, no
// leading zeroes, no bare decimal point, no hex.
func scanNumber(s string, i int) (int, bool) {
	j := i
	if j < len(s) && s[j] == '-' {
		j++
	}
	switch {
	case j >= len(s):
		return 0, false
	case s[j] == '0':
		j++
	case s[j] >= '1' && s[j] <= '9':
		for j < len(s) && isDigit(s[j]) {
			j++
		}
	default:
		return 0, false
	}
	if j < len(s) && s[j] == '.' {
		j++
		if j >= len(s) || !isDigit(s[j]) {
			return 0, false
		}
		for j < len(s) && isDigit(s[j]) {
			j++
		}
	}
	if j < len(s) && (s[j] == 'e' || s[j] == 'E') {
		j++
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j >= len(s) || !isDigit(s[j]) {
			return 0, false
		}
		for j < len(s) && isDigit(s[j]) {
			j++
		}
	}
	return j, true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHex(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
