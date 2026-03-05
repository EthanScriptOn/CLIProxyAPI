#!/bin/bash

# Claude API 反向代理一键部署脚本
# 服务: Caddy + CLIProxyAPI

PROXY_API_KEY="sk-proxy-eoEgBNSGZ6eWYkYGSJlUaOFk9ZmTRTTQnfZyoTxGQ"
CLI_PROXY_DIR="/root/cliproxyapi"

# 动态读取域名
while true; do
    read -rp "请输入你的域名（例如 example.com）: " DOMAIN
    DOMAIN="${DOMAIN// /}"   # 去除空格
    if [[ -n "$DOMAIN" ]]; then
        break
    fi
    echo "域名不能为空，请重新输入。"
done
echo "使用域名: $DOMAIN"

# ==============================
# 工具函数
# ==============================
ask_reinstall() {
    local name=$1
    read -p "$name 已安装，是否重新安装？(y/N): " choice
    [[ "$choice" == "y" || "$choice" == "Y" ]]
}

# ==============================
# 1. 安装 Caddy
# ==============================
echo "=== 1. 检查 Caddy ==="
if command -v caddy &>/dev/null; then
    if ask_reinstall "Caddy"; then
        apt remove -y caddy
    else
        echo "跳过 Caddy 安装"
    fi
fi

if ! command -v caddy &>/dev/null; then
    echo "安装 Caddy..."
    apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list
    apt update
    apt install -y caddy
    echo "Caddy 安装完成"
fi

# ==============================
# 2. 安装 CLIProxyAPI
# ==============================
echo "=== 2. 检查 CLIProxyAPI ==="
if [ -f "$CLI_PROXY_DIR/cli-proxy-api" ]; then
    if ask_reinstall "CLIProxyAPI"; then
        rm -rf "$CLI_PROXY_DIR"
    else
        echo "跳过 CLIProxyAPI 安装"
    fi
fi

if [ ! -f "$CLI_PROXY_DIR/cli-proxy-api" ]; then
    echo "安装 CLIProxyAPI..."
    CLI_PROXY_DIR="$CLI_PROXY_DIR" bash "$(dirname "$0")/cliproxyapi-installer.sh"
    echo "CLIProxyAPI 安装完成"
fi

# ==============================
# 3. 配置 CLIProxyAPI
# ==============================
echo "=== 3. 配置 CLIProxyAPI ==="
cat > $CLI_PROXY_DIR/config.yaml <<EOF
host: "0.0.0.0"
port: 8080

api-keys:
  - "$PROXY_API_KEY"

auth-dir: "/root/.cli-proxy-api"

debug: false
logging-to-file: true
logs-max-total-size-mb: 1000
request-retry: 3
max-retry-credentials: 2
max-retry-interval: 30
EOF

# ==============================
# 4. 配置 Caddyfile
# ==============================
echo "=== 4. 配置 Caddyfile ==="
cat > /etc/caddy/Caddyfile <<EOF
$DOMAIN {
    handle {
        reverse_proxy 127.0.0.1:8080
    }
}
EOF

# ==============================
# 5. 开放防火墙端口
# ==============================
echo "=== 5. 开放防火墙端口 ==="
ufw allow 22/tcp
ufw allow 80/tcp
ufw allow 443/tcp
ufw --force enable

# ==============================
# 6. 启动 Caddy
# ==============================
echo "=== 6. 启动 Caddy ==="
systemctl enable caddy
systemctl restart caddy

# ==============================
# 7. 启动 CLIProxyAPI
# ==============================
echo "=== 7. 启动 CLIProxyAPI ==="
systemctl --user enable cliproxyapi.service
systemctl --user start cliproxyapi.service
systemctl --user status cliproxyapi.service --no-pager

echo ""
echo "=== 部署完成 ==="
echo "代理地址: https://$DOMAIN"
echo "API Key:  $PROXY_API_KEY"
echo "如需授权 Claude 账号，请运行: bash /root/claude-auth.sh"
