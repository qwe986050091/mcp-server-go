// Package websearch 实现 MCP 工具 web_search：免 API Key 的网页搜索。
//
// 与自有 Java Agent 的检索顺序保持一致：先 Bing（cn.bing.com），无结果或失败再退到
// DuckDuckGo（html.duckduckgo.com）；任一出结果即返回，两源皆失败才报错。
package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mcp-server-go/internal/htmlx"
	"mcp-server-go/internal/safehttp"
	"mcp-server-go/internal/tools"
)

// 限额与超时预算（总预算必须显著小于 MCP 客户端 60s 的工具执行超时）。
const (
	DefaultCount     = 5
	MaxCount         = 10
	MaxQueryRunes    = 200
	perSourceTimeout = 8 * time.Second
	overallTimeout   = 15 * time.Second
)

// Result 是一条搜索结果。
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// engine 是一个搜索源：负责拼 URL 与解析结果页。
type engine interface {
	// label 是给用户看的来源名。
	label() string
	// searchURL 拼出该源的检索地址。
	searchURL(query string, count int) string
	// parse 从结果页 HTML 中解析出结果（纯函数，便于用固化页面做单测）。
	parse(page string, limit int) []Result
}

// engines 是检索顺序，顺序即优先级。
var engines = []engine{bingEngine{}, duckduckgoEngine{}}

// Options 是构造参数。
type Options struct {
	// Client 为 nil 时使用 safehttp 默认配置。
	Client *safehttp.Client
}

// Tool 是 web_search 工具。
type Tool struct {
	client *safehttp.Client
}

// New 构造 web_search 工具。
func New(opts Options) *Tool {
	client := opts.Client
	if client == nil {
		client = safehttp.New(safehttp.Options{})
	}
	return &Tool{client: client}
}

// Name 返回工具名。
func (*Tool) Name() string { return "web_search" }

// Description 返回给模型看的说明。
func (*Tool) Description() string {
	return "无需 API Key 的网页搜索，依次尝试 Bing（cn.bing.com）与 DuckDuckGo（html.duckduckgo.com），" +
		"返回标题、URL 与摘要。count 默认 5，最大 10。适合查资料、找官网与核实事实；" +
		"若需要某个具体页面的正文，再对结果 URL 调用 web_fetch。"
}

// InputSchema 返回入参 schema。
func (*Tool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "搜索关键词，越具体越好",
			},
			"count": map[string]any{
				"type":        "integer",
				"description": "期望返回的条数，默认 5，最大 10",
			},
		},
		"required": []any{"query"},
	}
}

// Execute 依次尝试各搜索源。
func (t *Tool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Query string `json:"query"`
		Count *int   `json:"count"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("参数解析失败: %w", err)
		}
	}
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return "", errors.New("query 不能为空")
	}
	if len([]rune(query)) > MaxQueryRunes {
		return "", fmt.Errorf("query 过长（%d 字符，上限 %d）", len([]rune(query)), MaxQueryRunes)
	}
	count := DefaultCount
	if in.Count != nil && *in.Count > 0 {
		count = *in.Count
	}
	if count > MaxCount {
		count = MaxCount
	}

	ctx, cancel := context.WithTimeout(ctx, overallTimeout)
	defer cancel()

	failures := make([]string, 0, len(engines))
	for _, e := range engines {
		sourceCtx, sourceCancel := context.WithTimeout(ctx, perSourceTimeout)
		results, err := t.searchOne(sourceCtx, e, query, count)
		sourceCancel()
		switch {
		case err != nil:
			failures = append(failures, e.label()+": "+err.Error())
		case len(results) == 0:
			failures = append(failures, e.label()+": 解析不到结果")
		default:
			return formatResults(e.label(), query, results), nil
		}
	}
	return "", fmt.Errorf("全部搜索源均不可用（%s）。可改用 web_fetch 直接抓取已知站点，或稍后重试",
		strings.Join(failures, "；"))
}

func (t *Tool) searchOne(ctx context.Context, e engine, query string, count int) ([]Result, error) {
	resp, err := t.client.Get(ctx, e.searchURL(query, count), map[string]string{
		"Accept":          "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return e.parse(strings.ToValidUTF8(string(resp.Body), ""), count), nil
}

func formatResults(source, query string, results []Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "搜索源：%s\n关键词：%s\n条数：%d\n\n", source, query, len(results))
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", r.Snippet)
		}
	}
	return b.String()
}

// addResult 去重后追加；URL 或标题为空、以及重复 URL 一律丢弃。
func addResult(out []Result, seen map[string]bool, r Result) []Result {
	if r.URL == "" || r.Title == "" || seen[r.URL] {
		return out
	}
	seen[r.URL] = true
	return append(out, r)
}

// isFetchableURL 只接受可直接抓取的绝对 http/https 链接。
func isFetchableURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != ""
}

// bingEngine 解析 cn.bing.com 的结果页。
type bingEngine struct{}

func (bingEngine) label() string { return "Bing（cn.bing.com）" }

func (bingEngine) searchURL(query string, count int) string {
	return "https://cn.bing.com/search?q=" + url.QueryEscape(query) +
		"&count=" + strconv.Itoa(count) + "&setlang=zh-CN"
}

func (bingEngine) parse(page string, limit int) []Result {
	doc := htmlx.Parse(page)
	out := make([]Result, 0, limit)
	seen := map[string]bool{}
	for _, item := range doc.FindClass("li", "b_algo") {
		anchor := firstResultAnchor(item)
		if anchor == nil {
			continue
		}
		href := strings.TrimSpace(anchor.AttrOr("href", ""))
		if !isFetchableURL(href) {
			continue
		}
		out = addResult(out, seen, Result{
			Title:   anchor.CompactText(),
			URL:     href,
			Snippet: bingSnippet(item),
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// firstResultAnchor 取条目内的标题链接：优先 h2 内的 a，退化为第一个 a。
func firstResultAnchor(item *htmlx.Node) *htmlx.Node {
	if h2 := item.First("h2"); h2 != nil {
		if a := h2.First("a"); a != nil {
			return a
		}
	}
	return item.First("a")
}

// bingSnippet 取摘要：优先 .b_caption 内的 p，退化为条目内第一个 p。
func bingSnippet(item *htmlx.Node) string {
	if caption := item.FirstClass("", "b_caption"); caption != nil {
		if p := caption.First("p"); p != nil {
			return p.CompactText()
		}
	}
	if p := item.First("p"); p != nil {
		return p.CompactText()
	}
	return ""
}

// duckduckgoEngine 解析 html.duckduckgo.com 的结果页。
type duckduckgoEngine struct{}

func (duckduckgoEngine) label() string { return "DuckDuckGo（html.duckduckgo.com）" }

func (duckduckgoEngine) searchURL(query string, _ int) string {
	return "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query) + "&kl=cn-zh"
}

func (duckduckgoEngine) parse(page string, limit int) []Result {
	doc := htmlx.Parse(page)
	anchors := doc.FindClass("a", "result__a")
	snippets := doc.FindClass("", "result__snippet")
	out := make([]Result, 0, limit)
	seen := map[string]bool{}
	for idx, a := range anchors {
		// 广告位结果（result--ad）不是真实检索结果，直接丢弃。
		if a.AncestorClassContains("result--ad", 4) {
			continue
		}
		href := realDDGURL(a.AttrOr("href", ""))
		if !isFetchableURL(href) {
			continue
		}
		out = addResult(out, seen, Result{
			Title:   a.CompactText(),
			URL:     href,
			Snippet: ddgSnippet(a, snippets, idx),
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// ddgSnippet 在标题链接所属的结果块内找摘要；块内找不到时退化为按序号配对。
func ddgSnippet(anchor *htmlx.Node, snippets []*htmlx.Node, idx int) string {
	cur := anchor
	for level := 0; cur != nil && level <= 4; level++ {
		if s := cur.FirstClass("", "result__snippet"); s != nil {
			return s.CompactText()
		}
		cur = cur.Parent
	}
	if idx < len(snippets) {
		return snippets[idx].CompactText()
	}
	return ""
}

// realDDGURL 还原 DuckDuckGo 跳转链接：页面里的 href 通常是
// `//duckduckgo.com/l/?uddg=<真实地址>`，需要把 uddg 参数解出来。
func realDDGURL(href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if !strings.Contains(strings.ToLower(u.Hostname()), "duckduckgo.com") {
		return href
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return href
	}
	if real := values.Get("uddg"); real != "" {
		return real
	}
	return href
}

// 编译期断言：确保实现满足 tools.Tool 接口。
var _ tools.Tool = (*Tool)(nil)
