// Command server runs the firmware release registry.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"firmware-releases/internal/server"
)

func main() {
	var (
		addr        = flag.String("addr", ":"+envOr("PORT", "8080"), "listen address")
		dataDir     = flag.String("data-dir", envOr("DATA_DIR", "/data"), "directory holding published releases")
		maxUpload   = flag.Int64("max-upload", server.DefaultMaxUpload, "maximum artifact size in bytes")
		healthcheck = flag.Bool("healthcheck", false, "probe the local /healthz endpoint and exit")
	)
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck(*addr))
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv, err := server.New(*dataDir, server.WithMaxUpload(*maxUpload), server.WithLogger(log))
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("listening", "addr", *addr, "dataDir", *dataDir)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// runHealthcheck probes the in-container health endpoint so the image can
// define a HEALTHCHECK without shipping curl or wget.
func runHealthcheck(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		port = "8080"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/healthz", port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: unexpected status", resp.StatusCode)
		return 1
	}
	return 0
}
