import { api, badge, clear, closeModal, el, field, openModal, section, state, toast } from "./lib.js";

const MODEL_CHOICES = [
  { value: "", label: "Channel / Default" },
  { value: "auto", label: "Auto (Decision Model)" },
  { value: "meta/muse-spark-open", label: "Muse Spark Open" },
  { value: "deepseek/deepseek-chat", label: "DeepSeek Chat" },
  { value: "openai/gpt-5-nano", label: "GPT-5 Nano" },
  { value: "openai/gpt-6-luna", label: "GPT-6 Luna" },
  { value: "anthropic/claude-haiku-4.5", label: "Claude Haiku" },
  { value: "anthropic/claude-sonnet-4.6", label: "Claude Sonnet" },
  { value: "anthropic/claude-opus-4.5", label: "Claude Opus" },
  { value: "openai/gpt-6-astra", label: "GPT-6 Astra" },
];

function currentTheme() {
  const explicit = document.documentElement.getAttribute("data-theme");
  if (explicit === "dark" || explicit === "light") {
    return explicit;
  }
  return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

function renderInlineDOM(text) {
  const frag = document.createDocumentFragment();
  const re = /(`[^`]+`|\*\*[^*]+\*\*|\[[^\]]+\]\((https?:\/\/[^)\s]+|#[^)\s]*)\))/g;
  let last = 0;
  let m;
  while ((m = re.exec(text)) !== null) {
    if (m.index > last) {
      frag.appendChild(document.createTextNode(text.slice(last, m.index)));
    }
    const tok = m[0];
    if (tok.startsWith("`") && tok.endsWith("`")) {
      frag.appendChild(el("code", { className: "inline-code", text: tok.slice(1, -1) }));
    } else if (tok.startsWith("**") && tok.endsWith("**")) {
      frag.appendChild(el("strong", { text: tok.slice(2, -2) }));
    } else {
      const linkMatch = /^\[([^\]]+)\]\(([^)]+)\)$/.exec(tok);
      if (linkMatch) {
        const href = linkMatch[2];
        const props = href.startsWith("#") ? { href, text: linkMatch[1] } : { href, target: "_blank", rel: "noopener noreferrer", text: linkMatch[1] };
        frag.appendChild(el("a", props));
      } else {
        frag.appendChild(document.createTextNode(tok));
      }
    }
    last = re.lastIndex;
  }
  if (last < text.length) {
    frag.appendChild(document.createTextNode(text.slice(last)));
  }
  return frag;
}

function renderMarkdownDOM(raw) {
  const wrap = el("div", { className: "chat-md" });
  const text = String(raw || "");
  if (!text) {
    return wrap;
  }
  const parts = text.split(/```/);
  for (let i = 0; i < parts.length; i++) {
    if (i % 2 === 1) {
      const lines = parts[i].split("\n");
      const codeBody = lines.length > 1 ? lines.slice(1).join("\n") : parts[i];
      wrap.appendChild(el("pre", { className: "code-block" }, el("code", { text: codeBody.replace(/\n$/, "") })));
      continue;
    }
    const blocks = parts[i].split(/\n{2,}/);
    for (const blk of blocks) {
      const trimmed = blk.trim();
      if (!trimmed) {
        continue;
      }
      const lines = trimmed.split("\n");
      const allBullets = lines.every((l) => /^\s*[-*]\s+/.test(l));
      if (allBullets) {
        const ul = el("ul", { className: "chat-list" });
        for (const l of lines) {
          ul.appendChild(el("li", null, renderInlineDOM(l.replace(/^\s*[-*]\s+/, ""))));
        }
        wrap.appendChild(ul);
      } else {
        const p = el("p", { className: "chat-p" });
        lines.forEach((l, idx) => {
          if (idx > 0) {
            p.appendChild(el("br"));
          }
          p.appendChild(renderInlineDOM(l));
        });
        wrap.appendChild(p);
      }
    }
  }
  return wrap;
}

function renderWidgetNode(w, sendPromptFn) {
  if (!w) {
    return null;
  }
  if (w.kind === "html") {
    const frame = el("iframe", {
      className: "gen-widget-frame",
      sandbox: "allow-scripts",
      src: "/assistant-widget",
      title: w.title || "Interactive widget",
      loading: "lazy",
    });
    const sendPayload = () => {
      try {
        if (frame.contentWindow) {
          frame.contentWindow.postMessage({ type: "render", html: w.html || "", theme: currentTheme() }, "*");
        }
      } catch {
        // ignore frame load race
      }
    };
    frame._fleetRender = sendPayload;
    frame.addEventListener("load", sendPayload);
    return el(
      "div",
      { className: "gen-widget-card" },
      w.title ? el("div", { className: "gen-widget-title", text: w.title }) : null,
      frame,
    );
  }
  const data = w.data || {};
  const card = el("div", { className: "gen-native-card" }, w.title ? el("div", { className: "gen-widget-title", text: w.title }) : null);
  if (w.kind === "choices" && Array.isArray(data.choices)) {
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
  if (w.kind === "key_value" && data && typeof data === "object") {
    const kv = el("div", { className: "kv-list" });
    for (const [k, v] of Object.entries(data)) {
      kv.appendChild(el("div", { className: "kv-label", text: k }));
      kv.appendChild(el("div", { className: "kv-val", text: typeof v === "string" ? v : JSON.stringify(v) }));
    }
    card.appendChild(kv);
    return card;
  }
  if (w.kind === "agents" && Array.isArray(data.agents)) {
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
  if (w.kind === "agenda" && Array.isArray(data.items)) {
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
  card.appendChild(el("pre", { className: "code-block" }, el("code", { text: JSON.stringify(data, null, 2) })));
  return card;
}

function renderApprovalCard(apr, onDecided) {
  const card = el(
    "div",
    { className: "approval-card" },
    el(
      "div",
      { className: "approval-head" },
      badge(apr.risk_level || "approval", "warn"),
      el("strong", { text: apr.summary || apr.tool_name }),
    ),
    el("pre", { className: "code-block small" }, el("code", { text: `${apr.tool_name}(${apr.args_json || "{}"})` })),
  );
  if (apr.status !== "pending") {
    card.appendChild(badge(apr.status, apr.status === "executed" || apr.status === "approved" ? "ok" : "danger"));
    return card;
  }
  const decide = async (decision) => {
    try {
      await api("POST", `/v1/assistant/approvals/${encodeURIComponent(apr.id)}/decide`, { decision });
      toast(decision === "approve" ? "Approved — resuming" : "Denied", decision === "approve" ? "ok" : "info");
      if (onDecided) {
        await onDecided();
      }
    } catch (err) {
      toast(err.message, "error");
    }
  };
  card.appendChild(
    el(
      "div",
      { className: "actions-row" },
      el("button", { type: "button", className: "btn btn-sm btn-primary", text: "Approve", onclick: () => decide("approve") }),
      el("button", { type: "button", className: "btn btn-sm btn-danger", text: "Deny", onclick: () => decide("deny") }),
    ),
  );
  return card;
}

function formatMessageFooter(m) {
  const parts = [];
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

async function openAssistantSettingsModal(onSaved) {
  let cfgData;
  let timersData;
  try {
    [cfgData, timersData] = await Promise.all([api("GET", "/v1/assistant/config"), api("GET", "/v1/assistant/timers")]);
  } catch (err) {
    toast(err.message, "error");
    return;
  }
  const cfg = cfgData.config || {};
  const models = cfg.models || {};
  const enabledChk = el("input", { type: "checkbox", checked: Boolean(cfg.enabled) });
  const zdrChk = el("input", { type: "checkbox", checked: cfg.zdr !== false });
  const apiKeyIn = el("input", { type: "password", placeholder: cfg.api_key_hint ? `Current: ${cfg.api_key_hint}` : "sk-or-v1-..." });
  const vaultIn = el("input", { type: "text", value: cfg.vault_path || "", placeholder: cfg.effective_vault_path || "" });
  const defaultChatIn = el("input", { type: "text", value: models.default_chat || "" });
  const fastIn = el("input", { type: "text", value: models.fast || "" });
  const frontierIn = el("input", { type: "text", value: models.frontier || "" });
  const decisionIn = el("input", { type: "text", value: models.decision || "" });
  const observerIn = el("input", { type: "text", value: models.observer || "" });
  const hbIn = el("input", { type: "number", value: String(cfg.heartbeat_interval_minutes ?? 30), min: "0" });
  const mcpIn = el("textarea", { rows: "5", className: "mono", value: JSON.stringify(cfg.mcp_servers || [], null, 2) });
  const credsIn = el("textarea", {
    rows: "4",
    className: "mono",
    placeholder: `[{"alias":"linear","op_ref":"op://Fleet-Agent/Linear/token","allowed_hosts":["api.linear.app"],"require_approval":true}]`,
  });
  const doctorBox = el("div", { className: "stack-sm mt-2" });

  const renderDoctorResults = (rep) => {
    clear(doctorBox);
    for (const c of rep.checks || []) {
      const variant = c.status === "ok" ? "ok" : c.status === "fail" ? "danger" : c.status === "warn" ? "warn" : "neutral";
      doctorBox.appendChild(
        el(
          "div",
          { className: "gen-native-card" },
          el("div", { className: "gen-mini-row" }, el("strong", { text: c.name }), badge(c.status, variant)),
          el("div", { className: "small", text: c.detail || "" }),
          c.hint ? el("div", { className: "small muted mono mt-1", text: c.hint }) : null,
        ),
      );
    }
  };

  const body = el(
    "div",
    { className: "stack" },
    el("label", { className: "check-row" }, enabledChk, el("span", { text: "Enable always-on scheduler & heartbeat" })),
    el("label", { className: "check-row" }, zdrChk, el("span", { text: "Enforce OpenRouter Zero Data Retention (ZDR)" })),
    field("OpenRouter API Key (leave blank to keep current)", apiKeyIn),
    field("Obsidian Vault Path on Lighthouse", vaultIn, el("div", { className: "small muted", text: `Active: ${cfg.effective_vault_path || ""}` })),
    el(
      "div",
      { className: "grid-2" },
      field("Default Chat Model", defaultChatIn),
      field("Fast / Heartbeat Model", fastIn),
      field("Frontier Model", frontierIn),
      field("Decision Model", decisionIn),
      field("Observer / Reflector Model", observerIn),
      field("Heartbeat Interval (mins, 0=off)", hbIn),
    ),
    field("External stdio MCP Servers (JSON)", mcpIn),
    field("Replace 1Password Credential Aliases (optional JSON)", credsIn),
    el(
      "div",
      { className: "actions-row" },
      el("button", {
        type: "button",
        className: "btn btn-primary",
        text: "Save & Test",
        onclick: async () => {
          let mcpServers = cfg.mcp_servers || [];
          try {
            mcpServers = JSON.parse(mcpIn.value || "[]");
          } catch {
            toast("Invalid MCP servers JSON", "error");
            return;
          }
          const payload = {
            enabled: enabledChk.checked,
            zdr: zdrChk.checked,
            base_url: cfg.base_url,
            data_collection: cfg.data_collection,
            vault_path: vaultIn.value.trim(),
            auto_write_dirs: cfg.auto_write_dirs,
            git_auto_commit: cfg.git_auto_commit !== false,
            heartbeat_interval_minutes: Number.parseInt(hbIn.value || "0", 10) || 0,
            models: {
              ...models,
              default_chat: defaultChatIn.value.trim(),
              fast: fastIn.value.trim(),
              heartbeat: fastIn.value.trim(),
              frontier: frontierIn.value.trim(),
              decision: decisionIn.value.trim(),
              observer: observerIn.value.trim(),
              reflector: observerIn.value.trim(),
            },
            mcp_servers: mcpServers,
            test: Boolean(apiKeyIn.value.trim()),
          };
          if (apiKeyIn.value.trim()) {
            payload.api_key = apiKeyIn.value.trim();
          }
          if (credsIn.value.trim()) {
            try {
              payload.credentials = JSON.parse(credsIn.value);
            } catch {
              toast("Invalid credentials JSON", "error");
              return;
            }
          }
          try {
            await api("POST", "/v1/assistant/config", payload);
            toast("Assistant configuration saved", "ok");
            closeModal();
            if (onSaved) {
              await onSaved();
            }
          } catch (err) {
            toast(err.message, "error");
          }
        },
      }),
      el("button", {
        type: "button",
        className: "btn",
        text: "Test Connection",
        onclick: async () => {
          try {
            await api("POST", "/v1/assistant/test");
            toast("OpenRouter connection OK", "ok");
          } catch (err) {
            toast(err.message, "error");
          }
        },
      }),
      el("button", {
        type: "button",
        className: "btn",
        text: "Run Doctor",
        onclick: async () => {
          try {
            const rep = await api("GET", "/v1/assistant/doctor");
            renderDoctorResults(rep);
          } catch (err) {
            toast(err.message, "error");
          }
        },
      }),
    ),
    doctorBox,
    el("h4", { className: "mt-3", text: `Timers (${(timersData.timers || []).length})` }),
    (timersData.timers || []).length === 0
      ? el("div", { className: "small muted", text: "No timers scheduled yet." })
      : el(
          "div",
          { className: "stack-sm" },
          ...(timersData.timers || []).map((t) =>
            el(
              "div",
              { className: "gen-mini-row" },
              el("span", { className: "small", text: `${t.name} (#${t.channel_id}) · next ${t.next_run_at || "-"}` }),
              el("button", {
                type: "button",
                className: "btn btn-sm btn-danger",
                text: "Delete",
                onclick: async () => {
                  try {
                    await api("DELETE", `/v1/assistant/timers/${encodeURIComponent(t.id)}`);
                    closeModal();
                    openAssistantSettingsModal(onSaved);
                  } catch (err) {
                    toast(err.message, "error");
                  }
                },
              }),
            ),
          ),
        ),
  );
  openModal("Assistant Settings", body);
}

export async function renderChat(main, ctx, sub) {
  clear(main);
  let channels = [];
  let cfgView = {};
  try {
    const [chRes, stRes] = await Promise.all([api("GET", "/v1/assistant/channels"), api("GET", "/v1/assistant/status")]);
    channels = chRes.channels || [];
    cfgView = (stRes && stRes.config) || {};
  } catch (err) {
    main.appendChild(el("div", { className: "alert-banner alert-danger", text: `Unable to load assistant: ${err.message}` }));
    return;
  }

  let activeChannel = (sub || "").trim() || localStorage.getItem("fleet_chat_channel") || "general";
  if (!channels.some((c) => c.id === activeChannel) && channels.length > 0) {
    activeChannel = channels[0].id;
  }
  localStorage.setItem("fleet_chat_channel", activeChannel);

  const channelListEl = el("div", { className: "chat-channels" });
  const msgListEl = el("div", { className: "chat-messages" });
  const statusPill = badge("connecting", "neutral");
  const streamBox = el("div", { className: "chat-msg chat-msg-assistant hidden" });
  const streamMeta = el("div", { className: "small muted mb-1" });
  const streamBody = el("div", { className: "chat-md" });
  streamBox.append(streamMeta, streamBody);

  const modelSelect = el(
    "select",
    { className: "chat-model-select", "aria-label": "Model override" },
    ...MODEL_CHOICES.map((m) => el("option", { value: m.value, text: m.label })),
  );
  const inputEl = el("textarea", {
    className: "chat-input",
    rows: "2",
    placeholder: "Message assistant… (tip: prefix /astra, /opus, /sonnet, /muse, /deepseek)",
  });
  const sendBtn = el("button", { type: "button", className: "btn btn-primary", text: "Send" });

  let pendingApprovals = [];
  const loadChannelData = async () => {
    const [msgRes, aprRes] = await Promise.all([
      api("GET", `/v1/assistant/messages?channel=${encodeURIComponent(activeChannel)}&limit=100`),
      api("GET", "/v1/assistant/approvals?status=pending&limit=20"),
    ]);
    pendingApprovals = (aprRes.approvals || []).filter((a) => a.channel_id === activeChannel);
    renderMessages(msgRes.messages || []);
  };

  const sendMessage = async (overrideText = null) => {
    const text = (overrideText !== null ? String(overrideText) : inputEl.value).trim();
    if (!text) {
      return;
    }
    if (overrideText === null) {
      inputEl.value = "";
    }
    try {
      await api("POST", "/v1/assistant/messages", {
        channel: activeChannel,
        content: text,
        model: modelSelect.value,
      });
    } catch (err) {
      toast(err.message, "error");
    }
  };

  const renderMessages = (messages) => {
    clear(msgListEl);
    if (messages.length === 0 && pendingApprovals.length === 0) {
      msgListEl.appendChild(
        el("div", {
          className: "muted small",
          text: cfgView.configured
            ? `No messages in #${activeChannel} yet. Ask about Fleet agents, shared memory, Slack, or your Obsidian skills.`
            : "Configure your OpenRouter API key in Settings to start chatting.",
        }),
      );
    }
    for (const m of messages) {
      const isUser = m.role === "user";
      const bubble = el("div", { className: `chat-msg ${isUser ? "chat-msg-user" : "chat-msg-assistant"}` });
      bubble.appendChild(renderMarkdownDOM(m.content));
      if (Array.isArray(m.widgets)) {
        for (const w of m.widgets) {
          const wNode = renderWidgetNode(w, (act) => sendMessage(act));
          if (wNode) {
            bubble.appendChild(wNode);
          }
        }
      }
      const footer = formatMessageFooter(m);
      if (footer) {
        bubble.appendChild(el("div", { className: "chat-msg-footer mono", text: footer }));
      }
      msgListEl.appendChild(bubble);
    }
    for (const apr of pendingApprovals) {
      msgListEl.appendChild(renderApprovalCard(apr, loadChannelData));
    }
    msgListEl.appendChild(streamBox);
    msgListEl.scrollTop = msgListEl.scrollHeight;
  };

  const renderChannelBar = () => {
    clear(channelListEl);
    for (const ch of channels) {
      const active = ch.id === activeChannel;
      channelListEl.appendChild(
        el("button", {
          type: "button",
          className: `btn btn-sm ${active ? "btn-primary" : ""}`,
          text: ch.title || `#${ch.id}`,
          onclick: () => ctx.navigate(`#chat/${ch.id}`),
        }),
      );
    }
    channelListEl.appendChild(
      el("button", {
        type: "button",
        className: "btn btn-sm btn-ghost",
        text: "+ Channel",
        onclick: () => {
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
        },
      }),
    );
  };

  sendBtn.addEventListener("click", () => sendMessage(null));
  inputEl.addEventListener("keydown", (ev) => {
    if (ev.key === "Enter" && !ev.shiftKey) {
      ev.preventDefault();
      sendMessage(null);
    }
  });

  const actions = el(
    "div",
    { className: "actions-row" },
    statusPill,
    el("button", {
      type: "button",
      className: "btn btn-sm",
      text: "Reset Context",
      title: "Start a fresh LLM context window while keeping scrollback",
      onclick: async () => {
        try {
          await api("POST", `/v1/assistant/channels/${encodeURIComponent(activeChannel)}/reset`);
          toast(`Context reset for #${activeChannel}`, "ok");
        } catch (err) {
          toast(err.message, "error");
        }
      },
    }),
    el("button", {
      type: "button",
      className: "btn btn-sm",
      text: "Settings",
      onclick: () => openAssistantSettingsModal(() => ctx.refresh()),
    }),
  );

  renderChannelBar();
  const shell = el(
    "div",
    { className: "chat-shell" },
    channelListEl,
    msgListEl,
    el(
      "div",
      { className: "chat-composer" },
      inputEl,
      el("div", { className: "chat-composer-bar" }, modelSelect, sendBtn),
    ),
  );
  main.appendChild(section("Assistant Chat", { subtitle: cfgView.configured ? `Model: ${(cfgView.models && cfgView.models.default_chat) || ""}` : "Not configured", actions }, shell));

  await loadChannelData();

  let streamText = "";
  const es = new EventSource("/v1/assistant/stream");
  const setStatus = (txt, variant) => {
    statusPill.textContent = txt;
    statusPill.className = `badge badge-${variant}`;
  };
  es.addEventListener("ready", () => setStatus("live", "ok"));
  es.onerror = () => setStatus("reconnecting", "warn");
  es.addEventListener("run_start", (ev) => {
    const d = JSON.parse(ev.data || "{}");
    if (d.channel_id !== activeChannel) {
      return;
    }
    streamText = "";
    streamMeta.textContent = `Running (${(d.data && d.data.model) || ""})…`;
    clear(streamBody);
    streamBox.classList.remove("hidden");
    msgListEl.scrollTop = msgListEl.scrollHeight;
  });
  es.addEventListener("tool_start", (ev) => {
    const d = JSON.parse(ev.data || "{}");
    if (d.channel_id === activeChannel) {
      streamMeta.textContent = `Calling ${(d.data && d.data.name) || "tool"}…`;
    }
  });
  es.addEventListener("delta", (ev) => {
    const d = JSON.parse(ev.data || "{}");
    if (d.channel_id !== activeChannel) {
      return;
    }
    streamText += (d.data && d.data.delta) || "";
    clear(streamBody);
    streamBody.appendChild(renderMarkdownDOM(streamText));
    msgListEl.scrollTop = msgListEl.scrollHeight;
  });
  const onReloadEvent = (ev) => {
    const d = JSON.parse(ev.data || "{}");
    if (d.channel_id === activeChannel) {
      streamBox.classList.add("hidden");
      streamText = "";
      loadChannelData();
    }
  };
  es.addEventListener("message_created", onReloadEvent);
  es.addEventListener("message_done", onReloadEvent);
  es.addEventListener("approval", onReloadEvent);
  es.addEventListener("approval_decided", onReloadEvent);
  es.addEventListener("error", (ev) => {
    if (!ev.data) {
      return;
    }
    const d = JSON.parse(ev.data || "{}");
    if (d.channel_id === activeChannel && d.data && d.data.error) {
      streamBox.classList.add("hidden");
      toast(d.data.error, "error");
    }
  });

  const onWindowMsg = (ev) => {
    if (!ev.data || typeof ev.data !== "object") {
      return;
    }
    if (ev.data.type === "fleet-widget-ready") {
      for (const fr of msgListEl.querySelectorAll("iframe.gen-widget-frame")) {
        if (fr.contentWindow === ev.source && typeof fr._fleetRender === "function") {
          fr._fleetRender();
        }
      }
    } else if (ev.data.type === "fleet-widget-resize" && typeof ev.data.height === "number") {
      for (const fr of msgListEl.querySelectorAll("iframe.gen-widget-frame")) {
        if (fr.contentWindow === ev.source) {
          fr.style.height = `${Math.min(Math.max(ev.data.height, 64), 520)}px`;
        }
      }
    } else if (ev.data.type === "fleet-widget-action" && ev.data.action) {
      sendMessage(String(ev.data.action));
    }
  };
  window.addEventListener("message", onWindowMsg);

  state.routeCleanup = () => {
    es.close();
    window.removeEventListener("message", onWindowMsg);
  };
}
