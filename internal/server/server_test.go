package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"mcp-server-go/internal/jsonrpc"
	"mcp-server-go/internal/protocol"
	"mcp-server-go/internal/tools"
)

// echoTool 回显参数；failTool 返回业务错误；panicTool 直接 panic。
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

type failTool struct{}

func (failTool) Name() string        { return "fail" }
func (failTool) Description() string { return "总是失败" }
func (failTool) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}}
}
func (failTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "", errors.New("上游超时")
}

type panicTool struct{}

func (panicTool) Name() string        { return "boom" }
func (panicTool) Description() string { return "会 panic" }
func (panicTool) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}}
}
func (panicTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	panic("测试用 panic")
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	reg := tools.NewRegistry()
	for _, tool := range []tools.Tool{echoTool{}, failTool{}, panicTool{}} {
		if err := reg.Register(tool); err != nil {
			t.Fatalf("注册工具失败: %v", err)
		}
	}
	return New(Options{
		Name:     protocol.ServerName,
		Version:  protocol.ServerVersion,
		Registry: reg,
	})
}

// call 构造并处理一条请求。
func call(t *testing.T, srv *Server, id *jsonrpc.ID, method string, params string) *jsonrpc.Message {
	t.Helper()
	msg := &jsonrpc.Message{JSONRPC: "2.0", ID: id, Method: method}
	if params != "" {
		msg.Params = json.RawMessage(params)
	}
	resp := srv.Handle(context.Background(), msg)
	if resp == nil {
		t.Fatalf("方法 %s 应返回响应，实际为 nil", method)
	}
	return resp
}

func TestInitializeEchoesClientVersion(t *testing.T) {
	srv := newTestServer(t)
	resp := call(t, srv, jsonrpc.NewNumberID(1), "initialize",
		`{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"c","version":"1"}}`)

	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	var decoded struct {
		Result struct {
			protocol.InitializeResult
			Capabilities struct {
				Tools struct {
					ListChanged bool `json:"listChanged"`
				} `json:"tools"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if decoded.Result.ProtocolVersion != "2024-11-05" {
		t.Fatalf("应回显客户端版本，实际 = %q", decoded.Result.ProtocolVersion)
	}
	if decoded.Result.ServerInfo.Name != protocol.ServerName {
		t.Fatalf("serverInfo.name = %s", decoded.Result.ServerInfo.Name)
	}
	if !strings.Contains(string(out), `"tools"`) {
		t.Fatalf("应声明 tools 能力: %s", out)
	}
}

func TestInitializeFallsBackForUnknownVersion(t *testing.T) {
	srv := newTestServer(t)
	resp := call(t, srv, jsonrpc.NewNumberID(1), "initialize",
		`{"protocolVersion":"1999-01-01","clientInfo":{"name":"c","version":"1"}}`)
	out, _ := json.Marshal(resp)
	if !strings.Contains(string(out), `"protocolVersion":"`+protocol.DefaultProtocolVersion+`"`) {
		t.Fatalf("未知版本应回退到 %s: %s", protocol.DefaultProtocolVersion, out)
	}
}

func TestInitializeWithoutParamsStillSucceeds(t *testing.T) {
	srv := newTestServer(t)
	resp := call(t, srv, jsonrpc.NewNumberID(1), "initialize", "")
	if resp.Error != nil {
		t.Fatalf("缺 params 不应报错: %v", resp.Error)
	}
}

func TestPingReturnsEmptyObject(t *testing.T) {
	srv := newTestServer(t)
	resp := call(t, srv, jsonrpc.NewStringID("p-1"), "ping", "")
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	want := `{"jsonrpc":"2.0","id":"p-1","result":{}}`
	if string(out) != want {
		t.Fatalf("ping 响应不符\n got: %s\nwant: %s", out, want)
	}
}

func TestToolsListReturnsAllToolsWithSchema(t *testing.T) {
	srv := newTestServer(t)
	resp := call(t, srv, jsonrpc.NewNumberID(2), "tools/list", "{}")
	out, _ := json.Marshal(resp)
	var decoded struct {
		Result protocol.ListToolsResult `json:"result"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(decoded.Result.Tools) != 3 {
		t.Fatalf("工具数 = %d, 期望 3", len(decoded.Result.Tools))
	}
	for _, tool := range decoded.Result.Tools {
		if err := tools.ValidateInputSchema(tool.InputSchema); err != nil {
			t.Fatalf("工具 %s 的 schema 不合规: %v", tool.Name, err)
		}
	}
}

func TestToolsListIsEmptyArrayNotNull(t *testing.T) {
	srv := New(Options{Registry: tools.NewRegistry()})
	resp := call(t, srv, jsonrpc.NewNumberID(2), "tools/list", "")
	out, _ := json.Marshal(resp)
	if !strings.Contains(string(out), `"tools":[]`) {
		t.Fatalf("空工具列表应序列化为 []: %s", out)
	}
}

func TestToolsCallSuccess(t *testing.T) {
	srv := newTestServer(t)
	resp := call(t, srv, jsonrpc.NewNumberID(3), "tools/call", `{"name":"echo","arguments":{"text":"hi"}}`)
	out, _ := json.Marshal(resp)
	var decoded struct {
		Result protocol.CallToolResult `json:"result"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if decoded.Result.IsError {
		t.Fatalf("不应标记为错误: %s", out)
	}
	if len(decoded.Result.Content) != 1 || decoded.Result.Content[0].Type != "text" {
		t.Fatalf("content 结构不符: %s", out)
	}
	if decoded.Result.Content[0].Text != "echo: hi" {
		t.Fatalf("工具输出 = %q", decoded.Result.Content[0].Text)
	}
}

func TestToolsCallMissingArgumentsBecomesEmptyObject(t *testing.T) {
	srv := newTestServer(t)
	resp := call(t, srv, jsonrpc.NewNumberID(3), "tools/call", `{"name":"fail"}`)
	if resp.Error != nil {
		t.Fatalf("arguments 缺省应视为 {}: %v", resp.Error)
	}
}

func TestToolsCallBusinessFailureUsesIsError(t *testing.T) {
	srv := newTestServer(t)
	resp := call(t, srv, jsonrpc.NewNumberID(3), "tools/call", `{"name":"fail","arguments":{}}`)
	if resp.Error != nil {
		t.Fatalf("业务失败不应走 JSON-RPC error: %v", resp.Error)
	}
	out, _ := json.Marshal(resp)
	var decoded struct {
		Result protocol.CallToolResult `json:"result"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !decoded.Result.IsError {
		t.Fatalf("应标记 isError=true: %s", out)
	}
	if !strings.Contains(decoded.Result.Content[0].Text, "上游超时") {
		t.Fatalf("应回传可读原因: %s", out)
	}
}

func TestToolsCallPanicIsContained(t *testing.T) {
	srv := newTestServer(t)
	resp := call(t, srv, jsonrpc.NewNumberID(3), "tools/call", `{"name":"boom","arguments":{}}`)
	if resp.Error != nil {
		t.Fatalf("panic 应被兜住而非冒泡为协议错误: %v", resp.Error)
	}
	out, _ := json.Marshal(resp)
	if !strings.Contains(string(out), `"isError":true`) || !strings.Contains(string(out), "panic") {
		t.Fatalf("panic 应转为 isError 文本: %s", out)
	}
}

func TestToolsCallUnknownToolIsInvalidParams(t *testing.T) {
	srv := newTestServer(t)
	resp := call(t, srv, jsonrpc.NewNumberID(3), "tools/call", `{"name":"nope","arguments":{}}`)
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("未知工具应返回 -32602: %+v", resp.Error)
	}
}

func TestToolsCallNonObjectArgumentsIsInvalidParams(t *testing.T) {
	srv := newTestServer(t)
	resp := call(t, srv, jsonrpc.NewNumberID(3), "tools/call", `{"name":"echo","arguments":"hi"}`)
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("arguments 非对象应返回 -32602: %+v", resp.Error)
	}
}

func TestToolsCallBadParamsJSONIsInvalidParams(t *testing.T) {
	srv := newTestServer(t)
	resp := call(t, srv, jsonrpc.NewNumberID(3), "tools/call", `{"name":`)
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("参数解析失败应返回 -32602: %+v", resp.Error)
	}
}

func TestUnknownMethodIsMethodNotFound(t *testing.T) {
	srv := newTestServer(t)
	resp := call(t, srv, jsonrpc.NewNumberID(4), "no/such/method", "")
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeMethodNotFound {
		t.Fatalf("未知方法应返回 -32601: %+v", resp.Error)
	}
}

func TestNotificationsAreSilent(t *testing.T) {
	srv := newTestServer(t)
	for _, method := range []string{"notifications/initialized", "notifications/cancelled"} {
		msg := &jsonrpc.Message{JSONRPC: "2.0", Method: method}
		if resp := srv.Handle(context.Background(), msg); resp != nil {
			t.Fatalf("%s 应静默，实际返回: %+v", method, resp)
		}
	}
}

func TestInvalidJSONRPCVersionIsInvalidRequest(t *testing.T) {
	srv := newTestServer(t)
	msg := &jsonrpc.Message{JSONRPC: "1.0", ID: jsonrpc.NewNumberID(1), Method: "ping"}
	resp := srv.Handle(context.Background(), msg)
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidRequest {
		t.Fatalf("版本不符应返回 -32600: %+v", resp.Error)
	}
}

func TestMissingMethodIsInvalidRequest(t *testing.T) {
	srv := newTestServer(t)
	msg := &jsonrpc.Message{JSONRPC: "2.0", ID: jsonrpc.NewNumberID(1)}
	resp := srv.Handle(context.Background(), msg)
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidRequest {
		t.Fatalf("缺少 method 应返回 -32600: %+v", resp.Error)
	}
}

func TestNilMessageIsIgnored(t *testing.T) {
	if resp := newTestServer(t).Handle(context.Background(), nil); resp != nil {
		t.Fatalf("nil 消息应被忽略，实际: %+v", resp)
	}
}
