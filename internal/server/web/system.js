import { api, badge, cell, clear, confirmDialog, el, field, formatBytes, formatDuration, makeCopyBtn, relTime, section, shortDate, state, toast } from "./lib.js";

const sysState = {
  logName: "serve",
  logLines: 150,
  logFilter: "",
  ddnsLive: null,
};

function formatWatchdogSummary(nebStatus) {
  const wd = nebStatus && nebStatus.watchdog;
  if (!wd || !wd.enabled) {
    return "inactive";
  }
  const base = `active (≥${wd.burst_threshold} relay errors/${wd.burst_window_sec}s)`;
  if (wd.last_error) {
    return `${base} · error: ${wd.last_error}`;
  }
  if (wd.recent_errors > 0) {
    return `${base} · ${wd.recent_errors} recent`;
  }
  return base;
}

function formatNebulaRestartSummary(nebStatus) {
  if (!nebStatus) {
    return "-";
  }
  if (nebStatus.pending && nebStatus.pending_request) {
    const req = nebStatus.pending_request;
    return `Pending (${req.reason || "manual"} by ${req.by || "admin"}, ${relTime(req.requested_at)})`;
  }
  const lr = nebStatus.last_restart;
  if (!lr) {
    return "none";
  }
  const status = lr.ok ? "ok" : `failed (${lr.error || "error"})`;
  return `${relTime(lr.restarted_at)} · ${status} (${lr.reason || "manual"} by ${lr.by || "admin"})`;
}

function renderLighthouseCard(ov, nebStatus, onRefresh) {
  const s = ov.server || {};
  const ca = ov.ca || {};
  const cfg = ov.settings || {};

  const restartBtn = el("button", {
    type: "button",
    className: "btn btn-sm btn-danger",
    text: nebStatus && nebStatus.pending ? "Restart Queued…" : "Restart Nebula",
    disabled: Boolean(nebStatus && nebStatus.pending),
    onclick: async () => {
      const ok = await confirmDialog(
        "Restart Nebula",
        "Restart the lighthouse Nebula daemon now? This flushes stale relay mappings; relayed peers will re-handshake for ~1–2 seconds.",
        "Restart Nebula",
        true,
      );
      if (!ok) {
        return;
      }
      restartBtn.disabled = true;
      try {
        await api("POST", "/v1/nebula/restart", { reason: "manual (web ui)" });
        toast("Nebula restart queued on lighthouse", "ok");
        if (onRefresh) {
          onRefresh();
          setTimeout(onRefresh, 2000);
        }
      } catch (err) {
        toast(err.message, "error");
        restartBtn.disabled = false;
      }
    },
  });

  return el(
    "article",
    { className: "card" },
    el(
      "div",
      { className: "card-header" },
      el("h3", { className: "card-title", text: "Lighthouse & PKI" }),
      el("div", { className: "actions-row" }, badge(`CA ${ca.ca_id || "-"}`, "info"), restartBtn),
    ),
    el(
      "dl",
      { className: "kv-list" },
      el("dt", { className: "kv-label", text: "Public Host" }),
      el("dd", { className: "kv-value mono", text: `${cfg.lighthouse_host || "-"}:${cfg.lighthouse_port || "443"}/udp` }),
      el("dt", { className: "kv-label", text: "LAN IP" }),
      el("dd", { className: "kv-value mono", text: cfg.lighthouse_lan_ip || "-" }),
      el("dt", { className: "kv-label", text: "Mesh Entry" }),
      el("dd", { className: "kv-value mono", text: `${s.lighthouse_ip}:${s.api_port} (${s.mesh_cidr})` }),
      el("dt", { className: "kv-label", text: "CA Fingerprint" }),
      el("dd", { className: "kv-value mono", text: ca.fingerprint || "-" }),
      el("dt", { className: "kv-label", text: "CA Expires" }),
      el("dd", { className: "kv-value", text: shortDate(ca.not_after) }),
      el("dt", { className: "kv-label", text: "Server Version" }),
      el("dd", { className: "kv-value mono", text: `${s.version} (${s.commit}) · ${s.os}/${s.arch}` }),
      el("dt", { className: "kv-label", text: "Built / Uptime" }),
      el("dd", { className: "kv-value", text: `${s.built_at || "dev"} · up ${formatDuration(s.uptime_seconds)}` }),
      el("dt", { className: "kv-label", text: "Relay Watchdog" }),
      el("dd", { className: "kv-value", text: formatWatchdogSummary(nebStatus) }),
      el("dt", { className: "kv-label", text: "Last Nebula Restart" }),
      el("dd", { className: "kv-value", text: formatNebulaRestartSummary(nebStatus) }),
    ),
  );
}

function renderBinariesCard(ov) {
  const m = ov.manifest;
  const card = el(
    "article",
    { className: "card" },
    el(
      "div",
      { className: "card-header" },
      el("h3", { className: "card-title", text: "Published Binaries" }),
      m ? badge(String(m.commit || "").slice(0, 7) || m.version, "ok") : badge("none", "neutral"),
    ),
  );
  if (!m || !m.binaries || Object.keys(m.binaries).length === 0) {
    card.appendChild(el("div", { className: "muted small", text: "No compiled binary manifest published in ~/.fleet/bin/." }));
    return card;
  }
  card.appendChild(
    el("div", { className: "small muted mb-2", text: `Version ${m.version} · Built ${m.built_at || "-"}` }),
  );
  const tbody = el("tbody");
  for (const [platform, entry] of Object.entries(m.binaries).sort()) {
    tbody.appendChild(
      el(
        "tr",
        null,
        cell("Platform", { className: "mono cell-primary", text: platform }),
        cell("File", { className: "mono small", text: entry.file }),
        cell("Size", { text: formatBytes(entry.size) }),
        cell("SHA-256", { className: "cell-actions" }, el("div", { className: "actions-row" }, el("span", { className: "mono small", text: `${entry.sha256.slice(0, 12)}…` }), makeCopyBtn(entry.sha256))),
      ),
    );
  }
  card.appendChild(
    el(
      "div",
      { className: "table-wrap" },
      el(
        "table",
        { className: "responsive-table" },
        el("thead", null, el("tr", null, el("th", { text: "Platform" }), el("th", { text: "File" }), el("th", { text: "Size" }), el("th", { text: "SHA-256" }))),
        tbody,
      ),
    ),
  );
  return card;
}

function renderDDNSCard(ddnsData, ov) {
  const card = el(
    "article",
    { className: "card" },
    el(
      "div",
      { className: "card-header" },
      el("h3", { className: "card-title", text: "Route 53 Dynamic DNS" }),
      badge(ddnsData.configured ? "configured" : "not configured", ddnsData.configured ? "ok" : "neutral"),
    ),
  );

  const liveArea = el("div", { className: "mt-2" });
  const renderLive = (d) => {
    clear(liveArea);
    if (!d) {
      return;
    }
    const items = [];
    if (d.public_ip) {
      items.push(`Public IP: ${d.public_ip}`);
    }
    if (d.dns_record) {
      items.push(`DNS Answer: ${d.dns_record}`);
    }
    if (typeof d.in_sync === "boolean") {
      items.push(d.in_sync ? "In Sync" : "Out of Sync");
    }
    if (d.live_error) {
      items.push(`Error: ${d.live_error}`);
    }
    liveArea.appendChild(
      el("div", { className: `alert-banner ${d.in_sync ? "alert-ok" : "alert-warn"}`, text: items.join(" · ") }),
    );
  };

  if (sysState.ddnsLive) {
    renderLive(sysState.ddnsLive);
  }

  if (ddnsData.configured) {
    const checkBtn = el("button", {
      type: "button",
      className: "btn btn-sm",
      text: "Check Live DNS",
      onclick: async () => {
        checkBtn.disabled = true;
        try {
          sysState.ddnsLive = await api("GET", "/v1/ddns?live=true");
          renderLive(sysState.ddnsLive);
        } catch (err) {
          toast(err.message, "error");
        } finally {
          checkBtn.disabled = false;
        }
      },
    });
    const syncBtn = el("button", {
      type: "button",
      className: "btn btn-sm btn-primary",
      text: "Sync DNS Now",
      onclick: async () => {
        syncBtn.disabled = true;
        try {
          const res = await api("POST", "/v1/ddns/sync", {});
          toast(res.changed ? `Updated DNS: ${res.before || "none"} -> ${res.after}` : `DNS already up to date (${res.after})`, "ok");
          sysState.ddnsLive = await api("GET", "/v1/ddns?live=true");
          renderLive(sysState.ddnsLive);
        } catch (err) {
          toast(err.message, "error");
        } finally {
          syncBtn.disabled = false;
        }
      },
    });

    card.appendChild(
      el(
        "dl",
        { className: "kv-list mb-2" },
        el("dt", { className: "kv-label", text: "Record" }),
        el("dd", { className: "kv-value mono", text: ddnsData.name }),
        el("dt", { className: "kv-label", text: "Zone / Profile" }),
        el("dd", { className: "kv-value mono", text: `${ddnsData.zone_id} (${ddnsData.profile})` }),
        el("dt", { className: "kv-label", text: "Nameservers" }),
        el("dd", { className: "kv-value mono", text: (ddnsData.nameservers || []).join(", ") || "-" }),
      ),
    );
    card.appendChild(el("div", { className: "actions-row" }, checkBtn, syncBtn));
    card.appendChild(liveArea);
  } else {
    card.appendChild(
      el("p", {
        className: "muted small m-0",
        text: "Route 53 DDNS is not configured. Run `fleet ddns setup --zone-id Z...` on the lighthouse after configuring AWS credentials.",
      }),
    );
  }

  const zoneIn = el("input", { type: "text", value: ddnsData.zone_id || "", placeholder: "Z1234567890ABC" });
  const nameIn = el("input", { type: "text", value: ddnsData.name || (ov.settings && ov.settings.lighthouse_host) || "", placeholder: "lighthouse.example.com" });
  const polOut = el("div", { className: "mt-2" });
  const genBtn = el("button", {
    type: "button",
    className: "btn btn-sm",
    text: "Generate IAM Policy",
    onclick: async () => {
      try {
        const pol = await api("GET", `/v1/ddns/policy?zone_id=${encodeURIComponent(zoneIn.value.trim())}&name=${encodeURIComponent(nameIn.value.trim())}`);
        const json = JSON.stringify(pol, null, 2);
        clear(polOut);
        polOut.appendChild(
          el("div", { className: "actions-row flex-end mb-2" }, makeCopyBtn(json, "Copy Policy JSON")),
        );
        polOut.appendChild(el("pre", { className: "code-block", text: json }));
      } catch (err) {
        toast(err.message, "error");
      }
    },
  });

  card.appendChild(
    el(
      "details",
      { className: "details-box" },
      el("summary", { text: "Route 53 Least-Privilege IAM Policy Generator" }),
      el(
        "div",
        { className: "form-grid mt-2" },
        el(
          "div",
          { className: "form-row-2" },
          field("Route 53 Zone ID", zoneIn),
          field("DNS Record Name", nameIn),
        ),
        el("div", { className: "actions-row flex-end" }, genBtn),
      ),
      polOut,
    ),
  );
  return card;
}

async function renderLogsCard(container) {
  clear(container);
  let logRes;
  try {
    logRes = await api("GET", `/v1/logs?name=${encodeURIComponent(sysState.logName)}&lines=${sysState.logLines}`);
  } catch (err) {
    container.appendChild(el("div", { className: "alert-banner alert-danger", text: err.message }));
    return;
  }

  const tabs = ["serve", "ddns", "slacker", "sync", "nebula"].map((name) =>
    el("button", {
      type: "button",
      className: `chip${sysState.logName === name ? " active" : ""}`,
      text: name,
      onclick: () => {
        sysState.logName = name;
        renderLogsCard(container);
      },
    }),
  );

  const linesSel = el(
    "select",
    {
      "aria-label": "Log lines count",
      onchange: (ev) => {
        sysState.logLines = Number.parseInt(ev.target.value, 10) || 150;
        renderLogsCard(container);
      },
    },
    ...[100, 150, 300, 500].map((n) => el("option", { value: String(n), text: `${n} lines`, selected: sysState.logLines === n })),
  );

  const preBox = el("pre", { className: "log-box" });
  const updateFiltered = () => {
    const q = sysState.logFilter.trim().toLowerCase();
    const lines = (logRes.lines || []).filter((ln) => !q || ln.toLowerCase().includes(q));
    preBox.textContent = lines.length > 0 ? lines.join("\n") : logRes.exists ? "(no matching log lines)" : "(log file does not exist yet)";
    preBox.scrollTop = preBox.scrollHeight;
  };

  const filterIn = el("input", {
    type: "search",
    className: "grow",
    "aria-label": "Filter log lines",
    placeholder: "Filter log lines…",
    value: sysState.logFilter,
    oninput: (ev) => {
      sysState.logFilter = ev.target.value;
      updateFiltered();
    },
  });

  container.appendChild(
    el(
      "article",
      { className: "card" },
      el(
        "div",
        { className: "card-header" },
        el("h3", { className: "card-title", text: "Service Log Tail" }),
        el("span", {
          className: "mono small muted",
          text: `${logRes.path} (${formatBytes(logRes.size || 0)}${logRes.updated_at ? `, ${relTime(logRes.updated_at)}` : ""})`,
        }),
      ),
      el(
        "div",
        { className: "toolbar" },
        el("div", { className: "chips-row" }, tabs),
        linesSel,
        filterIn,
        el("button", { type: "button", className: "btn btn-sm", text: "Refresh Log", onclick: () => renderLogsCard(container) }),
        makeCopyBtn((logRes.lines || []).join("\n"), "Copy"),
      ),
      logRes.error ? el("div", { className: "alert-banner alert-warn mb-2", text: logRes.error }) : null,
      preBox,
    ),
  );
  updateFiltered();
}

export async function renderSystem(root) {
  if (!state.isAdmin) {
    root.appendChild(el("div", { className: "empty-state", text: "Admin privileges required to inspect lighthouse system internals." }));
    return;
  }

  let ddnsData;
  let nebStatus = null;
  try {
    [ddnsData, nebStatus] = await Promise.all([
      api("GET", "/v1/ddns"),
      api("GET", "/v1/nebula/status"),
    ]);
  } catch (err) {
    clear(root);
    root.appendChild(el("div", { className: "alert-banner alert-danger", text: err.message }));
    return;
  }

  const ov = state.overview || {};
  clear(root);
  root.appendChild(
    el(
      "header",
      { className: "page-header" },
      el(
        "div",
        null,
        el("h1", { className: "page-title", text: "Lighthouse System" }),
        el("p", { className: "page-subtitle", text: "PKI, cross-compiled binaries, Route 53 DDNS, and service logs" }),
      ),
    ),
  );

  const infraWrap = el("div");
  const drawInfra = (st) => {
    clear(infraWrap);
    infraWrap.appendChild(
      el("div", { className: "split-grid" }, renderLighthouseCard(ov, st, refreshNebula), renderBinariesCard(ov)),
    );
  };
  const refreshNebula = async () => {
    try {
      const latest = await api("GET", "/v1/nebula/status");
      drawInfra(latest);
    } catch {
      // Ignore transient refresh failure right after daemon restart.
    }
  };
  drawInfra(nebStatus);

  root.appendChild(
    section(
      "Infrastructure & Binaries",
      { className: "ui-section" },
      infraWrap,
    ),
  );
  root.appendChild(section("Dynamic DNS", { className: "ui-section" }, renderDDNSCard(ddnsData, ov)));

  const logsContainer = el("div");
  root.appendChild(section("Lighthouse Logs", { className: "ui-section" }, logsContainer));
  await renderLogsCard(logsContainer);
}
