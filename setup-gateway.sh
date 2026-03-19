#!/bin/bash

# ProxyCore Caddy 网关安装配置脚本
# 功能：安装 Caddy，配置多后端负载均衡 + 健康探测，自动 HTTPS
# 用法：sudo bash setup-gateway.sh

set -euo pipefail

GREEN='\033[0;32m'
CYAN='\033[0;36m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

log_step()    { echo -e "${CYAN}[STEP]${NC} $1"; }
log_success() { echo -e "${GREEN}[OK]${NC}   $1"; }
log_warn()    { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error()   { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }

if [[ $EUID -ne 0 ]]; then
    echo "请用 root 运行: sudo bash $0"
    exit 1
fi

echo "========================================"
echo "   ProxyCore Caddy 网关配置脚本"
echo "========================================"
echo ""

# ==============================
# 1. 安装 Caddy
# ==============================
log_step "安装 Caddy..."
if command -v caddy &>/dev/null; then
    log_success "Caddy 已安装，跳过"
else
    if command -v apt-get &>/dev/null; then
        apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl
        curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
            | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
        curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
            | tee /etc/apt/sources.list.d/caddy-stable.list
        apt-get update -qq
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
fi

# ==============================
# 2. 收集配置信息
# ==============================
echo ""
echo "--- 网关配置 ---"
echo ""

# 域名（Caddy 会自动申请 HTTPS 证书）
read -rp "域名（如 proxy.example.com，留空使用 IP 直接访问）: " DOMAIN
DOMAIN="${DOMAIN// /}"

# 联系邮箱（Let's Encrypt 需要）
ACME_EMAIL=""
if [[ -n "$DOMAIN" ]]; then
    read -rp "邮箱（用于 Let's Encrypt 证书通知）: " ACME_EMAIL
    ACME_EMAIL="${ACME_EMAIL// /}"
fi

# 后端服务器列表
declare -a BACKENDS=()
echo ""
echo "添加后端 ProxyCore 实例（格式: IP:端口，如 1.2.3.4:8081）"
echo "输入完所有后端后，直接回车结束"
while true; do
    read -rp "后端地址 [已添加 ${#BACKENDS[@]} 个，回车结束]: " backend
    backend="${backend// /}"
    [[ -z "$backend" ]] && break
    if [[ ! "$backend" =~ ^[a-zA-Z0-9._-]+:[0-9]+$ ]]; then
        echo "  格式不正确，请输入 host:port 格式"
        continue
    fi
    BACKENDS+=("$backend")
    log_success "已添加: $backend"
done

if [[ ${#BACKENDS[@]} -eq 0 ]]; then
    log_error "至少需要添加一个后端地址"
fi

# ==============================
# 3. 生成 Caddyfile
# ==============================
log_step "生成 Caddyfile..."

CADDYFILE="/etc/caddy/Caddyfile"
mkdir -p /etc/caddy

BACKEND_LIST="${BACKENDS[*]}"

if [[ -n "$DOMAIN" ]]; then
    SITE_ADDR="$DOMAIN"
else
    SITE_ADDR=":80"
fi

# 用 printf 写文件，避免 heredoc 变量展开问题
{
    # 全局块（有邮箱才写）
    if [[ -n "$ACME_EMAIL" ]]; then
        printf '{\n    email %s\n}\n\n' "${ACME_EMAIL}"
    fi

    printf '%s {\n\n' "${SITE_ADDR}"
    printf '    reverse_proxy %s {\n\n' "${BACKEND_LIST}"
    printf '        # 负载均衡：最少连接（适合流式长连接）\n'
    printf '        lb_policy least_conn\n\n'
    printf '        # 被动健康探测：失败 3 次标记为不可用，30s 后重试\n'
    printf '        fail_duration 30s\n'
    printf '        max_fails 3\n'
    printf '        unhealthy_status 5xx\n'
    printf '        unhealthy_latency 10s\n\n'
    printf '        # 主动健康探测（如后端有 /health 路由可取消注释）\n'
    printf '        # health_uri /health\n'
    printf '        # health_interval 15s\n'
    printf '        # health_timeout 5s\n\n'
    printf '        # 流式响应（SSE / 大模型输出）禁用缓冲\n'
    printf '        flush_interval -1\n\n'
    printf '        # 透传真实 IP\n'
    printf '        header_up X-Real-IP {remote_host}\n'
    printf '        header_up X-Forwarded-For {remote_host}\n'
    printf '        header_up X-Forwarded-Proto {scheme}\n'
    printf '    }\n'
    printf '}\n'
} > "$CADDYFILE"

log_success "Caddyfile 已生成: ${CADDYFILE}"

# ==============================
# 4. 验证并重载
# ==============================
log_step "验证 Caddy 配置..."
caddy validate --config "$CADDYFILE" || log_error "Caddy 配置有误，请检查 ${CADDYFILE}"
log_success "配置验证通过"

log_step "重载/启动 Caddy..."
if systemctl is-active --quiet caddy; then
    systemctl reload caddy
else
    systemctl start caddy
fi
log_success "Caddy 已启动"

# ==============================
# 5. 开放防火墙
# ==============================
log_step "开放防火墙端口..."
if command -v ufw &>/dev/null; then
    ufw allow 80/tcp  2>/dev/null || true
    ufw allow 443/tcp 2>/dev/null || true
    log_success "ufw: 80/443 已开放"
elif command -v firewall-cmd &>/dev/null; then
    firewall-cmd --permanent --add-service=http  2>/dev/null || true
    firewall-cmd --permanent --add-service=https 2>/dev/null || true
    firewall-cmd --reload 2>/dev/null || true
    log_success "firewalld: http/https 已开放"
fi

# ==============================
# 6. 输出汇总
# ==============================
echo ""
echo "========================================"
echo -e "${GREEN}  Caddy 网关配置完成！${NC}"
echo "========================================"
echo ""
echo -e "${CYAN}后端列表：${NC}"
for backend in "${BACKENDS[@]}"; do
    echo "  - ${backend}"
done
echo ""
echo -e "${CYAN}访问地址：${NC}"
if [[ -n "$DOMAIN" ]]; then
    echo "  https://${DOMAIN}                    （自动 HTTPS）"
    echo "  https://${DOMAIN}/management.html   （管理面板）"
else
    HOST_IP=$(hostname -I | awk '{print $1}')
    echo "  http://${HOST_IP}"
    echo "  http://${HOST_IP}/management.html"
fi
echo ""
echo -e "${CYAN}健康探测说明：${NC}"
echo "  被动探测：后端连续失败 3 次自动剔除，30s 后重试恢复"
echo "  主动探测：如需启用，编辑 ${CADDYFILE} 取消 health_uri 注释"
echo ""
echo -e "${CYAN}管理命令：${NC}"
echo "  查看状态  : systemctl status caddy"
echo "  重载配置  : systemctl reload caddy"
echo "  配置文件  : ${CADDYFILE}"
echo ""
echo -e "${YELLOW}提示：新增/删除后端，编辑 ${CADDYFILE} 的 reverse_proxy 行，再 systemctl reload caddy 即可${NC}"
echo ""
