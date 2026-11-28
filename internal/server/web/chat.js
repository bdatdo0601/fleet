import { api, badge, clear, closeModal, confirmDialog, el, field, formatBytes, openModal, state, toast } from "./lib.js";
import { renderMarkdownDOM } from "./chat_markdown.js";
import { renderMCPOAuthCard, subscribeMCPOAuthDone } from "./chat_oauth.js";
import { openAssistantSettingsModal } from "./chat_settings.js";

const MODEL_CHOICES = [
  { value: "", label: "Channel / Default" },
  { value: "auto", label: "Auto (Decision Router)" },
  { value: "anthropic/claude-haiku-5.5", label: "Claude Haiku 5.5 (Fast Daily)" },
  { value: "thinkingmachines/inkling-2-mini", label: "Inkling 2 Mini (Ultra-Cheap)" },
  { value: "meta/muse-ember", label: "Muse Ember (Fast Agent)" },
  { value: "thinkingmachines/inkling-2-small", label: "Inkling 2 Small (Coding/Agent)" },
  { value: "deepseek/deepseek-v4.1-flash-1017", label: "DeepSeek V4.1 Flash (Low TTFT)" },
  { value: "openai/gpt-6-luna", label: "GPT-6 Luna" },
  { value: "openai/gpt-6.1-terra", label: "GPT-6.1 Terra (Mid)" },
  { value: "meta/muse-spark-open", label: "Muse Spark Open" },
  { value: "google/gemini-4-hydrogen", label: "Gemini 4 Hydrogen" },
  { value: "openai/gpt-6.2-sol", label: "GPT-6.2 Sol (Frontier Value)" },
  { value: "anthropic/claude-sonnet-5.5", label: "Claude Sonnet 5.5" },
  { value: "google/gemini-4-neon", label: "Gemini 4 Neon (Top Coding)" },
  { value: "anthropic/claude-opus-5.5", label: "Claude Opus 5.5 (Peak)" },
  { value: "openai/gpt-6.2-astra", label: "GPT-6.2 Astra (Peak)" },
];

const PRIVILEGED_WIDGET_DIRECTIVES = new Set(["auto-approve", "yolo"]);

/**
 * Classify an action string sent from an interactive widget iframe.
 * Privileged directives like /auto-approve or /yolo are blocked; other leading slash commands require confirmation.
 */
export function classifyWidgetAction(action) {
  const trimmed = String(action || "").trim();
  if (!trimmed) {
    return { kind: "empty" };
  }
  const tokens = trimmed.split(/\s+/);
  for (let i = 0; i < Math.min(2, tokens.length); i++) {
    const tok = tokens[i];
    if (!tok.startsWith("/")) {
      break;
    }
    const key = tok.slice(1).toLowerCase();
    if (PRIVILEGED_WIDGET_DIRECTIVES.has(key)) {
      return { kind: "block", directive: `/${key}` };
    }
  }
  if (trimmed.startsWith("/")) {
    return { kind: "confirm", action: trimmed };
  }
  return { kind: "allow", action: trimmed };
}

function shortModelName(slug) {
  const s = String(slug || "").trim();
  const idx = s.indexOf("/");
  return idx >= 0 ? s.slice(idx + 1) : s;
}

function currentTheme() {
  const explicit = document.documentElement.getAttribute("data-theme");
  if (explicit === "dark" || explicit === "light") {
    return explicit;
  }
  return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

function isNearBottom(listEl, threshold = 96) {
  return listEl.scrollHeight - listEl.scrollTop - listEl.clientHeight < threshold;
}

function findActiveWidgetFrame(msgListEl, source) {
  if (!source) {
    return null;
  }
  for (const fr of msgListEl.querySelectorAll("iframe.gen-widget-frame")) {
    if (fr.isConnected && fr.contentWindow === source) {
      return fr;
    }
  }
  return null;
}

function renderWidgetNode(w, sendPromptFn, onRetryFrontier, msgID) {
  if (!w) {
    return null;
  }
  const kind = String(w.kind || "").toLowerCase();
  if (kind === "html") {
    const frame = el("iframe", {
      className: "gen-widget-frame",
      sandbox: "allow-scripts",
      src: "/assistant-widget",
      title: w.title || "Interactive widget",
    });
    let acked = false;
    let retryTimer = null;
    const postRender = () => {
      if (acked) {
        return;
      }
      try {
        if (frame.contentWindow) {
          frame.contentWindow.postMessage({ type: "render", html: w.html || "", theme: currentTheme() }, "*");
        }
      } catch {
        // ignore transient iframe readiness error
      }
    };
    frame._fleetRender = postRender;
    frame._fleetAck = () => {
      acked = true;
      if (retryTimer) {
        clearTimeout(retryTimer);
        retryTimer = null;
      }
    };
    frame._fleetSetTheme = (theme) => {
      try {
        if (frame.contentWindow) {
          frame.contentWindow.postMessage({ type: "theme", theme }, "*");
        }
      } catch {
        // ignore detached frame
      }
    };
    frame.addEventListener("load", () => {
      postRender();
      if (!acked && !retryTimer) {
        retryTimer = setTimeout(() => {
          retryTimer = null;
          if (!acked && frame.isConnected) {
            postRender();
          }
        }, 150);
      }
    });
    return el(
      "div",
      { className: "gen-widget-card" },
      w.title ? el("div", { className: "gen-widget-title", text: w.title }) : null,
      frame,
    );
  }
  let data = w.data || {};
  if (typeof data === "string" && kind !== "markdown") {
    try {
      const parsed = JSON.parse(data);
      if (parsed && typeof parsed === "object") {
        data = parsed;
      }
    } catch {
      if (kind === "mcp_oauth") {
        data = { server: data };
      }
    }
  }
  const card = el("div", { className: "gen-native-card" }, w.title ? el("div", { className: "gen-widget-title", text: w.title }) : null);
  if (kind === "markdown") {
    const md = typeof data === "string" ? data : data.markdown || data.content || data.text || "";
    card.appendChild(renderMarkdownDOM(md));
    return card;
  }
  if (kind === "choices" && Array.isArray(data.choices)) {
    const row = el("div", { className: "actions-row" });
    for (const c of data.choices) {
      const label = typeof c === "string" ? c : c.label || c.value || "Option";
      const val = typeof c === "string" ? c : c.value || c.label || label;
      row.appendChild(
        el("button", {
          type: "button",
          className: "btn btn-sm",
          text: label,
          onclick: () => sendPromptFn(val),
        }),
      );
    }
    card.appendChild(row);
    return card;
  }
  if (kind === "key_value" && data && typeof data === "object") {
    const kv = el("div", { className: "kv-list" });
    for (const [k, v] of Object.entries(data)) {
      kv.appendChild(el("div", { className: "kv-label", text: k }));
      kv.appendChild(el("div", { className: "kv-value", text: typeof v === "string" ? v : JSON.stringify(v) }));
    }
    card.appendChild(kv);
    return card;
  }
  if (kind === "agents" && Array.isArray(data.agents)) {
    for (const ag of data.agents) {
      card.appendChild(
        el(
          "div",
          { className: "gen-mini-row" },
          el("span", { className: "mono", text: `${ag.name || ag.session_id || "agent"} (${ag.device || "?"})` }),
          badge(ag.state || "unknown", ag.state && ag.state.startsWith("waiting") ? "warn" : "info"),
        ),
      );
    }
    return card;
  }
  if (kind === "agenda" && Array.isArray(data.items)) {
    for (const it of data.items) {
      card.appendChild(
        el(
          "div",
          { className: "gen-mini-row" },
          el("span", { className: "mono small", text: it.time || "" }),
          el("span", { text: it.title || "" }),
        ),
      );
    }
    return card;
  }
  if (kind === "frontier_retry") {
    const modelSlug = data.frontier_model || "frontier";
    const shortName = shortModelName(modelSlug);
    const pct = typeof data.probability === "number" ? `${Math.round(data.probability * 100)}%` : "";
    const bar = el("div", { className: "frontier-retry-bar" });
    bar.appendChild(el("span", { className: "small muted", text: pct ? `Borderline complexity (${pct})` : "Borderline complexity" }));
    const row = el("div", { className: "actions-row" });
    const retryBtn = el("button", {
      type: "button",
      className: "btn btn-sm",
      text: `Retry with ${shortName}`,
      onclick: async () => {
        retryBtn.disabled = true;
        topicBtn.disabled = true;
        if (onRetryFrontier) {
          await onRetryFrontier(msgID, data.prompt || "", false);
        }
      },
    });
    const topicBtn = el("button", {
      type: "button",
      className: "btn btn-sm btn-primary",
      text: "Retry + unlock topic",
      onclick: async () => {
        retryBtn.disabled = true;
        topicBtn.disabled = true;
        if (onRetryFrontier) {
          await onRetryFrontier(msgID, data.prompt || "", true);
        }
      },
    });
    row.append(retryBtn, topicBtn);
    bar.appendChild(row);
    return bar;
  }
  if (kind === "files" && Array.isArray(data.files)) {
    const list = el("div", { className: "file-card-list" });
    for (const f of data.files) {
      if (!f || !f.url) {
        continue;
      }
      const row = el(
        "div",
        { className: "file-card-row" },
        f.is_image
          ? el("a", { href: `${f.url}?inline=1`, target: "_blank", rel: "noopener noreferrer" }, el("img", { className: "file-thumb", src: `${f.url}?inline=1`, alt: f.name || "attachment", loading: "lazy" }))
          : null,
        el(
          "div",
          { className: "file-card-meta" },
          el("div", { className: "mono small", text: f.name || "file" }),
          el("div", { className: "small muted", text: `${formatBytes(f.size_bytes)} · ${f.mime_type || "file"}` }),
        ),
        el("a", { className: "btn btn-sm", href: f.url, download: f.name || "file", text: "Download" }),
      );
      list.appendChild(row);
    }
    card.appendChild(list);
    return card;
  }
  if (kind === "mcp_oauth" && data && typeof data === "object") {
    return renderMCPOAuthCard(card, data, sendPromptFn);
  }
  card.appendChild(el("pre", { className: "code-block" }, el("code", { text: JSON.stringify(data, null, 2) })));
  return card;
}

function formatApprovalArgs(toolName, argsJSON) {
  const raw = String(argsJSON || "{}").trim() || "{}";
  let formatted = raw;
  try {
    formatted = JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    formatted = raw;
  }
  return `${toolName}(${formatted})`;
}

function renderApprovalCard(apr, onDecided) {
  const isEscalation = apr.tool_name === "frontier_escalation";
  const isElicitation = apr.tool_name === "mcp_elicitation";
  let escArgs = {};
  let elicArgs = {};
  if (isEscalation) {
    try {
      escArgs = JSON.parse(apr.args_json || "{}");
    } catch {
      escArgs = {};
    }
  } else if (isElicitation) {
    try {
      elicArgs = JSON.parse(apr.args_json || "{}");
    } catch {
      elicArgs = {};
    }
  }
  let deciding = false;
  let actionsRow = null;
  const setButtonsDisabled = (disabled) => {
    if (!actionsRow) {
      return;
    }
    for (const btn of actionsRow.querySelectorAll("button")) {
      btn.disabled = disabled;
    }
  };
  const decide = async (decision, scope = "") => {
    if (deciding) {
      return;
    }
    deciding = true;
    setButtonsDisabled(true);
    try {
      await api("POST", `/v1/assistant/approvals/${encodeURIComponent(apr.id)}/decide`, { decision, scope });
      const msg = isEscalation
        ? decision === "approve"
          ? scope === "topic"
            ? "Frontier unlocked for this topic"
            : "Running Frontier once"
          : "Using default chat model"
        : isElicitation
          ? decision === "approve"
            ? "Confirmed — resuming"
            : "Declined"
          : decision === "approve"
            ? "Approved — resuming"
            : "Denied";
      toast(msg, decision === "approve" ? "ok" : "info");
      if (onDecided) {
        await onDecided();
      }
    } catch (err) {
      deciding = false;
      setButtonsDisabled(false);
      toast(err.message, "error");
    }
  };
  if (isElicitation) {
    const card = el(
      "div",
      { className: "approval-card approval-card-escalation" },
      el(
        "div",
        { className: "approval-head" },
        badge(elicArgs.server_name || apr.server_name || "mcp", "info"),
        badge(elicArgs.remote_tool || "confirm", "warn"),
        el("strong", { text: elicArgs.title || apr.summary || "Confirm MCP action" }),
      ),
    );
    if (elicArgs.message) {
      card.appendChild(el("div", { className: "small approval-meta" }, renderMarkdownDOM(elicArgs.message)));
    }
    if (apr.status !== "pending") {
      card.appendChild(el("div", { className: "approval-status" }, badge(apr.status, apr.status === "executed" || apr.status === "approved" ? "ok" : "danger")));
      return card;
    }
    const opts = Array.isArray(elicArgs.options) && elicArgs.options.length > 0
      ? elicArgs.options
      : [
          { value: "ALLOW_ONCE", label: "Allow once" },
          { value: "DENY_ONCE", label: "Don't allow" },
        ];
    let allowPrimaryUsed = false;
    actionsRow = el(
      "div",
      { className: "actions-row" },
      ...opts.map((opt) => {
        const val = String((opt && opt.value) || "").trim();
        const label = String((opt && opt.label) || val || "Confirm");
        const isDeny = /^DENY_/i.test(val);
        let cls = "btn btn-sm";
        if (isDeny) {
          cls = "btn btn-sm btn-danger";
        } else if (val === "ALLOW_ONCE" || !allowPrimaryUsed) {
          cls = "btn btn-sm btn-primary";
          allowPrimaryUsed = true;
        }
        return el("button", {
          type: "button",
          className: cls,
          text: label,
          onclick: () => decide(isDeny ? "deny" : "approve", val),
        });
      }),
    );
    card.appendChild(actionsRow);
    return card;
  }
  if (isEscalation) {
    const shortFrontier = shortModelName(escArgs.frontier_model || "frontier");
    const shortFallback = shortModelName(escArgs.fallback_model || "Haiku");
    const metaParts = [];
    if (escArgs.topic_hint) {
      metaParts.push(`Topic: ${escArgs.topic_hint}`);
    }
    metaParts.push(`Topic grant: up to ${escArgs.grant_max_turns || 5} turns / $${Number(escArgs.grant_max_cost || 0.25).toFixed(2)}`);
    if (escArgs.daily_spend_usd > 0) {
      metaParts.push(`Today: $${Number(escArgs.daily_spend_usd).toFixed(2)}`);
    }
    const card = el(
      "div",
      { className: "approval-card approval-card-escalation" },
      el(
        "div",
        { className: "approval-head" },
        badge("frontier", "warn"),
        el("strong", { text: apr.summary || `Use ${shortFrontier} for this request?` }),
      ),
      el("div", { className: "small muted approval-meta", text: metaParts.join(" · ") }),
    );
    if (apr.status !== "pending") {
      card.appendChild(el("div", { className: "approval-status" }, badge(apr.status, apr.status === "executed" || apr.status === "approved" ? "ok" : "danger")));
      return card;
    }
    actionsRow = el(
      "div",
      { className: "actions-row" },
      el("button", { type: "button", className: "btn btn-sm btn-primary", text: `Use ${shortFrontier} (this topic)`, onclick: () => decide("approve", "topic") }),
      el("button", { type: "button", className: "btn btn-sm", text: "Once", onclick: () => decide("approve", "once") }),
      el("button", { type: "button", className: "btn btn-sm", text: `Use ${shortFallback}`, onclick: () => decide("deny") }),
    );
    card.appendChild(actionsRow);
    return card;
  }
  const card = el(
    "div",
    { className: "approval-card" },
    el(
      "div",
      { className: "approval-head" },
      badge(apr.risk_level || "approval", "warn"),
      el("strong", { text: apr.summary || apr.tool_name }),
    ),
    el("pre", { className: "code-block small approval-args" }, el("code", { text: formatApprovalArgs(apr.tool_name, apr.args_json) })),
  );
  if (apr.status !== "pending") {
    card.appendChild(el("div", { className: "approval-status" }, badge(apr.status, apr.status === "executed" || apr.status === "approved" ? "ok" : "danger")));
    return card;
  }
  actionsRow = el(
    "div",
    { className: "actions-row" },
    el("button", { type: "button", className: "btn btn-sm btn-primary", text: "Approve", onclick: () => decide("approve") }),
    el("button", { type: "button", className: "btn btn-sm btn-danger", text: "Deny", onclick: () => decide("deny") }),
  );
  card.appendChild(actionsRow);
  return card;
}

function formatMessageFooter(m) {
  const parts = [];
  if (m.source === "discord") {
    parts.push("via Discord");
  }
  if (m.model) {
    parts.push(m.model);
  }
  if (m.prompt_tokens > 0) {
    const pct = Math.round(((m.cached_tokens || 0) / m.prompt_tokens) * 100);
    parts.push(`${m.prompt_tokens} in (${pct}% cached) · ${m.completion_tokens || 0} out`);
  }
  if (m.cost_usd > 0) {
    parts.push(`$${m.cost_usd.toFixed(4)}`);
  }
  return parts.join(" · ");
}

export async function renderChat(main, ctx, sub) {
  const signal = ctx && ctx.signal;
  clear(main);
  let channels = [];
  let cfgView = {};
  try {
    const [chRes, stRes] = await Promise.all([
      api("GET", "/v1/assistant/channels", undefined, { signal }),
      api("GET", "/v1/assistant/status", undefined, { signal }),
    ]);
    if (signal && signal.aborted) {
      return;
    }
    channels = chRes.channels || [];
    cfgView = (stRes && stRes.config) || {};
  } catch (err) {
    if ((err && err.name === "AbortError") || (signal && signal.aborted)) {
      return;
    }
    main.appendChild(el("div", { className: "alert-banner alert-danger", text: `Unable to load assistant: ${err.message}` }));
    return;
  }
  if (signal && signal.aborted) {
    return;
  }

  let activeChannel = (sub || "").trim() || localStorage.getItem("fleet_chat_channel") || "general";
  if (!channels.some((c) => c.id === activeChannel) && channels.length > 0) {
    activeChannel = channels[0].id;
  }
  localStorage.setItem("fleet_chat_channel", activeChannel);
  state.activeChatChannel = activeChannel;

  const emptyBox = el("div", {
    className: "chat-empty muted small hidden",
    text: cfgView.configured
      ? `Short-term thread in #${activeChannel} is empty. New Chat clears this thread while keeping long-term Observations, Reflections, and Fleet Memory.`
      : "Configure your OpenRouter API key in Settings to start chatting.",
  });
  const msgItemsEl = el("div", { className: "chat-msg-group" });
  const approvalsBox = el("div", { className: "chat-msg-group hidden" });
  const streamBox = el("div", { className: "chat-msg chat-msg-assistant hidden" });
  const streamMeta = el("div", { className: "small muted mb-1" });
  const streamBody = el("div", { className: "chat-stream-body" });
  streamBox.append(streamMeta, streamBody);
  const msgInnerEl = el("div", { className: "chat-messages-inner" }, emptyBox, msgItemsEl, approvalsBox, streamBox);
  const msgListEl = el("div", { className: "chat-messages" }, msgInnerEl);
  const statusPill = badge("connecting", "neutral");
  const autoApprovePill = el("button", {
    type: "button",
    className: `badge badge-warn auto-approve-pill${cfgView.auto_approve ? "" : " hidden"}`,
    text: "Auto-Approve",
    title: "HITL approvals are bypassed — click to configure",
    onclick: () => openAssistantSettingsModal(() => ctx.refresh(), (h) => ctx.navigate(h)),
  });
  const grantPill = el("button", {
    type: "button",
    className: "badge badge-info grant-pill hidden",
    title: "Active frontier topic grant — click to revoke",
  });

  const channelSelect = el(
    "select",
    { className: "chat-channel-select", "aria-label": "Channel" },
    ...channels.map((ch) => el("option", { value: ch.id, text: ch.title || `#${ch.id}` })),
    el("option", { value: "__new__", text: "+ New Channel…" }),
  );
  channelSelect.value = activeChannel;

  const openNewChannelModal = () => {
    const idIn = el("input", { type: "text", placeholder: "e.g. research" });
    const modelIn = el("input", { type: "text", placeholder: "optional model slug" });
    openModal(
      "New Channel",
      el(
        "div",
        { className: "stack" },
        field("Channel Slug", idIn),
        field("Default Model (optional)", modelIn),
        el("button", {
          type: "button",
          className: "btn btn-primary",
          text: "Create",
          onclick: async () => {
            try {
              const res = await api("POST", "/v1/assistant/channels", { id: idIn.value.trim(), model: modelIn.value.trim() });
              closeModal();
              ctx.navigate(`#chat/${res.channel.id}`);
            } catch (err) {
              toast(err.message, "error");
            }
          },
        }),
      ),
    );
  };

  channelSelect.addEventListener("change", () => {
    if (channelSelect.value === "__new__") {
      channelSelect.value = activeChannel;
      openNewChannelModal();
      return;
    }
    ctx.navigate(`#chat/${channelSelect.value}`);
  });

  const chObj = channels.find((c) => c.id === activeChannel) || {};
  const routingMode = (cfgView.routing && cfgView.routing.mode) || "hybrid";
  const defaultChoiceLabel = chObj.model
    ? `Channel Default (${shortModelName(chObj.model)})`
    : routingMode === "hybrid"
      ? "Auto (Hybrid)"
      : routingMode === "gate"
        ? "Auto (HITL Gate)"
        : routingMode === "auto"
          ? "Auto (Unattended)"
          : "Channel / Default";
  const modelSelect = el(
    "select",
    { className: "chat-model-select", "aria-label": "Model override" },
    ...MODEL_CHOICES.map((m, idx) => el("option", { value: m.value, text: idx === 0 ? defaultChoiceLabel : m.label })),
  );
  const inputEl = el("textarea", {
    className: "chat-input",
    rows: "1",
    placeholder: "Message assistant… (/haiku, /sol, /auto-approve)",
  });
  const fileInput = el("input", { type: "file", multiple: true, className: "hidden" });
  const attachBtn = el("button", { type: "button", className: "btn btn-sm", text: "📎", title: "Attach file(s) for sandbox", "aria-label": "Attach files" });
  const attachListEl = el("div", { className: "composer-attachments hidden" });
  const sendBtn = el("button", { type: "button", className: "btn btn-sm btn-primary", text: "Send" });
  let pendingFiles = [];

  const renderPendingFiles = () => {
    clear(attachListEl);
    attachListEl.classList.toggle("hidden", pendingFiles.length === 0);
    for (const f of pendingFiles) {
      attachListEl.appendChild(
        el(
          "span",
          { className: "attach-chip mono small" },
          el("span", { text: f.name }),
          el("button", {
            type: "button",
            className: "attach-chip-rm",
            "aria-label": `Remove ${f.name}`,
            text: "×",
            onclick: () => {
              pendingFiles = pendingFiles.filter((it) => it.id !== f.id);
              renderPendingFiles();
            },
          }),
        ),
      );
    }
  };

  const uploadSelectedFiles = async (files) => {
    if (!files || files.length === 0) {
      return;
    }
    attachBtn.disabled = true;
    try {
      for (const file of files) {
        const fd = new FormData();
        fd.append("channel", activeChannel);
        fd.append("file", file);
        const resp = await fetch("/v1/assistant/files", { method: "POST", body: fd, credentials: "same-origin", signal });
        const data = await resp.json().catch(() => null);
        if (!resp.ok) {
          throw new Error((data && data.error) || `${resp.status} ${resp.statusText}`);
        }
        if (data && data.file) {
          pendingFiles.push(data.file);
        }
      }
      renderPendingFiles();
    } catch (err) {
      if (err && err.name === "AbortError") {
        return;
      }
      toast(err.message, "error");
    } finally {
      attachBtn.disabled = false;
      fileInput.value = "";
    }
  };

  attachBtn.addEventListener("click", () => fileInput.click());
  fileInput.addEventListener("change", () => uploadSelectedFiles(Array.from(fileInput.files || [])));
  inputEl.addEventListener("paste", (ev) => {
    const files = Array.from((ev.clipboardData && ev.clipboardData.files) || []);
    if (files.length > 0) {
      ev.preventDefault();
      uploadSelectedFiles(files);
    }
  });

  const resizeComposer = () => {
    inputEl.style.height = "auto";
    inputEl.style.height = `${Math.min(inputEl.scrollHeight, 128)}px`;
  };
  inputEl.addEventListener("input", resizeComposer);

  const scrollMessagesToBottom = () => {
    msgListEl.scrollTop = msgListEl.scrollHeight;
  };

  let pendingApprovals = [];
  let channelLoadSeq = 0;
  const updateGrantPill = (grant) => {
    if (!grant) {
      grantPill.classList.add("hidden");
      grantPill.textContent = "";
      return;
    }
    const cost = Number(grant.cost_usd || 0).toFixed(2);
    grantPill.textContent = `Frontier topic (${grant.turns_used || 0}/${grant.max_turns || 5} · $${cost}) ×`;
    grantPill.classList.remove("hidden");
  };
  const loadChannelData = async () => {
    const seq = ++channelLoadSeq;
    try {
      const [msgRes, aprRes] = await Promise.all([
        api("GET", `/v1/assistant/messages?channel=${encodeURIComponent(activeChannel)}&limit=100`, undefined, { signal }),
        api("GET", "/v1/assistant/approvals?status=pending&limit=20", undefined, { signal }),
      ]);
      if (seq !== channelLoadSeq || (signal && signal.aborted)) {
        return;
      }
      pendingApprovals = (aprRes.approvals || []).filter((a) => a.channel_id === activeChannel);
      updateGrantPill(msgRes.grant || null);
      renderMessages(msgRes.messages || []);
    } catch (err) {
      if ((err && err.name === "AbortError") || (signal && signal.aborted)) {
        return;
      }
      if (seq === channelLoadSeq) {
        toast(`Failed loading channel messages: ${err.message}`, "error");
      }
    }
  };
  grantPill.addEventListener("click", async () => {
    try {
      await api("DELETE", `/v1/assistant/channels/${encodeURIComponent(activeChannel)}/grant`);
      toast("Frontier topic grant revoked", "info");
      await loadChannelData();
    } catch (err) {
      toast(err.message, "error");
    }
  });
  const retryWithFrontier = async (replaceMsgID, prompt, grantTopic) => {
    if (!prompt) {
      return;
    }
    try {
      await api("POST", `/v1/assistant/channels/${encodeURIComponent(activeChannel)}/retry-frontier`, {
        replace_message_id: typeof replaceMsgID === "number" ? replaceMsgID : 0,
        prompt,
        grant_topic: Boolean(grantTopic),
      });
    } catch (err) {
      toast(err.message, "error");
    }
  };

  const sendMessage = async (overrideText = null) => {
    const isComposer = overrideText === null;
    const prevRaw = isComposer ? inputEl.value : "";
    const prevFiles = isComposer ? pendingFiles.slice() : [];
    const text = (isComposer ? prevRaw : String(overrideText)).trim();
    const fileIDs = isComposer ? prevFiles.map((f) => f.id) : [];
    if (!text && fileIDs.length === 0) {
      return;
    }
    if (isComposer) {
      inputEl.value = "";
      pendingFiles = [];
      renderPendingFiles();
      resizeComposer();
    }
    try {
      await api("POST", "/v1/assistant/messages", {
        channel: activeChannel,
        content: text,
        model: modelSelect.value,
        file_ids: fileIDs,
      });
    } catch (err) {
      if (isComposer) {
        if (!inputEl.value) {
          inputEl.value = prevRaw;
        }
        const seen = new Set(pendingFiles.map((f) => f.id));
        const restored = [];
        for (const f of prevFiles) {
          if (!seen.has(f.id)) {
            seen.add(f.id);
            restored.push(f);
          }
        }
        pendingFiles = [...restored, ...pendingFiles];
        renderPendingFiles();
        resizeComposer();
      }
      toast(err.message, "error");
    }
  };

  const renderedMsgs = new Map();
  const buildMessageBubble = (m) => {
    const isUser = m.role === "user";
    const hasHTMLWidget = Array.isArray(m.widgets) && m.widgets.some((w) => w && w.kind === "html");
    const bubble = el("div", { className: `chat-msg ${isUser ? "chat-msg-user" : "chat-msg-assistant"}${hasHTMLWidget ? " chat-msg-has-widget" : ""}` });
    bubble.appendChild(renderMarkdownDOM(m.content));
    if (Array.isArray(m.widgets)) {
      for (const w of m.widgets) {
        const wNode = renderWidgetNode(w, (act) => sendMessage(act), retryWithFrontier, m.id);
        if (wNode) {
          bubble.appendChild(wNode);
        }
      }
    }
    const footer = formatMessageFooter(m);
    if (footer) {
      bubble.appendChild(el("div", { className: "chat-msg-footer mono", text: footer }));
    }
    return bubble;
  };
  const messageRenderKey = (m) =>
    JSON.stringify([
      m.role || "",
      m.content || "",
      m.model || "",
      m.prompt_tokens || 0,
      m.cached_tokens || 0,
      m.completion_tokens || 0,
      m.cost_usd || 0,
      m.widgets || null,
    ]);

  const renderMessages = (messages) => {
    const shouldScroll = renderedMsgs.size === 0 || isNearBottom(msgListEl);
    const isEmpty = messages.length === 0 && pendingApprovals.length === 0;
    emptyBox.classList.toggle("hidden", !isEmpty);
    msgItemsEl.classList.toggle("hidden", messages.length === 0);

    const nextIds = new Set();
    for (let i = 0; i < messages.length; i++) {
      const m = messages[i];
      const id = m.id !== undefined && m.id !== null ? `id:${m.id}` : `idx:${i}`;
      nextIds.add(id);
      const key = messageRenderKey(m);
      const existing = renderedMsgs.get(id);
      if (existing && existing.key === key) {
        continue;
      }
      const bubble = buildMessageBubble(m);
      if (existing) {
        msgItemsEl.replaceChild(bubble, existing.node);
      } else {
        msgItemsEl.appendChild(bubble);
      }
      renderedMsgs.set(id, { node: bubble, key });
    }
    for (const [id, entry] of renderedMsgs) {
      if (!nextIds.has(id)) {
        entry.node.remove();
        renderedMsgs.delete(id);
      }
    }

    clear(approvalsBox);
    approvalsBox.classList.toggle("hidden", pendingApprovals.length === 0);
    for (const apr of pendingApprovals) {
      approvalsBox.appendChild(renderApprovalCard(apr, loadChannelData));
    }
    if (shouldScroll) {
      scrollMessagesToBottom();
    }
  };

  sendBtn.addEventListener("click", () => sendMessage(null));
  inputEl.addEventListener("keydown", (ev) => {
    if (ev.key === "Enter" && !ev.shiftKey) {
      ev.preventDefault();
      sendMessage(null);
    }
  });

  let streamText = "";
  let deltaRAF = 0;
  const cancelDeltaRAF = () => {
    if (deltaRAF) {
      cancelAnimationFrame(deltaRAF);
      deltaRAF = 0;
    }
  };
  const flushDeltaRender = () => {
    deltaRAF = 0;
    const nearBottom = isNearBottom(msgListEl);
    clear(streamBody);
    streamBody.appendChild(renderMarkdownDOM(streamText));
    if (nearBottom) {
      scrollMessagesToBottom();
    }
  };

  const topbar = el(
    "header",
    { className: "chat-topbar" },
    el(
      "div",
      { className: "chat-topbar-left" },
      el("button", {
        type: "button",
        className: "btn btn-sm chat-menu-btn",
        "aria-label": "Open Fleet menu",
        text: "☰",
        onclick: () => ctx.openMenu && ctx.openMenu(),
      }),
      channelSelect,
      statusPill,
      autoApprovePill,
      grantPill,
    ),
    el(
      "div",
      { className: "chat-topbar-right" },
      el("button", {
        type: "button",
        className: "btn btn-sm",
        text: "New Chat",
        title: "Clear this chat thread & start fresh context (long-term memory stays intact)",
        onclick: async () => {
          try {
            await api("POST", `/v1/assistant/channels/${encodeURIComponent(activeChannel)}/reset`, { clear_history: true });
            cancelDeltaRAF();
            streamBox.classList.add("hidden");
            streamText = "";
            await loadChannelData();
            toast("Chat cleared — long-term memory preserved", "ok");
          } catch (err) {
            toast(err.message, "error");
          }
        },
      }),
      modelSelect,
      el("button", {
        type: "button",
        className: "btn btn-sm",
        text: "Settings",
        onclick: () => openAssistantSettingsModal(() => ctx.refresh(), (h) => ctx.navigate(h)),
      }),
    ),
  );

  const composerBox = el(
    "div",
    { className: "chat-composer" },
    el(
      "div",
      { className: "chat-composer-inner" },
      attachListEl,
      fileInput,
      el(
        "div",
        { className: "chat-composer-row" },
        attachBtn,
        inputEl,
        sendBtn,
      ),
    ),
  );
  composerBox.addEventListener("dragover", (ev) => {
    ev.preventDefault();
  });
  composerBox.addEventListener("drop", (ev) => {
    ev.preventDefault();
    const files = Array.from((ev.dataTransfer && ev.dataTransfer.files) || []);
    if (files.length > 0) {
      uploadSelectedFiles(files);
    }
  });

  const shell = el(
    "div",
    { className: "chat-shell" },
    topbar,
    msgListEl,
    composerBox,
  );
  main.appendChild(shell);

  await loadChannelData();
  if (signal && signal.aborted) {
    return;
  }

  const kbTimers = new Set();
  const syncViewport = () => {
    const vv = window.visualViewport;
    const vh = vv ? vv.height : window.innerHeight;
    const vt = vv ? vv.offsetTop : 0;
    const kbOpen = vv ? window.innerHeight - vv.height > 110 : false;
    const nearBottom = isNearBottom(msgListEl);
    document.documentElement.style.setProperty("--vvh", `${Math.round(vh)}px`);
    document.documentElement.style.setProperty("--vvt", `${Math.round(vt)}px`);
    document.documentElement.classList.toggle("kb-open", kbOpen);
    if (kbOpen) {
      window.scrollTo(0, 0);
    }
    if (kbOpen || nearBottom) {
      scrollMessagesToBottom();
    }
  };
  const onFocusInput = () => {
    syncViewport();
    for (const delay of [80, 260]) {
      const id = setTimeout(() => {
        kbTimers.delete(id);
        syncViewport();
        scrollMessagesToBottom();
      }, delay);
      kbTimers.add(id);
    }
  };
  syncViewport();
  window.addEventListener("resize", syncViewport);
  inputEl.addEventListener("focus", onFocusInput);
  if (window.visualViewport) {
    window.visualViewport.addEventListener("resize", syncViewport);
    window.visualViewport.addEventListener("scroll", syncViewport);
  }

  const setStatus = (txt, variant) => {
    statusPill.textContent = txt;
    statusPill.className = `badge badge-${variant}`;
  };
  if (state.sseStatus === "live") {
    setStatus("live", "ok");
  } else if (state.sseStatus === "reconnecting") {
    setStatus("reconnecting", "warn");
  }
  const onFleetSSE = (ev) => {
    const detail = ev.detail || {};
    const type = detail.type;
    const d = detail.payload || {};
    if (type === "status") {
      setStatus(d.status || "live", d.status === "live" ? "ok" : "warn");
      return;
    }
    if (type === "ready") {
      setStatus("live", "ok");
      return;
    }
    if (type === "run_start") {
      if (d.channel_id !== activeChannel) {
        return;
      }
      cancelDeltaRAF();
      streamText = "";
      const gt = d.data && d.data.grant_turn ? ` · topic ${d.data.grant_turn}/${d.data.grant_max_turns || 5}` : "";
      streamMeta.textContent = `Running (${(d.data && d.data.model) || ""}${gt})…`;
      clear(streamBody);
      streamBox.classList.remove("hidden");
      if (isNearBottom(msgListEl)) {
        scrollMessagesToBottom();
      }
      return;
    }
    if (type === "tool_start") {
      if (d.channel_id === activeChannel) {
        streamMeta.textContent = `Calling ${(d.data && d.data.name) || "tool"}…`;
      }
      return;
    }
    if (type === "delta") {
      if (d.channel_id !== activeChannel) {
        return;
      }
      streamText += (d.data && d.data.delta) || "";
      if (!deltaRAF) {
        deltaRAF = requestAnimationFrame(flushDeltaRender);
      }
      return;
    }
    if (type === "config_updated") {
      const enabled = Boolean(d.data && d.data.auto_approve);
      autoApprovePill.classList.toggle("hidden", !enabled);
      return;
    }
    if (type === "channels_updated") {
      api("GET", "/v1/assistant/channels", undefined, { signal })
        .then((res) => {
          if (signal && signal.aborted) {
            return;
          }
          const list = res.channels || [];
          clear(channelSelect);
          for (const ch of list) {
            channelSelect.appendChild(el("option", { value: ch.id, text: ch.title || `#${ch.id}` }));
          }
          channelSelect.appendChild(el("option", { value: "__new__", text: "+ New Channel…" }));
          channelSelect.value = activeChannel;
        })
        .catch(() => {});
      return;
    }
    if (type === "message_created" || type === "message_done" || type === "approval" || type === "approval_decided" || type === "channel_reset" || type === "grant_updated") {
      if (d.channel_id === activeChannel) {
        cancelDeltaRAF();
        streamBox.classList.add("hidden");
        streamText = "";
        loadChannelData();
      }
      return;
    }
    if (type === "error" && d.channel_id === activeChannel && d.data && d.data.error) {
      cancelDeltaRAF();
      streamBox.classList.add("hidden");
      toast(d.data.error, "error");
    }
  };
  window.addEventListener("fleet-sse", onFleetSSE);

  const handleWidgetAction = async (rawAction) => {
    const verdict = classifyWidgetAction(rawAction);
    if (verdict.kind === "empty") {
      return;
    }
    if (verdict.kind === "block") {
      toast(`Blocked privileged widget directive (${verdict.directive})`, "warn");
      return;
    }
    if (verdict.kind === "confirm") {
      const ok = await confirmDialog(
        "Confirm Widget Command",
        `A widget requested sending a slash command:\n\n${verdict.action}`,
        "Send Command",
        true,
      );
      if (!ok) {
        return;
      }
    }
    await sendMessage(verdict.action);
  };

  const onWindowMsg = (ev) => {
    if (!ev.data || typeof ev.data !== "object") {
      return;
    }
    const frame = findActiveWidgetFrame(msgListEl, ev.source);
    if (!frame) {
      return;
    }
    if (ev.data.type === "fleet-widget-ready") {
      if (typeof frame._fleetRender === "function") {
        frame._fleetRender();
      }
    } else if (ev.data.type === "fleet-widget-rendered") {
      if (typeof frame._fleetAck === "function") {
        frame._fleetAck();
      }
    } else if (ev.data.type === "fleet-widget-resize" && typeof ev.data.height === "number") {
      const nearBottom = isNearBottom(msgListEl, 120);
      frame.style.height = `${Math.min(Math.max(ev.data.height, 48), 640)}px`;
      if (nearBottom) {
        scrollMessagesToBottom();
      }
    } else if (ev.data.type === "fleet-widget-action" && ev.data.action) {
      void handleWidgetAction(ev.data.action);
    }
  };
  window.addEventListener("message", onWindowMsg);

  const unsubOAuth = subscribeMCPOAuthDone((payload) => {
    const targetSrv = String(payload.server || "");
    for (const card of msgListEl.querySelectorAll("[data-mcp-oauth-server]")) {
      if ((!targetSrv || card.dataset.mcpOauthServer === targetSrv) && typeof card._onMCPOAuthDone === "function") {
        card._onMCPOAuthDone(payload);
      }
    }
  });

  const syncWidgetThemes = () => {
    const theme = currentTheme();
    for (const fr of msgListEl.querySelectorAll("iframe.gen-widget-frame")) {
      if (typeof fr._fleetSetTheme === "function") {
        fr._fleetSetTheme(theme);
      }
    }
  };
  window.addEventListener("fleet-theme-change", syncWidgetThemes);

  state.routeCleanup = () => {
    cancelDeltaRAF();
    unsubOAuth();
    window.removeEventListener("fleet-sse", onFleetSSE);
    window.removeEventListener("message", onWindowMsg);
    window.removeEventListener("fleet-theme-change", syncWidgetThemes);
    window.removeEventListener("resize", syncViewport);
    inputEl.removeEventListener("focus", onFocusInput);
    if (window.visualViewport) {
      window.visualViewport.removeEventListener("resize", syncViewport);
      window.visualViewport.removeEventListener("scroll", syncViewport);
    }
    for (const id of kbTimers) {
      clearTimeout(id);
    }
    kbTimers.clear();
    state.activeChatChannel = "";
    document.documentElement.classList.remove("kb-open");
  };
}
