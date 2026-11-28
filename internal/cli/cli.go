// Package cli implements the `fleet` command-line interface.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/server"
	"github.com/bdatdo0601/fleet/internal/version"
)

func usageText() string {
	return `usage: fleet <command> [options]

Commands:
  init            (lighthouse, once) create CA + registry, start services
  join            (new Mac) request to join, then paste the approval
  approve         (admin) approve a pasted join request
  approve-mobile  (admin) sign a phone/tablet key from Mobile Nebula
  ls              (admin) list devices
  revoke          (admin) revoke a device
  set             (admin) change a device's group (re-signs its cert)
  set-profile     (admin) change a device's trust profile (personal|work)
  sync            (root, launchd) pull config/certs/revocations and binary updates
  serve           (lighthouse, launchd) run the API
  edge            (lighthouse, root launchd) run HTTPS :443 proxy & private mesh DNS :53
  ddns            (lighthouse) keep the public DNS name pointed at home
  tls             (lighthouse/admin) manage Let's Encrypt HTTPS cert & private mesh DNS
  status          show this machine's fleet status
  agents          list, inspect, message, spawn, or kill AI agent sessions across the fleet
  usage           show quota and usage status across claude, codex, and antigravity
  session         manage detached Claude, Antigravity (agy), and Codex sessions
  events          list, tail, or emit authoritative Fleet events
  slacker         lighthouse Slack Socket Mode dispatcher & Mac task workers
  assistant       always-on lighthouse personal assistant, Obsidian skills & approvals
  memory          manage shared fleet agent memory and LLM curation
  mcp             manage skills, lifecycle hooks, and agent relay for claude, codex, and antigravity
  agent-hook      (internal) handle a harness lifecycle hook
  version         print fleet version
`
}

func dispatchCommand(cmd string, rest []string, cio *IO) (bool, error) {
	if cmd == "version" || cmd == "--version" || cmd == "-V" {
		if err := parseFlagsNoPositional("version", rest); err != nil {
			return true, err
		}
		cio.printf("%s\n", version.String())
		return true, nil
	}
	if cmd == "serve" {
		if err := parseFlagsNoPositional("serve", rest); err != nil {
			return true, err
		}
		return true, server.Serve(paths.APIPort(), "0.0.0.0")
	}
	handlers := map[string]func([]string, *IO) error{
		"init":           cmdInit,
		"join":           cmdJoin,
		"approve":        cmdApprove,
		"approve-mobile": cmdApproveMobile,
		"ls":             cmdLS,
		"revoke":         cmdRevoke,
		"set":            cmdSet,
		"set-profile":    cmdSetProfile,
		"sync":           cmdSync,
		"status":         cmdStatus,
		"ddns":           cmdDDNS,
		"tls":            cmdTLS,
		"edge":           cmdEdge,
		"agents":         cmdAgents,
		"events":         cmdEvents,
		"usage":          cmdUsage,
		"statusline":     cmdStatusLine,
		"session":        cmdSession,
		"slacker":        cmdSlacker,
		"assistant":      cmdAssistant,
		"memory":         cmdMemory,
		"mcp":            cmdMCP,
		"agent-hook":     cmdAgentHook,
	}
	fn, ok := handlers[cmd]
	if !ok {
		return false, nil
	}
	return true, fn(rest, cio)
}

// Run dispatches CLI args using cio and returns an exit code (0 on success).
func Run(args []string, cio *IO) int {
	if cio == nil {
		cio = defaultIO()
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, _ = io.WriteString(cio.Stdout, usageText())
		return 0
	}
	ok, err := dispatchCommand(args[0], args[1:], cio)
	if !ok {
		_, _ = fmt.Fprintf(cio.Stderr, "error: unknown command %q\n\n%s", args[0], usageText())
		return 2
	}
	if err != nil {
		if helpErr, isHelp := errors.AsType[flagHelpError](err); isHelp {
			if helpErr.name != "" {
				_, _ = fmt.Fprintf(cio.Stdout, "usage: fleet %s [options]\n", helpErr.name)
			}
			_, _ = io.WriteString(cio.Stdout, helpErr.defaults)
			return 0
		}
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(cio.Stdout, usageText())
			return 0
		}
		_, _ = fmt.Fprintf(cio.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

// Main is the binary entry point returning the process exit code.
func Main(args []string) int {
	return Run(args, defaultIO())
}
