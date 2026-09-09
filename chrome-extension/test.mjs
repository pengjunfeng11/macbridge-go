import assert from "node:assert/strict";
import { webcrypto } from "node:crypto";
import { Workspace, permitted } from "./workspace.js";
import { BrowserService, startNative } from "./service-worker.js";
import { chatgptPage, runtimeInventory } from "./chatgpt.js";
import { pageOperation } from "./page.js";

const event = () => ({
  listeners: [],
  addListener(fn) {
    this.listeners.push(fn);
  },
  emit(value) {
    for (const fn of this.listeners) fn(value);
  },
});
function fakeChrome() {
  const state = {
    focused: false,
    next: 1,
    group: null,
    tabs: new Map(),
    storage: {},
    created: 0,
    reloads: 0,
    updates: [],
  };
  const api = {
    runtime: {
      id: "a".repeat(32),
      getURL: (file) => `chrome-extension://${"a".repeat(32)}/${file}`,
      onStartup: event(),
    },
    action: { onClicked: event() },
    alarms: { create() {}, onAlarm: event() },
    management: {
      async get() {
        throw Error("not installed");
      },
    },
    storage: {
      local: {
        async get(key) {
          return { [key]: structuredClone(state.storage[key]) };
        },
        async set(values) {
          Object.assign(state.storage, structuredClone(values));
        },
      },
    },
    windows: {
      async getLastFocused() {
        return { id: 1, focused: state.focused };
      },
      async get() {
        return { id: 1, focused: state.focused };
      },
      onFocusChanged: event(),
    },
    tabGroups: {
      async get(id) {
        if (!state.group || state.group.id !== id) throw Error("missing group");
        return { ...state.group };
      },
      async query() {
        return state.group ? [{ ...state.group }] : [];
      },
      async update(id, patch) {
        Object.assign(state.group, patch);
      },
    },
    tabs: {
      async get(id) {
        const tab = state.tabs.get(id);
        if (!tab) throw Error("missing tab");
        return { ...tab };
      },
      async query(query = {}) {
        return [...state.tabs.values()]
          .filter(
            (t) =>
              (query.groupId === undefined || t.groupId === query.groupId) &&
              (!query.url || t.url.startsWith("https://chatgpt.com/")),
          )
          .map((t) => ({ ...t }));
      },
      async create(options) {
        assert.equal(
          state.focused,
          true,
          "must only create in already-focused Chrome",
        );
        assert.equal(options.active, false);
        state.created++;
        const tab = {
          id: state.next++,
          windowId: 1,
          active: false,
          status: "complete",
          groupId: -1,
          ...options,
        };
        state.tabs.set(tab.id, tab);
        return { ...tab };
      },
      async group(options) {
        state.group ||= { id: 7, windowId: 1, title: "MDB" };
        for (const id of options.tabIds) state.tabs.get(id).groupId = 7;
        return 7;
      },
      async update(id, patch) {
        assert.notEqual(patch.active, true, "never activate a tab");
        state.updates.push({ id, patch });
        const tab = state.tabs.get(id);
        Object.assign(tab, patch);
        return { ...tab };
      },
      async remove(id) {
        state.tabs.delete(id);
      },
      async reload() {
        state.reloads++;
      },
    },
    scripting: {
      async executeScript({ func, args }) {
        return [{ result: await func(...args) }];
      },
    },
  };
  return { api, state };
}

assert.equal(
  permitted("https://example.com/path", ["https://example.com/*"]),
  true,
);
assert.equal(
  permitted("https://example.com.evil/path", ["https://example.com/*"]),
  false,
);
assert.equal(permitted("http://localhost:9876/a", ["http://*:*/*"]), true);
assert.equal(
  permitted("https://example.com:444/a", ["https://example.com/*"]),
  false,
);
assert.equal(permitted("chrome://settings/", ["*://*/*"]), false);
const { api, state } = fakeChrome();
const pool = new Workspace(api);
assert.equal((await pool.setup(12)).targetPoolSize, 12);
assert.equal(state.created, 0);
state.focused = true;
assert.equal((await pool.setup(8)).poolSize, 12);
state.focused = false;
const leases = await Promise.all([
  pool.open("https://example.com/a", ["https://example.com/*"]),
  pool.open("https://example.com/b", ["https://example.com/*"]),
]);
assert.notEqual(leases[0].tabId, leases[1].tabId);
assert.equal(state.created, 12, "routine open reuses tabs");
const restarted = new Workspace(api);
assert.equal((await restarted.status()).leasedTabs, 2);
state.tabs.get(leases[0].tabId).active = true;
assert.equal(
  (await restarted.release(leases[0].tabId)).wasActive,
  true,
  "explicit pool cleanup retains upstream active-tab behavior",
);
state.tabs.get(leases[0].tabId).active = false;
await restarted.release(leases[1].tabId);
assert.equal((await pool.status()).leasedTabs, 0);
state.focused = false;
assert.equal((await pool.setup(20)).pendingExpansion, 8);
assert.equal(state.created, 12);
state.focused = true;
assert.equal((await pool.setup(8)).poolSize, 20);
state.focused = false;
const service = new BrowserService(api);
let runtimeCalls = 0,
  persistedCalls = 0;
const exact = "conversation-123",
  assistant = "assistant-123";
api.scripting.executeScript = async ({ target, args }) => {
  const input = args[0];
  if (input.operation === "persisted") {
    persistedCalls++;
    assert.equal(input.conversationId, exact);
    assert.equal(input.assistantMessageId, assistant);
    return [
      {
        result: {
          ok: true,
          complete: true,
          conversation_id: exact,
          assistant_message_id: assistant,
          assistant_text: "persisted answer",
        },
      },
    ];
  }
  runtimeCalls++;
  state.tabs.get(target.tabId).url = `https://chatgpt.com/c/${exact}`;
  return [
    {
      result: {
        ok: true,
        complete: true,
        conversation_id: exact,
        assistant_message_id: assistant,
        assistant_text: "stream answer",
      },
    },
  ];
};
const started = await service.dispatch({
  method: "tabs.chatgptConversationStart",
  args: { prompt: "test", projectId: "g-p-project123", model: "gpt-5-6-pro" },
  allowedUrlPatterns: ["https://chatgpt.com/*"],
});
assert.equal(started.assistant_text, "persisted answer");
assert.equal(started.persisted_verified, true);
assert.equal(runtimeCalls, 1);
assert.equal(persistedCalls, 1);
assert.equal(state.reloads, 1);
assert.equal((await pool.status()).leasedTabs, 0);
assert.ok(
  state.updates.some((u) =>
    u.patch.url?.includes("/g/g-p-project123/project?"),
  ),
);
api.scripting.executeScript = async () => {
  runtimeCalls++;
  return [
    {
      result: {
        ok: false,
        error: {
          code: "CHATGPT_RUNTIME_INVOCATION_REJECTED",
          message: "ambiguous submission",
        },
      },
    },
  ];
};
await assert.rejects(
  service.dispatch({
    method: "tabs.chatgptConversationStart",
    args: { prompt: "no duplicate" },
    allowedUrlPatterns: ["https://chatgpt.com/*"],
  }),
  { code: "CHATGPT_RUNTIME_INVOCATION_REJECTED" },
);
assert.equal(runtimeCalls, 2, "submission errors must not retry");
assert.equal((await pool.status()).leasedTabs, 0);
await assert.rejects(
  service.dispatch({
    method: "tabs.chatgptConversationStart",
    args: { prompt: "x", transport: "raw", conversationId: exact },
    allowedUrlPatterns: ["https://chatgpt.com/*"],
  }),
  { code: "CHATGPT_CONTINUATION_TRANSPORT_INVALID" },
);

// Exercise the actual injected runtime using a mounted React/store contract and SSE.
const saved = new Map();
function globals(values) {
  for (const [key, value] of Object.entries(values)) {
    if (!saved.has(key))
      saved.set(key, Object.getOwnPropertyDescriptor(globalThis, key));
    Object.defineProperty(globalThis, key, {
      value,
      writable: true,
      configurable: true,
    });
  }
}
function restore() {
  for (const [key, descriptor] of saved) {
    if (descriptor) Object.defineProperty(globalThis, key, descriptor);
    else delete globalThis[key];
  }
  saved.clear();
}
let submitted = 0,
  bodySeen,
  assistantNodes = [];
const assistantNode = {
  innerText: "visible answer",
  closest() {
    return this;
  },
  getAttribute(name) {
    return name === "data-message-id" ? assistant : null;
  },
};
const shared = {
  isComposerSubmissionReady: true,
  conversation: null,
  composerController: {},
  isNewThread: true,
  conversationMode: { kind: "primary_assistant" },
  availableSystemHints: [],
  submitComposer(_event, action) {
    submitted++;
    assert.equal(action.kind, "text_action");
    const completion = fetch("/backend-api/f/conversation", {
      method: "POST",
    }).then(() => {
      globalThis.location = new URL(`https://chatgpt.com/c/${exact}`);
      assistantNodes = [assistantNode];
      return true;
    });
    return { accepted: true, completion };
  },
};
const props = {
  currentModelId: "gpt-5-6-pro",
  currentModelConfig: {},
  conversation: null,
  isNewThread: true,
  composerStore: {
    getSharedProps() {
      return shared;
    },
  },
  onCreateNewCompletion(arg) {
    return typeof arg.content === "string" && arg.content.length > 0;
  },
};
const composer = {
  __reactFiber$test: {
    memoizedProps: props,
    return: null,
    child: null,
    sibling: null,
  },
};
const stream = () =>
  new Response(
    `data: ${JSON.stringify({ conversation_id: exact, message: { id: assistant, author: { role: "assistant" }, content: { parts: ["stream answer"] }, status: "finished_successfully" } })}\n\ndata: [DONE]\n\n`,
    { status: 200, headers: { "content-type": "text/event-stream" } },
  );
const fetchMock = async (url, options) => {
  if (url === "/api/auth/session")
    return new Response(
      JSON.stringify({ accessToken: "synthetic-browser-only-token" }),
    );
  if (options?.body) bodySeen = JSON.parse(options.body);
  return stream();
};
try {
  globals({
    crypto: webcrypto,
    location: new URL(
      "https://chatgpt.com/?model=gpt-5-6-pro&thinking_effort=standard",
    ),
    document: {
      readyState: "complete",
      documentElement: { lang: "en" },
      querySelector: () => composer,
      querySelectorAll: (selector) =>
        selector.includes("data-message-author-role") ? assistantNodes : [],
    },
    fetch: fetchMock,
    getComputedStyle: () => ({ display: "block", visibility: "visible" }),
  });
  globals({ window: new EventTarget() });
  document.documentElement.getAttribute = (name) =>
    name === "data-chatgpt-extension-side-panel"
      ? "available"
      : '{"connected":true}';
  window.addEventListener("chatgpt-extension-request-status", () =>
    window.dispatchEvent(new Event("chatgpt-extension-status")),
  );
  const extensionStatus = await chatgptPage({ operation: "extensionStatus" });
  assert.equal(extensionStatus.available, true);
  assert.equal(extensionStatus.state.connected, true);
  assert.equal(extensionStatus.timedOut, false);
  const inventory = await runtimeInventory();
  assert.equal(inventory.ok, true);
  assert.equal(inventory.submit_candidates.length, 1);
  assert.ok(
    inventory.fiber_chain.some(
      (f) => f.safe_props.currentModelId === "gpt-5-6-pro",
    ),
  );
  assert.equal(submitted, 0, "inventory must not submit anything");
  const result = await chatgptPage({ prompt: "hello", model: "gpt-5-6-pro" });
  assert.equal(result.ok, true, JSON.stringify(result));
  assert.equal(result.assistant_text, "stream answer");
  assert.equal(result.assistant_message_id, assistant);
  assert.equal(submitted, 1);
  assert.equal(
    globalThis.fetch,
    fetchMock,
    "fetch observation must be restored",
  );
  const persisted = await chatgptPage({
    operation: "persisted",
    conversationId: exact,
    assistantMessageId: assistant,
  });
  assert.equal(persisted.assistant_text, "visible answer");
  assert.equal(
    (
      await chatgptPage({
        operation: "persisted",
        conversationId: exact,
        assistantMessageId: "wrong-message",
        timeoutMs: 20,
      })
    ).ok,
    false,
  );
  globalThis.location = new URL("https://chatgpt.com/?model=other");
  assert.equal(
    (await chatgptPage({ prompt: "x", model: "other" })).error.code,
    "CHATGPT_RUNTIME_MODEL_MISMATCH",
  );
  assert.equal(submitted, 1);
  globalThis.location = new URL(
    "https://chatgpt.com/g/g-p-project123/project?model=gpt-5-6-pro",
  );
  assert.equal(
    (await chatgptPage({ prompt: "x", projectId: "g-p-project123" })).error
      .code,
    "CHATGPT_RUNTIME_PROJECT_MISMATCH",
  );
  assert.equal(submitted, 1);
  globalThis.location = new URL(
    `https://chatgpt.com/c/${exact}?model=gpt-5-6-pro`,
  );
  props.isNewThread = false;
  shared.isNewThread = false;
  assistantNodes = [];
  const continuation = await chatgptPage({
    prompt: "continue",
    conversationId: exact,
  });
  assert.equal(continuation.operation, "continue");
  assert.equal(submitted, 2);
  globalThis.location = new URL("https://chatgpt.com/");
  const raw = await chatgptPage({
    prompt: "raw diagnostics",
    transport: "raw",
    thinkingEffort: "high",
    continueInWork: false,
  });
  assert.equal(raw.ok, true);
  assert.deepEqual(bodySeen.local_function_names, []);
  assert.equal(bodySeen.thinking_effort, "high");
  assert.equal(
    JSON.stringify(raw).includes("synthetic-browser-only-token"),
    false,
  );
  await chatgptPage({ prompt: "raw work", transport: "raw" });
  assert.deepEqual(bodySeen.local_function_names, ["local.continue_in_work"]);
} finally {
  restore();
}

// Native element setters, hidden-tab timer fallback, and adaptive click behavior.
class FakeElement extends EventTarget {
  constructor(tag) {
    super();
    this.tagName = tag;
    this.id = "field";
    this.disabled = false;
    this.isConnected = true;
    this.attrs = {};
    this.parentElement = null;
    this.innerText = "";
    this.textContent = "";
  }
  getAttribute(name) {
    return this.attrs[name] ?? null;
  }
  getBoundingClientRect() {
    return { width: 100, height: 20 };
  }
  closest() {
    return null;
  }
  click() {
    this.clicks = (this.clicks || 0) + 1;
  }
}
class FakeInput extends FakeElement {
  constructor() {
    super("INPUT");
    this.type = "text";
    this._value = "";
  }
  get value() {
    return this._value;
  }
  set value(value) {
    this._value = value;
  }
}
class FakeEvent extends Event {
  constructor(type, options) {
    super(type, options);
  }
}
const field = new FakeInput();
let inputEvents = 0;
field.addEventListener("input", () => inputEvents++);
try {
  globals({
    location: new URL("https://example.com/"),
    document: {
      title: "Fixture",
      body: { innerText: "page text" },
      querySelectorAll: (selector) =>
        selector === "#field"
          ? [field]
          : selector.includes("a[href]")
            ? [field]
            : [],
    },
    getComputedStyle: () => ({ display: "block", visibility: "visible" }),
    CSS: { escape: (value) => value },
    HTMLInputElement: FakeInput,
    HTMLTextAreaElement: class extends FakeElement {},
    HTMLSelectElement: class extends FakeElement {},
    PointerEvent: FakeEvent,
    InputEvent: FakeEvent,
    MouseEvent: FakeEvent,
    KeyboardEvent: FakeEvent,
    requestAnimationFrame: () => 0,
  });
  const filled = await pageOperation("fill", {
    selector: "#field",
    value: "new text",
  });
  assert.equal(filled.filled, true);
  assert.equal(field.value, "new text");
  assert.equal(inputEvents, 1);
  field.type = "password";
  const snapshot = await pageOperation("snapshot", {});
  assert.equal(snapshot.elements[0].value, "<redacted>");
  assert.equal(
    (await pageOperation("fill", { selector: "#field", value: "password" }))
      .value,
    "<redacted>",
  );
  field.type = "file";
  await assert.rejects(
    pageOperation("fill", { selector: "#field", value: "x" }),
    { code: "CHROME_FOREGROUND_REQUIRED" },
  );
  field.type = "text";
  field.addEventListener("pointerdown", () => {
    field.attrs["aria-expanded"] = "true";
  });
  const click = await pageOperation("click", { selector: "#field" });
  assert.equal(click.activation, "mousedown");
  assert.equal(
    field.clicks,
    undefined,
    "pointerdown activation must not get a duplicate click",
  );
  const hidden = new FakeInput();
  hidden.getBoundingClientRect = () => ({ width: 0, height: 0 });
  document.querySelectorAll = () => [hidden, field];
  const duplicate = await pageOperation("fill", {
    selector: ".duplicates",
    value: "visible match",
  });
  assert.equal(duplicate.selectedMatchIndex, 1);
  const select = new HTMLSelectElement("SELECT");
  select.options = [{ value: "b", textContent: "Beta" }];
  document.querySelectorAll = () => [select];
  assert.equal(
    (await pageOperation("fill", { selector: "select", value: " BETA " }))
      .value,
    "b",
  );
  const editable = new FakeElement("DIV");
  editable.isContentEditable = true;
  document.querySelectorAll = () => [editable];
  let inserted = 0,
    entered = 0;
  globals({
    window: { getSelection: () => ({ removeAllRanges() {}, addRange() {} }) },
  });
  document.createRange = () => ({ selectNodeContents() {} });
  document.execCommand = (command, unused, value) => {
    assert.equal(command, "insertText");
    inserted++;
    editable.textContent = value;
    return true;
  };
  editable.addEventListener("keydown", () => entered++);
  const rich = await pageOperation("fill", {
    selector: "[contenteditable]",
    value: "rich input",
    submit: true,
  });
  assert.equal(inserted, 1);
  assert.equal(entered, 1);
  assert.equal(rich.submitStrategy, "keyboard-enter");
} finally {
  restore();
}
// A pending profile read, an alarm and a reconnect must not create parallel hosts.
{
  const { api } = fakeChrome();
  const ports = [],
    timers = new Map();
  let nextTimer = 0,
    resolveProfile,
    firstRead = true;
  const readProfile = api.storage.local.get.bind(api.storage.local);
  api.storage.local.get = async (key) => {
    if (key === "macbridgeProfileId" && firstRead) {
      firstRead = false;
      await new Promise((resolve) => {
        resolveProfile = resolve;
      });
    }
    return readProfile(key);
  };
  api.identity = {
    async getProfileUserInfo() {
      return { email: "owner@example.com", id: "account" };
    },
    onSignInChanged: event(),
  };
  api.runtime.connectNative = (name) => {
    assert.equal(name, "com.macbridge.native");
    const current = {
      messages: [],
      onMessage: event(),
      onDisconnect: event(),
      postMessage(value) {
        this.messages.push(value);
      },
      disconnect() {
        this.onDisconnect.emit();
      },
    };
    ports.push(current);
    return current;
  };
  globals({
    crypto: webcrypto,
    setTimeout: (callback) => {
      const id = ++nextTimer;
      timers.set(id, callback);
      return id;
    },
    clearTimeout: (id) => timers.delete(id),
  });
  const flush = async () => {
    for (let i = 0; i < 20; i++) await Promise.resolve();
  };
  try {
    const service = startNative(api);
    api.alarms.onAlarm.emit({ name: "macbridge-maintenance" });
    resolveProfile();
    await flush();
    assert.equal(ports.length, 1);
    assert.equal(ports[0].messages[0].profile.id, "account");
    let finishRequest;
    service.dispatch = () =>
      new Promise((resolve) => {
        finishRequest = resolve;
      });
    const request = Buffer.from(
      JSON.stringify({
        type: "request",
        id: "unicode",
        method: "test",
        args: { text: "中文" },
      }),
    );
    const split = Math.floor(request.length / 2);
    for (const index of [1, 0])
      ports[0].onMessage.emit({
        type: "chunk",
        id: "unicode",
        total: 2,
        index,
        data: request
          .subarray(index ? split : 0, index ? request.length : split)
          .toString("base64"),
      });
    await flush();
    assert.equal(typeof finishRequest, "function");
    ports[0].disconnect();
    api.alarms.onAlarm.emit({ name: "macbridge-maintenance" });
    await flush();
    assert.equal(ports.length, 2);
    assert.equal(timers.size, 0, "alarm consumes the scheduled reconnect");
    ports[0].onDisconnect.emit();
    assert.equal(
      timers.size,
      0,
      "stale disconnect cannot clear a new connection",
    );
    finishRequest({ ok: true });
    await flush();
    assert.equal(
      ports[1].messages.some((message) => message.id === "unicode"),
      false,
      "old responses stay on their original connection",
    );
    assert.equal(ports[0].messages.at(-1).id, "unicode");
  } finally {
    restore();
  }
}
console.log(
  "Browser tests passed: focus-safe pool, persisted leases, single dispatch, runtime/continuation/project/raw, exact persistence, DOM fill/snapshot/click.",
);
