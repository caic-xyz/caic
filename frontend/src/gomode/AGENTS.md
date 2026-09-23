# Go Mode Frontend Integration

`gomode/` contains generic integration between a hosted frontend and the Go
Mode shell. It owns host-mode detection, narrow native bridge capabilities,
and browser implementations of shell features such as voice transport and
notifications.

Do not add caic product-domain behavior here. In particular, task DTO imports,
task ordering or numbering, task UI, and task-specific voice semantics belong
in the frontend root or a caic product-domain directory.

Keep the native bridge capability-oriented and versioned. It may expose shell
state such as a connected voice session, but must not proxy normal product APIs
or expose product-specific task data.

Caic task-number synchronization and live task voice updates live in the
product-owned frontend root, not the generic voice transport.
