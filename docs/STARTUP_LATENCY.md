# Container Startup Latency

## Measured phases

Timing is logged at `slog.Debug` (per-phase) and `slog.Info` (totals) under the
`startup` message key. Run with `-v` to see phase breakdown.

| Phase | Typical range | Notes |
|---|---|---|
| `image_check_ok` | 200–800 ms | `docker manifest inspect` network call |
| `image_build` | 10–60 s | Only when base image or SSH keys change |
| `docker_run` | 100–400 ms | `docker run -d` returns after container is created |
| `inspect_ports` | 50–200 ms | Two `docker inspect` calls for SSH port + creation time |
| `ssh_wait` | 500 ms–3 s | Polling loop; container must boot sshd |
| `git_init` | 50–200 ms | One SSH round-trip |
| `git_push_branch` | 200 ms–5 s | Scales with repo size |
| `git_switch_base` | 50–150 ms | One SSH round-trip (batched) |
| `push_submodules` | 0–10 s | Only when submodules exist |
| `set_origin` | 50–200 ms | `resolveDefaults` + `git remote add` |
| `sync_default_branch` | 200 ms–4 s | Second `git push` for main/master |
| **`run_container_total`** | **1.5–15 s** | docker_run through sync_default_branch |
| **`container_start_total`** | **2–16 s** | image_check through run_container_total |

## Improvement opportunities

### 1. Overlap branch allocation with container SSH boot

**Impact: 500 ms–2 s**

In `caic`, `setup()` runs branch allocation (git fetch + create) before calling
`Container.Start`. Branch allocation only needs to complete before the `git push`
step, not before `docker run`. The container's SSH boot time (~1–3 s) is
currently dead time.

Required change: split `Container.Start` into two phases:

```
Phase A (no branch needed):  prepare + image check/build + docker run
Phase B (branch needed):     wait for SSH + git push + SyncDefaultBranch
```

`caic` would call Phase A immediately, allocate the branch concurrently, then
call Phase B once both are done.

### 2. Parallelize the two git pushes

**Impact: 200 ms–4 s**

`git_push_branch` and `sync_default_branch` are independent `git push` calls to
the same container. Both are currently sequential. Running them with `errgroup`
would eliminate whichever is slower from the critical path.

The two pushes go to different refs (`refs/heads/<branch>` vs
`refs/heads/main`), so they do not conflict.

### 3. Reduce SSH polling interval

**Impact: 50–200 ms**

`docker.go` sleeps 100 ms between SSH probes. Containers often respond within
500–600 ms of `docker run`. Switching to a shorter initial interval (e.g. 10 ms)
with linear or exponential backoff to 100 ms cap would reduce the average wait.

```go
// current
time.Sleep(100 * time.Millisecond)

// proposed: start at 10 ms, double each miss up to 100 ms cap
sleep = min(sleep*2, 100*time.Millisecond)
time.Sleep(sleep)
```

### 4. Cache the remote image digest check

**Impact: 200–800 ms per start**

`imageBuildNeeded` calls `docker manifest inspect -v <image>` (a registry
network request) on every `Container.Start` to detect whether the base image
has been updated. In the common case the image has not changed.

A simple in-process cache keyed on `(imageName, baseImage)` with a short TTL
(e.g. 5 minutes, or until the process exits) would skip this round-trip for
every container start after the first.

### 5. Drop `--tags` from git push

**Impact: 100 ms–2 s for tag-heavy repos**

Both the task branch push and the extra-repo pushes include `--tags`. Tags are
not used by agents or by `md diff`/`md pull`. Removing `--tags` reduces
transfer size for repos with many tags.

Affected lines: `docker.go` pushes of `c.primary().Branch` and extra repos.

### 6. Merge `git init` SSH call into the switch command

**Impact: 50–100 ms**

Currently two sequential SSH connections:
1. `git init -q ~/src/<repo>`
2. `git switch -q <branch> && git branch -f base ...`

These can be one:
```bash
git init -q ~/src/<repo> && cd ~/src/<repo> && git switch -q <branch> && ...
```

Saves one TCP handshake + SSH key negotiation round-trip.

## Priority order

1. **Overlap branch alloc + SSH wait** — requires md API change, high value
2. **Parallel git pushes** — self-contained change in `runContainer`, high value
3. **SSH poll backoff** — trivial change, immediate improvement
4. **Remote digest cache** — moderate change, improves every cold start
5. **Drop `--tags`** — one-line change, low risk
6. **Merge git init SSH call** — one-line change, low risk
