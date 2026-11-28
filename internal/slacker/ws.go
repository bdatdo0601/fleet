package slacker

import "github.com/bdatdo0601/fleet/internal/ws"

// WSConn is an alias to the shared stdlib WebSocket connection in internal/ws.
type WSConn = ws.WSConn

// DialWS connects to a ws:// or wss:// endpoint.
var DialWS = ws.DialWS

var wsAcceptHash = ws.AcceptHash
