export const state = {
  overview: null,
  caller: null,
  isAdmin: false,
  route: "overview",
  activeChatChannel: "",
  modalOpen: false,
  editing: false,
};

let nextFieldID = 0;

export async function api(method, path, body, opts = {}) {
  const fetchOpts = {
    method,
    headers: {},
    credentials: "same-origin",
  };
  if (opts && opts.signal) {
    fetchOpts.signal = opts.signal;
  }
  if (body !== undefined) {
    fetchOpts.headers["Content-Type"] = "application/json";
    fetchOpts.body = JSON.stringify(body);
  }
  const resp = await fetch(path, fetchOpts);
  const text = await resp.text();
  let data = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = null;
    }
  }
  if (!resp.ok) {
    const msg = (data && data.error) || `${resp.status} ${resp.statusText}`;
    const err = new Error(msg);
    err.status = resp.status;
    throw err;
  }
  return data;
}

export function el(tag, props, ...children) {
  const node = document.createElement(tag);
  if (props) {
    for (const [key, val] of Object.entries(props)) {
      if (val === null || val === undefined || val === false) {
        continue;
      }
      if (key === "className") {
        node.className = val;
      } else if (key === "text") {
        node.textContent = String(val);
      } else if (key === "style") {
        if (typeof val === "object" && !Array.isArray(val)) {
          Object.assign(node.style, val);
        }
      } else if (key === "dataset" && typeof val === "object") {
        for (const [dk, dv] of Object.entries(val)) {
          if (dv !== undefined && dv !== null) {
            node.dataset[dk] = String(dv);
          }
        }
      } else if (key.startsWith("on") && typeof val === "function") {
        node.addEventListener(key.slice(2).toLowerCase(), val);
      } else if (val === true) {
        node.setAttribute(key, "");
      } else if (key === "value" && (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || tag === "input" || tag === "textarea" || tag === "select")) {
        node.value = String(val);
      } else if (key === "checked" && (tag === "INPUT" || tag === "input")) {
        node.checked = Boolean(val);
      } else {
        node.setAttribute(key, String(val));
      }
    }
  }
  const appendChild = (child) => {
    if (child === null || child === undefined || child === false) {
      return;
    }
    if (Array.isArray(child)) {
      for (const nested of child) {
        appendChild(nested);
      }
      return;
    }
    if (child instanceof Node) {
      node.appendChild(child);
    } else {
      node.appendChild(document.createTextNode(String(child)));
    }
  };
  for (const c of children) {
    appendChild(c);
  }
  return node;
}

export function svgIcon(paths, className = "nav-icon") {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", "1.9");
  svg.setAttribute("stroke-linecap", "round");
  svg.setAttribute("stroke-linejoin", "round");
  svg.setAttribute("aria-hidden", "true");
  if (className) {
    svg.setAttribute("class", className);
  }
  for (const d of Array.isArray(paths) ? paths : [paths]) {
    const p = document.createElementNS(ns, "path");
    p.setAttribute("d", d);
    svg.appendChild(p);
  }
  return svg;
}

export function field(labelText, control, hint = null, extraClass = "") {
  if (control && typeof control.getAttribute === "function") {
    let id = control.getAttribute("id");
    if (!id) {
      id = `fleet-field-${++nextFieldID}`;
      control.setAttribute("id", id);
    }
    return el(
      "div",
      { className: extraClass ? `field ${extraClass}` : "field" },
      el("label", { for: id, text: labelText }),
      control,
      hint,
    );
  }
  return el(
    "div",
    { className: extraClass ? `field ${extraClass}` : "field" },
    el("span", { className: "field-label", text: labelText }),
    control,
    hint,
  );
}

export function section(title, opts = {}, ...children) {
  const headingId = title ? `fleet-sec-${++nextFieldID}` : null;
  const head = title
    ? el(
        "header",
        { className: "section-head" },
        el(
          "div",
          { className: "section-head-text" },
          el("h2", { id: headingId, className: "section-title", text: title }),
          opts.subtitle ? el("p", { className: "section-subtitle", text: opts.subtitle }) : null,
        ),
        opts.actions || null,
      )
    : null;
  return el(
    "section",
    {
      className: opts.className ? `ui-section ${opts.className}` : "ui-section",
      "aria-labelledby": headingId,
      "aria-label": !headingId ? opts.ariaLabel || null : null,
    },
    head,
    ...children,
  );
}

export function cell(label, props, ...children) {
  const merged = { ...(props || {}), "data-label": label };
  return el("td", merged, ...children);
}

export function clear(node) {
  while (node.firstChild) {
    node.removeChild(node.firstChild);
  }
}

export function toast(message, kind = "info") {
  const container = document.getElementById("toast-container");
  const text = String(message ?? "").trim();
  if (!container || !text) {
    return;
  }
  while (container.children.length >= 3) {
    container.firstElementChild.remove();
  }
  const cls = kind === "ok" ? "toast toast-ok" : kind === "warn" ? "toast toast-warn" : kind === "error" ? "toast toast-error" : "toast";
  let timer = null;
  const box = el("div", { className: cls });
  const dismiss = () => {
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
    box.remove();
  };
  box.append(
    el("span", { className: "toast-msg", text }),
    el("button", {
      type: "button",
      className: "toast-close",
      "aria-label": "Dismiss notification",
      text: "×",
      onclick: dismiss,
    }),
  );
  container.appendChild(box);
  timer = setTimeout(dismiss, kind === "error" ? 5000 : 4000);
}

export async function copyText(text, triggerBtn) {
  const val = String(text ?? "");
  let copied = false;
  if (window.isSecureContext && navigator.clipboard && navigator.clipboard.writeText) {
    try {
      await navigator.clipboard.writeText(val);
      copied = true;
    } catch {
      copied = false;
    }
  }
  if (!copied) {
    const ta = document.createElement("textarea");
    ta.value = val;
    ta.className = "offscreen-copy";
    ta.setAttribute("readonly", "");
    document.body.appendChild(ta);
    ta.focus();
    ta.select();
    try {
      copied = document.execCommand("copy");
    } catch {
      copied = false;
    }
    ta.remove();
  }
  if (copied) {
    if (triggerBtn) {
      const orig = triggerBtn.textContent;
      triggerBtn.textContent = "Copied";
      setTimeout(() => {
        triggerBtn.textContent = orig;
      }, 1200);
    }
    toast("Copied to clipboard", "ok");
  } else {
    toast("Copy failed — select and copy manually", "warn");
  }
}

let modalCleanup = null;
let lastFocusedEl = null;

export function closeModal() {
  const backdrop = document.getElementById("modal-backdrop");
  const body = document.getElementById("modal-body");
  if (backdrop) {
    backdrop.classList.add("hidden");
    backdrop.setAttribute("aria-hidden", "true");
  }
  if (body) {
    clear(body);
  }
  state.modalOpen = false;
  if (modalCleanup) {
    const fn = modalCleanup;
    modalCleanup = null;
    fn();
  }
  if (!state.modalOpen) {
    if (lastFocusedEl && typeof lastFocusedEl.focus === "function" && lastFocusedEl.isConnected) {
      lastFocusedEl.focus();
    }
    lastFocusedEl = null;
  }
}

export function openModal(title, contentEl, opts = {}) {
  const backdrop = document.getElementById("modal-backdrop");
  const titleEl = document.getElementById("modal-title");
  const body = document.getElementById("modal-body");
  if (!backdrop || !titleEl || !body) {
    return;
  }
  if (!state.modalOpen && document.activeElement && typeof document.activeElement.focus === "function") {
    lastFocusedEl = document.activeElement;
  }
  if (modalCleanup) {
    modalCleanup();
    modalCleanup = null;
  }
  titleEl.textContent = title;
  clear(body);
  if (contentEl) {
    body.appendChild(contentEl);
  }
  modalCleanup = opts.onClose || null;
  state.modalOpen = true;
  backdrop.classList.remove("hidden");
  backdrop.setAttribute("aria-hidden", "false");
  const focusTarget = body.querySelector("input:not([disabled]), textarea:not([disabled]), select:not([disabled]), button:not([disabled])") || document.getElementById("modal-close");
  if (focusTarget && typeof focusTarget.focus === "function") {
    if (typeof requestAnimationFrame === "function") {
      requestAnimationFrame(() => focusTarget.focus());
    } else {
      focusTarget.focus();
    }
  }
}

export function confirmDialog(title, message, confirmLabel = "Confirm", danger = false) {
  return new Promise((resolve) => {
    const backdrop = document.getElementById("modal-backdrop");
    const titleEl = document.getElementById("modal-title");
    const body = document.getElementById("modal-body");

    let savedTitle = null;
    let savedFrag = null;
    let savedCleanup = null;
    if (state.modalOpen && titleEl && body) {
      savedTitle = titleEl.textContent;
      savedFrag = document.createDocumentFragment();
      while (body.firstChild) {
        savedFrag.appendChild(body.firstChild);
      }
      savedCleanup = modalCleanup;
      modalCleanup = null;
    }

    const restoreSavedModal = () => {
      if (!savedFrag || !titleEl || !body) {
        return;
      }
      titleEl.textContent = savedTitle || "";
      clear(body);
      body.appendChild(savedFrag);
      savedFrag = null;
      modalCleanup = savedCleanup;
      savedCleanup = null;
      state.modalOpen = true;
      if (backdrop) {
        backdrop.classList.remove("hidden");
        backdrop.setAttribute("aria-hidden", "false");
      }
    };

    let settled = false;
    const finish = (val) => {
      if (settled) {
        return;
      }
      settled = true;
      if (!val && savedFrag) {
        restoreSavedModal();
        resolve(false);
        return;
      }
      savedFrag = null;
      if (val && savedCleanup) {
        savedCleanup();
        savedCleanup = null;
      }
      resolve(val);
    };

    const box = el(
      "div",
      { className: "form-grid" },
      el("p", { className: "m-0", text: message }),
      el(
        "div",
        { className: "actions-row flex-end mt-2" },
        el("button", {
          type: "button",
          className: "btn",
          text: "Cancel",
          onclick: () => {
            const hadSaved = Boolean(savedFrag);
            finish(false);
            if (!hadSaved) {
              closeModal();
            }
          },
        }),
        el("button", {
          type: "button",
          className: danger ? "btn btn-danger" : "btn btn-primary",
          text: confirmLabel,
          onclick: () => {
            finish(true);
            closeModal();
          },
        }),
      ),
    );
    openModal(title, box, { onClose: () => finish(false) });
  });
}

export function relTime(iso) {
  if (!iso) {
    return "never";
  }
  const ms = Date.parse(iso);
  if (Number.isNaN(ms)) {
    return iso;
  }
  const secs = Math.max(Math.floor((Date.now() - ms) / 1000), 0);
  if (secs < 45) {
    return "just now";
  }
  const units = [
    ["d", 86400],
    ["h", 3600],
    ["m", 60],
  ];
  for (const [label, size] of units) {
    if (secs >= size) {
      return `${Math.floor(secs / size)}${label} ago`;
    }
  }
  return `${secs}s ago`;
}

export function formatDuration(secs) {
  const s = Math.max(Math.floor(Number(secs) || 0), 0);
  if (s < 60) {
    return `${s}s`;
  }
  const mins = Math.floor(s / 60);
  if (mins < 60) {
    return `${mins}m`;
  }
  const hrs = Math.floor(mins / 60);
  const rem = mins % 60;
  if (hrs < 24) {
    return rem > 0 ? `${hrs}h${rem}m` : `${hrs}h`;
  }
  const days = Math.floor(hrs / 24);
  const remH = hrs % 24;
  return remH > 0 ? `${days}d${remH}h` : `${days}d`;
}

export function shortDate(iso) {
  if (!iso) {
    return "-";
  }
  return String(iso).slice(0, 10);
}

export function daysUntil(iso) {
  if (!iso) {
    return null;
  }
  const ms = Date.parse(iso);
  if (Number.isNaN(ms)) {
    return null;
  }
  return Math.floor((ms - Date.now()) / 86400000);
}

export function formatBytes(bytes) {
  const n = Number(bytes) || 0;
  if (n < 1024) {
    return `${n} B`;
  }
  if (n < 1024 * 1024) {
    return `${(n / 1024).toFixed(1)} KB`;
  }
  return `${(n / (1024 * 1024)).toFixed(2)} MB`;
}

export function makeCopyBtn(text, label = "Copy") {
  return el("button", {
    type: "button",
    className: "btn btn-sm",
    text: label,
    onclick: (ev) => {
      ev.stopPropagation();
      copyText(text, ev.currentTarget);
    },
  });
}

export function badge(text, variant = "neutral") {
  return el("span", { className: `badge badge-${variant}`, text });
}

export async function fetchPathsCatalog(device = "", opts = {}) {
  const qs = device ? `?device=${encodeURIComponent(device)}` : "";
  try {
    const res = await api("GET", `/v1/agents/paths${qs}`, undefined, opts);
    return {
      paths: Array.isArray(res.paths) ? res.paths : [],
      repos: Array.isArray(res.repos) ? res.repos : [],
      workspaces: res.workspaces && typeof res.workspaces === "object" ? res.workspaces : {},
    };
  } catch (err) {
    if (err && err.name === "AbortError") {
      throw err;
    }
    return { paths: [], repos: [], workspaces: {} };
  }
}

export function isWorkerOnline(runtimes, dev, nowMs = Date.now()) {
  const rt = (runtimes || []).find((r) => r.role === `worker:${dev}`);
  if (!rt || !rt.connected) {
    return false;
  }
  const ts = Date.parse(rt.last_poll_at || rt.updated_at || "");
  return Number.isFinite(ts) && nowMs - ts <= 90 * 1000;
}

export function formatMessageTarget(m) {
  if (!m) {
    return "all";
  }
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

export function formatDefaultPoolHint(defaultDevices = []) {
  return `default (${defaultDevices.length > 0 ? defaultDevices.join(", ") : "none"})`;
}

export function createDevicePoolPicker({
  pool = [],
  selected = [],
  defaultDevices = [],
  labelForDevice = (devName) => devName,
  onChange = null,
}) {
  let currentSelected = pool.filter((d) => selected.includes(d));
  const poolHint = el("span", {
    className: "small muted",
    text: currentSelected.length === 0 ? formatDefaultPoolHint(defaultDevices) : "",
  });

  const chipButtons = pool.map((devName) => {
    const isSelected = currentSelected.includes(devName);
    const btn = el("button", {
      type: "button",
      className: `chip${isSelected ? " active" : ""}`,
      text: labelForDevice(devName),
      onclick: () => {
        const cur = new Set(currentSelected);
        if (cur.has(devName)) {
          cur.delete(devName);
        } else {
          cur.add(devName);
        }
        currentSelected = pool.filter((d) => cur.has(d));
        btn.classList.toggle("active", cur.has(devName));
        poolHint.textContent = currentSelected.length === 0 ? formatDefaultPoolHint(defaultDevices) : "";
        if (onChange) {
          onChange(currentSelected.slice());
        }
      },
    });
    return btn;
  });

  return {
    element: el("div", { className: "chips-row" }, ...chipButtons, poolHint),
    getSelected: () => currentSelected.slice(),
  };
}

export function populateDatalist(listEl, items) {
  if (!listEl) {
    return;
  }
  clear(listEl);
  for (const item of items) {
    const val = typeof item === "string" ? item : item.value;
    const label = typeof item === "string" ? "" : item.label || item.detail || "";
    if (!val) {
      continue;
    }
    listEl.appendChild(el("option", label ? { value: val, label } : { value: val }));
  }
}

export function buildRepoSuggestions(catalog, targetDevices = [], deviceConfigs = {}) {
  const entries = (catalog && Array.isArray(catalog.paths) ? catalog.paths : []).filter((p) => p && p.path);
  const targets = Array.isArray(targetDevices) ? targetDevices.filter(Boolean) : [];
  const hasTargets = targets.length > 0;
  const allHaveWS = hasTargets && targets.every((d) => Boolean(deviceConfigs[d] && deviceConfigs[d].workspace_dir));

  const byValue = new Map();
  for (const p of entries) {
    const matchesTarget = !hasTargets || targets.includes(p.device);
    if (!matchesTarget && !(allHaveWS && p.subpath)) {
      continue;
    }
    let val = "";
    if (hasTargets) {
      val = allHaveWS && p.subpath ? p.subpath : p.tilde_path || p.path;
    } else {
      const devHasWS = Boolean(deviceConfigs[p.device] && deviceConfigs[p.device].workspace_dir);
      val = (devHasWS && p.subpath) || p.tilde_path || p.path;
    }
    if (!val) {
      continue;
    }
    const existing = byValue.get(val);
    if (!existing) {
      byValue.set(val, {
        value: val,
        repo: p.repo || "",
        subpath: p.subpath || "",
        path: p.path,
        devices: new Set([p.device]),
        matchesTarget,
        last_seen: p.last_seen || "",
        use_count: p.use_count || 1,
        sampleDetail: `${p.device}: ${p.path}`,
      });
    } else {
      existing.devices.add(p.device);
      existing.matchesTarget = existing.matchesTarget || matchesTarget;
      existing.use_count += p.use_count || 1;
      if ((p.last_seen || "") > existing.last_seen) {
        existing.last_seen = p.last_seen || "";
      }
      if (!existing.repo && p.repo) {
        existing.repo = p.repo;
      }
    }
  }

  return Array.from(byValue.values())
    .sort((a, b) => {
      if (a.matchesTarget !== b.matchesTarget) {
        return a.matchesTarget ? -1 : 1;
      }
      if (a.last_seen !== b.last_seen) {
        return a.last_seen > b.last_seen ? -1 : 1;
      }
      if (a.use_count !== b.use_count) {
        return b.use_count - a.use_count;
      }
      return a.value.localeCompare(b.value);
    })
    .map((item) => {
      const devList = Array.from(item.devices).sort();
      const repoTag = item.repo && item.repo !== item.value ? ` (${item.repo})` : "";
      const devTag = devList.length > 0 ? ` · ${devList.join(", ")}` : "";
      return {
        ...item,
        devices: devList,
        label: `${item.value}${repoTag}`,
        detail: `${item.repo ? `repo:${item.repo} · ` : ""}${item.sampleDetail}${devTag}`,
      };
    });
}

export function renderTrackedPathsBox(catalog, opts = {}, onChanged = null) {
  const paths = (catalog && Array.isArray(catalog.paths) ? catalog.paths : []).slice();
  const knownDevices = Array.isArray(opts.devices) && opts.devices.length > 0
    ? opts.devices
    : Array.from(new Set([state.caller ? state.caller.name : "", ...paths.map((p) => p.device)].filter(Boolean))).sort();

  const details = el(
    "details",
    { className: "details-box mt-3 mb-3", open: Boolean(opts.open) },
    el("summary", { text: `Known Directories & Repos (${paths.length})` }),
  );
  const content = el("div", { className: "form-grid mt-2" });

  if (paths.length === 0) {
    content.appendChild(el("div", { className: "muted small", text: "No directories or repos recorded yet — they are captured automatically from agent heartbeats, spawns, and Slacker configs." }));
  } else {
    const tbody = el("tbody");
    for (const p of paths) {
      const canEdit = state.isAdmin || (state.caller && p.device === state.caller.name);
      const acts = el("div", { className: "actions-row" });
      if (opts.onUse) {
        acts.appendChild(
          el("button", {
            type: "button",
            className: "btn btn-sm",
            text: opts.useLabel || "Use",
            onclick: () => opts.onUse(p),
          }),
        );
      }
      acts.appendChild(makeCopyBtn(p.subpath || p.tilde_path || p.path, "Copy"));
      if (canEdit) {
        acts.appendChild(
          el("button", {
            type: "button",
            className: "btn btn-sm btn-danger",
            text: "Remove",
            onclick: async () => {
              try {
                await api("DELETE", `/v1/agents/paths?device=${encodeURIComponent(p.device)}&path=${encodeURIComponent(p.path)}`);
                toast(`Removed ${p.path}`, "ok");
                if (onChanged) {
                  onChanged();
                }
              } catch (err) {
                toast(err.message, "error");
              }
            },
          }),
        );
      }
      const dirCell = cell("Directory", { className: "mono small cell-primary" }, p.path);
      if (p.subpath) {
        dirCell.appendChild(el("div", { className: "mt-1" }, badge(`subpath: ${p.subpath}`, "info")));
      }
      tbody.appendChild(
        el(
          "tr",
          null,
          cell("Device", { className: "mono small", text: p.device }),
          cell("Repo", { className: "mono small", text: p.repo || "-" }),
          dirCell,
          cell("Source", { className: "small muted", text: `${p.source || "-"} (${p.use_count || 1})` }),
          cell("Last Seen", { className: "small", text: relTime(p.last_seen) }),
          cell("Actions", { className: "cell-actions" }, acts),
        ),
      );
    }
    content.appendChild(
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
              el("th", { text: "Device" }),
              el("th", { text: "Repo" }),
              el("th", { text: "Directory" }),
              el("th", { text: "Source" }),
              el("th", { text: "Last Seen" }),
              el("th", { text: "Actions" }),
            ),
          ),
          tbody,
        ),
      ),
    );
  }

  const devInput = state.isAdmin && knownDevices.length > 0
    ? el("select", { "aria-label": "Device" }, ...knownDevices.map((d) => el("option", { value: d, text: d, selected: state.caller && d === state.caller.name })))
    : el("input", { type: "text", "aria-label": "Device", value: state.caller ? state.caller.name : "", disabled: true });
  const pathInput = el("input", { type: "text", className: "grow", "aria-label": "Directory path", placeholder: "Add directory (/abs/path, ~/path, or workspace subpath)" });
  const repoInput = el("input", { type: "text", className: "max-w-sm", "aria-label": "Repo name", placeholder: "Repo name (optional)" });
  const addBtn = el("button", { type: "submit", className: "btn btn-sm btn-primary", text: "+ Track Path" });

  content.appendChild(
    el(
      "form",
      {
        className: "toolbar m-0",
        onsubmit: async (ev) => {
          ev.preventDefault();
          const pVal = pathInput.value.trim();
          if (!pVal) {
            toast("Directory path is required", "warn");
            return;
          }
          addBtn.disabled = true;
          try {
            await api("POST", "/v1/agents/paths", {
              device: devInput.value.trim(),
              path: pVal,
              repo: repoInput.value.trim(),
            });
            pathInput.value = "";
            repoInput.value = "";
            toast("Tracked directory saved", "ok");
            if (onChanged) {
              onChanged();
            }
          } catch (err) {
            toast(err.message, "error");
          } finally {
            addBtn.disabled = false;
          }
        },
      },
      devInput,
      pathInput,
      repoInput,
      addBtn,
    ),
  );

  details.appendChild(content);
  return details;
}
