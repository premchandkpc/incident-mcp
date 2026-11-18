// Command incident-mcp runs the Production Incident Investigator MCP server:
// a read-only, stdio, newline-delimited JSON-RPC 2.0 server built on the Go
// standard library only.
//
// Configuration comes from the environment:
//
//	PROMETHEUS_URL          https://prometheus.internal
//	KUBERNETES_URL          https://kubernetes.default.svc
//	KUBERNETES_TOKEN        scoped ServiceAccount token (never logged)
//	ALLOWED_NAMESPACE       production
//	ALLOWED_SERVICE_PREFIX  alerts-
//	MAX_WINDOW_MINUTES      60
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"incident-mcp/internal/backend"
	"incident-mcp/internal/config"
	"incident-mcp/internal/mcp"
	"incident-mcp/internal/tools"
)

func main() {
	// All logs go to stderr; stdout carries only JSON-RPC responses.
	logger := log.New(os.Stderr, "incident-mcp ", log.LstdFlags|log.Lmsgprefix)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("startup failed: %v", err)
	}

	prom, err := backend.NewPrometheus(cfg.PrometheusURL, nil, logger)
	if err != nil {
		logger.Fatalf("startup failed: %v", err)
	}
	kube, err := backend.NewKubernetes(cfg.KubernetesURL, cfg.KubernetesToken, nil, logger)
	if err != nil {
		logger.Fatalf("startup failed: %v", err)
	}

	registry := tools.NewRegistry(cfg, prom, kube, logger)
	server := mcp.New(cfg, registry, logger)

	logger.Printf("starting %s v%s (stdio, read-only)", mcp.ServerName, mcp.Version)
	logger.Printf("scope namespace=%s service_prefix=%q max_window_minutes=%d",
		cfg.AllowedNamespace, cfg.AllowedServicePrefix, cfg.MaxWindowMinutes)
	// Endpoints are logged; the bearer token is described but never printed.
	logger.Printf("backends prometheus=%s kubernetes=%s token=%s",
		prom.URL(), kube.URL(), config.TokenStatus(cfg.KubernetesToken))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		logger.Fatalf("serve failed: %v", err)
	}
	logger.Print("stopped")
}
