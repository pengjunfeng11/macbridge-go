# 验收记录

环境：2026-09-09，macOS / Apple Silicon，Go 1.26.2。开发期测试使用隔离临时数据；后续真实安装、Keychain、Chrome 与 ChatGPT Chat 验证见文末补充。

## 已覆盖的执行链

- `go test -race ./...`：host、server、peers、browser 和 CLI。包含真实子进程、PTY、持久后台任务，HTTP/OAuth 内存服务器、native framing/socket，以及假子 MCP/Codex。
- `go vet ./...`、gofmt、shell 语法检查。
- `npm test --prefix chrome-extension`：工作区、Chrome API、页面 DOM、原生消息重连与分片、ChatGPT 契约、任务编辑/探针等确定性测试。Node 只承担测试。
- 原项目 `tests/smoke.mjs` 与 `tests/integration.mjs` 对新的 Go 二进制执行，106 处断言通过。仅将假 Codex fixture 的固定 request ID 改为回显收到的 ID，断言未改；临时 Node 包装器只用于这两份上游测试，不打包到新服务。
- `make acceptance`：实际编译产物启动 HTTP → 401 身份校验 → MCP session → 33 工具目录 → 真 shell → 原子 UTF-8 文件与 SHA-256 → 真 PTY → 停止并重启 HTTP → 读取后台任务的输出与退出码 → `stop` 核实回收。
- `make build`：Go 与 Swift 均生成 arm64/x86_64 通用 Mach-O；App bundle 本地签名并执行 codesign verify。Intel 切片经过编译，未在 Intel 硬件上执行。

## 关键回归

| 问题 | 覆盖 |
|---|---|
| Go typed-nil 响应误输出 JSON null | stdio 通知不产生响应；原项目集成测试 |
| 取消注册与并发调度竞态 | 分发前登记；预先取消禁止副作用；HTTP session 取消 |
| HTTP 目录重复 Codex 工具 | 编译产物断言准确 33 个内置工具 |
| 慢子 MCP 阻塞整个 bridge 启动 | New 快速返回，目录独立等待；Codex 不等待邻接 provider |
| 重启后只见 PID 不见结果 | 独立 supervisor 保留终态；实际 HTTP 重启验收 |
| PTY 主进程退出后遗留 job-control 子进程 | tty 子进程记录、回收及真实 PTY 测试 |
| 重用 PID 被误认成旧服务 | executable + ps start identity；错误身份明确拒绝停止 |
| Chrome 双 native host 启动删除活 socket | socket 专属 flock 与重复启动测试 |
| native 断线/分片影响后续请求 | 端口身份与重连代次、Unicode 分片测试 |
| cloudflared 主进程退出但留下子进程 | 进程组生命周期测试；停止后检查存活状态 |
| 配置路径包含空格或自定义 token/unlock/socket | 假 launchctl/Keychain 的安装与卸载测试 |
| Secure MCP Tunnel 把 App 路径拆成多个命令参数 | 测试二进制复制到含空格和单引号的路径；真实安装函数生成的命令解析后仍为完整、唯一 executable |
| Tunnel 控制平面省略 stdio 初始化完成通知 | 官方 `tunnel-client` 0.0.14 的本地 `dev proxy` 对接已安装 Go 核心：默认返回 `-32002`，启用通知后发现全部 33 工具；启动参数回归覆盖已有 profile |

## 尚未做的 live 验收

- 在已登录 ChatGPT 账号中创建、续聊、保存任务，核实当前线上 DOM/runtime 是否仍满足契约。原站内部接口会变化；当前没有把模拟通过描述成线上通过。
- 连接真实 Codex app-server 历史。已使用协议 fixture 验证字段、分页、取消和错误处理。
- 真实 Cloudflare 公网连接与 OAuth 回调。OpenAI Secure MCP Tunnel 与 Keychain 已完成真实验收，见文末。
- 真正打开菜单栏 UI、Apple Developer ID 签名、公证及 Intel 机器运行。当前菜单栏已编译、类型检查与本地签名验证。

这份交付是可构建、可运行的完整功能重写；外部账号与发行渠道的验收仍需在目标安装环境完成。

## 首次安装补充

2026-09-09：安装到 `~/Applications/MacBridge Go.app`，已启动真实 HTTP LaunchAgent，并配置 Codex stdio MCP。安装路径的握手、33 个工具和 shell 执行，以及 HTTP Bearer 调用均通过。

首次安装发现并修复 macOS 大小写不敏感造成的包内命名冲突：Swift 程序是 `MacBridge`，Go 核心改为 `macbridge-core`。构建新增两个文件独立性、AppKit 链接和核心版本检查。

## Chrome 与 ChatGPT Chat 实测补充

2026-09-09，已加载 Chrome 扩展并连接 Native Host。通过安装后的认证 HTTP MCP，在真实本地测试页完成后台打开、读取、填写、点击与释放；实际 DOM 内容及事件结果匹配，标签池恢复为 8 个空闲标签。

随后使用官方 `tunnel-client 0.0.14` 和真实 Keychain 凭据安装登录启动服务，`/readyz` 返回 200。在 ChatGPT 网页的 **Chat 聊天模式** 创建自定义 Tunnel 插件，发现全部 33 个工具，完成：

- `bridge_status` 返回实际 Mac 的 Go 实现、系统与架构。
- `fs_write` 在 Documents 写入唯一验收标记，`fs_read` 读回相同的 32 字节；本机独立校验 SHA-256 一致。
- `shell_exec` 执行 `uname -s`，返回 `Darwin`、退出码 0。
- `chrome_workspace_status` 和 `chrome_tabs` 确认连接正常、8 个空闲标签，并找到真实验收聊天标签。

六次调用均与本机审计及官方隧道的转发时间对应。官方客户端会启动 Codex 管理进程，但该次验收未观察到 thread、turn 或推理事件；这不是整个账户用量的测量。个人会话链接、隧道 ID、账号信息、密钥与原始运行日志不随源码分发。

这里验证的是 ChatGPT 调用 MCP 工具的完整路径；不等同于上面的实验性 `chatgpt_conversation_start` 或 Responses 浏览器适配已通过线上验收。
