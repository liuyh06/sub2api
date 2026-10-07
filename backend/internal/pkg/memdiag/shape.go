package memdiag

import (
	"bytes"
	"errors"
	"sort"
	"strconv"
)

type Item struct {
	Index int    `json:"index"`
	Type  string `json:"type"`
	Bytes int    `json:"bytes"`
}
type Shape struct {
	Top            map[string]int `json:"top_level_raw_bytes"`
	Items          int            `json:"input_items"`
	Largest        []Item         `json:"largest_input_items"`
	ImageItems     int            `json:"image_items"`
	DataImageBytes int            `json:"data_image_string_raw_bytes"`
	DataImages     int            `json:"data_image_strings"`
	EncryptedBytes int            `json:"encrypted_content_raw_bytes"`
	OutputBytes    int            `json:"output_field_raw_bytes"`
	Namespaces     int            `json:"direct_input_namespaces"`
}

// Inspect scans raw JSON once. It never decodes content strings or retains body slices.
// Field byte counts include JSON syntax; nested categories may overlap.
func Inspect(body []byte) (Shape, error) {
	p := scanner{b: body, shape: Shape{Top: map[string]int{}}}
	_, err := p.value(0, "root", false)
	p.space()
	if err == nil && p.i != len(body) {
		err = errors.New("trailing JSON")
	}
	return p.shape, err
}

type scanner struct {
	b     []byte
	i     int
	shape Shape
}

func (p *scanner) space() {
	for p.i < len(p.b) && (p.b[p.i] == ' ' || p.b[p.i] == '\n' || p.b[p.i] == '\r' || p.b[p.i] == '\t') {
		p.i++
	}
}
func (p *scanner) str() error {
	if p.i >= len(p.b) || p.b[p.i] != '"' {
		return errors.New("expected string")
	}
	p.i++
	for p.i < len(p.b) {
		c := p.b[p.i]
		p.i++
		if c == '"' {
			return nil
		}
		if c == '\\' {
			if p.i >= len(p.b) {
				break
			}
			p.i++
		}
	}
	return errors.New("unterminated string")
}
func safeType(raw []byte) string {
	for _, s := range []string{"message", "reasoning", "function_call", "custom_tool_call", "tool_call", "mcp_tool_call", "function_call_output", "custom_tool_call_output", "input_image", "image_url", "input_text", "output_text", "item_reference", "additional_tools"} {
		if len(raw) == len(s)+2 && raw[0] == '"' && raw[len(raw)-1] == '"' && bytes.Equal(raw[1:len(raw)-1], []byte(s)) {
			return s
		}
	}
	return "other"
}
func topKey(s string) string {
	switch s {
	case "input", "tools", "instructions", "metadata", "model", "messages", "text", "reasoning":
		return s
	}
	return "other"
}
func (p *scanner) value(depth int, where string, direct bool) (string, error) {
	if depth > 128 {
		return "", errors.New("depth limit")
	}
	p.space()
	start := p.i
	if start >= len(p.b) {
		return "", errors.New("missing value")
	}
	kind := "other"
	switch p.b[p.i] {
	case '"':
		if err := p.str(); err != nil {
			return "", err
		}
		if bytes.HasPrefix(p.b[start:p.i], []byte(`"data:image/`)) {
			p.shape.DataImages++
			p.shape.DataImageBytes += p.i - start
		}
	case '{':
		p.i++
		p.space()
		if p.i < len(p.b) && p.b[p.i] == '}' {
			p.i++
			return kind, nil
		}
		for {
			p.space()
			kstart := p.i
			if err := p.str(); err != nil {
				return "", err
			}
			key := ""
			if p.i-kstart <= 64 {
				key, _ = strconv.Unquote(string(p.b[kstart:p.i]))
			}
			p.space()
			if p.i >= len(p.b) || p.b[p.i] != ':' {
				return "", errors.New("missing colon")
			}
			p.i++
			p.space()
			vstart := p.i
			childWhere := ""
			if where == "root" && key == "input" {
				childWhere = "input"
			}
			_, err := p.value(depth+1, childWhere, false)
			if err != nil {
				return "", err
			}
			size := p.i - vstart
			if where == "root" {
				p.shape.Top[topKey(key)] += size
			}
			if key == "type" {
				kind = safeType(p.b[vstart:p.i])
				if kind == "input_image" || kind == "image_url" {
					p.shape.ImageItems++
				}
			}
			if key == "encrypted_content" {
				p.shape.EncryptedBytes += size
			}
			if key == "output" {
				p.shape.OutputBytes += size
			}
			if direct && key == "namespace" {
				p.shape.Namespaces++
			}
			p.space()
			if p.i >= len(p.b) {
				return "", errors.New("unclosed object")
			}
			c := p.b[p.i]
			p.i++
			if c == '}' {
				break
			}
			if c != ',' {
				return "", errors.New("missing comma")
			}
		}
	case '[':
		p.i++
		p.space()
		if p.i < len(p.b) && p.b[p.i] == ']' {
			p.i++
			return kind, nil
		}
		for {
			p.space()
			vstart := p.i
			typ, err := p.value(depth+1, "", where == "input")
			if err != nil {
				return "", err
			}
			if where == "input" {
				item := Item{p.shape.Items, typ, p.i - vstart}
				p.shape.Items++
				p.shape.Largest = append(p.shape.Largest, item)
				sort.Slice(p.shape.Largest, func(i, j int) bool { return p.shape.Largest[i].Bytes > p.shape.Largest[j].Bytes })
				if len(p.shape.Largest) > 5 {
					p.shape.Largest = p.shape.Largest[:5]
				}
			}
			p.space()
			if p.i >= len(p.b) {
				return "", errors.New("unclosed array")
			}
			c := p.b[p.i]
			p.i++
			if c == ']' {
				break
			}
			if c != ',' {
				return "", errors.New("missing comma")
			}
		}
	default:
		for p.i < len(p.b) && !bytes.ContainsRune([]byte(",]} \t\r\n"), rune(p.b[p.i])) {
			p.i++
		}
		if p.i == start {
			return "", errors.New("invalid scalar")
		}
	}
	return kind, nil
}
