// Package jsonrpc 实现 JSON-RPC 2.0 消息的编解码。
//
// 只覆盖 MCP 需要的子集：请求 / 通知 / 成功响应 / 错误响应。
// 关键点：id 保留原始 JSON 片段（数字 / 字符串 / null），回显时类型不变形 —— 不少
// 客户端（含 LangChain4j）会对 id 做类型敏感的匹配。
package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Version 是 JSON-RPC 固定版本号。
const Version = "2.0"

// 标准 JSON-RPC 错误码。
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// nullLiteral 是 id 缺省时使用的字面量。
var nullLiteral = json.RawMessage("null")

// ID 表示 JSON-RPC 的 id 字段，内部保留原始编码片段。
type ID struct {
	raw json.RawMessage
}

// NullID 构造 id=null（用于无法确定请求 id 的错误响应）。
func NullID() *ID { return &ID{raw: nullLiteral} }

// NewNumberID 构造数字型 id（主要用于测试与内部构造）。
func NewNumberID(n int64) *ID {
	return &ID{raw: json.RawMessage(fmt.Sprintf("%d", n))}
}

// NewStringID 构造字符串型 id。
func NewStringID(s string) *ID {
	b, _ := json.Marshal(s)
	return &ID{raw: b}
}

// MarshalJSON 原样输出已保留的编码；空值退化为 null。
func (id *ID) MarshalJSON() ([]byte, error) {
	if id == nil || len(id.raw) == 0 {
		return nullLiteral, nil
	}
	return id.raw, nil
}

// UnmarshalJSON 校验 id 类型（仅接受字符串 / 数字 / null）并保留原始片段。
func (id *ID) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return errors.New("jsonrpc: id 为空")
	}
	switch {
	case trimmed[0] == '"': // 字符串
	case trimmed[0] == 'n': // null
		if !bytes.Equal(trimmed, nullLiteral) {
			return errors.New("jsonrpc: id 只能是字符串、数字或 null")
		}
	case trimmed[0] == '-' || (trimmed[0] >= '0' && trimmed[0] <= '9'): // 数字
	default:
		return fmt.Errorf("jsonrpc: id 类型不支持: %s", trimmed)
	}
	id.raw = append(json.RawMessage(nil), trimmed...)
	return nil
}

// String 返回 id 的可读形式，仅用于日志。
func (id *ID) String() string {
	if id == nil || len(id.raw) == 0 {
		return "<nil>"
	}
	return string(id.raw)
}

// Error 是 JSON-RPC 错误对象。
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error 实现 error 接口。
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("jsonrpc %d: %s", e.Code, e.Message)
}

// Message 是 JSON-RPC 消息的统一载体（请求 / 通知 / 响应共用）。
//
// 字段顺序即编码顺序，保持稳定以便金标测试逐字节比对。
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *ID             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// IsNotification 判断是否为通知：带 method 且无 id。
func (m *Message) IsNotification() bool {
	return m != nil && m.Method != "" && m.ID == nil
}

// IsRequest 判断是否为请求：带 method 且有 id。
func (m *Message) IsRequest() bool {
	return m != nil && m.Method != "" && m.ID != nil
}

// NewResult 构造成功响应。
func NewResult(id *ID, result any) *Message {
	return &Message{JSONRPC: Version, ID: id, Result: result}
}

// NewError 构造错误响应；id 为空时退化为 null。
func NewError(id *ID, code int, message string) *Message {
	if id == nil {
		id = NullID()
	}
	return &Message{JSONRPC: Version, ID: id, Error: &Error{Code: code, Message: message}}
}

// Validate 校验请求/通知的基本结构；返回非 nil 表示应回 Invalid Request。
func (m *Message) Validate() error {
	if m == nil {
		return errors.New("空消息")
	}
	if m.JSONRPC != Version {
		return fmt.Errorf("jsonrpc 字段必须为 %q，实际为 %q", Version, m.JSONRPC)
	}
	if m.Method == "" {
		return errors.New("缺少 method")
	}
	return nil
}
