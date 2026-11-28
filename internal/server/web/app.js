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
    id: "chat",
    label: "Chat",
    href: "#chat",
    adminOnly: true,
    icon: ["M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"],
  },
  {
    id: "overview",
    label: "Overview",
    href: "#overview",
    adminOnly: false,
    icon: ["M3 9.5L12 3l9 6.5V20a1 1 0 0 1-1 1h-5v-6H9v6H4a1 1 0 0 1-1-1V9.5z"],
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
  const raw = (window.location.hash || "").replace(/^#/, "");
  if (!raw) {
    return { section: "", sub: "" };
  }
  const [section, sub = ""] = raw.split("/", 2);
  return { section: section || "", sub };
}

function updateThemeUI(mode) {
  const label = mode === "dark" ? "Theme: Dark" : mode === "light" ? "Theme: Light" : "Theme: Auto";
  for (const id of ["theme-btn", "drawer-theme"]) {
    const btn = document.getElementById(id);
    if (btn) {
      btn.textContent = label;
      btn.setAttribute("aria-label", `${label}. Click to cycle theme.`);
    }
  }
}

function resolveTheme(saved, sysDark) {
  if (saved === "light" || saved === "dark") {
    return saved;
  }
  return sysDark ? "dark" : "light";
}

function applySavedTheme() {
  const saved = localStorage.getItem("fleet_theme");
  const sysDark = Boolean(window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches);
  document.documentElement.setAttribute("data-theme", resolveTheme(saved, sysDark));
  updateThemeUI(saved === "light" || saved === "dark" ? saved : "auto");
  window.dispatchEvent(new CustomEvent("fleet-theme-change"));
}

function toggleTheme() {
  const saved = localStorage.getItem("fleet_theme");
  let next = "dark";
  if (saved === "dark") {
    next = "light";
  } else if (saved === "light") {
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

function setDrawerOpen(open) {
  const backdrop = document.getElementById("nav-drawer-backdrop");
  if (!backdrop) {
    return;
  }
  backdrop.classList.toggle("hidden", !open);
  backdrop.setAttribute("aria-hidden", open ? "false" : "true");
  if (open) {
    const closeBtn = document.getElementById("drawer-close");
    if (closeBtn) {
      closeBtn.focus();
    }
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
  syncOverviewBadgeCount();
  updateAlertsUI();
  const drawerCaller = document.getElementById("drawer-caller");
  if (drawerCaller && ov && ov.caller) {
    drawerCaller.textContent = `${ov.caller.name} · ${ov.caller.ip} · ${ov.caller.group}`;
  }
  const drawerNav = document.getElementById("drawer-nav");
  if (drawerNav) {
    clear(drawerNav);
    for (const item of visibleItems) {
      const isActive = activeSection === item.id;
      const badgeCount = item.id === "agents" ? waitingCount : item.id === "memory" ? propCount : item.id === "chat" ? aprCount : 0;
      drawerNav.appendChild(
        el(
          "a",
          {
            href: item.href,
            className: `drawer-nav-link${isActive ? " active" : ""}`,
            "aria-current": isActive ? "page" : null,
            onclick: () => setDrawerOpen(false),
          },
          el("span", { className: "drawer-nav-left" }, svgIcon(item.icon, "bottom-nav-icon"), el("span", { text: item.label })),
          badgeCount > 0
            ? el("span", {
                className: `badge ${item.id === "agents" ? "badge-warn" : "badge-info"}`,
                text: String(badgeCount),
              })
            : null,
        ),
      );
    }
  }
}

function navigate(hash) {
  setControlsOpen(false);
  setDrawerOpen(false);
  if (window.location.hash === hash) {
    renderRoute();
  } else {
    window.location.hash = hash;
  }
}

let renderSeq = 0;
let routeAbortController = null;

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
    ensureAssistantSSE();
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
  if (routeAbortController) {
    routeAbortController.abort();
    routeAbortController = null;
  }
  if (typeof state.routeCleanup === "function") {
    state.routeCleanup();
    state.routeCleanup = null;
  }
  const ac = new AbortController();
  routeAbortController = ac;
  const { signal } = ac;

  let { section, sub } = parseHash();
  if (!section) {
    section = state.isAdmin ? "chat" : "overview";
  }
  if ((section === "devices" || section === "system" || section === "slacker" || section === "chat" || section === "skills") && !state.isAdmin) {
    toast("Admin device required for that view", "warn");
    section = "overview";
    sub = "";
    window.location.hash = "#overview";
  }
  state.route = section;
  state.editing = false;
  const isChat = section === "chat";
  document.documentElement.classList.toggle("view-chat", isChat);
  if (!isChat) {
    document.documentElement.classList.remove("kb-open");
    document.documentElement.style.removeProperty("--vvh");
    document.documentElement.style.removeProperty("--vvt");
  }
  renderTopbar(state.overview, section);

  const main = document.getElementById("app-main");
  if (!main || seq !== renderSeq || signal.aborted) {
    return;
  }
  const ctx = { refresh, navigate, openMenu: () => setDrawerOpen(true), signal };

  try {
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
  } catch (err) {
    if (err && err.name === "AbortError") {
      return;
    }
    throw err;
  }
}

let refreshTimer = null;
let assistantES = null;
const SSE_EVENT_TYPES = [
  "ready",
  "run_start",
  "tool_start",
  "tool_done",
  "delta",
  "message_created",
  "message_done",
  "approval",
  "approval_decided",
  "channel_reset",
  "channels_updated",
  "config_updated",
  "grant_updated",
  "outbox",
  "delegation",
  "error",
];

function alertsEnabled() {
  return localStorage.getItem("fleet_alerts") === "1" && typeof Notification !== "undefined" && Notification.permission === "granted";
}

function updateAlertsUI() {
  const active = alertsEnabled();
  const label = active ? "Alerts: On" : "Alerts: Off";
  for (const id of ["notify-btn", "drawer-notify"]) {
    const btn = document.getElementById(id);
    if (!btn) {
      continue;
    }
    btn.classList.toggle("hidden", !state.isAdmin);
    btn.textContent = label;
  }
}

function syncOverviewBadgeCount() {
  const waitingCount = (state.overview && state.overview.agents && state.overview.agents.waiting) || 0;
  const aprCount = (state.overview && state.overview.assistant && state.overview.assistant.pending_approvals) || 0;
  updateAppBadgeCount(aprCount + waitingCount);
}

function updateAppBadgeCount(count) {
  if (typeof navigator === "undefined") {
    return;
  }
  try {
    if (count > 0 && typeof navigator.setAppBadge === "function") {
      navigator.setAppBadge(count).catch(() => {});
    } else if (count <= 0 && typeof navigator.clearAppBadge === "function") {
      navigator.clearAppBadge().catch(() => {});
    }
  } catch {
    // ignore unsupported badge API
  }
}

async function toggleAlerts() {
  if (!window.isSecureContext || typeof Notification === "undefined") {
    toast("Browser notifications require HTTPS (configure `fleet tls`)", "warn");
    return;
  }
  if (alertsEnabled()) {
    localStorage.setItem("fleet_alerts", "0");
    updateAlertsUI();
    toast("Desktop PWA alerts muted", "info");
    return;
  }
  let perm = Notification.permission;
  if (perm === "default") {
    perm = await Notification.requestPermission();
  }
  if (perm !== "granted") {
    toast("Notification permission was denied by browser", "warn");
    updateAlertsUI();
    return;
  }
  localStorage.setItem("fleet_alerts", "1");
  updateAlertsUI();
  toast("Desktop PWA alerts enabled", "ok");
}

function showBrowserAlert(title, body, channelID) {
  if (!document.hidden || !alertsEnabled()) {
    return;
  }
  try {
    const n = new Notification(title || "Fleet Assistant", {
      body: body || "",
      tag: `fleet-${channelID || "general"}`,
    });
    n.onclick = () => {
      try {
        window.focus();
      } catch {
        // ignore focus restriction
      }
      navigate(`#chat/${channelID || "general"}`);
      n.close();
    };
  } catch {
    // ignore notification failure
  }
}

function dispatchSSE(type, payload) {
  window.dispatchEvent(new CustomEvent("fleet-sse", { detail: { type, payload } }));
}

function ensureAssistantSSE() {
  if (!state.isAdmin) {
    if (assistantES) {
      assistantES.close();
      assistantES = null;
    }
    state.sseStatus = "disconnected";
    return;
  }
  if (assistantES) {
    if (assistantES.readyState !== EventSource.CLOSED) {
      return;
    }
    assistantES.close();
    assistantES = null;
  }
  state.sseStatus = "connecting";
  assistantES = new EventSource("/v1/assistant/stream");
  assistantES.onerror = () => {
    if (assistantES && assistantES.readyState === EventSource.CLOSED) {
      assistantES.close();
      assistantES = null;
      state.sseStatus = "disconnected";
      dispatchSSE("status", { status: "disconnected" });
      return;
    }
    state.sseStatus = "reconnecting";
    dispatchSSE("status", { status: "reconnecting" });
  };
  for (const evType of SSE_EVENT_TYPES) {
    assistantES.addEventListener(evType, (ev) => {
      let payload = {};
      if (ev.data) {
        try {
          payload = JSON.parse(ev.data);
        } catch {
          payload = {};
        }
      }
      if (evType === "ready") {
        state.sseStatus = "live";
        dispatchSSE("status", { status: "live" });
      } else if (evType === "outbox") {
        const it = payload.data || {};
        const ch = payload.channel_id || it.channel_id || "briefing";
        const title = it.title || `Assistant (#${ch})`;
        if (!document.hidden) {
          toast(`${title}: ${it.body || ""}`, it.priority === "urgent" ? "warn" : "info");
        } else {
          showBrowserAlert(title, it.body || "", ch);
        }
      } else if (evType === "approval") {
        const apr = payload.data || {};
        const ch = payload.channel_id || apr.channel_id || "general";
        const curApr = (state.overview && state.overview.assistant && state.overview.assistant.pending_approvals) || 0;
        if (state.overview && state.overview.assistant) {
          state.overview.assistant.pending_approvals = curApr + 1;
        }
        syncOverviewBadgeCount();
        const viewingChannel = state.route === "chat" && state.activeChatChannel === ch;
        if (apr.tool_name === "frontier_escalation") {
          if (!document.hidden && !viewingChannel) {
            toast(`Frontier check (#${ch}): ${apr.summary || "Approve model escalation?"}`, "info");
          }
        } else if (!document.hidden) {
          if (!viewingChannel) {
            toast(`Approval needed (#${ch}): ${apr.summary || apr.tool_name || "tool"}`, "warn");
          }
        } else {
          showBrowserAlert(`Approval needed (#${ch})`, apr.summary || apr.tool_name || "", ch);
        }
      } else if (evType === "approval_decided") {
        const curApr = (state.overview && state.overview.assistant && state.overview.assistant.pending_approvals) || 0;
        if (state.overview && state.overview.assistant) {
          state.overview.assistant.pending_approvals = Math.max(0, curApr - 1);
        }
        syncOverviewBadgeCount();
      }
      dispatchSSE(evType, payload);
    });
  }
}

function setAutoRefresh(ms) {
  if (refreshTimer) {
    clearInterval(refreshTimer);
    refreshTimer = null;
  }
  localStorage.setItem("fleet_refresh_ms", String(ms));
  for (const id of ["refresh-select", "drawer-refresh-select"]) {
    const sel = document.getElementById(id);
    if (sel && sel.value !== String(ms)) {
      sel.value = String(ms);
    }
  }
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
  const darkMedia = window.matchMedia ? window.matchMedia("(prefers-color-scheme: dark)") : null;
  if (darkMedia && typeof darkMedia.addEventListener === "function") {
    darkMedia.addEventListener("change", () => {
      const saved = localStorage.getItem("fleet_theme");
      if (saved !== "light" && saved !== "dark") {
        applySavedTheme();
      }
    });
  }
  setupPWA();
  updateOfflineBanner(false);

  const refreshSel = document.getElementById("refresh-select");
  const drawerRefreshSel = document.getElementById("drawer-refresh-select");
  const savedRefresh = Number.parseInt(localStorage.getItem("fleet_refresh_ms") || "15000", 10);
  const initialMs = Number.isNaN(savedRefresh) ? 15000 : savedRefresh;
  for (const sel of [refreshSel, drawerRefreshSel]) {
    if (sel) {
      sel.value = String(initialMs);
      sel.addEventListener("change", () => {
        setAutoRefresh(Number.parseInt(sel.value, 10) || 0);
      });
    }
  }
  setAutoRefresh(initialMs);

  const refreshBtn = document.getElementById("refresh-btn");
  if (refreshBtn) {
    refreshBtn.addEventListener("click", () => refresh());
  }
  const drawerRefresh = document.getElementById("drawer-refresh");
  if (drawerRefresh) {
    drawerRefresh.addEventListener("click", () => {
      setDrawerOpen(false);
      refresh();
    });
  }

  const themeBtn = document.getElementById("theme-btn");
  if (themeBtn) {
    themeBtn.addEventListener("click", toggleTheme);
  }
  const drawerTheme = document.getElementById("drawer-theme");
  if (drawerTheme) {
    drawerTheme.addEventListener("click", toggleTheme);
  }
  for (const id of ["notify-btn", "drawer-notify"]) {
    const btn = document.getElementById(id);
    if (btn) {
      btn.addEventListener("click", () => {
        setDrawerOpen(false);
        toggleAlerts();
      });
    }
  }

  const drawerClose = document.getElementById("drawer-close");
  if (drawerClose) {
    drawerClose.addEventListener("click", () => setDrawerOpen(false));
  }
  const drawerBackdrop = document.getElementById("nav-drawer-backdrop");
  if (drawerBackdrop) {
    drawerBackdrop.addEventListener("click", (ev) => {
      if (ev.target === drawerBackdrop) {
        setDrawerOpen(false);
      }
    });
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
      const drawerBackdropEl = document.getElementById("nav-drawer-backdrop");
      if (state.modalOpen) {
        closeModal();
      } else if (drawerBackdropEl && !drawerBackdropEl.classList.contains("hidden")) {
        setDrawerOpen(false);
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
