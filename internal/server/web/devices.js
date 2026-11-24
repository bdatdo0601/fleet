import {
  api,
  badge,
  clear,
  closeModal,
  confirmDialog,
  daysUntil,
  el,
  makeCopyBtn,
  openModal,
  relTime,
  shortDate,
  state,
  toast,
} from "./lib.js";

const devFilter = {
  status: "all",
  query: "",
};

function expectedClientVersion() {
  const manifest = state.overview && state.overview.manifest;
  if (!manifest || !manifest.commit) {
    return "";
  }
  return `go/${String(manifest.commit).slice(0, 7)}`;
}

function renderMobileResult(resp, onDone) {
  const lh = resp.lighthouse || {};
  const box = el(
    "div",
    { className: "form-grid" },
    el("div", { className: "alert-banner alert-ok", text: `${resp.name} approved at ${resp.ip} (expires ${shortDate(resp.not_after)}).` }),
    el(
      "div",
      { className: "field" },
      el("div", { className: "actions-row flex-between" }, el("label", { text: "Certificate (paste into Mobile Nebula)" }), makeCopyBtn(resp.cert)),
      el("pre", { className: "code-block", text: String(resp.cert || "").trim() }),
    ),
    el(
      "div",
      { className: "field" },
      el("div", { className: "actions-row flex-between" }, el("label", { text: "CA Certificate" }), makeCopyBtn(resp.ca)),
      el("pre", { className: "code-block", text: String(resp.ca || "").trim() }),
    ),
    el(
      "div",
      { className: "card" },
      el("h4", { className: "card-title mb-2", text: "Lighthouse Host Settings" }),
      el(
        "dl",
        { className: "kv-list" },
        el("dt", { className: "kv-label", text: "Mesh IP" }),
        el("dd", { className: "kv-value mono", text: lh.ip || "10.213.0.1" }),
        el("dt", { className: "kv-label", text: "Lighthouse" }),
        el("dd", { className: "kv-value", text: "yes" }),
        el("dt", { className: "kv-label", text: "Address" }),
        el("dd", { className: "kv-value mono", text: lh.address || "-" }),
      ),
    ),
    el(
      "div",
      { className: "actions-row flex-end" },
      el("button", {
        type: "button",
        className: "btn btn-primary",
        text: "Done",
        onclick: () => {
          closeModal();
          if (onDone) {
            onDone();
          }
        },
      }),
    ),
  );
  return box;
}

function openApproveMacModal(onDone) {
  let decoded = null;
  const blockInput = el("textarea", {
    rows: 6,
    placeholder: "-----BEGIN FLEET JOIN REQUEST-----\n...\n-----END FLEET JOIN REQUEST-----",
  });
  const decodeBox = el("div", { className: "field-hint", text: "Paste the join request from `fleet join NAME`." });
  const nameInput = el("input", { type: "text", placeholder: "e.g. office" });
  const groupSelect = el(
    "select",
    null,
    el("option", { value: "managed", text: "managed" }),
    el("option", { value: "admin", text: "admin" }),
  );
  const replaceCheck = el("input", { type: "checkbox" });
  const passInput = el("input", { type: "password", autocomplete: "off", placeholder: "CA passphrase" });
  const submitBtn = el("button", { type: "submit", className: "btn btn-primary", text: "Approve Device" });
  const container = el("div", { className: "form-grid" });

  const parseBlock = async () => {
    const raw = blockInput.value.trim();
    if (!raw) {
      toast("Paste a join request block first", "warn");
      return;
    }
    try {
      const res = await api("POST", "/v1/join/decode", { block: raw });
      if (res.kind !== "mac") {
        toast("That looks like a raw mobile public key — use Approve Mobile instead", "warn");
        return;
      }
      decoded = res;
      if (res.name && !nameInput.value.trim()) {
        nameInput.value = res.name;
      }
      clear(decodeBox);
      decodeBox.appendChild(
        el(
          "div",
          { className: "actions-row" },
          badge(`request id: ${res.request_id}`, "info"),
          res.existing
            ? badge(`existing: ${res.existing_ip} (${res.existing_group}${res.existing_revoked ? ", revoked" : ""})`, "warn")
            : badge("new device", "ok"),
        ),
      );
    } catch (err) {
      decoded = null;
      decodeBox.textContent = err.message;
      toast(err.message, "error");
    }
  };

  blockInput.addEventListener("input", () => {
    if (blockInput.value.includes("END FLEET JOIN REQUEST")) {
      parseBlock();
    }
  });

  const form = el(
    "form",
    {
      className: "form-grid",
      onsubmit: async (ev) => {
        ev.preventDefault();
        if (!decoded) {
          await parseBlock();
          if (!decoded) {
            return;
          }
        }
        const passphrase = passInput.value;
        passInput.value = "";
        if (!passphrase) {
          toast("CA passphrase is required", "warn");
          return;
        }
        submitBtn.disabled = true;
        try {
          const resp = await api("POST", "/v1/approve", {
            name: nameInput.value.trim(),
            pubkey: decoded.pubkey,
            kind: "mac",
            group: groupSelect.value,
            replace: replaceCheck.checked,
            passphrase,
          });
          clear(container);
          container.appendChild(
            el(
              "div",
              { className: "form-grid" },
              el("div", {
                className: "alert-banner alert-ok",
                text: `Approved ${resp.name} -> ${resp.ip} (CA id ${resp.ca_id})`,
              }),
              el(
                "div",
                { className: "field" },
                el(
                  "div",
                  { className: "actions-row flex-between" },
                  el("label", { text: "FLEET JOIN RESPONSE (paste into `fleet join`)" }),
                  makeCopyBtn(resp.response_block || ""),
                ),
                el("pre", { className: "code-block", text: resp.response_block || "" }),
              ),
              el(
                "div",
                { className: "actions-row flex-end" },
                el("button", {
                  type: "button",
                  className: "btn btn-primary",
                  text: "Done",
                  onclick: () => {
                    closeModal();
                    onDone();
                  },
                }),
              ),
            ),
          );
        } catch (err) {
          toast(err.message, "error");
        } finally {
          submitBtn.disabled = false;
        }
      },
    },
    el(
      "div",
      { className: "field" },
      el("div", { className: "actions-row flex-between" }, el("label", { text: "Join Request Block" }), el("button", { type: "button", className: "btn btn-sm", text: "Parse Block", onclick: parseBlock })),
      blockInput,
      decodeBox,
    ),
    el(
      "div",
      { className: "form-row-2" },
      el("div", { className: "field" }, el("label", { text: "Device Name" }), nameInput),
      el("div", { className: "field" }, el("label", { text: "Group" }), groupSelect),
    ),
    el("label", { className: "checkbox-row" }, replaceCheck, "Replace existing device certificate (keep IP)"),
    el("div", { className: "field" }, el("label", { text: "CA Passphrase" }), passInput),
    el(
      "div",
      { className: "actions-row flex-end" },
      el("button", { type: "button", className: "btn", text: "Cancel", onclick: closeModal }),
      submitBtn,
    ),
  );

  container.appendChild(form);
  openModal("Approve Mac Device", container);
}

function openApproveMobileModal(onDone) {
  const nameInput = el("input", { type: "text", placeholder: "e.g. phone" });
  const groupSelect = el(
    "select",
    null,
    el("option", { value: "admin", text: "admin" }),
    el("option", { value: "managed", text: "managed" }),
  );
  const pubInput = el("textarea", {
    rows: 4,
    placeholder: "-----BEGIN NEBULA X25519 PUBLIC KEY-----\n...\n-----END NEBULA X25519 PUBLIC KEY-----",
  });
  const keyBadgeBox = el("div", { className: "field-hint", text: "Paste the public key from the Mobile Nebula app." });
  const replaceCheck = el("input", { type: "checkbox" });
  const passInput = el("input", { type: "password", autocomplete: "off", placeholder: "CA passphrase" });
  const submitBtn = el("button", { type: "submit", className: "btn btn-primary", text: "Approve Mobile" });
  const container = el("div", { className: "form-grid" });

  const verifyKey = async () => {
    const raw = pubInput.value.trim();
    if (!raw) {
      toast("Paste a Nebula public key first", "warn");
      return;
    }
    try {
      const res = await api("POST", "/v1/join/decode", { block: raw });
      clear(keyBadgeBox);
      keyBadgeBox.appendChild(badge(`key id: ${res.request_id}`, "info"));
    } catch (err) {
      keyBadgeBox.textContent = err.message;
      toast(err.message, "error");
    }
  };

  const form = el(
    "form",
    {
      className: "form-grid",
      onsubmit: async (ev) => {
        ev.preventDefault();
        const passphrase = passInput.value;
        passInput.value = "";
        if (!nameInput.value.trim() || !pubInput.value.trim() || !passphrase) {
          toast("Name, public key, and CA passphrase are required", "warn");
          return;
        }
        submitBtn.disabled = true;
        try {
          const resp = await api("POST", "/v1/approve", {
            name: nameInput.value.trim(),
            pubkey: pubInput.value,
            kind: "mobile",
            group: groupSelect.value,
            replace: replaceCheck.checked,
            passphrase,
          });
          clear(container);
          container.appendChild(renderMobileResult(resp, onDone));
        } catch (err) {
          toast(err.message, "error");
        } finally {
          submitBtn.disabled = false;
        }
      },
    },
    el(
      "div",
      { className: "form-row-2" },
      el("div", { className: "field" }, el("label", { text: "Device Name" }), nameInput),
      el("div", { className: "field" }, el("label", { text: "Group" }), groupSelect),
    ),
    el(
      "div",
      { className: "field" },
      el("div", { className: "actions-row flex-between" }, el("label", { text: "Nebula X25519 Public Key" }), el("button", { type: "button", className: "btn btn-sm", text: "Verify Key ID", onclick: verifyKey })),
      pubInput,
      keyBadgeBox,
    ),
    el("label", { className: "checkbox-row" }, replaceCheck, "Replace existing device certificate (keep IP)"),
    el("div", { className: "field" }, el("label", { text: "CA Passphrase" }), passInput),
    el(
      "div",
      { className: "actions-row flex-end" },
      el("button", { type: "button", className: "btn", text: "Cancel", onclick: closeModal }),
      submitBtn,
    ),
  );

  container.appendChild(form);
  openModal("Approve Mobile Device", container);
}

function openSetGroupModal(dev, onDone) {
  const groupSelect = el(
    "select",
    null,
    el("option", { value: "admin", text: "admin", selected: dev.group === "admin" }),
    el("option", { value: "managed", text: "managed", selected: dev.group === "managed" }),
  );
  const passInput = el("input", { type: "password", autocomplete: "off", placeholder: "CA passphrase" });
  const submitBtn = el("button", { type: "submit", className: "btn btn-primary", text: "Re-sign Certificate" });
  const container = el("div", { className: "form-grid" });

  const form = el(
    "form",
    {
      className: "form-grid",
      onsubmit: async (ev) => {
        ev.preventDefault();
        const passphrase = passInput.value;
        passInput.value = "";
        if (!passphrase) {
          toast("CA passphrase is required", "warn");
          return;
        }
        submitBtn.disabled = true;
        try {
          const resp = await api("POST", "/v1/set-group", {
            name: dev.name,
            group: groupSelect.value,
            passphrase,
          });
          if (resp.kind === "mobile") {
            clear(container);
            container.appendChild(renderMobileResult(resp, onDone));
          } else {
            toast(`${dev.name} is now ${resp.group}; picks up cert on next sync (<= 5 min)`, "ok");
            closeModal();
            onDone();
          }
        } catch (err) {
          toast(err.message, "error");
        } finally {
          submitBtn.disabled = false;
        }
      },
    },
    el("p", {
      className: "m-0 muted small",
      text: `Re-signs ${dev.name} (${dev.ip}, ${dev.kind}) and immediately blocklists its previous certificate.`,
    }),
    el("div", { className: "field" }, el("label", { text: "Target Group" }), groupSelect),
    el("div", { className: "field" }, el("label", { text: "CA Passphrase" }), passInput),
    el(
      "div",
      { className: "actions-row flex-end" },
      el("button", { type: "button", className: "btn", text: "Cancel", onclick: closeModal }),
      submitBtn,
    ),
  );

  container.appendChild(form);
  openModal(`Group / Renew: ${dev.name}`, container);
}

async function openDeviceConfigModal(name) {
  try {
    const res = await api("GET", `/v1/devices/${encodeURIComponent(name)}/config`);
    const body = el(
      "div",
      { className: "form-grid" },
      el(
        "div",
        { className: "actions-row flex-between" },
        el("span", { className: "muted small", text: "Rendered Nebula configuration YAML" }),
        makeCopyBtn(res.config, "Copy YAML"),
      ),
      el("pre", { className: "code-block", text: res.config }),
    );
    openModal(`Nebula Config: ${name}`, body);
  } catch (err) {
    toast(err.message, "error");
  }
}

async function handleRevoke(dev, onDone) {
  const ok = await confirmDialog(
    `Revoke ${dev.name}?`,
    `Revoke ${dev.name} (${dev.ip})? It will lose mesh access everywhere within ~5 minutes. Also remove its SSH key from authorized_keys on machines it could reach.`,
    "Revoke Device",
    true,
  );
  if (!ok) {
    return;
  }
  try {
    await api("POST", "/v1/revoke", { name: dev.name });
    toast(`Revoked ${dev.name}`, "ok");
    onDone();
  } catch (err) {
    toast(err.message, "error");
  }
}

export async function renderDevices(root, ctx) {
  if (!state.isAdmin) {
    root.appendChild(el("div", { className: "empty-state", text: "Admin privileges required to manage devices." }));
    return;
  }

  let devData;
  let revData;
  try {
    [devData, revData] = await Promise.all([api("GET", "/v1/devices"), api("GET", "/v1/revocations")]);
  } catch (err) {
    clear(root);
    root.appendChild(el("div", { className: "alert-banner alert-danger", text: err.message }));
    return;
  }

  const expectedVer = expectedClientVersion();
  const allDevices = devData.devices || [];
  const revocations = revData.revocations || [];

  clear(root);
  root.appendChild(
    el(
      "div",
      { className: "page-header" },
      el(
        "div",
        null,
        el("h1", { className: "page-title", text: "Devices & PKI" }),
        el("p", { className: "page-subtitle", text: `${allDevices.length} registered · CA id ${devData.ca_id}` }),
      ),
      el(
        "div",
        { className: "actions-row" },
        el("button", { type: "button", className: "btn btn-primary", text: "+ Approve Mac", onclick: () => openApproveMacModal(() => ctx.refresh()) }),
        el("button", { type: "button", className: "btn", text: "+ Approve Mobile", onclick: () => openApproveMobileModal(() => ctx.refresh()) }),
      ),
    ),
  );

  const tableContainer = el("div");
  const statusChips = ["all", "active", "revoked"].map((st) =>
    el("button", {
      type: "button",
      className: `chip${devFilter.status === st ? " active" : ""}`,
      dataset: { status: st },
      text: st === "all" ? "All" : st === "active" ? "Active" : "Revoked",
      onclick: () => {
        devFilter.status = st;
        for (const btn of statusChips) {
          btn.classList.toggle("active", btn.dataset.status === st);
        }
        drawTable();
      },
    }),
  );

  const searchInput = el("input", {
    type: "search",
    placeholder: "Filter name, IP, kind, group…",
    value: devFilter.query,
    oninput: (ev) => {
      devFilter.query = ev.target.value;
      drawTable();
    },
  });

  root.appendChild(el("div", { className: "toolbar" }, el("div", { className: "chips-row" }, statusChips), el("div", { className: "toolbar-spacer" }), searchInput));
  root.appendChild(tableContainer);

  const drawTable = () => {
    clear(tableContainer);
    const q = devFilter.query.trim().toLowerCase();
    const filtered = allDevices.filter((d) => {
      const revoked = Boolean(d.revoked_at);
      if (devFilter.status === "active" && revoked) {
        return false;
      }
      if (devFilter.status === "revoked" && !revoked) {
        return false;
      }
      if (!q) {
        return true;
      }
      return [d.name, d.ip, d.kind, d.group, d.client_version || ""].some((v) => String(v).toLowerCase().includes(q));
    });

    const tbody = el("tbody");
    for (const d of filtered) {
      const revoked = Boolean(d.revoked_at);
      const remDays = daysUntil(d.not_after);
      const expBadges = [el("span", { className: "mono", text: shortDate(d.not_after) })];
      if (!revoked && remDays !== null && remDays <= 30) {
        expBadges.push(" ", badge(remDays <= 0 ? "expired" : `${remDays}d`, remDays <= 0 ? "danger" : "warn"));
      }
      const clientCell = [el("span", { className: "mono", text: d.client_version || "-" })];
      if (!revoked && d.kind !== "mobile" && expectedVer && d.client_version !== expectedVer) {
        clientCell.push(" ", badge("outdated", "warn"));
      }
      const actions = el("div", { className: "actions-row" });
      if (d.kind !== "mobile") {
        actions.appendChild(
          el("button", { type: "button", className: "btn btn-sm", text: "Config", onclick: () => openDeviceConfigModal(d.name) }),
        );
      }
      if (d.kind !== "lighthouse" && !revoked) {
        actions.appendChild(
          el("button", { type: "button", className: "btn btn-sm", text: "Group / Renew", onclick: () => openSetGroupModal(d, () => ctx.refresh()) }),
        );
        actions.appendChild(
          el("button", { type: "button", className: "btn btn-sm btn-danger", text: "Revoke", onclick: () => handleRevoke(d, () => ctx.refresh()) }),
        );
      }
      tbody.appendChild(
        el(
          "tr",
          null,
          el("td", null, el("strong", { text: d.name })),
          el(
            "td",
            null,
            el("div", { className: "actions-row" }, makeCopyBtn(d.ip, d.ip), d.kind !== "mobile" ? makeCopyBtn(`ssh ${d.ip}`, "ssh") : null),
          ),
          el("td", { text: d.kind }),
          el("td", null, badge(d.group, d.group === "admin" || d.group === "lighthouse" ? "info" : "neutral")),
          el("td", null, badge(revoked ? "revoked" : "active", revoked ? "danger" : "ok")),
          el("td", { text: relTime(d.last_seen) }),
          el("td", null, expBadges),
          el("td", null, clientCell),
          el("td", null, actions),
        ),
      );
    }

    if (filtered.length === 0) {
      tableContainer.appendChild(el("div", { className: "empty-state", text: "No devices match the current filter." }));
      return;
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
              el("th", { text: "Name" }),
              el("th", { text: "IP" }),
              el("th", { text: "Kind" }),
              el("th", { text: "Group" }),
              el("th", { text: "Status" }),
              el("th", { text: "Last Seen" }),
              el("th", { text: "Cert Expires" }),
              el("th", { text: "Client" }),
              el("th", { text: "Actions" }),
            ),
          ),
          tbody,
        ),
      ),
    );
  };

  drawTable();

  const revRows = el("tbody");
  for (const r of revocations) {
    revRows.appendChild(
      el(
        "tr",
        null,
        el("td", { text: r.name }),
        el("td", { className: "mono", text: r.fingerprint }),
        el("td", { text: r.reason }),
        el("td", { text: relTime(r.revoked_at) }),
      ),
    );
  }
  root.appendChild(
    el(
      "details",
      { className: "details-box" },
      el("summary", { text: `Revoked Certificates / PKI Blocklist (${revocations.length})` }),
      revocations.length === 0
        ? el("p", { className: "muted small mt-2 m-0", text: "No revoked or replaced certificates recorded." })
        : el(
            "div",
            { className: "table-wrap mt-2" },
            el(
              "table",
              null,
              el(
                "thead",
                null,
                el("tr", null, el("th", { text: "Device" }), el("th", { text: "Fingerprint" }), el("th", { text: "Reason" }), el("th", { text: "Revoked" })),
              ),
              revRows,
            ),
          ),
    ),
  );
}
