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
   /detour            # 状态（端口 + 链路探测 + 改写范围 + 全局代理）
   /detour start      # 启动 detour
   /detour stop       # 停止（仅停插件/脚本拉起的实例）
   /detour restart    # 重启
   /detour check      # 探测链路：能到上游返回非 502 即正常
   /detour env        # 打印生效配置
   /detour key        # 密钥配置指引
   ```

## 作用范围（只管 opencode，不动别的 provider）

插件只调用 `pi.registerProvider("opencode-go", { baseUrl })`，而 pi 的
`registerProvider` 是按 provider 隔离的（重写只作用于该 provider 自己模型的
baseUrl）。所以：

- **默认只有 `opencode-go` 走本地 detour/梯子**；
- `qwen-token-plan-cn`（本机可直连）、`deepseek` 等 provider 保持 pi 原有
  baseUrl，**永远不经过 detour 和梯子**；
- 想让别的 provider 也走 detour，必须显式写 `DETOUR_EXTRA_PROVIDERS`。

唯一的例外是**全局代理**：pi 的 HTTP 层是全局 undici EnvHttpProxyAgent，只要
进程里有 `HTTP_PROXY`/`HTTPS_PROXY`（或 `settings.json` 的 `httpProxy`），
**所有 provider 都会走那个代理**，qwen 也不例外。插件在这种情况下会：

- 把本地 detour 地址（及其它 `DETOUR_DIRECT_HOSTS`）写进 `NO_PROXY`，让它们
  保持直连（undici 每次请求都重读 `NO_PROXY`，运行期设置也生效）；
- 在会话启动时弹出告警说明。

> 换句话说：想让 opencode 走梯子、qwen 直连，**不要设全局代理**，交给 detour
> 就够了；如果确实需要全局代理，记得 `DETOUR_DIRECT_HOSTS=token-plan.cn-beijing.maas.aliyuncs.com`。

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
| `DETOUR_DIRECT_HOSTS` | 空 | 逗号分隔域名。存在全局代理时写入 `NO_PROXY` 强制直连，例如 `token-plan.cn-beijing.maas.aliyuncs.com` |
| `PI_DETOUR_AUTO_START` | 开 | 设为 `0` 关闭自动拉起 |

detour 二进制查找顺序：`DETOUR_BIN` → 插件同级 `../detour`（仓库布局）→
`~/self-git/detour/detour` → `PATH`。找到同目录 `detour.sh` 时优先用它
管理（pidfile/日志），否则直接 spawn 二进制并写 `detour.log`。

## 原理

- 插件在扩展工厂阶段调用 `pi.registerProvider("opencode-go", { baseUrl })`，
  覆盖已有 provider 的 baseUrl 而保留其全部内置模型与 `OPENCODE_API_KEY`
  认证（pi 的合并规则：只给 baseUrl 时所有模型统一改地址，且不影响其它 provider）。
- detour v0.3.0 起支持"防重复 /v1 拼接"：上游路径以 `/v1` 结尾而请求路径
  也以 `/v1` 开头时不再叠加，因此三种 API 形态都能正确转发：
  - Anthropic Messages SDK：`/v1/messages` → `/zen/go/v1/messages`
  - OpenAI Responses SDK：`/responses` → `/zen/go/v1/responses`
  - OpenAI Completions：`/chat/completions` → `/zen/go/v1/chat/completions`
- 链路探测：`GET {baseUrl}/v1/models`，返回 502 说明 detour 连不上上游
  （梯子没开/规则没命中），其余状态均视为链路正常。

## 故障排查

- **qwen token-plan 之类的 provider 也变慢了/走代理了**：先 `/detour env` 看
  “全局代理”一行。有 `HTTP_PROXY`/`HTTPS_PROXY`（或 settings.json 的 `httpProxy`）
  就是它在生效——它不是插件设的，删掉它、或用 `DETOUR_DIRECT_HOSTS` 把域名加进
  `NO_PROXY`。
- **`/detour check` 报 502 或超时**：梯子没开、节点死了，或 Clash 规则没把
  `opencode.ai` / `*.opencode.ai` 加进代理组。
- **detour 能在别处跑但 pi 里起不来**：端口被占 → `PI_DETOUR_AUTO_START=0`
  或手动 `detour.sh start` 后 `/detour status` 确认。
- **模型列表里没有 opencode-go**：没配 key。`export OPENCODE_API_KEY`
  或 `/login opencode-go`。

## 测试

```bash
bun test pi-extension     # 锁定“只改写 opencode-go”与 NO_PROXY 行为
```