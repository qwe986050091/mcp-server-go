#!/usr/bin/env bash
# 后端就绪轮询 + MCP 技能目录落库校验（口令从 agent-backend.service 现取，不回显）。
set -uo pipefail

echo "--- 等待后端就绪 ---"
for i in $(seq 1 12); do
  c=$(curl -s -m 5 -o /dev/null -w '%{http_code}' http://127.0.0.1:8081/api/health)
  echo "try$i backend_health=$c"
  if [ "$c" = "200" ]; then break; fi
  sleep 5
done
echo "is-active=$(systemctl is-active agent-backend)"
echo "NRestarts=$(systemctl show agent-backend -p NRestarts --value)"

echo "--- 技能目录同步日志 ---"
journalctl -u agent-backend --since '5 min ago' --no-pager | grep -iE 'SkillSync|内置 Skill|mcp:' | tail -12

echo "--- skill 表中 mcp:* 行（agent 目录同步结果）---"
ENVV="$(systemctl show agent-backend -p Environment --value)"
MU="$(printf '%s\n' "$ENVV" | tr ' ' '\n' | sed -n 's/^MYSQL_USER=//p' | tail -1)"
MP="$(printf '%s\n' "$ENVV" | tr ' ' '\n' | sed -n 's/^MYSQL_PASSWORD=//p' | tail -1)"
if [ -z "$MU" ] || [ -z "$MP" ]; then echo "[WARN] 取不到 MySQL 凭据，跳过 SQL 校验"; exit 0; fi
mysql -N -B -u "$MU" -p"$MP" agent_chat \
  -e "select name, builtin, short_description from skill where name like 'mcp:%' or name in ('web','http-api','code-runner','calculator','datetime') order by builtin desc, name" 2>/dev/null \
  || echo "[WARN] SQL 校验失败"
echo "--- user_skill 挂载关系（当前无 mcp 挂载则应只有既有行）---"
mysql -N -B -u "$MU" -p"$MP" agent_chat \
  -e "select count(*) as mounts_total from user_skill" 2>/dev/null || true
