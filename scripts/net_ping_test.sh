#!/usr/bin/env bash
#
# NET-005 网络稳定性测试 - 持续 ping 指定 IP 并统计丢包率
#
# 用法:
#   ./net_ping_test.sh <IP地址> [时长秒数] [间隔秒数]
#
# 示例:
#   ./net_ping_test.sh 192.168.42.1
#   ./net_ping_test.sh 192.168.42.1 300
#   ./net_ping_test.sh 192.168.42.1 300 1
#   ./net_ping_test.sh 8.8.8.8 600 0.5
#

set -eo pipefail  # 移除 u，避免未定义变量严格检查导致的意外退出

# ---- 默认配置 ----
DEFAULT_DURATION=300        # 默认测试 5 分钟
DEFAULT_INTERVAL=1          # 默认 1 秒间隔
TIMEOUT=2                   # ping 超时 2 秒
LOG_DIR="./ping_logs"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)

# 检测系统（macOS ping 的 -W 和 Linux 不同）
if [[ "$(uname)" == "Darwin" ]]; then
    PING_TIMEOUT_OPT="-t"  # macOS 用 -t (TTL timeout)
else
    PING_TIMEOUT_OPT="-W"  # Linux 用 -W (wait time)
fi

# ---- 日志函数 ----
log() {
    local msg="$1"
    echo "[$(date +'%Y-%m-%d %H:%M:%S')] $msg"
}

log_target() {
    local msg="$1"
    echo "[$(date +'%Y-%m-%d %H:%M:%S')] TARGET: $msg"
}

# ---- 参数解析 ----
if [ $# -lt 1 ]; then
    echo "错误: 缺少 IP 地址参数" >&2
    echo "" >&2
    echo "用法: $0 <IP地址> [时长秒数] [间隔秒数]" >&2
    echo "" >&2
    echo "参数说明:" >&2
    echo "  IP地址       必需，目标 IP 地址" >&2
    echo "  时长秒数     可选，测试持续时间（默认 300 秒）" >&2
    echo "  间隔秒数     可选，ping 间隔（默认 1 秒）" >&2
    echo "" >&2
    echo "示例:" >&2
    echo "  $0 192.168.42.1" >&2
    echo "  $0 192.168.42.1 600" >&2
    echo "  $0 8.8.8.8 300 0.5" >&2
    exit 1
fi

TARGET_IP="$1"
DURATION="${2:-$DEFAULT_DURATION}"
INTERVAL="${3:-$DEFAULT_INTERVAL}"

# 验证 IP 格式（简单检查）
if ! [[ "$TARGET_IP" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "错误: 无效的 IP 地址格式: $TARGET_IP" >&2
    exit 1
fi

# 创建日志目录
mkdir -p "$LOG_DIR"
LOG_FILE="$LOG_DIR/ping_${TARGET_IP}_${TIMESTAMP}.log"

# ---- 显示测试配置 ----
log "========================================"
log "网络稳定性测试开始"
log "========================================"
log "目标 IP:        $TARGET_IP"
log "测试时长:       $DURATION 秒"
log "Ping 间隔:      $INTERVAL 秒"
log "超时设置:       $TIMEOUT 秒"
log "日志文件:       $LOG_FILE"
log "========================================"
log ""

# 记录配置到日志文件
{
    echo "========================================"
    echo "测试配置"
    echo "========================================"
    echo "目标 IP:       $TARGET_IP"
    echo "测试时长:      $DURATION 秒"
    echo "Ping 间隔:     $INTERVAL 秒"
    echo "超时:          $TIMEOUT 秒"
    echo "开始时间:      $(date +'%Y-%m-%d %H:%M:%S')"
    echo "========================================"
    echo ""
} > "$LOG_FILE"

# ---- 开始 ping 测试 ----
START_TIME=$(date +%s)
END_TIME=$((START_TIME + DURATION))
PING_COUNT=0
SUCCESS_COUNT=0
FAIL_COUNT=0

log "开始 ping $TARGET_IP..."
echo ""

while [ "$(date +%s)" -lt "$END_TIME" ]; do
    PING_COUNT=$((PING_COUNT + 1))

    # 执行一次 ping，捕获输出（set +e 避免 ping 失败退出）
    set +e
    PING_OUT=$(ping -c 1 "$PING_TIMEOUT_OPT" "$TIMEOUT" "$TARGET_IP" 2>/dev/null)
    PING_RC=$?
    set -e

    if [ "$PING_RC" -eq 0 ]; then
        SUCCESS_COUNT=$((SUCCESS_COUNT + 1))
        # 兼容 Linux/macOS ping 格式：time=12.3 或 time=12.3 ms
        RTT=$(echo "$PING_OUT" | grep -Eo 'time=[0-9.]+' | cut -d= -f2 || echo "?")
        log_target "[$PING_COUNT] ✓ 成功 - RTT=${RTT} ms" | tee -a "$LOG_FILE"
    else
        FAIL_COUNT=$((FAIL_COUNT + 1))
        log_target "[$PING_COUNT] ✗ 失败 - 超时或不可达" | tee -a "$LOG_FILE"
    fi

    # 每 10 次显示一次统计
    if [ $((PING_COUNT % 10)) -eq 0 ]; then
        CURRENT_LOSS=$(awk "BEGIN {printf \"%.2f\", ($FAIL_COUNT / $PING_COUNT) * 100}")
        log "进度: $PING_COUNT 次 | 成功: $SUCCESS_COUNT | 失败: $FAIL_COUNT | 丢包率: ${CURRENT_LOSS}%"
    fi

    # 等待间隔
    sleep "$INTERVAL"
done

# ---- 计算最终统计 ----
ACTUAL_END=$(date +%s)
ACTUAL_DURATION=$((ACTUAL_END - START_TIME))
LOSS_RATE=$(awk "BEGIN {printf \"%.2f\", ($FAIL_COUNT / $PING_COUNT) * 100}")
SUCCESS_RATE=$(awk "BEGIN {printf \"%.2f\", ($SUCCESS_COUNT / $PING_COUNT) * 100}")

# ---- 输出测试结果 ----
echo ""
log "========================================"
log "测试完成"
log "========================================"
log "目标 IP:        $TARGET_IP"
log "实际测试时长:   $ACTUAL_DURATION 秒"
log "总 Ping 次数:   $PING_COUNT"
log "成功次数:       $SUCCESS_COUNT (${SUCCESS_RATE}%)"
log "失败次数:       $FAIL_COUNT (${LOSS_RATE}%)"
log "丢包率:         ${LOSS_RATE}%"
log "结束时间:       $(date +'%Y-%m-%d %H:%M:%S')"
log "========================================"

# 记录最终统计到日志文件
{
    echo ""
    echo "========================================"
    echo "测试结果"
    echo "========================================"
    echo "实际测试时长:  $ACTUAL_DURATION 秒"
    echo "总 Ping 次数:  $PING_COUNT"
    echo "成功次数:      $SUCCESS_COUNT (${SUCCESS_RATE}%)"
    echo "失败次数:      $FAIL_COUNT (${LOSS_RATE}%)"
    echo "丢包率:        ${LOSS_RATE}%"
    echo "结束时间:      $(date +'%Y-%m-%d %H:%M:%S')"
    echo "========================================"
} >> "$LOG_FILE"

# ---- 测试判定 ----
echo ""
if [ "$FAIL_COUNT" -eq 0 ]; then
    log "✅ 测试通过: 网络连接完美，无丢包"
    echo "✅ 测试通过: 网络连接完美，无丢包" >> "$LOG_FILE"
    EXIT_CODE=0
elif awk "BEGIN { exit !($LOSS_RATE <= 5.0) }"; then
    log "✅ 测试通过: 丢包率 ${LOSS_RATE}% ≤ 5%"
    echo "✅ 测试通过: 丢包率 ${LOSS_RATE}% ≤ 5%" >> "$LOG_FILE"
    EXIT_CODE=0
else
    log "❌ 测试失败: 丢包率 ${LOSS_RATE}% > 5%"
    echo "❌ 测试失败: 丢包率 ${LOSS_RATE}% > 5%" >> "$LOG_FILE"
    EXIT_CODE=1
fi

log "日志文件: $LOG_FILE"

exit $EXIT_CODE
