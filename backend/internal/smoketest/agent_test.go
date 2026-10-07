// Tests selected fake session metadata and capture work that remains active until shutdown.

package smoketest

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

func TestFakeBackend(t *testing.T) {
	t.Setenv("CAIC_E2E_VISUALS", "1")
	t.Run("Start", func(t *testing.T) {
		for _, tc := range []struct {
			harness harness.Name
			model   string
			effort  string
			version string
		}{
			{harness.Codex, "gpt-6.1-sol", "high", ""},
			{harness.Claude, "opus-5.5", "high", "2.1.185"},
			{harness.Pi, "deepseek/deepseek-flashh", "medium", ""},
			{harness.Antigravity, "gemini-3.8-flash-high", "", ""},
		} {
			for _, selected := range []bool{true, false} {
				name := "default"
				model := ""
				if selected {
					name = "selected"
					model = tc.model
				}
				t.Run(string(tc.harness)+"/"+name, func(t *testing.T) {
					t.Parallel()
					b := NewFakeBackend()
					b.HarnessID = tc.harness
					ch := make(chan agent.TimedMessage, 8)
					s, err := b.Start(t.Context(), &agent.Options{
						Logger: slog.New(slog.DiscardHandler),
						Log:    agent.DiscardLogSink{Version: agent.LogVersionV2},
						MsgCh:  ch,
						Model:  model,
						Effort: tc.effort,
					})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := s.Close(); err != nil {
							t.Error(err)
						}
					})
					select {
					case msg := <-ch:
						init, ok := msg.Message.(*agent.InitMessage)
						if !ok {
							t.Fatalf("first message = %T, want init", msg.Message)
						}
						if init.ReportedModel != tc.model || init.ReportedEffort != tc.effort {
							t.Errorf("session model/effort = %q/%q, want %q/%q", init.ReportedModel, init.ReportedEffort, tc.model, tc.effort)
						}
						if init.Version != tc.version {
							t.Errorf("session version = %q, want %q", init.Version, tc.version)
						}
					case <-t.Context().Done():
						t.Fatal(t.Context().Err())
					}
				})
			}
		}
	})
	t.Run("CaptureRunning", func(t *testing.T) {
		for _, shutdown := range []string{"stop", "eof"} {
			t.Run(shutdown, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				t.Cleanup(cancel)
				b := NewFakeBackend()
				b.HarnessID = harness.Antigravity
				ch := make(chan agent.TimedMessage, 32)
				s, err := b.Start(ctx, &agent.Options{
					Logger:        slog.New(slog.DiscardHandler),
					Log:           agent.DiscardLogSink{Version: agent.LogVersionV2},
					MsgCh:         ch,
					InitialPrompt: agent.Prompt{Text: "Implement rate limiting for API endpoints"},
				})
				if err != nil {
					t.Fatal(err)
				}
				// Stop/EOF below closes the pipe; cleanup also covers failed assertions.
				t.Cleanup(func() { _ = s.Close() })
				var pending string
				check := func(msg agent.Message) {
					switch m := msg.(type) {
					case *agent.ToolUseMessage:
						if m.Name == "Bash" {
							pending = m.ToolUseID
						}
					case *agent.ToolResultMessage:
						if pending != "" && m.ToolUseID == pending {
							t.Error("active verification command completed before shutdown")
						}
					case *agent.ResultMessage, *agent.AskMessage:
						t.Errorf("running capture emitted terminal or subsequent-prompt message %T", msg)
					}
				}
				for pending == "" {
					select {
					case msg := <-ch:
						check(msg.Message)
					case <-s.Done():
						t.Fatal("capture session ended before active verification")
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				select {
				case <-s.Done():
					t.Fatal("capture session ended while verification was active")
				default:
				}
				// A queued prompt must not settle this scene or start another turn.
				if err := s.SendPrompt(agent.Prompt{Text: "FAKE_ASK"}); err != nil {
					t.Fatal(err)
				}
				if shutdown == "stop" {
					if err := s.Stop(ctx); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}
					if err := s.Wait(); err != nil {
						t.Fatal(err)
					}
				}
				// Wait/Stop guarantees the message reader has finished writing.
				close(ch)
				for msg := range ch {
					check(msg.Message)
				}
			})
		}
	})
}
