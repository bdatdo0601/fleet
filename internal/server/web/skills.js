import { api, badge, clear, closeModal, confirmDialog, el, field, openModal, section, toast } from "./lib.js";

const DEFAULT_SKILL_TEMPLATE = `---
name: my-skill
description: Short 1-line summary for progressive skill discovery
tags: [assistant]
---

# Instructions

Describe step-by-step how the assistant should execute this workflow using Fleet and Vault tools.
`;

function openSkillEditor(existing, onSaved) {
  const isNew = !existing;
  const nameIn = el("input", {
    type: "text",
    value: existing ? existing.name : "",
    disabled: !isNew,
    placeholder: "e.g. morning-briefing",
  });
  const bodyIn = el("textarea", {
    rows: "16",
    className: "mono",
    value: existing ? existing.content || "" : DEFAULT_SKILL_TEMPLATE,
  });

  const saveBtn = el("button", {
    type: "button",
    className: "btn btn-primary",
    text: "Save Skill",
    onclick: async () => {
      const slug = nameIn.value.trim();
      if (!slug) {
        toast("Skill name is required", "warn");
        return;
      }
      try {
        await api("PUT", `/v1/assistant/skills/${encodeURIComponent(slug)}`, { content: bodyIn.value });
        toast(`Saved skill ${slug}`, "ok");
        closeModal();
        if (onSaved) {
          await onSaved();
        }
      } catch (err) {
        toast(err.message, "error");
      }
    },
  });

  openModal(
    isNew ? "New SKILL.md" : `Edit ${existing.name}`,
    el("div", { className: "stack" }, field("Skill Slug", nameIn), field("SKILL.md Content", bodyIn), el("div", { className: "actions-row" }, saveBtn)),
  );
}

function openSkillRegistryModal(onInstalled) {
  const sourceIn = el("input", {
    type: "text",
    placeholder: "e.g. anthropics/skills@pdf or https://github.com/owner/repo",
  });
  const overwriteIn = el("input", { type: "checkbox" });
  const installStatus = el("div", { className: "small muted" });

  const doInstall = async (source, skill = "") => {
    const src = String(source || "").trim();
    if (!src) {
      toast("Skill source is required", "warn");
      return;
    }
    installStatus.textContent = `Installing ${src}…`;
    try {
      const res = await api("POST", "/v1/assistant/skills/install", {
        source: src,
        skill,
        overwrite: overwriteIn.checked,
      });
      const sk = res.skill || {};
      const extra = res.has_scripts ? " (includes scripts — sandbox only)" : "";
      toast(`Installed skill ${sk.name || src}${extra}`, "ok");
      closeModal();
      if (onInstalled) {
        await onInstalled();
      }
    } catch (err) {
      installStatus.textContent = err.message;
      toast(err.message, "error");
    }
  };

  const queryIn = el("input", { type: "search", placeholder: "Search skills.sh (e.g. pdf, code review, postgres)…" });
  const resultsBox = el("div", { className: "stack" });

  const doSearch = async () => {
    const q = queryIn.value.trim();
    if (!q) {
      toast("Enter a search query", "warn");
      return;
    }
    clear(resultsBox);
    resultsBox.appendChild(el("div", { className: "small muted", text: "Searching skills.sh…" }));
    try {
      const res = await api("GET", `/v1/assistant/skills/search?q=${encodeURIComponent(q)}&limit=10`);
      const items = res.results || [];
      clear(resultsBox);
      if (items.length === 0) {
        resultsBox.appendChild(el("div", { className: "small muted", text: "No skills matched that query." }));
        return;
      }
      for (const item of items) {
        const spec = item.install_spec || item.source || "";
        resultsBox.appendChild(
          el(
            "article",
            { className: "card" },
            el(
              "div",
              { className: "card-header" },
              el("div", { className: "actions-row" }, el("strong", { text: item.name || item.skill_id }), badge(item.source || "registry", "neutral"), item.installs ? badge(`${item.installs} installs`, "info") : null),
              item.url ? el("a", { href: item.url, target: "_blank", rel: "noopener noreferrer", className: "small", text: "skills.sh" }) : null,
            ),
            el(
              "div",
              { className: "actions-row" },
              el("span", { className: "mono small muted", text: spec }),
              el("button", {
                type: "button",
                className: "btn btn-sm btn-primary",
                text: "Install",
                onclick: () => doInstall(spec),
              }),
            ),
          ),
        );
      }
    } catch (err) {
      clear(resultsBox);
      resultsBox.appendChild(el("div", { className: "alert-banner alert-danger", text: err.message }));
    }
  };

  queryIn.addEventListener("keydown", (ev) => {
    if (ev.key === "Enter") {
      doSearch();
    }
  });

  openModal(
    "Install Skill from Registry",
    el(
      "div",
      { className: "stack" },
      field("Direct Source (GitHub owner/repo@skill, skills.sh URL, or HTTPS SKILL.md)", sourceIn),
      el(
        "div",
        { className: "actions-row" },
        el("label", { className: "actions-row small" }, overwriteIn, "Overwrite if exists"),
        el("button", {
          type: "button",
          className: "btn btn-primary btn-sm",
          text: "Install",
          onclick: () => doInstall(sourceIn.value),
        }),
      ),
      installStatus,
      el("hr"),
      el(
        "div",
        { className: "actions-row" },
        queryIn,
        el("button", { type: "button", className: "btn btn-sm", text: "Search skills.sh", onclick: doSearch }),
      ),
      resultsBox,
    ),
  );
}

async function renderVaultFileEditor(container, relPath, title, hint, extraActions = null, signal = null) {
  let content = "";
  try {
    const res = await api("GET", `/v1/assistant/vault/file?path=${encodeURIComponent(relPath)}`, undefined, { signal });
    if (signal && signal.aborted) {
      return;
    }
    content = res.content || "";
  } catch (err) {
    if ((err && err.name === "AbortError") || (signal && signal.aborted)) {
      return;
    }
    container.appendChild(el("div", { className: "alert-banner alert-danger", text: err.message }));
    return;
  }
  const ta = el("textarea", { rows: "16", className: "mono", value: content });
  container.appendChild(
    el(
      "article",
      { className: "card" },
      el("div", { className: "card-header" }, el("h3", { className: "card-title", text: title }), el("span", { className: "mono small muted", text: relPath })),
      hint ? el("p", { className: "small muted mb-2", text: hint }) : null,
      ta,
      el(
        "div",
        { className: "actions-row mt-2" },
        el("button", {
          type: "button",
          className: "btn btn-primary btn-sm",
          text: "Save File",
          onclick: async () => {
            try {
              await api("PUT", "/v1/assistant/vault/file", { path: relPath, content: ta.value });
              toast(`Saved ${relPath}`, "ok");
            } catch (err) {
              toast(err.message, "error");
            }
          },
        }),
        extraActions,
      ),
    ),
  );
}

export async function renderSkills(main, ctx, sub) {
  const signal = ctx && ctx.signal;
  if (signal && signal.aborted) {
    return;
  }
  clear(main);
  const tab = (sub || "").trim() || "library";
  const tabDefs = [
    ["library", "Skills Library", "#skills/library"],
    ["agent", "SOUL & Heartbeat", "#skills/agent"],
    ["memory", "Observational Memory", "#skills/memory"],
    ["vault", "Vault Search", "#skills/vault"],
    ["history", "History", "#skills/history"],
  ];
  const tabs = el(
    "div",
    { className: "sub-tabs", role: "tablist", "aria-label": "Skills views" },
    ...tabDefs.map(([id, label, hash]) =>
      el("a", {
        href: hash,
        role: "tab",
        "aria-selected": tab === id ? "true" : "false",
        className: `sub-tab${tab === id ? " active" : ""}`,
        text: label,
      }),
    ),
  );

  const actions = el(
    "div",
    { className: "actions-row" },
    el("button", {
      type: "button",
      className: "btn btn-sm",
      text: "Install from Registry",
      onclick: () => openSkillRegistryModal(() => ctx.refresh()),
    }),
    el("button", {
      type: "button",
      className: "btn btn-sm btn-primary",
      text: "New Skill",
      onclick: () => openSkillEditor(null, () => ctx.refresh()),
    }),
  );

  const body = el("div", { className: "stack" });
  main.appendChild(section("Skills & Second Brain", { subtitle: "Obsidian-backed SKILL.md library, agent directives, and Observational Memory", actions }, tabs, body));

  if (tab === "agent") {
    await renderVaultFileEditor(body, "_agent/SOUL.md", "SOUL.md", "Static system directives cached at the top of every run.", null, signal);
    if (signal && signal.aborted) {
      return;
    }
    await renderVaultFileEditor(body, "_agent/HEARTBEAT.md", "HEARTBEAT.md", "Periodic autonomous checklist evaluated on the heartbeat timer.", null, signal);
    return;
  }

  if (tab === "memory") {
    const observeActions = el(
      "div",
      { className: "actions-row" },
      el("button", {
        type: "button",
        className: "btn btn-sm",
        text: "Run Observer Now",
        onclick: async () => {
          try {
            const res = await api("POST", "/v1/assistant/memory/observe", { reflect: false });
            toast(`Observed ${res.observed || 0} messages`, "ok");
            ctx.refresh();
          } catch (err) {
            toast(err.message, "error");
          }
        },
      }),
      el("button", {
        type: "button",
        className: "btn btn-sm",
        text: "Run Reflector Now",
        onclick: async () => {
          try {
            await api("POST", "/v1/assistant/memory/observe", { reflect: true });
            toast("Reflector compaction complete", "ok");
            ctx.refresh();
          } catch (err) {
            toast(err.message, "error");
          }
        },
      }),
    );
    await renderVaultFileEditor(body, "_agent/memory/REFLECTIONS.md", "REFLECTIONS.md", "Condensed long-term context kept in the cached prompt prefix.", observeActions, signal);
    if (signal && signal.aborted) {
      return;
    }
    await renderVaultFileEditor(body, "_agent/memory/OBSERVATIONS.md", "OBSERVATIONS.md", "Append-only dated observations distilled by the background Observer.", null, signal);
    return;
  }

  if (tab === "vault") {
    let extraVaults = [];
    try {
      const cfgRes = await api("GET", "/v1/assistant/config", undefined, { signal });
      extraVaults = (cfgRes.config && cfgRes.config.extra_vaults) || [];
    } catch (err) {
      if ((err && err.name === "AbortError") || (signal && signal.aborted)) {
        return;
      }
      // fall back to primary vault if config lookup fails
    }
    if (signal && signal.aborted) {
      return;
    }
    const vaultSel = el(
      "select",
      { "aria-label": "Target vault" },
      el("option", { value: "all", text: "All vaults" }),
      el("option", { value: "agent", text: "agent (primary)" }),
      ...extraVaults.map((m) => el("option", { value: m.name, text: `${m.name}${m.read_only ? " (read-only)" : ""}` })),
    );
    const qIn = el("input", { type: "search", placeholder: "Search Obsidian Markdown notes…" });
    const hitsBox = el("div", { className: "stack" });
    const runSearch = async () => {
      try {
        const res = await api(
          "GET",
          `/v1/assistant/vault/search?vault=${encodeURIComponent(vaultSel.value)}&q=${encodeURIComponent(qIn.value.trim())}&limit=25`,
          undefined,
          { signal },
        );
        if (signal && signal.aborted) {
          return;
        }
        clear(hitsBox);
        const hits = res.hits || [];
        if (hits.length === 0) {
          hitsBox.appendChild(el("div", { className: "small muted", text: "No matching notes found." }));
          return;
        }
        for (const h of hits) {
          const hitVault = h.vault || "agent";
          hitsBox.appendChild(
            el(
              "article",
              { className: "card" },
              el(
                "div",
                { className: "card-header" },
                el("div", { className: "actions-row" }, badge(hitVault, hitVault === "agent" ? "neutral" : "info"), el("strong", { text: h.title })),
                el("span", { className: "mono small muted", text: h.path }),
              ),
              el("div", { className: "small muted mb-2", text: h.snippet }),
              el("button", {
                type: "button",
                className: "btn btn-sm",
                text: "Open Note",
                onclick: async () => {
                  try {
                    const file = await api(
                      "GET",
                      `/v1/assistant/vault/file?vault=${encodeURIComponent(hitVault)}&path=${encodeURIComponent(h.path)}`,
                    );
                    openModal(`${hitVault}:${h.path}`, el("pre", { className: "code-block" }, el("code", { text: file.content || "" })));
                  } catch (err) {
                    toast(err.message, "error");
                  }
                },
              }),
            ),
          );
        }
      } catch (err) {
        if ((err && err.name === "AbortError") || (signal && signal.aborted)) {
          return;
        }
        toast(err.message, "error");
      }
    };
    vaultSel.addEventListener("change", runSearch);
    body.appendChild(
      el(
        "div",
        { className: "actions-row" },
        vaultSel,
        qIn,
        el("button", { type: "button", className: "btn btn-sm btn-primary", text: "Search", onclick: runSearch }),
      ),
    );
    qIn.addEventListener("keydown", (ev) => {
      if (ev.key === "Enter") {
        runSearch();
      }
    });
    body.appendChild(hitsBox);
    await runSearch();
    return;
  }

  if (tab === "history") {
    let commits = [];
    try {
      const res = await api("GET", "/v1/assistant/vault/history?vault=agent&limit=25", undefined, { signal });
      if (signal && signal.aborted) {
        return;
      }
      commits = res.commits || [];
    } catch (err) {
      if ((err && err.name === "AbortError") || (signal && signal.aborted)) {
        return;
      }
      body.appendChild(el("div", { className: "alert-banner alert-danger", text: err.message }));
      return;
    }
    if (commits.length === 0) {
      body.appendChild(el("div", { className: "card" }, el("div", { className: "small muted", text: "No git commit history found in the primary agent vault." })));
      return;
    }
    for (const c of commits) {
      body.appendChild(
        el(
          "article",
          { className: "card" },
          el(
            "div",
            { className: "card-header" },
            el("div", { className: "actions-row" }, badge(c.short_sha, "info"), el("strong", { text: c.message })),
            el("span", { className: "mono small muted", text: c.date }),
          ),
          el(
            "div",
            { className: "actions-row" },
            el("button", {
              type: "button",
              className: "btn btn-sm",
              text: "View Diff",
              onclick: async () => {
                try {
                  const res = await api("GET", `/v1/assistant/vault/diff?vault=agent&sha=${encodeURIComponent(c.sha)}`);
                  openModal(`Commit ${c.short_sha}`, el("pre", { className: "code-block" }, el("code", { text: res.diff || "(empty diff)" })));
                } catch (err) {
                  toast(err.message, "error");
                }
              },
            }),
            el("button", {
              type: "button",
              className: "btn btn-sm btn-danger",
              text: "Rollback",
              onclick: async () => {
                const ok = await confirmDialog("Rollback Vault", `Restore agent vault files to commit ${c.short_sha} (${c.message})?`, "Rollback", true);
                if (!ok) {
                  return;
                }
                try {
                  await api("POST", "/v1/assistant/vault/rollback", { vault: "agent", sha: c.sha });
                  toast(`Rolled back vault to ${c.short_sha}`, "ok");
                  ctx.refresh();
                } catch (err) {
                  toast(err.message, "error");
                }
              },
            }),
          ),
        ),
      );
    }
    return;
  }

  let skills = [];
  try {
    const res = await api("GET", "/v1/assistant/skills", undefined, { signal });
    if (signal && signal.aborted) {
      return;
    }
    skills = res.skills || [];
  } catch (err) {
    if ((err && err.name === "AbortError") || (signal && signal.aborted)) {
      return;
    }
    body.appendChild(el("div", { className: "alert-banner alert-danger", text: err.message }));
    return;
  }
  if (skills.length === 0) {
    body.appendChild(
      el("div", { className: "card" }, el("div", { className: "small muted", text: "No skills yet in _skills/. Create one or ask the assistant in #chat to draft a SKILL.md." })),
    );
    return;
  }
  const grid = el("div", { className: "grid-2" });
  for (const sk of skills) {
    grid.appendChild(
      el(
        "article",
        { className: "card" },
        el(
          "div",
          { className: "card-header" },
          el("h3", { className: "card-title", text: sk.name }),
          el(
            "div",
            { className: "actions-row" },
            sk.sync_to_fleet ? badge("sync_to_fleet", "ok") : null,
            sk.source && sk.source.source ? badge(sk.source.source, "info") : null,
            sk.files && sk.files.length > 0 ? badge(`+${sk.files.length} files`, "neutral") : null,
            sk.model ? badge(sk.model, "info") : null,
            ...(sk.tags || []).map((t) => badge(t, "neutral")),
          ),
        ),
        el("div", { className: "small muted mb-2", text: sk.description || "(no description)" }),
        el(
          "div",
          { className: "actions-row" },
          el("button", {
            type: "button",
            className: "btn btn-sm",
            text: "Edit",
            onclick: async () => {
              try {
                const detail = await api("GET", `/v1/assistant/skills/${encodeURIComponent(sk.name)}`);
                openSkillEditor(detail.skill, () => ctx.refresh());
              } catch (err) {
                toast(err.message, "error");
              }
            },
          }),
          el("button", {
            type: "button",
            className: "btn btn-sm btn-danger",
            text: "Delete",
            onclick: async () => {
              const ok = await confirmDialog("Delete Skill?", `Delete skill "${sk.name}" from _skills/?`, "Delete", true);
              if (!ok) {
                return;
              }
              try {
                await api("DELETE", `/v1/assistant/skills/${encodeURIComponent(sk.name)}`);
                toast(`Deleted ${sk.name}`, "ok");
                ctx.refresh();
              } catch (err) {
                toast(err.message, "error");
              }
            },
          }),
        ),
      ),
    );
  }
  body.appendChild(grid);
}
