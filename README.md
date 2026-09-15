# detour — 本地模型 API 转发小工具

公司网络封了模型 API（opencode / pi 直连不通），又不想一直开梯子全局模式。
detour 是一个跑在本地的小型转发服务：把模型客户端的地址指到本地，
detour 收到请求后通过本地梯子转发到真实 API。梯子保持规则模式即可，
只有模型 API 的流量走代理。

```
pi / opencode ──http://127.0.0.1:8787──▶ detour ──梯子(127.0.0.1:7897)──▶ opencode.ai / api.openai.com
```

支持三种 API 形态（v0.3.0 起），转发时自动拼接上游路径、不会重复 `/v1`：

| API | 客户端请求 | 转发到上游 |
|---|---|---|
| OpenAI Responses | `/responses` | `<upstream>/responses` |
| OpenAI Chat Completions | `/chat/completions` | `<upstream>/chat/completions` |
| Anthropic Messages | `/v1/messages` | `<upstream>/v1/messages` |

---

## 目录

- [一、构建与启动](#一构建与启动)
- [二、修改代理 IP / 端口（三种方式）](#二修改代理-ip--端口三种方式)
- [三、给 pi 用（pi 插件，配好 key 直接用）](#三给-pi-用pi-插件配好-key-直接用)
- [四、给 opencode 用](#四给-opencode-用)
- [五、对接其他 OpenAI 兼容服务](#五对接其他-openai-兼容服务)
- [六、常用命令](#六常用命令)
- [故障排查](#故障排查)
- [安全](#安全)
- [工作原理](#工作原理)

---

## 一、构建与启动

### 1. 构建二进制（可选，已有二进制可跳过）

```bash
go build -o detour .      # 单文件静态二进制，零依赖，拷到公司电脑直接跑
```

### 2. 启动 detour

```bash
./detour.sh start         # 一键启动，默认对接 opencode go
```

启动后：

```
detour 0.3.0 listening on http://127.0.0.1:8787
  upstream: https://opencode.ai/zen/go/v1/
  proxy: http://127.0.0.1:7897
```

`detour.sh` 管理命令：

```bash
./detour.sh start      # 启动（后台运行，日志写 detour.log）
./detour.sh stop       # 停止
./detour.sh restart    # 重启
./detour.sh status     # 查看状态（pid / 上游 / 最近日志）
./detour.sh check      # 体检代理链路（梯子→上游），不通会报错
./detour.sh log        # 跟踪日志（tail -f）
./detour.sh build      # 重新编译
```

也可以直接前台运行：

```bash
./detour                          # 前台启动，Ctrl+C 退出
./detour -config detour.json      # 使用配置文件启动
```

---

## 二、修改代理 IP / 端口（三种方式）

梯子代理地址（`-proxy`）和本地监听地址（`-listen`）、上游地址（`-upstream`）
都有三种配置方式，**优先级：命令行参数 > 配置文件 > 环境变量 > 内置默认值**。

### 方式一：环境变量（最常用，改端口最快）

```bash
export DETOUR_PROXY=http://127.0.0.1:7897      # 代理地址（http / socks5 均可）
export DETOUR_LISTEN=127.0.0.1:8787            # 本地监听地址
export DETOUR_UPSTREAM=https://opencode.ai/zen/go/v1/   # 上游 API 地址
./detour.sh start                              # 设置后再启动
```

**常见梯子端口对照：**

| 梯子软件 | 代理地址（`DETOUR_PROXY`） |
|---|---|
| Clash / Clash Verge | `http://127.0.0.1:7897` |
| Clash（混合端口） | `http://127.0.0.1:7890` |
| V2Ray / V2rayN | `http://127.0.0.1:10809` 或 `socks5://127.0.0.1:10808` |
| sing-box | `socks5://127.0.0.1:1080` |
| Surge / Shadowrocket | `socks5://127.0.0.1:6153`（HTTP 见软件设置） |

不想走代理时用 `direct`：`export DETOUR_PROXY=direct`。

### 方式二：配置文件（`detour.json`）

参考 `detour.example.json`，所有参数写进文件：

```json
{
  "listen": "127.0.0.1:8787",
  "upstream": "https://opencode.ai/zen/go/v1/",
  "proxy": "http://127.0.0.1:7897",
  "verbose": false
}
```

```bash
./detour.sh start        # 启动时自动读取同目录 detour.json
./detour -config detour.json   # 或显式指定配置文件
```

> ⚠️ `detour.json` 已被 `.gitignore` 忽略，不会提交到仓库；里面可能含代理
> 认证信息，不要改名后提交。

### 方式三：命令行参数（临时/测试用）

```bash
./detour -listen 127.0.0.1:8787 \
         -upstream https://opencode.ai/zen/go/v1/ \
         -proxy socks5://127.0.0.1:1080
./detour -proxy direct    # 直连（测试用）
```

### 配置项一览

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `-listen` / `DETOUR_LISTEN` | `127.0.0.1:8787` | 本地监听地址（只监听本机，勿改 0.0.0.0） |
| `-upstream` / `DETOUR_UPSTREAM` | `https://opencode.ai/zen/go/v1/` | 真实 API 地址 |
| `-proxy` / `DETOUR_PROXY` | `http://127.0.0.1:7897` | 梯子代理，`http://` / `socks5://` / `direct` |
| `-v` | 关 | 详细日志（打印请求头，Authorization 脱敏） |
| `-check` | — | 体检代理链路后退出 |
| `-config` | — | 指定配置文件 |
| `-tls-cert` / `-tls-key` | 无 | 用 HTTPS 提供本地服务（个别 SDK 不接受 http） |

---

## 三、给 pi 用（pi 插件，配好 key 直接用）

[pi](https://pi.dev) 自带 opencode-go provider（模型目录 + 密钥体系都是现成的）。
公司网络直连不通时，装这个插件即可：插件自动把 opencode-go 的模型地址指到
本地 detour，detour 没在跑会自动拉起，**密钥沿用原来的 `OPENCODE_API_KEY`
或 `/login opencode-go`——不用配置任何模型和参数**。

### 1. 安装插件（二选一）

```bash
# 方式一：复制到全局扩展目录（推荐）
cp pi-extension/index.ts ~/.pi/agent/extensions/detour.ts

# 方式二：作为 pi 包安装（以后更新仓库自动同步）
pi install git:github.com/ross/detour
```

### 2. 配置密钥

```bash
export OPENCODE_API_KEY=sk-...     # 或进入 pi 后 /login opencode-go
```

### 3. 启动 pi

```bash
pi
```

启动后看到 `[Extensions]` 里有 `detour.ts`、并提示 `detour 就绪` 即成功。
然后 `/model` 选 opencode-go 模型（kimi-k3、deepseek-v4-pro、gpt-5.6-luna、
minimax-m3 等），直接开聊。

### 插件行为

- **直连判断（默认 auto）**：启动时探测 opencode.ai 能否直连——
  - 能直连（家里/非公司网络）：**不走代理、不拉起 detour**，直接用原始地址；
  - 不能直连（公司网络）：自动改走本地 detour 并拉起 detour。
  - 可用 `DETOUR_MODE=always`（强制走代理）或 `DETOUR_MODE=never`（强制直连）覆盖。
- **只改 opencode-go**：插件只覆盖 `opencode-go` 这一个 provider 的 baseUrl
  （pi 的 registerProvider 按 provider 隔离），**`qwen-token-plan-cn`、`deepseek`
  等 provider 保持原有地址直连，永远不经过 detour/梯子**；要额外转发必须显式写
  `DETOUR_EXTRA_PROVIDERS`。
- **全局代理是唯一例外**：如果进程里有 `HTTP_PROXY`/`HTTPS_PROXY`（或
  `settings.json` 的 `httpProxy`），pi 会让**所有** provider 都走它（qwen 也一样）。
  此时插件会把本地 detour 与 `DETOUR_DIRECT_HOSTS` 写进 `NO_PROXY` 保持直连，
  并在会话启动时告警。用 `/detour env` 可查看。
- **自动拉起 detour**：detour 没在跑时，自动用 `detour.sh start` 拉起来
  （监听、上游、代理参数跟随你的 detour 配置）。`PI_DETOUR_AUTO_START=0` 可关闭。

### 插件内管理命令

```
/detour            # 状态：直连/转发判定、端口、链路、配置
/detour start      # 启动 detour
/detour stop       # 停止 detour
/detour restart    # 重启 detour
/detour check      # 链路体检（直连可达性 + detour 链路）
/detour env        # 打印生效配置
/detour key        # 密钥配置指引
```

### 插件环境变量（全部可选）

| 变量 | 默认值 | 说明 |
|---|---|---|
| `OPENCODE_API_KEY` | — | OpenCode Go 密钥（pi 内置认证，也可 `/login`） |
| `DETOUR_MODE` | `auto` | `auto` 直连探测 / `always` 强制走 detour / `never` 强制直连 |
| `DETOUR_BASE_URL` | `http://127.0.0.1:8787` | 本地 detour 地址（完整 URL） |
| `DETOUR_LISTEN` | `127.0.0.1:8787` | 兼容 detour.sh 的 host:port 写法 |
| `DETOUR_UPSTREAM` | `https://opencode.ai/zen/go/v1/` | 实际上游地址 |
| `DETOUR_PROXY` | `http://127.0.0.1:7897` | 梯子代理（改端口在这里） |
| `DETOUR_BIN` | 自动查找 | detour 二进制路径（查找顺序：本变量 → 插件同级 ../detour → ~/self-git/detour/detour → PATH） |
| `DETOUR_EXTRA_PROVIDERS` | 空 | 逗号分隔的额外 provider 名，同样指到本地 detour |
| `DETOUR_DIRECT_HOSTS` | 空 | 逗号分隔域名。存在全局代理时写进 `NO_PROXY` 强制直连（如 qwen token-plan 的域名） |
| `PI_DETOUR_AUTO_START` | 开 | 设 `0` 关闭自动拉起 detour |

> 详细说明见 [pi-extension/README.md](pi-extension/README.md)。

---

## 四、给 opencode 用

编辑 `~/.config/opencode/opencode.json`，把 `opencode-go` 的 baseURL 指向本地：

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "opencode-go": {
      "options": {
        "baseURL": "http://127.0.0.1:8787"
      }
    }
  }
}
```

如果之前用 `/connect` 登录过 opencode go，key 仍然生效；否则在 `options` 里加
`"apiKey": "你的 OPENCODE_API_KEY"`。

---

## 五、对接其他 OpenAI 兼容服务

把 detour 的 `-upstream` 换成对应供应商的真实地址，客户端 baseURL 指到本地：

| 供应商 | API | `-upstream` |
|---|---|---|
| OpenAI | Responses / Chat | `https://api.openai.com/v1` |
| DeepSeek | Chat | `https://api.deepseek.com/v1` |
| Kimi / Moonshot | Chat | `https://api.moonshot.cn/v1` |
| 智谱 GLM | Chat | `https://open.bigmodel.cn/api/paas/v4` |
| 其他 OpenAI 兼容 | Chat | 供应商文档里的 base URL |

例如 DeepSeek：

```json
{
  "provider": {
    "deepseek": {
      "options": {
        "baseURL": "http://127.0.0.1:8787"
      }
    }
  }
}
```

路径自动拼接：客户端发 `/responses` 或 `/chat/completions`，detour 转发为
`<upstream>/responses` 或 `<upstream>/chat/completions`；即使客户端 baseURL
配成 `http://127.0.0.1:8787/v1`（带路径）也不会拼重（v0.3.0 起连
`/v1/messages` 也不会重复）。

---

## 六、常用命令

```bash
./detour.sh start                       # 后台启动
./detour.sh status                      # 状态
./detour.sh check                       # 体检代理链路
./detour.sh log                         # 跟踪日志
./detour.sh stop                        # 停止
./detour -config detour.json            # 用配置文件启动
./detour -proxy socks5://127.0.0.1:1080 # 换 socks5 代理
./detour -v                             # 详细日志（请求头，Authorization 脱敏）
./detour -check                         # 命令行体检，然后退出
```

---

## 故障排查

- **`detour.sh check` / `/detour check` 失败**：先确认梯子在跑、节点活着
  （Clash 里测一下延迟）。规则模式下确认模型 API 域名走代理节点
  （Clash Verge 的"规则"页面可以看到）。
- **请求返回 502**：detour 连不上上游，看日志里的 `upstream error` 一行；
  多半是梯子没开会话、节点失效、或 `DETOUR_PROXY` 端口写错。
- **pi 提示"链路异常"**：同上，检查梯子与代理规则。
- **opencode 报 models.dev 相关错误**：opencode 会从 models.dev 拉模型元数据，
  如果它也被墙，给 models.dev 单独配代理规则即可。
- **梯子规则模式**：确保 `opencode.ai`、`api.openai.com` 等域名命中代理规则；
  或在 Clash 里把这些域名加进代理组。detour 的意义就是让你不用开全局。

---

## 安全

- 默认只监听 `127.0.0.1`，因为 detour 会原样透传你的 API key。
  不要改成 `0.0.0.0`，除非你知道自己在干什么。
- `detour.json`（含代理认证信息）与 `detour.log`（请求日志）已被
  `.gitignore` 忽略，不会提交到仓库。
- 日志中请求头打印时 Authorization / X-Api-Key 自动脱敏。

---

## 工作原理

- 用 Go 标准库实现，零依赖，单个静态二进制，拷到公司电脑直接跑（不用装 Go）。
- HTTP 代理走标准 CONNECT 隧道；SOCKS5 是内置的最小实现，
  域名始终交给代理解析（ATYP=3），避免本地 DNS 污染。
- SSE 流式响应逐块透传，不缓冲，首 token 延迟和直连一致。
- 路径拼接（v0.3.0）：上游路径以 `/v1` 结尾且请求路径以 `/v1` 开头时不再重复
  拼接（如 Anthropic SDK 的 `/v1/messages` 配上游 `.../zen/go/v1` 会正确转成
  `.../zen/go/v1/messages` 而不是 `.../zen/go/v1/v1/messages`）。
- 请求日志：`POST /v1/messages -> https://opencode.ai/zen/go/v1/messages | 200 | 2.15s | 8.9 KB`