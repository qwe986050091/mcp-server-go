package htmlx

import (
	"strings"
	"testing"
)

func TestParseBasicTreeAndAttrs(t *testing.T) {
	doc := Parse(`<html><body><div id="main" class="wrap  box"><span>hi</span></div></body></html>`)
	div := doc.First("div")
	if div == nil {
		t.Fatal("未找到 div")
	}
	if got := div.AttrOr("id", ""); got != "main" {
		t.Fatalf("id = %q", got)
	}
	if !div.HasClass("box") || !div.HasClass("wrap") {
		t.Fatalf("class 解析错误: %q", div.AttrOr("class", ""))
	}
	if div.HasClass("wr") {
		t.Fatal("class 必须整体匹配，不能子串匹配")
	}
	if span := div.First("span"); span == nil || span.CompactText() != "hi" {
		t.Fatalf("span 文本错误: %+v", span)
	}
	if div.Parent == nil || div.Parent.Tag != "body" {
		t.Fatalf("父节点应为 body，实际 %+v", div.Parent)
	}
}

func TestAttributeQuoteStyles(t *testing.T) {
	doc := Parse(`<a href="a.html" title='t' data-x=ux rel disabled>x</a>`)
	a := doc.First("a")
	cases := map[string]string{
		"href":   "a.html",
		"title":  "t",
		"data-x": "ux",
		"rel":    "",
	}
	for name, want := range cases {
		if got := a.AttrOr(name, "<missing>"); got != want {
			t.Fatalf("属性 %s = %q, 期望 %q", name, got, want)
		}
	}
	if _, ok := a.Attr("disabled"); !ok {
		t.Fatal("无值属性应存在且值为空串")
	}
}

func TestTitleSkipsHeadInBodyText(t *testing.T) {
	doc := Parse(`<html><head><title>标题 &amp; 副标题</title><meta charset="utf-8"></head><body>正文</body></html>`)
	title := doc.First("title")
	if title == nil {
		t.Fatal("未找到 title")
	}
	if got := title.CompactText(); got != "标题 & 副标题" {
		t.Fatalf("title 文本 = %q", got)
	}
	body := doc.First("body")
	if got := body.Text(); got != "正文" {
		t.Fatalf("body 文本 = %q（head 必须被跳过）", got)
	}
}

func TestScriptAndStyleAreSkipped(t *testing.T) {
	doc := Parse(`<body>前<script>var a = "</div>"; if (a < b) {}</script><style>.x{color:red}</style><p>后</p></body>`)
	got := doc.First("body").Text()
	if got != "前\n后" {
		t.Fatalf("正文 = %q（script/style 内容必须整段跳过）", got)
	}
	// 内联脚本里的 "</div>" 不能破坏树结构。
	if len(doc.Find("div")) != 0 {
		t.Fatalf("script 内的标签被误解析: %v", doc.Find("div"))
	}
	if script := doc.First("script"); script == nil || !strings.Contains(script.CompactText(), "var a") {
		t.Fatalf("script 原文应被保留为文本: %+v", script)
	}
}

func TestRawTextTitleWithNestedMarkup(t *testing.T) {
	doc := Parse(`<title>A <b>B</b> C</title>`)
	// title 是 raw text 元素，内部标签不解析。
	if got := doc.First("title").CompactText(); got != "A <b>B</b> C" {
		t.Fatalf("title 文本 = %q", got)
	}
	if doc.First("b") != nil {
		t.Fatal("raw text 元素内部不应建节点")
	}
}

func TestVoidElements(t *testing.T) {
	doc := Parse(`<body><p>a<br>b<img src="x.png"><hr>c</p></body>`)
	body := doc.First("body")
	if got := body.Text(); got != "a\nb\nc" {
		t.Fatalf("正文 = %q", got)
	}
	img := doc.First("img")
	if img == nil || len(img.Children) != 0 {
		t.Fatal("img 应为空元素")
	}
	// <hr> 不能吞掉后面的文本。
	if !strings.Contains(body.Text(), "c") {
		t.Fatalf("hr 之后的内容丢失: %q", body.Text())
	}
}

func TestAutoCloseParagraphAndListItem(t *testing.T) {
	doc := Parse(`<div><p>one<p>two<ul><li>a<li>b</ul><p>three</div>`)
	if got := len(doc.Find("p")); got != 3 {
		t.Fatalf("p 数量 = %d, 期望 3（隐式闭合）", got)
	}
	if got := len(doc.Find("li")); got != 2 {
		t.Fatalf("li 数量 = %d, 期望 2", got)
	}
	ul := doc.First("ul")
	if len(ul.Children) != 2 || ul.Children[0].Tag != "li" {
		t.Fatalf("<li> 必须是 <ul> 的直接子节点: %+v", ul.Children)
	}
	if got := doc.First("p").Text(); got != "one" {
		t.Fatalf("第一个 p = %q", got)
	}
}

func TestBlockElementsProduceNewlines(t *testing.T) {
	doc := Parse(`<div><h1>T</h1><p>para one</p><p>para two</p></div>`)
	got := doc.First("div").Text()
	want := "T\npara one\npara two"
	if got != want {
		t.Fatalf("正文 = %q, 期望 %q", got, want)
	}
}

func TestWhitespaceIsCollapsed(t *testing.T) {
	doc := Parse("<div>  a\t\tb  \n\n   c&nbsp;d  </div>")
	if got := doc.First("div").Text(); got != "a b\nc d" {
		t.Fatalf("正文 = %q", got)
	}
}

func TestUnclosedTagsRecovery(t *testing.T) {
	doc := Parse(`<div><span>abc`)
	span := doc.First("span")
	if span == nil || span.CompactText() != "abc" {
		t.Fatalf("未闭合标签应仍可解析: %+v", span)
	}
}

func TestMismatchedEndTagIsIgnored(t *testing.T) {
	doc := Parse(`<div><p>a</span>b`)
	div := doc.First("div")
	if got := div.Text(); !strings.Contains(got, "a") || !strings.Contains(got, "b") {
		t.Fatalf("错配的结束标签不应弹栈: %q", got)
	}
	if len(div.Children) == 0 || div.Children[0].Tag != "p" {
		t.Fatalf("p 应仍在 div 之下: %+v", div.Children)
	}
}

func TestCommentsAndDoctypeAreSkipped(t *testing.T) {
	doc := Parse(`<!DOCTYPE html><!-- <div>隐藏</div> --><body><p>可见</p></body>`)
	if len(doc.Find("div")) != 0 {
		t.Fatal("注释内的标签不应被解析")
	}
	if got := doc.First("body").Text(); got != "可见" {
		t.Fatalf("正文 = %q", got)
	}
}

func TestFindClass(t *testing.T) {
	doc := Parse(`
<ul>
  <li class="b_algo"><h2><a href="https://a.example/1">第一条</a></h2><div class="b_caption"><p>摘要一</p></div></li>
  <li class="b_algo"><h2><a href="https://a.example/2">第二条</a></h2><div class="b_caption"><p>摘要二</p></div></li>
  <li class="other">无关</li>
</ul>`)
	items := doc.FindClass("li", "b_algo")
	if len(items) != 2 {
		t.Fatalf("b_algo 数量 = %d, 期望 2", len(items))
	}
	first := items[0]
	a := first.First("h2").First("a")
	if got := a.CompactText(); got != "第一条" {
		t.Fatalf("标题 = %q", got)
	}
	if got := a.AttrOr("href", ""); got != "https://a.example/1" {
		t.Fatalf("链接 = %q", got)
	}
	caption := first.FirstClass("div", "b_caption")
	if caption == nil || caption.First("p").CompactText() != "摘要一" {
		t.Fatalf("摘要解析错误: %+v", caption)
	}
}

func TestAllReturnsDocumentOrder(t *testing.T) {
	doc := Parse(`<div><a>1</a><b>2</b></div>`)
	var tags []string
	for _, n := range doc.All() {
		tags = append(tags, n.Tag)
	}
	got := strings.Join(tags, ",")
	if got != "div,a,b" {
		t.Fatalf("文档顺序 = %q", got)
	}
}

func TestDecodeEntities(t *testing.T) {
	cases := map[string]string{
		"a &amp; b":        "a & b",
		"&#39;q&#39;":      "'q'",
		"&#x4E2D;&#x6587;": "中文",
		"&nbsp;x":          "\u00a0x",
		"&mdash;&hellip;":  "—…",
		"&unknown; &bad":   "&unknown; &bad",
		"&amp":             "&amp",
		"a & b":            "a & b",
		"&#xD800;":         "&#xD800;",
		"&#0;":             "&#0;",
		"plain text":       "plain text",
		"&lt;script&gt;":   "<script>",
		"&amp;amp;":        "&amp;",
	}
	for in, want := range cases {
		if got := DecodeEntities(in); got != want {
			t.Fatalf("DecodeEntities(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestAttrsWithEntitiesAndNewlines(t *testing.T) {
	doc := Parse("<a href=\"https://x/?a=1&amp;b=2\"\ntitle=\"line1\nline2\">t</a>")
	a := doc.First("a")
	if got := a.AttrOr("href", ""); got != "https://x/?a=1&b=2" {
		t.Fatalf("href = %q（实体应解码）", got)
	}
	if got := a.AttrOr("title", ""); got != "line1\nline2" {
		t.Fatalf("跨行属性值 = %q", got)
	}
}

func TestEmptyAndGarbageInput(t *testing.T) {
	for _, in := range []string{"", "   ", "<", "<<<", "no tags at all", "<div", "</div>", "<>"} {
		if doc := Parse(in); doc == nil {
			t.Fatalf("Parse(%q) 返回 nil", in)
		}
	}
	if got := Parse("no tags at all").Text(); got != "no tags at all" {
		t.Fatalf("纯文本输入 = %q", got)
	}
}

func TestDeepNestingDoesNotPanic(t *testing.T) {
	deep := strings.Repeat("<div>", 5000) + "x" + strings.Repeat("</div>", 5000)
	doc := Parse(deep)
	if !strings.Contains(doc.Text(), "x") {
		t.Fatal("超深嵌套下文本仍应可见")
	}
}
