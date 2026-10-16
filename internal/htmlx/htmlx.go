// Package htmlx 是一个最小可用的 HTML 解析与文本抽取实现（零第三方依赖）。
//
// 它不是完整的 HTML5 解析器，而是为 web_fetch / web_search 场景做了精准裁剪：
//   - 支持注释、doctype、属性（双引号/单引号/无引号）、void 元素、raw text 元素；
//   - 支持少量隐式闭合规则（p / li / td / th / tr / dt / dd / option），足以还原常见页面结构；
//   - 不做 CSS 选择器引擎，只提供标签名与 class 查找 —— 抓取目标结构固定，够用即可。
package htmlx

import (
	"strings"
)

const (
	maxDepth = 256
	maxNodes = 200000
	// textTag 是文本节点的标记：文本与元素共用同一个有序 Children 列表，
	// 才能正确还原 `<p>a<br>b</p>` 这类文本/标签交错的内容。
	textTag = "#text"
)

// Node 是解析后的元素节点；根节点的 Tag 为空字符串，文本节点的 Tag 为 "#text"。
type Node struct {
	Tag      string
	Attrs    map[string]string
	Children []*Node
	Parent   *Node

	// text 仅文本节点使用（已解码的文本片段）。
	text []string
}

// IsText 判断是否为文本节点。
func (n *Node) IsText() bool { return n != nil && n.Tag == textTag }

// voidElements 是不允许有子节点的空元素。
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true,
	"img": true, "input": true, "link": true, "meta": true, "param": true,
	"source": true, "track": true, "wbr": true,
}

// rawTextElements 的内容按纯文本处理，不再解析标签。
var rawTextElements = map[string]bool{
	"script": true, "style": true, "textarea": true, "title": true,
}

// blockTags 是块级标签：换行与隐式闭合 <p> 都以它为依据。
var blockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true, "center": true,
	"details": true, "div": true, "dl": true, "fieldset": true, "figcaption": true,
	"figure": true, "footer": true, "form": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "header": true, "hr": true, "li": true,
	"main": true, "nav": true, "ol": true, "p": true, "pre": true, "section": true,
	"summary": true, "table": true, "tbody": true, "td": true, "tfoot": true,
	"th": true, "thead": true, "tr": true, "ul": true,
}

// Parse 解析 HTML 并返回虚拟根节点（Tag == ""）。
func Parse(input string) *Node {
	p := &parser{src: input, root: &Node{Attrs: map[string]string{}}}
	p.stack = []*Node{p.root}
	p.run()
	return p.root
}

type parser struct {
	src   string
	pos   int
	root  *Node
	stack []*Node
	nodes int
}

func (p *parser) run() {
	for p.pos < len(p.src) {
		lt := strings.IndexByte(p.src[p.pos:], '<')
		if lt < 0 {
			p.addText(p.src[p.pos:])
			return
		}
		if lt > 0 {
			p.addText(p.src[p.pos : p.pos+lt])
			p.pos += lt
		}
		rest := p.src[p.pos:]
		switch {
		case strings.HasPrefix(rest, "<!--"):
			if end := strings.Index(rest[4:], "-->"); end < 0 {
				p.pos = len(p.src)
			} else {
				p.pos += 4 + end + 3
			}
		case strings.HasPrefix(rest, "</"):
			p.endTag()
		case strings.HasPrefix(rest, "<!"), strings.HasPrefix(rest, "<?"):
			if end := strings.IndexByte(rest, '>'); end < 0 {
				p.pos = len(p.src)
			} else {
				p.pos += end + 1
			}
		default:
			p.startTag()
		}
	}
}

func (p *parser) cur() *Node {
	return p.stack[len(p.stack)-1]
}

func (p *parser) pop() {
	if len(p.stack) > 1 {
		p.stack = p.stack[:len(p.stack)-1]
	}
}

// addText 把文本片段挂到当前节点上（实体解码后保存）。
func (p *parser) addText(raw string) {
	p.addTextTo(p.cur(), raw)
}

// addTextTo 把文本片段作为目标节点的子节点（保持与元素的相对顺序）。
func (p *parser) addTextTo(node *Node, raw string) {
	if raw == "" || p.nodes >= maxNodes {
		return
	}
	p.nodes++
	node.Children = append(node.Children, &Node{
		Tag:    textTag,
		Parent: node,
		text:   []string{DecodeEntities(raw)},
	})
}

// startTag 解析开始标签：<name attr="v" ...> 或 <name .../>
func (p *parser) startTag() {
	at := p.pos
	i := at + 1
	nameStart := i
	for i < len(p.src) && isNameByte(p.src[i]) {
		i++
	}
	if i == nameStart {
		// 不是合法标签（例如裸 '<'），丢弃该字符。
		p.pos = at + 1
		return
	}
	name := strings.ToLower(p.src[nameStart:i])
	attrs := map[string]string{}
	selfClosed := false

	for i < len(p.src) {
		for i < len(p.src) && isSpace(p.src[i]) {
			i++
		}
		if i >= len(p.src) {
			break
		}
		if p.src[i] == '>' {
			i++
			break
		}
		if p.src[i] == '/' {
			selfClosed = true
			i++
			continue
		}
		aStart := i
		for i < len(p.src) && !isSpace(p.src[i]) && p.src[i] != '=' && p.src[i] != '>' && p.src[i] != '/' {
			i++
		}
		if i == aStart {
			i++
			continue
		}
		aName := strings.ToLower(p.src[aStart:i])

		j := i
		for j < len(p.src) && isSpace(p.src[j]) {
			j++
		}
		if j < len(p.src) && p.src[j] == '=' {
			j++
			for j < len(p.src) && isSpace(p.src[j]) {
				j++
			}
			var value string
			if j < len(p.src) && (p.src[j] == '"' || p.src[j] == '\'') {
				quote := p.src[j]
				j++
				vStart := j
				for j < len(p.src) && p.src[j] != quote {
					j++
				}
				value = p.src[vStart:j]
				if j < len(p.src) {
					j++
				}
			} else {
				vStart := j
				for j < len(p.src) && !isSpace(p.src[j]) && p.src[j] != '>' {
					j++
				}
				value = p.src[vStart:j]
			}
			attrs[aName] = DecodeEntities(value)
			i = j
		} else {
			attrs[aName] = ""
		}
	}
	p.pos = i

	if p.nodes >= maxNodes {
		return
	}
	p.autoClose(name)
	node := &Node{Tag: name, Attrs: attrs, Parent: p.cur()}
	p.cur().Children = append(p.cur().Children, node)
	p.nodes++

	if voidElements[name] || selfClosed {
		return
	}
	if rawTextElements[name] {
		// 内容按纯文本处理：直接跳到对应的结束标签，且不入栈
		// （结束标签在栈中找不到匹配时会被忽略，不会误弹祖先）。
		closeIdx := indexCloseTag(p.src, name, p.pos)
		if closeIdx < 0 {
			p.addTextTo(node, p.src[p.pos:])
			p.pos = len(p.src)
			return
		}
		p.addTextTo(node, p.src[p.pos:closeIdx])
		p.pos = closeIdx
		return
	}
	if len(p.stack) >= maxDepth {
		return
	}
	p.stack = append(p.stack, node)
}

// endTag 解析结束标签；栈中找不到匹配时静默忽略。
func (p *parser) endTag() {
	i := p.pos + 2
	nameStart := i
	for i < len(p.src) && isNameByte(p.src[i]) {
		i++
	}
	name := strings.ToLower(p.src[nameStart:i])
	if end := strings.IndexByte(p.src[i:], '>'); end < 0 {
		p.pos = len(p.src)
	} else {
		p.pos = i + end + 1
	}
	if name == "" {
		return
	}
	for k := len(p.stack) - 1; k >= 1; k-- {
		if p.stack[k].Tag == name {
			p.stack = p.stack[:k]
			return
		}
	}
}

// autoClose 处理少量隐式闭合规则，避免 unclosed <p>/<li> 把整棵树串成一条链。
func (p *parser) autoClose(name string) {
	switch name {
	case "li":
		p.closeInlineP()
		if p.cur().Tag == "li" {
			p.pop()
		}
	case "dt", "dd":
		p.closeInlineP()
		if p.cur().Tag == "dt" || p.cur().Tag == "dd" {
			p.pop()
		}
	case "td", "th":
		p.closeInlineP()
		if p.cur().Tag == "td" || p.cur().Tag == "th" {
			p.pop()
		}
	case "tr":
		if t := p.cur().Tag; t == "td" || t == "th" || t == "tr" {
			p.pop()
		}
	case "option":
		if p.cur().Tag == "option" {
			p.pop()
		}
	case "p":
		if p.cur().Tag == "p" {
			p.pop()
		}
	}
	if blockTags[name] {
		p.closeInlineP()
	}
}

// closeInlineP 关闭当前未闭合的 <p>（块级开始标签会隐式闭合它）。
func (p *parser) closeInlineP() {
	for p.cur().Tag == "p" {
		p.pop()
	}
}

// indexCloseTag 在 from 之后查找 </name>，返回 '<' 的下标；未找到返回 -1。
func indexCloseTag(src, name string, from int) int {
	needle := "</" + name
	lower := strings.ToLower(src)
	for i := from; i < len(lower); {
		idx := strings.Index(lower[i:], needle)
		if idx < 0 {
			return -1
		}
		end := i + idx + len(needle)
		// 结束标签名后必须是空白、'>' 或 '/'，避免 </scriptx>
		if end >= len(src) || isSpace(src[end]) || src[end] == '>' || src[end] == '/' {
			return i + idx
		}
		i = end
	}
	return -1
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

func isNameByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return c == '-' || c == '_' || c == ':' || c == '.'
}

// Attr 读取属性（属性名大小写不敏感，已在解析时归一为小写）。
func (n *Node) Attr(name string) (string, bool) {
	if n == nil {
		return "", false
	}
	v, ok := n.Attrs[strings.ToLower(name)]
	return v, ok
}

// AttrOr 读取属性，缺失时返回默认值。
func (n *Node) AttrOr(name, def string) string {
	if v, ok := n.Attr(name); ok {
		return v
	}
	return def
}

// HasClass 判断是否含指定 class（空格分隔，大小写敏感，与 HTML 语义一致）。
func (n *Node) HasClass(class string) bool {
	if n == nil {
		return false
	}
	for _, c := range strings.Fields(n.AttrOr("class", "")) {
		if c == class {
			return true
		}
	}
	return false
}

// All 按文档顺序返回全部后代元素节点（不含文本节点）。
func (n *Node) All() []*Node {
	var out []*Node
	var walk func(*Node)
	walk = func(cur *Node) {
		if cur == nil {
			return
		}
		for _, c := range cur.Children {
			if c.Tag == textTag {
				continue
			}
			out = append(out, c)
			walk(c)
		}
	}
	walk(n)
	return out
}

// Find 按标签名查找后代（文档顺序）。
func (n *Node) Find(tag string) []*Node {
	tag = strings.ToLower(tag)
	var out []*Node
	for _, c := range n.All() {
		if c.Tag == tag {
			out = append(out, c)
		}
	}
	return out
}

// First 返回第一个匹配标签名的后代，找不到返回 nil。
func (n *Node) First(tag string) *Node {
	tag = strings.ToLower(tag)
	for _, c := range n.All() {
		if c.Tag == tag {
			return c
		}
	}
	return nil
}

// FindClass 按 class 查找后代（tag 为空表示不限标签）。
func (n *Node) FindClass(tag, class string) []*Node {
	tag = strings.ToLower(tag)
	var out []*Node
	for _, c := range n.All() {
		if (tag == "" || c.Tag == tag) && c.HasClass(class) {
			out = append(out, c)
		}
	}
	return out
}

// FirstClass 返回第一个含指定 class 的后代。
func (n *Node) FirstClass(tag, class string) *Node {
	found := n.FindClass(tag, class)
	if len(found) == 0 {
		return nil
	}
	return found[0]
}

// AncestorClassContains 判断节点自身或最近 levels 层祖先中，是否有节点的 class 属性包含给定子串。
// 用于剔除广告块（例如 DuckDuckGo 的 `result--ad`）。
func (n *Node) AncestorClassContains(substr string, levels int) bool {
	cur := n
	for level := 0; cur != nil && level <= levels; level++ {
		if strings.Contains(cur.AttrOr("class", ""), substr) {
			return true
		}
		cur = cur.Parent
	}
	return false
}
