import { api, badge, clear, el, formatDuration, relTime, shortDate, state, toast } from "./lib.js";
import { openAgentInspectModal, openQuickMessageModal } from "./agents.js";

function summaryCard(title, value, subtitle, onClick, badgeNode = null) {
  return el(
    "div",
    {
      className: "card card-clickable",
      onclick: onClick,
      tabindex: "0",
      onkeydown: (ev) => {
        if (ev.key === "Enter" || ev.key === " ") {
          ev.preventDefault();
          onClick();
        }
      },
    },
    el("div", { className: "card-header" }, el("h3", { className: "card-title", text: title }), badgeNode),
    el("div", { className: "stat-value", text: String(value) }),
    el("div", { className: "stat-sub", text: subtitle }),
  );
}

function buildAlerts(ov, navigate) {
  const stack = el("div", { className: "alerts-stack" });
  const pushAlert = (variant, text, btnLabel, hash) => {
    stack.appendChild(
      el(
        "div",
        { className: `alert-banner alert-${variant}` },
        el("span", { text }),
        el("button", { type: "button", className: "btn btn-sm", text: btnLabel, onclick: () => navigate(hash) }),
      ),
    );
  };

  if (ov.agents && ov.agents.waiting > 0) {
    pushAlert(
      "warn",
      `${ov.agents.waiting} agent session(s) waiting for permission or user question.`,
      "View Waiting",
      "#agents/waiting",
    );
  }
  if (ov.memory && ov.memory.pending_proposals > 0) {
    pushAlert(
      "info",
      `${ov.memory.pending_proposals} memory curator proposal(s) awaiting review.`,
      "Review Proposals",
      "#memory/proposals",
    );
  }
  if (ov.devices && ov.devices.expiring_soon > 0) {
    pushAlert(
      "warn",
      `${ov.devices.expiring_soon} active device certificate(s) expire within 30 days.`,
      "Manage Devices",
      "#devices",
    );
  }
  if (ov.devices && ov.devices.outdated > 0) {
    pushAlert(
      "warn",
      `${ov.devices.outdated} Mac device(s) have not yet synced to the latest published binary.`,
      "View Devices",
      "#devices",
    );
  }
  if (ov.curator && ov.curator.last_error) {
    pushAlert("danger", `Curator last error: ${ov.curator.last_error}`, "Open Curator", "#memory/curator");
  } else if (ov.curator && ov.curator.failed_jobs > 0) {
    pushAlert(
      "warn",
      `${ov.curator.failed_jobs} memory curation job(s) failed after retries.`,
      "Inspect Curator",
      "#memory/curator",
    );
  }
  return stack.childElementCount > 0 ? stack : null;
}

function renderWaitingCard(waiting, ctx) {
  const card = el(
    "div",
    { className: "card" },
    el(
      "div",
      { className: "card-header" },
      el("h3", { className: "card-title", text: "Waiting Agents" }),
      el("a", { href: "#agents/waiting", className: "small", text: "All waiting" }),
    ),
  );
  if (!waiting || waiting.length === 0) {
    card.appendChild(el("div", { className: "muted small", text: "No agents currently waiting for permission or user input." }));
    return card;
  }
  for (const ag of waiting.slice(0, 6)) {
    const pending = ag.pending_action || {};
    const detail = [pending.tool, pending.detail].filter(Boolean).join(": ");
    const waitText = ag.waiting_seconds != null ? formatDuration(ag.waiting_seconds) : "";
    card.appendChild(
      el(
        "div",
        { className: "message-card" },
        el(
          "div",
          { className: "message-meta" },
          el(
            "span",
            null,
            el("strong", { text: ag.name || ag.session_id }),
            " · ",
            el("span", { className: "mono", text: `${ag.device}/${ag.harness}` }),
          ),
          badge(`${ag.state}${waitText ? ` (${waitText})` : ""}`, "warn"),
        ),
        detail ? el("div", { className: "small mono mb-2", text: detail }) : null,
        ag.title ? el("div", { className: "small muted mb-2", text: ag.title }) : null,
        el(
          "div",
          { className: "actions-row" },
          el("button", {
            type: "button",
            className: "btn btn-sm",
            text: "Inspect",
            onclick: () => openAgentInspectModal(ag.session_id, () => ctx.refresh()),
          }),
          el("button", {
            type: "button",
            className: "btn btn-sm",
            text: "Message",
            onclick: () => openQuickMessageModal(ag.session_id, {}, () => ctx.refresh()),
          }),
        ),
      ),
    );
  }
  return card;
}

function formatMsgTarget(m) {
  if (m.to_session) {
    return m.to_session;
  }
  if (m.to_device) {
    return `device:${m.to_device}`;
  }
  if (m.to_repo) {
    return `repo:${m.to_repo}`;
  }
  return "all";
}

function renderMessagesCard(messages) {
  const card = el(
    "div",
    { className: "card" },
    el(
      "div",
      { className: "card-header" },
      el("h3", { className: "card-title", text: "Recent Fleet Messages" }),
      el("a", { href: "#agents/messages", className: "small", text: "Open Messages" }),
    ),
  );
  if (!messages || messages.length === 0) {
    card.appendChild(el("div", { className: "muted small", text: "No agent messages recorded yet." }));
    return card;
  }
  for (const m of messages.slice(0, 6)) {
    const sender = m.from_name ? `${m.from_name} (${m.from_device})` : m.from_session;
    const snippet = m.body.length > 180 ? `${m.body.slice(0, 180)}…` : m.body;
    card.appendChild(
      el(
        "div",
        { className: "message-card" },
        el(
          "div",
          { className: "message-meta" },
          el("span", null, el("strong", { text: sender }), " → ", el("span", { className: "mono", text: formatMsgTarget(m) })),
          el("span", { text: relTime(m.created_at) }),
        ),
        el("div", { className: "message-body small", text: snippet }),
      ),
    );
  }
  return card;
}

export async function renderOverview(root, ctx) {
  const ov = state.overview;
  if (!ov) {
    root.appendChild(el("div", { className: "loading-box", text: "Loading overview…" }));
    return;
  }

  let waiting = [];
  let messages = [];
  try {
    const [wRes, mRes] = await Promise.all([
      api("GET", "/v1/agents?state=waiting"),
      api("GET", "/v1/agents/messages?limit=8"),
    ]);
    waiting = wRes.agents || [];
    messages = mRes.messages || [];
  } catch (err) {
    toast(`Failed loading overview details: ${err.message}`, "warn");
  }

  clear(root);
  root.appendChild(
    el(
      "div",
      { className: "page-header" },
      el(
        "div",
        null,
        el("h1", { className: "page-title", text: "Fleet Overview" }),
        el("p", {
          className: "page-subtitle",
          text: `Signed in as ${ov.caller.name} (${ov.caller.ip}, ${ov.caller.group}) · Lighthouse ${ov.server.version} (${ov.server.commit})`,
        }),
      ),
    ),
  );

  const alerts = buildAlerts(ov, ctx.navigate);
  if (alerts) {
    root.appendChild(alerts);
  }

  const cards = el("div", { className: "cards-grid" });
  if (state.isAdmin && ov.devices) {
    cards.appendChild(
      summaryCard(
        "Devices",
        `${ov.devices.active} active`,
        `${ov.devices.admins} admin · ${ov.devices.managed} managed · ${ov.devices.mobiles} mobile · ${ov.devices.revoked} revoked`,
        () => ctx.navigate("#devices"),
        badge("admin", "info"),
      ),
    );
  }
  cards.appendChild(
    summaryCard(
      "AI Agents",
      `${ov.agents.live} live`,
      `${ov.agents.working} working · ${ov.agents.waiting} waiting · ${ov.agents.idle} idle · ${ov.agents.listening} listening`,
      () => ctx.navigate("#agents"),
      ov.agents.waiting > 0 ? badge(`${ov.agents.waiting} waiting`, "warn") : badge(`${ov.agents.stale} stale`, "neutral"),
    ),
  );
  cards.appendChild(
    summaryCard(
      "Shared Memory",
      `${ov.memory.active} active`,
      `${ov.memory.always_count} always · ${ov.memory.searchable_count} searchable · ${(ov.memory.scopes || []).length} scopes · ${ov.memory.total_revisions} rev`,
      () => ctx.navigate("#memory"),
      ov.memory.pending_proposals > 0 ? badge(`${ov.memory.pending_proposals} proposals`, "info") : null,
    ),
  );
  const curLabel = ov.curator.enabled ? "Enabled" : ov.curator.configured ? "Disabled" : "Not set";
  cards.appendChild(
    summaryCard(
      "LLM Curator",
      curLabel,
      `${ov.curator.model || "no model"} · ${ov.curator.pending_jobs} queued · ${ov.curator.processed_count} processed`,
      () => ctx.navigate("#memory/curator"),
      badge(curLabel, ov.curator.enabled ? "ok" : "neutral"),
    ),
  );
  if (state.isAdmin && ov.slacker) {
    const slk = ov.slacker;
    const slkLabel = slk.enabled ? (slk.listener_connected ? "Online" : "Enabled") : slk.configured ? "Disabled" : "Not set";
    cards.appendChild(
      summaryCard(
        "Slacker",
        slkLabel,
        `${slk.channels || 0} channel(s) · ${slk.queued_jobs || 0} queued · ${slk.failed_jobs || 0} failed`,
        () => ctx.navigate("#slacker"),
        badge(slkLabel, slk.listener_connected ? "ok" : slk.enabled ? "warn" : "neutral"),
      ),
    );
  }
  if (state.isAdmin && ov.ca) {
    cards.appendChild(
      summaryCard(
        "Lighthouse & PKI",
        `CA ${ov.ca.ca_id}`,
        `${(ov.settings && ov.settings.lighthouse_host) || ov.server.lighthouse_ip}:${(ov.settings && ov.settings.lighthouse_port) || "443"} · expires ${shortDate(ov.ca.not_after)} · up ${formatDuration(ov.server.uptime_seconds)}`,
        () => ctx.navigate("#system"),
        ov.ddns && ov.ddns.configured ? badge("DDNS on", "ok") : badge("DDNS off", "neutral"),
      ),
    );
  }
  root.appendChild(cards);

  root.appendChild(
    el("div", { className: "split-grid" }, renderWaitingCard(waiting, ctx), renderMessagesCard(messages)),
  );
}
