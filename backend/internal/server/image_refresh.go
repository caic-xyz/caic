// Background refresh of specialized images and installed coding agents by runtime.

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/caic-xyz/md"

	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/runtime/mdruntime"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

var errImageRefreshBusy = errors.New("another image refresh is already running")

// ImageWarmer builds a specialized image without launching a container.
type ImageWarmer interface {
	Warmup(ctx context.Context, stdout, stderr io.Writer, opts *md.WarmupOpts) (bool, error)
}

// ImageRefresh tracks user-requested specialized image rebuilds per runtime.
type ImageRefresh struct {
	Log     *slog.Logger
	Clients map[string]ImageWarmer
	Prefs   *preferences.Store

	mu       sync.Mutex
	running  map[string]imageRefreshRun
	statuses map[imageRefreshKey]v1.ImageRefreshStatus
}

// Status returns the current or most recent refresh result for one runtime.
func (r *ImageRefresh) Status(userID, runtime string) (v1.ImageRefreshStatus, error) {
	if r.Clients[runtime] == nil {
		return v1.ImageRefreshStatus{}, fmt.Errorf("container runtime %q is unavailable", runtime)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if run, ok := r.running[runtime]; ok && run.scheduled {
		return v1.ImageRefreshStatus{State: v1.ImageRefreshRunning, Scheduled: true}, nil
	}
	return r.statuses[imageRefreshKey{userID: userID, runtime: runtime}], nil
}

// Start begins a refresh using a snapshot of the user's image preferences.
// The server context keeps the build alive after the HTTP request completes.
func (r *ImageRefresh) Start(ctx context.Context, userID, runtime string) (v1.ImageRefreshStatus, error) {
	p := r.Prefs.Get(userID)
	client := r.Clients[runtime]
	if client == nil {
		return v1.ImageRefreshStatus{}, fmt.Errorf("container runtime %q is unavailable", runtime)
	}
	caches, err := cacheMountsFromSettings(&p.Settings)
	if err != nil {
		return v1.ImageRefreshStatus{}, err
	}
	mdCaches := make([]md.CacheMount, len(caches))
	for i, c := range caches {
		mdCaches[i] = md.CacheMount{
			Name: c.Name, Description: c.Description, HostPath: c.HostPath,
			ContainerPath: c.ContainerPath, ReadOnly: c.ReadOnly, Shallow: c.Shallow,
		}
	}
	image := p.Settings.BaseImage
	if image == "" {
		image = md.DefaultBaseImage + ":latest"
	}
	platform := p.Settings.RuntimeSettings[runtime].ContainerPlatform.String()
	r.mu.Lock()
	if _, ok := r.running[runtime]; ok {
		r.mu.Unlock()
		return v1.ImageRefreshStatus{}, errImageRefreshBusy
	}
	if r.running == nil {
		r.running = make(map[string]imageRefreshRun)
	}
	run := imageRefreshRun{done: make(chan struct{})}
	r.running[runtime] = run
	if r.statuses == nil {
		r.statuses = make(map[imageRefreshKey]v1.ImageRefreshStatus)
	}
	key := imageRefreshKey{userID: userID, runtime: runtime}
	r.statuses[key] = v1.ImageRefreshStatus{State: v1.ImageRefreshRunning}
	r.mu.Unlock()
	go func() {
		w := &mdruntime.SlogWriter{Context: ctx, Logger: r.Log, Phase: "image-refresh"}
		_, buildErr := client.Warmup(ctx, w, w, &md.WarmupOpts{
			BaseImage: image, Platform: platform,
			Caches: mdCaches, Quiet: true, Force: true,
		})
		r.mu.Lock()
		delete(r.running, runtime)
		close(run.done)
		if buildErr != nil {
			r.statuses[key] = v1.ImageRefreshStatus{State: v1.ImageRefreshFailed, Error: buildErr.Error()}
		} else {
			r.statuses[key] = v1.ImageRefreshStatus{State: v1.ImageRefreshSucceeded}
		}
		r.mu.Unlock()
	}()
	return v1.ImageRefreshStatus{State: v1.ImageRefreshRunning}, nil
}

// RunScheduled checks configured images at startup and at each interval for
// every available runtime. Each runtime runs independently and shares the same
// build coordination as user-requested refreshes.
func (r *ImageRefresh) RunScheduled(ctx context.Context, interval time.Duration) error {
	var wg sync.WaitGroup
	errCh := make(chan error, len(r.Clients))
	for runtime := range r.Clients {
		wg.Go(func() {
			if err := r.runScheduledRuntime(ctx, runtime, interval); err != nil {
				errCh <- err
			}
		})
	}
	wg.Wait()
	close(errCh)
	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (r *ImageRefresh) runScheduledRuntime(ctx context.Context, runtime string, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		images := []preferences.ContainerImage{{BaseImage: md.DefaultBaseImage + ":latest"}}
		for _, img := range r.Prefs.BaseImages() {
			if !slices.Contains(images, img) {
				images = append(images, img)
			}
		}
		if err := r.warmScheduledImages(ctx, runtime, images); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return nil
		}
	}
}

func (r *ImageRefresh) warmScheduledImages(ctx context.Context, runtime string, images []preferences.ContainerImage) error {
	for {
		r.mu.Lock()
		if active, ok := r.running[runtime]; ok {
			r.mu.Unlock()
			select {
			case <-active.done:
				continue
			case <-ctx.Done():
				return nil
			}
		}
		if r.running == nil {
			r.running = make(map[string]imageRefreshRun)
		}
		run := imageRefreshRun{done: make(chan struct{}), scheduled: true}
		r.running[runtime] = run
		r.mu.Unlock()

		var buildErr error
		for _, img := range images {
			if ctx.Err() != nil {
				break
			}
			w := &mdruntime.SlogWriter{Context: ctx, Logger: r.Log, Phase: "warmup"}
			built, err := r.Clients[runtime].Warmup(ctx, w, w, &md.WarmupOpts{
				BaseImage: img.BaseImage, Platform: img.Platform, Quiet: true,
			})
			if err != nil {
				buildErr = fmt.Errorf("warmup image %s platform %s on %s: %w", img.BaseImage, img.Platform, runtime, err)
				break
			}
			if built {
				r.Log.InfoContext(ctx, "image built", "image", img.BaseImage, "platform", img.Platform, "runtime", runtime)
			}
		}
		r.mu.Lock()
		delete(r.running, runtime)
		close(run.done)
		r.mu.Unlock()
		return buildErr
	}
}

type imageRefreshRun struct {
	done      chan struct{}
	scheduled bool
}

type imageRefreshKey struct {
	userID  string
	runtime string
}
