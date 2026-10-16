#!/usr/bin/env bash
# S6 云端工具验收：直连 mcp-server-go（127.0.0.1:3001），按客户端真实报文打 tools/call。
# 用法：bash /tmp/verify-tools.sh [kb_user_id]
set -uo pipefail

URL="http://127.0.0.1:3001/mcp"
ACCEPT="application/json,text/event-stream"
KB_USER="${1:-zlf}"

call() {
  local name="$1" args="$2" limit="${3:-1800}"
  echo "===== tools/call $name ====="
  curl -s -m 45 -X POST "$URL" \
    -H "Content-Type: application/json" -H "Accept: $ACCEPT" \
    --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$name\",\"arguments\":$args}}" \
    | head -c "$limit"
  echo
  echo
}

# 1) kb_search：走 agent 的 /api/agent/rag/search（真实混合检索 + RRF + 重排）
call kb_search "{\"query\":\"项目收获与总结\",\"top_k\":3,\"user_id\":\"$KB_USER\"}"

# 2) code_run：新机未装 Docker，必须回可读 isError（绝不退化为宿主机直跑）
call code_run '{"language":"python","code":"print(sum(range(10)))"}'

# 3) calc：沙箱无关，纯计算
call calc '{"expression":"(1+2)*3^2"}'

# 4) web_search：云端网络，验证 Bing 免 Key 检索
call web_search '{"query":"golang mcp server 实现","count":3}'

# 5) web_fetch：真实抓取 + 正文抽取
call web_fetch '{"url":"https://golang.google.cn/"}' 900

# 6) SSRF：云元数据地址必须被拒
call web_fetch '{"url":"http://169.254.169.254/latest/meta-data/"}'
