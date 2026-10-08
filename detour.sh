#!/usr/bin/env bash
# detour.sh — detour 转发服务管理脚本（默认对接 opencode go）
#
# 用法:
#   ./detour.sh start     启动
#   ./detour.sh stop      停止
#   ./detour.sh restart   重启
#   ./detour.sh status    查看状态（pid / 生效配置 / 最近日志）
#   ./detour.sh check     体检代理链路（梯子→上游）
#   ./detour.sh log       跟踪日志
#   ./detour.sh build     重新编译二进制
#
# 配置来源（优先级从高到低）:
#   1. 环境变量 DETOUR_LISTEN / DETOUR_UPSTREAM / DETOUR_PROXY / DETOUR_CONFIG
#      （只有显式设置的那一项才会覆盖，未设置就交给下面两级）
#   2. 配置文件，按顺序找第一个存在的:
#        $DETOUR_CONFIG -> ~/.detour/detour.json -> 二进制同目录 -> 当前目录
#   3. 二进制内置默认值：127.0.0.1:8787 -> api.openai.com/v1 via 127.0.0.1:7897
#
# 想知道最终吃的是哪套配置: ./detour.sh status，或 detour -print-config

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$SCRIPT_DIR/detour"
PID_FILE="$SCRIPT_DIR/.detour.pid"
LOG_FILE="$SCRIPT_DIR/detour.log"

# 只把「显式设置」的环境变量传下去，未设置的一律不传 —— 否则脚本的内置默认值
# 会把 detour.json 覆盖掉（命令行参数优先级高于配置文件）。配置文件同理：不指定
# -config 时由二进制自己按 ~/.detour/detour.json -> 同目录 -> 当前目录 的顺序找。
ARGS=()
if [[ -n "${DETOUR_CONFIG:-}" ]]; then
  ARGS+=(-config "$DETOUR_CONFIG")
fi
[[ -n "${DETOUR_LISTEN:-}"   ]] && ARGS+=(-listen   "$DETOUR_LISTEN")
[[ -n "${DETOUR_UPSTREAM:-}" ]] && ARGS+=(-upstream "$DETOUR_UPSTREAM")
[[ -n "${DETOUR_PROXY:-}"    ]] && ARGS+=(-proxy    "$DETOUR_PROXY")

# effective <key> — 问二进制它最终生效的值（默认值 + detour.json + 环境变量）
effective() {
  "$BIN" ${ARGS[@]+"${ARGS[@]}"} -print-config 2>/dev/null |
    sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" || true
}

config_source() {
  local cfg
  cfg="$(effective config)"
  if [[ -n "$cfg" ]]; then
    echo "$cfg"
  else
    echo "(无 detour.json，用内置默认值)"
  fi
}

listen_addr() {
  local addr
  addr="$(effective listen)"
  echo "${addr:-127.0.0.1:8787}"
}

is_running() {
  [[ -f "$PID_FILE" ]] || return 1
  local pid
  pid="$(cat "$PID_FILE" 2>/dev/null || true)"
  [[ -n "$pid" ]] && ps -p "$pid" -o comm= 2>/dev/null | grep -q detour
}

port_in_use() {
  local port
  port="$(listen_addr | sed -E 's/.*://')"
  lsof -nP -iTCP:"$port" -sTCP:LISTEN &>/dev/null
}

ensure_bin() {
  if [[ ! -x "$BIN" ]]; then
    echo "==> 未找到 $BIN，先构建..."
    (cd "$SCRIPT_DIR" && go build -o detour .)
  fi
}

print_hint() {
  cat <<EOF

opencode 配置（~/.config/opencode/opencode.json）:
{
  "provider": {
    "opencode-go": {
      "options": {
        "baseURL": "http://$(listen_addr)"
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
    echo "detour 已在运行 (pid $(cat "$PID_FILE"))，监听 $(listen_addr)"
    return 0
  fi
  if port_in_use; then
    echo "错误: 端口 $(listen_addr) 已被占用。释放端口，或改 DETOUR_LISTEN / detour.json。" >&2
    return 1
  fi
  echo "==> 启动 detour"
  echo "    监听    $(listen_addr)"
  echo "    上游    $(effective upstream)"
  echo "    代理    $(effective proxy)"
  echo "    配置    $(config_source)"
  echo "    日志    $LOG_FILE"
  nohup "$BIN" ${ARGS[@]+"${ARGS[@]}"} >>"$LOG_FILE" 2>&1 &
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
  if ! is_running; then
    echo "detour 未在运行"
    return 1
  fi
  echo "detour 运行中 (pid $(cat "$PID_FILE"))"
  echo "监听: $(listen_addr)"
  echo "上游: $(effective upstream)"
  echo "代理: $(effective proxy)"
  echo "配置: $(config_source)"
  echo "日志:"
  tail -n 5 "$LOG_FILE" 2>/dev/null | sed 's/^/  /'
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
  check)   ensure_bin; "$BIN" ${ARGS[@]+"${ARGS[@]}"} -check ;;
  log)     exec tail -f "$LOG_FILE" ;;
  build)   (cd "$SCRIPT_DIR" && go build -o detour .) && echo "==> 构建完成: $BIN" ;;
  *)       echo "用法: $0 {start|stop|restart|status|check|log|build}" >&2; exit 1 ;;
esac
