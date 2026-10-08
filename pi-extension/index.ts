/**
 * detour-pi — 让 pi 通过本地 detour 转发访问 OpenCode Go（公司网络直连不通时）。
 *
 * 背景：公司网络直连 opencode.ai 不通，需要把模型 API 流量走本地梯子。
 * 这个插件把 pi 内置 opencode-go provider 的所有模型 baseUrl 指到本地 detour
 * （默认 http://127.0.0.1:8787），并由插件自动拉起 detour。
 *
 * 直连判断：不强迫所有环境走代理。启动时探测一次 opencode.ai 直连是否可达：
 *   - 能直连 → 不覆盖 baseUrl，直接用 pi 内置的原始地址（不走代理、不拉起 detour）
 *   - 不能直连 → 覆盖到本地 detour，并自动拉起 detour
 * 用户也可以强制指定 DETOUR_MODE。
 *
 * 作用范围：只改写 opencode-go（以及 DETOUR_EXTRA_PROVIDERS 显式列出的 provider）。
 * 其它 provider（例如本机可直连的 qwen token-plan）保持 pi 原有 baseUrl，绝不经过
 * detour/梯子。pi 的 registerProvider(name, { baseUrl }) 是按 provider 隔离的。
 *
 * 全局代理注意：pi 的 HTTP 层是全局 undici EnvHttpProxyAgent——只要进程里有
 * HTTP_PROXY/HTTPS_PROXY（或 settings.json 的 httpProxy，pi 会把它写进这两个变量），
 * 所有 provider 都会走那个代理，包括 qwen。插件会把本地 detour 地址和
 * DETOUR_DIRECT_HOSTS 里的域名写进 NO_PROXY 让它们保持直连（undici 每次请求都会
 * 重读 NO_PROXY，所以运行期设置同样生效）。
 *
 * 零运行时依赖：只 import 类型 + Node 内置模块，可单文件复制到
 * ~/.pi/agent/extensions/ 使用，也可 pi install git:... 安装。
 *
 * 环境变量（均可选）:
 *   DETOUR_MODE          auto(默认) | always(强制走detour) | never(强制直连)
 *   DETOUR_BASE_URL      本地 detour 地址，默认 http://127.0.0.1:8787
 *   DETOUR_LISTEN        兼容 detour.sh 的写法（host:port），默认 127.0.0.1:8787
 *   DETOUR_UPSTREAM      实际上游，默认 https://opencode.ai/zen/go/v1/
 *   DETOUR_PROXY         梯子代理，默认 http://127.0.0.1:7897
 *   DETOUR_CONFIG        指定 detour.json 路径（不设则按 ~/.detour/detour.json →
 *                        二进制同目录 → 当前目录 的顺序自动找）
 *   DETOUR_BIN           detour 二进制路径（默认自动查找：扩展同级的 ../detour、
 *                        ~/self-git/detour/detour、PATH；Windows 自动补 .exe）
 *   DETOUR_EXTRA_PROVIDERS 逗号分隔的额外 provider 名，同样改写 baseUrl 到本地
 *   DETOUR_DIRECT_HOSTS  逗号分隔域名，存在全局代理时强制直连（写入 NO_PROXY），
 *                        例如 token-plan.cn-beijing.maas.aliyuncs.com
 *   PI_DETOUR_AUTO_START 设为 0 关闭自动拉起 detour
 *
 * 监听 / 上游 / 代理三项的取值顺序：环境变量 > 配置文件（默认 ~/.detour/detour.json，
 * 用 `detour -print-config` 问二进制要最终生效值）> 内置默认值。只有显式设置的环境变量
 * 才会作为命令行参数传给 detour，所以不会拿默认值去盖掉你自己的 detour.json。
 */
import type { ExtensionAPI, ExtensionCommandContext } from "@earendil-works/pi-coding-agent";
import { spawn, spawnSync } from "node:child_process";
import { connect } from "node:net";
import { existsSync, openSync } from "node:fs";
import { homedir } from "node:os";
import { delimiter, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const EXT_DIR = typeof __dirname !== "undefined"
  ? __dirname
  : dirname(fileURLToPath(import.meta.url));

const IS_WINDOWS = process.platform === "win32";
const EXE_NAMES = IS_WINDOWS ? ["detour.exe", "detour"] : ["detour"];

const DEFAULT_BASE_URL = "http://127.0.0.1:8787";
const DEFAULT_UPSTREAM = "https://opencode.ai/zen/go/v1/";
const DEFAULT_PROXY = "http://127.0.0.1:7897";
const DIRECT_PROBE_TIMEOUT_MS = 7000; // 直连探测超时；公司网络典型表现为连接挂起

/** 默认只改这一个 provider；其它 provider（qwen token-plan 等）永远保持原地址。 */
const PRIMARY_PROVIDER = "opencode-go";

type DetourMode = "auto" | "always" | "never";

/** 只有显式来自环境变量的项才会作为命令行参数传给 detour。 */
interface DetourOverrides {
  config?: string;
  listen?: string;
  upstream?: string;
  proxy?: string;
}

interface DetourConfig {
  baseUrl: string;          // full local URL, no trailing slash, e.g. http://127.0.0.1:8787
  listen: string;           // host:port form for detour flags
  upstream: string;
  proxy: string;
  mode: DetourMode;
  autoStart: boolean;
  extraProviders: string[];
  directHosts: string[];     // 有全局代理时仍要直连的域名（写入 NO_PROXY）
  overrides: DetourOverrides;
  bin?: string;             // detour binary path found
  script?: string;          // detour.sh path found next to the binary (never on Windows)
  configFile?: string;      // detour.json the binary reported as effective
}

interface RuntimeState {
  directOk: boolean;        // true = 直连可达，直接走上游（不覆盖、不拉起 detour）
  mode: DetourMode;
  noProxyAdded?: string;    // 本次写进 NO_PROXY 的直连域名，供 /detour 显示
  spawnedPid?: number;      // 本插件拉起的 detour 进程，Windows 上用它来 stop
}

const state: RuntimeState = { directOk: true, mode: "auto" };

/**
 * 读环境变量：去掉首尾空白，以及 Windows 上习惯性加的引号
 * （cmd 的 `set DETOUR_PROXY="http://127.0.0.1:7890"` 会把引号一起存进去）。
 */
function envValue(name: string): string | undefined {
  const raw = process.env[name]?.trim();
  if (!raw) return undefined;
  const unquoted = raw.replace(/^"(.*)"$/s, "$1").trim();
  return unquoted || undefined;
}

/** baseUrl 优先看 DETOUR_BASE_URL，其次 DETOUR_LISTEN / detour 实际监听的地址。 */
function resolveBaseUrl(effective?: EffectiveConfig): string {
  const explicit = envValue("DETOUR_BASE_URL");
  if (explicit) return explicit.replace(/\/+$/, "");
  const listen = envValue("DETOUR_LISTEN") || effective?.listen;
  if (listen) return `http://${listen.replace(/^https?:\/\//, "")}`;
  return DEFAULT_BASE_URL;
}

/** 带路径分隔符才算路径（Windows 上是 "\"，不能只认 "/"）。 */
function looksLikePath(candidate: string): boolean {
  return candidate.includes("/") || candidate.includes("\\");
}

/** 在 PATH 里找可执行文件（Windows 会补 .exe）。 */
function whichSync(name: string): string | undefined {
  const dirs = (process.env.PATH ?? "").split(IS_WINDOWS ? ";" : delimiter);
  for (const dir of dirs) {
    if (!dir) continue;
    const candidate = join(dir, name);
    const found = existingFile([candidate]);
    if (found) return found;
  }
  return undefined;
}

/** 返回第一个存在的文件；Windows 上给无后缀的名字补 .exe 再试一次。 */
function existingFile(candidates: string[]): string | undefined {
  for (const candidate of candidates) {
    if (!candidate) continue;
    if (existsSync(candidate)) return candidate;
    if (IS_WINDOWS && !/\.exe$/i.test(candidate) && existsSync(`${candidate}.exe`)) {
      return `${candidate}.exe`;
    }
  }
  return undefined;
}

function resolveMode(): DetourMode {
  const m = envValue("DETOUR_MODE")?.toLowerCase();
  if (m === "always" || m === "never") return m;
  return "auto";
}

function parseHostPort(baseUrl: string): { host: string; port: number } {
  try {
    const u = new URL(baseUrl);
    return { host: u.hostname, port: Number(u.port || (u.protocol === "https:" ? 443 : 80)) };
  } catch {
    return { host: "127.0.0.1", port: 8787 };
  }
}

function listenFromBaseUrl(baseUrl: string): string {
  const { host, port } = parseHostPort(baseUrl);
  return `${host}:${port}`;
}

/**
 * pi 唯一的「全局代理」入口：HTTP(S)_PROXY（settings.json 的 httpProxy 也会被
 * pi 复制到这两个变量）。一旦有值，pi 的全局 undici EnvHttpProxyAgent 会把所有
 * provider 都代理掉——这是「所有请求都走代理」的唯一途径，插件本身不会设置它。
 */
function globalProxyEnv(): string | undefined {
  const env = process.env;
  return (env.HTTPS_PROXY ?? env.https_proxy ?? env.HTTP_PROXY ?? env.http_proxy)?.trim() || undefined;
}

/** 把域名并入 NO_PROXY（返回新加入的部分）。undici 每次 dispatch 都重读该变量。 */
function addNoProxyHosts(hosts: string[]): string | undefined {
  const env = process.env;
  const current = (env.NO_PROXY ?? env.no_proxy ?? "").trim();
  if (current === "*") return undefined; // NO_PROXY=* 表示全部直连，无需追加
  const entries = current.split(",").map((s) => s.trim()).filter(Boolean);
  const seen = new Set(entries.map((s) => s.toLowerCase()));
  const added: string[] = [];
  for (const host of hosts) {
    const h = host.trim().toLowerCase();
    if (!h || seen.has(h)) continue;
    seen.add(h);
    entries.push(h);
    added.push(h);
  }
  if (!added.length) return undefined;
  env.NO_PROXY = entries.join(",");
  return added.join(",");
}

/**
 * 有全局代理时，保证「该直连的东西」不被它代理：本地 detour + DETOUR_DIRECT_HOSTS。
 * 幂等，可重复调用（例如 /reload 后）。
 */
function applyDirectBypass(cfg: DetourConfig): void {
  if (!globalProxyEnv()) return;
  const added = addNoProxyHosts([parseHostPort(cfg.baseUrl).host, "localhost", ...cfg.directHosts]);
  if (added) state.noProxyAdded = [state.noProxyAdded, added].filter(Boolean).join(",");
}

/** 需要改写 baseUrl 的 provider 白名单（默认只有 opencode-go）。 */
function rewriteTargets(cfg: DetourConfig): string[] {
  return [PRIMARY_PROVIDER, ...cfg.extraProviders];
}

function findDetour(): { bin: string; script?: string } | undefined {
  const env = process.env;
  const candidates: string[] = [];
  const binHint = envValue("DETOUR_BIN");
  if (binHint) {
    const hint = binHint;
    if (looksLikePath(hint)) candidates.push(hint);
    else candidates.push(whichSync(hint) ?? "");
  }
  for (const name of EXE_NAMES) {
    candidates.push(join(EXT_DIR, "..", name));                        // repo layout: pi-extension/../detour
    candidates.push(join(homedir(), "self-git", "detour", name));
    candidates.push(whichSync(name) ?? "");                            // PATH lookup
  }
  const bin = existingFile(candidates);
  if (!bin) return undefined;
  const script = join(dirname(bin), "detour.sh");
  // Windows 没有 bash/nohup/lsof，detour.sh 用不了：直接 spawn 二进制。
  return { bin, script: !IS_WINDOWS && existsSync(script) ? script : undefined };
}

interface EffectiveConfig {
  listen?: string;
  upstream?: string;
  proxy?: string;
  config?: string;
}

/**
 * 问二进制它最终生效的配置（内置默认值 + detour.json + DETOUR_* 环境变量）。
 * 这样 detour.json 里改的端口不会被插件按默认值盖掉，/detour env 显示的也是真值。
 * 老版本二进制没有 -print-config，直接返回 undefined 退回默认值。
 */
function readEffectiveConfig(bin: string): EffectiveConfig | undefined {
  try {
    const res = spawnSync(bin, ["-print-config"], {
      encoding: "utf8",
      timeout: 5000,
      windowsHide: true,
    });
    if (res.status !== 0 || !res.stdout) return undefined;
    const parsed = JSON.parse(res.stdout) as Record<string, unknown>;
    const pick = (key: string): string | undefined => {
      const value = parsed[key];
      return typeof value === "string" && value.trim() ? value.trim() : undefined;
    };
    return { listen: pick("listen"), upstream: pick("upstream"), proxy: pick("proxy"), config: pick("config") };
  } catch {
    return undefined;
  }
}

function loadConfig(): DetourConfig {
  const env = process.env;
  const found = findDetour();
  const effective = found ? readEffectiveConfig(found.bin) : undefined;
  const baseUrl = resolveBaseUrl(effective);

  const explicitListen = envValue("DETOUR_LISTEN");
  const explicitUpstream = envValue("DETOUR_UPSTREAM");
  const explicitProxy = envValue("DETOUR_PROXY");
  const explicitConfig = envValue("DETOUR_CONFIG");

  // 只有显式设置的环境变量才传命令行参数，避免用插件的默认值覆盖用户的 detour.json。
  const overrides: DetourOverrides = {};
  if (explicitConfig) overrides.config = explicitConfig;
  if (explicitListen) overrides.listen = explicitListen;
  else if (envValue("DETOUR_BASE_URL")) overrides.listen = listenFromBaseUrl(baseUrl);
  if (explicitUpstream) overrides.upstream = explicitUpstream;
  if (explicitProxy) overrides.proxy = explicitProxy;

  return {
    baseUrl,
    listen: listenFromBaseUrl(baseUrl),
    upstream: explicitUpstream || effective?.upstream || DEFAULT_UPSTREAM,
    proxy: explicitProxy || effective?.proxy || DEFAULT_PROXY,
    mode: resolveMode(),
    autoStart: env.PI_DETOUR_AUTO_START !== "0",
    extraProviders: (envValue("DETOUR_EXTRA_PROVIDERS") ?? "")
      .split(",").map((s) => s.trim()).filter(Boolean),
    directHosts: (envValue("DETOUR_DIRECT_HOSTS") ?? "")
      .split(",").map((s) => s.trim()).filter(Boolean),
    overrides,
    bin: found?.bin,
    script: found?.script,
    configFile: effective?.config,
  };
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** TCP connect probe: is anything listening on the local relay port? */
function portOpen(host: string, port: number, timeoutMs = 800): Promise<boolean> {
  return new Promise((resolve) => {
    const sock = connect({ host, port });
    const timer = setTimeout(() => { sock.destroy(); resolve(false); }, timeoutMs);
    sock.once("connect", () => { clearTimeout(timer); sock.destroy(); resolve(true); });
    sock.once("error", () => { clearTimeout(timer); resolve(false); });
  });
}

/**
 * Probe whether opencode.ai is reachable directly (no proxy). Node's fetch does
 * NOT honor HTTP(S)_PROXY by default, so this is a true direct-connect probe.
 * Any HTTP response (including 401/403) means the network path works; only a
 * network error / timeout / DNS failure means we should fall back to detour.
 */
async function probeDirect(upstream: string, timeoutMs = DIRECT_PROBE_TIMEOUT_MS): Promise<boolean> {
  const target = `${upstream.replace(/\/+$/, "")}/models`;
  try {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), timeoutMs);
    const res = await fetch(target, { method: "GET", signal: ctrl.signal });
    clearTimeout(timer);
    void res.body?.cancel?.();
    return true; // got an HTTP response -> network reacheable
  } catch {
    return false; // timeout / ENOTFOUND / connection refused / abort
  }
}

/**
 * HTTP probe through the relay. Any non-502 status means the chain works
 * (401/404 from the upstream still prove detour reached it); 502 means detour
 * itself cannot reach the upstream (proxy down, proxy rules missing...).
 */
async function probeRelay(baseUrl: string, timeoutMs = 6000): Promise<{ ok: boolean; status?: number; error?: string }> {
  try {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), timeoutMs);
    const res = await fetch(`${baseUrl}/v1/models`, { method: "GET", signal: ctrl.signal });
    clearTimeout(timer);
    if (res.status !== 502) return { ok: true, status: res.status };
    // 502 = detour 连不上上游。把它的错误原文带出来：通常是梯子不在那个端口上。
    let detail: string | undefined;
    try {
      const body = (await res.json()) as { error?: { message?: string } };
      const message = body?.error?.message;
      if (typeof message === "string" && message) detail = message.slice(0, 300);
    } catch {
      // 响应不是 JSON：保持 502 状态即可
    }
    return { ok: false, status: res.status, error: detail };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

/**
 * 只有显式设置的环境变量才转成命令行参数（未设置的不传，交给 detour 自己决定
 * —— 否则插件的内置默认值会盖掉用户写好的 detour.json）。
 */
function spawnArgs(cfg: DetourConfig): string[] {
  const o = cfg.overrides;
  const args: string[] = [];
  if (o.config) args.push("-config", o.config);
  if (o.listen) args.push("-listen", o.listen);
  if (o.upstream) args.push("-upstream", o.upstream);
  if (o.proxy) args.push("-proxy", o.proxy);
  return args;
}

/** detour.sh 从环境变量读覆盖项，把「显式设置」的那几项同步给它。 */
function scriptEnv(cfg: DetourConfig): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = { ...process.env };
  const o = cfg.overrides;
  if (o.config) env.DETOUR_CONFIG = o.config;
  if (o.listen) env.DETOUR_LISTEN = o.listen;
  if (o.upstream) env.DETOUR_UPSTREAM = o.upstream;
  if (o.proxy) env.DETOUR_PROXY = o.proxy;
  return env;
}

/**
 * Start detour if it is not already listening. Prefers detour.sh (manages its
 * own pidfile/log) on Unix; on Windows (no bash/nohup/lsof) spawns the binary
 * directly. Never touches a detour that is already running.
 */
async function ensureDetour(cfg: DetourConfig): Promise<"already-running" | "started" | "no-binary" | "start-failed"> {
  const { host, port } = parseHostPort(cfg.baseUrl);
  if (await portOpen(host, port, 500)) return "already-running";
  if (!cfg.bin) return "no-binary";

  if (cfg.script) {
    spawn("bash", [cfg.script, "start"], {
      env: scriptEnv(cfg),
      detached: true,
      stdio: "ignore",
    }).unref();
  } else {
    const logPath = join(dirname(cfg.bin), "detour.log");
    const fd = openSync(logPath, "a");
    const child = spawn(cfg.bin, spawnArgs(cfg), {
      detached: true,
      stdio: ["ignore", fd, fd],
      windowsHide: true,
    });
    state.spawnedPid = child.pid ?? undefined;
    child.unref();
  }

  for (let i = 0; i < 16; i++) {
    await sleep(250);
    if (await portOpen(host, port, 300)) return "started";
  }
  return "start-failed";
}

/**
 * Windows 上没有 pkill：用 netstat 找到监听该端口的 PID，再用 tasklist 确认它
 * 确实是 detour，避免误杀别的进程。
 */
function findListeningPid(port: number): number | undefined {
  try {
    const netstat = spawnSync("netstat", ["-ano", "-p", "tcp"], {
      encoding: "utf8",
      timeout: 5000,
      windowsHide: true,
    });
    for (const line of (netstat.stdout ?? "").split(/\r?\n/)) {
      if (!line.includes("LISTENING")) continue;
      const cols = line.trim().split(/\s+/);
      if (!(cols[1] ?? "").endsWith(`:${port}`)) continue;
      const pid = Number.parseInt(cols[cols.length - 1] ?? "", 10);
      if (!Number.isInteger(pid) || pid <= 0) continue;
      if (isDetourProcess(pid)) return pid;
    }
  } catch {
    // netstat 不存在/unparseable：放弃精确匹配
  }
  return undefined;
}

function isDetourProcess(pid: number): boolean {
  try {
    const tasklist = spawnSync("tasklist", ["/FI", `PID eq ${pid}`, "/FO", "CSV", "/NH"], {
      encoding: "utf8",
      timeout: 5000,
      windowsHide: true,
    });
    return /detour/i.test(tasklist.stdout ?? "");
  } catch {
    return false;
  }
}

async function stopDetour(cfg: DetourConfig): Promise<string> {
  const { host, port } = parseHostPort(cfg.baseUrl);
  if (cfg.script) {
    spawn("bash", [cfg.script, "stop"], { detached: true, stdio: "ignore" }).unref();
  } else if (IS_WINDOWS) {
    const pid = state.spawnedPid ?? findListeningPid(port);
    if (pid) {
      spawn("taskkill", ["/PID", String(pid), "/F", "/T"], { stdio: "ignore", windowsHide: true });
    }
  } else if (cfg.bin) {
    spawn("pkill", ["-f", `detour -listen ${cfg.listen}`], { detached: true, stdio: "ignore" }).unref();
  }
  for (let i = 0; i < 20; i++) {
    await sleep(100);
    if (!(await portOpen(host, port, 200))) {
      state.spawnedPid = undefined;
      return "stopped";
    }
  }
  return "still-running";
}

function configSourceText(cfg: DetourConfig): string {
  const parts: string[] = [];
  if (cfg.configFile) parts.push(`detour.json (${cfg.configFile})`);
  const envKeys = Object.keys(cfg.overrides).map((key) => `DETOUR_${key.toUpperCase()}`);
  if (envKeys.length) parts.push(`环境变量 ${envKeys.join(", ")}`);
  return parts.length ? parts.join(" + ") : "内置默认值";
}

function overrideText(cfg: DetourConfig): string {
  const args = spawnArgs(cfg);
  return args.length ? `拉起时传参 ${args.join(" ")}` : "无（用 detour 自己的配置）";
}

function fmtConfig(cfg: DetourConfig): string {
  return [
    `本地地址   ${cfg.baseUrl}`,
    `上游       ${cfg.upstream}`,
    `代理       ${cfg.proxy}（只用于 detour 这一条链路）`,
    `配置来源   ${configSourceText(cfg)}`,
    `取值顺序   环境变量 > ~/.detour/detour.json（或 -config 指定）> 内置默认值`,
    `模式       ${cfg.mode}${cfg.mode === "auto" ? `（本次: ${state.directOk ? "直连" : "经 detour"}）` : ""}`,
    `自动拉起   ${cfg.autoStart ? "开" : "关 (PI_DETOUR_AUTO_START=0)"}`,
    `拉起传参   ${overrideText(cfg)}`,
    `改写 provider ${rewriteTargets(cfg).join(", ")}（其余 provider 一律直连）`,
    `全局代理   ${globalProxyEnv() ?? "未设置（各 provider 按自身 baseUrl 直连）"}`,
    state.noProxyAdded ? `NO_PROXY 直连 ${state.noProxyAdded}` : "",
    cfg.directHosts.length ? `直连域名   ${cfg.directHosts.join(", ")}` : "",
    `detour 二进制 ${cfg.bin ? cfg.bin : "(未找到，用 DETOUR_BIN=/path/to/detour 指定)"}`,
    cfg.script ? `管理脚本   ${cfg.script}` : "",
  ].filter(Boolean).join("\n");
}

export default async function (pi: ExtensionAPI) {
  // 1) 工厂阶段：判断直连可达性，决定是否把 opencode-go 指到本地 detour。
  //    auto 模式先探测一次；never 强制直连；always 强制走 detour。
  const cfg = loadConfig();
  if (cfg.mode === "always") state.directOk = false;
  else if (cfg.mode === "never") state.directOk = true;
  else state.directOk = await probeDirect(cfg.upstream);
  state.mode = cfg.mode;

  // 若进程里已有全局代理，先让该直连的域名豁免（opencode 本身仍可由全局代理覆盖，
  // 但本地 detour 与 DETOUR_DIRECT_HOSTS 必须直连，否则 qwen 之类也会被代理）。
  applyDirectBypass(cfg);

  if (!state.directOk) {
    // 直连不可用（或强制走 detour）：只覆盖白名单里的 provider，逐个注册，
    // 保留它们的内置模型与认证。绝不动 qwen-token-plan 等其它 provider。
    for (const name of rewriteTargets(cfg)) {
      pi.registerProvider(name, { baseUrl: cfg.baseUrl });
    }
  }
  // 直连可用时：不覆盖任何东西，pi 保持内置行为直连上游。

  // 2) session 开始：确保 detour 在跑（仅当需要走 detour），并提示直连/转发状态。
  pi.on("session_start", async (_event, ctx) => {
    const current = loadConfig();
    applyDirectBypass(current); // 幂等：把该直连的域名持续挡在全局代理之外

    // 全局代理是「所有请求都走代理」的唯一来源：pi 的 HTTP 层对每个 provider 都
    // 生效，qwen token-plan 也会被带上。说清楚，并给出让指定域名直连的办法。
    const globalProxy = globalProxyEnv();
    if (globalProxy && ctx.hasUI) {
      ctx.ui.notify([
        `检测到全局代理 ${globalProxy}：pi 会让所有 provider 都走它（qwen token-plan 也不例外）。`,
        state.noProxyAdded ? `已加入 NO_PROXY 保持直连: ${state.noProxyAdded}` : undefined,
        "要豁免更多域名：DETOUR_DIRECT_HOSTS=host1,host2；最省事的做法是不设全局代理，opencode 交给 detour。",
      ].filter(Boolean).join("\n"), "warning");
    }

    if (state.directOk) {
      if (ctx.hasUI) {
        ctx.ui.setStatus("detour", undefined);
        ctx.ui.notify(
          `opencode.ai 可直连${current.mode === "auto" ? "（auto 探测）" : `（${current.mode}）`}，未启用代理转发`,
          "info",
        );
      }
      return; // 直连模式：不检查 detour
    }

    if (ctx.hasUI) ctx.ui.setStatus("detour", "detour: 检查中…");
    if (current.autoStart) {
      const result = await ensureDetour(current);
      if (ctx.hasUI) {
        if (result === "already-running" || result === "started") {
          ctx.ui.setStatus("detour", undefined);
        } else {
          ctx.ui.setStatus("detour", `detour: ${result}`);
        }
      }
      if (result === "already-running" || result === "started") {
        const probe = await probeRelay(current.baseUrl);
        if (ctx.hasUI) {
          ctx.ui.notify(
            probe.ok
              ? `opencode.ai 直连不可用，已走 detour 转发 (${current.baseUrl})${probe.status ? `，链路探测 HTTP ${probe.status}` : ""}`
              : `detour 端口已开，但链路异常: ${probe.error ?? `HTTP ${probe.status}`} — 梯子/代理规则有问题（具体看上面这句错误）`,
            probe.ok ? "info" : "warning",
          );
        }
      } else if (result === "no-binary") {
        if (ctx.hasUI) {
          ctx.ui.notify(
            "未找到 detour 二进制。设置 DETOUR_BIN=/path/to/detour（Windows 上是 detour.exe），或用 detour.sh 手动启动。",
            "warning",
          );
        }
      } else {
        if (ctx.hasUI) ctx.ui.notify("detour 启动失败，看 detour.log", "error");
      }
    }

    // 密钥提示（只在没配好时打扰一次）
    const auth = (ctx.modelRegistry as { getProviderAuthStatus?: (p: string) => { configured?: boolean } | undefined })
      .getProviderAuthStatus?.("opencode-go");
    if (auth && !auth.configured && ctx.hasUI) {
      ctx.ui.notify("opencode-go 未配置密钥：export OPENCODE_API_KEY=sk-... 或 /login opencode-go", "warning");
    }
  });

  // 3) /detour 管理命令。
  pi.registerCommand("detour", {
    description: "管理本地 detour 转发（status/start/stop/restart/check/env/key）",
    handler: async (args: string, ctx: ExtensionCommandContext) => {
      const current = loadConfig();
      const verb = (args ?? "").trim().split(/\s+/)[0] || "status";
      const { host, port } = parseHostPort(current.baseUrl);
      const notify = (msg: string, level: "info" | "warning" | "error" = "info") => {
        if (ctx.hasUI) ctx.ui.notify(msg, level);
      };

      switch (verb) {
        case "status": {
          const up = await portOpen(host, port, 600);
          const probe = up ? await probeRelay(current.baseUrl) : undefined;
          notify([
            `模式: ${current.mode}${current.mode === "auto" ? `（本次启动判定: ${state.directOk ? "直连" : "经 detour"}）` : ""}`,
            state.directOk
              ? "当前直连 opencode.ai，不走代理"
              : `detour: ${up ? "运行中" : "未运行"} (${current.baseUrl})`,
            up ? `链路: ${probe?.ok ? "正常" : `异常 (${probe?.error ?? `HTTP ${probe?.status}`})`}` : "链路: 未探测（detour 未运行）",
            `改写 provider: ${rewriteTargets(current).join(", ")}（其余直连）`,
            `上游: ${current.upstream}`,
            `代理: ${current.proxy}（仅 detour 链路）`,
            `配置来源: ${configSourceText(current)}`,
            `拉起传参: ${overrideText(current)}`,
            `全局代理: ${globalProxyEnv() ?? "未设置（qwen 等 provider 直连）"}`,
            state.noProxyAdded ? `NO_PROXY 直连: ${state.noProxyAdded}` : "",
            current.bin ? `二进制: ${current.bin}` : "二进制: 未找到（DETOUR_BIN 或 detour.sh）",
          ].filter(Boolean).join("\n"), state.directOk ? "info" : (probe?.ok === false ? "warning" : "info"));
          return;
        }
        case "start": {
          if (state.directOk) {
            notify("当前可直连 opencode.ai（auto/never 判定），无需 detour。如要强制走 detour 请设 DETOUR_MODE=always。", "info");
            return;
          }
          const result = await ensureDetour(current);
          notify(
            result === "already-running" ? "detour 已在运行" :
            result === "started" ? "detour 已启动" :
            result === "no-binary" ? "未找到 detour 二进制（设置 DETOUR_BIN）" :
            "启动失败，看 detour.log",
            result === "started" || result === "already-running" ? "info" : "error",
          );
          return;
        }
        case "stop": {
          const result = await stopDetour(current);
          notify(result === "stopped" ? "detour 已停止" : `停止失败（${result}）`, result === "stopped" ? "info" : "warning");
          return;
        }
        case "restart": {
          if (state.directOk) {
            notify("当前可直连，无需 detour（auto/never 判定）。设 DETOUR_MODE=always 可强制走 detour。", "info");
            return;
          }
          await stopDetour(current);
          const result = await ensureDetour(current);
          notify(result === "started" || result === "already-running" ? "detour 已重启" : `重启失败（${result}）`, "info");
          return;
        }
        case "check": {
          // 直连可达性 + detour 链路一起报
          const direct = state.directOk
            ? { ok: true, status: undefined }
            : await probeDirect(current.upstream);
          const up = state.directOk ? false : await portOpen(host, port, 600);
          const relay = up ? await probeRelay(current.baseUrl) : undefined;
          notify([
            `直连 opencode.ai: ${state.directOk ? "可达" : "不可达"}`,
            state.directOk
              ? "  当前走直连，链路正常"
              : `detour: ${up ? "运行中" : "未运行"}${relay ? `，链路 ${relay.ok ? "正常" : `异常 (${relay.error ?? `HTTP ${relay.status}`})`}` : ""}`,
          ].join("\n"), state.directOk || relay?.ok ? "info" : "error");
          return;
        }
        case "env": {
          notify(fmtConfig(current), "info");
          return;
        }
        case "key": {
          notify([
            "OpenCode Go 密钥配置（二选一）:",
            "  1) export OPENCODE_API_KEY=sk-... 再启动 pi",
            "  2) 在 pi 里执行 /login opencode-go 输入密钥",
            "配好后 /model 选择 opencode-go 模型即可。",
          ].join("\n"), "info");
          return;
        }
        default: {
          notify(`用法: /detour {status|start|stop|restart|check|env|key}`, "warning");
        }
      }
    },
  });
}