// Standalone function: Chrome serializes this function into the selected page.
export async function pageOperation(operation, input) {
  const fail = (code, message) => {
    throw Object.assign(new Error(message), { code });
  };
  const visible = (element) => {
    const r = element.getBoundingClientRect(),
      s = getComputedStyle(element);
    return (
      r.width > 0 &&
      r.height > 0 &&
      s.display !== "none" &&
      s.visibility !== "hidden"
    );
  };
  const selector = (element) => {
    if (
      element.id &&
      document.querySelectorAll(`#${CSS.escape(element.id)}`).length === 1
    )
      return `#${CSS.escape(element.id)}`;
    const path = [];
    for (
      let node = element;
      node && path.length < 32;
      node = node.parentElement
    ) {
      let part = node.tagName.toLowerCase();
      if (node.parentElement) {
        const siblings = [...node.parentElement.children].filter(
          (n) => n.tagName === node.tagName,
        );
        if (siblings.length > 1)
          part += `:nth-of-type(${siblings.indexOf(node) + 1})`;
      }
      path.unshift(part);
    }
    return path.join(" > ");
  };
  if (operation === "snapshot") {
    const maxText = input.maxTextChars ?? 50000,
      maxElements = input.maxElements ?? 200;
    const text = document.body?.innerText || "";
    const candidates = document.querySelectorAll(
      'a[href],button,input,textarea,select,[contenteditable="true"],[role="button"],[role="link"],[role="textbox"],[role="checkbox"],[role="radio"],[role="menuitem"],[role="option"],[role="menuitemcheckbox"],[role="menuitemradio"]',
    );
    const elements = [];
    let more = false;
    for (const e of candidates) {
      if (!visible(e)) continue;
      if (elements.length >= maxElements) {
        more = true;
        break;
      }
      const value =
        "value" in e
          ? e.type === "password"
            ? "<redacted>"
            : String(e.value).slice(0, 20000)
          : null;
      elements.push({
        selector: selector(e),
        tag: e.tagName.toLowerCase(),
        type: e.type || null,
        role: e.getAttribute("role"),
        text: (e.innerText || e.textContent || "").trim().slice(0, 500),
        ariaLabel: e.getAttribute("aria-label"),
        ariaExpanded: e.getAttribute("aria-expanded"),
        ariaHasPopup: e.getAttribute("aria-haspopup"),
        ariaControls: e.getAttribute("aria-controls"),
        dataState: e.getAttribute("data-state"),
        name: e.getAttribute("name"),
        placeholder: e.getAttribute("placeholder"),
        href: e.href || null,
        disabled: !!e.disabled,
        checked: "checked" in e ? !!e.checked : null,
        required: "required" in e ? !!e.required : null,
        valid: e.validity?.valid ?? null,
        selectedOptionText:
          e.selectedOptions?.[0]?.textContent?.trim().slice(0, 500) ?? null,
        value,
      });
    }
    return {
      title: document.title,
      url: location.href,
      bodyText: text.slice(0, maxText),
      bodyTextTruncated: text.length > maxText,
      elements,
      elementsTruncated: more,
    };
  }
  let matches;
  try {
    matches = [...document.querySelectorAll(input.selector)];
  } catch {
    fail("CHROME_SELECTOR_INVALID", "Invalid CSS selector");
  }
  if (!matches.length)
    fail("CHROME_ELEMENT_NOT_FOUND", "No element matches the selector");
  const disabled = (e) =>
    !!e.disabled || e.getAttribute("aria-disabled") === "true";
  const fillable = matches.filter(
    (e) =>
      e.isContentEditable ||
      e instanceof HTMLInputElement ||
      e instanceof HTMLTextAreaElement ||
      e instanceof HTMLSelectElement,
  );
  const usable = fillable.filter(
    (e) => visible(e) && getComputedStyle(e).opacity !== "0",
  );
  const element =
    operation === "fill" ? usable.find((e) => !disabled(e)) : matches[0];
  if (!element)
    fail(
      !fillable.length
        ? "CHROME_ELEMENT_NOT_FILLABLE"
        : !usable.length
          ? "CHROME_ELEMENT_NOT_VISIBLE"
          : "CHROME_ELEMENT_DISABLED",
      "No visible, enabled, fillable match",
    );
  if (disabled(element)) fail("CHROME_ELEMENT_DISABLED", "Element is disabled");
  if (operation === "click")
    element.scrollIntoView?.({
      block: "center",
      inline: "center",
      behavior: "instant",
    });
  if (!visible(element))
    fail("CHROME_ELEMENT_NOT_VISIBLE", "Element is not visible");
  if (element.type === "file")
    fail(
      "CHROME_FOREGROUND_REQUIRED",
      "File inputs require a real user gesture",
    );
  if (operation === "click") {
    const popupCount = () =>
      [
        ...document.querySelectorAll(
          '[role="menu"],[role="listbox"],[role="dialog"],[data-state="open"]',
        ),
      ].filter(visible).length;
    const state = () => ({
      ariaExpanded: element.getAttribute("aria-expanded"),
      ariaHasPopup: element.getAttribute("aria-haspopup"),
      dataState: element.getAttribute("data-state"),
      visiblePopupCount: popupCount(),
    });
    const before = state(),
      box = element.getBoundingClientRect(),
      view = globalThis.window || globalThis;
    const clientX = Math.max(
      0,
      Math.min(
        (view.innerWidth || box.width) - 1,
        (box.left || 0) + box.width / 2,
      ),
    );
    const clientY = Math.max(
      0,
      Math.min(
        (view.innerHeight || box.height) - 1,
        (box.top || 0) + box.height / 2,
      ),
    );
    const common = {
      bubbles: true,
      cancelable: true,
      composed: true,
      clientX,
      clientY,
      screenX: (view.screenX || 0) + clientX,
      screenY: (view.screenY || 0) + clientY,
      button: 0,
    };
    const events = [];
    const pointer = (type, buttons) => {
      if (typeof PointerEvent !== "function") return true;
      events.push(type);
      return element.dispatchEvent(
        new PointerEvent(type, {
          ...common,
          buttons,
          pointerId: 1,
          pointerType: "mouse",
          isPrimary: true,
          pressure: buttons ? 0.5 : 0,
        }),
      );
    };
    const mouse = (type, buttons) => {
      events.push(type);
      return element.dispatchEvent(
        new MouseEvent(type, {
          ...common,
          buttons,
          detail: type === "mousedown" ? 1 : 0,
        }),
      );
    };
    pointer("pointerover", 0);
    mouse("mouseover", 0);
    pointer("pointermove", 0);
    mouse("mousemove", 0);
    pointer("pointerdown", 1);
    const mouseDownAllowed = mouse("mousedown", 1);
    if (mouseDownAllowed) element.focus?.({ preventScroll: true });
    await new Promise((resolve) => setTimeout(resolve, 0));
    const afterMouseDown = state(),
      changed = JSON.stringify(before) !== JSON.stringify(afterMouseDown);
    const semantic = !!(
      element.matches?.('[role="combobox"],[aria-haspopup]') ||
      element.closest('[role="combobox"],[aria-haspopup]')
    );
    const activated =
      changed || (!mouseDownAllowed && semantic) || !element.isConnected;
    pointer("pointerup", 0);
    mouse("mouseup", 0);
    let activation = "mousedown";
    if (!activated && element.isConnected) {
      events.push("click");
      element.click();
      activation = "click";
    }
    await new Promise((resolve) => setTimeout(resolve, 0));
    let after = state(),
      keyboardFallbackUsed = false;
    if (
      semantic &&
      after.ariaExpanded !== "true" &&
      after.dataState !== "open" &&
      !after.visiblePopupCount
    ) {
      element.focus?.({ preventScroll: true });
      for (const type of ["keydown", "keyup"]) {
        events.push(`${type}:ArrowDown`);
        element.dispatchEvent(
          new KeyboardEvent(type, {
            bubbles: true,
            cancelable: true,
            composed: true,
            key: "ArrowDown",
            code: "ArrowDown",
          }),
        );
      }
      keyboardFallbackUsed = true;
      await new Promise((resolve) => setTimeout(resolve, 0));
      after = state();
      if (
        after.ariaExpanded === "true" ||
        after.dataState === "open" ||
        after.visiblePopupCount
      )
        activation = "keyboard-arrowdown";
    }
    return {
      clicked: true,
      selector: input.selector,
      strategy: "adaptive-pointer-mouse-sequence",
      activation,
      trusted: false,
      synthetic: true,
      mouseDownAllowed,
      semanticMouseDownControl: semantic,
      keyboardFallbackUsed,
      stateChangedOnMouseDown: changed,
      before,
      afterMouseDown,
      after,
      clientX,
      clientY,
      events,
      title: document.title,
      url: location.href,
    };
  }
  if (operation !== "fill")
    fail("CHROME_UNKNOWN_METHOD", "Unknown page operation");
  const value = String(input.value);
  let wanted = value;
  const notify = () => {
    element.dispatchEvent(
      new InputEvent("input", {
        bubbles: true,
        composed: true,
        inputType: "insertText",
        data: value,
      }),
    );
    element.dispatchEvent(new Event("change", { bubbles: true }));
  };
  const settle = () =>
    new Promise((resolve) => {
      const timer = setTimeout(resolve, 250);
      try {
        requestAnimationFrame(() =>
          requestAnimationFrame(() => {
            clearTimeout(timer);
            resolve();
          }),
        );
      } catch {
        clearTimeout(timer);
        resolve();
      }
    });
  if (element.isContentEditable) {
    element.focus?.({ preventScroll: true });
    let inserted = false;
    try {
      const selection = window.getSelection(),
        range = document.createRange();
      range.selectNodeContents(element);
      selection.removeAllRanges();
      selection.addRange(range);
      inserted = !!document.execCommand("insertText", false, value);
    } catch {}
    if (!inserted) element.textContent = value;
    notify();
  } else if (element instanceof HTMLSelectElement) {
    const option =
      [...element.options].find((o) => o.value === value) ||
      [...element.options].find(
        (o) =>
          String(o.textContent || "")
            .trim()
            .toLowerCase() === value.trim().toLowerCase(),
      );
    if (!option)
      fail(
        "CHROME_SELECT_OPTION_NOT_FOUND",
        "No option matches that value or label",
      );
    wanted = option.value;
    element.value = wanted;
    notify();
  } else {
    element.focus?.({ preventScroll: true });
    element.select?.();
    const set = () => {
      const prototype =
        element instanceof HTMLTextAreaElement
          ? HTMLTextAreaElement.prototype
          : HTMLInputElement.prototype;
      const setter = Object.getOwnPropertyDescriptor(prototype, "value")?.set;
      if (setter) setter.call(element, value);
      else element.value = value;
      notify();
    };
    let inserted = false;
    try {
      inserted = !!document.execCommand("insertText", false, value);
    } catch {}
    if (!inserted || element.value !== value) set();
    await settle();
    if (element.value !== value) {
      set();
      await settle();
    }
    if (element.value !== value)
      fail(
        "CHROME_FILL_NOT_STICKY",
        "The page did not retain the filled value",
      );
  }
  let submitStrategy = null,
    submitterTag = null,
    submitterType = null;
  const form = element.form || element.closest("form");
  if (input.submit) {
    const button = form
      ? [
          ...form.querySelectorAll(
            'button:not([type]),button[type="submit"],input[type="submit"]',
          ),
        ].find((e) => visible(e) && !disabled(e))
      : null;
    submitterTag = button?.tagName.toLowerCase() || null;
    submitterType =
      button?.getAttribute("type") ||
      (submitterTag === "button" ? "submit" : null);
    if (form?.requestSubmit) {
      if (button) {
        form.requestSubmit(button);
        submitStrategy = "requestSubmit:visible-submitter";
      } else {
        form.requestSubmit();
        submitStrategy = "requestSubmit";
      }
    } else if (button?.click) {
      button.click();
      submitStrategy = "submitter.click";
    } else {
      element.dispatchEvent(
        new KeyboardEvent("keydown", {
          key: "Enter",
          code: "Enter",
          bubbles: true,
        }),
      );
      submitStrategy = "keyboard-enter";
    }
  }
  return {
    filled: true,
    submitted: !!input.submit,
    submitStrategy,
    submitterTag,
    submitterType,
    selector: input.selector,
    matchCount: matches.length,
    fillableMatchCount: fillable.length,
    selectedMatchIndex: matches.indexOf(element),
    selectedVisible: true,
    selectedDisabled: false,
    selectedTag: element.tagName.toLowerCase(),
    formAction: form?.action || null,
    formMethod: form?.method?.toUpperCase() || null,
    value:
      element.type === "password"
        ? "<redacted>"
        : element.isContentEditable
          ? element.textContent
          : wanted,
    title: document.title,
    url: location.href,
  };
}
