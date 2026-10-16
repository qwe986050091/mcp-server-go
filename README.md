# mcp-server-go

零第三方依赖的通用 **MCP（Model Context Protocol）Server**：只用 Go 标准库实现协议层与工具层，
同时提供 **stdio** 与 **Streamable HTTP** 两种传输，内置 5 个开箱即用的工具。
任何 MCP 客户端（Cursor / Claude Desktop / Trae / 自研 Agent）都能直接接入。

- **零依赖**：`go.mod` 里没有任何 `require`。协议编解码、HTML 解析、SSRF 守卫、表达式求值全部自研，
  既能离线构建，也便于按需替换为官方 SDK。
- **双传输**：stdio（按行分隔的 JSON 帧）+ Streamable HTTP（单端点 POST，内存会话表）。
- **五个工具**：`web_search`、`web_fetch`、`kb_search`、`code_run`、`calc`。
- **安全默认**：SSRF 三层防护 + 逐跳重校验的重定向；Docker 沙箱不可用时**直接报错，绝不退化为宿主机直跑**。
- **生产验证**：已在自有服务器上接入一个 LangChain4j 编写的 Java Agent，跑通
  「Java Agent → MCP Client → Go MCP Server → 回调 Java 检索接口」的跨语言闭环（含人工审批门）。

## 架构

```text
              MCP 客户端（Cursor / Claude Desktop / Trae / LangChain4j Agent …）
                                        │
          ┌─────────────────────────────┴─────────────────────────────┐
          │ stdio                                                      │ Streamable HTTP
          │ 按行分隔 JSON，stdout 只走协议                              │ 单端点 POST /mcp
          ▼                                                            ▼
  internal/transport/stdio                              internal/transport/http
          │ bufio.Scanner · json.Encoder + Flush                 │ 会话表 · Origin / 共享密钥校验
          │ 1MiB 帧上限 · 容忍 CRLF 与 BOM                        │ 200/202/204/405/406/413/415，绝不 3xx
          └─────────────────────────────┬─────────────────────────────┘
                                        ▼
                            internal/server（方法路由，传输无关）
      initialize（回显客户端协议版本）· ping · tools/list · tools/call
      通知静默处理 · JSON-RPC 错误码映射 · 工具 panic 兜底为 isError
                                        │
                    ┌───────────────────┴───────────────────┐
                    ▼                                       ▼
        internal/tools（Tool 接口 + Registry，注册顺序即 tools/list 顺序）
    ┌───────────┬───────────┬───────────┬────────────┬──────────┐
    ▼           ▼           ▼           ▼            ▼
web_search  web_fetch   kb_search    code_run      calc
    │           │            │            │            │
    │    internal/safehttp   │       Docker 沙箱    自研词法 + 递归下降
    │    （SSRF 三层防护）   │      （不可用即拒绝）  求值（无 eval / 无标识符）
    └──── internal/htmlx（最小 HTML 分词 · 取文本 · 实体解码）
                             │
                    POST /api/agent/rag/search → 外部知识库（可换成任意 HTTP 检索后端）
```

## 快速开始

```bash
go build -o bin/mcp-server-go ./cmd/mcp-server          # 需要 Go 1.22+
```

### 方式一：stdio（本地子进程，最常用）

```bash
./bin/mcp-server-go --transport stdio
```

客户端配置（Claude Desktop `claude_desktop_config.json` / Cursor `mcp.json`）：

```json
{
  "mcpServers": {
    "go": {
      "command": "/absolute/path/to/mcp-server-go",
      "args": ["--transport", "stdio"]
    }
  }
}
```

### 方式二：Streamable HTTP（常驻服务）

```bash
./bin/mcp-server-go --transport http --addr 127.0.0.1:3001 --path /mcp
```

- **只允许绑定回环地址**：`--addr 0.0.0.0:3001` 会被直接拒绝（显式校验，避免误暴露到公网）。
- 可选共享密钥：`--token <secret>`（或环境变量 `MCP_HTTP_TOKEN`），非空时所有请求必须带 `X-MCP-Token`。

客户端配置：

```json
{
  "mcpServers": {
    "go": { "type": "http", "url": "http://127.0.0.1:3001/mcp" }
  }
}
```

### 命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `--transport` | `stdio` | `stdio` 或 `http` |
| `--addr` | `127.0.0.1:3001` | HTTP 监听地址，**只接受回环** |
| `--path` | `/mcp` | HTTP 端点路径 |
| `--token` | 空 | HTTP 共享密钥（也可用 `MCP_HTTP_TOKEN`） |

### 环境变量

| 变量 | 作用 |
|---|---|
| `KB_BASE_URL` | `kb_search` 的目标检索服务地址，默认 `http://127.0.0.1:8080` |
| `AGENT_INTERNAL_TOKEN` | `kb_search` 调用内部检索接口时携带的口令（stdio 模式下子进程会继承父进程环境） |
| `KB_USER_ID` | `kb_search` 未收到注入的 `user_id` 时使用的兜底用户标识 |

## 工具

| 工具 | 关键参数 | 说明 | 约束 |
|---|---|---|---|
| `web_search` | `query`、`count`(默认 5，上限 10) | 免 API Key 网页搜索：依次尝试 Bing（`cn.bing.com`）与 DuckDuckGo（`html.duckduckgo.com`），任一出结果即返回 | 单源 8s、总 15s；UA + `Accept-Language: zh-CN` |
| `web_fetch` | `url` | 抓取网页，返回标题 + 正文纯文本（剥离 script/style/iframe/svg） | 仅 http/https；最多跟随 3 跳重定向且**每跳重校验**；响应体 ≤2MB，正文 ≤8000 字符并标注截断 |
| `kb_search` | `query`、`top_k`(默认 5，上限 10) | 检索外部私有知识库（本仓库对接的是 Java 侧的混合检索接口） | 接口不可用时**降级为说明文本**而不是报错，避免中断整轮对话 |
| `code_run` | `code`、`language`(`python`/`node`) | 在一次性 Docker 沙箱中执行代码，代码经 stdin 传入 | 无网络、256MB 内存、0.5 核、64 进程、只读根文件系统、仅 `/tmp` 16MB 可写、`nobody` 用户、30s 上限 |
| `calc` | `expression` | 数学表达式求值：`+ - * / % ^`（右结合）、括号、一元正负号、常量 `pi`/`e`、函数 `abs/sqrt/pow/min/max/round/floor/ceil/log/exp` | 语法上不接受任何标识符变量，无 `eval`；除零 / 非有限结果返回可读错误 |

### `tools/call` 结果约定

业务失败（超时、SSRF 拒绝、沙箱不可用、表达式非法）一律走 **`isError: true` + 可读文本**，
只有协议级错误才用 JSON-RPC error：`-32601` 未知方法 / `-32602` 未知工具或参数非法 / `-32700` 解析失败。

## 协议实现要点

写这个项目的过程中有若干「不遵守就连不上」的细节，都来自对真实客户端（`langchain4j-mcp 1.17.2-beta27`）
字节码与 MCP 规范的双向核对：

- **协议版本回显**：`initialize` 原样回显客户端请求的版本；不在支持集内时回退到 `2025-11-25`，
  **绝不因版本不符拒绝连接**；`MCP-Protocol-Version` 头缺失也不报错。
- **stdio 帧格式是按行 JSON**（不是 `Content-Length`）：`bufio.Scanner` 提升到 1MiB，
  写入用 `json.Encoder` 单行 + `\n` 并 Flush；**stdout 只承载协议帧，日志一律走 stderr**。
- **`tools/list` 的 `inputSchema` 只使用白名单关键字**（`type/description/enum/items/properties/required`）：
  LangChain4j 的 `ToolSpecificationHelper` 遇到未知类型会抛错并导致 `listTools()` 失败，连接随即被判定为失败。
  本项目把该校验放在**注册期**（fail-fast），并有单测遍历全部工具 schema。
- **Streamable HTTP 契约**：`POST /mcp` 直接回 `application/json`（不做 SSE）；通知类输入回 **202 空体**；
  `initialize` 响应带 `Mcp-Session-Id`；`GET /mcp` → `405`；`DELETE /mcp` → `204`；
  `Accept` 缺少 `text/event-stream` → `406`；**任何路径都不返回 3xx**（`/mcp/` 直接 404 而不是 301）。
- **会话校验选择宽松策略**：服务端每请求无状态，未知或过期会话 id 仅告警放行 —— 因为真实客户端遇到
  `404` 会自动重新初始化并重试，为无状态的场景付出一次完整重协商纯属损耗。

## 安全设计

- **SSRF 三层防护**（`internal/safehttp`）：
  1. 语法层：只允许 `http`/`https`，禁止 userinfo，必须有主机名；
  2. 解析层：解析出**全部** A/AAAA 并逐一拒绝回环、私有、链路本地、多播、CGNAT(`100.64/10`)、
     `0.0.0.0/8`、保留段、IPv4-mapped、`fc00::/7` 等；**解析失败 fail-closed**；
     该判定在 `DialContext` 里再执行一次，封死「先解析到公网、连接时被重绑定到内网」的 TOCTOU 窗口；
  3. 跳转层：禁自动跟随重定向，逐跳重新校验，最多 3 跳。
  另外显式禁用环境代理（`Proxy: nil`），避免 `http_proxy` 绕过 IP 校验。
- **`code_run` 绝不降级**：检测不到 Docker 就直接返回可读错误。宿主机直跑意味着模型生成的任意代码
  以服务进程权限执行，等价于放弃隔离 —— 这是本项目明确不接受的风险。
  超时被强制中止时会显式补一次 `docker kill`（CLI 被 kill 不会自动停容器）。
- **多租户隔离**：`kb_search` 的 `user_id` 是**不写进 schema 的隐藏参数**，由调用方（服务端）注入并覆盖，
  模型既看不到也伪造不了，从而无法越权检索他人数据。
- **HTTP 侧防误暴露**：只绑回环、校验 `Origin` 仅放行回环来源、可选共享密钥（常量时间比较）、
  请求体 4MiB 上限、`http.Server` 设置读头超时与空闲超时。

## 端到端验证记录

### 本机（协议层 + 全部工具，无需网络）

- `go vet ./...` 干净；**151 个测试函数**全绿（含：JSON-RPC 帧金标比对、HTTP 状态码矩阵、
  SSRF 规则表、docker argv 逐项断言、工具 panic 兜底、schema 白名单）；
- `scripts/smoke.ps1` 一条命令跑两段冒烟，**53 项断言**全过：
  - stdio：喂 9 帧（含通知、坏帧）→ 断言 stdout 恰好 10 行合法 JSON、逐字节金标比对、日志不混入 stdout；
  - Streamable HTTP：起服务后用**客户端真实请求头**（`Accept: application/json,text/event-stream`）
    打 `initialize`/`tools/list`/`tools/call`，断言 `Mcp-Session-Id`、`202` 空体、`405`、`204`、
    `/mcp/` 不重定向、非回环 `Origin` 被拒。

### 云端（与 Java Agent 组成跨语言闭环）

部署形态：Go MCP Server 作为 systemd 常驻服务监听 `127.0.0.1:3001`，Java Agent 在同一个
`application.yml` 里同时挂载 http 与 stdio 两条链路（`injectUserId: true`）。实测结果：

```text
[MCP] Server「...」连接成功，可用工具 5 个          ← 两条传输链路（HTTP 与 stdio）各验证一次

# 挂载 mcp:{server} 技能后由真实模型触发工具调用，SSE 流中出现：
event:tool
data:{"round":0,"name":"kb_search","arguments":"{\"query\": \"…\", \"top_k\": 5}",
      "result":"知识库检索：\"…\"（topK=5，命中 5 条）\n\n1. 来源：…｜相关度：0.757｜通道：vector…",
      "success":true,"durationMs":6503}

# 审批门（同一 server 改成 approval: confirm 后重触发）：
event:approval-request
data:{"tool":"mcp:{server}:kb_search","riskLevel":"high","title":"MCP 工具（{server}/kb_search）",
      "argsPreview":"{\"query\":\"…\",\"user_id\":\"…\"}"}      ← 隐藏 user_id 注入生效，模型侧不可见
→ 调用决策接口放行 → 工具真正执行并返回结果

# 安全面同样在云端复核：web_fetch 抓取云元数据地址被拒
"SSRF 防护拒绝访问 169.254.169.254：链路本地地址 169.254/16"

# 未安装 Docker 的机器上 code_run 的降级（不直跑）：
"未检测到 docker 可执行文件 \"docker\"。本工具不会降级为在宿主机上直接运行代码"
```

## 性能

| 指标 | 实测 |
|---|---|
| 常驻内存 | **9.2MB**（云端 4c4g 上 systemd 服务实测） |
| 启动 | 零阻塞：不预建容器、不预解析域名、不预探测 Docker（懒探测 + 成功永久缓存 / 失败 30s 重试） |
| `initialize` + `tools/list` | 本机 ~1ms 级；工具列表在装配期一次性注册后只读 |
| 自身计算开销 | `calc` 亚毫秒；`web_search`/`web_fetch` 耗时取决于网络 |
| 超时预算 | HTTP 客户端默认 15s、`web_search` 单源 8s / 总 15s、`kb_search` 15s、`code_run` 30s，均显著小于 MCP 客户端 60s 的工具执行超时 |
| `kb_search` 端到端 | 6.1–6.7s，其中开销几乎全在下游检索链路（向量 + 关键词 + 重排），本服务只做转发与格式化 |

> `kb_search` 的 15s 超时是按实测数据定的：新机稳态 6.1–6.7s，冷启动（下游首次加载 embedding / rerank 模型）
> 会超过 8s，故留出冷启动余量。

## 目录结构

```text
cmd/mcp-server/            入口：flag 解析 + 显式登记工具 + 选择传输
internal/jsonrpc/          JSON-RPC 2.0 编解码（id 保型、错误码常量）
internal/protocol/         MCP 数据结构（initialize / tools / content）+ 版本协商
internal/server/           方法路由（与传输无关）
internal/transport/stdio/  stdio 传输
internal/transport/http/   Streamable HTTP 传输（包名 mcphttp，避免与 net/http 撞名）
internal/tools/            Tool 接口 + Registry + inputSchema 白名单校验
internal/tools/{calc,coderun,kbsearch,webfetch,websearch}/
internal/safehttp/         SSRF 守卫 + 受限 HTTP 客户端（可注入 resolver/transport 便于测试）
internal/htmlx/            最小 HTML 分词 / 取文本 / 取属性 / 实体解码
testdata/stdio/            stdio 帧与响应的逐字节金标
scripts/smoke.ps1          stdio + HTTP 双段冒烟
scripts/deploy/            部署与端到端验收脚本（作者自用）
```

## 测试

```bash
go vet ./...
go test ./... -count=1
```

CI 见 `.github/workflows/ci.yml`（`go vet` + `go test`）。

测试可全离线运行：网络相关逻辑通过注入 `Resolver` 与 `http.RoundTripper` 用固化 HTML 断言，
`code_run` 的沙箱参数用注入的 `CommandRunner` 逐项断言，因此无需 Docker、无需外网。

## License

[MIT](LICENSE)
