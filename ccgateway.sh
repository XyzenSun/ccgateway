#!/usr/bin/env bash
set -euo pipefail

# ccgateway 管理脚本
# 用法:
#   直接运行:  ./ccgateway.sh --start [--password 密码] [--port 端口] [--dir 目录]
#              ./ccgateway.sh --stop
#              ./ccgateway.sh --restart [--password 密码] [--port 端口] [--dir 目录]
#   source 后: source ccgateway.sh; ccgateway start [端口] [密码]
# 环境变量:
#   CCGATEWAY_DIR - 工作目录 (默认: 脚本所在目录, 可通过 --dir 参数覆盖)
#   ADMIN_KEY     - 管理密钥 (默认: defaultpassword)
#   LISTEN_ADDR   - 监听地址 (默认: 0.0.0.0:端口或9999)
#   SQLITE_PATH   - 数据库路径 (默认: ./data/app.sqlite)

# ── 工作目录: 优先用 --dir 参数，其次用 CCGATEWAY_DIR 环境变量，最后回退到脚本所在目录 ──
WORK_DIR="${CCGATEWAY_DIR:-}"
BINARY="claude-code-gateway"
PIDFILE="data/ccgateway.pid"
LOGFILE="data/ccgateway.log"

# 解析全局选项
ACTION=""
PORT=""
PASSWORD=""
while [ $# -gt 0 ]; do
  case "$1" in
    --start|--stop|--restart|--status)
      ACTION="$1" ;;
    --password)
      shift; PASSWORD="${1:-}" ;;
    --port)
      shift; PORT="${1:-}" ;;
    --dir)
      shift; WORK_DIR="${1:-}" ;;
    *)
      # 兼容旧用法: --start 9999
      if [ -n "$ACTION" ] && [ -z "$PORT" ] && [[ "$1" =~ ^[0-9]+$ ]]; then
        PORT="$1"
      fi ;;
  esac
  shift
done

# 确定最终工作目录
if [ -z "$WORK_DIR" ]; then
  # 回退到脚本所在目录 (兼容直接运行和 source 两种方式)
  if [[ -n "${BASH_SOURCE[0]}" ]]; then
    WORK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  else
    WORK_DIR="$(pwd)"
  fi
fi

cd "$WORK_DIR"

do_start() {
  local port="${PORT:-9999}"
  if [ -n "$PASSWORD" ]; then
    export ADMIN_KEY="$PASSWORD"
  else
    export ADMIN_KEY="${ADMIN_KEY:-defaultpassword}"
  fi
  export LISTEN_ADDR="${LISTEN_ADDR:-0.0.0.0:$port}"
  export SQLITE_PATH="${SQLITE_PATH:-./data/app.sqlite}"

  if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
    echo "ccgateway 已在运行 (PID $(cat "$PIDFILE"))"
    echo "如需重启请使用 --restart"
    return 1
  fi

  if [ ! -f "$BINARY" ]; then
    echo "二进制文件不存在: $WORK_DIR/$BINARY"
    echo "请先编译: go build -o $BINARY ./cmd/claude-code-gateway"
    echo "或运行 install.sh 从 Release 下载"
    return 1
  fi

  mkdir -p "$(dirname "$SQLITE_PATH")"

  echo "启动 ccgateway..."
  nohup "./$BINARY" > "$LOGFILE" 2>&1 &
  echo $! > "$PIDFILE"

  sleep 1
  if kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
    echo "ccgateway 已启动"
    echo "  工作目录: $WORK_DIR"
    echo "  PID:      $(cat "$PIDFILE")"
    echo "  地址:     http://$LISTEN_ADDR"
    echo "  管理后台: http://$LISTEN_ADDR/admin"
    echo "  日志:     $WORK_DIR/$LOGFILE"
    echo "  数据库:   $WORK_DIR/$SQLITE_PATH"
  else
    echo "启动失败，查看日志:"
    tail -20 "$LOGFILE"
    return 1
  fi
}

do_stop() {
  if [ ! -f "$PIDFILE" ]; then
    PID=$(pgrep -f claude-code-gateway 2>/dev/null | head -1 || true)
    if [ -n "$PID" ]; then
      kill "$PID"
      echo "ccgateway 已停止 (PID $PID)"
    else
      echo "ccgateway 未在运行"
    fi
    return 0
  fi

  PID=$(cat "$PIDFILE")
  if kill -0 "$PID" 2>/dev/null; then
    kill "$PID"
    for _ in $(seq 1 10); do
      if ! kill -0 "$PID" 2>/dev/null; then
        break
      fi
      sleep 0.5
    done
    if kill -0 "$PID" 2>/dev/null; then
      echo "进程未响应，强制终止..."
      kill -9 "$PID" 2>/dev/null || true
    fi
    rm -f "$PIDFILE"
    echo "ccgateway 已停止 (PID $PID)"
  else
    rm -f "$PIDFILE"
    echo "ccgateway 未在运行 (PID $PID 已不存在)"
  fi
}

do_status() {
  if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
    echo "ccgateway 正在运行 (PID $(cat "$PIDFILE"))"
    echo "  工作目录: $WORK_DIR"
    echo "  地址:     http://${LISTEN_ADDR:-0.0.0.0:9999}"
  else
    echo "ccgateway 未在运行"
  fi
}

# ── source 快捷指令 ──
# 当脚本被 source 后，提供 ccgateway 函数作为快捷入口:
#   ccgateway start [端口] [密码]
#   ccgateway stop
#   ccgateway restart [端口] [密码]
#   ccgateway status
#   ccgateway dir [目录]   - 设置/查看工作目录
ccgateway() {
  local sub_action="$1"
  shift || true
  case "$sub_action" in
    start)
      PORT=""
      PASSWORD=""
      while [ $# -gt 0 ]; do
        if [[ "$1" =~ ^[0-9]+$ ]] && [ -z "$PORT" ]; then
          PORT="$1"
        elif [ -z "$PASSWORD" ]; then
          PASSWORD="$1"
        fi
        shift
      done
      ACTION="--start"
      do_start
      ;;
    stop)
      do_stop
      ;;
    restart)
      PORT=""
      PASSWORD=""
      while [ $# -gt 0 ]; do
        if [[ "$1" =~ ^[0-9]+$ ]] && [ -z "$PORT" ]; then
          PORT="$1"
        elif [ -z "$PASSWORD" ]; then
          PASSWORD="$1"
        fi
        shift
      done
      ACTION="--restart"
      do_stop
      sleep 1
      do_start
      ;;
    status)
      do_status
      ;;
    dir)
      if [ -n "${1:-}" ]; then
        WORK_DIR="$1"
        cd "$WORK_DIR"
        echo "工作目录已设置为: $WORK_DIR"
      else
        echo "当前工作目录: $WORK_DIR"
      fi
      ;;
    *)
      echo "用法: ccgateway start [端口] [密码]"
      echo "       ccgateway stop"
      echo "       ccgateway restart [端口] [密码]"
      echo "       ccgateway status"
      echo "       ccgateway dir [目录]"
      ;;
  esac
}

# ── 判断运行模式 ──
# 如果脚本被 source，只定义 ccgateway 函数，不执行任何动作
# 如果脚本被直接执行，按 ACTION 参数执行
if [[ -n "${BASH_SOURCE[0]}" ]] && [[ "${BASH_SOURCE[0]}" != "${0}" ]]; then
  # source 模式: 函数已定义，什么都不做
  return 0 2>/dev/null || true
fi

# ── 直接运行模式 ──
usage() {
  echo "用法: $0 --start [--password 密码] [--port 端口] [--dir 目录]"
  echo "       $0 --stop [--dir 目录]"
  echo "       $0 --restart [--password 密码] [--port 端口] [--dir 目录]"
  echo "       $0 --status [--dir 目录]"
  echo ""
  echo "source 模式:"
  echo "  source $0"
  echo "  ccgateway start [端口] [密码]"
  echo "  ccgateway stop"
  echo "  ccgateway status"
  echo "  ccgateway dir [目录]"
  echo ""
  echo "环境变量:"
  echo "  CCGATEWAY_DIR  - 工作目录 (优先级低于 --dir)"
  echo "  ADMIN_KEY      - 管理密钥 (默认: defaultpassword)"
  echo "  LISTEN_ADDR    - 监听地址 (默认: 0.0.0.0:端口)"
  echo "  SQLITE_PATH    - 数据库路径 (默认: ./data/app.sqlite)"
  exit 1
}

case "$ACTION" in
  --start)
    do_start
    ;;
  --stop)
    do_stop
    ;;
  --restart)
    do_stop
    sleep 1
    do_start
    ;;
  --status)
    do_status
    ;;
  *)
    usage
    ;;
esac