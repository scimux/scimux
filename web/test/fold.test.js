/* Folding of intermediate agent bubbles. The module is pure: these tests
 * pass the timeline and the storage record in, and read the plan back.
 * Nothing here touches a browser or a server. */
import test from "node:test";
import assert from "node:assert/strict";
import {
  foldKey,
  readFoldState,
  writeFoldState,
  foldPlan,
  foldTurn,
  foldAll,
  openRange,
  closeRange,
  foldRowHTML,
  foldStateVersion,
} from "../js/fold.js";

function empty(){
  return { all: false, turns: [], open: [] };
}

function user(record, extra = {}){
  return {
    kind: "turn",
    record,
    turn: { role: "user", uid: "log-1", segment: 0, record, text: "u" + record, ...extra },
  };
}

function agent(record, extra = {}){
  return {
    kind: "turn",
    record,
    turn: { role: "assistant", uid: "log-1", segment: 0, record, text: "a" + record, ...extra },
  };
}

function decision(record){
  return { kind: "decision", record, decision: { record } };
}

function seam(){
  return { kind: "seam" };
}

function keyOf(record, segment = 0){
  return segment + "/" + record;
}

/* One user prompt followed by n agent bubbles, records 1..n+1. */
function promptWithAgents(n){
  const items = [user(1)];
  for (let i = 0; i < n; i++) items.push(agent(i + 2));
  return items;
}

function plan(items, state = empty(), protectedRecords = null){
  return foldPlan(items, {
    state,
    isProtected: protectedRecords
      ? (turn => protectedRecords.has(turn.record))
      : undefined,
  });
}

function shownText(items, result){
  const out = [];
  for (const step of result.sequence){
    if (step.row){
      out.push(step.row.kind + ":" + step.row.count + ":" + step.row.key);
      continue;
    }
    const item = items[step.index];
    if (!item){
      out.push("empty");
      continue;
    }
    if (item.kind === "decision") out.push("decision:" + item.record);
    else if (item.kind === "seam") out.push("seam");
    else if (item.kind === "turn"){
      const role = item.turn.role === "user" ? "user" : "agent";
      out.push(role + ":" + item.turn.text);
    } else out.push(item.kind);
  }
  return out;
}

function memoryStore(seed = {}){
  const map = new Map(Object.entries(seed));
  return {
    getItem: key => (map.has(key) ? map.get(key) : null),
    setItem: (key, value) => { map.set(key, String(value)); },
    map,
  };
}

test("foldKey reads a missing segment and a missing record as 0", () => {
  assert.equal(foldKey({ uid: "log-1" }), "0/0");
  assert.equal(foldKey({ uid: "log-1", segment: 0, record: 0 }), "0/0");
  assert.equal(foldKey({ uid: "log-1", record: 4 }), "0/4");
  assert.equal(foldKey({ uid: "log-1", segment: 3 }), "3/0");
  assert.equal(foldKey({ uid: "log-1", segment: 2, record: 5 }), "2/5");
  assert.equal(foldKey({ uid: "log-1", segment: "4", record: "8" }), "4/8");
});

test("foldKey without a uid is empty", () => {
  assert.equal(foldKey({ segment: 1, record: 2 }), "");
  assert.equal(foldKey({ uid: "", segment: 1, record: 2 }), "");
  assert.equal(foldKey(null), "");
  assert.equal(foldKey(undefined), "");
});

test("a prompt with no agent bubbles has nothing to fold", () => {
  const items = promptWithAgents(0);
  const result = plan(items);
  assert.deepEqual(result.turns, []);
  assert.deepEqual(result.rows, []);
  assert.equal(result.canFoldAny, false);
  assert.deepEqual(shownText(items, result), ["user:u1"]);
});

test("a prompt with one agent bubble has no range", () => {
  const items = promptWithAgents(1);
  const result = plan(items);
  assert.deepEqual(result.rows, []);
  assert.equal(result.turns.length, 1);
  assert.equal(result.turns[0].firstKey, "0/2");
  assert.equal(result.turns[0].canFold, false);
  assert.deepEqual(result.turns[0].bubbleKeys, ["0/2"]);
  assert.deepEqual(shownText(items, result), ["user:u1", "agent:a2"]);
});

test("a prompt with two agent bubbles has no range", () => {
  const items = promptWithAgents(2);
  const result = plan(items);
  assert.deepEqual(result.rows, []);
  assert.equal(result.turns[0].canFold, false);
  assert.deepEqual(result.ranges, []);
  assert.deepEqual(shownText(items, result), ["user:u1", "agent:a2", "agent:a3"]);
});

test("a prompt with three agent bubbles has one middle bubble", () => {
  const items = promptWithAgents(3);
  const open = plan(items);
  assert.equal(open.canFoldAny, true);
  assert.equal(open.turns[0].canFold, true);
  assert.deepEqual(open.ranges.map(range => range.keys), [["0/3"]]);
  assert.deepEqual(open.rows, []);
  assert.deepEqual(shownText(items, open), ["user:u1", "agent:a2", "agent:a3", "agent:a4"]);

  const folded = plan(items, { all: false, turns: ["0/2"], open: [] });
  assert.equal(folded.turns[0].canFold, false);
  assert.equal(folded.rows.length, 1);
  assert.equal(folded.rows[0].kind, "open");
  assert.equal(folded.rows[0].count, 1);
  assert.equal(folded.rows[0].key, "0/3");
  assert.deepEqual(folded.rows[0].keys, ["0/3"]);
  assert.deepEqual(shownText(items, folded), ["user:u1", "agent:a2", "open:1:0/3", "agent:a4"]);
});

test("leadFirstKey folds the leading window group by its supplied key", () => {
  const items = [agent(5), agent(6), agent(7), agent(8)];
  const open = foldPlan(items, { state: empty(), leadFirstKey: "0/3" });
  assert.equal(open.turns[0].firstKey, "0/3");
  assert.equal(open.turns[0].canFold, true);
  const folded = foldPlan(items, {
    state: { all: false, turns: ["0/3"], open: [] },
    leadFirstKey: "0/3",
  });
  assert.deepEqual(folded.shown, [false, false, false, true]);
  assert.deepEqual(folded.rows.map(row => row.count), [3]);
});

test("leadFirstKey supports a single leading agent and ignores a user-led list", () => {
  const one = foldPlan([agent(5)], { state: empty(), leadFirstKey: "0/3" });
  assert.equal(one.turns[0].firstKey, "0/3");
  assert.equal(one.turns[0].canFold, false);
  const items = [user(1), agent(2), agent(3), agent(4)];
  const ordinary = foldPlan(items, { state: empty() });
  const withLead = foldPlan(items, { state: empty(), leadFirstKey: "8/99" });
  assert.deepEqual(withLead, ordinary);
});

test("a prompt with twelve agent bubbles folds the ten in the middle", () => {
  const items = promptWithAgents(12);
  const folded = plan(items, { all: false, turns: ["0/2"], open: [] });
  assert.equal(folded.rows.length, 1);
  assert.equal(folded.rows[0].kind, "open");
  assert.equal(folded.rows[0].count, 10);
  assert.equal(folded.rows[0].key, "0/3");
  assert.equal(folded.rows[0].keys.length, 10);
  assert.equal(folded.rows[0].keys[9], "0/12");
  const labels = shownText(items, folded);
  assert.deepEqual(labels, ["user:u1", "agent:a2", "open:10:0/3", "agent:a13"]);
  assert.equal(folded.shown.filter(Boolean).length, 3);
});

test("two prompts fold as two turns", () => {
  const items = [
    user(1), agent(2), agent(3), agent(4),
    user(5), agent(6), agent(7), agent(8),
  ];
  const folded = plan(items, { all: false, turns: ["0/2"], open: [] });
  assert.equal(folded.turns.length, 2);
  assert.equal(folded.turns[0].firstKey, "0/2");
  assert.equal(folded.turns[0].canFold, false);
  assert.equal(folded.turns[1].firstKey, "0/6");
  assert.equal(folded.turns[1].canFold, true);
  assert.deepEqual(shownText(items, folded), [
    "user:u1", "agent:a2", "open:1:0/3", "agent:a4",
    "user:u5", "agent:a6", "agent:a7", "agent:a8",
  ]);
});

test("agent bubbles before the first prompt are a turn of their own", () => {
  const items = [agent(1), agent(2), agent(3), agent(4), user(5), agent(6)];
  const folded = plan(items, { all: false, turns: ["0/1"], open: [] });
  assert.equal(folded.turns.length, 2);
  assert.equal(folded.turns[0].firstKey, "0/1");
  assert.deepEqual(folded.turns[0].bubbleKeys, ["0/1", "0/2", "0/3", "0/4"]);
  assert.equal(folded.turns[1].canFold, false);
  assert.deepEqual(shownText(items, folded), [
    "agent:a1", "open:2:0/2", "agent:a4", "user:u5", "agent:a6",
  ]);
});

test("a seam ends the turn", () => {
  const items = [
    user(1), agent(2), agent(3), agent(4),
    seam(),
    agent(5), agent(6), agent(7),
  ];
  const folded = plan(items, { all: false, turns: ["0/2"], open: [] });
  assert.equal(folded.turns.length, 2);
  assert.equal(folded.turns[0].firstKey, "0/2");
  assert.equal(folded.turns[1].firstKey, "0/5");
  assert.equal(folded.turns[1].canFold, true);
  assert.deepEqual(shownText(items, folded), [
    "user:u1", "agent:a2", "open:1:0/3", "agent:a4",
    "seam",
    "agent:a5", "agent:a6", "agent:a7",
  ]);
});

test("a protected bubble in the middle gives two ranges", () => {
  const items = [
    user(1), agent(2), agent(3), agent(4), agent(5), agent(6), agent(7), agent(8),
  ];
  const seen = [];
  const result = foldPlan(items, {
    state: { all: false, turns: ["0/2"], open: [] },
    isProtected(turn){
      seen.push(turn.record);
      return turn.record === 5;
    },
  });
  assert.deepEqual(seen, [3, 4, 5, 6, 7]);
  assert.deepEqual(result.ranges.map(range => range.keys), [
    ["0/3", "0/4"],
    ["0/6", "0/7"],
  ]);
  assert.deepEqual(shownText(items, result), [
    "user:u1", "agent:a2", "open:2:0/3", "agent:a5", "open:2:0/6", "agent:a8",
  ]);
});

test("two protected bubbles next to each other leave the side ranges", () => {
  const items = [
    user(1), agent(2), agent(3), agent(4), agent(5), agent(6), agent(7),
  ];
  const result = plan(items, { all: false, turns: ["0/2"], open: [] }, new Set([4, 5]));
  assert.deepEqual(result.ranges.map(range => range.keys), [["0/3"], ["0/6"]]);
  assert.deepEqual(shownText(items, result), [
    "user:u1", "agent:a2", "open:1:0/3", "agent:a4", "agent:a5", "open:1:0/6", "agent:a7",
  ]);
});

test("a protected first bubble changes nothing about the range", () => {
  const items = promptWithAgents(4);
  const plain = plan(items, { all: false, turns: ["0/2"], open: [] });
  const marked = plan(items, { all: false, turns: ["0/2"], open: [] }, new Set([2]));
  assert.deepEqual(marked.ranges.map(range => range.keys), plain.ranges.map(range => range.keys));
  assert.deepEqual(shownText(items, marked), shownText(items, plain));
  assert.deepEqual(marked.ranges.map(range => range.keys), [["0/3", "0/4"]]);
});

test("a protected last bubble changes nothing about the range", () => {
  const items = promptWithAgents(4);
  const marked = plan(items, { all: false, turns: ["0/2"], open: [] }, new Set([5]));
  assert.deepEqual(marked.ranges.map(range => range.keys), [["0/3", "0/4"]]);
  assert.deepEqual(shownText(items, marked), [
    "user:u1", "agent:a2", "open:2:0/3", "agent:a5",
  ]);
});

test("a bubble without a key splits a range", () => {
  const items = [
    user(1),
    agent(2),
    agent(3),
    agent(4, { uid: "" }),
    agent(5),
    agent(6),
  ];
  const result = plan(items, { all: false, turns: ["0/2"], open: [] });
  assert.deepEqual(result.ranges.map(range => range.keys), [["0/3"], ["0/5"]]);
  assert.deepEqual(result.turns[0].bubbleKeys, ["0/2", "0/3", "0/5", "0/6"]);
  assert.deepEqual(shownText(items, result), [
    "user:u1", "agent:a2", "open:1:0/3", "agent:a4", "open:1:0/5", "agent:a6",
  ]);
});

test("a decision inside a range is hidden with it", () => {
  const items = [
    user(1), agent(2), agent(3), decision(4), agent(5), agent(6),
  ];
  const result = plan(items, { all: false, turns: ["0/2"], open: [] });
  assert.equal(result.rows[0].count, 2);
  assert.equal(result.shown[3], false);
  assert.deepEqual(shownText(items, result), [
    "user:u1", "agent:a2", "open:2:0/3", "agent:a6",
  ]);
});

test("a decision at the edge of a range stays", () => {
  const items = [
    user(1), agent(2), decision(3), agent(4), agent(5), decision(6), agent(7),
  ];
  const result = plan(items, { all: false, turns: ["0/2"], open: [] });
  assert.deepEqual(shownText(items, result), [
    "user:u1", "agent:a2", "decision:3", "open:2:0/4", "decision:6", "agent:a7",
  ]);
});

test("an empty fold state hides nothing and adds no row", () => {
  const items = promptWithAgents(4);
  const result = plan(items, empty());
  assert.deepEqual(result.rows, []);
  assert.equal(result.canFoldAny, true);
  assert.ok(result.shown.every(Boolean));
  assert.deepEqual(result.sequence.map(step => step.index), [0, 1, 2, 3, 4]);
});

test("folding one turn hides its middle and leaves the other turn", () => {
  const items = [
    user(1), agent(2), agent(3), agent(4), agent(5),
    user(6), agent(7), agent(8), agent(9),
  ];
  const result = plan(items, { all: false, turns: ["0/2"], open: [] });
  assert.deepEqual(shownText(items, result), [
    "user:u1", "agent:a2", "open:2:0/3", "agent:a5",
    "user:u6", "agent:a7", "agent:a8", "agent:a9",
  ]);
});

test("all folds every turn, including one with no stored key", () => {
  const items = [
    user(1), agent(2), agent(3), agent(4),
    user(5), agent(6), agent(7), agent(8),
  ];
  const result = plan(items, { all: true, turns: [], open: [] });
  assert.equal(result.canFoldAny, false);
  assert.deepEqual(shownText(items, result), [
    "user:u1", "agent:a2", "open:1:0/3", "agent:a4",
    "user:u5", "agent:a6", "open:1:0/7", "agent:a8",
  ]);
});

test("opening one range of two shows it with a Fold row and leaves the other hidden", () => {
  const items = [
    user(1), agent(2), agent(3), agent(4), agent(5), agent(6), agent(7), agent(8),
  ];
  const state = { all: false, turns: ["0/2"], open: ["0/3", "0/4"] };
  const result = plan(items, state, new Set([5]));
  assert.equal(result.turns[0].canFold, true);
  assert.deepEqual(shownText(items, result), [
    "user:u1", "agent:a2",
    "close:2:0/3", "agent:a3", "agent:a4",
    "agent:a5",
    "open:2:0/6",
    "agent:a8",
  ]);
  const top = result.rows[0];
  assert.equal(top.side, "before");
  assert.equal(top.kind, "close");
  assert.deepEqual(top.keys, ["0/3", "0/4"]);
});

test("closing that range hides it again", () => {
  const items = [
    user(1), agent(2), agent(3), agent(4), agent(5), agent(6), agent(7), agent(8),
  ];
  const opened = { all: false, turns: ["0/2"], open: ["0/3", "0/4"] };
  const closed = closeRange(opened, ["0/3", "0/4"]);
  const result = plan(items, closed, new Set([5]));
  assert.deepEqual(closed.open, []);
  assert.deepEqual(shownText(items, result), [
    "user:u1", "agent:a2", "open:2:0/3", "agent:a5", "open:2:0/6", "agent:a8",
  ]);
});

test("a later bubble in a folded turn stays visible and the previous latest hides", () => {
  const before = promptWithAgents(3);
  const early = plan(before, { all: false, turns: ["0/2"], open: [] });
  assert.deepEqual(shownText(before, early), ["user:u1", "agent:a2", "open:1:0/3", "agent:a4"]);
  const later = promptWithAgents(4);
  const next = plan(later, { all: false, turns: ["0/2"], open: [] });
  assert.deepEqual(shownText(later, next), [
    "user:u1", "agent:a2", "open:2:0/3", "agent:a5",
  ]);
  assert.equal(next.rows[0].keys.includes("0/4"), true);
});

test("a partly open range shows a Show row and a Fold row without reordering", () => {
  const items = promptWithAgents(6);
  const result = plan(items, { all: false, turns: ["0/2"], open: ["0/5", "0/6"] });
  assert.deepEqual(shownText(items, result), [
    "user:u1", "agent:a2",
    "open:2:0/3",
    "close:2:0/5", "agent:a5", "agent:a6",
    "agent:a7",
  ]);
  assert.deepEqual(result.rows.map(row => row.side), ["replace", "before"]);
});

test("a decision between a hidden run and a visible run stays", () => {
  const items = [
    user(1), agent(2), agent(3), decision(4), agent(5), agent(6),
  ];
  const hiddenThenShown = plan(items, { all: false, turns: ["0/2"], open: ["0/5"] });
  assert.deepEqual(shownText(items, hiddenThenShown), [
    "user:u1", "agent:a2", "open:1:0/3", "decision:4", "close:1:0/5", "agent:a5", "agent:a6",
  ]);
  const shownThenHidden = plan(items, { all: false, turns: ["0/2"], open: ["0/3"] });
  assert.deepEqual(shownText(items, shownThenHidden), [
    "user:u1", "agent:a2", "close:1:0/3", "agent:a3", "decision:4", "open:1:0/5", "agent:a6",
  ]);
});

test("a bubble that becomes protected is shown and splits its range", () => {
  const items = promptWithAgents(5);
  const hidden = plan(items, { all: false, turns: ["0/2"], open: [] });
  assert.deepEqual(hidden.ranges.map(range => range.keys), [["0/3", "0/4", "0/5"]]);
  const marked = plan(items, { all: false, turns: ["0/2"], open: [] }, new Set([4]));
  assert.deepEqual(marked.ranges.map(range => range.keys), [["0/3"], ["0/5"]]);
  assert.deepEqual(shownText(items, marked), [
    "user:u1", "agent:a2", "open:1:0/3", "agent:a4", "open:1:0/5", "agent:a6",
  ]);
});

test("eight visible bubbles repeat the Fold control below the range", () => {
  const items = promptWithAgents(10);
  const keys = [];
  for (let record = 3; record <= 10; record++) keys.push(keyOf(record));
  const result = plan(items, { all: false, turns: ["0/2"], open: keys });
  assert.equal(result.rows.length, 2);
  assert.equal(result.rows[0].side, "before");
  assert.equal(result.rows[1].side, "after");
  assert.equal(result.rows[0].count, 8);
  assert.equal(result.rows[1].count, 8);
  assert.equal(result.rows[0].kind, "close");
  assert.equal(result.rows[1].key, result.rows[0].key);
  const labels = shownText(items, result);
  assert.equal(labels[2], "close:8:0/3");
  assert.equal(labels[labels.length - 2], "close:8:0/3");
  assert.equal(labels[labels.length - 1], "agent:a11");
});

test("seven visible bubbles have the Fold control only above the range", () => {
  const items = promptWithAgents(9);
  const keys = [];
  for (let record = 3; record <= 9; record++) keys.push(keyOf(record));
  const result = plan(items, { all: false, turns: ["0/2"], open: keys });
  assert.equal(result.rows.length, 1);
  assert.equal(result.rows[0].side, "before");
  assert.equal(result.rows[0].count, 7);
});

test("foldTurn removes that turn's keys from open and only those", () => {
  const items = [
    user(1), agent(2), agent(3), agent(4),
    user(5), agent(6), agent(7), agent(8),
  ];
  const view = plan(items);
  const state = { all: false, turns: ["0/9"], open: ["0/2", "0/3", "0/4", "0/7"] };
  const next = foldTurn(state, view, "0/2");
  assert.deepEqual(state, { all: false, turns: ["0/9"], open: ["0/2", "0/3", "0/4", "0/7"] });
  assert.deepEqual(next.turns, ["0/9", "0/2"]);
  assert.deepEqual(next.open, ["0/7"]);
  assert.equal(next.all, false);
});

test("foldTurn moves a key it already holds to the newest end", () => {
  const state = { all: false, turns: ["0/2", "0/9"], open: ["0/3"] };
  const planView = {
    turns: [{ firstKey: "0/2", bubbleKeys: ["0/2", "0/3"] }],
  };
  const next = foldTurn(state, planView, "0/2");
  assert.deepEqual(next.turns, ["0/9", "0/2"]);
  assert.deepEqual(next.open, []);
  assert.notEqual(next.turns, state.turns);
});

test("foldTurn with no plan still records the turn", () => {
  const state = empty();
  const next = foldTurn(state, null, "0/2");
  assert.deepEqual(next, { all: false, turns: ["0/2"], open: [] });
  const missing = foldTurn(empty(), { turns: [{ firstKey: "0/9", bubbleKeys: ["0/9"] }] }, "0/2");
  assert.deepEqual(missing.open, []);
  assert.deepEqual(missing.turns, ["0/2"]);
  const shapeless = foldTurn({ all: false, turns: [], open: ["0/8"] }, {}, "0/2");
  assert.deepEqual(shapeless, { all: false, turns: ["0/2"], open: ["0/8"] });
  const blankKey = foldTurn(
    { all: false, turns: [], open: ["0/3", "0/8"] },
    { turns: [null, { firstKey: "0/2", bubbleKeys: ["", "0/3"] }] },
    "0/2",
  );
  assert.deepEqual(blankKey.open, ["0/8"]);
  assert.deepEqual(blankKey.turns, ["0/2"]);
});

test("foldTurn ignores an empty key and a bubbleKeys value that is not a list", () => {
  const state = { all: true, turns: ["0/2"], open: ["0/3"] };
  const copy = foldTurn(state, null, "");
  assert.deepEqual(copy, state);
  assert.notEqual(copy.turns, state.turns);
  assert.notEqual(copy.open, state.open);
  const odd = foldTurn(state, { turns: [{ firstKey: "0/2", bubbleKeys: "0/3" }] }, "0/2");
  assert.deepEqual(odd.open, ["0/3"]);
  assert.deepEqual(odd.turns, ["0/2"]);
});

test("foldAll empties open and turns", () => {
  const state = { all: false, turns: ["0/2"], open: ["0/3", "0/4"] };
  const next = foldAll(state);
  assert.deepEqual(next, { all: true, turns: [], open: [] });
  assert.deepEqual(state.open, ["0/3", "0/4"]);
  next.turns.push("0/1");
  next.open.push("0/1");
  const again = foldAll(state);
  assert.deepEqual(again.turns, []);
  assert.deepEqual(again.open, []);
  assert.deepEqual(foldAll(null), { all: true, turns: [], open: [] });
});

test("openRange adds the keys and closeRange takes them back", () => {
  const state = { all: false, turns: ["0/2"], open: [] };
  const opened = openRange(state, ["0/3", "0/4"]);
  assert.deepEqual(state.open, []);
  assert.deepEqual(opened, { all: false, turns: ["0/2"], open: ["0/3", "0/4"] });
  const closed = closeRange(opened, ["0/3", "0/4"]);
  assert.deepEqual(opened.open, ["0/3", "0/4"]);
  assert.deepEqual(closed.open, []);
  assert.deepEqual(closed.turns, ["0/2"]);
});

test("openRange moves a key it already holds to the newest end", () => {
  const state = { all: false, turns: [], open: ["0/1", "0/2", "0/3"] };
  const next = openRange(state, ["0/1"]);
  assert.deepEqual(next.open, ["0/2", "0/3", "0/1"]);
});

test("openRange at the cap drops the oldest key", () => {
  const open = [];
  for (let i = 1; i <= 2000; i++) open.push("0/" + i);
  const state = { all: false, turns: [], open };
  const next = openRange(state, ["0/2001"]);
  assert.equal(next.open.length, 2000);
  assert.equal(next.open[0], "0/2");
  assert.equal(next.open[1999], "0/2001");
  assert.equal(state.open.length, 2000);
  assert.equal(state.open[0], "0/1");
});

test("closeRange of a missing key returns the same keys in a new list", () => {
  const state = { all: false, turns: ["0/2"], open: ["0/3", "0/4"] };
  const next = closeRange(state, ["0/9"]);
  assert.deepEqual(next.open, ["0/3", "0/4"]);
  assert.notEqual(next.open, state.open);
  const absent = closeRange(state, null);
  assert.deepEqual(absent.open, ["0/3", "0/4"]);
  const mixed = closeRange(state, ["", "0/3"]);
  assert.deepEqual(mixed.open, ["0/4"]);
  const junk = openRange(empty(), ["0/3", "", 2, null, "0/4"]);
  assert.deepEqual(junk.open, ["0/3", "0/4"]);
  assert.deepEqual(openRange(empty(), null).open, []);
});

test("state functions do not change their argument", () => {
  const state = { all: false, turns: ["0/2"], open: ["0/3"] };
  const snapshot = structuredClone(state);
  const opened = openRange(state, ["0/4"]);
  const closed = closeRange(state, ["0/3"]);
  const folded = foldTurn(state, { turns: [{ firstKey: "0/2", bubbleKeys: ["0/2", "0/3"] }] }, "0/2");
  const all = foldAll(state);
  const view = foldPlan([user(1), agent(2), agent(3), agent(4)], { state, isProtected: () => false });
  assert.deepEqual(state, snapshot);
  assert.deepEqual(opened.open, ["0/3", "0/4"]);
  assert.deepEqual(closed.open, []);
  assert.deepEqual(folded.open, []);
  assert.deepEqual(all, { all: true, turns: [], open: [] });
  assert.equal(view.rows.length, 1);
  assert.equal(view.rows[0].kind, "close");
  assert.equal(view.canFoldAny, true);
});

test("a fold record round-trips through storage", () => {
  const store = memoryStore();
  const state = { all: false, turns: ["0/12"], open: ["0/14", "0/15"] };
  const written = writeFoldState(store, "log-1", state);
  assert.deepEqual(written, state);
  assert.equal(
    store.map.get("scimux:fold:v1:log-1"),
    '{"all":false,"turns":["0/12"],"open":["0/14","0/15"]}',
  );
  assert.deepEqual(readFoldState(store, "log-1"), state);
  assert.deepEqual(state, { all: false, turns: ["0/12"], open: ["0/14", "0/15"] });
});

test("text that does not parse counts as empty and the next write replaces it", () => {
  const store = memoryStore({ "scimux:fold:v1:log-1": "{not json" });
  assert.deepEqual(readFoldState(store, "log-1"), empty());
  writeFoldState(store, "log-1", { all: true, turns: [], open: [] });
  assert.deepEqual(readFoldState(store, "log-1"), { all: true, turns: [], open: [] });
});

test("a wrong shape of all counts as empty", () => {
  const store = memoryStore({
    "scimux:fold:v1:log-1": '{"all":"yes","turns":[],"open":[]}',
  });
  assert.deepEqual(readFoldState(store, "log-1"), empty());
  const missing = memoryStore({
    "scimux:fold:v1:log-1": '{"turns":[],"open":[]}',
  });
  assert.deepEqual(readFoldState(missing, "log-1"), empty());
});

test("a wrong shape of turns counts as empty", () => {
  const asText = memoryStore({
    "scimux:fold:v1:log-1": '{"all":false,"turns":"0/1","open":[]}',
  });
  assert.deepEqual(readFoldState(asText, "log-1"), empty());
  const hole = memoryStore({
    "scimux:fold:v1:log-1": '{"all":false,"turns":["0/1",""],"open":[]}',
  });
  assert.deepEqual(readFoldState(hole, "log-1"), empty());
  const number = memoryStore({
    "scimux:fold:v1:log-1": '{"all":false,"turns":[1],"open":[]}',
  });
  assert.deepEqual(readFoldState(number, "log-1"), empty());
});

test("a wrong shape of open counts as empty", () => {
  const asObject = memoryStore({
    "scimux:fold:v1:log-1": '{"all":false,"turns":[],"open":{}}',
  });
  assert.deepEqual(readFoldState(asObject, "log-1"), empty());
  const hole = memoryStore({
    "scimux:fold:v1:log-1": '{"all":false,"turns":[],"open":["0/1",null]}',
  });
  assert.deepEqual(readFoldState(hole, "log-1"), empty());
});

test("a record that is not an object counts as empty", () => {
  for (const text of ["null", "[]", "true", '"x"']){
    const store = memoryStore({ "scimux:fold:v1:log-1": text });
    assert.deepEqual(readFoldState(store, "log-1"), empty(), text);
  }
});

test("extra fields are ignored and duplicate keys keep the newest", () => {
  const store = memoryStore({
    "scimux:fold:v1:log-1": '{"all":false,"turns":["0/1","0/2","0/1"],"open":["0/3","0/3"],"extra":1}',
  });
  assert.deepEqual(readFoldState(store, "log-1"), {
    all: false,
    turns: ["0/2", "0/1"],
    open: ["0/3"],
  });
});

test("the 2000 limit drops the oldest keys", () => {
  const turns = [];
  for (let i = 0; i <= 2000; i++) turns.push("0/" + i);
  const raw = JSON.stringify({ all: false, turns, open: [] });
  const store = memoryStore({ "scimux:fold:v1:log-1": raw });
  const got = readFoldState(store, "log-1");
  assert.equal(got.turns.length, 2000);
  assert.equal(got.turns[0], "0/1");
  assert.equal(got.turns[1999], "0/2000");
  const written = writeFoldState(store, "log-1", { all: false, turns, open: [] });
  assert.equal(written.turns.length, 2000);
  assert.equal(written.turns[0], "0/1");
});

test("a storage that throws on read counts as empty", () => {
  const store = {
    getItem(){ throw new Error("SecurityError"); },
    setItem(){ throw new Error("SecurityError"); },
  };
  assert.deepEqual(readFoldState(store, "log-1"), empty());
  assert.deepEqual(readFoldState({}, "log-1"), empty());
  assert.deepEqual(readFoldState(null, "log-1"), empty());
});

test("a storage that throws on write still returns the state", () => {
  let writes = 0;
  const store = {
    getItem(){ return null; },
    setItem(){ writes += 1; throw new Error("QuotaExceededError"); },
  };
  const next = writeFoldState(store, "log-1", { all: true, turns: ["0/2"], open: [] });
  assert.equal(writes, 1);
  assert.deepEqual(next, { all: true, turns: ["0/2"], open: [] });
  const noSetter = writeFoldState({}, "log-1", { all: false, turns: [], open: ["0/3"] });
  assert.deepEqual(noSetter, { all: false, turns: [], open: ["0/3"] });
});

test("a value that is not text counts as empty", () => {
  const store = { getItem(){ return 4; }, setItem(){} };
  assert.deepEqual(readFoldState(store, "log-1"), empty());
  const blank = memoryStore({ "scimux:fold:v1:log-1": "" });
  assert.deepEqual(readFoldState(blank, "log-1"), empty());
  const missing = memoryStore();
  assert.deepEqual(readFoldState(missing, "log-1"), empty());
});

test("no uid touches no storage", () => {
  let reads = 0;
  let writes = 0;
  const store = {
    getItem(){ reads += 1; throw new Error("no"); },
    setItem(){ writes += 1; throw new Error("no"); },
  };
  assert.deepEqual(readFoldState(store, ""), empty());
  assert.deepEqual(readFoldState(store, undefined), empty());
  const written = writeFoldState(store, "", { all: true, turns: ["0/2"], open: ["0/3"] });
  assert.deepEqual(written, { all: true, turns: ["0/2"], open: ["0/3"] });
  assert.deepEqual(writeFoldState(store, undefined, null), empty());
  assert.equal(reads, 0);
  assert.equal(writes, 0);
});

test("a wrong state handed to a transition counts as empty first", () => {
  assert.deepEqual(openRange(null, ["0/3"]), { all: false, turns: [], open: ["0/3"] });
  assert.deepEqual(openRange({ all: "no", turns: [], open: [] }, ["0/3"]).open, ["0/3"]);
  assert.deepEqual(closeRange("nope", ["0/3"]).open, []);
  assert.deepEqual(foldTurn(null, null, "0/2").turns, ["0/2"]);
  assert.equal(foldStateVersion(null), foldStateVersion(empty()));
  assert.equal(foldStateVersion({ all: "no" }), foldStateVersion(empty()));
});

test("foldRowHTML renders both kinds, singular and plural", () => {
  assert.equal(
    foldRowHTML({ kind: "open", key: "0/14", count: 6 }),
    '<div class="foldrow"><button type="button" class="foldtoggle" data-fold="open" data-foldkey="0/14" aria-expanded="false">Show 6 intermediate bubbles</button></div>',
  );
  assert.equal(
    foldRowHTML({ kind: "open", key: "0/3", count: 1 }),
    '<div class="foldrow"><button type="button" class="foldtoggle" data-fold="open" data-foldkey="0/3" aria-expanded="false">Show 1 intermediate bubble</button></div>',
  );
  assert.equal(
    foldRowHTML({ kind: "close", key: "0/6", count: 2 }),
    '<div class="foldrow"><button type="button" class="foldtoggle" data-fold="close" data-foldkey="0/6" aria-expanded="true">Fold 2 intermediate bubbles</button></div>',
  );
  assert.equal(
    foldRowHTML({ kind: "close", key: "0/6", count: 1 }),
    '<div class="foldrow"><button type="button" class="foldtoggle" data-fold="close" data-foldkey="0/6" aria-expanded="true">Fold 1 intermediate bubble</button></div>',
  );
});

test("foldRowHTML escapes a key", () => {
  const html = foldRowHTML({ kind: "open", key: 'a&b<c>d"e', count: 2 });
  assert.equal(
    html,
    '<div class="foldrow"><button type="button" class="foldtoggle" data-fold="open" data-foldkey="a&amp;b&lt;c&gt;d&quot;e" aria-expanded="false">Show 2 intermediate bubbles</button></div>',
  );
  assert.equal(html.includes('a&b<c>d"e'), false);
});

test("foldStateVersion differs for every pair of states that render differently", () => {
  const items = [
    user(1), agent(2), agent(3), agent(4), agent(5), agent(6), agent(7), agent(8),
  ];
  const states = [
    empty(),
    { all: false, turns: ["0/2"], open: [] },
    { all: true, turns: [], open: [] },
    { all: false, turns: ["0/2"], open: ["0/3", "0/4"] },
    { all: false, turns: ["0/2"], open: ["0/3"] },
    { all: false, turns: ["0/2"], open: ["0/6", "0/7"] },
    { all: true, turns: [], open: ["0/3", "0/4"] },
  ];
  function renderSig(state){
    const result = plan(items, state, new Set([5]));
    return JSON.stringify({
      shown: result.shown,
      rows: result.rows.map(row => [row.side, row.kind, row.key, row.count, row.keys]),
    });
  }
  for (let i = 0; i < states.length; i++){
    for (let j = i + 1; j < states.length; j++){
      const left = renderSig(states[i]);
      const right = renderSig(states[j]);
      if (left !== right){
        assert.notEqual(
          foldStateVersion(states[i]),
          foldStateVersion(states[j]),
          "states " + i + " and " + j,
        );
      }
    }
  }
  assert.equal(foldStateVersion(empty()), foldStateVersion({ all: false, turns: [], open: [] }));
  const state = { all: false, turns: ["0/2"], open: ["0/3"] };
  const snapshot = structuredClone(state);
  foldStateVersion(state);
  assert.deepEqual(state, snapshot);
});

test("items that are not a timeline stay shown and are not folded", () => {
  assert.deepEqual(foldPlan(null, { state: { all: true, turns: [], open: [] } }), {
    shown: [],
    rows: [],
    turns: [],
    ranges: [],
    canFoldAny: false,
    sequence: [],
  });
  const items = promptWithAgents(3);
  const result = foldPlan(items, null);
  assert.deepEqual(result.rows, []);
  assert.equal(result.canFoldAny, false);
  assert.ok(result.shown.every(Boolean));
  assert.equal(result.sequence.length, items.length);
});

test("a turn whose first bubble has no key folds only when the whole chat is folded", () => {
  const items = [
    user(1, { uid: "" }),
    agent(2, { uid: "" }),
    agent(3),
    agent(4),
    agent(5),
  ];
  const byTurn = plan(items, { all: false, turns: ["0/3"], open: [] });
  assert.equal(byTurn.rows.length, 0);
  assert.equal(byTurn.turns[0].firstKey, "");
  const byAll = plan(items, { all: true, turns: [], open: [] });
  assert.equal(byAll.rows.length, 1);
  assert.equal(byAll.rows[0].count, 2);
  assert.deepEqual(shownText(items, byAll), ["user:u1", "agent:a2", "open:2:0/3", "agent:a5"]);
});

test("a leading decision stays, and an item that is not a bubble stays", () => {
  const items = [
    decision(1),
    agent(2),
    agent(3),
    null,
    { kind: "marker" },
    agent(4),
    agent(5),
  ];
  const result = plan(items, { all: false, turns: ["0/2"], open: [] });
  assert.equal(result.shown[0], true);
  assert.equal(result.shown[3], true);
  assert.equal(result.shown[4], true);
  assert.equal(result.shown[2], false);
  assert.equal(result.shown[5], false);
  assert.deepEqual(shownText(items, result), [
    "decision:1", "agent:a2", "open:2:0/3", "empty", "marker", "agent:a5",
  ]);
});

test("a decision-only span and a turn with no turn object stay shown", () => {
  const items = [decision(1), { kind: "turn" }, user(2)];
  const result = plan(items);
  assert.deepEqual(result.turns, []);
  assert.deepEqual(result.rows, []);
  assert.ok(result.shown.every(Boolean));
});

test("folding does not change the timeline it was given", () => {
  const items = [
    user(1), agent(2), agent(3), decision(4), agent(5), seam(), agent(6),
  ];
  const snapshot = structuredClone(items);
  const result = plan(items, { all: true, turns: [], open: [] }, new Set([3]));
  assert.deepEqual(items, snapshot);
  assert.deepEqual(result.rows, []);
  assert.equal(result.canFoldAny, false);
});

test("a roleless bubble counts as an agent bubble", () => {
  const items = [
    user(1),
    { kind: "turn", record: 2, turn: { uid: "log-1", segment: 0, record: 2, text: "a2" } },
    agent(3),
    agent(4),
  ];
  const result = plan(items, { all: false, turns: ["0/2"], open: [] });
  assert.equal(result.turns[0].firstKey, "0/2");
  assert.deepEqual(shownText(items, result), ["user:u1", "agent:a2", "open:1:0/3", "agent:a4"]);
});

test("duplicate keys added together are kept once, at the newest end", () => {
  const next = openRange(empty(), ["0/3", "0/3", "0/4"]);
  assert.deepEqual(next.open, ["0/3", "0/4"]);
  const again = openRange({ all: false, turns: [], open: ["0/4"] }, ["0/3", "0/3"]);
  assert.deepEqual(again.open, ["0/4", "0/3"]);
});

test("a stored all of true is kept", () => {
  const store = memoryStore({
    "scimux:fold:v1:log-1": '{"all":true,"turns":["0/2"],"open":[]}',
  });
  assert.deepEqual(readFoldState(store, "log-1"), {
    all: true,
    turns: ["0/2"],
    open: [],
  });
});
