import {
  Workspace,
  approve,
  permitted,
  tabInfo,
  fault,
  sleep,
} from "./workspace.js";
import { pageOperation } from "./page.js";
import { taskPage } from "./tasks.js";
import { chatgptPage, runtimeInventory } from "./chatgpt.js";

export class BrowserService {
  constructor(api) {
    this.api = api;
    this.workspace = new Workspace(api);
    this.busyTabs = new Set();
  }
  async execute(tabId, func, input, world = "ISOLATED") {
    const results = await this.api.scripting.executeScript({
      target: { tabId },
      world,
      func,
      args: [input],
    });
    if (!results.length || results[0].error)
      throw fault(
        "CHROME_PAGE_ERROR",
        results[0]?.error?.message || "No page result",
      );
    return results[0].result;
  }
  async approvedTab(id, patterns) {
    if (!Number.isInteger(id) || id < 0)
      throw fault("INVALID_ARGUMENT", "tabId must be a nonnegative integer");
    const tab = await this.api.tabs.get(id);
    approve(tab.url, patterns);
    await this.workspace.touch(id);
    return tab;
  }
  async extensionStatus() {
    const extension = await this.api.management
      .get("hehggadaopoacecdllhhajmbjkdcmajg")
      .catch(() => null);
    const tabs = await this.api.tabs.query({ url: "https://chatgpt.com/*" });
    const base = {
      installed: !!extension,
      extensionId: "hehggadaopoacecdllhhajmbjkdcmajg",
      enabled: extension?.enabled ?? false,
      version: extension?.version ?? null,
      openChatgptTabs: tabs.length,
    };
    for (const tab of tabs) {
      const page = await this.execute(
        tab.id,
        chatgptPage,
        { operation: "extensionStatus" },
        "MAIN",
      ).catch(() => null);
      if (page?.available || page?.pageBridgeAvailable)
        return {
          ...base,
          ...page,
          pageBridge: page,
          tabId: tab.id,
          windowId: tab.windowId,
          tabActive: !!tab.active,
          tabGroupId: tab.groupId >= 0 ? tab.groupId : null,
          url: tab.url || "",
        };
    }
    return {
      ...base,
      available: false,
      pageBridgeAvailable: false,
      candidateTabs: tabs.length,
      reason: tabs.length ? "page-bridge-unavailable" : "no-chatgpt-tab",
    };
  }
  async conversation(args, patterns) {
    const transport = args.transport || "runtime",
      model = args.model || "gpt-5-6-pro",
      effort = args.thinkingEffort || "standard";
    if (!["runtime", "raw"].includes(transport))
      throw fault(
        "CHATGPT_TRANSPORT_INVALID",
        "Unknown conversation transport",
      );
    if (args.projectId && !/^g-p-[A-Za-z0-9_-]{8,128}$/.test(args.projectId))
      throw fault("CHATGPT_PROJECT_ID_INVALID", "Invalid project id");
    if (
      args.conversationId &&
      !/^[A-Za-z0-9_-]{8,128}$/.test(args.conversationId)
    )
      throw fault("CHATGPT_CONVERSATION_ID_INVALID", "Invalid conversation id");
    if (transport === "raw" && args.conversationId)
      throw fault(
        "CHATGPT_CONTINUATION_TRANSPORT_INVALID",
        "Raw transport cannot continue a conversation",
      );
    let tab,
      leased = false;
    if (args.tabId !== undefined)
      tab = await this.approvedTab(args.tabId, patterns);
    else if (transport === "runtime") {
      const route = args.conversationId
        ? `/c/${encodeURIComponent(args.conversationId)}`
        : args.projectId
          ? `/g/${encodeURIComponent(args.projectId)}/project`
          : "/";
      const query = new URLSearchParams({ model, thinking_effort: effort });
      const allocated = await this.workspace.open(
        `https://chatgpt.com${route}?${query}`,
        patterns,
      );
      tab = await this.api.tabs.get(allocated.tabId);
      leased = true;
    } else {
      tab = (await this.api.tabs.query({ url: "https://chatgpt.com/*" })).find(
        (t) => permitted(t.url, patterns),
      );
      if (!tab)
        throw fault(
          "CHATGPT_TAB_UNAVAILABLE",
          "Open and sign in to ChatGPT before using raw diagnostics",
        );
    }
    if (new URL(tab.url).origin !== "https://chatgpt.com")
      throw fault("CHATGPT_TAB_UNAVAILABLE", "Selected tab is not ChatGPT");
    if (this.busyTabs.has(tab.id))
      throw fault(
        "CHATGPT_RUNTIME_BUSY",
        "This tab is already running a conversation",
      );
    this.busyTabs.add(tab.id);
    const heartbeat = setInterval(
      () => this.workspace.touch(tab.id).catch(() => {}),
      60000,
    );
    try {
      const deadline = Date.now() + 20000;
      let result;
      do {
        await this.approvedTab(tab.id, patterns);
        result = await this.execute(
          tab.id,
          chatgptPage,
          { ...args, transport, model, thinkingEffort: effort },
          "MAIN",
        );
        // Retry only failures that occur before dispatch; never submit a turn twice.
        if (
          ![
            "CHATGPT_RUNTIME_MODEL_MISMATCH",
            "CHATGPT_RUNTIME_THINKING_EFFORT_MISMATCH",
            "CHATGPT_RUNTIME_NOT_READY",
          ].includes(result?.error?.code) ||
          Date.now() >= deadline
        )
          break;
        await sleep(250);
      } while (true);
      if (!result?.ok)
        throw fault(
          result?.error?.code || "CHATGPT_CONVERSATION_FAILED",
          result?.error?.message || "ChatGPT turn failed",
        );
      if (result.complete && transport === "runtime") {
        if (!result.conversation_id || !result.assistant_message_id)
          throw fault(
            "CHATGPT_CONVERSATION_HANDOFF_UNCERTAIN",
            "Generation did not identify an exact conversation and assistant message",
          );
        if (
          args.conversationId &&
          result.conversation_id !== args.conversationId
        )
          throw fault(
            "CHATGPT_RUNTIME_CONVERSATION_MISMATCH",
            "Runtime generated into an unexpected conversation",
          );
        await this.api.tabs.reload(tab.id);
        await sleep(250);
        await this.workspace.settle(tab.id, patterns);
        const persisted = await this.execute(
          tab.id,
          chatgptPage,
          {
            operation: "persisted",
            conversationId: result.conversation_id,
            assistantMessageId: result.assistant_message_id,
            timeoutMs: 15000,
          },
          "MAIN",
        );
        if (!persisted?.ok)
          throw fault(
            persisted?.error?.code || "CHATGPT_CONVERSATION_HANDOFF_UNCERTAIN",
            persisted?.error?.message || "Could not verify persisted reply",
          );
        result = { ...result, ...persisted, persisted_verified: true };
      }
      const settled = await this.api.tabs.get(tab.id).catch(() => tab);
      return {
        ...result,
        transport,
        tabId: tab.id,
        autoLeased: leased,
        tab_id: tab.id,
        tab_url: result.page_url || settled.url || "",
        tab_active: !!settled.active,
      };
    } finally {
      clearInterval(heartbeat);
      this.busyTabs.delete(tab.id);
      if (leased) await this.workspace.release(tab.id).catch(() => {});
    }
  }
  async dispatch(message) {
    const args = message.args || {},
      patterns = message.allowedUrlPatterns || [];
    switch (message.method) {
      case "status":
        return {
          connected: true,
          version: "1.0.0",
          extensionId: this.api.runtime.id,
        };
      case "extension.reload":
        setTimeout(() => this.api.runtime.reload(), 100);
        return { reloading: true };
      case "workspace.status":
        return this.workspace.status();
      case "workspace.init":
        return this.workspace.setup(args.poolSize ?? 8);
      case "workspace.release":
        if (this.busyTabs.has(args.tabId))
          throw fault(
            "CHROME_TAB_BUSY",
            "Tab is running an active conversation",
          );
        return this.workspace.release(args.tabId);
      case "chatgpt.extensionStatus":
        return this.extensionStatus();
      case "workspace.open":
      case "tabs.open":
        return this.workspace.open(args.url, patterns);
      case "tabs.list": {
        const tabs = (await this.api.tabs.query({}))
          .filter(
            (t) =>
              permitted(t.url, patterns) &&
              String(t.url)
                .toLowerCase()
                .includes(String(args.urlContains || "").toLowerCase()) &&
              String(t.title || "")
                .toLowerCase()
                .includes(String(args.titleContains || "").toLowerCase()),
          )
          .slice(0, args.maxTabs ?? 200)
          .map(tabInfo);
        return { tabs, count: tabs.length };
      }
      case "tabs.chatgptConversationStart":
        return this.conversation(args, patterns);
      case "tabs.chatgptRuntimeInventory": {
        const tab = await this.approvedTab(args.tabId, patterns);
        const result = await this.execute(tab.id, runtimeInventory, {}, "MAIN");
        if (!result?.ok)
          throw fault(
            result?.error?.code || "CHATGPT_RUNTIME_INVENTORY_FAILED",
            result?.error?.message || "Runtime inspection failed",
          );
        return { ...result, tab_id: tab.id, tab_active: !!tab.active };
      }
      case "tabs.taskAudit":
      case "tabs.exactTaskSave":
      case "tabs.prepareExactTaskEditor":
      case "tabs.installExactTaskSaveRewrite":
      case "tabs.readExactTaskSaveRewrite":
      case "tabs.removeExactTaskSaveRewrite":
      case "tabs.installTaskMutationProbe":
      case "tabs.readTaskMutationProbe":
      case "tabs.removeTaskMutationProbe": {
        const tab = await this.approvedTab(args.tabId, patterns);
        if (this.busyTabs.has(tab.id))
          throw fault(
            "CHROME_TAB_BUSY",
            "Tab is running an active conversation",
          );
        return this.execute(
          tab.id,
          taskPage,
          { ...args, operation: message.method.slice(5) },
          "MAIN",
        );
      }
      case "tabs.navigate": {
        if (this.busyTabs.has(args.tabId))
          throw fault(
            "CHROME_TAB_BUSY",
            "Tab is running an active conversation",
          );
        const tab = await this.approvedTab(args.tabId, patterns);
        approve(args.url, patterns);
        await this.api.tabs.update(tab.id, { url: args.url });
        return tabInfo(await this.workspace.settle(tab.id, patterns));
      }
      case "tabs.close": {
        if (this.busyTabs.has(args.tabId))
          throw fault(
            "CHROME_TAB_BUSY",
            "Tab is running an active conversation",
          );
        const release = await this.workspace.release(args.tabId);
        if (release.released) return release;
        const tab = await this.approvedTab(args.tabId, patterns);
        if (tab.active && !args.allowActive)
          throw fault(
            "CHROME_ACTIVE_TAB_REFUSED",
            "Refusing to close the active tab without allowActive=true",
          );
        await this.api.tabs.remove(tab.id);
        return {
          closed: true,
          released: false,
          workspace: false,
          tabId: tab.id,
          wasActive: !!tab.active,
        };
      }
      case "tabs.snapshot":
      case "tabs.click":
      case "tabs.fill": {
        const tab = await this.approvedTab(args.tabId, patterns);
        if (this.busyTabs.has(tab.id) && message.method !== "tabs.snapshot")
          throw fault(
            "CHROME_TAB_BUSY",
            "Tab is running an active conversation",
          );
        const results = await this.api.scripting.executeScript({
          target: { tabId: tab.id },
          func: pageOperation,
          args: [message.method.slice(5), args],
        });
        if (!results.length || results[0].error)
          throw fault(
            "CHROME_PAGE_ERROR",
            results[0]?.error?.message || "No page result",
          );
        const current = await this.api.tabs.get(tab.id);
        approve(current.url, patterns);
        return { ...results[0].result, tabId: tab.id };
      }
      default:
        throw fault(
          "CHROME_UNKNOWN_METHOD",
          `Unknown browser method ${message.method}`,
        );
    }
  }
}

export function startNative(api) {
  const service = new BrowserService(api);
  let port,
    connecting = false,
    retryTimer,
    retry = 1000;
  const fragments = new Map();
  const scheduleReconnect = () => {
    if (retryTimer) return;
    retryTimer = setTimeout(() => {
      retryTimer = undefined;
      connect();
    }, retry);
    retry = Math.min(10000, retry * 2);
  };
  const connect = async () => {
    if (port || connecting) return;
    connecting = true;
    if (retryTimer) clearTimeout(retryTimer);
    retryTimer = undefined;
    let connectedPort;
    try {
      const stored = await api.storage.local.get("macbridgeProfileId");
      const profileId = stored.macbridgeProfileId || crypto.randomUUID();
      if (!stored.macbridgeProfileId)
        await api.storage.local.set({ macbridgeProfileId: profileId });
      connectedPort = api.runtime.connectNative("com.macbridge.native");
      port = connectedPort;
      const receive = (message) => {
        if (port !== connectedPort) return;
        if (message.type === "ready") {
          retry = 1000;
          return;
        }
        if (message.type === "chunk") {
          if (
            !Number.isInteger(message.total) ||
            message.total < 1 ||
            message.total > 24 ||
            !Number.isInteger(message.index) ||
            message.index < 0 ||
            message.index >= message.total
          )
            return;
          let entry = fragments.get(message.id);
          if (!entry) {
            entry = {
              parts: new Array(message.total),
              size: 0,
              expires: Date.now() + 30000,
            };
            fragments.set(message.id, entry);
          }
          if (
            entry.parts.length !== message.total ||
            entry.parts[message.index]
          ) {
            fragments.delete(message.id);
            return;
          }
          const binary = atob(message.data);
          entry.parts[message.index] = Uint8Array.from(binary, (c) =>
            c.charCodeAt(0),
          );
          entry.size += binary.length;
          if (entry.size > 10485760) {
            fragments.delete(message.id);
            return;
          }
          if (entry.parts.filter(Boolean).length === message.total) {
            const combined = new Uint8Array(entry.size);
            let offset = 0;
            for (const part of entry.parts) {
              combined.set(part, offset);
              offset += part.length;
            }
            fragments.delete(message.id);
            try {
              receive(JSON.parse(new TextDecoder().decode(combined)));
            } catch {}
          }
          return;
        }
        if (message.type !== "request") return;
        const requestPort = connectedPort;
        Promise.resolve()
          .then(() => service.dispatch(message))
          .then(
            (result) =>
              requestPort.postMessage({
                type: "response",
                id: message.id,
                ok: true,
                result,
              }),
            (e) =>
              requestPort.postMessage({
                type: "response",
                id: message.id,
                ok: false,
                error: {
                  code: e.code || "CHROME_EXTENSION_ERROR",
                  message: String(e.message || e),
                },
              }),
          )
          .catch(() => {});
      };
      connectedPort.onMessage.addListener(receive);
      connectedPort.onDisconnect.addListener(() => {
        void api.runtime.lastError;
        if (port !== connectedPort) return;
        port = null;
        fragments.clear();
        scheduleReconnect();
      });
      let profile = { signedIn: false, email: null, id: null };
      try {
        const info = await api.identity.getProfileUserInfo({
          accountStatus: "ANY",
        });
        profile = {
          signedIn: !!(info.email && info.id),
          email: info.email || null,
          id: info.id || null,
        };
      } catch {}
      if (port !== connectedPort) return;
      connectedPort.postMessage({
        type: "hello",
        profileId,
        extensionId: api.runtime.id,
        version: "1.0.0",
        profile,
      });
    } catch {
      if (port === connectedPort) port = null;
      try {
        connectedPort?.disconnect();
      } catch {}
      scheduleReconnect();
    } finally {
      connecting = false;
      if (!port) scheduleReconnect();
    }
  };
  api.windows.onFocusChanged.addListener((id) => {
    if (id >= 0) service.workspace.setup(8).catch(() => {});
  });
  api.action.onClicked.addListener(() =>
    service.workspace.setup(8).catch(() => {}),
  );
  api.alarms.create("macbridge-maintenance", { periodInMinutes: 1 });
  api.alarms.onAlarm.addListener((alarm) => {
    if (alarm.name !== "macbridge-maintenance") return;
    for (const [id, entry] of fragments)
      if (entry.expires < Date.now()) fragments.delete(id);
    service.workspace.status().catch(() => {});
    if (!port) connect();
  });
  api.runtime.onStartup.addListener(() =>
    service.workspace.setup(8).catch(() => {}),
  );
  api.identity?.onSignInChanged?.addListener(() => {
    if (port) port.disconnect();
  });
  connect();
  return service;
}
if (typeof chrome !== "undefined" && chrome.runtime?.connectNative)
  startNative(chrome);
