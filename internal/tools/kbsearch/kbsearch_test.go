package kbsearch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mcp-server-go/internal/tools"
)

// captured 记录服务端收到的请求，用于断言契约。
type captured struct {
	path    string
	method  string
	token   string
	ctype   string
	body    searchRequest
	rawBody string
	calls   int
}

func newTestServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, c *captured)) (*httptest.Server, *captured) {
	t.Helper()
	c := &captured{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.calls++
		c.path = r.URL.Path
		c.method = r.Method
		c.token = r.Header.Get(DefaultTokenHeader)
		c.ctype = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		c.rawBody = string(raw)
		_ = json.Unmarshal(raw, &c.body)
		handler(w, r, c)
	}))
	t.Cleanup(ts.Close)
	return ts, c
}

func TestExecuteSendsContractAndFormatsHits(t *testing.T) {
	ts, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request, _ *captured) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"items":[
			{"text":"知识库第一条原文","source":"docs/a.pdf","docId":"doc-1","chunkIndex":2,"score":0.83,"channel":"both","rerankScore":0.91},
			{"text":"知识库第二条原文","source":"notes.md","docId":"doc-2","score":0.41}
		]}`))
	})

	tool := New(Options{BaseURL: ts.URL, Token: "tok-123", DefaultUserID: "u-9"})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"如何部署","top_k":3}`))
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}

	if c.method != http.MethodPost || c.path != SearchPath {
		t.Fatalf("请求方法与路径不符: %s %s", c.method, c.path)
	}
	if c.token != "tok-123" {
		t.Fatalf("缺少内部鉴权头: %q", c.token)
	}
	if !strings.Contains(c.ctype, "application/json") {
		t.Fatalf("Content-Type 不符: %q", c.ctype)
	}
	if c.body.UserID != "u-9" || c.body.Query != "如何部署" || c.body.TopK != 3 {
		t.Fatalf("请求体不符: %+v", c.body)
	}

	if !strings.Contains(out, `知识库检索："如何部署"`) || !strings.Contains(out, "命中 2 条") {
		t.Fatalf("结果头部不符:\n%s", out)
	}
	if !strings.Contains(out, "来源：docs/a.pdf（doc-1）") || !strings.Contains(out, "相关度：0.830") {
		t.Fatalf("来源与相关度缺失:\n%s", out)
	}
	if !strings.Contains(out, "知识库第一条原文") || !strings.Contains(out, "知识库第二条原文") {
		t.Fatalf("原文缺失:\n%s", out)
	}
}

func TestHiddenUserIDArgumentWinsOverDefault(t *testing.T) {
	ts, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request, _ *captured) {
		w.Write([]byte(`{"items":[{"text":"x"}]}`))
	})
	tool := New(Options{BaseURL: ts.URL, Token: "t", DefaultUserID: "env-user"})

	// user_id 不在 inputSchema 中公开，但允许上层注入。
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"q","user_id":"injected-user"}`)); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if c.body.UserID != "injected-user" {
		t.Fatalf("隐藏参数未生效: %+v", c.body)
	}

	props := tool.InputSchema()["properties"].(map[string]any)
	if _, exposed := props["user_id"]; exposed {
		t.Fatal("user_id 绝不应出现在公开 schema 中（防模型伪造越权检索）")
	}
}

func TestTopKIsClamped(t *testing.T) {
	ts, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request, _ *captured) {
		w.Write([]byte(`{"items":[{"text":"x"}]}`))
	})
	tool := New(Options{BaseURL: ts.URL, Token: "t"})

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"q","top_k":999}`)); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if c.body.TopK != MaxTopK {
		t.Fatalf("topK 应被夹到 %d，实际 %d", MaxTopK, c.body.TopK)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"q"}`)); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if c.body.TopK != DefaultTopK {
		t.Fatalf("topK 默认值应为 %d，实际 %d", DefaultTopK, c.body.TopK)
	}
}

func TestDegradesInsteadOfFailing(t *testing.T) {
	cases := []struct {
		name    string
		handler func(w http.ResponseWriter, r *http.Request, _ *captured)
		hint    string
	}{
		{
			name:    "接口 500",
			handler: func(w http.ResponseWriter, r *http.Request, _ *captured) { w.WriteHeader(500); w.Write([]byte("boom")) },
			hint:    "HTTP 500",
		},
		{
			name: "接口 403",
			handler: func(w http.ResponseWriter, r *http.Request, _ *captured) {
				w.WriteHeader(403)
				w.Write([]byte("no token"))
			},
			hint: "HTTP 403",
		},
		{
			name:    "返回体不是 JSON",
			handler: func(w http.ResponseWriter, r *http.Request, _ *captured) { w.Write([]byte("<html>login</html>")) },
			hint:    "无法解析",
		},
		{
			name:    "返回体结构不符",
			handler: func(w http.ResponseWriter, r *http.Request, _ *captured) { w.Write([]byte(`{"items":"not-an-array"}`)) },
			hint:    "无法解析",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := newTestServer(t, tc.handler)
			tool := New(Options{BaseURL: ts.URL, Token: "t"})
			out, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"q"}`))
			if err != nil {
				t.Fatalf("降级路径绝不能报错（否则会中断整轮对话）: %v", err)
			}
			if !strings.Contains(out, tc.hint) {
				t.Fatalf("降级说明应包含 %q:\n%s", tc.hint, out)
			}
			if !strings.Contains(out, "web_search") {
				t.Fatalf("降级说明应给出替代路径:\n%s", out)
			}
		})
	}
}

func TestDegradesOnTimeout(t *testing.T) {
	ts, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request, _ *captured) {
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte(`{"items":[]}`))
	})
	tool := New(Options{BaseURL: ts.URL, Token: "t", Timeout: 50 * time.Millisecond})

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatalf("超时应降级而非报错: %v", err)
	}
	if !strings.Contains(out, "知识库检索请求失败") {
		t.Fatalf("超时说明不符:\n%s", out)
	}
}

func TestDegradesWhenTokenMissingWithoutCallingServer(t *testing.T) {
	ts, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request, _ *captured) {
		w.Write([]byte(`{"items":[]}`))
	})
	tool := New(Options{BaseURL: ts.URL, Token: ""})

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if !strings.Contains(out, "AGENT_INTERNAL_TOKEN") {
		t.Fatalf("应提示缺少内部口令:\n%s", out)
	}
	if c.calls != 0 {
		t.Fatalf("未配置口令时不应发请求，实际 %d 次", c.calls)
	}
}

func TestDegradesWhenServerUnreachable(t *testing.T) {
	// 指向一个必然连不上的本地端口。
	tool := New(Options{BaseURL: "http://127.0.0.1:1", Token: "t", Timeout: 200 * time.Millisecond})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatalf("连不上应降级: %v", err)
	}
	if !strings.Contains(out, "web_search") {
		t.Fatalf("降级说明应给出替代路径:\n%s", out)
	}
}

func TestEmptyHitsGetFriendlyText(t *testing.T) {
	ts, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request, _ *captured) {
		w.Write([]byte(`{"items":[]}`))
	})
	tool := New(Options{BaseURL: ts.URL, Token: "t"})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"不存在的问题"}`))
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if !strings.Contains(out, "未检索到") || !strings.Contains(out, "web_search") {
		t.Fatalf("空命中说明不符:\n%s", out)
	}
}

func TestRejectsBadArguments(t *testing.T) {
	tool := New(Options{BaseURL: "http://127.0.0.1:1", Token: "t"})
	for _, args := range []string{`{}`, `{"query":""}`, `{"query":"   "}`, `bad json`} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("args=%s 应报错", args)
		}
	}
}

func TestTruncatesVeryLongItemText(t *testing.T) {
	long := strings.Repeat("字", maxItemChars+50)
	ts, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request, _ *captured) {
		body, _ := json.Marshal(searchResponse{Items: []searchItem{{Text: long, Source: "s"}}})
		w.Write(body)
	})
	out, err := New(Options{BaseURL: ts.URL, Token: "t"}).Execute(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if n := strings.Count(out, "字"); n != maxItemChars {
		t.Fatalf("片段截断字符数 = %d, 期望 %d", n, maxItemChars)
	}
}

func TestBaseURLTrailingSlashIsTrimmed(t *testing.T) {
	ts, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request, _ *captured) {
		w.Write([]byte(`{"items":[{"text":"x"}]}`))
	})
	tool := New(Options{BaseURL: ts.URL + "/", Token: "t"})
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"q"}`)); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if c.path != SearchPath {
		t.Fatalf("路径拼接出错: %q", c.path)
	}
}

func TestSchemaAndName(t *testing.T) {
	tool := New(Options{})
	if tool.Name() != "kb_search" {
		t.Fatalf("工具名 = %q", tool.Name())
	}
	if err := tools.ValidateInputSchema(tool.InputSchema()); err != nil {
		t.Fatalf("schema 不合规: %v", err)
	}
}
