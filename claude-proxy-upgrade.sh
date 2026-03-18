#!/bin/bash

# ProxyCore 服务升级脚本
# 只更新二进制和 static，保留 config.yaml 和 auth 数据
# 用法：bash claude-proxy-upgrade.sh

set -euo pipefail

REPO="EthanScriptOn/CLIProxyAPI"
RAW_BASE="https://raw.githubusercontent.com/${REPO}/main"
API_URL="https://api.github.com/repos/${REPO}/contents/dist"
DOWNLOAD_DIR="/tmp/proxycore_upgrade"
SYSTEMD_DIR="/etc/systemd/system"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

log_info()    { echo -e "${CYAN}[INFO]${NC} $1"; }
log_success() { echo -e "${GREEN}[OK]${NC} $1"; }
log_warn()    { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error()   { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }
log_step()    { echo -e "${CYAN}[STEP]${NC} $1"; }

cleanup() {
    rm -rf "${DOWNLOAD_DIR}"
}
trap cleanup EXIT

# ==============================
# 工具函数
# ==============================
detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)  echo "linux_amd64" ;;
        arm64|aarch64) echo "linux_arm64" ;;
        *) log_error "不支持的架构: $(uname -m)" ;;
    esac
}

find_service_by_install_dir() {
    local install_dir=$1
    grep -rl "WorkingDirectory=${install_dir}$" "${SYSTEMD_DIR}"/*.service 2>/dev/null \
        | xargs -I{} basename {} .service 2>/dev/null \
        | head -1 || true
}

# 列出所有实例，填充到数组
collect_instances() {
    INSTANCE_DIRS=()
    INSTANCE_DOMAINS=()
    INSTANCE_SERVICES=()
    INSTANCE_PORTS=()

    for dir in /root/proxycore-*/; do
        [[ -d "$dir" ]] || continue
        local domain="${dir#/root/proxycore-}"
        domain="${domain%/}"
        local install_dir="${dir%/}"
        local service
        service=$(find_service_by_install_dir "${install_dir}" || true)
        local port=""
        [[ -f "${install_dir}/config.yaml" ]] && \
            port=$(grep "^port:" "${install_dir}/config.yaml" | awk '{print $2}')
        INSTANCE_DIRS+=("${install_dir}")
        INSTANCE_DOMAINS+=("${domain}")
        INSTANCE_SERVICES+=("${service:-}")
        INSTANCE_PORTS+=("${port:-?}")
    done
}

list_instances() {
    if [[ ${#INSTANCE_DOMAINS[@]} -eq 0 ]]; then
        log_error "未找到任何实例（/root/proxycore-* 目录不存在）"
    fi
    echo "序号  域名                          端口   服务"
    echo "----  ----------------------------  -----  -------------------------"
    for i in "${!INSTANCE_DOMAINS[@]}"; do
        printf "%-4s  %-28s  %-5s  %s\n" \
            "$((i+1))" \
            "${INSTANCE_DOMAINS[$i]}" \
            "${INSTANCE_PORTS[$i]}" \
            "${INSTANCE_SERVICES[$i]:-未知}"
    done
    echo ""
}

# ==============================
# 从 GitHub 拉取最新包
# ==============================
fetch_from_github() {
    log_step "检测系统架构..."
    local arch
    arch=$(detect_arch)
    log_success "架构: ${arch}"

    mkdir -p "${DOWNLOAD_DIR}"

    log_step "查找最新版本包..."
    local tar_name
    tar_name=$(curl -sf "${API_URL}" \
        | grep '"name"' \
        | grep "${arch}\.tar\.gz" \
        | sed 's/.*"name": "\(.*\)".*/\1/' \
        | sort | tail -1 || true)
    [[ -n "$tar_name" ]] || log_error "未找到 ${arch} 的 tar.gz 包"
    log_success "最新包: ${tar_name}"

    log_step "下载 ${tar_name}..."
    wget -q --show-progress \
        -O "${DOWNLOAD_DIR}/${tar_name}" \
        "${RAW_BASE}/dist/${tar_name}" || log_error "下载失败"

    log_step "解压..."
    local extract_dir="${DOWNLOAD_DIR}/extracted"
    mkdir -p "${extract_dir}"
    tar -xzf "${DOWNLOAD_DIR}/${tar_name}" -C "${extract_dir}"

    NEW_BIN=$(find "${extract_dir}" -name "proxycore" -type f | head -1 || true)
    NEW_STATIC=$(find "${extract_dir}" -type d -name "static" | head -1 || true)
    [[ -n "${NEW_BIN}" ]] || log_error "解压后未找到 proxycore 二进制"
    chmod +x "${NEW_BIN}"
    log_success "新版本就绪"
}

# ==============================
# 升级单个实例
# ==============================
upgrade_instance() {
    local install_dir=$1
    local domain=$2
    local service=$3

    log_info "升级实例: ${domain}"

    # 停止服务
    if [[ -n "$service" ]] && systemctl is-active --quiet "${service}.service" 2>/dev/null; then
        systemctl stop "${service}.service"
        log_success "[${domain}] 服务已停止"
    elif [[ -n "$service" ]]; then
        systemctl stop "${service}.service" 2>/dev/null || true
    fi

    # 替换二进制（先删除旧文件再复制，避免 "Text file busy"）
    rm -f "${install_dir}/proxycore"
    cp "${NEW_BIN}" "${install_dir}/proxycore"
    chmod +x "${install_dir}/proxycore"
    log_success "[${domain}] 二进制已更新"

    # 替换 static（先删旧的再复制）
    if [[ -n "${NEW_STATIC}" ]]; then
        rm -rf "${install_dir}/static"
        cp -r "${NEW_STATIC}" "${install_dir}/static"
        log_success "[${domain}] static 已更新"
    fi

    # 启动服务
    if [[ -n "$service" ]]; then
        systemctl start "${service}.service"
        sleep 1
        if systemctl is-active --quiet "${service}.service"; then
            log_success "[${domain}] 服务已重新启动"
        else
            log_warn "[${domain}] 服务可能未正常启动，请检查: systemctl status ${service}.service"
        fi
    else
        log_warn "[${domain}] 未找到对应服务，请手动启动"
    fi
}

# ==============================
# 主流程
# ==============================
main() {
    echo "========================================"
    echo "   ProxyCore 服务升级工具"
    echo "========================================"
    echo ""

    collect_instances
    list_instances

    # 选择升级哪些实例
    echo "请选择要升级的实例："
    echo "  a   升级全部"
    echo "  序号 升级指定实例（多个用空格分隔，如 1 3）"
    echo ""
    read -rp "输入选择: " selection
    selection="${selection// /}"  # 临时去空格判断是否为 a
    if [[ "$selection" == "a" || "$selection" == "A" ]]; then
        selection="a"
    fi

    # 确定目标列表
    declare -a targets=()
    if [[ "$selection" == "a" ]]; then
        for i in "${!INSTANCE_DOMAINS[@]}"; do
            targets+=("$i")
        done
    else
        read -ra nums <<< "$selection"
        for num in "${nums[@]}"; do
            if [[ "$num" =~ ^[0-9]+$ ]] && [[ "$num" -ge 1 ]] && [[ "$num" -le ${#INSTANCE_DOMAINS[@]} ]]; then
                targets+=("$((num-1))")
            else
                log_warn "无效序号: ${num}，跳过"
            fi
        done
    fi

    [[ ${#targets[@]} -gt 0 ]] || log_error "没有有效的升级目标"

    echo ""
    echo "将升级以下实例："
    for i in "${targets[@]}"; do
        echo "  - ${INSTANCE_DOMAINS[$i]}"
    done
    echo ""
    read -rp "确认执行？(y/N): " confirm
    [[ "$confirm" == "y" || "$confirm" == "Y" ]] || { echo "已取消"; exit 0; }

    # 拉取新版本
    echo ""
    fetch_from_github
    echo ""

    # 逐个升级
    for i in "${targets[@]}"; do
        upgrade_instance \
            "${INSTANCE_DIRS[$i]}" \
            "${INSTANCE_DOMAINS[$i]}" \
            "${INSTANCE_SERVICES[$i]}"
        echo ""
    done

    echo "========================================"
    echo -e "${GREEN}  升级完成${NC}"
    echo "========================================"
    for i in "${targets[@]}"; do
        echo "  ✓ ${INSTANCE_DOMAINS[$i]}"
    done
    echo ""
}

main "$@"
