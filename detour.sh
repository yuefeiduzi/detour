#!/usr/bin/env bash
# detour.sh — detour 转发服务管理脚本（默认对接 opencode go）
#
# 用法:
#   ./detour.sh start     启动（默认: 127.0.0.1:8787 -> https://opencode.ai/zen/go/v1/ via 127.0.0.1:7897）
#   ./detour.sh stop      停止
#   ./detour.sh restart   重启
#   ./detour.sh status    查看状态
#   ./detour.sh check     体检代理链路（调用 detour -check）
#   ./detour.sh log       跟踪日志
#   ./detour.sh build     重新编译二进制
#
# 环境变量覆盖（可选）:
#   DETOUR_LISTEN   监听地址,   默认 127.0.0.1:8787
#   DETOUR_UPSTREAM 真实上游,   默认 https://opencode.ai/zen/go/v1/
#   DETOUR_PROXY    梯子代理,   默认 http://127.0.0.1:7897

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$SCRIPT_DIR/detour"
PID_FILE="$SCRIPT_DIR/.detour.pid"
LOG_FILE="$SCRIPT_DIR/detour.log"

DETOUR_LISTEN="${DETOUR_LISTEN:-127.0.0.1:8787}"
DETOUR_UPSTREAM="${DETOUR_UPSTREAM:-https://opencode.ai/zen/go/v1/}"
DETOUR_PROXY="${DETOUR_PROXY:-http://127.0.0.1:7897}"

is_running() {
  [[ -f "$PID_FILE" ]] || return 1
  local pid
  pid="$(cat "$PID_FILE" 2>/dev/null || true)"
  [[ -n "$pid" ]] && ps -p "$pid" -o comm= 2>/dev/null | grep -q detour
}

port_in_use() {
  local port
  port="$(printf '%s' "$DETOUR_LISTEN" | sed -E 's/.*://')"
  lsof -nP -iTCP:"$port" -sTCP:LISTEN &>/dev/null
}

ensure_bin() {
  if [[ ! -x "$BIN" ]]; then
    echo "==> 未找到 $BIN，先构建..."
    (cd "$SCRIPT_DIR" && go build -o detour .)
  fi
}

print_hint() {
  cat <<'EOF'

opencode 配置（~/.config/opencode/opencode.json）:
{
  "provider": {
    "opencode-go": {
      "options": {
        "baseURL": "http://127.0.0.1:8787"
      }
    }
  }
}

如果之前用 /connect 登录过 opencode go，key 仍然生效；否则在 options 里加
"apiKey": "你的 OPENCODE_API_KEY"。
EOF
}

cmd_start() {
  ensure_bin
  if is_running; then
    echo "detour 已在运行 (pid $(cat "$PID_FILE"))，监听 $DETOUR_LISTEN"
    return 0
  fi
  if port_in_use; then
    echo "错误: 端口 $DETOUR_LISTEN 已被占用。释放端口，或用 DETOUR_LISTEN 换一个。" >&2
    return 1
  fi
  echo "==> 启动 detour"
  echo "    监听    $DETOUR_LISTEN"
  echo "    上游    $DETOUR_UPSTREAM"
  echo "    代理    $DETOUR_PROXY"
  echo "    日志    $LOG_FILE"
  nohup "$BIN" -listen "$DETOUR_LISTEN" -upstream "$DETOUR_UPSTREAM" -proxy "$DETOUR_PROXY" >>"$LOG_FILE" 2>&1 &
  echo $! >"$PID_FILE"
  sleep 0.5
  if is_running; then
    echo "==> 已启动 (pid $(cat "$PID_FILE"))"
    print_hint
  else
    echo "启动失败，看日志: $LOG_FILE" >&2
    return 1
  fi
}

cmd_stop() {
  if ! is_running; then
    echo "detour 未在运行"
    rm -f "$PID_FILE"
    return 0
  fi
  local pid
  pid="$(cat "$PID_FILE")"
  echo "==> 停止 detour (pid $pid)"
  kill "$pid"
  for _ in $(seq 1 20); do
    is_running || break
    sleep 0.1
  done
  if is_running; then
    echo "未退出，强制 kill" >&2
    kill -9 "$pid"
  fi
  rm -f "$PID_FILE"
  echo "==> 已停止"
}

cmd_status() {
  if is_running; then
    echo "detour 运行中 (pid $(cat "$PID_FILE"))，监听 $DETOUR_LISTEN"
    echo "上游: $DETOUR_UPSTREAM"
    tail -n 5 "$LOG_FILE" 2>/dev/null | sed 's/^/  /'
  else
    echo "detour 未在运行"
    return 1
  fi
}

cmd_restart() {
  cmd_stop
  cmd_start
}

case "${1:-start}" in
  start)   cmd_start ;;
  stop)    cmd_stop ;;
  restart) cmd_restart ;;
  status)  cmd_status ;;
  check)   ensure_bin; "$BIN" -listen "$DETOUR_LISTEN" -upstream "$DETOUR_UPSTREAM" -proxy "$DETOUR_PROXY" -check ;;
  log)     exec tail -f "$LOG_FILE" ;;
  build)   (cd "$SCRIPT_DIR" && go build -o detour .) && echo "==> 构建完成: $BIN" ;;
  *)       echo "用法: $0 {start|stop|restart|status|check|log|build}" >&2; exit 1 ;;
esac
