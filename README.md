# api-register-go

OpenAI 账号注册工具 — **⚡ 高并发 Go 版**

> 采用 Go 语言实现，goroutine 轻量并发，支持 50+ 账号同时注册，内存占用极低。  
> 无需安装任何环境，直接双击 `register.exe` 即可启动。

## 快速开始

1. 双击 `register.exe` 启动
2. 浏览器打开 `http://localhost:8899`
3. 粘贴账号列表 → 配置参数 → 开始注册

## 账号格式

每行一个账号，支持两种格式：

```
# 格式一：Outlook 密码认证
邮箱----密码

# 格式二：Outlook XOAUTH2 认证（推荐，更稳定）
邮箱----密码----client_id----refresh_token

# 示例
DeannaSmith1590@outlook.com----MyPass123
StephanieWilkins6224@outlook.com----MyPass456----your_client_id----your_refresh_token
```

## 参数说明

| 参数 | 说明 | 默认 |
|------|------|------|
| 并发数 | 同时注册的账号数量 | 1 |
| 代理 | HTTP 代理地址，如 `http://127.0.0.1:7890` | 无 |
| 跳过已完成 | 跳过 `tokens/` 中已有结果的账号 | 开启 |
| 注册转登录 | 已注册账号直接走登录流程刷新 Token | 关闭 |

## 域名邮箱模式

在 Web 界面配置 IMAP 服务后，系统自动监听收件箱分发验证码，支持高并发 catch-all：

- **IMAP 主机**：如 `mail.yourdomain.com`
- **端口**：`993`（TLS）
- **用户名/密码**：catch-all 邮箱的登录凭证

## XOAUTH2 说明

Outlook 官方已关闭普通密码 IMAP 认证，推荐使用 XOAUTH2：

1. 在 [Microsoft Azure](https://portal.azure.com) 注册应用，获取 `client_id`
2. 通过 OAuth2 授权流程获取 `refresh_token`
3. 账号格式填写 `邮箱----密码----client_id----refresh_token`

程序会**自动优先使用 XOAUTH2**，失败时自动回退到密码认证。

## 从源码编译

需要 Go 1.21+：

```bash
go build -o register.exe .
```

## 结果目录

注册成功的账号保存在 `tokens/<邮箱>.json`，包含 `access_token`、`refresh_token`、`expires_at` 等字段。
