# CLIProxyAPI 部署流程

## 前置条件

- 一台美国服务器（已有 SSH 访问权限）
- 一个域名，已接入 Cloudflare（橙色云朵代理开启）
- 本机已安装 Go、Node.js、npm

---

## Step 1：本地构建

在本机项目根目录执行，编译前端 + 交叉编译 Linux 二进制，产物输出到 `dist/`：

```bash
bash deploy.sh
```

完成后会提示产物路径，类似：
```
dist/CLIProxyAPI_20260306_linux_amd64.tar.gz
```

---

## Step 2：上传文件到服务器

将构建产物和部署脚本上传到服务器：

```bash
scp dist/CLIProxyAPI_*_linux_amd64.tar.gz root@<服务器IP>:/root/cliproxyapi/
scp cliproxyapi-installer.sh root@<服务器IP>:/root/cliproxyapi/
scp claude-proxy-setup.sh root@<服务器IP>:/root/cliproxyapi/
scp claude-auth.sh root@<服务器IP>:/root/cliproxyapi/
scp claude-verify.sh root@<服务器IP>:/root/cliproxyapi/
```

> 也可以用 `server-fetch.sh` 让服务器直接从 GitHub 拉取（需要服务器能访问 GitHub）：
> ```bash
> scp server-fetch.sh root@<服务器IP>:/root/
> ssh root@<服务器IP> "bash /root/server-fetch.sh"
> ```

---

## Step 3：服务器端部署

SSH 登录服务器，执行一键部署脚本：

```bash
ssh root@<服务器IP>
bash /root/cliproxyapi/claude-proxy-setup.sh
```

脚本会：
1. 安装 Caddy
2. 安装 CLIProxyAPI
3. 写入配置文件（输入你的域名）
4. 配置 Caddyfile 反向代理
5. 开放防火墙 22/80/443 端口
6. 启动 Caddy 和 CLIProxyAPI 服务

---

## 完成

部署成功后：

- 代理地址：`https://<你的域名>`
- API Key：`sk-proxy-eoEgBNSGZ6eWYkYGSJlUaOFk9ZmTRTTQnfZyoTxGQ`

---

## 常用运维命令

```bash
# 停止服务
systemctl --user stop cliproxyapi.service && sudo systemctl stop caddy

# 启动服务
sudo systemctl start caddy && systemctl --user start cliproxyapi.service

# 查看服务状态
systemctl --user status cliproxyapi.service
sudo systemctl status caddy

# 查看日志
journalctl --user -u cliproxyapi.service -f
sudo journalctl -u caddy -f
```
