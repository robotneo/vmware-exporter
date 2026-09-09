#!/usr/bin/env bash
#
# vmware-exporter 安装 / 升级脚本（systemd + YAML 配置）
#
# 用法：
#   sudo ./install.sh
#
# 行为：
#   - 全新安装：装二进制、service、config.yaml，enable 但**不 start**
#     （配置还没填，起来也是登录失败刷日志）
#   - 升级：只换二进制和 service，**保留现有 config.yaml**；
#     原本在运行的服务会自动重启
#
set -euo pipefail

BIN_NAME="vmware-exporter"

# DESTDIR 只为自动化测试而存在：把所有写入重定向到一个沙箱目录，
# 这样安装流程可以在没有 root、没有 systemd 的机器上被完整验证。
# 生产使用时不要设置它 —— 留空就是真实的系统路径。
DESTDIR="${DESTDIR:-}"

BIN_DST="${DESTDIR}/usr/bin/${BIN_NAME}"
CONF_DIR="${DESTDIR}/etc/vmware-exporter"
CONF_DST="${CONF_DIR}/config.yaml"
UNIT_DST="${DESTDIR}/etc/systemd/system/${BIN_NAME}.service"

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() { echo "错误：$*" >&2; exit 1; }
info() { echo "==> $*"; }

# ── 前置检查 ────────────────────────────────────────────────────────────

# DESTDIR 模式是测试沙箱，写的是普通目录，不需要 root。
if [[ -z "${DESTDIR}" ]]; then
  [[ ${EUID} -eq 0 ]] || die "需要 root 权限，请用 sudo 运行"
fi

# 文件属主只有真正装到系统路径时才有意义。sudo 下 install 的 -o/-g 才能生效。
#
# 用字符串而不是数组：bash 3.2（macOS 自带）在 set -u 下展开空数组会报
# unbound variable。这里的值是固定的字面量，不涉及带空格的路径，
# 不加引号直接展开是安全的。
OWNER=""
if [[ -z "${DESTDIR}" ]]; then
  OWNER="-o root -g root"
fi

arch="$(uname -m)"
[[ "${arch}" == "x86_64" ]] || die "本包是 linux/amd64 构建，当前架构是 ${arch}"

command -v systemctl >/dev/null 2>&1 || die "找不到 systemctl，本脚本只支持 systemd 系统"

for f in "${BIN_NAME}" "${BIN_NAME}.service" "config.yaml"; do
  [[ -f "${SRC_DIR}/${f}" ]] || die "包内缺少 ${f}，解压是否完整？"
done

# systemd 版本检查。service 里用了 DynamicUser=yes，需要 232+。
#
# 版本号解析不能用 awk '{print $2}'：真实世界里会遇到 252~rc1、
# 255.4-1ubuntu8、249.11-0ubuntu3.12 这些格式，取出来是脏值。
sd_ver="$(systemctl --version 2>/dev/null | head -1 | grep -oE '[0-9]+' | head -1 || true)"
if [[ -n "${sd_ver}" ]] && (( sd_ver < 232 )); then
  echo "警告：systemd ${sd_ver} 不支持 DynamicUser=yes（需要 232+）。" >&2
  echo "      安装后请手工编辑 ${UNIT_DST}：删掉 DynamicUser=yes，" >&2
  echo "      创建专用账号并改用 User=/Group=。详见 DEPLOY-zh.md。" >&2
fi

# ── 判断是全新安装还是升级 ──────────────────────────────────────────────

is_upgrade=0
if [[ -f "${BIN_DST}" ]]; then
  is_upgrade=1
fi

# 记录升级前的运行状态，决定装完要不要拉起来。
# 注意 `set -e` 下 is-active 返回非 0 会中断脚本，所以要 || true。
was_active=0
if [[ ${is_upgrade} -eq 1 ]]; then
  systemctl is-active --quiet "${BIN_NAME}" && was_active=1 || true
fi

if [[ ${is_upgrade} -eq 1 ]]; then
  info "检测到已安装，执行升级（保留 ${CONF_DST}）"
else
  info "全新安装"
fi

# ── 安装二进制 ──────────────────────────────────────────────────────────
#
# 先写 .new 再 mv：mv 在同一文件系统上是原子的，避免升级过程中
# 有人正好在执行这个文件导致 "Text file busy"。

info "安装二进制到 ${BIN_DST}"
install -d -m 0755 ${OWNER} "$(dirname "${BIN_DST}")"
install -m 0755 ${OWNER} "${SRC_DIR}/${BIN_NAME}" "${BIN_DST}.new"
mv -f "${BIN_DST}.new" "${BIN_DST}"

# ── 安装配置 ────────────────────────────────────────────────────────────

install -d -m 0755 ${OWNER} "${CONF_DIR}"

if [[ -f "${CONF_DST}" ]]; then
  info "保留已有配置 ${CONF_DST}"
  # 顺手修正权限。0600 会让 DynamicUser 起不来，这个坑值得每次都堵一下。
  chmod 0644 "${CONF_DST}"
  if [[ -z "${DESTDIR}" ]]; then
    chown root:root "${CONF_DST}"
  fi
  # 新版本可能加了配置项，把模板留在旁边供对照。
  install -m 0644 ${OWNER} "${SRC_DIR}/config.yaml" "${CONF_DST}.example"
  info "本版本的配置模板已放在 ${CONF_DST}.example"
else
  # 0644 而不是 0600：这个文件由 exporter 进程自己读，而 DynamicUser=yes
  # 分配的临时 uid 读不了 root:root 0600。
  install -m 0644 ${OWNER} "${SRC_DIR}/config.yaml" "${CONF_DST}"
  info "已写入配置模板 ${CONF_DST}"
fi

# ── 安装 unit ───────────────────────────────────────────────────────────

info "安装 systemd unit 到 ${UNIT_DST}"
install -d -m 0755 ${OWNER} "$(dirname "${UNIT_DST}")"
install -m 0644 ${OWNER} "${SRC_DIR}/${BIN_NAME}.service" "${UNIT_DST}"

systemctl daemon-reload

# ── 启动 ────────────────────────────────────────────────────────────────

if [[ ${is_upgrade} -eq 1 ]]; then
  systemctl enable --quiet "${BIN_NAME}" 2>/dev/null || true
  if [[ ${was_active} -eq 1 ]]; then
    info "重启服务"
    systemctl restart "${BIN_NAME}"
  else
    info "服务此前未运行，保持停止状态"
  fi
else
  info "设置开机自启"
  systemctl enable --quiet "${BIN_NAME}"
  echo
  echo "安装完成，但服务尚未启动 —— 配置里还是占位值。"
  echo
  echo "接下来："
  echo "  1. 编辑配置：sudo vi ${CONF_DST}"
  echo "     至少要改 vmware.vcenter / vmware.username / vmware.password"
  echo "  2. 启动：    sudo systemctl start ${BIN_NAME}"
  echo "  3. 看状态：  systemctl status ${BIN_NAME}"
  echo "  4. 看日志：  journalctl -u ${BIN_NAME} -f"
  echo "  5. 验证：    curl -s localhost:9169/metrics | head"
  echo
  echo "改完配置后用 sudo systemctl reload ${BIN_NAME} 即可生效，不用重启。"
  exit 0
fi

echo
info "完成。当前状态："
systemctl --no-pager --lines=0 status "${BIN_NAME}" || true
