// Package relay embeds the maintained Python relay used for writable task logs inside containers.
package relay

import _ "embed"

// ScriptV2 is the maintained Python relay with canonical agent framing.
// V2 identifies the relay implementation, not the physical task-log format.
// Relay and task-log versions are independent. This relay supports task-log
// formats v2 and v3. Implementation size does not define a version boundary.
//
// Compatibility has three independent boundaries:
//   - Physical task logs: preserve supported record bytes, field meanings, and
//     replay semantics. An incompatible format needs a new log version and
//     independent data types. Keep historical readers; declare write and
//     continuation support separately. This version belongs to agent.LogVersion,
//     not to the Python implementation.
//   - Live relay protocol: preserve framing, control messages, byte offsets,
//     attach, MCP routing, and shutdown across backend restarts. A backend must
//     adopt supported existing daemons without replacing them. An incompatible
//     change needs an explicit protocol discriminator and compatibility check
//     before attachment, plus an adapter or a declared unsupported boundary.
//     The current protocol has no separate negotiated version; a filename does
//     not provide that check.
//   - Launch CLI: caic deploys the embedded script before starting a new daemon.
//     Update CLI arguments and callers together. Such coordinated changes need
//     no protocol or log version bump when live adoption and replay stay valid.
//     Existing daemons attach through their deployed script, not a new launch.
//
// Freeze another implementation only when a supported historical contract
// requires it. Test historical log reading and backend adoption of an existing
// daemon, not only fresh startup. Preserve supported harness configuration
// formats and task isolation regardless of which boundary changes.
//
//go:embed relay_v2.py
var ScriptV2 []byte
