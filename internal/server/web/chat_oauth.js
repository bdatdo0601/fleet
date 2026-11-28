import { api, badge, el, toast } from "./lib.js";

/**
 * Parse a pasted OAuth callback URL or raw authorization code.
 */
export function parseMCPOAuthPaste(raw) {
  const txt = String(raw || "").trim();
  if (!txt) {
    return { code: "" };
  }
  try {
    const u = new URL(txt);
    const err = (u.searchParams.get("error") || "").trim();
    const errDesc = (u.searchParams.get("error_description") || "").trim();
    if (err) {
      return { error: errDesc || err };
    }
    const code = (u.searchParams.get("code") || "").trim();
    const state = (u.searchParams.get("state") || "").trim();
    if (code) {
      return { code, state };
    }
  } catch {
    // Raw authorization code entered directly.
  }
  return { code: txt };
}

/**
 * Resolve the OAuth redirect URI for web callback vs desktop loopback fallback.
 */
export function buildMCPOAuthRedirectURI(opts = {}) {
  const explicit = String(opts.explicitRedirectURI || "").trim();
  if (explicit) {
    return explicit;
  }
  const loc = typeof window !== "undefined" && window.location ? window.location : {};
  const protocol = opts.protocol ?? loc.protocol ?? "";
  const hostname = opts.hostname ?? loc.hostname ?? "";
  const origin = opts.origin ?? loc.origin ?? "";
  if (opts.mode === "loopback") {
    return "http://127.0.0.1:8765/oauth2callback";
  }
  if (opts.mode === "web" && origin) {
    return `${origin}/v1/assistant/mcp/oauth/callback`;
  }
  const isHTTPS = protocol === "https:";
  const isLocal = hostname === "localhost" || hostname === "127.0.0.1";
  if (isHTTPS || isLocal || opts.preferOriginRedirect) {
    return `${origin}/v1/assistant/mcp/oauth/callback`;
  }
  return "http://127.0.0.1:8765/oauth2callback";
}

/**
 * Initiate an MCP OAuth flow and open the authorize URL safely without triggering popup blockers.
 */
export async function startMCPOAuth(server, opts = {}) {
  const srvName = String(server || "").trim();
  if (!srvName) {
    throw new Error("MCP server name is missing");
  }
  const loc = typeof window !== "undefined" && window.location ? window.location : {};
  const isHTTPS = loc.protocol === "https:";
  const isLocal = loc.hostname === "localhost" || loc.hostname === "127.0.0.1";
  const redirectURI = buildMCPOAuthRedirectURI({
    explicitRedirectURI: opts.explicitRedirectURI,
    preferOriginRedirect: opts.preferOriginRedirect,
    mode: opts.mode,
  });
  if (opts.warnIfInsecure && !isHTTPS && !isLocal && !opts.explicitRedirectURI && opts.mode !== "loopback") {
    toast(`Google Web OAuth requires an HTTPS fleet tls domain, or run: fleet assistant mcp-auth ${srvName}`, "warn");
  }
  const canOpen = typeof window !== "undefined" && typeof window.open === "function";
  const popup = canOpen ? window.open("", "_blank") : null;
  let startRes;
  try {
    startRes = await api("POST", `/v1/assistant/mcp/${encodeURIComponent(srvName)}/oauth/start`, { redirect_uri: redirectURI });
  } catch (err) {
    if (popup && !popup.closed) {
      try {
        popup.close();
      } catch {
        // ignore close failure
      }
    }
    throw err;
  }
  let popupBlocked = canOpen && !popup;
  if (startRes && startRes.authorize_url && popup && !popup.closed) {
    try {
      popup.opener = null;
    } catch {
      // ignore read-only opener
    }
    try {
      if (popup.location && typeof popup.location.replace === "function") {
        popup.location.replace(startRes.authorize_url);
      } else {
        popup.location.href = startRes.authorize_url;
      }
    } catch {
      popupBlocked = true;
    }
  }
  return { startRes, isHTTPS, isLocal, redirectURI, popupBlocked };
}

const oauthSubscribers = new Set();
let oauthChannel = null;
let oauthWindowListener = null;
let lastEventSig = "";
let lastEventTime = 0;

function handleOAuthCompletionPayload(payload) {
  if (!payload || typeof payload !== "object" || payload.type !== "fleet-mcp-oauth-done") {
    return;
  }
  const sig = JSON.stringify([payload.server || "", Boolean(payload.ok), payload.tool_count || 0, payload.error || ""]);
  const now = Date.now();
  if (sig === lastEventSig && now - lastEventTime < 1500) {
    return;
  }
  lastEventSig = sig;
  lastEventTime = now;

  if (payload.ok) {
    toast(`Connected OAuth for ${payload.server || "MCP"} (${payload.tool_count || 0} tools)`, "ok");
  } else if (payload.error) {
    toast(payload.error, "error");
  }

  for (const fn of [...oauthSubscribers]) {
    try {
      fn(payload);
    } catch {
      // keep dispatching to remaining subscribers
    }
  }
}

function attachOAuthTransport() {
  if (!oauthChannel && typeof BroadcastChannel === "function") {
    oauthChannel = new BroadcastChannel("fleet-mcp-oauth");
    oauthChannel.addEventListener("message", (ev) => {
      handleOAuthCompletionPayload(ev.data);
    });
  }
  if (!oauthWindowListener && typeof window !== "undefined") {
    oauthWindowListener = (ev) => {
      if (ev.origin !== window.location.origin) {
        return;
      }
      handleOAuthCompletionPayload(ev.data);
    };
    window.addEventListener("message", oauthWindowListener);
  }
}

function detachOAuthTransportIfIdle() {
  if (oauthSubscribers.size > 0) {
    return;
  }
  if (oauthChannel) {
    oauthChannel.close();
    oauthChannel = null;
  }
  if (oauthWindowListener && typeof window !== "undefined") {
    window.removeEventListener("message", oauthWindowListener);
    oauthWindowListener = null;
  }
}

/**
 * Subscribe to consolidated MCP OAuth completion events via BroadcastChannel + postMessage fallback.
 */
export function subscribeMCPOAuthDone(fn) {
  oauthSubscribers.add(fn);
  attachOAuthTransport();
  return () => {
    oauthSubscribers.delete(fn);
    detachOAuthTransportIfIdle();
  };
}

export function renderMCPOAuthCard(card, data, sendPromptFn) {
  const server = String(data.server || data.name || data.server_name || "").trim();
  let provider = String(data.provider || "").toLowerCase();
  let scopes = Array.isArray(data.scopes) ? data.scopes : [];
  let explicitRedirectURI = String(data.redirect_uri || "").trim();
  const cliCmd = data.cli_command || (server ? `fleet assistant mcp-auth ${server}` : "");
  let authorized = Boolean(data.authorized);
  let activeState = "";
  let redirectMode = "auto";
  let oauthDisabledReason = "";

  const loc = typeof window !== "undefined" && window.location ? window.location : {};
  const isWebOrigin = loc.protocol === "https:" || loc.hostname === "localhost" || loc.hostname === "127.0.0.1";

  card.dataset.mcpOauthServer = server;
  const statusBadge = badge(authorized ? "Authorized" : "Not connected", authorized ? "ok" : "warn");
  const providerBadge = badge(provider === "google" ? "Google OAuth" : "OAuth 2.0", "info");
  const header = el(
    "div",
    { className: "gen-mini-row" },
    el("span", { className: "mono", text: server || "mcp" }),
    providerBadge,
    statusBadge,
  );
  card.appendChild(header);

  const urlLine = el("div", { className: data.url ? "small muted mono" : "small muted mono hidden", text: data.url || "" });
  const scopesLine = el("div", {
    className: scopes.length > 0 ? "small muted" : "small muted hidden",
    text: scopes.length > 0 ? `Scopes: ${scopes.join(", ")}` : "",
  });
  card.append(urlLine, scopesLine);

  const redirectInfo = el("div", { className: "small muted mono" });
  const modeSelect = isWebOrigin
    ? el(
        "select",
        { "aria-label": "OAuth redirect mode" },
        el("option", { value: "auto", text: "Web callback (auto)", selected: true }),
        el("option", { value: "loopback", text: "Desktop loopback (paste URL)" }),
      )
    : null;
  const syncRedirectInfo = () => {
    const uri = buildMCPOAuthRedirectURI({ explicitRedirectURI, mode: redirectMode });
    redirectInfo.textContent = `Redirect URI: ${uri}`;
    if (modeSelect) {
      modeSelect.classList.toggle("hidden", Boolean(explicitRedirectURI));
    }
  };
  syncRedirectInfo();
  card.appendChild(el("div", { className: "gen-mini-row" }, redirectInfo, modeSelect));

  const detailLine = el("div", { className: "small muted" });
  if (data.expires_at) {
    detailLine.textContent = `Expires: ${data.expires_at}`;
  }
  card.appendChild(detailLine);

  const linkRow = el("div", { className: "actions-row mt-1 hidden" });
  const openLink = el("a", { className: "btn btn-sm", target: "_blank", rel: "noopener noreferrer", text: "Open Sign-In Tab ↗" });
  linkRow.appendChild(openLink);

  const manualBox = el("div", { className: "stack-sm mt-1 hidden" });
  const pasteIn = el("input", { type: "text", placeholder: "Paste redirected URL (http://127.0.0.1:.../oauth2callback?code=...) or code" });
  const completeBtn = el("button", { type: "button", className: "btn btn-sm btn-primary", text: "Complete Sign-In" });
  manualBox.append(pasteIn, el("div", { className: "actions-row" }, completeBtn));

  if (modeSelect) {
    modeSelect.addEventListener("change", () => {
      redirectMode = modeSelect.value || "auto";
      syncRedirectInfo();
      if (redirectMode === "loopback") {
        manualBox.classList.remove("hidden");
      }
    });
  }

  const connectBtn = el("button", { type: "button", className: "btn btn-sm btn-primary" });
  const pasteToggleBtn = el("button", {
    type: "button",
    className: "btn btn-sm",
    text: "Paste callback / code",
    onclick: () => {
      manualBox.classList.toggle("hidden");
    },
  });
  const verifyBtn = el("button", {
    type: "button",
    className: "btn btn-sm btn-primary",
    text: "Verify & Continue",
    onclick: () => {
      if (sendPromptFn && server) {
        sendPromptFn(`I've completed OAuth sign-in for MCP server ${server}. Please verify with mcp_test and continue.`);
      }
    },
  });
  const disconnectBtn = el("button", { type: "button", className: "btn btn-sm", text: "Disconnect" });

  const syncButtons = () => {
    providerBadge.textContent = provider === "google" ? "Google OAuth" : "OAuth 2.0";
    connectBtn.textContent = authorized ? "Reconnect" : provider === "google" ? "Connect with Google" : "Connect OAuth";
    connectBtn.disabled = Boolean(oauthDisabledReason);
    pasteToggleBtn.disabled = Boolean(oauthDisabledReason);
    statusBadge.textContent = oauthDisabledReason ? "HTTP OAuth off" : authorized ? "Authorized" : "Not connected";
    statusBadge.className = `badge badge-${oauthDisabledReason ? "neutral" : authorized ? "ok" : "warn"}`;
    verifyBtn.classList.toggle("hidden", !authorized || Boolean(oauthDisabledReason));
    disconnectBtn.classList.toggle("hidden", !authorized || Boolean(oauthDisabledReason));
  };
  syncButtons();

  connectBtn.onclick = async () => {
    if (!server) {
      toast("MCP server name is missing", "error");
      return;
    }
    connectBtn.disabled = true;
    try {
      const { startRes, redirectURI, popupBlocked } = await startMCPOAuth(server, {
        explicitRedirectURI,
        mode: redirectMode,
      });
      activeState = startRes.state || "";
      openLink.href = startRes.authorize_url || "#";
      openLink.className = popupBlocked ? "btn btn-sm btn-primary" : "btn btn-sm";
      linkRow.classList.remove("hidden");
      const isLoopback = redirectURI.startsWith("http://127.0.0.1");
      if (isLoopback) {
        manualBox.classList.remove("hidden");
        detailLine.textContent = popupBlocked
          ? `Browser blocked the popup. Tap "Open Sign-In Tab ↗" below, complete sign-in, then paste the redirected URL (or code) — or run: ${cliCmd}`
          : `Opened sign-in with Desktop loopback redirect. After signing in, paste the redirected URL (or code) below — or run: ${cliCmd}`;
      } else if (popupBlocked) {
        detailLine.textContent = 'Browser blocked the popup. Tap "Open Sign-In Tab ↗" below to authorize.';
      } else {
        detailLine.textContent = "Complete sign-in in the opened browser tab; status will update automatically.";
      }
    } catch (err) {
      toast(err.message, "error");
    } finally {
      connectBtn.disabled = Boolean(oauthDisabledReason);
    }
  };

  completeBtn.onclick = async () => {
    const parsed = parseMCPOAuthPaste(pasteIn.value);
    if (parsed.error) {
      toast(parsed.error, "error");
      return;
    }
    if (!parsed.code) {
      toast("Paste the redirected URL or authorization code", "error");
      return;
    }
    const stateToUse = parsed.state || activeState;
    if (!stateToUse) {
      toast("Click Connect first to initialize an OAuth state", "error");
      return;
    }
    completeBtn.disabled = true;
    try {
      const res = await api("POST", `/v1/assistant/mcp/${encodeURIComponent(server)}/oauth/exchange`, {
        state: stateToUse,
        code: parsed.code,
      });
      authorized = Boolean(res.authorized);
      manualBox.classList.add("hidden");
      linkRow.classList.add("hidden");
      detailLine.textContent = res.test_error
        ? `Authorized, but handshake test failed: ${res.test_error}`
        : `Authorized · ${res.tool_count || 0} tool(s) ready`;
      syncButtons();
      toast(`Connected OAuth for ${server} (${res.tool_count || 0} tools)`, "ok");
    } catch (err) {
      toast(err.message, "error");
    } finally {
      completeBtn.disabled = false;
    }
  };

  disconnectBtn.onclick = async () => {
    disconnectBtn.disabled = true;
    try {
      await api("DELETE", `/v1/assistant/mcp/${encodeURIComponent(server)}/oauth`);
      authorized = false;
      detailLine.textContent = "Disconnected.";
      syncButtons();
      toast(`Disconnected OAuth for ${server}`, "ok");
    } catch (err) {
      toast(err.message, "error");
    } finally {
      disconnectBtn.disabled = false;
    }
  };

  card._onMCPOAuthDone = (payload) => {
    if (!payload) {
      return;
    }
    if (payload.ok) {
      authorized = true;
      manualBox.classList.add("hidden");
      linkRow.classList.add("hidden");
      detailLine.textContent = payload.test_error
        ? `Authorized, but handshake test failed: ${payload.test_error}`
        : `Authorized · ${payload.tool_count || 0} tool(s) ready`;
      syncButtons();
    } else if (payload.error) {
      detailLine.textContent = `Authorization failed: ${payload.error}`;
    }
  };

  const syncLiveStatus = async () => {
    if (!server || typeof fetch !== "function") {
      return;
    }
    try {
      const res = await api("GET", "/v1/assistant/mcp");
      if (card.isConnected === false) {
        return;
      }
      const srv = (res.servers || []).find((s) => s.name === server);
      const st = (res.statuses || []).find((s) => s.name === server);
      if (!srv) {
        return;
      }
      if (srv.url) {
        urlLine.textContent = srv.url;
        urlLine.classList.remove("hidden");
      }
      if (!srv.oauth || !srv.oauth.enabled) {
        oauthDisabledReason = `${server} is configured as ${srv.transport || "stdio"} without Fleet HTTP OAuth.`;
        detailLine.textContent = oauthDisabledReason;
        syncButtons();
        return;
      }
      oauthDisabledReason = "";
      provider = String(srv.oauth.provider || provider).toLowerCase();
      if (Array.isArray(srv.oauth.scopes)) {
        scopes = srv.oauth.scopes;
        scopesLine.textContent = scopes.length > 0 ? `Scopes: ${scopes.join(", ")}` : "";
        scopesLine.classList.toggle("hidden", scopes.length === 0);
      }
      if (srv.oauth.redirect_uri) {
        explicitRedirectURI = String(srv.oauth.redirect_uri).trim();
        syncRedirectInfo();
      }
      if (st) {
        authorized = Boolean(st.oauth_authorized);
        if (st.oauth_authorized) {
          if (st.error) {
            detailLine.textContent = `Authorized, but handshake check failed: ${st.error}`;
          } else if (st.tool_count > 0) {
            detailLine.textContent = `Authorized · ${st.tool_count} tool(s) ready${st.oauth_expires_at ? ` · expires ${st.oauth_expires_at}` : ""}`;
          } else if (st.oauth_expires_at) {
            detailLine.textContent = `Authorized · expires ${st.oauth_expires_at}`;
          }
        } else if (st.error) {
          detailLine.textContent = st.error;
        }
      }
      syncButtons();
    } catch {
      // Keep card functional with embedded payload if status endpoint is unavailable.
    }
  };
  void syncLiveStatus();

  card.append(
    el("div", { className: "actions-row mt-1" }, connectBtn, pasteToggleBtn, verifyBtn, disconnectBtn),
    linkRow,
    manualBox,
    cliCmd ? el("div", { className: "small muted mono mt-1", text: `CLI alternative: ${cliCmd}` }) : null,
  );
  return card;
}
