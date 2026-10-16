// Package protocol 定义 MCP 协议的数据结构（初始化协商、工具列表、工具调用）。
//
// 与 LangChain4j 1.17.2-beta27 客户端实测行为对齐：
//   - 客户端 initialize 发送的 protocolVersion 为 2025-11-25；
//   - 服务端应回显客户端请求的版本，不支持时回退到 DefaultProtocolVersion，
//     但绝不因版本不匹配而拒绝连接。
package protocol

import "encoding/json"

// 服务端身份与版本常量。
const (
	// ServerName 是 initialize 响应里 serverInfo.name 的取值。
	ServerName = "mcp-server-go"
	// ServerVersion 是 initialize 响应里 serverInfo.version 的取值。
	ServerVersion = "0.1.0"
	// DefaultProtocolVersion 是客户端请求了未知版本时的回退版本。
	DefaultProtocolVersion = "2025-11-25"
)

// HTTP 传输用到的协议头（stdio 传输不使用）。
const (
	// HeaderProtocolVersion 是后续请求携带的协议版本头（标准名为 MCP-Protocol-Version）。
	HeaderProtocolVersion = "MCP-Protocol-Version"
	// HeaderSessionID 是 initialize 响应下发的会话 id 头。
	HeaderSessionID = "Mcp-Session-Id"
)

// supportedVersions 是允许原样回显的版本集合。
var supportedVersions = map[string]bool{
	"2025-11-25": true,
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

// NegotiateVersion 返回应回显给客户端的协议版本：
// 客户端请求的版本在支持集内则原样回显，否则回退到 DefaultProtocolVersion。
func NegotiateVersion(requested string) string {
	if supportedVersions[requested] {
		return requested
	}
	return DefaultProtocolVersion
}

// ImplementationInfo 描述客户端或服务端实现信息。
type ImplementationInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeParams 是 initialize 请求的 params。
type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    map[string]any     `json:"capabilities,omitempty"`
	ClientInfo      ImplementationInfo `json:"clientInfo"`
}

// ToolsCapability 声明服务端工具能力。
type ToolsCapability struct {
	// ListChanged 恒为 false：本项目工具列表在运行期不变。
	ListChanged bool `json:"listChanged"`
}

// ServerCapabilities 声明服务端能力集合（resources / prompts 暂不实现）。
type ServerCapabilities struct {
	Tools *ToolsCapability `json:"tools,omitempty"`
}

// InitializeResult 是 initialize 的响应。
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      ImplementationInfo `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

// Tool 是 tools/list 返回的工具描述。
//
// 注意：InputSchema 必须只含白名单关键字，否则 LangChain4j 的
// ToolSpecificationHelper 会抛错并导致 listTools() 失败（连接判定失败）。
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ListToolsResult 是 tools/list 的响应。
type ListToolsResult struct {
	Tools []Tool `json:"tools"`
}

// CallToolParams 是 tools/call 的 params。
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Content 是 tools/call 结果的内容块；当前只支持 type=text。
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Text 构造一个文本内容块。
func Text(s string) Content {
	return Content{Type: "text", Text: s}
}

// CallToolResult 是 tools/call 的响应：必须含 content 数组；
// 业务失败用 IsError=true + 可读文本，协议错误才走 JSON-RPC error。
type CallToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}
