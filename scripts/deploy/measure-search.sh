#!/usr/bin/env bash
# 测量 /api/agent/rag/search 的冷/热延迟（口令从 agent.service 现取，不回显）。
# 用法：bash /tmp/measure-search.sh [user] [runs]
set -uo pipefail

USER_ID="${1:-zlf}"
RUNS="${2:-4}"
TOKEN="$(systemctl show agent -p Environment --value | tr ' ' '\n' | sed -n 's/^AGENT_INTERNAL_TOKEN=//p' | tail -1)"
if [ -z "$TOKEN" ]; then echo "[FAIL] 取不到内部口令"; exit 1; fi

echo "--- 内存与 swap ---"
free -m | sed -n '1,3p'
echo "--- RAG 相关配置 ---"
systemctl show agent -p Environment --value | tr ' ' '\n' | grep -E '^RAG_(RERANK_ENABLED|STORE|RERANK_DIR)=' || true

echo "--- 连续 $RUNS 次 /search ---"
for i in $(seq 1 "$RUNS"); do
  body="{\"userId\":\"$USER_ID\",\"query\":\"项目收获\",\"topK\":3}"
  code_time=$(curl -s -m 60 -o /tmp/search-out.json -w '%{http_code} %{time_total}' \
    -X POST http://127.0.0.1:8080/api/agent/rag/search \
    -H "Content-Type: application/json" -H "X-Agent-Token: $TOKEN" \
    --data "$body")
  items=$(grep -o '"text"' /tmp/search-out.json | wc -l)
  head=$(head -c 120 /tmp/search-out.json)
  echo "run#$i http+t=$code_time items=$items body=$head"
done
rm -f /tmp/search-out.json
echo "--- agent RSS ---"
ps -o rss=,cmd= -C java | awk '{printf "RSS=%.0fMB\n", $1/1024}'
