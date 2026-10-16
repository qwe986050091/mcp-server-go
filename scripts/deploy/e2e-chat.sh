#!/usr/bin/env bash
# S6 端到端：直连 agent 的 /api/agent/chat/stream（带 X-Agent-Token），挂载指定 MCP 技能，
# 观察 1) McpClientManager 的懒连接日志 2) SSE event: tool 的真实工具调用。
#
# 用法：bash /tmp/e2e-chat.sh [userId] [skill] [conversationId]
set -uo pipefail

USER_ID="${1:-zlf}"
SKILL="${2:-mcp:goweb}"
CONV="${3:-9001}"
OUT=/tmp/e2e-chat.out
REQ=/tmp/chat-req.json

TOKEN="$(systemctl show agent -p Environment --value | tr ' ' '\n' | sed -n 's/^AGENT_INTERNAL_TOKEN=//p' | tail -1)"
if [ -z "$TOKEN" ]; then echo "[FAIL] 取不到内部口令"; exit 1; fi

cat > "$REQ" <<JSON
{
  "userId": "$USER_ID",
  "conversationId": $CONV,
  "message": "请调用知识库检索工具检索「项目收获与总结」，然后用一句话说明命中的来源文件名，不要联网搜索。",
  "skills": [{"name": "$SKILL"}]
}
JSON

echo "--- 请求体（不含口令）---"
cat "$REQ"
echo "--- 开始 chat/stream（挂载 skill=$SKILL）---"
curl -sN -m 180 -X POST http://127.0.0.1:8080/api/agent/chat/stream \
  -H "Content-Type: application/json" -H "X-Agent-Token: $TOKEN" \
  --data-binary @"$REQ" > "$OUT"
echo "curl exit=$?  输出字节=$(wc -c < "$OUT")"

echo "--- SSE 事件类型计数 ---"
grep -o '^event: *[a-zA-Z-]*' "$OUT" | sort | uniq -c

echo "--- 工具调用相关（event: tool / approval-request 的数据行）---"
awk '/^event: *(tool|approval-request)/{print "\n[" $0 "]"; getline; print}' "$OUT" | head -c 1600
echo

echo "--- 回答片段（最后 500 字节）---"
tail -c 500 "$OUT"
echo

echo "--- agent 侧 MCP 连接日志（近 3 分钟）---"
journalctl -u agent --since '3 min ago' --no-pager | grep -E '\[MCP\]|\[RAG\] 独立检索|工具' | tail -20

# 收尾：不留临时文件（排查时可临时注释掉本行以保留 $OUT 原文）
rm -f "$OUT" "$REQ"
