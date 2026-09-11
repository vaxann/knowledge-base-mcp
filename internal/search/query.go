package search

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Expr is a boolean predicate over note metadata.
type Expr interface {
	Eval(n *NoteMeta) bool
}

type andExpr struct{ parts []Expr }
type orExpr struct{ parts []Expr }
type notExpr struct{ inner Expr }
type cmpExpr struct {
	field string
	op    string
	value any
}

func (e andExpr) Eval(n *NoteMeta) bool {
	for _, p := range e.parts {
		if !p.Eval(n) {
			return false
		}
	}
	return true
}

func (e orExpr) Eval(n *NoteMeta) bool {
	for _, p := range e.parts {
		if p.Eval(n) {
			return true
		}
	}
	return false
}

func (e notExpr) Eval(n *NoteMeta) bool { return !e.inner.Eval(n) }

// FieldValue returns a note field: path, folder, title, tags, modified, size,
// or a frontmatter key.
func FieldValue(n *NoteMeta, field string) (any, bool) {
	switch field {
	case "path":
		return n.Path, true
	case "folder":
		d := path.Dir(n.Path)
		if d == "." {
			d = ""
		}
		return d, true
	case "title":
		return n.Title, true
	case "tags":
		out := make([]any, len(n.Tags))
		for i, t := range n.Tags {
			out[i] = t
		}
		return out, true
	case "modified":
		return n.ModTime, true
	case "size":
		return n.Size, true
	}
	if n.Frontmatter == nil {
		return nil, false
	}
	v, ok := n.Frontmatter[field]
	return v, ok && v != nil
}

func (e cmpExpr) Eval(n *NoteMeta) bool {
	v, ok := FieldValue(n, e.field)
	switch e.op {
	case "exists":
		want := true
		if b, isB := e.value.(bool); isB {
			want = b
		}
		return ok == want
	}
	if !ok {
		return false
	}
	switch e.op {
	case "eq":
		return equalAny(v, e.value)
	case "ne":
		return !equalAny(v, e.value)
	case "contains":
		return containsAny(v, e.value)
	case "in":
		list, _ := e.value.([]any)
		for _, item := range list {
			if equalAny(v, item) {
				return true
			}
		}
		return false
	case "gt", "gte", "lt", "lte":
		c, ok := compareAny(v, e.value)
		if !ok {
			return false
		}
		switch e.op {
		case "gt":
			return c > 0
		case "gte":
			return c >= 0
		case "lt":
			return c < 0
		default:
			return c <= 0
		}
	}
	return false
}

func equalAny(a, b any) bool {
	if list, ok := a.([]any); ok {
		for _, item := range list {
			if equalAny(item, b) {
				return true
			}
		}
		return false
	}
	if c, ok := compareAny(a, b); ok {
		return c == 0
	}
	return strings.EqualFold(strings.TrimSpace(fmt.Sprint(a)), strings.TrimSpace(fmt.Sprint(b)))
}

func containsAny(a, b any) bool {
	switch x := a.(type) {
	case []any:
		for _, item := range x {
			if equalAny(item, b) {
				return true
			}
		}
		return false
	case string:
		return strings.Contains(strings.ToLower(x), strings.ToLower(fmt.Sprint(b)))
	default:
		return equalAny(a, b)
	}
}

// compareAny compares dates, numbers, or falls back to strings.
func compareAny(a, b any) (int, bool) {
	if ta, ok := asTime(a); ok {
		if tb, ok := asTime(b); ok {
			switch {
			case ta.Before(tb):
				return -1, true
			case ta.After(tb):
				return 1, true
			}
			return 0, true
		}
		return 0, false
	}
	if fa, ok := asFloat(a); ok {
		if fb, ok := asFloat(b); ok {
			switch {
			case fa < fb:
				return -1, true
			case fa > fb:
				return 1, true
			}
			return 0, true
		}
		return 0, false
	}
	sa, sb := strings.ToLower(fmt.Sprint(a)), strings.ToLower(fmt.Sprint(b))
	return strings.Compare(sa, sb), true
}

func asTime(v any) (time.Time, bool) {
	switch x := v.(type) {
	case time.Time:
		return x, true
	case string:
		for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02 15:04", "2006-01-02T15:04"} {
			if t, err := time.Parse(layout, strings.TrimSpace(x)); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

// ParseWhere parses the small predicate language:
//
//	type eq "document" and (persons contains "Alice" or not status exists)
//
// Operators: eq ne in contains exists gt gte lt lte; combinators and/or/not;
// values: "strings", numbers, YYYY-MM-DD dates, true/false, [lists].
func ParseWhere(s string) (Expr, error) {
	if strings.TrimSpace(s) == "" {
		return andExpr{}, nil
	}
	p := &parser{toks: tokenize(s)}
	e, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.toks) {
		return nil, fmt.Errorf("unexpected %q", p.toks[p.pos])
	}
	return e, nil
}

type parser struct {
	toks []string
	pos  int
}

func tokenize(s string) []string {
	var toks []string
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case c == '(' || c == ')' || c == '[' || c == ']' || c == ',':
			toks = append(toks, string(c))
			i++
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(s) && s[j] != c {
				j++
			}
			toks = append(toks, s[i:min(j+1, len(s))])
			i = j + 1
		default:
			j := i
			for j < len(s) && !strings.ContainsRune(" \t\n()[],", rune(s[j])) {
				j++
			}
			toks = append(toks, s[i:j])
			i = j
		}
	}
	return toks
}

func (p *parser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *parser) next() string {
	t := p.peek()
	p.pos++
	return t
}

func (p *parser) parseOr() (Expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	parts := []Expr{left}
	for strings.EqualFold(p.peek(), "or") {
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		parts = append(parts, right)
	}
	if len(parts) == 1 {
		return left, nil
	}
	return orExpr{parts}, nil
}

func (p *parser) parseAnd() (Expr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	parts := []Expr{left}
	for strings.EqualFold(p.peek(), "and") {
		p.next()
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		parts = append(parts, right)
	}
	if len(parts) == 1 {
		return left, nil
	}
	return andExpr{parts}, nil
}

func (p *parser) parseNot() (Expr, error) {
	if strings.EqualFold(p.peek(), "not") {
		p.next()
		inner, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return notExpr{inner}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (Expr, error) {
	if p.peek() == "(" {
		p.next()
		e, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.next() != ")" {
			return nil, fmt.Errorf("expected )")
		}
		return e, nil
	}
	field := p.next()
	if field == "" {
		return nil, fmt.Errorf("expected field name")
	}
	op := strings.ToLower(p.next())
	if sym, ok := map[string]string{"=": "eq", "==": "eq", "!=": "ne", ">": "gt", ">=": "gte", "<": "lt", "<=": "lte"}[op]; ok {
		op = sym
	}
	switch op {
	case "exists":
		return cmpExpr{field: field, op: op, value: true}, nil
	case "eq", "ne", "in", "contains", "gt", "gte", "lt", "lte":
	default:
		return nil, fmt.Errorf("unknown operator %q after %q", op, field)
	}
	val, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	return cmpExpr{field: field, op: op, value: val}, nil
}

func (p *parser) parseValue() (any, error) {
	t := p.next()
	switch {
	case t == "":
		return nil, fmt.Errorf("expected value")
	case t == "[":
		var list []any
		for p.peek() != "]" && p.peek() != "" {
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			list = append(list, v)
			if p.peek() == "," {
				p.next()
			}
		}
		if p.next() != "]" {
			return nil, fmt.Errorf("expected ]")
		}
		return list, nil
	case len(t) >= 2 && (t[0] == '"' || t[0] == '\'') && t[len(t)-1] == t[0]:
		return t[1 : len(t)-1], nil
	case strings.EqualFold(t, "true"):
		return true, nil
	case strings.EqualFold(t, "false"):
		return false, nil
	}
	if f, err := strconv.ParseFloat(t, 64); err == nil {
		return f, nil
	}
	return t, nil
}

// QueryRequest is a structured metadata query.
type QueryRequest struct {
	Where  string
	Select []string
	Sort   string // "field asc|desc"
	Limit  int
}

// Row is one query result.
type Row map[string]any

// RunQuery evaluates a query over the catalog.
func (c *Catalog) RunQuery(req QueryRequest) ([]Row, error) {
	expr, err := ParseWhere(req.Where)
	if err != nil {
		return nil, err
	}
	var matched []*NoteMeta
	for _, n := range c.Notes() {
		if expr.Eval(n) {
			matched = append(matched, n)
		}
	}
	if req.Sort != "" {
		f := strings.Fields(req.Sort)
		field := f[0]
		desc := len(f) > 1 && strings.EqualFold(f[1], "desc")
		sort.SliceStable(matched, func(i, j int) bool {
			a, aok := FieldValue(matched[i], field)
			b, bok := FieldValue(matched[j], field)
			if !aok || !bok {
				return aok && !bok
			}
			cmp, _ := compareAny(a, b)
			if desc {
				return cmp > 0
			}
			return cmp < 0
		})
	}
	if req.Limit > 0 && len(matched) > req.Limit {
		matched = matched[:req.Limit]
	}
	rows := make([]Row, 0, len(matched))
	for _, n := range matched {
		row := Row{"path": n.Path}
		if len(req.Select) == 0 {
			row["title"] = n.Title
			for k, v := range n.Frontmatter {
				row[k] = v
			}
		} else {
			for _, f := range req.Select {
				if v, ok := FieldValue(n, f); ok {
					row[f] = v
				}
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}
