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
 * 零运行时依赖：只 import 类型 + Node 内置模块，可单文件复制到
 * ~/.pi/agent/extensions/ 使用，也可 pi install git:... 安装。
 *
 * 环境变量（均可选）:
 *   DETOUR_MODE          auto(默认) | always(强制走detour) | never(强制直连)
 *   DETOUR_BASE_URL      本地 detour 地址，默认 http://127.0.0.1:8787
 *   DETOUR_LISTEN        兼容 detour.sh 的写法（host:port），默认 127.0.0.1:8787
 *   DETOUR_UPSTREAM      实际上游，默认 https://opencode.ai/zen/go/v1/
 *   DETOUR_PROXY         梯子代理，默认 http://127.0.0.1:7897
 *   DETOUR_BIN           detour 二进制路径（默认自动查找：扩展同级的 ../detour、
 *                        ~/self-git/detour/detour、PATH）
 *   DETOUR_EXTRA_PROVIDERS 逗号分隔的额外 provider 名，同样改写 baseUrl 到本地
 *   PI_DETOUR_AUTO_START 设为 0 关闭自动拉起 detour
 */
import type { ExtensionAPI, ExtensionCommandContext } from "@earendil-works/pi-coding-agent";
import { spawn } from "node:child_process";
import { connect } from "node:net";
import { existsSync, openSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const EXT_DIR = typeof __dirname !== "undefined"
  ? __dirname
  : dirname(fileURLToPath(import.meta.url));

const DEFAULT_BASE_URL = "http://127.0.0.1:8787";
const DEFAULT_UPSTREAM = "https://opencode.ai/zen/go/v1/";
const DEFAULT_PROXY = "http://127.0.0.1:7897";
const DIRECT_PROBE_TIMEOUT_MS = 7000; // 直连探测超时；公司网络典型表现为连接挂起

type DetourMode = "auto" | "always" | "never";

interface DetourConfig {
  baseUrl: string;          // full local URL, no trailing slash, e.g. http://127.0.0.1:8787
  listen: string;           // host:port form for detour flags
  upstream: string;
  proxy: string;
  mode: DetourMode;
  autoStart: boolean;
  extraProviders: string[];
  bin?: string;             // detour binary path found
  script?: string;          // detour.sh path found next to the binary
}

interface RuntimeState {
  directOk: boolean;        // true = 直连可达，直接走上游（不覆盖、不拉起 detour）
  mode: DetourMode;
}

const state: RuntimeState = { directOk: true, mode: "auto" };

function resolveBaseUrl(): string {
  const env = process.env;
  const explicit = env.DETOUR_BASE_URL?.trim();
  if (explicit) return explicit.replace(/\/+$/, "");
  const listen = env.DETOUR_LISTEN?.trim();
  if (listen) return `http://${listen.replace(/^https?:\/\//, "")}`;
  return DEFAULT_BASE_URL;
}

function resolveMode(): DetourMode {
  const m = process.env.DETOUR_MODE?.trim().toLowerCase();
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

function findDetour(): { bin: string; script?: string } | undefined {
  const env = process.env;
  const candidates: string[] = [];
  if (env.DETOUR_BIN?.trim()) candidates.push(env.DETOUR_BIN.trim());
  candidates.push(join(EXT_DIR, "..", "detour"));            // repo layout: pi-extension/../detour
  candidates.push(join(homedir(), "self-git", "detour", "detour"));
  candidates.push("detour");                                 // PATH lookup
  for (const c of candidates) {
    if (!c || !c.includes("/")) continue;
    if (!existsSync(c)) continue;
    const script = join(dirname(c), "detour.sh");
    return { bin: c, script: existsSync(script) ? script : undefined };
  }
  return undefined;
}

function loadConfig(): DetourConfig {
  const env = process.env;
  const baseUrl = resolveBaseUrl();
  const found = findDetour();
  return {
    baseUrl,
    listen: listenFromBaseUrl(baseUrl),
    upstream: env.DETOUR_UPSTREAM?.trim() || DEFAULT_UPSTREAM,
    proxy: env.DETOUR_PROXY?.trim() || DEFAULT_PROXY,
    mode: resolveMode(),
    autoStart: env.PI_DETOUR_AUTO_START !== "0",
    extraProviders: (env.DETOUR_EXTRA_PROVIDERS ?? "")
      .split(",").map((s) => s.trim()).filter(Boolean),
    bin: found?.bin,
    script: found?.script,
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
    const ok = res.status !== 502;
    return { ok, status: res.status };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

/**
 * Start detour if it is not already listening. Prefers detour.sh (manages its
 * own pidfile/log), otherwise spawns the binary directly with a log file next
 * to it. Never touches a detour that is already running.
 */
async function ensureDetour(cfg: DetourConfig): Promise<"already-running" | "started" | "no-binary" | "start-failed"> {
  const { host, port } = parseHostPort(cfg.baseUrl);
  if (await portOpen(host, port, 500)) return "already-running";
  if (!cfg.bin) return "no-binary";

  if (cfg.script) {
    spawn("bash", [cfg.script, "start"], {
      env: {
        ...process.env,
        DETOUR_LISTEN: cfg.listen,
        DETOUR_UPSTREAM: cfg.upstream,
        DETOUR_PROXY: cfg.proxy,
      },
      detached: true,
      stdio: "ignore",
    }).unref();
  } else {
    const logPath = join(dirname(cfg.bin), "detour.log");
    const fd = openSync(logPath, "a");
    spawn(cfg.bin, ["-listen", cfg.listen, "-upstream", cfg.upstream, "-proxy", cfg.proxy], {
      detached: true,
      stdio: ["ignore", fd, fd],
    }).unref();
  }

  for (let i = 0; i < 16; i++) {
    await sleep(250);
    if (await portOpen(host, port, 300)) return "started";
  }
  return "start-failed";
}

async function stopDetour(cfg: DetourConfig): Promise<string> {
  const { host, port } = parseHostPort(cfg.baseUrl);
  if (cfg.script) {
    spawn("bash", [cfg.script, "stop"], { detached: true, stdio: "ignore" }).unref();
    for (let i = 0; i < 20; i++) {
      await sleep(100);
      if (!(await portOpen(host, port, 200))) return "stopped";
    }
    return "still-running";
  }
  if (cfg.bin) {
    spawn("pkill", ["-f", `detour -listen ${cfg.listen}`], { detached: true, stdio: "ignore" }).unref();
    for (let i = 0; i < 20; i++) {
      await sleep(100);
      if (!(await portOpen(host, port, 200))) return "stopped";
    }
  }
  return "still-running";
}

function fmtConfig(cfg: DetourConfig): string {
  return [
    `本地地址   ${cfg.baseUrl}`,
    `上游       ${cfg.upstream}`,
    `代理       ${cfg.proxy}`,
    `模式       ${cfg.mode}${cfg.mode === "auto" ? `（本次: ${state.directOk ? "直连" : "经 detour"}）` : ""}`,
    `自动拉起   ${cfg.autoStart ? "开" : "关 (PI_DETOUR_AUTO_START=0)"}`,
    `detour 二进制 ${cfg.bin ? cfg.bin : "(未找到)"}`,
    cfg.script ? `管理脚本   ${cfg.script}` : "",
    cfg.extraProviders.length ? `额外 provider: ${cfg.extraProviders.join(", ")}` : "",
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

  if (!state.directOk) {
    // 直连不可用（或强制走 detour）：覆盖 baseUrl，保留内置模型与 OPENCODE_API_KEY 认证。
    pi.registerProvider("opencode-go", { baseUrl: cfg.baseUrl });
    for (const name of cfg.extraProviders) {
      pi.registerProvider(name, { baseUrl: cfg.baseUrl });
    }
  }
  // 直连可用时：不覆盖任何东西，pi 保持内置行为直连上游。

  // 2) session 开始：确保 detour 在跑（仅当需要走 detour），并提示直连/转发状态。
  pi.on("session_start", async (_event, ctx) => {
    const current = loadConfig();

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
              : `detour 端口已开，但链路异常: ${probe.error ?? `HTTP ${probe.status}`} — 检查梯子/代理规则`,
            probe.ok ? "info" : "warning",
          );
        }
      } else if (result === "no-binary") {
        if (ctx.hasUI) {
          ctx.ui.notify(
            "未找到 detour 二进制。设置 DETOUR_BIN=/path/to/detour，或用 detour.sh 手动启动。",
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
            `上游: ${current.upstream}`,
            `代理: ${current.proxy}`,
            current.bin ? `二进制: ${current.bin}` : "二进制: 未找到（DETOUR_BIN 或 detour.sh）",
          ].join("\n"), state.directOk ? "info" : (probe?.ok === false ? "warning" : "info"));
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