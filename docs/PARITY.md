# 功能对应与兼容边界

规格基线：[upstream fea70d1](https://github.com/alexanderradahl/mac-developer-bridge/tree/fea70d1a3c5524164f2159f6063ba685fef91324)。保留 33 个公开工具，子 MCP 工具在此基础上追加且不重复注册 Codex 工具。

## 全部公开工具

| 工具 | 新实现 | 验证方式 |
|---|---|---|
| `bridge_status`, `audit_tail` | server + 各模块状态聚合 | 原项目 smoke、Go 协议测试 |
| `shell_exec` | Go exec、进程组、上下文超时、有界双输出流 | 原项目 smoke/integration、真实 HTTP |
| `shell_start`, `shell_job_status`, `shell_job_list`, `shell_job_kill` | 原生 detached supervisor、控制 socket、持久状态与日志 | 原项目 integration、Go 生命周期、编译产物重启验收 |
| `fs_read`, `fs_write`, `fs_list`, `fs_stat`, `fs_manage`, `apply_patch` | Go 文件系统、原子替换、符号链接、git apply | 原项目 smoke/integration、Go 边界测试、真实 HTTP 文件往返 |
| `pty_start`, `pty_read`, `pty_write`, `pty_resize`, `pty_signal`, `pty_close` | creack/pty + 原生 ioctl/termios、环形缓冲、游标与会话回收 | 真 PTY 测试：交互、退出、并发、空闲、job control；HTTP 验收 |
| `codex_thread_read`, `codex_thread_list`, `codex_thread_turns_list` | Go Codex app-server 客户端 | 原项目 integration 的假 Codex、Go 精确参数与分页测试 |
| `chrome_workspace_status`, `chrome_workspace_setup`, `chrome_tabs` | Go native client + 持久工作区 | native framing/socket 与 Chrome API 模拟测试 |
| `chrome_open`, `chrome_navigate`, `chrome_snapshot`, `chrome_click`, `chrome_fill`, `chrome_close` | 重写的 MV3 workspace 与页面 DOM 模块 | native 往返、工作区/DOM 模拟测试 |
| `chatgpt_extension_status`, `chatgpt_conversation_start` | OpenAI 页面扩展桥、runtime/raw、Project/续聊/结果核验 | 重写适配器、页面契约和持久化结构测试 |

## 非工具功能

| 原有能力 | 对应实现与范围 |
|---|---|
| stdio JSON-RPC | `internal/server/rpc.go`；初始化、通知、取消、EOF 清理 |
| 现代 MCP | `server/discover`、协议 `_meta`、`resultType`；支持基线中的 `2026-07-28` 契约 |
| 旧版 MCP | `2025-11-25 / 2025-06-18 / 2025-03-26 / 2024-11-05` 协商 |
| HTTP JSON/SSE | `/mcp`、Bearer、legacy headerless 兼容、可选独立 session、DELETE 关闭 |
| OAuth | PKCE S256、授权页、客户端配置、元数据别名、refresh/revoke、重启持久化 |
| Responses 兼容 | 消息与历史、function/custom/namespace、工具结果、模型/effort、Project、JSON 与 SSE |
| 子 MCP federation | 版本协商、分页、命名空间、roots、原样内容、超时、取消、恢复、personal grant |
| Chrome Native Messaging | 长度前缀帧、Unix socket、分片大消息、单 host 锁、profile/account、重连 |
| Chrome 持久工作区 | MDB 分组、容量、自然焦点扩容、lease/claim、重启恢复、到期回收 |
| 额外 native 任务接口 | `taskAudit`, `exactTaskSave`, `prepareExactTaskEditor`, `installExactTaskSaveRewrite`, `readExactTaskSaveRewrite`, `removeExactTaskSaveRewrite`, `installTaskMutationProbe`, `readTaskMutationProbe`, `removeTaskMutationProbe`, `chatgptRuntimeInventory` |
| 菜单栏 | Start/Stop、状态、复制 URL/Client ID/Token/Setup、Strict 切换、轮换 Token、日志、Finder、退出回收 |
| Cloudflare | 固定/快速隧道、URL 捕获、先确定 issuer 后启动 HTTP、失败回收 |
| Secure MCP Tunnel | `tunnel-client` profile 初始化、Keychain、run、LaunchAgent、key 轮换 |
| 本地运维 | init/stop/doctor/install/uninstall、授权文件、审计、custom data/log/socket 路径 |

## 有意变化

1. 运行核心换成 Go；Chrome 扩展仍是平台要求的 JavaScript，菜单栏是原生 Swift。原服务端 JS 与 Perl 不再参与运行。
2. 可执行文件名改为 `macbridge`，Native Host 改为 `com.macbridge.native`，LaunchAgent 使用 `local.macbridge.*`。旧配置需要换 command；公开 MCP 工具名保留。
3. OAuth 状态独立存放在 `oauth-go.json`，旧授权不迁移。原版本任务元数据不是新 supervisor 的恢复格式；迁移前结束旧任务。
4. 新增文件 hash 前置条件与有界后台日志；真实退出码被持久化。PTY 保留原本不跨进程重启恢复的边界。
5. 子 MCP 初次发现独立进行；本机工具不等待慢子服务。首次目录发现仍等待其有界启动尝试，以免返回暂时缺失的目录。
6. 与原项目相同，raw ChatGPT 路径不接受已有 `conversation_id`；续聊使用 runtime。runtime 才提供重新载入与持久内容核验。
7. 任务审计不再引用上游作者的私人 hardcoded task ID；从传入 ID 或当前页面确定目标。

“已实现”不等于外部服务当前页面的真实账号验收。浏览器、Codex、Cloudflare、Secure MCP Tunnel 的隔离测试与 live 验收边界见 [VALIDATION.md](VALIDATION.md)。
