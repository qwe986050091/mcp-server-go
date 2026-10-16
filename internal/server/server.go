// Package server 实现 MCP 的方法路由：把 JSON-RPC 请求映射为协议响应。
//
// 传输层只管收发帧，所有协议语义集中在这里，保证 stdio 与 HTTP 行为一致。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"

	"mcp-server-go/internal/jsonrpc"
	"mcp-server-go/internal/protocol"
	"mcp-server-go/internal/tools"
)

// 方法名常量。
const (
	MethodInitialize = "initialize"
	MethodPing       = "ping"
	MethodToolsList  = "tools/list"
	MethodToolsCall  = "tools/call"
)

// Options 是 Server 的构造参数。
type Options struct {
	// Name / Version 用于 initialize 响应里的 serverInfo。
	Name    string
	Version string
	// Instructions 会随 initialize 下发给客户端（可为空，空则不下发该字段）。
	Instructions string
	// Registry 是工具注册表，不可为 nil。
	Registry *tools.Registry
	// Logger 用于运行期日志，必须写 stderr；为 nil 时静默。
	Logger *log.Logger
}

// Server 是协议路由核心，无状态、可被多连接复用。
type Server struct {
	name         string
	version      string
	instructions string
	registry     *tools.Registry
	logger       *log.Logger
}

// New 构造 Server。
func New(opts Options) *Server {
	name := opts.Name
	if name == "" {
		name = protocol.ServerName
	}
	version := opts.Version
	if version == "" {
		version = protocol.ServerVersion
	}
	registry := opts.Registry
	if registry == nil {
		registry = tools.NewRegistry()
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Server{
		name:         name,
		version:      version,
		instructions: opts.Instructions,
		registry:     registry,
		logger:       logger,
	}
}

// Handle 处理一条入站消息；返回 nil 表示无需响应（通知）。
func (s *Server) Handle(ctx context.Context, req *jsonrpc.Message) *jsonrpc.Message {
	if req == nil {
		return nil
	}
	// 通知一律静默（notifications/initialized、notifications/cancelled 等），不回任何东西。
	if req.IsNotification() {
		s.logger.Printf("静默处理通知: method=%s", req.Method)
		return nil
	}
	if err := req.Validate(); err != nil {
		s.logger.Printf("非法请求（id=%s）: %v", req.ID.String(), err)
		return jsonrpc.NewError(idOrNull(req.ID), jsonrpc.CodeInvalidRequest, "非法请求: "+err.Error())
	}

	switch req.Method {
	case MethodInitialize:
		return s.handleInitialize(req)
	case MethodPing:
		// ping 结果必须是空对象 {}，不能是 null。
		return jsonrpc.NewResult(req.ID, json.RawMessage("{}"))
	case MethodToolsList:
		return s.handleToolsList(req)
	case MethodToolsCall:
		return s.handleToolsCall(ctx, req)
	default:
		return jsonrpc.NewError(req.ID, jsonrpc.CodeMethodNotFound, "未知方法: "+req.Method)
	}
}

// Tools 暴露注册表，便于测试与后续 HTTP 传输复用。
func (s *Server) Tools() *tools.Registry {
	return s.registry
}

func (s *Server) handleInitialize(req *jsonrpc.Message) *jsonrpc.Message {
	var params protocol.InitializeParams
	if len(req.Params) > 0 && !isJSONNull(req.Params) {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return jsonrpc.NewError(req.ID, jsonrpc.CodeInvalidParams, "initialize 参数解析失败: "+err.Error())
		}
	}
	result := protocol.InitializeResult{
		// 回显客户端版本；未知版本回退到 DefaultProtocolVersion，不拒绝连接。
		ProtocolVersion: protocol.NegotiateVersion(params.ProtocolVersion),
		Capabilities: protocol.ServerCapabilities{
			Tools: &protocol.ToolsCapability{ListChanged: false},
		},
		ServerInfo:   protocol.ImplementationInfo{Name: s.name, Version: s.version},
		Instructions: s.instructions,
	}
	s.logger.Printf("initialize: client=%s/%s 请求版本=%s 回显版本=%s",
		params.ClientInfo.Name, params.ClientInfo.Version,
		params.ProtocolVersion, result.ProtocolVersion)
	return jsonrpc.NewResult(req.ID, result)
}

func (s *Server) handleToolsList(req *jsonrpc.Message) *jsonrpc.Message {
	list := s.registry.List()
	out := make([]protocol.Tool, 0, len(list))
	for _, t := range list {
		out = append(out, protocol.Tool{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		})
	}
	// 非 nil 空切片保证序列化为 []，而不是 null。
	return jsonrpc.NewResult(req.ID, protocol.ListToolsResult{Tools: out})
}

func (s *Server) handleToolsCall(ctx context.Context, req *jsonrpc.Message) *jsonrpc.Message {
	var params protocol.CallToolParams
	if len(req.Params) > 0 && !isJSONNull(req.Params) {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return jsonrpc.NewError(req.ID, jsonrpc.CodeInvalidParams, "tools/call 参数解析失败: "+err.Error())
		}
	}
	if params.Name == "" {
		return jsonrpc.NewError(req.ID, jsonrpc.CodeInvalidParams, "tools/call 缺少 name")
	}
	tool, ok := s.registry.Get(params.Name)
	if !ok {
		return jsonrpc.NewError(req.ID, jsonrpc.CodeInvalidParams, "未知工具: "+params.Name)
	}
	args := params.Arguments
	if len(args) == 0 || isJSONNull(args) {
		args = json.RawMessage("{}")
	} else if firstByte(args) != '{' {
		return jsonrpc.NewError(req.ID, jsonrpc.CodeInvalidParams, "tools/call 的 arguments 必须是 JSON 对象")
	}

	text, err := s.execute(ctx, tool, args)
	if err != nil {
		// 业务失败用 isError 结果回传，让模型看到可读原因（超时/SSRF 拒绝/沙箱不可用等）。
		s.logger.Printf("工具执行失败: %s: %v", tool.Name(), err)
		return jsonrpc.NewResult(req.ID, protocol.CallToolResult{
			Content: []protocol.Content{protocol.Text(err.Error())},
			IsError: true,
		})
	}
	s.logger.Printf("工具执行成功: %s（输出 %d 字节）", tool.Name(), len(text))
	return jsonrpc.NewResult(req.ID, protocol.CallToolResult{
		Content: []protocol.Content{protocol.Text(text)},
	})
}

// execute 执行工具并把 panic 兜成错误 —— 单个工具崩溃绝不能带挂整个 server。
func (s *Server) execute(ctx context.Context, t tools.Tool, args json.RawMessage) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("工具 %s 内部 panic: %v", t.Name(), r)
		}
	}()
	return t.Execute(ctx, args)
}

// idOrNull 在 id 缺失时返回 null，保证错误响应结构合法。
func idOrNull(id *jsonrpc.ID) *jsonrpc.ID {
	if id == nil {
		return jsonrpc.NullID()
	}
	return id
}

// isJSONNull 判断原始 JSON 片段是否为 null。
func isJSONNull(raw json.RawMessage) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case 'n':
			return true
		default:
			return false
		}
	}
	return false
}

// firstByte 返回首个非空白字节；空片段返回 0。
func firstByte(raw json.RawMessage) byte {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return b
		}
	}
	return 0
}
