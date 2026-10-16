package calc

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"mcp-server-go/internal/tools"
)

func TestEvalArithmetic(t *testing.T) {
	cases := map[string]string{
		"1+2*3":              "7",
		"(1+2)*3":            "9",
		"10/4":               "2.5",
		"7%3":                "1",
		"-5":                 "-5",
		"+5":                 "5",
		"--5":                "5",
		"-(-5)":              "5",
		"2^3^2":              "512", // 右结合
		"(2^3)^2":            "64",
		"-2^2":               "-4", // 一元负号优先级低于 ^：-(2^2)
		"2*-3":               "-6",
		"1.5*2":              "3",
		".5+.5":              "1",
		"1e3":                "1000",
		"1e-3":               "0.001",
		"2E2":                "200",
		"pi":                 "3.141592653589793",
		"e":                  "2.718281828459045",
		"2*pi":               "6.283185307179586",
		"abs(-3)":            "3",
		"sqrt(16)":           "4",
		"sqrt(2)*sqrt(2)":    "2.0000000000000004",
		"pow(2,10)":          "1024",
		"min(3,1,2)":         "1",
		"max(3,1,2)":         "3",
		"round(2.5)":         "3",
		"floor(-1.5)":        "-2",
		"ceil(1.2)":          "2",
		"log(1)":             "0",
		"exp(0)":             "1",
		"min(1+2, 4-1)":      "3",
		"pow(pow(2,3),2)":    "64",
		"1+2*3-4/2":          "5",
		"  ( 1 + 2 ) * 3  ":  "9",
		"0":                  "0",
		"-0":                 "0",
		"0.0":                "0",
		"1000000000000*1000": "1e+15",
	}
	for expr, want := range cases {
		v, err := Eval(expr)
		if err != nil {
			t.Errorf("Eval(%q) 报错: %v", expr, err)
			continue
		}
		if got := FormatNumber(v); got != want {
			t.Errorf("Eval(%q) = %s, 期望 %s", expr, got, want)
		}
	}
}

func TestEvalRejectsUnsafeOrMalformedInput(t *testing.T) {
	cases := map[string]string{
		"":             "意外结束",
		"1/0":          "除数为零",
		"1%0":          "取余的除数为零",
		"1+":           "意外结束",
		"1+*2":         "意料之外",
		"(1+2":         "缺少右括号",
		"1+2)":         "多余输入",
		"1 2":          "多余输入",
		"foo":          "未知标识符",
		"x+1":          "未知标识符",
		"sin(1)":       "未知函数",
		"abs(1,2)":     "需要 1 个参数",
		"pow(2)":       "需要 2 个参数",
		"sqrt(-1)":     "不能为负数",
		"log(0)":       "必须为正数",
		"log(-1)":      "必须为正数",
		"min()":        "至少需要 1 个参数",
		"1&2":          "非法字符",
		"1;2":          "非法字符",
		"system('ls')": "非法字符",
		"__import__":   "非法字符",
		"1//2":         "意料之外",
		"$1":           "非法字符",
		"1e":           "多余输入",
	}
	for expr, hint := range cases {
		_, err := Eval(expr)
		if err == nil {
			t.Errorf("Eval(%q) 应报错", expr)
			continue
		}
		if !strings.Contains(err.Error(), hint) {
			t.Errorf("Eval(%q) 错误信息 %q 未包含 %q", expr, err.Error(), hint)
		}
	}
}

func TestEvalOverflowAndNonFinite(t *testing.T) {
	cases := map[string]string{
		"1e308*10":     "溢出",
		"1e308^2":      "溢出",
		"0/0":          "除数为零",
		"1e400":        "数字",
		"pow(1e308,2)": "溢出",
	}
	for expr, hint := range cases {
		_, err := Eval(expr)
		if err == nil {
			t.Errorf("Eval(%q) 应报错", expr)
			continue
		}
		if !strings.Contains(err.Error(), hint) {
			t.Errorf("Eval(%q) 错误信息 %q 未包含 %q", expr, err.Error(), hint)
		}
	}
}

func TestEvalRejectsOverlongExpression(t *testing.T) {
	long := strings.Repeat("1+", MaxExpressionLength) + "1"
	if _, err := Eval(long); err == nil || !strings.Contains(err.Error(), "过长") {
		t.Fatalf("超长表达式应被拒绝, err=%v", err)
	}
}

func TestDeepNestingDoesNotPanic(t *testing.T) {
	depth := 500
	expr := strings.Repeat("(", depth) + "1" + strings.Repeat(")", depth)
	v, err := Eval(expr)
	if err != nil {
		t.Fatalf("深括号应可求值: %v", err)
	}
	if v != 1 {
		t.Fatalf("结果 = %v", v)
	}
}

func TestToolSchemaIsValid(t *testing.T) {
	tool := New()
	if err := tools.ValidateInputSchema(tool.InputSchema()); err != nil {
		t.Fatalf("schema 不合规: %v", err)
	}
	if tool.Name() != "calc" {
		t.Fatalf("工具名 = %q", tool.Name())
	}
}

func TestToolExecute(t *testing.T) {
	tool := New()
	cases := []struct {
		args string
		want string
	}{
		{`{"expression":"1+2*3"}`, "7"},
		{`{"expression":" sqrt(81) "}`, "9"},
		{`{}`, ""},
	}
	for _, tc := range cases {
		got, err := tool.Execute(context.Background(), json.RawMessage(tc.args))
		if tc.want == "" {
			if err == nil {
				t.Errorf("args=%s 应报错", tc.args)
			}
			continue
		}
		if err != nil {
			t.Errorf("args=%s 报错: %v", tc.args, err)
			continue
		}
		if got != tc.want {
			t.Errorf("args=%s 结果 = %q, 期望 %q", tc.args, got, tc.want)
		}
	}

	if _, err := tool.Execute(context.Background(), json.RawMessage(`not json`)); err == nil {
		t.Error("坏 JSON 应报错")
	}
	// 上下文取消时求值应立即返回（本工具是纯计算，至少不应 panic）。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Execute(ctx, json.RawMessage(`{"expression":"1+1"}`)); err != nil {
		t.Logf("取消上下文下的求值结果: %v", err)
	}
}

func TestFormatNumberShortestForm(t *testing.T) {
	cases := map[float64]string{
		0:                    "0",
		math.Copysign(0, -1): "0",
		1:                    "1",
		-1:                   "-1",
		0.1:                  "0.1",
		1.0 / 3.0:            "0.3333333333333333",
		1e20:                 "1e+20",
		-1e20:                "-1e+20",
	}
	for v, want := range cases {
		if got := FormatNumber(v); got != want {
			t.Errorf("FormatNumber(%v) = %q, 期望 %q", v, got, want)
		}
	}
}
