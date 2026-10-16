// Package stdio 实现 MCP 的 stdio 传输。
//
// 帧格式为「按行分隔的 JSON」（每帧一行，以 \n 结束），不是 Content-Length 头。
// 铁律：stdout 只允许出现协议帧，任何日志/横幅一律走 stderr ——
// stdout 混入一行非 JSON 文本就会让客户端解析失败、连接断开。
package stdio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"

	"mcp-server-go/internal/jsonrpc"
	"mcp-server-go/internal/server"
)

// LogPrefix 是所有日志行的统一前缀，便于客户端侧区分。
const LogPrefix = "[mcp-server-go] "

// maxFrameBytes 是单帧上限（1 MiB），足够容纳图片类结果的内联文本。
const maxFrameBytes = 1 << 20

// utf8BOM 是 UTF-8 字节序标记；部分写入端（例如 Windows PowerShell 5.1 的管道）会
// 在首帧前面带上它。这里做宽容处理：剥掉即可，不必让客户端为此连接失败。
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// Transport 是 stdio 传输实例。
type Transport struct {
	scanner *bufio.Scanner
	encoder *json.Encoder
	sink    io.Writer
	logger  *log.Logger
	writeMu sync.Mutex
}

// New 构造 stdio 传输：in 为 stdin、out 为 stdout、logOut 为 stderr。
func New(in io.Reader, out io.Writer, logOut io.Writer) *Transport {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), maxFrameBytes)

	encoder := json.NewEncoder(out)
	// 关闭 HTML 转义：web 抓取类工具会返回含 < > & 的正文，转义后会显著膨胀且不可读。
	encoder.SetEscapeHTML(false)

	return &Transport{
		scanner: scanner,
		encoder: encoder,
		sink:    out,
		logger:  log.New(logOut, LogPrefix, log.LstdFlags),
	}
}

// Serve 循环读取帧直到 stdin 结束或 ctx 取消；返回 nil 表示正常收尾。
func (t *Transport) Serve(ctx context.Context, srv *server.Server) error {
	for {
		if err := ctx.Err(); err != nil {
			// 上下文取消视为正常退出路径。
			return nil
		}
		if !t.scanner.Scan() {
			break
		}
		// Scanner 的分隔符已吃掉 \n，这里再兜一层 \r 以兼容 CRLF 写入端。
		line := bytes.TrimRight(t.scanner.Bytes(), "\r\n")
		line = bytes.TrimPrefix(line, utf8BOM)
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var req jsonrpc.Message
		if err := json.Unmarshal(line, &req); err != nil {
			t.logger.Printf("解析入站帧失败: %v", err)
			t.write(jsonrpc.NewError(jsonrpc.NullID(), jsonrpc.CodeParseError, "JSON 解析失败: "+err.Error()))
			continue
		}
		if resp := srv.Handle(ctx, &req); resp != nil {
			t.write(resp)
		}
	}

	if err := t.scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			t.write(jsonrpc.NewError(jsonrpc.NullID(), jsonrpc.CodeParseError,
				fmt.Sprintf("单帧超过 %d 字节上限", maxFrameBytes)))
			return fmt.Errorf("入站帧超长: %w", err)
		}
		return fmt.Errorf("读取 stdin 失败: %w", err)
	}
	return nil
}

// write 输出一条响应：单行 JSON + \n，并立即 Flush（缓冲写入端）。
func (t *Transport) write(msg *jsonrpc.Message) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	if err := t.encoder.Encode(msg); err != nil {
		t.logger.Printf("写出响应失败: %v", err)
		return
	}
	if flusher, ok := t.sink.(interface{ Flush() error }); ok {
		if err := flusher.Flush(); err != nil {
			t.logger.Printf("flush stdout 失败: %v", err)
		}
	}
}
