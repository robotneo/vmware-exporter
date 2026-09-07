#!/usr/bin/env bash
#
# 构建 vmware-exporter systemd 部署包
#
# 用法：./scripts/build-systemd-pkg.sh
# 产出：dist/vmware-exporter-<version>-linux-amd64-systemd.tar.gz + .sha256
#
# 依赖：bash, go, git, tar, sha256sum（或 shasum -a 256）
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="${ROOT}/dist"
PKG_DIR="${ROOT}/packaging/systemd"
VER="$(cd "${ROOT}" && git describe --tags --always --dirty 2>/dev/null || echo "unknown")"
ARCHIVE_NAME="vmware-exporter-${VER}-linux-amd64-systemd"
OUTPUT="${DIST}/${ARCHIVE_NAME}.tar.gz"

mkdir -p "${DIST}"

# 1. 构建 linux/amd64 二进制
echo "==> 构建 linux/amd64 二进制 (version=${VER})"
cd "${ROOT}"

BUILD_USER_WHOM="${SUDO_USER:-${USER:-unknown}}"
BUILD_DATE="$(date -u +'%Y-%m-%dT%H:%M:%S%z')"

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath \
  -ldflags="
    -s -w
    -X github.com/prometheus/common/version.Version=${VER}
    -X github.com/prometheus/common/version.Revision=$(git rev-parse HEAD 2>/dev/null || echo unknown)
    -X github.com/prometheus/common/version.Branch=$(git branch --show-current 2>/dev/null || echo unknown)
    -X github.com/prometheus/common/version.BuildDate=${BUILD_DATE}
    -X github.com/prometheus/common/version.BuildUser=${BUILD_USER_WHOM}
  " \
  -o "${DIST}/vmware-exporter-linux-amd64" .

echo "    binary: $(ls -lh "${DIST}/vmware-exporter-linux-amd64" | awk '{print $5}')"

# 2. 组织打包文件
TMPDIR="$(mktemp -d /tmp/vme-pkg.XXXXXX)"
STAGING="${TMPDIR}/${ARCHIVE_NAME}"
mkdir -p "${STAGING}"

# 二进制
cp "${DIST}/vmware-exporter-linux-amd64" "${STAGING}/vmware-exporter"
chmod 0755 "${STAGING}/vmware-exporter"

# systemd unit
cp "${PKG_DIR}/vmware-exporter.service" "${STAGING}/"

# 配置模板
cp "${PKG_DIR}/config.yaml" "${STAGING}/"

# 安装脚本
cp "${PKG_DIR}/install.sh" "${STAGING}/"
chmod 0755 "${STAGING}/install.sh"

# 卸载脚本
cp "${PKG_DIR}/uninstall.sh" "${STAGING}/"
chmod 0755 "${STAGING}/uninstall.sh"

# 部署文档
cp "${PKG_DIR}/DEPLOY-zh.md" "${STAGING}/"
cp "${ROOT}/LICENSE" "${STAGING}/"

# 3. 打包（macOS 兼容：bsdtar 没有 --owner=0 --group=0）
echo "==> 打包 ${ARCHIVE_NAME}.tar.gz"
cd "${TMPDIR}"
COPYFILE_DISABLE=1 tar --no-mac-metadata --numeric-owner --uid 0 --gid 0 -czf "${OUTPUT}" "${ARCHIVE_NAME}"
cd "${ROOT}"

# 4. SHA256
#
# 在 dist/ 里执行，让校验文件只记录文件名而不是绝对路径 —— 否则
# `sha256sum -c` 在别人的机器上会因为找不到那个路径而失败。
cd "${DIST}"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum "$(basename "${OUTPUT}")" > "$(basename "${OUTPUT}").sha256"
elif command -v shasum >/dev/null 2>&1; then
  shasum -a 256 "$(basename "${OUTPUT}")" > "$(basename "${OUTPUT}").sha256"
else
  echo "警告：找不到 sha256sum 或 shasum，跳过 .sha256 生成" >&2
fi
cd "${ROOT}"

# 5. 清理临时文件
rm -rf "${TMPDIR}" "${DIST}/vmware-exporter-linux-amd64"

echo "==> 完成：${OUTPUT}"
echo "    $(cat "${OUTPUT}.sha256" 2>/dev/null || echo "(no sha256)")"
echo "    $(tar -tzf "${OUTPUT}" | head -20)"
echo "    ... ($(tar -tzf "${OUTPUT}" | wc -l) files total)"