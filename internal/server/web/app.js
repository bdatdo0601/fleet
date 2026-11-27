import { api, clear, closeModal, el, state, svgIcon, toast } from "./lib.js";
import { renderOverview } from "./overview.js";
import { renderDevices } from "./devices.js";
import { renderAgents } from "./agents.js";
import { renderMemory } from "./memory.js";
import { renderSlacker } from "./slacker.js";
import { renderSystem } from "./system.js";
import { renderChat } from "./chat.js";
import { renderSkills } from "./skills.js";

const NAV_ITEMS = [
  {
    id: "overview",
    label: "Overview",
    href: "#overview",
    adminOnly: false,
    icon: ["M3 9.5L12 3l9 6.5V20a1 1 0 0 1-1 1h-5v-6H9v6H4a1 1 0 0 1-1-1V9.5z"],
  },
  {
    id: "chat",
    label: "Chat",
    href: "#chat",
    adminOnly: true,
    icon: ["M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"],
  },
  {
    id: "skills",
    label: "Skills",
    href: "#skills",
    adminOnly: true,
    icon: ["M4 19.5A2.5 2.5 0 0 1 6.5 17H20", "M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"],
  },
  {
    id: "agents",
    label: "Agents",
    href: "#agents",
    adminOnly: false,
    icon: ["M12 2a3 3 0 0 0-3 3v1H7a3 3 0 0 0-3 3v7a3 3 0 0 0 3 3h10a3 3 0 0 0 3-3V9a3 3 0 0 0-3-3h-2V5a3 3 0 0 0-3-3z", "M9 13h.01M15 13h.01"],
  },
  {
    id: "memory",
    label: "Memory",
    href: "#memory",
    adminOnly: false,
    icon: ["M4 6a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6z", "M8 9h8M8 13h6M8 17h4"],
  },
  {
    id: "devices",
    label: "Devices",
    href: "#devices",
    adminOnly: true,
    icon: ["M4 5h16v10H4z", "M8 19h8M12 15v4"],
  },
  {
    id: "slacker",
    label: "Slacker",
    href: "#slacker",
    adminOnly: true,
    icon: ["M4 9h16M4 15h16M9 4v16M15 4v16"],
  },
  {
    id: "system",
    label: "System",
    href: "#system",
    adminOnly: true,
    icon: ["M12 3v3M12 18v3M4.9 4.9l2.1 2.1M17 17l2.1 2.1M3 12h3M18 12h3M4.9 19.1L7 17M17 7l2.1-2.1", "M12 8.5a3.5 3.5 0 1 0 0 7 3.5 3.5 0 0 0 0-7z"],
  },
];

function parseHash() {
  const raw = (window.location.hash || "#overview").replace(/^#/, "");
  const [section, sub = ""] = raw.split("/", 2);
  return { section: section || "overview", sub };
}

function updateThemeUI(mode) {
  const themeBtn = document.getElementById("theme-btn");
  if (themeBtn) {
    const label = mode === "dark" ? "Theme: Dark" : mode === "light" ? "Theme: Light" : "Theme: Auto";
    themeBtn.textContent = label;
    themeBtn.setAttribute("aria-label", `${label}. Click to cycle theme.`);
  }
}

function applySavedTheme() {
  const saved = localStorage.getItem("fleet_theme");
  if (saved === "light" || saved === "dark") {
    document.documentElement.setAttribute("data-theme", saved);
    updateThemeUI(saved);
  } else {
    document.documentElement.removeAttribute("data-theme");
    updateThemeUI("auto");
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

function setControlsOpen(open) {
  const controls = document.getElementById("topbar-controls");
  const toggle = document.getElementById("controls-toggle");
  if (controls) {
    controls.classList.toggle("open", open);
  }
  if (toggle) {
    toggle.setAttribute("aria-expanded", open ? "true" : "false");
  }
}

function updateOfflineBanner(forceOffline = false) {
  const banner = document.getElementById("offline-banner");
  if (!banner) {
    return;
  }
  const show = forceOffline || (typeof navigator !== "undefined" && navigator.onLine === false);
  banner.classList.toggle("hidden", !show);
}

function renderTopbar(ov, activeSection) {
  const nav = document.getElementById("main-nav");
  const mobileNav = document.getElementById("mobile-nav");
  const pills = document.getElementById("status-pills");
  const callerBadge = document.getElementById("caller-badge");

  const visibleItems = NAV_ITEMS.filter((item) => !item.adminOnly || state.isAdmin);
  const waitingCount = ov && ov.agents ? ov.agents.waiting || 0 : 0;
  const propCount = ov && ov.memory ? ov.memory.pending_proposals || 0 : 0;
  const aprCount = ov && ov.assistant ? ov.assistant.pending_approvals || 0 : 0;

  if (nav) {
    clear(nav);
    for (const item of visibleItems) {
      const isActive = activeSection === item.id;
      nav.appendChild(
        el("a", {
          href: item.href,
          className: `nav-link${isActive ? " active" : ""}`,
          "aria-current": isActive ? "page" : null,
          text: item.label,
        }),
      );
    }
  }

  if (mobileNav) {
    clear(mobileNav);
    for (const item of visibleItems) {
      const isActive = activeSection === item.id;
      const badgeCount = item.id === "agents" ? waitingCount : item.id === "memory" ? propCount : item.id === "chat" ? aprCount : 0;
      mobileNav.appendChild(
        el(
          "a",
          {
            href: item.href,
            className: `bottom-nav-link${isActive ? " active" : ""}`,
            "aria-current": isActive ? "page" : null,
          },
          el(
            "span",
            { className: "bottom-nav-icon-wrap" },
            svgIcon(item.icon, "bottom-nav-icon"),
            badgeCount > 0
              ? el("span", {
                  className: `bottom-nav-dot ${item.id === "agents" ? "dot-warn" : "dot-info"}`,
                  text: badgeCount > 9 ? "9+" : String(badgeCount),
                })
              : null,
          ),
          el("span", { className: "bottom-nav-label", text: item.label }),
        ),
      );
    }
  }

  if (pills && ov) {
    clear(pills);
    if (waitingCount > 0) {
      pills.appendChild(
        el("button", {
          type: "button",
          className: "badge badge-warn pill-btn",
          text: `${waitingCount} waiting`,
          onclick: () => navigate("#agents/waiting"),
        }),
      );
    }
    if (aprCount > 0) {
      pills.appendChild(
        el("button", {
          type: "button",
          className: "badge badge-warn pill-btn",
          text: `${aprCount} approvals`,
          onclick: () => navigate("#chat"),
        }),
      );
    }
    if (propCount > 0) {
      pills.appendChild(
        el("button", {
          type: "button",
          className: "badge badge-info pill-btn",
          text: `${propCount} proposals`,
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
  setControlsOpen(false);
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
    updateOfflineBanner(false);
    state.overview = ov;
    state.caller = ov.caller;
    state.isAdmin = Boolean(ov.caller && ov.caller.is_admin);
  } catch (err) {
    updateOfflineBanner(err.status === 503 || navigator.onLine === false);
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
  setControlsOpen(false);
  if (!state.overview) {
    await refresh();
    return;
  }
  await renderCurrentRoute(++renderSeq);
}

async function renderCurrentRoute(seq) {
  if (typeof state.routeCleanup === "function") {
    state.routeCleanup();
    state.routeCleanup = null;
  }
  let { section, sub } = parseHash();
  if ((section === "devices" || section === "system" || section === "slacker" || section === "chat" || section === "skills") && !state.isAdmin) {
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
    case "chat":
      await renderChat(main, ctx, sub);
      break;
    case "skills":
      await renderSkills(main, ctx, sub);
      break;
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
      await renderSlacker(main, ctx, sub);
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
      const skipSection = section === "system" || section === "slacker" || section === "chat" || section === "skills" || (section === "memory" && sub === "curator") || (section === "agents" && sub === "messages");
      const ae = document.activeElement;
      const typing = Boolean(ae && ae.closest("#app-main") && /^(INPUT|TEXTAREA|SELECT)$/.test(ae.tagName));
      if (document.hidden || state.modalOpen || state.editing || typing || skipSection || navigator.onLine === false) {
        return;
      }
      refresh();
    }, ms);
  }
}

function setupPWA() {
  if ("serviceWorker" in navigator && window.isSecureContext) {
    navigator.serviceWorker.register("/sw.js").catch(() => {});
  }

  let deferredPrompt = null;
  const installBtn = document.getElementById("install-btn");
  if (!installBtn) {
    return;
  }

  window.addEventListener("beforeinstallprompt", (ev) => {
    ev.preventDefault();
    deferredPrompt = ev;
    installBtn.classList.remove("hidden");
  });

  installBtn.addEventListener("click", async () => {
    if (!deferredPrompt) {
      return;
    }
    deferredPrompt.prompt();
    try {
      await deferredPrompt.userChoice;
    } catch {
      // ignore cancelled prompt
    }
    deferredPrompt = null;
    installBtn.classList.add("hidden");
  });

  window.addEventListener("appinstalled", () => {
    deferredPrompt = null;
    installBtn.classList.add("hidden");
    toast("Fleet installed as app", "ok");
  });
}

function init() {
  applySavedTheme();
  setupPWA();
  updateOfflineBanner(false);

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

  const controlsToggle = document.getElementById("controls-toggle");
  if (controlsToggle) {
    controlsToggle.addEventListener("click", (ev) => {
      ev.stopPropagation();
      const controls = document.getElementById("topbar-controls");
      setControlsOpen(!(controls && controls.classList.contains("open")));
    });
  }

  document.addEventListener("click", (ev) => {
    const controls = document.getElementById("topbar-controls");
    const toggle = document.getElementById("controls-toggle");
    if (controls && controls.classList.contains("open") && !controls.contains(ev.target) && ev.target !== toggle) {
      setControlsOpen(false);
    }
  });

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
    if (ev.key === "Escape") {
      if (state.modalOpen) {
        closeModal();
      } else {
        setControlsOpen(false);
      }
    }
  });

  window.addEventListener("online", () => {
    updateOfflineBanner(false);
    refresh();
  });
  window.addEventListener("offline", () => {
    updateOfflineBanner(true);
  });

  window.addEventListener("hashchange", () => {
    renderRoute();
  });

  refresh();
}

init();
