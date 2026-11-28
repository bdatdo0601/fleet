package slacker

import (
	"encoding/json"
	"fmt"
	"strings"
)

var manifestUserScopes = []string{
	"reactions:read",
	"channels:history",
	"groups:history",
	"im:history",
	"mpim:history",
	"channels:read",
	"groups:read",
	"users:read",
	"chat:write",
	"search:read",
}

// ManifestYAML returns the Slack App Manifest YAML for Socket Mode, user reaction dispatch, and agent read/send.
func ManifestYAML() string {
	var b strings.Builder
	b.WriteString(`display_information:
  name: Fleet Slacker
  description: Dispatch Fleet agent sessions from Slack reactions and let agents read/send Slack context
settings:
  event_subscriptions:
    user_events:
      - reaction_added
  interactivity:
    is_enabled: false
  org_deploy_enabled: false
  socket_mode_enabled: true
  token_rotation_enabled: false
oauth_config:
  scopes:
    user:
`)
	for _, sc := range manifestUserScopes {
		fmt.Fprintf(&b, "      - %s\n", sc)
	}
	return b.String()
}

// ManifestJSON returns the Slack App Manifest JSON for Socket Mode, user reaction dispatch, and agent read/send.
func ManifestJSON() string {
	obj := map[string]any{
		"display_information": map[string]any{
			"name":        "Fleet Slacker",
			"description": "Dispatch Fleet agent sessions from Slack reactions and let agents read/send Slack context",
		},
		"settings": map[string]any{
			"event_subscriptions": map[string]any{
				"user_events": []string{"reaction_added"},
			},
			"interactivity":          map[string]any{"is_enabled": false},
			"org_deploy_enabled":     false,
			"socket_mode_enabled":    true,
			"token_rotation_enabled": false,
		},
		"oauth_config": map[string]any{
			"scopes": map[string]any{
				"user": manifestUserScopes,
			},
		},
	}
	b, _ := json.MarshalIndent(obj, "", "  ")
	return string(b) + "\n"
}
