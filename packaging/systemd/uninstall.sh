#!/usr/bin/env bash
#
# vmware-exporter 卸载脚本（systemd + YAML 配置）
#
# 用法：
#   sudo ./uninstall.sh               # 保留配置
#   sudo ./uninstall.sh --purge        # 删配置（y/N 确认）
#   sudo ./uninstall.sh --purge -y     # 删配置（免确认）
#
# 可重复执行。
#
set -euo pipefail

BIN_NAME="vmware-exporter"

# 见 install.sh 里的说明：只为测试沙箱而存在，生产使用时留空。
DESTDIR="${DESTDIR:-}"

BIN_DST="${DESTDIR}/usr/bin/${BIN_NAME}"
CONF_DIR="${DESTDIR}/etc/vmware-exporter"
UNIT_DST="${DESTDIR}/etc/systemd/system/${BIN_NAME}.service"

die() { echo "错误：$*" >&2; exit 1; }
info() { echo "==> $*"; }
warn() { echo "警告：$*" >&2; }

if [[ -z "${DESTDIR}" ]]; then
  [[ ${EUID} -eq 0 ]] || die "需要 root 权限，请用 sudo 运行"
fi

purge=0
if [[ "${1:-}" == "--purge" ]]; then
  purge=1
  shift
  if [[ "${1:-}" != "-y" ]]; then
    echo
    echo "⚠️  将删除 ${CONF_DIR} 下的所有配置文件（包括 config.yaml）"
    echo "   确认删除？[y/N] "
    read -r confirm
    [[ "${confirm}" =~ ^[Yy]$ ]] || die "已取消"
  fi
fi

# ── 停止服务（如果运行中）─

if systemctl is-active --quiet "${BIN_NAME}" 2>/dev/null; then
  info "停止服务"
  systemctl stop "${BIN_NAME}"
fi

# ── 禁用开机自启 ─

if systemctl is-enabled --quiet "${BIN_NAME}" 2>/dev/null; then
  info "关闭开机自启"
  systemctl disable --quiet "${BIN_NAME}"
fi

# ── 删除文件 ─

found=0
if [[ -f "${UNIT_DST}" ]]; then
  rm -f "${UNIT_DST}"
  info "已删除 ${UNIT_DST}"
  found=1
fi

if [[ -f "${BIN_DST}" ]]; then
  rm -f "${BIN_DST}"
  info "已删除 ${BIN_DST}"
  found=1
fi

if [[ ${purge} -eq 1 ]] && [[ -d "${CONF_DIR}" ]]; then
  rm -rf "${CONF_DIR}"
  info "已删除配置目录 ${CONF_DIR}"
  found=1
fi

# ── 清理 systemd ─

systemctl daemon-reload 2>/dev/null || true
systemctl reset-failed "${BIN_NAME}" 2>/dev/null || true

if [[ ${found} -eq 0 ]]; then
  info "未发现已安装的 ${BIN_NAME}，无操作。"
  exit 0
fi

info "卸载完成。"