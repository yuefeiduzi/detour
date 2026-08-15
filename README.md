# detour — 本地模型 API 转发小工具

公司网络封了模型 API（opencode 直连不通），又不想一直开梯子全局模式。
detour 是一个跑在本地的小型转发服务：把 opencode 的模型地址指到本地，
detour 收到请求后通过本地梯子转发到真实 API。梯子保持规则模式即可，
只有模型 API 的流量走代理。

```
opencode ──http://127.0.0.1:8787──▶ detour ──梯子(127.0.0.1:7897)──▶ api.anthropic.com
```

## 快速开始

```bash
go build -o detour .        # 在能上网的机器上构建（或直接下载二进制）
./detour                    # 默认配置开箱即用（见下）
```

默认配置已经匹配 Clash Verge 的混合端口：

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `-listen` | `127.0.0.1:8787` | 本地监听地址 |
| `-upstream` | `https://api.anthropic.com/v1` | 真实 API 地址（供应商 base URL） |
| `-proxy` | `http://127.0.0.1:7897` | 梯子代理地址，支持 `http://` 和 `socks5://`，`direct` 表示直连 |

其他梯子（V2Ray / sing-box / Surge 等）把端口改成自己的就行：
`./detour -proxy socks5://127.0.0.1:1080`。

## 配置 opencode

编辑 `~/.config/opencode/opencode.json`（或项目里的 `opencode.json`），
把模型供应商的 `baseURL` 指向本地：

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "anthropic": {
      "options": {
        "baseURL": "http://127.0.0.1:8787"
      }
    }
  }
}
```

其他供应商同理，只要把 `-upstream` 换成供应商的真实地址：

| 供应商 | `-upstream` |
|---|---|
| Anthropic | `https://api.anthropic.com/v1` |
| OpenAI | `https://api.openai.com/v1` |
| DeepSeek | `https://api.deepseek.com/v1` |
| 其他 OpenAI 兼容 | 供应商文档里的 base URL |

路径会自动拼接：opencode 发 `POST /messages`，detour 转发为
`POST https://api.anthropic.com/v1/messages`。就算你在 opencode 里把
baseURL 配成了 `http://127.0.0.1:8787/v1`（带路径）也不会拼重。

## 配置文件

所有参数也可以写进 `detour.json`（参考 `detour.example.json`）：

```json
{
  "listen": "127.0.0.1:8787",
  "upstream": "https://api.anthropic.com/v1",
  "proxy": "http://127.0.0.1:7897",
  "verbose": false
}
```

优先级：命令行参数 > 配置文件 > 内置默认值。

## 常用命令

```bash
./detour                      # 启动（Ctrl+C 退出）
./detour -check               # 体检：验证代理链路通不通，然后退出
./detour -v                   # 详细日志（打印请求头，Authorization 会脱敏）
./detour -config detour.json  # 使用配置文件
./detour -listen 0.0.0.0:8787 # 监听所有网卡（⚠️ 会暴露你的 API key，别这么干）
./detour -tls-cert cert.pem -tls-key key.pem  # 用 HTTPS 提供本地服务（个别 SDK 不接受 http）
```

## 排查

- **`./detour -check` 失败**：先确认梯子在跑、节点活着（Clash 里测一下延迟）。
  规则模式下确认模型 API 域名走代理节点（Clash Verge 的"规则"页面可以看到）。
- **请求返回 502**：detour 连不上上游，看日志里的 `upstream error` 一行。
- **opencode 报 models.dev 相关错误**：opencode 会从 models.dev 拉模型元数据，
  如果它也被墙，在 opencode 配置里加 `"models.dev": true` 之外，可考虑
  给 models.dev 单独配代理规则。
- **梯子规则模式**：确保 `api.anthropic.com` 等域名命中代理规则；或者
  在 Clash 里把这些域名加进代理组。detour 的意义就是让你不用开全局。

## 安全

默认只监听 `127.0.0.1`，因为 detour 会原样透传你的 API key。
不要改成 `0.0.0.0`，除非你知道自己在干什么。

## 工作原理

- 用 Go 标准库实现，零依赖，单个静态二进制，拷到公司电脑直接跑（不用装 Go）。
- HTTP 代理走标准 CONNECT 隧道；SOCKS5 是内置的最小实现，
  域名始终交给代理解析（ATYP=3），避免本地 DNS 污染。
- SSE 流式响应（`text/event-stream`）逐块透传，不缓冲，首 token 延迟和直连一致。
- 请求日志：`POST /v1/messages -> https://api.anthropic.com/v1/messages | 401 | 730ms | 106 B`
