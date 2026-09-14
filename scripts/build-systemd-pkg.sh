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
  -o "${DIST}/vmware-exporter-linux-amd64" ./cmd/vmware-exporter

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

# 3. 打包
#
# 三条 macOS 侧的去苹果元数据措施缺一不可 —— 这个包是在 macOS 上构建、
# 拿到 Ubuntu 的 GNU tar 上解开的：
#
#   - COPYFILE_DISABLE=1 阻止 bsdtar 写入 ._ AppleDouble 文件；
#   - --no-mac-metadata 不导出常规苹果扩展属性；
#   - --format=ustar 是关键：bsdtar 默认产出 pax 扩展头，而 macOS 13.7+
#     会给每个文件挂 com.apple.provenance（Gatekeeper 来源追踪）xattr，
#     该 xattr 会被写进 pax 头（LIBARCHIVE.xattr.* / SCHILY.xattr.*），
#     --no-mac-metadata 在 bsdtar 3.5.3 下挡不住它。Ubuntu 的 GNU tar
#     不认识这个关键字，解包时刷一屏
#     "tar: Ignoring unknown extended header keyword ..."。
#     ustar 格式根本没有 pax 扩展头，告警无从产生。
#
# ustar 的代价是单路径 ≤100/整体前缀+名 ≤255 字符、单文件 ≤8GiB —— 本包
# 成员路径最长约 55 字符、二进制约 25MiB，远在限制内，故无影响。
#
# --numeric-owner/--uid 0/--gid 0：bsdtar 没有 GNU tar 的 --owner=0，
# 用这三个组合把属主钉成 root 且不写用户名/组名。
echo "==> 打包 ${ARCHIVE_NAME}.tar.gz"
cd "${TMPDIR}"
COPYFILE_DISABLE=1 tar --format=ustar --no-mac-metadata \
  --numeric-owner --uid 0 --gid 0 -czf "${OUTPUT}" "${ARCHIVE_NAME}"
cd "${ROOT}"

# 打包自检：任何成员都不许带 pax 扩展头（即苹果 provenance xattr 不得泄漏
# 进包）。在 macOS 上用 Python 的 tarfile 校验，等于替 Ubuntu 的 GNU tar
# 提前确认解包不会有 "Ignoring unknown extended header keyword" 告警。
if command -v python3 >/dev/null 2>&1; then
  if ! python3 - "${OUTPUT}" <<'PYEOF'
import sys, tarfile
bad = []
with tarfile.open(sys.argv[1], "r:gz") as tf:
    for m in tf.getmembers():
        leaked = [k for k in m.pax_headers if "xattr" in k or "apple" in k]
        if leaked:
            bad.append((m.name, leaked))
if bad:
    for name, keys in bad:
        print(f"  {name}: leaked pax keys {keys}", file=sys.stderr)
    sys.exit("archive contains platform-specific xattr pax headers "
             "(com.apple.provenance); GNU tar would warn on extraction")
PYEOF
  then
    echo "错误：归档自检失败，中止" >&2
    exit 1
  fi
  echo "    自检通过：归档不含平台相关扩展属性头"
fi

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