# GenImage MCP（open-image-mcp）

GenImage MCP 是一个可自托管的桌面应用（macOS / Windows），界面基于 Go 和 [viva](https://github.com/vivaui/viva) 实现，把任意 **OpenAI 兼容** 的图像接口（`/v1/images/generations`、`/v1/images/edits`）封装成标准 MCP 工具，通过 **Streamable HTTP** 提供给 Claude Desktop、Codex、Claude Code、Cursor、Cherry Studio 等客户端使用。

- 和 Cherry Studio 一样，服务商与模型是解耦的：每个服务商单独配置 base_url、API Key 和模型列表，调用时按模型自动找到对应的服务商。
- 支持 OpenAI、new-api、one-api 等中转，以及 SiliconFlow 的非标准格式（通过 `dialect` 选择）。
- 生成的图片保存在本机，工具结果返回绝对路径和缩略图预览，后续工具可以直接引用这个路径。
- 自带调试台，可以直接测试生图和改图。图库页列出所有历史图片，可以预览大图、查看提示词和删除图片；MCP 把图片保存到其他目录时，图库也会保留一份。
- 「关于」页可以检查更新：有新版本时提示，并直接给出本平台安装包的下载按钮。

## 下载安装

**macOS 推荐用 Homebrew 安装**：

```bash
brew install --cask 972579/tap/genimage-mcp
```

用 Homebrew 安装后，第一次打开不会被 Gatekeeper 拦截：cask 装好后会清除下载标记（应用是 ad-hoc 签名，没有经过 Apple 公证）。之后的版本由应用内更新完成，`brew upgrade` 默认跳过它（加 `--greedy` 才会升级）。卸载用 `brew uninstall --cask genimage-mcp`，加 `--zap` 会一并删除 `~/.gen-openai-image` 里的配置和历史记录（图片目录不会删除）。

也可以到 [Releases](https://github.com/972579/open-image-mcp/releases/latest) 下载对应平台的文件：

| 平台 | 文件 |
|---|---|
| macOS（Apple 芯片） | `genimage-<版本>-darwin-arm64.dmg`（或 `.zip`） |
| macOS（Intel） | `genimage-<版本>-darwin-amd64.dmg`（或 `.zip`） |
| Windows x64 | `genimage-<版本>-windows-amd64.zip` |
| Windows ARM | `genimage-<版本>-windows-arm64.zip` |

`SHA256SUMS.txt` 里有每个文件的校验值。

**macOS**（需要 macOS 13 及以上）：打开 .dmg，把 GenImage MCP 拖进「应用程序」。安装包是 ad-hoc 签名，没有经过 Apple 公证，第一次打开时 macOS 会提示「无法验证开发者」。点「完成」后打开「系统设置 → 隐私与安全性」，在页面下方点「仍要打开」并确认即可（macOS 15 起右键「打开」已经不能绕过这个提示）。也可以在终端执行：

```bash
xattr -dr com.apple.quarantine "/Applications/GenImage MCP.app"
```

只有第一次安装需要这一步：之后的版本由应用内更新直接下载安装，不会再被拦截。

**Windows**：解压 zip，双击 `genimage.exe`。安装包没有签名，SmartScreen 弹出提示时点「更多信息 → 仍要运行」。Windows 版由 viva 的实验性 Windows 后端构建，遇到问题欢迎提 Issue。

### 检查更新

「关于」页有「检查更新」按钮，默认在启动时也会自动检查一次（可在同一页关闭，对应配置 `update.check_on_startup`）。发现新版本后点「下载并更新」：应用会在内部下载安装包，校验后自动退出、替换自己并重新打开，MCP 服务会中断几秒。配置、历史和图片都保存在用户目录，升级不会丢失。

- 更新信息来自每个 Release 附带的 `latest.json`，它用 Ed25519 私钥签名，应用只信任内置公钥能验证的版本；安装包的大小和 SHA-256 必须与其中记录的一致，macOS 上还会检查新版本的代码签名和 bundle id。发布说明从 `api.github.com` 读取，只用于展示。检查时不发送任何配置或个人数据。
- 下载中断或取消后再次更新会从断点继续；下载文件放在 `~/.gen-openai-image/updates`，新版本启动后自动清理。
- 应用需要放在可写的位置（例如「应用程序」文件夹）才能替换自己。直接从 .dmg 或「下载」文件夹里运行时，macOS 会把应用放到只读的临时位置，这时会提示先移到「应用程序」，并提供浏览器下载作为备选。
- Windows 目前发布的是免安装 zip，应用不能自动替换，发现新版本后会在浏览器中下载 zip。

### 从源码构建

依赖：Go 1.26 及以上；macOS 上还需要 Xcode Command Line Tools。

```bash
go install github.com/vivaui/viva/cmd/viva@v0.6.5
```

```bash
viva package --format app,dmg
```

构建结果是 `build/GenImage MCP.app`。在 Mac 上也可以交叉编译 Windows 版：

```bash
viva package --target windows --arch amd64 --format zip
```

直接运行开发版：

```bash
go run .
```

## 快速开始

1. 打开应用，进入 **服务商** 页，点「新增服务商」，填写名称、Base URL（要包含 `/v1`）、API Key 和图像模型。可以点「测试连接」检查连通性，或用「从 /models 导入」直接选模型。最后点 **保存**。
2. 进入 **服务** 页，点 **启动服务**，默认地址是 `http://127.0.0.1:8765/mcp`。
3. 进入 **客户端配置** 页，复制对应客户端的配置，粘贴到客户端的配置文件里（见下文）。
4. 在 Claude 或 Codex 里说「画一张赛博朋克风格的雨夜街道」，Agent 就会调用 `generate_image`，图片保存到 `~/Pictures/gen-openai-image/日期/` 下。

也可以不打开窗口，只运行服务（适合开发调试或配合 launchd 使用）：

```bash
go run . serve --port 8765
```

Windows 的发布版是图形界面程序，`genimage.exe serve` 不会在命令行打印输出，运行情况请看日志文件。

## 客户端配置

「客户端配置」页会按当前的地址和 Token 自动生成配置。下面是不设 Token 时的示例。

**Claude Desktop**（macOS：`~/Library/Application Support/Claude/claude_desktop_config.json`；Windows：`%APPDATA%\Claude\claude_desktop_config.json`）。这个配置文件只能启动本地命令，所以要借助 `mcp-remote` 桥接到本地的 HTTP 服务，因此需要安装 Node.js。Windows 上应用生成的配置会改用 `"command": "cmd"`、`"args": ["/c", "npx", ...]` 启动：

```json
{
  "mcpServers": {
    "gen-image": {
      "command": "npx",
      "args": ["-y", "mcp-remote", "http://127.0.0.1:8765/mcp"]
    }
  }
}
```

**Codex**（`~/.codex/config.toml`，Windows 为 `%USERPROFILE%\.codex\config.toml`，TOML 格式）：

```toml
[mcp_servers.gen-image]
url = "http://127.0.0.1:8765/mcp"
tool_timeout_sec = 300
```

也可以用命令添加：

```bash
codex mcp add gen-image --url http://127.0.0.1:8765/mcp
```

**Claude Code**：

```bash
claude mcp add --transport http gen-image http://127.0.0.1:8765/mcp
```

**Cursor**（`~/.cursor/mcp.json`，Windows 为 `%USERPROFILE%\.cursor\mcp.json`）：`{"mcpServers":{"gen-image":{"url":"http://127.0.0.1:8765/mcp"}}}`

**Cherry Studio**：设置 → MCP 服务器 → 添加，类型选「可流式传输的 HTTP」，填入上面的 URL。

设置 Token 后，客户端请求要带上 `Authorization: Bearer <token>` 请求头，生成的配置示例会自动带上。

## 配置

配置文件：`~/.gen-openai-image/config.yaml`（Windows 为 `%USERPROFILE%\.gen-openai-image\config.yaml`）。完整示例见 [config.example.yaml](config.example.yaml)。界面里的修改都会写回这个文件，在 macOS 上文件权限为 0600。「关于」页列出了配置、历史、日志和图片的实际位置。

各层优先级：**环境变量 > 配置文件 > 启动参数 > 内置默认值**。

| 环境变量 | 作用 |
|---|---|
| `OPENAI_API_KEY` / `OPENAI_BASE_URL` / `OPENAI_MODELS` | 自动生成一个名为 `env` 的服务商，排在最前面，不会写入配置文件 |
| `DEFAULT_MODEL` | 默认模型 |
| `OUTPUT_DIR` | 图片保存目录 |
| `GENIMAGE_HOST` / `GENIMAGE_PORT` / `GENIMAGE_TOKEN` | 监听地址、端口、访问 Token |
| `GENIMAGE_CONFIG` | 配置文件路径 |

启动参数：`--config`、`--host`、`--port`、`--output-dir`、`--default-model`、`--autostart`。

> 从 Finder 或程序坞打开 .app 时，应用拿不到 shell 里设置的环境变量。环境变量只有在终端里直接运行程序时才生效，例如 `"/Applications/GenImage MCP.app/Contents/MacOS/genimage"`。

服务商的 `api_key`、`base_url`、`headers` 支持 `${ENV_NAME}` 引用。在界面里保存时，这些引用会原样保留，不会被替换成实际的值。

## 工具

| 工具 | 必填参数 | 说明 |
|---|---|---|
| `generate_image` | `prompt` | 文生图。可选参数：`model`、`provider`、`size`、`n`(1-10)、`quality`、`style`、`seed`、`response_format`、`output_format`、`background`、`negative_prompt`、`output_dir`、`filename_prefix`、`include_preview`、`extra` |
| `edit_image` | `prompt`、`images` | 图生图/编辑。`images` 可以是绝对路径、data URL 或 base64，最多 16 张。可选 `mask` 以及与生成相同的参数。模型需要在配置中勾选「编辑」 |
| `list_models` | — | 列出可用模型、所属服务商以及支持生成还是编辑，可按 `provider`、`capability` 过滤 |
| `list_generated_images` | — | 按时间倒序列出历史图片，可按 `limit`、`offset`、`model`、`keyword`、`since` 过滤 |

返回内容包括三部分：一段文字说明（列出每张图的绝对路径）、最多 4 张 ≤512px 的 JPEG 预览，以及 `structuredContent`（含 `images[].path`、尺寸、模型、服务商、耗时、每次尝试的记录）。

**保存位置与图库**：不传 `output_dir` 时，图片保存在默认图片目录（设置页的「图片目录」，默认 `~/Pictures/gen-openai-image`）。传了 `output_dir` 时，图片保存到该目录（`images[].path`），默认图片目录里也会保留一份同名副本（`images[].gallery_path`）。这样即使项目里的文件被移走或删除，图库里也还能看到所有生成过的图片。`list_generated_images` 返回的 `path` 是图库里的那份，`output_path` 是当时指定目录里的那份。

**图库页**：点缩略图打开大图预览，右侧显示完整的提示词（以及服务商改写后的提示词），可以一键复制。预览里的「删除」会在确认后移除这条记录并删除图库里的图片文件（不进废纸篓）。指定 `output_dir` 时保存在那个目录里的文件不会被删除。

## 兼容性

| dialect | 生成 | 编辑 | 响应 |
|---|---|---|---|
| `openai`（默认） | `POST /images/generations`（JSON） | `POST /images/edits`（multipart；一张图用 `image` 字段，多张用 `image[]`，每个文件带真实的 MIME 类型） | `data[].b64_json` 或 `data[].url` |
| `siliconflow` | 同一个端点，`size` 换成 `image_size`，`n` 换成 `batch_size` | 同样走 `/images/generations`，参考图以 data URL 放在 `image` / `image2` / `image3` 字段 | `images[].url` |

- 不论哪种 dialect，解析时都同时识别 `data[]` 和 `images[]`。返回的如果是 URL，会立刻下载保存，因为有些服务商的链接很快就会失效。
- `response_format` 只在调用时显式指定才会发送，因为 gpt-image 系列不接受这个参数。
- 各家的私有参数可以通过三层透传：服务商级的 `extra_body`、模型级的 `extra`、调用时的 `extra`。后面的覆盖前面的。

## 常见问题

- **端口被占用**：到「设置」页换一个端口，然后在「服务」页重启。
- **[鉴权失败] 401**：检查 API Key，以及 base_url 有没有包含 `/v1`。
- **[模型不支持]**：确认模型 ID 拼写正确，并且这个服务商确实提供该模型。new-api 返回「无可用渠道」也会被识别为这一类错误，并自动切换到下一个提供同名模型的服务商。
- **客户端超时**：生图期间服务会持续发送 MCP 进度通知，避免客户端提前断开。Codex 用户可以调大 `tool_timeout_sec`。
- **看实际发了什么请求**：在调试台打开「显示请求 / 响应详情」，里面的密钥和 base64 内容都已脱敏。

## 安全

- 服务默认只监听 `127.0.0.1`，并开启 go-sdk 自带的 DNS 重绑定防护。如果要监听非回环地址，必须先设置 Token。
- API Key 在界面上只显示 `sk-****abcd` 这样的脱敏形式。日志中的密钥会被统一替换掉，日志文件在 `~/.gen-openai-image/logs/app.log`。
- macOS 上配置文件和历史记录的权限都是 0600。
- 检查更新只访问 `api.github.com` 读取公开的版本信息；只会在浏览器中打开 `https://github.com/` 下的链接。

## 开发

```bash
go test ./...          # 单元测试 + MCP 集成测试 + 无窗口 GUI 测试（viva Driver）
go vet ./...
viva vet ./...
viva dev .             # 热重载开发
```

发布新版本：同时修改 `internal/version/version.go` 和 `viva.toml` 里的版本号（测试会检查两者一致），提交并推送后执行：

```bash
scripts/release.sh
```

脚本会跑测试，构建 macOS（arm64 / amd64）和 Windows（amd64 / arm64）安装包，生成签名的更新源 `latest.json` / `latest.json.sig` 和 `SHA256SUMS.txt`，并用 `gh` 创建 GitHub Release。如果存在 `build/RELEASE_NOTES.md`，会用它作为更新说明。加 `--dry-run` 只构建不发布。

- **更新源签名密钥**：`latest.json` 用 Ed25519 私钥签名，公钥写在 `internal/update/feed.go` 的 `PublicKeys`。私钥默认读 `~/.genimage-release/update.key`（可用 `GENIMAGE_UPDATE_KEY_FILE` 指定文件，或用 `VIVA_UPDATE_KEY` 直接传入密钥内容），**不要提交到仓库，并妥善备份**：丢失后已安装的版本将无法再验证新的更新。需要换密钥时，用 `viva update-keygen` 生成新密钥，把新公钥加进 `PublicKeys` 先发布一个版本（仍用旧私钥签名），之后再换用新私钥签名。
- **macOS 签名**：默认 ad-hoc 签名。设置 `APPLE_SIGNING_IDENTITY`（Developer ID Application 证书）后改用正式签名；再设置 `APPLE_NOTARY_PROFILE`（`xcrun notarytool store-credentials` 创建）时还会自动公证，首次安装也不会再有提示。
- **Homebrew tap**：`scripts/brew-cask.sh` 根据本次构建的两个 macOS zip 生成 `build/genimage-mcp.rb`。发布成功后，脚本会把它提交到 `972579/homebrew-tap` 仓库的 `Casks/genimage-mcp.rb`（可用 `GENIMAGE_TAP_REPO` 改成别的仓库）。这个仓库不存在时只给出提示，不影响发布。cask 使用 Homebrew 5 起的写法（`postflight_steps`），已在 Homebrew 7 上验证过 `brew style` 和实际安装。

目录结构：

```
main.go                 入口：GUI / serve / version
internal/config         分层配置（env > file > flags）、保存、脱敏
internal/imageapi       OpenAI 兼容客户端、dialect、错误分类、响应解析
internal/provider       模型到服务商的路由
internal/storage        图片落盘、history.jsonl、缩略图
internal/engine         重试 / 回退 / 保存：GUI 调试台与 MCP 工具共用
internal/mcpserver      4 个 MCP 工具、Streamable HTTP 启停、Token 鉴权
internal/clientcfg      各客户端配置片段（按 macOS / Windows 生成路径）
internal/update         签名更新源：检查、下载、校验并替换应用
internal/version        版本号与仓库地址
internal/logx           日志（文件 + 环形缓冲 + 脱敏）
internal/appcore        服务装配
internal/gui            viva 界面
```

## 许可证

本项目以 [MIT 许可证](LICENSE) 发布。

发布的安装包静态链接了以下第三方库，它们各自遵循自己的许可证：[viva](https://github.com/vivaui/viva)（Apache-2.0）、[MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)、[jsonschema-go](https://github.com/google/jsonschema-go)、[go-yaml](https://github.com/yaml/go-yaml)、[golang.org/x/image](https://pkg.go.dev/golang.org/x/image) 等，完整列表见 `go.mod`。
