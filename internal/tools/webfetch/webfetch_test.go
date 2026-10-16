package webfetch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-server-go/internal/safehttp"
	"mcp-server-go/internal/tools"
)

// localClient 构造放行回环的客户端，用于驱动 httptest。
func localClient() *safehttp.Client {
	g := safehttp.NewGuard(nil)
	g.AllowLoopback = true
	return safehttp.New(safehttp.Options{Guard: g})
}

func newTool() *Tool { return New(Options{Client: localClient()}) }

func TestFetchExtractsTitleAndBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><head><title>示例 &amp; 标题</title>
<script>var x=1;</script><style>.a{}</style></head>
<body><nav>导航噪音</nav><h1>正文标题</h1><p>第一段。</p><p>第二段 &nbsp;结束。</p>
<iframe src="//ads.example/x"></iframe>
<footer>页脚</footer></body></html>`))
	}))
	defer ts.Close()

	out, err := newTool().Execute(context.Background(), json.RawMessage(`{"url":"`+ts.URL+`"}`))
	if err != nil {
		t.Fatalf("抓取失败: %v", err)
	}
	if !strings.Contains(out, "标题：示例 & 标题") {
		t.Errorf("标题抽取错误:\n%s", out)
	}
	if !strings.Contains(out, "第一段。") || !strings.Contains(out, "第二段 结束。") {
		t.Errorf("正文抽取错误:\n%s", out)
	}
	if strings.Contains(out, "var x=1") || strings.Contains(out, ".a{}") {
		t.Errorf("script/style 未被剥离:\n%s", out)
	}
	// 方案只要求剥离 script/style/iframe/svg：结构性的 nav/footer 会保留，这里固化该行为。
	if !strings.Contains(out, "导航噪音") || !strings.Contains(out, "页脚") {
		t.Errorf("nav/footer 按设计应保留:\n%s", out)
	}
	if !strings.Contains(out, "HTTP 状态：200") {
		t.Errorf("缺少状态行:\n%s", out)
	}
}

func TestFetchPlainTextResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("line1\r\n\r\n\r\nline2\r\n"))
	}))
	defer ts.Close()

	out, err := newTool().Execute(context.Background(), json.RawMessage(`{"url":"`+ts.URL+`"}`))
	if err != nil {
		t.Fatalf("抓取失败: %v", err)
	}
	if !strings.Contains(out, "line1\nline2") {
		t.Errorf("纯文本归一化错误:\n%s", out)
	}
	if strings.Contains(out, "标题：") {
		t.Errorf("纯文本响应不应输出标题行:\n%s", out)
	}
}

func TestFetchReportsHTTPErrorStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("nope"))
	}))
	defer ts.Close()

	_, err := newTool().Execute(context.Background(), json.RawMessage(`{"url":"`+ts.URL+`"}`))
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("应报告 404，实际: %v", err)
	}
}

func TestFetchTruncatesLongBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html><body><p>" + strings.Repeat("字", 9000) + "</p></body></html>"))
	}))
	defer ts.Close()

	out, err := newTool().Execute(context.Background(), json.RawMessage(`{"url":"`+ts.URL+`"}`))
	if err != nil {
		t.Fatalf("抓取失败: %v", err)
	}
	body, _, truncated := strings.Cut(out, "[内容已截断")
	if !truncated {
		t.Fatalf("超长正文应标注截断:\n%s", out)
	}
	if got := strings.Count(body, "字"); got != maxBodyChars {
		t.Errorf("截断字符数 = %d, 期望 %d", got, maxBodyChars)
	}
}

func TestFetchFollowsSafeRedirect(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, ts.URL+"/final", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html><title>落地页</title><body>最终内容</body></html>"))
	}))
	defer ts.Close()

	out, err := newTool().Execute(context.Background(), json.RawMessage(`{"url":"`+ts.URL+`"}`))
	if err != nil {
		t.Fatalf("抓取失败: %v", err)
	}
	if !strings.Contains(out, "最终内容") || !strings.Contains(out, "/final") {
		t.Errorf("重定向落地错误:\n%s", out)
	}
}

func TestFetchRejectsInternalTarget(t *testing.T) {
	// 严格守卫（默认配置 New(nil) 等价于 Options{}）下，直连内网/元数据地址必须被拒。
	tool := New(Options{})
	for _, u := range []string{
		"http://127.0.0.1:8080/",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/",
		"http://[::1]/",
	} {
		_, err := tool.Execute(context.Background(), json.RawMessage(`{"url":"`+u+`"}`))
		if err == nil {
			t.Errorf("%s 必须被拒绝", u)
			continue
		}
		if !strings.Contains(err.Error(), "SSRF") {
			t.Errorf("%s 的错误信息应说明 SSRF 拦截，实际: %v", u, err)
		}
	}
}

func TestFetchRejectsBadArguments(t *testing.T) {
	tool := newTool()
	for _, args := range []string{`{}`, `{"url":""}`, `{"url":"   "}`, `{"url":"ftp://example.com/x"}`, `{"url":"not-a-url"}`, `bad json`} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("args=%s 应报错", args)
		}
	}
}

func TestFetchSchemaAndName(t *testing.T) {
	tool := newTool()
	if tool.Name() != "web_fetch" {
		t.Fatalf("工具名 = %q", tool.Name())
	}
	if err := tools.ValidateInputSchema(tool.InputSchema()); err != nil {
		t.Fatalf("schema 不合规: %v", err)
	}
}

func TestTruncateRunesKeepsUTF8Intact(t *testing.T) {
	s := strings.Repeat("中", 10)
	got := truncateRunes(s, 3)
	if !strings.HasPrefix(got, "中中中") || !strings.Contains(got, "已截断") {
		t.Fatalf("截断结果错误: %q", got)
	}
	if strings.Contains(got, "\ufffd") {
		t.Fatalf("截断产生了乱码: %q", got)
	}
	if short := truncateRunes("短", 10); short != "短" {
		t.Fatalf("未超限不应截断: %q", short)
	}
}

func TestNormalizePlainCollapsesBlankLines(t *testing.T) {
	got := normalizePlain("a\r\n \r\n\r\nb\n\n\n\nc  \n")
	if got != "a\nb\nc" {
		t.Fatalf("归一化结果 = %q", got)
	}
}
