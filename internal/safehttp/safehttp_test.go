package safehttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCheckURLRejectsUnsafeForms(t *testing.T) {
	g := NewGuard(nil)
	cases := []struct {
		name string
		url  string
		ok   bool
	}{
		{"http", "http://example.com/a", true},
		{"https", "https://example.com/a?b=1#c", true},
		{"带端口", "http://example.com:8080/a", true},
		{"IPv6 字面量", "http://[2001:4860:4860::8888]/", true},
		{"空", "", false},
		{"缺协议", "example.com/a", false},
		{"协议相对", "//example.com/a", false},
		{"ftp", "ftp://example.com/a", false},
		{"file", "file:///etc/passwd", false},
		{"javascript", "javascript:alert(1)", false},
		{"data", "data:text/html;base64,PHNjcmlwdD4=", false},
		{"userinfo", "http://user:pass@example.com/", false},
		{"仅 userinfo 无 host", "http://user@/", false},
		{"缺 host", "http:///path", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := g.CheckURL(tc.url)
			if (err == nil) != tc.ok {
				t.Fatalf("CheckURL(%q) err=%v, 期望放行=%v", tc.url, err, tc.ok)
			}
		})
	}
}

func TestCheckIPBlocksInternalRanges(t *testing.T) {
	g := NewGuard(nil)
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "10.0.0.1", "172.16.5.5", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "100.127.255.254", "0.0.0.0", "0.1.2.3",
		"255.255.255.255", "224.0.0.1", "239.255.255.250", "240.0.0.1",
		"192.0.0.1", "192.0.2.1", "192.88.99.1", "198.18.0.1", "198.51.100.1", "203.0.113.1",
		"::1", "::", "fc00::1", "fd12:3456::1", "fe80::1", "ff02::1",
		"2001:db8::1", "2002::1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:192.168.0.1",
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("测试用例 IP 非法: %s", s)
		}
		if err := g.CheckIP(ip); err == nil {
			t.Errorf("%s 应被拒绝", s)
		}
	}

	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "100.63.255.255", "100.128.0.1",
		"2001:4860:4860::8888", "2606:4700:4700::1111", "199.255.255.255"}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		if err := g.CheckIP(ip); err != nil {
			t.Errorf("%s 应放行，实际: %v", s, err)
		}
	}
}

func TestAllowLoopbackOnlyRelaxesLoopback(t *testing.T) {
	g := NewGuard(nil)
	g.AllowLoopback = true
	if err := g.CheckIP(net.ParseIP("127.0.0.1")); err != nil {
		t.Fatalf("AllowLoopback 下回环应放行: %v", err)
	}
	if err := g.CheckIP(net.ParseIP("::1")); err != nil {
		t.Fatalf("AllowLoopback 下 ::1 应放行: %v", err)
	}
	// 私网、CGNAT 等仍必须拒绝。
	for _, s := range []string{"10.0.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "fc00::1"} {
		if err := g.CheckIP(net.ParseIP(s)); err == nil {
			t.Errorf("AllowLoopback 不应放行 %s", s)
		}
	}
}

// fakeResolver 用于在不依赖真实 DNS 的前提下验证解析层策略。
type fakeResolver struct {
	ips []net.IPAddr
	err error
}

func (f fakeResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.ips, nil
}

func TestCheckHostFailsClosedOnResolveError(t *testing.T) {
	g := NewGuard(fakeResolver{err: errors.New("nxdomain")})
	if _, err := g.CheckHost(context.Background(), "nope.example.com"); err == nil {
		t.Fatal("解析失败必须 fail-closed 拒绝")
	}
	g2 := NewGuard(fakeResolver{})
	if _, err := g2.CheckHost(context.Background(), "empty.example.com"); err == nil {
		t.Fatal("解析到空结果必须拒绝")
	}
}

func TestCheckHostRejectsIfAnyResolvedIPIsInternal(t *testing.T) {
	// 典型的 DNS rebinding：公网 + 内网混合解析，必须整体拒绝。
	g := NewGuard(fakeResolver{ips: []net.IPAddr{
		{IP: net.ParseIP("93.184.216.34")},
		{IP: net.ParseIP("169.254.169.254")},
	}})
	if _, err := g.CheckHost(context.Background(), "mixed.example.com"); err == nil {
		t.Fatal("解析结果含内网地址时必须整体拒绝")
	}

	g2 := NewGuard(fakeResolver{ips: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}})
	if _, err := g2.CheckHost(context.Background(), "good.example.com"); err != nil {
		t.Fatalf("纯公网解析应放行: %v", err)
	}
}

// newLocalClient 构造测试用客户端：放行回环以便驱动 httptest。
func newLocalClient(t *testing.T, opts Options) *Client {
	t.Helper()
	if opts.Guard == nil {
		g := NewGuard(nil)
		g.AllowLoopback = true
		opts.Guard = g
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "test-agent"
	}
	return New(opts)
}

func TestClientGetAndUserAgent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "test-agent" {
			t.Errorf("User-Agent = %q", got)
		}
		if got := r.Header.Get("Accept-Language"); got != "zh-CN" {
			t.Errorf("自定义头未透传: %q", got)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html><body>hello</body></html>"))
	}))
	defer ts.Close()

	c := newLocalClient(t, Options{})
	resp, err := c.Get(context.Background(), ts.URL, map[string]string{"Accept-Language": "zh-CN"})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), "hello") {
		t.Fatalf("响应体 = %q", resp.Body)
	}
	if !strings.Contains(resp.ContentType, "text/html") {
		t.Fatalf("Content-Type = %q", resp.ContentType)
	}
	if resp.FinalURL != ts.URL {
		t.Fatalf("FinalURL = %q", resp.FinalURL)
	}
}

func TestClientDoesNotAutoFollowRedirect(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hop" {
			http.Redirect(w, r, ts.URL+"/final", http.StatusFound)
			return
		}
		w.Write([]byte("final"))
	}))
	defer ts.Close()

	c := newLocalClient(t, Options{})
	// once 直接走底层客户端：必须原样返回 302，绝不被自动跟随。
	resp, err := c.once(context.Background(), http.MethodGet, ts.URL+"/hop", nil)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("状态码 = %d, 期望 302（禁止自动跟随）", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); !strings.HasSuffix(got, "/final") {
		t.Fatalf("Location = %q", got)
	}
}

func TestClientFollowsRedirectsUpToLimit(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a":
			http.Redirect(w, r, "/b", http.StatusFound) // 相对 Location
		case "/b":
			http.Redirect(w, r, ts.URL+"/c", http.StatusMovedPermanently)
		case "/c":
			w.Write([]byte("done"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	c := newLocalClient(t, Options{})
	resp, err := c.Get(context.Background(), ts.URL+"/a", nil)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(resp.Body) != "done" {
		t.Fatalf("未正确落地最终页: %d %q", resp.StatusCode, resp.Body)
	}
	if resp.FinalURL != ts.URL+"/c" {
		t.Fatalf("FinalURL = %q", resp.FinalURL)
	}
}

func TestClientAbortsWhenRedirectLimitExceeded(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, ts.URL+"/loop", http.StatusFound)
	}))
	defer ts.Close()

	c := newLocalClient(t, Options{MaxRedirects: 3})
	_, err := c.Get(context.Background(), ts.URL+"/loop", nil)
	if err == nil {
		t.Fatal("无限重定向必须中止")
	}
	if !strings.Contains(err.Error(), "重定向超过") {
		t.Fatalf("错误信息不符: %v", err)
	}
}

func TestClientRejectsRedirectToInternalAddress(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer ts.Close()

	c := newLocalClient(t, Options{})
	_, err := c.Get(context.Background(), ts.URL+"/", nil)
	if err == nil {
		t.Fatal("跳到元数据地址必须被拒绝")
	}
	// 拒绝可能来自「重定向目标语法校验」或「下一跳 IP 预校验」，二者都算合格。
	if !strings.Contains(err.Error(), "SSRF 防护") && !strings.Contains(err.Error(), "重定向目标被拒绝") {
		t.Fatalf("错误信息不符: %v", err)
	}
}

func TestClientRejectsRedirectToNonHTTP(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	}))
	defer ts.Close()

	c := newLocalClient(t, Options{})
	_, err := c.Get(context.Background(), ts.URL+"/", nil)
	if err == nil {
		t.Fatal("协议降级/切换必须被拒绝")
	}
}

func TestClientTruncatesOversizedBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 5000)))
	}))
	defer ts.Close()

	c := newLocalClient(t, Options{MaxBodyBytes: 100})
	resp, err := c.Get(context.Background(), ts.URL, nil)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if !resp.Truncated {
		t.Fatal("应标记 Truncated")
	}
	if len(resp.Body) != 100 {
		t.Fatalf("响应体长度 = %d, 期望 100", len(resp.Body))
	}
}

func TestClientHonorsContextDeadline(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.Write([]byte("late"))
	}))
	defer ts.Close()

	c := newLocalClient(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := c.Get(ctx, ts.URL, nil); err == nil {
		t.Fatal("超时未被中止")
	}
}

func TestClientRefusesInternalTargetDirectly(t *testing.T) {
	// 严格守卫（不允许回环）下，直连 127.0.0.1 必须被拒。
	c := New(Options{Guard: NewGuard(nil), MaxRedirects: 1})
	if _, err := c.Get(context.Background(), "http://127.0.0.1:9/", nil); err == nil {
		t.Fatal("严格策略下回环地址必须被拒绝")
	}
	if _, err := c.Get(context.Background(), "http://169.254.169.254/", nil); err == nil {
		t.Fatal("严格策略下链路本地地址必须被拒绝")
	}
}

func TestMustCIDRsAreValid(t *testing.T) {
	if len(blockedV4) == 0 || len(blockedV6) == 0 {
		t.Fatal("内置封锁网段不能为空")
	}
}
