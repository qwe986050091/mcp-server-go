// Package safehttp 提供带 SSRF 防护的 HTTP 客户端。
//
// 三层防护（对应 web_fetch 的安全要求）：
//  1. 语法层：只允许 http/https，禁止 userinfo，必须带 host；
//  2. 解析层：解析出全部 A/AAAA 记录并逐一拒绝回环/私网/链路本地/多播/CGNAT/保留段，
//     **解析失败一律 fail-closed**；该判定在 DialContext 里再执行一次，从而封死
//     「先解析到公网 IP、连接时又被重绑定到内网」的 TOCTOU 窗口；
//  3. 跳转层：客户端禁止自动跟随重定向，调用方逐跳重校验（见 Do）。
package safehttp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Resolver 抽象 DNS 解析，便于测试注入。
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// blockedV4 是显式封锁的非全局单播 IPv4 段。
var blockedV4 = mustCIDRs(
	"0.0.0.0/8",       // 本网络（含 0.1.2.3 这类）
	"100.64.0.0/10",   // CGNAT（RFC 6598）
	"192.0.0.0/24",    // IETF 协议专用
	"192.0.2.0/24",    // TEST-NET-1
	"192.88.99.0/24",  // 6to4 中继（已废弃）
	"198.18.0.0/15",   // 基准测试
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"224.0.0.0/4",     // 多播（含 224/8、239/8）
	"240.0.0.0/4",     // 保留（含 255.255.255.255）
	"255.255.255.255/32",
)

// blockedV6 是显式封锁的 IPv6 段。
var blockedV6 = mustCIDRs(
	"2001:db8::/32", // 文档用
	"2002::/16",     // 6to4，内嵌 IPv4
)

func mustCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("safehttp: 内置 CIDR 非法: " + c)
		}
		out = append(out, n)
	}
	return out
}

// Guard 是 SSRF 策略判定器。
type Guard struct {
	Resolver Resolver

	// AllowLoopback 仅用于测试：放行回环地址（127/8、::1），
	// 以便在 httptest 本地服务器上验证抓取与跳转逻辑。其余私网/链路本地/CGNAT 等仍拒绝。
	// 生产环境必须保持 false。
	AllowLoopback bool
}

// NewGuard 构造守卫；resolver 为 nil 时使用系统解析器。
func NewGuard(resolver Resolver) *Guard {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return &Guard{Resolver: resolver}
}

// CheckURL 执行语法层校验并返回解析后的 URL。
func (g *Guard) CheckURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("URL 为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("URL 解析失败: %w", err)
	}
	if err := g.CheckParsedURL(u); err != nil {
		return nil, err
	}
	return u, nil
}

// CheckParsedURL 对已解析的 URL 执行语法层校验（重定向逐跳复检复用此函数）。
func (g *Guard) CheckParsedURL(u *url.URL) error {
	if u == nil {
		return errors.New("URL 为空")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	case "":
		return errors.New("URL 缺少协议头（必须以 http:// 或 https:// 开头）")
	default:
		return fmt.Errorf("只允许 http/https 协议，已拒绝 %q", u.Scheme)
	}
	if u.User != nil {
		return errors.New("URL 不允许携带 userinfo（user:pass@host）")
	}
	if u.Hostname() == "" {
		return errors.New("URL 缺少主机名")
	}
	return nil
}

// CheckHost 解析主机名的全部 A/AAAA 并逐一校验；任一失败即整体拒绝。
func (g *Guard) CheckHost(ctx context.Context, host string) ([]net.IP, error) {
	ips, err := g.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if err := g.CheckIP(ip); err != nil {
			return nil, err
		}
	}
	return ips, nil
}

// resolve 解析主机名；主机名本身是字面 IP 时直接使用，不发 DNS 查询。
func (g *Guard) resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	addrs, err := g.Resolver.LookupIPAddr(ctx, host)
	if err != nil {
		// fail-closed：解析不了就不放行，而不是当成公网地址继续。
		return nil, fmt.Errorf("域名 %q 解析失败，按 fail-closed 拒绝: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("域名 %q 未解析到任何 IP，按 fail-closed 拒绝", host)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// CheckIP 校验单个 IP 是否可访问。
func (g *Guard) CheckIP(ip net.IP) error {
	if ip == nil {
		return errors.New("IP 为空")
	}
	// IPv4-mapped（::ffff:a.b.c.d）按 IPv4 规则判定。
	if v4 := ip.To4(); v4 != nil {
		if g.AllowLoopback && v4[0] == 127 {
			return nil
		}
		return checkIPv4(v4)
	}
	if g.AllowLoopback && ip.IsLoopback() {
		return nil
	}
	return checkIPv6(ip)
}

func checkIPv4(ip net.IP) error {
	switch {
	case ip.IsUnspecified():
		return reject(ip, "未指定地址")
	case ip.IsLoopback():
		return reject(ip, "回环地址")
	case ip.IsPrivate():
		return reject(ip, "私有网段（RFC 1918）")
	case ip.IsLinkLocalUnicast():
		return reject(ip, "链路本地地址 169.254/16")
	case ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return reject(ip, "多播地址")
	}
	for _, n := range blockedV4 {
		if n.Contains(ip) {
			return reject(ip, "保留/非单播网段 "+n.String())
		}
	}
	return nil
}

func checkIPv6(ip net.IP) error {
	switch {
	case ip.IsUnspecified():
		return reject(ip, "未指定地址 ::")
	case ip.IsLoopback():
		return reject(ip, "回环地址 ::1")
	case ip.IsPrivate():
		return reject(ip, "唯一本地地址 fc00::/7")
	case ip.IsLinkLocalUnicast():
		return reject(ip, "链路本地地址 fe80::/10")
	case ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return reject(ip, "多播地址")
	}
	for _, n := range blockedV6 {
		if n.Contains(ip) {
			return reject(ip, "保留网段 "+n.String())
		}
	}
	return nil
}

func reject(ip net.IP, reason string) error {
	return fmt.Errorf("SSRF 防护拒绝访问 %s：%s", ip, reason)
}
