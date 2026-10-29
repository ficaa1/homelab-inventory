// Package scripts embeds the installer shipped with the API.
package scripts

import _ "embed"

//go:embed install-agent.sh
var Installer string
