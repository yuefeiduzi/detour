## 下载与运行

| 平台 | 资产 |
|---|---|
| Windows x64 | `detour-<版本>-windows-amd64.zip`（解压后得到 `detour.exe`） |
| macOS Apple Silicon | `detour-<版本>-darwin-arm64.tar.gz` |
| macOS Intel | `detour-<版本>-darwin-amd64.tar.gz` |

macOS / Linux：

```bash
tar xzf detour-<版本>-darwin-arm64.tar.gz
./detour -print-config                      # 看生效配置（默认监听 127.0.0.1:8787）
./detour -check                             # 体检代理链路（梯子 → 上游）
./detour -proxy http://127.0.0.1:7890       # 梯子端口不是 7897 时指定
./detour                                    # 前台运行；后台跑用 ./detour.sh start
```

Windows（cmd / PowerShell）：

```bat
detour.exe -print-config
detour.exe -check
detour.exe
```

配置文件默认读 `~/.detour/detour.json`（Windows 是 `%USERPROFILE%\.detour\detour.json`），
格式参考包里的 `detour.example.json`；也可以直接用 `DETOUR_PROXY` 等环境变量。
完整说明、pi / opencode 接法见仓库 README。

## 校验与签名

```bash
shasum -a 256 -c SHA256SUMS.txt            # macOS
certutil -hashfile <文件名> SHA256          # Windows
```

这些二进制没有代码签名和公证：

- macOS 首次运行若被 Gatekeeper 拦下，`xattr -d com.apple.quarantine ./detour` 解除隔离；
- Windows SmartScreen 提示时选「更多信息 → 仍要运行」。
