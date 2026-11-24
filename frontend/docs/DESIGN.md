# A calm engineering workspace

caic is a precise, compact tool for managing coding agents. The interface
supports sustained work, not chatbot conversation or dashboard decoration.

## Brand

- Write the name as `caic`, lowercase.
- Use “Coding Agents in Containers” as the descriptor.
- Keep the voice capable, direct, and restrained. Avoid promotional language,
  mascots, and AI sparkle imagery.
- Let the wordmark carry the identity. Keep the workspace visually quiet.

## Visual direction

- Use neutral surfaces with blue undertones. Blue identifies primary actions,
  links, and selection. Status colors identify task and service states; green
  means success, so the primary hue never uses it.
- Give the task transcript and its fallback prompt an untinted, near-white
  reading surface in light mode. Badges, tabs, graphs, and menus keep their
  own surface colors inside the task detail view.
- Establish hierarchy through spacing, alignment, typography, and subtle
  separators before adding cards, borders, or shadows.
- Diagrams use flat fills with distinct node and group surfaces. A faint, tight
  shadow may separate node shapes from their background. Keep labels and
  connectors crisp; do not filter the whole diagram or add decorative gradients.
- Use Material Design icons for familiar actions. Keep action buttons
  borderless and transparent, with subtle hover feedback and visible keyboard
  focus. The task-summary collapse and task-detail close controls are the
  visual references. Use 20px icons, 4px padding, and a 28×28px minimum action
  button size on desktop and mobile. Text actions grow to fit their labels.
  Keep accessible names and enough spacing to avoid accidental activation.
- Use visible labels when an icon would be ambiguous.
- Keep desktop information compact, not cramped. Preserve readable content
  and comfortable touch targets on mobile.
- Use system sans-serif for the interface and monospace for paths, code, and
  identifiers.
- Use motion to explain a change or acknowledge an action. Do not compete
  with agent output.

## Interaction character

- Make the task and its output the main content. Keep chrome subordinate.
- Give familiar actions consistent behavior across panes.
- Close (×) dismisses a task pane and returns to its conversation, even from
  full screen. Full-screen exit restores the pane. Browser Back and Forward
  traverse navigation history, including links between documents.
- Make state, errors, and recovery explicit.
- Preserve the user's place, draft, and focus.

## Apply incrementally

Use this direction for new and touched components. It does not require a
navigation redesign, wholesale restyling, or UX migration. Keep the existing
light and dark semantic token boundaries. Implementation rules live in
[the frontend guidelines](../AGENTS.md#styling-and-theming).

Judge changes by whether someone can monitor several agents for hours without
fighting the interface.
