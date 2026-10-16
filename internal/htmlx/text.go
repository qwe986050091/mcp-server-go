package htmlx

import "strings"

// skipText 是取正文时整体跳过的标签：脚本、样式、内嵌框架、矢量图、以及表单控件噪声。
var skipText = map[string]bool{
	"script": true, "style": true, "iframe": true, "svg": true, "noscript": true,
	"template": true, "head": true, "title": true, "meta": true, "link": true,
	"canvas": true, "object": true, "embed": true, "video": true, "audio": true,
	"select": true, "option": true, "input": true, "textarea": true, "button": true,
}

// breakTags 是取正文时需要在前后插入换行的标签（块级 + <br>）。
var breakTags = map[string]bool{"br": true}

func init() {
	for tag := range blockTags {
		breakTags[tag] = true
	}
}

// Text 返回子树的纯文本：
// 块级标签产生换行，行内空白压缩为单空格，空行丢弃，实体已解码。
func (n *Node) Text() string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	writeText(&b, n, 0)
	return normalizeLines(b.String())
}

// CompactText 返回单行文本，适合标题与摘要。
func (n *Node) CompactText() string {
	return strings.Join(strings.Fields(n.Text()), " ")
}

func writeText(b *strings.Builder, n *Node, depth int) {
	if n == nil || depth > maxDepth {
		return
	}
	if n.Tag == textTag {
		for _, part := range n.text {
			b.WriteString(part)
		}
		return
	}
	// 起始节点自身不跳过：对 <title> / <script> 调用 Text() 时应返回它们自身的文本，
	// 只有作为后代出现时才整段剔除。
	if depth > 0 && skipText[n.Tag] {
		return
	}
	brk := breakTags[n.Tag]
	if brk {
		b.WriteByte('\n')
	}
	for _, c := range n.Children {
		writeText(b, c, depth+1)
	}
	if brk {
		b.WriteByte('\n')
	}
}

// normalizeLines 逐行压缩空白并丢弃空行。
func normalizeLines(s string) string {
	parts := strings.Split(s, "\n")
	lines := make([]string, 0, len(parts))
	for _, line := range parts {
		if line = strings.Join(strings.Fields(line), " "); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}
