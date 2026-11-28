# UI rules

These rules apply to every dashboard change: `internal/webui/index.html`, plugin pages, the
declarative view renderer and any new view type. Plugins get most of them for free because the
dashboard renders their views. A plugin still owns its copy and its tones.

Breakpoint: phones are `max-width: 640px`. Check every change at 375px and 1440px, in both themes,
and with reduced motion on. Name what still needs a real phone (gestures, the software keyboard,
safe areas) in the PR.

## Phones

- Wide tables become list rows: a main line, a sub line, a badge and a chevron. In a plugin
  `table`, the first column is the main line, the first right-aligned column after it is the
  trailing value, and the other columns join into the sub line. The trailing value carries its
  column label as a caption. In the sub line, numbers and Yes/No get their column label, and
  empty (`—`) cells are left out. Put the identifying value first and keep the columns short.
- Drawers are bottom sheets. Open them with `openDrawer`. Don't build a separate overlay.
- Admin uses grouped inset rows, with switches that carry `role="switch"`. Help text stays
  attached to its field.
- Filter selects become segmented controls that stay in sync with the underlying select across
  polls.

## Touch

- Hover styles go only under `@media (hover: hover) and (pointer: fine)`.
- Use `:active` for press feedback and `touch-action: manipulation` on controls.
- Inputs are 16px on coarse pointers, so iOS doesn't zoom into them. Never disable zoom.
- Use `dvh` units and set `overscroll-behavior` on scroll containers that are sheets or the root.
- Use `user-select: none` only on controls (tabs, the sheet handle, segments), never on content.

## Colour and status

- Colour is never the only signal. Status always has text or a glyph too (`GLYPH`, `TONE_WORD`).
  In plugin views, a tone always comes with a word in the value, badge or meta: "Drift", not
  only a warn tone.
- Use the status tokens, which pass AA: light `--warn #8e5100`, `--ok #116f40`, `--crit #b62822`;
  dark `--crit #ff8179`. Don't add literal status colours.
- Dim rows with token colours, not `opacity`.

## Type

- Use the `rem` scale, with a 12px floor.
- Heading sizes descend by level.
- Numbers that change on refresh use `font-variant-numeric: tabular-nums`.
- A clipped value can always be read in full (a `title`, an expanding row or a wrap).

## Copy

- Destructive confirmations name the object and the consequence: "Remove source *x*? Its skills
  are uninstalled on every host at their next sync." not "Are you sure?".
- Errors say how to fix the input: "Use a URL that starts with https://", not "Invalid URL".
- Terms stay consistent across views, actions, alerts and docs. Pick one word for a thing and keep
  it.
- On phones, confirmations focus Cancel first, have no Done button, and give destructive buttons
  the critical fill (`.btn.primary.destructive`). Use `confirmDrawer`, not `window.confirm`.
- A successful action shows the shared `toast()` above the tab bar and names what changed
  ("Trial ended for *x*"). Failures stay inline, next to the control.

## Focus and refresh

- Every modal goes through the shared inert and focus-trap helper (`setModal` and `modalLayers`).
  Focus returns to the opener when it closes, found again by row key after a refresh.
- A failed refresh shows a persistent critical toast with Retry. It is announced once per failure
  streak and cleared by the next success.
- Polls never replay entrance animations. A refresh keeps scroll, focus and open disclosures.

## Motion

- Motion has a purpose: it shows where something came from or that something changed. No
  decorative motion, no count-ups.
- Enters take 140ms (`--dur-1`) and exits are immediate or shorter.
- Animate `transform` and `opacity`, not layout properties (`width`, `height`, `top`). Use
  `scaleX` for progress.
- Respect `prefers-reduced-motion`: sheets, disclosures and pull-to-refresh become instant.
