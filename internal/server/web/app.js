import { api, clear, closeModal, el, state, toast } from "./lib.js";
import { renderOverview } from "./overview.js";
import { renderDevices } from "./devices.js";
import { renderAgents } from "./agents.js";
import { renderMemory } from "./memory.js";
import { renderSlacker } from "./slacker.js";
import { renderSystem } from "./system.js";

function parseHash() {
  const raw = (window.location.hash || "#overview").replace(/^#/, "");
  const [section, sub = ""] = raw.split("/", 2);
  return { section: section || "overview", sub };
}

function applySavedTheme() {
  const saved = localStorage.getItem("fleet_theme");
  if (saved === "light" || saved === "dark") {
    document.documentElement.setAttribute("data-theme", saved);
  } else {
    document.documentElement.removeAttribute("data-theme");
  }
}

function toggleTheme() {
  const cur = document.documentElement.getAttribute("data-theme");
  let next = "dark";
  if (cur === "dark") {
    next = "light";
  } else if (cur === "light") {
    next = "auto";
  } else {
    const sysDark = window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;
    next = sysDark ? "light" : "dark";
  }
  if (next === "auto") {
    localStorage.removeItem("fleet_theme");
  } else {
    localStorage.setItem("fleet_theme", next);
  }
  applySavedTheme();
}

function renderTopbar(ov, activeSection) {
  const nav = document.getElementById("main-nav");
  const pills = document.getElementById("status-pills");
  const callerBadge = document.getElementById("caller-badge");

  if (nav) {
    clear(nav);
    const links = [
      ["overview", "Overview", "#overview", false],
      ["devices", "Devices", "#devices", true],
      ["agents", "Agents", "#agents", false],
      ["memory", "Memory", "#memory", false],
      ["slacker", "Slacker", "#slacker", true],
      ["system", "System", "#system", true],
    ];
    for (const [id, label, href, adminOnly] of links) {
      if (adminOnly && !state.isAdmin) {
        continue;
      }
      nav.appendChild(
        el("a", {
          href,
          className: `nav-link${activeSection === id ? " active" : ""}`,
          text: label,
        }),
      );
    }
  }

  if (pills && ov) {
    clear(pills);
    if (ov.agents && ov.agents.waiting > 0) {
      pills.appendChild(
        el("button", {
          type: "button",
          className: "badge badge-warn pill-btn",
          text: `${ov.agents.waiting} waiting`,
          onclick: () => navigate("#agents/waiting"),
        }),
      );
    }
    if (ov.memory && ov.memory.pending_proposals > 0) {
      pills.appendChild(
        el("button", {
          type: "button",
          className: "badge badge-info pill-btn",
          text: `${ov.memory.pending_proposals} proposals`,
          onclick: () => navigate("#memory/proposals"),
        }),
      );
    }
  }

  if (callerBadge && ov && ov.caller) {
    callerBadge.textContent = `${ov.caller.name} · ${ov.caller.ip} · ${ov.caller.group}`;
  }
}

function navigate(hash) {
  if (window.location.hash === hash) {
    renderRoute();
  } else {
    window.location.hash = hash;
  }
}

let renderSeq = 0;

async function refresh() {
  const seq = ++renderSeq;
  try {
    const ov = await api("GET", "/v1/overview");
    if (seq !== renderSeq) {
      return;
    }
    state.overview = ov;
    state.caller = ov.caller;
    state.isAdmin = Boolean(ov.caller && ov.caller.is_admin);
  } catch (err) {
    const main = document.getElementById("app-main");
    if (main) {
      clear(main);
      main.appendChild(el("div", { className: "alert-banner alert-danger", text: `Unable to load fleet overview: ${err.message}` }));
    }
    return;
  }
  await renderCurrentRoute(seq);
}

async function renderRoute() {
  if (!state.overview) {
    await refresh();
    return;
  }
  await renderCurrentRoute(++renderSeq);
}

async function renderCurrentRoute(seq) {
  let { section, sub } = parseHash();
  if ((section === "devices" || section === "system" || section === "slacker") && !state.isAdmin) {
    toast("Admin device required for that view", "warn");
    section = "overview";
    sub = "";
    window.location.hash = "#overview";
  }
  state.route = section;
  state.editing = false;
  renderTopbar(state.overview, section);

  const main = document.getElementById("app-main");
  if (!main || seq !== renderSeq) {
    return;
  }
  const ctx = { refresh, navigate };

  switch (section) {
    case "devices":
      await renderDevices(main, ctx);
      break;
    case "agents":
      await renderAgents(main, ctx, sub);
      break;
    case "memory":
      await renderMemory(main, ctx, sub);
      break;
    case "slacker":
      await renderSlacker(main, ctx);
      break;
    case "system":
      await renderSystem(main, ctx);
      break;
    default:
      await renderOverview(main, ctx);
      break;
  }
}

let refreshTimer = null;

function setAutoRefresh(ms) {
  if (refreshTimer) {
    clearInterval(refreshTimer);
    refreshTimer = null;
  }
  localStorage.setItem("fleet_refresh_ms", String(ms));
  if (ms > 0) {
    refreshTimer = setInterval(() => {
      const { section, sub } = parseHash();
      const skipSection = section === "system" || section === "slacker" || (section === "memory" && sub === "curator") || (section === "agents" && sub === "messages");
      const ae = document.activeElement;
      const typing = Boolean(ae && ae.closest("#app-main") && /^(INPUT|TEXTAREA|SELECT)$/.test(ae.tagName));
      if (document.hidden || state.modalOpen || state.editing || typing || skipSection) {
        return;
      }
      refresh();
    }, ms);
  }
}

function init() {
  applySavedTheme();

  const refreshSel = document.getElementById("refresh-select");
  const savedRefresh = Number.parseInt(localStorage.getItem("fleet_refresh_ms") || "15000", 10);
  const initialMs = Number.isNaN(savedRefresh) ? 15000 : savedRefresh;
  if (refreshSel) {
    refreshSel.value = String(initialMs);
    refreshSel.addEventListener("change", () => {
      setAutoRefresh(Number.parseInt(refreshSel.value, 10) || 0);
    });
  }
  setAutoRefresh(initialMs);

  const refreshBtn = document.getElementById("refresh-btn");
  if (refreshBtn) {
    refreshBtn.addEventListener("click", () => refresh());
  }

  const themeBtn = document.getElementById("theme-btn");
  if (themeBtn) {
    themeBtn.addEventListener("click", toggleTheme);
  }

  const modalClose = document.getElementById("modal-close");
  if (modalClose) {
    modalClose.addEventListener("click", closeModal);
  }

  const backdrop = document.getElementById("modal-backdrop");
  if (backdrop) {
    backdrop.addEventListener("click", (ev) => {
      if (ev.target === backdrop) {
        closeModal();
      }
    });
  }

  document.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape" && state.modalOpen) {
      closeModal();
    }
  });

  window.addEventListener("hashchange", () => {
    renderRoute();
  });

  refresh();
}

init();
