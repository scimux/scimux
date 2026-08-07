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

test("chatBackState with a context names the origin and marks itself a return", () => {
  const st = chatBackState(makeReturnContext(RETURN_MAP));
  assert.equal(st.hasReturn, true);
  assert.equal(st.label, "Map");
  assert.equal(st.html, "&#8249; Map");
  assert.match(st.ariaLabel, /back to Map/i);
});

test("chatBackState escapes a note title so it cannot inject markup", () => {
  const st = chatBackState(makeReturnContext(RETURN_NOTE, { title: "a<b>&c" }));
  assert.ok(!st.html.includes("<b>"), `raw markup leaked into the back button: ${st.html}`);
  assert.ok(st.html.includes("&lt;b&gt;"), `title must be escaped: ${st.html}`);
  assert.ok(st.html.includes("&amp;c"), `ampersand must be escaped: ${st.html}`);
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
