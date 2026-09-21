#!/usr/bin/env bash
#
# 定时采集 vmware-exporter 的 CPU/goroutine/heap，用来判断资源占用是否
# 「随运行时间推移越来越高」。
#
# 用法：
#   ./scripts/pprof-trend.sh <base-url> [间隔秒] [每次CPU采样秒] [轮数]
#
# 示例（每 10 分钟采一次、每次抓 60s CPU、共采 12 轮 ≈ 2 小时）：
#   ./scripts/pprof-trend.sh http://172.17.40.25:9169 600 60 12
#
# 0 或省略「轮数」表示一直采，直到 Ctrl+C。
#
# 依赖：bash, curl, date。分析 CPU profile 才需要 `go tool pprof`（见结尾）。
#
set -euo pipefail

BASE="${1:?用法: $0 <base-url> [间隔秒] [cpu采样秒] [轮数]}"
INTERVAL="${2:-600}"
CPU_SECONDS="${3:-60}"
ROUNDS="${4:-0}"

# 采集结果放在运行脚本时所在目录下的 pprof-snapshots/。
OUT="$(pwd)/pprof-snapshots"
mkdir -p "${OUT}"

TREND="${OUT}/trend.csv"

# CSV 只在首次创建时写表头。
if [ ! -f "${TREND}" ]; then
  echo "timestamp,elapsed_min,goroutines,heap_inuse_bytes,heap_objects,cpu_profile" > "${TREND}"
fi

START_TS="$(date +%s)"

echo "==> 目标 ${BASE}"
echo "    间隔 ${INTERVAL}s | CPU 采样 ${CPU_SECONDS}s | 轮数 $([ "${ROUNDS}" -gt 0 ] && echo "${ROUNDS}" || echo 无限)"
echo "    结果目录 ${OUT}"
echo "    趋势 CSV ${TREND}"

n=0
while :; do
  n=$((n + 1))
  if [ "${ROUNDS}" -gt 0 ] && [ "${n}" -gt "${ROUNDS}" ]; then
    break
  fi

  NOW_TS="$(date +%s)"
  ELAPSED_MIN="$(( (NOW_TS - START_TS) / 60 ))"
  STAMP="$(date +%Y%m%d-%H%M%S)"

  echo ""
  echo "==> [第 ${n} 轮] $(date '+%F %T') 已运行约 ${ELAPSED_MIN} 分钟"

  # 1) goroutine 总数：首行形如 "goroutine profile: total 37"。
  #
  # 必须先把整个响应读进变量、再交给 awk，不能 curl | awk：awk 取到首行后
  # 就 exit 关闭管道，而 curl 还在写后面的 goroutine 栈，会触发
  # "curl: (23) Failure writing output to destination" 并在 pipefail 下中止。
  GOROUTINE_TXT="$(curl -fsS "${BASE}/debug/pprof/goroutine?debug=1")"
  GOROUTINES="$(printf '%s\n' "${GOROUTINE_TXT}" \
    | awk -F'total ' '/^goroutine profile:/{print $2}')"
  GOROUTINES="${GOROUTINES:-NA}"

  # 2) 堆：debug=1 文本形如 "# HeapInuse = 3514368"（4 个字段），数字取第 4 列。
  # awk 不提前 exit：让 printf 把整段写完，避免管道提前关闭。
  HEAP_TXT="$(curl -fsS "${BASE}/debug/pprof/heap?debug=1")"
  HEAP_INUSE="$(printf '%s\n' "${HEAP_TXT}" \
    | awk '/^# HeapInuse/{print $4}')"
  HEAP_OBJECTS="$(printf '%s\n' "${HEAP_TXT}" \
    | awk '/^# HeapObjects/{print $4}')"
  HEAP_INUSE="${HEAP_INUSE:-NA}"
  HEAP_OBJECTS="${HEAP_OBJECTS:-NA}"

  # 3) CPU profile：固定抓 CPU_SECONDS 秒，使各轮可横向对比。
  CPU_FILE="cpu-${STAMP}.pb.gz"
  if curl -fsS -o "${OUT}/${CPU_FILE}" \
      "${BASE}/debug/pprof/profile?seconds=${CPU_SECONDS}"; then
    echo "    CPU profile -> ${CPU_FILE} (${CPU_SECONDS}s)"
  else
    CPU_FILE="FAILED"
    echo "    [警告] CPU profile 抓取失败" >&2
  fi

  echo "${NOW_TS},${ELAPSED_MIN},${GOROUTINES},${HEAP_INUSE},${HEAP_OBJECTS},${CPU_FILE}" >> "${TREND}"
  echo "    goroutines=${GOROUTINES} heap_inuse=${HEAP_INUSE} heap_objects=${HEAP_OBJECTS}"

  # 最后一轮不再等待。
  if [ "${ROUNDS}" -gt 0 ] && [ "${n}" -ge "${ROUNDS}" ]; then
    break
  fi

  echo "    休眠 ${INTERVAL}s …"
  sleep "${INTERVAL}"
done

echo ""
echo "==> 采集结束。"
echo "    查看趋势（重点看 goroutines / heap_inuse 是否持续上升）："
echo "      column -s, -t < '${TREND}'"
echo ""
echo "    对比两轮 CPU profile 的总采样量（总采样≈该窗口烧的 CPU）："
echo "      go tool pprof -top -nodecount=10 '${OUT}/cpu-<时间戳A>.pb.gz'"
echo "      go tool pprof -top -nodecount=10 '${OUT}/cpu-<时间戳B>.pb.gz'"
echo "    或生成火焰图（在装有 go 的机器上）："
echo "      go tool pprof -http=:0 '${OUT}/cpu-<时间戳>.pb.gz'"
