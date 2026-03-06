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

## 域名被 GFW 封锁后的更换流程

当域名遭到 GFW SNI 拦截时，按以下步骤重新申请域名并接入 Cloudflare：

### 1. 申请新域名
- 去西部数码（或其他注册商）购买一个新域名

### 2. 接入 Cloudflare
1. 登录 [cloudflare.com](https://cloudflare.com) → Add a Site → 输入新域名
2. 选 Free 计划
3. Cloudflare 会给你两个 NS 地址（如 `anahi.ns.cloudflare.com`）
4. 去西部数码后台，把该域名的 **DNS 服务器** 改成 Cloudflare 给的那两个
5. 等待生效（通常几分钟到几小时），Cloudflare 显示 Active 即可

### 3. 配置 DNS 记录
在 Cloudflare → DNS → Records → Add record：
- Type: `A`
- Name: `@`
- IPv4: 服务器 IP（如 `144.202.99.107`）
- Proxy status: **Proxied（橙色云朵）** ← 关键，隐藏真实 IP

### 4. 更新服务器 Caddy 配置

```bash
sudo nano /etc/caddy/Caddyfile
```

将域名改为新域名：

```
新域名.xyz {
    handle {
        reverse_proxy 127.0.0.1:8080
    }
}
```

重启 Caddy：

```bash
sudo systemctl reload caddy
```

### 5. 验证

```bash
curl https://新域名
```

返回如下内容即成功：

```json
{"endpoints":["POST /v1/chat/completions","POST /v1/completions","GET /v1/models"],"message":"CLI Proxy API Server"}
```

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
