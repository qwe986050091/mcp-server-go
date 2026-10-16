#!/usr/bin/env bash
# S6 审批门验收：挂载 confirm 策略的 MCP 技能 → 触发工具 → 观察 event: approval-request
# → 调 agent 的 /api/agent/approvals/{id}/decision 放行 → 观察工具真正执行。
#
# 用法：bash /tmp/e2e-approval.sh [userId] [skill] [conversationId]
set -uo pipefail

USER_ID="${1:-zlf}"
SKILL="${2:-mcp:gostdio}"
CONV="${3:-9003}"
OUT=/tmp/e2e-appr.out
REQ=/tmp/chat-appr.json
AGENT=http://127.0.0.1:8080

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

echo "--- 挂载 $SKILL（confirm 策略），后台发起对话 ---"
curl -sN -m 180 -X POST "$AGENT/api/agent/chat/stream" \
  -H "Content-Type: application/json" -H "X-Agent-Token: $TOKEN" \
  --data-binary @"$REQ" > "$OUT" 2>&1 &
CURL_PID=$!

APPROVAL_ID=""
for _ in $(seq 1 60); do
  APPROVAL_ID="$(grep -o '"approvalId":"[^"]*"' "$OUT" 2>/dev/null | head -1 | cut -d'"' -f4)"
  [ -n "$APPROVAL_ID" ] && break
  sleep 1
done

if [ -z "$APPROVAL_ID" ]; then
  echo "[FAIL] 60 秒内未出现 approval-request（审批门未触发）"
  echo "--- 事件计数 ---"; grep -o '^event: *[a-zA-Z-]*' "$OUT" | sort | uniq -c
  echo "--- 输出尾部 ---"; tail -c 800 "$OUT"
  wait "$CURL_PID" 2>/dev/null
  exit 1
fi

echo "[OK] 收到审批请求 approvalId=$APPROVAL_ID"
echo "--- approval-request 事件原文 ---"
grep -A1 '^event: *approval-request' "$OUT" | head -6

echo "--- 调 decision 接口放行 ---"
curl -s -m 10 -o /tmp/appr-decision.out -w 'http=%{http_code}\n' \
  -X POST "$AGENT/api/agent/approvals/$APPROVAL_ID/decision" \
  -H "Content-Type: application/json" -H "X-Agent-Token: $TOKEN" \
  --data "{\"userId\":\"$USER_ID\",\"approved\":true,\"remember\":false}"
cat /tmp/appr-decision.out; echo

wait "$CURL_PID" 2>/dev/null
echo "--- 事件类型计数 ---"
grep -o '^event: *[a-zA-Z-]*' "$OUT" | sort | uniq -c

echo "--- event: tool（放行后是否真的执行）---"
awk '/^event: *tool/{getline; print}' "$OUT" | head -c 700
echo
echo "--- 回答尾部 ---"
tail -c 300 "$OUT"
echo
echo "--- agent 侧日志 ---"
journalctl -u agent --since '3 min ago' --no-pager | grep -E '\[MCP\]|审批|pproval' | tail -12
rm -f "$OUT" "$REQ" /tmp/appr-decision.out
