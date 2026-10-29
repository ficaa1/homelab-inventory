// Command mcpbridge serves generic inventory GET tools over MCP stdio.
// MCP_API_URL is required. API_TOKEN or API_TOKEN_FILE supplies the
// bearer token without putting credentials in argv.
package main

import (
	"context"
	"log"
	"os"

	"github.com/ficaa1/homelab-inventory/internal/mcpbridge"
)

func main() {
	baseURL := os.Getenv("MCP_API_URL")
	if baseURL == "" {
		log.Fatal("mcpbridge: MCP_API_URL is required (e.g. http://localhost:8080)")
	}
	// Logs go to stderr; stdout is reserved for the JSON-RPC protocol.
	log.SetOutput(os.Stderr)

	token, err := mcpbridge.EnvToken()
	if err != nil {
		log.Fatal("mcpbridge: ", err)
	}
	srv := mcpbridge.New(baseURL, token, os.Stdin, os.Stdout)
	if err := srv.Run(context.Background()); err != nil {
		log.Fatalf("mcpbridge: %v", err)
	}
}
