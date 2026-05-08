# Development

Dependencies to build locally from scratch including the frontend:

- Go
- brotli
- make
- node
- pnpm

Then run:

```
make build
```

## Profiling

Enable profiling by adding a `[debug]` section to `~/.config/caic/config.toml`:

```toml
[debug]
# Expose /debug/pprof/* endpoints (CPU, heap, goroutines, trace).
pprof = true

# Write a CPU profile to file on shutdown.
cpuprofile = "~/.cache/caic/cpu.prof"

# Write an execution trace to file on shutdown.
trace = "~/.cache/caic/trace.out"

# Write a heap profile to file on shutdown.
memprofile = "~/.cache/caic/mem.prof"
```

Analyze the output:

```
go tool pprof -http=:7010 ~/.cache/caic/cpu.prof
go tool trace -http=:7010 ~/.cache/caic/trace.out
```

The execution trace captures named tasks and regions for each server startup
phase: repo discovery, log loading, container listing, runner initialization,
and per-container adoption (with sub-regions for relay status checks, relay
output reads, and message loading). Start the server, let it initialize, stop
it with Ctrl-C, then open the trace viewer to see where time is spent.
