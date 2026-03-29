// Standalone WebRTC relay: authenticates users via shared JWT secret, bridges WebRTC to Gemini Live.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/server/voicertc"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
)

func mainImpl() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	addr := flag.String("http", envDefault("CAIC_RELAY_HTTP", ":3479"), "HTTP listen address for signaling")
	udpPort := flag.Int("udp-port", envDefaultInt("CAIC_WEBRTC_PORT", 3478), "UDP port for WebRTC ICE")
	logLevel := flag.String("log-level", envDefault("CAIC_LOG_LEVEL", "info"), "log level (debug, info, warn, error)")
	configDir := flag.String("config-dir", defaultConfigDir(), "caic config directory (shares settings.json with caic server)")
	flag.Parse()

	initLogging(*logLevel)

	geminiAPIKey := os.Getenv("GEMINI_API_KEY")
	if geminiAPIKey == "" {
		return errors.New("GEMINI_API_KEY is required")
	}

	// Read session secret from caic's settings.json to validate JWTs issued by the main server.
	secret, err := readSessionSecret(filepath.Join(*configDir, "settings.json"))
	if err != nil {
		return fmt.Errorf("session secret: %w", err)
	}

	// Auth store for user lookup.
	store, err := auth.Open(filepath.Join(*configDir, "users.json"))
	if err != nil {
		return fmt.Errorf("open users store: %w", err)
	}

	bridge, err := voicertc.NewBridge(geminiAPIKey, *udpPort)
	if err != nil {
		return err
	}
	defer bridge.CloseAll()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("POST /offer", handleOffer(bridge))
	mux.HandleFunc("DELETE /sessions/{sessionID}", handleClose(bridge))

	// Wrap with auth middleware: validate JWT, require authenticated user.
	handler := auth.RequireUser(auth.Middleware(store, secret)(mux))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		// Use Background because the parent ctx is already cancelled.
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(shutCtx) //nolint:contextcheck // parent ctx is already cancelled at shutdown time
		shutCancel()
	}()

	slog.Info("webrtc-relay", "http", *addr, "udp", *udpPort)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type settingsFile struct {
	SessionSecret string `json:"sessionSecret"`
}

type offerReq struct {
	SDP string `json:"sdp"`
}

func handleOffer(bridge *voicertc.Bridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
	_ = json.NewEncoder(w).Encode(v)
}

// readSessionSecret reads the hex-encoded session secret from caic's settings.json.
func readSessionSecret(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: trusted config path
	if err != nil {
		return nil, fmt.Errorf("read %s: %w (run caic server once to generate it)", path, err)
	}
	var s settingsFile
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.SessionSecret == "" {
		return nil, fmt.Errorf("%s has no sessionSecret (run caic server once to generate it)", path)
	}
	secret, err := hex.DecodeString(s.SessionSecret)
	if err != nil {
		return nil, fmt.Errorf("decode sessionSecret: %w", err)
	}
	return secret, nil
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

func envDefaultInt(name string, def int) int {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func defaultConfigDir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "caic")
}

func main() {
	if err := mainImpl(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "webrtc-relay: %v\n", err)
		os.Exit(1)
	}
}
