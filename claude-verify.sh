#!/bin/bash

# CLIProxyAPI 验证脚本
# 授权完成后运行，验证服务是否正常工作

PROXY_API_KEY="sk-proxy-eoEgBNSGZ6eWYkYGSJlUaOFk9ZmTRTTQnfZyoTxGQ"

echo "=== 验证 CLIProxyAPI 服务 ==="
RESPONSE=$(curl -s http://localhost:8080/v1/chat/completions \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $PROXY_API_KEY" \
    -d '{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"你好，回复OK即可"}]}')

echo "$RESPONSE" | head -c 500
echo ""

if echo "$RESPONSE" | grep -q '"content"'; then
    echo "========================================="
    echo "✅ 服务可用"
    echo "========================================="
else
    echo "========================================="
    echo "❌ 服务不可用"
    echo "========================================="
fi
