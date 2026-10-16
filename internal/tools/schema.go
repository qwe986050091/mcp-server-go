package tools

import (
	"errors"
	"fmt"
)

// allowedKeywords 是 inputSchema 允许出现的关键字全集。
//
// 白名单刻意收紧：LangChain4j 的 ToolSpecificationHelper 遇到未知类型/关键字会抛错，
// 进而使 listTools() 失败、MCP 连接被判定为失败。宁可启动期报错，也不要运行期连不上。
var allowedKeywords = map[string]bool{
	"type":        true,
	"description": true,
	"enum":        true,
	"items":       true,
	"properties":  true,
	"required":    true,
}

// allowedTypes 是允许的 JSON Schema 类型名。
var allowedTypes = map[string]bool{
	"object":  true,
	"string":  true,
	"integer": true,
	"number":  true,
	"boolean": true,
	"array":   true,
}

// ValidateInputSchema 校验工具入参 schema 是否满足 MCP 客户端兼容要求。
//
// 约定形状：{"type":"object","properties":{...},"required":[...]}
// 无参数的工具也必须给出空 properties 与空 required。
func ValidateInputSchema(schema map[string]any) error {
	if len(schema) == 0 {
		return errors.New("inputSchema 为空")
	}
	if t, _ := schema["type"].(string); t != "object" {
		return fmt.Errorf("顶层 type 必须为 \"object\"（当前 %v）", schema["type"])
	}
	if _, ok := schema["properties"]; !ok {
		return errors.New("缺少 properties 字段（无参数工具也必须给空对象）")
	}
	if _, ok := schema["required"]; !ok {
		return errors.New("缺少 required 字段（无必填参数也必须给空数组）")
	}
	return validateSchemaNode(schema, "inputSchema")
}

// validateSchemaNode 递归校验单个 schema 节点。
func validateSchemaNode(node map[string]any, path string) error {
	if _, ok := node["type"]; !ok {
		return fmt.Errorf("%s: 缺少 type", path)
	}
	for key, value := range node {
		if !allowedKeywords[key] {
			return fmt.Errorf("%s: 不允许的关键字 %q（仅支持 type/description/enum/items/properties/required）", path, key)
		}
		switch key {
		case "type":
			name, ok := value.(string)
			if !ok {
				return fmt.Errorf("%s.type 必须是字符串", path)
			}
			if !allowedTypes[name] {
				return fmt.Errorf("%s.type 不支持: %q", path, name)
			}
		case "description":
			if _, ok := value.(string); !ok {
				return fmt.Errorf("%s.description 必须是字符串", path)
			}
		case "enum":
			list, ok := value.([]any)
			if !ok || len(list) == 0 {
				return fmt.Errorf("%s.enum 必须是非空数组", path)
			}
		case "required":
			list, ok := value.([]any)
			if !ok {
				return fmt.Errorf("%s.required 必须是数组", path)
			}
			for i, item := range list {
				if _, ok := item.(string); !ok {
					return fmt.Errorf("%s.required[%d] 必须是字符串", path, i)
				}
			}
		case "properties":
			props, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("%s.properties 必须是对象", path)
			}
			for name, raw := range props {
				child, ok := raw.(map[string]any)
				if !ok {
					return fmt.Errorf("%s.properties.%s 必须是对象", path, name)
				}
				if err := validateSchemaNode(child, path+".properties."+name); err != nil {
					return err
				}
			}
		case "items":
			child, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("%s.items 必须是对象", path)
			}
			if err := validateSchemaNode(child, path+".items"); err != nil {
				return err
			}
		}
	}
	if t, _ := node["type"].(string); t == "array" {
		if _, ok := node["items"]; !ok {
			return fmt.Errorf("%s: 数组必须声明 items", path)
		}
	}
	return nil
}
