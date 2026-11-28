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
      kv.appendChild(el("div", { className: "kv-value", text: typeof v === "string" ? v : JSON.stringify(v) }));
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
  if (w.kind === "files" && Array.isArray(data.files)) {
    const list = el("div", { className: "file-card-list" });
    for (const f of data.files) {
      if (!f || !f.url) {
        continue;
      }
      const sizeKB = f.size_bytes >= 1024 * 1024
        ? `${(f.size_bytes / (1024 * 1024)).toFixed(1)} MB`
        : f.size_bytes >= 1024
          ? `${(f.size_bytes / 1024).toFixed(1)} KB`
          : `${f.size_bytes || 0} B`;
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
          el("div", { className: "small muted", text: `${sizeKB} · ${f.mime_type || "file"}` }),
        ),
        el("a", { className: "btn btn-sm", href: f.url, download: f.name || "file", text: "Download" }),
      );
      list.appendChild(row);
    }
    card.appendChild(list);
    return card;
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
  let escArgs = {};
  if (isEscalation) {
    try {
      escArgs = JSON.parse(apr.args_json || "{}");
    } catch {
      escArgs = {};
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
  const discCfg = cfg.discord || {};
  const discEnabledChk = el("input", { type: "checkbox", checked: Boolean(discCfg.enabled) });
  const discAutoSyncChk = el("input", { type: "checkbox", checked: discCfg.auto_sync_channels !== false });
  const discAllowApprovalsChk = el("input", { type: "checkbox", checked: discCfg.allow_approvals !== false });
  const discMirrorWebChk = el("input", { type: "checkbox", checked: discCfg.mirror_web_turns !== false });
  const discTokenRefIn = el("input", { type: "text", value: discCfg.bot_token_op_ref || "", placeholder: "op://Fleet-Agent/Discord/bot_token" });
  const discUserIDIn = el("input", { type: "text", value: discCfg.authorized_user_id || "", placeholder: "18-digit Discord User ID" });
  const discGuildIDIn = el("input", { type: "text", value: discCfg.guild_id || "", placeholder: "Auto-detected if in 1 server" });
  const discCategoryIn = el("input", { type: "text", value: discCfg.category_name || "Fleet Assistant", placeholder: "Fleet Assistant" });
  const discBaseURLIn = el("input", { type: "text", value: discCfg.fleet_base_url || "", placeholder: "https://fleet.example.com" });
  const discChannelsIn = el("textarea", {
    rows: "4",
    className: "mono",
    value: JSON.stringify(discCfg.channels || {}, null, 2),
    placeholder: `{"general":"123456789012345678","fleet-ops":"234567890123456789","briefing":"345678901234567890"}`,
  });
  const sbCfg = cfg.sandbox || {};
  const sbEnabledChk = el("input", { type: "checkbox", checked: Boolean(sbCfg.enabled) });
  const sbCPUsIn = el("input", { type: "number", min: "1", max: "4", value: String(sbCfg.cpus ?? 2) });
  const sbMemIn = el("input", { type: "number", min: "256", max: "8192", step: "128", value: String(sbCfg.memory_mb ?? 2048) });
  const sbIdleIn = el("input", { type: "number", min: "1", max: "120", value: String(sbCfg.idle_minutes ?? 10) });
  const sbMaxFileIn = el("input", { type: "number", min: "1", max: "100", value: String(sbCfg.max_file_mb ?? 25) });
  const extraPkgsSummary = [
    ...(sbCfg.extra_apt_packages || []).map((p) => `apt:${p}`),
    ...(sbCfg.extra_pip_packages || []).map((p) => `pip:${p}`),
  ];
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
    el("div", { className: "small muted", text: "Optional if Two-Way Discord Bot is enabled below (bot already delivers interactive approvals & outbox alerts to mapped channels)." }),
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
    el("h4", { className: "mt-2", text: "Discord Bot (Two-Way 1-to-1 Sync & Approvals)" }),
    el("label", { className: "check-row" }, discEnabledChk, el("span", { text: "Enable outbound Discord Gateway bot (two-way chat)" })),
    el("label", { className: "check-row" }, discAutoSyncChk, el("span", { text: "Auto-manage Discord channels under category (requires Manage Channels)" })),
    el("label", { className: "check-row" }, discAllowApprovalsChk, el("span", { text: "Allow interactive Approve / Deny buttons in Discord" })),
    el("label", { className: "check-row" }, discMirrorWebChk, el("span", { text: "Mirror Fleet Console & timer turns to Discord (non-spoofed blockquote)" })),
    el(
      "div",
      { className: "grid-2" },
      field("Bot Token 1Password Ref", discTokenRefIn),
      field("Authorized Discord User ID", discUserIDIn),
      field("Discord Server ID (optional if 1 server)", discGuildIDIn),
      field("Category Name", discCategoryIn),
      field("Fleet Console Base URL (deep links)", discBaseURLIn),
    ),
    field("1-to-1 Channel Map (auto-populated or JSON override)", discChannelsIn),
    el(
      "div",
      { className: "actions-row" },
      el("button", {
        type: "button",
        className: "btn",
        text: "Sync Discord Channels",
        onclick: async (ev) => {
          const btn = ev.currentTarget;
          btn.disabled = true;
          btn.textContent = "Syncing…";
          try {
            const res = await api("POST", "/v1/assistant/discord/sync");
            const s = res.sync || {};
            if (s.guild_id) {
              discGuildIDIn.value = s.guild_id;
            }
            if (s.channels) {
              discChannelsIn.value = JSON.stringify(s.channels, null, 2);
            }
            toast(
              `Synced ${Object.keys(s.channels || {}).length} Discord channel(s) (created: ${(s.created_in_discord || []).length}, adopted: ${(s.adopted_in_discord || []).length})`,
              "ok",
            );
          } catch (err) {
            toast(err.message, "error");
          } finally {
            btn.disabled = false;
            btn.textContent = "Sync Discord Channels";
          }
        },
      }),
    ),
    el("h4", { className: "mt-2", text: "Sandboxed Computer (Apple Container)" }),
    el("label", { className: "check-row" }, sbEnabledChk, el("span", { text: "Enable isolated micro-VM sandbox for file reading & generation" })),
    el(
      "div",
      { className: "grid-2" },
      field("CPUs (1-4)", sbCPUsIn),
      field("Memory MB (256-8192)", sbMemIn),
      field("Idle Auto-Stop (mins)", sbIdleIn),
      field("Max File Size (MB)", sbMaxFileIn),
    ),
    extraPkgsSummary.length > 0
      ? el("div", { className: "small muted mono", text: `Custom packages: ${extraPkgsSummary.join(", ")}` })
      : el("div", { className: "small muted", text: "Pre-baked offline PDF/Office/CSV/chart toolkit; extra packages are approval-gated." }),
    el(
      "div",
      { className: "actions-row" },
      el("button", {
        type: "button",
        className: "btn",
        text: "Build Sandbox Image",
        onclick: async (ev) => {
          const btn = ev.currentTarget;
          btn.disabled = true;
          btn.textContent = "Building…";
          try {
            const res = await api("POST", "/v1/assistant/sandbox/build", { rebuild_base: false });
            toast(`Sandbox image ready: ${res.image}`, "ok");
          } catch (err) {
            toast(err.message, "error");
          } finally {
            btn.disabled = false;
            btn.textContent = "Build Sandbox Image";
          }
        },
      }),
      el("button", {
        type: "button",
        className: "btn",
        text: "Reset Sandbox",
        onclick: async () => {
          try {
            await api("POST", "/v1/assistant/sandbox/reset", { channel: localStorage.getItem("fleet_chat_channel") || "general", clear_output: true });
            toast("Sandbox reset", "ok");
          } catch (err) {
            toast(err.message, "error");
          }
        },
      }),
    ),
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
          let discordChannels = {};
          if (discChannelsIn.value.trim()) {
            try {
              discordChannels = JSON.parse(discChannelsIn.value);
            } catch {
              toast("Invalid Discord channel map JSON", "error");
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
            discord: {
              enabled: discEnabledChk.checked,
              bot_token_op_ref: discTokenRefIn.value.trim(),
              authorized_user_id: discUserIDIn.value.trim(),
              guild_id: discGuildIDIn.value.trim(),
              category_name: discCategoryIn.value.trim(),
              auto_sync_channels: discAutoSyncChk.checked,
              channels: discordChannels,
              allow_approvals: discAllowApprovalsChk.checked,
              mirror_web_turns: discMirrorWebChk.checked,
              fleet_base_url: discBaseURLIn.value.trim(),
            },
            sandbox: {
              ...sbCfg,
              enabled: sbEnabledChk.checked,
              cpus: Number.parseInt(sbCPUsIn.value || "2", 10) || 2,
              memory_mb: Number.parseInt(sbMemIn.value || "2048", 10) || 2048,
              idle_minutes: Number.parseInt(sbIdleIn.value || "10", 10) || 10,
              max_file_mb: Number.parseInt(sbMaxFileIn.value || "25", 10) || 25,
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
        const resp = await fetch("/v1/assistant/files", { method: "POST", body: fd, credentials: "same-origin" });
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
    const fileIDs = overrideText === null ? pendingFiles.map((f) => f.id) : [];
    if (!text && fileIDs.length === 0) {
      return;
    }
    if (overrideText === null) {
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

  const composerBox = el(
    "div",
    { className: "chat-composer" },
    el(
      "div",
      { className: "chat-composer-inner" },
      attachListEl,
      inputEl,
      fileInput,
      el(
        "div",
        { className: "chat-composer-bar" },
        el("div", { className: "chat-composer-left" }, attachBtn, modelSelect),
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
    if (type === "channels_updated") {
      api("GET", "/v1/assistant/channels")
        .then((res) => {
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
    state.activeChatChannel = "";
    document.documentElement.classList.remove("kb-open");
  };
}
