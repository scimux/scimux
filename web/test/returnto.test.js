/* Tests for web/js/returnto.js — the contextual return control (item 18).
 *
 * HIG idiom: the navigation bar's leading back button labelled with its
 * *destination* (the system "‹ Messages" pill). It names where the tap lands,
 * it lives at the leading edge, and it disappears the moment the navigation
 * stack stops being valid. These tests pin exactly those three properties. */
import test from "node:test";
import assert from "node:assert/strict";
import {
  RETURN_MAP,
  RETURN_SEARCH,
  RETURN_NOTE,
  RETURN_LABEL_MAX,
  DEFAULT_BACK_LABEL,
  makeReturnContext,
  returnLabel,
  chatBackState,
  returnPillState,
  returnAfterSelection,
} from "../js/returnto.js";

/* ---------- makeReturnContext ---------- */

test("makeReturnContext carries the origin kind and its restore payload", () => {
  const ctx = makeReturnContext(RETURN_MAP, { nodeId: "n1", stopTime: "2026-08-01T10:00:00Z" });
  assert.equal(ctx.kind, "map");
  assert.equal(ctx.nodeId, "n1");
  assert.equal(ctx.stopTime, "2026-08-01T10:00:00Z");
});

test("makeReturnContext rejects an unknown origin — no silent half-context", () => {
  assert.equal(makeReturnContext("elsewhere", {}), null);
  assert.equal(makeReturnContext("", {}), null);
  assert.equal(makeReturnContext(null), null);
});

test("makeReturnContext tolerates a missing payload", () => {
  const ctx = makeReturnContext(RETURN_SEARCH);
  assert.equal(ctx.kind, "search");
});

/* ---------- returnLabel: name the destination ---------- */

test("returnLabel names the destination, never a generic Back", () => {
  assert.equal(returnLabel(makeReturnContext(RETURN_MAP)), "Map");
  assert.equal(returnLabel(makeReturnContext(RETURN_SEARCH)), "Search");
});

test("returnLabel uses the note's own title when it is short enough", () => {
  const ctx = makeReturnContext(RETURN_NOTE, { noteId: "s1", title: "Fare design" });
  assert.equal(returnLabel(ctx), "Fare design");
});

test("returnLabel falls back to the generic noun when the title would overflow", () => {
  const long = "x".repeat(RETURN_LABEL_MAX + 1);
  const ctx = makeReturnContext(RETURN_NOTE, { noteId: "s1", title: long });
  assert.equal(returnLabel(ctx), "Note");
});

test("returnLabel falls back to the generic noun for an untitled note", () => {
  assert.equal(returnLabel(makeReturnContext(RETURN_NOTE, { noteId: "s1" })), "Note");
  assert.equal(returnLabel(makeReturnContext(RETURN_NOTE, { noteId: "s1", title: "   " })), "Note");
});

test("returnLabel of no context is the plain Activities parent", () => {
  assert.equal(returnLabel(null), DEFAULT_BACK_LABEL);
});

/* ---------- chatBackState: the one decision behind #chatback ---------- */

test("chatBackState with no context is the ordinary Activities back button", () => {
  const st = chatBackState(null);
  assert.equal(st.hasReturn, false);
  assert.equal(st.label, "Activities");
  assert.match(st.html, /^&#8249;/);          // leading chevron, HIG back button
  assert.match(st.ariaLabel, /Activities/);
});

/* A bare "‹ Map" next to the chat title was too quiet to find (review 2,
   item 3): the control has to say what it does, in a framed pill. */
test("chatBackState with a context spells out the return and marks itself", () => {
  const st = chatBackState(makeReturnContext(RETURN_MAP));
  assert.equal(st.hasReturn, true);
  assert.equal(st.label, "Map");
  assert.equal(st.html, "&#8249; Return to Map");
  assert.match(st.ariaLabel, /return to Map/i);
});

test("a note origin is quoted, because the label is a name the user gave it", () => {
  const st = chatBackState(makeReturnContext(RETURN_NOTE, { title: "Fare plan" }));
  assert.equal(st.label, "Fare plan");
  assert.equal(st.html, "&#8249; Return to &#8220;Fare plan&#8221;");
});

test("a generic origin is not quoted: Map and Search are UI places, not names", () => {
  assert.ok(!chatBackState(makeReturnContext(RETURN_SEARCH)).html.includes("&#8220;"));
});

test("chatBackState escapes a note title so it cannot inject markup", () => {
  const st = chatBackState(makeReturnContext(RETURN_NOTE, { title: "a<b>&c" }));
  assert.ok(!st.html.includes("<b>"), `raw markup leaked into the back button: ${st.html}`);
  assert.ok(st.html.includes("&lt;b&gt;"), `title must be escaped: ${st.html}`);
  assert.ok(st.html.includes("&amp;c"), `ampersand must be escaped: ${st.html}`);
});

/* ---------- returnPillState: the same idiom on a pushed modal ---------- */

/* HIG's plain rule — a back button is titled with the previous screen — applies
   cleanly here, unlike on #chatback. The chat's pill has to shout "Return to
   Search" because it crosses a real distance: the user is in a different screen
   than the one they left, with the chat's own title beside it. The preview never
   left: it is drawn in the search overlay's own panel, at the same width and the
   same offset, so from the seat the results simply became a chat. One level up
   is the results, and the shortest true name for them is what the pill says. */
test("a pushed modal names the screen underneath it, plainly", () => {
  const st = returnPillState(makeReturnContext(RETURN_SEARCH));
  assert.equal(st.hasReturn, true);
  assert.equal(st.label, "Results");
  assert.match(st.html, /^&#8249; Results$/);
  assert.equal(st.ariaLabel, "back to results");
});

test("the modal pill is shorter than the chat's — different distances, different words", () => {
  const ctx = makeReturnContext(RETURN_SEARCH);
  assert.match(chatBackState(ctx).html, /Return to Search/);
  assert.ok(!returnPillState(ctx).html.includes("Return to"),
    "a one-level dismissal inside the same panel does not need the verb");
});

test("a note-presented modal names the note generically, not the note's own title", () => {
  /* The preview shows a *chat*. Titling its back button with the note's name
     would put two document names side by side in one head and make the user
     work out which one they are reading. */
  const st = returnPillState(makeReturnContext(RETURN_NOTE, { title: "Fare study" }));
  assert.equal(st.label, "Note");
  assert.match(st.html, /^&#8249; Note$/);
});

test("with no presenter to name, a pushed modal shows no return control at all", () => {
  /* #chatback falls back to its parent pane ("‹ Activities"); a modal has no
     parent pane, and offering one would send the user somewhere they never
     came from. */
  const st = returnPillState(null);
  assert.equal(st.hasReturn, false);
  assert.equal(st.html, "");
  assert.ok(!st.html.includes(DEFAULT_BACK_LABEL));
});

/* ---------- returnAfterSelection: the invalidation rule ---------- */

test("a jump keeps the return context alive", () => {
  const ctx = makeReturnContext(RETURN_SEARCH, { query: "fare" });
  assert.deepEqual(returnAfterSelection(ctx, "jump"), ctx);
});

test("selecting a chat any other way drops the context — the trajectory deviated", () => {
  const ctx = makeReturnContext(RETURN_MAP);
  assert.equal(returnAfterSelection(ctx, "card"), null);
  assert.equal(returnAfterSelection(ctx, "flag"), null);
  assert.equal(returnAfterSelection(ctx, "new"), null);
  assert.equal(returnAfterSelection(ctx, ""), null);
  assert.equal(returnAfterSelection(ctx, undefined), null);
});

test("returnAfterSelection on an already-empty context stays empty", () => {
  assert.equal(returnAfterSelection(null, "jump"), null);
});

/* ---------- P5a: a dock open is not a jump ---------- */

/* The wall map's "Open chat" used to always remember RETURN_MAP, so #chatback
   re-appeared as "‹ Return to Map" (#chatback.hasreturn beats .backbtn's
   desktop display:none). Under the dock the map never left the screen, so that
   control is a no-op that points at what you are already looking at. The
   context is documented as surviving "a jump and nothing else" — opening into
   the dock is not a jump. */
import * as returnto from "../js/returnto.js";

test("P5a: a wall jump remembers the map only when the map actually left", () => {
  assert.equal(returnto.mapJumpReturnKind({ how: "jump", docked: false }), RETURN_MAP);
  assert.equal(returnto.mapJumpReturnKind({ how: "jump", docked: true }), null,
    "docked: the map is still on screen, so there is nothing to return to");
});

test("P5a: only a jump is remembered — other selections never are", () => {
  for (const how of ["card", "flag", "new", "", undefined]){
    assert.equal(returnto.mapJumpReturnKind({ how, docked: false }), null,
      `how=${String(how)} is not a jump`);
    assert.equal(returnto.mapJumpReturnKind({ how, docked: true }), null,
      `how=${String(how)} is not a jump (docked)`);
  }
});

test("P5a: mapJumpReturnKind tolerates a missing argument", () => {
  assert.equal(returnto.mapJumpReturnKind(), null);
});
