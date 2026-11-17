#!/usr/bin/env bash
# 安全换二进制重启 wb2api：预检 → 等空闲 → 原子替换 → 健康检查 → 失败自动回滚。
#
# 为什么需要它：这台机器的 wb2api 没有守护进程，硬杀之后没人拉起；而它同时是
# 本机 AI 对话的模型出口，杀错了会直接掐断正在进行的会话。所以：
#   1. 先在同目录的临时目录里用备用端口把候选二进制跑一遍（健康检查通过才继续）；
#   2. 等监听端口上没有已建立连接（默认连续 10 秒空闲）再动手，避免切正在跑的流；
#   3. 换完健康检查失败，立刻把旧二进制换回来并重新拉起，不会留下「起不来」的状态。
#
# 用法：
#   scripts/safe-restart.sh [候选二进制] [--now] [--check]
#   scripts/safe-restart.sh wb2api.next          # 等空闲再换
#   scripts/safe-restart.sh wb2api.next --now    # 立即换（会掐断正在进行的请求）
#   scripts/safe-restart.sh wb2api.next --check  # 只做预检，不碰线上进程
#
# 默认候选二进制是同目录的 wb2api.next；不给就只用现有 ./wb2api 重启一次。

set -uo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
CONFIG="$ROOT/config.json"
CANDIDATE=""
NOW=0
CHECK_ONLY=0
IDLE_SECS=${IDLE_SECS:-10}       # 连续空闲多少秒才算安全
MAX_WAIT=${MAX_WAIT:-300}        # 最多等多久空闲，超时就不动手
HEALTH_TIMEOUT=${HEALTH_TIMEOUT:-30}

for arg in "$@"; do
  case "$arg" in
    --now) NOW=1 ;;
    --check) CHECK_ONLY=1 ;;
    -h|--help) sed -n '2,24p' "$0"; exit 0 ;;
    *) CANDIDATE="$arg" ;;
  esac
done

log() { printf '[safe-restart] %s\n' "$*"; }
die() { log "失败：$*"; exit 1; }

[ -f "$CONFIG" ] || die "找不到配置文件 $CONFIG"

PORT=$(python3 - "$CONFIG" <<'PY' 2>/dev/null || echo 7863
import json,sys
c=json.load(open(sys.argv[1]))
print((c.get("listen") or "127.0.0.1:7863").split(":")[-1])
PY
)
BASE="http://127.0.0.1:${PORT}"
[ -n "${CANDIDATE:-}" ] || CANDIDATE="$ROOT/wb2api.next"
[ -n "${CANDIDATE:-}" ] && [ "$CANDIDATE" = "$ROOT/wb2api" ] && CANDIDATE=""
# 预检会切到临时目录里跑，相对路径必须先转成绝对路径
if [ -n "${CANDIDATE:-}" ]; then
  CANDIDATE=$(cd "$(dirname "$CANDIDATE")" 2>/dev/null && pwd)/$(basename "$CANDIDATE")
fi

healthy() { curl -fsS -m 3 "${1:-$BASE}/healthz" >/dev/null 2>&1; }

# 进程是否已经在提供服务。健康检查在「一个账号都不健康」时返回 503，
# 那仍然是「起来了」，不能用 200 当唯一判据。
serving() {
  local body
  body=$(curl -sS -m 3 "${1:-$BASE}/healthz" 2>/dev/null) || return 1
  case "$body" in *workbuddy2api*) return 0 ;; *) return 1 ;; esac
}

# ---------------------------------------------------------------------------
# 1. 预检：在临时目录里用备用端口把候选二进制跑一遍
# ---------------------------------------------------------------------------
preflight() {
  local bin="$1" tmp port pid i ok=1
  tmp=$(mktemp -d "${TMPDIR:-/tmp}/wb2api-preflight.XXXXXX") || return 1
  port=$(( PORT + 100 ))
  python3 - "$CONFIG" "$tmp/config.json" "$port" <<'PY' || { rm -rf "$tmp"; return 1; }
import json,sys
src,dst,port=sys.argv[1],sys.argv[2],int(sys.argv[3])
c=json.load(open(src))
# 预检只需要能起来：密钥与会话中转不进临时文件
c.pop("api_key",None); c.pop("upstash",None)
c["listen"]="127.0.0.1:%d"%port
c["auth_dir"]="%s/auths"%__import__("os").path.dirname(dst)
c["state_file"]="%s/state.json"%__import__("os").path.dirname(dst)
s=c.get("schedule")
if isinstance(s,dict):
    for k in list(s):
        if k.endswith("_enabled"): s[k]=False
json.dump(c,open(dst,"w"),ensure_ascii=False)
PY
  mkdir -p "$tmp/auths"
  # exec 让 $! 就是二进制本身，否则记到的是包装子进程，杀不干净
  ( cd "$tmp" && exec nohup "$bin" -config "$tmp/config.json" ) >"$tmp/out.log" 2>&1 &
  pid=$!
  for i in $(seq 1 20); do
    if serving "http://127.0.0.1:$port"; then ok=0; break; fi
    sleep 0.5
  done
  [ -n "$pid" ] && kill -TERM "$pid" 2>/dev/null
  sleep 0.5
  [ -n "$pid" ] && kill -KILL "$pid" 2>/dev/null
  if [ "$ok" -ne 0 ]; then
    log "候选二进制启动失败，日志末尾："
    tail -20 "$tmp/out.log" 2>/dev/null
  fi
  rm -rf "$tmp"
  return "$ok"
}

if [ -n "$CANDIDATE" ]; then
  [ -x "$CANDIDATE" ] || die "候选二进制不可执行：$CANDIDATE"
  log "预检候选二进制：$CANDIDATE"
  preflight "$CANDIDATE" || die "预检未通过，线上进程一个字节没动"
  log "预检通过（备用端口能起来）"
else
  log "未指定候选二进制，只重启现有 ./wb2api"
fi

[ "$CHECK_ONLY" -eq 1 ] && { log "只做预检，已结束"; exit 0; }

# ---------------------------------------------------------------------------
# 2. 等空闲：端口上没有已建立连接
# ---------------------------------------------------------------------------
if [ "$NOW" -eq 0 ]; then
  log "等待端口 $PORT 连续 ${IDLE_SECS}s 无连接（最多 ${MAX_WAIT}s）..."
  waited=0; idle=0
  while [ "$waited" -lt "$MAX_WAIT" ]; do
    n=$(lsof -nP -iTCP:"$PORT" -sTCP:ESTABLISHED 2>/dev/null | tail -n +2 | wc -l | tr -d ' ')
    if [ "${n:-0}" -eq 0 ]; then
      idle=$((idle + 2))
      [ "$idle" -ge "$IDLE_SECS" ] && break
    else
      idle=0
    fi
    sleep 2; waited=$((waited + 2))
  done
  if [ "$idle" -lt "$IDLE_SECS" ]; then
    die "等不到空闲（${MAX_WAIT}s 内一直有连接）。要强制换就加 --now"
  fi
  log "已空闲 ${IDLE_SECS}s，开始切换"
else
  log "--now：立即切换，正在进行的请求会被切断"
fi

# ---------------------------------------------------------------------------
# 3. 先装好新二进制（用 rename 换目录项：直接覆盖运行中的文件可能把进程写崩），
#    装不上就不碰线上进程；装好之后再停旧进程、起新的。
# ---------------------------------------------------------------------------
if [ -n "$CANDIDATE" ]; then
  cp -p "$ROOT/wb2api" "$ROOT/wb2api.rollback" || die "备份当前二进制失败，线上进程一个字节没动"
  if cmp -s "$CANDIDATE" "$ROOT/wb2api"; then
    log "候选与当前二进制一致，只重启不替换"
  else
    cp -p "$CANDIDATE" "$ROOT/wb2api.installing" || die "拷贝候选二进制失败，线上进程一个字节没动"
    chmod +x "$ROOT/wb2api.installing"
    mv -f "$ROOT/wb2api.installing" "$ROOT/wb2api" || die "安装新二进制失败，线上进程一个字节没动"
    log "已换上新二进制（旧的存为 wb2api.rollback）"
  fi
fi

OLD_PID=$(lsof -nP -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | awk 'NR==2{print $2}')
if [ -n "${OLD_PID:-}" ]; then
  log "停止旧进程 PID $OLD_PID"
  kill -TERM "$OLD_PID" 2>/dev/null
  for i in $(seq 1 30); do kill -0 "$OLD_PID" 2>/dev/null || break; sleep 0.5; done
  kill -0 "$OLD_PID" 2>/dev/null && { log "旧进程不退，用 KILL"; kill -KILL "$OLD_PID" 2>/dev/null; sleep 0.5; }
fi

start_and_wait() {
  local bin="$1"
  ( cd "$ROOT" && exec nohup "$bin" -config "$CONFIG" ) >>"$ROOT/logs/wb2api.log" 2>&1 &
  for i in $(seq 1 "$HEALTH_TIMEOUT"); do
    serving && return 0
    sleep 1
  done
  return 1
}

mkdir -p "$ROOT/logs"
log "启动..."
if start_and_wait "$ROOT/wb2api"; then
  NEW_PID=$(lsof -nP -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | awk 'NR==2{print $2}')
  log "成功：监听 $PORT，PID ${NEW_PID:-?}，健康检查通过"
  exit 0
fi

# ---------------------------------------------------------------------------
# 4. 回滚
# ---------------------------------------------------------------------------
log "新二进制 ${HEALTH_TIMEOUT}s 内没通过健康检查，开始回滚"
if [ -x "$ROOT/wb2api.rollback" ]; then
  P=$(lsof -nP -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | awk 'NR==2{print $2}')
  [ -n "${P:-}" ] && kill -KILL "$P" 2>/dev/null
  cp -p "$ROOT/wb2api.rollback" "$ROOT/wb2api"
  chmod +x "$ROOT/wb2api"
  if start_and_wait "$ROOT/wb2api"; then
    log "已回滚到旧二进制并恢复服务，PID $(lsof -nP -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | awk 'NR==2{print $2}')"
    exit 1
  fi
  log "回滚也起不来，需要人工处理：cd $ROOT && ./wb2api -config $CONFIG"
  exit 2
fi
log "没有可用的 wb2api.rollback，需要人工处理：cd $ROOT && ./wb2api -config $CONFIG"
exit 2
