// Tests specialized image refresh selection, serialization, and status isolation.

package server

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/caic-xyz/md"
	"golang.org/x/sync/errgroup"

	"github.com/caic-xyz/caic/backend/internal/preferences"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

type imageWarmerFake struct {
	opts    chan md.WarmupOpts
	release chan error
}

func (f *imageWarmerFake) Warmup(ctx context.Context, _, _ io.Writer, opts *md.WarmupOpts) (bool, error) {
	f.opts <- *opts
	select {
	case err := <-f.release:
		return true, err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func TestImageRefresh(t *testing.T) {
	t.Parallel()
	prefs, err := preferences.Open(filepath.Join(t.TempDir(), "preferences.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := prefs.Update("alice", func(p *preferences.Preferences) {
		p.Settings.BaseImage = "example.com/agent:v1"
		p.Settings.RuntimeName = "podman"
		p.Settings.RuntimeSettings = map[string]preferences.RuntimeSettings{
			"docker": {ContainerPlatform: md.PlatformLinuxARM64},
			"podman": {ContainerPlatform: md.PlatformLinuxAMD64},
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := prefs.Update("bob", func(p *preferences.Preferences) {
		p.Settings.RuntimeName = "podman"
	}); err != nil {
		t.Fatal(err)
	}
	docker := &imageWarmerFake{opts: make(chan md.WarmupOpts, 1), release: make(chan error, 1)}
	podman := &imageWarmerFake{opts: make(chan md.WarmupOpts, 1), release: make(chan error, 1)}
	t.Cleanup(func() {
		close(docker.release)
		close(podman.release)
	})
	r := &ImageRefresh{
		Log: testLogger(), Prefs: prefs,
		Clients: map[string]ImageWarmer{"docker": docker, "podman": podman},
	}
	status, err := r.Start(t.Context(), "alice", "docker")
	if err != nil || status.State != v1.ImageRefreshRunning {
		t.Fatalf("Start = %+v, %v", status, err)
	}
	opts := <-docker.opts
	if opts.BaseImage != "example.com/agent:v1" || opts.Platform != md.PlatformLinuxARM64.String() || !opts.Force {
		t.Errorf("Warmup options = %+v", opts)
	}
	if _, err := r.Start(t.Context(), "bob", "docker"); !errors.Is(err, errImageRefreshBusy) {
		t.Errorf("concurrent Start error = %v, want busy", err)
	}
	if got, err := r.Status("bob", "docker"); err != nil || got.State != "" {
		t.Errorf("other user's status = %+v, want empty", got)
	}
	if got, err := r.Status("alice", "podman"); err != nil || got.State != "" {
		t.Errorf("other runtime's status = %+v, %v, want empty", got, err)
	}
	if _, err := r.Start(t.Context(), "bob", "podman"); err != nil {
		t.Fatalf("other runtime Start: %v", err)
	}
	<-podman.opts
	docker.release <- nil
	podman.release <- errors.New("podman build failed")
	deadline := time.After(time.Second)
	for {
		dockerStatus, err := r.Status("alice", "docker")
		if err != nil {
			t.Fatal(err)
		}
		podmanStatus, err := r.Status("bob", "podman")
		if err != nil {
			t.Fatal(err)
		}
		if dockerStatus.State == v1.ImageRefreshSucceeded && podmanStatus.State == v1.ImageRefreshFailed {
			if podmanStatus.Error != "podman build failed" {
				t.Errorf("podman error = %q", podmanStatus.Error)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("image refreshes did not complete")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if _, err := r.Start(t.Context(), "alice", "missing"); err == nil {
		t.Error("unknown runtime refresh succeeded")
	}
	t.Run("Scheduled", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		docker := &imageWarmerFake{opts: make(chan md.WarmupOpts, 1), release: make(chan error, 1)}
		podman := &imageWarmerFake{opts: make(chan md.WarmupOpts, 1), release: make(chan error, 1)}
		scheduler := &ImageRefresh{
			Log: testLogger(), Prefs: prefs,
			Clients: map[string]ImageWarmer{"docker": docker, "podman": podman},
		}
		group, runCtx := errgroup.WithContext(ctx)
		t.Cleanup(func() { cancel(); _ = group.Wait() })
		group.Go(func() error { return scheduler.RunScheduled(runCtx, time.Hour) })
		for _, ch := range []<-chan md.WarmupOpts{docker.opts, podman.opts} {
			select {
			case opts := <-ch:
				if opts.Force || opts.BaseImage != md.DefaultBaseImage+":latest" {
					t.Errorf("scheduled Warmup options = %+v", opts)
				}
			case <-time.After(time.Second):
				t.Fatal("runtimes did not warm concurrently")
			}
		}
		if status, err := scheduler.Status("alice", "docker"); err != nil || !status.Scheduled || status.State != v1.ImageRefreshRunning {
			t.Errorf("scheduled status = %+v, %v", status, err)
		}
		if _, err := scheduler.Start(ctx, "alice", "docker"); !errors.Is(err, errImageRefreshBusy) {
			t.Errorf("manual refresh during warmup = %v, want busy", err)
		}
		cancel()
		docker.release <- nil
		podman.release <- nil
		if err := group.Wait(); err != nil {
			t.Errorf("RunScheduled after cancellation = %v", err)
		}
	})
}

func TestImageRefreshShutdown(t *testing.T) {
	t.Parallel()
	prefs, err := preferences.Open(filepath.Join(t.TempDir(), "preferences.json"))
	if err != nil {
		t.Fatal(err)
	}
	warmer := &imageWarmerFake{opts: make(chan md.WarmupOpts, 1), release: make(chan error)}
	r := &ImageRefresh{Log: testLogger(), Prefs: prefs, Clients: map[string]ImageWarmer{"docker": warmer}}
	ctx, cancel := context.WithCancel(t.Context())
	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error { return r.Run(ctx) })
	if _, err := r.Start(t.Context(), "alice", "docker"); err != nil {
		cancel()
		t.Fatal(err)
	}
	<-warmer.opts
	cancel()
	if err := group.Wait(); err != nil {
		t.Fatal(err)
	}
	status, err := r.Status("alice", "docker")
	if err != nil || status.State != v1.ImageRefreshFailed {
		t.Fatalf("status after shutdown = %+v, %v", status, err)
	}
	if _, err := r.Start(t.Context(), "alice", "docker"); !errors.Is(err, context.Canceled) {
		t.Fatalf("start after shutdown = %v, want cancellation", err)
	}
}
