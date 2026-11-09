package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Page is one capability page: a Markdown file that starts with YAML front matter.
type Page struct {
	Path   string
	ID     string
	Fields map[string]string // top-level scalar fields of the front matter, unquoted
}

func (p *Page) Get(key string) string { return p.Fields[key] }

// Check is the page's check command, trimmed. "" means the page has no check field.
func (p *Page) Check() string { return strings.TrimSpace(p.Fields["check"]) }

// Host is where the check runs: "" for this machine, else an ssh host.
func (p *Page) Host() string { return normalizeHost(p.Fields["check_on"]) }

// IsDoc reports whether the page says there is nothing to check.
func (p *Page) IsDoc() bool { return strings.EqualFold(p.Check(), "none") }

func (p *Page) IsRetired() bool { return strings.EqualFold(p.Get("status"), "retired") }

var localNames = map[string]bool{"": true, "local": true, "localhost": true}

func normalizeHost(h string) string {
	h = strings.TrimSpace(h)
	if localNames[strings.ToLower(h)] {
		return ""
	}
	if name, err := os.Hostname(); err == nil {
		short, _, _ := strings.Cut(name, ".")
		if strings.EqualFold(h, name) || strings.EqualFold(h, short) {
			return ""
		}
	}
	return h
}

// LoadDir reads every *.md file in dir (not recursive) whose front matter has
// an id or a check field. Other files are skipped. Pages are sorted by id.
func LoadDir(dir string) ([]*Page, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return nil, err
	}
	var pages []*Page
	seen := map[string]string{}
	for _, path := range names {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		fields, ok := parseFrontMatter(data)
		if !ok {
			continue
		}
		// A capability page has an id or a check. Other front matter (domain
		// lists, indexes) is not a capability.
		_, hasID := fields["id"]
		_, hasCheck := fields["check"]
		if !hasID && !hasCheck {
			continue
		}
		id := fields["id"]
		if id == "" {
			id = strings.TrimSuffix(filepath.Base(path), ".md")
		}
		if prev, dup := seen[id]; dup {
			return nil, fmt.Errorf("id %q is in both %s and %s", id, prev, path)
		}
		seen[id] = path
		pages = append(pages, &Page{Path: path, ID: id, Fields: fields})
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].ID < pages[j].ID })
	return pages, nil
}

// splitFrontMatter returns the front matter lines (without the --- fences) and
// the byte offset where the body starts. ok is false when there is none.
func splitFrontMatter(data []byte) (lines []string, bodyStart int, ok bool) {
	if !bytes.HasPrefix(data, []byte("---\n")) && !bytes.HasPrefix(data, []byte("---\r\n")) {
		return nil, 0, false
	}
	pos := bytes.IndexByte(data, '\n') + 1
	for pos < len(data) {
		end := bytes.IndexByte(data[pos:], '\n')
		var line string
		next := len(data)
		if end >= 0 {
			line = string(data[pos : pos+end])
			next = pos + end + 1
		} else {
			line = string(data[pos:])
		}
		if strings.TrimRight(line, "\r") == "---" {
			return lines, next, true
		}
		lines = append(lines, strings.TrimRight(line, "\r"))
		pos = next
	}
	return nil, 0, false
}

func parseFrontMatter(data []byte) (map[string]string, bool) {
	lines, _, ok := splitFrontMatter(data)
	if !ok {
		return nil, false
	}
	fields := map[string]string{}
	for _, line := range lines {
		key, val, ok := topLevelKey(line)
		if !ok {
			continue
		}
		fields[key] = parseScalar(val)
	}
	return fields, true
}

// topLevelKey splits "key: value" when the line is not indented and not a comment.
func topLevelKey(line string) (key, val string, ok bool) {
	if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' || line[0] == '-' {
		return "", "", false
	}
	key, val, ok = strings.Cut(line, ":")
	if !ok || strings.ContainsAny(key, " \t\"'") {
		return "", "", false
	}
	return key, strings.TrimSpace(val), true
}

// parseScalar unquotes a one-line YAML scalar: plain, 'single' or "double" quoted.
// A plain scalar loses a trailing " # comment".
func parseScalar(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'':
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	case len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"':
		return unescapeDouble(s[1 : len(s)-1])
	}
	if i := strings.Index(s, " #"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

func unescapeDouble(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 == len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '0':
			b.WriteByte(0)
		case 'u', 'U', 'x':
			n := map[byte]int{'x': 2, 'u': 4, 'U': 8}[s[i]]
			if i+n < len(s) {
				if r, err := strconv.ParseUint(s[i+1:i+1+n], 16, 32); err == nil {
					b.WriteRune(rune(r))
					i += n
					continue
				}
			}
			b.WriteByte('\\')
			b.WriteByte(s[i])
		default: // \" \\ \/ and anything else: the character itself
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// quoteScalar writes s as a one-line YAML scalar, quoting only when needed.
func quoteScalar(s string) string {
	if s != "" && plainSafe(s) {
		return s
	}
	return quoteDouble(s)
}

// quoteDouble always writes a "double quoted" YAML scalar.
func quoteDouble(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func plainSafe(s string) bool {
	if strings.ContainsAny(s[:1], "!&*-?{}[],#|>@`\"'%: ") || strings.HasSuffix(s, " ") {
		return false
	}
	if strings.Contains(s, ": ") || strings.Contains(s, " #") || strings.ContainsAny(s, "\n\t\r\"'\\") {
		return false
	}
	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "on", "off", "null", "~":
		return false
	}
	return true
}

// Edit is one front matter change: set Key to Value, or delete Key when Delete is true.
type Edit struct {
	Key    string
	Value  string // already a YAML scalar (use quoteScalar)
	Delete bool
}

// anchorOrder is where new keys go: after the last of these that is present.
var anchorOrder = []string{"status", "last_verified", "last_checked", "last_error"}

// EditFrontMatter applies edits to the front matter only. The body is copied
// byte for byte. It returns the new content and whether anything changed.
func EditFrontMatter(data []byte, edits []Edit) ([]byte, bool, error) {
	lines, bodyStart, ok := splitFrontMatter(data)
	if !ok {
		return nil, false, fmt.Errorf("no front matter")
	}
	orig := strings.Join(lines, "\n")
	for _, e := range edits {
		idx, n := findKey(lines, e.Key)
		switch {
		case e.Delete && idx >= 0:
			lines = append(lines[:idx], lines[idx+n:]...)
		case e.Delete:
		case idx >= 0:
			repl := []string{e.Key + ": " + e.Value}
			lines = append(lines[:idx], append(repl, lines[idx+n:]...)...)
		default:
			at := insertPoint(lines, e.Key)
			lines = append(lines[:at], append([]string{e.Key + ": " + e.Value}, lines[at:]...)...)
		}
	}
	if strings.Join(lines, "\n") == orig {
		return data, false, nil
	}
	nl := "\n"
	if bytes.HasPrefix(data, []byte("---\r\n")) {
		nl = "\r\n"
	}
	var b bytes.Buffer
	b.WriteString("---" + nl)
	for _, l := range lines {
		b.WriteString(l + nl)
	}
	b.WriteString("---" + nl)
	b.Write(data[bodyStart:])
	return b.Bytes(), true, nil
}

// findKey returns the index of key's line and how many lines it spans
// (indented continuation lines count), or -1.
func findKey(lines []string, key string) (int, int) {
	for i, l := range lines {
		if k, _, ok := topLevelKey(l); ok && k == key {
			n := 1
			for i+n < len(lines) && (strings.HasPrefix(lines[i+n], " ") || strings.HasPrefix(lines[i+n], "\t")) {
				n++
			}
			return i, n
		}
	}
	return -1, 0
}

func insertPoint(lines []string, key string) int {
	// Keys in anchorOrder go right after the nearest earlier anchor that exists.
	for i, a := range anchorOrder {
		if a == key {
			for j := i - 1; j >= 0; j-- {
				if idx, n := findKey(lines, anchorOrder[j]); idx >= 0 {
					return idx + n
				}
			}
			break
		}
	}
	if idx, n := findKey(lines, "status"); idx >= 0 {
		return idx + n
	}
	return len(lines)
}
