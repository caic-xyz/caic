// Standalone voice gateway: bridges WebRTC voice sessions to Gemini Live.

package main

import (
	"context"
	"encoding/json"
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

	"github.com/caic-xyz/caic/backend/internal/server/voicertc"
	"github.com/caic-xyz/caic/backend/internal/voicegateway"
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

	geminiAPIKey := cfg.GeminiAPIKey()
	if geminiAPIKey == "" {
		return fmt.Errorf("%s is required", voicegateway.GeminiAPIKeyEnv)
	}
	bridge, err := voicertc.NewBridge(ctx, geminiAPIKey, cfg.Server.WebRTCUDPPort)
	if err != nil {
		return err
	}
	defer bridge.CloseAll()

	srv := &http.Server{
		Addr:              cfg.Server.HTTP,
		Handler:           gatewayHandler(&cfg, bridge),
		ReadHeaderTimeout: 10 * time.Second,
	}

	shutdownBase := context.WithoutCancel(ctx)
	go func() {
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(shutdownBase, 5*time.Second)
		_ = srv.Shutdown(shutCtx)
		shutCancel()
	}()

	slog.Info("voice-gateway", "http", cfg.Server.HTTP, "udp", cfg.Server.WebRTCUDPPort, "config", *configPath)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func gatewayHandler(cfg *voicegateway.Config, bridge *voicertc.Bridge) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /compat", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, cfg.Compatibility())
	})
	mux.HandleFunc("POST /offer", handleOffer(bridge))
	mux.HandleFunc("POST /sessions/{sessionID}", handleClose(bridge))
	return mux
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

type offerReq struct {
	SDP string `json:"sdp"`
}

func handleOffer(bridge *voicertc.Bridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if bridge == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "voice bridge unavailable"})
			return
		}
		var req offerReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		if req.SDP == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sdp is required"})
			return
		}
		sdpAnswer, sessionID, err := bridge.HandleOffer(r.Context(), req.SDP)
		if err != nil {
			slog.Error("offer failed", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "offer failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"sdp":       sdpAnswer,
			"sessionID": sessionID,
		})
	}
}

func handleClose(bridge *voicertc.Bridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if bridge == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "voice bridge unavailable"})
			return
		}
		sessionID := r.PathValue("sessionID")
		if sessionID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sessionID is required"})
			return
		}
		bridge.Close(sessionID)
		writeJSON(w, http.StatusOK, map[string]string{"status": "closed"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("json encode", "err", err)
	}
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
