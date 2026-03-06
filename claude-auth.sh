#!/bin/bash

# Claude OAuth 授权脚本
# 可单独执行，授权失败时重试无需重装

CLI_PROXY_DIR="/root/proxycore"
SERVER_IP=$(curl -s ifconfig.me)

if [ ! -f "$CLI_PROXY_DIR/proxycore" ]; then
    echo "错误：ProxyCore 未安装，请先运行 claude-proxy-setup.sh"
    exit 1
fi

cd $CLI_PROXY_DIR

while true; do
    echo ""
    echo "========================================="
    echo "Claude 登录"
    echo "========================================="
    echo "请在本机新开一个终端，执行以下命令建立SSH隧道："
    echo ""
    echo "  ssh -L 54545:localhost:54545 root@$SERVER_IP"
    echo ""
    echo "隧道建好后按回车继续..."
    read

    trap '' INT
    ./proxycore --claude-login --no-browser
    EXIT_CODE=$?
    trap - INT

    if [ $EXIT_CODE -eq 0 ]; then
        echo "登录成功"
        break
    else
        echo "登录失败或被中断"
        read -p "是否重试？(y/N): " retry
        if [[ "$retry" != "y" && "$retry" != "Y" ]]; then
            echo "退出"
            exit 1
        fi
    fi
done
