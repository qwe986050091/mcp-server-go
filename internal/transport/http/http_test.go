package mcphttp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mcp-server-go/internal/protocol"
	"mcp-server-go/internal/server"
	"mcp-server-go/internal/tools"
)

// clientAccept 是 LangChain4j StreamableHttpMcpTransport 固定发送的 Accept 头取值。
const clientAccept = "application/json,text/event-stream"

// echoTool 用于覆盖 tools/call 的成功路径。
type echoTool struct{}

func (echoTool) Name() string        { return "echo" }
func (echoTool) Description() string { return "回显输入文本" }
func (echoTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "要回显的文本"},
		},
		"required": []any{"text"},
	}
}
func (echoTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	return "echo: " + in.Text, nil
}

func newTestTransport(t *testing.T, opts Options) *Transport {
	t.Helper()
	reg := tools.NewRegistry()
	if err := reg.Register(echoTool{}); err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	if opts.Logger == nil {
		opts.Logger = log.New(io.Discard, LogPrefix, 0)
	}
	if opts.Path == "" {
		opts.Path = DefaultPath
	}
	return New(server.New(server.Options{
		Name:     protocol.ServerName,
		Version:  protocol.ServerVersion,
		Registry: reg,
		Logger:   opts.Logger,
	}), opts)
}

// doPost 按客户端真实头组合发一次 POST。
func doPost(t *testing.T, base, path, body string, extra map[string]string) *http.Response {
	t.Helper()
	return doPostWithAccept(t, base, path, body, clientAccept, extra)
}

func doPostWithAccept(t *testing.T, base, path, body, accept string, extra map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	return string(raw)
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"langchain4j-mcp","version":"1.17.2-beta27"}}}`

func TestInitializeReturnsJSONAndSessionID(t *testing.T) {
	ts := httptest.NewServer(newTestTransport(t, Options{}).Handler())
	defer ts.Close()

	resp := doPost(t, ts.URL, DefaultPath, initBody, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（响应体 %s）", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, 期望 application/json（回 text/event-stream 会让客户端走 SSE 解析）", ct)
	}
	sid := resp.Header.Get(protocol.HeaderSessionID)
	if sid == "" {
		t.Fatal("initialize 响应必须带 Mcp-Session-Id")
	}
	if len(sid) != 32 {
		t.Fatalf("会话 id 长度 = %d, 期望 32 位十六进制", len(sid))
	}
	var decoded struct {
		Result protocol.InitializeResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("响应体不是合法 JSON-RPC: %v（%s）", err, body)
	}
	if decoded.Result.ProtocolVersion != "2025-11-25" {
		t.Fatalf("应回显 2025-11-25，实际 %q", decoded.Result.ProtocolVersion)
	}
}

func TestNotificationsReturn202WithEmptyBody(t *testing.T) {
	ts := httptest.NewServer(newTestTransport(t, Options{}).Handler())
	defer ts.Close()

	resp := doPost(t, ts.URL, DefaultPath, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("通知状态码 = %d, 期望 202", resp.StatusCode)
	}
	if body != "" {
		t.Fatalf("通知响应体应为空，实际 %q", body)
	}
	if resp.Header.Get(protocol.HeaderSessionID) != "" {
		t.Fatal("通知响应不应下发会话 id")
	}
}

func TestSessionIDIsOptionalAndUnknownIsTolerated(t *testing.T) {
	tr := newTestTransport(t, Options{})
	ts := httptest.NewServer(tr.Handler())
	defer ts.Close()

	initResp := doPost(t, ts.URL, DefaultPath, initBody, nil)
	readBody(t, initResp)
	sid := initResp.Header.Get(protocol.HeaderSessionID)

	// 不带会话 id：放行（客户端实现里初始化前也不带）。
	resp := doPost(t, ts.URL, DefaultPath, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("不带会话 id 应放行，实际 %d（%s）", resp.StatusCode, readBody(t, resp))
	}
	readBody(t, resp)

	// 带正确会话 id：放行。
	resp = doPost(t, ts.URL, DefaultPath, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		map[string]string{protocol.HeaderSessionID: sid})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("带合法会话 id 应放行，实际 %d", resp.StatusCode)
	}
	readBody(t, resp)

	// 带未知会话 id：刻意放行（服务端每请求无状态，避免客户端被 404 触发重新 initialize）。
	resp = doPost(t, ts.URL, DefaultPath, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`,
		map[string]string{protocol.HeaderSessionID: "0123456789abcdef0123456789abcdef"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("未知会话 id 应放行（宽松校验），实际 %d", resp.StatusCode)
	}
	readBody(t, resp)
}

func TestMissingAcceptEventStreamIs406(t *testing.T) {
	ts := httptest.NewServer(newTestTransport(t, Options{}).Handler())
	defer ts.Close()

	for _, accept := range []string{"", "application/json", "*/*", "text/html"} {
		resp := doPostWithAccept(t, ts.URL, DefaultPath, initBody, accept, nil)
		readBody(t, resp)
		if resp.StatusCode != http.StatusNotAcceptable {
			t.Fatalf("Accept=%q 状态码 = %d, 期望 406", accept, resp.StatusCode)
		}
	}
}

func TestWrongContentTypeIs415(t *testing.T) {
	ts := httptest.NewServer(newTestTransport(t, Options{}).Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+DefaultPath, strings.NewReader(initBody))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Accept", clientAccept)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("状态码 = %d, 期望 415", resp.StatusCode)
	}
}

func TestGetIs405AndDeleteIs204(t *testing.T) {
	tr := newTestTransport(t, Options{})
	ts := httptest.NewServer(tr.Handler())
	defer ts.Close()

	getResp, err := http.Get(ts.URL + DefaultPath)
	if err != nil {
		t.Fatalf("GET 失败: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET 状态码 = %d, 期望 405", getResp.StatusCode)
	}
	if allow := getResp.Header.Get("Allow"); !strings.Contains(allow, "POST") {
		t.Fatalf("405 必须带 Allow 头，实际 %q", allow)
	}

	sid := tr.sessions.create()
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+DefaultPath, nil)
	req.Header.Set(protocol.HeaderSessionID, sid)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE 失败: %v", err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE 状态码 = %d, 期望 204", delResp.StatusCode)
	}
	if tr.sessions.len() != 0 {
		t.Fatalf("DELETE 后会话表应清空，实际 %d 条", tr.sessions.len())
	}
}

func TestToolsListAndCallOverHTTP(t *testing.T) {
	tr := newTestTransport(t, Options{})
	ts := httptest.NewServer(tr.Handler())
	defer ts.Close()

	resp := doPost(t, ts.URL, DefaultPath, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	body := readBody(t, resp)
	var listed struct {
		Result protocol.ListToolsResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("解析失败: %v（%s）", err, body)
	}
	if len(listed.Result.Tools) != 1 || listed.Result.Tools[0].Name != "echo" {
		t.Fatalf("工具列表不符: %s", body)
	}
	if err := tools.ValidateInputSchema(listed.Result.Tools[0].InputSchema); err != nil {
		t.Fatalf("HTTP 下发的 schema 不合规: %v", err)
	}

	resp = doPost(t, ts.URL, DefaultPath, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`, nil)
	body = readBody(t, resp)
	if !strings.Contains(body, `"text":"echo: hi"`) {
		t.Fatalf("tools/call 结果不符: %s", body)
	}
}

func TestMalformedBodyIs400WithParseError(t *testing.T) {
	ts := httptest.NewServer(newTestTransport(t, Options{}).Handler())
	defer ts.Close()

	for _, body := range []string{`{"jsonrpc":`, `[]`, `   `} {
		resp := doPost(t, ts.URL, DefaultPath, body, nil)
		raw := readBody(t, resp)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body=%q 状态码 = %d, 期望 400", body, resp.StatusCode)
		}
		if !strings.Contains(raw, `"code":-32700`) && !strings.Contains(raw, `"code":-32600`) {
			t.Fatalf("错误帧体不符: %s", raw)
		}
	}
}

func TestOversizedBodyIs413(t *testing.T) {
	tr := newTestTransport(t, Options{})
	tr.maxBodyLen = 64
	ts := httptest.NewServer(tr.Handler())
	defer ts.Close()

	resp := doPost(t, ts.URL, DefaultPath, initBody, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d, 期望 413", resp.StatusCode)
	}
}

func TestOriginValidation(t *testing.T) {
	ts := httptest.NewServer(newTestTransport(t, Options{}).Handler())
	defer ts.Close()

	cases := []struct {
		origin string
		want   int
	}{
		{"", http.StatusOK},
		{"http://127.0.0.1:5173", http.StatusOK},
		{"http://localhost:3000", http.StatusOK},
		{"http://[::1]:8080", http.StatusOK},
		{"https://evil.example.com", http.StatusForbidden},
		{"http://192.168.1.10", http.StatusForbidden},
	}
	for _, tc := range cases {
		extra := map[string]string{}
		if tc.origin != "" {
			extra["Origin"] = tc.origin
		}
		resp := doPost(t, ts.URL, DefaultPath, initBody, extra)
		readBody(t, resp)
		if resp.StatusCode != tc.want {
			t.Fatalf("Origin=%q 状态码 = %d, 期望 %d", tc.origin, resp.StatusCode, tc.want)
		}
	}
}

func TestSharedTokenIsEnforced(t *testing.T) {
	ts := httptest.NewServer(newTestTransport(t, Options{Token: "s3cr3t-token"}).Handler())
	defer ts.Close()

	// 缺失 / 错误 / 长度不同：一律 403。
	for _, token := range []string{"", "wrong", "s3cr3t", "s3cr3t-token-extra"} {
		extra := map[string]string{}
		if token != "" {
			extra[TokenHeader] = token
		}
		resp := doPost(t, ts.URL, DefaultPath, initBody, extra)
		readBody(t, resp)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("token=%q 状态码 = %d, 期望 403", token, resp.StatusCode)
		}
	}
	resp := doPost(t, ts.URL, DefaultPath, initBody, map[string]string{TokenHeader: "s3cr3t-token"})
	readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("正确 token 应放行，实际 %d", resp.StatusCode)
	}
}

func TestProtocolVersionHeaderIsIgnored(t *testing.T) {
	ts := httptest.NewServer(newTestTransport(t, Options{}).Handler())
	defer ts.Close()

	// 客户端根本不发该头；即便发了不支持的版本也不能 400（本服务不因版本不符拒绝连接）。
	for _, v := range []string{"", "2025-03-26", "1999-01-01"} {
		extra := map[string]string{}
		if v != "" {
			extra[protocol.HeaderProtocolVersion] = v
		}
		resp := doPost(t, ts.URL, DefaultPath, initBody, extra)
		readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("MCP-Protocol-Version=%q 状态码 = %d, 期望 200", v, resp.StatusCode)
		}
	}
}

func TestUnknownPathIs404AndTrailingSlashIsNotRedirected(t *testing.T) {
	ts := httptest.NewServer(newTestTransport(t, Options{}).Handler())
	defer ts.Close()

	for _, path := range []string{"/", "/other", "/mcp/", "/mcp/x"} {
		resp := doPost(t, ts.URL, path, initBody, nil)
		raw := readBody(t, resp)
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			t.Fatalf("path=%s 返回了 3xx（%d），客户端会判定失败", path, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("path=%s 状态码 = %d, 期望 404（响应体 %s）", path, resp.StatusCode, raw)
		}
	}
}

func TestNoRedirectForAnyMethod(t *testing.T) {
	ts := httptest.NewServer(newTestTransport(t, Options{}).Handler())
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodOptions, http.MethodHead} {
		req, _ := http.NewRequest(method, ts.URL+DefaultPath, nil)
		req.Header.Set("Accept", clientAccept)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s 请求失败: %v", method, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Fatalf("%s 状态码 = %d, 期望 4xx 且非 3xx", method, resp.StatusCode)
		}
	}
}

func TestCustomPathIsHonored(t *testing.T) {
	tr := newTestTransport(t, Options{Path: "rpc"})
	ts := httptest.NewServer(tr.Handler())
	defer ts.Close()

	resp := doPost(t, ts.URL, "/rpc", initBody, nil)
	readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("自定义路径状态码 = %d, 期望 200", resp.StatusCode)
	}
	resp = doPost(t, ts.URL, DefaultPath, initBody, nil)
	readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("默认路径应 404，实际 %d", resp.StatusCode)
	}
}

func TestEnsureLoopbackRejectsNonLoopback(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:3001", true},
		{"localhost:3001", true},
		{"[::1]:3001", true},
		{":3001", false},
		{"0.0.0.0:3001", false},
		{"192.168.1.10:3001", false},
		{"example.com:3001", false},
		{"127.0.0.1", false},
	}
	for _, tc := range cases {
		err := EnsureLoopback(tc.addr)
		if (err == nil) != tc.ok {
			t.Fatalf("EnsureLoopback(%q) 结果与期望不符: err=%v ok=%v", tc.addr, err, tc.ok)
		}
	}
}

func TestSessionStoreExpiresAndRenews(t *testing.T) {
	store := newSessionStore(50 * time.Millisecond)
	id := store.create()
	if !store.known(id) {
		t.Fatal("新建会话应命中")
	}
	time.Sleep(70 * time.Millisecond)
	if store.known(id) {
		t.Fatal("超过 TTL 且期间未续期的会话应过期")
	}
}
