import { api, badge, clear, closeModal, el, field, openModal, toast } from "./lib.js";
import { startMCPOAuth, subscribeMCPOAuthDone } from "./chat_oauth.js";

export async function openAssistantSettingsModal(onSaved, onNavigate) {
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
  const autoApproveChk = el("input", { type: "checkbox", checked: Boolean(cfg.auto_approve) });
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
  const mcpStatusBox = el("div", { className: "stack-sm" });
  const loadMCPStatus = async () => {
    clear(mcpStatusBox);
    try {
      const res = await api("GET", "/v1/assistant/mcp");
      const servers = res.servers || [];
      const byName = new Map((res.statuses || []).map((st) => [st.name, st]));
      if (servers.length === 0) {
        return;
      }
      for (const srv of servers) {
        const st = byName.get(srv.name) || {};
        const oauthOn = Boolean(srv.oauth && srv.oauth.enabled);
        const actions = [];
        if (oauthOn) {
          actions.push(
            el("button", {
              type: "button",
              className: "btn btn-sm",
              text: st.oauth_authorized ? "Reconnect OAuth" : "Connect OAuth",
              onclick: async () => {
                try {
                  await startMCPOAuth(srv.name, {
                    explicitRedirectURI: srv.oauth && srv.oauth.redirect_uri,
                    preferOriginRedirect: true,
                    warnIfInsecure: true,
                  });
                } catch (err) {
                  toast(err.message, "error");
                }
              },
            }),
          );
          if (st.oauth_authorized) {
            actions.push(
              el("button", {
                type: "button",
                className: "btn btn-sm",
                text: "Disconnect",
                onclick: async () => {
                  try {
                    await api("DELETE", `/v1/assistant/mcp/${encodeURIComponent(srv.name)}/oauth`);
                    toast(`Disconnected OAuth for ${srv.name}`, "ok");
                    await loadMCPStatus();
                  } catch (err) {
                    toast(err.message, "error");
                  }
                },
              }),
            );
          }
        }
        const detail = st.error
          ? st.error
          : srv.enabled
            ? `${st.tool_count || 0} tool(s) ready`
            : "Disabled";
        mcpStatusBox.appendChild(
          el(
            "div",
            { className: "gen-native-card" },
            el(
              "div",
              { className: "gen-mini-row" },
              el("strong", { text: srv.name }),
              badge(srv.transport || "stdio", "neutral"),
              oauthOn ? badge(st.oauth_authorized ? "OAuth: Authorized" : "OAuth: Not connected", st.oauth_authorized ? "ok" : "warn") : null,
            ),
            el("div", { className: "small muted", text: detail }),
            actions.length > 0 ? el("div", { className: "actions-row mt-1" }, ...actions) : null,
          ),
        );
      }
    } catch (err) {
      mcpStatusBox.appendChild(el("div", { className: "small muted", text: `Failed to load MCP status: ${err.message}` }));
    }
  };
  void loadMCPStatus();
  const unsubOAuth = subscribeMCPOAuthDone(() => {
    void loadMCPStatus();
  });

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
  const discAutoApproveChk = el("input", { type: "checkbox", checked: Boolean(discCfg.auto_approve) });
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
    el("label", { className: "check-row" }, autoApproveChk, el("span", { text: "Auto-approve all tool calls & frontier escalations (bypass HITL)" })),
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
    mcpStatusBox,
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
    el("label", { className: "check-row" }, discAutoApproveChk, el("span", { text: "Auto-approve Discord turns (bypass HITL for Discord messages)" })),
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
            auto_approve: autoApproveChk.checked,
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
              auto_approve: discAutoApproveChk.checked,
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
                    openAssistantSettingsModal(onSaved, onNavigate);
                  } catch (err) {
                    toast(err.message, "error");
                  }
                },
              }),
            ),
          ),
        ),
  );
  openModal("Assistant Settings", body, {
    onClose: unsubOAuth,
  });
}
