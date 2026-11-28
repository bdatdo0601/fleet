package cli

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/bdatdo0601/fleet/internal/assistant"
	"github.com/bdatdo0601/fleet/internal/client"
)

func cmdAssistantMCPAuth(args []string, cio *IO) error {
	fs := flag.NewFlagSet("assistant mcp-auth", flag.ContinueOnError)
	port := fs.Int("port", 0, "local loopback callback port (0 = ephemeral)")
	redirectURI := fs.String("redirect-uri", "", "explicit OAuth redirect URI")
	noOpen := fs.Bool("no-open", false, "do not automatically open browser")
	manual := fs.Bool("manual", false, "paste callback URL or authorization code manually")
	revoke := fs.Bool("revoke", false, "revoke stored OAuth tokens for the MCP server")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 || strings.TrimSpace(pos[0]) == "" {
		return errors.New("usage: fleet assistant mcp-auth [--port N] [--redirect-uri URL] [--no-open] [--manual] [--revoke] <server-name>")
	}
	srvName := strings.ToLower(strings.TrimSpace(pos[0]))
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	if *revoke {
		if err := client.Call(api, http.MethodDelete, "/v1/assistant/mcp/"+url.PathEscape(srvName)+"/oauth", nil, 15*time.Second, nil, nil); err != nil {
			return err
		}
		cio.info(fmt.Sprintf("revoked OAuth tokens for MCP server %q", srvName))
		return nil
	}
	if *manual {
		return runManualMCPOAuth(api, srvName, *redirectURI, *noOpen, cio)
	}
	return runLoopbackMCPOAuth(api, srvName, *port, *redirectURI, *noOpen, cio)
}

func startMCPOAuthFlow(api, srvName, redir string) (assistant.MCPOAuthStartResult, error) {
	var out assistant.MCPOAuthStartResult
	err := client.Call(api, http.MethodPost, "/v1/assistant/mcp/"+url.PathEscape(srvName)+"/oauth/start", map[string]string{
		"redirect_uri": redir,
	}, 15*time.Second, nil, &out)
	return out, err
}

func completeMCPOAuthFlow(api, srvName, state, code string, cio *IO) error {
	var out assistant.MCPOAuthCompleteResult
	if err := client.Call(api, http.MethodPost, "/v1/assistant/mcp/"+url.PathEscape(srvName)+"/oauth/exchange", map[string]string{
		"state": state,
		"code":  code,
	}, 30*time.Second, nil, &out); err != nil {
		return err
	}
	cio.info(fmt.Sprintf("authorized MCP server %q (scope=%q expires=%s)", out.ServerName, out.Scope, cmp.Or(out.ExpiresAt, "none")))
	if out.TestError != "" {
		cio.printf("warning: handshake test reported: %s\n", out.TestError)
		return nil
	}
	cio.info(fmt.Sprintf("discovered %d tool(s): %s", out.ToolCount, strings.Join(out.Tools, ", ")))
	return nil
}

func runManualMCPOAuth(api, srvName, explicitRedir string, noOpen bool, cio *IO) error {
	redir := cmp.Or(strings.TrimSpace(explicitRedir), "http://127.0.0.1:8765/oauth2callback")
	startRes, err := startMCPOAuthFlow(api, srvName, redir)
	if err != nil {
		return err
	}
	cio.printf("Authorize URL:\n%s\n\n", startRes.AuthorizeURL)
	if !noOpen {
		openLocalBrowser(startRes.AuthorizeURL)
	}
	cio.printf("Paste the redirected callback URL (or authorization code): ")
	line, err := cio.stdinReader().ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return err
	}
	raw := strings.TrimSpace(line)
	code := raw
	state := startRes.State
	if u, uErr := url.Parse(raw); uErr == nil && u.Scheme != "" {
		if qErr := strings.TrimSpace(u.Query().Get("error")); qErr != "" {
			return fmt.Errorf("OAuth authorization denied: %s", cmp.Or(u.Query().Get("error_description"), qErr))
		}
		if c := strings.TrimSpace(u.Query().Get("code")); c != "" {
			code = c
		}
		if s := strings.TrimSpace(u.Query().Get("state")); s != "" {
			if s != startRes.State {
				return errors.New("OAuth state mismatch")
			}
			state = s
		}
	}
	if code == "" {
		return errors.New("authorization code is required")
	}
	return completeMCPOAuthFlow(api, srvName, state, code, cio)
}

func resolveLoopbackBind(port int, explicitRedir string) (bindAddr, callbackPath, redirURL string, err error) {
	callbackPath = "/oauth2callback"
	if strings.TrimSpace(explicitRedir) != "" {
		u, pErr := url.Parse(strings.TrimSpace(explicitRedir))
		if pErr != nil || u.Scheme != "http" {
			return "", "", "", errors.New("--redirect-uri in loopback mode must use http://127.0.0.1:<port>/... (or pass --manual)")
		}
		host := u.Hostname()
		if host != "127.0.0.1" && !strings.EqualFold(host, "localhost") && host != "::1" {
			return "", "", "", errors.New("--redirect-uri in loopback mode must use a loopback host (or pass --manual)")
		}
		p := u.Port()
		if p == "" {
			p = fmt.Sprintf("%d", port)
		}
		if u.Path != "" {
			callbackPath = u.Path
		}
		return net.JoinHostPort(host, p), callbackPath, u.String(), nil
	}
	return fmt.Sprintf("127.0.0.1:%d", port), callbackPath, "", nil
}

func runLoopbackMCPOAuth(api, srvName string, port int, explicitRedir string, noOpen bool, cio *IO) error {
	bindAddr, cbPath, redirURL, err := resolveLoopbackBind(port, explicitRedir)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return fmt.Errorf("listen loopback callback on %s: %w", bindAddr, err)
	}
	defer func() { _ = ln.Close() }()
	if redirURL == "" {
		tcpAddr, ok := ln.Addr().(*net.TCPAddr)
		if !ok {
			return errors.New("unexpected loopback listener address")
		}
		redirURL = fmt.Sprintf("http://127.0.0.1:%d%s", tcpAddr.Port, cbPath)
	}
	startRes, err := startMCPOAuthFlow(api, srvName, redirURL)
	if err != nil {
		return err
	}

	type cbPayload struct {
		code string
		err  error
	}
	ch := make(chan cbPayload, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(cbPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if qErr := strings.TrimSpace(q.Get("error")); qErr != "" {
			msg := cmp.Or(strings.TrimSpace(q.Get("error_description")), qErr)
			writeLoopbackHTML(w, http.StatusBadRequest, "Authorization Failed", msg)
			select {
			case ch <- cbPayload{err: fmt.Errorf("OAuth error: %s", msg)}:
			default:
			}
			return
		}
		if q.Get("state") != startRes.State {
			writeLoopbackHTML(w, http.StatusBadRequest, "Authorization Failed", "Invalid OAuth state")
			select {
			case ch <- cbPayload{err: errors.New("OAuth state mismatch")}:
			default:
			}
			return
		}
		code := strings.TrimSpace(q.Get("code"))
		if code == "" {
			writeLoopbackHTML(w, http.StatusBadRequest, "Authorization Failed", "Missing authorization code")
			select {
			case ch <- cbPayload{err: errors.New("missing OAuth authorization code")}:
			default:
			}
			return
		}
		writeLoopbackHTML(w, http.StatusOK, "Authorization Received", "You can close this tab and return to your terminal.")
		select {
		case ch <- cbPayload{code: code}:
		default:
		}
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		_ = srv.Serve(ln)
	}()
	defer func() {
		sCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sCtx)
	}()

	cio.printf("Opening browser for %s OAuth sign-in:\n%s\n", srvName, startRes.AuthorizeURL)
	if !noOpen {
		openLocalBrowser(startRes.AuthorizeURL)
	}
	cio.printf("Waiting for callback on %s ...\n", redirURL)

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()

	var code string
	select {
	case res := <-ch:
		if res.err != nil {
			return res.err
		}
		code = res.code
	case <-sigCtx.Done():
		return errors.New("OAuth authorization cancelled")
	case <-timer.C:
		return errors.New("timed out waiting for OAuth callback (5m)")
	}
	return completeMCPOAuthFlow(api, srvName, startRes.State, code, cio)
}

func writeLoopbackHTML(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<!doctype html><html><body style=\"font-family:sans-serif;max-width:480px;margin:40px auto\"><h2>%s</h2><p>%s</p></body></html>",
		html.EscapeString(title), html.EscapeString(msg))
}

func openLocalBrowser(targetURL string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", targetURL)
	case "linux":
		cmd = exec.Command("xdg-open", targetURL)
	default:
		return
	}
	_ = cmd.Start()
}
