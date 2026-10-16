// Package tools 定义工具抽象与注册表。
//
// 装配期一次性注册，之后只读；输出顺序与注册顺序一致，保证 tools/list 稳定。
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Tool 是 MCP 工具的抽象。实现者只需关心参数与业务结果，
// content 包装、isError 标记与 panic 兜底统一由 server 层完成。
type Tool interface {
	// Name 是工具在 tools/call 中的调用名（全局唯一，建议 snake_case）。
	Name() string
	// Description 是给模型看的自然语言说明。
	Description() string
	// InputSchema 是 JSON Schema 片段，必须通过 ValidateInputSchema 校验。
	InputSchema() map[string]any
	// Execute 执行工具；返回可读文本结果，err 非 nil 表示业务失败。
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry 是工具的注册表。
type Registry struct {
	order  []string
	byName map[string]Tool
}

// NewRegistry 构造空注册表。
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Tool)}
}

// Register 注册工具：重名或 inputSchema 非法都会返回错误（装配期 fail-fast）。
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return errors.New("工具不能为 nil")
	}
	name := t.Name()
	if name == "" {
		return errors.New("工具名不能为空")
	}
	if _, dup := r.byName[name]; dup {
		return fmt.Errorf("工具重名: %s", name)
	}
	if err := ValidateInputSchema(t.InputSchema()); err != nil {
		return fmt.Errorf("工具 %s 的 inputSchema 非法: %w", name, err)
	}
	r.order = append(r.order, name)
	r.byName[name] = t
	return nil
}

// Get 按名取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// Len 返回已注册工具数量。
func (r *Registry) Len() int {
	return len(r.order)
}

// List 按注册顺序返回全部工具。
func (r *Registry) List() []Tool {
	out := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.byName[name])
	}
	return out
}
