// Standalone voice gateway: bridges WebRTC voice sessions to the configured backend.

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
	"time"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"

	"github.com/caic-xyz/caic/backend/internal/httplog"
	"github.com/caic-xyz/caic/gomode/voicegateway"
	"github.com/caic-xyz/caic/gomode/voicegateway/voicertc"
)

func mainImpl(args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	flags := flag.NewFlagSet("voice-gateway", flag.ContinueOnError)
	configPath := flags.String("config", envDefault("VOICE_GATEWAY_CONFIG", voicegateway.DefaultConfigPath()), "voice gateway config.toml path")
	addr := flags.String("http", envDefault("VOICE_GATEWAY_HTTP", ""), "HTTP listen address for signaling")
	udpPort := flags.String("udp-port", envDefault("VOICE_GATEWAY_WEBRTC_UDP_PORT", ""), "UDP port for WebRTC ICE")
	logLevel := flags.String("log-level", envDefault("VOICE_GATEWAY_LOG_LEVEL", "info"), "log level (debug, info, warn, error)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	initLogging(*logLevel)

	cfg, err := voicegateway.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *addr != "" {
		cfg.Server.HTTP = *addr
	}
	if *udpPort != "" {
		port, err := strconv.Atoi(*udpPort)
		if err != nil {
			return fmt.Errorf("parse udp port: %w", err)
		}
		cfg.Server.WebRTCUDPPort = port
	}
	if err := validateStandaloneConfig(&cfg); err != nil {
		return err
	}

	geminiAPIKey := os.Getenv("GEMINI_API_KEY")
	var bridge *voicertc.Bridge
	if cfg.Backend == voicegateway.BackendGeminiLive && geminiAPIKey == "" {
		slog.WarnContext(ctx, "voice media disabled", "reason", "GEMINI_API_KEY is not configured")
	} else {
		bridge, err = voicertc.NewBridge(ctx, &cfg, geminiAPIKey, cfg.Server.WebRTCUDPPort)
		if err != nil {
			return err
		}
		defer bridge.CloseAll()
	}

	handler, err := voicegateway.NewHandler(&cfg, bridge)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.Server.HTTP,
		Handler:           httplog.Handler{Handler: handler},
		ReadHeaderTimeout: 10 * time.Second,
	}

	shutdownBase := context.WithoutCancel(ctx)
	go func() {
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(shutdownBase, 5*time.Second)
		_ = srv.Shutdown(shutCtx)
		shutCancel()
	}()

	slog.InfoContext(ctx, "voice-gateway", "http", cfg.Server.HTTP, "udp", cfg.Server.WebRTCUDPPort, "config", *configPath)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func validateStandaloneConfig(cfg *voicegateway.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.Server.WebRTCUDPPort < 0 {
		return errors.New("server.webrtc_udp_port cannot be -1 for standalone voice-gateway")
	}
	return nil
}

func initLogging(level string) {
	ll := &slog.LevelVar{}
	switch level {
	case "debug":
		ll.Set(slog.LevelDebug)
	case "warn":
		ll.Set(slog.LevelWarn)
	case "error":
		ll.Set(slog.LevelError)
	}
	slog.SetDefault(slog.New(tint.NewHandler(colorable.NewColorable(os.Stderr), &tint.Options{
		Level:      ll,
		TimeFormat: "15:04:05.000",
		NoColor:    !isatty.IsTerminal(os.Stderr.Fd()),
	})))
}

func envDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func main() {
	if err := mainImpl(os.Args[1:]); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "voice-gateway: %v\n", err)
		os.Exit(1)
	}
}
