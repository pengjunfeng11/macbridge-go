// Runs as a self-contained MAIN-world function; no imported closures are required.
export async function taskPage(input = {}) {
  const { operation, expected = {}, proposed = {} } = input;
  const slot = "__macbridgeTaskHooksV1";
  const hooks = () => (window[slot] ||= {});
  const secret =
    /cookie|token|authorization|credential|password|secret|email|user|account/i;
  const prose = /prompt|instruction|message|content/i;
  const digest = async (value) => {
    const bytes = new TextEncoder().encode(String(value ?? ""));
    const hash = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
    return {
      bytes: bytes.length,
      sha256: Array.from(hash, (b) => b.toString(16).padStart(2, "0")).join(""),
    };
  };
  const matchesDigest = (actual, source, field) =>
    actual.bytes === source[`${field}_bytes`] &&
    actual.sha256 === source[`${field}_sha256`];
  const sanitize = async (
    value,
    key = "root",
    depth = 0,
    seen = new WeakSet(),
  ) => {
    if (depth > 10) return "<depth-limit>";
    if (typeof value === "string") {
      if (prose.test(key))
        return { value: "<redacted>", ...(await digest(value)) };
      if (secret.test(key)) return "<redacted>";
      return value.length > 500
        ? { value: "<redacted-long>", ...(await digest(value)) }
        : value;
    }
    if (
      value == null ||
      typeof value === "number" ||
      typeof value === "boolean"
    )
      return value;
    if (typeof value !== "object") return `<${typeof value}>`;
    if (seen.has(value)) return "<cycle>";
    seen.add(value);
    if (Array.isArray(value))
      return await Promise.all(
        value
          .slice(0, 100)
          .map((v, i) => sanitize(v, String(i), depth + 1, seen)),
      );
    const result = {};
    for (const [name, child] of Object.entries(value).slice(0, 300)) {
      result[name] =
        secret.test(name) && !prose.test(name)
          ? "<redacted>"
          : await sanitize(child, name, depth + 1, seen);
    }
    return result;
  };
  const taskID = (task) =>
    String(task?.id || task?.task_id || task?.automation_id || "");
  const walk = (root, visitor, maxDepth = 14) => {
    const seen = new WeakSet();
    let visited = 0;
    const next = (value, path, depth) => {
      if (++visited > 50000 || depth > maxDepth || value == null) return null;
      try {
        const found = visitor(value, path);
        if (found) return found;
      } catch {}
      if (!["object", "function"].includes(typeof value) || seen.has(value))
        return null;
      seen.add(value);
      let names;
      try {
        names = Reflect.ownKeys(value);
      } catch {
        return null;
      }
      for (const name of names.slice(0, 500)) {
        if (typeof name !== "string" || secret.test(name)) continue;
        let child;
        try {
          child = value[name];
        } catch {
          continue;
        }
        const found = next(child, `${path}.${name}`, depth + 1);
        if (found) return found;
      }
      return null;
    };
    return next(root, "root", 0);
  };
  const findTask = (root, id) =>
    walk(root, (v) =>
      v &&
      typeof v === "object" &&
      typeof v.schedule === "string" &&
      taskID(v) &&
      (!id || taskID(v) === id)
        ? v
        : null,
    );
  const editorRoots = () =>
    [
      ...document.querySelectorAll(
        'textarea[aria-label="Title"],textarea[aria-label="Instructions"],button[aria-label="Repeat"],button[aria-label="End repeat"]',
      ),
      document.body,
    ]
      .filter(Boolean)
      .flatMap((node) =>
        Object.keys(node)
          .filter((name) => /^__react(?:Props|Fiber|Container)\$/.test(name))
          .map((name) => node[name]),
      );
  const identity = async (task) => {
    const pd = await digest(task.prompt || "");
    const pairs = {
      stable_id: [taskID(task), expected.stable_id],
      conversation_id: [
        String(task.conversation_id || ""),
        expected.conversation_id,
      ],
      title: [task.title, expected.title],
      enabled: [task.is_enabled, expected.enabled],
      executor: [task.executor, expected.executor],
      timezone: [task.default_timezone, expected.timezone],
      timing_mode: [task.timing_mode, expected.timing_mode],
      work_mode: [
        task.source_conversation_is_work_mode,
        expected.source_conversation_is_work_mode,
      ],
      prompt_bytes: [pd.bytes, expected.prompt_bytes],
      prompt_sha256: [pd.sha256, expected.prompt_sha256],
      schedule: [task.schedule, expected.schedule],
      last_run_time: [task.last_run_time, expected.last_run_time],
    };
    return {
      failed: Object.entries(pairs)
        .filter(([, [a, b]]) => a !== b)
        .map(([name]) => name),
      prompt_digest: pd,
    };
  };
  const proposedDigests = async () => ({
    prompt: await digest(proposed.prompt || ""),
    schedule: await digest(proposed.schedule || ""),
  });
  const requestBody = async (input, init) => {
    if (init && Object.hasOwn(init, "body")) {
      if (typeof init.body === "string") return init.body;
      if (init.body instanceof URLSearchParams) return init.body.toString();
      if (init.body instanceof Blob) return await init.body.text();
      return "";
    }
    if (input instanceof Request)
      return await input
        .clone()
        .text()
        .catch(() => "");
    return "";
  };
  const record = (state, item) => {
    state.records.push(item);
    if (state.records.length > 1000) state.records.shift();
    return item;
  };
  const installHooks = (state, intercept) => {
    state.originalFetch = window.fetch;
    state.originalOpen = XMLHttpRequest.prototype.open;
    state.originalSend = XMLHttpRequest.prototype.send;
    const requests = new WeakMap();
    state.fetch = async function (resource, options) {
      const url = resource instanceof Request ? resource.url : String(resource);
      const method =
        options?.method ||
        (resource instanceof Request ? resource.method : "GET");
      if (!state.installed || !state.matches(url, method))
        return state.originalFetch.apply(this, arguments);
      const checked = await intercept(
        await requestBody(resource, options),
        "fetch",
        url,
        method,
      );
      if (!checked.ok) throw new Error(checked.error);
      let response;
      try {
        response =
          resource instanceof Request
            ? await state.originalFetch.call(
                this,
                new Request(resource, { ...options, body: checked.body }),
              )
            : await state.originalFetch.call(this, resource, {
                ...options,
                body: checked.body,
              });
      } catch (error) {
        checked.record.response = {
          error: String(error?.message || error),
          uncertain: true,
        };
        throw error;
      }
      void response
        .clone()
        .text()
        .then(async (text) => {
          checked.record.response = {
            status: response.status,
            ok: response.ok,
            digest: await digest(text),
          };
        })
        .catch((error) => {
          checked.record.response = {
            status: response.status,
            ok: response.ok,
            error: String(error),
          };
        });
      return response;
    };
    state.open = function (method, url) {
      requests.set(this, { method, url });
      return state.originalOpen.apply(this, arguments);
    };
    state.send = function (body) {
      const request = requests.get(this) || {};
      if (!state.installed || !state.matches(request.url, request.method))
        return state.originalSend.apply(this, arguments);
      void (async () => {
        const checked = await intercept(
          await requestBody(null, { body }),
          "xhr",
          request.url,
          request.method,
        );
        if (!checked.ok) {
          try {
            this.abort();
          } catch {}
          this.dispatchEvent(new Event("error"));
          return;
        }
        this.addEventListener(
          "loadend",
          async () => {
            try {
              checked.record.response = {
                status: this.status,
                ok: this.status >= 200 && this.status < 300,
                digest: await digest(this.responseText || ""),
              };
            } catch (error) {
              checked.record.response = {
                status: this.status,
                error: String(error),
              };
            }
          },
          { once: true },
        );
        state.originalSend.call(this, checked.body);
      })().catch((error) => {
        record(state, {
          channel: "xhr",
          submitted: false,
          code: "interceptor_error",
          error: String(error),
        });
        try {
          this.abort();
          this.dispatchEvent(new Event("error"));
        } catch {}
      });
    };
    window.fetch = state.fetch;
    XMLHttpRequest.prototype.open = state.open;
    XMLHttpRequest.prototype.send = state.send;
  };
  const remove = (kind) => {
    const state = hooks()[kind];
    if (!state?.installed) return { removed: false };
    state.installed = false;
    if (window.fetch === state.fetch) window.fetch = state.originalFetch;
    if (XMLHttpRequest.prototype.open === state.open)
      XMLHttpRequest.prototype.open = state.originalOpen;
    if (XMLHttpRequest.prototype.send === state.send)
      XMLHttpRequest.prototype.send = state.originalSend;
    delete hooks()[kind];
    return { removed: true };
  };

  if (operation === "readTaskMutationProbe")
    return {
      installed: !!hooks().probe?.installed,
      records: hooks().probe?.records || [],
    };
  if (operation === "removeTaskMutationProbe") return remove("probe");
  if (operation === "readExactTaskSaveRewrite") {
    const state = hooks().rewrite;
    return state?.installed
      ? {
          installed: true,
          installed_at: state.installed_at,
          submission_seen: state.submission_seen,
          records: state.records,
        }
      : { installed: false, records: [] };
  }
  if (operation === "removeExactTaskSaveRewrite") return remove("rewrite");
  if (operation === "installTaskMutationProbe") {
    if (hooks().probe?.installed)
      return { installed: true, alreadyInstalled: true };
    if (hooks().rewrite?.installed)
      return { installed: false, code: "save_rewrite_still_installed" };
    const state = {
      installed: true,
      records: [],
      matches: (url, method) =>
        String(method || "GET").toUpperCase() !== "GET" &&
        /automation|scheduled|task/i.test(String(url)),
    };
    installHooks(state, async (raw, channel, url, method) => {
      let parsed = raw;
      try {
        parsed = raw ? JSON.parse(raw) : null;
      } catch {}
      record(state, {
        channel,
        url: String(url),
        method: String(method || "GET").toUpperCase(),
        captured_at: new Date().toISOString(),
        body: await sanitize(parsed),
        body_digest: await digest(raw),
      });
      return { ok: false, error: "MDB_TASK_MUTATION_PROBE_BLOCKED" };
    });
    hooks().probe = state;
    return { installed: true, alreadyInstalled: false };
  }
  if (operation === "installExactTaskSaveRewrite") {
    if (hooks().rewrite?.installed)
      return {
        installed: true,
        already_installed: true,
        records: hooks().rewrite.records,
      };
    if (hooks().probe?.installed)
      return { installed: false, code: "mutation_probe_still_installed" };
    const state = {
      installed: true,
      installed_at: new Date().toISOString(),
      records: [],
      submission_seen: false,
      checking: false,
      matches: (url, method) =>
        String(method || "GET").toUpperCase() === "POST" &&
        /\/backend-api\/automations\/save(?:\?|$)/.test(String(url)),
    };
    installHooks(state, async (raw, channel) => {
      const fail = (code, extra = {}) => ({
        ok: false,
        record: record(state, {
          channel,
          at: new Date().toISOString(),
          submitted: false,
          code,
          ...extra,
        }),
        error: `MDB_EXACT_TASK_SAVE_BLOCKED:${code}`,
      });
      if (state.checking || state.submission_seen)
        return fail("duplicate_submission_attempt");
      state.checking = true; // Reserve synchronously before digest awaits: concurrent sends cannot both win.
      try {
        let body;
        try {
          body = JSON.parse(raw);
          if (!body || typeof body !== "object" || Array.isArray(body))
            throw new Error();
        } catch {
          return fail("invalid_json_body", { body_digest: await digest(raw) });
        }
        const pd = await digest(body.prompt || "");
        const checks = {
          stable_id: body.jawbone_id === expected.stable_id,
          title: body.title === proposed.title,
          prompt_bytes: pd.bytes === proposed.prompt_bytes,
          prompt_sha256: pd.sha256 === proposed.prompt_sha256,
          native_schedule: body.schedule === proposed.native_schedule,
          timezone: body.default_timezone === expected.timezone,
          enabled: body.is_enabled === true,
          last_run_time: body.last_run_time === expected.last_run_time,
          timing_mode: body.timing_mode === 0,
        };
        const failed = Object.keys(checks).filter((key) => !checks[key]);
        if (failed.length)
          return fail("native_payload_mismatch", {
            failed_checks: failed,
            body_digest: await digest(raw),
            prompt_digest: pd,
            schedule_digest: await digest(body.schedule || ""),
          });
        const sd = await digest(proposed.schedule || "");
        if (!matchesDigest(sd, proposed, "schedule"))
          return fail("rewritten_schedule_digest_mismatch", {
            schedule_digest: sd,
          });
        if (!state.installed) return fail("rewrite_removed");
        body.schedule = proposed.schedule;
        const serialized = JSON.stringify(body);
        const item = record(state, {
          channel,
          at: new Date().toISOString(),
          submitted: true,
          code: "submitted_once",
          original_request_digest: await digest(raw),
          rewritten_request_digest: await digest(serialized),
          prompt_digest: pd,
          schedule_digest: sd,
          response: null,
        });
        if (!state.installed) {
          item.submitted = false;
          item.code = "rewrite_removed";
          return {
            ok: false,
            error: "MDB_EXACT_TASK_SAVE_BLOCKED:rewrite_removed",
          };
        }
        state.submission_seen = true;
        return { ok: true, body: serialized, record: item };
      } finally {
        state.checking = false;
      }
    });
    hooks().rewrite = state;
    return {
      installed: true,
      already_installed: false,
      installed_at: state.installed_at,
    };
  }

  if (operation === "prepareExactTaskEditor") {
    const title = document.querySelector('textarea[aria-label="Title"]');
    const prompt = document.querySelector(
      'textarea[aria-label="Instructions"]',
    );
    const repeat = document.querySelector('button[aria-label="Repeat"]');
    const end = document.querySelector('button[aria-label="End repeat"]');
    const save = [...document.querySelectorAll("button")].find(
      (button) => button.textContent?.trim() === "Save",
    );
    if (
      !(title instanceof HTMLTextAreaElement) ||
      !(prompt instanceof HTMLTextAreaElement) ||
      !repeat ||
      !save
    )
      return { prepared: false, code: "task_editor_controls_missing" };
    const current = editorRoots()
      .map((root) => findTask(root, expected.stable_id))
      .find(Boolean);
    if (!current)
      return { prepared: false, code: "stable_task_not_found_in_editor" };
    const checked = await identity(current);
    if (checked.failed.length)
      return {
        prepared: false,
        code: "identity_drift",
        failed_checks: checked.failed,
      };
    const pd = await digest(proposed.prompt || "");
    if (!matchesDigest(pd, proposed, "prompt"))
      return {
        prepared: false,
        code: "proposed_prompt_digest_mismatch",
        proposed_prompt_digest: pd,
      };
    const label = (node) => node?.textContent?.trim() || "";
    if (label(repeat) !== proposed.native_repeat_label)
      return {
        prepared: false,
        code: "native_repeat_not_ready",
        current_repeat_label: label(repeat),
      };
    const setter = Object.getOwnPropertyDescriptor(
      HTMLTextAreaElement.prototype,
      "value",
    )?.set;
    if (!setter) return { prepared: false, code: "textarea_setter_missing" };
    for (const [node, value] of [
      [title, proposed.title],
      [prompt, proposed.prompt],
    ]) {
      setter.call(node, value);
      node.dispatchEvent(
        new InputEvent("input", {
          bubbles: true,
          inputType: "insertText",
          data: null,
        }),
      );
      node.dispatchEvent(new Event("change", { bubbles: true }));
    }
    // Hidden background tabs may throttle animation frames, so include a bounded fallback.
    await new Promise((resolve) => {
      const timer = setTimeout(resolve, 100);
      requestAnimationFrame(() =>
        requestAnimationFrame(() => {
          clearTimeout(timer);
          resolve();
        }),
      );
    });
    const promptDigest = await digest(prompt.value);
    const prepared =
      title.value === proposed.title &&
      matchesDigest(promptDigest, proposed, "prompt") &&
      label(repeat) === proposed.native_repeat_label &&
      label(end) === "Never" &&
      !save.disabled;
    return {
      prepared,
      code: prepared ? "ready" : "editor_state_mismatch",
      stable_id: expected.stable_id,
      conversation_id: expected.conversation_id,
      title_digest: await digest(title.value),
      prompt_digest: promptDigest,
      repeat_label: label(repeat),
      end_repeat_label: label(end),
      save_enabled: !save.disabled,
    };
  }

  if (operation === "exactTaskSave") {
    if (hooks().probe?.installed)
      return { submitted: false, code: "mutation_probe_still_installed" };
    let response, text, inventory;
    try {
      response = await fetch("/backend-api/automations?filter=scheduled", {
        credentials: "same-origin",
        cache: "no-store",
      });
      text = await response.text();
      if (!response.ok)
        return {
          submitted: false,
          code: "read_only_inventory_http_error",
          inventory_status: response.status,
          inventory_digest: await digest(text),
        };
      inventory = JSON.parse(text);
    } catch (error) {
      return {
        submitted: false,
        code: "read_only_inventory_failed",
        error: String(error?.message || error),
      };
    }
    const current = findTask(inventory, expected.stable_id);
    if (!current)
      return {
        submitted: false,
        code: "stable_task_not_found",
        inventory_digest: await digest(text),
      };
    const checked = await identity(current);
    if (checked.failed.length)
      return {
        submitted: false,
        code: "identity_drift",
        failed_checks: checked.failed,
        current: {
          stable_id: taskID(current),
          conversation_id: current.conversation_id ?? null,
          title: current.title ?? null,
          enabled: current.is_enabled,
          executor: current.executor ?? null,
          timezone: current.default_timezone ?? null,
          timing_mode: current.timing_mode ?? null,
          source_conversation_is_work_mode:
            current.source_conversation_is_work_mode,
          prompt_digest: checked.prompt_digest,
          schedule_digest: await digest(current.schedule || ""),
          last_run_time: current.last_run_time ?? null,
        },
      };
    const d = await proposedDigests();
    for (const field of ["prompt", "schedule"])
      if (!matchesDigest(d[field], proposed, field))
        return {
          submitted: false,
          code: `proposed_${field}_digest_mismatch`,
          [`proposed_${field}_digest`]: d[field],
        };
    const body = JSON.stringify({
      default_timezone: expected.timezone,
      email_enabled: !!current.email_enabled,
      is_enabled: true,
      jawbone_id: expected.stable_id,
      last_run_time: expected.last_run_time,
      notifications_enabled: !!current.notifications_enabled,
      prompt: proposed.prompt,
      schedule: proposed.schedule,
      timing_mode: 0,
      title: proposed.title,
    });
    const evidence = {
      submitted: true,
      reconciled: false,
      submitted_at: new Date().toISOString(),
      request_digest: await digest(body),
    };
    try {
      const saved = await fetch("/backend-api/automations/save", {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body,
      });
      const raw = await saved.text();
      let parsed = null;
      try {
        parsed = JSON.parse(raw);
      } catch {}
      return {
        ...evidence,
        uncertain: false,
        endpoint: "/backend-api/automations/save",
        response_status: saved.status,
        response_ok: saved.ok,
        response_digest: await digest(raw),
        response: await sanitize(parsed),
      };
    } catch (error) {
      return {
        ...evidence,
        uncertain: true,
        code: "save_transport_uncertain",
        error: String(error?.message || error),
      };
    }
  }

  if (operation === "taskAudit") {
    const redact = (value) => {
      const text = String(value ?? "");
      return text.length > 180
        ? `<redacted:${new TextEncoder().encode(text).length}-bytes>`
        : text;
    };
    const attrs = (node) =>
      Object.fromEntries(
        [...node.attributes]
          .filter((a) =>
            /^(aria-|data-|id$|name$|role$|type$|value$)/.test(a.name),
          )
          .map((a) => [a.name, redact(a.value)]),
      );
    const describe = (node) => ({
      tag: node.tagName.toLowerCase(),
      role: node.getAttribute("role"),
      ariaLabel: node.getAttribute("aria-label"),
      ariaControls: node.getAttribute("aria-controls"),
      ariaChecked: node.getAttribute("aria-checked"),
      dataState: node.getAttribute("data-state"),
      dataValue: node.getAttribute("data-value"),
      name: node.getAttribute("name"),
      type: node.getAttribute("type"),
      value:
        "value" in node
          ? /password/i.test(node.type || "")
            ? "<redacted>"
            : redact(node.value)
          : null,
      text: redact((node.innerText || node.textContent || "").trim()),
      attrs: attrs(node),
    });
    const result = {
      title: document.title,
      url: location.href,
      timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone,
      scripts: [...document.scripts]
        .map((s) => s.src)
        .filter(Boolean)
        .slice(0, 200),
      resources: performance
        .getEntriesByType("resource")
        .map((e) => String(e.name || ""))
        .filter((s) =>
          /task|sched|backend-api|conversation|rrule|recurr/i.test(s),
        )
        .slice(-300),
      dialogs: [],
      taskScalars: [],
      menus: [],
      api: [],
    };
    for (const dialog of document.querySelectorAll(
      '[role="dialog"],[data-radix-dialog-content],form',
    )) {
      const rect = dialog.getBoundingClientRect(),
        style = getComputedStyle(dialog);
      if (
        rect.width <= 0 ||
        rect.height <= 0 ||
        style.display === "none" ||
        style.visibility === "hidden"
      )
        continue;
      result.dialogs.push({
        tag: dialog.tagName.toLowerCase(),
        role: dialog.getAttribute("role"),
        attrs: attrs(dialog),
        controls: [
          ...dialog.querySelectorAll(
            "input,textarea,select,button,[role],[data-state],[data-radix-collection-item]",
          ),
        ]
          .slice(0, 500)
          .map(describe),
      });
    }
    result.menus = [
      ...document.querySelectorAll(
        '[role="menu"],[role="menuitem"],[role="menuitemradio"],[role="menuitemcheckbox"],[role="option"],[role="listbox"]',
      ),
    ].map((node) => {
      const { x, y, width, height } = node.getBoundingClientRect();
      return {
        ...describe(node),
        id: node.id || null,
        rect: { x, y, width, height },
      };
    });
    const scalarKey =
      /^(id|task_?id|automation_id|title|enabled|is_?enabled|status|state|time_?zone|default_timezone|dtstart|rrule|recurrence|schedule|frequency|next_?run(?:_?at)?|last_?run(?:_?at|_time)?|next_execution(?:_at)?|last_execution(?:_at)?|model(?:_slug)?|effort|reasoning(?:_effort)?|conversation_?id|prompt_?hash|created_at|updated_at|is_recurring|finished|paused)$/i;
    const scheduleValue =
      /RRULE:|DTSTART|FREQ=|(?:Africa|America|Antarctica|Asia|Atlantic|Australia|Europe|Indian|Pacific)\//i;
    const roots = editorRoots();
    for (const root of roots)
      walk(
        root,
        (value, path) => {
          if (result.taskScalars.length >= 1000) return true;
          if (
            ["string", "number", "boolean"].includes(typeof value) &&
            !prose.test(path.split(".").at(-1)) &&
            (scalarKey.test(path.split(".").at(-1)) ||
              scheduleValue.test(String(value)))
          )
            result.taskScalars.push({
              path: `react.${path}`,
              value: redact(value),
            });
        },
        10,
      );
    let id = String(
      input.stable_id || input.taskId || expected.stable_id || "",
    );
    let task = roots.map((root) => findTask(root, id)).find(Boolean);
    if (!id && task) id = taskID(task);
    const endpoints = [
      ...(id ? [`/backend-api/automation/${encodeURIComponent(id)}`] : []),
      "/backend-api/automations?filter=scheduled",
    ];
    for (const endpoint of endpoints) {
      try {
        const response = await fetch(endpoint, {
            credentials: "same-origin",
            cache: "no-store",
          }),
          text = await response.text();
        let parsed;
        try {
          parsed = JSON.parse(text);
        } catch {}
        const scalars = [];
        if (parsed) {
          task ||= findTask(parsed, id);
          const pending = [];
          walk(
            parsed,
            (value, path) => {
              if (scalars.length + pending.length >= 2000) return true;
              const key = path.split(".").at(-1);
              if (["string", "number", "boolean"].includes(typeof value)) {
                if (prose.test(key))
                  pending.push(
                    digest(value).then((d) =>
                      scalars.push({ path, value: "<redacted>", ...d }),
                    ),
                  );
                else if (
                  scalarKey.test(key) ||
                  scheduleValue.test(String(value))
                )
                  scalars.push({ path, value: redact(value) });
              }
            },
            10,
          );
          await Promise.all(pending);
        }
        result.api.push({
          endpoint,
          status: response.status,
          ok: response.ok,
          responseBytes: new TextEncoder().encode(text).length,
          topLevelKeys:
            parsed && typeof parsed === "object" && !Array.isArray(parsed)
              ? Object.keys(parsed).slice(0, 200)
              : null,
          safeScalars: scalars,
        });
      } catch (error) {
        result.api.push({ endpoint, error: String(error?.message || error) });
      }
    }
    return {
      ...result,
      automationObjectFound: !!task,
      automationObjectKeys: task ? Object.keys(task) : [],
      automationObject: task ? await sanitize(task) : null,
    };
  }
  throw new Error(`Unknown task page operation: ${operation}`);
}
