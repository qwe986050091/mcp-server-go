package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fakeTool 是测试用最小工具。
type fakeTool struct{}

func (fakeTool) Name() string        { return "fake" }
func (fakeTool) Description() string { return "测试工具" }
func (fakeTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "回显文本"},
			"tags": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
		},
		"required": []any{"text"},
	}
}
func (fakeTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "ok", nil
}

func TestValidateInputSchemaAcceptsWhitelistedShape(t *testing.T) {
	if err := ValidateInputSchema(fakeTool{}.InputSchema()); err != nil {
		t.Fatalf("合法 schema 应通过: %v", err)
	}
}

func TestValidateInputSchemaRejectsIllegalShapes(t *testing.T) {
	cases := []struct {
		name   string
		schema map[string]any
		hint   string
	}{
		{
			name:   "顶层不是 object",
			schema: map[string]any{"type": "string", "properties": map[string]any{}, "required": []any{}},
			hint:   "object",
		},
		{
			name:   "缺少 properties",
			schema: map[string]any{"type": "object", "required": []any{}},
			hint:   "properties",
		},
		{
			name:   "缺少 required",
			schema: map[string]any{"type": "object", "properties": map[string]any{}},
			hint:   "required",
		},
		{
			name: "未知关键字 additionalProperties",
			schema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties":           map[string]any{},
				"required":             []any{},
			},
			hint: "additionalProperties",
		},
		{
			name: "属性类型不在白名单 format/date",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"when": map[string]any{"type": "string", "format": "date"}},
				"required":   []any{"when"},
			},
			hint: "format",
		},
		{
			name: "不支持的类型名",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"x": map[string]any{"type": "null"}},
				"required":   []any{},
			},
			hint: "null",
		},
		{
			name: "array 缺少 items",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"tags": map[string]any{"type": "array"}},
				"required":   []any{},
			},
			hint: "items",
		},
		{
			name: "属性缺少 type",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"x": map[string]any{"description": "没有类型"}},
				"required":   []any{},
			},
			hint: "type",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateInputSchema(tc.schema)
			if err == nil {
				t.Fatalf("非法 schema 应被拒绝")
			}
			if !strings.Contains(err.Error(), tc.hint) {
				t.Fatalf("错误信息应提及 %q，实际: %v", tc.hint, err)
			}
		})
	}
}

func TestRegistryRegisterValidatesNameAndSchema(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(fakeTool{}); err != nil {
		t.Fatalf("注册应成功: %v", err)
	}
	if reg.Len() != 1 {
		t.Fatalf("注册数量 = %d, 期望 1", reg.Len())
	}
	if err := reg.Register(fakeTool{}); err == nil {
		t.Fatal("重名注册应失败")
	}

	bad := NewRegistry()
	if err := bad.Register(badSchemaTool{}); err == nil {
		t.Fatal("非法 schema 的注册应失败")
	}
	if bad.Len() != 0 {
		t.Fatal("校验失败的注册不应污染注册表")
	}
}

type badSchemaTool struct{}

func (badSchemaTool) Name() string                { return "bad" }
func (badSchemaTool) Description() string         { return "非法 schema" }
func (badSchemaTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (badSchemaTool) Execute(context.Context, json.RawMessage) (string, error) {
	return "", nil
}

func TestRegistryKeepsRegistrationOrder(t *testing.T) {
	reg := NewRegistry()
	for _, t2 := range []Tool{namedTool{"b"}, namedTool{"a"}, namedTool{"c"}} {
		if err := reg.Register(t2); err != nil {
			t.Fatalf("注册失败: %v", err)
		}
	}
	got := []string{}
	for _, t2 := range reg.List() {
		got = append(got, t2.Name())
	}
	want := []string{"b", "a", "c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("顺序 = %v, 期望 %v", got, want)
	}
}

type namedTool struct{ name string }

func (n namedTool) Name() string                                           { return n.name }
func (namedTool) Description() string                                      { return "命名工具" }
func (namedTool) InputSchema() map[string]any                              { return emptySchema() }
func (namedTool) Execute(context.Context, json.RawMessage) (string, error) { return "", nil }

// emptySchema 返回无参数工具的合法 schema。
func emptySchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
		"required":   []any{},
	}
}
