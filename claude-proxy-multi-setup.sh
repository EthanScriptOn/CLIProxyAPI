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
# 1. 从 GitHub 拉取最新包
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
# 2. 创建单个实例
# ==============================
create_instance() {
    local domain=$1
    local port=$2
    local api_key=$3
    local pgstore_dsn=${4:-""}
    local node_ip=${5:-""}

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
    if [[ -n "${pgstore_dsn}" ]]; then
        # postgres 模式：api-keys 和 auth-dir 由数据库管理，不写入 config
        cat > "${install_dir}/config.yaml" << EOF
host: "0.0.0.0"
port: ${port}

debug: false
logging-to-file: true
logs-max-total-size-mb: 1000
request-retry: 3
max-retry-credentials: 2
max-retry-interval: 30

remote-management:
  allow-remote: true
  disable-control-panel: false

usage-statistics-enabled: true
EOF
    else
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
  disable-control-panel: false

usage-statistics-enabled: true
EOF
    fi

    # Build environment block for systemd service
    local env_block="Environment=HOME=${HOME}"
    if [[ -n "${pgstore_dsn}" ]]; then
        env_block="${env_block}
Environment=PGSTORE_DSN=${pgstore_dsn}"
    fi
    if [[ -n "${node_ip}" ]]; then
        env_block="${env_block}
Environment=NODE_IP=${node_ip}"
    fi

    # systemd service
    cat > "${systemd_dir}/${service_name}.service" << EOF
[Unit]
Description=Proxy API Service (${domain})
After=network.target

[Service]
Type=simple
WorkingDirectory=${install_dir}
ExecStart=${install_dir}/proxycore -config ${install_dir}/config.yaml
Restart=always
RestartSec=10
${env_block}

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

    # postgres 模式下 static 目录路径不同，需要额外复制 management.html
    if [[ -n "${pgstore_dsn}" ]] && [[ -f "${install_dir}/static/management.html" ]]; then
        mkdir -p "${install_dir}/pgstore/config/static"
        cp "${install_dir}/static/management.html" "${install_dir}/pgstore/config/static/management.html"
        log_success "management.html 已复制到 pgstore/config/static/"
    fi
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

    # 从 GitHub 拉取
    fetch_from_github

    echo ""

    # 询问共享数据库 DSN（可选）
    local pgstore_dsn=""
    echo "【可选】数据库集中化配置"
    echo "  所有实例共用同一 PostgreSQL，api_key 和 usage 全局共享，认证文件按机器隔离。"
    read -rp "PGSTORE_DSN（留空跳过，格式: postgres://user:pass@host:5432/db）: " pgstore_dsn
    pgstore_dsn="${pgstore_dsn// /}"

    # 检测本机公网 IP（仅在使用数据库时需要）
    local node_ip=""
    if [[ -n "${pgstore_dsn}" ]]; then
        node_ip=$(curl -sf --max-time 5 https://api.ipify.org 2>/dev/null || true)
        if [[ -z "${node_ip}" ]]; then
            node_ip=$(curl -sf --max-time 5 https://ifconfig.me 2>/dev/null || true)
        fi
        if [[ -n "${node_ip}" ]]; then
            log_info "检测到本机公网 IP: ${node_ip}"
        else
            log_warn "无法自动检测公网 IP，将由程序自动获取内网出口 IP"
        fi
    fi

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
            read -rp "实例名称（如 node1，用于目录和服务名，默认 node${i}）: " domain
            domain="${domain:-node${i}}"
            domain="${domain// /}"
            [[ -n "$domain" ]] || { echo "实例名称不能为空"; continue; }
            instance_exists "$domain" && { echo "实例 ${domain} 已存在，请换一个名称"; continue; }
            break
        done

        local api_key
        read -rp "API Key（留空自动生成）: " api_key
        api_key="${api_key:-$(generate_api_key)}"

        INST_PORTS+=("$port")
        INST_DOMAINS+=("$domain")
        INST_KEYS+=("$api_key")

        create_instance "$domain" "$port" "$api_key" "${pgstore_dsn}" "${node_ip}"
    done

    # 开放防火墙（仅开放服务端口，由 nginx 网关对外）
    if command -v ufw &>/dev/null && ufw status | grep -q "Status: active"; then
        for port in "${INST_PORTS[@]}"; do
            ufw allow "${port}/tcp" &>/dev/null && log_success "ufw 已放行端口 ${port}"
        done
        ufw reload &>/dev/null
    fi

    # 汇总
    echo ""
    echo "========================================"
    echo -e "${GREEN}  部署完成！实例汇总${NC}"
    echo "========================================"
    if [[ -n "${pgstore_dsn}" ]]; then
        echo ""
        echo -e "${CYAN}▶ 数据库集中化已启用${NC}"
        echo "  所有实例共用同一数据库，api_key 在任意实例管理面板创建后"
        echo "  约 30 秒内自动同步到所有机器。"
        echo "  认证文件（OAuth 登录）按机器 IP 隔离，互不影响。"
    fi
    for i in "${!INST_DOMAINS[@]}"; do
        local d="${INST_DOMAINS[$i]}"
        echo ""
        echo -e "${CYAN}▶ ${d}${NC}"
        echo "  实例名称 : ${d}"
        echo "  API Key  : ${INST_KEYS[$i]}"
        echo "  监听端口 : ${INST_PORTS[$i]}（通过 nginx 网关对外暴露）"
        echo "  工作目录 : /root/proxycore-${d}"
        echo "  Auth目录 : /root/.proxycore-${d}"
        echo "  服务名   : proxycore-${d}.service"
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
