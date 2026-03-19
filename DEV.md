# Development

Dependencies to build locally from scratch including the frontend:

- Go
- brotli
- make
- node
- pnpm

Optional, for WebRTC voice (Opus codec via CGo):

- libopus-dev (`apt install libopus-dev` / `brew install opus`)

The Makefile auto-detects libopus via `pkg-config` and sets `CGO_ENABLED`
accordingly. Without it, WebRTC falls back to data-channel-only passthrough
(no RTP audio). Override with `CGO_ENABLED=1 make build` to force CGo.

Then run:

```
make build
```
