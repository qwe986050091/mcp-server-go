#!/usr/bin/env bash
# S6 收尾巡检：各服务状态 + 健康码 + 资源占用 + Go MCP Server 协议自检，并清理 /tmp 上传物。
set -uo pipefail

echo "=== 服务状态 ==="
for svc in agent agent-backend mcp-server-go postgresql prometheus updates-server; do
  printf '%-18s %-10s NRestarts=%s\n' "$svc" "$(systemctl is-active "$svc")" "$(systemctl show "$svc" -p NRestarts --value)"
done

echo "=== 健康检查 ==="
echo "agent        $(curl -s -m 5 -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/api/agent/health)"
echo "agent-backend $(curl -s -m 5 -o /dev/null -w '%{http_code}' http://127.0.0.1:8081/api/health)"
echo "mcp initialize $(curl -s -m 5 -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:3001/mcp -H 'Content-Type: application/json' -H 'Accept: application/json,text/event-stream' --data '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"final","version":"1"}}}')"
echo "updates      $(curl -s -m 5 -o /dev/null -w '%{http_code}' -H 'expo-protocol-version: 1' http://127.0.0.1:8082/updates)"

echo "=== 资源 ==="
free -m | sed -n '1,3p'
df -h / | sed -n '2p'
echo "mcp-server-go RSS: $(ps -o rss= -C mcp-server-go | awk '{printf "%.1fMB", $1/1024}')"
echo "go 二进制/unit: $(stat -c '%s bytes' /opt/agent/mcp-server-go) / $(stat -c '%a' /etc/systemd/system/mcp-server-go.service)"
echo "agent 备份数: $(ls /opt/agent/agent.jar.bak-* 2>/dev/null | wc -l)"

echo "=== 清理 /tmp 上传物 ==="
rm -f /tmp/agent-new.jar /tmp/deploy-jars.sh /tmp/verify-tools.sh /tmp/install-mcp-server.sh \
      /tmp/measure-search.sh /tmp/e2e-chat.sh /tmp/e2e-approval.sh /tmp/check-backend-sync.sh \
      /tmp/mcp-server-go-linux-amd64 /tmp/recon-rag.sql
echo "剩余 /tmp 相关文件: $(ls /tmp 2>/dev/null | grep -Ei 'mcp|agent|deploy|e2e|recon|verify|install|measure|check' | wc -l) 个"
