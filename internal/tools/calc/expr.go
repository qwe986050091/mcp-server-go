package calc

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// 运算符优先级（由低到高）：+ -  <  * / %  <  一元 ±  <  ^（右结合）。
//
// 刻意实现为「词法 + 递归下降」而不是调用任何表达式求值库：
// 既避免引入依赖，也从结构上杜绝 eval 类注入，且不接受任何标识符变量。

type tokenKind int

const (
	tokNumber tokenKind = iota
	tokIdent
	tokOperator
	tokLParen
	tokRParen
	tokComma
	tokEOF
)

type token struct {
	kind tokenKind
	text string
	num  float64
	pos  int // 从 0 开始的字节下标，报错时 +1 转成人类可读位置
}

// MaxExpressionLength 是表达式长度上限，用于阻断病态输入。
const MaxExpressionLength = 2000

// constants 是内置常量。
var constants = map[string]float64{
	"pi": math.Pi,
	"e":  math.E,
}

// Eval 对表达式求值。
func Eval(src string) (float64, error) {
	if len(src) > MaxExpressionLength {
		return 0, fmt.Errorf("表达式过长（%d 字符，上限 %d）", len(src), MaxExpressionLength)
	}
	toks, err := lex(src)
	if err != nil {
		return 0, err
	}
	p := &parser{toks: toks}
	v, err := p.parseExpr()
	if err != nil {
		return 0, err
	}
	if tok := p.peek(); tok.kind != tokEOF {
		return 0, fmt.Errorf("位置 %d 起有多余输入 %q", tok.pos+1, tok.text)
	}
	if math.IsNaN(v) {
		return 0, errors.New("计算结果不是有效数字（NaN）")
	}
	if math.IsInf(v, 0) {
		return 0, errors.New("计算结果溢出（Inf）")
	}
	return v, nil
}

// FormatNumber 以最短精确形式输出数值。
func FormatNumber(v float64) string {
	if v == 0 {
		return "0" // 同时把 -0 归一为 0
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func lex(src string) ([]token, error) {
	out := make([]token, 0, 16)
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case isDigit(c) || c == '.':
			tok, next, err := lexNumber(src, i)
			if err != nil {
				return nil, err
			}
			out = append(out, tok)
			i = next
		case isLetter(c):
			j := i
			for j < len(src) && (isLetter(src[j]) || isDigit(src[j]) || src[j] == '_') {
				j++
			}
			out = append(out, token{kind: tokIdent, text: strings.ToLower(src[i:j]), pos: i})
			i = j
		case strings.IndexByte("+-*/%^", c) >= 0:
			out = append(out, token{kind: tokOperator, text: string(c), pos: i})
			i++
		case c == '(':
			out = append(out, token{kind: tokLParen, text: "(", pos: i})
			i++
		case c == ')':
			out = append(out, token{kind: tokRParen, text: ")", pos: i})
			i++
		case c == ',':
			out = append(out, token{kind: tokComma, text: ",", pos: i})
			i++
		default:
			return nil, fmt.Errorf("位置 %d 出现非法字符 %q", i+1, string(c))
		}
	}
	out = append(out, token{kind: tokEOF, text: "", pos: len(src)})
	return out, nil
}

// lexNumber 读取数字字面量，支持 123 / 1.5 / .5 / 1e-3。
// 关键点：只有当 'e'/'E' 后面确实跟着（可带符号的）数字时才算科学计数法，
// 否则 'e' 应留给常量 e 使用（例如 "2e" 应解析为 2*e… 实际上这里按「2 后跟标识符 e」处理，
// 由于缺少运算符会报语法错误，比默默算错更安全）。
func lexNumber(src string, start int) (token, int, error) {
	j := start
	for j < len(src) && isDigit(src[j]) {
		j++
	}
	if j < len(src) && src[j] == '.' {
		j++
		for j < len(src) && isDigit(src[j]) {
			j++
		}
	}
	if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
		k := j + 1
		if k < len(src) && (src[k] == '+' || src[k] == '-') {
			k++
		}
		if k < len(src) && isDigit(src[k]) {
			for k < len(src) && isDigit(src[k]) {
				k++
			}
			j = k
		}
	}
	text := src[start:j]
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return token{}, 0, fmt.Errorf("位置 %d 附近的数字 %q 非法", start+1, text)
	}
	return token{kind: tokNumber, text: text, num: v, pos: start}, j, nil
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token { return p.toks[p.pos] }

func (p *parser) next() token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

// parseExpr := term (('+' | '-') term)*
func (p *parser) parseExpr() (float64, error) {
	left, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for {
		t := p.peek()
		if t.kind != tokOperator || (t.text != "+" && t.text != "-") {
			return left, nil
		}
		p.next()
		right, err := p.parseTerm()
		if err != nil {
			return 0, err
		}
		if t.text == "+" {
			left += right
		} else {
			left -= right
		}
	}
}

// parseTerm := unary (('*' | '/' | '%') unary)*
func (p *parser) parseTerm() (float64, error) {
	left, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	for {
		t := p.peek()
		if t.kind != tokOperator || (t.text != "*" && t.text != "/" && t.text != "%") {
			return left, nil
		}
		p.next()
		right, err := p.parseUnary()
		if err != nil {
			return 0, err
		}
		switch t.text {
		case "*":
			left *= right
		case "/":
			if right == 0 {
				return 0, fmt.Errorf("位置 %d 处除数为零", t.pos+1)
			}
			left /= right
		case "%":
			if right == 0 {
				return 0, fmt.Errorf("位置 %d 处取余的除数为零", t.pos+1)
			}
			left = math.Mod(left, right)
		}
	}
}

// parseUnary := ('+' | '-') unary | power
func (p *parser) parseUnary() (float64, error) {
	t := p.peek()
	if t.kind == tokOperator && (t.text == "+" || t.text == "-") {
		p.next()
		v, err := p.parseUnary()
		if err != nil {
			return 0, err
		}
		if t.text == "-" {
			return -v, nil
		}
		return v, nil
	}
	return p.parsePower()
}

// parsePower := atom ('^' unary)?  —— 指数侧复用 unary 以获得右结合：2^3^2 = 2^(3^2)
func (p *parser) parsePower() (float64, error) {
	base, err := p.parseAtom()
	if err != nil {
		return 0, err
	}
	if t := p.peek(); t.kind == tokOperator && t.text == "^" {
		p.next()
		exp, err := p.parseUnary()
		if err != nil {
			return 0, err
		}
		return math.Pow(base, exp), nil
	}
	return base, nil
}

// parseAtom := number | '(' expr ')' | func '(' args ')' | const
func (p *parser) parseAtom() (float64, error) {
	t := p.next()
	switch t.kind {
	case tokNumber:
		return t.num, nil
	case tokLParen:
		v, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		if err := p.expectRParen(); err != nil {
			return 0, err
		}
		return v, nil
	case tokIdent:
		if p.peek().kind == tokLParen {
			p.next()
			args, err := p.parseArgs()
			if err != nil {
				return 0, err
			}
			return callFunction(t.text, args)
		}
		if v, ok := constants[t.text]; ok {
			return v, nil
		}
		return 0, fmt.Errorf("位置 %d 处出现未知标识符 %q（表达式不接受变量，只支持常量 pi/e 与内置函数）", t.pos+1, t.text)
	case tokEOF:
		return 0, errors.New("表达式意外结束")
	default:
		return 0, fmt.Errorf("位置 %d 处出现意料之外的 %q", t.pos+1, t.text)
	}
}

func (p *parser) parseArgs() ([]float64, error) {
	if p.peek().kind == tokRParen {
		p.next()
		return nil, nil
	}
	var args []float64
	for {
		v, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		args = append(args, v)
		if p.peek().kind == tokComma {
			p.next()
			continue
		}
		if err := p.expectRParen(); err != nil {
			return nil, err
		}
		return args, nil
	}
}

func (p *parser) expectRParen() error {
	if t := p.next(); t.kind != tokRParen {
		return fmt.Errorf("位置 %d 处缺少右括号", t.pos+1)
	}
	return nil
}

// callFunction 调用内置函数；函数名已在词法阶段归一为小写。
func callFunction(name string, args []float64) (float64, error) {
	need := func(n int) error {
		if len(args) != n {
			return fmt.Errorf("函数 %s 需要 %d 个参数，实际 %d 个", name, n, len(args))
		}
		return nil
	}
	switch name {
	case "abs":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Abs(args[0]), nil
	case "sqrt":
		if err := need(1); err != nil {
			return 0, err
		}
		if args[0] < 0 {
			return 0, fmt.Errorf("sqrt 的参数不能为负数（%s）", FormatNumber(args[0]))
		}
		return math.Sqrt(args[0]), nil
	case "pow":
		if err := need(2); err != nil {
			return 0, err
		}
		return math.Pow(args[0], args[1]), nil
	case "min":
		if len(args) == 0 {
			return 0, errors.New("min 至少需要 1 个参数")
		}
		m := args[0]
		for _, v := range args[1:] {
			m = math.Min(m, v)
		}
		return m, nil
	case "max":
		if len(args) == 0 {
			return 0, errors.New("max 至少需要 1 个参数")
		}
		m := args[0]
		for _, v := range args[1:] {
			m = math.Max(m, v)
		}
		return m, nil
	case "round":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Round(args[0]), nil
	case "floor":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Floor(args[0]), nil
	case "ceil":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Ceil(args[0]), nil
	case "log":
		if err := need(1); err != nil {
			return 0, err
		}
		if args[0] <= 0 {
			return 0, fmt.Errorf("log 的参数必须为正数（%s）", FormatNumber(args[0]))
		}
		return math.Log(args[0]), nil
	case "exp":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Exp(args[0]), nil
	default:
		return 0, fmt.Errorf("未知函数 %q（支持 abs/sqrt/pow/min/max/round/floor/ceil/log/exp）", name)
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
