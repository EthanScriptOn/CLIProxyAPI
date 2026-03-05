#!/bin/bash

# 交叉编译 Linux 二进制并打包，产物输出到 dist/ 供手动上传

set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
BUILD_DIR="${PROJECT_DIR}/.deploy_build"
DIST_DIR="${PROJECT_DIR}/dist"
BINARY_NAME="cli-proxy-api"

# 颜色
RED='\033[0;31m'
GREEN='\033[0;32m'
CYAN='\033[0;36m'
NC='\033[0m'

log_success() { echo -e "${GREEN}[OK]${NC} $1"; }
log_error()   { echo -e "${RED}[ERROR]${NC} $1"; }
log_step()    { echo -e "${CYAN}[STEP]${NC} $1"; }

cleanup() { rm -rf "$BUILD_DIR"; }
trap cleanup EXIT

# ==============================
# 1. 检查依赖
# ==============================
if ! command -v go >/dev/null 2>&1; then
    log_error "未找到 Go 编译器，请先安装 Go: https://go.dev/dl/"
    exit 1
fi

if ! command -v npm >/dev/null 2>&1; then
    log_error "未找到 npm，请先安装 Node.js"
    exit 1
fi

# ==============================
# 2. 构建前端
# ==============================
log_step "构建前端..."
cd "${PROJECT_DIR}/frontend"
npm install --silent
npm run build --silent
cp "${PROJECT_DIR}/frontend/dist/index.html" "${PROJECT_DIR}/static/index.html"
cp "${PROJECT_DIR}/frontend/dist/index.html" "${PROJECT_DIR}/static/management.html"
log_success "前端构建完成，已同步到 static/"

# ==============================
# 3. 交叉编译 linux/amd64
# ==============================
log_step "交叉编译 linux/amd64 二进制..."

mkdir -p "$BUILD_DIR"
BINARY_PATH="${BUILD_DIR}/${BINARY_NAME}"

cd "$PROJECT_DIR"
GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" \
    -o "$BINARY_PATH" \
    ./cmd/server/

log_success "编译完成: $(du -sh "$BINARY_PATH" | cut -f1)"

# ==============================
# 3. 打包 tar.gz 到 dist/
# ==============================
VERSION=$(git -C "$PROJECT_DIR" describe --tags --exact-match 2>/dev/null \
    || git -C "$PROJECT_DIR" describe --tags 2>/dev/null \
    || date +%Y%m%d)
VERSION="${VERSION#v}"

PACKAGE_NAME="CLIProxyAPI_${VERSION}_linux_amd64.tar.gz"

mkdir -p "$DIST_DIR"
PACKAGE_PATH="${DIST_DIR}/${PACKAGE_NAME}"

log_step "打包 → ${PACKAGE_NAME}"

tar_args=(-czf "$PACKAGE_PATH" -C "$BUILD_DIR" "${BINARY_NAME}")
if [[ -f "${PROJECT_DIR}/config.example.yaml" ]]; then
    tar_args+=(-C "${PROJECT_DIR}" config.example.yaml)
fi
if [[ -d "${PROJECT_DIR}/static" ]]; then
    tar_args+=(-C "${PROJECT_DIR}" static)
fi
tar "${tar_args[@]}"

log_success "打包完成: $(du -sh "$PACKAGE_PATH" | cut -f1)"

# ==============================
# 4. 完成
# ==============================
echo ""
echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}  构建完成！${NC}"
echo -e "${GREEN}========================================${NC}"
echo ""
echo -e "产物路径: ${PACKAGE_PATH}"
echo ""
echo -e "请手动上传以下文件到服务器 /root/cliproxyapi/："
echo -e "  ${PACKAGE_PATH}"
echo -e "  ${PROJECT_DIR}/cliproxyapi-installer.sh"
echo -e "  ${PROJECT_DIR}/claude-proxy-setup.sh"
echo ""
echo -e "上传后在服务器执行:"
echo -e "  bash /root/cliproxyapi/claude-proxy-setup.sh"
echo ""
