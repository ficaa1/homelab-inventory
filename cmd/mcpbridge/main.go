// Command mcpbridge serves fixed inventory tools over MCP stdio.
// MCP_API_URL is required. API or MCP-prefixed token variables supply the
// bearer; OPERATOR_TOKEN authorizes exec separately. Tokens may use private files.
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
	operator, err := mcpbridge.EnvOperatorToken()
	if err != nil {
		log.Fatal("mcpbridge: ", err)
	}
	srv := mcpbridge.New(baseURL, token, os.Stdin, os.Stdout)
	srv.OperatorToken = operator
	if err := srv.Run(context.Background()); err != nil {
		log.Fatalf("mcpbridge: %v", err)
	}
}
