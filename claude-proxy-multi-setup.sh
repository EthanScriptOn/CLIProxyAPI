#!/bin/bash

# ProxyCore 多实例部署脚本
# 自动从 GitHub 拉取最新版本，创建任意数量的独立实例
# 用法：bash claude-proxy-multi-setup.sh

set -euo pipefail

# ==============================
# 全局配置
# ==============================
REPO="EthanScriptOn/CLIProxyAPI"
RAW_BASE="https://raw.githubusercontent.com/${REPO}/main"
API_URL="https://api.github.com/repos/${REPO}/contents/dist"
CADDYFILE="/etc/caddy/Caddyfile"

DOWNLOAD_DIR="/tmp/proxycore_dl"
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

log_info()    { echo -e "${CYAN}[INFO]${NC} $1"; }
log_success() { echo -e "${GREEN}[OK]${NC} $1"; }
log_warn()    { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error()   { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }
log_step()    { echo -e "${CYAN}[STEP]${NC} $1"; }

# ==============================
# 清理临时目录
# ==============================
cleanup() {
    rm -rf "${DOWNLOAD_DIR}"
}
trap cleanup EXIT

# ==============================
# 工具函数
# ==============================
generate_api_key() {
    local chars="abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
    local key="sk-proxy-"
    for i in {1..40}; do
        key="${key}${chars:$((RANDOM % ${#chars})):1}"
    done
    echo "$key"
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)  echo "linux_amd64" ;;
        arm64|aarch64) echo "linux_arm64" ;;
        *) log_error "不支持的架构: $(uname -m)，仅支持 x86_64 和 arm64" ;;
    esac
}

check_deps() {
    local missing=()
    for cmd in wget curl tar; do
        command -v "$cmd" &>/dev/null || missing+=("$cmd")
    done
    if [[ ${#missing[@]} -gt 0 ]]; then
        log_error "缺少依赖工具: ${missing[*]}，请先安装"
    fi
}

port_in_use() {
    local port=$1
    # 检查系统端口占用
    if ss -tlnp 2>/dev/null | grep -q ":${port}[[:space:]]"; then
        return 0
    fi
    # 检查所有实例的配置（含旧单实例路径 /root/proxycore/）
    if grep -rl "^port: ${port}$" /root/proxycore/config.yaml /root/proxycore-*/config.yaml 2>/dev/null | grep -q .; then
        return 0
    fi
    return 1
}

instance_exists() {
    [[ -d "/root/proxycore-${1}" ]]   # 参数传入 domain
}

# ==============================
# 1. 安装 Caddy（如未安装）
# ==============================
ensure_caddy() {
    if command -v caddy &>/dev/null; then
        log_success "Caddy 已安装，跳过"
        return
    fi
    log_step "安装 Caddy..."
    if command -v apt-get &>/dev/null; then
        apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl
        curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
            | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
        curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
            | tee /etc/apt/sources.list.d/caddy-stable.list
        apt-get update
        apt-get install -y caddy
    elif command -v dnf &>/dev/null; then
        dnf install -y 'dnf-command(copr)'
        dnf copr enable -y @caddy/caddy
        dnf install -y caddy
    elif command -v yum &>/dev/null; then
        yum install -y yum-plugin-copr
        yum copr enable -y @caddy/caddy
        yum install -y caddy
    else
        log_error "不支持的包管理器，请手动安装 Caddy"
    fi
    systemctl enable caddy
    systemctl start caddy
    log_success "Caddy 安装完成"
}

# ==============================
# 2. 从 GitHub 拉取最新包
# ==============================
fetch_from_github() {
    log_step "检测系统架构..."
    ARCH=$(detect_arch)
    log_success "架构: ${ARCH}"

    mkdir -p "${DOWNLOAD_DIR}"

    # 获取最新 tar.gz 包名
    log_step "查找最新版本包..."
    local tar_name
    tar_name=$(curl -sf "${API_URL}" \
        | grep '"name"' \
        | grep "${ARCH}\.tar\.gz" \
        | sed 's/.*"name": "\(.*\)".*/\1/' \
        | sort | tail -1 || true)

    if [[ -z "$tar_name" ]]; then
        log_error "未在 dist/ 目录找到 ${ARCH} 的 tar.gz 包，请检查仓库"
    fi
    log_success "最新包: ${tar_name}"

    # 下载 tar.gz
    log_step "下载 ${tar_name}..."
    wget -q --show-progress \
        -O "${DOWNLOAD_DIR}/${tar_name}" \
        "${RAW_BASE}/dist/${tar_name}" || log_error "下载 ${tar_name} 失败"

    # 下载辅助脚本
    log_step "下载辅助脚本..."
    for script in claude-auth.sh claude-verify.sh; do
        wget -q -O "${DOWNLOAD_DIR}/${script}" "${RAW_BASE}/${script}" \
            && chmod +x "${DOWNLOAD_DIR}/${script}" \
            && log_success "${script}" \
            || log_warn "${script} 下载失败，跳过"
    done

    # 解压
    log_step "解压..."
    local extract_dir="${DOWNLOAD_DIR}/extracted"
    mkdir -p "${extract_dir}"
    tar -xzf "${DOWNLOAD_DIR}/${tar_name}" -C "${extract_dir}"

    # 定位二进制和 static 目录
    PROXYCORE_BIN=$(find "${extract_dir}" -name "proxycore" -type f | head -1 || true)
    PROXYCORE_STATIC=$(find "${extract_dir}" -type d -name "static" | head -1 || true)

    if [[ -z "${PROXYCORE_BIN}" ]]; then
        log_error "解压后未找到 proxycore 二进制文件"
    fi
    log_success "下载就绪，待安装到各实例工作目录"
}

# ==============================
# 3. 创建单个实例
# ==============================
create_instance() {
    local domain=$1
    local port=$2
    local api_key=$3

    local install_dir="/root/proxycore-${domain}"
    local auth_dir="/root/.proxycore-${domain}"
    local service_name="proxycore-${domain}"
    local systemd_dir="/etc/systemd/system"

    log_info "创建实例 [${domain}]  端口:${port}"

    mkdir -p "${install_dir}" "${auth_dir}" "${systemd_dir}"

    # 从下载目录直接复制所有内容到工作目录
    cp "${PROXYCORE_BIN}" "${install_dir}/proxycore"
    chmod +x "${install_dir}/proxycore"
    [[ -n "${PROXYCORE_STATIC}" ]] && cp -r "${PROXYCORE_STATIC}" "${install_dir}/"
    for script in claude-auth.sh claude-verify.sh; do
        [[ -f "${DOWNLOAD_DIR}/${script}" ]] && cp "${DOWNLOAD_DIR}/${script}" "${install_dir}/${script}"
    done

    # config.yaml
    cat > "${install_dir}/config.yaml" << EOF
host: "0.0.0.0"
port: ${port}

api-keys:
  - "${api_key}"

auth-dir: "${auth_dir}"

debug: false
logging-to-file: true
logs-max-total-size-mb: 1000
request-retry: 3
max-retry-credentials: 2
max-retry-interval: 30

remote-management:
  allow-remote: true
  secret-key: "${domain}"
  disable-control-panel: false

usage-statistics-enabled: true
EOF

    # systemd service
    cat > "${systemd_dir}/${service_name}.service" << EOF
[Unit]
Description=Proxy API Service (${domain})
After=network.target

[Service]
Type=simple
WorkingDirectory=${install_dir}
ExecStart=${install_dir}/proxycore
Restart=always
RestartSec=10
Environment=HOME=${HOME}

[Install]
WantedBy=default.target
EOF

    loginctl enable-linger root 2>/dev/null || true
    systemctl daemon-reload
    systemctl enable "${service_name}.service"
    systemctl start "${service_name}.service"

    sleep 2
    if systemctl is-active --quiet "${service_name}.service"; then
        log_success "实例 [${domain}] 服务已启动"
    else
        log_warn "实例 [${domain}] 服务可能未启动，请检查: systemctl status ${service_name}.service"
    fi
}

# ==============================
# 4. 追加 Caddy 配置
# ==============================
append_caddy_block() {
    local domain=$1
    local port=$2

    if grep -q "^${domain}" "${CADDYFILE}" 2>/dev/null; then
        log_warn "Caddyfile 中已存在 ${domain}，跳过"
        return
    fi

    cat >> "${CADDYFILE}" << EOF

${domain} {
    handle {
        reverse_proxy 127.0.0.1:${port}
    }
}
EOF
    log_success "Caddy 配置: ${domain} -> 127.0.0.1:${port}"
}

# ==============================
# 主流程
# ==============================
main() {
    echo "========================================"
    echo "   ProxyCore 多实例部署工具"
    echo "========================================"
    echo ""

    check_deps
    ensure_caddy

    # 初始化 Caddyfile
    if [[ ! -f "${CADDYFILE}" ]]; then
        mkdir -p "$(dirname "${CADDYFILE}")"
        touch "${CADDYFILE}"
        log_info "已创建空 Caddyfile: ${CADDYFILE}"
    fi

    # 从 GitHub 拉取
    fetch_from_github

    echo ""

    # 询问实例数量
    local count
    while true; do
        read -rp "要创建几个实例？: " count
        [[ "$count" =~ ^[1-9][0-9]*$ ]] && break
        echo "请输入大于 0 的整数"
    done

    declare -a INST_PORTS=()
    declare -a INST_DOMAINS=()
    declare -a INST_KEYS=()

    # 收集每个实例的参数
    for i in $(seq 1 "$count"); do
        echo ""
        echo "--- 实例 ${i}/${count} ---"

        local default_port=$((8080 + i))
        local port
        while true; do
            read -rp "监听端口（默认 ${default_port}）: " port
            port="${port:-$default_port}"
            [[ "$port" =~ ^[0-9]+$ ]] && [[ "$port" -ge 1024 ]] && [[ "$port" -le 65535 ]] \
                || { echo "请输入 1024-65535 之间的端口"; continue; }
            port_in_use "$port" && { echo "端口 ${port} 已被占用"; continue; }
            break
        done

        local domain
        while true; do
            read -rp "域名（如 n${i}.myclaudeproxy.xyz）: " domain
            domain="${domain// /}"
            [[ -n "$domain" ]] || { echo "域名不能为空"; continue; }
            instance_exists "$domain" && { echo "域名 ${domain} 的实例已存在，请换一个"; continue; }
            break
        done

        local api_key
        read -rp "API Key（留空自动生成）: " api_key
        api_key="${api_key:-$(generate_api_key)}"

        INST_PORTS+=("$port")
        INST_DOMAINS+=("$domain")
        INST_KEYS+=("$api_key")

        create_instance "$domain" "$port" "$api_key"
        append_caddy_block "$domain" "$port"
    done

    # 重载 Caddy
    echo ""
    log_step "重载 Caddy..."
    if systemctl reload caddy 2>/dev/null; then
        log_success "Caddy 重载完成"
    elif caddy reload --config "${CADDYFILE}" 2>/dev/null; then
        log_success "Caddy 重载完成"
    else
        log_warn "Caddy reload 失败，尝试 restart..."
        systemctl restart caddy && log_success "Caddy 重启完成"
    fi

    # 开放防火墙
    if command -v ufw &>/dev/null; then
        ufw allow 80/tcp  2>/dev/null || true
        ufw allow 443/tcp 2>/dev/null || true
    elif command -v firewall-cmd &>/dev/null; then
        firewall-cmd --permanent --add-service=http  2>/dev/null || true
        firewall-cmd --permanent --add-service=https 2>/dev/null || true
        firewall-cmd --reload 2>/dev/null || true
    fi

    # 汇总
    echo ""
    echo "========================================"
    echo -e "${GREEN}  部署完成！实例汇总${NC}"
    echo "========================================"
    for i in "${!INST_DOMAINS[@]}"; do
        local d="${INST_DOMAINS[$i]}"
        echo ""
        echo -e "${CYAN}▶ ${d}${NC}"
        echo "  代理地址 : https://${d}"
        echo "  API Key  : ${INST_KEYS[$i]}"
        echo "  工作目录 : /root/proxycore-${d}"
        echo "  Auth目录 : /root/.proxycore-${d}"
        echo "  服务名   : proxycore-${d}.service"
        echo "  端口     : ${INST_PORTS[$i]}"
        echo ""
        echo "  授权账号 :"
        echo "    cd /root/proxycore-${d} && ./proxycore --claude-login"
        echo "  查看状态 :"
        echo "    systemctl status proxycore-${d}.service"
        echo "  查看日志 :"
        echo "    journalctl -u proxycore-${d}.service -f"
    done
    echo ""
}

main "$@"
