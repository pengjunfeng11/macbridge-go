import test from "node:test";
import assert from "node:assert/strict";
import vm from "node:vm";
import { webcrypto, createHash } from "node:crypto";
import { taskPage } from "./tasks.js";

const digest = (value) => ({
  bytes: Buffer.byteLength(value),
  sha256: createHash("sha256").update(value).digest("hex"),
});
const tick = () => new Promise((resolve) => setTimeout(resolve, 20));
function fixture(fetcher = async () => new Response("{}", { status: 200 })) {
  const sent = [];
  class FakeXHR extends EventTarget {
    open(method, url) {
      this.method = method;
      this.url = url;
    }
    send(body) {
      sent.push({ channel: "xhr", method: this.method, url: this.url, body });
      this.status = 200;
      this.responseText = '{"ok":true}';
      this.dispatchEvent(new Event("loadend"));
    }
    abort() {
      this.aborted = true;
    }
  }
  const context = vm.createContext({
    crypto: webcrypto,
    TextEncoder,
    Blob,
    URLSearchParams,
    Request,
    Response,
    Event,
    InputEvent: Event,
    EventTarget,
    XMLHttpRequest: FakeXHR,
    setTimeout,
    clearTimeout,
    requestAnimationFrame: (fn) => setTimeout(fn, 0),
    fetch: async (url, options) => {
      sent.push({ channel: "fetch", url, options });
      return fetcher(url, options);
    },
    document: {
      title: "Tasks",
      scripts: [],
      body: {},
      querySelectorAll: () => [],
      querySelector: () => null,
    },
    location: { href: "https://chatgpt.com/tasks" },
    performance: { getEntriesByType: () => [] },
    getComputedStyle: () => ({}),
  });
  context.window = context;
  const run = vm.runInContext(`(${taskPage.toString()})`, context);
  return {
    context,
    sent,
    run: (operation, args = {}) => run({ operation, ...args }),
    json: (value) => JSON.parse(JSON.stringify(value)),
  };
}
function data() {
  const prompt = "existing private instructions",
    proposedPrompt = "new private instructions\n多字节",
    schedule = "DTSTART:20260910T010000Z\nRRULE:FREQ=DAILY";
  const current = {
    id: "task-123",
    conversation_id: "conversation-1",
    title: "Daily review",
    is_enabled: true,
    executor: "chatgpt",
    default_timezone: "Asia/Shanghai",
    timing_mode: 0,
    source_conversation_is_work_mode: false,
    prompt,
    schedule: "RRULE:FREQ=WEEKLY",
    last_run_time: null,
    email_enabled: true,
    notifications_enabled: false,
  };
  const expected = {
    stable_id: current.id,
    conversation_id: current.conversation_id,
    title: current.title,
    enabled: current.is_enabled,
    executor: current.executor,
    timezone: current.default_timezone,
    timing_mode: current.timing_mode,
    source_conversation_is_work_mode: current.source_conversation_is_work_mode,
    prompt_bytes: digest(prompt).bytes,
    prompt_sha256: digest(prompt).sha256,
    schedule: current.schedule,
    last_run_time: current.last_run_time,
  };
  const proposed = {
    title: "Updated daily review",
    prompt: proposedPrompt,
    prompt_bytes: digest(proposedPrompt).bytes,
    prompt_sha256: digest(proposedPrompt).sha256,
    schedule,
    schedule_bytes: digest(schedule).bytes,
    schedule_sha256: digest(schedule).sha256,
    native_schedule: "RRULE:FREQ=DAILY",
    native_repeat_label: "Every day",
  };
  const payload = {
    jawbone_id: expected.stable_id,
    title: proposed.title,
    prompt: proposed.prompt,
    schedule: proposed.native_schedule,
    default_timezone: expected.timezone,
    is_enabled: true,
    last_run_time: expected.last_run_time,
    timing_mode: 0,
    untouched: { allowed: true },
  };
  return { current, expected, proposed, payload };
}

test("mutation probe blocks fetch and XHR, records digests, and restores both channels", async () => {
  const f = fixture();
  assert.equal(
    (await f.run("installTaskMutationProbe")).alreadyInstalled,
    false,
  );
  assert.equal(
    (await f.run("installTaskMutationProbe")).alreadyInstalled,
    true,
  );
  await f.context.fetch("/backend-api/automations?filter=scheduled");
  await assert.rejects(
    f.context.fetch("/backend-api/automations/save", {
      method: "POST",
      body: JSON.stringify({
        prompt: "private",
        token: "secret",
        title: "New task",
      }),
    }),
    /MDB_TASK_MUTATION_PROBE_BLOCKED/,
  );
  const xhr = new f.context.XMLHttpRequest();
  xhr.open("PATCH", "/scheduled/task-1");
  let errors = 0;
  xhr.addEventListener("error", () => errors++);
  xhr.send('{"instructions":"hidden"}');
  await tick();
  assert.equal(errors, 1);
  assert.equal(xhr.aborted, true);
  const read = f.json(await f.run("readTaskMutationProbe"));
  assert.equal(read.records.length, 2);
  assert.deepEqual(read.records[0].body.prompt, {
    value: "<redacted>",
    ...digest("private"),
  });
  assert.equal(read.records[0].body.token, "<redacted>");
  assert.equal(f.sent.length, 1);
  assert.equal(
    (await f.run("installExactTaskSaveRewrite")).code,
    "mutation_probe_still_installed",
  );
  assert.equal((await f.run("removeTaskMutationProbe")).removed, true);
  assert.equal((await f.run("removeTaskMutationProbe")).removed, false);
  await f.context.fetch("/backend-api/automations/save", {
    method: "POST",
    body: "{}",
  });
  xhr.open("POST", "/task");
  xhr.send("{}");
  assert.equal(f.sent.length, 3);
});

test("save rewrite validates payload, preserves request options, and consumes once across concurrent requests", async () => {
  const f = fixture(),
    d = data();
  await f.run("installExactTaskSaveRewrite", d);
  assert.equal(
    (await f.run("installTaskMutationProbe")).code,
    "save_rewrite_still_installed",
  );
  await assert.rejects(
    f.context.fetch("/backend-api/automations/save", {
      method: "POST",
      body: JSON.stringify({ ...d.payload, jawbone_id: "wrong" }),
    }),
    /native_payload_mismatch/,
  );
  const request = new Request(
    "https://chatgpt.com/backend-api/automations/save",
    {
      method: "POST",
      headers: { "X-Fixture": "preserved" },
      body: JSON.stringify(d.payload),
    },
  );
  const outcomes = await Promise.allSettled([
    f.context.fetch(request),
    f.context.fetch(request.clone()),
  ]);
  assert.equal(outcomes.filter((o) => o.status === "fulfilled").length, 1);
  assert.equal(f.sent.length, 1);
  const sent = f.sent[0].url;
  assert.ok(sent instanceof Request);
  assert.equal(sent.headers.get("X-Fixture"), "preserved");
  const actual = await sent.json();
  assert.equal(actual.schedule, d.proposed.schedule);
  assert.deepEqual(actual.untouched, { allowed: true });
  await tick();
  const read = f.json(await f.run("readExactTaskSaveRewrite"));
  assert.equal(read.submission_seen, true);
  assert.equal(read.records.filter((r) => r.submitted).length, 1);
  assert.equal(read.records.find((r) => r.submitted).response.status, 200);
  assert.equal(read.records[0].failed_checks.includes("stable_id"), true);
  assert.equal((await f.run("removeExactTaskSaveRewrite")).removed, true);
  await f.context.fetch("/backend-api/automations/save", {
    method: "POST",
    body: "{}",
  });
  assert.equal(f.sent.length, 2);
});

test("XHR rewrite sends exact schedule and captures response once", async () => {
  const f = fixture(),
    d = data();
  await f.run("installExactTaskSaveRewrite", d);
  const xhr = new f.context.XMLHttpRequest();
  xhr.open("POST", "/backend-api/automations/save?x=1");
  xhr.send(JSON.stringify(d.payload));
  await tick();
  assert.equal(f.sent.length, 1);
  assert.equal(JSON.parse(f.sent[0].body).schedule, d.proposed.schedule);
  const read = f.json(await f.run("readExactTaskSaveRewrite"));
  assert.deepEqual(read.records[0].response.digest, digest('{"ok":true}'));
});

test("removing rewrite while request digest is pending prevents a late send", async () => {
  const f = fixture(),
    d = data();
  await f.run("installExactTaskSaveRewrite", d);
  const pending = f.context.fetch("/backend-api/automations/save", {
    method: "POST",
    body: JSON.stringify(d.payload),
  });
  await f.run("removeExactTaskSaveRewrite");
  await assert.rejects(pending, /rewrite_removed/);
  assert.equal(f.sent.length, 0);
});

test("exact save refuses drift or incorrect digest before submitting and preserves notification flags", async () => {
  const d = data();
  const f = fixture(
    async (url) =>
      new Response(
        JSON.stringify(
          String(url).includes("?filter")
            ? { tasks: [d.current] }
            : { prompt: d.proposed.prompt, token: "redact", saved: true },
        ),
      ),
  );
  const drift = await f.run("exactTaskSave", {
    ...d,
    expected: { ...d.expected, title: "stale" },
  });
  assert.equal(drift.code, "identity_drift");
  assert.equal(f.sent.length, 1);
  const mismatch = await f.run("exactTaskSave", {
    ...d,
    proposed: { ...d.proposed, schedule_sha256: "wrong" },
  });
  assert.equal(mismatch.code, "proposed_schedule_digest_mismatch");
  assert.equal(f.sent.length, 2);
  const result = f.json(await f.run("exactTaskSave", d));
  assert.equal(result.submitted, true);
  assert.equal(result.reconciled, false);
  assert.equal(result.uncertain, false);
  assert.equal(result.response_ok, true);
  const body = JSON.parse(f.sent.at(-1).options.body);
  assert.equal(body.email_enabled, true);
  assert.equal(body.notifications_enabled, false);
  assert.equal(body.schedule, d.proposed.schedule);
  assert.equal(result.response.token, "<redacted>");
  assert.deepEqual(result.response.prompt, {
    value: "<redacted>",
    ...digest(d.proposed.prompt),
  });
});

test("uncertain save transport does not retry and returns request evidence", async () => {
  const d = data();
  const f = fixture(async (_url, opts) => {
    if (opts?.method === "POST") throw new Error("connection lost");
    return new Response(JSON.stringify([d.current]));
  });
  const result = await f.run("exactTaskSave", d);
  assert.equal(result.code, "save_transport_uncertain");
  assert.equal(result.uncertain, true);
  assert.equal(result.submitted, true);
  assert.ok(result.request_digest.sha256);
  assert.equal(f.sent.length, 2);
});

test("task audit reads dynamic stable IDs, hashes prompts, and handles cyclic React state", async () => {
  const d = data();
  const f = fixture(
    async () => new Response(JSON.stringify({ automations: [d.current] })),
  );
  f.context.document.body.__reactProps$test = { current: d.current };
  f.context.document.body.__reactProps$test.loop =
    f.context.document.body.__reactProps$test;
  const result = f.json(await f.run("taskAudit", { stable_id: d.current.id }));
  assert.equal(result.automationObjectFound, true);
  assert.equal(f.sent[0].url, `/backend-api/automation/${d.current.id}`);
  assert.deepEqual(result.automationObject.prompt, {
    value: "<redacted>",
    ...digest(d.current.prompt),
  });
  assert.ok(
    result.api[1].safeScalars.some(
      (s) =>
        s.path.endsWith(".prompt") &&
        s.sha256 === digest(d.current.prompt).sha256,
    ),
  );
  assert.equal(JSON.stringify(result).includes(d.current.prompt), false);
});

test("editor preparation verifies identity, updates controlled fields, and never clicks Save", async () => {
  const d = data(),
    f = fixture();
  class Textarea extends EventTarget {
    set value(value) {
      this.text = String(value);
    }
    get value() {
      return this.text || "";
    }
  }
  const title = new Textarea(),
    prompt = new Textarea();
  const repeat = { textContent: "Every day" },
    end = { textContent: "Never" },
    save = {
      textContent: "Save",
      disabled: false,
      click: () => assert.fail("preparation submitted"),
    };
  title.__reactProps$fixture = { task: d.current };
  f.context.HTMLTextAreaElement = Textarea;
  f.context.document.querySelector = (selector) =>
    selector.includes('"Title"')
      ? title
      : selector.includes('"Instructions"')
        ? prompt
        : selector.includes('"End repeat"')
          ? end
          : selector.includes('"Repeat"')
            ? repeat
            : null;
  f.context.document.querySelectorAll = (selector) =>
    selector === "button"
      ? [repeat, end, save]
      : selector.includes("textarea")
        ? [title, prompt, repeat, end]
        : [];
  assert.equal(
    (
      await f.run("prepareExactTaskEditor", {
        ...d,
        expected: { ...d.expected, title: "stale" },
      })
    ).code,
    "identity_drift",
  );
  assert.equal(title.value, "");
  const result = await f.run("prepareExactTaskEditor", d);
  assert.equal(result.prepared, true);
  assert.equal(title.value, d.proposed.title);
  assert.equal(prompt.value, d.proposed.prompt);
  assert.equal(f.sent.length, 0);
});
