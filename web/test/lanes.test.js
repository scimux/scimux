/* Characterization tests for web/js/lanes.js — pure lane/fork/stop model.
 * Imports the real module (not HTML substring extraction). */
import test from "node:test";
import assert from "node:assert/strict";
import {
  LANE_COLORS,
  hashStr,
  laneList,
  byNameID,
  sortedLaneList,
  laneById,
  laneName,
  laneColor,
  uniqueLaneID,
  nextLaneColor,
  makeLane,
  servedLanes,
  laneModel,
  groupLanes,
  laneInGroup,
  stationsInGroup,
  forkKind,
  stopTimes,
  stopsOf,
  stopKey,
  stopLabel,
  newestFirst,
  parentStopIndex,
  laneColumnOrder,
} from "../js/lanes.js";
import { safeColor, stampMS } from "../js/format.js";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const lanesSrc = readFileSync(join(__dirname, "../js/lanes.js"), "utf8");

/* ---------- purity / no browser globals ---------- */
test("lanes.js has no browser globals or implicit app state", () => {
  const code = lanesSrc
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
    /\blaneOptionsHTML\b/,
    /\blaneSelectHTML\b/,
    /\bfillLaneSelect\b/,
    /\bsyncLanePicker\b/,
    /\bforkCueHTML\b/,
    /\brenderMap\b/,
    /\brenderLaneChips\b/,
    /\bnodeById\b/,
  ];
  for (const re of forbidden) {
    assert.equal(re.test(code), false, `lanes.js must not reference ${re}`);
  }
  // No implicit app globals (UI store, selection, map tab filters)
  assert.equal(/\bUI\./.test(code), false, "must not read UI.*");
  assert.equal(/\blaneList\s*\(\s*\)/.test(code), false, "laneList must take explicit lanes");
  // Duplicate safeColor / stampMS bodies are forbidden; import from format.js
  assert.match(lanesSrc, /from\s+["']\.\/format\.js["']/);
  assert.equal(/\bfunction\s+safeColor\b/.test(code), false);
  assert.equal(/\bconst\s+stampMS\b/.test(code), false);
  assert.equal(/\bfunction\s+stampMS\b/.test(code), false);
});

test("lanes.js exports named pure helpers", async () => {
  const mod = await import("../js/lanes.js");
  for (const name of [
    "LANE_COLORS", "hashStr", "laneList", "byNameID", "sortedLaneList",
    "laneById", "laneName", "laneColor", "uniqueLaneID", "nextLaneColor",
    "makeLane", "servedLanes", "laneModel", "groupLanes", "laneInGroup",
    "stationsInGroup", "forkKind", "stopTimes", "stopsOf", "stopKey",
    "stopLabel", "newestFirst", "parentStopIndex", "laneColumnOrder", "hashTurns",
  ]) {
    assert.ok(name in mod, name);
  }
  assert.ok(Array.isArray(mod.LANE_COLORS));
  assert.equal(mod.LANE_COLORS.length, 7);
});

test("lanes.js reuses format.js safeColor and stampMS (no local copies)", () => {
  // Live import identity: if lanes reimplemented them, tests would still pass
  // against locals — the source import + absence of local defs is the check.
  // Also prove the shared functions behave as expected when used by laneColor.
  assert.equal(safeColor("#007AFF"), "#007AFF");
  assert.equal(safeColor("javascript:alert(1)"), null);
  assert.equal(stampMS("2024-01-01T00:00:00.000Z"), Date.parse("2024-01-01T00:00:00.000Z"));
  assert.equal(stampMS(""), 0);
  assert.equal(stampMS("not-a-date"), 0);
});

/* ---------- byNameID / sortedLaneList ---------- */
test("byNameID: sorts by name then id; base-insensitive; ties break on id", () => {
  const a = { id: "b", name: "Alpha" };
  const b = { id: "a", name: "alpha" };
  const c = { id: "c", name: "Beta" };
  // same name (base): id order a before b
  assert.ok(byNameID(b, a) < 0);
  assert.ok(byNameID(a, b) > 0);
  assert.ok(byNameID(a, c) < 0);
  // missing name falls back to id
  assert.ok(byNameID({ id: "z" }, { id: "m" }) > 0);
  // equal name+id → 0
  assert.equal(byNameID({ id: "x", name: "N" }, { id: "x", name: "N" }), 0);
});

test("sortedLaneList: deterministic copy; non-array → empty", () => {
  const lanes = [
    { id: "z", name: "Zebra" },
    { id: "a", name: "Alpha" },
    { id: "b", name: "alpha" },
  ];
  const sorted = sortedLaneList(lanes);
  assert.deepEqual(sorted.map(l => l.id), ["a", "b", "z"]);
  assert.notEqual(sorted, lanes); // copy
  assert.deepEqual(sortedLaneList(null), []);
  assert.deepEqual(sortedLaneList(undefined), []);
  assert.deepEqual(laneList(undefined), []);
  assert.deepEqual(laneList([{ id: "x" }]).map(l => l.id), ["x"]);
});

/* ---------- laneById / laneName ---------- */
test("laneById and laneName: lookup and fallbacks", () => {
  const lanes = [{ id: "eng", name: "Engineering", color: "#007AFF" }];
  assert.equal(laneById("eng", lanes)?.name, "Engineering");
  assert.equal(laneById("missing", lanes), null);
  assert.equal(laneName("eng", lanes), "Engineering");
  assert.equal(laneName("orphan", lanes), "orphan");
  assert.equal(laneName("", lanes), "unset");
  assert.equal(laneName(null, lanes), "unset");
  assert.equal(laneName(undefined, []), "unset");
});

/* ---------- palette / color ---------- */
test("LANE_COLORS palette and nextLaneColor wrapping", () => {
  assert.equal(LANE_COLORS.length, 7);
  assert.equal(nextLaneColor([]), LANE_COLORS[0]);
  assert.equal(nextLaneColor([{ id: "a" }]), LANE_COLORS[1]);
  // wrap
  const full = LANE_COLORS.map((_, i) => ({ id: "l" + i }));
  assert.equal(nextLaneColor(full), LANE_COLORS[0]);
  assert.equal(nextLaneColor(full.concat({ id: "x" })), LANE_COLORS[1]);
});

test("laneColor: safe authored color, unlane empty, hash palette fallback", () => {
  const lanes = [
    { id: "ok", name: "OK", color: "#FF9500" },
    { id: "bad", name: "Bad", color: 'red;"><script>' },
    { id: "var", name: "Var", color: "var(--accent)" },
  ];
  assert.equal(laneColor("ok", lanes), "#FF9500");
  assert.equal(laneColor("var", lanes), "var(--accent)");
  // unsafe authored → treat as missing, fall through to hash palette
  const badExpected = LANE_COLORS[Math.abs(hashStr("bad")) % LANE_COLORS.length];
  assert.equal(laneColor("bad", lanes), badExpected);
  assert.equal(laneColor("", lanes), "var(--unlane)");
  assert.equal(laneColor(null, lanes), "var(--unlane)");
  // unknown id: hash into palette
  const orphan = LANE_COLORS[Math.abs(hashStr("ghost")) % LANE_COLORS.length];
  assert.equal(laneColor("ghost", lanes), orphan);
});

test("hashStr: deterministic signed 32-bit djb2", () => {
  assert.equal(hashStr("eng"), hashStr("eng"));
  assert.notEqual(hashStr("a"), hashStr("b"));
  // known stability: same algorithm as inline
  let h = 5381;
  const s = "test";
  for (let i = 0; i < s.length; i++) h = ((h << 5) + h + s.charCodeAt(i)) | 0;
  assert.equal(hashStr("test"), h);
});

test("hashTurns equals joining the turns and hashing that, without building the string", async () => {
  /* The chat signature hashed every turn of the transcript on every poll —
     including the 304 path and including the branch that then skips the
     rebuild. The join allocated the whole conversation as one fresh string
     every 2s. hashTurns must be a drop-in: same value, no allocation. */
  const { hashTurns } = await import("../js/lanes.js");
  const joined = turns => hashStr(turns.map(t => t.role + "\u0000" + t.text).join("\u0001"));
  const cases = [
    [],
    [{ role: "user", text: "hello" }],
    [{ role: "user", text: "hello" }, { role: "assistant", text: "hi there" }],
    [{ role: "user", text: "" }, { role: "user", text: "" }],
    [{ role: "user", text: "a\u0001b" }, { role: "user", text: "c" }],
    [{ role: undefined, text: undefined }],
    [{ role: "user", text: "\u{1F600} unicode" }],
  ];
  for (const c of cases)
    assert.equal(hashTurns(c), joined(c), JSON.stringify(c));
  assert.equal(hashTurns(null), hashStr(""), "no turns hashes like no text");
  /* Separators matter: moving a boundary must move the hash. */
  assert.notEqual(
    hashTurns([{ role: "user", text: "ab" }]),
    hashTurns([{ role: "user", text: "a" }, { role: "b", text: "" }]));
});

/* ---------- uniqueLaneID / makeLane ---------- */
test("uniqueLaneID: normalize and collision suffixes", () => {
  assert.equal(uniqueLaneID("Engineering", []), "engineering");
  assert.equal(uniqueLaneID("  Foo Bar  ", []), "foo-bar");
  assert.equal(uniqueLaneID("!!!", []), "lane"); // stripped empty → "lane"
  assert.equal(uniqueLaneID("", []), "lane");
  assert.equal(uniqueLaneID(null, []), "lane");
  const used = [{ id: "foo" }, { id: "foo-2" }];
  assert.equal(uniqueLaneID("Foo", used), "foo-3");
  assert.equal(uniqueLaneID("foo", used), "foo-3");
  // dots underscores hyphens preserved
  assert.equal(uniqueLaneID("a.b_c-d", []), "a.b_c-d");
});

test("makeLane: trim name, assign id and next palette color", () => {
  const lanes = [{ id: "a", name: "A", color: "#007AFF" }];
  const lane = makeLane("  New Topic  ", lanes);
  assert.equal(lane.name, "New Topic");
  assert.equal(lane.id, "new-topic");
  assert.equal(lane.color, LANE_COLORS[1]); // length 1 → index 1
  // collision with existing
  const lanes2 = [{ id: "new-topic", name: "x", color: "#000" }];
  assert.equal(makeLane("New Topic", lanes2).id, "new-topic-2");
});

/* ---------- servedLanes ---------- */
test("servedLanes: single membership; unset drops", () => {
  assert.deepEqual(servedLanes({ lane_id: "eng" }), ["eng"]);
  assert.deepEqual(servedLanes({ lane_id: "" }), []);
  assert.deepEqual(servedLanes({}), []);
  assert.deepEqual(servedLanes({ lane_id: null }), []);
});

/* ---------- laneModel ---------- */
test("laneModel: projects used configured lanes + missing ids; sorts", () => {
  const lanes = [
    { id: "z", name: "Zebra", color: "#34C759" },
    { id: "a", name: "Alpha", color: "#007AFF" },
    { id: "unused", name: "Unused", color: "#FF2D55" },
  ];
  const nodes = [
    { id: "n1", lane_id: "z", title: "Z1" },
    { id: "n2", lane_id: "a", title: "A1" },
    { id: "n3", lane_id: "ghost", title: "orphan lane id" },
    { id: "n4", lane_id: "", title: "unassigned" },
  ];
  const lm = laneModel(nodes, lanes);
  assert.deepEqual(lm.lanes.map(l => l.id), ["a", "ghost", "z"]);
  // unused configured lane omitted
  assert.equal(lm.lanes.some(l => l.id === "unused"), false);
  // synthetic missing lane uses id as name and hash color
  const ghost = lm.lanes.find(l => l.id === "ghost");
  assert.equal(ghost.name, "ghost");
  assert.equal(ghost.color, laneColor("ghost", lanes));
  // byId index
  assert.equal(lm.byId.n1.title, "Z1");
  assert.equal(lm.color("a"), "#007AFF");
  assert.equal(lm.name("a"), "Alpha");
  assert.equal(lm.name("ghost"), "ghost");
  // empty / non-array safety
  assert.deepEqual(laneModel(null, null).lanes, []);
  assert.deepEqual(laneModel([], lanes).lanes, []);
});

/* ---------- groupLanes / filtering ---------- */
test("groupLanes: all → null; known group → lanes; unknown → null", () => {
  const groups = [
    { id: "g1", name: "G1", lanes: ["a", "b"] },
    { id: "g2", name: "G2", lanes: [] },
  ];
  assert.equal(groupLanes("all", groups), null);
  assert.deepEqual(groupLanes("g1", groups), ["a", "b"]);
  assert.deepEqual(groupLanes("g2", groups), []);
  assert.equal(groupLanes("missing", groups), null);
  assert.equal(groupLanes("g1", null), null);
  assert.equal(groupLanes("g1", undefined), null);
});

test("laneInGroup and stationsInGroup filtering", () => {
  assert.equal(laneInGroup(null, "a"), true);
  assert.equal(laneInGroup(["a", "b"], "a"), true);
  assert.equal(laneInGroup(["a", "b"], "c"), false);
  const nodes = [
    { id: "1", lane_id: "a" },
    { id: "2", lane_id: "b" },
    { id: "3", lane_id: "c" },
    { id: "4", lane_id: "" },
  ];
  assert.deepEqual(stationsInGroup(nodes, null).map(n => n.id), ["1", "2", "3"]);
  assert.deepEqual(stationsInGroup(nodes, ["a", "c"]).map(n => n.id), ["1", "3"]);
  assert.deepEqual(stationsInGroup(nodes, ["x"]).map(n => n.id), []);
});

/* ---------- forkKind ---------- */
test("forkKind: null without parent or when parent missing", () => {
  const nodes = [{ id: "root", lane_id: "a", created_at: "2024-01-01T00:00:00Z" }];
  assert.equal(forkKind({ id: "root", lane_id: "a" }, nodes), null);
  assert.equal(forkKind({ id: "c", parent: "gone", lane_id: "a" }, nodes), null);
});

test("forkKind: stored fork_kind takes precedence over legacy recompute", () => {
  const nodes = [
    { id: "p", lane_id: "a", created_at: "2024-01-01T00:00:00Z" },
    { id: "older", lane_id: "b", created_at: "2024-01-01T01:00:00Z" },
    // Without stored kind this would be "s" (prior in dest); stored says y-new
    { id: "c", parent: "p", lane_id: "b", created_at: "2024-01-01T02:00:00Z", fork_kind: "y-new" },
  ];
  assert.equal(forkKind(nodes[2], nodes), "y-new");
  // Stored y-stay even if lanes differ
  const cross = { id: "x", parent: "p", lane_id: "b", created_at: "2024-01-01T03:00:00Z", fork_kind: "y-stay" };
  assert.equal(forkKind(cross, nodes.concat(cross)), "y-stay");
  const s = { id: "s", parent: "p", lane_id: "b", created_at: "2024-01-01T03:00:00Z", fork_kind: "s" };
  assert.equal(forkKind(s, nodes.concat(s)), "s");
});

test("forkKind legacy: y-stay, y-new, s crossover", () => {
  const p = { id: "p", lane_id: "a", created_at: "2024-01-01T00:00:00Z" };
  const same = { id: "c1", parent: "p", lane_id: "a", created_at: "2024-01-01T02:00:00Z" };
  const nodesStay = [p, same];
  assert.equal(forkKind(same, nodesStay), "y-stay");

  const fresh = { id: "c2", parent: "p", lane_id: "b", created_at: "2024-01-01T02:00:00Z" };
  assert.equal(forkKind(fresh, [p, fresh]), "y-new");

  const prior = { id: "prior", lane_id: "b", created_at: "2024-01-01T01:00:00Z" };
  const cross = { id: "c3", parent: "p", lane_id: "b", created_at: "2024-01-01T02:00:00Z" };
  assert.equal(forkKind(cross, [p, prior, cross]), "s");
});

test("forkKind stability: stored kind survives sibling deletion", () => {
  const p = { id: "p", lane_id: "a", created_at: "2024-01-01T00:00:00Z" };
  const older = { id: "older", lane_id: "b", created_at: "2024-01-01T01:00:00Z" };
  // Stored as crossover when created with an older peer on dest
  const child = {
    id: "c", parent: "p", lane_id: "b",
    created_at: "2024-01-01T02:00:00Z", fork_kind: "s",
  };
  assert.equal(forkKind(child, [p, older, child]), "s");
  // Sibling removed: live recompute would become y-new; stored stays "s"
  assert.equal(forkKind(child, [p, child]), "s");
  // Without stored field, sibling deletion WOULD flip — characterize that too
  const legacy = { id: "c", parent: "p", lane_id: "b", created_at: "2024-01-01T02:00:00Z" };
  assert.equal(forkKind(legacy, [p, older, legacy]), "s");
  assert.equal(forkKind(legacy, [p, legacy]), "y-new");
});

test("forkKind stability: lane-membership change does not rewrite stored kind", () => {
  const p = { id: "p", lane_id: "a", created_at: "2024-01-01T00:00:00Z" };
  const child = {
    id: "c", parent: "p", lane_id: "b",
    created_at: "2024-01-01T02:00:00Z", fork_kind: "y-new",
  };
  // Even if we pretend the child now shares the parent lane, stored wins
  const moved = { ...child, lane_id: "a" };
  assert.equal(forkKind(moved, [p, moved]), "y-new");
});

/* ---------- stops ---------- */
test("stopTimes / stopsOf / stopKey / newestFirst", () => {
  const n = {
    id: "n1",
    created_at: "2024-01-01T00:00:00Z",
    stops: ["2024-01-02T00:00:00Z", "2024-01-03T00:00:00Z"],
    title: "T",
  };
  assert.deepEqual(stopTimes(n), [
    "2024-01-01T00:00:00Z",
    "2024-01-02T00:00:00Z",
    "2024-01-03T00:00:00Z",
  ]);
  assert.deepEqual(stopTimes({}), [""]);
  assert.deepEqual(stopTimes({ created_at: "x" }), ["x"]);

  const stops = stopsOf(n);
  assert.equal(stops.length, 3);
  assert.equal(stops[0].i, 0);
  assert.equal(stops[0].head, false);
  assert.equal(stops[0].time, "2024-01-01T00:00:00Z");
  assert.equal(stops[2].head, true);
  assert.equal(stops[2].n, n);
  assert.equal(stopKey(stops[0]), "n1#0");
  assert.equal(stopKey(stops[2]), "n1#2");

  const ordered = [...stops].sort(newestFirst);
  assert.deepEqual(ordered.map(s => s.i), [2, 1, 0]);

  // missing/invalid timestamps: stampMS → 0, then localeCompare on time strings
  const a = { n: {}, i: 0, time: "" };
  const b = { n: {}, i: 1, time: "zzz" };
  assert.equal(stampMS("zzz"), 0);
  // newestFirst(a,b) = 0 || b.time.localeCompare(a.time)
  assert.equal(newestFirst(a, b), "zzz".localeCompare(""));
  assert.equal(newestFirst(b, a), "".localeCompare("zzz"));
});

test("stopLabel: head is live; frozen snapshot; legacy fallback", () => {
  const n = {
    id: "n1",
    title: "Live title",
    description: "Live desc",
    prompt: "prompt fallback",
    created_at: "2024-01-01T00:00:00Z",
    stops: ["2024-01-02T00:00:00Z"],
    station_labels: {
      "2024-01-01T00:00:00Z": { title: "Frozen", desc: "Old desc" },
    },
  };
  const [s0, s1] = stopsOf(n);
  // head (s1) always live
  assert.deepEqual(stopLabel(s1), { title: "Live title", desc: "Live desc" });
  // historical with snapshot
  assert.deepEqual(stopLabel(s0), { title: "Frozen", desc: "Old desc" });
  // historical without snapshot → live fallback
  const legacy = {
    id: "n2", title: "T", description: "", prompt: "P",
    created_at: "2024-01-01T00:00:00Z", stops: ["2024-01-02T00:00:00Z"],
  };
  const [h0, h1] = stopsOf(legacy);
  assert.deepEqual(stopLabel(h0), { title: "T", desc: "P" }); // prompt fills desc
  assert.deepEqual(stopLabel(h1), { title: "T", desc: "P" });
  // empty snap fields
  const n3 = {
    id: "n3", title: "Live", description: "D",
    created_at: "t0", stops: [],
    station_labels: { t0: { title: "", desc: "" } },
  };
  // single stop is head — still live even if a label key exists
  const only = stopsOf(n3)[0];
  assert.equal(only.head, true);
  assert.deepEqual(stopLabel(only), { title: "Live", desc: "D" });
});

test("parentStopIndex: timestamp boundaries and missing timestamps", () => {
  const p = {
    id: "p",
    created_at: "2024-01-01T00:00:00Z",
    stops: [
      "2024-01-01T12:00:00Z",
      "2024-01-02T00:00:00Z",
      "2024-01-03T00:00:00Z",
    ],
  };
  // before any clear → creation stop 0
  assert.equal(parentStopIndex(p, "2024-01-01T06:00:00Z"), 0);
  // exactly at stop 1 → index 1
  assert.equal(parentStopIndex(p, "2024-01-01T12:00:00Z"), 1);
  // between stop 1 and 2 → 1
  assert.equal(parentStopIndex(p, "2024-01-01T18:00:00Z"), 1);
  // after last → last index
  assert.equal(parentStopIndex(p, "2024-01-04T00:00:00Z"), 3);
  // missing / invalid childCreated → 0 (c is falsy via stampMS 0)
  assert.equal(parentStopIndex(p, ""), 0);
  assert.equal(parentStopIndex(p, null), 0);
  assert.equal(parentStopIndex(p, "bogus"), 0);
  // parent with only creation
  assert.equal(parentStopIndex({ created_at: "2024-01-01T00:00:00Z" }, "2024-01-02T00:00:00Z"), 0);
  assert.equal(parentStopIndex({}, "2024-01-02T00:00:00Z"), 0);
});

/* ---------- laneColumnOrder ---------- */
test("laneColumnOrder: adjacency via cross-lane forks, weight, recency, isolates", () => {
  // Three lanes: A forks heavily to B, C is isolated and newest
  const colLanes = [
    { id: "a", name: "A" },
    { id: "b", name: "B" },
    { id: "c", name: "C" },
  ];
  const stations = [
    { id: "ra", lane_id: "a", created_at: "2024-01-01T00:00:00Z" },
    { id: "rb", lane_id: "b", created_at: "2024-01-01T01:00:00Z", parent: "ra" },
    { id: "rb2", lane_id: "b", created_at: "2024-01-01T02:00:00Z", parent: "ra" },
    { id: "rc", lane_id: "c", created_at: "2024-01-05T00:00:00Z" },
  ];
  const byId = Object.fromEntries(stations.map(s => [s.id, s]));
  const order = laneColumnOrder(colLanes, stations, byId);
  // a and b are adjacent (edge weight 2); c isolated, high recency
  const ai = order.indexOf("a"), bi = order.indexOf("b");
  assert.equal(Math.abs(ai - bi), 1, `a and b should be adjacent: ${order}`);
  assert.ok(order.includes("c"));
  assert.equal(order.length, 3);
});

test("laneColumnOrder: ties break by recency; missing timestamps → rec 0", () => {
  // No fork edges: pure bestFree by recency (then Set iteration order of sort)
  const colLanes = [
    { id: "old", name: "Old" },
    { id: "new", name: "New" },
    { id: "mid", name: "Mid" },
  ];
  const stations = [
    { id: "1", lane_id: "old", created_at: "2024-01-01T00:00:00Z" },
    { id: "2", lane_id: "mid", created_at: "2024-01-02T00:00:00Z" },
    { id: "3", lane_id: "new", created_at: "2024-01-03T00:00:00Z" },
  ];
  const byId = Object.fromEntries(stations.map(s => [s.id, s]));
  assert.deepEqual(laneColumnOrder(colLanes, stations, byId), ["new", "mid", "old"]);

  // Missing created_at → Date.parse falsy → rec 0
  const bare = [
    { id: "x", name: "X" },
    { id: "y", name: "Y" },
  ];
  const bareStations = [
    { id: "sx", lane_id: "x" }, // missing created_at
    { id: "sy", lane_id: "y", created_at: "not-a-date" },
  ];
  const bareById = Object.fromEntries(bareStations.map(s => [s.id, s]));
  const bareOrder = laneColumnOrder(bare, bareStations, bareById);
  assert.equal(bareOrder.length, 2);
  assert.ok(bareOrder.includes("x") && bareOrder.includes("y"));
});

test("laneColumnOrder: same-lane fork does not create edge; parent outside set ignored", () => {
  const colLanes = [{ id: "a" }, { id: "b" }];
  const stations = [
    { id: "p", lane_id: "a", created_at: "2024-01-01T00:00:00Z" },
    // same-lane child: not a column edge
    { id: "c1", lane_id: "a", created_at: "2024-01-02T00:00:00Z", parent: "p" },
    // parent outside col set
    { id: "c2", lane_id: "b", created_at: "2024-01-03T00:00:00Z", parent: "outside" },
  ];
  const byId = { p: stations[0], c1: stations[1], c2: stations[2] };
  // no edges → order by recency only (b newer than a)
  assert.deepEqual(laneColumnOrder(colLanes, stations, byId), ["b", "a"]);
});

test("laneColumnOrder: higher edge weight preferred when chaining from last", () => {
  // Seed: start free-pick is highest total weight. a-b weight 3, a-c weight 1.
  const colLanes = [{ id: "a" }, { id: "b" }, { id: "c" }];
  const stations = [];
  const mk = (id, lane, parent, t) => {
    stations.push({ id, lane_id: lane, parent, created_at: t });
  };
  mk("ra", "a", null, "2024-01-01T00:00:00Z");
  mk("rb0", "b", "ra", "2024-01-01T01:00:00Z");
  mk("rb1", "b", "ra", "2024-01-01T02:00:00Z");
  mk("rb2", "b", "ra", "2024-01-01T03:00:00Z");
  mk("rc0", "c", "ra", "2024-01-01T04:00:00Z");
  const byId = Object.fromEntries(stations.map(s => [s.id, s]));
  const order = laneColumnOrder(colLanes, stations, byId);
  // a has highest total weight → starts; then b (weight 3) over c (weight 1)
  assert.equal(order[0], "a");
  assert.equal(order[1], "b");
  assert.equal(order[2], "c");
});

/* ---------- explicit-input compatibility smoke (mirrors inline shapes) ---------- */
test("explicit-input shapes match inline algorithm contracts", () => {
  // Inline: laneList() → UI.lanes; module: laneList(lanes)
  // Inline: forkKind uses nodeById + nodes; module takes nodes array
  // Inline: laneModel uses nodes + UI.lanes; module takes both
  const lanes = [{ id: "eng", name: "Eng", color: "#007AFF" }];
  const nodes = [
    { id: "p", lane_id: "eng", created_at: "2024-06-01T00:00:00Z", title: "Parent" },
    {
      id: "c", parent: "p", lane_id: "eng", created_at: "2024-06-02T00:00:00Z",
      title: "Child", fork_kind: "y-stay", stops: ["2024-06-03T00:00:00Z"],
      // snapshot keyed by the historical stop's start time (creation seam)
      station_labels: { "2024-06-02T00:00:00Z": { title: "Was", desc: "Then" } },
    },
  ];
  const lm = laneModel(nodes, lanes);
  assert.equal(lm.lanes.length, 1);
  assert.equal(forkKind(nodes[1], nodes), "y-stay");
  assert.equal(parentStopIndex(nodes[0], nodes[1].created_at), 0);
  const chain = stopsOf(nodes[1]);
  assert.equal(chain.length, 2);
  assert.deepEqual(stopLabel(chain[0]), { title: "Was", desc: "Then" });
  assert.deepEqual(stopLabel(chain[1]), { title: "Child", desc: "" });
  assert.deepEqual(
    laneColumnOrder(lm.lanes, nodes, lm.byId),
    ["eng"],
  );
});
