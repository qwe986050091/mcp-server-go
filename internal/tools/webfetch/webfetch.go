// Package webfetch 实现 MCP 工具 web_fetch：抓取网页并抽取可读正文。
//
// 所有网络访问都经过 safehttp（语法层校验、IP 层 SSRF 拦截、逐跳重校验的重定向），
// 正文抽取由 htmlx 完成；本包只负责编排与结果格式化。
package webfetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"mcp-server-go/internal/htmlx"
	"mcp-server-go/internal/safehttp"
	"mcp-server-go/internal/tools"
)

// 输出限额。
const (
	maxTitleChars = 300
	maxBodyChars  = 8000
)

// Options 是构造参数。
type Options struct {
	// Client 为 nil 时使用 safehttp 默认配置。
	Client *safehttp.Client
}

// Tool 是 web_fetch 工具。
type Tool struct {
	client *safehttp.Client
}

// New 构造 web_fetch 工具。
func New(opts Options) *Tool {
	client := opts.Client
	if client == nil {
		client = safehttp.New(safehttp.Options{})
	}
	return &Tool{client: client}
}

// Name 返回工具名。
func (*Tool) Name() string { return "web_fetch" }

// Description 返回给模型看的说明。
func (*Tool) Description() string {
	return "抓取指定 URL 的网页并返回其标题与正文纯文本（已剥离脚本/样式/内嵌框架）。" +
		"仅支持 http/https，最多跟随 3 次重定向，正文最多返回 " +
		"8000 个字符；内网、回环、链路本地与云元数据地址一律拒绝。仅支持 UTF-8 编码页面。"
}

// InputSchema 返回入参 schema。
func (*Tool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "要抓取的完整 URL，必须以 http:// 或 https:// 开头",
			},
		},
		"required": []any{"url"},
	}
}

// Execute 抓取并抽取正文。
func (t *Tool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		URL string `json:"url"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("参数解析失败: %w", err)
		}
	}
	rawURL := strings.TrimSpace(in.URL)
	if rawURL == "" {
		return "", errors.New("url 不能为空")
	}

	resp, err := t.client.Get(ctx, rawURL, map[string]string{
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,text/plain;q=0.8,*/*;q=0.5",
		"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
	})
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("目标返回 HTTP %d，无法抽取正文: %s", resp.StatusCode, resp.FinalURL)
	}

	text, title := extract(resp)
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("页面 %s 未抽取到任何正文（可能是纯脚本页或需要登录）", resp.FinalURL)
	}

	var b strings.Builder
	if title != "" {
		fmt.Fprintf(&b, "标题：%s\n", title)
	}
	fmt.Fprintf(&b, "URL：%s\n", resp.FinalURL)
	fmt.Fprintf(&b, "HTTP 状态：%d\n", resp.StatusCode)
	if resp.Truncated {
		fmt.Fprintf(&b, "提示：响应体超过 %d 字节上限，已截断\n", len(resp.Body))
	}
	b.WriteString("正文：\n")
	b.WriteString(truncateRunes(text, maxBodyChars))
	return b.String(), nil
}

// extract 根据 Content-Type 选择抽取策略，返回正文与标题。
func extract(resp *safehttp.Response) (text, title string) {
	raw := strings.ToValidUTF8(string(resp.Body), "")
	contentType := strings.ToLower(resp.ContentType)
	isHTML := strings.Contains(contentType, "html") || strings.Contains(contentType, "xml")
	if !isHTML && resp.ContentType == "" {
		// 未声明类型时，按是否有标签特征猜一把。
		isHTML = strings.Contains(strings.ToLower(raw), "<html") || strings.Contains(strings.ToLower(raw), "<body")
	}
	if !isHTML {
		return normalizePlain(raw), ""
	}

	doc := htmlx.Parse(raw)
	if t := doc.First("title"); t != nil {
		title = truncateRunes(t.CompactText(), maxTitleChars)
	}
	body := doc.First("body")
	if body == nil {
		body = doc
	}
	return body.Text(), title
}

// normalizePlain 归一化纯文本响应：统一换行、去掉行尾空白、丢弃空行。
func normalizePlain(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.TrimRight(line, " \t"); line != "" {
			out = append(out, line)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// truncateRunes 按字符（而非字节）截断，避免截断多字节字符产生乱码。
func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit]) + fmt.Sprintf("\n\n[内容已截断，仅显示前 %d 个字符]", limit)
}

// 编译期断言：确保实现满足 tools.Tool 接口。
var _ tools.Tool = (*Tool)(nil)
