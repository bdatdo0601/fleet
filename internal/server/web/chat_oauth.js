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
  const isHTTPS = protocol === "https:";
  const isLocal = hostname === "localhost" || hostname === "127.0.0.1";
  if (isHTTPS || isLocal || opts.preferOriginRedirect) {
    return `${origin}/v1/assistant/mcp/oauth/callback`;
  }
  return "http://127.0.0.1:8765/oauth2callback";
}

/**
 * Initiate an MCP OAuth flow and open the authorize URL safely with noopener,noreferrer.
 */
export async function startMCPOAuth(server, opts = {}) {
  const srvName = String(server || "").trim();
  if (!srvName) {
    throw new Error("MCP server name is missing");
  }
  const isHTTPS = typeof window !== "undefined" && window.location.protocol === "https:";
  const isLocal = typeof window !== "undefined" && (window.location.hostname === "localhost" || window.location.hostname === "127.0.0.1");
  const redirectURI = buildMCPOAuthRedirectURI({ explicitRedirectURI: opts.explicitRedirectURI, preferOriginRedirect: opts.preferOriginRedirect });
  if (opts.warnIfInsecure && !isHTTPS && !isLocal && !opts.explicitRedirectURI) {
    toast(`Google Web OAuth requires an HTTPS fleet tls domain, or run: fleet assistant mcp-auth ${srvName}`, "warn");
  }
  const startRes = await api("POST", `/v1/assistant/mcp/${encodeURIComponent(srvName)}/oauth/start`, { redirect_uri: redirectURI });
  if (typeof window !== "undefined" && typeof window.open === "function" && startRes.authorize_url) {
    window.open(startRes.authorize_url, "_blank", "noopener,noreferrer");
  }
  return { startRes, isHTTPS, isLocal, redirectURI };
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
  const server = String(data.server || "").trim();
  const provider = String(data.provider || "").toLowerCase();
  const scopes = Array.isArray(data.scopes) ? data.scopes : [];
  const cliCmd = data.cli_command || (server ? `fleet assistant mcp-auth ${server}` : "");
  let authorized = Boolean(data.authorized);
  let activeState = "";

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
  if (data.url) {
    card.appendChild(el("div", { className: "small muted mono", text: data.url }));
  }
  if (scopes.length > 0) {
    card.appendChild(el("div", { className: "small muted", text: `Scopes: ${scopes.join(", ")}` }));
  }

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
    connectBtn.textContent = authorized ? "Reconnect" : provider === "google" ? "Connect with Google" : "Connect OAuth";
    statusBadge.textContent = authorized ? "Authorized" : "Not connected";
    statusBadge.className = `badge badge-${authorized ? "ok" : "warn"}`;
    verifyBtn.classList.toggle("hidden", !authorized);
    disconnectBtn.classList.toggle("hidden", !authorized);
  };
  syncButtons();

  connectBtn.onclick = async () => {
    if (!server) {
      toast("MCP server name is missing", "error");
      return;
    }
    connectBtn.disabled = true;
    try {
      const { startRes, isHTTPS, isLocal } = await startMCPOAuth(server, { explicitRedirectURI: data.redirect_uri });
      activeState = startRes.state || "";
      openLink.href = startRes.authorize_url;
      linkRow.classList.remove("hidden");
      if (!isHTTPS && !isLocal && !data.redirect_uri) {
        manualBox.classList.remove("hidden");
        detailLine.textContent = `Opened sign-in with Desktop loopback redirect. After signing in, paste the redirected URL (or code) below — or run: ${cliCmd}`;
      } else {
        detailLine.textContent = "Complete sign-in in the opened browser tab; status will update automatically.";
      }
    } catch (err) {
      toast(err.message, "error");
    } finally {
      connectBtn.disabled = false;
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

  card.append(
    el("div", { className: "actions-row mt-1" }, connectBtn, pasteToggleBtn, verifyBtn, disconnectBtn),
    linkRow,
    manualBox,
    cliCmd ? el("div", { className: "small muted mono mt-1", text: `CLI alternative: ${cliCmd}` }) : null,
  );
  return card;
}
