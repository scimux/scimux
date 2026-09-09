# Attention and input

Part of the scimux invariant set (`AGENTS.md`), split out because it binds
only once you are inside the code it governs. **Read this before touching
liveness, attention or needs-input detection, `internal/dialoghint`,
`handlePeek`, the inspect/Dismiss surface, `SendKey`, or any polled render
region.** These rules draw one line over and over: evidence is structural,
pane text may only corroborate, and no heuristic may type a key.

## Liveness is mechanical only

Liveness states (active/quiet/exited/unavailable) come from pane-change
detection. Never add regexes matching agent TUI strings. The
inspect/owing/quiet-fallback path is for non-Claude agents: an owned Claude
node never raises inspect from quietness, AX, owing or transcript faults.
- Needs-input detection follows the same rule: an unresolved tool call in
  the transcript (structured data — both CLIs log the call when the agent
  asks and the result only after the human answers) plus a mechanically
  quiet pane (running tools animate a timer; approval dialogs are static).
  Pane text never creates attention on its own.
- The fenced exceptions are the `internal/dialoghint` matchers, which only
  *corroborate or classify*: on a quiet pane as the regex fallback, and on
  an active pane **only** when the transcript already shows an unresolved
  call *and* successive capture diffs stay confined to a few stable lines
  (`noteAnim` — diff geometry, still mechanical). That second path exists
  because a dialog with parallel calls queued behind it animates the queued
  spinner indefinitely, so quietness never arrives. If the matcher goes dark
  on a TUI rewording, the same confined-animation state with a stalled
  transcript degrades to neutral `inspect` after `animStallAfter`, never a
  classified dialog. For an owned Claude launch the escalation notice
  supersedes this whole active-pane chain.
- A quiet-branch backstop covers Claude's late tool_use flush (the call
  record is not on disk until approval, so `WaitingOn` stays false for the
  whole wait): when the newest recognized record is a **user** turn — human
  prompt or tool result, i.e. the agent owes the next output — and the pane
  has been static past `owedStallAfter`, raise neutral `inspect`. Turn role
  plus pane quietness only; it never feeds liveness.
- One narrow use of pane text rides on top: `dialoghint.HasCancelAnchor`
  ("esc to cancel") shortens that wait to `owedStallCorroborated`. It only
  sharpens the timing of a verdict the mechanical evidence already reached —
  it cannot raise attention alone and cannot change the kind, because the
  phrase also occurs in ordinary agent prose. The `esc to interrupt` working
  footer is the inverse, suppression-only hint: it may suppress neutral
  `inspect`, but never feeds liveness, creates attention, retires a hook
  notice or overrides a classified dialog.
- Claude's explicit interrupted-message record is a completed turn boundary
  even though Claude writes it with role `user`; it clears Owing/pending and
  releases the next-prompt gate.
- `handlePeek` runs the same quiet-branch predicate one-shot when a human
  opens the terminal view, so a late-flush dialog is visible without an
  unresolved call in the transcript. It is deliberately *not* quiet-gated —
  that is the point — so the predicate enforces the static-pane precondition
  itself (`paneQuietAfter`): on an active pane only the corroborated path
  may raise.

## Neutral inspect is acknowledgeable, not actionable

A fresh `inspect`
may unfold the terminal once. Non-AX inspect shows the default action bar
(digits, y/n, arrows, Enter, Escape) plus a distinct **Dismiss** that never
sends a key: Escape stays a remote key so a misclassified dialog can be
backed out of, while Dismiss is the local "I looked — all good"
acknowledgement. **AX inspect is Dismiss-only**, because `tmuxKeySequence`
expands `"1"` to `["1","Enter"]` and a keypad on a spurious inspect would
submit a prompt. The acknowledgement hides the terminal and action row for
that exact evidence epoch and returns only when fresh evidence changes
`attention_at`, the attention kind changes, or attention clears and is later
raised again. Ordinary approval/question/dialog buttons still send their
whitelisted keys and keep only the short stale-poll suppression.

## Remote keys are a whitelist

`SendKey` accepts only the dialog keys
(digits, y/n, arrows, Tab, Enter, Escape) — it answers prompts, it is not a
keystroke injector. Every key pressed via the API is recorded in the store
with the pane's bottom lines as decision evidence.

## Polling must never clobber user input

The web UI re-renders a region
only when its state signature changes; the composer is a singleton outside
all render regions (drafts persist per node); and any editable element
living *inside* a polled region must survive the rebuild with value, focus
and cursor intact (`withCardEditsPreserved` for the card editors,
build-once + `dataset.node` guards for the chat-head editors). Any new
polled UI element must respect this.
