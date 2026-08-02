/* Characterization tests for web/js/state.js — UI document reducer / replay /
 * isolated storage. Imports the real module (not HTML substring extraction).
 *
 * Mutation model (documented, not redesigned): applyOp mutates `doc` in place.
 * Tests assert object identity for retained vs replaced list arrays and for
 * the operation object when stampAndEnqueue writes op.rev. */
import test from "node:test";
import assert from "node:assert/strict";
import {
  PENDING_OPS_KEY,
  CACHED_UI_KEY,
  emptyUI,
  normUI,
  idempotentOp,
  applyOp,
  replayOps,
  stampAndEnqueue,
  queueLocalOp,
  eligibleForLoadReplay,
  eligibleForConflictReplay,
  filterOpsForLoad,
  filterOpsForConflict,
  mergeLoadOps,
  adoptRemoteAndReplay,
  loadDocumentWithReplay,
  savePendingOps,
  loadPendingOps,
  saveCachedUI,
  loadCachedUI,
} from "../js/state.js";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const stateSrc = readFileSync(join(__dirname, "../js/state.js"), "utf8");

/* ---------- purity / no browser globals ---------- */
test("state.js has no browser globals or implicit app state", () => {
  const code = stateSrc
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/\/\/.*$/gm, "");
  const forbidden = [
    /\bdocument\b/,
    /\bwindow\b/,
    /\bfetch\b/,
    /\blocalStorage\b/,
    /\bnavigator\b/,
    /\bsetTimeout\b/,
    /\bsetInterval\b/,
    /\brequestAnimationFrame\b/,
    /\bgetComputedStyle\b/,
    /\bcreateElement\b/,
    /\binnerHTML\b/,
    /\bXMLHttpRequest\b/,
    /\bwithCsrf\b/,
    /\bflushUI\b/,
    /\bpollUI\b/,
    /\brenderSyncState\b/,
    /\brenderBookmarksPane\b/,
    /\brenderWsInbox\b/,
    /\brenderCards\b/,
    /\bapplyRemoteUI\b/,
  ];
  for (const re of forbidden) {
    assert.equal(re.test(code), false, `state.js must not reference ${re}`);
  }
  // Explicit ban on production globals (parameters use `doc`/`op`/`rev`/`queue`)
  assert.equal(/\buiOps\b/.test(code), false, "must not reference uiOps");
  assert.equal(/\buiRev\b/.test(code), false, "must not reference uiRev");
  assert.equal(/\buiLoaded\b/.test(code), false);
  assert.equal(/\buiSaving\b/.test(code), false);
  assert.equal(/\buiTimer\b/.test(code), false);
  // No bare production UI store reads (UI.groups etc.)
  assert.equal(/\bUI\./.test(code), false, "must not read UI.*");
});

test("state.js exports named pure helpers", async () => {
  const mod = await import("../js/state.js");
  for (const name of [
    "normUI",
    "idempotentOp",
    "applyOp",
    "replayOps",
    "stampAndEnqueue",
    "queueLocalOp",
    "filterOpsForLoad",
    "filterOpsForConflict",
    "mergeLoadOps",
    "adoptRemoteAndReplay",
    "loadDocumentWithReplay",
    "savePendingOps",
    "loadPendingOps",
    "saveCachedUI",
    "loadCachedUI",
  ]) {
    assert.equal(typeof mod[name], "function", name);
  }
  assert.equal(mod.PENDING_OPS_KEY, "scimux-ui-ops");
  assert.equal(mod.CACHED_UI_KEY, "scimux-ui");
});

/* ---------- memory storage adapter ---------- */
function memStorage(initial = {}) {
  const map = new Map(Object.entries(initial));
  return {
    map,
    getItem(k) {
      return map.has(k) ? map.get(k) : null;
    },
    setItem(k, v) {
      map.set(k, String(v));
    },
    removeItem(k) {
      map.delete(k);
    },
  };
}

function throwingStorage({ get = false, set = false, getValue = null } = {}) {
  return {
    getItem() {
      if (get) throw new Error("get denied");
      return getValue;
    },
    setItem() {
      if (set) throw new Error("set denied");
    },
  };
}

/* ---------- normUI ---------- */
test("normUI: empty / null / undefined → full defaults", () => {
  for (const j of [undefined, null, {}, ""]) {
    const d = normUI(j === "" ? "" : j);
    assert.deepEqual(d.groups, []);
    assert.deepEqual(d.archived, []);
    assert.deepEqual(d.bookmarks, []);
    assert.deepEqual(d.lanes, []);
    assert.deepEqual(d.pinned, []);
  }
  // empty string is truthy for || only as j || {} — "" is falsy → defaults
  assert.deepEqual(normUI(""), emptyUI());
});

test("normUI: fills missing keys without wiping present ones", () => {
  const d = normUI({ archived: ["a"], pinned: ["p"] });
  assert.deepEqual(d.archived, ["a"]);
  assert.deepEqual(d.pinned, ["p"]);
  assert.deepEqual(d.groups, []);
  assert.deepEqual(d.bookmarks, []);
  assert.deepEqual(d.lanes, []);
});

test("normUI: preserves unknown top-level fields", () => {
  const d = normUI({ archived: ["x"], future: 42, extra: { nested: true } });
  assert.equal(d.future, 42);
  assert.deepEqual(d.extra, { nested: true });
  assert.deepEqual(d.archived, ["x"]);
});

test("normUI: shallow — nested arrays/objects keep the input reference", () => {
  const archived = ["n1"];
  const bookmarks = [{ t: 1 }];
  const j = { archived, bookmarks, custom: { a: 1 } };
  const d = normUI(j);
  assert.equal(d.archived, archived, "archived array identity retained");
  assert.equal(d.bookmarks, bookmarks, "bookmarks array identity retained");
  assert.equal(d.custom, j.custom, "unknown nested object identity retained");
});

test("normUI: returns a new outer object each call (not the input)", () => {
  const j = { groups: [1] };
  const d = normUI(j);
  assert.notEqual(d, j);
  // emptyUI arrays are fresh each time
  const a = emptyUI();
  const b = emptyUI();
  assert.notEqual(a.groups, b.groups);
});

/* ---------- idempotentOp ---------- */
test("idempotentOp: only bookmark-add and bookmark-del", () => {
  assert.equal(idempotentOp({ k: "bookmark-add" }), true);
  assert.equal(idempotentOp({ k: "bookmark-del" }), true);
  for (const k of [
    "arch",
    "unarch",
    "pin",
    "unpin",
    "pin-order",
    "groups",
    "lanes",
    "unknown",
    "",
  ]) {
    assert.equal(idempotentOp({ k }), false, k);
  }
});

/* ---------- applyOp mutation model helpers ---------- */
function freshDoc(over = {}) {
  return normUI(over);
}

test("applyOp mutates doc in place and returns the same reference", () => {
  const doc = freshDoc();
  const out = applyOp(doc, { k: "arch", id: "n1" });
  assert.equal(out, doc, "return identity === input doc");
  assert.deepEqual(doc.archived, ["n1"]);
});

/* ---------- arch / unarch ---------- */
test("arch: appends id; dedupes; creates archived array if missing", () => {
  const doc = {};
  applyOp(doc, { k: "arch", id: "a" });
  assert.deepEqual(doc.archived, ["a"]);
  const arr = doc.archived;
  applyOp(doc, { k: "arch", id: "a" });
  assert.deepEqual(doc.archived, ["a"]);
  assert.equal(doc.archived, arr, "dedupe retains same array object via push path");
  applyOp(doc, { k: "arch", id: "b" });
  assert.deepEqual(doc.archived, ["a", "b"]);
  assert.equal(doc.archived, arr, "push retains array identity");
});

test("unarch: removes id and replaces archived with a new array", () => {
  const doc = freshDoc({ archived: ["a", "b", "c"] });
  const before = doc.archived;
  applyOp(doc, { k: "unarch", id: "b" });
  assert.deepEqual(doc.archived, ["a", "c"]);
  assert.notEqual(doc.archived, before, "unarch replaces the array");
  applyOp(doc, { k: "unarch", id: "missing" });
  assert.deepEqual(doc.archived, ["a", "c"]);
});

test("unarch: missing archived → empty array", () => {
  const doc = {};
  applyOp(doc, { k: "unarch", id: "x" });
  assert.deepEqual(doc.archived, []);
});

/* ---------- bookmark-add ---------- */
test("bookmark-add: pushes bookmark; dedupes on t; creates array if missing", () => {
  const doc = {};
  const bm = { t: 100, text: "hello", node: "n1" };
  applyOp(doc, { k: "bookmark-add", bookmark: bm });
  assert.equal(doc.bookmarks.length, 1);
  assert.equal(doc.bookmarks[0], bm, "bookmark object identity retained in list");
  const arr = doc.bookmarks;
  applyOp(doc, {
    k: "bookmark-add",
    bookmark: { t: 100, text: "duplicate t" },
  });
  assert.equal(doc.bookmarks.length, 1);
  assert.equal(doc.bookmarks[0].text, "hello");
  assert.equal(doc.bookmarks, arr, "dedupe push-path retains array identity");
  applyOp(doc, { k: "bookmark-add", bookmark: { t: 200, text: "second" } });
  assert.equal(doc.bookmarks.length, 2);
  assert.equal(doc.bookmarks, arr);
});

/* ---------- bookmark-del + anchored-comment promotion ---------- */
test("bookmark-del: removes target and replaces bookmarks array", () => {
  const a = { t: 1, text: "a" };
  const b = { t: 2, text: "b" };
  const doc = freshDoc({ bookmarks: [a, b] });
  const before = doc.bookmarks;
  applyOp(doc, { k: "bookmark-del", t: 1 });
  assert.deepEqual(
    doc.bookmarks.map((n) => n.t),
    [2]
  );
  assert.notEqual(doc.bookmarks, before, "bookmark-del replaces the array");
  assert.equal(doc.bookmarks[0], b, "surviving bookmark object retained");
});

test("bookmark-del: promotes anchored comments; inherits node/turnTime/lane", () => {
  const capture = {
    t: 10,
    text: "capture",
    node: "n1",
    turnTime: 999,
    lane: "L1",
  };
  const comment = {
    t: 11,
    text: "comment",
    anchor: 10,
    /* no node/lane/turnTime — should inherit */
  };
  const alreadyHas = {
    t: 12,
    text: "has-own",
    anchor: 10,
    node: "own",
    lane: "ownL",
  };
  const other = { t: 13, text: "other", anchor: 99 };
  const plain = { t: 14, text: "plain" };
  const doc = freshDoc({
    bookmarks: [capture, comment, alreadyHas, other, plain],
  });
  applyOp(doc, { k: "bookmark-del", t: 10 });
  const byT = Object.fromEntries(doc.bookmarks.map((n) => [n.t, n]));
  assert.equal(byT[10], undefined, "capture removed");
  // comment promoted
  assert.equal(byT[11].anchor, undefined);
  assert.equal(byT[11].node, "n1");
  assert.equal(byT[11].turnTime, 999);
  assert.equal(byT[11].lane, "L1");
  // alreadyHas keeps own fields; only loses anchor
  assert.equal(byT[12].anchor, undefined);
  assert.equal(byT[12].node, "own");
  assert.equal(byT[12].lane, "ownL");
  assert.equal(byT[12].turnTime, undefined);
  // other (different anchor) untouched
  assert.equal(byT[13].anchor, 99);
  assert.equal(byT[14].text, "plain");
});

test("bookmark-del: inherits turnTime only when dead has turnTime and comment lacks node", () => {
  // Inline: if (dead.node && !c.node){ c.node = dead.node; if (dead.turnTime) c.turnTime = dead.turnTime; }
  const capture = { t: 1, node: "n", /* no turnTime */ lane: "L" };
  const c = { t: 2, anchor: 1 };
  const doc = freshDoc({ bookmarks: [capture, c] });
  applyOp(doc, { k: "bookmark-del", t: 1 });
  assert.equal(doc.bookmarks[0].node, "n");
  assert.equal(doc.bookmarks[0].turnTime, undefined);
  assert.equal(doc.bookmarks[0].lane, "L");
});

test("bookmark-del: no inheritance when dead has no node; lane still inherited", () => {
  const capture = { t: 1, lane: "L", /* no node */ };
  const c = { t: 2, anchor: 1 };
  const doc = freshDoc({ bookmarks: [capture, c] });
  applyOp(doc, { k: "bookmark-del", t: 1 });
  assert.equal(doc.bookmarks[0].node, undefined);
  assert.equal(doc.bookmarks[0].lane, "L");
});

test("bookmark-del: missing target is a no-op filter (array still replaced)", () => {
  const bm = { t: 5, text: "x" };
  const doc = freshDoc({ bookmarks: [bm] });
  const before = doc.bookmarks;
  applyOp(doc, { k: "bookmark-del", t: 999 });
  assert.equal(doc.bookmarks.length, 1);
  assert.equal(doc.bookmarks[0], bm);
  assert.notEqual(doc.bookmarks, before);
});

/* ---------- pin / unpin ---------- */
test("pin: inserts at front with deduplication", () => {
  const doc = freshDoc({ pinned: ["a", "b"] });
  applyOp(doc, { k: "pin", id: "c" });
  assert.deepEqual(doc.pinned, ["c", "a", "b"]);
  // re-pin existing moves to front
  const before = doc.pinned;
  applyOp(doc, { k: "pin", id: "a" });
  assert.deepEqual(doc.pinned, ["a", "c", "b"]);
  assert.notEqual(doc.pinned, before, "pin replaces via filter+unshift");
});

test("pin: creates pinned array when missing", () => {
  const doc = {};
  applyOp(doc, { k: "pin", id: "x" });
  assert.deepEqual(doc.pinned, ["x"]);
});

test("unpin: removes id; replaces array", () => {
  const doc = freshDoc({ pinned: ["a", "b", "c"] });
  const before = doc.pinned;
  applyOp(doc, { k: "unpin", id: "b" });
  assert.deepEqual(doc.pinned, ["a", "c"]);
  assert.notEqual(doc.pinned, before);
  applyOp(doc, { k: "unpin", id: "missing" });
  assert.deepEqual(doc.pinned, ["a", "c"]);
});

/* ---------- pin-order / groups / lanes — assign by reference ---------- */
test("pin-order: assigns op.pinned by reference (identity retained)", () => {
  const doc = freshDoc({ pinned: ["old"] });
  const order = ["b", "a", "c"];
  applyOp(doc, { k: "pin-order", pinned: order });
  assert.equal(doc.pinned, order, "same array object as op.pinned");
  assert.deepEqual(doc.pinned, ["b", "a", "c"]);
});

test("groups: assigns op.groups by reference", () => {
  const doc = freshDoc({ groups: [{ name: "old" }] });
  const groups = [{ name: "G", lanes: ["L"] }];
  applyOp(doc, { k: "groups", groups });
  assert.equal(doc.groups, groups);
});

test("lanes: assigns op.lanes by reference", () => {
  const doc = freshDoc({ lanes: [{ id: "old" }] });
  const lanes = [{ id: "L1", name: "Main" }];
  applyOp(doc, { k: "lanes", lanes });
  assert.equal(doc.lanes, lanes);
});

/* ---------- unknown op ---------- */
test("unknown operation is a no-op (doc unchanged aside from identity)", () => {
  const doc = freshDoc({ archived: ["a"], pinned: ["p"] });
  const snap = JSON.stringify(doc);
  const out = applyOp(doc, { k: "nope", id: "x" });
  assert.equal(out, doc);
  assert.equal(JSON.stringify(doc), snap);
  applyOp(doc, { k: undefined });
  applyOp(doc, {});
  assert.equal(JSON.stringify(doc), snap);
});

/* ---------- replayOps ---------- */
test("replayOps applies in order and mutates the same doc", () => {
  const doc = freshDoc();
  const out = replayOps(doc, [
    { k: "arch", id: "a" },
    { k: "pin", id: "p1" },
    { k: "pin", id: "p2" },
    { k: "bookmark-add", bookmark: { t: 1, text: "n" } },
  ]);
  assert.equal(out, doc);
  assert.deepEqual(doc.archived, ["a"]);
  assert.deepEqual(doc.pinned, ["p2", "p1"]);
  assert.equal(doc.bookmarks.length, 1);
});

/* ---------- stamp / queue ---------- */
test("stampAndEnqueue mutates op.rev and pushes onto queue in order", () => {
  const op1 = { k: "arch", id: "a" };
  const op2 = { k: "pin", id: "p" };
  const queue = [];
  stampAndEnqueue(op1, '"rev-1"', queue);
  stampAndEnqueue(op2, '"rev-1"', queue);
  assert.equal(op1.rev, '"rev-1"');
  assert.equal(op2.rev, '"rev-1"');
  assert.equal(queue.length, 2);
  assert.equal(queue[0], op1);
  assert.equal(queue[1], op2);
});

test("queueLocalOp: apply then stamp then enqueue (uiMutate pure core)", () => {
  const doc = freshDoc();
  const queue = [];
  const op = { k: "arch", id: "n1" };
  const r = queueLocalOp(doc, op, '"etag-9"', queue);
  assert.equal(r.doc, doc);
  assert.deepEqual(doc.archived, ["n1"]);
  assert.equal(op.rev, '"etag-9"');
  assert.equal(queue[0], op);
});

/* ---------- eligibility filters ---------- */
test("eligibleForLoadReplay: offline (!uiRev) keeps all ops", () => {
  const stale = { k: "arch", id: "a", rev: '"old"' };
  const note = { k: "bookmark-add", bookmark: { t: 1 }, rev: '"old"' };
  assert.equal(eligibleForLoadReplay(stale, ""), true);
  assert.equal(eligibleForLoadReplay(stale, null), true);
  assert.equal(eligibleForLoadReplay(stale, undefined), true);
  assert.equal(eligibleForLoadReplay(note, ""), true);
});

test("eligibleForLoadReplay: with known rev, non-idempotent must match", () => {
  const arch = { k: "arch", id: "a", rev: '"r1"' };
  const groups = { k: "groups", groups: [], rev: '"r1"' };
  const note = { k: "bookmark-add", bookmark: { t: 1 }, rev: '"r0"' };
  assert.equal(eligibleForLoadReplay(arch, '"r1"'), true);
  assert.equal(eligibleForLoadReplay(arch, '"r2"'), false);
  assert.equal(eligibleForLoadReplay(groups, '"r2"'), false);
  assert.equal(eligibleForLoadReplay(note, '"r2"'), true, "bookmark always");
  assert.equal(
    eligibleForLoadReplay({ k: "bookmark-del", t: 1, rev: '"x"' }, '"y"'),
    true
  );
});

test("eligibleForConflictReplay: never has the offline !uiRev loophole", () => {
  // Conflict path uses: idempotentOp(op) || op.rev === uiRev
  // empty uiRev means only idempotent ops survive (offline ops never match "")
  const arch = { k: "arch", id: "a", rev: '"r1"' };
  const note = { k: "bookmark-add", bookmark: { t: 1 }, rev: '"r1"' };
  assert.equal(eligibleForConflictReplay(arch, ""), false);
  assert.equal(eligibleForConflictReplay(arch, '"r1"'), true);
  assert.equal(eligibleForConflictReplay(arch, '"r2"'), false);
  assert.equal(eligibleForConflictReplay(note, ""), true);
  assert.equal(eligibleForConflictReplay(note, '"r2"'), true);
});

test("filterOpsForLoad drops stale non-idempotent; keeps bookmarks across revs", () => {
  const ops = [
    { k: "arch", id: "a", rev: '"old"' },
    { k: "bookmark-add", bookmark: { t: 1, text: "n" }, rev: '"old"' },
    { k: "groups", groups: [{ name: "G" }], rev: '"old"' },
    { k: "pin", id: "p", rev: '"new"' },
    { k: "bookmark-del", t: 9, rev: '"x"' },
  ];
  const kept = filterOpsForLoad(ops, '"new"');
  assert.deepEqual(
    kept.map((o) => o.k),
    ["bookmark-add", "pin", "bookmark-del"]
  );
});

test("filterOpsForConflict matches flushUI 409 filter", () => {
  const ops = [
    { k: "lanes", lanes: [], rev: '"r1"' },
    { k: "bookmark-del", t: 1, rev: '"r0"' },
    { k: "unarch", id: "a", rev: '"r2"' },
  ];
  const kept = filterOpsForConflict(ops, '"r2"');
  assert.deepEqual(
    kept.map((o) => o.k),
    ["bookmark-del", "unarch"]
  );
});

/* ---------- load merge ordering ---------- */
test("mergeLoadOps: persisted (filtered) ordered before current-session ops", () => {
  const persisted = [
    { k: "arch", id: "stale", rev: '"old"' },
    { k: "bookmark-add", bookmark: { t: 1, text: "persisted" }, rev: '"old"' },
  ];
  const session = [{ k: "pin", id: "early", rev: undefined }];
  const merged = mergeLoadOps(persisted, session, '"new"');
  assert.equal(merged.length, 2);
  assert.equal(merged[0].k, "bookmark-add");
  assert.equal(merged[0].bookmark.text, "persisted");
  assert.equal(merged[1], session[0], "session op identity retained");
});

test("mergeLoadOps: offline keeps all persisted before session", () => {
  const persisted = [
    { k: "arch", id: "a", rev: '"x"' },
    { k: "groups", groups: [1], rev: '"x"' },
  ];
  const session = [{ k: "pin", id: "p" }];
  const merged = mergeLoadOps(persisted, session, "");
  assert.equal(merged.length, 3);
  assert.equal(merged[0].k, "arch");
  assert.equal(merged[1].k, "groups");
  assert.equal(merged[2].k, "pin");
});

/* ---------- remote normalize + local replay ---------- */
test("adoptRemoteAndReplay: normalize remote, filter, replay eligible ops", () => {
  const remote = { archived: ["server-a"], custom: 1 };
  const ops = [
    { k: "arch", id: "local-stale", rev: '"old"' },
    { k: "bookmark-add", bookmark: { t: 7, text: "keep" }, rev: '"old"' },
    { k: "pin", id: "p", rev: '"cur"' },
  ];
  const { doc, ops: kept } = adoptRemoteAndReplay(remote, ops, '"cur"');
  assert.equal(doc.custom, 1);
  assert.deepEqual(doc.archived, ["server-a"]); // stale arch dropped
  assert.equal(doc.bookmarks.length, 1);
  assert.equal(doc.bookmarks[0].text, "keep");
  assert.deepEqual(doc.pinned, ["p"]);
  assert.deepEqual(
    kept.map((o) => o.k),
    ["bookmark-add", "pin"]
  );
  // normUI creates a new outer object but preserves nested remote references.
  assert.notEqual(doc, remote);
  assert.equal(doc.archived, remote.archived);
});

test("loadDocumentWithReplay: remote norm + persisted-before-session + replay", () => {
  const remote = { lanes: [{ id: "L" }] };
  const persisted = [
    { k: "groups", groups: [{ name: "stale" }], rev: '"old"' },
    { k: "bookmark-add", bookmark: { t: 1, text: "p" }, rev: '"old"' },
  ];
  const session = [{ k: "arch", id: "early" }];
  const { doc, ops } = loadDocumentWithReplay(
    remote,
    persisted,
    session,
    '"new"'
  );
  assert.deepEqual(doc.lanes, [{ id: "L" }]);
  assert.deepEqual(doc.groups, []); // stale groups dropped
  assert.equal(doc.bookmarks[0].text, "p");
  assert.deepEqual(doc.archived, ["early"]);
  assert.equal(ops[0].k, "bookmark-add");
  assert.equal(ops[1].k, "arch");
});

test("bookmark replay across revision changes is always eligible", () => {
  const doc = freshDoc({ bookmarks: [{ t: 1, text: "server" }] });
  const ops = [
    {
      k: "bookmark-add",
      bookmark: { t: 2, text: "local" },
      rev: '"ancient"',
    },
    { k: "bookmark-del", t: 1, rev: '"also-old"' },
  ];
  const kept = filterOpsForLoad(ops, '"brand-new"');
  assert.equal(kept.length, 2);
  replayOps(doc, kept);
  assert.equal(doc.bookmarks.length, 1);
  assert.equal(doc.bookmarks[0].text, "local");
});

/* ---------- storage: pending ops ---------- */
test("savePendingOps / loadPendingOps round-trip", () => {
  const s = memStorage();
  const ops = [{ k: "arch", id: "a", rev: '"r"' }];
  savePendingOps(ops, s);
  assert.equal(s.map.get(PENDING_OPS_KEY), JSON.stringify(ops));
  assert.deepEqual(loadPendingOps(s), ops);
});

test("loadPendingOps: missing key → []", () => {
  assert.deepEqual(loadPendingOps(memStorage()), []);
});

test("loadPendingOps: null JSON → []", () => {
  const s = memStorage({ [PENDING_OPS_KEY]: "null" });
  assert.deepEqual(loadPendingOps(s), []);
});

test("loadPendingOps: malformed JSON → []", () => {
  const s = memStorage({ [PENDING_OPS_KEY]: "{not json" });
  assert.deepEqual(loadPendingOps(s), []);
});

test("loadPendingOps: wrong-shaped non-array truthy passes through (inline || [])", () => {
  // Characterization of current semantics — not a design endorsement.
  const s = memStorage({ [PENDING_OPS_KEY]: '{"k":1}' });
  const got = loadPendingOps(s);
  assert.deepEqual(got, { k: 1 });
});

test("loadPendingOps: getItem exception → []", () => {
  assert.deepEqual(loadPendingOps(throwingStorage({ get: true })), []);
});

test("savePendingOps: setItem exception is swallowed", () => {
  assert.doesNotThrow(() =>
    savePendingOps([{ k: "arch", id: "a" }], throwingStorage({ set: true }))
  );
});

/* ---------- storage: cached UI ---------- */
test("saveCachedUI / loadCachedUI round-trip with normUI", () => {
  const s = memStorage();
  const doc = normUI({ archived: ["a"], future: true });
  saveCachedUI(doc, s);
  const loaded = loadCachedUI(s);
  assert.deepEqual(loaded.archived, ["a"]);
  assert.equal(loaded.future, true);
  assert.deepEqual(loaded.pinned, []);
});

test("loadCachedUI: missing → empty defaults", () => {
  const d = loadCachedUI(memStorage());
  assert.deepEqual(d, emptyUI());
});

test("loadCachedUI: null JSON → defaults (normUI(null))", () => {
  const s = memStorage({ [CACHED_UI_KEY]: "null" });
  assert.deepEqual(loadCachedUI(s), emptyUI());
});

test("loadCachedUI: malformed JSON → null (caller leaves prior UI)", () => {
  const s = memStorage({ [CACHED_UI_KEY]: "not-json{{{" });
  assert.equal(loadCachedUI(s), null);
});

test("loadCachedUI: getItem exception → null", () => {
  assert.equal(loadCachedUI(throwingStorage({ get: true })), null);
});

test("saveCachedUI: setItem exception propagates", () => {
  assert.throws(() =>
    saveCachedUI(emptyUI(), throwingStorage({ set: true }))
  );
});

/* ---------- end-to-end offline/no-known-revision behavior ---------- */
test("offline load path: no rev keeps non-idempotent ops and replays on cache", () => {
  const s = memStorage({
    [CACHED_UI_KEY]: JSON.stringify({ archived: ["base"], pinned: [] }),
    [PENDING_OPS_KEY]: JSON.stringify([
      { k: "arch", id: "local", rev: '"when-online"' },
      { k: "bookmark-add", bookmark: { t: 3, text: "note" }, rev: '"when-online"' },
    ]),
  });
  const cached = loadCachedUI(s);
  assert.ok(cached);
  const persisted = loadPendingOps(s);
  const { doc, ops } = loadDocumentWithReplay(
    cached,
    persisted,
    [],
    ""
  );
  // loadDocumentWithReplay re-normalizes the already-normalized cache value.
  assert.ok(ops.length === 2);
  assert.deepEqual(doc.archived.sort(), ["base", "local"].sort());
  assert.equal(doc.bookmarks[0].text, "note");
});

test("online load path drops stale whole-list ops that would roll back remote", () => {
  const remote = {
    groups: [{ name: "server-group" }],
    lanes: [{ id: "L-server" }],
    archived: ["server-arch"],
  };
  const persisted = [
    { k: "groups", groups: [{ name: "phone-old" }], rev: '"r0"' },
    { k: "lanes", lanes: [{ id: "L-phone" }], rev: '"r0"' },
    { k: "arch", id: "phone-arch", rev: '"r0"' },
    {
      k: "bookmark-add",
      bookmark: { t: 50, text: "still-mine" },
      rev: '"r0"',
    },
  ];
  const { doc, ops } = loadDocumentWithReplay(remote, persisted, [], '"r1"');
  assert.deepEqual(doc.groups, [{ name: "server-group" }]);
  assert.deepEqual(doc.lanes, [{ id: "L-server" }]);
  assert.deepEqual(doc.archived, ["server-arch"]); // phone-arch dropped
  assert.equal(doc.bookmarks[0].text, "still-mine");
  assert.equal(ops.length, 1);
  assert.equal(ops[0].k, "bookmark-add");
});

/* ---------- full operation matrix smoke ---------- */
test("operation matrix: every known k applied once produces expected fields", () => {
  const doc = freshDoc();
  const bm = { t: 100, text: "cap", node: "n", turnTime: 1, lane: "L" };
  const comment = { t: 101, text: "c", anchor: 100 };
  applyOp(doc, { k: "arch", id: "a1" });
  applyOp(doc, { k: "unarch", id: "a1" });
  applyOp(doc, { k: "arch", id: "a2" });
  applyOp(doc, { k: "bookmark-add", bookmark: bm });
  applyOp(doc, { k: "bookmark-add", bookmark: comment });
  applyOp(doc, { k: "bookmark-del", t: 100 });
  applyOp(doc, { k: "pin", id: "p1" });
  applyOp(doc, { k: "pin", id: "p2" });
  applyOp(doc, { k: "unpin", id: "p1" });
  const order = ["p2", "p3"];
  applyOp(doc, { k: "pin-order", pinned: order });
  const groups = [{ name: "G" }];
  applyOp(doc, { k: "groups", groups });
  const lanes = [{ id: "L", name: "Lane" }];
  applyOp(doc, { k: "lanes", lanes });
  applyOp(doc, { k: "totally-unknown" });

  assert.deepEqual(doc.archived, ["a2"]);
  assert.equal(doc.bookmarks.length, 1);
  assert.equal(doc.bookmarks[0].t, 101);
  assert.equal(doc.bookmarks[0].node, "n");
  assert.equal(doc.bookmarks[0].turnTime, 1);
  assert.equal(doc.bookmarks[0].lane, "L");
  assert.equal(doc.bookmarks[0].anchor, undefined);
  assert.equal(doc.pinned, order);
  assert.equal(doc.groups, groups);
  assert.equal(doc.lanes, lanes);
});
