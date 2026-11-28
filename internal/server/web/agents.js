import {
  api,
  badge,
  buildRepoSuggestions,
  cell,
  clear,
  closeModal,
  confirmDialog,
  createDevicePoolPicker,
  el,
  fetchPathsCatalog,
  field,
  formatDuration,
  formatMessageTarget,
  isWorkerOnline,
  makeCopyBtn,
  openModal,
  populateDatalist,
  relTime,
  renderTrackedPathsBox,
  section,
  state,
  toast,
} from "./lib.js";
import { renderUsageCard } from "./usage.js";

const agentsFilter = {
  tab: "sessions", // sessions | messages | usage
  stateChip: "live", // live | waiting | working | idle | all
  device: "",
  harness: "",
  repo: "",
  query: "",
  msgSession: "",
  msgDevice: "",
  msgThread: "",
  msgLimit: 50,
};

function stateVariant(st, live) {
  if (!live || st === "exited") {
    return st === "exited" ? "danger" : "neutral";
  }
  if (st === "working") {
    return "info";
  }
  if (st === "waiting_permission" || st === "waiting_question") {
    return "warn";
  }
  return "neutral";
}

function deliveryVariant(del) {
  switch (del) {
    case "immediate":
      return "ok";
    case "next_hook":
      return "info";
    case "waiting_user":
      return "warn";
    case "offline":
      return "danger";
    default:
      return "neutral";
  }
}

function canControlSession(ag) {
  return state.isAdmin || (state.caller && ag.device === state.caller.name);
}

function buildSendMessageForm(initialTo = "", opts = {}, onSent = null) {
  const datalistID = Array.isArray(opts.targets) && opts.targets.length > 0 ? "send-msg-targets" : "";
  const toInput = el("input", {
    type: "text",
    value: initialTo,
    list: datalistID || null,
    placeholder: "session name/id, device:<name>, repo:<name>, or all",
  });
  const targetList = datalistID
    ? el("datalist", { id: datalistID }, ...opts.targets.map((t) => el("option", { value: t.value, label: t.label || "" })))
    : null;
  const threadInput = el("input", { type: "text", value: opts.thread_id || "", placeholder: "Optional thread_id" });
  const replyToInput = el("input", { type: "number", value: opts.reply_to ? String(opts.reply_to) : "", placeholder: "Optional reply_to message ID" });
  const bodyInput = el("textarea", { rows: 3, placeholder: "Message to deliver to agent inbox / live listener…" });
  const reportBox = el("div");
  const sendBtn = el("button", { type: "submit", className: "btn btn-primary", text: "Send Message" });

  const markEditing = () => {
    state.editing = Boolean(bodyInput.value.trim());
  };
  bodyInput.addEventListener("input", markEditing);

  return el(
    "form",
    {
      className: "form-grid",
      onsubmit: async (ev) => {
        ev.preventDefault();
        const to = toInput.value.trim();
        const body = bodyInput.value.trim();
        if (!to || !body) {
          toast("Recipient and message body are required", "warn");
          return;
        }
        const payload = { to, body };
        if (threadInput.value.trim()) {
          payload.thread_id = threadInput.value.trim();
        }
        if (replyToInput.value.trim()) {
          const rid = Number.parseInt(replyToInput.value.trim(), 10);
          if (!Number.isNaN(rid) && rid > 0) {
            payload.reply_to = rid;
          }
        }
        sendBtn.disabled = true;
        try {
          const res = await api("POST", "/v1/agents/messages", payload);
          bodyInput.value = "";
          state.editing = false;
          clear(reportBox);
          reportBox.appendChild(
            el(
              "div",
              { className: `alert-banner ${res.warning ? "alert-warn" : "alert-ok"}` },
              el(
                "span",
                {
                  text: res.warning
                    ? `${res.delivery}: ${res.warning}`
                    : `Sent (delivery=${res.delivery}, woke=${res.woke_waiters}, thread=${res.message.thread_id})`,
                },
              ),
            ),
          );
          toast(res.warning ? `Sent (${res.delivery}): ${res.warning}` : `Message sent (${res.delivery})`, res.warning ? "warn" : "ok");
          if (onSent) {
            onSent(res);
          }
        } catch (err) {
          toast(err.message, "error");
        } finally {
          sendBtn.disabled = false;
        }
      },
    },
    opts.hideTo ? null : field("To", toInput, targetList),
    opts.compact
      ? null
      : el(
          "div",
          { className: "form-row-2" },
          field("Thread ID (optional)", threadInput),
          field("Reply-To ID (optional)", replyToInput),
        ),
    field("Message Body", bodyInput),
    el("div", { className: "actions-row flex-end" }, sendBtn),
    reportBox,
  );
}

export function openQuickMessageModal(target, opts = {}, onSent = null) {
  const box = buildSendMessageForm(target, opts, () => {
    closeModal();
    if (onSent) {
      onSent();
    }
  });
  openModal(`Message: ${target}`, box, {
    onClose: () => {
      state.editing = false;
    },
  });
}

export async function openSpawnSessionModal(onSpawned = null, initial = {}) {
  let statusRes;
  let devicesRes;
  let pathsCatalog;
  try {
    [statusRes, devicesRes, pathsCatalog] = await Promise.all([
      api("GET", "/v1/slacker/status"),
      api("GET", "/v1/devices"),
      fetchPathsCatalog(),
    ]);
  } catch (err) {
    toast(err.message, "error");
    return;
  }

  const cfg = statusRes.config || {};
  const runtimes = statusRes.runtimes || [];
  const agentCounts = statusRes.agent_counts || {};
  const pendingCounts = statusRes.pending_counts || {};
  const devices = (devicesRes.devices || []).filter((d) => !d.revoked_at && d.kind !== "mobile").map((d) => d.name);
  const defaultDevices = Array.isArray(cfg.default_devices) ? cfg.default_devices : [];
  const globalStrategyLabel = (cfg.default_strategy || "least_agents") === "round_robin" ? "round-robin" : "least agents";
  const globalPolicyLabel = cfg.default_policy || "auto";
  const wsHints = Object.entries(cfg.devices || {})
    .filter(([, dc]) => dc && dc.workspace_dir)
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([d, dc]) => `${d}:${dc.workspace_dir}`);

  const harnessSel = el(
    "select",
    null,
    el("option", { value: "claude", text: "claude" }),
    el("option", { value: "agy", text: "agy" }),
    el("option", { value: "codex", text: "codex" }),
  );
  const policySel = el(
    "select",
    null,
    el("option", { value: "", text: `Default (${globalPolicyLabel})` }),
    el("option", { value: "auto", text: "Auto" }),
    el("option", { value: "bypass", text: "Bypass (skip permissions)" }),
    el("option", { value: "safe", text: "Safe (manual / plan)" }),
  );
  const stratSel = el(
    "select",
    null,
    el("option", { value: "", text: `Default (${globalStrategyLabel})` }),
    el("option", { value: "least_agents", text: "Least agents" }),
    el("option", { value: "round_robin", text: "Round-robin" }),
  );
  const repoDatalist = el("datalist", { id: "spawn-repo-list" });
  const repoIn = el("input", {
    type: "text",
    list: "spawn-repo-list",
    value: initial.repo_path || "",
    placeholder: "subpath (e.g. fleet), blank=workspace, or ~/path",
  });
  const repoChipsRow = el("div", { className: "chips-row mt-1" });
  const nameIn = el("input", { type: "text", placeholder: "auto-generated if blank" });
  const promptIn = el("textarea", { rows: 4, placeholder: "Optional prompt (leave blank for an interactive session)" });
  const tmuxChk = el("input", { type: "checkbox" });

  let selectedDevices = initial.device && devices.includes(initial.device)
    ? [initial.device]
    : defaultDevices.length > 0
      ? []
      : devices.length > 0
        ? [devices[0]]
        : [];

  const markEditing = () => {
    state.editing = Boolean(promptIn.value.trim() || repoIn.value.trim() || nameIn.value.trim());
  };

  const syncRepoSuggestions = () => {
    const effDevs = selectedDevices.length > 0 ? selectedDevices : defaultDevices;
    const suggestions = buildRepoSuggestions(pathsCatalog, effDevs, cfg.devices || {});
    populateDatalist(repoDatalist, suggestions);
    clear(repoChipsRow);
    const curVal = repoIn.value.trim();
    for (const s of suggestions.slice(0, 12)) {
      repoChipsRow.appendChild(
        el("button", {
          type: "button",
          className: `chip${curVal === s.value ? " active" : ""}`,
          text: s.label,
          title: s.detail,
          onclick: () => {
            repoIn.value = s.value;
            markEditing();
            syncRepoSuggestions();
          },
        }),
      );
    }
    repoChipsRow.classList.toggle("hidden", suggestions.length === 0);
  };

  const { element: deviceChipsBox } = createDevicePoolPicker({
    pool: devices,
    selected: selectedDevices,
    defaultDevices,
    labelForDevice: (devName) => {
      const load = (agentCounts[devName] || 0) + (pendingCounts[devName] || 0);
      const online = isWorkerOnline(runtimes, devName);
      return `${devName} (${load})${online ? " · online" : ""}`;
    },
    onChange: (next) => {
      selectedDevices = next;
      syncRepoSuggestions();
    },
  });

  promptIn.addEventListener("input", markEditing);
  repoIn.addEventListener("input", () => {
    markEditing();
    for (const chip of repoChipsRow.children) {
      chip.classList.toggle("active", chip.textContent === repoIn.value.trim() || chip.textContent.startsWith(`${repoIn.value.trim()} (`));
    }
  });
  nameIn.addEventListener("input", markEditing);
  syncRepoSuggestions();
  markEditing();

  const spawnBtn = el("button", { type: "submit", className: "btn btn-primary", text: "Spawn Session" });
  const form = el(
    "form",
    {
      className: "form-grid",
      onsubmit: async (ev) => {
        ev.preventDefault();
        spawnBtn.disabled = true;
        try {
          const res = await api("POST", "/v1/agents/spawn", {
            harness: harnessSel.value,
            name: nameIn.value.trim(),
            devices: selectedDevices,
            strategy: stratSel.value,
            policy: policySel.value,
            repo_path: repoIn.value.trim(),
            prompt: promptIn.value.trim(),
            use_tmux: tmuxChk.checked,
          });
          const j = res.job || {};
          const msg = res.worker_online
            ? `Spawned ${j.harness} session ${j.session_name} on ${j.target_device} (${j.repo_path})`
            : `Queued ${j.harness} session ${j.session_name} on ${j.target_device} (${j.repo_path}) — worker offline, will start when connected`;
          toast(msg, res.worker_online ? "ok" : "warn");
          state.editing = false;
          closeModal();
          if (onSpawned) {
            onSpawned(res);
          }
        } catch (err) {
          toast(err.message, "error");
        } finally {
          spawnBtn.disabled = false;
        }
      },
    },
    el(
      "div",
      { className: "form-row-2" },
      field("Harness", harnessSel),
      field("Permission Policy", policySel),
    ),
    el(
      "div",
      { className: "field" },
      el("span", { className: "field-label", text: "Target Machine(s)" }),
      deviceChipsBox,
    ),
    el(
      "div",
      { className: "form-row-2" },
      field("Routing Strategy", stratSel),
      field("Session Name (optional)", nameIn),
    ),
    field(
      "Directory / Repo",
      repoIn,
      el("div", null, repoDatalist, repoChipsRow, wsHints.length > 0 ? el("div", { className: "field-hint mono mt-1", text: `Workspaces: ${wsHints.join(" · ")}` }) : null),
    ),
    field("Initial Prompt (optional)", promptIn),
    el("label", { className: "checkbox-row" }, tmuxChk, "Use tmux"),
    el(
      "div",
      { className: "actions-row flex-end" },
      el("button", { type: "button", className: "btn", text: "Cancel", onclick: closeModal }),
      spawnBtn,
    ),
  );

  openModal("Spawn Agent Session", form, {
    onClose: () => {
      state.editing = false;
    },
  });
}

async function renameSessionPrompt(ag, onDone) {
  const input = el("input", { type: "text", value: ag.name || "", placeholder: "new-session-name" });
  const saveBtn = el("button", { type: "submit", className: "btn btn-primary", text: "Save Name" });
  const form = el(
    "form",
    {
      className: "form-grid",
      onsubmit: async (ev) => {
        ev.preventDefault();
        const name = input.value.trim();
        if (!name) {
          toast("Session name is required", "warn");
          return;
        }
        saveBtn.disabled = true;
        try {
          const res = await api("POST", "/v1/agents/rename", { session_id: ag.session_id, name });
          toast(`Renamed -> ${res.name}`, "ok");
          closeModal();
          onDone();
        } catch (err) {
          toast(err.message, "error");
        } finally {
          saveBtn.disabled = false;
        }
      },
    },
    field("Session Handle", input),
    el(
      "div",
      { className: "actions-row flex-end" },
      el("button", { type: "button", className: "btn", text: "Cancel", onclick: closeModal }),
      saveBtn,
    ),
  );
  openModal(`Rename ${ag.session_id}`, form);
}

async function killSession(ag, onDone) {
  const ok = await confirmDialog(
    "Kill Agent Session?",
    `Terminate ${ag.name || ag.session_id} (${ag.harness}) on ${ag.device} and mark it exited?`,
    "Kill Session",
    true,
  );
  if (!ok) {
    return;
  }
  try {
    const res = await api("POST", "/v1/agents/kill", { session_id: ag.session_id });
    toast(
      res.worker_online
        ? `Kill queued for ${ag.name || ag.session_id} on ${ag.device}`
        : `Kill queued for ${ag.name || ag.session_id} on ${ag.device} (worker offline, will run when connected)`,
      res.worker_online ? "ok" : "warn",
    );
    onDone();
  } catch (err) {
    toast(err.message, "error");
  }
}

async function killAllAgents(harness, label, deviceFilter, liveCount, onDone) {
  const scopeDev = deviceFilter ? `on ${deviceFilter}` : state.isAdmin ? "across all devices" : `on ${state.caller ? state.caller.name : "this device"}`;
  const countTxt = liveCount > 0 ? ` (${liveCount} live)` : "";
  const ok = await confirmDialog(
    `Kill ${label}?`,
    `Terminate ${label.toLowerCase()} ${scopeDev}${countTxt}?`,
    `Kill ${label}`,
    true,
  );
  if (!ok) {
    return;
  }
  try {
    const res = await api("POST", "/v1/agents/kill-all", { harness, device: deviceFilter || "" });
    const devs = (res.devices || []).join(", ") || "none";
    const off = res.offline_devices || [];
    const msg =
      off.length > 0
        ? `Killed ${res.deregistered || 0} ${label.toLowerCase()} (queued for ${devs}; offline: ${off.join(", ")})`
        : `Killed ${res.deregistered || 0} ${label.toLowerCase()} (dispatched to ${devs})`;
    toast(msg, off.length > 0 ? "warn" : "ok");
    onDone();
  } catch (err) {
    toast(err.message, "error");
  }
}

export async function openAgentInspectModal(sessionID, onDone) {
  let detail;
  let msgData;
  try {
    const [dRes, mRes] = await Promise.all([
      api("GET", `/v1/agents/${encodeURIComponent(sessionID)}`),
      api("GET", `/v1/agents/messages?session=${encodeURIComponent(sessionID)}&limit=30`),
    ]);
    detail = dRes.agent;
    msgData = mRes.messages || [];
  } catch (err) {
    toast(err.message, "error");
    return;
  }

  const root = el("div", { className: "form-grid" });
  const pending = detail.pending_action || null;
  if (pending) {
    root.appendChild(
      el(
        "div",
        { className: "alert-banner alert-warn" },
        el(
          "div",
          null,
          el("strong", { text: `${detail.state} (${formatDuration(detail.waiting_seconds || 0)})` }),
          " · ",
          el("span", { className: "mono", text: [pending.kind, pending.tool, pending.event].filter(Boolean).join(" / ") }),
          pending.detail ? el("div", { className: "mono small mt-1", text: pending.detail }) : null,
        ),
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
        el("h3", { className: "card-title", text: detail.name || detail.session_id }),
        el(
          "div",
          { className: "actions-row" },
          badge(`${detail.state}${detail.live ? "" : " (stale)"}`, stateVariant(detail.state, detail.live)),
          badge(detail.delivery, deliveryVariant(detail.delivery)),
          makeCopyBtn(detail.session_id, "Copy ID"),
        ),
      ),
      el(
        "dl",
        { className: "kv-list" },
        el("dt", { className: "kv-label", text: "Session ID" }),
        el("dd", { className: "kv-value mono", text: detail.session_id }),
        el("dt", { className: "kv-label", text: "Device / Harness" }),
        el("dd", { className: "kv-value", text: `${detail.device} / ${detail.harness}${detail.pid ? ` (pid ${detail.pid})` : ""}` }),
        el("dt", { className: "kv-label", text: "Repo / Branch" }),
        el("dd", { className: "kv-value mono", text: detail.repo ? `${detail.repo}${detail.branch ? `@${detail.branch}` : ""}` : "-" }),
        el("dt", { className: "kv-label", text: "Working Dir" }),
        el("dd", { className: "kv-value mono", text: detail.cwd || "-" }),
        el("dt", { className: "kv-label", text: "Title" }),
        el("dd", { className: "kv-value", text: detail.title || detail.summary || "-" }),
        el("dt", { className: "kv-label", text: "Updated" }),
        el("dd", { className: "kv-value", text: `${relTime(detail.updated_at)} (started ${relTime(detail.started_at)})` }),
      ),
    ),
  );

  if (canControlSession(detail)) {
    const renameInput = el("input", { type: "text", className: "grow", "aria-label": "Session handle", value: detail.name || "", placeholder: "session name" });
    const controlRow = el(
      "div",
      { className: "actions-row" },
      renameInput,
      el("button", {
        type: "button",
        className: "btn btn-sm",
        text: "Rename",
        onclick: async () => {
          try {
            const res = await api("POST", "/v1/agents/rename", { session_id: detail.session_id, name: renameInput.value.trim() });
            toast(`Renamed -> ${res.name}`, "ok");
            if (onDone) {
              onDone();
            }
            openAgentInspectModal(detail.session_id, onDone);
          } catch (err) {
            toast(err.message, "error");
          }
        },
      }),
      detail.state !== "exited"
        ? el("button", {
            type: "button",
            className: "btn btn-sm btn-danger",
            text: "Kill Session",
            onclick: () => killSession(detail, () => {
              closeModal();
              if (onDone) {
                onDone();
              }
            }),
          })
        : null,
    );
    root.appendChild(controlRow);
  }

  if (detail.git_status) {
    root.appendChild(
      el(
        "details",
        { className: "details-box m-0" },
        el("summary", { text: "Git Status" }),
        el("pre", { className: "code-block mt-2", text: detail.git_status }),
      ),
    );
  }

  const tailCard = el("div", { className: "card" }, el("h3", { className: "card-title mb-2", text: `Transcript Tail (${(detail.tail || []).length})` }));
  if (!detail.tail || detail.tail.length === 0) {
    tailCard.appendChild(el("div", { className: "muted small", text: "No transcript turns captured." }));
  } else {
    const tailList = el("div", { className: "tail-list" });
    for (const item of detail.tail) {
      tailList.appendChild(
        el("div", { className: "tail-item" }, el("div", { className: "tail-role", text: item.role }), el("div", { className: "tail-text", text: item.text })),
      );
    }
    tailCard.appendChild(tailList);
  }
  root.appendChild(tailCard);

  const msgsCard = el("div", { className: "card" }, el("h3", { className: "card-title mb-2", text: `Messages (${msgData.length})` }));
  if (msgData.length === 0) {
    msgsCard.appendChild(el("div", { className: "muted small", text: "No messages for this session." }));
  } else {
    for (const m of msgData.slice(0, 12)) {
      msgsCard.appendChild(
        el(
          "div",
          { className: "message-card" },
          el(
            "div",
            { className: "message-meta" },
            el("span", { text: `${m.from_name || m.from_session} → ${formatMessageTarget(m)} · thread ${m.thread_id}` }),
            el("span", { text: relTime(m.created_at) }),
          ),
          el("div", { className: "message-body small", text: m.body }),
        ),
      );
    }
  }
  root.appendChild(msgsCard);

  root.appendChild(
    el(
      "div",
      { className: "card" },
      el("h3", { className: "card-title mb-2", text: "Send Message to Session" }),
      buildSendMessageForm(detail.session_id, { hideTo: true, compact: true }, () => {
        openAgentInspectModal(detail.session_id, onDone);
      }),
    ),
  );

  openModal(`Session: ${detail.name || detail.session_id}`, root, {
    onClose: () => {
      state.editing = false;
    },
  });
}

function renderSessionsView(root, sessions, ctx, pathsCatalog) {
  const devices = [...new Set(sessions.map((s) => s.device).filter(Boolean))].sort();
  const harnesses = [...new Set(sessions.map((s) => s.harness).filter(Boolean))].sort();
  const repos = [...new Set([...sessions.map((s) => s.repo), ...(pathsCatalog.repos || [])].filter(Boolean))].sort();

  const tableContainer = el("div");
  const chips = [
    ["live", "Live"],
    ["waiting", "Waiting"],
    ["working", "Working"],
    ["idle", "Idle"],
    ["all", "All (incl. stale)"],
  ].map(([val, label]) =>
    el("button", {
      type: "button",
      className: `chip${agentsFilter.stateChip === val ? " active" : ""}`,
      dataset: { state: val },
      text: label,
      onclick: () => {
        agentsFilter.stateChip = val;
        for (const btn of chips) {
          btn.classList.toggle("active", btn.dataset.state === val);
        }
        drawTable();
      },
    }),
  );

  const devSel = el(
    "select",
    {
      "aria-label": "Filter by device",
      onchange: (ev) => {
        agentsFilter.device = ev.target.value;
        drawTable();
      },
    },
    el("option", { value: "", text: "All devices", selected: agentsFilter.device === "" }),
    ...devices.map((d) => el("option", { value: d, text: d, selected: agentsFilter.device === d })),
  );

  const harSel = el(
    "select",
    {
      "aria-label": "Filter by harness",
      onchange: (ev) => {
        agentsFilter.harness = ev.target.value;
        drawTable();
      },
    },
    el("option", { value: "", text: "All harnesses", selected: agentsFilter.harness === "" }),
    ...harnesses.map((h) => el("option", { value: h, text: h, selected: agentsFilter.harness === h })),
  );

  const repoSel = el(
    "select",
    {
      "aria-label": "Filter by repo",
      onchange: (ev) => {
        agentsFilter.repo = ev.target.value;
        drawTable();
      },
    },
    el("option", { value: "", text: "All repos", selected: agentsFilter.repo === "" }),
    ...repos.map((r) => el("option", { value: r, text: r, selected: agentsFilter.repo === r })),
  );

  const queryInput = el("input", {
    type: "search",
    className: "grow",
    "aria-label": "Search sessions",
    placeholder: "Search name, title, repo, session…",
    value: agentsFilter.query,
    oninput: (ev) => {
      agentsFilter.query = ev.target.value;
      drawTable();
    },
  });

  const computeScopeStats = () => {
    const scopeDevice = state.isAdmin ? agentsFilter.device : state.caller ? state.caller.name : "";
    const liveByHarness = { claude: 0, codex: 0, antigravity: 0 };
    let totalLiveForScope = 0;
    for (const s of sessions) {
      if (!s.live) {
        continue;
      }
      if (scopeDevice && s.device !== scopeDevice) {
        continue;
      }
      totalLiveForScope++;
      if (s.harness in liveByHarness) {
        liveByHarness[s.harness]++;
      }
    }
    return { scopeDevice, liveByHarness, totalLiveForScope };
  };

  const killSummary = el("summary");
  const killButtonsRow = el("div", { className: "actions-row mt-2" });
  const killControls = el("details", { className: "details-box m-0" }, killSummary, killButtonsRow);

  const updateBulkControls = () => {
    const { scopeDevice, liveByHarness, totalLiveForScope } = computeScopeStats();
    killSummary.textContent = scopeDevice ? `Bulk Session Controls (${scopeDevice})` : "Bulk Session Controls (all devices)";
    clear(killButtonsRow);
    for (const [h, label] of [
      ["claude", "Claude"],
      ["codex", "Codex"],
      ["antigravity", "Antigravity"],
    ]) {
      const cnt = liveByHarness[h];
      killButtonsRow.appendChild(
        el("button", {
          type: "button",
          className: "btn btn-sm",
          text: cnt > 0 ? `Kill ${label} (${cnt})` : `Kill ${label}`,
          onclick: () => {
            const latest = computeScopeStats();
            killAllAgents(h, `${label} Agents`, latest.scopeDevice, latest.liveByHarness[h], () => ctx.refresh());
          },
        }),
      );
    }
    killButtonsRow.appendChild(
      el("button", {
        type: "button",
        className: "btn btn-sm btn-danger",
        text: totalLiveForScope > 0 ? `Kill All Agents (${totalLiveForScope})` : "Kill All Agents",
        onclick: () => {
          const latest = computeScopeStats();
          killAllAgents("all", "All Agents", latest.scopeDevice, latest.totalLiveForScope, () => ctx.refresh());
        },
      }),
    );
  };

  const drawTable = () => {
    updateBulkControls();
    clear(tableContainer);
    const q = agentsFilter.query.trim().toLowerCase();
    const filtered = sessions.filter((s) => {
      if (agentsFilter.stateChip === "live" && !s.live) {
        return false;
      }
      if (agentsFilter.stateChip === "waiting" && (!s.live || (s.state !== "waiting_permission" && s.state !== "waiting_question"))) {
        return false;
      }
      if (agentsFilter.stateChip === "working" && (!s.live || s.state !== "working")) {
        return false;
      }
      if (agentsFilter.stateChip === "idle" && (!s.live || s.state !== "idle")) {
        return false;
      }
      if (agentsFilter.device && s.device !== agentsFilter.device) {
        return false;
      }
      if (agentsFilter.harness && s.harness !== agentsFilter.harness) {
        return false;
      }
      if (agentsFilter.repo && s.repo !== agentsFilter.repo) {
        return false;
      }
      if (!q) {
        return true;
      }
      return [s.name, s.title, s.summary, s.repo, s.branch, s.cwd, s.session_id, s.device, s.harness].some((v) =>
        String(v || "").toLowerCase().includes(q),
      );
    });

    if (filtered.length === 0) {
      tableContainer.appendChild(el("div", { className: "empty-state", text: "No agent sessions match the current filters." }));
      return;
    }

    const tbody = el("tbody");
    for (const ag of filtered) {
      const pending = ag.pending_action || {};
      const pendingText = [pending.tool, pending.detail].filter(Boolean).join(": ");
      const repoLabel = ag.repo ? (ag.branch ? `${ag.repo}@${ag.branch}` : ag.repo) : ag.cwd || "-";
      const actions = el(
        "div",
        { className: "actions-row" },
        el("button", {
          type: "button",
          className: "btn btn-sm",
          text: "Inspect",
          onclick: (ev) => {
            ev.stopPropagation();
            openAgentInspectModal(ag.session_id, () => ctx.refresh());
          },
        }),
        el("button", {
          type: "button",
          className: "btn btn-sm",
          text: "Message",
          onclick: (ev) => {
            ev.stopPropagation();
            openQuickMessageModal(ag.session_id, {}, () => ctx.refresh());
          },
        }),
      );
      if (canControlSession(ag)) {
        actions.appendChild(
          el("button", {
            type: "button",
            className: "btn btn-sm",
            text: "Rename",
            onclick: (ev) => {
              ev.stopPropagation();
              renameSessionPrompt(ag, () => ctx.refresh());
            },
          }),
        );
        if (ag.state !== "exited") {
          actions.appendChild(
            el("button", {
              type: "button",
              className: "btn btn-sm btn-danger",
              text: "Kill",
              onclick: (ev) => {
                ev.stopPropagation();
                killSession(ag, () => ctx.refresh());
              },
            }),
          );
        }
      }

      const waitStr = ag.waiting_seconds != null ? ` (${formatDuration(ag.waiting_seconds)})` : "";
      tbody.appendChild(
        el(
          "tr",
          {
            className: "row-clickable",
            onclick: () => openAgentInspectModal(ag.session_id, () => ctx.refresh()),
          },
          cell(
            "Session",
            { className: "cell-primary" },
            el("div", null, el("strong", { text: ag.name || "-" })),
            ag.title || ag.summary ? el("div", { className: "small muted", text: ag.title || ag.summary }) : null,
            el("div", { className: "mono small muted", text: ag.session_id }),
          ),
          cell("Device / Harness", null, el("div", { text: ag.device }), badge(ag.harness, "neutral")),
          cell("Repo", { className: "mono small", text: repoLabel }),
          cell(
            "State",
            null,
            badge(`${ag.state}${!ag.live ? " (stale)" : waitStr}`, stateVariant(ag.state, ag.live)),
            pendingText ? el("div", { className: "mono small mt-1", text: pendingText }) : null,
          ),
          cell("Delivery", null, badge(ag.delivery, deliveryVariant(ag.delivery)), ag.listening ? el("div", { className: "small muted mt-1", text: "listening" }) : null),
          cell("Last Seen", { text: relTime(ag.updated_at) }),
          cell("Actions", { className: "cell-actions" }, actions),
        ),
      );
    }

    tableContainer.appendChild(
      el(
        "div",
        { className: "table-wrap" },
        el(
          "table",
          { className: "responsive-table" },
          el(
            "thead",
            null,
            el(
              "tr",
              null,
              el("th", { text: "Session" }),
              el("th", { text: "Device / Harness" }),
              el("th", { text: "Repo" }),
              el("th", { text: "State" }),
              el("th", { text: "Delivery" }),
              el("th", { text: "Last Seen" }),
              el("th", { text: "Actions" }),
            ),
          ),
          tbody,
        ),
      ),
    );
  };

  root.appendChild(
    section(
      "Agent Sessions",
      { className: "ui-section" },
      el("div", { className: "toolbar" }, el("div", { className: "chips-row" }, chips), devSel, harSel, repoSel, queryInput),
      tableContainer,
    ),
  );

  root.appendChild(section(null, { ariaLabel: "Bulk session controls", className: "ui-section" }, killControls));

  drawTable();
}

async function renderMessagesView(root, sessions, ctx, pathsCatalog) {
  const signal = ctx && ctx.signal;
  const liveSessions = sessions.filter((s) => s.live);
  const devices = [...new Set(sessions.map((s) => s.device).filter(Boolean))].sort();
  const repos = [...new Set([...sessions.map((s) => s.repo), ...(pathsCatalog.repos || [])].filter(Boolean))].sort();

  const feedBox = el("div");
  const loadFeed = async () => {
    const q = new URLSearchParams();
    if (agentsFilter.msgSession) {
      q.set("session", agentsFilter.msgSession);
    }
    if (agentsFilter.msgDevice) {
      q.set("device", agentsFilter.msgDevice);
    }
    if (agentsFilter.msgThread) {
      q.set("thread", agentsFilter.msgThread);
    }
    q.set("limit", String(agentsFilter.msgLimit));
    try {
      const res = await api("GET", `/v1/agents/messages?${q.toString()}`, undefined, { signal });
      if (signal && signal.aborted) {
        return;
      }
      renderFeed(res.messages || []);
    } catch (err) {
      if ((err && err.name === "AbortError") || (signal && signal.aborted)) {
        return;
      }
      clear(feedBox);
      feedBox.appendChild(el("div", { className: "alert-banner alert-danger", text: err.message }));
    }
  };

  const renderFeed = (msgs) => {
    clear(feedBox);
    if (msgs.length === 0) {
      feedBox.appendChild(el("div", { className: "empty-state", text: "No messages match the current filters." }));
      return;
    }
    for (const m of msgs) {
      const sender = m.from_name ? `${m.from_name} (${m.from_device})` : m.from_session;
      feedBox.appendChild(
        el(
          "article",
          { className: "message-card" },
          el(
            "div",
            { className: "message-meta" },
            el(
              "span",
              null,
              el("strong", { text: `#${m.id} ${sender}` }),
              " → ",
              el("span", { className: "mono", text: formatMessageTarget(m) }),
              " · thread ",
              el("span", { className: "mono", text: m.thread_id }),
              m.reply_to ? ` (reply to #${m.reply_to})` : "",
            ),
            el("span", { text: relTime(m.created_at) }),
          ),
          el("div", { className: "message-body mb-2", text: m.body }),
          el(
            "div",
            { className: "actions-row" },
            el("button", {
              type: "button",
              className: "btn btn-sm",
              text: "Reply",
              onclick: () => openQuickMessageModal(m.from_session, { thread_id: m.thread_id, reply_to: m.id }, () => loadFeed()),
            }),
            el("button", {
              type: "button",
              className: "btn btn-sm",
              text: "Filter Thread",
              onclick: () => {
                agentsFilter.msgThread = m.thread_id;
                threadFilterInput.value = m.thread_id;
                loadFeed();
              },
            }),
          ),
        ),
      );
    }
  };

  const presetSel = el(
    "select",
    { "aria-label": "Recipient preset" },
    el("option", { value: "", text: "Pick recipient preset…" }),
    el("option", { value: "all", text: "all (broadcast to fleet)" }),
    ...devices.map((d) => el("option", { value: `device:${d}`, text: `device:${d}` })),
    ...repos.map((r) => el("option", { value: `repo:${r}`, text: `repo:${r}` })),
    ...liveSessions.map((s) => el("option", { value: s.session_id, text: `${s.name || s.session_id} (${s.device}/${s.harness})` })),
  );

  const targetSuggestions = [
    { value: "all", label: "broadcast to fleet" },
    ...devices.map((d) => ({ value: `device:${d}`, label: "device" })),
    ...repos.map((r) => ({ value: `repo:${r}`, label: "repo" })),
    ...liveSessions.map((s) => ({ value: s.session_id, label: `${s.name || s.session_id} (${s.device}/${s.harness})` })),
  ];
  const composeCard = el(
    "div",
    { className: "card" },
    el("div", { className: "card-header" }, el("h3", { className: "card-title", text: "New Message" }), presetSel),
  );
  const composeForm = buildSendMessageForm("", { targets: targetSuggestions }, () => loadFeed());
  const toField = composeForm.querySelector('input[type="text"]');
  presetSel.addEventListener("change", () => {
    if (presetSel.value && toField) {
      toField.value = presetSel.value;
    }
  });
  composeCard.appendChild(composeForm);
  root.appendChild(section("Compose Message", { className: "ui-section" }, composeCard));

  const sessionFilterInput = el("input", {
    type: "text",
    "aria-label": "Filter by session ID",
    placeholder: "Filter session_id…",
    value: agentsFilter.msgSession,
  });
  const threadFilterInput = el("input", {
    type: "text",
    "aria-label": "Filter by thread ID",
    placeholder: "Filter thread_id…",
    value: agentsFilter.msgThread,
  });
  const devFilterSel = el(
    "select",
    { "aria-label": "Filter messages by device" },
    el("option", { value: "", text: "All devices", selected: agentsFilter.msgDevice === "" }),
    ...devices.map((d) => el("option", { value: d, text: d, selected: agentsFilter.msgDevice === d })),
  );
  const applyBtn = el("button", {
    type: "button",
    className: "btn btn-sm btn-primary",
    text: "Apply",
    onclick: () => {
      agentsFilter.msgSession = sessionFilterInput.value.trim();
      agentsFilter.msgThread = threadFilterInput.value.trim();
      agentsFilter.msgDevice = devFilterSel.value;
      loadFeed();
    },
  });
  const clearBtn = el("button", {
    type: "button",
    className: "btn btn-sm",
    text: "Clear",
    onclick: () => {
      agentsFilter.msgSession = "";
      agentsFilter.msgThread = "";
      agentsFilter.msgDevice = "";
      sessionFilterInput.value = "";
      threadFilterInput.value = "";
      devFilterSel.value = "";
      loadFeed();
    },
  });

  root.appendChild(
    section(
      "Message Feed",
      { className: "ui-section" },
      el("div", { className: "toolbar" }, devFilterSel, sessionFilterInput, threadFilterInput, applyBtn, clearBtn),
      feedBox,
    ),
  );
  await loadFeed();
}

function renderUsageAndPathsView(root, sessions, ctx, pathsCatalog) {
  const devices = [...new Set(sessions.map((s) => s.device).filter(Boolean))].sort();
  root.appendChild(
    section(
      "Harness Usage & Quotas",
      {
        subtitle: "Claude statusLine · Codex app-server · Antigravity",
        className: "ui-section",
      },
      renderUsageCard((state.overview && state.overview.usage) || [], { omitHeader: true }),
    ),
  );
  root.appendChild(
    section(
      "Known Directories & Repos",
      {
        subtitle: "Captured automatically from heartbeats, spawns, and Slacker configs",
        className: "ui-section",
      },
      renderTrackedPathsBox(
        pathsCatalog,
        {
          open: true,
          devices,
          onUse: state.isAdmin
            ? (p) => openSpawnSessionModal(() => ctx.refresh(), { device: p.device, repo_path: p.subpath || p.tilde_path || p.path })
            : null,
          useLabel: "Spawn",
        },
        () => ctx.refresh(),
      ),
    ),
  );
}

export async function renderAgents(root, ctx, subRoute = "") {
  const signal = ctx && ctx.signal;
  if (subRoute === "waiting") {
    agentsFilter.tab = "sessions";
    agentsFilter.stateChip = "waiting";
  } else if (subRoute === "messages") {
    agentsFilter.tab = "messages";
  } else if (subRoute === "usage") {
    agentsFilter.tab = "usage";
  } else if (subRoute === "sessions") {
    agentsFilter.tab = "sessions";
  }

  let sessions = [];
  let pathsCatalog = { paths: [], repos: [], workspaces: {} };
  try {
    const [res, cat] = await Promise.all([
      api("GET", "/v1/agents?include_stale=true", undefined, { signal }),
      fetchPathsCatalog("", { signal }),
    ]);
    if (signal && signal.aborted) {
      return;
    }
    sessions = res.agents || [];
    pathsCatalog = cat;
  } catch (err) {
    if ((err && err.name === "AbortError") || (signal && signal.aborted)) {
      return;
    }
    clear(root);
    root.appendChild(el("div", { className: "alert-banner alert-danger", text: err.message }));
    return;
  }

  const draw = async () => {
    clear(root);
    const liveCount = sessions.filter((s) => s.live).length;
    const waitCount = sessions.filter((s) => s.live && (s.state === "waiting_permission" || s.state === "waiting_question")).length;

    const headerActions = el("div", { className: "actions-row" });
    if (state.isAdmin) {
      headerActions.appendChild(
        el("button", {
          type: "button",
          className: "btn btn-primary",
          text: "+ Spawn Session",
          onclick: () => openSpawnSessionModal(() => ctx.refresh()),
        }),
      );
    }
    headerActions.appendChild(
      el("button", {
        type: "button",
        className: state.isAdmin ? "btn" : "btn btn-primary",
        text: "Broadcast / Compose",
        onclick: () => openQuickMessageModal("all", {}, () => ctx.refresh()),
      }),
    );

    root.appendChild(
      el(
        "header",
        { className: "page-header" },
        el(
          "div",
          null,
          el("h1", { className: "page-title", text: "AI Agents" }),
          el("p", { className: "page-subtitle", text: `${liveCount} live · ${waitCount} waiting · ${sessions.length} retained` }),
        ),
        headerActions,
      ),
    );

    const tabDefs = [
      ["sessions", `Sessions (${liveCount})`, "#agents"],
      ["messages", "Messages", "#agents/messages"],
      ["usage", "Quotas & Paths", "#agents/usage"],
    ];
    root.appendChild(
      el(
        "div",
        { className: "sub-tabs", role: "tablist", "aria-label": "Agent views" },
        ...tabDefs.map(([id, label, hash]) =>
          el("button", {
            type: "button",
            role: "tab",
            "aria-selected": agentsFilter.tab === id ? "true" : "false",
            className: `sub-tab${agentsFilter.tab === id ? " active" : ""}`,
            text: label,
            onclick: () => {
              agentsFilter.tab = id;
              ctx.navigate(hash);
            },
          }),
        ),
      ),
    );

    if (agentsFilter.tab === "messages") {
      await renderMessagesView(root, sessions, ctx, pathsCatalog);
    } else if (agentsFilter.tab === "usage") {
      renderUsageAndPathsView(root, sessions, ctx, pathsCatalog);
    } else {
      renderSessionsView(root, sessions, ctx, pathsCatalog);
    }
  };

  await draw();
}
