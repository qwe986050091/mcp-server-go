package safehttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// 默认限额。
const (
	DefaultTimeout      = 10 * time.Second
	DefaultMaxBodyBytes = 2 << 20 // 2 MiB
	DefaultMaxRedirects = 3
	DefaultUserAgent    = "mcp-server-go/0.1 (+https://github.com/qwe986050091/mcp-server-go)"
	dialTimeout         = 5 * time.Second
	tlsTimeout          = 5 * time.Second
)

// Options 是 Client 的构造参数。
type Options struct {
	// Guard 为 nil 时使用 NewGuard(nil)（系统解析器 + 严格策略）。
	Guard *Guard
	// Timeout 是单次 Do 的默认超时；调用方 ctx 已带 deadline 时以调用方为准。
	Timeout time.Duration
	// MaxBodyBytes 是单次响应体读取上限，超出即截断并置 Truncated。
	MaxBodyBytes int64
	// MaxRedirects 是允许的最大跳数（默认 3，绝不自动跟随）。
	MaxRedirects int
	// UserAgent 为空时使用 DefaultUserAgent。
	UserAgent string
	// Transport 覆盖底层 RoundTripper，仅供测试注入。
	Transport http.RoundTripper
}

// Client 是带 SSRF 防护的 HTTP 客户端。
type Client struct {
	guard        *Guard
	httpClient   *http.Client
	maxBody      int64
	maxRedirects int
	userAgent    string
	timeout      time.Duration
}

// Response 是一次请求的最终响应。
type Response struct {
	StatusCode  int
	Header      http.Header
	Body        []byte
	FinalURL    string
	ContentType string
	// Truncated 表示响应体超过 MaxBodyBytes 被截断。
	Truncated bool
}

// New 构造 Client。
func New(opts Options) *Client {
	guard := opts.Guard
	if guard == nil {
		guard = NewGuard(nil)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}
	maxRedirects := opts.MaxRedirects
	if maxRedirects < 0 {
		maxRedirects = 0
	} else if maxRedirects == 0 {
		maxRedirects = DefaultMaxRedirects
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	transport := opts.Transport
	if transport == nil {
		transport = newTransport(guard)
	}
	return &Client{
		guard: guard,
		httpClient: &http.Client{
			Transport: transport,
			// 绝不自动跟随重定向：跳转策略由 Do 逐跳重校验后自行推进。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		maxBody:      maxBody,
		maxRedirects: maxRedirects,
		userAgent:    ua,
		timeout:      timeout,
	}
}

// newTransport 构造底层连接池：显式禁用环境代理（否则 http_proxy 可绕过 IP 校验），
// 并在拨号时重新解析 + 重新校验，封死 DNS rebinding。
func newTransport(guard *Guard) *http.Transport {
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("拨号地址非法: %w", err)
			}
			ips, err := guard.CheckHost(ctx, host)
			if err != nil {
				return nil, err
			}
			var lastErr error
			for _, ip := range ips {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr == nil {
				lastErr = errors.New("没有可用地址")
			}
			return nil, lastErr
		},
		TLSHandshakeTimeout: tlsTimeout,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
	}
}

// Get 是 Do(GET, ...) 的语法糖，使用客户端配置的最大跳数。
func (c *Client) Get(ctx context.Context, rawURL string, headers map[string]string) (*Response, error) {
	return c.Do(ctx, http.MethodGet, rawURL, headers, c.maxRedirects)
}

// Do 发起请求并**手动**逐跳推进重定向：每一跳都重新做语法与 IP 校验，
// 超过 maxRedirects 立刻中止。
func (c *Client) Do(ctx context.Context, method, rawURL string, headers map[string]string, maxRedirects int) (*Response, error) {
	if maxRedirects < 0 {
		maxRedirects = c.maxRedirects
	}
	if _, ok := ctx.Deadline(); !ok && c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	u, err := c.guard.CheckURL(rawURL)
	if err != nil {
		return nil, err
	}
	for hop := 0; ; hop++ {
		// 预校验：提前失败并给出精确原因（拨号时还会再校验一次）。
		if _, err := c.guard.CheckHost(ctx, u.Hostname()); err != nil {
			return nil, err
		}
		resp, err := c.once(ctx, method, u.String(), headers)
		if err != nil {
			return nil, err
		}
		if !isRedirect(resp.StatusCode) {
			resp.FinalURL = u.String()
			return resp, nil
		}
		location := resp.Header.Get("Location")
		if location == "" {
			// 3xx 但没有 Location：按最终响应返回，交给上层判断。
			resp.FinalURL = u.String()
			return resp, nil
		}
		if hop >= maxRedirects {
			return nil, fmt.Errorf("重定向超过 %d 跳上限，已中止（最后一跳指向 %s）", maxRedirects, location)
		}
		next, err := resolveLocation(u, location)
		if err != nil {
			return nil, fmt.Errorf("重定向目标非法: %w", err)
		}
		if err := c.guard.CheckParsedURL(next); err != nil {
			return nil, fmt.Errorf("重定向目标被拒绝: %w", err)
		}
		u = next
	}
}

// once 执行单次请求并读取响应体（超限截断）。
func (c *Client) once(ctx context.Context, method, urlStr string, headers map[string]string) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", urlStr, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("读取 %s 响应体失败: %w", urlStr, err)
	}
	truncated := int64(len(body)) > c.maxBody
	if truncated {
		body = body[:c.maxBody]
	}
	return &Response{
		StatusCode:  resp.StatusCode,
		Header:      resp.Header,
		Body:        body,
		ContentType: resp.Header.Get("Content-Type"),
		Truncated:   truncated,
	}, nil
}

// isRedirect 判断是否为需要跟随的重定向状态码。
func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// resolveLocation 相对当前 URL 解析 Location。
func resolveLocation(base *url.URL, location string) (*url.URL, error) {
	next, err := base.Parse(location)
	if err != nil {
		return nil, err
	}
	return next, nil
}
