import { badge, el, relTime } from "./lib.js";

const HARNESS_META = [
  ["claude", "Claude Code"],
  ["codex", "Codex"],
  ["antigravity", "Antigravity"],
];

function formatResetIn(ts) {
  if (!ts) {
    return "";
  }
  const ms = Date.parse(ts);
  if (Number.isNaN(ms)) {
    return "";
  }
  const secs = Math.floor((ms - Date.now()) / 1000);
  if (secs <= 0) {
    return "resetting soon";
  }
  const days = Math.floor(secs / 86400);
  const rem = secs % 86400;
  const hours = Math.floor(rem / 3600);
  const mins = Math.floor((rem % 3600) / 60);
  if (days > 0) {
    return `resets in ${days}d ${hours}h`;
  }
  if (hours > 0) {
    return `resets in ${hours}h ${mins}m`;
  }
  return `resets in ${Math.max(1, mins)}m`;
}

function formatMetricSummary(metrics) {
  if (!metrics || typeof metrics !== "object") {
    return "";
  }
  const parts = [];
  if (metrics.model) {
    parts.push(String(metrics.model));
  }
  if (metrics.session_cost_usd != null) {
    parts.push(`$${metrics.session_cost_usd}`);
  }
  if (metrics.context_used_pct != null) {
    parts.push(`ctx ${metrics.context_used_pct}%`);
  }
  if (metrics.lifetime_tokens != null) {
    const n = Number(metrics.lifetime_tokens);
    parts.push(`${Number.isFinite(n) ? n.toLocaleString() : metrics.lifetime_tokens} lifetime tokens`);
  }
  if (metrics.current_streak_days != null) {
    parts.push(`${metrics.current_streak_days}d streak`);
  }
  if (metrics.conversations_24h != null) {
    parts.push(`${metrics.conversations_24h} convs/24h`);
  }
  if (metrics.steps_24h != null) {
    parts.push(`${metrics.steps_24h} steps/24h`);
  }
  if (metrics.active_conversations != null && Number(metrics.active_conversations) > 0) {
    parts.push(`${metrics.active_conversations} active`);
  }
  return parts.join(" · ");
}

function fillClass(pct) {
  if (pct >= 85) {
    return "fill-danger";
  }
  if (pct >= 60) {
    return "fill-warn";
  }
  return "fill-ok";
}

function statusVariant(st) {
  if (st === "ok") {
    return "ok";
  }
  if (st === "error") {
    return "danger";
  }
  return "neutral";
}

function renderUsageTile(id, label, item) {
  if (!item) {
    return el(
      "div",
      { className: "usage-tile" },
      el("div", { className: "usage-tile-head" }, el("strong", { text: label }), badge("no data", "neutral")),
      el("div", { className: "small muted mt-2", text: "No data reported yet." }),
    );
  }

  const headBadges = el("div", { className: "actions-row" });
  if (item.plan) {
    headBadges.appendChild(badge(item.plan, "info"));
  }
  headBadges.appendChild(badge(item.status || "ok", statusVariant(item.status)));

  const body = el("div", { className: "usage-tile-body" });
  const windows = Array.isArray(item.windows) ? item.windows : [];
  if (windows.length > 0) {
    for (const w of windows) {
      const pct = Math.max(0, Math.min(100, Number(w.used_percent) || 0));
      const resetTxt = formatResetIn(w.resets_at);
      body.appendChild(
        el(
          "div",
          { className: "usage-window" },
          el(
            "div",
            { className: "usage-window-head" },
            el("span", { className: "small", text: `${w.label}: ${Math.round(pct)}%` }),
            resetTxt ? el("span", { className: "small muted", text: resetTxt }) : null,
          ),
          el(
            "div",
            { className: "usage-bar" },
            el("div", {
              className: `usage-bar-fill ${fillClass(pct)}`,
              style: `width: ${pct}%`,
            }),
          ),
        ),
      );
    }
  } else {
    body.appendChild(
      el("div", {
        className: "small muted mt-1",
        text: item.detail || (item.status === "ok" ? "Activity tracked (no quota percentages)" : "Unavailable"),
      }),
    );
  }

  const metText = formatMetricSummary(item.metrics);
  if (metText) {
    body.appendChild(el("div", { className: "usage-metrics small muted", text: metText }));
  }
  if (windows.length > 0 && item.detail) {
    body.appendChild(el("div", { className: "small muted mt-1", text: item.detail }));
  }

  const metaParts = [item.device, item.source, item.updated_at ? relTime(item.updated_at) : ""].filter(Boolean);

  return el(
    "div",
    { className: "usage-tile", dataset: { harness: id } },
    el("div", { className: "usage-tile-head" }, el("strong", { text: label }), headBadges),
    body,
    metaParts.length > 0 ? el("div", { className: "usage-meta small muted", text: metaParts.join(" · ") }) : null,
  );
}

export function renderUsageCard(usageList) {
  const byHarness = new Map();
  for (const item of usageList || []) {
    if (item && item.harness) {
      byHarness.set(item.harness, item);
    }
  }

  const grid = el("div", { className: "usage-grid" });
  for (const [id, label] of HARNESS_META) {
    grid.appendChild(renderUsageTile(id, label, byHarness.get(id) || null));
  }

  return el(
    "div",
    { className: "card mb-3" },
    el(
      "div",
      { className: "card-header" },
      el("h3", { className: "card-title", text: "Harness Usage & Quotas" }),
      el("span", { className: "small muted", text: "Claude statusLine · Codex app-server · Antigravity" }),
    ),
    grid,
  );
}
