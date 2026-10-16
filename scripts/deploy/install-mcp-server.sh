#!/usr/bin/env bash
# 安装/升级 mcp-server-go（Streamable HTTP，127.0.0.1:3001）为 systemd 常驻服务。
#
# 幂等：可重复执行，每次都会用当前二进制与最新 token 重写 unit 并重启。
#
# 前置：
#   /tmp/mcp-server-go-linux-amd64 已上传（scp）
# 用法：
#   bash /tmp/install-mcp-server.sh
#
# 安全：AGENT_INTERNAL_TOKEN 从 agent.service 现取现写，全脚本不打印其值。

set -euo pipefail

BIN_SRC="/tmp/mcp-server-go-linux-amd64"
BIN_DST="/opt/agent/mcp-server-go"
UNIT="/etc/systemd/system/mcp-server-go.service"
SVC="mcp-server-go"

if [ ! -f "$BIN_SRC" ]; then
  echo "[FAIL] 未找到 $BIN_SRC，请先 scp 上传"
  exit 1
fi

# 1) 取内部口令（只取不打印）
TOKEN="$(systemctl show agent -p Environment --value | tr ' ' '\n' | sed -n 's/^AGENT_INTERNAL_TOKEN=//p' | tail -1)"
if [ -z "$TOKEN" ]; then
  echo "[FAIL] 未能从 agent.service 取到 AGENT_INTERNAL_TOKEN"
  exit 1
fi
echo "[OK] 已从 agent.service 取得内部口令（长度 ${#TOKEN}，不回显）"

# 2) 安装二进制：先落临时名再原子替换，避免覆盖运行中的可执行文件失败
install -m 0755 -o agent -g agent "$BIN_SRC" "$BIN_DST.new"
mv -f "$BIN_DST.new" "$BIN_DST"
chown agent:agent "$BIN_DST"
echo "[OK] 二进制已安装: $BIN_DST ($(stat -c '%s' "$BIN_DST") 字节)"

# 3) 写 unit（含口令，文件权限 600）
install -d -m 0755 /etc/systemd/system
umask 077
cat > "$UNIT" <<EOF
[Unit]
Description=Go MCP Server (mcp-server-go, Streamable HTTP)
Documentation=https://github.com/qwe986050091/mcp-server-go
After=network.target

[Service]
Type=simple
User=agent
Group=agent
WorkingDirectory=/opt/agent
Environment=AGENT_INTERNAL_TOKEN=$TOKEN
ExecStart=$BIN_DST --transport http --addr 127.0.0.1:3001 --path /mcp
Restart=on-failure
RestartSec=3
# 内存硬上限：本机内存紧张，MCP Server 实际只需十几 MB
MemoryHigh=64M
MemoryMax=128M
# http 模式下 stdout 不应该有内容；显式丢弃，日志只走 journal（stderr）
StandardOutput=null
StandardError=journal
SyslogIdentifier=mcp-server-go

[Install]
WantedBy=multi-user.target
EOF
chmod 600 "$UNIT"
echo "[OK] unit 已写入: $UNIT (权限 $(stat -c '%a' "$UNIT"))"

# 4) 启动并自检
systemctl daemon-reload
systemctl enable "$SVC" >/dev/null 2>&1
systemctl restart "$SVC"
sleep 2

echo "--- 服务状态 ---"
systemctl is-active "$SVC" || true
systemctl show "$SVC" -p NRestarts --value | sed 's/^/NRestarts=/'
echo "--- 监听 ---"
ss -ltnp | grep 3001 || echo "未监听 3001"
echo "--- 协议自检（initialize + tools/list，带客户端真实头）---"
ACCEPT="application/json,text/event-stream"
INIT_BODY='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"install-check","version":"1.0.0"}}}'
curl -s -m 5 -D /tmp/mcp-h.txt -o /tmp/mcp-b1.txt \
  -X POST http://127.0.0.1:3001/mcp \
  -H "Content-Type: application/json" -H "Accept: $ACCEPT" \
  --data "$INIT_BODY"
echo "HTTP: $(head -1 /tmp/mcp-h.txt | tr -d '\r')"
echo "会话头: $(grep -i '^Mcp-Session-Id:' /tmp/mcp-h.txt | tr -d '\r' | cut -c1-30)..."
echo "initialize: $(cat /tmp/mcp-b1.txt)"
echo "tools/list: $(curl -s -m 5 -X POST http://127.0.0.1:3001/mcp -H 'Content-Type: application/json' -H "Accept: $ACCEPT" --data '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' | head -c 400)"
echo "GET(应 405): $(curl -s -m 5 -o /dev/null -w '%{http_code}' -H 'Accept: text/event-stream' http://127.0.0.1:3001/mcp)"
rm -f /tmp/mcp-h.txt /tmp/mcp-b1.txt
echo "--- 内存占用 ---"
ps -o rss=,cmd= -C mcp-server-go | awk '{printf "RSS=%.1fMB %s\n", $1/1024, $2}'
