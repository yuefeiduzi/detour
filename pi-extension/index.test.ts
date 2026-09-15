/**
 * bun test pi-extension
 *
 * 锁定两条边界：
 *  1) 只有 opencode-go（默认）会被改写到本地 detour，qwen-token-plan 等 provider 绝不触碰；
 *  2) 只有显式设置 HTTP(S)_PROXY 时才会影响全部 provider，此时 NO_PROXY 里写入的域名保持直连。
 */
import { afterEach, beforeEach, expect, test } from "bun:test";

const EXT = new URL("./index.ts", import.meta.url).pathname;
let seq = 0;

interface Registry {
  registered: Array<{ name: string; config: Record<string, unknown> }>;
}

/** 用桩 ExtensionAPI 跑一遍扩展工厂，返回它注册了哪些 provider。 */
async function loadExtension(): Promise<Registry> {
  const registered: Registry["registered"] = [];
  const pi = {
    registerProvider: (name: string, config: Record<string, unknown>) => registered.push({ name, config }),
    registerCommand: () => {},
    on: () => {},
  };
  const mod: { default: (pi: unknown) => Promise<void> } = await import(`${EXT}?test=${seq++}`);
  await mod.default(pi);
  return { registered };
}

const KEYS = ["HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy",
  "DETOUR_MODE", "DETOUR_EXTRA_PROVIDERS", "DETOUR_DIRECT_HOSTS", "PI_DETOUR_AUTO_START", "DETOUR_BASE_URL"];
const saved = new Map<string, string | undefined>();

beforeEach(() => {
  for (const key of KEYS) {
    saved.set(key, process.env[key]);
    delete process.env[key];
  }
  process.env.DETOUR_MODE = "always"; // 跳过直连探测，避免测试联网
});

afterEach(() => {
  for (const [key, value] of saved) {
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
  saved.clear();
});

test("走 detour 时只改写 opencode-go，qwen 等 provider 保持原样", async () => {
  const { registered } = await loadExtension();
  expect(registered.map((entry) => entry.name)).toEqual(["opencode-go"]);
  expect(registered[0].config).toEqual({ baseUrl: "http://127.0.0.1:8787" });
});

test("直连可达时不改写任何 provider", async () => {
  process.env.DETOUR_MODE = "never";
  const { registered } = await loadExtension();
  expect(registered).toEqual([]);
});

test("DETOUR_EXTRA_PROVIDERS 是显式 opt-in", async () => {
  process.env.DETOUR_EXTRA_PROVIDERS = "my-other-provider, another";
  const { registered } = await loadExtension();
  expect(registered.map((entry) => entry.name)).toEqual(["opencode-go", "my-other-provider", "another"]);
});

test("没有全局代理时不写 NO_PROXY", async () => {
  await loadExtension();
  expect(process.env.NO_PROXY).toBeUndefined();
});

test("有全局代理时本地 relay 与 DETOUR_DIRECT_HOSTS 写入 NO_PROXY（qwen 保持直连）", async () => {
  process.env.HTTPS_PROXY = "http://127.0.0.1:7897";
  process.env.DETOUR_DIRECT_HOSTS = "token-plan.cn-beijing.maas.aliyuncs.com";
  const { registered } = await loadExtension();
  expect(process.env.NO_PROXY).toBe("127.0.0.1,localhost,token-plan.cn-beijing.maas.aliyuncs.com");
  // 全局代理下依然只改写 opencode-go
  expect(registered.map((entry) => entry.name)).toEqual(["opencode-go"]);
});

test("已有 NO_PROXY 条目不会被覆盖，重复加载幂等", async () => {
  process.env.HTTPS_PROXY = "http://127.0.0.1:7897";
  process.env.NO_PROXY = "example.com";
  process.env.DETOUR_DIRECT_HOSTS = "example.com";
  await loadExtension();
  expect(process.env.NO_PROXY).toBe("example.com,127.0.0.1,localhost");
  await loadExtension();
  expect(process.env.NO_PROXY).toBe("example.com,127.0.0.1,localhost");
});