# detour-pi — pi 的 OpenCode Go 本地转发插件

公司网络连不上 opencode.ai？不需要全局代理，不需要手动配模型。
这个插件把 pi 内置的 `opencode-go` provider 自动指到本地 detour 转发服务，
流量只走模型 API 这一段（通过你的梯子），其余网络照常。**你只需要配一个
API key，剩下的插件全包了。**

```
pi ──http://127.0.0.1:8787──▶ detour ──梯子(127.0.0.1:7897)──▶ opencode.ai/zen/go/v1
```

## 安装

插件零运行时依赖，两种装法任选：

**方式一：直接放扩展目录（推荐，最简单）**

```bash
cp pi-extension/index.ts ~/.pi/agent/extensions/detour.ts
# 重启 pi 即在启动头看到 extension 已加载
```

**方式二：作为 pi 包安装（整个 detour 仓库）**

```bash
pi install git:github.com/ross/detour
# 或本地路径: pi install /path/to/detour
```

## 使用

1. 配 key（原来 opencode go 怎么配就怎么配）：

   ```bash
   export OPENCODE_API_KEY=sk-...
   pi
   ```

   或者进 pi 后 `/login opencode-go` 输入密钥。

2. 直接 `/model` 选 opencode-go 的模型，开聊。插件会自动：
   - 把 opencode-go 所有模型的 baseUrl 改写成本地 detour；
   - 检测到 detour 没在跑时，自动用仓库里的 `detour.sh start` 拉起来
     （监听 `127.0.0.1:8787`，上游 `https://opencode.ai/zen/go/v1/`，
     代理 `http://127.0.0.1:7897`）。

3. 管理命令：

   ```
   /detour            # 状态（端口 + 链路探测 + 配置）
   /detour start      # 启动 detour
   /detour stop       # 停止（仅停插件/脚本拉起的实例）
   /detour restart    # 重启
   /detour check      # 探测链路：能到上游返回非 502 即正常
   /detour env        # 打印生效配置
   /detour key        # 密钥配置指引
   ```

## 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `OPENCODE_API_KEY` | — | OpenCode Go 密钥（pi 内置认证，也可 `/login`） |
| `DETOUR_BASE_URL` | `http://127.0.0.1:8787` | 本地 detour 地址（完整 URL） |
| `DETOUR_LISTEN` | `127.0.0.1:8787` | 兼容 detour.sh 的 host:port 写法，二选一 |
| `DETOUR_UPSTREAM` | `https://opencode.ai/zen/go/v1/` | 实际上游（换供应商时改，如 `https://api.openai.com/v1`） |
| `DETOUR_PROXY` | `http://127.0.0.1:7897` | 梯子代理，支持 `socks5://`，`direct` 直连 |
| `DETOUR_BIN` | 自动查找 | detour 二进制路径 |
| `DETOUR_EXTRA_PROVIDERS` | 空 | 逗号分隔的额外 provider 名，同样改写 baseUrl 到本地（需自行把 DETOUR_UPSTREAM 配成对应 API） |
| `PI_DETOUR_AUTO_START` | 开 | 设为 `0` 关闭自动拉起 |

detour 二进制查找顺序：`DETOUR_BIN` → 插件同级 `../detour`（仓库布局）→
`~/self-git/detour/detour` → `PATH`。找到同目录 `detour.sh` 时优先用它
管理（pidfile/日志），否则直接 spawn 二进制并写 `detour.log`。

## 原理

- 插件在扩展工厂阶段调用 `pi.registerProvider("opencode-go", { baseUrl })`，
  覆盖已有 provider 的 baseUrl 而保留其全部内置模型与 `OPENCODE_API_KEY`
  认证（pi 的合并规则：只给 baseUrl 时所有模型统一改地址）。
- detour v0.3.0 起支持"防重复 /v1 拼接"：上游路径以 `/v1` 结尾而请求路径
  也以 `/v1` 开头时不再叠加，因此三种 API 形态都能正确转发：
  - Anthropic Messages SDK：`/v1/messages` → `/zen/go/v1/messages`
  - OpenAI Responses SDK：`/responses` → `/zen/go/v1/responses`
  - OpenAI Completions：`/chat/completions` → `/zen/go/v1/chat/completions`
- 链路探测：`GET {baseUrl}/v1/models`，返回 502 说明 detour 连不上上游
  （梯子没开/规则没命中），其余状态均视为链路正常。

## 故障排查

- **`/detour check` 报 502 或超时**：梯子没开、节点死了，或 Clash 规则没把
  `opencode.ai` / `*.opencode.ai` 加进代理组。
- **detour 能在别处跑但 pi 里起不来**：端口被占 → `PI_DETOUR_AUTO_START=0`
  或手动 `detour.sh start` 后 `/detour status` 确认。
- **模型列表里没有 opencode-go**：没配 key。`export OPENCODE_API_KEY`
  或 `/login opencode-go`。