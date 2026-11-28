import { api, badge, clear, closeModal, el, field, openModal, section, state, toast } from "./lib.js";

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
      if (lines.length === 1 && /^#{1,3}\s+/.test(lines[0])) {
        wrap.appendChild(el("h4", { className: "chat-h" }, renderInlineDOM(lines[0].replace(/^#{1,3}\s+/, ""))));
        continue;
      }
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

function renderWidgetNode(w, sendPromptFn, onRetryFrontier, msgID) {
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
    let rendered = false;
    const sendPayload = () => {
      if (rendered) {
        return;
      }
      try {
        if (frame.contentWindow) {
          rendered = true;
          frame.contentWindow.postMessage({ type: "render", html: w.html || "", theme: currentTheme() }, "*");
        }
      } catch {
        rendered = false;
      }
    };
    frame._fleetRender = sendPayload;
    frame._fleetSetTheme = (theme) => {
      try {
        if (frame.contentWindow) {
          frame.contentWindow.postMessage({ type: "theme", theme }, "*");
        }
      } catch {
        // ignore detached frame
      }
    };
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
  if (w.kind === "markdown") {
    const md = typeof data === "string" ? data : data.markdown || data.content || data.text || "";
    card.appendChild(renderMarkdownDOM(md));
    return card;
  }
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
  if (w.kind === "frontier_retry") {
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
  card.appendChild(el("pre", { className: "code-block" }, el("code", { text: JSON.stringify(data, null, 2) })));
  return card;
}

function renderApprovalCard(apr, onDecided) {
  const isEscalation = apr.tool_name === "frontier_escalation";
  let escArgs = {};
  if (isEscalation) {
    try {
      escArgs = JSON.parse(apr.args_json || "{}");
    } catch {
      escArgs = {};
    }
  }
  const decide = async (decision, scope = "") => {
    try {
      await api("POST", `/v1/assistant/approvals/${encodeURIComponent(apr.id)}/decide`, { decision, scope });
      const msg = isEscalation
        ? decision === "approve"
          ? scope === "topic"
            ? "Frontier unlocked for this topic"
            : "Running Frontier once"
          : "Using default chat model"
        : decision === "approve"
          ? "Approved — resuming"
          : "Denied";
      toast(msg, decision === "approve" ? "ok" : "info");
      if (onDecided) {
        await onDecided();
      }
    } catch (err) {
      toast(err.message, "error");
    }
  };
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
      { className: "approval-card" },
      el(
        "div",
        { className: "approval-head" },
        badge("frontier", "warn"),
        el("strong", { text: apr.summary || `Use ${shortFrontier} for this request?` }),
      ),
      el("div", { className: "small muted mb-1", text: metaParts.join(" · ") }),
    );
    if (apr.status !== "pending") {
      card.appendChild(badge(apr.status, apr.status === "executed" || apr.status === "approved" ? "ok" : "danger"));
      return card;
    }
    card.appendChild(
      el(
        "div",
        { className: "actions-row" },
        el("button", { type: "button", className: "btn btn-sm btn-primary", text: `Use ${shortFrontier} (this topic)`, onclick: () => decide("approve", "topic") }),
        el("button", { type: "button", className: "btn btn-sm", text: "Once", onclick: () => decide("approve", "once") }),
        el("button", { type: "button", className: "btn btn-sm", text: `Use ${shortFallback}`, onclick: () => decide("deny") }),
      ),
    );
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
    el("pre", { className: "code-block small" }, el("code", { text: `${apr.tool_name}(${apr.args_json || "{}"})` })),
  );
  if (apr.status !== "pending") {
    card.appendChild(badge(apr.status, apr.status === "executed" || apr.status === "approved" ? "ok" : "danger"));
    return card;
  }
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

async function openAssistantSettingsModal(onSaved, onNavigate) {
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
  const routing = cfg.routing || {};
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
  const routingModeSel = el(
    "select",
    null,
    ...[
      { value: "hybrid", label: "Hybrid (Auto + 1-tap Retry + HITL)" },
      { value: "gate", label: "HITL Gate Only (Complex asks first)" },
      { value: "auto", label: "Unattended Auto (No HITL)" },
      { value: "off", label: "Off (Always Default Chat Model)" },
    ].map((opt) => el("option", { value: opt.value, text: opt.label, selected: (routing.mode || "hybrid") === opt.value })),
  );
  const grantTurnsIn = el("input", { type: "number", value: String(routing.grant_max_turns ?? 5), min: "1", max: "50" });
  const grantCostIn = el("input", { type: "number", step: "0.05", min: "0.05", value: String(routing.grant_max_cost_usd ?? 0.25) });
  const grantIdleIn = el("input", { type: "number", value: String(routing.grant_idle_minutes ?? 25), min: "1", max: "240" });
  const dailyCapIn = el("input", { type: "number", step: "0.25", min: "0", value: String(routing.daily_soft_cap_usd ?? 0) });
  const extraVaultsIn = el("textarea", {
    rows: "4",
    className: "mono",
    value: JSON.stringify(cfg.extra_vaults || [], null, 2),
    placeholder: `[{"name":"personal","path":"~/personal-vault","read_only":false,"auto_write_dirs":["Inbox"]}]`,
  });
  const mcpIn = el("textarea", { rows: "5", className: "mono", value: JSON.stringify(cfg.mcp_servers || [], null, 2) });
  const credsIn = el("textarea", {
    rows: "4",
    className: "mono",
    placeholder: `[{"alias":"linear","op_ref":"op://Fleet-Agent/Linear/token","allowed_hosts":["api.linear.app"],"require_approval":true}]`,
  });
  const notifCfg = cfg.notifications || {};
  const notifEnabledChk = el("input", { type: "checkbox", checked: Boolean(notifCfg.enabled) });
  const notifApprovalsChk = el("input", { type: "checkbox", checked: notifCfg.notify_approvals !== false });
  const notifBaseURLIn = el("input", { type: "text", value: notifCfg.fleet_base_url || "", placeholder: "https://fleet.example.com" });
  const notifWebhookRefIn = el("input", { type: "text", value: notifCfg.discord_webhook_op_ref || "", placeholder: "op://Fleet-Agent/Discord/webhook" });
  const notifMinPrioSel = el(
    "select",
    null,
    ...["low", "normal", "urgent"].map((p) => el("option", { value: p, text: p, selected: (notifCfg.min_priority || "normal") === p })),
  );
  const notifChannelRefsIn = el("textarea", {
    rows: "3",
    className: "mono",
    value: JSON.stringify(notifCfg.discord_channel_op_refs || {}, null, 2),
    placeholder: `{"approvals":"op://Fleet-Agent/Discord-Approvals/webhook"}`,
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
    field(
      "Mounted Extra Vaults (JSON)",
      extraVaultsIn,
      el("div", { className: "small muted", text: "Extra vaults are never scaffolded; writes outside auto_write_dirs require approval." }),
    ),
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
    el("h4", { className: "mt-2", text: "Frontier Auto-Routing & Topic Grants" }),
    el(
      "div",
      { className: "grid-2" },
      field("Routing Mode", routingModeSel),
      field("Topic Grant Max Turns", grantTurnsIn),
      field("Topic Grant Budget ($)", grantCostIn),
      field("Topic Grant Idle Timeout (mins)", grantIdleIn),
      field("Daily Spend Soft Cap ($, 0=off)", dailyCapIn),
    ),
    field("External stdio / HTTP MCP Servers (JSON)", mcpIn),
    field("Replace 1Password Credential Aliases (optional JSON)", credsIn),
    el("h4", { className: "mt-2", text: "Outbound Discord Notifications (Read-Only Alerts)" }),
    el("label", { className: "check-row" }, notifEnabledChk, el("span", { text: "Enable outbound Discord webhook alerts" })),
    el("label", { className: "check-row" }, notifApprovalsChk, el("span", { text: "Notify immediately on pending HITL approvals" })),
    el(
      "div",
      { className: "grid-2" },
      field("Discord Webhook 1Password Ref", notifWebhookRefIn),
      field("Fleet Console Base URL (deep links)", notifBaseURLIn),
      field("Minimum Outbox Priority", notifMinPrioSel),
    ),
    field("Per-Category / Channel Webhook Refs (optional JSON)", notifChannelRefsIn),
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
          let extraVaults = cfg.extra_vaults || [];
          try {
            extraVaults = JSON.parse(extraVaultsIn.value || "[]");
            if (!Array.isArray(extraVaults)) {
              throw new Error("extra_vaults must be a JSON array");
            }
          } catch {
            toast("Invalid extra vaults JSON", "error");
            return;
          }
          let discordChannelOPRefs = {};
          if (notifChannelRefsIn.value.trim()) {
            try {
              discordChannelOPRefs = JSON.parse(notifChannelRefsIn.value);
            } catch {
              toast("Invalid Discord channel refs JSON", "error");
              return;
            }
          }
          const payload = {
            enabled: enabledChk.checked,
            zdr: zdrChk.checked,
            base_url: cfg.base_url,
            data_collection: cfg.data_collection,
            vault_path: vaultIn.value.trim(),
            auto_write_dirs: cfg.auto_write_dirs,
            extra_vaults: extraVaults,
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
            routing: {
              ...routing,
              mode: routingModeSel.value,
              grant_max_turns: Number.parseInt(grantTurnsIn.value || "5", 10) || 5,
              grant_max_cost_usd: Number.parseFloat(grantCostIn.value || "0.25") || 0.25,
              grant_idle_minutes: Number.parseInt(grantIdleIn.value || "25", 10) || 25,
              daily_soft_cap_usd: Math.max(0, Number.parseFloat(dailyCapIn.value || "0") || 0),
            },
            mcp_servers: mcpServers,
            notifications: {
              enabled: notifEnabledChk.checked,
              fleet_base_url: notifBaseURLIn.value.trim(),
              discord_webhook_op_ref: notifWebhookRefIn.value.trim(),
              discord_channel_op_refs: discordChannelOPRefs,
              notify_approvals: notifApprovalsChk.checked,
              min_priority: notifMinPrioSel.value,
            },
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
      el("button", {
        type: "button",
        className: "btn",
        text: "Test Discord Alert",
        onclick: async () => {
          try {
            await api("POST", "/v1/assistant/notifications/test");
            toast("Test Discord alert sent", "ok");
          } catch (err) {
            toast(err.message, "error");
          }
        },
      }),
      el("button", {
        type: "button",
        className: "btn",
        text: "Long-Term Memory",
        onclick: () => {
          closeModal();
          if (onNavigate) {
            onNavigate("#skills/memory");
          } else {
            window.location.hash = "#skills/memory";
          }
        },
      }),
    ),
    doctorBox,
    el(
      "div",
      { className: "gen-mini-row mt-3" },
      el("h4", { className: "m-0", text: `Timers (${(timersData.timers || []).length})` }),
      el("button", {
        type: "button",
        className: "btn btn-sm",
        text: "+ Weekday Morning Briefing (8am)",
        onclick: async () => {
          try {
            await api("POST", "/v1/assistant/timers", {
              channel_id: "briefing",
              name: "morning-briefing",
              schedule_kind: "cron",
              cron_expr: "0 8 * * 1-5",
              prompt: "Morning briefing: check waiting Fleet agents, unread Slack channels, and today's calendar/vault tasks, then send a concise summary.",
            });
            toast("Added 8:00 AM weekday briefing timer", "ok");
            closeModal();
            openAssistantSettingsModal(onSaved, onNavigate);
          } catch (err) {
            toast(err.message, "error");
          }
        },
      }),
    ),
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
  const streamBody = el("div", { className: "chat-md" });
  streamBox.append(streamMeta, streamBody);
  const msgInnerEl = el("div", { className: "chat-messages-inner" }, emptyBox, msgItemsEl, approvalsBox, streamBox);
  const msgListEl = el("div", { className: "chat-messages" }, msgInnerEl);
  const statusPill = badge("connecting", "neutral");
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
    placeholder: "Message assistant… (/haiku, /mini, /sol, /sonnet, /neon)",
  });
  const sendBtn = el("button", { type: "button", className: "btn btn-sm btn-primary", text: "Send" });

  const resizeComposer = () => {
    inputEl.style.height = "auto";
    inputEl.style.height = `${Math.min(inputEl.scrollHeight, 128)}px`;
  };
  inputEl.addEventListener("input", resizeComposer);

  const scrollMessagesToBottom = () => {
    msgListEl.scrollTop = msgListEl.scrollHeight;
  };

  let pendingApprovals = [];
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
    const [msgRes, aprRes] = await Promise.all([
      api("GET", `/v1/assistant/messages?channel=${encodeURIComponent(activeChannel)}&limit=100`),
      api("GET", "/v1/assistant/approvals?status=pending&limit=20"),
    ]);
    pendingApprovals = (aprRes.approvals || []).filter((a) => a.channel_id === activeChannel);
    updateGrantPill(msgRes.grant || null);
    renderMessages(msgRes.messages || []);
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
    const text = (overrideText !== null ? String(overrideText) : inputEl.value).trim();
    if (!text) {
      return;
    }
    if (overrideText === null) {
      inputEl.value = "";
      resizeComposer();
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

  const renderedMsgs = new Map();
  const buildMessageBubble = (m) => {
    const isUser = m.role === "user";
    const bubble = el("div", { className: `chat-msg ${isUser ? "chat-msg-user" : "chat-msg-assistant"}` });
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
    scrollMessagesToBottom();
  };

  sendBtn.addEventListener("click", () => sendMessage(null));
  inputEl.addEventListener("keydown", (ev) => {
    if (ev.key === "Enter" && !ev.shiftKey) {
      ev.preventDefault();
      sendMessage(null);
    }
  });

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
            streamBox.classList.add("hidden");
            streamText = "";
            await loadChannelData();
            toast("Chat cleared — long-term memory preserved", "ok");
          } catch (err) {
            toast(err.message, "error");
          }
        },
      }),
      el("button", {
        type: "button",
        className: "btn btn-sm",
        text: "Settings",
        onclick: () => openAssistantSettingsModal(() => ctx.refresh(), (h) => ctx.navigate(h)),
      }),
    ),
  );

  const shell = el(
    "div",
    { className: "chat-shell" },
    topbar,
    msgListEl,
    el(
      "div",
      { className: "chat-composer" },
      el(
        "div",
        { className: "chat-composer-inner" },
        inputEl,
        el("div", { className: "chat-composer-bar" }, modelSelect, sendBtn),
      ),
    ),
  );
  main.appendChild(shell);

  await loadChannelData();

  const kbTimers = new Set();
  const syncViewport = () => {
    const vv = window.visualViewport;
    const vh = vv ? vv.height : window.innerHeight;
    const vt = vv ? vv.offsetTop : 0;
    const kbOpen = vv ? window.innerHeight - vv.height > 110 : false;
    const nearBottom = msgListEl.scrollHeight - msgListEl.scrollTop - msgListEl.clientHeight < 96;
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

  let streamText = "";
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
      streamText = "";
      const gt = d.data && d.data.grant_turn ? ` · topic ${d.data.grant_turn}/${d.data.grant_max_turns || 5}` : "";
      streamMeta.textContent = `Running (${(d.data && d.data.model) || ""}${gt})…`;
      clear(streamBody);
      streamBox.classList.remove("hidden");
      scrollMessagesToBottom();
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
      clear(streamBody);
      streamBody.appendChild(renderMarkdownDOM(streamText));
      scrollMessagesToBottom();
      return;
    }
    if (type === "message_created" || type === "message_done" || type === "approval" || type === "approval_decided" || type === "channel_reset" || type === "grant_updated") {
      if (d.channel_id === activeChannel) {
        streamBox.classList.add("hidden");
        streamText = "";
        loadChannelData();
      }
      return;
    }
    if (type === "error" && d.channel_id === activeChannel && d.data && d.data.error) {
      streamBox.classList.add("hidden");
      toast(d.data.error, "error");
    }
  };
  window.addEventListener("fleet-sse", onFleetSSE);

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
          fr.style.height = `${Math.min(Math.max(ev.data.height, 48), 640)}px`;
        }
      }
    } else if (ev.data.type === "fleet-widget-action" && ev.data.action) {
      sendMessage(String(ev.data.action));
    }
  };
  window.addEventListener("message", onWindowMsg);

  const syncWidgetThemes = () => {
    const theme = currentTheme();
    for (const fr of msgListEl.querySelectorAll("iframe.gen-widget-frame")) {
      if (typeof fr._fleetSetTheme === "function") {
        fr._fleetSetTheme(theme);
      }
    }
  };
  window.addEventListener("fleet-theme-change", syncWidgetThemes);
  const darkMedia = window.matchMedia ? window.matchMedia("(prefers-color-scheme: dark)") : null;
  if (darkMedia && typeof darkMedia.addEventListener === "function") {
    darkMedia.addEventListener("change", syncWidgetThemes);
  }

  state.routeCleanup = () => {
    window.removeEventListener("fleet-sse", onFleetSSE);
    window.removeEventListener("message", onWindowMsg);
    window.removeEventListener("fleet-theme-change", syncWidgetThemes);
    if (darkMedia && typeof darkMedia.removeEventListener === "function") {
      darkMedia.removeEventListener("change", syncWidgetThemes);
    }
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
    document.documentElement.classList.remove("kb-open");
  };
}
