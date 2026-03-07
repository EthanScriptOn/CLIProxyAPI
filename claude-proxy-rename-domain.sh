#!/bin/bash

# ProxyCore 域名变更脚本
# 将现有实例从旧域名迁移到新域名
# 用法：bash claude-proxy-rename-domain.sh

set -euo pipefail

CADDYFILE="/etc/caddy/Caddyfile"
SYSTEMD_DIR="${HOME}/.config/systemd/user"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

log_info()    { echo -e "${CYAN}[INFO]${NC} $1"; }
log_success() { echo -e "${GREEN}[OK]${NC} $1"; }
log_warn()    { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error()   { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }

# 通过工作目录找到对应的 systemd 服务名
find_service_by_domain() {
    local install_dir=$1
    grep -rl "WorkingDirectory=${install_dir}$" "${SYSTEMD_DIR}"/*.service 2>/dev/null \
        | xargs -I{} basename {} .service 2>/dev/null \
        | head -1 || true
}

# 列出所有现有实例
list_instances() {
    echo "当前已有实例："
    local found=false
    for dir in /root/proxycore-*/; do
        [[ -d "$dir" ]] || continue
        local domain="${dir#/root/proxycore-}"
        domain="${domain%/}"
        local service
        service=$(find_service_by_domain "${dir%/}" || true)
        local port=""
        [[ -f "${dir}config.yaml" ]] && port=$(grep "^port:" "${dir}config.yaml" | awk '{print $2}')
        echo "  域名: ${domain}  端口: ${port}  服务: ${service:-未知}"
        found=true
    done
    $found || echo "  （无）"
    echo ""
}

main() {
    echo "========================================"
    echo "   ProxyCore 域名变更工具"
    echo "========================================"
    echo ""

    list_instances

    # 输入旧域名
    local old_domain
    while true; do
        read -rp "要变更的旧域名: " old_domain
        old_domain="${old_domain// /}"
        [[ -n "$old_domain" ]] || { echo "域名不能为空"; continue; }
        [[ -d "/root/proxycore-${old_domain}" ]] || { echo "实例目录不存在: /root/proxycore-${old_domain}"; continue; }
        break
    done

    local old_install_dir="/root/proxycore-${old_domain}"
    local old_auth_dir="/root/.proxycore-${old_domain}"

    # 找到对应的 systemd 服务
    local service_name
    service_name=$(find_service_by_domain "${old_install_dir}")
    if [[ -z "$service_name" ]]; then
        log_error "未找到 WorkingDirectory=${old_install_dir} 对应的 systemd 服务，请手动检查 ${SYSTEMD_DIR}"
    fi
    log_info "找到服务: ${service_name}.service"

    # 输入新域名
    local new_domain
    while true; do
        read -rp "新域名: " new_domain
        new_domain="${new_domain// /}"
        [[ -n "$new_domain" ]] || { echo "域名不能为空"; continue; }
        [[ "$new_domain" != "$old_domain" ]] || { echo "新域名与旧域名相同"; continue; }
        [[ ! -d "/root/proxycore-${new_domain}" ]] || { echo "新域名目录已存在: /root/proxycore-${new_domain}"; continue; }
        break
    done

    local new_install_dir="/root/proxycore-${new_domain}"
    local new_auth_dir="/root/.proxycore-${new_domain}"

    echo ""
    echo "变更内容预览："
    echo "  工作目录: ${old_install_dir} → ${new_install_dir}"
    echo "  Auth目录: ${old_auth_dir}    → ${new_auth_dir}"
    echo "  Caddy:    ${old_domain}      → ${new_domain}"
    echo "  secret-key: ${old_domain}   → ${new_domain}"
    echo ""
    read -rp "确认执行？(y/N): " confirm
    [[ "$confirm" == "y" || "$confirm" == "Y" ]] || { echo "已取消"; exit 0; }

    # 1. 停止服务
    log_info "停止服务 ${service_name}..."
    systemctl --user stop "${service_name}.service" 2>/dev/null || true
    log_success "服务已停止"

    # 2. 重命名工作目录
    mv "${old_install_dir}" "${new_install_dir}"
    log_success "工作目录已重命名"

    # 3. 重命名 auth 目录（如存在）
    if [[ -d "${old_auth_dir}" ]]; then
        mv "${old_auth_dir}" "${new_auth_dir}"
        log_success "Auth 目录已重命名"
    else
        mkdir -p "${new_auth_dir}"
        log_warn "旧 Auth 目录不存在，已新建: ${new_auth_dir}"
    fi

    # 4. 更新 config.yaml（sed 特殊字符转义）
    local old_escaped="${old_domain//./\\.}"
    local new_config="${new_install_dir}/config.yaml"
    if [[ -f "$new_config" ]]; then
        # 更新 secret-key
        sed -i "s|secret-key: \"${old_domain}\"|secret-key: \"${new_domain}\"|g" "$new_config"
        # 更新 auth-dir 路径
        sed -i "s|auth-dir: \"${old_auth_dir}\"|auth-dir: \"${new_auth_dir}\"|g" "$new_config"
        log_success "config.yaml 已更新"
    else
        log_warn "config.yaml 不存在，跳过"
    fi

    # 5. 更新 systemd service 文件
    local service_file="${SYSTEMD_DIR}/${service_name}.service"
    if [[ -f "$service_file" ]]; then
        sed -i "s|WorkingDirectory=${old_install_dir}|WorkingDirectory=${new_install_dir}|g" "$service_file"
        sed -i "s|ExecStart=${old_install_dir}/proxycore|ExecStart=${new_install_dir}/proxycore|g" "$service_file"
        log_success "systemd service 文件已更新"
    else
        log_warn "service 文件不存在: ${service_file}"
    fi

    # 6. 更新 Caddyfile
    if [[ -f "$CADDYFILE" ]]; then
        if grep -q "^${old_escaped} {" "$CADDYFILE"; then
            sed -i "s|^${old_domain} {|${new_domain} {|g" "$CADDYFILE"
            log_success "Caddyfile 已更新"
        else
            log_warn "Caddyfile 中未找到 ${old_domain} 的配置块，请手动检查 ${CADDYFILE}"
        fi
    else
        log_warn "Caddyfile 不存在: ${CADDYFILE}"
    fi

    # 7. 重载 systemd 并启动服务
    systemctl --user daemon-reload
    systemctl --user start "${service_name}.service"
    sleep 2
    if systemctl --user is-active --quiet "${service_name}.service"; then
        log_success "服务已重新启动"
    else
        log_warn "服务可能未正常启动，请检查: systemctl --user status ${service_name}.service"
    fi

    # 8. 重载 Caddy
    if systemctl reload caddy 2>/dev/null; then
        log_success "Caddy 已重载"
    else
        systemctl restart caddy && log_success "Caddy 已重启"
    fi

    echo ""
    echo "========================================"
    echo -e "${GREEN}  域名变更完成${NC}"
    echo "========================================"
    echo "  新代理地址 : https://${new_domain}"
    echo "  工作目录   : ${new_install_dir}"
    echo "  Auth目录   : ${new_auth_dir}"
    echo "  服务名     : ${service_name}.service"
    echo ""
    echo "  DNS 记录请同步更新（如需要）"
    echo ""
}

main "$@"
