import { api, badge, clear, confirmDialog, el, makeCopyBtn, relTime, state, toast } from "./lib.js";

function renderStatusCard(statusRes) {
  const cfg = statusRes.config || {};
  const counts = statusRes.counts || {};
  const runtimes = statusRes.runtimes || [];
  const listener = runtimes.find((r) => r.role === "listener");
  const acctListeners = runtimes.filter((r) => r.role.startsWith("listener:"));
  const workers = runtimes.filter((r) => r.role.startsWith("worker:"));

  const card = el(
    "div",
    { className: "card mb-3" },
    el(
      "div",
      { className: "card-header" },
      el("h3", { className: "card-title", text: "Slacker Runtime" }),
      badge(cfg.enabled ? (listener && listener.connected ? "connected" : "enabled (offline)") : "disabled", cfg.enabled && listener && listener.connected ? "ok" : cfg.enabled ? "warn" : "neutral"),
    ),
    el(
      "dl",
      { className: "kv-list mb-2" },
      el("dt", { className: "kv-label", text: "Accounts" }),
      el("dd", {
        className: "kv-value mono",
        text: acctListeners.length > 0 ? acctListeners.map((r) => `${r.role.slice(9)} (${r.connected ? "online" : "offline"})`).join(", ") : `${Object.keys(cfg.accounts || {}).length} configured`,
      }),
      el("dt", { className: "kv-label", text: "Queue Counts" }),
      el("dd", { className: "kv-value", text: `${counts.queued || 0} queued · ${counts.claimed || 0} claimed · ${counts.launched || 0} launched · ${counts.failed || 0} failed` }),
      el("dt", { className: "kv-label", text: "Workers Seen" }),
      el("dd", {
        className: "kv-value mono",
        text: workers.length > 0 ? workers.map((w) => `${w.role.slice(7)} (${relTime(w.last_poll_at || w.updated_at)})`).join(", ") : "none yet",
      }),
    ),
  );
  if (listener && listener.last_error) {
    card.appendChild(el("div", { className: "alert-banner alert-warn", text: `Listener: ${listener.last_error}` }));
  }
  return card;
}

function renderAccountsCard(cfg, runtimes, ctx) {
  const accounts = Object.values(cfg.accounts || {}).sort((a, b) => a.id.localeCompare(b.id));
  const card = el(
    "div",
    { className: "card mb-3" },
    el("div", { className: "card-header" }, el("h3", { className: "card-title", text: "Slack Accounts" }), badge(`${accounts.length} account(s)`, "info")),
  );

  const idIn = el("input", { type: "text", value: accounts.length === 0 ? "default" : "", placeholder: "account id (e.g. default, work, personal)" });
  const enabledChk = el("input", { type: "checkbox", checked: true });
  const unmappedChk = el("input", { type: "checkbox" });
  const authUserIn = el("input", { type: "text", placeholder: "U0123456789 (or verify)" });
  const appTokIn = el("input", { type: "password", autocomplete: "off", placeholder: "xapp-..." });
  const userTokIn = el("input", { type: "password", autocomplete: "off", placeholder: "xoxp-..." });
  const botTokIn = el("input", { type: "password", autocomplete: "off", placeholder: "optional xoxb-... for agent sends" });
  const verifyChk = el("input", { type: "checkbox" });

  if (accounts.length > 0) {
    const tbody = el("tbody");
    for (const a of accounts) {
      const rt = runtimes.find((r) => r.role === `listener:${a.id}`);
      const stBadge = !a.enabled ? badge("disabled", "neutral") : rt && rt.connected ? badge("connected", "ok") : badge("enabled", "warn");
      tbody.appendChild(
        el(
          "tr",
          null,
          el("td", { className: "mono", text: a.id }),
          el("td", { className: "small", text: `${a.team_name || a.team_id || "-"} · ${a.authorized_user || a.authorized_user_id || "-"}` }),
          el("td", { className: "mono small", text: `app:${a.app_token_hint || "-"} · user:${a.user_token_hint || "-"}${a.bot_token_hint ? ` · bot:${a.bot_token_hint}` : ""}` }),
          el("td", null, stBadge, a.allow_unmapped_send ? " unmapped-send" : ""),
          el(
            "td",
            null,
            el(
              "div",
              { className: "actions-row" },
              el("button", {
                type: "button",
                className: "btn btn-sm",
                text: "Edit",
                onclick: () => {
                  idIn.value = a.id;
                  enabledChk.checked = Boolean(a.enabled);
                  unmappedChk.checked = Boolean(a.allow_unmapped_send);
                  authUserIn.value = a.authorized_user_id || "";
                  appTokIn.value = "";
                  appTokIn.placeholder = a.has_app_token ? `Keep (${a.app_token_hint})` : "xapp-...";
                  userTokIn.value = "";
                  userTokIn.placeholder = a.has_user_token ? `Keep (${a.user_token_hint})` : "xoxp-...";
                  botTokIn.value = "";
                  botTokIn.placeholder = a.has_bot_token ? `Keep (${a.bot_token_hint})` : "optional xoxb-...";
                },
              }),
              el("button", {
                type: "button",
                className: "btn btn-sm btn-danger",
                text: "Delete",
                onclick: async () => {
                  if (!(await confirmDialog("Delete Slack account", `Remove account '${a.id}' and its channel mappings?`, "Delete", true))) {
                    return;
                  }
                  await api("POST", "/v1/slacker/config", { delete_account: a.id });
                  toast(`Removed account ${a.id}`, "ok");
                  ctx.refresh();
                },
              }),
            ),
          ),
        ),
      );
    }
    card.appendChild(
      el(
        "div",
        { className: "table-wrap mb-3" },
        el("table", null, el("thead", null, el("tr", null, el("th", { text: "Account" }), el("th", { text: "Workspace / User" }), el("th", { text: "Tokens" }), el("th", { text: "Status" }), el("th", { text: "Actions" }))), tbody),
      ),
    );
  }

  const saveAcctBtn = el("button", { type: "submit", className: "btn btn-primary", text: "Save Account" });
  card.appendChild(
    el(
      "form",
      {
        className: "form-grid",
        onsubmit: async (ev) => {
          ev.preventDefault();
          const acctID = idIn.value.trim() || "default";
          saveAcctBtn.disabled = true;
          try {
            const body = {
              account: acctID,
              enabled: enabledChk.checked,
              allow_unmapped_send: unmappedChk.checked,
              authorized_user_id: authUserIn.value.trim(),
              verify: verifyChk.checked,
            };
            if (appTokIn.value.trim()) {
              body.app_token = appTokIn.value.trim();
            }
            if (userTokIn.value.trim()) {
              body.user_token = userTokIn.value.trim();
            }
            if (botTokIn.value.trim()) {
              body.bot_token = botTokIn.value.trim();
            }
            await api("POST", "/v1/slacker/config", body);
            toast(`Saved Slack account ${acctID}`, "ok");
            ctx.refresh();
          } catch (err) {
            toast(err.message, "error");
          } finally {
            saveAcctBtn.disabled = false;
          }
        },
      },
      el(
        "div",
        { className: "form-row-2" },
        el("div", { className: "field" }, el("label", { text: "Account ID" }), idIn),
        el("div", { className: "field" }, el("label", { text: "Authorized Slack User ID" }), authUserIn),
      ),
      el(
        "div",
        { className: "form-row-2" },
        el("div", { className: "field" }, el("label", { text: "App-Level Token (xapp-...)" }), appTokIn),
        el("div", { className: "field" }, el("label", { text: "User OAuth Token (xoxp-...)" }), userTokIn),
      ),
      el(
        "div",
        { className: "form-row-2" },
        el("div", { className: "field" }, el("label", { text: "Optional Bot Token (xoxb-...)" }), botTokIn),
        el(
          "div",
          { className: "field" },
          el("label", { text: "Options" }),
          el("div", { className: "actions-row" }, el("label", { className: "checkbox-row" }, enabledChk, "Enabled"), el("label", { className: "checkbox-row" }, unmappedChk, "Allow unmapped send"), el("label", { className: "checkbox-row" }, verifyChk, "Verify")),
        ),
      ),
      el("div", { className: "actions-row flex-end" }, saveAcctBtn),
    ),
  );
  return card;
}

export async function renderSlacker(root, ctx) {
  if (!state.isAdmin) {
    root.appendChild(el("div", { className: "empty-state", text: "Admin privileges required to configure Slacker." }));
    return;
  }

  let statusRes;
  let cfgRes;
  let jobsRes;
  let devicesRes;
  try {
    [statusRes, cfgRes, jobsRes, devicesRes] = await Promise.all([
      api("GET", "/v1/slacker/status"),
      api("GET", "/v1/slacker/config"),
      api("GET", "/v1/slacker/jobs?limit=50"),
      api("GET", "/v1/devices"),
    ]);
  } catch (err) {
    clear(root);
    root.appendChild(el("div", { className: "alert-banner alert-danger", text: err.message }));
    return;
  }

  const cfg = cfgRes.config || {};
  const runtimes = statusRes.runtimes || [];
  const accountIDs = Object.keys(cfg.accounts || {}).sort();
  if (accountIDs.length === 0) {
    accountIDs.push("default");
  }
  const devices = (devicesRes.devices || []).filter((d) => !d.revoked_at && d.kind !== "mobile").map((d) => d.name);

  const channels = Object.values(cfg.channels || {}).map((c) => ({ account: c.account || "default", ...c }));
  const emojis = Object.entries(cfg.emojis || {}).map(([emoji, harness]) => ({ emoji, harness }));
  if (emojis.length === 0) {
    emojis.push({ emoji: "claude", harness: "claude" }, { emoji: "agy", harness: "agy" }, { emoji: "codex", harness: "codex" });
  }

  clear(root);
  root.appendChild(
    el(
      "div",
      { className: "page-header" },
      el(
        "div",
        null,
        el("h1", { className: "page-title", text: "Slack Reaction Dispatcher" }),
        el("p", { className: "page-subtitle", text: "Multi-account Slack Socket Mode routing and agent read/send access over Nebula" }),
      ),
    ),
  );

  root.appendChild(renderStatusCard(statusRes));
  root.appendChild(renderAccountsCard(cfg, runtimes, ctx));

  const chanList = el("div", { className: "form-grid" });
  const renderChannels = () => {
    clear(chanList);
    for (let i = 0; i < channels.length; i++) {
      const ch = channels[i];
      const acctSel = el(
        "select",
        { onchange: (e) => { ch.account = e.target.value; } },
        ...Array.from(new Set([...accountIDs, ch.account].filter(Boolean))).map((a) => el("option", { value: a, text: a, selected: a === ch.account })),
      );
      const idIn = el("input", { type: "text", value: ch.channel_id || "", placeholder: "C0123456789", oninput: (e) => { ch.channel_id = e.target.value; } });
      const devSel = el(
        "select",
        { onchange: (e) => { ch.device = e.target.value; } },
        ...Array.from(new Set([...devices, ch.device].filter(Boolean))).map((d) => el("option", { value: d, text: d, selected: d === ch.device })),
      );
      if (!ch.device && devices.length > 0) {
        ch.device = devices[0];
      }
      const repoIn = el("input", { type: "text", value: ch.repo_path || "", placeholder: "~/workspace/repo", oninput: (e) => { ch.repo_path = e.target.value; } });
      const tmuxChk = el("input", { type: "checkbox", checked: Boolean(ch.use_tmux), onchange: (e) => { ch.use_tmux = e.target.checked; } });
      const rmBtn = el("button", {
        type: "button",
        className: "btn btn-sm btn-danger",
        text: "Remove",
        onclick: () => {
          channels.splice(i, 1);
          renderChannels();
        },
      });
      chanList.appendChild(
        el("div", { className: "toolbar m-0" }, acctSel, idIn, devSel, repoIn, el("label", { className: "checkbox-row" }, tmuxChk, "tmux"), rmBtn),
      );
    }
  };
  renderChannels();

  const emojiList = el("div", { className: "form-grid" });
  const renderEmojis = () => {
    clear(emojiList);
    for (let i = 0; i < emojis.length; i++) {
      const row = emojis[i];
      const emIn = el("input", { type: "text", value: row.emoji, placeholder: "emoji_name", oninput: (e) => { row.emoji = e.target.value; } });
      const hSel = el(
        "select",
        { onchange: (e) => { row.harness = e.target.value; } },
        ...["claude", "agy", "codex"].map((h) => el("option", { value: h, text: h, selected: row.harness === h })),
      );
      const rmBtn = el("button", {
        type: "button",
        className: "btn btn-sm btn-danger",
        text: "Remove",
        onclick: () => {
          emojis.splice(i, 1);
          renderEmojis();
        },
      });
      emojiList.appendChild(el("div", { className: "toolbar m-0" }, emIn, hSel, rmBtn));
    }
  };
  renderEmojis();

  const saveMapBtn = el("button", { type: "submit", className: "btn btn-primary", text: "Save Mappings" });
  root.appendChild(
    el(
      "form",
      {
        className: "card mb-3 form-grid",
        onsubmit: async (ev) => {
          ev.preventDefault();
          saveMapBtn.disabled = true;
          try {
            const chMap = {};
            for (const ch of channels) {
              const cid = (ch.channel_id || "").trim();
              const acct = (ch.account || "default").trim();
              if (cid) {
                const key = acct === "default" ? cid : `${acct}:${cid}`;
                chMap[key] = { account: acct, channel_id: cid, channel_name: (ch.channel_name || "").trim(), device: (ch.device || "").trim(), repo_path: (ch.repo_path || "").trim(), use_tmux: Boolean(ch.use_tmux) };
              }
            }
            const emMap = {};
            for (const e of emojis) {
              const name = (e.emoji || "").trim();
              if (name) {
                emMap[name] = e.harness;
              }
            }
            await api("POST", "/v1/slacker/config", { channels: chMap, emojis: emMap });
            toast("Mappings saved", "ok");
            ctx.refresh();
          } catch (err) {
            toast(err.message, "error");
          } finally {
            saveMapBtn.disabled = false;
          }
        },
      },
      el("div", { className: "actions-row flex-between" }, el("strong", { text: "Channel → Account / Device / Repo" }), el("button", { type: "button", className: "btn btn-sm", text: "+ Add Channel", onclick: () => { channels.push({ account: accountIDs[0] || "default", channel_id: "", device: devices[0] || "", repo_path: "" }); renderChannels(); } })),
      chanList,
      el("div", { className: "actions-row flex-between" }, el("strong", { text: "Emoji → Harness" }), el("button", { type: "button", className: "btn btn-sm", text: "+ Add Emoji", onclick: () => { emojis.push({ emoji: "", harness: "claude" }); renderEmojis(); } })),
      emojiList,
      el("div", { className: "actions-row flex-end" }, saveMapBtn),
    ),
  );

  root.appendChild(
    el(
      "details",
      { className: "card details-box mb-3" },
      el("summary", { text: "Slack App Manifest (YAML)" }),
      el("div", { className: "actions-row flex-end mt-2 mb-2" }, makeCopyBtn(cfgRes.manifest_yaml || "", "Copy Manifest")),
      el("pre", { className: "code-block", text: cfgRes.manifest_yaml || "" }),
    ),
  );

  const jobs = jobsRes.jobs || [];
  const tbody = el("tbody");
  for (const j of jobs) {
    const acts = el("div", { className: "actions-row" });
    if (j.status === "failed" || j.status === "cancelled" || j.status === "claimed") {
      acts.appendChild(el("button", { type: "button", className: "btn btn-sm", text: "Retry", onclick: async () => { await api("POST", `/v1/slacker/jobs/${encodeURIComponent(j.id)}/retry`, {}); ctx.refresh(); } }));
    }
    if (j.status === "queued" || j.status === "claimed") {
      acts.appendChild(el("button", { type: "button", className: "btn btn-sm btn-danger", text: "Cancel", onclick: async () => { await api("POST", `/v1/slacker/jobs/${encodeURIComponent(j.id)}/cancel`, {}); ctx.refresh(); } }));
    }
    const variant = j.status === "launched" ? "ok" : j.status === "failed" ? "danger" : j.status === "queued" || j.status === "claimed" ? "warn" : "neutral";
    tbody.appendChild(
      el(
        "tr",
        null,
        el("td", { className: "mono small", text: j.session_name }),
        el("td", { className: "mono small", text: `${j.account || "default"} · ${j.target_device}/${j.harness}` }),
        el("td", { className: "mono small", text: `:${j.reaction}: #${j.channel_name || j.channel_id}` }),
        el("td", null, badge(j.status, variant)),
        el("td", { className: "small", text: `${j.backend || "-"} · ${relTime(j.updated_at)}${j.error ? ` (${j.error})` : ""}` }),
        el("td", null, acts),
      ),
    );
  }

  root.appendChild(
    el(
      "div",
      { className: "card" },
      el(
        "div",
        { className: "card-header" },
        el("h3", { className: "card-title", text: "Recent Dispatch Jobs" }),
        el("button", {
          type: "button",
          className: "btn btn-sm",
          text: "Prune Completed",
          onclick: async () => {
            if (!(await confirmDialog("Prune terminal jobs", "Delete launched, failed, and cancelled job records? Active sessions and repos are untouched.", "Prune"))) {
              return;
            }
            const res = await api("POST", "/v1/slacker/cleanup", { all_terminal: true });
            toast(`Pruned ${res.pruned_jobs || 0} job(s)`, "ok");
            ctx.refresh();
          },
        }),
      ),
      jobs.length === 0
        ? el("div", { className: "muted small", text: "No Slack dispatch jobs recorded yet." })
        : el(
            "div",
            { className: "table-wrap" },
            el("table", null, el("thead", null, el("tr", null, el("th", { text: "Session" }), el("th", { text: "Account / Target" }), el("th", { text: "Trigger" }), el("th", { text: "Status" }), el("th", { text: "Detail" }), el("th", { text: "Actions" }))), tbody),
          ),
    ),
  );
}
