/* Characterization tests for web/js/notes.js — pure decisions + factory
 * adapters with a small fake DOM, controlled promises, and fake timers.
 * Does not re-test format escaping, bookmark lane/sort/clamp algorithms,
 * navigation swipe math, or search/archive. No mutable production test accessors. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  SAVE_DEBOUNCE_MS,
  STORAGE_KEY_FOLDS,
  STORAGE_KEY_INBOX_TAB,
  STORAGE_KEY_WS_LAYOUT,
  COPY_ACK_MS,
  WS_LAYOUT_MINS,
  WS_LAYOUT_PEEK,
  WS_DIVIDER_NUDGE,
  parseNoteFolds,
  foldsAfterSet,
  parseWorkspaceLayout,
  layoutAfterPlan,
  dividerResizePlan,
  wsLayoutMins,
  clampWorkspaceLayout,
  WS_INBOX_FLAT_ZONE_MIN,
  provLabel,
  sectionSwapPlan,
  noteCardReorderPlan,
  cardLanesFromDoc,
  bookmarkSource,
  buildBookmarkSnapshot,
  createSaveEnqueue,
  sectionBodyInner,
  sectionHTML,
  referenceHTML,
  noteCardHTML,
  noteCardDragEnabled,
  noteCardRenamePlan,
  noteCardKeySelects,
  inboxLaneTabs,
  resolveInboxTab,
  inboxListModel,
  inboxTabsHTML,
  inboxItemHTML,
  applyNotesSwipeAction,
  createNotesFeature,
} from "../js/notes.js";
import { bookmarkLaneId, bookmarkSortKey, bookmarkClampState } from "../js/bookmarks.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const notesSrc = readFileSync(join(__dirname, "../js/notes.js"), "utf8");
const bookmarksSrc = readFileSync(join(__dirname, "../js/bookmarks.js"), "utf8");
const tokensSrc = readFileSync(join(__dirname, "../css/tokens.css"), "utf8");
const notesCssSrc = readFileSync(join(__dirname, "../css/notes.css"), "utf8");
const menuCssSrc = readFileSync(join(__dirname, "../css/menu.css"), "utf8");
const baseCssSrc = readFileSync(join(__dirname, "../css/base.css"), "utf8");
const indexSrc = readFileSync(join(__dirname, "../index.html"), "utf8");

/* ---------- module shape ---------- */
test("notes.js exports factory and pure helpers; no later-feature imports", () => {
  assert.match(notesSrc, /export function createNotesFeature/);
  assert.match(notesSrc, /from "\.\/format\.js"/);
  assert.match(notesSrc, /from "\.\/bookmarks\.js"/);
  assert.match(notesSrc, /from "\.\/navigation\.js"/);
  assert.match(notesSrc, /from "\.\/lanes\.js"/);
  assert.doesNotMatch(notesSrc, /from "\.\/search\.js"/);
  assert.doesNotMatch(notesSrc, /from "\.\/sheets\.js"/);
  assert.doesNotMatch(notesSrc, /from "\.\/polling\.js"/);
  assert.doesNotMatch(notesSrc, /from "\.\/app\.js"/);
  assert.doesNotMatch(notesSrc, /from "\.\/chat\.js"/);
  assert.doesNotMatch(notesSrc, /from "\.\/composer\.js"/);
  assert.doesNotMatch(notesSrc, /test\/debug accessors|getWsActive|_wsNotes/);
  assert.doesNotMatch(bookmarksSrc, /from ["'].*notes/);
});

test("storage keys, debounce, copy ack constants", () => {
  assert.equal(STORAGE_KEY_FOLDS, "scimux-notefolds");
  /* item 17: the inbox does not own a tab key — it shares the Bookmarks
     pane's, so opening the workspace lands on the lane you were reading */
  assert.equal(STORAGE_KEY_INBOX_TAB, "scimux-bookmarktab");
  assert.equal(STORAGE_KEY_WS_LAYOUT, "scimux-wslayout");
  assert.equal(SAVE_DEBOUNCE_MS, 1100);
  assert.equal(COPY_ACK_MS, 900);
  assert.equal(WS_LAYOUT_PEEK, 48);
  assert.equal(WS_DIVIDER_NUDGE, 20);
  assert.equal(WS_LAYOUT_MINS.inbox, 210);
  assert.equal(WS_LAYOUT_MINS.nav, 190);
  assert.equal(WS_LAYOUT_MINS.note, 280);
});

/* ---------- Phase 1a/1b/1e: --danger token; destructive UI is red ---------- */
test("--danger token is defined in light and dark; distinct from --attn", () => {
  /* light :root block owns the first --danger; dark block redefines it. */
  const lightMatch = tokensSrc.match(/:root\s*\{([\s\S]*?)\n\}/);
  assert.ok(lightMatch, "tokens.css must declare a light :root block");
  assert.match(lightMatch[1], /--danger:\s*#FF3B30\s*;/);
  assert.match(lightMatch[1], /--attn:/);
  assert.doesNotMatch(lightMatch[1], /--danger:\s*var\(--attn\)/);

  const darkMatch = tokensSrc.match(/@media\s*\(prefers-color-scheme:\s*dark\)\s*\{\s*:root\s*\{([\s\S]*?)\n\s*\}/);
  assert.ok(darkMatch, "tokens.css must declare a dark :root block");
  assert.match(darkMatch[1], /--danger:\s*#FF453A\s*;/);
  assert.match(darkMatch[1], /--attn:/);

  /* semantic split: attention yellow must not be the destructive colour. */
  const lightAttn = lightMatch[1].match(/--attn:\s*([^;]+);/);
  const lightDanger = lightMatch[1].match(/--danger:\s*([^;]+);/);
  assert.ok(lightAttn && lightDanger);
  assert.notEqual(lightAttn[1].trim(), lightDanger[1].trim());
});

test(".popmenu button.danger uses --danger, not --attn (covers Delete section)", () => {
  /* Section menu data-mi=del uses class="danger"; note delete is on the card.
     P5: generic .popmenu rules live in menu.css; del is built via menuButtonHTML
     (attrs + danger: true) rather than an inline class="danger" string. */
  /* P5 review: keep del and danger COUPLED. Asserting the two strings exist
     independently let `danger: true` move onto "Move up" with the suite green —
     and this test's own name claims to cover Delete section. */
  assert.match(notesSrc, /data-mi="del"[^)]*danger:\s*true/);
  assert.match(menuCssSrc, /\.popmenu\s+button\.danger\s*\{[^}]*color:\s*var\(--danger\)/);
  assert.doesNotMatch(menuCssSrc, /\.popmenu\s+button\.danger\s*\{[^}]*color:\s*var\(--attn\)/);
});

test("card delete action uses --danger (not --attn)", () => {
  /* the geometry moved to the shared .actionbar/.btn-plain ladder in base.css
     (UI review items 6 + 9); danger stays a modifier on a tier, never a tier */
  assert.match(baseCssSrc, /\.btn-plain\.danger\s*\{[^}]*color:\s*var\(--danger\)/);
  assert.doesNotMatch(baseCssSrc, /\.btn-plain\.danger\s*\{[^}]*color:\s*var\(--attn\)/);
});

/* ---------- folds ---------- */
test("parseNoteFolds: empty, valid, corrupt", () => {
  assert.deepEqual(parseNoteFolds(null), {});
  assert.deepEqual(parseNoteFolds(""), {});
  assert.deepEqual(parseNoteFolds("{}"), {});
  assert.deepEqual(parseNoteFolds('{"s1":1}'), { s1: 1 });
  assert.deepEqual(parseNoteFolds("not-json"), {});
  assert.deepEqual(parseNoteFolds("null"), {});
});

test("foldsAfterSet set and clear", () => {
  assert.deepEqual(foldsAfterSet({}, "a", true), { a: 1 });
  assert.deepEqual(foldsAfterSet({ a: 1, b: 1 }, "a", false), { b: 1 });
  assert.deepEqual(foldsAfterSet({ a: 1 }, "a", true), { a: 1 });
});

/* ---------- Phase 2a: workspace layout + dividerResizePlan ---------- */

const WS_MINS = { inbox: 180, nav: 160, note: 280 };
const WS_PEEK = 48;
const WS_TOTAL = 900;
const WS_ZONES = { inbox: 240, nav: 220, note: 440 }; /* 240+220+440 = 900 */

function resizeArgs(over = {}){
  return {
    boundary: "inbox",
    pointerX: 240,
    zones: { ...WS_ZONES },
    mins: { ...WS_MINS },
    peek: WS_PEEK,
    total: WS_TOTAL,
    collapsed: { inbox: false, nav: false },
    last: { inbox: 240, nav: 220 },
    ...over,
    zones: { ...WS_ZONES, ...(over.zones || {}) },
    mins: { ...WS_MINS, ...(over.mins || {}) },
    collapsed: { inbox: false, nav: false, ...(over.collapsed || {}) },
    last: { inbox: 240, nav: 220, ...(over.last || {}) },
  };
}

function assertTotal(plan, total = WS_TOTAL){
  const w = plan.widths;
  assert.equal(w.inbox + w.nav + w.note, total);
}

/* P5b item 1: desktop inbox floor fits the flat action bar.
 *
 * Arithmetic (must match the product constants / comments):
 *   flat inbox bar = 5×44 + 4×4 gap = 236px content
 *   chrome = list pad 10+10 + border 1+1 + bar pad 6+6 = 34px
 *   zone min = 236 + 34 = 270px
 * Phone keeps 210 (overflow more-menu bar fits; notes.css clamp lower bound). */
test("P5b: wsLayoutMins raises inbox floor on desktop only (flat-row arithmetic)", () => {
  assert.equal(typeof wsLayoutMins, "function", "wsLayoutMins must be exported");
  assert.equal(typeof clampWorkspaceLayout, "function");
  assert.ok(Number.isFinite(WS_INBOX_FLAT_ZONE_MIN) && WS_INBOX_FLAT_ZONE_MIN > 210,
    "flat zone min must exceed the phone floor");
  /* derive: 5 buttons, 4 gaps, chrome from real CSS (notes.css list/bar/border) */
  const flatRow = 5 * 44 + 4 * 4;
  const chrome = 10 + 10 + 1 + 1 + 6 + 6;
  assert.equal(WS_INBOX_FLAT_ZONE_MIN, flatRow + chrome,
    "WS_INBOX_FLAT_ZONE_MIN must be row + chrome, not a bare magic 270");

  const desk = wsLayoutMins({ isDesktop: true });
  assert.equal(desk.inbox, WS_INBOX_FLAT_ZONE_MIN);
  assert.equal(desk.nav, WS_LAYOUT_MINS.nav);
  assert.equal(desk.note, WS_LAYOUT_MINS.note);

  const phone = wsLayoutMins({ isDesktop: false });
  assert.equal(phone.inbox, 210, "below 900px keep the phone/overflow floor");
  assert.equal(phone.nav, 190);
  assert.equal(phone.note, 280);
});

test("P5b: dividerResizePlan at desktop mins never yields a flat-row-clipping inbox", () => {
  const mins = wsLayoutMins({ isDesktop: true });
  /* Drag into the old 210–269 clip band: either collapse to peek, or expand
     at ≥ flat min — never land expanded in the band where Delete is clipped. */
  for (const pointerX of [100, 210, 230, 250, 269, 270, 300]){
    const plan = dividerResizePlan(resizeArgs({
      mins,
      boundary: "inbox",
      pointerX,
      zones: { inbox: 300, nav: 220, note: 380 },
      last: { inbox: 300, nav: 220 },
      total: 900,
    }));
    if (plan.collapsed.inbox){
      assert.equal(plan.widths.inbox, WS_PEEK,
        `collapsed inbox at pointerX=${pointerX} must be peek`);
    } else {
      assert.ok(plan.widths.inbox >= mins.inbox,
        `expanded inbox ${plan.widths.inbox} at pointerX=${pointerX} must be ≥ ${mins.inbox}`);
    }
  }
});

test("P5b: clampWorkspaceLayout restores a too-narrow persisted inbox on desktop", () => {
  /* iPad dragged to 210 + overflow bar, then desktop reloads the stored width */
  const mins = wsLayoutMins({ isDesktop: true });
  const clamped = clampWorkspaceLayout(
    { inboxW: 210, navW: 200, inboxCollapsed: false, navCollapsed: false },
    mins,
  );
  assert.equal(clamped.inboxW, mins.inbox,
    "restore must raise 210 → flat-row min so Delete is reachable");
  assert.equal(clamped.navW, 200);
  /* collapsed keeps stored last width (peek is applied separately) */
  const col = clampWorkspaceLayout(
    { inboxW: 210, navW: 200, inboxCollapsed: true, navCollapsed: false },
    mins,
  );
  assert.equal(col.inboxW, 210, "collapsed: do not rewrite last width via the flat floor");
  /* phone mins leave 210 alone */
  const phone = clampWorkspaceLayout(
    { inboxW: 210, navW: 200, inboxCollapsed: false, navCollapsed: false },
    wsLayoutMins({ isDesktop: false }),
  );
  assert.equal(phone.inboxW, 210);
});

test("P5b: open/apply clamps persisted too-narrow inbox on desktop (iPad→desktop)", async () => {
  const mins = wsLayoutMins({ isDesktop: true });
  const stored = {
    inboxW: 210, navW: 200, inboxCollapsed: false, navCollapsed: false,
  };
  const ctx = createFeature({
    storageInit: { [STORAGE_KEY_WS_LAYOUT]: JSON.stringify(stored) },
    isDesktop: true,
  });
  const { feature, roots, storage } = ctx;
  feature.bind();
  feature.open();
  await settle();
  const applied = parseFloat(roots.wszones.style.getPropertyValue("--wsinbox-w"));
  assert.ok(applied >= mins.inbox,
    `applied --wsinbox-w ${applied} must be ≥ ${mins.inbox} on desktop restore`);
  /* The clamp is applied, not persisted. It is idempotent, so every apply
     re-raises it; writing it back would destroy a width the user deliberately
     chose under the narrow layout, where 210 is legal. */
  const L = JSON.parse(storage.getItem(STORAGE_KEY_WS_LAYOUT));
  assert.equal(L.inboxW, 210,
    "the stored narrow-layout width must survive a desktop restore clamp");
});

test("parseWorkspaceLayout: empty, valid, corrupt → defaults", () => {
  const def = { inboxW: null, navW: null, inboxCollapsed: false, navCollapsed: false };
  assert.deepEqual(parseWorkspaceLayout(null), def);
  assert.deepEqual(parseWorkspaceLayout(""), def);
  assert.deepEqual(parseWorkspaceLayout("not-json"), def);
  assert.deepEqual(parseWorkspaceLayout("null"), def);
  assert.deepEqual(parseWorkspaceLayout("[]"), def);
  assert.deepEqual(parseWorkspaceLayout("0"), def);
  assert.deepEqual(parseWorkspaceLayout("{}"), def);
  assert.deepEqual(parseWorkspaceLayout('{"inboxW":240,"navW":200,"inboxCollapsed":true,"navCollapsed":false}'), {
    inboxW: 240, navW: 200, inboxCollapsed: true, navCollapsed: false,
  });
  /* garbage field types fall back per-field */
  assert.deepEqual(parseWorkspaceLayout('{"inboxW":"wide","navW":-3,"inboxCollapsed":1,"navCollapsed":"yes"}'), {
    inboxW: null, navW: null, inboxCollapsed: true, navCollapsed: true,
  });
});

test("layoutAfterPlan merges plan into persisted layout shape", () => {
  const prev = { inboxW: 240, navW: 220, inboxCollapsed: false, navCollapsed: false };
  const plan = {
    widths: { inbox: 48, nav: 412, note: 440 },
    collapsed: { inbox: true, nav: false },
    last: { inbox: 240, nav: 220 },
  };
  assert.deepEqual(layoutAfterPlan(prev, plan), {
    inboxW: 240, navW: 220, inboxCollapsed: true, navCollapsed: false,
  });
  /* expand nav after resize: last tracks expanded widths */
  const plan2 = {
    widths: { inbox: 240, nav: 300, note: 360 },
    collapsed: { inbox: false, nav: false },
    last: { inbox: 240, nav: 300 },
  };
  assert.deepEqual(layoutAfterPlan(prev, plan2), {
    inboxW: 240, navW: 300, inboxCollapsed: false, navCollapsed: false,
  });
  /* null plan / absent prev tolerate */
  assert.deepEqual(layoutAfterPlan(null, null), {
    inboxW: null, navW: null, inboxCollapsed: false, navCollapsed: false,
  });
});

test("dividerResizePlan: reallocate within range preserves total (inbox|nav)", () => {
  const plan = dividerResizePlan(resizeArgs({ boundary: "inbox", pointerX: 300 }));
  assert.equal(plan.widths.inbox, 300);
  assert.equal(plan.widths.nav, 160); /* 220 - 60 */
  assert.equal(plan.widths.note, 440);
  assert.deepEqual(plan.collapsed, { inbox: false, nav: false });
  assertTotal(plan);
});

test("dividerResizePlan: reallocate within range preserves total (nav|note)", () => {
  /* divider at inbox+nav = 460; pointerX is absolute from zones left */
  const plan = dividerResizePlan(resizeArgs({ boundary: "nav", pointerX: 500 }));
  /* desired nav = 500 - 240 = 260; note = 900 - 240 - 260 = 400 */
  assert.equal(plan.widths.inbox, 240);
  assert.equal(plan.widths.nav, 260);
  assert.equal(plan.widths.note, 400);
  assert.deepEqual(plan.collapsed, { inbox: false, nav: false });
  assertTotal(plan);
});

test("dividerResizePlan: drag zone below min snaps to collapsed (peek)", () => {
  const plan = dividerResizePlan(resizeArgs({ boundary: "inbox", pointerX: 100 }));
  assert.equal(plan.widths.inbox, WS_PEEK);
  assert.equal(plan.widths.nav, WS_TOTAL - WS_PEEK - 440);
  assert.equal(plan.widths.note, 440);
  assert.equal(plan.collapsed.inbox, true);
  assert.equal(plan.collapsed.nav, false);
  assert.equal(plan.last.inbox, 240); /* remembered pre-collapse width */
  assertTotal(plan);
});

test("dividerResizePlan: drag nav below min snaps to collapsed (peek)", () => {
  const plan = dividerResizePlan(resizeArgs({ boundary: "nav", pointerX: 240 + 50 }));
  assert.equal(plan.widths.inbox, 240);
  assert.equal(plan.widths.nav, WS_PEEK);
  assert.equal(plan.widths.note, WS_TOTAL - 240 - WS_PEEK);
  assert.equal(plan.collapsed.nav, true);
  assert.equal(plan.collapsed.inbox, false);
  assert.equal(plan.last.nav, 220);
  assertTotal(plan);
});

test("dividerResizePlan: inverse drag restores collapsed zone to last width", () => {
  const collapsedZones = { inbox: WS_PEEK, nav: WS_TOTAL - WS_PEEK - 440, note: 440 };
  /* pointer at last width → restore */
  const plan = dividerResizePlan(resizeArgs({
    boundary: "inbox",
    pointerX: 240,
    zones: collapsedZones,
    collapsed: { inbox: true, nav: false },
    last: { inbox: 240, nav: 220 },
  }));
  assert.equal(plan.collapsed.inbox, false);
  assert.equal(plan.widths.inbox, 240);
  assert.equal(plan.widths.nav, 220);
  assert.equal(plan.widths.note, 440);
  assertTotal(plan);
});

test("dividerResizePlan: restore nav from collapsed via inverse drag", () => {
  const collapsedZones = { inbox: 240, nav: WS_PEEK, note: WS_TOTAL - 240 - WS_PEEK };
  const plan = dividerResizePlan(resizeArgs({
    boundary: "nav",
    pointerX: 240 + 220,
    zones: collapsedZones,
    collapsed: { inbox: false, nav: true },
    last: { inbox: 240, nav: 220 },
  }));
  assert.equal(plan.collapsed.nav, false);
  assert.equal(plan.widths.nav, 220);
  assert.equal(plan.widths.inbox, 240);
  assertTotal(plan);
});

test("dividerResizePlan: zone-3 (note) never collapses and keeps ≥ min", () => {
  /* try to grow nav so note would fall below min */
  const plan = dividerResizePlan(resizeArgs({
    boundary: "nav",
    pointerX: 240 + 700, /* absurd: want nav=700 */
  }));
  assert.ok(plan.widths.note >= WS_MINS.note);
  assert.equal(plan.widths.note, WS_MINS.note);
  assert.equal(plan.widths.nav, WS_TOTAL - 240 - WS_MINS.note);
  assert.equal(plan.collapsed.nav, false); /* note never collapses; nav stays expanded at max */
  assert.equal("note" in plan.collapsed, false); /* only inbox/nav in collapsed map */
  assertTotal(plan);

  /* shrinking note via tiny pointer still floors note at min when expanded nav hits its floor…
     if nav would go below min it collapses, note takes the free width (≥ min). */
  const plan2 = dividerResizePlan(resizeArgs({
    boundary: "nav",
    pointerX: 240 + 10,
  }));
  assert.ok(plan2.widths.note >= WS_MINS.note);
  assert.equal(plan2.collapsed.nav, true);
  assert.equal(plan2.widths.nav, WS_PEEK);
  assertTotal(plan2);
});

test("dividerResizePlan: never leave both collapsible zones without a tappable peek", () => {
  /* collapse inbox first */
  const p1 = dividerResizePlan(resizeArgs({ boundary: "inbox", pointerX: 20 }));
  assert.equal(p1.collapsed.inbox, true);
  assert.equal(p1.widths.inbox, WS_PEEK);
  /* then collapse nav — both stay at peek (tappable), never width 0 */
  const p2 = dividerResizePlan(resizeArgs({
    boundary: "nav",
    pointerX: WS_PEEK + 10,
    zones: p1.widths,
    collapsed: p1.collapsed,
    last: p1.last,
  }));
  assert.equal(p2.collapsed.inbox, true);
  assert.equal(p2.collapsed.nav, true);
  assert.equal(p2.widths.inbox, WS_PEEK);
  assert.equal(p2.widths.nav, WS_PEEK);
  assert.ok(p2.widths.inbox > 0 && p2.widths.nav > 0);
  assert.ok(p2.widths.note >= WS_MINS.note);
  assertTotal(p2);

  /* trying to drive a zone to zero still yields peek */
  const p3 = dividerResizePlan(resizeArgs({
    boundary: "inbox",
    pointerX: 0,
    zones: p2.widths,
    collapsed: p2.collapsed,
    last: p2.last,
  }));
  assert.equal(p3.widths.inbox, WS_PEEK);
  assert.equal(p3.widths.nav, WS_PEEK);
  assertTotal(p3);
});

test("dividerResizePlan: expanding one collapsed zone leaves the other collapsed", () => {
  const both = {
    zones: { inbox: WS_PEEK, nav: WS_PEEK, note: WS_TOTAL - 2 * WS_PEEK },
    collapsed: { inbox: true, nav: true },
    last: { inbox: 240, nav: 220 },
  };
  const plan = dividerResizePlan(resizeArgs({
    ...both,
    boundary: "inbox",
    pointerX: 240,
  }));
  assert.equal(plan.collapsed.inbox, false);
  assert.equal(plan.collapsed.nav, true);
  assert.equal(plan.widths.inbox, 240);
  assert.equal(plan.widths.nav, WS_PEEK);
  assertTotal(plan);
});

/* Cascade (pill-2 drag): past nav's collapse the gesture keeps travelling and
   starts shrinking, then collapsing, the inbox. Dragging back restores in
   reverse order — inbox first (to its remembered width), then nav — so a drag
   out and back returns the layout you started with. */
test("dividerResizePlan: pill-2 leftwards cascades from nav into inbox", () => {
  /* far enough left that nav is collapsed and inbox must give ground */
  const plan = dividerResizePlan(resizeArgs({ boundary: "nav", pointerX: 250 }));
  assert.equal(plan.collapsed.nav, true);
  assert.equal(plan.widths.nav, WS_PEEK);
  assert.equal(plan.collapsed.inbox, false);
  assert.equal(plan.widths.inbox, 250 - WS_PEEK); /* pill stays under the pointer */
  assertTotal(plan);
});

test("dividerResizePlan: pill-2 leftwards past inbox min collapses inbox too", () => {
  const plan = dividerResizePlan(resizeArgs({ boundary: "nav", pointerX: 200 }));
  assert.equal(plan.collapsed.nav, true);
  assert.equal(plan.collapsed.inbox, true);
  assert.equal(plan.widths.inbox, WS_PEEK);
  assert.equal(plan.widths.nav, WS_PEEK);
  assert.equal(plan.last.inbox, 240); /* remembered pre-collapse width */
  assert.equal(plan.last.nav, 220);
  assert.equal(plan.widths.note, WS_TOTAL - 2 * WS_PEEK);
  assertTotal(plan);
});

test("dividerResizePlan: pill-2 cascade floors at two peeks", () => {
  const plan = dividerResizePlan(resizeArgs({ boundary: "nav", pointerX: 0 }));
  assert.equal(plan.widths.inbox, WS_PEEK);
  assert.equal(plan.widths.nav, WS_PEEK);
  assert.ok(plan.widths.note >= WS_MINS.note);
  assertTotal(plan);
});

test("dividerResizePlan: pill-2 rightwards un-cascades inbox before nav", () => {
  const both = {
    zones: { inbox: WS_PEEK, nav: WS_PEEK, note: WS_TOTAL - 2 * WS_PEEK },
    collapsed: { inbox: true, nav: true },
    last: { inbox: 240, nav: 220 },
  };
  /* partway back: inbox restores, nav is still too narrow to expand */
  const mid = dividerResizePlan(resizeArgs({ ...both, boundary: "nav", pointerX: 250 }));
  assert.equal(mid.collapsed.inbox, false);
  assert.equal(mid.widths.inbox, 250 - WS_PEEK);
  assert.equal(mid.collapsed.nav, true);
  assert.equal(mid.widths.nav, WS_PEEK);
  assertTotal(mid);

  /* all the way back to where the drag began: original layout is restored */
  const back = dividerResizePlan(resizeArgs({ ...both, boundary: "nav", pointerX: 240 + 220 }));
  assert.deepEqual(back.collapsed, { inbox: false, nav: false });
  assert.equal(back.widths.inbox, 240);
  assert.equal(back.widths.nav, 220);
  assert.equal(back.widths.note, 440);
  assertTotal(back);
});

test("dividerResizePlan: pill-1 rightwards collapses nav, then yields note to its min", () => {
  /* zone-3 never collapses: the cascade stops at mins.note */
  const plan = dividerResizePlan(resizeArgs({ boundary: "inbox", pointerX: 800 }));
  assert.equal(plan.collapsed.nav, true);
  assert.equal(plan.widths.nav, WS_PEEK);
  assert.equal(plan.widths.note, WS_MINS.note);
  assert.equal(plan.widths.inbox, WS_TOTAL - WS_PEEK - WS_MINS.note);
  assert.equal(plan.collapsed.inbox, false);
  assertTotal(plan);
});

test("dividerResizePlan: unknown boundary is a no-op", () => {
  const plan = dividerResizePlan(resizeArgs({ boundary: "note", pointerX: 100 }));
  assert.deepEqual(plan.widths, WS_ZONES);
  assert.deepEqual(plan.collapsed, { inbox: false, nav: false });
});

/* ---------- provenance ---------- */
test("provLabel joins non-empty parts", () => {
  assert.equal(provLabel("Alpha", "agent", "Jul 28"), "Alpha · agent · Jul 28");
  assert.equal(provLabel("", "", "Jul 28"), "Jul 28");
  assert.equal(provLabel("Alpha", "", ""), "Alpha");
  assert.equal(provLabel("", "", ""), "");
});

/* ---------- section swap boundaries ---------- */
test("sectionSwapPlan: visual order, no wrap at boundaries", () => {
  const secs = [{ id: "b", order: 1 }, { id: "a", order: 0 }, { id: "c", order: 2 }];
  assert.equal(sectionSwapPlan(secs, "a", "up"), null);
  assert.equal(sectionSwapPlan(secs, "c", "down"), null);
  const up = sectionSwapPlan(secs, "b", "up");
  assert.deepEqual({ a: up.a, b: up.b }, { a: "b", b: "a" });
  assert.equal(up.ao, 0);
  assert.equal(up.bo, 1);
  const dn = sectionSwapPlan(secs, "b", "down");
  assert.deepEqual({ a: dn.a, b: dn.b }, { a: "b", b: "c" });
  assert.equal(sectionSwapPlan(secs, "zzz", "up"), null);
  assert.equal(sectionSwapPlan([], "a", "up"), null);
});

/* ---------- note-card reorder plan ---------- */
test("noteCardReorderPlan patches only changed orders", () => {
  const cards = [
    { id: "n1", order: 0, title: "A" },
    { id: "n2", order: 1, title: "B" },
    { id: "n3", order: 2, title: "C" },
  ];
  const plan = noteCardReorderPlan(cards, ["n3", "n1", "n2"]);
  assert.deepEqual(plan.cards.map(c => c.id), ["n3", "n1", "n2"]);
  assert.deepEqual(plan.patches, [
    { id: "n3", order: 0 },
    { id: "n1", order: 1 },
    { id: "n2", order: 2 },
  ]);
  const noop = noteCardReorderPlan(cards, ["n1", "n2", "n3"]);
  assert.deepEqual(noop.patches, []);
});

/* ---------- card lane dedupe / order ---------- */
test("cardLanesFromDoc dedupes first-seen lane colors in encounter order", () => {
  const doc = {
    sections: [
      {
        references: [
          { snapshot: { lane: "#f00" } },
          { snapshot: { lane: "#0f0" } },
          { snapshot: { lane: "#f00" } },
        ],
      },
      { references: [{ snapshot: { lane: "#00f" } }, { snapshot: {} }] },
    ],
  };
  assert.deepEqual(cardLanesFromDoc(doc), ["#f00", "#0f0", "#00f"]);
  assert.deepEqual(cardLanesFromDoc({ sections: [] }), []);
});

/* ---------- source / snapshot ---------- */
test("bookmarkSource durable fields and empties", () => {
  assert.deepEqual(bookmarkSource({
    uid: "u1", segment: 2, record: 3, node: "n1", turnTime: "T",
  }), { uid: "u1", segment: 2, record: 3, node: "n1", turnTime: "T" });
  assert.deepEqual(bookmarkSource({}), {
    uid: "", segment: 0, record: 0, node: "", turnTime: "",
  });
});

test("buildBookmarkSnapshot: self-contained; comment inherits parent lane", () => {
  const bookmarks = [
    { t: "1", lane: "lane-a", text: "parent" },
    { t: "2", anchor: "1", text: "comment", role: "assistant", turnTime: "TT" },
  ];
  const snap = buildBookmarkSnapshot(bookmarks[1], {
    bookmarks,
    nodeById: () => null,
    laneColor: id => (id ? "#c-" + id : ""),
    bookmarkLaneId,
  });
  assert.equal(snap.lane, "#c-lane-a");
  assert.equal(snap.speaker, "agent");
  assert.equal(snap.time, "TT");
  assert.equal(snap.text, "comment");
  assert.equal(snap.station, "");

  const withNode = buildBookmarkSnapshot(
    { t: "3", node: "n1", text: "hi", role: "user", lane: "lane-a" },
    {
      bookmarks: [{ t: "3", node: "n1", text: "hi", role: "user", lane: "lane-a" }],
      nodeById: id => (id === "n1" ? { id: "n1", title: "Alpha", lane_id: "lane-a" } : null),
      laneColor: id => (id === "lane-a" ? "#abc" : ""),
      bookmarkLaneId,
    },
  );
  assert.equal(withNode.station, "Alpha");
  assert.equal(withNode.speaker, "you");
  assert.equal(withNode.lane, "#abc");
});

test("buildBookmarkSnapshot freezes role, agent, prov; speaker is actual agent", () => {
  const prov = { _meta: { src: "synth-note", n: 3 } };
  const nt = {
    t: "1", role: "assistant", agent: "muse", text: "frozen reply",
    turnTime: "2026-09-10T12:00:00Z", node: "n1", lane: "lane-a",
    uid: "u1", segment: 0, record: 4, prov,
  };
  const snap = buildBookmarkSnapshot(nt, {
    bookmarks: [nt],
    nodeById: id => (id === "n1" ? { id: "n1", title: "Station", lane_id: "lane-a" } : null),
    laneColor: id => (id === "lane-a" ? "#c0392b" : ""),
    bookmarkLaneId,
  });
  assert.equal(snap.role, "assistant");
  assert.equal(snap.agent, "muse");
  assert.equal(snap.speaker, "muse");
  assert.equal(snap.station, "Station");
  assert.equal(snap.lane, "#c0392b");
  assert.equal(snap.time, "2026-09-10T12:00:00Z");
  assert.equal(snap.text, "frozen reply");
  assert.deepEqual(snap.prov, prov);
  assert.equal(typeof snap.prov, "object");
  const encoded = JSON.stringify(snap);
  const decoded = JSON.parse(encoded);
  assert.equal(typeof decoded.prov, "object");
  assert.deepEqual(decoded.prov, prov);
  assert.equal(decoded.agent, "muse");
  assert.equal(decoded.role, "assistant");
});

test("buildBookmarkSnapshot omits circular provenance without throwing", () => {
  const circ = { src: "c" };
  circ.self = circ;
  const nt = {
    t: "1", role: "assistant", agent: "muse", text: "x",
    turnTime: "T", node: "n1", lane: "lane-a", prov: circ,
  };
  let snap;
  assert.doesNotThrow(() => {
    snap = buildBookmarkSnapshot(nt, {
      bookmarks: [nt],
      nodeById: id => (id === "n1" ? { id: "n1", title: "Station", lane_id: "lane-a" } : null),
      laneColor: id => (id === "lane-a" ? "#f00" : ""),
      bookmarkLaneId,
    });
  });
  assert.equal(Object.prototype.hasOwnProperty.call(snap, "prov"), false);
  assert.equal(snap.role, "assistant");
  assert.equal(snap.agent, "muse");
  assert.equal(snap.speaker, "muse");
  assert.equal(snap.text, "x");
  assert.equal(snap.time, "T");
  assert.equal(snap.station, "Station");
  assert.equal(snap.lane, "#f00");
});

test("buildBookmarkSnapshot deep-freezes nested provenance", () => {
  const nested = { loc: "item", key: "provenance", v: { src: "snap", kids: [{ n: 1 }] } };
  const nt = {
    t: "1", role: "assistant", agent: "grok", text: "x",
    turnTime: "T", prov: [nested],
  };
  const snap = buildBookmarkSnapshot(nt, {
    bookmarks: [nt], nodeById: () => null, laneColor: () => "", bookmarkLaneId,
  });
  nested.v.src = "mutated";
  nested.v.kids[0].n = 9;
  nt.agent = "nope";
  assert.equal(snap.agent, "grok");
  assert.equal(snap.prov[0].v.src, "snap");
  assert.equal(snap.prov[0].v.kids[0].n, 1);
});

test("buildBookmarkSnapshot speaker: you / actual agent / legacy agent fallback", () => {
  const deps = {
    bookmarks: [],
    nodeById: () => null,
    laneColor: () => "",
    bookmarkLaneId,
  };
  assert.equal(buildBookmarkSnapshot({ role: "user", text: "q" }, deps).speaker, "you");
  assert.equal(buildBookmarkSnapshot({ role: "assistant", agent: "grok", text: "a" }, deps).speaker, "grok");
  assert.equal(buildBookmarkSnapshot({ role: "assistant", agent: "codex", text: "a" }, deps).speaker, "codex");
  assert.equal(buildBookmarkSnapshot({ role: "assistant", text: "legacy" }, deps).speaker, "agent");
});

test("referenceHTML displays actual agent, not only the generic word agent", () => {
  const html = referenceHTML({
    id: "r1",
    snapshot: {
      station: "Lane", speaker: "muse", time: "T", text: "hi",
      role: "assistant", agent: "muse",
    },
  }, { fmtWhen: t => t });
  assert.match(html, />.*muse.*</);
  assert.doesNotMatch(html, />agent</);
});

/* ---------- save enqueue serialization ---------- */
test("createSaveEnqueue: same-field serializes edit then revert", async () => {
  const { enqueue } = createSaveEnqueue();
  const order = [];
  let resolveFirst;
  const first = enqueue("k", () => new Promise(r => {
    resolveFirst = () => { order.push("edit"); r(); };
  }));
  const revert = enqueue("k", () => { order.push("revert"); return Promise.resolve(); });
  /* first run is scheduled on a microtask via prev.then */
  await Promise.resolve();
  assert.equal(typeof resolveFirst, "function");
  assert.deepEqual(order, []);
  resolveFirst();
  await Promise.all([first, revert]);
  assert.deepEqual(order, ["edit", "revert"]);
});

test("createSaveEnqueue: independent fields do not block each other", async () => {
  const { enqueue } = createSaveEnqueue();
  const order = [];
  let resolveA;
  const a = enqueue("title", () => new Promise(r => {
    resolveA = () => { order.push("a"); r(); };
  }));
  const b = enqueue("body", () => { order.push("b"); return Promise.resolve(); });
  await b;
  assert.deepEqual(order, ["b"]);
  resolveA();
  await a;
  assert.deepEqual(order, ["b", "a"]);
});

/* ---------- HTML builders ---------- */
test("referenceHTML and sectionBodyInner preserve references", () => {
  const r = {
    id: "r1",
    snapshot: { lane: "#f00", station: "S", speaker: "you", time: "T1", text: "**hi**" },
  };
  const html = referenceHTML(r, {
    esc: s => s, md: s => `[md:${s}]`, fmtWhen: s => `W(${s})`,
    icons: { ICON_JUMP: "J", ICON_COPY: "C", ICON_TRASH: "T" },
  });
  assert.match(html, /data-ref="r1"/);
  /* P3: visible bar is jump · copy · sendto · trash; no overflow menu */
  assert.match(html, /data-refact="jump"/);
  assert.match(html, /data-refact="copy"/);
  assert.match(html, /data-refact="sendto"/);
  assert.match(html, /data-refact="trash"/);
  assert.doesNotMatch(html, /data-refact="more"/);
  assert.match(html, /data-refmore/); /* clamp trigger — distinct from any overflow */
  assert.match(html, /S · you · W\(T1\)/);
  assert.match(html, /\[md:\*\*hi\*\*\]/);

  const body = sectionBodyInner(
    { body: "para", references: [r] },
    { esc: s => s, md: s => s, fmtWhen: s => s, icons: {} },
  );
  assert.match(body, /data-secrender/);
  assert.match(body, /class="wsrefs"/);
  assert.match(body, /data-ref="r1"/);
});

test("sectionHTML folded class and controls", () => {
  const html = sectionHTML({ id: "s1", title: "Sec", body: "x", references: [] }, true, {
    esc: s => s, md: s => s, fmtWhen: s => s,
    icons: { ICON_CHEV_RIGHT: "R", ICON_CHEV_DOWN: "D", ICON_PLUS: "+", ICON_MENU_DOTS: "M" },
  });
  assert.match(html, /class="wssec folded"/);
  assert.match(html, /data-sec="s1"/);
  assert.match(html, /data-addhere/);
  assert.match(html, /data-secmenu/);
  assert.match(html, />R</);
});

test("noteCardHTML active class, draggable, lanes", () => {
  const html = noteCardHTML(
    { id: "n1", title: "Hello", lanes: ["#f00", "#0f0"] },
    "n1",
    { esc: s => s, fmtNoteMeta: () => "1 section · now" },
  );
  assert.match(html, /class="wscard on"/);
  assert.match(html, /draggable="true"/);
  assert.match(html, /data-note="n1"/);
  assert.match(html, /wclanes/);
});

test("noteCardHTML carries Rename + Delete card actions; delete is danger", () => {
  const html = noteCardHTML(
    { id: "n1", title: "Hello", lanes: [] },
    "n1",
    { esc: s => s, fmtNoteMeta: () => "meta", icons: { ICON_PENCIL: "P", ICON_TRASH: "T" } },
  );
  /* the shared floating cluster (Activities' idiom), not the in-card footer bar
     that used to grow the row downwards */
  assert.match(html, /class="wscardactions roundactions"/);
  assert.doesNotMatch(html, /actionbar/);
  assert.match(html, /data-wcact="rename"/);
  assert.match(html, /data-wcact="delete"/);
  assert.match(html, /data-wcact="delete"[^>]*class="[^"]*danger|class="[^"]*danger[^"]*"[^>]*data-wcact="delete"/);
  assert.match(html, /aria-label="rename note"/);
  assert.match(html, /aria-label="delete note"/);
  /* card must host buttons (not be a <button>) so nested actions stay valid HTML */
  assert.match(html, /<div class="wscard/);
  assert.doesNotMatch(html, /<button class="wscard/);
});

/* P7: missing deps.esc / deps.md must fall back to the real escaper, never
   identity. A caller that forgets deps used to get raw <script> into
   innerHTML. Assert the escaped form is present so a merely-empty result
   cannot pass. */
test("HTML builders escape untrusted fields when deps are omitted", () => {
  const x = "<script>alert(1)</script>";
  const escaped = "&lt;script&gt;alert(1)&lt;/script&gt;";
  const hasLiteralScript = html => html.includes("<script>");
  const hasEscaped = html => html.includes(escaped);

  const body = sectionBodyInner({ body: x, references: [] });
  assert.equal(hasLiteralScript(body), false, "sectionBodyInner body");
  assert.equal(hasEscaped(body), true, "sectionBodyInner body escaped");

  const sec = sectionHTML({ id: x, title: x, body: x, references: [] }, false);
  assert.equal(hasLiteralScript(sec), false, "sectionHTML");
  assert.equal(hasEscaped(sec), true, "sectionHTML escaped");

  const ref = referenceHTML({
    id: x,
    snapshot: { lane: x, station: x, speaker: x, time: "T", text: x },
  });
  assert.equal(hasLiteralScript(ref), false, "referenceHTML");
  assert.equal(hasEscaped(ref), true, "referenceHTML escaped");

  const card = noteCardHTML({ id: x, title: x, lanes: [x] }, null);
  assert.equal(hasLiteralScript(card), false, "noteCardHTML");
  assert.equal(hasEscaped(card), true, "noteCardHTML escaped");

  const inbox = inboxItemHTML({ t: x, text: x, node: "n1" }, "var(--unlane)", {});
  assert.equal(hasLiteralScript(inbox), false, "inboxItemHTML");
  assert.equal(hasEscaped(inbox), true, "inboxItemHTML escaped");

  /* md fallbacks: missing md must render as escaped plain text, not raw HTML. */
  const bodyMd = sectionBodyInner({ body: x, references: [] }, {});
  assert.equal(hasLiteralScript(bodyMd), false, "sectionBodyInner md fallback");
  assert.equal(hasEscaped(bodyMd), true, "sectionBodyInner md escaped");

  const refMd = referenceHTML({ id: "r", snapshot: { text: x } }, {});
  assert.equal(hasLiteralScript(refMd), false, "referenceHTML md fallback");
  assert.equal(hasEscaped(refMd), true, "referenceHTML md escaped");
});

test("noteCardDragEnabled is false while renaming", () => {
  assert.equal(noteCardDragEnabled(false), true);
  assert.equal(noteCardDragEnabled(true), false);
});

test("noteCardRenamePlan: PATCH title; top bar only when active", () => {
  const active = noteCardRenamePlan({ noteId: "n1", activeId: "n1", title: "New" });
  assert.deepEqual(active.patch, { title: "New" });
  assert.equal(active.updateTopBar, true);
  assert.equal(active.topBarText, "New");
  assert.equal(active.updateActive, true);

  const other = noteCardRenamePlan({ noteId: "n2", activeId: "n1", title: "" });
  assert.deepEqual(other.patch, { title: "" });
  assert.equal(other.updateTopBar, false);
  assert.equal(other.topBarText, "Note");
  assert.equal(other.updateActive, false);
});

test("noteCardKeySelects: Enter/Space open the card; ignore edit/actions/rename", () => {
  assert.equal(noteCardKeySelects({ key: "Enter" }), true);
  assert.equal(noteCardKeySelects({ key: " " }), true);
  assert.equal(noteCardKeySelects({ key: "Spacebar" }), true);
  assert.equal(noteCardKeySelects({ key: "a" }), false);
  assert.equal(noteCardKeySelects({ key: "Enter", inTitleEdit: true }), false);
  assert.equal(noteCardKeySelects({ key: "Enter", onAction: true }), false);
  assert.equal(noteCardKeySelects({ key: " ", renaming: true }), false);
});

/* ---------- inbox lane / order / tabs / clamp reuse ---------- */
test("inbox lane tabs, resolve, list order, HTML", () => {
  const bms = [
    { t: "2", text: "later", lane: "lane-a" },
    { t: "1", text: "first", lane: "lane-a" },
    { t: "3", text: "gen" },
    { t: "4", anchor: "1", text: "comment" },
  ];
  const lanes = [{ id: "lane-a", name: "Alpha", color: "#f00" }];
  const nodeById = () => null;
  const { byT, laneOf, tabs } = inboxLaneTabs(bms, lanes, bookmarkLaneId, nodeById);
  assert.equal(tabs.length, 1);
  assert.equal(tabs[0].id, "lane-a");
  assert.equal(resolveInboxTab("missing", tabs), "GENERAL");
  assert.equal(resolveInboxTab("lane-a", tabs), "lane-a");

  const listA = inboxListModel(bms, "lane-a", laneOf, byT, bookmarkSortKey);
  assert.deepEqual(listA.map(n => n.t), ["1", "4", "2"]); /* comment after anchor */

  const th = inboxTabsHTML("GENERAL", tabs, s => s, id => "#f00");
  assert.match(th, /data-wsitab="GENERAL"[^>]*class="on"/);
  assert.match(th, /data-wsitab="lane-a"/);

  const deps = {
    esc: s => s, md: s => s, fmtWhen: s => "W", nodeById: () => ({ title: "Alpha" }),
    icons: { ICON_CLIP: "L" },
  };
  const closed = inboxItemHTML({ t: "1", text: "hi", node: "n1" }, "var(--unlane)", deps);
  /* the row is tap-to-reveal, like the compact pane's card and the embedded
     reference — the three bookmark surfaces answer a tap the same way */
  assert.doesNotMatch(closed, /class="actionbar tear"/);
  assert.doesNotMatch(closed, /data-bmact=/);
  assert.match(closed, /data-wsimore/);
  assert.match(closed, /Alpha/);

  const item = inboxItemHTML({ t: "1", text: "hi", node: "n1" }, "var(--unlane)",
    { ...deps, open: true });
  /* P5: inbox flat bar is jump · copy · note · sendto · del */
  assert.match(item, /class="actionbar tear"/);
  for (const act of ["jump", "copy", "note", "sendto", "del"]) {
    assert.match(item, new RegExp(`data-bmact="${act}"`), act);
  }
  assert.doesNotMatch(item, /data-bmact="comment"/);
  assert.doesNotMatch(item, /data-bmact="more"/);
});

test("bookmarkClampState reused at inbox/reference boundaries", () => {
  assert.deepEqual(bookmarkClampState(900, false), {
    clamped: true, showBtn: true, label: "Show more",
  });
  assert.deepEqual(bookmarkClampState(100, true), {
    clamped: false, showBtn: false, label: "Show less",
  });
});

/* ---------- swipe action applicator ---------- */
test("applyNotesSwipeAction maps decision types", () => {
  assert.equal(applyNotesSwipeAction(null), null);
  assert.equal(applyNotesSwipeAction({ type: "endPlacement" }), "endPlacement");
  assert.equal(applyNotesSwipeAction({ type: "noteToList" }), "noteToList");
  assert.equal(applyNotesSwipeAction({ type: "closeWorkspace" }), "closeWorkspace");
  assert.equal(applyNotesSwipeAction({ type: "other" }), null);
});

/* ---------- fake DOM helpers ---------- */
function classList(el){
  const set = new Set((el.className || "").split(/\s+/).filter(Boolean));
  return {
    contains: c => set.has(c),
    add: c => { set.add(c); el.className = [...set].join(" "); },
    remove: c => { set.delete(c); el.className = [...set].join(" "); },
    toggle: (c, force) => {
      if (force === true){ set.add(c); el.className = [...set].join(" "); return true; }
      if (force === false){ set.delete(c); el.className = [...set].join(" "); return false; }
      if (set.has(c)){ set.delete(c); el.className = [...set].join(" "); return false; }
      set.add(c); el.className = [...set].join(" "); return true;
    },
  };
}

function makeStyle(){
  const props = Object.create(null);
  return {
    setProperty(k, v){ props[k] = String(v); },
    removeProperty(k){ delete props[k]; },
    getPropertyValue(k){ return props[k] || ""; },
    get _props(){ return props; },
  };
}

function el(tag, props = {}){
  const listeners = {};
  const node = {
    tagName: tag.toUpperCase(),
    className: props.className || "",
    hidden: !!props.hidden,
    innerHTML: props.innerHTML || "",
    textContent: props.textContent || "",
    value: props.value || "",
    dataset: Object.assign({}, props.dataset || {}),
    style: makeStyle(),
    children: [],
    parentNode: null,
    parentElement: null,
    nextSibling: null,
    scrollHeight: props.scrollHeight || 0,
    offsetHeight: props.offsetHeight || 0,
    clientHeight: props.clientHeight || 0,
    get classList(){ return classList(node); },
    addEventListener(type, fn){
      (listeners[type] || (listeners[type] = [])).push(fn);
    },
    removeEventListener(type, fn){
      if (!listeners[type]) return;
      listeners[type] = listeners[type].filter(f => f !== fn);
    },
    dispatch(type, ev = {}){
      const e = Object.assign({
        type, target: node, currentTarget: node, preventDefault(){}, stopPropagation(){},
      }, ev);
      if (!e.target) e.target = node;
      /* bubble to ancestors (pointer events on dividers are bound on #wszones) */
      let cur = node;
      while (cur){
        const list = cur._listeners && cur._listeners[type];
        if (list) list.forEach(fn => fn(e));
        cur = cur.parentNode;
      }
    },
    focus(){ node._focused = true; documentRef.activeElement = node; },
    blur(){ node._focused = false; },
    select(){ node._selected = true; },
    setSelectionRange(a, b){
      node.selectionStart = a; node.selectionEnd = b; node._range = [a, b];
    },
    click(){ node.dispatch("click"); },
    remove(){
      if (node.parentNode && node.parentNode.children){
        node.parentNode.children = node.parentNode.children.filter(c => c !== node);
      }
    },
    closest(sel){
      if (matchSel(node, sel)) return node;
      return node.parentNode && node.parentNode.closest
        ? node.parentNode.closest(sel) : null;
    },
    querySelector(sel){
      for (const c of walk(node)){
        if (matchSel(c, sel)) return c;
      }
      return null;
    },
    querySelectorAll(sel){
      return [...walk(node)].filter(c => matchSel(c, sel));
    },
    appendChild(child){
      child.parentNode = node;
      child.parentElement = node;
      node.children.push(child);
      return child;
    },
    insertBefore(child, ref){
      child.parentNode = node;
      child.parentElement = node;
      if (!ref){ node.children.push(child); return child; }
      const i = node.children.indexOf(ref);
      if (i < 0) node.children.push(child);
      else node.children.splice(i, 0, child);
      return child;
    },
    getBoundingClientRect(){
      return node._rect || { top: 0, left: 0, bottom: 20, right: 100, height: 20, width: 100 };
    },
    setPointerCapture(){ node._captured = true; },
    releasePointerCapture(){ node._captured = false; },
    setAttribute(k, v){ node[k] = v; },
    insertAdjacentHTML(pos, html){
      if (pos === "beforeend") node.innerHTML = (node.innerHTML || "") + html;
    },
    get _listeners(){ return listeners; },
  };
  if (props.id) node.id = props.id;
  if (props.dataset) Object.assign(node.dataset, props.dataset);
  return node;
}

function* walk(node){
  for (const c of node.children || []){
    yield c;
    yield* walk(c);
  }
}

function matchSel(node, sel){
  if (!sel) return false;
  if (sel.startsWith(".")) return (node.className || "").split(/\s+/).includes(sel.slice(1));
  if (sel.startsWith("#")) return node.id === sel.slice(1);
  if (sel.startsWith("[") && sel.endsWith("]")){
    const body = sel.slice(1, -1);
    if (body.startsWith("data-")){
      const [k, v] = body.split("=");
      const key = k.replace(/^data-/, "").replace(/-([a-z])/g, (_, c) => c.toUpperCase());
      /* dataset keys in HTML are camelCase of data-foo-bar → fooBar; our dataset uses short keys */
      const dk = k.replace(/^data-/, "");
      const camel = dk.replace(/-([a-z])/g, (_, c) => c.toUpperCase());
      const val = node.dataset[dk] ?? node.dataset[camel] ?? node.dataset[key];
      if (v == null) return val != null || node[k] != null;
      const want = v.replace(/^"|"$/g, "");
      return String(val) === want;
    }
  }
  if (sel.includes(".")){
    const [tag, cls] = sel.split(".");
    return node.tagName === tag.toUpperCase() && (node.className || "").includes(cls);
  }
  if (sel.includes("[")){
    const m = sel.match(/^([a-z0-9]+)(\[.+\])$/i);
    if (m) return matchSel(node, m[1]) && matchSel(node, m[2]);
  }
  return node.tagName === sel.toUpperCase();
}

let documentRef = { activeElement: null, contains: () => true };

function makeStorage(init = {}){
  const m = new Map(Object.entries(init));
  return {
    getItem: k => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => { m.set(k, String(v)); },
    removeItem: k => { m.delete(k); },
    _map: m,
  };
}

function makeRoots(){
  const notesworkspace = el("div", { id: "notesworkspace", hidden: true });
  const wsscrim = el("div", { id: "wsscrim" });
  const wspanel = el("div", { id: "wspanel" });
  const wstitle = el("h2", { id: "wstitle", textContent: "Notes" });
  const wsback = el("button", { id: "wsback", hidden: true });
  const wsclose = el("button", { id: "wsclose" });
  const wsplacetext = el("span", { id: "wsplacetext" });
  const wsplacecancel = el("button", { id: "wsplacecancel" });
  const wszones = el("div", { id: "wszones" });
  const wsinbox = el("div", { id: "wsinbox" });
  const wsnav = el("div", { id: "wsnav" });
  const wsnote = el("div", { id: "wsnote" });
  const wsinboxtabs = el("div", { id: "wsinboxtabs" });
  const wsinboxlist = el("div", { id: "wsinboxlist" });
  const wscards = el("div", { id: "wscards" });
  const wsnewnote = el("button", { id: "wsnewnote" });
  const wssections = el("div", { id: "wssections" });
  const wsnoteempty = el("div", { id: "wsnoteempty" });
  const divInbox = el("div", { className: "wsdivider", dataset: { boundary: "inbox" } });
  const divNav = el("div", { className: "wsdivider", dataset: { boundary: "nav" } });
  const notesbtn = el("button", { id: "notesbtn" });

  /* zone rects: inbox 240 | nav 220 | note 440 (total 900) — desktop three-zone */
  wsinbox._rect = { top: 0, left: 0, width: 240, height: 400, right: 240, bottom: 400 };
  wsnav._rect = { top: 0, left: 240, width: 220, height: 400, right: 460, bottom: 400 };
  wsnote._rect = { top: 0, left: 460, width: 440, height: 400, right: 900, bottom: 400 };
  wszones._rect = { top: 0, left: 0, width: 900, height: 400, right: 900, bottom: 400 };

  notesworkspace.appendChild(wsscrim);
  notesworkspace.appendChild(wspanel);
  wspanel.appendChild(wstitle);
  wspanel.appendChild(wsback);
  wspanel.appendChild(wsclose);
  wspanel.appendChild(wsplacetext);
  wspanel.appendChild(wsplacecancel);
  wspanel.appendChild(wszones);
  wszones.appendChild(wsinbox);
  wszones.appendChild(divInbox);
  wszones.appendChild(wsnav);
  wszones.appendChild(divNav);
  wszones.appendChild(wsnote);
  wsinbox.appendChild(wsinboxtabs);
  wsinbox.appendChild(wsinboxlist);
  wsnav.appendChild(wscards);
  wsnav.appendChild(wsnewnote);
  wsnote.appendChild(wssections);
  wsnote.appendChild(wsnoteempty);

  documentRef = {
    activeElement: null,
    contains: n => !!n,
    createElement: tag => el(tag),
    querySelector: () => null,
    getElementById: () => null,
    addEventListener: (...a) => { /* filled below */ },
    removeEventListener: (...a) => {},
    _listeners: {},
  };
  documentRef.addEventListener = (type, fn, opts) => {
    (documentRef._listeners[type] || (documentRef._listeners[type] = [])).push({ fn, opts });
  };
  documentRef.removeEventListener = (type, fn) => {
    if (!documentRef._listeners[type]) return;
    documentRef._listeners[type] = documentRef._listeners[type].filter(x => x.fn !== fn);
  };

  return {
    roots: {
      notesworkspace, wsscrim, wspanel, wstitle, wsback, wsclose,
      wsplacetext, wsplacecancel, wszones, wsinbox, wsnav, wsnote,
      wsinboxtabs, wsinboxlist, wscards, wsnewnote, wssections, wsnoteempty,
      divInbox, divNav,
    },
    document: documentRef,
    notesbtn,
  };
}

function createFeature(overrides = {}){
  const { roots, document, notesbtn } = makeRoots();
  const storage = makeStorage(overrides.storageInit || {});
  const timers = [];
  let now = 0;
  const setTimeoutFn = (fn, ms) => {
    const id = { fn, at: now + (ms || 0), cleared: false };
    timers.push(id);
    return id;
  };
  const clearTimeoutFn = id => { if (id) id.cleared = true; };
  const flush = ms => {
    now += ms || 0;
    const due = timers.filter(t => !t.cleared && t.at <= now);
    for (const t of due){
      t.cleared = true;
      t.fn();
    }
  };

  const apiLog = [];
  let notes = overrides.notes || [];
  const docs = overrides.docs || {};
  const api = async (url, opts = {}) => {
    apiLog.push({ url, method: opts.method || "GET", body: opts.body });
    if (typeof overrides.api === "function") return overrides.api(url, opts, { notes, docs, apiLog });
    if (url === "/api/notes" && (!opts.method || opts.method === "GET"))
      return { notes };
    if (url === "/api/notes" && opts.method === "POST"){
      const sh = { id: "new1", title: "Untitled", order: notes.length, sections: [] };
      notes = [...notes, sh];
      docs[sh.id] = { ...sh, sections: [{ id: "s1", title: "", body: "", order: 0, references: [] }] };
      return sh;
    }
    const m = url.match(/^\/api\/notes\/([^/]+)$/);
    if (m && (!opts.method || opts.method === "GET")){
      if (overrides.failOpen) throw new Error("open fail");
      return docs[decodeURIComponent(m[1])] || {
        id: decodeURIComponent(m[1]), title: "Note", sections: [],
      };
    }
    if (m && opts.method === "PATCH"){
      const id = decodeURIComponent(m[1]);
      const body = opts.body ? JSON.parse(opts.body) : {};
      const doc = docs[id] || { id, title: "Note", sections: [] };
      if (body.title != null) doc.title = body.title;
      if (body.order != null){
        const card = notes.find(c => c.id === id);
        if (card) card.order = body.order;
      }
      if (body.add_section != null){
        doc.sections = doc.sections || [];
        doc.sections.push({ id: "s" + (doc.sections.length + 1), title: "", body: "", order: doc.sections.length, references: [] });
      }
      if (body.section){
        const sec = (doc.sections || []).find(s => s.id === body.section.id);
        if (body.section.delete) doc.sections = (doc.sections || []).filter(s => s.id !== body.section.id);
        else if (sec){
          if (body.section.title != null) sec.title = body.section.title;
          if (body.section.body != null) sec.body = body.section.body;
          if (body.section.order != null) sec.order = body.section.order;
        }
      }
      doc.edited_at = "E" + apiLog.length;
      docs[id] = doc;
      const card = notes.find(c => c.id === id);
      if (card){
        card.title = doc.title;
        card.edited_at = doc.edited_at;
        card.section_count = (doc.sections || []).length;
      }
      return doc;
    }
    if (m && opts.method === "DELETE"){
      notes = notes.filter(c => c.id !== decodeURIComponent(m[1]));
      return {};
    }
    const refPost = url.match(/^\/api\/notes\/([^/]+)\/sections\/([^/]+)\/references$/);
    if (refPost && opts.method === "POST"){
      const id = decodeURIComponent(refPost[1]);
      const sid = decodeURIComponent(refPost[2]);
      const doc = docs[id];
      const sec = doc.sections.find(s => s.id === sid);
      const body = JSON.parse(opts.body);
      sec.references = sec.references || [];
      sec.references.push({ id: "r" + sec.references.length, source: body.source, snapshot: body.snapshot });
      docs[id] = doc;
      return doc;
    }
    const refDel = url.match(/^\/api\/notes\/([^/]+)\/sections\/([^/]+)\/references\/([^/]+)$/);
    if (refDel && opts.method === "DELETE"){
      const id = decodeURIComponent(refDel[1]);
      const sid = decodeURIComponent(refDel[2]);
      const rid = decodeURIComponent(refDel[3]);
      const doc = docs[id];
      const sec = doc.sections.find(s => s.id === sid);
      sec.references = (sec.references || []).filter(r => r.id !== rid);
      return doc;
    }
    return null;
  };

  let bms = overrides.bookmarks || [];
  const effects = {
    toast: [],
    copy: [],
    jump: [],
  };

  const feature = createNotesFeature({
    roots,
    document,
    storage,
    setTimeout: setTimeoutFn,
    clearTimeout: clearTimeoutFn,
    requestAnimationFrame: fn => setTimeoutFn(fn, 0),
    CSS: { escape: s => s },
    esc: s => String(s ?? "").replace(/[<>&"]/g, c => ({
      "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;",
    }[c])),
    md: s => String(s ?? ""),
    fmtWhen: s => "W(" + s + ")",
    fmtNoteMeta: c => `${c.section_count || 0} sections`,
    hashStr: s => {
      let h = 0;
      for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0;
      return String(h);
    },
    icons: {
      ICON_PLUS: "+", ICON_MENU_DOTS: "M", ICON_CLIP: "L",
      ICON_JUMP: "J", ICON_COPY: "C", ICON_TRASH: "T",
      ICON_CHEV_RIGHT: "R", ICON_CHEV_DOWN: "D",
      ICON_PENCIL: "P", ICON_MOVE_UP: "U", ICON_MOVE_DOWN: "N",
    },
    api,
    bookmarks: () => bms,
    nodeById: id => (overrides.nodes || {})[id] || null,
    laneList: () => overrides.lanes || [],
    laneColor: id => ((overrides.lanes || []).find(l => l.id === id) || {}).color || "",
    toast: msg => effects.toast.push(msg),
    copyText: s => effects.copy.push(s),
    jumpToChatAddress: a => {
      effects.jump.push(a);
      return overrides.jumpOk !== false;
    },
    isNarrow: () => !!overrides.isNarrow,
    isDesktop: () => !!overrides.isDesktop,
    notesbtn: () => notesbtn,
    activeElement: () => document.activeElement,
    ...overrides.deps,
  });

  return {
    feature, roots, document, notesbtn, storage, apiLog, effects, timers, flush,
    setBookmarks(next){ bms = next; },
    setNotes(next){ notes = next; },
    docs,
  };
}

/* ---------- open/close focus ---------- */
test("open/close: focus return and notesbtn fallback", () => {
  const ctx = createFeature();
  const { feature, roots, notesbtn, document } = ctx;
  feature.bind();
  const prior = el("button");
  document.activeElement = prior;
  feature.open();
  assert.equal(feature.isOpen(), true);
  assert.equal(roots.notesworkspace.hidden, false);
  ctx.flush(0); /* rAF focus new note */
  assert.equal(roots.wsnewnote._focused, true);

  feature.close();
  assert.equal(feature.isOpen(), false);
  assert.equal(prior._focused, true);

  document.activeElement = null;
  /* second open with no prior focus → close focuses notesbtn */
  document.contains = () => false;
  feature.open();
  feature.close();
  assert.equal(notesbtn._focused, true);
});

test("open is idempotent when already open", () => {
  const ctx = createFeature();
  const { feature, roots } = ctx;
  feature.bind();
  feature.open();
  ctx.flush(0); /* drain first open's rAF focus */
  const focusCalls = [];
  roots.wsnewnote.focus = () => focusCalls.push(1);
  feature.open();
  ctx.flush(0);
  assert.equal(focusCalls.length, 0);
});

/* ---------- debounce / flush / serialize via factory ---------- */
async function settle(n = 8){
  for (let i = 0; i < n; i++) await Promise.resolve();
}

async function openNote(ctx, id){
  const card = el("button", { className: "wscard", dataset: { note: id } });
  card.dataset.note = id;
  card.closest = sel => (sel === ".wscard" ? card : null);
  ctx.roots.wscards.dispatch("click", { target: card });
  await settle();
}

test("save debounce 1100ms, same-field serialize, close flush (section title)", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "T", order: 0 }],
    docs: {
      n1: { id: "n1", title: "T", sections: [{ id: "s1", title: "S", body: "b", order: 0, references: [] }] },
    },
  });
  const { feature, roots, apiLog, flush } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");
  assert.equal(roots.wstitle.textContent, "T");

  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  sec.dataset.sec = "s1";
  const title = el("input", { dataset: { sectitle: "" }, value: "S" });
  title.dataset.sectitle = "";
  title.value = "S";
  title.closest = sel => {
    if (sel === "[data-sectitle]") return title;
    if (sel === ".wssec") return sec;
    return null;
  };
  sec.appendChild(title);
  roots.wssections.appendChild(sec);

  roots.wssections.dispatch("focusin", { target: title });
  title.value = "A";
  roots.wssections.dispatch("input", { target: title });
  title.value = "B";
  roots.wssections.dispatch("input", { target: title });

  const patchesBefore = apiLog.filter(x => x.method === "PATCH");
  assert.equal(patchesBefore.length, 0, "debounced — no PATCH yet");

  flush(1100);
  await settle();
  const afterDebounce = apiLog.filter(x => x.method === "PATCH" && x.body && x.body.includes("section"));
  assert.ok(afterDebounce.length >= 1);
  const last = JSON.parse(afterDebounce[afterDebounce.length - 1].body);
  assert.equal(last.section.title, "B", "last debounced value wins");

  title.value = "C";
  roots.wssections.dispatch("input", { target: title });
  const beforeClose = apiLog.length;
  feature.close();
  await settle();
  const flushed = apiLog.slice(beforeClose).filter(x => x.method === "PATCH");
  assert.ok(flushed.length >= 1, "close flushes pending save");
  assert.equal(JSON.parse(flushed[flushed.length - 1].body).section.title, "C");
});

/* ---------- a save belongs to the note it was typed into ---------- */

/* The workspace edits one note while queued PATCHes from another are still in
   flight, so every save has to carry its own address. Reading the selection
   when the request finally runs sends the edit to whatever the user opened in
   the meantime, where its section id means nothing: the PATCH succeeds, the
   edit is gone, and nothing says so. */
function attachSectionTitle(ctx, secId, value){
  const sec = el("div", { className: "wssec", dataset: { sec: secId } });
  sec.dataset.sec = secId;
  const title = el("input", { dataset: { sectitle: "" }, value });
  title.dataset.sectitle = "";
  title.value = value;
  title.closest = sel => {
    if (sel === "[data-sectitle]") return title;
    if (sel === ".wssec") return sec;
    return null;
  };
  sec.appendChild(title);
  ctx.roots.wssections.appendChild(sec);
  ctx.roots.wssections.dispatch("focusin", { target: title });
  return title;
}

/* Two notes with *different* section ids: a misdirected PATCH is one the
   receiving note cannot apply, which is exactly why the edit disappears. */
function twoNotes(){
  return {
    notes: [{ id: "n1", title: "One", order: 0 }, { id: "n2", title: "Two", order: 1 }],
    docs: {
      n1: { id: "n1", title: "One", sections: [{ id: "s1", title: "S", body: "", order: 0, references: [] }] },
      n2: { id: "n2", title: "Two", sections: [{ id: "s2", title: "S", body: "", order: 0, references: [] }] },
    },
  };
}

function patchesMatching(apiLog, text){
  return apiLog.filter(x => x.method === "PATCH" && x.body && x.body.includes(text));
}

test("a debounced save addresses the note it was typed into", async () => {
  const ctx = createFeature(twoNotes());
  ctx.feature.bind();
  ctx.feature.open();
  await settle();
  await openNote(ctx, "n1");

  const title = attachSectionTitle(ctx, "s1", "S");
  title.value = "typed into n1";
  ctx.roots.wssections.dispatch("input", { target: title });

  await openNote(ctx, "n2");   /* the user moves on before the debounce fires */
  ctx.flush(SAVE_DEBOUNCE_MS);
  await settle();

  const patches = patchesMatching(ctx.apiLog, "typed into n1");
  assert.equal(patches.length, 1, "the debounced edit must still be sent");
  assert.equal(patches[0].url, "/api/notes/n1",
    "the edit went to the note on screen, where section s1 does not exist");
});

test("a save queued behind an in-flight one keeps its own note", async () => {
  const base = twoNotes();
  const release = [];
  const ctx = createFeature({
    ...base,
    api: async (url, opts = {}) => {
      if (opts.method === "PATCH")
        return new Promise(r => release.push(() => r({ id: "n1", edited_at: "e" })));
      if (url === "/api/notes") return { notes: base.notes };
      const m = url.match(/^\/api\/notes\/([^/]+)$/);
      if (m) return base.docs[m[1]];
      return null;
    },
  });
  ctx.feature.bind();
  ctx.feature.open();
  await settle();
  await openNote(ctx, "n1");

  const title = attachSectionTitle(ctx, "s1", "S");
  title.value = "first into n1";
  ctx.roots.wssections.dispatch("input", { target: title });
  ctx.flush(SAVE_DEBOUNCE_MS);
  await settle();
  assert.equal(release.length, 1, "the first save is in flight");

  title.value = "second into n1";
  ctx.roots.wssections.dispatch("input", { target: title });
  ctx.flush(SAVE_DEBOUNCE_MS);
  await settle();

  await openNote(ctx, "n2");   /* switch while one save runs and one waits */
  release.forEach(fn => fn());
  await settle();
  release.forEach(fn => fn());
  await settle();

  const patches = patchesMatching(ctx.apiLog, "into n1");
  assert.equal(patches.length, 2, "both saves must be sent");
  assert.deepEqual(patches.map(p => p.url), ["/api/notes/n1", "/api/notes/n1"],
    "the queued save read the selection at execution time");
});

test("a save the server refuses is kept for the next flush, not just announced", async () => {
  const base = twoNotes();
  let refuse = true;
  const ctx = createFeature({
    ...base,
    api: async (url, opts = {}) => {
      if (opts.method === "PATCH"){
        if (refuse){ refuse = false; throw new Error("server said no"); }
        return { id: "n1", edited_at: "e" };
      }
      if (url === "/api/notes") return { notes: base.notes };
      const m = url.match(/^\/api\/notes\/([^/]+)$/);
      if (m) return base.docs[m[1]];
      return null;
    },
  });
  ctx.feature.bind();
  ctx.feature.open();
  await settle();
  await openNote(ctx, "n1");

  const title = attachSectionTitle(ctx, "s1", "S");
  title.value = "worth keeping";
  ctx.roots.wssections.dispatch("input", { target: title });
  ctx.flush(SAVE_DEBOUNCE_MS);
  await settle();
  assert.equal(patchesMatching(ctx.apiLog, "worth keeping").length, 1, "the first attempt failed");

  ctx.feature.close();   /* leaving the workspace flushes what is still unsaved */
  await settle();

  const patches = patchesMatching(ctx.apiLog, "worth keeping");
  assert.equal(patches.length, 2,
    "'will retry on next edit' keeps nothing when the user has moved on");
  assert.equal(patches[1].url, "/api/notes/n1");
});

test("Escape section title revert enqueues after in-flight via PatchNow path", async () => {
  const pending = [];
  const ctx = createFeature({
    notes: [{ id: "n1", title: "Orig", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "Orig",
        sections: [{ id: "s1", title: "OrigSec", body: "", order: 0, references: [] }],
      },
    },
    api: async (url, opts) => {
      if (opts && opts.method === "PATCH"){
        return new Promise(r => {
          pending.push(() => {
            const body = JSON.parse(opts.body);
            r({
              id: "n1", title: "Orig",
              sections: [{
                id: "s1",
                title: (body.section && body.section.title) || "OrigSec",
                body: "", order: 0, references: [],
              }],
              edited_at: "e",
            });
          });
        });
      }
      if (url === "/api/notes") return { notes: [{ id: "n1", title: "Orig", order: 0 }] };
      if (url.includes("/api/notes/n1") && (!opts || !opts.method || opts.method === "GET"))
        return {
          id: "n1", title: "Orig",
          sections: [{ id: "s1", title: "OrigSec", body: "", order: 0, references: [] }],
        };
      return null;
    },
  });
  const { feature, roots, flush } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");

  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  sec.dataset.sec = "s1";
  const title = el("input", { dataset: { sectitle: "" }, value: "OrigSec" });
  title.dataset.sectitle = "";
  title.value = "OrigSec";
  title.closest = sel => {
    if (sel === "[data-sectitle]") return title;
    if (sel === ".wssec") return sec;
    return null;
  };
  sec.appendChild(title);
  roots.wssections.appendChild(sec);

  roots.wssections.dispatch("focusin", { target: title });
  title.value = "Edited";
  roots.wssections.dispatch("input", { target: title });
  flush(1100);
  await settle();
  assert.equal(pending.length, 1, "in-flight edit");

  roots.wssections.dispatch("keydown", {
    key: "Escape", target: title,
    preventDefault(){}, stopPropagation(){},
  });
  assert.equal(title.value, "OrigSec");
  pending[0]();
  await settle();
  assert.ok(pending.length >= 2, "revert enqueued after in-flight");
  pending[1]();
  await settle();
});

/* ---------- note CRUD ---------- */
test("create note success path", async () => {
  const ctx = createFeature({ notes: [] });
  const { feature, roots, apiLog } = ctx;
  feature.bind();
  feature.open();
  await settle();
  assert.match(roots.wscards.innerHTML, /No notes yet/);
  roots.wsnewnote.dispatch("click");
  await settle(12);
  assert.ok(apiLog.some(x => x.url === "/api/notes" && x.method === "POST"));
  assert.equal(feature.isOpen(), true);
  assert.ok(roots.notesworkspace.classList.contains("note-open"));
});

test("create note error toasts", async () => {
  const ctx = createFeature({
    api: async () => { throw new Error("fail"); },
  });
  const { feature, roots, effects } = ctx;
  feature.bind();
  feature.open();
  roots.wsnewnote.dispatch("click");
  await settle(12);
  assert.ok(effects.toast.includes("Couldn't create note"));
});

test("delete note success clears editor", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "T", order: 0 }],
    docs: { n1: { id: "n1", title: "T", sections: [] } },
  });
  const { feature, roots, apiLog } = ctx;
  feature.bind();
  feature.open();
  await settle();
  const card = el("button", { className: "wscard", dataset: { note: "n1" } });
  card.dataset.note = "n1";
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.dispatch("click", { target: card });
  await Promise.resolve();
  assert.ok(roots.notesworkspace.classList.contains("note-open"));

  /* card Delete → archive-aware confirm → ok */
  const delBtn = el("button", { className: "danger", dataset: { wcact: "delete" } });
  delBtn.dataset.wcact = "delete";
  delBtn.closest = sel => {
    if (sel === "[data-wcact]" || sel.includes("data-wcact")) return delBtn;
    if (sel === ".wscard") return card;
    return null;
  };
  delBtn.getBoundingClientRect = () => ({ top: 10, left: 10, bottom: 50, height: 40, width: 44 });
  roots.wspanel.getBoundingClientRect = () => ({ top: 0, left: 0, width: 400, height: 600 });
  roots.wscards.dispatch("click", {
    target: delBtn, stopPropagation(){}, preventDefault(){},
  });
  await settle();
  assert.equal(apiLog.some(x => x.method === "DELETE"), false, "await confirm");
  const confirm = roots.wspanel.children.find(c => (c.className || "").includes("popmenu"));
  assert.ok(confirm);
  const ok = el("button", { dataset: { wconfirm: "ok" } });
  ok.dataset.wconfirm = "ok";
  ok.closest = sel => (sel === "[data-wconfirm]" || sel.includes("data-wconfirm") ? ok : null);
  confirm.dispatch("click", { target: ok });
  await settle();
  assert.ok(apiLog.some(x => x.method === "DELETE"));
  assert.equal(roots.notesworkspace.classList.contains("note-open"), false);
});

/* ---------- card drag suppresses rebuild ---------- */
test("renderCards skips while dragging", async () => {
  const ctx = createFeature({
    notes: [
      { id: "n1", title: "A", order: 0 },
      { id: "n2", title: "B", order: 1 },
    ],
  });
  const { feature, roots } = ctx;
  feature.bind();
  feature.open();
  await settle();
  const before = roots.wscards.innerHTML;
  const card = el("button", { className: "wscard", dataset: { note: "n1" } });
  card.dataset.note = "n1";
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.dispatch("dragstart", { target: card, dataTransfer: { setData(){}, effectAllowed: "" } });
  roots.wscards.innerHTML = "DIRTY";
  feature.close();
  feature.open(); /* starts the production card load while the drag guard is set */
  await settle();
  assert.equal(roots.wscards.innerHTML, "DIRTY", "mid-drag load must not rebuild");
  roots.wscards.dispatch("dragend");
  await Promise.resolve();
  await Promise.resolve();
  assert.notEqual(roots.wscards.innerHTML, "DIRTY");
  assert.ok(before.includes("wscard") || roots.wscards.innerHTML.includes("wscard") || roots.wscards.innerHTML.includes("A"));
});

/* ---------- inbox render + placement ---------- */
test("inbox tabs/list render; use-in-note starts placement", async () => {
  const ctx = createFeature({
    bookmarks: [
      { t: "1", text: "hello bookmark" }, /* General lane (no lane id) */
      { t: "2", text: "laned", lane: "lane-a" },
    ],
    lanes: [{ id: "lane-a", name: "Alpha", color: "#f00" }],
  });
  const { feature, roots } = ctx;
  feature.bind();
  feature.open();
  await settle();
  assert.match(roots.wsinboxtabs.innerHTML, /data-wsitab="GENERAL"/);
  assert.match(roots.wsinboxtabs.innerHTML, /data-wsitab="lane-a"/);
  assert.match(roots.wsinboxlist.innerHTML, /hello bookmark/);
  /* closed until tapped: the actions arrive with the first tap on the card */
  assert.doesNotMatch(roots.wsinboxlist.innerHTML, /data-bmact="note"/);

  const card = el("div", { className: "wsibookmark", dataset: { t: "1" } });
  const tap = { closest: sel => (sel === ".wsibookmark" ? card : null) };
  roots.wsinboxlist.dispatch("click", { target: tap });
  assert.match(roots.wsinboxlist.innerHTML, /data-bmact="note"/);
  /* and a second tap on the same card folds it away again */
  roots.wsinboxlist.dispatch("click", { target: tap });
  assert.doesNotMatch(roots.wsinboxlist.innerHTML, /data-bmact="note"/);
  roots.wsinboxlist.dispatch("click", { target: tap });

  const use = el("button");
  use.dataset.bmact = "note";
  const row = el("div", { className: "wsibookmark", dataset: { t: "1" } });
  row.dataset.t = "1";
  use.closest = sel => {
    if (sel === "[data-bmact]") return use;
    if (sel === ".wsibookmark") return row;
    return null;
  };
  roots.wsinboxlist.dispatch("click", { target: use });
  assert.ok(roots.notesworkspace.classList.contains("placing"));
  assert.equal(roots.wsplacetext.textContent, "hello bookmark");
});

test("placement payload uses bookmarkSource + snapshot; targeted path available", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "Note", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "Note",
        sections: [{ id: "s1", title: "S", body: "body", order: 0, references: [] }],
      },
    },
    bookmarks: [{ t: "1", text: "cap", uid: "u1", segment: 1, record: 2, node: "nX", turnTime: "TT", lane: "lane-a" }],
    lanes: [{ id: "lane-a", name: "A", color: "#f00" }],
    nodes: { nX: { id: "nX", title: "Station", lane_id: "lane-a" } },
  });
  const { feature, roots, apiLog, effects } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");

  feature.startPlacement({
    t: "1", text: "cap", uid: "u1", segment: 1, record: 2, node: "nX", turnTime: "TT", lane: "lane-a",
  });
  /* place via addhere */
  const add = el("button");
  add.dataset.addhere = "";
  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  sec.dataset.sec = "s1";
  const body = el("div", { className: "wssecbody" });
  sec.appendChild(body);
  add.closest = sel => {
    if (sel === "[data-addhere]") return add;
    if (sel === ".wssec") return sec;
    return null;
  };
  roots.wssections.dispatch("click", { target: add });
  await settle();
  const post = apiLog.find(x => x.method === "POST" && x.url.includes("/references"));
  assert.ok(post);
  const payload = JSON.parse(post.body);
  assert.deepEqual(payload.source, {
    uid: "u1", segment: 1, record: 2, node: "nX", turnTime: "TT",
  });
  assert.equal(payload.snapshot.text, "cap");
  assert.equal(payload.snapshot.station, "Station");
  assert.equal(payload.snapshot.lane, "#f00");
  assert.ok(effects.toast.some(t => t.startsWith("Added to")));
  assert.equal(roots.notesworkspace.classList.contains("placing"), false);
});

/* ---------- reference copy / jump / trash (P3: trash confirms first) ---------- */
test("reference copy, jump, and in-place trash", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "N", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "N",
        sections: [{
          id: "s1", title: "", body: "x", order: 0,
          references: [{
            id: "r1",
            source: { node: "nX", uid: "u", segment: 0, record: 0 },
            snapshot: { text: "snap text", lane: "#f00" },
          }],
        }],
      },
    },
  });
  const { feature, roots, effects, apiLog, flush } = ctx;
  feature.bind();
  feature.open();
  await settle();
  const card = el("button", { className: "wscard", dataset: { note: "n1" } });
  card.dataset.note = "n1";
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.dispatch("click", { target: card });
  await Promise.resolve();

  /* build minimal DOM for actions */
  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  sec.dataset.sec = "s1";
  const ref = el("div", { className: "wsref", dataset: { ref: "r1" } });
  ref.dataset.ref = "r1";
  const wrap = el("div", { className: "wsrefs" });
  wrap.appendChild(ref);
  sec.appendChild(wrap);
  roots.wssections.appendChild(sec);

  const copyBtn = el("button", { dataset: { refact: "copy" } });
  copyBtn.dataset.refact = "copy";
  copyBtn.closest = sel => {
    if (sel === "[data-refact]") return copyBtn;
    if (sel === ".wsref") return ref;
    if (sel === ".wssec") return sec;
    return null;
  };
  roots.wssections.dispatch("click", { target: copyBtn });
  assert.deepEqual(effects.copy, ["snap text"]);
  flush(900);

  const jumpBtn = el("button", { dataset: { refact: "jump" } });
  jumpBtn.dataset.refact = "jump";
  jumpBtn.closest = sel => {
    if (sel === "[data-refact]") return jumpBtn;
    if (sel === ".wsref") return ref;
    if (sel === ".wssec") return sec;
    return null;
  };
  roots.wssections.dispatch("click", { target: jumpBtn });
  assert.equal(effects.jump.length, 1);
  assert.equal(effects.jump[0].uid, "u");

  /* P3: trash is inline + .danger and requires confirm (no window.confirm) */
  const trashBtn = el("button", { dataset: { refact: "trash" } });
  trashBtn.dataset.refact = "trash";
  trashBtn.closest = sel => {
    if (sel === "[data-refact]") return trashBtn;
    if (sel === ".wsref") return ref;
    if (sel === ".wssec") return sec;
    return null;
  };
  trashBtn.getBoundingClientRect = () =>
    ({ top: 100, left: 100, bottom: 144, right: 144, width: 44, height: 44 });
  roots.wspanel.getBoundingClientRect = () =>
    ({ top: 0, left: 0, width: 400, height: 600 });
  let removed = false;
  ref.remove = () => { removed = true; wrap.children = []; };
  roots.wssections.dispatch("click", { target: trashBtn });
  await Promise.resolve();
  assert.ok(!apiLog.some(x => x.method === "DELETE"),
    "trash must not DELETE before confirm");
  const confirm = roots.wspanel.children.find(c => (c.className || "").includes("popmenu"));
  assert.ok(confirm, "confirm popover opens");
  assert.match(confirm.className, /wsconfirm/);
  const ok = el("button", { dataset: { wconfirm: "ok" } });
  ok.dataset.wconfirm = "ok";
  ok.closest = sel => (sel === "[data-wconfirm]" ? ok : null);
  confirm.dispatch("click", { target: ok });
  await Promise.resolve();
  await Promise.resolve();
  assert.ok(apiLog.some(x => x.method === "DELETE" && x.url.includes("/references/r1")));
  assert.equal(removed, true);
});

/* ---------- narrow swipe ---------- */
test("narrow swipe applies endPlacement / noteToList / close", () => {
  const ctx = createFeature({ isNarrow: true });
  const { feature, roots } = ctx;
  feature.bind();
  feature.open();
  feature.startPlacement({ t: "1", text: "x" });
  assert.ok(roots.notesworkspace.classList.contains("placing"));

  roots.notesworkspace.dispatch("touchstart", {
    touches: [{ clientX: 10, clientY: 100 }],
    target: roots.wscards,
  });
  roots.notesworkspace.dispatch("touchend", {
    changedTouches: [{ clientX: 100, clientY: 100 }],
  });
  assert.equal(roots.notesworkspace.classList.contains("placing"), false);

  roots.notesworkspace.classList.add("note-open");
  roots.wsback.hidden = false;
  let backClicks = 0;
  roots.wsback.click = () => { backClicks++; roots.wsback.dispatch("click"); };
  roots.notesworkspace.dispatch("touchstart", {
    touches: [{ clientX: 10, clientY: 100 }],
    target: roots.wscards,
  });
  roots.notesworkspace.dispatch("touchend", {
    changedTouches: [{ clientX: 100, clientY: 100 }],
  });
  assert.equal(backClicks, 1);
  assert.equal(roots.notesworkspace.classList.contains("note-open"), false);

  roots.notesworkspace.dispatch("touchstart", {
    touches: [{ clientX: 10, clientY: 100 }],
    target: roots.wscards,
  });
  roots.notesworkspace.dispatch("touchend", {
    changedTouches: [{ clientX: 100, clientY: 100 }],
  });
  assert.equal(feature.isOpen(), false);
});

/* ---------- bind/destroy ---------- */
test("bind is idempotent; destroy cleans document listener and timers", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "T", order: 0 }],
    docs: { n1: { id: "n1", title: "T", sections: [] } },
  });
  const { feature, document, roots, timers, flush } = ctx;
  feature.bind();
  feature.bind();
  assert.equal((roots.wsclose._listeners.click || []).length, 1);
  assert.equal((document._listeners.click || []).length, 1);

  feature.open();
  flush(0); /* drain open rAF so only save timers remain pending later */
  await settle();
  await openNote(ctx, "n1");
  /* schedule a debounced section-title save to prove destroy clears timers */
  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  sec.dataset.sec = "s1";
  const title = el("input", { dataset: { sectitle: "" }, value: "S" });
  title.dataset.sectitle = "";
  title.closest = sel => {
    if (sel === "[data-sectitle]") return title;
    if (sel === ".wssec") return sec;
    return null;
  };
  sec.appendChild(title);
  roots.wssections.appendChild(sec);
  const timerCountBefore = timers.length;
  roots.wssections.dispatch("focusin", { target: title });
  title.value = "X";
  roots.wssections.dispatch("input", { target: title });
  const saveTimers = timers.slice(timerCountBefore).filter(t => !t.cleared);
  assert.ok(saveTimers.length >= 1, "debounce timer scheduled");
  feature.destroy();
  assert.equal((document._listeners.click || []).length, 0);
  assert.equal((roots.wsclose._listeners.click || []).length, 0);
  assert.ok(saveTimers.every(t => t.cleared), "destroy clears save timers");
});

/* ---------- fold storage with error semantics ---------- */
test("fold storage get/set errors are swallowed", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "T", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "T",
        sections: [{ id: "s1", title: "S", body: "", order: 0, references: [] }],
      },
    },
  });
  const { feature, roots, storage } = ctx;
  feature.bind();
  feature.open();
  await settle();
  const card = el("button", { className: "wscard", dataset: { note: "n1" } });
  card.dataset.note = "n1";
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.dispatch("click", { target: card });
  await Promise.resolve();

  /* inject section DOM */
  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  sec.dataset.sec = "s1";
  const foldBtn = el("button");
  foldBtn.dataset.secfold = "";
  foldBtn.closest = sel => {
    if (sel === "[data-secfold]") return foldBtn;
    if (sel === ".wssec") return sec;
    return null;
  };
  roots.wssections.appendChild(sec);
  roots.wssections.dispatch("click", { target: foldBtn });
  assert.equal(storage.getItem(STORAGE_KEY_FOLDS), JSON.stringify({ s1: 1 }));

  let threw = false;
  storage.setItem = () => { threw = true; throw new Error("quota"); };
  roots.wssections.dispatch("click", { target: foldBtn });
  assert.equal(threw, true, "setItem was attempted");

  storage.getItem = () => { throw new Error("private mode"); };
  assert.doesNotThrow(() => roots.wssections.dispatch("click", { target: foldBtn }));
});

/* ---------- Phase 1h: empty states share vertical centre across zones 2 & 3 ---------- */
test("zone-2 and zone-3 empty states share absolute full-column centring", () => {
  /* #wsnav and #wsnote are the positioned ancestors of equal-height columns. */
  assert.match(notesCssSrc, /#wsnav\s*\{[^}]*position:\s*relative/);
  assert.match(notesCssSrc, /#wsnote\s*\{[^}]*position:\s*relative/);

  const navEmpty = notesCssSrc.match(/#wscards\s+\.empty\s*\{([^}]*)\}/);
  const noteEmpty = notesCssSrc.match(/#wsnoteempty\s*\{([^}]*)\}/);
  assert.ok(navEmpty, "#wscards .empty rule present");
  assert.ok(noteEmpty, "#wsnoteempty rule present");
  for (const [name, block] of [["#wscards .empty", navEmpty[1]], ["#wsnoteempty", noteEmpty[1]]]) {
    assert.match(block, /position:\s*absolute/, `${name} is absolute overlay`);
    assert.match(block, /inset:\s*0/, `${name} fills the column`);
    assert.match(block, /align-items:\s*center/);
    assert.match(block, /justify-content:\s*center/);
  }
});

/* ---------- Phase 1g: zone-3 header removed; top bar is sole title home ---------- */
test("no #wsnotehead / #wsnotetitle / #wsnotemenu; #wstitle shows note title", async () => {
  assert.doesNotMatch(indexSrc, /id="wsnotehead"/);
  assert.doesNotMatch(indexSrc, /id="wsnotetitle"/);
  assert.doesNotMatch(indexSrc, /id="wsnotemenu"/);
  assert.doesNotMatch(notesSrc, /wsnotehead|wsnotetitle|wsnotemenu|onNoteMenuClick|data-si=/);
  assert.doesNotMatch(notesCssSrc, /#wsnotehead|#wsnotetitle|#wsnotemenu/);

  const ctx = createFeature({
    notes: [{ id: "n1", title: "Alpha", order: 0 }],
    docs: { n1: { id: "n1", title: "Alpha", sections: [] } },
  });
  ctx.feature.bind();
  ctx.feature.open();
  await settle();
  assert.equal(ctx.roots.wstitle.textContent, "Notes");
  await openNote(ctx, "n1");
  assert.equal(ctx.roots.wstitle.textContent, "Alpha");
  assert.equal(ctx.roots.wsnotehead, undefined);
  assert.equal(ctx.roots.wsnotetitle, undefined);
  assert.equal(ctx.roots.wsnotemenu, undefined);
});

/* ---------- Phase 1c: Rename / Delete on zone-2 note cards ---------- */
test("card Enter/Space activates wsSelect (keyboard a11y after div role=button)", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "Alpha", order: 0 }],
    docs: { n1: { id: "n1", title: "Alpha", sections: [] } },
  });
  const { feature, roots } = ctx;
  feature.bind();
  feature.open();
  await settle();
  assert.equal(roots.wstitle.textContent, "Notes");

  const card = el("div", { className: "wscard", dataset: { note: "n1" } });
  card.dataset.note = "n1";
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.appendChild(card);

  let prevented = false;
  roots.wscards.dispatch("keydown", {
    key: "Enter",
    target: card,
    preventDefault(){ prevented = true; },
  });
  await settle();
  assert.equal(prevented, true);
  assert.equal(roots.wstitle.textContent, "Alpha");

  /* Space also works; action buttons / title edit must not re-select */
  const card2 = el("div", { className: "wscard", dataset: { note: "n1" } });
  card2.dataset.note = "n1";
  card2.closest = sel => (sel === ".wscard" ? card2 : null);
  const act = el("button", { dataset: { wcact: "rename" } });
  act.dataset.wcact = "rename";
  act.closest = sel => {
    if (sel === "[data-wcact]" || String(sel).includes("data-wcact")) return act;
    if (sel === ".wscard") return card2;
    if (sel === ".wctitleedit") return null;
    return null;
  };
  roots.wscards.appendChild(card2);
  roots.wscards.dispatch("keydown", {
    key: " ",
    target: act,
    preventDefault(){},
  });
  await settle();
  /* action keydown must not force select; no throw, title still Alpha from prior select */
  assert.equal(roots.wstitle.textContent, "Alpha");
});

test("card rename: not draggable while editing; commit PATCHes title and updates #wstitle", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "Orig", order: 0 }],
    docs: { n1: { id: "n1", title: "Orig", sections: [] } },
  });
  const { feature, roots, apiLog } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");
  assert.equal(roots.wstitle.textContent, "Orig");

  /* build a card with actions the way renderCards would (fake DOM has no HTML parse) */
  const card = el("div", { className: "wscard", dataset: { note: "n1" }, draggable: true });
  card.dataset.note = "n1";
  card.draggable = true;
  const title = el("div", { className: "wctitle", textContent: "Orig" });
  card.appendChild(title);
  const renameBtn = el("button", { dataset: { wcact: "rename" } });
  renameBtn.dataset.wcact = "rename";
  renameBtn.closest = sel => {
    if (sel === "[data-wcact]" || sel === "[data-wcact=\"rename\"]") return renameBtn;
    if (sel === ".wscard") return card;
    return null;
  };
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.appendChild(card);

  let stopped = false;
  roots.wscards.dispatch("click", {
    target: renameBtn,
    stopPropagation(){ stopped = true; },
    preventDefault(){},
  });
  assert.equal(stopped, true, "action click must not select/reorder");
  assert.equal(card.draggable, false, "card is not draggable while renaming");
  assert.equal(noteCardDragEnabled(true), false);

  const input = card.querySelector("input") || card.querySelector(".wctitleedit");
  assert.ok(input, "rename swaps title for an input");
  input.value = "Renamed";
  input.dispatch("keydown", {
    key: "Enter", target: input,
    preventDefault(){}, stopPropagation(){},
  });
  await settle();

  const patch = apiLog.find(x => x.method === "PATCH" && x.url.includes("/api/notes/n1"));
  assert.ok(patch, "rename commits a PATCH");
  assert.equal(JSON.parse(patch.body).title, "Renamed");
  assert.equal(roots.wstitle.textContent, "Renamed");
});

test("card delete opens archive-aware confirm; cancel skips DELETE", async () => {
  const ctx = createFeature({
    notes: [
      { id: "n1", title: "Keep", order: 0 },
      { id: "n2", title: "Gone", order: 1 },
    ],
    docs: {
      n1: { id: "n1", title: "Keep", sections: [] },
      n2: { id: "n2", title: "Gone", sections: [] },
    },
  });
  const { feature, roots, apiLog } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");

  const card = el("div", { className: "wscard", dataset: { note: "n2" } });
  card.dataset.note = "n2";
  const delBtn = el("button", { className: "danger", dataset: { wcact: "delete" } });
  delBtn.dataset.wcact = "delete";
  delBtn.closest = sel => {
    if (sel === "[data-wcact]" || sel.includes("data-wcact")) return delBtn;
    if (sel === ".wscard") return card;
    return null;
  };
  delBtn.getBoundingClientRect = () => ({ top: 10, left: 10, bottom: 50, height: 40, width: 44 });
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.appendChild(card);
  roots.wspanel.getBoundingClientRect = () => ({ top: 0, left: 0, width: 400, height: 600 });

  roots.wscards.dispatch("click", {
    target: delBtn,
    stopPropagation(){},
    preventDefault(){},
  });
  await settle();

  assert.equal(
    apiLog.some(x => x.method === "DELETE"),
    false,
    "delete must not fire before confirm",
  );
  const menu = roots.wspanel.children.find(c => (c.className || "").includes("popmenu"));
  assert.ok(menu, "confirm popover opens");
  assert.match(menu.innerHTML, /archive/i);
  assert.match(menu.innerHTML, /data-wconfirm="cancel"/);
  assert.match(menu.innerHTML, /data-wconfirm="ok"/);

  const cancel = el("button", { dataset: { wconfirm: "cancel" } });
  cancel.dataset.wconfirm = "cancel";
  cancel.closest = sel => (sel === "[data-wconfirm]" || sel.includes("data-wconfirm") ? cancel : null);
  menu.dispatch("click", { target: cancel });
  await settle();
  assert.equal(apiLog.some(x => x.method === "DELETE"), false, "cancel skips DELETE");
});

test("card delete confirm issues DELETE for that note id", async () => {
  const ctx = createFeature({
    notes: [
      { id: "n1", title: "Keep", order: 0 },
      { id: "n2", title: "Gone", order: 1 },
    ],
    docs: {
      n1: { id: "n1", title: "Keep", sections: [] },
      n2: { id: "n2", title: "Gone", sections: [] },
    },
  });
  const { feature, roots, apiLog } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");

  const card = el("div", { className: "wscard", dataset: { note: "n2" } });
  card.dataset.note = "n2";
  const delBtn = el("button", { className: "danger", dataset: { wcact: "delete" } });
  delBtn.dataset.wcact = "delete";
  delBtn.closest = sel => {
    if (sel === "[data-wcact]" || sel.includes("data-wcact")) return delBtn;
    if (sel === ".wscard") return card;
    return null;
  };
  delBtn.getBoundingClientRect = () => ({ top: 10, left: 10, bottom: 50, height: 40, width: 44 });
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.appendChild(card);
  roots.wspanel.getBoundingClientRect = () => ({ top: 0, left: 0, width: 400, height: 600 });

  roots.wscards.dispatch("click", {
    target: delBtn,
    stopPropagation(){},
    preventDefault(){},
  });
  await settle();

  const menu = roots.wspanel.children.find(c => (c.className || "").includes("popmenu"));
  assert.ok(menu);
  const ok = el("button", { dataset: { wconfirm: "ok" } });
  ok.dataset.wconfirm = "ok";
  ok.closest = sel => (sel === "[data-wconfirm]" || sel.includes("data-wconfirm") ? ok : null);
  menu.dispatch("click", { target: ok });
  await settle();

  const del = apiLog.find(x => x.method === "DELETE" && x.url.includes("/api/notes/n2"));
  assert.ok(del, "confirm issues DELETE for the card's note");
  assert.equal(roots.wstitle.textContent, "Keep");
});

/* P5 review: the two call-site offsets were unpinned after the lift. The pure
   menuPlacement is covered at both offsets, but nothing bound notes.js to the
   RIGHT one — swapping 150 and 40 left the whole suite green while the confirm
   popover moved 110px. The existing confirm fixtures cannot see it: with an
   anchor at left 10 both offsets clamp to the same 8px floor. Anchor far enough
   right that the clamp does not bite and the offsets separate:
     offset 40  -> max(8, min(300 - 40, 400 - 190)) = 210
     offset 150 -> max(8, min(300 - 150, 400 - 190)) = 150 */
test("P5 review: openDeleteConfirm positions with offset 40, not the section menu's 150", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "Keep", order: 0 }, { id: "n2", title: "Gone", order: 1 }],
    docs: {
      n1: { id: "n1", title: "Keep", sections: [] },
      n2: { id: "n2", title: "Gone", sections: [] },
    },
  });
  const { feature, roots } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");

  const card = el("div", { className: "wscard", dataset: { note: "n2" } });
  card.dataset.note = "n2";
  const delBtn = el("button", { className: "danger", dataset: { wcact: "delete" } });
  delBtn.dataset.wcact = "delete";
  delBtn.closest = sel => {
    if (sel === "[data-wcact]" || sel.includes("data-wcact")) return delBtn;
    if (sel === ".wscard") return card;
    return null;
  };
  delBtn.getBoundingClientRect = () => ({ top: 260, left: 300, bottom: 300, height: 40, width: 44 });
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.appendChild(card);
  roots.wspanel.getBoundingClientRect = () => ({ top: 0, left: 0, width: 400, height: 600 });

  roots.wscards.dispatch("click", { target: delBtn, stopPropagation(){}, preventDefault(){} });
  await settle();

  const menu = roots.wspanel.children.find(c => (c.className || "").includes("popmenu"));
  assert.ok(menu, "confirm popover opens");
  assert.equal(menu.style.left, "210px", "offset 40 — 150 would put it at 150px");
  assert.equal(menu.style.top, "304px");
});

/* Source-level companion: there is no harness that opens the section menu, so
   the 150 side of the swap is pinned where it is written. Binds the construct
   to its offset rather than asserting the two numbers exist somewhere. */
test("P5 review: each popover call site keeps its own offset (150 section, 40 confirm)", () => {
  const section = notesSrc.slice(notesSrc.indexOf("function openSectionMenu"));
  assert.match(section.slice(0, 900), /offset:\s*150/);
  const confirm = notesSrc.slice(notesSrc.indexOf("function openDeleteConfirm"));
  assert.match(confirm.slice(0, 900), /offset:\s*40/);
});

/* ---------- section title Enter/Escape contract in source ---------- */
test("source: debounce 1100, PatchNow reverts, sectionBodyInner on blur", () => {
  assert.match(notesSrc, /SAVE_DEBOUNCE_MS = 1100/);
  assert.match(notesSrc, /wsPatchNow\(\{ section: \{ id: secId, title: orig \}/);
  assert.match(notesSrc, /wsPatchNow\(\{ section: \{ id: secId, body: orig \}/);
  assert.match(notesSrc, /sectionBodyInner\(s/);
  assert.match(notesSrc, /scrollHeight/);
  /* note title is card-rename only (immediate PATCH), not debounced zone-3 input */
  assert.doesNotMatch(notesSrc, /wsTitleOrig|note-title/);
});

/* ---------- lazy Bookmarks callbacks without import cycle ---------- */
test("bookmarks does not import notes; notes may import bookmark pure helpers", () => {
  assert.doesNotMatch(bookmarksSrc, /from ["'].*notes/);
  assert.match(notesSrc, /bookmarkLaneId|bookmarkSortKey|bookmarkClampState/);
  assert.match(notesSrc, /startPlacement/);
});

/* ---------- poll/edit non-clobber decision (source + drag guard) ---------- */
test("renderNote is not invoked from poll path; drag guards cards", () => {
  assert.match(notesSrc, /if \(wsCardDragging\) return/);
  assert.match(notesSrc, /DOM text is authoritative|never rebuilds\s+focused editors|Editors never rebuilt by polling/i);
});

/* ---------- Phase 1f: always-present trailing Add Section ---------- */
test("sections render always includes trailing data-addsection (even with sections)", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "N", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "N",
        sections: [
          { id: "s1", title: "One", body: "a", order: 0, references: [] },
          { id: "s2", title: "Two", body: "b", order: 1, references: [] },
        ],
      },
    },
  });
  ctx.feature.bind();
  ctx.feature.open();
  await settle();
  await openNote(ctx, "n1");
  const html = ctx.roots.wssections.innerHTML;
  assert.match(html, /data-addsection/);
  assert.match(html, /Add section/i);
  const lastSec = html.lastIndexOf("data-sec=");
  const add = html.lastIndexOf("data-addsection");
  assert.ok(add > lastSec, "Add Section is the trailing block after sections");
  /* empty (zero-section) path still offers the control */
  const emptyCtx = createFeature({
    notes: [{ id: "n2", title: "E", order: 0 }],
    docs: { n2: { id: "n2", title: "E", sections: [] } },
  });
  emptyCtx.feature.bind();
  emptyCtx.feature.open();
  await settle();
  await openNote(emptyCtx, "n2");
  assert.match(emptyCtx.roots.wssections.innerHTML, /data-addsection/);
});

test("trailing Add Section PATCHes add_section", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "N", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "N",
        sections: [{ id: "s1", title: "One", body: "", order: 0, references: [] }],
      },
    },
  });
  const { feature, roots, apiLog } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");

  const btn = el("button", { dataset: { addsection: "" } });
  btn.dataset.addsection = "";
  btn.closest = sel => (sel === "[data-addsection]" ? btn : null);
  roots.wssections.dispatch("click", { target: btn });
  await settle();
  const patch = apiLog.find(x => x.method === "PATCH" && x.url.includes("/api/notes/n1"));
  assert.ok(patch);
  assert.equal(JSON.parse(patch.body).add_section, "");
});

/* ---------- Phase 3b: body edit completion signals commit for git ---------- */
test("body edit: mid-edit autosave omits commit; blur completing PATCH sets commit:true", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "N", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "N",
        sections: [{ id: "s1", title: "S", body: "original", order: 0, references: [] }],
      },
    },
  });
  ctx.feature.bind();
  ctx.feature.open();
  await settle();
  await openNote(ctx, "n1");

  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  const body = el("div", { className: "wssecbody" });
  const render = el("div", { dataset: { secrender: "" } });
  sec.appendChild(body);
  body.appendChild(render);
  ctx.roots.wssections.appendChild(sec);
  ctx.roots.wssections.dispatch("click", { target: render });

  const textarea = body.querySelector("textarea");
  assert.ok(textarea, "body click must enter edit mode");
  const before = ctx.apiLog.length;
  textarea.value = "draft mid-edit";
  textarea.dispatch("input", { target: textarea });
  ctx.flush(SAVE_DEBOUNCE_MS);
  await settle();

  const mid = ctx.apiLog.slice(before).filter(x => x.method === "PATCH");
  assert.ok(mid.length >= 1, "debounced autosave fires");
  for (const p of mid){
    const parsed = JSON.parse(p.body);
    assert.equal(parsed.section.body, "draft mid-edit");
    assert.equal(parsed.section.commit, undefined, "mid-edit autosave must omit commit");
  }

  const beforeBlur = ctx.apiLog.length;
  textarea.value = "final body";
  textarea.dispatch("blur", { target: textarea });
  await settle();
  const completing = ctx.apiLog.slice(beforeBlur).filter(x => x.method === "PATCH");
  assert.ok(completing.length >= 1, "blur issues completing PATCH");
  const last = JSON.parse(completing[completing.length - 1].body);
  assert.equal(last.section.body, "final body");
  assert.equal(last.section.commit, true, "completing body PATCH must set commit:true");
});

/* ---------- Phase 3 follow-up: section-title completion signals commit ---------- */
test("section title: mid-edit autosave omits commit; blur completing PATCH sets commit:true", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "N", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "N",
        sections: [{ id: "s1", title: "Original", body: "b", order: 0, references: [] }],
      },
    },
  });
  const { feature, roots, apiLog, flush } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");

  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  sec.dataset.sec = "s1";
  const title = el("input", { dataset: { sectitle: "" }, value: "Original" });
  title.dataset.sectitle = "";
  title.value = "Original";
  title.closest = sel => {
    if (sel === "[data-sectitle]") return title;
    if (sel === ".wssec") return sec;
    return null;
  };
  sec.appendChild(title);
  roots.wssections.appendChild(sec);

  roots.wssections.dispatch("focusin", { target: title });
  const before = apiLog.length;
  title.value = "Draft mid-edit";
  roots.wssections.dispatch("input", { target: title });
  flush(SAVE_DEBOUNCE_MS);
  await settle();

  const mid = apiLog.slice(before).filter(x => x.method === "PATCH");
  assert.ok(mid.length >= 1, "debounced section-title autosave fires");
  for (const p of mid){
    const parsed = JSON.parse(p.body);
    assert.equal(parsed.section.title, "Draft mid-edit");
    assert.equal(parsed.section.commit, undefined, "mid-edit title autosave must omit commit");
  }

  const beforeBlur = apiLog.length;
  title.value = "Final title";
  /* focusout is the delegated completion path (Enter already blurs). */
  roots.wssections.dispatch("focusout", { target: title });
  await settle();
  const completing = apiLog.slice(beforeBlur).filter(x => x.method === "PATCH");
  assert.ok(completing.length >= 1, "focusout issues completing section-title PATCH");
  const last = JSON.parse(completing[completing.length - 1].body);
  assert.equal(last.section.title, "Final title");
  assert.equal(last.section.commit, true, "completing section-title PATCH must set commit:true");
});

/* ---------- body edit preserves references via sectionBodyInner ---------- */
test("body edit Escape restores rendered body and references", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "N", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "N",
        sections: [{
          id: "s1", title: "S", body: "original", order: 0,
          references: [{
            id: "r1",
            snapshot: { station: "Alpha", speaker: "you", text: "quoted" },
          }],
        }],
      },
    },
  });
  ctx.feature.bind();
  ctx.feature.open();
  await settle();
  await openNote(ctx, "n1");

  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  const body = el("div", { className: "wssecbody" });
  const render = el("div", { dataset: { secrender: "" } });
  sec.appendChild(body);
  body.appendChild(render);
  ctx.roots.wssections.appendChild(sec);
  ctx.roots.wssections.dispatch("click", { target: render });

  const textarea = body.querySelector("textarea");
  assert.ok(textarea, "body click must enter edit mode");
  textarea.value = "changed";
  let prevented = false;
  let stopped = false;
  textarea.dispatch("keydown", {
    key: "Escape",
    preventDefault(){ prevented = true; },
    stopPropagation(){ stopped = true; },
  });
  await settle();
  assert.equal(prevented, true);
  assert.equal(stopped, true);
  assert.match(body.innerHTML, /original/);
  assert.match(body.innerHTML, /data-ref="r1"/);
  assert.match(body.innerHTML, /quoted/);
});

/* ---------- Phase 2b/2c: dividers, collapse-to-peek, localStorage ---------- */

function syncZoneRects(roots){
  const iw = parseFloat(roots.wszones.style.getPropertyValue("--wsinbox-w")) || 240;
  const nw = parseFloat(roots.wszones.style.getPropertyValue("--wsnav-w")) || 220;
  const noteW = 900 - iw - nw;
  roots.wsinbox._rect = { top: 0, left: 0, width: iw, height: 400, right: iw, bottom: 400 };
  roots.wsnav._rect = { top: 0, left: iw, width: nw, height: 400, right: iw + nw, bottom: 400 };
  roots.wsnote._rect = { top: 0, left: iw + nw, width: noteW, height: 400, right: 900, bottom: 400 };
}

test("index.html has two vertical separators; CSS uses custom-prop widths ≥768", () => {
  assert.match(indexSrc, /class="wsdivider"[^>]*data-boundary="inbox"|data-boundary="inbox"[^>]*class="wsdivider"/);
  assert.match(indexSrc, /data-boundary="nav"/);
  assert.match(indexSrc, /role="separator"/);
  assert.match(indexSrc, /aria-orientation="vertical"/);
  assert.match(notesCssSrc, /--wsinbox-w/);
  assert.match(notesCssSrc, /--wsnav-w/);
  assert.match(notesCssSrc, /\.wsdivider/);
  assert.match(notesCssSrc, /cursor:\s*col-resize/);
  assert.match(notesCssSrc, /touch-action:\s*none/);
  /* collapsed rail uses peek; dividers only in min-width 768 block as display:block */
  assert.match(notesCssSrc, /#wsinbox\.collapsed|#wsnav\.collapsed/);
  assert.match(notesCssSrc, /var\(--peek\)/);
  const m768 = notesCssSrc.match(/@media\s*\(min-width:\s*768px\)\s*\{([\s\S]*?)@media\s*\(min-width:\s*900px\)/);
  assert.ok(m768, "768px media block present");
  assert.match(m768[1], /\.wsdivider\s*\{[^}]*display:\s*block/);
  assert.match(m768[1], /var\(--wsinbox-w,\s*clamp/);
  assert.match(m768[1], /var\(--wsnav-w,\s*clamp/);
  /* phone: divider default is display:none outside the media query */
  assert.match(notesCssSrc, /\.wsdivider\s*\{[^}]*display:\s*none/);
});

test("each divider carries a centred grab pill as its only affordance", () => {
  /* ::after is the hairline; ::before is the pill the user aims at. */
  const pill = notesCssSrc.match(/\.wsdivider::before\s*\{([^}]*)\}/);
  assert.ok(pill, ".wsdivider::before pill rule present");
  assert.match(pill[1], /border-radius/);
  /* vertically centred on the frame line */
  assert.match(pill[1], /top:\s*50%/);
  assert.match(pill[1], /left:\s*50%/);
  assert.match(pill[1], /translate\(\s*-50%\s*,\s*-50%\s*\)/);
  /* the pill is decoration: the whole divider stays the hit target */
  assert.match(pill[1], /pointer-events:\s*none/);
  /* no chevron/collapse buttons on the frame lines — the pill is it */
  const dividers = indexSrc.match(/<div class="wsdivider"[\s\S]*?<\/div>/g) || [];
  assert.equal(dividers.length, 2);
  for (const d of dividers){
    assert.equal(/<button|chevron/.test(d), false, "divider carries no buttons");
  }
});

test("divider drag reallocates live and persists layout on pointerup", async () => {
  const ctx = createFeature({});
  const { feature, roots, storage } = ctx;
  feature.bind();
  feature.open();
  await settle();

  const handle = roots.divInbox;
  /* drag inbox boundary right by 20px → inbox 260, nav 200 (both ≥ mins) */
  handle.dispatch("pointerdown", {
    target: handle, pointerId: 1, clientX: 240, button: 0,
    preventDefault(){},
  });
  handle.dispatch("pointermove", {
    target: handle, pointerId: 1, clientX: 260,
  });
  assert.equal(roots.wszones.style.getPropertyValue("--wsinbox-w"), "260px");
  assert.equal(roots.wszones.style.getPropertyValue("--wsnav-w"), "200px");
  assert.equal(roots.wsinbox.classList.contains("collapsed"), false);
  assert.equal(roots.wsnav.classList.contains("collapsed"), false);

  handle.dispatch("pointerup", { target: handle, pointerId: 1, clientX: 260 });
  const raw = storage.getItem(STORAGE_KEY_WS_LAYOUT);
  assert.ok(raw, "layout persisted");
  const L = JSON.parse(raw);
  assert.equal(L.inboxW, 260);
  assert.equal(L.navW, 200);
  assert.equal(L.inboxCollapsed, false);
  assert.equal(L.navCollapsed, false);
});

test("divider drag past min collapses to peek; tap rail restores", async () => {
  const ctx = createFeature({});
  const { feature, roots, storage } = ctx;
  feature.bind();
  feature.open();
  await settle();

  const handle = roots.divInbox;
  handle.dispatch("pointerdown", {
    target: handle, pointerId: 1, clientX: 240, button: 0, preventDefault(){},
  });
  handle.dispatch("pointermove", {
    target: handle, pointerId: 1, clientX: 80, /* well below min 210 */
  });
  assert.equal(roots.wsinbox.classList.contains("collapsed"), true);
  assert.equal(roots.wszones.style.getPropertyValue("--wsinbox-w"), WS_LAYOUT_PEEK + "px");
  handle.dispatch("pointerup", { target: handle, pointerId: 1, clientX: 80 });

  let L = JSON.parse(storage.getItem(STORAGE_KEY_WS_LAYOUT));
  assert.equal(L.inboxCollapsed, true);
  assert.equal(L.inboxW, 240); /* last expanded remembered */

  /* clear suppress-click from drag, then tap the collapsed rail */
  syncZoneRects(roots);
  roots.wszones.dispatch("click", { target: roots.wsinbox }); /* eat suppress */
  roots.wsinbox.dispatch("click", { target: roots.wsinbox });
  assert.equal(roots.wsinbox.classList.contains("collapsed"), false);
  assert.equal(roots.wszones.style.getPropertyValue("--wsinbox-w"), "240px");
  L = JSON.parse(storage.getItem(STORAGE_KEY_WS_LAYOUT));
  assert.equal(L.inboxCollapsed, false);
  assert.equal(L.inboxW, 240);
});

test("arrow keys on focused separator nudge width and persist", async () => {
  const ctx = createFeature({});
  const { feature, roots, storage } = ctx;
  feature.bind();
  feature.open();
  await settle();

  const handle = roots.divInbox;
  handle.dispatch("keydown", {
    target: handle, key: "ArrowRight", preventDefault(){},
  });
  assert.equal(roots.wszones.style.getPropertyValue("--wsinbox-w"), (240 + WS_DIVIDER_NUDGE) + "px");
  const L = JSON.parse(storage.getItem(STORAGE_KEY_WS_LAYOUT));
  assert.equal(L.inboxW, 240 + WS_DIVIDER_NUDGE);

  handle.dispatch("keydown", {
    target: handle, key: "ArrowLeft", preventDefault(){},
  });
  /* back toward start (may not be exact if measure uses style) */
  assert.ok(parseFloat(roots.wszones.style.getPropertyValue("--wsinbox-w")) < 240 + WS_DIVIDER_NUDGE);
});

test("workspace open hydrates layout from localStorage; garbage → defaults", async () => {
  const stored = {
    inboxW: 260, navW: 200, inboxCollapsed: true, navCollapsed: false,
  };
  const ctx = createFeature({
    storageInit: { [STORAGE_KEY_WS_LAYOUT]: JSON.stringify(stored) },
  });
  const { feature, roots } = ctx;
  feature.bind();
  feature.open();
  await settle();
  assert.equal(roots.wsinbox.classList.contains("collapsed"), true);
  assert.equal(roots.wsnav.classList.contains("collapsed"), false);
  assert.equal(roots.wszones.style.getPropertyValue("--wsinbox-w"), WS_LAYOUT_PEEK + "px");
  /* nav expanded with stored width */
  assert.equal(roots.wszones.style.getPropertyValue("--wsnav-w"), "200px");

  /* garbage storage: no throw, no collapsed, no custom props (clamp defaults) */
  const bad = createFeature({
    storageInit: { [STORAGE_KEY_WS_LAYOUT]: "not-json{{{" },
  });
  bad.feature.bind();
  bad.feature.open();
  await settle();
  assert.equal(bad.roots.wsinbox.classList.contains("collapsed"), false);
  assert.equal(bad.roots.wsnav.classList.contains("collapsed"), false);
  assert.equal(bad.roots.wszones.style.getPropertyValue("--wsinbox-w"), "");
  assert.equal(bad.roots.wszones.style.getPropertyValue("--wsnav-w"), "");
});

test("dividers are inert when narrow (<768)", async () => {
  const ctx = createFeature({ isNarrow: true });
  const { feature, roots, storage } = ctx;
  feature.bind();
  feature.open();
  await settle();

  const handle = roots.divInbox;
  handle.dispatch("pointerdown", {
    target: handle, pointerId: 1, clientX: 240, button: 0, preventDefault(){},
  });
  handle.dispatch("pointermove", {
    target: handle, pointerId: 1, clientX: 100,
  });
  handle.dispatch("pointerup", { target: handle, pointerId: 1, clientX: 100 });
  assert.equal(storage.getItem(STORAGE_KEY_WS_LAYOUT), null);
  assert.equal(roots.wsinbox.classList.contains("collapsed"), false);

  handle.dispatch("keydown", {
    target: handle, key: "ArrowRight", preventDefault(){},
  });
  assert.equal(roots.wszones.style.getPropertyValue("--wsinbox-w"), "");
});

test("#wsnote never receives .collapsed; note flex stays 2.1", async () => {
  assert.match(notesCssSrc, /#wsnote\s*\{[^}]*flex:\s*2\.1/);
  assert.doesNotMatch(notesCssSrc, /#wsnote\.collapsed/);
  const ctx = createFeature({});
  const { feature, roots } = ctx;
  feature.bind();
  feature.open();
  await settle();
  /* collapse nav hard against note — note must not collapse */
  const handle = roots.divNav;
  handle.dispatch("pointerdown", {
    target: handle, pointerId: 2, clientX: 460, button: 0, preventDefault(){},
  });
  handle.dispatch("pointermove", {
    target: handle, pointerId: 2, clientX: 250, /* shrink nav hard */
  });
  handle.dispatch("pointerup", { target: handle, pointerId: 2, clientX: 250 });
  assert.equal(roots.wsnote.classList.contains("collapsed"), false);
  assert.equal(roots.wsnav.classList.contains("collapsed"), true);
});

/* ---------- P1a: desktop media-block structural integrity ---------- */

/**
 * Extract the body of `@media (min-width: 900px)` by counting braces from the
 * opening `{` to its true match. Do NOT slice on the next `}` or on text order
 * — a premature `}` inside the block is exactly the failure class this guards.
 */
function desktopMedia900Body(css){
  const m = /@media\s*\(\s*min-width:\s*900px\s*\)\s*\{/.exec(css);
  if (!m) return null;
  const open = m.index + m[0].length - 1; // index of '{'
  let depth = 0;
  for (let i = open; i < css.length; i++){
    const ch = css[i];
    if (ch === "{") depth++;
    else if (ch === "}"){
      depth--;
      if (depth === 0) return css.slice(open + 1, i);
    }
  }
  return null;
}

test("P1a: desktop-only shell rules live inside @media (min-width: 900px)", () => {
  /* Blast radius of a truncated desktop media block: these rules hide phone
     chrome (.backbtn = "‹ Activities") and restyle sheets to a 480px centred
     box. They must never apply at phone widths. */
  const mediaBody = desktopMedia900Body(notesCssSrc);
  assert.ok(mediaBody, "@media (min-width: 900px) block present in notes.css");

  assert.match(mediaBody, /\.backbtn\s*\{\s*display:\s*none\s*;?\s*\}/,
    ".backbtn { display: none } is desktop-only (phone primary nav)");
  assert.match(mediaBody, /\.sheet\s*\{[^}]*width:\s*480px/,
    ".sheet desktop centering is inside the 900px block");
  assert.match(mediaBody, /\.sheet\.open\s*\{[^}]*transform:\s*translate\(\s*-50%\s*,\s*0\s*\)/,
    ".sheet.open desktop rule is inside the 900px block");
  assert.match(mediaBody, /#scrim\s*\{\s*display:\s*none\s*;?\s*\}/,
    "#scrim { display: none } is desktop-only");
  assert.match(mediaBody, /#bookmarkpeek\b[^\{]*\{[^}]*display:\s*none/,
    "#bookmarkpeek hide rule is desktop-only");
});

test("P1a: desktop #bookmarkspane is a positioned, hidden fixed-width flex item", () => {
  /* The desktop #bookmarkspane override must remain a single complete rule
     inside the 900px block. A stray } after the first three decls orphans
     display:none / width / flex:none and leaves the pane as a permanent
     flex column — disfiguring the dock and the whole desktop row. */
  const mediaBody = desktopMedia900Body(notesCssSrc);
  assert.ok(mediaBody, "@media (min-width: 900px) block present in notes.css");
  // Standalone #bookmarkspane (not body.map-full #bookmarkspane / body.bookmarks-open …).
  const rule = mediaBody.match(/(?:^|\n)\s*#bookmarkspane\s*\{([^}]+)\}/);
  assert.ok(rule, "desktop #bookmarkspane rule present inside 900px media");
  assert.match(rule[1], /position:\s*relative/, "desktop pane anchors its popovers while staying in flex flow");
  for (const edge of ["top", "right", "bottom", "left"]){
    assert.match(rule[1], new RegExp(`${edge}:\\s*auto`), `desktop pane clears the phone ${edge} inset`);
  }
  assert.match(rule[1], /display:\s*none/, "pane stays hidden until bookmarks-open");
  assert.match(rule[1], /width:\s*var\(--pane\)/, "pane column width from the shared token");
  assert.match(rule[1], /flex:\s*none/, "pane does not grow/shrink in the row");
});

test("--pane is one token for every side column, and half of it insets the docked chat", () => {
  /* The 340px side-column width was written out four times (#cards, #map's
     override, #bookmarkspane, and the two workspace zone defaults) with a
     comment saying they deliberately share one rhythm — exactly the drift
     the --grab extraction just fixed on the dividers. */
  const pane = tokensSrc.match(/--pane:\s*([0-9]+)px/);
  assert.ok(pane, "--pane token defined in tokens.css");
  assert.equal(pane[1], "340", "--pane keeps the established side-column width");

  const mediaBody = desktopMedia900Body(notesCssSrc);
  const cards = mediaBody.match(/(?:^|\n)\s*#cards,\s*#map\s*\{([^}]+)\}/);
  assert.ok(cards, "#cards, #map desktop rule present");
  assert.match(cards[1], /width:\s*var\(--pane\)/, "#cards width from --pane");
  assert.doesNotMatch(mediaBody, /width:\s*340px/, "no stale 340px literal in the 900px block");
  assert.match(notesCssSrc, /#wsinbox\s*\{\s*width:\s*var\(--wsinbox-w,\s*var\(--pane\)\)/,
    "#wsinbox default from --pane");
  assert.match(notesCssSrc, /#wsnav\s*\{\s*width:\s*var\(--wsnav-w,\s*var\(--pane\)\)/,
    "#wsnav default from --pane");

  /* Under the dock #cards is hidden, so the chat runs edge to edge and puts
     (+) and send in the two bottom corners where iPadOS parks its
     input-source pill. Give the content column back the width it has beside
     the Activities pane — half that pane per side. */
  /* Which elements share the column is map.test.js's question; this one only
     asks that the inset stays derived from the same token. */
  const inset = mediaBody.match(
    /body\.map-full\.map-dock\s+#msgs,[^{}]*\{([^}]+)\}/);
  assert.ok(inset, "docked content-column inset rule present inside the 900px block");
  assert.match(inset[1], /padding-inline:\s*calc\(var\(--pane\)\s*\/\s*2\)/,
    "inset is half a pane per side, derived not literal");
});

/* ---------- P3: caret at the end, never select-all (notes call sites) ---------- */

test("P3 9: note card rename places caret at end; never select-all", async () => {
  assert.match(notesSrc, /from "\.\/caret\.js"/, "notes imports caret.js");
  assert.match(notesSrc, /focusAtEnd\s*\(/, "rename uses focusAtEnd");

  const ctx = createFeature({
    notes: [{ id: "n1", title: "Original Title", order: 0 }],
    docs: { n1: { id: "n1", title: "Original Title", sections: [] } },
  });
  const { feature, roots } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");

  const card = el("div", { className: "wscard", dataset: { note: "n1" }, draggable: true });
  card.dataset.note = "n1";
  card.draggable = true;
  const title = el("div", { className: "wctitle", textContent: "Original Title" });
  card.appendChild(title);
  const renameBtn = el("button", { dataset: { wcact: "rename" } });
  renameBtn.dataset.wcact = "rename";
  renameBtn.closest = sel => {
    if (sel === "[data-wcact]" || sel === "[data-wcact=\"rename\"]") return renameBtn;
    if (sel === ".wscard") return card;
    return null;
  };
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.appendChild(card);

  roots.wscards.dispatch("click", {
    target: renameBtn,
    stopPropagation(){},
    preventDefault(){},
  });

  const input = card.querySelector("input") || card.querySelector(".wctitleedit");
  assert.ok(input, "rename swaps title for an input");
  assert.equal(input.value, "Original Title");
  assert.equal(input._focused, true, "rename input focused");
  assert.ok(!input._selected, "select() must not run — no select-all");
  assert.deepEqual(input._range, [input.value.length, input.value.length],
    "caret at end of existing title");
});

test("P3 10: note section body edit places caret at end", async () => {
  assert.match(notesSrc, /from "\.\/caret\.js"/);
  assert.match(notesSrc, /focusAtEnd\s*\(/);

  const bodyText = "existing section body text";
  const ctx = createFeature({
    notes: [{ id: "n1", title: "N", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "N",
        sections: [{ id: "s1", title: "S", body: bodyText, order: 0, references: [] }],
      },
    },
  });
  ctx.feature.bind();
  ctx.feature.open();
  await settle();
  await openNote(ctx, "n1");

  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  sec.dataset.sec = "s1";
  const body = el("div", { className: "wssecbody" });
  const render = el("div", { dataset: { secrender: "" } });
  render.dataset.secrender = "";
  sec.appendChild(body);
  body.appendChild(render);
  ctx.roots.wssections.appendChild(sec);
  ctx.roots.wssections.dispatch("click", { target: render });

  const textarea = body.querySelector("textarea");
  assert.ok(textarea, "body click enters edit mode");
  assert.equal(textarea.value, bodyText);
  assert.equal(textarea._focused, true, "body textarea focused");
  assert.ok(!textarea._selected, "select() must not run");
  assert.deepEqual(textarea._range, [bodyText.length, bodyText.length],
    "caret at end of existing body — bare focus() lands at 0 in WebKit");
});


/* ---------- P2: workspace send-to reuses the one #sendto dialogue ----------
 * CSS lifts .sheet above #notesworkspace; this locks the JS contract the
 * fix makes visible — note-section bubbles call the shared openSendTo with
 * the bubble text and source node id (bookmarks owns #sendto). */

test("P2: the workspace send-to path opens the one shared #sendto sheet", async () => {
  const calls = [];
  const ctx = createFeature({
    notes: [{ id: "n1", title: "N", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "N",
        sections: [{
          id: "s1", title: "", body: "x", order: 0,
          references: [{
            id: "r1",
            source: { node: "src-node", uid: "u", segment: 0, record: 0 },
            snapshot: { text: "bubble text from the note", lane: "#0a0" },
          }],
        }],
      },
    },
    deps: { openSendTo: opts => calls.push(opts) },
  });
  const { feature, roots } = ctx;
  feature.bind();
  feature.open();
  await settle();
  const card = el("button", { className: "wscard", dataset: { note: "n1" } });
  card.dataset.note = "n1";
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.dispatch("click", { target: card });
  await settle();

  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  sec.dataset.sec = "s1";
  const ref = el("div", { className: "wsref", dataset: { ref: "r1" } });
  ref.dataset.ref = "r1";
  const btn = el("button", { dataset: { refact: "sendto" } });
  btn.dataset.refact = "sendto";
  btn.closest = sel => {
    if (sel === "[data-refact]") return btn;
    if (sel === ".wsref") return ref;
    if (sel === ".wssec") return sec;
    return null;
  };
  roots.wssections.dispatch("click", { target: btn });

  assert.equal(calls.length, 1, "openSendTo called once for data-refact=sendto");
  assert.equal(calls[0].text, "bubble text from the note",
    "bubble snapshot text is the send-to payload");
  assert.equal(calls[0].exceptId, "src-node",
    "source node id is withheld from the target list");
});

/* ---------- P6 review: notes send-to routes ---------- */

test("P6 review: inbox send-to strips asset markers and excepts the source chat", () => {
  const calls = [];
  const ctx = createFeature({
    bookmarks: [{ t: "1", node: "nX", text: "see ![shot](scimux-asset:a_1) here" }],
    deps: { openSendTo: opts => calls.push(opts) },
  });
  const { feature, roots } = ctx;
  feature.bind();

  const row = el("div", { className: "wsibookmark", dataset: { t: "1" } });
  row.dataset.t = "1";
  const btn = el("button", { dataset: { bmact: "sendto" } });
  btn.dataset.bmact = "sendto";
  btn.closest = sel => {
    if (sel === "[data-bmact]") return btn;
    if (sel === ".wsibookmark") return row;
    return null;
  };
  roots.wsinboxlist.dispatch("click", { target: btn });

  assert.equal(calls.length, 1);
  assert.equal(calls[0].text, "see shot here", "asset markers stripped");
  assert.equal(calls[0].exceptId, "nX", "the bookmark's own chat is withheld");
});

test("P6 review: reference send-to strips asset markers and excepts the source chat", async () => {
  const calls = [];
  const ctx = createFeature({
    notes: [{ id: "n1", title: "N", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "N",
        sections: [{
          id: "s1", title: "", body: "x", order: 0,
          references: [{
            id: "r1",
            source: { node: "nX", uid: "u", segment: 0, record: 0 },
            snapshot: { text: "see ![shot](scimux-asset:a_1) here", lane: "#f00" },
          }],
        }],
      },
    },
    deps: { openSendTo: opts => calls.push(opts) },
  });
  const { feature, roots } = ctx;
  feature.bind();
  feature.open();
  await settle();
  const card = el("button", { className: "wscard", dataset: { note: "n1" } });
  card.dataset.note = "n1";
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.dispatch("click", { target: card });
  await settle();

  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  sec.dataset.sec = "s1";
  const ref = el("div", { className: "wsref", dataset: { ref: "r1" } });
  ref.dataset.ref = "r1";
  const btn = el("button", { dataset: { refact: "sendto" } });
  btn.dataset.refact = "sendto";
  btn.closest = sel => {
    if (sel === "[data-refact]") return btn;
    if (sel === ".wsref") return ref;
    if (sel === ".wssec") return sec;
    return null;
  };
  roots.wssections.dispatch("click", { target: btn });

  assert.equal(calls.length, 1);
  assert.equal(calls[0].text, "see shot here", "asset markers stripped");
  assert.equal(calls[0].exceptId, "nX", "the reference's source chat is withheld");
});

/* ---------- Phase 3 Packet D: section menu / save fail / rename / inbox ---------- */

async function openSectionMenuAction(ctx, secId, action){
  const sec = el("div", { className: "wssec", dataset: { sec: secId } });
  sec.dataset.sec = secId;
  const menuBtn = el("button");
  menuBtn.dataset.secmenu = "";
  menuBtn.closest = sel => {
    if (sel === "[data-secmenu]") return menuBtn;
    if (sel === ".wssec") return sec;
    return null;
  };
  menuBtn.getBoundingClientRect = () =>
    ({ top: 100, left: 200, bottom: 144, right: 244, width: 44, height: 44 });
  ctx.roots.wspanel.getBoundingClientRect = () =>
    ({ top: 0, left: 0, width: 400, height: 600 });
  ctx.roots.wssections.appendChild(sec);
  ctx.roots.wssections.dispatch("click", { target: menuBtn });
  await settle();
  const menu = ctx.roots.wspanel.children.find(c => (c.className || "").includes("popmenu"));
  assert.ok(menu, "section menu opens");
  const mi = el("button");
  mi.dataset.mi = action;
  mi.closest = sel => (sel === "[data-mi]" ? mi : null);
  menu.dispatch("click", { target: mi });
  await settle();
  return menu;
}

test("Packet D: section move up/down via menu; boundary is a no-op", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "N", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "N",
        sections: [
          { id: "s1", title: "One", body: "", order: 0, references: [] },
          { id: "s2", title: "Two", body: "", order: 1, references: [] },
          { id: "s3", title: "Three", body: "", order: 2, references: [] },
        ],
      },
    },
  });
  const { feature, apiLog } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");

  const before = apiLog.length;
  await openSectionMenuAction(ctx, "s1", "up");
  assert.equal(
    apiLog.slice(before).filter(x => x.method === "PATCH").length,
    0,
    "first section Move up must not PATCH",
  );

  const beforeDown = apiLog.length;
  await openSectionMenuAction(ctx, "s3", "down");
  assert.equal(
    apiLog.slice(beforeDown).filter(x => x.method === "PATCH").length,
    0,
    "last section Move down must not PATCH",
  );

  const beforeMove = apiLog.length;
  await openSectionMenuAction(ctx, "s2", "up");
  const patches = apiLog.slice(beforeMove).filter(x => x.method === "PATCH");
  assert.equal(patches.length, 2, "swap issues two order PATCHes");
  const bodies = patches.map(p => JSON.parse(p.body).section);
  assert.deepEqual(
    bodies.sort((a, b) => a.id.localeCompare(b.id)),
    [
      { id: "s1", order: 1 },
      { id: "s2", order: 0 },
    ],
  );

  const beforeDownMove = apiLog.length;
  await openSectionMenuAction(ctx, "s2", "down");
  const downPatches = apiLog.slice(beforeDownMove).filter(x => x.method === "PATCH");
  assert.equal(downPatches.length, 2, "Move down also swaps via two PATCHes");
});

test("Packet D: section delete via menu PATCHes delete:true", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "N", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "N",
        sections: [
          { id: "s1", title: "One", body: "", order: 0, references: [] },
          { id: "s2", title: "Two", body: "", order: 1, references: [] },
        ],
      },
    },
  });
  const { feature, apiLog, docs } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");

  const before = apiLog.length;
  await openSectionMenuAction(ctx, "s2", "del");
  const del = apiLog.slice(before).find(x => x.method === "PATCH");
  assert.ok(del, "delete section issues PATCH");
  assert.deepEqual(JSON.parse(del.body), { section: { id: "s2", delete: true } });
  assert.equal((docs.n1.sections || []).some(s => s.id === "s2"), false);
});

test("Packet D: save failure toasts and retains the local edit value", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "N", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "N",
        sections: [{ id: "s1", title: "S", body: "original", order: 0, references: [] }],
      },
    },
    api: async (url, opts = {}, { notes, docs }) => {
      if (opts.method === "PATCH") throw new Error("disk full");
      if (url === "/api/notes") return { notes };
      const m = url.match(/^\/api\/notes\/([^/]+)$/);
      if (m && (!opts.method || opts.method === "GET"))
        return docs[decodeURIComponent(m[1])];
      return null;
    },
  });
  const { feature, effects, flush } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");

  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  sec.dataset.sec = "s1";
  const body = el("div", { className: "wssecbody" });
  const render = el("div", { dataset: { secrender: "" } });
  render.dataset.secrender = "";
  body.appendChild(render);
  sec.appendChild(body);
  ctx.roots.wssections.appendChild(sec);
  ctx.roots.wssections.dispatch("click", { target: render });

  const textarea = body.querySelector("textarea");
  assert.ok(textarea);
  textarea.value = "kept locally after fail";
  textarea.dispatch("input", { target: textarea });
  flush(SAVE_DEBOUNCE_MS);
  await settle();

  assert.ok(
    effects.toast.includes("Save failed — will retry on next edit"),
    "failed PATCH must toast",
  );
  assert.equal(textarea.value, "kept locally after fail",
    "local editor value must survive the failed save");
});

test("Packet D: rename Escape restores title and issues no PATCH", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "Orig", order: 0 }],
    docs: { n1: { id: "n1", title: "Orig", sections: [] } },
  });
  const { feature, roots, apiLog } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");

  const card = el("div", { className: "wscard", dataset: { note: "n1" } });
  card.dataset.note = "n1";
  card.draggable = true;
  const title = el("div", { className: "wctitle", textContent: "Orig" });
  card.appendChild(title);
  const renameBtn = el("button");
  renameBtn.dataset.wcact = "rename";
  renameBtn.closest = sel => {
    if (sel === "[data-wcact]" || String(sel).includes("data-wcact")) return renameBtn;
    if (sel === ".wscard") return card;
    return null;
  };
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.appendChild(card);

  roots.wscards.dispatch("click", {
    target: renameBtn, stopPropagation(){}, preventDefault(){},
  });
  const input = card.querySelector("input") || card.querySelector(".wctitleedit");
  assert.ok(input);
  input.value = "Should not commit";
  const before = apiLog.filter(x => x.method === "PATCH").length;
  input.dispatch("keydown", {
    key: "Escape", target: input,
    preventDefault(){}, stopPropagation(){},
  });
  await settle();
  assert.equal(
    apiLog.filter(x => x.method === "PATCH").length,
    before,
    "Escape rename must not PATCH",
  );
  assert.equal(roots.wstitle.textContent, "Orig", "active title stays original");
});

test("Packet D: delete note API failure toasts and keeps the editor open", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "Keep", order: 0 }],
    docs: { n1: { id: "n1", title: "Keep", sections: [] } },
    api: async (url, opts = {}, { notes, docs }) => {
      if (opts.method === "DELETE") throw new Error("refuse");
      if (url === "/api/notes") return { notes };
      const m = url.match(/^\/api\/notes\/([^/]+)$/);
      if (m && (!opts.method || opts.method === "GET"))
        return docs[decodeURIComponent(m[1])];
      return {};
    },
  });
  const { feature, roots, effects, apiLog } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");
  assert.ok(roots.notesworkspace.classList.contains("note-open"));

  const card = el("div", { className: "wscard", dataset: { note: "n1" } });
  card.dataset.note = "n1";
  const delBtn = el("button", { className: "danger" });
  delBtn.dataset.wcact = "delete";
  delBtn.closest = sel => {
    if (sel === "[data-wcact]" || String(sel).includes("data-wcact")) return delBtn;
    if (sel === ".wscard") return card;
    return null;
  };
  delBtn.getBoundingClientRect = () =>
    ({ top: 10, left: 10, bottom: 50, height: 40, width: 44 });
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.appendChild(card);
  roots.wspanel.getBoundingClientRect = () =>
    ({ top: 0, left: 0, width: 400, height: 600 });

  roots.wscards.dispatch("click", {
    target: delBtn, stopPropagation(){}, preventDefault(){},
  });
  await settle();
  const confirm = roots.wspanel.children.find(c => (c.className || "").includes("popmenu"));
  assert.ok(confirm);
  const ok = el("button");
  ok.dataset.wconfirm = "ok";
  ok.closest = sel => (sel === "[data-wconfirm]" || String(sel).includes("data-wconfirm") ? ok : null);
  confirm.dispatch("click", { target: ok });
  await settle();

  assert.ok(apiLog.some(x => x.method === "DELETE"), "DELETE was attempted");
  assert.ok(effects.toast.includes("Delete failed"));
  assert.equal(roots.notesworkspace.classList.contains("note-open"), true,
    "failed delete must not clear the open editor");
  assert.equal(roots.wstitle.textContent, "Keep");
});

test("Packet D: layout localStorage setItem throw still applies live widths", async () => {
  const ctx = createFeature({});
  const { feature, roots, storage } = ctx;
  feature.bind();
  feature.open();
  await settle();

  let threw = false;
  storage.setItem = () => { threw = true; throw new Error("quota"); };

  const handle = roots.divInbox;
  assert.doesNotThrow(() => {
    handle.dispatch("pointerdown", {
      target: handle, pointerId: 1, clientX: 240, button: 0, preventDefault(){},
    });
    handle.dispatch("pointermove", {
      target: handle, pointerId: 1, clientX: 260,
    });
    handle.dispatch("pointerup", {
      target: handle, pointerId: 1, clientX: 260,
    });
  });
  assert.equal(threw, true, "persist attempted setItem");
  assert.equal(roots.wszones.style.getPropertyValue("--wsinbox-w"), "260px",
    "live layout must continue after storage failure");
  assert.equal(roots.wszones.style.getPropertyValue("--wsnav-w"), "200px");
});

test("Packet D: inbox del cancel skips mutate; confirm deletes", async () => {
  const mutations = [];
  const ctx = createFeature({
    bookmarks: [{ t: "1", text: "bye", node: "nX", uid: "u1" }],
    deps: { uiMutate: op => mutations.push(op) },
  });
  const { feature, roots } = ctx;
  feature.bind();
  feature.open();
  await settle();

  const row = el("div", { className: "wsibookmark", dataset: { t: "1" } });
  row.dataset.t = "1";
  const bar = el("div", { className: "actionbar" });
  const del = el("button");
  del.dataset.bmact = "del";
  del.closest = sel => {
    if (sel === "[data-bmact]") return del;
    if (sel === ".actionbar") return bar;
    if (sel === ".wsibookmark") return row;
    return null;
  };
  del.getBoundingClientRect = () =>
    ({ top: 40, left: 40, bottom: 84, right: 84, width: 44, height: 44 });
  roots.wspanel.getBoundingClientRect = () =>
    ({ top: 0, left: 0, width: 400, height: 600 });

  roots.wsinboxlist.dispatch("click", { target: del });
  await settle();
  assert.equal(mutations.length, 0, "flat-bar delete waits for confirm");
  const confirm = roots.wspanel.children.find(c => (c.className || "").includes("popmenu"));
  assert.ok(confirm);
  assert.match(confirm.className, /wsconfirm/);

  const cancel = el("button");
  cancel.dataset.wconfirm = "cancel";
  cancel.closest = sel =>
    (sel === "[data-wconfirm]" || String(sel).includes("data-wconfirm") ? cancel : null);
  confirm.dispatch("click", { target: cancel });
  await settle();
  assert.equal(mutations.length, 0, "cancel must not bookmark-del");

  roots.wsinboxlist.dispatch("click", { target: del });
  await settle();
  const confirm2 = roots.wspanel.children.find(c => (c.className || "").includes("popmenu"));
  assert.ok(confirm2);
  const ok = el("button");
  ok.dataset.wconfirm = "ok";
  ok.closest = sel =>
    (sel === "[data-wconfirm]" || String(sel).includes("data-wconfirm") ? ok : null);
  confirm2.dispatch("click", { target: ok });
  await settle();
  assert.deepEqual(mutations, [{ k: "bookmark-del", t: "1" }]);
});

test("Packet D: inbox jump miss toasts when the chat is gone", async () => {
  const ctx = createFeature({
    bookmarks: [{ t: "1", text: "orphan", node: "gone", uid: "u-gone" }],
    jumpOk: false,
  });
  const { feature, roots, effects } = ctx;
  feature.bind();
  feature.open();
  await settle();

  const row = el("div", { className: "wsibookmark", dataset: { t: "1" } });
  row.dataset.t = "1";
  const jump = el("button");
  jump.dataset.bmact = "jump";
  jump.closest = sel => {
    if (sel === "[data-bmact]") return jump;
    if (sel === ".wsibookmark") return row;
    return null;
  };
  roots.wsinboxlist.dispatch("click", { target: jump });
  assert.equal(effects.jump.length, 1);
  assert.equal(effects.jump[0].node, "gone");
  assert.ok(effects.toast.includes("That chat is no longer available."));
});

/* ---------- AI disclosure on send-to ----------
   bookmarks.js owns the marker; notes.js owns two of the four callers, and a
   caller that hands over no role disables the disclosure silently. */
test("placement sends frozen role, agent, and provenance and survives source deletion", async () => {
  const prov = [{ loc: "chunk", key: "_meta", v: { src: "place" } }];
  const ctx = createFeature({
    notes: [{ id: "n1", title: "Note", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "Note",
        sections: [{ id: "s1", title: "S", body: "body", order: 0, references: [] }],
      },
    },
    bookmarks: [{
      t: "1", text: "quoted", uid: "u1", segment: 1, record: 4,
      node: "nX", turnTime: "TT", lane: "lane-a",
      role: "assistant", agent: "muse", prov,
    }],
    lanes: [{ id: "lane-a", name: "A", color: "#f00" }],
    nodes: { nX: { id: "nX", title: "Station", lane_id: "lane-a" } },
  });
  const { feature, roots, apiLog, docs } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");
  feature.startPlacement({
    t: "1", text: "quoted", uid: "u1", segment: 1, record: 4,
    node: "nX", turnTime: "TT", lane: "lane-a",
    role: "assistant", agent: "muse", prov,
  });
  const add = el("button");
  add.dataset.addhere = "";
  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  sec.dataset.sec = "s1";
  const body = el("div", { className: "wssecbody" });
  sec.appendChild(body);
  add.closest = sel => {
    if (sel === "[data-addhere]") return add;
    if (sel === ".wssec") return sec;
    return null;
  };
  roots.wssections.dispatch("click", { target: add });
  await settle();
  const post = apiLog.find(x => x.method === "POST" && x.url.includes("/references"));
  assert.ok(post);
  const payload = JSON.parse(post.body);
  assert.equal(payload.snapshot.role, "assistant");
  assert.equal(payload.snapshot.agent, "muse");
  assert.equal(payload.snapshot.speaker, "muse");
  assert.deepEqual(payload.snapshot.prov, prov);
  ctx.setBookmarks([]);
  await openNote(ctx, "n1");
  await settle();
  const snap = docs.n1.sections[0].references[0].snapshot;
  assert.equal(snap.role, "assistant");
  assert.equal(snap.agent, "muse");
  assert.equal(snap.speaker, "muse");
  assert.deepEqual(snap.prov, prov);
  assert.match(roots.wssections.innerHTML, /muse/);
});

test("reference send-to passes frozen snapshot.role when speaker is grok", async () => {
  const calls = [];
  const ctx = createFeature({
    notes: [{ id: "n1", title: "N", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "N",
        sections: [{
          id: "s1", title: "", body: "x", order: 0,
          references: [{
            id: "r1",
            source: { node: "src-node", uid: "u", segment: 0, record: 0 },
            snapshot: {
              text: "quoted grok", lane: "#0a0",
              role: "assistant", speaker: "grok", agent: "grok",
            },
          }],
        }],
      },
    },
    deps: { openSendTo: opts => calls.push(opts) },
  });
  const { feature, roots } = ctx;
  feature.bind();
  feature.open();
  await settle();
  const card = el("button", { className: "wscard", dataset: { note: "n1" } });
  card.dataset.note = "n1";
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.dispatch("click", { target: card });
  await settle();
  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  sec.dataset.sec = "s1";
  const ref = el("div", { className: "wsref", dataset: { ref: "r1" } });
  ref.dataset.ref = "r1";
  const btn = el("button", { dataset: { refact: "sendto" } });
  btn.dataset.refact = "sendto";
  btn.closest = sel => {
    if (sel === "[data-refact]") return btn;
    if (sel === ".wsref") return ref;
    if (sel === ".wssec") return sec;
    return null;
  };
  roots.wssections.dispatch("click", { target: btn });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].role, "assistant");
  assert.equal(calls[0].text, "quoted grok");
});

test("the workspace's two send-to callers pass the frozen transcript role", () => {
  assert.match(notesSrc, /openSendTo\(\{[\s\S]{0,240}?role:\s*nt\.role/,
    "inbox bookmark send-to passes the bookmark's role");
  assert.match(notesSrc, /role:\s*\(ref\.snapshot && ref\.snapshot\.role\)/,
    "a reference's send-to passes its frozen role even when speaker is an agent name");
});
