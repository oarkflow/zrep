package query

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type Record map[string]string

type Predicate interface {
	Match(Record) bool
}

type predicateFunc func(Record) bool

func (f predicateFunc) Match(r Record) bool { return f(r) }

func Always() Predicate { return predicateFunc(func(Record) bool { return true }) }

func FieldEquals(field, value string) Predicate {
	field = strings.ToLower(strings.TrimSpace(field))
	value = strings.TrimSpace(value)
	return predicateFunc(func(r Record) bool {
		return strings.EqualFold(r[field], value)
	})
}

func And(preds ...Predicate) Predicate {
	return predicateFunc(func(r Record) bool {
		for _, pred := range preds {
			if pred != nil && !pred.Match(r) {
				return false
			}
		}
		return true
	})
}

func Parse(expr string) (Predicate, error) {
	tokens := tokenize(expr)
	if len(tokens) == 0 {
		return Always(), nil
	}
	p := parser{tokens: tokens}
	pred, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(tokens) {
		return nil, fmt.Errorf("unexpected token %q", tokens[p.pos])
	}
	return pred, nil
}

type parser struct {
	tokens []string
	pos    int
}

func (p *parser) parseOr() (Predicate, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek("OR") {
		p.pos++
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		prev := left
		left = predicateFunc(func(r Record) bool { return prev.Match(r) || right.Match(r) })
	}
	return left, nil
}

func (p *parser) parseAnd() (Predicate, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.peek("AND") {
		p.pos++
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		prev := left
		left = predicateFunc(func(r Record) bool { return prev.Match(r) && right.Match(r) })
	}
	return left, nil
}

func (p *parser) parseUnary() (Predicate, error) {
	if p.peek("NOT") {
		p.pos++
		child, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return predicateFunc(func(r Record) bool { return !child.Match(r) }), nil
	}
	if p.peek("(") {
		p.pos++
		child, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if !p.peek(")") {
			return nil, fmt.Errorf("missing closing parenthesis")
		}
		p.pos++
		return child, nil
	}
	return p.parseComparison()
}

func (p *parser) parseComparison() (Predicate, error) {
	if p.pos >= len(p.tokens) {
		return nil, fmt.Errorf("expected expression")
	}
	field := normalizeField(p.tokens[p.pos])
	p.pos++
	if p.pos >= len(p.tokens) {
		term := strings.ToLower(field)
		return predicateFunc(func(r Record) bool {
			for _, value := range r {
				if strings.Contains(strings.ToLower(value), term) {
					return true
				}
			}
			return false
		}), nil
	}
	op := p.tokens[p.pos]
	if !isOperator(op) {
		term := strings.ToLower(field)
		return predicateFunc(func(r Record) bool {
			for _, value := range r {
				if strings.Contains(strings.ToLower(value), term) {
					return true
				}
			}
			return false
		}), nil
	}
	p.pos++
	if p.pos >= len(p.tokens) {
		return nil, fmt.Errorf("missing value after %s", op)
	}
	value := unquote(p.tokens[p.pos])
	p.pos++
	return comparison(field, op, value), nil
}

func comparison(field, op, value string) Predicate {
	return predicateFunc(func(r Record) bool {
		got := r[field]
		switch strings.ToUpper(op) {
		case "=", "==":
			return strings.EqualFold(got, value)
		case "!=", "<>":
			return !strings.EqualFold(got, value)
		case "~", "CONTAINS":
			return strings.Contains(strings.ToLower(got), strings.ToLower(value))
		case "=~":
			re, err := regexp.Compile(value)
			return err == nil && re.MatchString(got)
		case ">", ">=", "<", "<=":
			a, aErr := strconv.ParseFloat(got, 64)
			b, bErr := strconv.ParseFloat(value, 64)
			if aErr != nil || bErr != nil {
				cmp := strings.Compare(got, value)
				switch op {
				case ">":
					return cmp > 0
				case ">=":
					return cmp >= 0
				case "<":
					return cmp < 0
				case "<=":
					return cmp <= 0
				}
			}
			switch op {
			case ">":
				return a > b
			case ">=":
				return a >= b
			case "<":
				return a < b
			case "<=":
				return a <= b
			}
		}
		return false
	})
}

func (p *parser) peek(token string) bool {
	return p.pos < len(p.tokens) && strings.EqualFold(p.tokens[p.pos], token)
}

func isOperator(token string) bool {
	switch strings.ToUpper(token) {
	case "=", "==", "!=", "<>", ">", ">=", "<", "<=", "~", "=~", "CONTAINS":
		return true
	default:
		return false
	}
}

func tokenize(expr string) []string {
	var out []string
	var b strings.Builder
	quote := rune(0)
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for i := 0; i < len(expr); i++ {
		r := rune(expr[i])
		if quote != 0 {
			b.WriteRune(r)
			if r == quote {
				quote = 0
				flush()
			}
			continue
		}
		switch {
		case r == '\'' || r == '"':
			flush()
			quote = r
			b.WriteRune(r)
		case r == '(' || r == ')':
			flush()
			out = append(out, string(r))
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		case strings.ContainsRune("=!<>~", r):
			flush()
			op := string(r)
			if i+1 < len(expr) && (expr[i+1] == '=' || r == '<' && expr[i+1] == '>') {
				op += string(expr[i+1])
				i++
			}
			out = append(out, op)
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return out
}

func normalizeField(field string) string {
	field = strings.TrimSpace(field)
	field = strings.TrimPrefix(field, ".")
	return strings.ToLower(field)
}

func unquote(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'' {
			return value[1 : len(value)-1]
		}
	}
	return value
}
