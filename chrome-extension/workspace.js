export const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
export function fault(code, message) {
  return Object.assign(new Error(message), { code });
}
export function permitted(url, patterns) {
  let parsed;
  try {
    parsed = new URL(url);
  } catch {
    return false;
  }
  if (
    !["http:", "https:"].includes(parsed.protocol) ||
    parsed.username ||
    parsed.password
  )
    return false;
  if (typeof URLPattern === "function")
    return patterns.some((pattern) => {
      try {
        return new URLPattern(pattern).test(url);
      } catch {
        throw fault(
          "CHROME_INVALID_URL_PATTERN",
          "Invalid approved URL pattern",
        );
      }
    });
  return patterns.some((pattern) => {
    if (typeof pattern !== "string") return false;
    const match = /^(https?|\*):\/\/([^/]+)(\/.*)$/.exec(pattern);
    if (!match || (match[1] !== "*" && `${match[1]}:` !== parsed.protocol))
      return false;
    const host = match[2];
    const hostOK =
      host === "*" ||
      (host.endsWith(":*") &&
        (host === "*:*" || parsed.hostname === host.slice(0, -2))) ||
      (host.startsWith("*.") &&
        !parsed.port &&
        parsed.hostname.endsWith(host.slice(1))) ||
      parsed.host === host;
    if (!hostOK) return false;
    return new RegExp(
      `^${match[3]
        .split("*")
        .map((p) => p.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"))
        .join(".*")}$`,
    ).test(parsed.pathname + parsed.search);
  });
}
export function approve(url, patterns) {
  if (!permitted(url, patterns))
    throw fault(
      "CHROME_URL_NOT_APPROVED",
      `URL is outside the active browser grant: ${url}`,
    );
}
export function tabInfo(tab) {
  return {
    tabId: tab.id,
    windowId: tab.windowId,
    groupId: tab.groupId ?? null,
    active: !!tab.active,
    pinned: !!tab.pinned,
    url: tab.url || "",
    title: tab.title || "",
    status: tab.status || null,
  };
}

export class Workspace {
  constructor(api) {
    this.api = api;
    this.queue = Promise.resolve();
    this.key = "macbridgeWorkspace";
    this.idle = api.runtime.getURL("workspace.html");
  }
  transaction(fn) {
    const task = this.queue.then(fn);
    this.queue = task.catch(() => {});
    return task;
  }
  async load() {
    const state = (await this.api.storage.local.get(this.key))[this.key] || {
      desired: 8,
      tabs: [],
    };
    state.desired = Math.min(32, Math.max(8, state.desired || 8));
    state.tabs ||= [];
    let group =
      state.groupId === undefined
        ? null
        : await this.api.tabGroups.get(state.groupId).catch(() => null);
    if (!group) {
      group = (await this.api.tabGroups.query({ title: "MDB" })).find(
        (g) => g.title === "MDB",
      );
      if (group) {
        state.groupId = group.id;
        state.windowId = group.windowId;
        const tabs = await this.api.tabs.query({ groupId: group.id });
        state.tabs = tabs.map((t) => ({
          id: t.id,
          leasedAt: t.url === this.idle ? null : Date.now(),
        }));
      } else {
        delete state.groupId;
        delete state.windowId;
        state.tabs = [];
      }
    }
    const live = [];
    for (const entry of state.tabs) {
      const tab = await this.api.tabs.get(entry.id).catch(() => null);
      if (!tab || tab.groupId !== state.groupId) continue;
      if (
        entry.leasedAt &&
        Date.now() - entry.leasedAt > 600000 &&
        !tab.active
      ) {
        await this.api.tabs.update(tab.id, { url: this.idle });
        entry.leasedAt = null;
      }
      live.push(entry);
    }
    state.tabs = live;
    state.desired = Math.max(state.desired, live.length);
    return state;
  }
  async save(state) {
    await this.api.storage.local.set({ [this.key]: state });
  }
  describe(state) {
    const leased = state.tabs.filter((t) => t.leasedAt).length;
    return {
      initialized: state.groupId !== undefined,
      groupId: state.groupId ?? null,
      windowId: state.windowId ?? null,
      groupTitle: "MDB",
      poolSize: state.tabs.length,
      currentPoolSize: state.tabs.length,
      targetPoolSize: state.desired,
      maxPoolSize: 32,
      leasedTabs: leased,
      idleTabs: state.tabs.length - leased,
      pendingExpansion: Math.max(0, state.desired - state.tabs.length),
      tabs: state.tabs.map((t) => ({
        tabId: t.id,
        leased: !!t.leasedAt,
        lastUsedAt: t.leasedAt,
      })),
    };
  }
  status() {
    return this.transaction(async () => {
      const state = await this.load();
      await this.save(state);
      return this.describe(state);
    });
  }
  async grow(state) {
    const window =
      state.windowId === undefined
        ? await this.api.windows
            .getLastFocused({ windowTypes: ["normal"] })
            .catch(() => null)
        : await this.api.windows.get(state.windowId).catch(() => null);
    if (!window?.focused) return;
    for (let i = state.tabs.length; i < state.desired; i++) {
      if (!(await this.api.windows.get(window.id)).focused) break;
      const tab = await this.api.tabs.create({
        windowId: window.id,
        url: this.idle,
        active: false,
      });
      try {
        const groupId = await this.api.tabs.group(
          state.groupId === undefined
            ? { tabIds: [tab.id], createProperties: { windowId: window.id } }
            : { tabIds: [tab.id], groupId: state.groupId },
        );
        state.groupId = groupId;
        state.windowId = window.id;
        state.tabs.push({ id: tab.id, leasedAt: null });
        await this.api.tabGroups.update(groupId, {
          title: "MDB",
          color: "blue",
          collapsed: !state.tabs.some((t) => t.leasedAt),
        });
        await this.save(state);
      } catch (error) {
        await this.api.tabs.remove(tab.id).catch(() => {});
        throw error;
      }
    }
  }
  setup(size = 8) {
    if (!Number.isInteger(size) || size < 1 || size > 32)
      throw fault("INVALID_ARGUMENT", "poolSize must be 1–32");
    return this.transaction(async () => {
      const state = await this.load();
      state.desired = Math.max(state.desired, size);
      await this.save(state);
      await this.grow(state);
      return this.describe(state);
    });
  }
  async touch(id) {
    return this.transaction(async () => {
      const state = await this.load();
      const entry = state.tabs.find((t) => t.id === id);
      if (entry) {
        entry.leasedAt = Date.now();
        await this.save(state);
      }
      return !!entry;
    });
  }
  async release(id) {
    return this.transaction(async () => {
      const state = await this.load();
      const entry = state.tabs.find((t) => t.id === id);
      if (!entry) return { released: false, workspace: false, tabId: id };
      const tab = await this.api.tabs.get(id);
      await this.api.tabs.update(id, { url: this.idle });
      entry.leasedAt = null;
      await this.save(state);
      await this.api.tabGroups
        .update(state.groupId, {
          collapsed: !state.tabs.some((t) => t.leasedAt),
        })
        .catch(() => {});
      return {
        released: true,
        workspace: true,
        closed: false,
        tabId: id,
        wasActive: !!tab.active,
      };
    });
  }
  async settle(id, patterns, timeout = 15000) {
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) {
      const tab = await this.api.tabs.get(id);
      if (tab.url && tab.url !== this.idle && tab.url !== "about:blank") {
        approve(tab.url, patterns);
        if (tab.status === "complete") return tab;
      }
      await sleep(100);
    }
    throw fault(
      "CHROME_NAVIGATION_TIMEOUT",
      "Background navigation did not complete within its deadline",
    );
  }
  async open(url, patterns) {
    approve(url, patterns);
    const deadline = Date.now() + 20000;
    let tabId;
    do {
      tabId = await this.transaction(async () => {
        const state = await this.load();
        for (const entry of state.tabs) {
          if (entry.leasedAt) continue;
          const tab = await this.api.tabs.get(entry.id).catch(() => null);
          if (!tab || tab.active) continue;
          entry.leasedAt = Date.now();
          await this.save(state);
          await this.api.tabGroups
            .update(state.groupId, { collapsed: false })
            .catch(() => {});
          return entry.id;
        }
        if (state.desired <= state.tabs.length)
          state.desired = Math.min(32, Math.max(8, state.tabs.length + 4));
        await this.save(state);
        await this.grow(state);
        return null;
      });
      if (tabId !== null) break;
      await sleep(250);
    } while (Date.now() < deadline);
    if (tabId === null)
      throw fault(
        "CHROME_WORKSPACE_CAPACITY",
        "No idle MDB tab; expansion is persisted and will happen when its Chrome window is naturally focused",
      );
    try {
      await this.api.tabs.update(tabId, { url });
      const tab = await this.settle(tabId, patterns);
      return { ...tabInfo(tab), workspace: true, leased: true };
    } catch (error) {
      await this.release(tabId).catch(() => {});
      throw error;
    }
  }
}
