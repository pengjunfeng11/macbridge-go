// Serialized into MAIN world. No credentials leave this function or the page.
export async function chatgptPage(input) {
  const error = (code, message, details = {}) => ({
    ok: false,
    error: { code, message, ...details },
  });
  const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
  const currentConversation = () =>
    location.pathname.match(/^\/(?:g\/[^/]+\/)?c\/([^/]+)/)?.[1] || null;
  const textOf = (node) =>
    String(node?.innerText || node?.textContent || "").trim();
  const messageID = (node) =>
    node?.closest?.("[data-message-id]")?.getAttribute("data-message-id") ||
    null;
  const assistants = () => [
    ...document.querySelectorAll('[data-message-author-role="assistant"]'),
  ];
  const generating = () =>
    [
      ...document.querySelectorAll(
        'button[data-testid*="stop"],button[aria-label*="Stop generating"],button[aria-label*="Stop streaming"]',
      ),
    ].some((node) => {
      const box = node.getBoundingClientRect(),
        style = getComputedStyle(node);
      return (
        box.width > 0 &&
        box.height > 0 &&
        style.display !== "none" &&
        style.visibility !== "hidden"
      );
    });
  if (location.origin !== "https://chatgpt.com")
    return error("CHATGPT_TAB_UNAVAILABLE", "The selected page is not ChatGPT");
  if (input.operation === "extensionStatus") {
    return await new Promise((resolve) => {
      let timer;
      const pageBridgeAvailable =
        document.documentElement.getAttribute(
          "data-chatgpt-extension-side-panel",
        ) === "available";
      const finish = (timedOut) => {
        clearTimeout(timer);
        window.removeEventListener("chatgpt-extension-status", receive);
        const raw = document.documentElement.getAttribute(
          "data-chatgpt-extension-status",
        );
        let state = null;
        if (raw) {
          try {
            state = JSON.parse(raw);
          } catch {
            state = { raw };
          }
        }
        resolve({
          available: !timedOut || pageBridgeAvailable,
          pageBridgeAvailable,
          state,
          timedOut,
          pageUrl: location.href,
        });
      };
      const receive = () => finish(false);
      window.addEventListener("chatgpt-extension-status", receive, {
        once: true,
      });
      timer = setTimeout(() => finish(true), 1200);
      window.dispatchEvent(new Event("chatgpt-extension-request-status"));
    });
  }
  if (input.operation === "persisted") {
    if (!input.conversationId || !input.assistantMessageId)
      return error(
        "CHATGPT_CONVERSATION_HANDOFF_UNCERTAIN",
        "Exact conversation and assistant message identifiers are required for persistence verification",
      );
    if (currentConversation() !== input.conversationId)
      return error(
        "CHATGPT_RUNTIME_CONVERSATION_MISMATCH",
        "Reloaded page does not match the generated conversation",
      );
    let prior = "",
      stable = 0;
    const deadline = Date.now() + (input.timeoutMs || 15000);
    while (Date.now() < deadline) {
      const node = assistants().find(
        (node) => messageID(node) === input.assistantMessageId,
      );
      const text = textOf(node);
      if (text.length > 100000)
        return error(
          "CHATGPT_CONVERSATION_OUTPUT_LIMIT",
          "Persisted assistant exceeds 100000 characters",
        );
      stable = text && text === prior ? stable + 1 : 0;
      prior = text;
      if (
        text &&
        stable >= 2 &&
        !generating() &&
        document.readyState === "complete"
      )
        return {
          ok: true,
          complete: true,
          conversation_id: input.conversationId,
          assistant_message_id: input.assistantMessageId,
          assistant_text: text,
          persisted_response_bytes: new TextEncoder().encode(text).length,
          observation_source: "persisted-conversation",
        };
      await pause(150);
    }
    return error(
      "CHATGPT_CONVERSATION_HANDOFF_UNCERTAIN",
      "The exact generated assistant message was not persisted after reload",
    );
  }
  const prompt = input.prompt,
    model = input.model || "gpt-5-6-pro",
    effort = input.thinkingEffort || "standard";
  const expected = input.conversationId || null,
    project = input.projectId || null,
    runtimeMs = (input.maxRuntimeSeconds || 600) * 1000;
  if (
    typeof prompt !== "string" ||
    !prompt ||
    new TextEncoder().encode(prompt).length > 4000000
  )
    return error(
      "CHATGPT_PROMPT_INVALID",
      "Prompt must contain 1–4000000 UTF-8 bytes",
    );
  if (
    !/^[A-Za-z0-9._:/-]{1,128}$/.test(model) ||
    !["minimal", "low", "standard", "high", "max"].includes(effort)
  )
    return error("CHATGPT_MODEL_INVALID", "Invalid model or thinking effort");
  const readers = new Set();
  async function parseStream(response) {
    if (!response.ok)
      return error(
        {
          401: "CHATGPT_SESSION_UNAVAILABLE",
          403: "CHATGPT_CONVERSATION_REQUIREMENTS_UNAVAILABLE",
          404: "CHATGPT_CONVERSATION_PROTOCOL_CHANGED",
          429: "CHATGPT_CONVERSATION_RATE_LIMITED",
        }[response.status] || "CHATGPT_CONVERSATION_REJECTED",
        `ChatGPT returned HTTP ${response.status}`,
      );
    if (!response.body)
      return error(
        "CHATGPT_CONVERSATION_PROTOCOL_CHANGED",
        "ChatGPT returned no event stream",
      );
    const reader = response.body.getReader();
    readers.add(reader);
    const decoder = new TextDecoder();
    let carry = "",
      bytes = 0,
      count = 0,
      parsed = 0,
      bad = 0,
      text = "",
      conversation = expected,
      assistant = null,
      complete = false;
    const types = new Set();
    function consume(frame) {
      const data = frame
        .split("\n")
        .filter((line) => line.startsWith("data:"))
        .map((line) => line.slice(5).trimStart())
        .join("\n")
        .trim();
      if (!data) return;
      if (++count > 2000) throw new Error("Event count exceeded 2000");
      if (data === "[DONE]") {
        complete = true;
        return;
      }
      let value;
      try {
        value = JSON.parse(data);
        parsed++;
      } catch {
        bad++;
        return;
      }
      if (!value || typeof value !== "object") return;
      const type = value.type || value.event || "";
      if (type.length < 100) types.add(type);
      conversation =
        value.conversation_id || value.conversationId || conversation;
      const message = value.message;
      if (message?.author?.role === "assistant") {
        assistant = message.id || assistant;
        const content = message.content;
        const next =
          typeof content === "string"
            ? content
            : content?.text ||
              content?.parts
                ?.map((p) => (typeof p === "string" ? p : p?.text || ""))
                .join("");
        if (next) text = next;
        if (message.status === "finished_successfully") complete = true;
      }
      assistant = value.message_id || assistant;
      if (typeof value.output_text === "string") text = value.output_text;
      if (typeof value.delta === "string") text += value.delta;
      if (typeof value.text === "string" && /delta|text/i.test(type))
        text += value.text;
      if (typeof value.choices?.[0]?.delta?.content === "string")
        text += value.choices[0].delta.content;
      if (/(?:^|[._-])(done|completed|finished)(?:$|[._-])/i.test(type))
        complete = true;
      if (text.length > 100000)
        throw new Error("Assistant output exceeded 100000 characters");
    }
    try {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        bytes += value.byteLength;
        if (bytes > 4194304) throw new Error("Stream exceeded 4 MiB");
        carry += decoder.decode(value, { stream: true });
        carry = carry.replace(/\r\n/g, "\n");
        let boundary;
        while ((boundary = carry.indexOf("\n\n")) >= 0) {
          consume(carry.slice(0, boundary));
          carry = carry.slice(boundary + 2);
        }
      }
      carry += decoder.decode();
      if (carry.trim()) consume(carry);
      if (!parsed && bytes)
        return error(
          "CHATGPT_CONVERSATION_PROTOCOL_CHANGED",
          "Stream has no recognized JSON events",
        );
      return {
        ok: true,
        complete,
        conversation_id: conversation,
        assistant_message_id: assistant,
        assistant_text: text,
        response_bytes: bytes,
        event_count: count,
        parsed_event_count: parsed,
        parse_failure_count: bad,
        event_types: [...types].slice(0, 50),
      };
    } catch (e) {
      reader.cancel().catch(() => {});
      return error("CHATGPT_CONVERSATION_STREAM_ERROR", e.message, {
        conversation_id: conversation,
        complete: false,
      });
    } finally {
      readers.delete(reader);
    }
  }
  const enrich = (result) => ({
    ...result,
    transport: input.transport || "runtime",
    model,
    thinking_effort: effort,
    ...(project ? { project_id: project } : {}),
    prompt_bytes: new TextEncoder().encode(prompt).length,
    max_runtime_seconds: runtimeMs / 1000,
    endpoint: "/backend-api/f/conversation",
    page_url: location.href,
    operation: expected ? "continue" : "start",
  });
  if (input.transport === "raw") {
    if (expected)
      return error(
        "CHATGPT_CONTINUATION_TRANSPORT_INVALID",
        "Raw transport does not continue existing conversations",
      );
    let token = "";
    const controller = new AbortController();
    const timeout = setTimeout(
      () => controller.abort(),
      Math.min(runtimeMs, 180000),
    );
    try {
      const session = await fetch("/api/auth/session", {
        credentials: "same-origin",
        signal: controller.signal,
      });
      if (!session.ok)
        return error(
          "CHATGPT_SESSION_UNAVAILABLE",
          "Browser session is unavailable",
        );
      token = (await session.json()).accessToken || "";
      if (!token)
        return error("CHATGPT_SESSION_UNAVAILABLE", "Sign in to ChatGPT first");
      const body = {
        action: "next",
        messages: [
          {
            id: crypto.randomUUID(),
            author: { role: "user" },
            create_time: Date.now() / 1000,
            content: { content_type: "text", parts: [prompt] },
            metadata: {
              selected_sources: [],
              serialization_metadata: { custom_symbol_offsets: [] },
            },
          },
        ],
        parent_message_id: "client-created-root",
        model,
        client_prepare_state: "success",
        timezone_offset_min: new Date().getTimezoneOffset(),
        timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC",
        conversation_mode: project
          ? { kind: "gizmo_interaction", gizmo_id: project }
          : { kind: "primary_assistant" },
        enable_message_followups: true,
        system_hints: [],
        supports_buffering: true,
        supported_encodings: [],
        paragen_cot_summary_display_override: "allow",
        force_parallel_switch: "auto",
        thinking_effort: effort,
        local_function_names:
          input.continueInWork === false ? [] : ["local.continue_in_work"],
      };
      const response = await fetch("/backend-api/f/conversation", {
        method: "POST",
        credentials: "same-origin",
        signal: controller.signal,
        headers: {
          accept: "text/event-stream",
          "content-type": "application/json",
          "oai-language": document.documentElement.lang || "en-US",
          authorization: `Bearer ${token}`,
        },
        body: JSON.stringify(body),
      });
      token = "";
      return enrich(await parseStream(response));
    } catch {
      return error(
        controller.signal.aborted
          ? "CHATGPT_CONVERSATION_TIMEOUT"
          : "CHATGPT_CONVERSATION_NETWORK_ERROR",
        "Raw browser request did not complete",
      );
    } finally {
      token = "";
      clearTimeout(timeout);
      for (const reader of readers) reader.cancel().catch(() => {});
    }
  }
  let node, fiber;
  const mountDeadline = Date.now() + 20000;
  while (Date.now() < mountDeadline) {
    node = document.querySelector(
      '#prompt-textarea,[contenteditable="true"][data-testid*="composer"],form textarea',
    );
    if (node) {
      const key = Object.keys(node).find((key) =>
        /^__react(?:Fiber|InternalInstance)\$/.test(key),
      );
      if (key) {
        fiber = node[key];
        break;
      }
    }
    await pause(100);
  }
  if (!fiber)
    return error(
      "CHATGPT_RUNTIME_CONTRACT_CHANGED",
      "Mounted React composer is unavailable; no submission occurred",
    );
  let root = fiber;
  const contexts = new Map();
  for (let hops = 0; root && hops < 120; hops++) {
    const p = root.memoizedProps;
    if (
      p &&
      typeof p.onCreateNewCompletion === "function" &&
      typeof p.currentModelId === "string" &&
      "currentModelConfig" in p &&
      "conversation" in p &&
      "isNewThread" in p
    ) {
      const source = Function.prototype.toString.call(p.onCreateNewCompletion);
      if (
        p.onCreateNewCompletion.length === 1 &&
        /\.content\.length/.test(source) &&
        /typeof\s+[A-Za-z_$][\w$]*\.content/.test(source)
      )
        contexts.set(p.onCreateNewCompletion, p);
    }
    if (!root.return) break;
    root = root.return;
  }
  if (contexts.size !== 1)
    return error(
      "CHATGPT_RUNTIME_CONTRACT_CHANGED",
      `Expected one model context, found ${contexts.size}`,
    );
  const context = [...contexts.values()][0];
  if (context.currentModelId !== model)
    return error(
      "CHATGPT_RUNTIME_MODEL_MISMATCH",
      "Requested ChatGPT model is not active",
    );
  const routeEffort = new URL(location.href).searchParams.get(
    "thinking_effort",
  );
  if (routeEffort && routeEffort !== effort)
    return error(
      "CHATGPT_RUNTIME_THINKING_EFFORT_MISMATCH",
      "Thinking effort does not match the route",
    );
  if (
    currentConversation() !== expected ||
    context.isNewThread !== (expected === null)
  )
    return error(
      "CHATGPT_RUNTIME_CONVERSATION_MISMATCH",
      "The mounted conversation is not the requested exact thread",
    );
  if (project && !expected && location.pathname !== `/g/${project}/project`)
    return error(
      "CHATGPT_RUNTIME_PROJECT_MISMATCH",
      "The project route does not match",
    );
  if (
    context.disabled ||
    context.submitPending ||
    context.isCompletionInProgress
  )
    return error("CHATGPT_RUNTIME_NOT_READY", "The composer is busy");
  const stores = new Map(),
    seen = new WeakSet();
  function inspect(value, depth = 0) {
    if (
      !value ||
      !["object", "function"].includes(typeof value) ||
      seen.has(value) ||
      depth > 5
    )
      return;
    seen.add(value);
    let properties;
    try {
      properties = Object.getOwnPropertyDescriptors(value);
    } catch {
      return;
    }
    for (const [key, d] of Object.entries(properties).slice(0, 300)) {
      if (
        !("value" in d) ||
        /token|cookie|authorization|credential|secret|password|account|email/i.test(
          key,
        )
      )
        continue;
      if (key === "getSharedProps" && typeof d.value === "function") {
        try {
          const p = d.value.call(value);
          const required = [
            "isComposerSubmissionReady",
            "conversation",
            "composerController",
            "isNewThread",
            "submitComposer",
            "conversationMode",
            "availableSystemHints",
          ];
          if (
            required.every((k) => Object.hasOwn(p, k)) &&
            typeof p.submitComposer === "function"
          )
            stores.set(p.submitComposer, p);
        } catch {}
      } else if (
        /store|composer|bridge|shared|controller|state|current/i.test(key)
      )
        inspect(d.value, depth + 1);
    }
  }
  const fibers = [root],
    visited = new Set();
  while (fibers.length && visited.size < 50000) {
    const next = fibers.pop();
    if (!next || visited.has(next)) continue;
    visited.add(next);
    inspect(next.memoizedProps);
    let hook = next.memoizedState;
    for (let i = 0; hook && i < 200; i++, hook = hook.next) {
      inspect(hook.memoizedState);
      inspect(hook.baseState);
    }
    fibers.push(next.child, next.sibling);
  }
  if (stores.size !== 1)
    return error(
      "CHATGPT_RUNTIME_CONTRACT_CHANGED",
      `Expected one submitComposer store, found ${stores.size}`,
    );
  const [submit, shared] = [...stores.entries()][0];
  if (shared.isNewThread !== (expected === null))
    return error(
      "CHATGPT_RUNTIME_CONVERSATION_MISMATCH",
      "Composer thread state differs from the route",
    );
  if (
    project &&
    (shared.conversationMode?.kind !== "gizmo_interaction" ||
      shared.conversationMode?.gizmo_id !== project)
  )
    return error(
      "CHATGPT_RUNTIME_PROJECT_MISMATCH",
      "Composer project binding differs from the requested project",
    );
  if (!shared.isComposerSubmissionReady || shared.isDisabled)
    return error(
      "CHATGPT_RUNTIME_NOT_READY",
      "Composer submission is not ready",
    );
  const material =
    Object.keys(shared).sort().join(",") +
    Function.prototype.toString.call(submit);
  const hash = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(material),
  );
  const fingerprint = [...new Uint8Array(hash)]
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("")
    .slice(0, 16);
  const initialIDs = new Set(assistants().map(messageID));
  const initialNodes = new Set(assistants());
  const original = globalThis.fetch;
  let observed = false,
    network = null,
    actionDone = false,
    actionFailed = false;
  let last = "",
    stable = 0;
  const wrapper = async function (...args) {
    let matches = false;
    try {
      const url = new URL(
        args[0] instanceof Request ? args[0].url : String(args[0]),
        location.href,
      );
      matches =
        url.origin === location.origin &&
        url.pathname === "/backend-api/f/conversation";
    } catch {}
    try {
      const response = await original.apply(this, args);
      if (matches && !observed) {
        observed = true;
        parseStream(response.clone()).then((value) => {
          network = value;
        });
      }
      return response;
    } catch (e) {
      if (matches)
        network = error(
          "CHATGPT_CONVERSATION_NETWORK_ERROR",
          "The runtime request failed",
        );
      throw e;
    }
  };
  globalThis.fetch = wrapper;
  try {
    let dispatch;
    try {
      dispatch = submit(new Event("submit"), {
        kind: "text_action",
        text: prompt,
      });
    } catch {
      return error(
        "CHATGPT_RUNTIME_INVOCATION_REJECTED",
        "The submitComposer action rejected the request",
      );
    }
    if (dispatch?.accepted !== true)
      return error(
        "CHATGPT_RUNTIME_INVOCATION_REJECTED",
        "The submitComposer action did not accept the request",
      );
    Promise.resolve(dispatch.completion)
      .then(
        (value) => {
          actionFailed = value === false;
        },
        () => {
          actionFailed = true;
        },
      )
      .finally(() => {
        actionDone = true;
      });
    const deadline = Date.now() + runtimeMs;
    while (Date.now() < deadline) {
      if (network?.ok === false) return enrich(network);
      const latest = assistants()
        .filter(
          (n) =>
            !initialNodes.has(n) &&
            (!messageID(n) || !initialIDs.has(messageID(n))),
        )
        .at(-1);
      const text = textOf(latest);
      if (text.length > 100000)
        return error(
          "CHATGPT_CONVERSATION_OUTPUT_LIMIT",
          "Assistant response exceeds 100000 characters",
        );
      stable = text && text === last ? stable + 1 : 0;
      last = text;
      if (
        actionDone &&
        network?.complete &&
        network.assistant_text &&
        network.assistant_message_id
      )
        return {
          ...enrich(network),
          runtime_action: "submitComposer:text_action",
          runtime_fingerprint: fingerprint,
        };
      if (
        actionDone &&
        text &&
        stable >= 3 &&
        !generating() &&
        messageID(latest)
      )
        return {
          ...enrich({
            ok: true,
            complete: true,
            conversation_id: currentConversation(),
            assistant_message_id: messageID(latest),
            assistant_text: text,
            response_bytes: new TextEncoder().encode(text).length,
            observation_source: "rendered-runtime",
          }),
          runtime_action: "submitComposer:text_action",
          runtime_fingerprint: fingerprint,
        };
      if (actionDone && actionFailed && !latest)
        return error(
          "CHATGPT_RUNTIME_INVOCATION_REJECTED",
          "Runtime completion was rejected; no retry occurred",
        );
      await pause(250);
    }
    return error(
      "CHATGPT_CONVERSATION_HANDOFF_UNCERTAIN",
      "Runtime deadline elapsed without a complete response; no retry occurred",
      { conversation_id: currentConversation(), complete: false },
    );
  } finally {
    if (globalThis.fetch === wrapper) globalThis.fetch = original;
    for (const reader of readers) reader.cancel().catch(() => {});
  }
}

// Read-only diagnostics for changing first-party bundles and mounted composer contracts.
export async function runtimeInventory() {
  if (location.origin !== "https://chatgpt.com")
    return {
      ok: false,
      error: {
        code: "CHATGPT_TAB_UNAVAILABLE",
        message: "Runtime inventory requires a ChatGPT page",
      },
    };
  const sensitive =
    /cookie|token|authorization|password|secret|credential|email|account|device.?id/i;
  const action =
    /send|submit|start|create|continue|conversation|message|prompt|turn|completion|dispatch|mutate/i;
  const signalRules = [
    ["conversation-endpoint", /backend-api\/(?:f\/)?conversation/i],
    ["chat-requirements", /sentinel|turnstile|arkose|chat.?requirements/i],
    [
      "conversation-action",
      /parent_message_id|thinking_effort|client_prepare_state/i,
    ],
    ["submit-action", /submit|sendMessage|createConversation/i],
  ];
  const hash = async (value) =>
    [
      ...new Uint8Array(
        await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value)),
      ),
    ]
      .map((b) => b.toString(16).padStart(2, "0"))
      .join("");
  async function describe(fn, limit = 1200) {
    const source = Function.prototype.toString.call(fn);
    return {
      name: fn.name || "",
      arity: fn.length,
      signals: signalRules
        .filter(([, rule]) => rule.test(source))
        .map(([name]) => name),
      source_sha256: await hash(source),
      source_preview: source.replace(/\s+/g, " ").slice(0, limit),
    };
  }
  const candidates = [],
    submitCandidates = [],
    factories = [],
    bundlers = [],
    fiberChain = [];
  const functions = new WeakSet(),
    submits = new WeakSet(),
    objects = new WeakSet();
  let containers = 0;
  async function walk(value, path, kind, moduleId = null, depth = 0) {
    if (
      !value ||
      !["object", "function"].includes(typeof value) ||
      depth > 6 ||
      containers++ > 100000 ||
      objects.has(value)
    )
      return;
    objects.add(value);
    if (
      typeof value === "function" &&
      !functions.has(value) &&
      candidates.length < 300
    ) {
      const item = await describe(value);
      if (action.test(path) || item.signals.length) {
        functions.add(value);
        candidates.push({ ...item, path, kind, module_id: moduleId });
      }
    }
    let descriptors;
    try {
      descriptors = Object.getOwnPropertyDescriptors(value);
    } catch {
      return;
    }
    for (const [key, d] of Object.entries(descriptors).slice(0, 250)) {
      if (!("value" in d) || sensitive.test(key)) continue;
      const nested = d.value;
      if (key === "getSharedProps" && typeof nested === "function") {
        try {
          const shared = nested.call(value);
          const fn = Object.getOwnPropertyDescriptor(
            shared || {},
            "submitComposer",
          )?.value;
          if (
            typeof fn === "function" &&
            !submits.has(fn) &&
            submitCandidates.length < 50
          ) {
            submits.add(fn);
            submitCandidates.push({
              ...(await describe(fn, 1800)),
              path: `${path}.getSharedProps().submitComposer`,
              owner_type_name: "",
              shared_props_keys: Object.keys(shared)
                .filter((k) => !sensitive.test(k))
                .slice(0, 250),
            });
          }
        } catch {}
      }
      if (
        key === "submitComposer" &&
        typeof nested === "function" &&
        !submits.has(nested) &&
        submitCandidates.length < 50
      ) {
        submits.add(nested);
        submitCandidates.push({
          ...(await describe(nested, 1800)),
          path: `${path}.submitComposer`,
          owner_type_name: "",
          shared_props_keys: [],
        });
      }
      await walk(nested, `${path}.${key}`, kind, moduleId, depth + 1);
    }
  }
  let runtime,
    cached = 0,
    factoryCount = 0;
  for (const key of Object.getOwnPropertyNames(globalThis)
    .filter((k) => /webpack.*chunk|chunk.*webpack/i.test(k))
    .slice(0, 20)) {
    const array = globalThis[key];
    if (!Array.isArray(array)) continue;
    const entry = {
      global: key,
      chunks: array.length,
      runtime_acquired: false,
    };
    if (!runtime) {
      try {
        array.push([
          [`macbridge-inventory-${crypto.randomUUID()}`],
          {},
          (value) => {
            runtime = value;
          },
        ]);
        entry.runtime_acquired = !!runtime;
      } catch (e) {
        entry.error = e.message.slice(0, 300);
      }
    }
    bundlers.push(entry);
  }
  if (runtime) {
    const modules = Object.entries(runtime.c || {}).slice(0, 30000);
    cached = modules.length;
    for (const [id, record] of modules) {
      if (candidates.length >= 300) break;
      await walk(record?.exports, `module.${id}.exports`, "webpack-export", id);
    }
    const definitions = Object.entries(runtime.m || {}).slice(0, 30000);
    factoryCount = definitions.length;
    for (const [id, fn] of definitions) {
      if (factories.length >= 200) break;
      if (typeof fn !== "function") continue;
      const d = await describe(fn, 1400);
      if (d.signals.length) factories.push({ ...d, module_id: id });
    }
  }
  const roots = [
    document.querySelector("#prompt-textarea"),
    document.querySelector('[contenteditable="true"][data-testid*="composer"]'),
    document.querySelector("form textarea"),
    document.querySelector("main"),
    document.body,
  ].filter(Boolean);
  let top;
  for (const [index, element] of roots.entries()) {
    for (const key of Object.keys(element).filter((k) =>
      /^__react(?:Props|Fiber|Container|InternalInstance)\$/.test(k),
    )) {
      await walk(element[key], `react.${index}.${key}`, "react-runtime");
      if (!top && /^__react(?:Fiber|InternalInstance)\$/.test(key))
        top = element[key];
    }
  }
  let fiber = top;
  for (let depth = 0; fiber && depth < 120; depth++, fiber = fiber.return) {
    top = fiber;
    const props = fiber.memoizedProps || {},
      keys = Object.keys(props)
        .filter((k) => !sensitive.test(k))
        .slice(0, 250),
      safe = {},
      actions = [];
    for (const key of [
      "currentModelId",
      "isNewThread",
      "isCompletionInProgress",
      "submitPending",
      "disabled",
      "commitComposerStateOnSubmit",
    ]) {
      const value = Object.getOwnPropertyDescriptor(props, key)?.value;
      if (
        value === null ||
        ["string", "boolean", "number"].includes(typeof value)
      )
        safe[key] = value;
    }
    for (const key of keys) {
      const value = Object.getOwnPropertyDescriptor(props, key)?.value;
      if (action.test(key) && typeof value === "function")
        actions.push({ key, ...(await describe(value, 1600)) });
    }
    const hooks = [];
    let hook = fiber.memoizedState;
    for (let i = 0; hook && i < 180; i++, hook = hook.next) {
      for (const slot of ["memoizedState", "baseState"]) {
        const value = hook[slot];
        if (typeof value === "function") {
          const d = await describe(value, 1600);
          if (action.test(d.name) || d.signals.length)
            hooks.push({ hook_index: i, slot, ...d });
        }
      }
    }
    const type = fiber.elementType || fiber.type;
    let typeHash = null;
    const excerpts = [];
    if (typeof type === "function") {
      const source = Function.prototype.toString.call(type);
      typeHash = await hash(source);
      for (const needle of [
        "onRequestCompletion",
        "onCreateNewCompletion",
        "onComposerSubmit",
        "commitComposerStateOnSubmit",
        "onSubmit",
        "timeStamp",
      ]) {
        const position = source.indexOf(needle);
        if (position >= 0)
          excerpts.push({
            needle,
            excerpt: source
              .slice(Math.max(0, position - 600), position + 1500)
              .replace(/\s+/g, " "),
          });
      }
    }
    fiberChain.push({
      depth,
      tag: fiber.tag ?? null,
      key: fiber.key ?? null,
      type_name:
        typeof type === "function"
          ? type.displayName || type.name || ""
          : typeof type === "string"
            ? type
            : "",
      prop_keys: keys,
      safe_props: safe,
      action_props: actions,
      hook_candidates: hooks,
      type_source_sha256: typeHash,
      type_source_excerpts: excerpts,
    });
  }
  const stack = [top],
    visited = new Set();
  while (stack.length && visited.size < 50000) {
    const current = stack.pop();
    if (!current || visited.has(current)) continue;
    visited.add(current);
    await walk(
      current.memoizedProps,
      `fiberTree.${visited.size}.memoizedProps`,
      "react-runtime",
    );
    let hook = current.memoizedState;
    for (let i = 0; hook && i < 200; i++, hook = hook.next) {
      await walk(
        hook.memoizedState,
        `fiberTree.${visited.size}.hook.${i}.memoizedState`,
        "react-runtime",
      );
      await walk(
        hook.baseState,
        `fiberTree.${visited.size}.hook.${i}.baseState`,
        "react-runtime",
      );
    }
    stack.push(current.child, current.sibling);
  }
  return {
    ok: true,
    title: document.title,
    url: location.href,
    scripts: [...(document.scripts || [])]
      .map((s) => s.src)
      .filter(Boolean)
      .slice(-250),
    bundlers,
    cached_module_count: cached,
    factory_module_count: factoryCount,
    candidates,
    factory_candidates: factories,
    fiber_chain: fiberChain,
    traversed_fiber_count: visited.size,
    submit_candidates: submitCandidates,
    rendered_assistant_messages: [
      ...document.querySelectorAll('[data-message-author-role="assistant"]'),
    ]
      .slice(-20)
      .map((node) => ({
        tag: (node.tagName || "").toLowerCase(),
        id: node.id || null,
        class_name: String(node.className || "").slice(0, 500),
        data_testid: node.getAttribute("data-testid"),
        text: String(node.innerText || node.textContent || "")
          .trim()
          .slice(0, 100000),
      })),
  };
}
