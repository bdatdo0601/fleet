import assert from "node:assert/strict";
import test from "node:test";

import {
  closeModal,
  confirmDialog,
  el,
  formatBytes,
  formatMessageTarget,
  isWorkerOnline,
  openModal,
  state,
} from "./web/lib.js";
import {
  parseMarkdownBlocks,
  renderMarkdownDOM,
  sanitizeLinkHref,
  tokenizeInlineMarkdown,
} from "./web/chat_markdown.js";
import { buildMCPOAuthRedirectURI, parseMCPOAuthPaste, renderMCPOAuthCard, startMCPOAuth } from "./web/chat_oauth.js";
import { classifyWidgetAction } from "./web/chat.js";

function createMockDOM() {
  class FakeClassList {
    constructor(node) {
      this._node = node;
      this._set = new Set();
    }
    _syncFrom(cls) {
      this._set = new Set(String(cls || "").split(/\s+/).filter(Boolean));
    }
    add(...tokens) {
      for (const t of tokens) this._set.add(t);
    }
    remove(...tokens) {
      for (const t of tokens) this._set.delete(t);
    }
    toggle(t, force) {
      if (force === true) {
        this._set.add(t);
        return true;
      }
      if (force === false) {
        this._set.delete(t);
        return false;
      }
      if (this._set.has(t)) {
        this._set.delete(t);
        return false;
      }
      this._set.add(t);
      return true;
    }
    contains(t) {
      return this._set.has(t);
    }
    toString() {
      return [...this._set].join(" ");
    }
  }

  class FakeNode {
    constructor(nodeType, tagName = "") {
      this.nodeType = nodeType;
      this.tagName = tagName;
      this.children = [];
      this.parentNode = null;
      this.attributes = new Map();
      this.style = {};
      this.dataset = {};
      this.listeners = new Map();
      this.classList = new FakeClassList(this);
      this._text = "";
      this.value = "";
      this.checked = false;
      this.disabled = false;
      this.focusCalls = 0;
      this.isConnected = true;
    }
    set className(val) {
      this.classList._syncFrom(val);
    }
    get className() {
      return this.classList.toString();
    }
    set textContent(val) {
      this._text = String(val ?? "");
      this.children = [];
    }
    get textContent() {
      if (this.children.length === 0) return this._text;
      return this.children.map((c) => c.textContent).join("");
    }
    get firstChild() {
      return this.children[0] || null;
    }
    appendChild(child) {
      if (child.nodeType === 11) {
        for (const nested of [...child.children]) {
          this.appendChild(nested);
        }
        child.children = [];
        return child;
      }
      if (child.parentNode) {
        child.parentNode.removeChild(child);
      }
      child.parentNode = this;
      this.children.push(child);
      return child;
    }
    append(...nodes) {
      for (const n of nodes) this.appendChild(n);
    }
    removeChild(child) {
      const idx = this.children.indexOf(child);
      if (idx >= 0) {
        this.children.splice(idx, 1);
        child.parentNode = null;
      }
      return child;
    }
    setAttribute(k, v) {
      this.attributes.set(k, String(v));
      if (k === "id") {
        doc._byId.set(String(v), this);
      }
    }
    getAttribute(k) {
      return this.attributes.has(k) ? this.attributes.get(k) : null;
    }
    addEventListener(type, fn) {
      if (!this.listeners.has(type)) this.listeners.set(type, []);
      this.listeners.get(type).push(fn);
    }
    click() {
      for (const fn of this.listeners.get("click") || []) {
        fn({ currentTarget: this, target: this, stopPropagation() {}, preventDefault() {} });
      }
    }
    focus() {
      this.focusCalls++;
      doc.activeElement = this;
    }
    querySelector(sel) {
      // Minimal support for openModal focusTarget lookup.
      if (sel.includes("button")) {
        const queue = [...this.children];
        while (queue.length > 0) {
          const cur = queue.shift();
          if (cur.tagName && ["INPUT", "TEXTAREA", "SELECT", "BUTTON"].includes(cur.tagName) && !cur.disabled) {
            return cur;
          }
          queue.push(...cur.children);
        }
      }
      return null;
    }
    querySelectorAll() {
      return [];
    }
  }

  const doc = {
    _byId: new Map(),
    activeElement: null,
    createElement(tag) {
      return new FakeNode(1, tag.toUpperCase());
    },
    createTextNode(text) {
      const n = new FakeNode(3);
      n.textContent = text;
      return n;
    },
    createDocumentFragment() {
      return new FakeNode(11);
    },
    getElementById(id) {
      return this._byId.get(id) || null;
    },
  };

  return { doc, FakeNode };
}

async function withMockDOM(fn) {
  const prevDoc = globalThis.document;
  const prevNode = globalThis.Node;
  const prevHTMLElement = globalThis.HTMLElement;
  const prevRAF = globalThis.requestAnimationFrame;
  const { doc, FakeNode } = createMockDOM();
  globalThis.document = doc;
  globalThis.Node = FakeNode;
  globalThis.HTMLElement = FakeNode;
  globalThis.requestAnimationFrame = (cb) => cb();
  try {
    return await fn(doc, FakeNode);
  } finally {
    globalThis.document = prevDoc;
    globalThis.Node = prevNode;
    globalThis.HTMLElement = prevHTMLElement;
    globalThis.requestAnimationFrame = prevRAF;
  }
}

test("sanitizeLinkHref allows http/https and #fragments, blocks dangerous schemes", () => {
  assert.equal(sanitizeLinkHref("https://example.com/path?a=1"), "https://example.com/path?a=1");
  assert.equal(sanitizeLinkHref("http://127.0.0.1:8080/ok"), "http://127.0.0.1:8080/ok");
  assert.equal(sanitizeLinkHref("#memory/proposals"), "#memory/proposals");
  assert.equal(sanitizeLinkHref("javascript:alert(1)"), "");
  assert.equal(sanitizeLinkHref("JAVASCRIPT:alert(1)"), "");
  assert.equal(sanitizeLinkHref("data:text/html,<script>alert(1)</script>"), "");
  assert.equal(sanitizeLinkHref("vbscript:msgbox(1)"), "");
  assert.equal(sanitizeLinkHref(""), "");
});

test("tokenizeInlineMarkdown and parseMarkdownBlocks render safely", async () => {
  const tokens = tokenizeInlineMarkdown("See `code` and **bold** and [ok](https://example.com) and [bad](javascript:evil)");
  assert.deepEqual(tokens, [
    { type: "text", text: "See " },
    { type: "code", text: "code" },
    { type: "text", text: " and " },
    { type: "strong", text: "bold" },
    { type: "text", text: " and " },
    { type: "link", text: "ok", href: "https://example.com/", external: true },
    { type: "text", text: " and " },
    { type: "text", text: "[bad](javascript:evil)" },
  ]);

  const blocks = parseMarkdownBlocks("# Title\n\n- one\n- two\n\n```js\nconst x = 1;\n```");
  assert.deepEqual(blocks, [
    { type: "heading", text: "Title" },
    { type: "list", items: ["one", "two"] },
    { type: "code", text: "const x = 1;" },
  ]);

  await withMockDOM(() => {
    const dom = renderMarkdownDOM("Click [safe](#agents) or [xss](javascript:evil)");
    const links = [];
    const walk = (n) => {
      if (n.tagName === "A") links.push(n);
      for (const c of n.children || []) walk(c);
    };
    walk(dom);
    assert.equal(links.length, 1);
    assert.equal(links[0].getAttribute("href"), "#agents");
  });
});

test("el() applies plain-object style via CSSOM and never sets inline style attribute", async () => {
  await withMockDOM(() => {
    const bar = el("div", { className: "usage-bar-fill", style: { width: "42%", opacity: "0.9" } });
    assert.equal(bar.style.width, "42%");
    assert.equal(bar.style.opacity, "0.9");
    assert.equal(bar.getAttribute("style"), null);

    const ignoredStrStyle = el("div", { style: "display:none" });
    assert.equal(ignoredStrStyle.getAttribute("style"), null);
  });
});

test("formatBytes, formatMessageTarget, and isWorkerOnline behave consistently", () => {
  assert.equal(formatBytes(0), "0 B");
  assert.equal(formatBytes(512), "512 B");
  assert.equal(formatBytes(1536), "1.5 KB");
  assert.equal(formatBytes(2 * 1024 * 1024), "2.00 MB");

  assert.equal(formatMessageTarget({ to_session: "s1" }), "s1");
  assert.equal(formatMessageTarget({ to_device: "mini" }), "device:mini");
  assert.equal(formatMessageTarget({ to_repo: "fleet" }), "repo:fleet");
  assert.equal(formatMessageTarget({}), "all");

  const now = Date.parse("2026-11-28T12:00:00Z");
  const runtimes = [
    { role: "worker:mini", connected: true, last_poll_at: "2026-11-28T11:59:10Z" },
    { role: "worker:old", connected: true, last_poll_at: "2026-11-28T11:55:00Z" },
    { role: "worker:off", connected: false, last_poll_at: "2026-11-28T11:59:50Z" },
  ];
  assert.equal(isWorkerOnline(runtimes, "mini", now), true);
  assert.equal(isWorkerOnline(runtimes, "old", now), false);
  assert.equal(isWorkerOnline(runtimes, "off", now), false);
  assert.equal(isWorkerOnline(runtimes, "missing", now), false);
});

test("parseMCPOAuthPaste and buildMCPOAuthRedirectURI handle URLs, errors, and codes", () => {
  assert.deepEqual(parseMCPOAuthPaste(""), { code: "" });
  assert.deepEqual(parseMCPOAuthPaste("4/0AfJohX..."), { code: "4/0AfJohX..." });
  assert.deepEqual(
    parseMCPOAuthPaste("http://127.0.0.1:8765/oauth2callback?code=abc123&state=st9"),
    { code: "abc123", state: "st9" },
  );
  assert.deepEqual(
    parseMCPOAuthPaste("https://fleet.example.com/v1/assistant/mcp/oauth/callback?error=access_denied&error_description=User%20cancelled"),
    { error: "User cancelled" },
  );

  assert.equal(
    buildMCPOAuthRedirectURI({ protocol: "https:", hostname: "fleet.example.com", origin: "https://fleet.example.com" }),
    "https://fleet.example.com/v1/assistant/mcp/oauth/callback",
  );
  assert.equal(
    buildMCPOAuthRedirectURI({ protocol: "http:", hostname: "localhost", origin: "http://localhost:7443" }),
    "http://localhost:7443/v1/assistant/mcp/oauth/callback",
  );
  assert.equal(
    buildMCPOAuthRedirectURI({ protocol: "http:", hostname: "10.213.0.1", origin: "http://10.213.0.1:7443" }),
    "http://127.0.0.1:8765/oauth2callback",
  );
  assert.equal(
    buildMCPOAuthRedirectURI({ explicitRedirectURI: "https://custom.example/cb", protocol: "http:", hostname: "10.213.0.1" }),
    "https://custom.example/cb",
  );
  assert.equal(
    buildMCPOAuthRedirectURI({ mode: "loopback", protocol: "https:", hostname: "fleet.example.com", origin: "https://fleet.example.com" }),
    "http://127.0.0.1:8765/oauth2callback",
  );
  assert.equal(
    buildMCPOAuthRedirectURI({ mode: "web", protocol: "http:", hostname: "10.213.0.1", origin: "http://10.213.0.1:7443" }),
    "http://10.213.0.1:7443/v1/assistant/mcp/oauth/callback",
  );
});

test("startMCPOAuth opens popup synchronously, clears opener, and reports popupBlocked", async () => {
  const prevWin = globalThis.window;
  const prevFetch = globalThis.fetch;
  try {
    let openedPopup = null;
    globalThis.window = {
      location: { protocol: "https:", hostname: "fleet.example.com", origin: "https://fleet.example.com" },
      open(url, target) {
        assert.equal(url, "");
        assert.equal(target, "_blank");
        openedPopup = {
          closed: false,
          opener: globalThis.window,
          location: {
            href: "",
            replace(next) {
              this.href = next;
            },
          },
          close() {
            this.closed = true;
          },
        };
        return openedPopup;
      },
    };
    globalThis.fetch = async (path) => {
      assert.equal(path, "/v1/assistant/mcp/gcal/oauth/start");
      assert.ok(openedPopup, "popup should open synchronously before await fetch finishes");
      return {
        ok: true,
        status: 200,
        statusText: "OK",
        text: async () => JSON.stringify({ authorize_url: "https://accounts.google.com/o/oauth2/v2/auth?state=st1", state: "st1" }),
      };
    };

    const res = await startMCPOAuth("gcal");
    assert.equal(res.popupBlocked, false);
    assert.equal(res.redirectURI, "https://fleet.example.com/v1/assistant/mcp/oauth/callback");
    assert.equal(openedPopup.opener, null);
    assert.equal(openedPopup.location.href, "https://accounts.google.com/o/oauth2/v2/auth?state=st1");

    // When popup is blocked (window.open returns null), startMCPOAuth sets popupBlocked: true.
    globalThis.window.open = () => null;
    const blockedRes = await startMCPOAuth("gcal", { mode: "loopback" });
    assert.equal(blockedRes.popupBlocked, true);
    assert.equal(blockedRes.redirectURI, "http://127.0.0.1:8765/oauth2callback");

    // On API error, synchronously opened blank popup is closed.
    let closedPopup = null;
    globalThis.window.open = () => {
      closedPopup = { closed: false, location: { href: "" }, close() { this.closed = true; } };
      return closedPopup;
    };
    globalThis.fetch = async () => ({
      ok: false,
      status: 400,
      statusText: "Bad Request",
      text: async () => JSON.stringify({ error: "bad config" }),
    });
    await assert.rejects(() => startMCPOAuth("gcal"), /bad config/);
    assert.equal(closedPopup.closed, true);
  } finally {
    globalThis.window = prevWin;
    globalThis.fetch = prevFetch;
  }
});

test("renderMCPOAuthCard accepts data.name alias and updates redirect URI on mode switch", async () => {
  await withMockDOM(async () => {
    const prevWin = globalThis.window;
    const prevFetch = globalThis.fetch;
    try {
      globalThis.window = {
        location: { protocol: "https:", hostname: "fleet.example.com", origin: "https://fleet.example.com" },
      };
      globalThis.fetch = undefined;

      const card = el("div", { className: "gen-native-card" });
      renderMCPOAuthCard(card, { name: "gworkspace", provider: "google", scopes: ["https://www.googleapis.com/auth/gmail.readonly"] });
      assert.equal(card.dataset.mcpOauthServer, "gworkspace");
      assert.ok(card.textContent.includes("Redirect URI: https://fleet.example.com/v1/assistant/mcp/oauth/callback"));

      const findTag = (node, tag) => {
        if (node.tagName === tag) return node;
        for (const c of node.children || []) {
          const hit = findTag(c, tag);
          if (hit) return hit;
        }
        return null;
      };
      const sel = findTag(card, "SELECT");
      assert.ok(sel, "expected redirect mode select on HTTPS origin");
      sel.value = "loopback";
      for (const fn of sel.listeners.get("change") || []) {
        fn({ target: sel });
      }
      assert.ok(card.textContent.includes("Redirect URI: http://127.0.0.1:8765/oauth2callback"));
    } finally {
      globalThis.window = prevWin;
      globalThis.fetch = prevFetch;
    }
  });
});

test("classifyWidgetAction blocks privileged directives and flags slash commands for confirmation", () => {
  assert.deepEqual(classifyWidgetAction(""), { kind: "empty" });
  assert.deepEqual(classifyWidgetAction("Check calendar"), { kind: "allow", action: "Check calendar" });
  assert.equal(classifyWidgetAction("/auto-approve delete everything").kind, "block");
  assert.equal(classifyWidgetAction("/sol /auto-approve run rm").kind, "block");
  assert.equal(classifyWidgetAction("/YOLO do stuff").kind, "block");
  assert.deepEqual(classifyWidgetAction("/sol analyze repo"), { kind: "confirm", action: "/sol analyze repo" });
  assert.deepEqual(classifyWidgetAction("/new"), { kind: "confirm", action: "/new" });
});

test("closeModal guards lastFocusedEl.focus() when stacked parent modal is restored", async () => {
  await withMockDOM(async (doc) => {
    state.modalOpen = false;

    const backdrop = el("div", { id: "modal-backdrop", className: "hidden" });
    const titleEl = el("h2", { id: "modal-title" });
    const bodyEl = el("div", { id: "modal-body" });
    const closeBtn = el("button", { id: "modal-close", type: "button", text: "Close" });
    doc._byId.set("modal-backdrop", backdrop);
    doc._byId.set("modal-title", titleEl);
    doc._byId.set("modal-body", bodyEl);
    doc._byId.set("modal-close", closeBtn);

    const triggerBtn = el("button", { type: "button", text: "Open Parent" });
    triggerBtn.focus();
    const initialFocusCalls = triggerBtn.focusCalls;

    const parentContent = el("div", null, el("input", { type: "text", value: "draft" }));
    openModal("Parent Modal", parentContent);
    assert.equal(state.modalOpen, true);

    // Open confirmDialog stacked on top of Parent Modal.
    const confirmPromise = confirmDialog("Confirm Action", "Proceed?", "Yes", true);
    assert.equal(state.modalOpen, true);
    assert.equal(titleEl.textContent, "Confirm Action");

    // Closing the modal while confirmDialog is stacked restores Parent Modal and keeps state.modalOpen === true.
    closeModal();
    const confirmed = await confirmPromise;
    assert.equal(confirmed, false);
    assert.equal(state.modalOpen, true);
    assert.equal(titleEl.textContent, "Parent Modal");
    // Must NOT have restored focus back to triggerBtn while parent modal is still open!
    assert.equal(triggerBtn.focusCalls, initialFocusCalls);

    // Now closing the parent modal should restore focus to triggerBtn once.
    closeModal();
    assert.equal(state.modalOpen, false);
    assert.equal(triggerBtn.focusCalls, initialFocusCalls + 1);
  });
});
