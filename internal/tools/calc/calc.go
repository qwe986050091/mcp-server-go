// Package calc 实现 MCP 工具 calc：对数学表达式求值。
//
// 设计要点：词法与递归下降求值全部自研，**不使用任何 eval/表达式引擎**，
// 且语法上不接受标识符变量 —— 输入永远只能是一个可判定的算术表达式。
package calc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"mcp-server-go/internal/tools"
)

// Tool 是 calc 工具。
type Tool struct{}

// New 构造 calc 工具。
func New() *Tool { return &Tool{} }

// Name 返回工具名。
func (Tool) Name() string { return "calc" }

// Description 返回给模型看的说明。
func (Tool) Description() string {
	return "对数学表达式求值并返回精确结果。支持 + - * / %（取余）、^（幂，右结合）、括号、一元正负号；" +
		"内置常量 pi 与 e；内置函数 abs/sqrt/pow(a,b)/min/max/round/floor/ceil/log（自然对数）/exp。" +
		"只接受纯算术表达式，不支持变量、赋值或任意标识符。"
}

// InputSchema 返回入参 schema；校验由 tools.ValidateInputSchema 保证。
func (Tool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"expression": map[string]any{
				"type":        "string",
				"description": "要求值的算术表达式，例如 (1+2)*3/4 或 sqrt(2)*pi",
			},
		},
		"required": []any{"expression"},
	}
}

// Execute 求值并返回结果文本。
func (Tool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Expression string `json:"expression"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("参数解析失败: %w", err)
		}
	}
	expr := strings.TrimSpace(in.Expression)
	if expr == "" {
		return "", errors.New("expression 不能为空")
	}
	v, err := Eval(expr)
	if err != nil {
		return "", fmt.Errorf("表达式 %q 求值失败: %w", expr, err)
	}
	return FormatNumber(v), nil
}

// 编译期断言：确保实现满足 tools.Tool 接口。
var _ tools.Tool = Tool{}
