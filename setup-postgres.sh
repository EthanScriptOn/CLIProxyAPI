#!/bin/bash

# PostgreSQL 安装 + 配置脚本 (Ubuntu 24.04 LTS)
# 幂等运行：已安装则跳过安装，直接配置
# 固定配置：数据库/用户/密码均为 proxycore，开放远程访问
# 用法：sudo bash setup-postgres.sh

set -euo pipefail

GREEN='\033[0;32m'
CYAN='\033[0;36m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_step()    { echo -e "${CYAN}[STEP]${NC} $1"; }
log_success() { echo -e "${GREEN}[OK]${NC}   $1"; }
log_warn()    { echo -e "${YELLOW}[WARN]${NC} $1"; }

if [[ $EUID -ne 0 ]]; then
    echo "请用 root 运行: sudo bash $0"
    exit 1
fi

DB_NAME="proxycore"
DB_USER="proxycore"
DB_PASS="proxycore"

echo "========================================"
echo "   PostgreSQL 安装+配置脚本"
echo "========================================"
echo ""

# ==============================
# 1. 安装 PostgreSQL（幂等）
# ==============================
if command -v psql &>/dev/null && systemctl is-active --quiet postgresql 2>/dev/null; then
    log_success "PostgreSQL 已安装并运行，跳过安装"
else
    log_step "更新包索引..."
    apt-get update -qq

    log_step "安装 PostgreSQL..."
    apt-get install -y postgresql postgresql-contrib

    log_step "启动并设置开机自启..."
    systemctl enable postgresql
    systemctl start postgresql
    log_success "PostgreSQL 安装完成"
fi

# ==============================
# 2. 检测版本目录
# ==============================
log_step "检测 PostgreSQL 版本..."
PG_VERSION=$(ls /etc/postgresql/ | sort -V | tail -1)
if [[ -z "${PG_VERSION}" ]]; then
    echo "未找到 /etc/postgresql/ 目录，请确认 PostgreSQL 已正确安装"
    exit 1
fi
PG_CONF_DIR="/etc/postgresql/${PG_VERSION}/main"
PG_CONF="${PG_CONF_DIR}/postgresql.conf"
PG_HBA="${PG_CONF_DIR}/pg_hba.conf"
log_success "检测到版本 ${PG_VERSION}，配置目录: ${PG_CONF_DIR}"

# ==============================
# 3. 创建用户和数据库（幂等）
# ==============================
log_step "确认数据库用户和数据库..."
sudo -u postgres psql -v ON_ERROR_STOP=1 << SQL
DO \$\$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '${DB_USER}') THEN
        CREATE ROLE "${DB_USER}" WITH LOGIN PASSWORD '${DB_PASS}';
    ELSE
        ALTER ROLE "${DB_USER}" WITH PASSWORD '${DB_PASS}';
    END IF;
END
\$\$;

SELECT 'CREATE DATABASE "${DB_NAME}" OWNER "${DB_USER}"'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '${DB_NAME}')
\gexec

GRANT ALL PRIVILEGES ON DATABASE "${DB_NAME}" TO "${DB_USER}";
SQL
log_success "用户和数据库就绪"

# ==============================
# 4. 开启监听所有网卡
# ==============================
log_step "配置 listen_addresses = '*'..."
sed -i "s/^#*\s*listen_addresses\s*=.*/listen_addresses = '*'/" "${PG_CONF}"
log_success "postgresql.conf 已更新"

# ==============================
# 5. 配置 pg_hba.conf 允许远程连接
# ==============================
log_step "配置 pg_hba.conf..."
RULE="host    ${DB_NAME}    ${DB_USER}    0.0.0.0/0    scram-sha-256"
if ! grep -qF "${RULE}" "${PG_HBA}"; then
    echo "${RULE}" >> "${PG_HBA}"
    log_success "pg_hba.conf 规则已添加"
else
    log_success "pg_hba.conf 规则已存在，跳过"
fi

# ==============================
# 6. 重启 PostgreSQL
# ==============================
log_step "重启 PostgreSQL..."
systemctl restart postgresql
log_success "PostgreSQL 已重启"

# ==============================
# 7. 开放防火墙 5432
# ==============================
log_step "开放防火墙 5432 端口..."
if command -v ufw &>/dev/null; then
    ufw allow 5432/tcp
    log_success "ufw: 5432 已开放"
elif command -v firewall-cmd &>/dev/null; then
    firewall-cmd --permanent --add-port=5432/tcp
    firewall-cmd --reload
    log_success "firewalld: 5432 已开放"
else
    log_warn "未检测到 ufw/firewalld，请手动开放 5432 端口"
fi

# ==============================
# 8. 验证本机连接
# ==============================
log_step "验证本机连接..."
if PGPASSWORD="${DB_PASS}" psql -U "${DB_USER}" -d "${DB_NAME}" -h 127.0.0.1 -c "SELECT version();" 2>&1 | grep -q "PostgreSQL"; then
    log_success "本机连接验证通过 ✓"
else
    log_warn "本机连接验证失败，请检查日志: journalctl -u postgresql -n 50"
fi

# ==============================
# 9. 输出结果
# ==============================
SERVER_IP=$(curl -sf --max-time 5 https://api.ipify.org 2>/dev/null || hostname -I | awk '{print $1}')

echo ""
echo "========================================"
echo -e "${GREEN}  配置完成！${NC}"
echo "========================================"
echo ""
echo -e "${CYAN}数据库信息：${NC}"
echo "  数据库名  : ${DB_NAME}"
echo "  用户名    : ${DB_USER}"
echo "  密码      : ${DB_PASS}"
echo ""
echo -e "${CYAN}PGSTORE_DSN（填入所有 ProxyCore 实例）：${NC}"
echo ""
echo "  postgres://${DB_USER}:${DB_PASS}@${SERVER_IP}:5432/${DB_NAME}"
echo ""
echo -e "${YELLOW}⚠  Vultr 需要在控制台「Firewall」页面手动放行 5432 端口${NC}"
echo ""
echo "  验证远程连通性（在其他机器执行）："
echo "    telnet ${SERVER_IP} 5432"
echo ""
