package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"mcp-server-go/internal/safehttp"
	"mcp-server-go/internal/tools"
)

func testdataPath(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位测试源文件所在目录")
	}
	return filepath.Join(filepath.Dir(file), "testdata", name)
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(testdataPath(t, name))
	if err != nil {
		t.Fatalf("读取 fixture 失败: %v", err)
	}
	return string(raw)
}

// stubResolver 让测试无需真实 DNS：任何域名都解析为公网地址。
type stubResolver struct{}

func (stubResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}

// roundTripFunc 用于注入确定性响应。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func htmlResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

// newToolWithResponses 构造工具，按主机名返回固定页面（未覆盖的主机返回 500）。
func newToolWithResponses(t *testing.T, pages map[string]string) *Tool {
	t.Helper()
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		host := strings.ToLower(req.URL.Hostname())
		body, ok := pages[host]
		if !ok {
			return htmlResponse(req, http.StatusInternalServerError, "boom"), nil
		}
		return htmlResponse(req, http.StatusOK, body), nil
	})
	client := safehttp.New(safehttp.Options{
		Guard:     safehttp.NewGuard(stubResolver{}),
		Transport: transport,
	})
	return New(Options{Client: client})
}

func TestParseBingFixture(t *testing.T) {
	results := bingEngine{}.parse(readFixture(t, "bing.html"), 10)
	if len(results) != 3 {
		t.Fatalf("结果数 = %d, 期望 3（广告块不该被采纳）\n%+v", len(results), results)
	}
	want := Result{
		Title:   "Go 语言实现 MCP Server 完整教程",
		URL:     "https://example.com/go-mcp-server",
		Snippet: "从协议握手到工具注册，一步步用 Go 标准库实现一个可被 Cursor 接入的 MCP Server。",
	}
	if results[0] != want {
		t.Fatalf("首条结果 = %+v, 期望 %+v", results[0], want)
	}
	if results[1].URL != "https://example.org/mcp-transport" {
		t.Fatalf("第二条 URL = %q", results[1].URL)
	}
	for _, r := range results {
		if strings.Contains(r.URL, "ad.example.com") || strings.Contains(r.Title, "推广") {
			t.Fatalf("广告条目混入结果: %+v", r)
		}
	}

	limited := bingEngine{}.parse(readFixture(t, "bing.html"), 2)
	if len(limited) != 2 {
		t.Fatalf("limit 未生效: %d", len(limited))
	}
}

func TestParseDuckDuckGoFixture(t *testing.T) {
	results := duckduckgoEngine{}.parse(readFixture(t, "duckduckgo.html"), 10)
	if len(results) != 2 {
		t.Fatalf("结果数 = %d, 期望 2（广告位应被剔除）\n%+v", len(results), results)
	}
	// uddg 参数必须被解出为真实地址；fixture 里还带了 &amp; 实体，顺带验证属性实体解码。
	if results[0].URL != "https://pkg.go.dev/example/mcp" {
		t.Fatalf("首条 URL = %q, 期望还原后的真实地址", results[0].URL)
	}
	if results[0].Title != "Go 包文档：example/mcp" {
		t.Fatalf("首条标题 = %q", results[0].Title)
	}
	if !strings.Contains(results[0].Snippet, "官方包文档") {
		t.Fatalf("首条摘要 = %q", results[0].Snippet)
	}
	if results[1].URL != "https://blog.example.com/mcp-stdio" {
		t.Fatalf("直链结果未被保留: %+v", results[1])
	}
	for _, r := range results {
		if strings.Contains(r.URL, "duckduckgo.com") {
			t.Fatalf("跳转地址未被还原: %+v", r)
		}
		if strings.Contains(r.URL, "ads.example.com") {
			t.Fatalf("广告结果混入: %+v", r)
		}
	}
}

func TestRealDDGURL(t *testing.T) {
	cases := map[string]string{
		"//duckduckgo.com/l/?uddg=https%3A%2F%2Fa.example%2Fp%3Fx%3D1&rut=z": "https://a.example/p?x=1",
		"https://duckduckgo.com/l/?uddg=https%3A%2F%2Fb.example%2F":          "https://b.example/",
		"https://example.com/direct":                                         "https://example.com/direct",
		"//example.com/rel":                                                  "https://example.com/rel",
		"":                                                                   "",
		"https://duckduckgo.com/l/?rut=z":                                    "https://duckduckgo.com/l/?rut=z",
	}
	for in, want := range cases {
		if got := realDDGURL(in); got != want {
			t.Errorf("realDDGURL(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestExecuteUsesBingFirst(t *testing.T) {
	tool := newToolWithResponses(t, map[string]string{
		"cn.bing.com": readFixture(t, "bing.html"),
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"go mcp server","count":2}`))
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if !strings.Contains(out, "搜索源：Bing") {
		t.Fatalf("应命中 Bing:\n%s", out)
	}
	if !strings.Contains(out, "1. Go 语言实现 MCP Server 完整教程") || !strings.Contains(out, "2. MCP 传输层对比") {
		t.Fatalf("结果格式不符:\n%s", out)
	}
	if strings.Contains(out, "3. ") {
		t.Fatalf("count=2 未生效:\n%s", out)
	}
}

func TestExecuteFallsBackToDuckDuckGo(t *testing.T) {
	// Bing 返回 500 → 必须退到 DuckDuckGo。
	tool := newToolWithResponses(t, map[string]string{
		"html.duckduckgo.com": readFixture(t, "duckduckgo.html"),
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"go mcp"}`))
	if err != nil {
		t.Fatalf("降级失败: %v", err)
	}
	if !strings.Contains(out, "搜索源：DuckDuckGo") {
		t.Fatalf("应降级到 DuckDuckGo:\n%s", out)
	}
	if !strings.Contains(out, "https://pkg.go.dev/example/mcp") {
		t.Fatalf("结果缺失:\n%s", out)
	}
}

func TestExecuteFallsBackWhenBingHasNoParsableResults(t *testing.T) {
	tool := newToolWithResponses(t, map[string]string{
		"cn.bing.com":         `<html><body><li class="b_algo"></li></body></html>`,
		"html.duckduckgo.com": readFixture(t, "duckduckgo.html"),
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"go mcp"}`))
	if err != nil {
		t.Fatalf("降级失败: %v", err)
	}
	if !strings.Contains(out, "搜索源：DuckDuckGo") {
		t.Fatalf("Bing 解析不到结果时应降级:\n%s", out)
	}
}

func TestExecuteReportsErrorWhenAllSourcesFail(t *testing.T) {
	tool := newToolWithResponses(t, nil)
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"go mcp"}`))
	if err == nil {
		t.Fatal("全部源失败时报错")
	}
	if !strings.Contains(err.Error(), "全部搜索源均不可用") {
		t.Fatalf("错误信息不符: %v", err)
	}
	if !strings.Contains(err.Error(), "Bing") || !strings.Contains(err.Error(), "DuckDuckGo") {
		t.Fatalf("错误信息应列出两个源: %v", err)
	}
}

func TestExecuteHonorsContextCancellation(t *testing.T) {
	tool := newToolWithResponses(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Execute(ctx, json.RawMessage(`{"query":"go mcp"}`)); err == nil {
		t.Fatal("已取消的上下文应报错")
	}
}

func TestExecuteRejectsBadArguments(t *testing.T) {
	tool := New(Options{})
	for _, args := range []string{`{}`, `{"query":""}`, `{"query":"   "}`, `bad json`} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("args=%s 应报错", args)
		}
	}
	long := strings.Repeat("字", MaxQueryRunes+1)
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"`+long+`"}`)); err == nil {
		t.Error("超长 query 应报错")
	}
}

func TestExecuteClampsCount(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<html><body>")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&sb, `<li class="b_algo"><h2><a href="https://e.example/%d">T%d</a></h2><div class="b_caption"><p>S%d</p></div></li>`, i, i, i)
	}
	sb.WriteString("</body></html>")

	tool := newToolWithResponses(t, map[string]string{"cn.bing.com": sb.String()})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"q","count":999}`))
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if got := strings.Count(out, "\n   https://"); got != MaxCount {
		t.Fatalf("条数 = %d, 期望被夹到 %d:\n%s", got, MaxCount, out)
	}
	if !strings.Contains(out, "条数："+strconv.Itoa(MaxCount)) {
		t.Fatalf("条数标题不符:\n%s", out)
	}
}

func TestSchemaAndName(t *testing.T) {
	tool := New(Options{})
	if tool.Name() != "web_search" {
		t.Fatalf("工具名 = %q", tool.Name())
	}
	if err := tools.ValidateInputSchema(tool.InputSchema()); err != nil {
		t.Fatalf("schema 不合规: %v", err)
	}
}

func TestAddResultDedupesAndDropsIncompleteRows(t *testing.T) {
	var out []Result
	seen := map[string]bool{}
	out = addResult(out, seen, Result{Title: "a", URL: "https://x/1"})
	if len(out) != 1 {
		t.Fatalf("首次追加异常: len=%d", len(out))
	}
	out = addResult(out, seen, Result{Title: "a-dup", URL: "https://x/1"})
	if len(out) != 1 {
		t.Fatalf("重复 URL 应被丢弃: len=%d", len(out))
	}
	out = addResult(out, seen, Result{Title: "", URL: "https://x/2"})
	if len(out) != 1 {
		t.Fatalf("空标题应被丢弃: len=%d", len(out))
	}
	out = addResult(out, seen, Result{Title: "b", URL: "https://x/3"})
	if len(out) != 2 {
		t.Fatalf("追加失败: len=%d", len(out))
	}
}

func TestSearchURLs(t *testing.T) {
	bingURL := bingEngine{}.searchURL("a b&c", 5)
	if !strings.HasPrefix(bingURL, "https://cn.bing.com/search?q=") || strings.Contains(bingURL, "a b") {
		t.Fatalf("Bing URL 未正确编码: %q", bingURL)
	}
	ddgURL := duckduckgoEngine{}.searchURL("a b", 5)
	if !strings.HasPrefix(ddgURL, "https://html.duckduckgo.com/html/?q=") {
		t.Fatalf("DDG URL 不符: %q", ddgURL)
	}
}

func TestIsFetchableURL(t *testing.T) {
	for _, u := range []string{"https://a.example/x", "http://a.example/x?y=1"} {
		if !isFetchableURL(u) {
			t.Errorf("%s 应可抓取", u)
		}
	}
	for _, u := range []string{"", "javascript:alert(1)", "/relative", "ftp://a.example/x", "mailto:a@b.c"} {
		if isFetchableURL(u) {
			t.Errorf("%s 不应被接受", u)
		}
	}
}

func TestStubResolverIsUsed(t *testing.T) {
	// 保证测试路径不依赖真实 DNS：解析失败时 stub 仍返回公网地址。
	if _, err := (stubResolver{}).LookupIPAddr(context.Background(), "cn.bing.com"); err != nil {
		t.Fatalf("stub 解析器不应报错: %v", err)
	}
	var _ safehttp.Resolver = stubResolver{}
}
