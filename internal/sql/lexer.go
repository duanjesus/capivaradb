package sql

import (
	"strings"
	"unicode/utf8"

	"github.com/duanjesus/capivaradb/internal/pgerr"
)

// TokenKind classifies a token.
type TokenKind uint8

const (
	TEOF TokenKind = iota
	TIdent
	TQuotedIdent
	TInt
	TFloat
	TString
	TParam
	TOp
)

// Token is one lexical unit.
type Token struct {
	Kind TokenKind
	// Text is the normalised value: lower-cased for identifiers, unescaped
	// for strings and quoted identifiers, the digits for parameters, the
	// operator itself for TOp.
	Text string
	// Raw is the exact source text, used in error messages.
	Raw string
	// Pos is the 1-based character offset of the token in the query.
	Pos int
}

type lexer struct {
	src string
	i   int

	// Positions reported to clients are counted in characters, not bytes.
	// Token offsets only move forward, so the rune count is accumulated
	// incrementally instead of rescanning the prefix for every token.
	countedOff int
	counted    int
}

func (l *lexer) pos(off int) int {
	l.counted += utf8.RuneCountInString(l.src[l.countedOff:off])
	l.countedOff = off
	return l.counted + 1
}

func (l *lexer) errorf(off int, format string, args ...any) error {
	return pgerr.New(pgerr.SyntaxError, format, args...).At(l.pos(off))
}

var twoCharOps = []string{"<>", "!=", "<=", ">=", "||", "::"}

const oneCharOps = "(),;.*+-/%=<>"

// Lex splits src into tokens. The result always ends with a TEOF token.
func Lex(src string) ([]Token, error) {
	l := &lexer{src: src}
	var toks []Token
	for {
		if err := l.skipSpace(); err != nil {
			return nil, err
		}
		start := l.i
		if l.i >= len(src) {
			return append(toks, Token{Kind: TEOF, Pos: l.pos(start)}), nil
		}
		c := src[l.i]
		tok := Token{}
		switch {
		case isIdentStart(c):
			for l.i < len(src) && isIdentPart(src[l.i]) {
				l.i++
			}
			tok.Kind, tok.Text = TIdent, strings.ToLower(src[start:l.i])
		case c == '"':
			text, err := l.quoted('"', "quoted identifier")
			if err != nil {
				return nil, err
			}
			if text == "" {
				return nil, l.errorf(start, "zero-length delimited identifier")
			}
			tok.Kind, tok.Text = TQuotedIdent, text
		case c == '\'':
			text, err := l.quoted('\'', "quoted string")
			if err != nil {
				return nil, err
			}
			tok.Kind, tok.Text = TString, text
		case isDigit(c) || (c == '.' && l.i+1 < len(src) && isDigit(src[l.i+1])):
			tok.Kind = l.number()
			tok.Text = src[start:l.i]
		case c == '$':
			l.i++
			for l.i < len(src) && isDigit(src[l.i]) {
				l.i++
			}
			if l.i == start+1 {
				return nil, l.errorf(start, "syntax error at or near \"$\"")
			}
			tok.Kind, tok.Text = TParam, src[start+1:l.i]
		default:
			op := ""
			for _, o := range twoCharOps {
				if strings.HasPrefix(src[l.i:], o) {
					op = o
					break
				}
			}
			if op == "" && strings.IndexByte(oneCharOps, c) >= 0 {
				op = string(c)
			}
			if op == "" {
				_, size := utf8.DecodeRuneInString(src[l.i:])
				return nil, l.errorf(start, "syntax error at or near %q", src[l.i:l.i+size])
			}
			l.i += len(op)
			tok.Kind, tok.Text = TOp, op
		}
		tok.Raw = src[start:l.i]
		tok.Pos = l.pos(start)
		toks = append(toks, tok)
	}
}

// skipSpace skips whitespace, "--" line comments and nested "/* */" block
// comments (PostgreSQL, unlike C, lets block comments nest).
func (l *lexer) skipSpace() error {
	src := l.src
	for l.i < len(src) {
		c := src[l.i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			l.i++
		case c == '-' && strings.HasPrefix(src[l.i:], "--"):
			for l.i < len(src) && src[l.i] != '\n' {
				l.i++
			}
		case c == '/' && strings.HasPrefix(src[l.i:], "/*"):
			start, depth := l.i, 1
			l.i += 2
			for depth > 0 {
				switch {
				case l.i >= len(src):
					return l.errorf(start, "unterminated /* comment")
				case strings.HasPrefix(src[l.i:], "/*"):
					depth++
					l.i += 2
				case strings.HasPrefix(src[l.i:], "*/"):
					depth--
					l.i += 2
				default:
					l.i++
				}
			}
		default:
			return nil
		}
	}
	return nil
}

// quoted scans a string or identifier delimited by q, where a doubled
// delimiter stands for one literal delimiter.
func (l *lexer) quoted(q byte, what string) (string, error) {
	start := l.i
	l.i++
	var sb strings.Builder
	for l.i < len(l.src) {
		c := l.src[l.i]
		if c != q {
			sb.WriteByte(c)
			l.i++
			continue
		}
		if l.i+1 < len(l.src) && l.src[l.i+1] == q {
			sb.WriteByte(q)
			l.i += 2
			continue
		}
		l.i++
		return sb.String(), nil
	}
	return "", l.errorf(start, "unterminated %s", what)
}

func (l *lexer) number() TokenKind {
	src := l.src
	kind := TInt
	for l.i < len(src) && isDigit(src[l.i]) {
		l.i++
	}
	if l.i < len(src) && src[l.i] == '.' {
		kind = TFloat
		l.i++
		for l.i < len(src) && isDigit(src[l.i]) {
			l.i++
		}
	}
	if l.i < len(src) && (src[l.i] == 'e' || src[l.i] == 'E') {
		j := l.i + 1
		if j < len(src) && (src[j] == '+' || src[j] == '-') {
			j++
		}
		if j < len(src) && isDigit(src[j]) {
			kind = TFloat
			for j < len(src) && isDigit(src[j]) {
				j++
			}
			l.i = j
		}
	}
	return kind
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) || c == '$' }
