import {
  api,
  badge,
  clear,
  closeModal,
  confirmDialog,
  el,
  formatDuration,
  makeCopyBtn,
  openModal,
  relTime,
  state,
  toast,
} from "./lib.js";

const agentsFilter = {
  tab: "sessions", // sessions | messages
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

function formatMessageTarget(m) {
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

function buildSendMessageForm(initialTo = "", opts = {}, onSent = null) {
  const toInput = el("input", { type: "text", value: initialTo, placeholder: "session name/id, device:<name>, repo:<name>, or all" });
  const threadInput = el("input", { type: "text", value: opts.thread_id || "", placeholder: "Optional thread_id" });
  const replyToInput = el("input", { type: "number", value: opts.reply_to ? String(opts.reply_to) : "", placeholder: "Optional reply_to message ID" });
  const bodyInput = el("textarea", { rows: 3, placeholder: "Message to deliver to agent inbox / live listener…" });
  const reportBox = el("div");
  const sendBtn = el("button", { type: "submit", className: "btn btn-primary", text: "Send Message" });

  const markEditing = () => {
    state.editing = Boolean(bodyInput.value.trim());
  };
  bodyInput.addEventListener("input", markEditing);

  const form = el(
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
    opts.hideTo
      ? null
      : el("div", { className: "field" }, el("label", { text: "To" }), toInput),
    opts.compact
      ? null
      : el(
          "div",
          { className: "form-row-2" },
          el("div", { className: "field" }, el("label", { text: "Thread ID (optional)" }), threadInput),
          el("div", { className: "field" }, el("label", { text: "Reply-To ID (optional)" }), replyToInput),
        ),
    el("div", { className: "field" }, el("label", { text: "Message Body" }), bodyInput),
    el("div", { className: "actions-row flex-end" }, sendBtn),
    reportBox,
  );
  return form;
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

function workerIsOnline(runtimes, dev) {
  const rt = (runtimes || []).find((r) => r.role === `worker:${dev}`);
  if (!rt || !rt.connected) {
    return false;
  }
  const ts = Date.parse(rt.last_poll_at || rt.updated_at || "");
  return Number.isFinite(ts) && Date.now() - ts <= 90 * 1000;
}

export async function openSpawnSessionModal(onSpawned = null) {
  let statusRes;
  let devicesRes;
  try {
    [statusRes, devicesRes] = await Promise.all([api("GET", "/v1/slacker/status"), api("GET", "/v1/devices")]);
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
  const globalPolicyLabel = cfg.default_policy || "bypass";
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
    el("option", { value: "bypass", text: "Bypass (skip permissions)" }),
    el("option", { value: "auto", text: "Auto" }),
    el("option", { value: "safe", text: "Safe (manual / plan)" }),
  );
  const stratSel = el(
    "select",
    null,
    el("option", { value: "", text: `Default (${globalStrategyLabel})` }),
    el("option", { value: "least_agents", text: "Least agents" }),
    el("option", { value: "round_robin", text: "Round-robin" }),
  );
  const repoIn = el("input", { type: "text", placeholder: "subpath (e.g. fleet), blank=workspace, or ~/path" });
  const nameIn = el("input", { type: "text", placeholder: "auto-generated if blank" });
  const promptIn = el("textarea", { rows: 4, placeholder: "Optional prompt (leave blank for an interactive session)" });
  const tmuxChk = el("input", { type: "checkbox" });

  let selectedDevices = defaultDevices.length > 0 ? [] : devices.length > 0 ? [devices[0]] : [];
  const poolHint = el("span", {
    className: "small muted",
    text: selectedDevices.length === 0 ? `default (${defaultDevices.length > 0 ? defaultDevices.join(", ") : "none"})` : "",
  });
  const chipButtons = devices.map((devName) => {
    const load = (agentCounts[devName] || 0) + (pendingCounts[devName] || 0);
    const online = workerIsOnline(runtimes, devName);
    const btn = el("button", {
      type: "button",
      className: `chip${selectedDevices.includes(devName) ? " active" : ""}`,
      text: `${devName} (${load})${online ? " · online" : ""}`,
      onclick: () => {
        const cur = new Set(selectedDevices);
        if (cur.has(devName)) {
          cur.delete(devName);
        } else {
          cur.add(devName);
        }
        selectedDevices = devices.filter((d) => cur.has(d));
        btn.classList.toggle("active", cur.has(devName));
        poolHint.textContent = selectedDevices.length === 0 ? `default (${defaultDevices.length > 0 ? defaultDevices.join(", ") : "none"})` : "";
      },
    });
    return btn;
  });

  const markEditing = () => {
    state.editing = Boolean(promptIn.value.trim() || repoIn.value.trim() || nameIn.value.trim());
  };
  promptIn.addEventListener("input", markEditing);
  repoIn.addEventListener("input", markEditing);
  nameIn.addEventListener("input", markEditing);

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
      el("div", { className: "field" }, el("label", { text: "Harness" }), harnessSel),
      el("div", { className: "field" }, el("label", { text: "Permission Policy" }), policySel),
    ),
    el(
      "div",
      { className: "field" },
      el("label", { text: "Target Machine(s)" }),
      el("div", { className: "chips-row" }, ...chipButtons, poolHint),
    ),
    el(
      "div",
      { className: "form-row-2" },
      el("div", { className: "field" }, el("label", { text: "Routing Strategy" }), stratSel),
      el("div", { className: "field" }, el("label", { text: "Session Name (optional)" }), nameIn),
    ),
    el(
      "div",
      { className: "field" },
      el("label", { text: "Directory / Repo" }),
      repoIn,
      wsHints.length > 0 ? el("div", { className: "field-hint mono", text: `Workspaces: ${wsHints.join(" · ")}` }) : null,
    ),
    el("div", { className: "field" }, el("label", { text: "Initial Prompt (optional)" }), promptIn),
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
    el("div", { className: "field" }, el("label", { text: "Session Handle" }), input),
    el(
      "div",
      { className: "actions-row flex-end" },
      el("button", { type: "button", className: "btn", text: "Cancel", onclick: closeModal }),
      saveBtn,
    ),
  );
  openModal(`Rename ${ag.session_id}`, form);
}

async function deregisterSession(ag, onDone) {
  const ok = await confirmDialog(
    "Mark Session Exited?",
    `Mark ${ag.name || ag.session_id} as exited and wake any active listeners?`,
    "Mark Exited",
    true,
  );
  if (!ok) {
    return;
  }
  try {
    await api("POST", "/v1/agents/deregister", { session_id: ag.session_id });
    toast(`Marked ${ag.name || ag.session_id} exited`, "ok");
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
        el("h4", { className: "card-title", text: detail.name || detail.session_id }),
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
    const renameInput = el("input", { type: "text", className: "grow", value: detail.name || "", placeholder: "session name" });
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
            text: "Mark Exited",
            onclick: () => deregisterSession(detail, () => {
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

  const tailCard = el("div", { className: "card" }, el("h4", { className: "card-title mb-2", text: `Transcript Tail (${(detail.tail || []).length})` }));
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

  const msgsCard = el("div", { className: "card" }, el("h4", { className: "card-title mb-2", text: `Messages (${msgData.length})` }));
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
      el("h4", { className: "card-title mb-2", text: "Send Message to Session" }),
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

function renderSessionsView(root, sessions, ctx) {
  const devices = [...new Set(sessions.map((s) => s.device).filter(Boolean))].sort();
  const harnesses = [...new Set(sessions.map((s) => s.harness).filter(Boolean))].sort();

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
      onchange: (ev) => {
        agentsFilter.harness = ev.target.value;
        drawTable();
      },
    },
    el("option", { value: "", text: "All harnesses", selected: agentsFilter.harness === "" }),
    ...harnesses.map((h) => el("option", { value: h, text: h, selected: agentsFilter.harness === h })),
  );

  const queryInput = el("input", {
    type: "search",
    placeholder: "Search name, title, repo, session…",
    value: agentsFilter.query,
    oninput: (ev) => {
      agentsFilter.query = ev.target.value;
      drawTable();
    },
  });

  root.appendChild(el("div", { className: "toolbar" }, el("div", { className: "chips-row" }, chips), devSel, harSel, el("div", { className: "toolbar-spacer" }), queryInput));
  root.appendChild(tableContainer);

  const drawTable = () => {
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
              text: "Exit",
              onclick: (ev) => {
                ev.stopPropagation();
                deregisterSession(ag, () => ctx.refresh());
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
          el(
            "td",
            null,
            el("div", null, el("strong", { text: ag.name || "-" })),
            ag.title || ag.summary ? el("div", { className: "small muted", text: ag.title || ag.summary }) : null,
            el("div", { className: "mono small muted", text: ag.session_id }),
          ),
          el("td", null, el("div", { text: ag.device }), badge(ag.harness, "neutral")),
          el("td", { className: "mono small", text: repoLabel }),
          el(
            "td",
            null,
            badge(`${ag.state}${!ag.live ? " (stale)" : waitStr}`, stateVariant(ag.state, ag.live)),
            pendingText ? el("div", { className: "mono small mt-1", text: pendingText }) : null,
          ),
          el("td", null, badge(ag.delivery, deliveryVariant(ag.delivery)), ag.listening ? el("div", { className: "small muted mt-1", text: "listening" }) : null),
          el("td", { text: relTime(ag.updated_at) }),
          el("td", null, actions),
        ),
      );
    }

    tableContainer.appendChild(
      el(
        "div",
        { className: "table-wrap" },
        el(
          "table",
          null,
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

  drawTable();
}

async function renderMessagesView(root, sessions, ctx) {
  const liveSessions = sessions.filter((s) => s.live);
  const devices = [...new Set(sessions.map((s) => s.device).filter(Boolean))].sort();
  const repos = [...new Set(sessions.map((s) => s.repo).filter(Boolean))].sort();

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
      const res = await api("GET", `/v1/agents/messages?${q.toString()}`);
      renderFeed(res.messages || []);
    } catch (err) {
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
          "div",
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
    null,
    el("option", { value: "", text: "Pick recipient preset…" }),
    el("option", { value: "all", text: "all (broadcast to fleet)" }),
    ...devices.map((d) => el("option", { value: `device:${d}`, text: `device:${d}` })),
    ...repos.map((r) => el("option", { value: `repo:${r}`, text: `repo:${r}` })),
    ...liveSessions.map((s) => el("option", { value: s.session_id, text: `${s.name || s.session_id} (${s.device}/${s.harness})` })),
  );

  const composeCard = el(
    "div",
    { className: "card mb-3" },
    el("div", { className: "card-header" }, el("h3", { className: "card-title", text: "Send Message" }), presetSel),
  );
  const composeForm = buildSendMessageForm("", {}, () => loadFeed());
  const toField = composeForm.querySelector('input[type="text"]');
  presetSel.addEventListener("change", () => {
    if (presetSel.value && toField) {
      toField.value = presetSel.value;
    }
  });
  composeCard.appendChild(composeForm);
  root.appendChild(composeCard);

  const sessionFilterInput = el("input", {
    type: "text",
    placeholder: "Filter session_id…",
    value: agentsFilter.msgSession,
  });
  const threadFilterInput = el("input", {
    type: "text",
    placeholder: "Filter thread_id…",
    value: agentsFilter.msgThread,
  });
  const devFilterSel = el(
    "select",
    null,
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

  root.appendChild(el("div", { className: "toolbar" }, devFilterSel, sessionFilterInput, threadFilterInput, applyBtn, clearBtn));
  root.appendChild(feedBox);
  await loadFeed();
}

export async function renderAgents(root, ctx, subRoute = "") {
  if (subRoute === "waiting") {
    agentsFilter.tab = "sessions";
    agentsFilter.stateChip = "waiting";
  } else if (subRoute === "messages") {
    agentsFilter.tab = "messages";
  } else if (subRoute === "sessions") {
    agentsFilter.tab = "sessions";
  }

  let sessions = [];
  try {
    const res = await api("GET", "/v1/agents?include_stale=true");
    sessions = res.agents || [];
  } catch (err) {
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
        "div",
        { className: "page-header" },
        el(
          "div",
          null,
          el("h1", { className: "page-title", text: "AI Agent Sessions & Messages" }),
          el("p", { className: "page-subtitle", text: `${liveCount} live · ${waitCount} waiting · ${sessions.length} retained` }),
        ),
        headerActions,
      ),
    );

    const tabs = el(
      "div",
      { className: "sub-tabs" },
      el("button", {
        type: "button",
        className: `sub-tab${agentsFilter.tab === "sessions" ? " active" : ""}`,
        text: `Sessions (${liveCount} live)`,
        onclick: () => {
          agentsFilter.tab = "sessions";
          ctx.navigate("#agents");
        },
      }),
      el("button", {
        type: "button",
        className: `sub-tab${agentsFilter.tab === "messages" ? " active" : ""}`,
        text: "Messages",
        onclick: () => {
          agentsFilter.tab = "messages";
          ctx.navigate("#agents/messages");
        },
      }),
    );
    root.appendChild(tabs);

    if (agentsFilter.tab === "messages") {
      await renderMessagesView(root, sessions, ctx);
    } else {
      renderSessionsView(root, sessions, ctx);
    }
  };

  await draw();
}
