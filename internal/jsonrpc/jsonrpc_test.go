package jsonrpc

import (
	"encoding/json"
	"testing"
)

func TestIDPreservesTypeOnRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"数字 id", `{"jsonrpc":"2.0","id":7,"method":"ping"}`, "7"},
		{"字符串 id", `{"jsonrpc":"2.0","id":"p-1","method":"ping"}`, `"p-1"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var msg Message
			if err := json.Unmarshal([]byte(tc.in), &msg); err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if msg.ID == nil {
				t.Fatal("id 不应为 nil")
			}
			if got := msg.ID.String(); got != tc.want {
				t.Fatalf("id 原始片段 = %s, 期望 %s", got, tc.want)
			}
			out, err := json.Marshal(msg)
			if err != nil {
				t.Fatalf("编码失败: %v", err)
			}
			if got := string(out); got != tc.in {
				t.Fatalf("往返后不一致\n got: %s\nwant: %s", got, tc.in)
			}
		})
	}
}

// TestNullIDIsTreatedAsAbsent 记录一个刻意的取舍：入站 `id: null` 会被 encoding/json
// 折叠为「无 id」，因此按通知语义静默处理。真实 MCP 客户端（含 LangChain4j）不会这么发。
func TestNullIDIsTreatedAsAbsent(t *testing.T) {
	var msg Message
	if err := json.Unmarshal([]byte(`{"jsonrpc":"2.0","id":null,"method":"ping"}`), &msg); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if msg.ID != nil {
		t.Fatalf("入站 id:null 应折叠为无 id，实际 = %s", msg.ID.String())
	}
	if !msg.IsNotification() {
		t.Fatal("应被识别为通知")
	}
}

func TestNotificationHasNilID(t *testing.T) {
	var msg Message
	if err := json.Unmarshal([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), &msg); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !msg.IsNotification() {
		t.Fatal("应被识别为通知")
	}
	if msg.IsRequest() {
		t.Fatal("不应被识别为请求")
	}
}

func TestRejectInvalidIDType(t *testing.T) {
	cases := []string{
		`{"jsonrpc":"2.0","id":true,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":{},"method":"ping"}`,
		`{"jsonrpc":"2.0","id":[],"method":"ping"}`,
	}
	for _, in := range cases {
		var msg Message
		if err := json.Unmarshal([]byte(in), &msg); err == nil {
			t.Fatalf("非法 id 应被拒绝: %s", in)
		}
	}
}

func TestErrorResponseEncoding(t *testing.T) {
	msg := NewError(NewStringID("abc"), CodeMethodNotFound, "未知方法: x")
	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	want := `{"jsonrpc":"2.0","id":"abc","error":{"code":-32601,"message":"未知方法: x"}}`
	if string(out) != want {
		t.Fatalf("错误响应编码不符\n got: %s\nwant: %s", out, want)
	}
}

func TestErrorWithNilIDFallsBackToNull(t *testing.T) {
	out, err := json.Marshal(NewError(nil, CodeParseError, "坏帧"))
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	want := `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"坏帧"}}`
	if string(out) != want {
		t.Fatalf("id 缺省应退化为 null\n got: %s\nwant: %s", out, want)
	}
}

func TestValidateRequiresJSONRPC20AndMethod(t *testing.T) {
	cases := []struct {
		name string
		msg  Message
		ok   bool
	}{
		{"正常请求", Message{JSONRPC: "2.0", ID: NewNumberID(1), Method: "ping"}, true},
		{"版本不对", Message{JSONRPC: "1.0", ID: NewNumberID(1), Method: "ping"}, false},
		{"缺少 method", Message{JSONRPC: "2.0", ID: NewNumberID(1)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.msg.Validate(); (err == nil) != tc.ok {
				t.Fatalf("Validate() 结果与期望不符: err=%v ok=%v", err, tc.ok)
			}
		})
	}
}
