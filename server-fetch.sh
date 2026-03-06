#!/bin/bash

# 从 GitHub 拉取 ProxyCore 所有部署文件
# 在服务器上执行，拉取完成后运行 claude-proxy-setup.sh

set -euo pipefail

REPO="EthanScriptOn/CLIProxyAPI"
# 修复：去掉空格，使用正确的 raw 域名
RAW_BASE="https://raw.githubusercontent.com/${REPO}/main"
INSTALL_DIR="/root/proxycore"

RED='\033[0;31m'
GREEN='\033[0;32m'
CYAN='\033[0;36m'
NC='\033[0m'

log_step()    { echo -e "${CYAN}[STEP]${NC} $1"; }
log_success() { echo -e "${GREEN}[OK]${NC} $1"; }
log_error()   { echo -e "${RED}[ERROR]${NC} $1"; }

mkdir -p "$INSTALL_DIR"

# ==============================
# 1. 拉取 sh 脚本
# ==============================
log_step "拉取部署脚本..."

wget -q --show-progress -O "${INSTALL_DIR}/proxycore-installer.sh" \
    "${RAW_BASE}/proxycore-installer.sh" || {
    log_error "下载 proxycore-installer.sh 失败"
    exit 1
}
chmod +x "${INSTALL_DIR}/proxycore-installer.sh"
log_success "proxycore-installer.sh"

wget -q --show-progress -O "${INSTALL_DIR}/claude-proxy-setup.sh" \
    "${RAW_BASE}/claude-proxy-setup.sh" || {
    log_error "下载 claude-proxy-setup.sh 失败"
    exit 1
}
chmod +x "${INSTALL_DIR}/claude-proxy-setup.sh"
log_success "claude-proxy-setup.sh"

wget -q --show-progress -O "${INSTALL_DIR}/claude-auth.sh" \
    "${RAW_BASE}/claude-auth.sh" || {
    log_error "下载 claude-auth.sh 失败"
    exit 1
}
chmod +x "${INSTALL_DIR}/claude-auth.sh"
log_success "claude-auth.sh"

wget -q --show-progress -O "${INSTALL_DIR}/claude-verify.sh" \
    "${RAW_BASE}/claude-verify.sh" || {
    log_error "下载 claude-verify.sh 失败"
    exit 1
}
chmod +x "${INSTALL_DIR}/claude-verify.sh"
log_success "claude-verify.sh"

# ==============================
# 2. 拉取最新 tar.gz 包
# ==============================
log_step "查找最新 tar.gz 包..."

# 修复：同样去掉 API URL 中的空格
API_URL="https://api.github.com/repos/${REPO}/contents/dist"
TAR_NAME=$(curl -s "$API_URL" \
    | grep '"name"' \
    | grep 'linux_amd64\.tar\.gz' \
    | sed 's/.*"name": "\(.*\)".*/\1/' \
    | sort | tail -1) || true

if [[ -z "$TAR_NAME" ]]; then
    log_error "未找到 dist/ 下的 tar.gz 包，请检查仓库"
    exit 1
fi

log_step "拉取 ${TAR_NAME}..."
wget -q --show-progress -O "${INSTALL_DIR}/${TAR_NAME}" \
    "${RAW_BASE}/dist/${TAR_NAME}" || {
    log_error "下载 ${TAR_NAME} 失败"
    exit 1
}
log_success "${TAR_NAME}"

# ==============================
# 完成
# ==============================
echo ""
echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}  所有文件拉取完成！${NC}"
echo -e "${GREEN}========================================${NC}"
echo ""
echo -e "下一步，执行部署："
echo -e "  bash ${INSTALL_DIR}/claude-proxy-setup.sh"
echo ""