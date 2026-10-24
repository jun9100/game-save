// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// envString reads the Game Save setting first. The former EDEN_SYNCD_* names
// remain accepted so an in-place container upgrade does not require rewriting
// secrets before the new image has proved healthy.
func envString(name, legacyName, fallback string) string {
	if value, ok := os.LookupEnv(name); ok && value != "" {
		return value
	}
	if legacyName != "" {
		if value, ok := os.LookupEnv(legacyName); ok && value != "" {
			return value
		}
	}
	return fallback
}

func envInt(name, legacyName string, fallback int) int {
	for _, candidate := range []string{name, legacyName} {
		if candidate == "" {
			continue
		}
		if value, err := strconv.Atoi(os.Getenv(candidate)); err == nil {
			return value
		}
	}
	return fallback
}

func envDuration(name, legacyName string, fallback time.Duration) time.Duration {
	for _, candidate := range []string{name, legacyName} {
		if candidate == "" {
			continue
		}
		if value, err := time.ParseDuration(os.Getenv(candidate)); err == nil {
			return value
		}
	}
	return fallback
}

// runHealthcheck lets container runtimes probe the service without a shell or HTTP client in
// the image.
func runHealthcheck(arguments []string) int {
	flags := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	target := flags.String("url", envString("GAME_SAVE_HEALTH_URL", "EDEN_SYNCD_HEALTH_URL", "http://127.0.0.1:8787/healthz"),
		"health endpoint to probe")
	_ = flags.Parse(arguments)
	client := http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(*target)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy:", response.Status)
		return 1
	}
	return 0
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck(os.Args[2:]))
	}

	dataDir := flag.String("data-dir", envString("GAME_SAVE_DATA", "EDEN_SYNCD_DATA", "./data"), "directory for objects and metadata")
	listenAddress := flag.String("listen", envString("GAME_SAVE_LISTEN", "EDEN_SYNCD_LISTEN", ":8787"), "HTTP listen address")
	token := flag.String("token", envString("GAME_SAVE_TOKEN", "EDEN_SYNCD_TOKEN", ""), "bearer token")
	adminToken := flag.String("admin-token", envString("GAME_SAVE_ADMIN_TOKEN", "EDEN_SYNCD_ADMIN_TOKEN", ""), "administrator token for the web dashboard")
	openSaveUsername := flag.String("opensave-username", envString("GAME_SAVE_OPENSAVE_USERNAME", "EDEN_SYNCD_OPENSAVE_USERNAME", ""),
		"OpenSave WebDAV username")
	openSavePassword := flag.String("opensave-password", envString("GAME_SAVE_OPENSAVE_PASSWORD", "EDEN_SYNCD_OPENSAVE_PASSWORD", ""),
		"OpenSave WebDAV password")
	maxObjectMiB := flag.Int64("max-object-mib", int64(envInt("GAME_SAVE_MAX_OBJECT_MIB", "EDEN_SYNCD_MAX_OBJECT_MIB",
		int(defaultMaxObjectBytes/(1024*1024)))), "maximum object size in MiB")
	keepSnapshots := flag.Int("keep-snapshots", envInt("GAME_SAVE_KEEP_SNAPSHOTS", "EDEN_SYNCD_KEEP_SNAPSHOTS", 0),
		"initial retention: newest snapshots kept per scope (0 = unlimited; dashboard settings take precedence)")
	keepDays := flag.Int("keep-days", envInt("GAME_SAVE_KEEP_DAYS", "EDEN_SYNCD_KEEP_DAYS", 0),
		"initial retention: keep snapshots younger than this many days (0 = unlimited)")
	maintenanceInterval := flag.Duration("maintenance-interval",
		envDuration("GAME_SAVE_MAINTENANCE_INTERVAL", "EDEN_SYNCD_MAINTENANCE_INTERVAL", 6*time.Hour),
		"how often retention and object garbage collection run (0 disables)")
	logFormat := flag.String("log-format", envString("GAME_SAVE_LOG_FORMAT", "EDEN_SYNCD_LOG_FORMAT", "text"), "log format: text or json")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	var logHandler slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if *logFormat == "json" {
		logHandler = slog.NewJSONHandler(os.Stderr, nil)
	}
	slog.SetDefault(slog.New(logHandler))

	s, err := newServerWithOptions(*dataDir, *token, *maxObjectMiB*1024*1024, serverOptions{
		adminToken:       *adminToken,
		openSaveUsername: *openSaveUsername,
		openSavePassword: *openSavePassword,
		settings: serverSettings{Retention: retentionPolicy{
			KeepSnapshots: *keepSnapshots,
			KeepDays:      *keepDays,
		}},
	})
	if err != nil {
		slog.Error("start", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *maintenanceInterval > 0 {
		go s.maintenanceLoop(ctx, *maintenanceInterval)
	}

	httpServer := &http.Server{
		Addr:              *listenAddress,
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownContext); err != nil {
			slog.Warn("shutdown", "error", err)
		}
	}()

	slog.Info("game-save listening", "address", *listenAddress, "version", version,
		"data_dir", *dataDir, "dashboard", *adminToken != "", "opensave_webdav", *openSaveUsername != "")
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("serve", "error", err)
		os.Exit(1)
	}
	slog.Info("game-save stopped")
}
