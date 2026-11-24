// Package render formats Nebula configuration files from embedded templates and registry settings.
package render

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/templates"
)

var templateForKind = map[string]string{
	"lighthouse": "lighthouse.yml.tmpl",
	"mac":        "node.yml.tmpl",
}

var placeholderRE = regexp.MustCompile(`@[A-Z_]+@`)

// BlocklistYAML formats revoked certificate fingerprints as a quoted YAML list.
func BlocklistYAML(fingerprints []string) string {
	if len(fingerprints) == 0 {
		return "  blocklist: []"
	}
	var b strings.Builder
	b.WriteString("  blocklist:")
	for _, fp := range fingerprints {
		// Quoted: an all-digit fingerprint would otherwise parse as a YAML integer.
		b.WriteString("\n    - \"")
		b.WriteString(fp)
		b.WriteByte('"')
	}
	return b.String()
}

// Render builds a Nebula config for kind ("lighthouse" or "mac") using embedded templates.
func Render(kind string, settings map[string]string, blocklist []string, pkiDir string) (string, error) {
	tmpl, ok := templateForKind[kind]
	if !ok {
		return "", fmt.Errorf("no Nebula config for kind %q", kind)
	}
	raw, err := templates.FS.ReadFile(tmpl)
	if err != nil {
		return "", fmt.Errorf("read template %s: %w", tmpl, err)
	}
	if pkiDir == "" {
		pkiDir = paths.NebulaEtc()
	}
	lanIP := settings["lighthouse_lan_ip"]
	if lanIP == "" {
		lanIP = settings["lighthouse_host"]
	}
	replacements := []string{
		"@NEBULA_ETC@", pkiDir,
		"@LIGHTHOUSE_IP@", paths.LighthouseIP,
		"@LIGHTHOUSE_HOST@", settings["lighthouse_host"],
		"@LIGHTHOUSE_PORT@", settings["lighthouse_port"],
		"@LIGHTHOUSE_LAN_IP@", lanIP,
		"@API_PORT@", strconv.Itoa(paths.APIPort()),
		"@BLOCKLIST@", BlocklistYAML(blocklist),
	}
	text := strings.NewReplacer(replacements...).Replace(string(raw))
	if leftovers := placeholderRE.FindAllString(text, -1); len(leftovers) > 0 {
		return "", fmt.Errorf("unrendered placeholders in %s: %v", tmpl, leftovers)
	}
	return text, nil
}
