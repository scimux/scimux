/* fold.js — which intermediate agent bubbles a chat hides.
 *
 * Pure. No document, window, storage global, or timer. The chat passes the
 * merged timeline of one surface and the session-storage record for that
 * log, and gets back what is shown, the disclosure rows, and the turn
 * handles the action bar and the chat head need.
 *
 * A bubble key is "<segment>/<record>" of a turn that has a uid. Missing
 * segment and record read as 0. A foldable bubble is a middle agent bubble
 * (not the first, not the last) with a key that the chat does not mark as
 * bookmarked or sent onward. A range is a longest run of those. Decisions
 * strictly inside a hidden run hide with it; a decision is never the first
 * or last item of a range.
 */

const FOLD_PREFIX = "scimux:fold:v1:";
const KEY_CAP = 2000;

function emptyState(){
  return { all: false, turns: [], open: [] };
}

function isKey(value){
  return typeof value === "string" && value.length > 0;
}

/* Newest copy wins, then the oldest keys past the cap fall off the front. */
function dedupeNewest(list){
  const seen = new Set();
  const newestFirst = [];
  for (let i = list.length - 1; i >= 0; i--){
    const key = list[i];
    if (seen.has(key)) continue;
    seen.add(key);
    newestFirst.push(key);
  }
  newestFirst.reverse();
  if (newestFirst.length > KEY_CAP) return newestFirst.slice(newestFirst.length - KEY_CAP);
  return newestFirst;
}

/* Null means the record is the wrong shape and counts as empty. */
function normalizeState(raw){
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return null;
  if (typeof raw.all !== "boolean") return null;
  if (!Array.isArray(raw.turns) || !Array.isArray(raw.open)) return null;
  for (let i = 0; i < raw.turns.length; i++){
    if (!isKey(raw.turns[i])) return null;
  }
  for (let i = 0; i < raw.open.length; i++){
    if (!isKey(raw.open[i])) return null;
  }
  return {
    all: raw.all,
    turns: dedupeNewest(raw.turns),
    open: dedupeNewest(raw.open),
  };
}

function coerceState(state){
  return normalizeState(state) || emptyState();
}

function escAttr(value){
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

export function foldKey(turn){
  if (!turn || !turn.uid) return "";
  const segment = Number(turn.segment) || 0;
  const record = Number(turn.record) || 0;
  return segment + "/" + record;
}

export function readFoldState(storage, uid){
  if (!uid) return emptyState();
  let text;
  try {
    text = storage.getItem(FOLD_PREFIX + uid);
  } catch {
    return emptyState();
  }
  if (typeof text !== "string" || text === "") return emptyState();
  let parsed;
  try {
    parsed = JSON.parse(text);
  } catch {
    return emptyState();
  }
  return normalizeState(parsed) || emptyState();
}

export function writeFoldState(storage, uid, state){
  const next = coerceState(state);
  if (!uid) return next;
  try {
    storage.setItem(FOLD_PREFIX + uid, JSON.stringify(next));
  } catch {
    /* The open page still uses the state handed back. */
  }
  return next;
}

/* Move added keys to the end, drop keys that are not strings, then cap. */
function appendKeys(list, keys){
  const adding = [];
  const drop = new Set();
  const source = Array.isArray(keys) ? keys : [];
  for (let i = 0; i < source.length; i++){
    const key = source[i];
    if (!isKey(key) || drop.has(key)) continue;
    drop.add(key);
    adding.push(key);
  }
  const kept = [];
  for (let i = 0; i < list.length; i++){
    if (!drop.has(list[i])) kept.push(list[i]);
  }
  return dedupeNewest(kept.concat(adding));
}

export function openRange(state, keys){
  const cur = coerceState(state);
  return {
    all: cur.all,
    turns: cur.turns.slice(),
    open: appendKeys(cur.open, keys),
  };
}

export function closeRange(state, keys){
  const cur = coerceState(state);
  const drop = new Set();
  const source = Array.isArray(keys) ? keys : [];
  for (let i = 0; i < source.length; i++){
    if (isKey(source[i])) drop.add(source[i]);
  }
  return {
    all: cur.all,
    turns: cur.turns.slice(),
    open: cur.open.filter(key => !drop.has(key)),
  };
}

export function foldAll(state){
  /* The argument is part of the call shape. The result does not depend on
     it, and this function does not read or change the caller's arrays. */
  void state;
  return { all: true, turns: [], open: [] };
}

export function foldTurn(state, plan, firstKey){
  const cur = coerceState(state);
  if (!isKey(firstKey)){
    return { all: cur.all, turns: cur.turns.slice(), open: cur.open.slice() };
  }
  let bubbleKeys = [];
  const turns = plan && Array.isArray(plan.turns) ? plan.turns : [];
  for (let i = 0; i < turns.length; i++){
    const entry = turns[i];
    if (!entry || entry.firstKey !== firstKey) continue;
    if (Array.isArray(entry.bubbleKeys)) bubbleKeys = entry.bubbleKeys;
    break;
  }
  const drop = new Set();
  for (let i = 0; i < bubbleKeys.length; i++){
    if (isKey(bubbleKeys[i])) drop.add(bubbleKeys[i]);
  }
  return {
    all: cur.all,
    turns: appendKeys(cur.turns, [firstKey]),
    open: cur.open.filter(key => !drop.has(key)),
  };
}

function itemRole(item){
  if (!item || item.kind !== "turn" || !item.turn) return "";
  return item.turn.role === "user" ? "user" : "agent";
}

function blankPlan(count){
  const shown = [];
  const sequence = [];
  for (let i = 0; i < count; i++){
    shown.push(true);
    sequence.push({ index: i });
  }
  return {
    shown,
    rows: [],
    turns: [],
    ranges: [],
    canFoldAny: false,
    sequence,
  };
}

function notProtected(){
  return false;
}

export function foldPlan(items, options){
  if (!Array.isArray(items)) return blankPlan(0);
  if (!options) return blankPlan(items.length);
  const state = coerceState(options.state);
  const isProtected = typeof options.isProtected === "function"
    ? options.isProtected
    : notProtected;
  const openSet = new Set(state.open);
  const turnSet = new Set(state.turns);
  const shown = items.map(() => true);
  const rows = [];
  const turns = [];
  const ranges = [];
  const before = new Map();
  const after = new Map();
  const replace = new Map();

  const groups = [];
  let current = null;
  for (let i = 0; i < items.length; i++){
    const item = items[i];
    if (item && item.kind === "seam"){
      current = null;
      continue;
    }
    const role = itemRole(item);
    if (role === "user"){
      current = { entries: [] };
      groups.push(current);
      current.entries.push({ index: i, item });
      continue;
    }
    if (!current){
      current = { entries: [] };
      groups.push(current);
    }
    current.entries.push({ index: i, item });
  }

  for (let g = 0; g < groups.length; g++){
    const agents = [];
    const entries = groups[g].entries;
    for (let i = 0; i < entries.length; i++){
      if (itemRole(entries[i].item) === "agent") agents.push(entries[i]);
    }
    if (agents.length === 0) continue;
    const first = agents[0];
    const leading = g === 0 && itemRole(entries[0]?.item) === "agent" &&
      isKey(options.leadFirstKey);
    const firstKey = leading ? options.leadFirstKey : foldKey(first.item.turn);
    const bubbleKeys = [];
    for (let i = 0; i < agents.length; i++){
      const key = foldKey(agents[i].item.turn);
      if (key) bubbleKeys.push(key);
    }
    const turnFolded = !!state.all || (firstKey !== "" && turnSet.has(firstKey));
    const middles = [];
    for (let i = leading ? 0 : 1; i < agents.length - 1; i++){
      const entry = agents[i];
      const key = foldKey(entry.item.turn);
      const protectedBubble = !!isProtected(entry.item.turn);
      middles.push({
        index: entry.index,
        key,
        foldable: key !== "" && !protectedBubble,
      });
    }

    const structural = [];
    let run = [];
    for (let i = 0; i < middles.length; i++){
      if (middles[i].foldable){
        run.push(middles[i]);
      } else if (run.length){
        structural.push(run);
        run = [];
      }
    }
    if (run.length) structural.push(run);

    let visibleFoldable = 0;
    for (let r = 0; r < structural.length; r++){
      const range = structural[r];
      const rangeKeys = range.map(middle => middle.key);
      ranges.push({
        key: rangeKeys[0],
        keys: rangeKeys.slice(),
        indexes: range.map(middle => middle.index),
      });
      const runs = [];
      let visibleRun = null;
      for (let i = 0; i < range.length; i++){
        const hidden = turnFolded && !openSet.has(range[i].key);
        if (!hidden) visibleFoldable += 1;
        if (!visibleRun || visibleRun.hidden !== hidden){
          visibleRun = { hidden, members: [] };
          runs.push(visibleRun);
        }
        visibleRun.members.push(range[i]);
      }
      for (let i = 0; i < runs.length; i++){
        const members = runs[i].members;
        const runKeys = members.map(middle => middle.key);
        const row = {
          index: members[0].index,
          key: runKeys[0],
          count: runKeys.length,
          keys: runKeys.slice(),
        };
        if (runs[i].hidden){
          const start = members[0].index;
          const end = members[members.length - 1].index;
          const memberAt = new Set(members.map(middle => middle.index));
          for (let at = start; at <= end; at++){
            if (memberAt.has(at)){
              shown[at] = false;
              continue;
            }
            const between = items[at];
            if (between && between.kind === "decision") shown[at] = false;
          }
          row.side = "replace";
          row.kind = "open";
          rows.push(row);
          replace.set(start, row);
        } else if (turnFolded){
          row.side = "before";
          row.kind = "close";
          rows.push(row);
          before.set(members[0].index, row);
          if (runKeys.length >= 8){
            const below = {
              index: members[members.length - 1].index,
              side: "after",
              kind: "close",
              key: runKeys[0],
              count: runKeys.length,
              keys: runKeys.slice(),
            };
            rows.push(below);
            after.set(below.index, below);
          }
        }
      }
    }

    const canFold = visibleFoldable > 0;
    turns.push({
      firstKey,
      firstIndex: first.index,
      canFold,
      bubbleKeys,
    });
  }

  const sequence = [];
  for (let i = 0; i < items.length; i++){
    const replaced = replace.get(i);
    if (replaced){
      sequence.push({ row: replaced });
      continue;
    }
    const lead = before.get(i);
    if (lead) sequence.push({ row: lead });
    if (shown[i]) sequence.push({ index: i });
    const tail = after.get(i);
    if (tail) sequence.push({ row: tail });
  }

  let canFoldAny = false;
  for (let i = 0; i < turns.length; i++){
    if (turns[i].canFold) canFoldAny = true;
  }

  return { shown, rows, turns, ranges, canFoldAny, sequence };
}

export function foldRowHTML(row){
  const close = row.kind === "close";
  const count = row.count;
  const noun = count === 1 ? "bubble" : "bubbles";
  const verb = close ? "Fold" : "Show";
  const action = close ? "close" : "open";
  const expanded = close ? "true" : "false";
  return '<div class="foldrow"><button type="button" class="foldtoggle" data-fold="'
    + action + '" data-foldkey="' + escAttr(row.key) + '" aria-expanded="'
    + expanded + '">' + verb + " " + count + " intermediate " + noun
    + "</button></div>";
}

export function foldStateVersion(state){
  const cur = coerceState(state);
  return JSON.stringify([cur.all ? 1 : 0, cur.turns, cur.open]);
}
