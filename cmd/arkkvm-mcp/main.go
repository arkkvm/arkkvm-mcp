// Command arkkvm-mcp is the Go implementation of the ArkKVM local MCP server.
// stdout is reserved for the MCP JSON-RPC transport; all logging goes to
// stderr and the optional ARKKVM_LOG_FILE.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"arkkvm-mcp/internal/config"
	"arkkvm-mcp/internal/logger"
	"arkkvm-mcp/internal/server"
)

func main() {
	log := logger.NewFromEnv()
	store := config.New()
	srv := server.New(store, log)

	logger.Info("MCP server starting", map[string]any{
		"configured": store.Configured(),
		"pid":        os.Getpid(),
		"logLevel":   envDefault("ARKKVM_LOG_LEVEL", "info"),
		"logFile":    envDefault("ARKKVM_LOG_FILE", ""),
	})
	// Self-heal the cloud device name when the startup seed (env/token file)
	// lacks it: one background device-list call records it next to the id.
	srv.StartupNameSelfHeal()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The long-lived MCP server must log async failures and keep serving
	// instead of exiting.
	go func() {
		<-ctx.Done()
		logger.Info("MCP server shutting down", map[string]any{"signal": "signal"})
		srv.CloseClient()
		os.Exit(0)
	}()

	if err := srv.Run(ctx); err != nil {
		logger.Error("MCP server failed to start", err, nil)
		srv.CloseClient()
		os.Exit(1)
	}
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
