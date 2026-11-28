import {
  api,
  badge,
  cell,
  clear,
  closeModal,
  confirmDialog,
  el,
  fetchPathsCatalog,
  field,
  makeCopyBtn,
  openModal,
  relTime,
  section,
  state,
  toast,
} from "./lib.js";

const memState = {
  tab: "entries", // entries | proposals | context | history | curator
  q: "",
  scope: "",
  kind: "",
  tier: "",
  status: "active",
  limit: 50,
  propStatus: "pending",
  selectedProps: new Set(),
  ctxRepo: "",
  ctxDevice: "",
  curatorReports: null,
};

function openNewMemoryModal(scopes, onDone) {
  const scopeInput = el("input", { type: "text", list: "mem-scopes-list", value: memState.scope || "global", placeholder: "global, repo:fleet, device:laptop" });
  const keyInput = el("input", { type: "text", placeholder: "short-slug-key" });
  const kindSel = el(
    "select",
    null,
    ...["preference", "feedback", "project", "reference"].map((k) => el("option", { value: k, text: k })),
  );
  const tierSel = el(
    "select",
    null,
    el("option", { value: "", text: "(auto: always for global pref/feedback)" }),
    el("option", { value: "always", text: "always" }),
    el("option", { value: "searchable", text: "searchable" }),
  );
  const summaryInput = el("input", { type: "text", placeholder: "Optional 1-line summary (derived if blank)" });
  const bodyInput = el("textarea", { rows: 7, placeholder: "Memory body…" });
  const lockCheck = el("input", { type: "checkbox" });
  const curateCheck = el("input", { type: "checkbox" });
  const saveBtn = el("button", { type: "submit", className: "btn btn-primary", text: "Create Memory" });

  const datalist = el("datalist", { id: "mem-scopes-list" }, ...scopes.map((s) => el("option", { value: s })));

  const form = el(
    "form",
    {
      className: "form-grid",
      onsubmit: async (ev) => {
        ev.preventDefault();
        if (!scopeInput.value.trim() || !keyInput.value.trim() || !bodyInput.value.trim()) {
          toast("Scope, key, and body are required", "warn");
          return;
        }
        saveBtn.disabled = true;
        try {
          const res = await api("POST", "/v1/memory", {
            scope: scopeInput.value.trim(),
            key: keyInput.value.trim(),
            kind: kindSel.value,
            tier: tierSel.value,
            summary: summaryInput.value.trim(),
            body: bodyInput.value,
            source: "human",
            locked: lockCheck.checked,
            reason: "web_create",
            enqueue_curate: curateCheck.checked,
          });
          toast(`${res.action} ${res.memory.scope}/${res.memory.key}`, "ok");
          closeModal();
          onDone();
        } catch (err) {
          toast(err.message, "error");
        } finally {
          saveBtn.disabled = false;
        }
      },
    },
    datalist,
    el(
      "div",
      { className: "form-row-2" },
      field("Scope", scopeInput),
      field("Key", keyInput),
    ),
    el(
      "div",
      { className: "form-row-2" },
      field("Kind", kindSel),
      field("Tier", tierSel),
    ),
    field("Summary", summaryInput),
    field("Body", bodyInput),
    el("label", { className: "checkbox-row" }, lockCheck, "Lock against automated edits"),
    el("label", { className: "checkbox-row" }, curateCheck, "Enqueue for LLM curator"),
    el(
      "div",
      { className: "actions-row flex-end" },
      el("button", { type: "button", className: "btn", text: "Cancel", onclick: closeModal }),
      saveBtn,
    ),
  );
  openModal("New Memory Entry", form);
}

async function openMemoryDetailModal(scope, key, onDone) {
  let detail;
  try {
    detail = await api("GET", `/v1/memory/item?scope=${encodeURIComponent(scope)}&key=${encodeURIComponent(key)}&history=true`);
  } catch (err) {
    toast(err.message, "error");
    return;
  }
  const it = detail.memory;
  const history = detail.history || [];

  const kindSel = el(
    "select",
    null,
    ...["preference", "feedback", "project", "reference"].map((k) => el("option", { value: k, text: k, selected: it.kind === k })),
  );
  const tierSel = el(
    "select",
    null,
    ...["always", "searchable"].map((t) => el("option", { value: t, text: t, selected: it.tier === t })),
  );
  const statusSel = el(
    "select",
    null,
    ...["active", "archived", "superseded"].map((s) => el("option", { value: s, text: s, selected: it.status === s })),
  );
  const summaryInput = el("input", { type: "text", value: it.summary || "" });
  const bodyInput = el("textarea", { rows: 8, value: it.body || "" });
  const reasonInput = el("input", { type: "text", placeholder: "web_edit" });
  const lockCheck = el("input", { type: "checkbox", checked: Boolean(it.locked) });
  const curateCheck = el("input", { type: "checkbox" });
  const saveBtn = el("button", { type: "submit", className: "btn btn-primary", text: "Save Changes" });

  const promoteBox = el("div", { className: "card hidden mt-2" });
  const toScopeIn = el("input", { type: "text", value: "global" });
  const toKeyIn = el("input", { type: "text", value: it.key });
  const toTierSel = el(
    "select",
    null,
    el("option", { value: "always", text: "always" }),
    el("option", { value: "searchable", text: "searchable" }),
  );
  const toLockCheck = el("input", { type: "checkbox" });
  promoteBox.appendChild(
    el(
      "div",
      { className: "form-grid" },
      el("h3", { className: "card-title m-0", text: "Promote / Copy Entry" }),
      el(
        "div",
        { className: "form-row-2" },
        field("To Scope", toScopeIn),
        field("To Key", toKeyIn),
      ),
      field("Destination Tier", toTierSel),
      el("label", { className: "checkbox-row" }, toLockCheck, "Lock destination"),
      el(
        "div",
        { className: "actions-row flex-end" },
        el("button", { type: "button", className: "btn btn-sm", text: "Hide", onclick: () => promoteBox.classList.add("hidden") }),
        el("button", {
          type: "button",
          className: "btn btn-sm btn-primary",
          text: "Promote",
          onclick: async () => {
            try {
              const res = await api("POST", "/v1/memory/promote", {
                from_scope: it.scope,
                from_key: it.key,
                to_scope: toScopeIn.value.trim(),
                to_key: toKeyIn.value.trim(),
                tier: toTierSel.value,
                summary: summaryInput.value.trim(),
                lock: toLockCheck.checked,
              });
              toast(`Promoted -> ${res.memory.scope}/${res.memory.key}`, "ok");
              promoteBox.classList.add("hidden");
              onDone();
            } catch (err) {
              toast(err.message, "error");
            }
          },
        }),
      ),
    ),
  );

  const form = el(
    "form",
    {
      className: "form-grid",
      onsubmit: async (ev) => {
        ev.preventDefault();
        saveBtn.disabled = true;
        try {
          const res = await api("POST", "/v1/memory", {
            scope: it.scope,
            key: it.key,
            kind: kindSel.value,
            tier: tierSel.value,
            status: statusSel.value,
            summary: summaryInput.value.trim(),
            body: bodyInput.value,
            source: "human",
            locked: lockCheck.checked,
            reason: reasonInput.value.trim() || "web_edit",
            enqueue_curate: curateCheck.checked,
          });
          toast(`${res.action} ${it.scope}/${it.key} (rev ${res.memory.revision})`, "ok");
          onDone();
          openMemoryDetailModal(it.scope, it.key, onDone);
        } catch (err) {
          toast(err.message, "error");
        } finally {
          saveBtn.disabled = false;
        }
      },
    },
    el(
      "div",
      { className: "actions-row flex-between" },
      el("div", { className: "actions-row" }, badge(`rev ${it.revision}`, "info"), badge(`source: ${it.source}`, "neutral"), it.device ? badge(it.device, "neutral") : null),
      el("span", { className: "small muted", text: `Updated ${relTime(it.updated_at)}` }),
    ),
    el(
      "div",
      { className: "form-row-2" },
      field("Kind", kindSel),
      field("Tier", tierSel),
    ),
    el(
      "div",
      { className: "form-row-2" },
      field("Status", statusSel),
      field("Edit Reason", reasonInput),
    ),
    field("Summary", summaryInput),
    field("Body", bodyInput),
    el(
      "div",
      { className: "actions-row" },
      el("label", { className: "checkbox-row" }, lockCheck, "Locked"),
      el("label", { className: "checkbox-row" }, curateCheck, "Enqueue curate"),
    ),
    el(
      "div",
      { className: "actions-row flex-between" },
      el(
        "div",
        { className: "actions-row" },
        el("button", { type: "button", className: "btn btn-sm", text: "Promote…", onclick: () => promoteBox.classList.toggle("hidden") }),
        it.status !== "archived"
          ? el("button", {
              type: "button",
              className: "btn btn-sm",
              text: "Archive (Opt-out)",
              onclick: async () => {
                const ok = await confirmDialog("Archive Memory?", `Archive ${it.scope}/${it.key}? Human archives are locked against curator re-application.`, "Archive", false);
                if (!ok) {
                  return;
                }
                try {
                  await api("DELETE", `/v1/memory/item?scope=${encodeURIComponent(it.scope)}&key=${encodeURIComponent(it.key)}`);
                  toast(`Archived ${it.scope}/${it.key}`, "ok");
                  closeModal();
                  onDone();
                } catch (err) {
                  toast(err.message, "error");
                }
              },
            })
          : null,
        el("button", {
          type: "button",
          className: "btn btn-sm btn-danger",
          text: "Hard Delete",
          onclick: async () => {
            const ok = await confirmDialog("Hard Delete Memory?", `Permanently delete ${it.scope}/${it.key}? Revision history is retained.`, "Delete Permanently", true);
            if (!ok) {
              return;
            }
            try {
              await api("DELETE", `/v1/memory/item?scope=${encodeURIComponent(it.scope)}&key=${encodeURIComponent(it.key)}&hard=true`);
              toast(`Deleted ${it.scope}/${it.key}`, "ok");
              closeModal();
              onDone();
            } catch (err) {
              toast(err.message, "error");
            }
          },
        }),
      ),
      saveBtn,
    ),
  );

  const histBox = el("div", { className: "card mt-3" }, el("h3", { className: "card-title mb-2", text: `Revision History (${history.length})` }));
  for (const r of history) {
    histBox.appendChild(
      el(
        "details",
        { className: "details-box mt-2" },
        el(
          "summary",
          null,
          `r${r.revision} · ${r.status} · ${r.source} · ${relTime(r.created_at)} · ${r.reason}`,
        ),
        el(
          "div",
          { className: "mt-2 form-grid" },
          el("pre", { className: "code-block", text: r.body }),
          el(
            "div",
            { className: "actions-row flex-end" },
            el("button", {
              type: "button",
              className: "btn btn-sm",
              text: `Revert to r${r.revision}`,
              onclick: async () => {
                const ok = await confirmDialog("Revert Memory?", `Restore ${it.scope}/${it.key} to revision r${r.revision}?`, "Revert", false);
                if (!ok) {
                  return;
                }
                try {
                  await api("POST", "/v1/memory/revert", { scope: it.scope, key: it.key, revision: r.revision });
                  toast(`Reverted ${it.scope}/${it.key} -> r${r.revision}`, "ok");
                  onDone();
                  openMemoryDetailModal(it.scope, it.key, onDone);
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

  const wrapper = el("div", { className: "form-grid" }, form, promoteBox, histBox);
  openModal(`${it.scope}/${it.key}`, wrapper);
}

async function renderEntriesView(root, scopes, ctx, signal) {
  const q = new URLSearchParams();
  if (memState.q.trim()) {
    q.set("q", memState.q.trim());
  }
  if (memState.scope) {
    q.set("scope", memState.scope);
  }
  if (memState.kind) {
    q.set("kind", memState.kind);
  }
  if (memState.tier) {
    q.set("tier", memState.tier);
  }
  if (memState.status === "all") {
    q.set("include_archived", "true");
  } else if (memState.status) {
    q.set("status", memState.status);
  }
  q.set("limit", String(memState.limit));

  let items = [];
  try {
    const res = await api("GET", `/v1/memory?${q.toString()}`, undefined, { signal });
    if (signal && signal.aborted) {
      return;
    }
    items = res.memories || [];
  } catch (err) {
    if ((err && err.name === "AbortError") || (signal && signal.aborted)) {
      return;
    }
    root.appendChild(el("div", { className: "alert-banner alert-danger", text: err.message }));
    return;
  }

  const scopeChips = [
    el("button", {
      type: "button",
      className: `chip${memState.scope === "" ? " active" : ""}`,
      text: "All scopes",
      onclick: () => {
        memState.scope = "";
        ctx.refresh();
      },
    }),
    ...scopes.map((sc) =>
      el("button", {
        type: "button",
        className: `chip${memState.scope === sc ? " active" : ""}`,
        text: sc,
        onclick: () => {
          memState.scope = sc;
          ctx.refresh();
        },
      }),
    ),
  ];

  const searchIn = el("input", {
    type: "search",
    className: "grow",
    "aria-label": "Search memories",
    placeholder: "FTS5 search memories…",
    value: memState.q,
    onkeydown: (ev) => {
      if (ev.key === "Enter") {
        memState.q = searchIn.value;
        ctx.refresh();
      }
    },
  });
  const kindSel = el(
    "select",
    {
      "aria-label": "Filter by kind",
      onchange: (ev) => {
        memState.kind = ev.target.value;
        ctx.refresh();
      },
    },
    el("option", { value: "", text: "All kinds", selected: memState.kind === "" }),
    ...["preference", "feedback", "project", "reference"].map((k) => el("option", { value: k, text: k, selected: memState.kind === k })),
  );
  const tierSel = el(
    "select",
    {
      "aria-label": "Filter by tier",
      onchange: (ev) => {
        memState.tier = ev.target.value;
        ctx.refresh();
      },
    },
    el("option", { value: "", text: "All tiers", selected: memState.tier === "" }),
    ...["always", "searchable"].map((t) => el("option", { value: t, text: t, selected: memState.tier === t })),
  );
  const statusSel = el(
    "select",
    {
      "aria-label": "Filter by status",
      onchange: (ev) => {
        memState.status = ev.target.value;
        ctx.refresh();
      },
    },
    el("option", { value: "active", text: "active", selected: memState.status === "active" }),
    el("option", { value: "all", text: "all statuses", selected: memState.status === "all" }),
    el("option", { value: "archived", text: "archived", selected: memState.status === "archived" }),
    el("option", { value: "superseded", text: "superseded", selected: memState.status === "superseded" }),
  );

  const filterBlock = el(
    "div",
    null,
    el("div", { className: "chips-row mb-2" }, scopeChips),
    el(
      "div",
      { className: "toolbar" },
      searchIn,
      el("button", {
        type: "button",
        className: "btn btn-sm btn-primary",
        text: "Search",
        onclick: () => {
          memState.q = searchIn.value;
          ctx.refresh();
        },
      }),
      kindSel,
      tierSel,
      statusSel,
    ),
  );

  let resultsNode;
  if (items.length === 0) {
    resultsNode = el("div", { className: "empty-state", text: "No memory entries match the current filter." });
  } else {
    const tbody = el("tbody");
    for (const it of items) {
      tbody.appendChild(
        el(
          "tr",
          {
            className: "row-clickable",
            onclick: () => openMemoryDetailModal(it.scope, it.key, () => ctx.refresh()),
          },
          cell("Scope / Key", { className: "cell-primary" }, el("div", { className: "mono small muted", text: it.scope }), el("strong", { className: "mono", text: it.key })),
          cell("Kind", null, badge(it.kind, "neutral")),
          cell("Tier", null, badge(it.tier, it.tier === "always" ? "info" : "neutral")),
          cell("Status", null, badge(it.status, it.status === "active" ? "ok" : "warn")),
          cell("Source", null, el("span", { className: "small", text: `${it.source} r${it.revision}` }), it.locked ? " " : null, it.locked ? badge("locked", "info") : null),
          cell("Updated", { text: relTime(it.updated_at) }),
          cell("Summary", { className: "small", text: it.summary }),
        ),
      );
    }
    resultsNode = el(
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
            el("th", { text: "Scope / Key" }),
            el("th", { text: "Kind" }),
            el("th", { text: "Tier" }),
            el("th", { text: "Status" }),
            el("th", { text: "Source" }),
            el("th", { text: "Updated" }),
            el("th", { text: "Summary" }),
          ),
        ),
        tbody,
      ),
    );
  }

  root.appendChild(section("Memory Entries", { className: "ui-section" }, filterBlock, resultsNode));
}

async function renderProposalsView(root, ctx, signal) {
  const qs = memState.propStatus ? `?status=${encodeURIComponent(memState.propStatus)}&limit=200` : "?limit=200";
  let proposals = [];
  try {
    const res = await api("GET", `/v1/memory/proposals${qs}`, undefined, { signal });
    if (signal && signal.aborted) {
      return;
    }
    proposals = res.proposals || [];
  } catch (err) {
    if ((err && err.name === "AbortError") || (signal && signal.aborted)) {
      return;
    }
    root.appendChild(el("div", { className: "alert-banner alert-danger", text: err.message }));
    return;
  }

  const pendingProps = proposals.filter((p) => p.status === "pending");
  const pendingIDs = new Set(pendingProps.map((p) => p.id));
  for (const id of memState.selectedProps) {
    if (!pendingIDs.has(id)) {
      memState.selectedProps.delete(id);
    }
  }

  const runBatch = async (body, label) => {
    try {
      const res = await api("POST", "/v1/memory/proposals/batch", body);
      memState.selectedProps.clear();
      toast(`${label}: ${res.count} proposal(s)`, "ok");
      ctx.refresh();
    } catch (err) {
      toast(err.message, "error");
    }
  };

  const statusSel = el(
    "select",
    {
      "aria-label": "Proposal status filter",
      onchange: (ev) => {
        memState.propStatus = ev.target.value;
        memState.selectedProps.clear();
        ctx.refresh();
      },
    },
    el("option", { value: "pending", text: "pending", selected: memState.propStatus === "pending" }),
    el("option", { value: "", text: "all", selected: memState.propStatus === "" }),
    el("option", { value: "approved", text: "approved", selected: memState.propStatus === "approved" }),
    el("option", { value: "rejected", text: "rejected", selected: memState.propStatus === "rejected" }),
  );

  const rowCheckboxes = new Map();
  const approveSelectedBtn = el("button", {
    type: "button",
    className: "btn btn-sm btn-primary",
    text: "Approve Selected",
    disabled: memState.selectedProps.size === 0,
    onclick: () => runBatch({ ids: [...memState.selectedProps], decision: "approve" }, "Approved"),
  });
  const rejectSelectedBtn = el("button", {
    type: "button",
    className: "btn btn-sm btn-danger",
    text: "Reject Selected",
    disabled: memState.selectedProps.size === 0,
    onclick: () => runBatch({ ids: [...memState.selectedProps], decision: "reject" }, "Rejected"),
  });
  const syncSelectionUI = () => {
    const hasSel = memState.selectedProps.size > 0;
    approveSelectedBtn.disabled = !hasSel;
    rejectSelectedBtn.disabled = !hasSel;
    for (const [id, cb] of rowCheckboxes) {
      cb.checked = memState.selectedProps.has(id);
    }
  };

  const batchBar = el(
    "div",
    { className: "toolbar" },
    statusSel,
    el("button", {
      type: "button",
      className: "btn btn-sm",
      text: "Select All Pending",
      disabled: pendingProps.length === 0,
      onclick: () => {
        for (const p of pendingProps) {
          memState.selectedProps.add(p.id);
        }
        syncSelectionUI();
      },
    }),
    approveSelectedBtn,
    rejectSelectedBtn,
    el("div", { className: "toolbar-spacer" }),
    el("button", {
      type: "button",
      className: "btn btn-sm",
      text: "Approve All Pending",
      disabled: pendingProps.length === 0,
      onclick: async () => {
        if (await confirmDialog("Approve All Pending?", "Approve all pending proposals?", "Approve All", false)) {
          await runBatch({ all_pending: true, decision: "approve" }, "Approved");
        }
      },
    }),
    el("button", {
      type: "button",
      className: "btn btn-sm btn-danger",
      text: "Reject All Pending",
      disabled: pendingProps.length === 0,
      onclick: async () => {
        if (await confirmDialog("Reject All Pending?", "Reject all pending proposals?", "Reject All", true)) {
          await runBatch({ all_pending: true, decision: "reject" }, "Rejected");
        }
      },
    }),
  );

  const listWrap = el("div");
  if (proposals.length === 0) {
    listWrap.appendChild(el("div", { className: "empty-state", text: "No proposals found." }));
  } else {
    for (const p of proposals) {
      const isPending = p.status === "pending";
      const cb = isPending
        ? el("input", {
            type: "checkbox",
            "aria-label": `Select proposal #${p.id}`,
            checked: memState.selectedProps.has(p.id),
            onchange: (ev) => {
              if (ev.target.checked) {
                memState.selectedProps.add(p.id);
              } else {
                memState.selectedProps.delete(p.id);
              }
              syncSelectionUI();
            },
          })
        : null;
      if (cb) {
        rowCheckboxes.set(p.id, cb);
      }

      const resolveOne = async (decision) => {
        try {
          await api("POST", "/v1/memory/proposals/resolve", { id: p.id, decision });
          memState.selectedProps.delete(p.id);
          toast(`Proposal #${p.id} ${decision}d`, "ok");
          ctx.refresh();
        } catch (err) {
          toast(err.message, "error");
        }
      };

      listWrap.appendChild(
        el(
          "article",
          { className: "card mb-2" },
          el(
            "div",
            { className: "actions-row flex-between mb-2" },
            el(
              "div",
              { className: "actions-row" },
              cb,
              el("strong", { className: "mono", text: `#${p.id} ${p.target_scope}/${p.target_key}` }),
              badge(p.action, "info"),
              badge(p.tier, "neutral"),
              badge(p.status, p.status === "approved" ? "ok" : p.status === "rejected" ? "danger" : "warn"),
            ),
            el("span", { className: "small muted", text: `${p.model || "curator"} · ${relTime(p.created_at)}` }),
          ),
          el("div", { className: "small mb-2", text: p.summary }),
          el("div", { className: "small muted mb-2", text: `Reason: ${p.reason}` }),
          el(
            "details",
            { className: "details-box mt-1" },
            el("summary", { text: "Proposed Body" }),
            el("pre", { className: "code-block mt-2", text: p.body }),
          ),
          isPending
            ? el(
                "div",
                { className: "actions-row flex-end mt-2" },
                el("button", { type: "button", className: "btn btn-sm btn-danger", text: "Reject", onclick: () => resolveOne("reject") }),
                el("button", { type: "button", className: "btn btn-sm btn-primary", text: "Approve", onclick: () => resolveOne("approve") }),
              )
            : null,
        ),
      );
    }
  }

  root.appendChild(section("Curator Proposals", { className: "ui-section" }, batchBar, listWrap));
}

async function renderContextView(root, scopes, signal) {
  const repoChoices = scopes.filter((s) => s.startsWith("repo:")).map((s) => s.slice(5));
  const repoInput = el("input", {
    type: "text",
    list: "ctx-repos",
    value: memState.ctxRepo,
    placeholder: "repo slug (e.g. fleet)",
  });
  const devInput = el("input", {
    type: "text",
    value: memState.ctxDevice || (state.caller ? state.caller.name : ""),
    placeholder: "device name",
  });
  const resultBox = el("div", { className: "mt-3" });

  const loadContext = async () => {
    memState.ctxRepo = repoInput.value.trim();
    memState.ctxDevice = devInput.value.trim();
    const q = new URLSearchParams();
    if (memState.ctxRepo) {
      q.set("repo", memState.ctxRepo);
    }
    if (memState.ctxDevice) {
      q.set("device", memState.ctxDevice);
    }
    try {
      const res = await api("GET", `/v1/memory/context?${q.toString()}`, undefined, { signal });
      if (signal && signal.aborted) {
        return;
      }
      clear(resultBox);
      resultBox.appendChild(
        el(
          "article",
          { className: "card" },
          el(
            "div",
            { className: "actions-row flex-between mb-2" },
            el(
              "div",
              { className: "actions-row" },
              badge(`always: ${res.always_count}`, "info"),
              badge(`searchable: ${res.searchable_count}`, "neutral"),
              badge(`${res.rendered.length} / 3500 B`, res.rendered.length > 3300 ? "warn" : "ok"),
            ),
            makeCopyBtn(res.rendered, "Copy Context"),
          ),
          el("div", { className: "small muted mb-2 mono", text: `scopes: ${(res.scopes || []).join(", ")}` }),
          el("pre", { className: "code-block", text: res.rendered }),
        ),
      );
    } catch (err) {
      if ((err && err.name === "AbortError") || (signal && signal.aborted)) {
        return;
      }
      toast(err.message, "error");
    }
  };

  root.appendChild(
    section(
      "SessionStart Context Preview",
      { className: "ui-section" },
      el(
        "div",
        { className: "card form-grid" },
        el("datalist", { id: "ctx-repos" }, ...repoChoices.map((r) => el("option", { value: r }))),
        el(
          "div",
          { className: "form-row-2" },
          field("Repo", repoInput),
          field("Device", devInput),
        ),
        el(
          "div",
          { className: "actions-row flex-end" },
          el("button", { type: "button", className: "btn btn-primary", text: "Preview SessionStart Context", onclick: loadContext }),
        ),
      ),
      resultBox,
    ),
  );
  await loadContext();
}

async function renderHistoryView(root, ctx, signal) {
  let revs = [];
  try {
    const res = await api("GET", "/v1/memory/history?limit=100", undefined, { signal });
    if (signal && signal.aborted) {
      return;
    }
    revs = res.revisions || [];
  } catch (err) {
    if ((err && err.name === "AbortError") || (signal && signal.aborted)) {
      return;
    }
    root.appendChild(el("div", { className: "alert-banner alert-danger", text: err.message }));
    return;
  }
  if (revs.length === 0) {
    root.appendChild(section("Audit History", { className: "ui-section" }, el("div", { className: "empty-state", text: "No memory revisions recorded yet." })));
    return;
  }

  const list = el("div", { className: "form-grid" });
  for (const r of revs) {
    list.appendChild(
      el(
        "details",
        { className: "details-box m-0" },
        el(
          "summary",
          { className: "history-summary" },
          el("span", { className: "mono", text: `${r.scope}/${r.key} r${r.revision}` }),
          " ",
          badge(r.status, r.status === "active" ? "ok" : "warn"),
          " ",
          el("span", { className: "small muted", text: `${r.source} · ${r.device || "-"} · ${relTime(r.created_at)} · ${r.reason}` }),
        ),
        el(
          "div",
          { className: "mt-2 form-grid" },
          el("div", { className: "small muted", text: r.summary }),
          el("pre", { className: "code-block", text: r.body }),
          el(
            "div",
            { className: "actions-row flex-end" },
            el("button", {
              type: "button",
              className: "btn btn-sm",
              text: "Inspect Current",
              onclick: () => openMemoryDetailModal(r.scope, r.key, () => ctx.refresh()),
            }),
            el("button", {
              type: "button",
              className: "btn btn-sm btn-primary",
              text: `Revert to r${r.revision}`,
              onclick: async () => {
                if (await confirmDialog("Revert Memory?", `Restore ${r.scope}/${r.key} to r${r.revision}?`, "Revert", false)) {
                  try {
                    await api("POST", "/v1/memory/revert", { scope: r.scope, key: r.key, revision: r.revision });
                    toast(`Reverted ${r.scope}/${r.key} -> r${r.revision}`, "ok");
                    ctx.refresh();
                  } catch (err) {
                    toast(err.message, "error");
                  }
                }
              },
            }),
          ),
        ),
      ),
    );
  }
  root.appendChild(section("Audit History", { className: "ui-section" }, list));
}

async function renderCuratorView(root, ctx, signal) {
  let st;
  try {
    st = await api("GET", "/v1/memory/curator", undefined, { signal });
    if (signal && signal.aborted) {
      return;
    }
  } catch (err) {
    if ((err && err.name === "AbortError") || (signal && signal.aborted)) {
      return;
    }
    root.appendChild(el("div", { className: "alert-banner alert-danger", text: err.message }));
    return;
  }

  const stateLabel = st.enabled ? "Enabled" : st.configured ? "Disabled" : "Not configured";
  const statusCard = el(
    "article",
    { className: "card" },
    el(
      "div",
      { className: "card-header" },
      el("h3", { className: "card-title", text: "LLM Memory Curator" }),
      badge(stateLabel, st.enabled ? "ok" : "neutral"),
    ),
    el(
      "dl",
      { className: "kv-list" },
      el("dt", { className: "kv-label", text: "Provider / URL" }),
      el("dd", { className: "kv-value mono", text: st.configured ? `${st.provider} (${st.base_url})` : "-" }),
      el("dt", { className: "kv-label", text: "Model" }),
      el("dd", { className: "kv-value mono", text: st.model || "-" }),
      el("dt", { className: "kv-label", text: "API Key" }),
      el("dd", { className: "kv-value mono", text: st.api_key_hint || "-" }),
      el("dt", { className: "kv-label", text: "Global Mode" }),
      el("dd", { className: "kv-value", text: st.review_global ? "Review queue (opt-in)" : "Auto-apply (opt-out)" }),
      el("dt", { className: "kv-label", text: "Queue" }),
      el("dd", { className: "kv-value", text: `${st.pending_jobs} pending · ${st.failed_jobs} failed · ${st.pending_proposals} proposals · ${st.processed_count} processed` }),
      el("dt", { className: "kv-label", text: "Last Run" }),
      el("dd", { className: "kv-value", text: st.last_run_at ? relTime(st.last_run_at) : "never" }),
      st.last_error ? el("dt", { className: "kv-label", text: "Last Error" }) : null,
      st.last_error ? el("dd", { className: "kv-value", text: st.last_error }) : null,
    ),
  );
  root.appendChild(section("Curator Status", { className: "ui-section" }, statusCard));

  if (!state.isAdmin) {
    root.appendChild(el("div", { className: "muted small", text: "Only admin devices can configure or manually trigger the curator." }));
    return;
  }

  const limitIn = el("input", { type: "number", value: "20", min: "1", max: "200", className: "max-w-sm" });
  const dryRunCheck = el("input", { type: "checkbox" });
  const includeAllCheck = el("input", { type: "checkbox" });
  const runBtn = el("button", { type: "button", className: "btn btn-primary", text: "Run Curator Now" });
  const reportsBox = el("div", { className: "mt-2" });

  const renderReports = (reports) => {
    clear(reportsBox);
    if (!reports || reports.length === 0) {
      reportsBox.appendChild(el("div", { className: "muted small", text: "No pending curation jobs processed." }));
      return;
    }
    const tbody = el("tbody");
    for (const r of reports) {
      tbody.appendChild(
        el(
          "tr",
          null,
          cell("Memory", { className: "mono small cell-primary", text: `${r.memory_scope}/${r.memory_key}` }),
          cell("Action", null, badge(r.action, r.error ? "danger" : r.applied ? "ok" : "info")),
          cell("Target", { className: "mono small", text: r.target_key ? `${r.target_scope || r.memory_scope}/${r.target_key}` : "-" }),
          cell("Applied", { text: r.applied ? "yes" : r.queued_proposal_id ? `#${r.queued_proposal_id}` : "no" }),
          cell("Reason / Error", { className: "small", text: r.error || r.reason || "-" }),
        ),
      );
    }
    reportsBox.appendChild(
      el(
        "div",
        { className: "table-wrap" },
        el(
          "table",
          { className: "responsive-table" },
          el("thead", null, el("tr", null, el("th", { text: "Memory" }), el("th", { text: "Action" }), el("th", { text: "Target" }), el("th", { text: "Applied" }), el("th", { text: "Reason / Error" }))),
          tbody,
        ),
      ),
    );
  };

  if (memState.curatorReports) {
    renderReports(memState.curatorReports);
  }

  runBtn.addEventListener("click", async () => {
    runBtn.disabled = true;
    try {
      const res = await api("POST", "/v1/memory/curator/run", {
        limit: Number.parseInt(limitIn.value, 10) || 20,
        dry_run: dryRunCheck.checked,
        include_all: includeAllCheck.checked,
      });
      memState.curatorReports = res.reports || [];
      toast(`Curator ran on ${memState.curatorReports.length} item(s)`, "ok");
      ctx.refresh();
    } catch (err) {
      toast(err.message, "error");
    } finally {
      runBtn.disabled = false;
    }
  });

  const runCard = el(
    "div",
    { className: "card" },
    el(
      "div",
      { className: "toolbar m-0" },
      field("Batch Limit", limitIn),
      el("label", { className: "checkbox-row" }, dryRunCheck, "Dry run"),
      el("label", { className: "checkbox-row" }, includeAllCheck, "Include all active"),
      runBtn,
    ),
    reportsBox,
  );
  root.appendChild(section("Manual Run", { className: "ui-section" }, runCard));

  const provSel = el(
    "select",
    null,
    ...["openrouter", "openai", "ollama", "custom"].map((p) => el("option", { value: p, text: p, selected: (st.provider || "openrouter") === p })),
  );
  const modelIn = el("input", { type: "text", value: st.model || "" });
  const urlIn = el("input", { type: "text", value: st.base_url || "" });
  const keyIn = el("input", {
    type: "password",
    autocomplete: "off",
    placeholder: st.api_key_hint ? `Leave blank to keep ${st.api_key_hint}` : "API key (optional for Ollama)",
  });
  const reviewGlobalCheck = el("input", { type: "checkbox", checked: Boolean(st.review_global) });
  const dataCollSel = el(
    "select",
    null,
    el("option", { value: "deny", text: "deny", selected: (st.data_collection || "deny") === "deny" }),
    el("option", { value: "allow", text: "allow", selected: st.data_collection === "allow" }),
  );
  const zdrCheck = el("input", { type: "checkbox", checked: Boolean(st.zdr) });
  const keepAliveIn = el("input", { type: "text", value: st.keep_alive || "", placeholder: "e.g. 2m" });
  const numCtxIn = el("input", { type: "number", value: st.num_ctx ? String(st.num_ctx) : "", placeholder: "e.g. 2048" });
  const testCheck = el("input", { type: "checkbox", checked: true });
  const saveCfgBtn = el("button", { type: "submit", className: "btn btn-primary", text: "Save & Enable Curator" });

  const cfgForm = el(
    "form",
    {
      className: "card form-grid",
      onsubmit: async (ev) => {
        ev.preventDefault();
        state.editing = true;
        const apiKey = keyIn.value;
        keyIn.value = "";
        saveCfgBtn.disabled = true;
        try {
          await api("POST", "/v1/memory/curator", {
            enabled: true,
            provider: provSel.value,
            model: modelIn.value.trim(),
            base_url: urlIn.value.trim(),
            api_key: apiKey,
            review_global: reviewGlobalCheck.checked,
            data_collection: dataCollSel.value,
            zdr: zdrCheck.checked,
            keep_alive: keepAliveIn.value.trim(),
            num_ctx: Number.parseInt(numCtxIn.value, 10) || 0,
            test: testCheck.checked,
          });
          toast("Curator saved and enabled", "ok");
          state.editing = false;
          ctx.refresh();
        } catch (err) {
          toast(err.message, "error");
        } finally {
          saveCfgBtn.disabled = false;
          state.editing = false;
        }
      },
    },
    el(
      "div",
      { className: "form-row-2" },
      field("Provider", provSel),
      field("Model", modelIn),
    ),
    el(
      "div",
      { className: "form-row-2" },
      field("Base URL", urlIn),
      field("API Key", keyIn),
    ),
    el(
      "div",
      { className: "form-row-2" },
      field("OpenRouter Data Collection", dataCollSel),
      field("Ollama Keep Alive", keepAliveIn),
    ),
    field("Ollama Num Ctx", numCtxIn),
    el(
      "div",
      { className: "actions-row" },
      el("label", { className: "checkbox-row" }, reviewGlobalCheck, "Stage global promotions for manual review"),
      el("label", { className: "checkbox-row" }, zdrCheck, "OpenRouter ZDR"),
      el("label", { className: "checkbox-row" }, testCheck, "Test connection before saving"),
    ),
    el(
      "div",
      { className: "actions-row flex-between" },
      el("button", {
        type: "button",
        className: "btn btn-danger",
        disabled: !st.enabled,
        text: "Disable Curator",
        onclick: async () => {
          try {
            await api("POST", "/v1/memory/curator", { disable: true });
            toast("Curator disabled", "ok");
            ctx.refresh();
          } catch (err) {
            toast(err.message, "error");
          }
        },
      }),
      saveCfgBtn,
    ),
  );
  root.appendChild(section("Curator Configuration", { className: "ui-section" }, cfgForm));
}

export async function renderMemory(root, ctx, subRoute = "") {
  const signal = ctx && ctx.signal;
  if (["entries", "proposals", "context", "history", "curator"].includes(subRoute)) {
    memState.tab = subRoute;
  }

  let scopes = ["global"];
  try {
    const [res, cat] = await Promise.all([
      api("GET", "/v1/memory/scopes", undefined, { signal }),
      fetchPathsCatalog("", { signal }),
    ]);
    const set = new Set([...(res.scopes || []), ...(cat.repos || []).map((r) => `repo:${r}`)]);
    if (set.size > 0) {
      scopes = Array.from(set).sort();
    }
  } catch (err) {
    if ((err && err.name === "AbortError") || (signal && signal.aborted)) {
      return;
    }
    // fallback to global
  }
  if (signal && signal.aborted) {
    return;
  }

  const ovMem = state.overview && state.overview.memory;
  const pendingCount = ovMem ? ovMem.pending_proposals : 0;

  clear(root);
  root.appendChild(
    el(
      "header",
      { className: "page-header" },
      el(
        "div",
        null,
        el("h1", { className: "page-title", text: "Shared Agent Memory" }),
        el("p", {
          className: "page-subtitle",
          text: ovMem
            ? `${ovMem.active} active (${ovMem.always_count} always, ${ovMem.searchable_count} searchable) · ${ovMem.archived} archived · ${ovMem.total_revisions} revisions`
            : "Cross-harness memory store & curator",
        }),
      ),
      el("button", {
        type: "button",
        className: "btn btn-primary",
        text: "+ New Memory",
        onclick: () => openNewMemoryModal(scopes, () => ctx.refresh()),
      }),
    ),
  );

  const tabs = [
    ["entries", "Entries", "#memory"],
    ["proposals", `Proposals (${pendingCount})`, "#memory/proposals"],
    ["context", "Context Preview", "#memory/context"],
    ["history", "Audit History", "#memory/history"],
    ["curator", "Curator", "#memory/curator"],
  ].map(([id, label, hash]) =>
    el("button", {
      type: "button",
      role: "tab",
      "aria-selected": memState.tab === id ? "true" : "false",
      className: `sub-tab${memState.tab === id ? " active" : ""}`,
      text: label,
      onclick: () => {
        memState.tab = id;
        ctx.navigate(hash);
      },
    }),
  );
  root.appendChild(el("div", { className: "sub-tabs", role: "tablist", "aria-label": "Memory views" }, tabs));

  switch (memState.tab) {
    case "proposals":
      await renderProposalsView(root, ctx, signal);
      break;
    case "context":
      await renderContextView(root, scopes, signal);
      break;
    case "history":
      await renderHistoryView(root, ctx, signal);
      break;
    case "curator":
      await renderCuratorView(root, ctx, signal);
      break;
    default:
      await renderEntriesView(root, scopes, ctx, signal);
      break;
  }
}
