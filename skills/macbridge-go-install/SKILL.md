---
name: macbridge-go-install
description: Download, install, connect, use, or troubleshoot MacBridge Go on macOS, including local files, shell jobs, interactive terminals, Chrome, and ChatGPT Secure MCP Tunnel. Use when the user requests this specific bridge or its setup and usage, not general Mac development.
---

# MacBridge Go: download, installation and use

Use the Go implementation from the public [pengjunfeng11/macbridge-go](https://github.com/pengjunfeng11/macbridge-go) repository. Source and Skill downloads require no GitHub login or repository approval. Each user still needs their own tunnel and runtime key for ChatGPT Secure MCP Tunnel integration. Its `README.md`, CLI source and discovered MCP tool schemas are the contract; do not substitute the upstream JavaScript project.

Keep the user's requested client and location. If the conversation does not specify a client, default to ChatGPT **Chat** connecting to this Mac. A request for instructions or to install this Skill alone does not authorize installing the bridge. For an actual bridge installation request, perform the requested setup and verification without repeatedly asking for the same authorization.

For a Skill-only request, use the available `skill-installer` with repository path `skills/macbridge-go-install`, or copy only this directory into `${CODEX_HOME:-$HOME/.codex}/skills/macbridge-go-install` after checking for an existing destination. Confirm the installed `SKILL.md` is valid and matches the source, then stop; no bridge inspection, setup or write test is needed.

For a usage request with connected tools, go directly to **Use the connected bridge**. Do not rebuild, reinstall, create a tunnel or run installation acceptance before each task. If tools are unavailable, inspect the existing connection and repair only the missing part within the user's request.

## Inspect before installing

- Require macOS 13+. Building needs Go 1.23+ and Xcode Command Line Tools; Node is only needed for extension tests. Chrome and the official `tunnel-client` are needed only for their respective integrations.
- Look for `macbridge` on PATH, `~/Applications/MacBridge Go.app` and `/Applications/MacBridge Go.app`. Read `version`, `doctor`, the applicable LaunchAgent, and existing configuration before changing anything. Respect `MAC_DEV_BRIDGE_*` custom paths.
- Reuse a working installation, existing tunnel and plugin. Do not recreate credentials, overwrite a dirty checkout, change model providers, or add another MCP client just to complete installation.
- `init` creates the full-access unlock and token; it is a setup action, not a read-only health check. `install` also initializes the bridge.
- The default data directory is shared with the upstream project: `~/Library/Application Support/MacDeveloperBridge`. Identify an existing upstream instance before migration; do not run both implementations against the same data.

## Build and choose a stable executable

Run this section only for a missing installation or a requested rebuild/upgrade. Skip it for a working installation or read-only diagnosis.

Use an existing checkout of the correct repository, or clone it over HTTPS into the user's chosen durable location (default `~/.local/share/macbridge-go`). Public downloads do not require `gh auth login` or an API key. Do not request that GitHub tokens be pasted into chat.

For a fresh checkout, after confirming the destination is absent:

```sh
mkdir -p "$HOME/.local/share"
git clone https://github.com/pengjunfeng11/macbridge-go.git "$HOME/.local/share/macbridge-go"
```

Use the user's chosen location instead when supplied. If downloading fails, check the actual URL, network/proxy response and Git configuration; stop dependent installation until the source is available, and do not substitute another repository or assume a personal token is required. Enter the successfully cloned directory before building. Existing checkouts must retain uncommitted changes; fetch/update only when the requested install or upgrade requires it.

From the successfully downloaded source directory (substitute the actual path for an existing or custom checkout):

```sh
cd "$HOME/.local/share/macbridge-go"
make build
make acceptance
```

`make build` produces `bin/macbridge` and `MacBridge Go.app`, both with Apple Silicon and Intel slices. `make acceptance` uses isolated temporary data. If code was changed, also run `make test check` (requires Node for extension tests).

For the app installation, place the **whole bundle** at `~/Applications/MacBridge Go.app` before registering any service. On an authorized upgrade, preserve the previous app and configuration, stage the replacement, verify its signature, and replace the bundle; do not merge files into a running app or leave extra discoverable app copies around. Preserve existing services unless the requested upgrade needs a restart.

For the first installation, from the built source directory:

```sh
test ! -e "$HOME/Applications/MacBridge Go.app" &&
  mkdir -p "$HOME/Applications" &&
  ditto "MacBridge Go.app" "$HOME/Applications/MacBridge Go.app"
```

If the guard fails, reuse the existing installation or follow the requested upgrade procedure; do not remove the guard to overwrite it.

The executable names are distinct even on case-insensitive disks:

- `Contents/MacOS/MacBridge`: Swift menu app, not the MCP server.
- `Contents/MacOS/macbridge-core`: Go CLI and MCP server.

Use the final absolute Go executable path for all registrations. For example:

```sh
MACBRIDGE_BIN="$HOME/Applications/MacBridge Go.app/Contents/MacOS/macbridge-core"
"$MACBRIDGE_BIN" version
"$MACBRIDGE_BIN" init
```

A source-only installation can keep `bin/macbridge` at a durable location instead. Service registrations embed the executable path, so do not register a temporary build and then move it. An optional `~/.local/bin/macbridge` symlink must not replace an unrelated command.

## Chrome integration

If browser tools are requested, run `install-browser` using the final Go executable. The command registers Native Messaging but does **not** copy or load the extension. Have the user load the directory it prints in Chrome's Extensions page with developer mode enabled, then click the extension icon once. Browser-native dialogs or pages unavailable to the available tools need a user handoff.

Keep the manifest's public key; it stabilizes the extension ID and is not a credential. The native host is `com.macbridge.native`. Check `chrome_workspace_status` after loading; missing pool capacity may wait until Chrome is naturally focused, so do not repeatedly recreate tabs or steal focus.

## Connect the requested client

### ChatGPT Chat through Secure MCP Tunnel

Use the current [official Tunnel guide](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels) and [ChatGPT plugin setup guide](https://developers.openai.com/plugins/build/app-quickstart#connect-your-mcp-server-in-chatgpt) for account-specific UI and permissions.

1. Install the official client if missing: `brew install openai/tools/tunnel-client`.
2. Reuse or create a Platform tunnel associated with the intended ChatGPT workspace. Account onboarding, personal attestations and login challenges are completed by the user. Never guess a tunnel/workspace ID.
3. Use a runtime API key with **Tunnels Read/Use** permissions. A project-key prefix alone does not establish whether it works. Validate the actual key and tunnel with `tunnel-client admin --json tunnels get` and the known tunnel ID. Prefer an existing Keychain entry or local hidden input; never echo secrets, commit them, or include them in a chat prompt.
4. Set `CONTROL_PLANE_TUNNEL_ID` and the absolute `TUNNEL_CLIENT_BIN`; provide `CONTROL_PLANE_API_KEY` only in the install process environment if it is not already in Keychain. Run `"$MACBRIDGE_BIN" install-tunnel`. It stores the runtime key in Keychain and installs `local.macbridge.tunnel`. Remove the transient key from the process environment afterward.
5. Confirm that LaunchAgent is running and the official client's `/readyz` returns HTTP 200 at the actual address reported in its startup log (normally `127.0.0.1:8080`). The log is normally `~/Library/Logs/MacDeveloperBridge/secure-tunnel.log`; read narrowly and redact any credentials.
6. In ChatGPT, enable developer mode and create or reuse the **MacBridge Go** plugin. Select **Tunnel**, the actual tunnel, and **No authentication** for this stdio bridge. The official tunnel connection supplies the transport authentication.
7. In a **Chat** conversation, select MacBridge Go from the composer `+` menu and perform the acceptance below. Keep the daemon running for discovery and calls.

The bridge's `install-tunnel` already quotes executable paths containing spaces and starts the official client with `--mcp.stdio-send-initialized-notification=true`. Do not remove this flag or weaken the MCP initialization handshake to work around `-32002`.

Secure Tunnel uses **stdio** and does not require the local HTTP server, menu **Start**, or Cloudflare. Do not start `macbridge tunnel` in parallel with its running LaunchAgent. Work and Codex share usage; ordinary Chat has its own model/message limits. This integration is not a token compressor or an unlimited-usage guarantee.

### Local MCP client or local HTTP

For a requested local client, register the final Go executable as `command`, with `args: ["stdio"]`, preserving its other MCP entries. Reload that client's tools and verify an actual call. Supply `CODEX_BIN` only when local Codex-history tools are needed. Do not configure an experimental Responses endpoint as the model provider.

Only when local HTTP is requested, run `"$MACBRIDGE_BIN" install` to start `local.macbridge.go`; its default MCP URL is `http://127.0.0.1:8787/mcp`. Use the generated Bearer or OAuth configuration. This loopback URL alone is not reachable from cloud ChatGPT. Do not run menu desktop HTTP and the HTTP LaunchAgent on the same port.

## Use the connected bridge

In ChatGPT, open a **Chat** conversation, select **MacBridge Go** from `+`, and describe the task. In a local MCP client, use the registered bridge tools directly. The target Mac must be awake, online and logged in; browser work additionally needs Chrome and the loaded extension. Discover tools in that client rather than assuming an MCP namespace prefix. Use `bridge_status` to resolve an uncertain target Mac or connection, then perform the requested work.

There are 33 built-in tools; configured child MCPs can add more:

| Task | Built-in tools |
|---|---|
| Host and audit | `bridge_status`, `audit_tail` |
| Files and patches | `fs_list`, `fs_stat`, `fs_read`, `fs_write`, `fs_manage`, `apply_patch` |
| Shell and persistent jobs | `shell_exec`, `shell_start`, `shell_job_list`, `shell_job_status`, `shell_job_kill` |
| Interactive terminal | `pty_start`, `pty_read`, `pty_write`, `pty_resize`, `pty_signal`, `pty_close` |
| Chrome workspace and pages | `chrome_workspace_status`, `chrome_workspace_setup`, `chrome_tabs`, `chrome_open`, `chrome_navigate`, `chrome_snapshot`, `chrome_click`, `chrome_fill`, `chrome_close` |
| Stored Codex history | `codex_thread_list`, `codex_thread_read`, `codex_thread_turns_list` |
| ChatGPT browser integration | `chatgpt_extension_status`, `chatgpt_conversation_start` |

Read the discovered schema for the tools needed for this task. Input fields and result fields intentionally differ in places; use actual returned identifiers and selectors, never invented ones.

- **Files:** use `fs_list`/`fs_stat` to identify the path, then `fs_read`. File paths are on the target Mac; relative paths resolve from its home, not the assistant's current project. For large files pass `max_bytes` and continue with returned `nextOffset` as the next `offset`. When replacing an existing file, read with `include_sha256: true` and pass its returned `sha256` as `fs_write.expected_sha256` to detect intervening changes. Use `apply_patch` for a unified diff with the actual repository `cwd`; read back the result and run relevant checks for requested code changes.
- **Commands and jobs:** use `shell_exec` with explicit `cwd` for bounded commands, checking `exitCode`, `timedOut`, `stdout` and `stderr`. A nonzero exit code is not successful work even if the tool call returned normally. For a long or detached command, `shell_start` returns **`id`**; pass it as **`job_id`** to `shell_job_status`/`shell_job_kill`. Use `shell_job_list` to recover an existing job instead of launching it again. Check `running`, `status`, `exitCode` and log tails until the requested outcome is established. Job metadata/logs survive a bridge restart.
- **Interactive programs:** `pty_start.command` plus `args` is an argv vector, not a shell expression; use `/bin/zsh` with `args: ["-i"]` for an interactive shell. It returns **`sessionId`**; use **`session_id`** in subsequent calls. Continue `pty_read` with returned **`nextCursor`** as input **`cursor`**, and inspect `lostBytes`/`truncated`. Submit a line with `pty_write.data` ending in `\r`; Ctrl-C is `\u0003`. Read final output after exit, then `pty_close` when the task is done. PTY sessions do not survive a bridge restart.
- **Browser:** check `chrome_workspace_status`, and use `chrome_workspace_setup` only when capacity is missing. Select a relevant page via `chrome_tabs`, or open the requested URL with `chrome_open`. Results contain **`tabId`**; pass it as **`tab_id`**. Read `chrome_snapshot`, then use an actual **`elements[].selector`** with `chrome_click`/`chrome_fill`. Refresh the snapshot after navigation or a material page change and verify the resulting page state. Release task-owned MDB tabs with `chrome_close`; do not close unrelated user tabs. Login challenges, native dialogs, file pickers and genuine user-gesture requirements may need a user handoff.
- **History and optional ChatGPT automation:** discover exact stored thread IDs with `codex_thread_list`, then read or page their turns. These tools require the local Codex executable and do not start model turns. Ordinary MacBridge use does not require `chatgpt_conversation_start`; it is an optional, separate browser ChatGPT conversation action and needs an explicit request to start or continue that conversation. Use the requested model/mode and actual conversation ID, verify its returned content, and do not automatically switch to Work or treat the experimental Responses adapter as a model provider.

Example requests users can send after selecting the plugin:

- “用 MacBridge Go 列出我 Documents 中最近修改的文件，再读取我指定的那份。”
- “用 MacBridge Go 在我给出的项目目录运行测试，告诉我失败原因。”
- “用 MacBridge Go 在后台运行这个任务，把任务 ID 给我，之后查它的日志。”
- “用 MacBridge Go 打开这个网页，读取页面并按我的要求填写表单。”
- “用 MacBridge Go 找到我指定项目以前的 Codex 任务，读出上次的结论。”

Report the requested result with its relevant file path, job ID or page evidence. For ordinary usage, verify that result rather than repeating the installation checklist below. If the client exposes no bridge tools, explain that the Skill is an operating guide and cannot itself provide the MCP connection.

## Acceptance and stopping conditions

`doctor.httpReachable` only means its HTTP request completed; it does not prove a 200 response, MCP readiness or Tunnel readiness. It can legitimately be false for stdio-only installations. The menu's Running/Stopped label also reflects HTTP, not the Secure Tunnel.

For an actual bridge installation or upgrade, verify through the **requested client**, not just a local substitute. For read-only diagnosis, reuse existing successful calls and inspect status/logs; do not run the write test unless requested.

- Discover the 33 built-in tools (configured child MCPs can add more) and call `bridge_status`.
- Write a unique, small test file with `fs_write` in a temporary directory on the target Mac (or a location the user selected), read it with `fs_read`, and independently check the local content. Avoid existing filenames; remove only the file created for this test when cleanup is appropriate.
- Run `shell_exec` with `uname -s`; expect `Darwin` and exit code 0.
- If Chrome was included, call `chrome_workspace_status` and read a known test tab with `chrome_tabs` or `chrome_snapshot`.

For ChatGPT, correlate its tool results with the corresponding narrow time range in `audit.jsonl` and `secure-tunnel.log`. The audit file has no transport/request ID, so audit entries alone cannot prove the request came from ChatGPT. The official tunnel may start a Codex management process; a process existing is not evidence of a model turn or billing.

If the remaining step requires the user's login, native extension loading or unavailable workspace permissions, finish independent setup and identify the exact remaining action. Do not call a saved configuration or healthy daemon a completed ChatGPT installation.

`macbridge stop` removes the shared unlock and stops this bridge's HTTP, tunnel, native host and jobs; it is not a tunnel-only restart. To restart an existing tunnel, use its LaunchAgent's targeted restart. `macbridge uninstall` stops the bridge and removes its service registrations while retaining data. Do not stop or uninstall a working installation for a read-only check.
