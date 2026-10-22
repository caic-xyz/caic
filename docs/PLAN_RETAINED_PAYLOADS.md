# Reduce retained CI, diff, and image payloads

Bound the payloads kept in agent prompts and browser state, and remove avoidable
conversion buffers. Preserve usable excerpts, full-log access, task isolation,
and existing prompt and history compatibility.

## Phase 1 — image-drafts: Keep draft attachments as binary values

- **Scope:** File/Blob draft ownership and object-URL previews, early per-image and aggregate size checks, bounded conversion during submission, and SSE-owned task membership with complete snapshots for draft cleanup.
- **Preserve:** Existing image submission format, supported attachment sources, per-task drafts, retry retention, authorization boundaries, and historical image rendering.
- **Verify:** Oversized and multi-image selections, successful and failed submission, removal and disposal, object-URL cleanup, and delayed snapshots across local mutations and server restarts; before/after conversion and draft-retention measurements.

## Later

- Drop the inline base64 payload from the user-input event once attachment URLs
  are the only render path.
- Deduplicate the image copies a harness echoes into the task log.
- Cap prompt images at each harness's documented vision limit before sending
  them to a model. The limit differs per harness and is not yet verified.
- A bounded diff preview or paged patch API is needed to limit the peak cost of loading one giant patch.
