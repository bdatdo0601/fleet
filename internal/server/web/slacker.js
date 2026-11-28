import { openSpawnSessionModal } from "./agents.js";
import {
  api,
  badge,
  buildRepoSuggestions,
  cell,
  clear,
  confirmDialog,
  createDevicePoolPicker,
  el,
  fetchPathsCatalog,
  field,
  isWorkerOnline,
  makeCopyBtn,
  populateDatalist,
  relTime,
  renderTrackedPathsBox,
  section,
  state,
  toast,
} from "./lib.js";

const slackerState = {
  tab: "jobs", // jobs | routing | accounts
};

function renderStatusCard(statusRes) {
  const cfg = statusRes.config || {};
  const counts = statusRes.counts || {};
  const runtimes = statusRes.runtimes || [];
  const listener = runtimes.find((r) => r.role === "listener");
  const acctListeners = runtimes.filter((r) => r.role.startsWith("listener:"));
  const workers = runtimes.filter((r) => r.role.startsWith("worker:"));

  const card = el(
    "article",
    { className: "card" },
    el(
      "div",
      { className: "card-header" },
      el("h3", { className: "card-title", text: "Socket Mode & Worker Queue" }),
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
    "article",
    { className: "card" },
    el("div", { className: "card-header" }, el("h3", { className: "card-title", text: "Configured Workspaces" }), badge(`${accounts.length} account(s)`, "info")),
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
          cell("Account", { className: "mono cell-primary", text: a.id }),
          cell("Workspace / User", { className: "small", text: `${a.team_name || a.team_id || "-"} · ${a.authorized_user || a.authorized_user_id || "-"}` }),
          cell("Tokens", { className: "mono small", text: `app:${a.app_token_hint || "-"} · user:${a.user_token_hint || "-"}${a.bot_token_hint ? ` · bot:${a.bot_token_hint}` : ""}` }),
          cell("Status", null, stBadge, a.allow_unmapped_send ? " unmapped-send" : ""),
          cell(
            "Actions",
            { className: "cell-actions" },
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
                  try {
                    await api("POST", "/v1/slacker/config", { delete_account: a.id });
                    toast(`Removed account ${a.id}`, "ok");
                    ctx.refresh();
                  } catch (err) {
                    toast(err.message, "error");
                  }
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
        el(
          "table",
          { className: "responsive-table" },
          el("thead", null, el("tr", null, el("th", { text: "Account" }), el("th", { text: "Workspace / User" }), el("th", { text: "Tokens" }), el("th", { text: "Status" }), el("th", { text: "Actions" }))),
          tbody,
        ),
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
        field("Account ID", idIn),
        field("Authorized Slack User ID", authUserIn),
      ),
      el(
        "div",
        { className: "form-row-2" },
        field("App-Level Token (xapp-...)", appTokIn),
        field("User OAuth Token (xoxp-...)", userTokIn),
      ),
      el(
        "div",
        { className: "form-row-2" },
        field("Optional Bot Token (xoxb-...)", botTokIn),
        el(
          "div",
          { className: "field" },
          el("span", { className: "field-label", text: "Options" }),
          el("div", { className: "actions-row" }, el("label", { className: "checkbox-row" }, enabledChk, "Enabled"), el("label", { className: "checkbox-row" }, unmappedChk, "Allow unmapped send"), el("label", { className: "checkbox-row" }, verifyChk, "Verify")),
        ),
      ),
      el("div", { className: "actions-row flex-end" }, saveAcctBtn),
    ),
  );
  return card;
}

function renderMachinesCard(cfg, statusRes, knownDevices, pathsCatalog, ctx) {
  const runtimes = statusRes.runtimes || [];
  const agentCounts = statusRes.agent_counts || {};
  const pendingCounts = statusRes.pending_counts || {};
  const deviceCfg = cfg.devices || {};
  const defaultDevices = Array.isArray(cfg.default_devices) ? cfg.default_devices : [];
  const allDevs = Array.from(new Set([...knownDevices, ...Object.keys(deviceCfg), ...defaultDevices])).sort();
  const wsCatalog = (pathsCatalog && pathsCatalog.workspaces) || {};

  const rowState = allDevs.map((name) => ({
    name,
    inDefault: defaultDevices.includes(name),
    workspaceDir: (deviceCfg[name] && deviceCfg[name].workspace_dir) || "",
  }));

  let defaultStrategy = cfg.default_strategy || "least_agents";
  const stratSel = el(
    "select",
    { onchange: (e) => { defaultStrategy = e.target.value; } },
    el("option", { value: "least_agents", text: "Least agents (round-robin tie-break)", selected: defaultStrategy === "least_agents" }),
    el("option", { value: "round_robin", text: "Round-robin", selected: defaultStrategy === "round_robin" }),
  );
  let defaultPolicy = cfg.default_policy || "auto";
  const policySel = el(
    "select",
    { onchange: (e) => { defaultPolicy = e.target.value; } },
    el("option", { value: "auto", text: "Auto", selected: defaultPolicy === "auto" }),
    el("option", { value: "bypass", text: "Bypass (skip permissions)", selected: defaultPolicy === "bypass" }),
    el("option", { value: "safe", text: "Safe (manual / plan)", selected: defaultPolicy === "safe" }),
  );

  const tbody = el("tbody");
  for (const row of rowState) {
    const online = isWorkerOnline(runtimes, row.name);
    const liveN = agentCounts[row.name] || 0;
    const pendN = pendingCounts[row.name] || 0;
    const defChk = el("input", {
      type: "checkbox",
      checked: row.inDefault,
      onchange: (e) => { row.inDefault = e.target.checked; },
    });
    const wsListID = `slacker-ws-${row.name}`;
    const wsChoices = Array.from(new Set([...(wsCatalog[row.name] || []), row.workspaceDir].filter(Boolean)));
    const wsDatalist = el("datalist", { id: wsListID }, ...wsChoices.map((w) => el("option", { value: w })));
    const wsIn = el("input", {
      type: "text",
      className: "w-full",
      list: wsListID,
      "aria-label": `Default workspace for ${row.name}`,
      value: row.workspaceDir,
      placeholder: "~/workspace",
      oninput: (e) => { row.workspaceDir = e.target.value; },
    });
    tbody.appendChild(
      el(
        "tr",
        null,
        cell("Default Pool / Machine", { className: "cell-primary" }, el("label", { className: "checkbox-row" }, defChk, el("span", { className: "mono", text: row.name }))),
        cell(
          "Worker & Load",
          null,
          el(
            "div",
            { className: "actions-row" },
            badge(online ? "worker online" : "worker offline", online ? "ok" : "neutral"),
            badge(`${liveN} agent${liveN === 1 ? "" : "s"}${pendN > 0 ? ` + ${pendN} queued` : ""}`, liveN + pendN > 0 ? "info" : "neutral"),
          ),
        ),
        cell("Workspace Dir", null, wsIn, wsDatalist),
      ),
    );
  }

  const saveBtn = el("button", { type: "submit", className: "btn btn-primary", text: "Save Machines & Defaults" });
  return el(
    "form",
    {
      className: "card form-grid",
      onsubmit: async (ev) => {
        ev.preventDefault();
        saveBtn.disabled = true;
        try {
          const devicesPayload = {};
          const defaultList = [];
          for (const row of rowState) {
            const ws = row.workspaceDir.trim();
            if (ws) {
              devicesPayload[row.name] = { workspace_dir: ws };
            }
            if (row.inDefault) {
              defaultList.push(row.name);
            }
          }
          await api("POST", "/v1/slacker/config", {
            devices: devicesPayload,
            default_devices: defaultList,
            default_strategy: defaultStrategy,
            default_policy: defaultPolicy,
          });
          toast("Saved machine workspaces & defaults", "ok");
          ctx.refresh();
        } catch (err) {
          toast(err.message, "error");
        } finally {
          saveBtn.disabled = false;
        }
      },
    },
    el("div", { className: "card-header" }, el("h3", { className: "card-title", text: "Machine Workspaces & Default Pool" }), badge(`${rowState.filter((r) => r.inDefault).length} in default pool`, "info")),
    allDevs.length === 0
      ? el("div", { className: "muted small", text: "No active Mac/desktop devices registered yet." })
      : el(
          "div",
          { className: "table-wrap" },
          el("table", { className: "responsive-table" }, el("thead", null, el("tr", null, el("th", { text: "Default Pool / Machine" }), el("th", { text: "Worker & Load" }), el("th", { text: "Default Workspace Dir" }))), tbody),
        ),
    el(
      "div",
      { className: "form-row-2" },
      field("Default Routing Strategy", stratSel),
      field("Default Permission Policy", policySel),
    ),
    el("div", { className: "actions-row flex-end" }, saveBtn),
  );
}

function renderMappingsCard(cfg, devices, pathsCatalog, ctx) {
  const accountIDs = Object.keys(cfg.accounts || {}).sort();
  if (accountIDs.length === 0) {
    accountIDs.push("default");
  }
  const defaultDevices = Array.isArray(cfg.default_devices) ? cfg.default_devices : [];
  const globalStrategyLabel = (cfg.default_strategy || "least_agents") === "round_robin" ? "round-robin" : "least agents";
  const globalPolicyLabel = cfg.default_policy || "auto";

  const channels = Object.values(cfg.channels || {}).map((c) => {
    const chDevs = Array.isArray(c.devices) && c.devices.length > 0 ? [...c.devices] : c.device ? [c.device] : [];
    return { account: c.account || "default", ...c, devices: chDevs, strategy: c.strategy || "", policy: c.policy || "" };
  });
  const emojis = Object.entries(cfg.emojis || {}).map(([emoji, harness]) => ({ emoji, harness }));
  if (emojis.length === 0) {
    emojis.push({ emoji: "claude", harness: "claude" }, { emoji: "agy", harness: "agy" }, { emoji: "codex", harness: "codex" });
  }

  const chanList = el("div", { className: "form-grid" });
  const renderChannels = () => {
    clear(chanList);
    if (channels.length === 0) {
      chanList.appendChild(el("div", { className: "muted small", text: "No channel mappings configured yet." }));
      return;
    }
    for (let i = 0; i < channels.length; i++) {
      const ch = channels[i];
      const acctSel = el(
        "select",
        { onchange: (e) => { ch.account = e.target.value; } },
        ...Array.from(new Set([...accountIDs, ch.account].filter(Boolean))).map((a) => el("option", { value: a, text: a, selected: a === ch.account })),
      );
      const idIn = el("input", { type: "text", value: ch.channel_id || "", placeholder: "C0123456789", oninput: (e) => { ch.channel_id = e.target.value; } });
      const chipPool = Array.from(new Set([...devices, ...(ch.devices || [])].filter(Boolean)));
      const repoListID = `slacker-ch-repo-${i}`;
      const repoDatalist = el("datalist", { id: repoListID });
      const repoIn = el("input", {
        type: "text",
        list: repoListID,
        value: ch.repo_path || "",
        placeholder: "subpath (e.g. fleet), blank=workspace, or ~/path",
        oninput: (e) => { ch.repo_path = e.target.value; },
      });
      const repoPickSel = el("select", {
        onchange: (e) => {
          if (e.target.value) {
            repoIn.value = e.target.value;
            ch.repo_path = e.target.value;
            e.target.value = "";
          }
        },
      });
      const syncRowRepoChoices = () => {
        const effDevs = (ch.devices || []).length > 0 ? ch.devices : defaultDevices;
        const suggestions = buildRepoSuggestions(pathsCatalog, effDevs, cfg.devices || {});
        populateDatalist(repoDatalist, suggestions);
        clear(repoPickSel);
        repoPickSel.appendChild(el("option", { value: "", text: suggestions.length > 0 ? "Pick tracked dir/repo…" : "No tracked dirs" }));
        for (const s of suggestions) {
          repoPickSel.appendChild(el("option", { value: s.value, text: s.label, title: s.detail }));
        }
        repoPickSel.disabled = suggestions.length === 0;
      };
      syncRowRepoChoices();

      const { element: devChipsBox } = createDevicePoolPicker({
        pool: chipPool,
        selected: ch.devices || [],
        defaultDevices,
        onChange: (next) => {
          ch.devices = next;
          syncRowRepoChoices();
        },
      });
      const stratSel = el(
        "select",
        { onchange: (e) => { ch.strategy = e.target.value; } },
        el("option", { value: "", text: `Default (${globalStrategyLabel})`, selected: !ch.strategy }),
        el("option", { value: "least_agents", text: "Least agents", selected: ch.strategy === "least_agents" }),
        el("option", { value: "round_robin", text: "Round-robin", selected: ch.strategy === "round_robin" }),
      );
      const polSel = el(
        "select",
        { onchange: (e) => { ch.policy = e.target.value; } },
        el("option", { value: "", text: `Default (${globalPolicyLabel})`, selected: !ch.policy }),
        el("option", { value: "auto", text: "Auto", selected: ch.policy === "auto" }),
        el("option", { value: "bypass", text: "Bypass", selected: ch.policy === "bypass" }),
        el("option", { value: "safe", text: "Safe", selected: ch.policy === "safe" }),
      );
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
        el(
          "div",
          { className: "mapping-card form-grid" },
          el(
            "div",
            { className: "actions-row flex-between" },
            el("strong", { className: "small mono", text: ch.channel_id ? `#${ch.channel_name || ch.channel_id}` : `Channel #${i + 1}` }),
            el("div", { className: "actions-row" }, el("label", { className: "checkbox-row" }, tmuxChk, "Use tmux"), rmBtn),
          ),
          el("div", { className: "form-row-2" }, field("Account", acctSel), field("Channel ID", idIn)),
          el("div", { className: "field" }, el("span", { className: "field-label", text: "Target Machine(s)" }), devChipsBox),
          el("div", { className: "form-row-2" }, field("Routing Strategy", stratSel), field("Permission Policy", polSel)),
          el("div", { className: "form-row-2" }, field("Repo / Directory", repoIn, repoDatalist), field("Tracked Preset", repoPickSel)),
        ),
      );
    }
  };
  renderChannels();

  const emojiList = el("div", { className: "form-grid" });
  const renderEmojis = () => {
    clear(emojiList);
    for (let i = 0; i < emojis.length; i++) {
      const row = emojis[i];
      const emIn = el("input", { type: "text", className: "grow", "aria-label": "Emoji name", value: row.emoji, placeholder: "emoji_name", oninput: (e) => { row.emoji = e.target.value; } });
      const hSel = el(
        "select",
        { "aria-label": "Harness", onchange: (e) => { row.harness = e.target.value; } },
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
  return el(
    "form",
    {
      className: "card form-grid",
      onsubmit: async (ev) => {
        ev.preventDefault();
        saveMapBtn.disabled = true;
        try {
          const chMap = {};
          for (const ch of channels) {
            const cid = (ch.channel_id || "").trim();
            const acct = (ch.account || "default").trim();
            const devs = Array.isArray(ch.devices) ? ch.devices.map((d) => d.trim()).filter(Boolean) : [];
            if (cid) {
              const key = acct === "default" ? cid : `${acct}:${cid}`;
              chMap[key] = {
                account: acct,
                channel_id: cid,
                channel_name: (ch.channel_name || "").trim(),
                device: devs.length === 1 ? devs[0] : "",
                devices: devs,
                strategy: (ch.strategy || "").trim(),
                policy: (ch.policy || "").trim(),
                repo_path: (ch.repo_path || "").trim(),
                use_tmux: Boolean(ch.use_tmux),
              };
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
    el(
      "div",
      { className: "card-header" },
      el("h3", { className: "card-title", text: "Channel → Account / Machines / Repo" }),
      el("button", {
        type: "button",
        className: "btn btn-sm",
        text: "+ Add Channel",
        onclick: () => {
          channels.push({
            account: accountIDs[0] || "default",
            channel_id: "",
            devices: defaultDevices.length > 0 ? [] : devices.length > 0 ? [devices[0]] : [],
            strategy: "",
            policy: "",
            repo_path: "",
          });
          renderChannels();
        },
      }),
    ),
    chanList,
    el("div", { className: "card-header mt-2" }, el("h3", { className: "card-title", text: "Emoji → Harness" }), el("button", { type: "button", className: "btn btn-sm", text: "+ Add Emoji", onclick: () => { emojis.push({ emoji: "", harness: "claude" }); renderEmojis(); } })),
    emojiList,
    el("div", { className: "actions-row flex-end" }, saveMapBtn),
  );
}

function renderJobsSection(jobs, ctx) {
  const tbody = el("tbody");
  for (const j of jobs) {
    const acts = el("div", { className: "actions-row" });
    if (j.status === "failed" || j.status === "cancelled" || j.status === "claimed") {
      acts.appendChild(
        el("button", {
          type: "button",
          className: "btn btn-sm",
          text: "Retry",
          onclick: async () => {
            try {
              await api("POST", `/v1/slacker/jobs/${encodeURIComponent(j.id)}/retry`, {});
              ctx.refresh();
            } catch (err) {
              toast(err.message, "error");
            }
          },
        }),
      );
    }
    if (j.status === "queued" || j.status === "claimed") {
      acts.appendChild(
        el("button", {
          type: "button",
          className: "btn btn-sm btn-danger",
          text: "Cancel",
          onclick: async () => {
            try {
              await api("POST", `/v1/slacker/jobs/${encodeURIComponent(j.id)}/cancel`, {});
              ctx.refresh();
            } catch (err) {
              toast(err.message, "error");
            }
          },
        }),
      );
    }
    const variant = j.status === "launched" ? "ok" : j.status === "failed" ? "danger" : j.status === "queued" || j.status === "claimed" ? "warn" : "neutral";
    const triggerText = j.account === "ui" ? "web-ui" : `:${j.reaction}: #${j.channel_name || j.channel_id}`;
    tbody.appendChild(
      el(
        "tr",
        null,
        cell("Session", { className: "mono small cell-primary", text: j.session_name }),
        cell("Account / Target", { className: "mono small", text: `${j.account || "default"} · ${j.target_device}/${j.harness}` }),
        cell("Trigger", { className: "mono small", text: triggerText }),
        cell("Status", null, badge(j.status, variant)),
        cell("Detail", { className: "small", text: `${j.backend || "-"} · ${relTime(j.updated_at)}${j.error ? ` (${j.error})` : ""}` }),
        cell("Actions", { className: acts.childElementCount > 0 ? "cell-actions" : "cell-actions cell-empty" }, acts),
      ),
    );
  }

  return section(
    "Recent Dispatch Jobs",
    {
      className: "ui-section",
      actions: el("button", {
        type: "button",
        className: "btn btn-sm",
        text: "Prune Completed",
        onclick: async () => {
          if (!(await confirmDialog("Prune terminal jobs", "Delete launched, failed, and cancelled job records? Active sessions and repos are untouched.", "Prune"))) {
            return;
          }
          try {
            const res = await api("POST", "/v1/slacker/cleanup", { all_terminal: true });
            toast(`Pruned ${res.pruned_jobs || 0} job(s)`, "ok");
            ctx.refresh();
          } catch (err) {
            toast(err.message, "error");
          }
        },
      }),
    },
    jobs.length === 0
      ? el("div", { className: "empty-state", text: "No Slack dispatch jobs recorded yet." })
      : el(
          "div",
          { className: "table-wrap" },
          el(
            "table",
            { className: "responsive-table" },
            el("thead", null, el("tr", null, el("th", { text: "Session" }), el("th", { text: "Account / Target" }), el("th", { text: "Trigger" }), el("th", { text: "Status" }), el("th", { text: "Detail" }), el("th", { text: "Actions" }))),
            tbody,
          ),
        ),
  );
}

export async function renderSlacker(root, ctx, subRoute = "") {
  const signal = ctx && ctx.signal;
  if (!state.isAdmin) {
    root.appendChild(el("div", { className: "empty-state", text: "Admin privileges required to configure Slacker." }));
    return;
  }

  if (["jobs", "routing", "accounts"].includes(subRoute)) {
    slackerState.tab = subRoute;
  }

  let statusRes;
  let cfgRes;
  let jobsRes;
  let devicesRes;
  let pathsCatalog;
  try {
    [statusRes, cfgRes, jobsRes, devicesRes, pathsCatalog] = await Promise.all([
      api("GET", "/v1/slacker/status", undefined, { signal }),
      api("GET", "/v1/slacker/config", undefined, { signal }),
      api("GET", "/v1/slacker/jobs?limit=50", undefined, { signal }),
      api("GET", "/v1/devices", undefined, { signal }),
      fetchPathsCatalog("", { signal }),
    ]);
  } catch (err) {
    if ((err && err.name === "AbortError") || (signal && signal.aborted)) {
      return;
    }
    clear(root);
    root.appendChild(el("div", { className: "alert-banner alert-danger", text: err.message }));
    return;
  }
  if (signal && signal.aborted) {
    return;
  }

  const cfg = cfgRes.config || {};
  const runtimes = statusRes.runtimes || [];
  const devices = (devicesRes.devices || []).filter((d) => !d.revoked_at && d.kind !== "mobile").map((d) => d.name);
  const jobs = jobsRes.jobs || [];

  clear(root);
  root.appendChild(
    el(
      "header",
      { className: "page-header" },
      el(
        "div",
        null,
        el("h1", { className: "page-title", text: "Slack Reaction Dispatcher" }),
        el("p", { className: "page-subtitle", text: "Multi-account Slack Socket Mode routing and agent read/send access over Nebula" }),
      ),
      el("button", {
        type: "button",
        className: "btn btn-primary",
        text: "+ Spawn Session",
        onclick: () => openSpawnSessionModal(() => ctx.refresh()),
      }),
    ),
  );

  const tabs = [
    ["jobs", `Runtime & Jobs (${jobs.length})`, "#slacker"],
    ["routing", "Routing & Mappings", "#slacker/routing"],
    ["accounts", "Accounts & Manifest", "#slacker/accounts"],
  ].map(([id, label, hash]) =>
    el("button", {
      type: "button",
      role: "tab",
      "aria-selected": slackerState.tab === id ? "true" : "false",
      className: `sub-tab${slackerState.tab === id ? " active" : ""}`,
      text: label,
      onclick: () => {
        slackerState.tab = id;
        ctx.navigate(hash);
      },
    }),
  );
  root.appendChild(el("div", { className: "sub-tabs", role: "tablist", "aria-label": "Slacker views" }, tabs));

  if (slackerState.tab === "routing") {
    root.appendChild(section("Machines & Defaults", { className: "ui-section" }, renderMachinesCard(cfg, statusRes, devices, pathsCatalog, ctx)));
    root.appendChild(section("Channel & Emoji Mappings", { className: "ui-section" }, renderMappingsCard(cfg, devices, pathsCatalog, ctx)));
    root.appendChild(
      section(
        "Known Directories & Repos",
        { className: "ui-section" },
        renderTrackedPathsBox(
          pathsCatalog,
          {
            devices,
            onUse: (p) => openSpawnSessionModal(() => ctx.refresh(), { device: p.device, repo_path: p.subpath || p.tilde_path || p.path }),
            useLabel: "Spawn",
          },
          () => ctx.refresh(),
        ),
      ),
    );
  } else if (slackerState.tab === "accounts") {
    root.appendChild(section("Slack Accounts", { className: "ui-section" }, renderAccountsCard(cfg, runtimes, ctx)));
    root.appendChild(
      section(
        "Slack App Manifest",
        { className: "ui-section" },
        el(
          "details",
          { className: "card details-box m-0" },
          el("summary", { text: "Slack App Manifest (YAML)" }),
          el("div", { className: "actions-row flex-end mt-2 mb-2" }, makeCopyBtn(cfgRes.manifest_yaml || "", "Copy Manifest")),
          el("pre", { className: "code-block", text: cfgRes.manifest_yaml || "" }),
        ),
      ),
    );
  } else {
    root.appendChild(section("Slacker Runtime", { className: "ui-section" }, renderStatusCard(statusRes)));
    root.appendChild(renderJobsSection(jobs, ctx));
  }
}
