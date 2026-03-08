# Telegram Bot 配置教程

## 前置条件

- Telegram 账号
- myclaw 已编译（`make build`）

## 第一步：创建 Telegram Bot

1. 在 Telegram 中搜索 **@BotFather**，发送 `/newbot`
2. 按提示输入 Bot 名称（如 `My Claw Assistant`）
3. 输入 Bot 用户名（必须以 `bot` 结尾，如 `myclaw_bot`）
4. BotFather 会返回一个 **Bot Token**，格式如：
   ```
   1234567890:ABCdefGHIjklMNOpqrsTUVwxyz
   ```
5. 保存此 Token

## 第二步：配置 myclaw

### 方式一：配置文件

编辑 `~/.myclaw/config.json`：

```json
{
  "channels": {
    "telegram": {
      "enabled": true,
      "token": "1234567890:ABCdefGHIjklMNOpqrsTUVwxyz",
      "allowFrom": [],
      "proxy": "",
      "rootDir": ""
    }
  }
}
```

### 方式二：环境变量

```bash
export MYCLAW_TELEGRAM_TOKEN="1234567890:ABCdefGHIjklMNOpqrsTUVwxyz"
```

## 第三步：配置参数说明

| 参数 | 类型 | 说明 |
|------|------|------|
| `enabled` | bool | 是否启用 Telegram 通道 |
| `token` | string | BotFather 提供的 Bot Token |
| `allowFrom` | []string | 允许的用户 ID 列表（空 = 允许所有人） |
| `proxy` | string | 代理地址（如 `socks5://127.0.0.1:1080`），国内网络需要 |
| `rootDir` | string | Telegram 资源目录，默认是 `<agent.workspace>/.telegram`，其中包含 `slashes/` 和 `handlers/` |

### 获取你的用户 ID

1. 在 Telegram 中搜索 **@userinfobot**，发送任意消息
2. 它会返回你的 User ID（纯数字）
3. 将 ID 添加到 `allowFrom` 限制只有你能使用：
   ```json
   "allowFrom": ["123456789"]
   ```

## 第四步：启动并测试

```bash
# 启动 gateway
make gateway

# 或直接运行
./myclaw gateway
```

日志中看到以下内容表示成功：
```
[telegram] authorized as @myclaw_bot
[telegram] polling started
```

在 Telegram 中搜索你的 Bot 用户名，发送消息即可测试。

## 代理配置（国内用户）

国内无法直接访问 Telegram API，需要配置代理：

```json
{
  "channels": {
    "telegram": {
      "enabled": true,
      "token": "your-token",
      "proxy": "socks5://127.0.0.1:1080"
    }
  }
}
```

支持的代理协议：`socks5://`、`http://`、`https://`

## 常见问题

**Q: Bot 没有响应？**
- 检查日志是否有 `[telegram] authorized as @xxx`
- 确认 API Key 已配置（`myclaw status`）
- 如果在国内，确认代理配置正确

**Q: 收到 "Sorry, I encountered an error" 回复？**
- 检查日志中的 `[gateway] agent error` 信息
- 确认 API 代理/密钥可用

**Q: 如何限制只有自己能用？**
- 获取你的 User ID，添加到 `allowFrom` 列表
