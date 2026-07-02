package http

import (
	"fmt"
	"strings"
	"unicode"
)

// SQL tokenizer. Keywords are case-insensitive; identifiers preserve case
// (JSON field names are case-sensitive). Strings use single quotes with ''
// escaping; identifiers may be double-quoted to escape keywords.

type tokKind int

const (
	tkEOF tokKind = iota
	tkIdent
	tkString
	tkNumber
	tkOp // = <> != < <= > >= ( ) , . *
)

type token struct {
	kind tokKind
	text string // for tkIdent: as written; for tkOp: the operator
	pos  int
}

func lexSQL(src string) ([]token, error) {
	var toks []token
	i, n := 0, len(src)
	for i < n {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == '\'': // string literal, '' escapes a quote
			j := i + 1
			var b strings.Builder
			for {
				if j >= n {
					return nil, fmt.Errorf("unterminated string at position %d", i)
				}
				if src[j] == '\'' {
					if j+1 < n && src[j+1] == '\'' {
						b.WriteByte('\'')
						j += 2
						continue
					}
					break
				}
				b.WriteByte(src[j])
				j++
			}
			toks = append(toks, token{kind: tkString, text: b.String(), pos: i})
			i = j + 1
		case c == '"': // quoted identifier
			j := i + 1
			for j < n && src[j] != '"' {
				j++
			}
			if j >= n {
				return nil, fmt.Errorf("unterminated quoted identifier at position %d", i)
			}
			toks = append(toks, token{kind: tkIdent, text: src[i+1 : j], pos: i})
			i = j + 1
		case c >= '0' && c <= '9':
			j := i
			for j < n && (src[j] >= '0' && src[j] <= '9' || src[j] == '.') {
				j++
			}
			toks = append(toks, token{kind: tkNumber, text: src[i:j], pos: i})
			i = j
		case c == '_' || unicode.IsLetter(rune(c)) || c >= 0x80:
			j := i
			for j < n {
				cj := src[j]
				if cj == '_' || cj >= '0' && cj <= '9' || unicode.IsLetter(rune(cj)) || cj >= 0x80 {
					j++
					continue
				}
				break
			}
			toks = append(toks, token{kind: tkIdent, text: src[i:j], pos: i})
			i = j
		default:
			switch {
			case strings.HasPrefix(src[i:], "<="), strings.HasPrefix(src[i:], ">="),
				strings.HasPrefix(src[i:], "<>"), strings.HasPrefix(src[i:], "!="):
				toks = append(toks, token{kind: tkOp, text: src[i : i+2], pos: i})
				i += 2
			case strings.ContainsRune("=<>(),.*", rune(c)):
				toks = append(toks, token{kind: tkOp, text: string(c), pos: i})
				i++
			case c == ';': // trailing statement terminator is tolerated
				i++
			default:
				return nil, fmt.Errorf("unexpected character %q at position %d", c, i)
			}
		}
	}
	toks = append(toks, token{kind: tkEOF, pos: n})
	return toks, nil
}
