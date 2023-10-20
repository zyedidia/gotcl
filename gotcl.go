package gotcl

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
)

// Simple struct for embedding in every
// token except the expand token to mark them
// as not an expand token.
// This is gross, but we need to check for expands
// when evaluating command args, and a type check is
// measurably slower than a method call.
// This is probably not worth the complexity.
type notExpand struct{}

func (ne notExpand) isExpand() bool {
	return false
}

type tliteral struct {
	notExpand
	strval string
	tval   *TclObj // cached
	loc    loc
}

func (l *tliteral) AsTclObj() *TclObj {
	if l.tval == nil {
		l.tval = FromStrLoc(l.strval, l.loc)
	}
	return l.tval
}

func (l *tliteral) String() string { return l.strval }
func (l *tliteral) Eval(i *Interp) TclStatus {
	i.loc = l.loc
	if l.tval == nil {
		l.tval = FromStrLoc(l.strval, l.loc)
	}
	i.retval = l.tval
	return kTclOK
}

// [w1 w2...]
type subcommand struct {
	notExpand
	cmd command
	loc loc
}

func (s *subcommand) String() string { return "[" + s.cmd.String() + "]" }
func (s *subcommand) Eval(i *Interp) TclStatus {
	i.loc = s.loc
	return s.cmd.eval(i)
}

// { ... }
type block struct {
	notExpand
	strval string
	tval   *TclObj
	loc    loc
}

func (b *block) String() string { return "{" + b.strval + "}" }

func (b *block) AsTclObj() *TclObj {
	if b.tval == nil {
		b.tval = FromStrLoc(b.strval, b.loc)
	}
	return b.tval
}

func (b *block) Eval(i *Interp) TclStatus {
	i.loc = b.loc
	if b.tval == nil {
		b.tval = FromStrLoc(b.strval, b.loc)
	}
	return i.Return(b.tval)
}

// {*}{...}
type expandTok struct {
	subject tclTok
	loc     loc
}

func (e *expandTok) isExpand() bool {
	return true
}

func (e *expandTok) Eval(i *Interp) TclStatus {
	i.loc = e.loc
	return e.subject.Eval(i)
}

func (e *expandTok) String() string {
	return "{*}" + e.subject.String()
}

// "..."
type strlit struct {
	notExpand
	toks []littok
	loc  loc
}

const (
	kRaw = iota
	kVar
	kSubcmd
)

type littok struct {
	kind   int
	value  string
	varref *varRef
	subcmd *subcommand
}

func (lt *littok) evalStr(i *Interp) (string, TclStatus) {
	switch lt.kind {
	case kRaw:
		return lt.value, kTclOK
	case kVar:
		rc := lt.varref.Eval(i)
		return i.retval.AsString(), rc
	case kSubcmd:
		rc := lt.subcmd.Eval(i)
		return i.retval.AsString(), rc
	}
	panic("unrecognized kind")
}

func (t strlit) String() string {
	var res bytes.Buffer
	res.WriteString(`"`)
	for _, tok := range t.toks {
		if tok.kind == kRaw {
			res.WriteString(tok.value)
		} else if tok.kind == kVar {
			res.WriteString(tok.varref.String())
		} else if tok.kind == kSubcmd {
			res.WriteString(tok.subcmd.String())
		}
	}
	res.WriteString(`"`)
	return res.String()
}

func (t strlit) Eval(i *Interp) TclStatus {
	i.loc = t.loc
	var res bytes.Buffer
	for _, tok := range t.toks {
		s, rc := tok.evalStr(i)
		if rc != kTclOK {
			return rc
		}
		res.WriteString(s)
	}
	return i.Return(FromStrLoc(res.String(), t.loc))
}

// $...
type varRef struct {
	notExpand
	is_global bool
	name      string
	arrind    tclTok
	loc       loc
}

func (v varRef) Eval(i *Interp) TclStatus {
	i.loc = v.loc
	x, e := i.getVar(v)
	if e != nil {
		return i.Fail(e)
	}
	i.retval = x
	return kTclOK
}

func (v varRef) String() string {
	str := v.name
	if v.is_global {
		str = "::" + str
	}
	return "$" + str
}

func toVarRef(s string) varRef {
	global := false
	if strings.HasPrefix(s, "::") {
		s = s[2:]
		global = true
	}
	if s[len(s)-1] == ')' {
		ri := strings.IndexRune(s, '(')
		if ri > 0 {
			ind := &tliteral{strval: s[ri+1 : len(s)-1]}
			s = s[0:ri]
			return varRef{name: s, is_global: global, arrind: ind}
		}
	}
	return varRef{name: s, is_global: global}
}

type simpleCall struct {
	cmdname string
	args    []*TclObj
}

// w1 w2...
type command struct {
	words     []tclTok
	no_expand bool
	simple    *simpleCall
}

// a simpleTok is a token that won't change.
// As such, we can get it's TclObj value without
// regard to interpreter state. This is used to
// cache evaluated arguments.
type simpleTok interface {
	AsTclObj() *TclObj
}

func makeCommand(words []tclTok) command {
	all_simpletok := true
	has_expand := false
	var simple *simpleCall
	for _, w := range words {
		if all_simpletok {
			if _, ok := w.(simpleTok); !ok {
				all_simpletok = false
			}
		}
		has_expand = has_expand || w.isExpand()

	}
	if all_simpletok && len(words) > 0 {
		args := make([]*TclObj, len(words))
		for i := range args {
			args[i] = words[i].(simpleTok).AsTclObj()
		}
		simple = &simpleCall{cmdname: args[0].AsString(), args: args[1:]}
	}
	return command{words: words, simple: simple, no_expand: !has_expand}
}

func (c *command) String() string {
	result := ""
	first := true
	for _, w := range c.words {
		if first {
			first = false
		} else {
			result += " "
		}
		result += w.String()
	}
	return result
}

type tclTok interface {
	String() string
	Eval(i *Interp) TclStatus
	isExpand() bool
}

type TclStatus int

const (
	kTclOK TclStatus = iota
	kTclErr
	kTclReturn
	kTclBreak
	kTclContinue
)

type framelink struct {
	frame *stackframe
	name  string
}

type varEntry struct {
	obj     *TclObj
	link    *framelink
	arrdata map[string]*TclObj
}

type varMap map[string]*varEntry

type stackframe struct {
	vars varMap
	next *stackframe
}

func newstackframe(tail *stackframe) *stackframe {
	return &stackframe{make(varMap), tail}
}

type Interp struct {
	cmds     map[string]TclCmd
	chans    map[string]interface{}
	frame    *stackframe
	retval   *TclObj
	err      error
	cmdcount int
	file     string
	loc      loc
}

func (i *Interp) Return(val *TclObj) TclStatus {
	i.retval = val
	return kTclOK
}

func (i *Interp) Fail(err error) TclStatus {
	// i.err = fmt.Errorf("%v: %v", i.loc, err)
	i.err = err
	return kTclErr
}

func (i *Interp) FailStr(msg string) TclStatus {
	return i.Fail(errors.New(msg))
}

type TclObj struct {
	value      *string
	intval     int
	has_intval bool
	listval    []*TclObj
	cmdsval    []command
	vrefval    *varRef
	exprval    eterm
	loc        loc
}

func (t *TclObj) AsString() string {
	if t.value == nil {
		if t.has_intval {
			v := strconv.Itoa(t.intval)
			t.value = &v
		} else if t.listval != nil {
			var str bytes.Buffer
			for ind, i := range t.listval {
				if ind != 0 {
					str.WriteString(" ")
				}
				sv := i.AsString()
				should_bracket := strings.IndexAny(sv, " \t\n\v") != -1 || len(sv) == 0
				if should_bracket {
					str.WriteString("{")
				}
				str.WriteString(sv)
				if should_bracket {
					str.WriteString("}")
				}
			}
			ss := str.String()
			t.value = &ss
		} else {
			return ""
		}
	}
	return *t.value
}

func (t *TclObj) AsInt() (int, error) {
	if !t.has_intval {
		v, e := strconv.Atoi(*t.value)
		if e != nil {
			return 0, errors.New("expected integer but got \"" + *t.value + "\"")
		}
		t.has_intval = true
		t.intval = v
	}
	return t.intval, nil
}

func (t *TclObj) asCmds() ([]command, error) {
	if t.cmdsval == nil {
		c, e := parseCommands(strings.NewReader(t.AsString()), loc{"<cmds>", 0, 0})
		if e != nil {
			return nil, e
		}
		t.cmdsval = c
	}
	return t.cmdsval, nil
}

func (t *TclObj) AsBool() bool {
	iv, err := t.AsInt()
	if err != nil {
		s := t.AsString()
		return s != "false" && s != "no"
	}
	return iv != 0
}

func (t *TclObj) asVarRef() varRef {
	if t.vrefval == nil {
		vr := toVarRef(t.AsString())
		t.vrefval = &vr
	}
	return *t.vrefval
}

func FromStrLoc(s string, loc loc) *TclObj {
	return &TclObj{value: &s, loc: loc}
}

func FromStr(s string) *TclObj {
	return &TclObj{value: &s}
}

var kTrue, kFalse *TclObj
var smallInts [256]TclObj

func init() {
	for i := range smallInts {
		smallInts[i] = TclObj{intval: i, has_intval: true}
	}
	kTrue = FromInt(1)
	kFalse = FromInt(0)
}

func FromInt(i int) *TclObj {
	if i >= 0 && i < len(smallInts) {
		return &smallInts[i]
	}
	return &TclObj{intval: i, has_intval: true}
}

func FromList(l []string) *TclObj {
	vl := make([]*TclObj, len(l))
	for i, s := range l {
		vl[i] = FromStr(s)
	}
	return fromList(vl)
}

func FromIntList(l []int) *TclObj {
	vl := make([]*TclObj, len(l))
	for i, s := range l {
		vl[i] = FromInt(s)
	}
	return fromList(vl)
}

var kNil = FromStr("")

func FromBool(b bool) *TclObj {
	if b {
		return kTrue
	}
	return kFalse
}

func fromList(items []*TclObj) *TclObj { return &TclObj{listval: items} }

func (t *TclObj) AsList() ([]*TclObj, error) {
	if t.listval == nil {
		var e error
		t.listval, e = parseList(t.AsString())
		if e != nil {
			return nil, e
		}
	}
	return t.listval, nil
}

func (t *TclObj) asExpr() (eterm, error) {
	if t.exprval == nil {
		ev, err := parseExpr(strings.NewReader(t.AsString()), t.loc)
		if err != nil {
			return nil, err
		}
		t.exprval = ev
	}
	return t.exprval, nil
}

func parseList(txt string) ([]*TclObj, error) {
	lst, err := parseListInner(strings.NewReader(txt), loc{"<list>", 0, 0})
	if err != nil {
		return nil, err
	}
	result := make([]*TclObj, len(lst))
	for i, s := range lst {
		result[i] = FromStr(s)
	}
	return result, nil
}

func (i *Interp) EvalObj(obj *TclObj) TclStatus {
	cmds, e := obj.asCmds()
	if e != nil {
		return i.Fail(e)
	}
	return i.evalCmds(cmds)
}

type argsig struct {
	name string
	def  *TclObj
}

func (i *Interp) bindArgs(vnames []argsig, args []*TclObj) error {
	lastind := len(vnames) - 1
	var vr varRef
	for ix, vn := range vnames {
		vr.name = vn.name
		if ix == lastind && vn.name == "args" {
			i.setVar(vr, fromList(args[ix:]))
			return nil
		} else if ix >= len(args) {
			if vn.def == nil {
				return errors.New("arg count mismatch")
			}
			i.setVar(vr, vn.def)
		} else {
			i.setVar(vr, args[ix])
		}
	}
	return nil
}

func makeArgSigs(sig []*TclObj) []argsig {
	sigs := make([]argsig, len(sig))
	for i, a := range sig {
		sl, lerr := a.AsList()
		if lerr == nil && len(sl) == 2 {
			sigs[i] = argsig{sl[0].AsString(), sl[1]}
		} else {
			sigs[i] = argsig{name: a.AsString()}
		}
	}
	return sigs
}

func makeProc(sig []*TclObj, body *TclObj) TclCmd {
	cmds, ce := body.asCmds()
	if ce != nil {
		return func(i *Interp, args []*TclObj) TclStatus { return i.Fail(ce) }
	}
	sigs := makeArgSigs(sig)
	return func(i *Interp, args []*TclObj) TclStatus {
		i.frame = newstackframe(i.frame)
		if be := i.bindArgs(sigs, args); be != nil {
			i.frame = i.frame.next
			return i.Fail(be)
		}
		rc := i.evalCmds(cmds)
		if rc == kTclReturn {
			rc = kTclOK
		}
		i.frame = i.frame.next
		return rc
	}
}

func tclProc(i *Interp, args []*TclObj) TclStatus {
	if len(args) != 3 {
		return i.FailStr("wrong # args")
	}
	sig, err := args[1].AsList()
	if err != nil {
		return i.Fail(err)
	}
	i.SetCmd(args[0].AsString(), makeProc(sig, args[2]))
	return i.Return(kNil)
}

var tclStdin = bufio.NewReader(os.Stdin)

func NewInterp() *Interp {
	i := new(Interp)
	i.cmds = make(map[string]TclCmd)
	i.frame = newstackframe(nil)
	i.chans = make(map[string]interface{})
	i.chans["stdin"] = tclStdin
	i.chans["stdout"] = os.Stdout
	i.chans["stderr"] = os.Stderr

	for n, f := range tclBasicCmds {
		i.SetCmd(n, f)
	}

	i.SetCmd("proc", tclProc)
	i.SetCmd("error", func(ni *Interp, args []*TclObj) TclStatus { return i.FailStr(args[0].AsString()) })
	return i
}

func NewInterpFrom(old *Interp) *Interp {
	i := new(Interp)
	i.cmds = old.cmds
	i.frame = newstackframe(nil)
	i.chans = make(map[string]interface{})
	i.chans["stdin"] = tclStdin
	i.chans["stdout"] = os.Stdout
	i.chans["stderr"] = os.Stderr
	return i
}

func (i *Interp) SetSource(file string) {
	i.file = file
}

type TclCmd func(*Interp, []*TclObj) TclStatus

func (i *Interp) SetCmd(name string, cmd TclCmd) {
	if cmd == nil {
		delete(i.cmds, name)
	} else {
		i.cmds[name] = cmd
	}
}

func (i *Interp) evalCmds(cmds []command) TclStatus {
	res := kTclOK
	for ind := 0; ind < len(cmds) && res == kTclOK; ind++ {
		res = cmds[ind].eval(i)
	}
	return res
}

func (i *Interp) getVarMap(global bool) varMap {
	f := i.frame
	if global {
		for f.next != nil {
			f = f.next
		}
	}
	return f.vars
}

func (i *Interp) LinkVar(level int, theirs, mine string) {
	theirf := i.frame
	for level > 0 {
		theirf = theirf.next
		level--
	}
	m := i.getVarMap(false)
	m[mine] = &varEntry{link: &framelink{theirf, theirs}}
}

func (i *Interp) SetVarRaw(name string, val *TclObj) {
	i.setVar(toVarRef(name), val)
}

func (i *Interp) setVar(vr varRef, val *TclObj) TclStatus {
	m := i.getVarMap(vr.is_global)
	if val == nil {
		delete(m, vr.name)
		return kTclOK
	}
	n := vr.name
	old, ok := m[n]
	for ok && old != nil && old.link != nil {
		m = old.link.frame.vars
		n = old.link.name
		old, ok = m[n]
	}
	if old == nil {
		old = &varEntry{}
		m[n] = old
		if vr.arrind != nil {
			old.arrdata = make(map[string]*TclObj)
		}
	} else {
		if vr.arrind != nil && old.arrdata == nil {
			return i.FailStr("can't set: variable is not an array")
		}
	}
	if vr.arrind != nil {
		rc := vr.arrind.Eval(i)
		if rc != kTclOK {
			return rc
		}
		sind := i.retval.AsString()
		old.arrdata[sind] = val
	} else {
		old.obj = val
	}
	i.retval = val
	return kTclOK
}

func (i *Interp) GetVarRaw(name string) (*TclObj, error) {
	return i.getVar(toVarRef(name))
}

func (i *Interp) getArray(vr varRef) (*varEntry, error) {
	v, ok := i.getVarMap(vr.is_global)[vr.name]
	if !ok {
		return nil, errors.New("variable not found: " + vr.String())
	}
	for v.link != nil {
		v, ok = v.link.frame.vars[v.link.name]
		if !ok {
			return nil, errors.New("variable not found: " + vr.String())
		}
	}
	if v.arrdata == nil {
		return nil, errors.New("not an array")
	}
	return v, nil
}

func (i *Interp) getVar(vr varRef) (*TclObj, error) {
	v, ok := i.getVarMap(vr.is_global)[vr.name]
	if !ok {
		return nil, errors.New("variable not found: " + vr.String())
	}
	for v.link != nil {
		v, ok = v.link.frame.vars[v.link.name]
		if !ok {
			return nil, errors.New("variable not found: " + vr.String())
		}
	}
	if vr.arrind != nil {
		if v.arrdata == nil {
			return nil, errors.New("can't get: variable isn't array")
		}
		if rc := vr.arrind.Eval(i); rc != kTclOK {
			return nil, i.err
		}
		sind := i.retval.AsString()
		elt, ok := v.arrdata[sind]
		if !ok {
			return nil, errors.New("can't read " + sind + ": no such element in array")
		}
		return elt, nil
	}
	if v.arrdata != nil {
		return nil, errors.New("can't get: variable is array")
	}
	return v.obj, nil
}

func evalArgs(i *Interp, toks []tclTok, no_expand bool) ([]*TclObj, TclStatus) {
	res := make([]*TclObj, 0, len(toks))
	rc := kTclOK
	for _, t := range toks {
		rc = t.Eval(i)
		if rc != kTclOK {
			break
		}
		if no_expand || !t.isExpand() {
			res = append(res, i.retval)
		} else {
			rlist, e := i.retval.AsList()
			if e != nil {
				i.err = e
				return nil, kTclErr
			}
			res = append(res, rlist...)
		}
	}
	return res, rc
}

func (i *Interp) ClearError() { i.err = nil }

func (cmd command) eval(i *Interp) TclStatus {
	i.cmdcount++
	if len(cmd.words) == 0 {
		return i.Return(kNil)
	}
	if cmd.simple != nil {
		if f, ok := i.cmds[cmd.simple.cmdname]; ok {
			return f(i, cmd.simple.args)
		}
	}
	args, rc := evalArgs(i, cmd.words, cmd.no_expand)
	if rc != kTclOK {
		return rc
	}
	fname := args[0].AsString()
	if f, ok := i.cmds[fname]; ok {
		return f(i, args[1:])
	}
	if f, ok := i.cmds["unknown"]; ok {
		return f(i, args)
	}
	return i.FailStr("command not found: " + fname)
}

func (i *Interp) EvalString(s string) (*TclObj, error) {
	return i.Run(strings.NewReader(s))
}

func (i *Interp) Run(in io.Reader) (*TclObj, error) {
	cmds, e := parseCommands(bufio.NewReader(in), loc{i.file, 0, 0})
	if e != nil {
		return nil, e
	}
	r := i.evalCmds(cmds)
	if r == kTclOK || r == kTclReturn {
		if i.retval == nil {
			return kNil, nil
		}
		return i.retval, nil
	}
	if i.err == nil {
		var estr string
		switch r {
		case kTclBreak:
			estr = `invoked "break" outside of a loop`
		case kTclContinue:
			estr = `invoked "continue" outside of a loop`
		default:
			estr = "uncaught error: " + strconv.Itoa(int(r))

		}
		i.err = errors.New(estr)
	}
	return nil, i.err
}
