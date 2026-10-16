// Package mcphttp 实现 MCP 的 Streamable HTTP 传输（单端点）。
//
// 包名刻意取作 mcphttp（而非目录名的 http）以避免与标准库 net/http 的包名冲突。
//
// 与 LangChain4j 1.17.2-beta27 的 StreamableHttpMcpTransport 实测行为对齐：
//   - 客户端只发 POST，请求头固定为
//     `Content-Type: application/json` + `Accept: application/json,text/event-stream`
//     （非 initialize 请求额外带 `Mcp-Session-Id`）；
//   - 客户端从不发送 Origin 与 MCP-Protocol-Version，因此二者都不得成为硬性拒绝条件；
//   - 客户端判定成功只看 2xx；响应 Content-Type 含 text/event-stream 时会走 SSE 解析，
//     所以本实现统一回 application/json（单个 JSON-RPC 对象）；
//   - 通知类输入回 202 空体。
package mcphttp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"mcp-server-go/internal/jsonrpc"
	"mcp-server-go/internal/protocol"
	"mcp-server-go/internal/server"
)

// LogPrefix 与 stdio 传输保持一致，便于日志检索。
const LogPrefix = "[mcp-server-go] "

// TokenHeader 是可选共享密钥头。
const TokenHeader = "X-MCP-Token"

// 默认参数。
const (
	DefaultPath     = "/mcp"
	defaultMaxBody  = 4 << 20 // 4 MiB
	defaultTTL      = 30 * time.Minute
	readHeaderLimit = 5 * time.Second
	idleTimeout     = 2 * time.Minute
	maxHeaderBytes  = 1 << 16
)

// Options 是 HTTP 传输的构造参数。
type Options struct {
	// Path 是端点路径，空则用 DefaultPath。
	Path string
	// Logger 必须写 stderr；为 nil 时静默。
	Logger *log.Logger
	// Token 非空时，所有请求必须带 TokenHeader 且常量时间相等。
	Token string
	// SessionTTL 是内存会话表条目的存活时间，0 表示 defaultTTL。
	SessionTTL time.Duration
}

// Transport 是 Streamable HTTP 端点。
type Transport struct {
	srv        *server.Server
	path       string
	logger     *log.Logger
	token      string
	sessions   *sessionStore
	maxBodyLen int64
}

// New 构造 HTTP 传输。
func New(srv *server.Server, opts Options) *Transport {
	if srv == nil {
		panic("mcphttp: server 不能为 nil")
	}
	path := opts.Path
	if path == "" {
		path = DefaultPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.New(io.Discard, LogPrefix, log.LstdFlags)
	}
	ttl := opts.SessionTTL
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &Transport{
		srv:        srv,
		path:       path,
		logger:     logger,
		token:      opts.Token,
		sessions:   newSessionStore(ttl),
		maxBodyLen: defaultMaxBody,
	}
}

// Handler 返回 http.Handler，便于 httptest 注入。
//
// 刻意不使用 http.ServeMux：ServeMux 会把 `/mcp/` 301 重定向到 `/mcp`，
// 而 MCP 客户端对 3xx 一律视为失败（本项目约定绝不返回 3xx）。
func (t *Transport) Handler() http.Handler { return t }

// Listen 在 addr 上监听并阻塞服务；只允许回环地址。
func (t *Transport) Listen(ctx context.Context, addr string) error {
	if err := EnsureLoopback(addr); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", addr, err)
	}
	httpSrv := &http.Server{
		Handler:           t,
		ReadHeaderTimeout: readHeaderLimit,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          t.logger,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()
	t.logger.Printf("HTTP 端点已监听 http://%s%s", addr, t.path)
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("HTTP 服务退出: %w", err)
	}
	return nil
}

// ServeHTTP 是端点分发：不做任何重定向，也不返回 3xx。
func (t *Transport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != t.path {
		t.fail(w, http.StatusNotFound, jsonrpc.CodeInvalidRequest, "未知端点: "+r.URL.Path)
		return
	}
	// 共享密钥（可选）：非空则必须常量时间匹配。
	if t.token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get(TokenHeader)), []byte(t.token)) != 1 {
		t.fail(w, http.StatusForbidden, jsonrpc.CodeInvalidRequest, "共享密钥校验失败（请在 "+TokenHeader+" 头提供正确口令）")
		return
	}
	// DNS rebinding 防护：仅放行来自回环 Origin 的浏览器请求；无 Origin 的非浏览器客户端（如 LangChain4j）放行。
	if origin := r.Header.Get("Origin"); origin != "" && !isLoopbackOrigin(origin) {
		t.fail(w, http.StatusForbidden, jsonrpc.CodeInvalidRequest, "Origin 非回环地址，已拒绝: "+origin)
		return
	}
	// MCP-Protocol-Version 头刻意不做校验：客户端根本不发，且我们不因版本不符拒绝连接。

	switch r.Method {
	case http.MethodPost:
		t.handlePost(w, r)
	case http.MethodDelete:
		t.handleDelete(w, r)
	case http.MethodGet:
		// 本服务不提供 SSE 长连接通道；客户端默认也不启用该通道，收到 405 仅打一条 warn。
		t.failWithAllow(w, http.StatusMethodNotAllowed, "本服务不提供 SSE 长连接通道，请使用 POST")
	default:
		t.failWithAllow(w, http.StatusMethodNotAllowed, "仅支持 POST 与 DELETE，收到 "+r.Method)
	}
}

func (t *Transport) handlePost(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream") {
		t.fail(w, http.StatusNotAcceptable, jsonrpc.CodeInvalidRequest,
			"Accept 头必须包含 text/event-stream（MCP 客户端标准取值：application/json,text/event-stream）")
		return
	}
	if contentType := strings.ToLower(r.Header.Get("Content-Type")); contentType != "" && !strings.Contains(contentType, "application/json") {
		t.fail(w, http.StatusUnsupportedMediaType, jsonrpc.CodeInvalidRequest, "Content-Type 必须为 application/json")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, t.maxBodyLen+1))
	if err != nil {
		t.fail(w, http.StatusBadRequest, jsonrpc.CodeParseError, "读取请求体失败: "+err.Error())
		return
	}
	if int64(len(body)) > t.maxBodyLen {
		t.fail(w, http.StatusRequestEntityTooLarge, jsonrpc.CodeInvalidRequest,
			fmt.Sprintf("请求体超过 %d 字节上限", t.maxBodyLen))
		return
	}

	// 会话校验采用「宽松校验」：会话表只用于管理生命周期与 DELETE 终止；
	// 服务端每请求无状态，所以未知/过期会话 id 仅告警放行，避免客户端因 404 触发重新 initialize。
	if sid := r.Header.Get(protocol.HeaderSessionID); sid != "" && !t.sessions.known(sid) {
		t.logger.Printf("收到未知会话 id，已放行（每请求无状态）: %s", sid)
	}

	msg, err := decodeSingleMessage(body)
	if err != nil {
		t.fail(w, http.StatusBadRequest, jsonrpc.CodeParseError, err.Error())
		return
	}

	// 通知（无 id）：静默处理并回 202 空体。
	if msg.IsNotification() {
		t.srv.Handle(r.Context(), msg)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := t.srv.Handle(r.Context(), msg)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	// initialize 响应下发会话 id（客户端会缓存并在后续请求回传）。
	if msg.Method == server.MethodInitialize {
		w.Header().Set(protocol.HeaderSessionID, t.sessions.create())
	}
	t.writeJSON(w, http.StatusOK, resp)
}

func (t *Transport) handleDelete(w http.ResponseWriter, r *http.Request) {
	sid := r.Header.Get(protocol.HeaderSessionID)
	if sid != "" {
		t.sessions.delete(sid)
		t.logger.Printf("已终止会话: %s", sid)
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeSingleMessage 解析单个 JSON-RPC 对象；不支持 JSON-RPC 批处理数组。
func decodeSingleMessage(body []byte) (*jsonrpc.Message, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil, errors.New("请求体为空")
	}
	if trimmed[0] == '[' {
		return nil, errors.New("不支持 JSON-RPC 批处理数组（MCP 2025-06-18 起已移除批处理）")
	}
	var msg jsonrpc.Message
	if err := json.Unmarshal([]byte(trimmed), &msg); err != nil {
		return nil, errors.New("JSON 解析失败: " + err.Error())
	}
	return &msg, nil
}

func (t *Transport) writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.logger.Printf("写出响应失败: %v", err)
	}
}

// fail 以 JSON-RPC 错误帧作为 HTTP 层错误的响应体：HTTP 状态码承载精确语义，
// 帧体保持可解析（id 恒为 null）。
func (t *Transport) fail(w http.ResponseWriter, status int, code int, message string) {
	t.logger.Printf("HTTP %d: %s", status, message)
	t.writeJSON(w, status, jsonrpc.NewError(nil, code, message))
}

// failWithAllow 在 405 时补 Allow 头。
func (t *Transport) failWithAllow(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Allow", "POST, DELETE")
	t.fail(w, status, jsonrpc.CodeInvalidRequest, message)
}

// EnsureLoopback 校验监听地址只指向回环，拒绝 ":3001" 这类隐式全网卡绑定。
func EnsureLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("监听地址必须形如 127.0.0.1:3001，解析 %q 失败: %w", addr, err)
	}
	if host == "" {
		return errors.New("必须显式写出回环地址（例如 127.0.0.1:3001），不接受隐式绑定全部网卡")
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("只允许绑定回环地址（127.0.0.1 / ::1 / localhost），当前为 %q", host)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("只允许绑定回环地址（127.0.0.1 / ::1 / localhost），当前为 %q", host)
	}
	return nil
}

// isLoopbackOrigin 判断 Origin 是否指向本机。
func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sessionStore 是内存会话表：只用于生命周期管理与 DELETE 终止，
// 不做鉴权（服务端无状态）。过期条目在读写时惰性清理，不额外起 goroutine。
type sessionStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]time.Time
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{ttl: ttl, entries: make(map[string]time.Time)}
}

func (s *sessionStore) create() string {
	id := randomID()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked(time.Now())
	s.entries[id] = time.Now()
	return id
}

func (s *sessionStore) known(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.purgeLocked(now)
	if _, ok := s.entries[id]; !ok {
		return false
	}
	// 命中即续期：活跃连接不会被 TTL 掉。
	s.entries[id] = now
	return true
}

func (s *sessionStore) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, id)
}

func (s *sessionStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func (s *sessionStore) purgeLocked(now time.Time) {
	for id, created := range s.entries {
		if now.Sub(created) > s.ttl {
			delete(s.entries, id)
		}
	}
}

// randomID 生成 32 位十六进制随机会话 id。
func randomID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// 熵源不可用属于极端异常；退回纳秒时间戳，保证仍能产出唯一 id。
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
