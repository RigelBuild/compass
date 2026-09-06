// Package schemaparse extracts the SDK's credential-marked setting paths from
// the OMP settings-schema TypeScript source.
//
// It exists as a real (non-generator) package so the parsing rules are unit
// testable. The logic decides which setting paths the store door rejects, so a
// silent under-collection here would quietly stop the door rejecting a
// credential — that risk is what earns this its own package and test suite
// rather than living behind the generator's //go:build ignore tag.
package schemaparse

import (
	"errors"
	"fmt"
	"strings"
)

// ExtractCredentialKeys parses the SETTINGS_SCHEMA object literal in the schema
// source and returns every top-level key whose def is credential-marked, plus
// the total number of top-level keys walked (for run's plausibility floor). It
// mirrors isCredential: a def is credential-marked when it has `credential:
// true` directly, or a `ui` object with `secret: true`.
func ExtractCredentialKeys(src string) ([]string, int, error) {
	p := &jsParser{s: src}
	if err := p.seekSchema(); err != nil {
		return nil, 0, err
	}
	schema, err := p.parseObject()
	if err != nil {
		return nil, 0, err
	}
	var keys []string
	for key, val := range schema {
		def, ok := val.(map[string]any)
		if !ok {
			continue
		}
		if IsLiteralTrue(def["credential"]) {
			keys = append(keys, key)
			continue
		}
		if ui, ok := def["ui"].(map[string]any); ok && IsLiteralTrue(ui["secret"]) {
			keys = append(keys, key)
		}
	}
	return keys, len(schema), nil
}

// IsLiteralTrue reports whether a parsed def value is the literal `true`.
// Non-object values arrive as their trimmed raw text, so ordinary authoring
// that decorates the literal must not read as false: a trailing line or block
// comment (`credential: true // yes`) and an `as const` assertion are both
// stripped before comparing. Anything else — a helper call, a variable, a
// computed expression, a quoted "true" — is deliberately NOT treated as true,
// since the generator cannot evaluate it. Erring toward false only ever widens
// the denylist review (a missed path fails the drift gate loudly); erring
// toward true would silently mark a non-credential path, so the trims below are
// deliberately delimiter-anchored rather than bare suffix strips.
func IsLiteralTrue(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	if i := strings.Index(s, "//"); i >= 0 {
		s = s[:i]
	}
	for {
		i := strings.Index(s, "/*")
		if i < 0 {
			break
		}
		j := strings.Index(s[i+2:], "*/")
		if j < 0 {
			s = s[:i]
			break
		}
		s = s[:i] + s[i+2+j+2:]
	}
	s = strings.TrimSpace(s)
	// Strip a trailing `as const` only as a whole, whitespace-delimited
	// assertion. A bare TrimSuffix("const") would also eat the tail of an
	// identifier like `trueconst`, turning it into a false positive.
	if rest, ok := strings.CutSuffix(s, "const"); ok {
		trimmed := strings.TrimRight(rest, " \t")
		if len(trimmed) < len(rest) {
			if rest, ok := strings.CutSuffix(trimmed, "as"); ok {
				if t := strings.TrimRight(rest, " \t"); len(t) < len(rest) {
					s = t
				}
			}
		}
	}
	return s == "true"
}

// jsParser is a minimal structural parser over a JS/TS source string. It reads
// object literals into map[string]any (nested objects → maps; every other value
// → its trimmed raw text, so a scalar `true` compares equal to the string
// "true"), skipping arrays and arbitrary expressions with balanced bracket,
// string, template, and comment handling. It is not a full JS parser — only
// enough to walk SETTINGS_SCHEMA's shape.
type jsParser struct {
	s string
	i int
}

// seekSchema advances past `SETTINGS_SCHEMA` and its `=` to the opening `{` of
// the schema object literal, leaving the cursor on that brace.
func (p *jsParser) seekSchema() error {
	idx := strings.Index(p.s, "SETTINGS_SCHEMA")
	if idx < 0 {
		return errors.New("SETTINGS_SCHEMA not found")
	}
	p.i = idx + len("SETTINGS_SCHEMA")
	p.skipTrivia()
	if p.i >= len(p.s) || p.s[p.i] != '=' {
		return errors.New("expected '=' after SETTINGS_SCHEMA")
	}
	p.i++
	p.skipTrivia()
	if p.i >= len(p.s) || p.s[p.i] != '{' {
		return errors.New("expected '{' opening SETTINGS_SCHEMA")
	}
	return nil
}

// parseObject parses `{ key: value, ... }` starting at the current `{`,
// returning key → value (nested object as map[string]any, else raw text).
func (p *jsParser) parseObject() (map[string]any, error) {
	if p.i >= len(p.s) || p.s[p.i] != '{' {
		return nil, fmt.Errorf("parseObject: expected '{' at offset %d", p.i)
	}
	p.i++ // consume '{'
	obj := map[string]any{}
	for {
		p.skipTrivia()
		if p.i >= len(p.s) {
			return nil, errors.New("parseObject: unterminated object")
		}
		if p.s[p.i] == '}' {
			p.i++
			return obj, nil
		}
		key, err := p.parseKey()
		if err != nil {
			return nil, err
		}
		p.skipTrivia()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, fmt.Errorf("parseObject: expected ':' after key %q", key)
		}
		p.i++ // consume ':'
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		obj[key] = val
		p.skipTrivia()
		if p.i < len(p.s) && p.s[p.i] == ',' {
			p.i++
		}
	}
}

// parseKey reads a property name: a quoted string or a bare identifier.
func (p *jsParser) parseKey() (string, error) {
	p.skipTrivia()
	if p.i >= len(p.s) {
		return "", errors.New("parseKey: unexpected EOF")
	}
	c := p.s[p.i]
	if c == '"' || c == '\'' || c == '`' {
		return p.parseStringLiteral()
	}
	start := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '$' {
			p.i++
			continue
		}
		break
	}
	if p.i == start {
		return "", fmt.Errorf("parseKey: no identifier at offset %d", start)
	}
	return p.s[start:p.i], nil
}

// parseValue returns a nested map for an object value, or the trimmed raw text
// for any other value (array, scalar, or expression), consuming a balanced span
// up to the next top-level ',' or '}'.
func (p *jsParser) parseValue() (any, error) {
	p.skipTrivia()
	if p.i >= len(p.s) {
		return nil, errors.New("parseValue: unexpected EOF")
	}
	if p.s[p.i] == '{' {
		obj, err := p.parseObject()
		if err != nil {
			return nil, err
		}
		// A nested object value may be followed by a type cast (e.g. `} as
		// const`); consume and discard it so the enclosing object parse resumes
		// on the next top-level ',' or '}'.
		p.skipTrivia()
		if p.i < len(p.s) && p.s[p.i] != ',' && p.s[p.i] != '}' {
			if _, err := p.parseRawExpr(); err != nil {
				return nil, err
			}
		}
		return obj, nil
	}
	return p.parseRawExpr()
}

// parseRawExpr consumes a value expression up to (not including) the next
// top-level ',' or '}', respecting nested (), [], {}, strings, templates, and
// comments. Returns the trimmed raw text.
func (p *jsParser) parseRawExpr() (string, error) {
	start := p.i
	depth := 0
	angle := 0
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch c {
		case '/':
			if p.i+1 < len(p.s) && (p.s[p.i+1] == '/' || p.s[p.i+1] == '*') {
				p.skipComment()
				continue
			}
			p.i++
		case '"', '\'', '`':
			if _, err := p.parseStringLiteral(); err != nil {
				return "", err
			}
		case '(', '[', '{':
			depth++
			p.i++
		case ')', ']', '}':
			if depth == 0 {
				return strings.TrimSpace(p.s[start:p.i]), nil
			}
			depth--
			p.i++
		case '<':
			// A generic type-argument list in a cast (e.g. `Record<string,
			// unknown>`) carries top-level commas that are NOT value
			// separators; track angle depth so they are skipped.
			angle++
			p.i++
		case '>':
			if angle > 0 {
				angle--
			}
			p.i++
		case ',':
			if depth == 0 && angle == 0 {
				return strings.TrimSpace(p.s[start:p.i]), nil
			}
			p.i++
		default:
			p.i++
		}
	}
	return strings.TrimSpace(p.s[start:p.i]), nil
}

// parseStringLiteral consumes a '...' , "..." , or `...` string starting at the
// current quote, handling escapes and (for templates) ${ } interpolation with
// balanced braces. Returns the unescaped-raw inner text for a simple string;
// for templates the raw inner text (interpolation left as-is) — callers only
// use it for keys, which are never templates.
func (p *jsParser) parseStringLiteral() (string, error) {
	quote := p.s[p.i]
	p.i++ // consume opening quote
	start := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '\\' {
			p.i += 2
			continue
		}
		if quote == '`' && c == '$' && p.i+1 < len(p.s) && p.s[p.i+1] == '{' {
			p.i += 2
			braces := 1
			for p.i < len(p.s) && braces > 0 {
				switch p.s[p.i] {
				case '{':
					braces++
				case '}':
					braces--
				}
				p.i++
			}
			continue
		}
		if c == quote {
			inner := p.s[start:p.i]
			p.i++ // consume closing quote
			return inner, nil
		}
		p.i++
	}
	return "", errors.New("parseStringLiteral: unterminated string")
}

// skipTrivia skips whitespace and comments.
func (p *jsParser) skipTrivia() {
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			p.i++
			continue
		}
		if c == '/' && p.i+1 < len(p.s) && (p.s[p.i+1] == '/' || p.s[p.i+1] == '*') {
			p.skipComment()
			continue
		}
		break
	}
}

// skipComment skips a // line or /* block */ comment starting at the current
// '/'. The caller guarantees the two-char opener.
func (p *jsParser) skipComment() {
	if p.s[p.i+1] == '/' {
		p.i += 2
		for p.i < len(p.s) && p.s[p.i] != '\n' {
			p.i++
		}
		return
	}
	p.i += 2
	for p.i < len(p.s) {
		if p.s[p.i] == '*' && p.i+1 < len(p.s) && p.s[p.i+1] == '/' {
			p.i += 2
			return
		}
		p.i++
	}
}
