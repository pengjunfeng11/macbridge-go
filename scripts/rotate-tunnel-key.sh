#!/bin/bash
set -euo pipefail
key="${CONTROL_PLANE_API_KEY:-}"
if [[ -z "$key" ]]; then read -r -s -p 'New tunnel runtime key: ' key; printf '\n'; fi
[[ -n "$key" ]] || { printf 'A runtime key is required.\n' >&2; exit 64; }
/usr/bin/security add-generic-password -U -s "${MAC_DEV_BRIDGE_KEYCHAIN_SERVICE:-OpenAI Secure MCP Tunnel Runtime}" -a "${MAC_DEV_BRIDGE_KEYCHAIN_ACCOUNT:-$(id -un)}" -w "$key"
unset key CONTROL_PLANE_API_KEY
launchctl kickstart -k "gui/$(id -u)/local.macbridge.tunnel"
