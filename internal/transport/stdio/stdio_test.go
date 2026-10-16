package stdio

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-server-go/internal/jsonrpc"
	"mcp-server-go/internal/protocol"
	"mcp-server-go/internal/server"
	"mcp-server-go/internal/tools"
)

// goldenPath 基于测试源文件定位仓库根下的 testdata/stdio，
// 这样无论以哪个工作目录运行测试二进制都能找到金标文件。
func goldenPath(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位测试源文件所在目录")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "stdio", name)
}

// newServer 构造被测 server；logOut 为 nil 时静默，便于断言「日志只走 stderr」。
func newServer(logOut io.Writer) *server.Server {
	logger := log.New(io.Discard, LogPrefix, 0)
	if logOut != nil {
		logger = log.New(logOut, LogPrefix, 0)
	}
	return server.New(server.Options{
		Name:     protocol.ServerName,
		Version:  protocol.ServerVersion,
		Registry: tools.NewRegistry(),
		Logger:   logger,
	})
}

// readLines 读取金标文件的行（忽略空行并去掉行尾 \r）。
func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取金标文件失败 %s: %v", path, err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// TestServeMatchesGoldenFrames 校验「喂 N 帧 → stdout 恰好 M 行合法 JSON」且逐字节与金标一致。
func TestServeMatchesGoldenFrames(t *testing.T) {
	frames := readLines(t, goldenPath(t, "frames.jsonl"))
	expected := readLines(t, goldenPath(t, "expected.jsonl"))

	var stdout, stderr bytes.Buffer
	in := strings.NewReader(strings.Join(frames, "\n") + "\n")
	if err := New(in, &stdout, &stderr).Serve(context.Background(), newServer(&stderr)); err != nil {
		t.Fatalf("Serve 返回错误: %v", err)
	}

	gotRaw := strings.TrimRight(stdout.String(), "\n")
	var got []string
	if gotRaw != "" {
		got = strings.Split(gotRaw, "\n")
	}

	if len(got) != len(expected) {
		t.Fatalf("响应行数 = %d, 期望 %d\n实际输出:\n%s", len(got), len(expected), stdout.String())
	}
	for i := range expected {
		if got[i] != expected[i] {
			t.Errorf("第 %d 行不一致\n got: %s\nwant: %s", i+1, got[i], expected[i])
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(got[i]), &decoded); err != nil {
			t.Errorf("第 %d 行不是合法 JSON: %v", i+1, err)
		}
	}

	// stdout 必须保持纯净：不能混入日志前缀，且日志确实落到了 stderr。
	if strings.Contains(stdout.String(), LogPrefix) {
		t.Errorf("stdout 被日志污染:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), LogPrefix) {
		t.Errorf("stderr 应有日志输出，实际:\n%s", stderr.String())
	}
}

// TestServeReportsParseError 校验坏帧回 -32700 且 id 为 null。
func TestServeReportsParseError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	in := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\"\n")
	if err := New(in, &stdout, &stderr).Serve(context.Background(), newServer(nil)); err != nil {
		t.Fatalf("Serve 返回错误: %v", err)
	}
	got := strings.TrimRight(stdout.String(), "\n")
	var decoded struct {
		ID    json.RawMessage `json:"id"`
		Error *jsonrpc.Error  `json:"error"`
	}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("坏帧响应不是合法 JSON: %v（原文 %s）", err, got)
	}
	if decoded.Error == nil || decoded.Error.Code != jsonrpc.CodeParseError {
		t.Fatalf("应返回 -32700，实际: %s", got)
	}
	if string(decoded.ID) != "null" {
		t.Fatalf("坏帧响应的 id 必须为 null，实际: %s", string(decoded.ID))
	}
}

// TestServeRejectsOversizedFrame 校验超长单帧被拒绝且不拖挂进程。
func TestServeRejectsOversizedFrame(t *testing.T) {
	var stdout, stderr bytes.Buffer
	huge := strings.Repeat("x", maxFrameBytes+1024)
	err := New(strings.NewReader(huge+"\n"), &stdout, &stderr).Serve(context.Background(), newServer(nil))
	if err == nil {
		t.Fatal("超长帧应返回错误")
	}
	if !strings.Contains(stdout.String(), `"code":-32700`) {
		t.Fatalf("超长帧应回 -32700: %s", stdout.String())
	}
}

// TestServeToleratesBlankLinesAndCRLF 校验空行与 CRLF 帧不影响协议帧计数。
func TestServeToleratesBlankLinesAndCRLF(t *testing.T) {
	var stdout, stderr bytes.Buffer
	in := strings.NewReader("\r\n" + `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\r\n\r\n")
	if err := New(in, &stdout, &stderr).Serve(context.Background(), newServer(nil)); err != nil {
		t.Fatalf("Serve 返回错误: %v", err)
	}
	got := strings.TrimRight(stdout.String(), "\n")
	want := `{"jsonrpc":"2.0","id":1,"result":{}}`
	if got != want {
		t.Fatalf("CRLF/空行处理不符\n got: %s\nwant: %s", got, want)
	}
}

// TestServeExitsOnContextCancel 校验上下文取消能正常收尾。
func TestServeExitsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if err := New(strings.NewReader(""), &stdout, &stderr).Serve(ctx, newServer(nil)); err != nil {
		t.Fatalf("取消上下文应正常退出: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("取消后不应写出任何内容: %s", stdout.String())
	}
}

// TestServeToleratesLeadingBOM 校验首帧带 UTF-8 BOM 时仍能正常解析。
func TestServeToleratesLeadingBOM(t *testing.T) {
	var stdout, stderr bytes.Buffer
	payload := "\ufeff" + `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"
	if err := New(strings.NewReader(payload), &stdout, &stderr).Serve(context.Background(), newServer(nil)); err != nil {
		t.Fatalf("Serve 返回错误: %v", err)
	}
	got := strings.TrimRight(stdout.String(), "\n")
	want := `{"jsonrpc":"2.0","id":1,"result":{}}`
	if got != want {
		t.Fatalf("BOM 帧处理不符\n got: %s\nwant: %s", got, want)
	}
}

// TestEncoderDoesNotEscapeHTML 校验正文里的 < > & 不被转义（web 抓取结果可读性）。
func TestEncoderDoesNotEscapeHTML(t *testing.T) {
	var out bytes.Buffer
	tr := New(strings.NewReader(""), &out, io.Discard)
	tr.write(jsonrpc.NewResult(jsonrpc.NewNumberID(1), map[string]any{"html": "<b>a&amp;b</b>"}))
	got := strings.TrimRight(out.String(), "\n")
	if !strings.Contains(got, `"<b>a&amp;b</b>"`) {
		t.Fatalf("HTML 不应被转义为 \\u003c 等序列: %s", got)
	}
	if strings.Contains(got, `\u003`) {
		t.Fatalf("出现了 HTML 转义序列: %s", got)
	}
}
