// Package coderun 实现 MCP 工具 code_run：在 Docker 沙箱里执行一段代码。
//
// 安全底线：**检测不到 Docker 就返回可读错误，绝不退化为宿主机直跑**。
// 宿主机直跑意味着模型生成的任意代码在服务进程的权限下执行，等价于直接放弃隔离，
// 这是本项目明确不接受的风险。
//
// 沙箱参数（对齐方案 S4）：
//
//	docker run --rm -i --name <random> --network none --memory 256m --cpus 0.5
//	           --pids-limit 64 --read-only --tmpfs /tmp:size=16m --user 65534
//	           --stop-timeout 3 -e HOME=/tmp -e PYTHONDONTWRITEBYTECODE=1
//	           <image> <entrypoint>
//
// 代码通过 stdin 喂入（`python -` / `node -`），因此不需要挂载任何卷。
package coderun

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"mcp-server-go/internal/tools"
)

// 默认限额。
const (
	DefaultTimeout       = 30 * time.Second
	DefaultMaxCodeBytes  = 20 << 10 // 20 KiB
	DefaultMaxOutputChar = 8000
	maxRawOutputBytes    = 64 << 10 // 进程输出先截到 64 KiB，再按字符截到 8000
	probeTimeout         = 5 * time.Second
	probeRetryInterval   = 30 * time.Second
	killTimeout          = 5 * time.Second
)

// dockerExitError 是 docker CLI 自身的错误退出码（区别于容器内进程的退出码）。
const dockerExitError = 125

// 默认语言白名单：语言 → 镜像。
var defaultImages = map[string]string{
	"python": "python:3.12-alpine",
	"node":   "node:22-alpine",
}

// 默认入口命令：语言 → 容器内 argv（末尾的 "-" 表示从 stdin 读代码）。
var defaultEntrypoints = map[string][]string{
	"python": {"python", "-"},
	"node":   {"node", "-"},
}

// Spec 是一次沙箱执行请求。
type Spec struct {
	Language string
	Image    string
	Code     string
}

// Result 是一次沙箱执行结果。
type Result struct {
	// Output 是 stdout 与 stderr 合并后的文本（已按字节上限截断）。
	Output string
	// ExitCode 是容器内进程的退出码；超时被中止时为 -1。
	ExitCode int
	// TimedOut 表示因超过时限被强制中止。
	TimedOut bool
}

// CommandRunner 抽象「执行一条外部命令并把 stdin 喂进去」，便于单测注入。
type CommandRunner interface {
	// Run 执行命令；stdin 为喂给进程的标准输入。
	// 返回合并输出与退出码；err 非 nil 表示命令本身没跑起来或被中止（超时/取消）。
	Run(ctx context.Context, bin string, args []string, stdin string) (output string, exitCode int, err error)
}

// Runner 抽象沙箱：探测可用性 + 执行代码。
type Runner interface {
	// Available 探测沙箱是否可用；返回 nil 表示可用，否则返回可读原因。
	Available(ctx context.Context) error
	// Run 执行代码。
	Run(ctx context.Context, spec Spec) (Result, error)
}

// DockerRunner 是基于 docker CLI 的 Runner 实现。
type DockerRunner struct {
	// Path 是 docker 可执行文件名或路径，空则用 "docker"。
	Path string
	// Exec 是命令执行器，空则用 execCommandRunner。
	Exec CommandRunner
	// LookPath 用于定位可执行文件，仅测试需要覆盖。
	LookPath func(string) (string, error)
}

// NewDockerRunner 构造生产用 Runner。
func NewDockerRunner() *DockerRunner {
	return &DockerRunner{Path: "docker", Exec: execCommandRunner{limit: maxRawOutputBytes}, LookPath: exec.LookPath}
}

func (d *DockerRunner) bin() string {
	if d.Path == "" {
		return "docker"
	}
	return d.Path
}

func (d *DockerRunner) exec() CommandRunner {
	if d.Exec == nil {
		return execCommandRunner{limit: maxRawOutputBytes}
	}
	return d.Exec
}

func (d *DockerRunner) lookPath() func(string) (string, error) {
	if d.LookPath == nil {
		return exec.LookPath
	}
	return d.LookPath
}

// Available 懒探测 docker 是否可用：先找可执行文件，再问一次 daemon 版本。
func (d *DockerRunner) Available(ctx context.Context) error {
	path, err := d.lookPath()(d.bin())
	if err != nil {
		return fmt.Errorf("未检测到 docker 可执行文件 %q", d.bin())
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, code, err := d.exec().Run(probeCtx, path, []string{"version", "--format", "{{.Server.Version}}"}, "")
	if err != nil || code != 0 {
		msg := strings.TrimSpace(out)
		if msg == "" && err != nil {
			msg = err.Error()
		}
		if msg == "" {
			msg = fmt.Sprintf("退出码 %d", code)
		}
		return fmt.Errorf("docker 守护进程不可用: %s", msg)
	}
	return nil
}

// Run 执行代码；超时会被强制中止并顺带清掉可能仍在运行的容器。
func (d *DockerRunner) Run(ctx context.Context, spec Spec) (Result, error) {
	name := containerName()
	args := buildDockerArgs(spec, name)
	output, exitCode, err := d.exec().Run(ctx, d.bin(), args, spec.Code)
	if err != nil {
		// docker CLI 被 kill 不会自动停容器，这里显式补一刀，避免死循环容器常驻。
		d.killContainer(name)
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return Result{Output: output, ExitCode: -1, TimedOut: true}, nil
		case errors.Is(err, context.Canceled):
			return Result{}, errors.New("沙箱执行被取消")
		default:
			return Result{}, fmt.Errorf("沙箱执行失败: %w", err)
		}
	}
	if exitCode == dockerExitError {
		return Result{}, fmt.Errorf("docker 拒绝执行（退出码 %d）：%s", dockerExitError, strings.TrimSpace(output))
	}
	return Result{Output: output, ExitCode: exitCode}, nil
}

// killContainer 尽力停掉容器；失败也不影响主流程。
func (d *DockerRunner) killContainer(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), killTimeout)
	defer cancel()
	_, _, _ = d.exec().Run(ctx, d.bin(), []string{"kill", name}, "")
}

// buildDockerArgs 构造 docker run 参数。
//
// 刻意做成纯函数：这是隔离强度的唯一来源，必须能被单测逐项断言。
func buildDockerArgs(spec Spec, containerName string) []string {
	args := []string{
		"run", "--rm", "-i",
		"--name", containerName,
		"--network", "none", // 无网络
		"--memory", "256m", // 内存上限
		"--cpus", "0.5", // CPU 上限
		"--pids-limit", "64", // 进程数上限，挡 fork 炸弹
		"--read-only",              // 根文件系统只读
		"--tmpfs", "/tmp:size=16m", // 只给 /tmp 一小块可写空间
		"--user", "65534", // 非 root（nobody）
		"--stop-timeout", "3",
		"-e", "HOME=/tmp",
		"-e", "PYTHONDONTWRITEBYTECODE=1",
		spec.Image,
	}
	return append(args, entrypointFor(spec.Language)...)
}

// entrypointFor 返回语言对应的容器内入口命令；未知语言退化为从 stdin 读代码。
func entrypointFor(language string) []string {
	if ep, ok := defaultEntrypoints[language]; ok {
		return ep
	}
	return defaultEntrypoints["python"]
}

// containerName 生成唯一容器名，便于超时后精确清理。
func containerName() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("mcp-code-run-%x", time.Now().UnixNano())
	}
	return "mcp-code-run-" + hex.EncodeToString(buf)
}

// execCommandRunner 是 CommandRunner 的生产实现。
type execCommandRunner struct {
	// limit 是输出保留的字节上限，超出部分直接丢弃（继续读取以避免子进程阻塞）。
	limit int
}

func (r execCommandRunner) Run(ctx context.Context, bin string, args []string, stdin string) (string, int, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(stdin)
	sink := &limitBuffer{limit: r.limit}
	cmd.Stdout = sink
	cmd.Stderr = sink

	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return sink.String(), -1, ctxErr
	}
	if err == nil {
		return sink.String(), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// 容器内进程非零退出属于正常业务结果，交给上层展示。
		return sink.String(), exitErr.ExitCode(), nil
	}
	return sink.String(), -1, err
}

// limitBuffer 是只保留前 limit 字节的写入端。
// 注意必须始终报告「全部写入」，否则 exec 的拷贝协程会以 ErrShortWrite 提前中断。
type limitBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *limitBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if remain := w.limit - w.buf.Len(); remain > 0 {
		if len(p) > remain {
			w.buf.Write(p[:remain])
			w.truncated = true
		} else {
			w.buf.Write(p)
		}
	} else {
		w.truncated = true
	}
	return n, nil
}

func (w *limitBuffer) String() string { return w.buf.String() }

// Options 是工具构造参数。
type Options struct {
	// Runner 为空时使用 DockerRunner（生产默认）。
	Runner Runner
	// Images 覆盖语言白名单（语言 → 镜像）；为空时用 defaultImages。
	Images map[string]string
	// DefaultLanguage 为空时用 "python"。
	DefaultLanguage string
	// Timeout 为空时用 DefaultTimeout。
	Timeout time.Duration
	// MaxCodeBytes 为 0 时用 DefaultMaxCodeBytes。
	MaxCodeBytes int
	// MaxOutputChars 为 0 时用 DefaultMaxOutputChar。
	MaxOutputChars int
}

// Tool 是 code_run 工具。
type Tool struct {
	runner          Runner
	images          map[string]string
	languages       []string
	defaultLanguage string
	timeout         time.Duration
	maxCodeBytes    int
	maxOutputChars  int

	probeMu      sync.Mutex
	probeOK      bool
	lastProbe    time.Time
	lastProbeErr error
}

// New 构造 code_run 工具。
func New(opts Options) *Tool {
	runner := opts.Runner
	if runner == nil {
		runner = NewDockerRunner()
	}
	images := opts.Images
	if len(images) == 0 {
		images = defaultImages
	}
	langs := make([]string, 0, len(images))
	for lang := range images {
		langs = append(langs, lang)
	}
	sort.Strings(langs)

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxCode := opts.MaxCodeBytes
	if maxCode <= 0 {
		maxCode = DefaultMaxCodeBytes
	}
	maxOut := opts.MaxOutputChars
	if maxOut <= 0 {
		maxOut = DefaultMaxOutputChar
	}
	defaultLang := opts.DefaultLanguage
	if defaultLang == "" {
		defaultLang = "python"
	}
	if _, ok := images[defaultLang]; !ok && len(langs) > 0 {
		defaultLang = langs[0]
	}
	return &Tool{
		runner:          runner,
		images:          images,
		languages:       langs,
		defaultLanguage: defaultLang,
		timeout:         timeout,
		maxCodeBytes:    maxCode,
		maxOutputChars:  maxOut,
	}
}

// Name 返回工具名。
func (*Tool) Name() string { return "code_run" }

// Description 返回给模型看的说明。
func (*Tool) Description() string {
	return "在一次性 Docker 沙箱中执行一段代码（默认 Python，可选 Node），用于精确计算、数据转换与验证片段。" +
		"沙箱无网络、内存 256MB、CPU 0.5 核、只读文件系统、仅 /tmp 16MB 可写、以 nobody 用户运行，最长 30 秒。" +
		"代码通过标准输入传入，因此不支持读写文件或安装依赖。若运行环境没有 Docker，本工具会直接报错而不会退化到宿主机执行。"
}

// InputSchema 返回入参 schema；语言枚举由实际白名单生成，保证与运行时一致。
func (t *Tool) InputSchema() map[string]any {
	enum := make([]any, 0, len(t.languages))
	for _, lang := range t.languages {
		enum = append(enum, lang)
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"language": map[string]any{
				"type":        "string",
				"description": "代码语言，默认 " + t.defaultLanguage,
				"enum":        enum,
			},
			"code": map[string]any{
				"type":        "string",
				"description": "要执行的完整代码，通过标准输入传入；只应依赖标准库",
			},
		},
		"required": []any{"code"},
	}
}

// Execute 在沙箱中执行代码。
func (t *Tool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Language string `json:"language"`
		Code     string `json:"code"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("参数解析失败: %w", err)
		}
	}
	if strings.TrimSpace(in.Code) == "" {
		return "", errors.New("code 不能为空")
	}
	if len(in.Code) > t.maxCodeBytes {
		return "", fmt.Errorf("code 过大（%d 字节，上限 %d 字节）", len(in.Code), t.maxCodeBytes)
	}
	lang := strings.ToLower(strings.TrimSpace(in.Language))
	if lang == "" {
		lang = t.defaultLanguage
	}
	image, ok := t.images[lang]
	if !ok {
		return "", fmt.Errorf("不支持的语言 %q（可用：%s）", in.Language, strings.Join(t.languages, ", "))
	}

	// 沙箱不可用一律直接报错：绝不退化为宿主机直跑。
	if err := t.available(ctx); err != nil {
		return "", fmt.Errorf("%w。本工具不会降级为在宿主机上直接运行代码", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	res, err := t.runner.Run(runCtx, Spec{Language: lang, Image: image, Code: in.Code})
	if err != nil {
		return "", err
	}
	return t.format(res), nil
}

// available 带 30 秒失败缓存地探测沙箱可用性：成功一次就永久放行，
// 失败则短暂缓存，避免每次调用都付出探测开销，也避免探测风暴。
func (t *Tool) available(ctx context.Context) error {
	t.probeMu.Lock()
	defer t.probeMu.Unlock()
	if t.probeOK {
		return nil
	}
	if t.lastProbeErr != nil && time.Since(t.lastProbe) < probeRetryInterval {
		return t.lastProbeErr
	}
	err := t.runner.Available(ctx)
	t.lastProbe = time.Now()
	t.lastProbeErr = err
	if err == nil {
		t.probeOK = true
	}
	return err
}

// format 组装给模型的结果文本。
func (t *Tool) format(res Result) string {
	var b strings.Builder
	if res.TimedOut {
		fmt.Fprintf(&b, "沙箱执行超过 %s 被强制中止，以下是中止前已产生的输出：\n", t.timeout)
	} else {
		fmt.Fprintf(&b, "沙箱执行完成（容器内退出码 %d）：\n", res.ExitCode)
	}
	if strings.TrimSpace(res.Output) == "" {
		b.WriteString("（无任何输出）")
		return b.String()
	}
	b.WriteString(truncateChars(res.Output, t.maxOutputChars))
	return b.String()
}

// truncateChars 按字符截断，避免截断多字节字符。
func truncateChars(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + fmt.Sprintf("\n\n[输出已截断，仅显示前 %d 个字符]", limit)
}

// 编译期断言：确保实现满足 tools.Tool 接口。
var _ tools.Tool = (*Tool)(nil)
var _ Runner = (*DockerRunner)(nil)
