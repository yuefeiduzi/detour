# detour — 本地模型 API 转发小工具

[![CI](https://github.com/yuefeiduzi/detour/actions/workflows/ci.yml/badge.svg)](https://github.com/yuefeiduzi/detour/actions/workflows/ci.yml)
[![Release](https://github.com/yuefeiduzi/detour/actions/workflows/release.yml/badge.svg)](https://github.com/yuefeiduzi/detour/actions/workflows/release.yml)

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

### 0. 直接下载（不想装 Go 的话）

[GitHub Releases](../../releases) 上有 CI 构建好的制品（同一套命令，见
[.github/workflows/release.yml](.github/workflows/release.yml)）：

| 平台 | 资产 |
|---|---|
| Windows x64 | `detour-<版本>-windows-amd64.zip`（解压即得 `detour.exe`） |
| macOS Apple Silicon | `detour-<版本>-darwin-arm64.tar.gz` |
| macOS Intel | `detour-<版本>-darwin-amd64.tar.gz` |

macOS 解压后 `./detour -print-config` 看生效配置；Windows 直接在 cmd 里
`detour.exe -check`。二进制没签名：macOS 被 Gatekeeper 拦下就
`xattr -d com.apple.quarantine ./detour`，Windows SmartScreen 选“仍要运行”。

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
detour 0.4.0 listening on http://127.0.0.1:8787
  config: /Users/you/.detour/detour.json
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
都有四种来源，**优先级：命令行参数 > 配置文件 > 环境变量 > 内置默认值**。
拿不准现在吃的是哪套：`./detour -print-config`（打印生效配置 JSON），
或 `./detour.sh status`。

### 方式一：环境变量（最常用，改端口最快）

```bash
set   DETOUR_PROXY=http://127.0.0.1:7897   # Windows cmd
export DETOUR_PROXY=http://127.0.0.1:7897  # macOS / Linux（代理地址，http / socks5 均可）
export DETOUR_LISTEN=127.0.0.1:8787        # 本地监听地址
export DETOUR_UPSTREAM=https://opencode.ai/zen/go/v1/   # 上游 API 地址
export DETOUR_CONFIG=/path/to/detour.json  # 指定配置文件（可选）
./detour.sh start                          # 设置后再启动
```

只有**显式设置**的那一项才会覆盖配置文件；没设的项交给 `detour.json` 和内置默认值。
裸二进制（Windows 的 `detour.exe`、没装 bash 的机器）同样认这几个环境变量。

**常见梯子端口对照：**

| 梯子软件 | 代理地址（`DETOUR_PROXY`） |
|---|---|
| Clash / Clash Verge | `http://127.0.0.1:7897` |
| Clash（混合端口） | `http://127.0.0.1:7890` |
| V2Ray / V2rayN | `http://127.0.0.1:10809` 或 `socks5://127.0.0.1:10808` |
| sing-box | `socks5://127.0.0.1:1080` |
| Surge / Shadowrocket | `socks5://127.0.0.1:6153`（HTTP 见软件设置） |

不想走代理时用 `direct`：`export DETOUR_PROXY=direct`。

### 方式二：配置文件（默认 `~/.detour/detour.json`）

参考 `detour.example.json`，所有参数写进文件。**默认位置是用户目录下的
`.detour/detour.json`**（Windows 是 `C:\Users\<你>\.detour\detour.json`），
与二进制放哪、从哪个目录启动都无关：

```bash
mkdir -p ~/.detour && cp detour.example.json ~/.detour/detour.json
```

```json
{
  "listen": "127.0.0.1:8787",
  "upstream": "https://opencode.ai/zen/go/v1/",
  "proxy": "http://127.0.0.1:7897",
  "verbose": false
}
```

查找顺序（第一个存在的生效）：`-config` / `$DETOUR_CONFIG` → `~/.detour/detour.json`
→ 二进制同目录 → 当前目录。所以仓库里放一份 `detour.json` 做实验也还能用，
但注意用户目录那份优先。

```bash
./detour.sh start              # 启动时自动加载（就是上面这个顺序）
./detour -config detour.json   # 或显式指定某个文件
./detour -print-config         # 看此刻到底读了哪个文件
```

> ⚠️ `detour.json` 已被 `.gitignore` 忽略；里面可能含代理认证信息，
> 别改个名字提交到仓库。
>
> 📄 文件编码：UTF-8 最好；**0.4.1 起也兼容 UTF-8 BOM 与 UTF-16**（PowerShell 5.1 的
> `> 文件`、`Out-File`、`Set-Content -Encoding UTF8` 以及记事本"Unicode"存出来的
> 都能读）。GBK/ANSI 保存的会明确报错，提示另存为 UTF-8。

### 方式三：命令行参数（临时/测试用）

```bash
./detour -listen 127.0.0.1:8787 \
         -upstream https://opencode.ai/zen/go/v1/ \
         -proxy socks5://127.0.0.1:1080
./detour -proxy direct    # 直连（测试用）
./detour -print-config    # 打印生效配置后退出（默认值 + detour.json + 环境变量）
```

### Windows 用法（没有 bash）

`detour.sh` 依赖 bash + `nohup`/`ps`/`lsof`，Windows 上不要用它，直接跑 exe：

在开始处加上：

```bat
:: 1. 写配置到用户目录（推荐；也可以放 exe 同目录）
mkdir "%USERPROFILE%\.detour"
notepad "%USERPROFILE%\.detour\detour.json"
::    {"listen":"127.0.0.1:8787","upstream":"https://opencode.ai/zen/go/v1/","proxy":"http://127.0.0.1:7890"}

:: 2. 看生效配置 / 体检链路（会打印它读的配置文件路径和连的梯子端口）
detour.exe -print-config
detour.exe -check

:: 3. 启动（前台；也可以 start /b detour.exe 让它待在后台）
detour.exe
```

`detour.exe` 按 `%USERPROFILE%\.detour\detour.json` → exe 同目录 → 当前目录的顺序找配置；
没有配置文件时用内置默认值，所以临时改端口只要
`set DETOUR_PROXY=http://127.0.0.1:7890` 再启动即可（加引号也行，会自动去掉）。

配置文件编码不用操心：UTF-8、UTF-8 BOM、UTF-16（记事本"Unicode"、PowerShell 的
`>` / `Out-File` / `Set-Content -Encoding UTF8` 产物）都能读；GBK 保存的会报错并
提示另存为 UTF-8（否则中文会变乱码）。

### 配置项一览

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `-listen` / `DETOUR_LISTEN` | `127.0.0.1:8787` | 本地监听地址（只监听本机，勿改 0.0.0.0） |
| `-upstream` / `DETOUR_UPSTREAM` | `https://opencode.ai/zen/go/v1/` | 真实 API 地址 |
| `-proxy` / `DETOUR_PROXY` | `http://127.0.0.1:7897` | 梯子代理，`http://` / `socks5://` / `direct` |
| `-v` | 关 | 详细日志（打印请求头，Authorization 脱敏） |
| `-check` | — | 体检代理链路后退出 |
| `-print-config` | — | 打印生效配置（JSON）后退出 |
| `-config` / `DETOUR_CONFIG` | `~/.detour/detour.json` | 配置文件；查找顺序：本项 → `~/.detour/detour.json` → exe 同目录 → 当前目录 |
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
pi install git:github.com/yuefeiduzi/detour
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
- **自动拉起 detour**：detour 没在跑时自动拉起——Unix 上用 `detour.sh start`，
  Windows 上直接 spawn `detour.exe`（没有 bash/nohup）。只把你**显式设置**的
  `DETOUR_*` 作为参数传下去，其余跟随二进制自己的配置（`detour.json` > 默认值），
  不会拿默认的 7897 盖掉你写好的配置。`PI_DETOUR_AUTO_START=0` 可关闭。

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
| `DETOUR_PROXY` | `http://127.0.0.1:7897` | 梯子代理（改端口在这里）；不设则用二进制报告的 `detour.json` 值 |
| `DETOUR_CONFIG` | — | 指定 `detour.json` 路径（不设则用二进制旁的 detour.json） |
| `DETOUR_BIN` | 自动查找 | detour 二进制路径（查找顺序：本变量 → 插件同级 ../detour → ~/self-git/detour/detour → PATH；Windows 自动补 `.exe`） |
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
./detour.sh status                      # 状态（含生效配置与来源）
./detour.sh check                       # 体检代理链路
./detour.sh log                         # 跟踪日志
./detour.sh stop                        # 停止
./detour -config detour.json            # 用配置文件启动
./detour -print-config                  # 看当前生效的监听/上游/代理
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
  502 的响应体会直接点名它连的是哪个代理，例如：
  `upstream request failed: proxyconnect tcp: dial tcp 127.0.0.1:7897: connection refused
  (proxy http://127.0.0.1:7897: is your ladder running on that port?)` —— 看到这句就是端口不对：
  用 `detour -print-config` 确认生效端口，`netstat -ano | findstr LISTENING`（Windows）
  或 Clash 设置里看梯子真实端口，然后 `detour -proxy http://127.0.0.1:<真实端口>` 重启。
- **pi 提示"链路异常"**：同上，检查梯子与代理规则。
- **opencode 报 models.dev 相关错误**：opencode 会从 models.dev 拉模型元数据，
  如果它也被墙，给 models.dev 单独配代理规则即可。
- **梯子规则模式**：确保 `opencode.ai`、`api.openai.com` 等域名命中代理规则；
  或在 Clash 里把这些域名加进代理组。detour 的意义就是让你不用开全局。
- **觉得慢？先看日志里三个数**（每一行请求结束后都会打印）：
  ```
  POST /chat/completions -> https://opencode.ai/zen/go/v1/chat/completions | 200 | 12.305s | ttfb 4.51s | req 956.4 KB in 2.06s | resp 1.6 MB
  ```
  - `req ... in ...`：客户端把请求体传完用了多久（**上传**）。pi 每一轮都会把整个会话
    重发一次，图片以 base64 内联，所以长会话 / 多截图时这里会很大 —— 这一段慢说明
    梯子上行不行，换节点或减少图片（`/compact`、给模型配 `inputLimits.images.resize`）。
  - `ttfb`：从收到请求到上游第一个字节（含上面的上传时间 + 上游排队/prefill）。
    `ttfb` 远大于 `req` 时，慢在上游排队/预填充，不在本地链路。
  - 总耗时 − `ttfb` ≈ 生成（流式吐 token）时间；配合 `resp` 大小能算出 tok/s。
  - 只想要数字说话的话：`detour.sh log | grep 'ttfb'`。

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