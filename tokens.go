package sev

import "strings"

// Token is one word of the command line, plus where it starts. Text is a view
// into the caller's line (no copy) unless the word was quoted or escaped.
type Token struct {
	Text  string
	Start int
}

// scan splits line into the tokens of its last pipeline segment: text before a
// |, &&, ; or parenthesis is a different command and cannot inform this one.
// It writes into dst so a warm caller allocates nothing, and reports whether
// the final token is still being typed.
//
// Assignment prefixes (FOO=bar cmd) are dropped from the token stream but kept
// in the line, so every Token.Start still points into the original string.
func scan(line string, dst []Token) (tokens []Token, segStart int, partial bool) {
	tokens = dst[:0]
	if line == "" {
		return tokens, 0, false
	}

	i, n := 0, len(line)
	atSegmentStart := true
	for i < n {
		c := line[i]
		if c == ' ' || c == '\t' {
			i++
			continue
		}
		if isSeparator(c) {
			i++
			for i < n && (line[i] == ' ' || line[i] == '\t') {
				i++
			}
			tokens = tokens[:0]
			segStart = i
			atSegmentStart = true
			continue
		}

		start, end, quoted, next := tokenEnd(line, i) // end is one past the last byte
		i = next
		text := line[start:end]
		if quoted {
			text = unquote(text)
		}
		if atSegmentStart && isAssignment(text) {
			continue
		}
		atSegmentStart = false
		tokens = append(tokens, Token{Text: text, Start: start})
	}

	last := line[n-1]
	partial = len(tokens) > 0 && last != ' ' && last != '\t'
	return tokens, segStart, partial
}

func isSeparator(c byte) bool {
	return c == '|' || c == ';' || c == '&' || c == '(' || c == ')' || c == '\n'
}

// tokenEnd finds the extent of the word starting at i, following quotes so a
// space inside "a b" does not split it. quoted reports that unquoting is needed;
// next is where scanning resumes.
func tokenEnd(line string, i int) (start, end int, quoted bool, next int) {
	start = i
	n := len(line)
	for i < n {
		switch c := line[i]; {
		case c == '\\' && i+1 < n:
			quoted = true
			i += 2
		case c == '\'':
			quoted = true
			i++
			for i < n && line[i] != '\'' {
				i++
			}
			if i < n {
				i++
			}
		case c == '"':
			quoted = true
			i++
			for i < n && line[i] != '"' {
				if line[i] == '\\' && i+1 < n {
					i++
				}
				i++
			}
			if i < n {
				i++
			}
		case c == ' ' || c == '\t':
			return start, i, quoted, i
		case isSeparator(c):
			return start, i, quoted, i
		default:
			i++
		}
	}
	return start, n, quoted, n
}

// unquote removes one level of shell quoting. Only reached for words that
// actually carry quotes or escapes, so the hot path never calls it.
func unquote(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		case '\'':
			for i++; i < len(s) && s[i] != '\''; i++ {
				b.WriteByte(s[i])
			}
		case '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				b.WriteByte(s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// isAssignment reports whether tok looks like NAME=value, the shell's
// per-command environment prefix.
func isAssignment(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := tok[i]
		switch {
		case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
