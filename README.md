# MacBridge Go

按 [mac-developer-bridge](https://github.com/alexanderradahl/mac-developer-bridge/tree/fea70d1a3c5524164f2159f6063ba685fef91324) 的功能契约重新实现的原生版本。

**服务端、HTTP/OAuth、shell、文件、后台任务、PTY、子 MCP、Chrome Native Messaging 和 Responses 适配均为 Go 新实现。** 菜单栏用 Swift/AppKit，Chrome 扩展用浏览器原生 JavaScript，二者也独立重写。运行服务端不需要 Node.js、Python 或 Perl。

保留原项目的 33 个 MCP 工具名称、主要参数与结果字段，以及本机完整权限的定位。仅复用了原项目公开的工具描述和 JSON Schema 作为兼容规格，没有调用或打包其 JavaScript/Perl 服务端。

## 让 Codex 帮你安装

仓库附带 [macbridge-go-install Skill](skills/macbridge-go-install/SKILL.md)，负责检查已有安装、构建、Chrome 扩展、ChatGPT Secure MCP Tunnel 接入及真实工具验收。

在 Codex 中发送：

```text
使用 $skill-installer 安装这个 Skill：
https://github.com/pengjunfeng11/macbridge-go/tree/main/skills/macbridge-go-install
```

安装成功后，下一条消息发送：

```text
使用 $macbridge-go-install，把 MacBridge Go 安装到我的 Mac，并接入 ChatGPT 聊天模式；如果已经安装，先检查并复用现有配置。
```

私有仓库需要当前 GitHub 账号具有访问权限。安装 Skill 只添加操作指南，不会自行启动或重装 MacBridge；也可以继续按下面的步骤手动安装。

## 直接使用

仓库存放源码，不包含编译产物或本机运行配置。需要 macOS 13+、Go 1.23+ 和 Xcode Command Line Tools。从源码安装：

```sh
git clone https://github.com/pengjunfeng11/macbridge-go.git
cd macbridge-go
make build
```

构建会生成 Apple Silicon / Intel 通用二进制 `bin/macbridge` 和 `MacBridge Go.app`。把整个目录放到准备长期保留的位置，再启动服务：

```sh
./bin/macbridge init
./bin/macbridge stdio
```

stdio 模式将标准输出专用于 MCP JSON-RPC。配置 MCP 客户端时，把 `command` 指向 `bin/macbridge` 的绝对路径，`args` 设为 `["stdio"]`。

```json
{
  "mcpServers": {
    "macbridge": {
      "command": "/absolute/path/macbridge-go/bin/macbridge",
      "args": ["stdio"]
    }
  }
}
```

HTTP 模式：

```sh
./bin/macbridge http
# http://127.0.0.1:8787/mcp
```

`init` 创建本地解锁文件、Bearer Token 和 OAuth 客户端 ID。Token 保存在数据目录的 `http-token`，命令不会打印 Token。HTTP 支持 Bearer 和 OAuth 授权码 + PKCE、刷新、撤销。可从菜单栏复制 MCP URL、Client ID、Bearer Token 和完整 ChatGPT 接入信息。

双击 `MacBridge Go.app`，点击菜单中的 **Start**。`desktop` 进程会启动 HTTP；若已安装 `cloudflared`，自动使用匹配本地端口的命名隧道，或创建 quick tunnel。未安装时提供本地 HTTP。Connection Settings 可选择 `auto / off / named / quick`、端口和固定公网 URL。

## Chrome 与 ChatGPT

```sh
./bin/macbridge install-browser
```

在 `chrome://extensions` 开启开发者模式，加载命令输出的 `chrome-extension` 目录，然后点击扩展图标一次。扩展使用新生成的稳定 ID，首次连接绑定 Chrome profile；可通过 `install-browser --profile-id ...` 预先指定。Native Host 名称为 `com.macbridge.native`。

`chrome_workspace_setup` 准备 MDB 标签组；`chrome_open / navigate / snapshot / click / fill / close` 操作该工作区。扩容等待 Chrome 自然获得焦点后再准备标签，不主动抢走用户当前应用。支持工作区恢复、租用、过期回收与并发请求隔离。

`chatgpt_conversation_start` 支持 runtime 与 raw transport、模型和 thinking effort、Project、新会话、runtime 续聊及 continue-in-work。runtime 路径会检查返回内容与持久化会话的对应关系；raw 路径解析返回流。另保留 OpenAI 扩展状态、任务审计、精确编辑/保存、fetch/XHR 诊断和 runtime inventory 等原项目内部 Native Messaging 方法。

本机实验接口：

- `POST /experimental/chatgpt/conversation`
- `POST /v1/responses`：消息、function/custom 工具、namespace、工具结果、JSON 和 SSE 输出。

实验接口沿用原项目限制，使用直接 loopback 连接和静态 Bearer。Responses 会把请求编译成浏览器 ChatGPT 回合；SSE 先发创建/运行事件和 keepalive，浏览器回合完成后发内容及工具事件，不承诺上游逐 token 流或真实 token usage。

## 后台任务与文件

后台任务由同一个 Go 二进制的 `supervise-job` 子命令独立持有。服务端重启后，可继续用 `shell_job_status` 读取日志、完成状态、退出码和信号。输出与日志有大小上限。

PTY 使用真正的系统伪终端，支持字节游标重复读取、ANSI/回车清理、resize、signals、空闲回收和退出后读取。PTY 是进程内会话，不跨服务重启恢复。

文件支持 UTF-8/Base64、分页、权限、链接、复制/移动/删除和 `git apply`。`fs_write` 默认同目录临时文件 + fsync + rename，并保留已有文件权限。新增可选 `fs_read.include_sha256` 与 `fs_write.expected_sha256`，用于检测读取后的内容变化；这不是对其他本机进程的跨进程事务锁。

## 子 MCP 与 Codex 历史

通过 `MAC_DEV_BRIDGE_MCP_SERVERS` 指定 JSON 文件，或 `MAC_DEV_BRIDGE_MCP_SERVERS_JSON` 直接传 JSON。格式与原项目相同：

```json
{
  "providers": [
    {
      "key": "example",
      "command": "/absolute/path/to/mcp-server",
      "args": [],
      "mode": "isolated",
      "callTimeoutMs": 120000,
      "maxResultBytes": 4000000
    }
  ]
}
```

子工具以 `example__tool_name` 暴露。支持现代/旧版协商、分页发现、roots 回调、取消、超时、健康检查、有限重启、原样保留图片/resource/_meta，以及 personal 模式的到期与单次授权。`isolated` 是进程配置模式，不是操作系统沙箱。

`codex_thread_read / list / turns_list` 使用本地 `codex app-server`。用 `CODEX_BIN` 指定可执行文件；保留过滤、分页和 turns 参数。

## 常驻服务与隧道

```sh
./bin/macbridge install        # 安装并启动本地 HTTP LaunchAgent
./bin/macbridge doctor
./bin/macbridge stop           # 撤销解锁、停止本版本服务并核实进程回收
./bin/macbridge uninstall      # 停止并移除本版本 LaunchAgent，保留数据
```

HTTP LaunchAgent 与菜单栏 desktop 是两种启动方式；同一端口选一种即可。`stop` 根据 PID、启动时间、可执行文件与进程组记录核实归属；不能核实的存活进程会返回 `stop incomplete`。

OpenAI Secure MCP Tunnel 保留独立接入方式，需要先安装官方 `tunnel-client`：

```sh
export CONTROL_PLANE_TUNNEL_ID='your-tunnel-id'
export CONTROL_PLANE_API_KEY='your-runtime-key'
export TUNNEL_CLIENT_BIN='/absolute/path/to/tunnel-client'
./bin/macbridge install-tunnel
unset CONTROL_PLANE_API_KEY
```

该命令初始化 stdio profile，将 runtime key 存入 macOS Keychain，安装 `local.macbridge.tunnel` LaunchAgent。已有 profile 可用 `macbridge tunnel` 运行；启动时启用官方 `tunnel-client` 的 stdio 初始化通知兼容选项，支持省略该通知的客户端。Keychain 更新脚本是 `scripts/rotate-tunnel-key.sh`。每个本机 shell/子 MCP 进程都会移除 tunnel runtime key。

### 在 ChatGPT 聊天里使用

1. 按 [官方 Secure MCP Tunnel 指南](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels) 创建隧道、关联自己的 ChatGPT 工作区，并生成具有 Tunnels Read/Use 权限的 runtime API key。安装官方客户端：`brew install openai/tools/tunnel-client`。
2. 使用上面的 `install-tunnel` 命令启动常驻连接；`TUNNEL_CLIENT_BIN` 可用 `command -v tunnel-client` 查到。
3. 在 ChatGPT 设置中开启开发者模式，进入 [插件目录](https://chatgpt.com/plugins)，创建名为 **MacBridge Go** 的插件。连接方式选「隧道」，选择刚创建的隧道；此 stdio 配置的插件身份验证选择「无身份验证」。
4. 新建「聊天」，点击输入框旁的「＋」选择 **MacBridge Go**，直接描述任务，并按需要允许具体工具操作。

例如：「用 MacBridge Go 查看 Documents 目录中的文件」或「检查这个项目并运行测试」。Mac 必须开机、联网并登录；浏览器工具还需要 Chrome 和扩展运行。菜单和可用性以当前账号界面为准。

这条路径是 `ChatGPT Chat → Secure MCP Tunnel → MacBridge Go → Mac`。文件、shell 等工具直接在本机执行，不自动发起 Codex 模型回合。普通 Chat 有自己的消息限制；[Work 与 Codex 共用额度](https://learn.chatgpt.com/docs/pricing)，切到 Work 不会得到另一份独立额度。桥接不会压缩 token，也不承诺无限用量或固定节省比例。

## 配置与迁移

默认路径沿用原项目，可用环境变量分离新旧实例：

| 配置 | 默认值 |
|---|---|
| `MAC_DEV_BRIDGE_DATA_DIR` | `~/Library/Application Support/MacDeveloperBridge` |
| `MAC_DEV_BRIDGE_LOG_DIR` | `~/Library/Logs/MacDeveloperBridge` |
| `MAC_DEV_BRIDGE_SHELL` | `/bin/zsh` |
| `MAC_DEV_BRIDGE_HTTP_PORT` | `8787` |
| `MAC_DEV_BRIDGE_PUBLIC_URL` | 按请求或 desktop 隧道配置 |
| `CODEX_BIN` | `codex` |
| `MAC_DEV_BRIDGE_AUDIT_MODE` | `metadata`，可设 `full` |
| `MAC_DEV_BRIDGE_PTY_MAX_SESSIONS` | `8` |
| `MAC_DEV_BRIDGE_PTY_RING_BYTES` | `262144` |
| `MAC_DEV_BRIDGE_JOB_LOG_MAX_BYTES` | `8000000` |

`runtime.json` 保存 `publicURL / httpPort / tunnelMode / cloudflaredConfig`。`settings.json` 保存可切换的 `strictApprovals`。可使用 `grant-browser --url-pattern 'https://example.com/*'`、`grant-gui --app TextEdit` 创建短期授权。

旧 OAuth 会话不会迁移到新实现的 `oauth-go.json`，需要重新连接；旧版本的正在运行任务和 PTY 不转移进程所有权。Chrome 扩展与 Native Host 也需要安装新版。不要同时让新旧版本写入同一数据目录。

## 构建与验证

需要 Go 1.23+；构建菜单栏还需 Xcode Command Line Tools。Node 仅用于扩展测试。Go 唯一第三方运行依赖是 `github.com/creack/pty v1.1.24`，版本与校验和已固定。

```sh
make test check
make build             # macOS 上同时构建 arm64 / x86_64 CLI 与 App
make acceptance        # 真实编译产物 + 隔离 HTTP/任务/PTY 验收
```

测试范围与尚需真实账号验收的部分见 [验收记录](docs/VALIDATION.md)，功能对应关系见 [功能矩阵](docs/PARITY.md)，架构取舍见 [架构说明](docs/ARCHITECTURE.md)。构建产物使用本地 ad-hoc 签名，尚未做 Apple 公证。

MIT License。原项目契约数据和唯一依赖的声明见 [NOTICE](NOTICE)。
