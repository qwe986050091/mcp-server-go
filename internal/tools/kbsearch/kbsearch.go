// Package kbsearch 实现 MCP 工具 kb_search：把检索请求转给自有 Java Agent 的知识库接口，
// 复用其已有的混合检索（向量 + 关键词）＋ RRF ＋ 重排链路。
//
// 契约（由 Java 侧 S5 落地）：
//
//	POST {KB_BASE_URL}/api/agent/rag/search
//	头：Content-Type: application/json、X-Agent-Token: <AGENT_INTERNAL_TOKEN>
//	体：{"userId":"<用户标识>","query":"...","topK":5}
//	响应：{"items":[{"text":"...","source":"...","docId":"...","score":0.83, ...}]}
//
// 降级策略：接口不可用（超时 / 5xx / 连不上 / 返回体不可解析）时**不报错**，
// 而是回一段说明文本并提示改用 web_search —— 知识库是锦上添花，绝不能因此中断整轮对话。
package kbsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"mcp-server-go/internal/tools"
)

// 默认值与限额。
const (
	DefaultBaseURL     = "http://127.0.0.1:8080"
	DefaultTokenHeader = "X-Agent-Token"
	// DefaultTimeout 是知识库检索的墙钟上限。
	// 生产实测（2026-10-17，4c4g 新机 + 已启用 reranker）：稳态 6.1~6.7s，冷启动
	// （embedding/reranker 模型首次加载）会超过 8s。取 15s 留出冷启动余量，
	// 同时远小于 MCP 客户端 60s 的工具执行超时。
	DefaultTimeout   = 15 * time.Second
	DefaultTopK      = 5
	MaxTopK          = 10
	SearchPath       = "/api/agent/rag/search"
	maxResponseBytes = 1 << 20
	maxItemChars     = 1200
)

// Options 是构造参数。
type Options struct {
	// BaseURL 是 agent 服务地址，空则用 DefaultBaseURL。
	BaseURL string
	// Token 是内部鉴权口令，通常由环境变量 AGENT_INTERNAL_TOKEN 注入。
	Token string
	// DefaultUserID 是参数未提供 user_id 时使用的默认用户标识（通常由环境变量 KB_USER_ID 注入）。
	DefaultUserID string
	// HTTPClient 可注入，便于单测；为空时使用带超时的默认客户端。
	HTTPClient *http.Client
	// Timeout 为空则用 DefaultTimeout。
	Timeout time.Duration
}

// Tool 是 kb_search 工具。
type Tool struct {
	baseURL       string
	token         string
	defaultUserID string
	client        *http.Client
	timeout       time.Duration
}

// New 构造 kb_search 工具。
func New(opts Options) *Tool {
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &Tool{
		baseURL:       baseURL,
		token:         strings.TrimSpace(opts.Token),
		defaultUserID: strings.TrimSpace(opts.DefaultUserID),
		client:        client,
		timeout:       timeout,
	}
}

// Name 返回工具名。
func (*Tool) Name() string { return "kb_search" }

// Description 返回给模型看的说明。
func (*Tool) Description() string {
	return "检索本项目的私有知识库（已入库的文档与历史对话），返回与问题最相关的原文片段及来源。" +
		"适合回答「我们内部文档里怎么写的」这类问题。若知识库不可用或没有命中，可改用 web_search 联网检索。"
}

// InputSchema 返回入参 schema。
//
// 刻意**不**暴露 user_id：用户标识由服务端注入，避免模型伪造越权检索他人数据。
func (*Tool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "检索问题或关键词",
			},
			"top_k": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("期望返回的片段数，默认 %d，最大 %d", DefaultTopK, MaxTopK),
			},
		},
		"required": []any{"query"},
	}
}

// searchRequest 是发给 Java 侧的请求体。
type searchRequest struct {
	UserID string `json:"userId"`
	Query  string `json:"query"`
	TopK   int    `json:"topK"`
}

// searchItem 是 Java 侧返回的单条命中。
type searchItem struct {
	Text   string   `json:"text"`
	Source string   `json:"source"`
	DocID  string   `json:"docId"`
	Score  *float64 `json:"score"`
	// 以下字段目前仅用于排错展示，缺失时忽略。
	ChunkIndex  *int     `json:"chunkIndex"`
	Channel     string   `json:"channel"`
	RerankScore *float64 `json:"rerankScore"`
}

type searchResponse struct {
	Items []searchItem `json:"items"`
}

// Execute 调用知识库检索接口。
func (t *Tool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Query string `json:"query"`
		TopK  *int   `json:"top_k"`
		// user_id 是隐藏参数：不在 schema 中公开，由上层按当前会话用户注入。
		UserID string `json:"user_id"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("参数解析失败: %w", err)
		}
	}
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return "", errors.New("query 不能为空")
	}
	topK := DefaultTopK
	if in.TopK != nil && *in.TopK > 0 {
		topK = *in.TopK
	}
	if topK > MaxTopK {
		topK = MaxTopK
	}
	userID := strings.TrimSpace(in.UserID)
	if userID == "" {
		userID = t.defaultUserID
	}

	if t.token == "" {
		return t.degrade("未配置内部鉴权口令（AGENT_INTERNAL_TOKEN），无法访问知识库检索接口"), nil
	}

	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	payload, err := json.Marshal(searchRequest{UserID: userID, Query: query, TopK: topK})
	if err != nil {
		return "", fmt.Errorf("构造请求体失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+SearchPath, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(DefaultTokenHeader, t.token)

	resp, err := t.client.Do(req)
	if err != nil {
		return t.degrade(fmt.Sprintf("知识库检索请求失败（%v）", err)), nil
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return t.degrade(fmt.Sprintf("读取知识库响应失败（%v）", err)), nil
	}
	if resp.StatusCode != http.StatusOK {
		return t.degrade(fmt.Sprintf("知识库检索接口返回 HTTP %d：%s", resp.StatusCode, snippet(string(raw)))), nil
	}

	var parsed searchResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return t.degrade("知识库检索接口返回体无法解析：" + snippet(string(raw))), nil
	}
	if len(parsed.Items) == 0 {
		return fmt.Sprintf("知识库中未检索到与 %q 相关的内容。可改用 web_search 联网检索。", query), nil
	}
	return formatHits(query, topK, parsed.Items), nil
}

// degrade 生成降级说明：明确告知知识库不可用，并给出替代路径。
func (t *Tool) degrade(reason string) string {
	return fmt.Sprintf("%s。本次知识库检索已跳过（不影响对话继续），可改用 web_search 联网检索，或稍后重试。", reason)
}

// formatHits 组装命中片段。
func formatHits(query string, topK int, items []searchItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "知识库检索：%q（topK=%d，命中 %d 条）\n\n", query, topK, len(items))
	for i, item := range items {
		source := item.Source
		if strings.TrimSpace(source) == "" {
			source = "未知来源"
		}
		if item.DocID != "" {
			source += "（" + item.DocID + "）"
		}
		fmt.Fprintf(&b, "%d. 来源：%s", i+1, source)
		if item.Score != nil {
			fmt.Fprintf(&b, "｜相关度：%.3f", *item.Score)
		}
		if item.Channel != "" {
			fmt.Fprintf(&b, "｜通道：%s", item.Channel)
		}
		b.WriteString("\n   ")
		b.WriteString(truncateChars(strings.TrimSpace(item.Text), maxItemChars))
		b.WriteString("\n")
	}
	return b.String()
}

// snippet 截取短文本用于错误信息，避免把整页响应灌进日志与结果。
func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return truncateChars(s, 200)
}

// truncateChars 按字符截断，避免截断多字节字符。
func truncateChars(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

// 编译期断言：确保实现满足 tools.Tool 接口。
var _ tools.Tool = (*Tool)(nil)
