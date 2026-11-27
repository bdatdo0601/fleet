import { api, badge, clear, closeModal, el, field, openModal, section, toast } from "./lib.js";

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

async function renderVaultFileEditor(container, relPath, title, hint, extraActions = null) {
  let content = "";
  try {
    const res = await api("GET", `/v1/assistant/vault/file?path=${encodeURIComponent(relPath)}`);
    content = res.content || "";
  } catch (err) {
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
  clear(main);
  const tab = (sub || "").trim() || "library";
  const tabs = el(
    "div",
    { className: "subtabs" },
    el("a", { href: "#skills/library", className: `subtab${tab === "library" ? " active" : ""}`, text: "Skills Library" }),
    el("a", { href: "#skills/agent", className: `subtab${tab === "agent" ? " active" : ""}`, text: "SOUL & Heartbeat" }),
    el("a", { href: "#skills/memory", className: `subtab${tab === "memory" ? " active" : ""}`, text: "Observational Memory" }),
    el("a", { href: "#skills/vault", className: `subtab${tab === "vault" ? " active" : ""}`, text: "Vault Search" }),
  );

  const actions = el(
    "div",
    { className: "actions-row" },
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
    await renderVaultFileEditor(body, "_agent/SOUL.md", "SOUL.md", "Static system directives cached at the top of every run.");
    await renderVaultFileEditor(body, "_agent/HEARTBEAT.md", "HEARTBEAT.md", "Periodic autonomous checklist evaluated on the heartbeat timer.");
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
    await renderVaultFileEditor(body, "_agent/memory/REFLECTIONS.md", "REFLECTIONS.md", "Condensed long-term context kept in the cached prompt prefix.", observeActions);
    await renderVaultFileEditor(body, "_agent/memory/OBSERVATIONS.md", "OBSERVATIONS.md", "Append-only dated observations distilled by the background Observer.");
    return;
  }

  if (tab === "vault") {
    const qIn = el("input", { type: "search", placeholder: "Search Obsidian Markdown notes…" });
    const hitsBox = el("div", { className: "stack" });
    const runSearch = async () => {
      try {
        const res = await api("GET", `/v1/assistant/vault/search?q=${encodeURIComponent(qIn.value.trim())}&limit=25`);
        clear(hitsBox);
        const hits = res.hits || [];
        if (hits.length === 0) {
          hitsBox.appendChild(el("div", { className: "small muted", text: "No matching notes found." }));
          return;
        }
        for (const h of hits) {
          hitsBox.appendChild(
            el(
              "article",
              { className: "card" },
              el("div", { className: "card-header" }, el("strong", { text: h.title }), el("span", { className: "mono small muted", text: h.path })),
              el("div", { className: "small muted mb-2", text: h.snippet }),
              el("button", {
                type: "button",
                className: "btn btn-sm",
                text: "Open Note",
                onclick: async () => {
                  const file = await api("GET", `/v1/assistant/vault/file?path=${encodeURIComponent(h.path)}`);
                  openModal(h.path, el("pre", { className: "code-block" }, el("code", { text: file.content || "" })));
                },
              }),
            ),
          );
        }
      } catch (err) {
        toast(err.message, "error");
      }
    };
    body.appendChild(
      el(
        "div",
        { className: "actions-row" },
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

  let skills = [];
  try {
    const res = await api("GET", "/v1/assistant/skills");
    skills = res.skills || [];
  } catch (err) {
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
          el("div", { className: "actions-row" }, sk.model ? badge(sk.model, "info") : null, ...(sk.tags || []).map((t) => badge(t, "neutral"))),
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
