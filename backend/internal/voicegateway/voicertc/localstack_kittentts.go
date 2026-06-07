// KittenTTS process adapter for local-stack speech synthesis.

package voicertc

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

//go:embed kittentts.py
var kittenTTSWorkerScript string

const (
	kittenTTSModel      = "KittenML/kitten-tts-mini-0.8"
	kittenTTSPackage    = "kittentts @ git+https://github.com/KittenML/KittenTTS@9f3e0d8b6600b56ebe1b4d7b6d8e1e020077d1f2"
	kittenTTSPython     = "3.12"
	kittenTTSVoice      = "Jasper"
	kittenTTSSpeed      = 1.0
	kittenTTSReadyKind  = "ready"
	kittenTTSStdoutName = "stdout"
	kittenTTSSynthesize = "/synthesize"
)

// kittenTTSAdapter synthesizes speech through a long-lived Python worker.
type kittenTTSAdapter struct {
	mu sync.Mutex

	start         kittenTTSCommandFactory
	client        *http.Client
	baseURL       string
	processCancel context.CancelFunc
	cmd           *exec.Cmd
	stdout        *bufio.Reader
	wait          chan error
}

type kittenTTSCommandFactory func(context.Context) (*exec.Cmd, error)

type kittenTTSReady struct {
	Kind   string   `json:"kind"`
	URL    string   `json:"url"`
	Voices []string `json:"voices"`
	Error  string   `json:"error"`
}

type kittenTTSRequest struct {
	Text  string  `json:"text"`
	Voice string  `json:"voice"`
	Speed float64 `json:"speed"`
}

type kittenTTSResponse struct {
	Error string `json:"error"`
}

func newKittenTTSAdapter(ctx context.Context) (*kittenTTSAdapter, error) {
	return newKittenTTSAdapterWithCommand(ctx, kittenTTSCommand)
}

func newKittenTTSAdapterWithCommand(ctx context.Context, start kittenTTSCommandFactory) (*kittenTTSAdapter, error) {
	if start == nil {
		return nil, errors.New("KittenTTS command factory is required")
	}
	a := &kittenTTSAdapter{start: start, client: http.DefaultClient}
	if err := a.ensureStartedLocked(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *kittenTTSAdapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.processCancel != nil {
		a.processCancel()
	}
	return a.stopLocked()
}

func (a *kittenTTSAdapter) synthesize(ctx context.Context, text string) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		if text == "" {
			return
		}
		a.mu.Lock()
		if err := a.ensureStartedLocked(ctx); err != nil {
			a.mu.Unlock()
			yield(nil, err)
			return
		}
		endpoint := a.baseURL + kittenTTSSynthesize
		client := a.client
		a.mu.Unlock()

		req := kittenTTSRequest{Text: text, Voice: kittenTTSVoice, Speed: kittenTTSSpeed}
		data, err := json.Marshal(req)
		if err != nil {
			yield(nil, fmt.Errorf("encode KittenTTS request: %w", err))
			return
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
		if err != nil {
			yield(nil, fmt.Errorf("create KittenTTS request: %w", err))
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpResp, err := client.Do(httpReq)
		if err != nil {
			if ctx.Err() != nil {
				yield(nil, ctx.Err())
				return
			}
			yield(nil, errors.Join(fmt.Errorf("post KittenTTS request: %w", err), a.stop()))
			return
		}
		defer func() {
			if err := httpResp.Body.Close(); err != nil {
				slog.WarnContext(ctx, "voicertc: close KittenTTS response body", "err", err)
			}
		}()
		if httpResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
			yield(nil, fmt.Errorf("KittenTTS HTTP status %s: %s", httpResp.Status, strings.TrimSpace(string(body))))
			return
		}
		var pending []byte
		buf := make([]byte, 32*1024)
		for {
			n, err := httpResp.Body.Read(buf)
			if n > 0 {
				pcm := make([]byte, len(pending)+n)
				copy(pcm, pending)
				copy(pcm[len(pending):], buf[:n])
				evenLen := len(pcm) - len(pcm)%2
				pending = append(pending[:0], pcm[evenLen:]...)
				if evenLen > 0 && !yield(slices.Clone(pcm[:evenLen]), nil) {
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					if len(pending) > 0 {
						yield(nil, errors.New("KittenTTS returned odd PCM byte count"))
					}
					return
				}
				yield(nil, fmt.Errorf("read KittenTTS PCM stream: %w", err))
				return
			}
		}
	}
}

func (a *kittenTTSAdapter) ensureStartedLocked(ctx context.Context) error {
	if a.cmd != nil {
		return nil
	}
	processCtx, cancel := context.WithCancel(ctx)
	cmd, err := a.start(processCtx)
	if err != nil {
		cancel()
		return err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("open KittenTTS stdout: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("open KittenTTS stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start KittenTTS worker: %w", err)
	}
	a.processCancel = cancel
	a.cmd = cmd
	a.stdout = bufio.NewReader(stdoutPipe)
	a.wait = make(chan error, 1)
	go func() {
		a.wait <- cmd.Wait()
	}()
	go logKittenTTSOutput(ctx, "stderr", stderrPipe)

	line, err := a.readLineLocked(ctx, kittenTTSStdoutName)
	if err != nil {
		return errors.Join(err, a.stopLocked())
	}
	var ready kittenTTSReady
	if err := json.Unmarshal(line, &ready); err != nil {
		return errors.Join(fmt.Errorf("decode KittenTTS ready message: %w", err), a.stopLocked())
	}
	if ready.Error != "" {
		return errors.Join(fmt.Errorf("KittenTTS startup: %s", ready.Error), a.stopLocked())
	}
	if ready.Kind != kittenTTSReadyKind {
		return errors.Join(fmt.Errorf("KittenTTS startup returned kind %q, want %q", ready.Kind, kittenTTSReadyKind), a.stopLocked())
	}
	if err := validateKittenTTSURL(ready.URL); err != nil {
		return errors.Join(err, a.stopLocked())
	}
	a.baseURL = strings.TrimRight(ready.URL, "/")
	slog.InfoContext(ctx, "voicertc: KittenTTS ready", "model", kittenTTSModel, "url", a.baseURL, "voices", ready.Voices)
	go logKittenTTSOutput(ctx, "stdout", a.stdout)
	return nil
}

func (a *kittenTTSAdapter) readLineLocked(ctx context.Context, name string) ([]byte, error) {
	type readResult struct {
		line []byte
		err  error
	}
	done := make(chan readResult, 1)
	go func() {
		line, err := a.stdout.ReadBytes('\n')
		done <- readResult{line: line, err: err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			if errors.Is(r.err, io.EOF) {
				return nil, a.processExitErrorLocked("read KittenTTS " + name)
			}
			return nil, fmt.Errorf("read KittenTTS %s: %w", name, r.err)
		}
		return r.line, nil
	case <-ctx.Done():
		return nil, errors.Join(ctx.Err(), a.stopLocked())
	}
}

func (a *kittenTTSAdapter) processExitErrorLocked(op string) error {
	select {
	case err := <-a.wait:
		a.wait = nil
		if err != nil {
			return fmt.Errorf("%s: worker exited: %w", op, err)
		}
		return fmt.Errorf("%s: worker exited", op)
	default:
		return fmt.Errorf("%s: worker closed pipe", op)
	}
}

func (a *kittenTTSAdapter) stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stopLocked()
}

func (a *kittenTTSAdapter) stopLocked() error {
	var errs []error
	if a.cmd != nil && a.cmd.Process != nil {
		if err := a.cmd.Process.Kill(); err != nil && !strings.Contains(err.Error(), "process already finished") {
			errs = append(errs, err)
		}
	}
	if a.wait != nil {
		if err := <-a.wait; err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "signal: killed") {
			errs = append(errs, err)
		}
	}
	if a.processCancel != nil {
		a.processCancel()
	}
	a.processCancel = nil
	a.cmd = nil
	a.baseURL = ""
	a.stdout = nil
	a.wait = nil
	return errors.Join(errs...)
}

func validateKittenTTSURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse KittenTTS URL: %w", err)
	}
	if u.Scheme != "http" || u.Host == "" {
		return fmt.Errorf("KittenTTS URL must be an http URL, got %q", rawURL)
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "localhost" {
		return fmt.Errorf("KittenTTS URL must be loopback, got %q", rawURL)
	}
	return nil
}

func kittenTTSCommand(ctx context.Context) (*exec.Cmd, error) {
	cache, err := localStackKittenTTSCacheDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cache, 0o750); err != nil {
		return nil, fmt.Errorf("create KittenTTS cache dir: %w", err)
	}
	args := []string{
		"run",
		"--isolated",
		"--python", kittenTTSPython,
		"--with", kittenTTSPackage,
		"python", "-u", "-c", kittenTTSWorkerScript,
		"--cache-dir", cache,
	}
	cmd := exec.CommandContext(ctx, "uv", args...) //nolint:gosec // command and arguments are fixed by the gateway.
	return cmd, nil
}

func localStackKittenTTSCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("get user cache dir: %w", err)
	}
	return filepath.Join(base, "caic", "kittentts"), nil
}

func logKittenTTSOutput(ctx context.Context, stream string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			slog.InfoContext(ctx, "voicertc: KittenTTS", "stream", stream, "line", line)
		}
	}
	if err := scanner.Err(); err != nil {
		if strings.Contains(err.Error(), "file already closed") {
			return
		}
		slog.WarnContext(ctx, "voicertc: read KittenTTS output", "stream", stream, "err", err)
	}
}
