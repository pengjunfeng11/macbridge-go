---
name: macbridge-go-install
description: Install, reconnect, or troubleshoot MacBridge Go on macOS, including its Chrome extension and ChatGPT Secure MCP Tunnel. Use for setup requests about this specific project, not general Mac development or routine bridge tool use.
---

# MacBridge Go installation

Install the Go implementation from [pengjunfeng11/macbridge-go](https://github.com/pengjunfeng11/macbridge-go). The repository may require the user's GitHub access. Its `README.md` and CLI source are the installation contract; do not substitute the upstream JavaScript project.

Keep the user's requested client and location. If the conversation does not specify a client, default to ChatGPT **Chat** connecting to this Mac. A request for instructions or to install this Skill alone does not authorize installing the bridge. For an actual bridge installation request, perform the requested setup and verification without repeatedly asking for the same authorization.

For a Skill-only request, use the available `skill-installer` with repository path `skills/macbridge-go-install`, or copy only this directory into `${CODEX_HOME:-$HOME/.codex}/skills/macbridge-go-install` after checking for an existing destination. Confirm the installed `SKILL.md` is valid and matches the source, then stop; no bridge inspection, setup or write test is needed.

## Inspect before installing

- Require macOS 13+. Building needs Go 1.23+ and Xcode Command Line Tools; Node is only needed for extension tests. Chrome and the official `tunnel-client` are needed only for their respective integrations.
- Look for `macbridge` on PATH, `~/Applications/MacBridge Go.app` and `/Applications/MacBridge Go.app`. Read `version`, `doctor`, the applicable LaunchAgent, and existing configuration before changing anything. Respect `MAC_DEV_BRIDGE_*` custom paths.
- Reuse a working installation, existing tunnel and plugin. Do not recreate credentials, overwrite a dirty checkout, change model providers, or add another MCP client just to complete installation.
- `init` creates the full-access unlock and token; it is a setup action, not a read-only health check. `install` also initializes the bridge.
- The default data directory is shared with the upstream project: `~/Library/Application Support/MacDeveloperBridge`. Identify an existing upstream instance before migration; do not run both implementations against the same data.

## Build and choose a stable executable

Run this section only for a missing installation or a requested rebuild/upgrade. Skip it for a working installation or read-only diagnosis.

Use an existing checkout of the correct repository, or clone it into the user's chosen durable location (default `~/.local/share/macbridge-go`). Check access with `gh auth status` or the user's existing Git credentials. Do not request that GitHub tokens be pasted into chat.

From the source directory:

```sh
make build
make acceptance
```

`make build` produces `bin/macbridge` and `MacBridge Go.app`, both with Apple Silicon and Intel slices. `make acceptance` uses isolated temporary data. If code was changed, also run `make test check` (requires Node for extension tests).

For the app installation, place the **whole bundle** at `~/Applications/MacBridge Go.app` before registering any service. On an authorized upgrade, preserve the previous app and configuration, stage the replacement, verify its signature, and replace the bundle; do not merge files into a running app or leave extra discoverable app copies around. Preserve existing services unless the requested upgrade needs a restart.

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
