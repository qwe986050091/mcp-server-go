package coderun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"mcp-server-go/internal/tools"
)

// ---------- fake Runner / CommandRunner ----------

type fakeRunner struct {
	availableErr error
	availableHit int
	runHit       int
	lastSpec     Spec
	ctxHasDead   bool
	ctxDeadline  time.Duration
	result       Result
	runErr       error
}

func (f *fakeRunner) Available(ctx context.Context) error {
	f.availableHit++
	return f.availableErr
}

func (f *fakeRunner) Run(ctx context.Context, spec Spec) (Result, error) {
	f.runHit++
	f.lastSpec = spec
	if deadline, ok := ctx.Deadline(); ok {
		f.ctxHasDead = true
		f.ctxDeadline = time.Until(deadline)
	}
	return f.result, f.runErr
}

type cmdCall struct {
	bin   string
	args  []string
	stdin string
}

type fakeCmd struct {
	calls   []cmdCall
	respond func(bin string, args []string) (string, int, error)
}

func (f *fakeCmd) Run(ctx context.Context, bin string, args []string, stdin string) (string, int, error) {
	f.calls = append(f.calls, cmdCall{bin: bin, args: args, stdin: stdin})
	if f.respond == nil {
		return "", 0, nil
	}
	return f.respond(bin, args)
}

func (f *fakeCmd) find(sub string) *cmdCall {
	for i := range f.calls {
		if len(f.calls[i].args) > 0 && f.calls[i].args[0] == sub {
			return &f.calls[i]
		}
	}
	return nil
}

// ---------- docker 参数拼装 ----------

func TestBuildDockerArgsCarriesAllIsolationFlags(t *testing.T) {
	args := buildDockerArgs(Spec{Language: "python", Image: "python:3.12-alpine", Code: "print(1)"}, "mcp-code-run-abc")
	joined := strings.Join(args, " ")

	mustContain := []string{
		"run --rm -i",
		"--name mcp-code-run-abc",
		"--network none",
		"--memory 256m",
		"--cpus 0.5",
		"--pids-limit 64",
		"--read-only",
		"--tmpfs /tmp:size=16m",
		"--user 65534",
		"--stop-timeout 3",
		"-e HOME=/tmp",
		"-e PYTHONDONTWRITEBYTECODE=1",
		"python:3.12-alpine python -",
	}
	for _, want := range mustContain {
		if !strings.Contains(joined, want) {
			t.Errorf("docker 参数缺少 %q\n实际: %s", want, joined)
		}
	}
	// 绝不能出现挂载宿主机目录的参数（否则沙箱可被穿透）。
	for _, forbidden := range []string{"-v ", "--volume", "--mount", "--privileged", "--network host"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("docker 参数不应包含 %q: %s", forbidden, joined)
		}
	}
}

func TestBuildDockerArgsNodeEntrypoint(t *testing.T) {
	args := buildDockerArgs(Spec{Language: "node", Image: "node:22-alpine"}, "n")
	tail := strings.Join(args[len(args)-3:], " ")
	if tail != "node:22-alpine node -" {
		t.Fatalf("node 入口命令不符: %q", tail)
	}
}

func TestContainerNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		name := containerName()
		if seen[name] {
			t.Fatalf("容器名重复: %s", name)
		}
		seen[name] = true
		if !strings.HasPrefix(name, "mcp-code-run-") {
			t.Fatalf("容器名前缀不符: %s", name)
		}
	}
}

// ---------- 工具层：绝不退化为宿主机直跑 ----------

func TestToolRefusesToRunWhenSandboxUnavailable(t *testing.T) {
	runner := &fakeRunner{availableErr: errors.New("未检测到 docker 可执行文件 \"docker\"")}
	tool := New(Options{Runner: runner})

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"code":"print(1)"}`))
	if err == nil {
		t.Fatal("沙箱不可用必须报错")
	}
	if !strings.Contains(err.Error(), "不会降级为在宿主机上直接运行代码") {
		t.Fatalf("错误信息应明确拒绝降级: %v", err)
	}
	if runner.runHit != 0 {
		t.Fatalf("沙箱不可用时绝不能调用执行器，实际调用 %d 次", runner.runHit)
	}
}

func TestToolCachesProbeSuccessAndRetriesFailure(t *testing.T) {
	ok := &fakeRunner{}
	tool := New(Options{Runner: ok})
	for i := 0; i < 3; i++ {
		if _, err := tool.Execute(context.Background(), json.RawMessage(`{"code":"pass"}`)); err != nil {
			t.Fatalf("执行失败: %v", err)
		}
	}
	if ok.availableHit != 1 {
		t.Fatalf("探测成功应被永久缓存，实际探测 %d 次", ok.availableHit)
	}

	bad := &fakeRunner{availableErr: errors.New("docker 守护进程不可用: down")}
	tool2 := New(Options{Runner: bad})
	for i := 0; i < 3; i++ {
		_, _ = tool2.Execute(context.Background(), json.RawMessage(`{"code":"pass"}`))
	}
	if bad.availableHit != 1 {
		t.Fatalf("失败应在 30 秒内缓存，实际探测 %d 次", bad.availableHit)
	}
}

func TestToolExecuteFormatsResult(t *testing.T) {
	runner := &fakeRunner{result: Result{Output: "42\n", ExitCode: 0}}
	tool := New(Options{Runner: runner, Timeout: 12 * time.Second})

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"language":"python","code":"print(42)"}`))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(out, "沙箱执行完成（容器内退出码 0）") || !strings.Contains(out, "42") {
		t.Fatalf("结果格式不符:\n%s", out)
	}
	if runner.lastSpec.Image != "python:3.12-alpine" || runner.lastSpec.Code != "print(42)" {
		t.Fatalf("传给 Runner 的 Spec 不符: %+v", runner.lastSpec)
	}
	// 工具必须给 Runner 施加自己的超时预算。
	if !runner.ctxHasDead || runner.ctxDeadline > 12*time.Second {
		t.Fatalf("ctx 未带 12s 超时预算: hasDead=%v deadline=%v", runner.ctxHasDead, runner.ctxDeadline)
	}
}

func TestToolReportsTimeoutWithPartialOutput(t *testing.T) {
	runner := &fakeRunner{result: Result{Output: "started...\n", ExitCode: -1, TimedOut: true}}
	tool := New(Options{Runner: runner, Timeout: 5 * time.Second})

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"code":"while True: pass"}`))
	if err != nil {
		t.Fatalf("超时应作为结果返回而非报错: %v", err)
	}
	if !strings.Contains(out, "超过 5s 被强制中止") || !strings.Contains(out, "started...") {
		t.Fatalf("超时结果格式不符:\n%s", out)
	}
}

func TestToolReportsEmptyOutput(t *testing.T) {
	runner := &fakeRunner{result: Result{Output: "   \n", ExitCode: 0}}
	out, err := New(Options{Runner: runner}).Execute(context.Background(), json.RawMessage(`{"code":"x=1"}`))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(out, "（无任何输出）") {
		t.Fatalf("空输出应显式说明:\n%s", out)
	}
}

func TestToolValidatesInput(t *testing.T) {
	tool := New(Options{Runner: &fakeRunner{}})
	cases := []struct {
		name string
		args string
		hint string
	}{
		{"空代码", `{}`, "code 不能为空"},
		{"空白代码", `{"code":"   "}`, "code 不能为空"},
		{"坏 JSON", `{`, "参数解析失败"},
		{"未知语言", `{"language":"ruby","code":"puts 1"}`, "不支持的语言"},
		{"超长代码", `{"code":"` + strings.Repeat("a", DefaultMaxCodeBytes+1) + `"}`, "code 过大"},
	}
	for _, tc := range cases {
		_, err := tool.Execute(context.Background(), json.RawMessage(tc.args))
		if err == nil {
			t.Errorf("%s 应报错", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.hint) {
			t.Errorf("%s 错误信息 %q 未包含 %q", tc.name, err.Error(), tc.hint)
		}
	}
}

func TestToolDefaultsLanguageToPython(t *testing.T) {
	runner := &fakeRunner{}
	if _, err := New(Options{Runner: runner}).Execute(context.Background(), json.RawMessage(`{"code":"pass"}`)); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if runner.lastSpec.Language != "python" {
		t.Fatalf("默认语言 = %q", runner.lastSpec.Language)
	}
}

func TestToolSupportsCustomImageWhitelist(t *testing.T) {
	runner := &fakeRunner{}
	tool := New(Options{
		Runner: runner,
		Images: map[string]string{"python": "python:3.12-alpine", "go": "golang:1.27-alpine"},
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"language":"go","code":"package main"}`))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if runner.lastSpec.Image != "golang:1.27-alpine" {
		t.Fatalf("自定义镜像未生效: %q", runner.lastSpec.Image)
	}
	if !strings.Contains(out, "沙箱执行完成") {
		t.Fatalf("结果格式不符: %s", out)
	}

	schema := tool.InputSchema()
	props := schema["properties"].(map[string]any)
	enum := props["language"].(map[string]any)["enum"].([]any)
	if len(enum) != 2 || enum[0] != "go" || enum[1] != "python" {
		t.Fatalf("语言枚举应与白名单一致且有序: %v", enum)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"language":"ruby","code":"x"}`)); err == nil {
		t.Fatal("白名单外的语言应被拒绝")
	}
}

func TestToolSchemaAndName(t *testing.T) {
	tool := New(Options{})
	if tool.Name() != "code_run" {
		t.Fatalf("工具名 = %q", tool.Name())
	}
	if err := tools.ValidateInputSchema(tool.InputSchema()); err != nil {
		t.Fatalf("schema 不合规: %v", err)
	}
}

// ---------- DockerRunner ----------

func TestDockerRunnerFeedsCodeOnStdinAndMergesOutput(t *testing.T) {
	cmd := &fakeCmd{respond: func(bin string, args []string) (string, int, error) {
		return "hello\nboom\n", 3, nil
	}}
	r := &DockerRunner{Path: "docker", Exec: cmd}

	res, err := r.Run(context.Background(), Spec{Language: "python", Image: "python:3.12-alpine", Code: "print('hello')"})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Output != "hello\nboom\n" || res.ExitCode != 3 || res.TimedOut {
		t.Fatalf("结果不符: %+v", res)
	}
	run := cmd.find("run")
	if run == nil {
		t.Fatal("未调用 docker run")
	}
	if run.stdin != "print('hello')" {
		t.Fatalf("代码未经 stdin 传入: %q", run.stdin)
	}
	if cmd.find("kill") != nil {
		t.Fatal("正常结束时不应清理容器")
	}
}

func TestDockerRunnerKillsContainerOnTimeout(t *testing.T) {
	var containerName string
	cmd := &fakeCmd{respond: func(bin string, args []string) (string, int, error) {
		if args[0] == "run" {
			for i := 0; i < len(args)-1; i++ {
				if args[i] == "--name" {
					containerName = args[i+1]
				}
			}
			return "partial\n", -1, context.DeadlineExceeded
		}
		return "", 0, nil
	}}
	r := &DockerRunner{Path: "docker", Exec: cmd}

	res, err := r.Run(context.Background(), Spec{Language: "python", Image: "img", Code: "while True: pass"})
	if err != nil {
		t.Fatalf("超时不应报错: %v", err)
	}
	if !res.TimedOut || res.ExitCode != -1 || !strings.Contains(res.Output, "partial") {
		t.Fatalf("超时结果不符: %+v", res)
	}
	kill := cmd.find("kill")
	if kill == nil {
		t.Fatal("超时后必须显式 kill 容器（CI 被 kill 不会自动停容器）")
	}
	if kill.args[1] != containerName {
		t.Fatalf("kill 的容器名不符: %q vs %q", kill.args[1], containerName)
	}
}

func TestDockerRunnerRejectsDockerExit125(t *testing.T) {
	cmd := &fakeCmd{respond: func(string, []string) (string, int, error) {
		return "docker: Error response from daemon: pull access denied", dockerExitError, nil
	}}
	r := &DockerRunner{Path: "docker", Exec: cmd}

	if _, err := r.Run(context.Background(), Spec{Language: "python", Image: "x", Code: "1"}); err == nil {
		t.Fatal("exit 125 应报错")
	} else if !strings.Contains(err.Error(), "docker 拒绝执行") {
		t.Fatalf("错误信息不符: %v", err)
	}
}

func TestDockerRunnerReportsCanceledRun(t *testing.T) {
	cmd := &fakeCmd{respond: func(string, []string) (string, int, error) {
		return "", -1, context.Canceled
	}}
	r := &DockerRunner{Path: "docker", Exec: cmd}
	if _, err := r.Run(context.Background(), Spec{Image: "x"}); err == nil {
		t.Fatal("取消应报错")
	}
	if cmd.find("kill") == nil {
		t.Fatal("取消后也应清理容器")
	}
}

func TestDockerRunnerAvailable(t *testing.T) {
	// 找不到可执行文件
	r := &DockerRunner{
		Path:     "docker",
		Exec:     &fakeCmd{},
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
	}
	if err := r.Available(context.Background()); err == nil || !strings.Contains(err.Error(), "未检测到 docker") {
		t.Fatalf("应报告找不到 docker: %v", err)
	}

	// 找到了但 daemon 不可用
	cmd := &fakeCmd{respond: func(string, []string) (string, int, error) {
		return "Cannot connect to the Docker daemon", 1, nil
	}}
	r = &DockerRunner{Path: "docker", Exec: cmd, LookPath: func(string) (string, error) { return "/usr/bin/docker", nil }}
	if err := r.Available(context.Background()); err == nil || !strings.Contains(err.Error(), "守护进程不可用") {
		t.Fatalf("应报告 daemon 不可用: %v", err)
	}

	// 正常
	cmd = &fakeCmd{respond: func(string, []string) (string, int, error) { return "27.0.0", 0, nil }}
	r = &DockerRunner{Path: "docker", Exec: cmd, LookPath: func(string) (string, error) { return "/usr/bin/docker", nil }}
	if err := r.Available(context.Background()); err != nil {
		t.Fatalf("可用时应返回 nil: %v", err)
	}
	probe := cmd.find("version")
	if probe == nil || probe.args[1] != "--format" {
		t.Fatalf("探测命令不符: %+v", probe)
	}
}

// ---------- 辅助 ----------

func TestLimitBufferAlwaysReportsFullWrite(t *testing.T) {
	w := &limitBuffer{limit: 5}
	n, err := w.Write([]byte("abcdefgh"))
	if err != nil || n != 8 {
		t.Fatalf("必须报告完整写入长度（否则 exec 会以 ErrShortWrite 中断）：n=%d err=%v", n, err)
	}
	if w.String() != "abcde" || !w.truncated {
		t.Fatalf("截断行为不符: %q truncated=%v", w.String(), w.truncated)
	}
	if n, _ := w.Write([]byte("ij")); n != 2 {
		t.Fatalf("超限后仍应返回写入长度: %d", n)
	}
}

func TestTruncateCharsKeepsUTF8Intact(t *testing.T) {
	got := truncateChars(strings.Repeat("中", 10), 3)
	if !strings.HasPrefix(got, "中中中") || !strings.Contains(got, "已截断") {
		t.Fatalf("截断结果错误: %q", got)
	}
	if strings.Contains(got, "\ufffd") {
		t.Fatalf("出现乱码: %q", got)
	}
	if short := truncateChars("ok", 10); short != "ok" {
		t.Fatalf("未超限不应截断: %q", short)
	}
}

func TestOutputTruncationAppliesToLargeSandboxOutput(t *testing.T) {
	runner := &fakeRunner{result: Result{Output: strings.Repeat("x", 20000), ExitCode: 0}}
	out, err := New(Options{Runner: runner}).Execute(context.Background(), json.RawMessage(`{"code":"pass"}`))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(out, "输出已截断") {
		t.Fatalf("超长输出应标注截断:\n%s", out[:120])
	}
	if n := strings.Count(out, "x"); n != DefaultMaxOutputChar {
		t.Fatalf("截断字符数 = %d, 期望 %d", n, DefaultMaxOutputChar)
	}
}
