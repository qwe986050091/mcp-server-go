// Command mcp-server 是一个零第三方依赖的 MCP Server。
//
// 用法：
//
//	mcp-server --transport stdio
//	mcp-server --transport http --addr 127.0.0.1:3001 --path /mcp
//
// 注意：stdout 只承载协议帧，所有日志一律写 stderr。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"

	"mcp-server-go/internal/protocol"
	"mcp-server-go/internal/server"
	"mcp-server-go/internal/tools"
	"mcp-server-go/internal/tools/calc"
	"mcp-server-go/internal/tools/coderun"
	"mcp-server-go/internal/tools/kbsearch"
	"mcp-server-go/internal/tools/webfetch"
	"mcp-server-go/internal/tools/websearch"
	mcphttp "mcp-server-go/internal/transport/http"
	"mcp-server-go/internal/transport/stdio"
)

// serverInstructions 会随 initialize 下发给客户端，帮助模型选择工具。
const serverInstructions = "本服务提供五个工具：web_search（免 API Key 网页搜索）、" +
	"web_fetch（抓取网页并返回正文纯文本）、kb_search（检索私有知识库）、" +
	"code_run（在无网络的 Docker 沙箱中执行代码）、calc（数学表达式求值）。" +
	"抓取类工具带 SSRF 防护：内网、回环、链路本地与云元数据地址一律拒绝。"

// registerTools 显式登记全部内置工具（新增工具必须在此登记，否则不对外可见）。
func registerTools(logger *log.Logger) *tools.Registry {
	registry := tools.NewRegistry()
	for _, tool := range []tools.Tool{
		calc.New(),
		webfetch.New(webfetch.Options{}),
		websearch.New(websearch.Options{}),
		coderun.New(coderun.Options{}),
		kbsearch.New(kbsearch.Options{
			BaseURL:       os.Getenv("KB_BASE_URL"),
			Token:         os.Getenv("AGENT_INTERNAL_TOKEN"),
			DefaultUserID: os.Getenv("KB_USER_ID"),
		}),
	} {
		if err := registry.Register(tool); err != nil {
			logger.Fatalf("工具登记失败: %v", err)
		}
	}
	return registry
}

func main() {
	transport := flag.String("transport", "stdio", "传输方式：stdio | http")
	addr := flag.String("addr", "127.0.0.1:3001", "HTTP 监听地址，只允许回环（transport=http 时生效）")
	path := flag.String("path", "/mcp", "HTTP 端点路径（transport=http 时生效）")
	token := flag.String("token", os.Getenv("MCP_HTTP_TOKEN"), "HTTP 共享密钥，非空则要求 "+mcphttp.TokenHeader+" 头；也可用环境变量 MCP_HTTP_TOKEN 注入")
	flag.Parse()

	logger := log.New(os.Stderr, stdio.LogPrefix, log.LstdFlags)

	registry := registerTools(logger)
	srv := server.New(server.Options{
		Name:         protocol.ServerName,
		Version:      protocol.ServerVersion,
		Instructions: serverInstructions,
		Registry:     registry,
		Logger:       logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch *transport {
	case "stdio":
		logger.Printf("启动：transport=stdio，工具 %d 个，%s %s",
			registry.Len(), protocol.ServerName, protocol.ServerVersion)
		if err := stdio.New(os.Stdin, os.Stdout, os.Stderr).Serve(ctx, srv); err != nil {
			logger.Fatalf("stdio 传输退出（异常）: %v", err)
		}
		logger.Printf("stdin 已关闭，正常退出")
	case "http":
		logger.Printf("启动：transport=http，工具 %d 个，%s %s",
			registry.Len(), protocol.ServerName, protocol.ServerVersion)
		t := mcphttp.New(srv, mcphttp.Options{
			Path:   *path,
			Logger: logger,
			Token:  *token,
		})
		if err := t.Listen(ctx, *addr); err != nil {
			logger.Fatalf("HTTP 传输退出（异常）: %v", err)
		}
		logger.Printf("HTTP 服务已停止，正常退出")
	default:
		logger.Fatalf("不支持的 transport: %q（仅支持 stdio | http）", *transport)
	}
}
