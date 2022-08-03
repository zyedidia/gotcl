package gotcl

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"unicode"
)

type loc struct {
	file string
	line int
	col  int
}

func (l loc) String() string {
	return fmt.Sprintf("%s:%d:%d", l.file, l.line+1, l.col)
}

type parser struct {
	data   io.RuneReader
	tmpbuf *bytes.Buffer
	ch     rune
	src    loc
}

func newParser(input io.RuneReader, loc loc) *parser {
	p := &parser{
		data:   input,
		tmpbuf: bytes.NewBuffer(make([]byte, 0, 1024)),
		src:    loc,
	}
	p.advance()
	return p
}

func issepspace(c rune) bool { return c == '\t' || c == ' ' }
func isvarword(c rune) bool {
	return unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_'
}

func (p *parser) fail(s string) {
	fmt.Fprintf(os.Stderr, "%v: %s\n", p.src, s)
	os.Exit(1)
}

func (p *parser) advance() (result rune) {
	if p.ch == -1 {
		p.fail("unexpected EOF")
	}
	result = p.ch
	r, sz, e := p.data.ReadRune()
	if e != nil {
		if e != io.EOF {
			p.fail(e.Error())
		}
		p.ch = -1
	} else {
		p.src.col += sz
		if r == '\n' {
			p.src.col = 0
			p.src.line++
		}
		p.ch = r
	}
	return
}

func (p *parser) consumeWhile1(fn func(rune) bool, desc string) string {
	p.tmpbuf.Reset()
	for p.ch != -1 && fn(p.ch) {
		p.tmpbuf.WriteRune(p.advance())
	}
	res := p.tmpbuf.String()
	if res == "" {
		p.expectFailed(desc, p.ch)
	}
	return res
}

func (p *parser) expectFailed(expected string, ch rune) {
	got := "EOF"
	if ch != -1 {
		got = string(ch)
	}
	p.fail("Expected " + expected + ", got '" + got + "'")
}

func (p *parser) consumeRune(r rune) {
	if p.ch != r {
		p.expectFailed("'"+string(r)+"'", p.ch)
	}
	p.advance()
}

func (p *parser) eatSpace() {
	for p.ch != -1 && unicode.IsSpace(p.ch) {
		p.advance()
	}
}

func (p *parser) eatWhile(fn func(rune) bool) {
	for p.ch != -1 && fn(p.ch) {
		p.advance()
	}
}

func isword(c rune) bool {
	switch c {
	case '[', ']', ';', '$', '"':
		return false
	}
	return !unicode.IsSpace(c)
}
func (p *parser) parseSimpleWordTil(til rune) *tliteral {
	loc := p.src
	p.tmpbuf.Reset()
	prev_esc := false
	for p.ch != -1 && p.ch != til {
		if p.ch == '\\' && !prev_esc {
			prev_esc = true
			p.advance()
		} else if prev_esc || isword(p.ch) {
			c := p.advance()
			if prev_esc {
				p.tmpbuf.WriteString(escaped(c))
				prev_esc = false
			} else {
				p.tmpbuf.WriteRune(c)
			}
		} else {
			break
		}
	}
	res := p.tmpbuf.String()
	if len(res) == 0 {
		p.expectFailed("word", p.ch)
	}
	return &tliteral{strval: res, loc: loc}
}

func (p *parser) parseSubcommand() *subcommand {
	loc := p.src
	p.consumeRune('[')
	res := make([]tclTok, 0, 16)
	p.eatWhile(issepspace)
	for p.ch != ']' {
		res = append(res, p.parseToken())
		p.eatWhile(issepspace)
	}
	p.consumeRune(']')
	return &subcommand{cmd: makeCommand(res), loc: loc}
}

func (p *parser) parseBlockData() string {
	p.consumeRune('{')
	nest := 0
	p.tmpbuf.Reset()
	for {
		switch p.ch {
		case '\\':
			p.tmpbuf.WriteRune(p.advance())
		case '{':
			nest++
		case '}':
			if nest == 0 {
				p.advance()
				return p.tmpbuf.String()
			}
			nest--
		case -1:
			p.fail("unclosed block")
		}
		p.tmpbuf.WriteRune(p.advance())
	}
}

func (p *parser) hasExtraChars() bool {
	return p.ch != -1 && !unicode.IsSpace(p.ch) && p.ch != '}' && p.ch != ']' && p.ch != ';'
}

func (p *parser) checkForExtraChars() {
	if p.hasExtraChars() {
		p.fail("extra characters after close-brace")
	}
}

func (p *parser) parseBlock() *block {
	loc := p.src
	bd := p.parseBlockData()
	p.checkForExtraChars()
	return &block{strval: bd, loc: loc}
}

func (p *parser) parseBlockOrExpand() tclTok {
	loc := p.src
	bd := p.parseBlockData()
	if bd == "*" && p.hasExtraChars() {
		return &expandTok{subject: p.parseToken(), loc: loc}
	}
	p.checkForExtraChars()
	return &block{strval: bd, loc: loc}
}

func (p *parser) parseVariable() varRef {
	p.consumeRune('$')
	return p.parseVarRef()
}

func (p *parser) parseVarRef() varRef {
	loc := p.src
	if p.ch == '{' {
		return toVarRef(p.parseBlockData())
	}
	global := false
	if p.ch == ':' {
		p.advance()
		p.consumeRune(':')
		global = true
	}
	name := p.consumeWhile1(isvarword, "variable name")
	var ind tclTok
	if p.ch == '(' {
		p.advance()
		ind = p.parseTokenTil(')')
		p.consumeRune(')')
	}
	return varRef{is_global: global, name: name, arrind: ind, loc: loc}
}

var escMap = map[rune]string{
	'n': "\n", 't': "\t", 'a': "\a", 'v': "\v", 'r': "\r"}

func escaped(r rune) string {
	if v, ok := escMap[r]; ok {
		return v
	}
	return string(r)
}

func (p *parser) parseListStringLit() string {
	p.consumeRune('"')
	var buf bytes.Buffer
	for {
		switch p.ch {
		case '"':
			p.advance()
			if p.ch != -1 && !unicode.IsSpace(p.ch) {
				p.fail("list element in quotes not followed by space")
			}
			return buf.String()
		case '\\':
			p.advance()
			buf.WriteString(escaped(p.advance()))
		case -1:
			p.fail("unmatched open quote in list")
		default:
			buf.WriteRune(p.advance())
		}
	}
}

func (p *parser) parseStringLit() strlit {
	loc := p.src
	p.consumeRune('"')
	var accum bytes.Buffer
	toks := make([]littok, 0, 8)
	record_accum := func() {
		if accum.Len() != 0 {
			toks = append(toks, littok{kind: kRaw, value: accum.String()})
			accum.Reset()
		}
	}
	for {
		switch p.ch {
		case '"':
			record_accum()
			p.advance()
			return strlit{toks: toks, loc: loc}
		case '$':
			record_accum()
			vref := p.parseVariable()
			toks = append(toks, littok{kind: kVar, varref: &vref})
		case '[':
			record_accum()
			subcmd := p.parseSubcommand()
			toks = append(toks, littok{kind: kSubcmd, subcmd: subcmd})
		case '\\':
			p.advance()
			accum.WriteString(escaped(p.advance()))
		case -1:
			p.fail("missing \"")
		default:
			accum.WriteRune(p.advance())
		}
	}
}

func isEol(ch rune) bool {
	switch ch {
	case -1, ';', '\n':
		return true
	}
	return false
}

func (p *parser) eatExtra() {
	p.eatSpace()
	for p.ch == ';' {
		p.advance()
		p.eatSpace()
	}
}

func (p *parser) parseComment() {
	p.consumeRune('#')
	p.eatWhile(func(c rune) bool { return c != '\n' })
}

func (p *parser) parseCommands() []command {
	res := make([]command, 0, 128)
	p.eatSpace()
	for p.ch != -1 {
		if p.ch == '#' {
			p.parseComment()
		} else {
			res = append(res, p.parseCommand())
		}
		p.eatExtra()
	}
	return res
}

func notspace(c rune) bool { return !unicode.IsSpace(c) }
func (p *parser) parseList() []string {
	res := make([]string, 0, 8)
Loop:
	for {
		p.eatSpace()
		switch p.ch {
		case -1:
			break Loop
		case '{':
			res = append(res, p.parseBlockData())
		case '"':
			res = append(res, p.parseListStringLit())
		default:
			res = append(res, p.consumeWhile1(notspace, "word"))
		}
	}
	return res
}

func (p *parser) parseCommand() command {
	res := make([]tclTok, 0, 16)
	res = append(res, p.parseToken())
	p.eatWhile(issepspace)
	for !isEol(p.ch) {
		res = append(res, p.parseToken())
		p.eatWhile(issepspace)
	}
	return makeCommand(res)
}

func (p *parser) parseToken() tclTok {
	return p.parseTokenTil(-1)
}

func (p *parser) parseTokenTil(til rune) tclTok {
	switch p.ch {
	case '[':
		return p.parseSubcommand()
	case '{':
		return p.parseBlockOrExpand()
	case '"':
		return p.parseStringLit()
	case '$':
		return p.parseVariable()
	}
	return p.parseSimpleWordTil(til)
}

func setError(err *error) {
	if e := recover(); e != nil {
		*err = e.(error)
	}
}

func parseListInner(in io.RuneReader, loc loc) (items []string, err error) {
	p := newParser(in, loc)
	defer setError(&err)
	items = p.parseList()
	return
}

func parseCommands(in io.RuneReader, loc loc) (cmds []command, err error) {
	p := newParser(in, loc)
	defer setError(&err)
	cmds = p.parseCommands()
	return
}
