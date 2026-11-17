import test from "node:test";
import assert from "node:assert/strict";
import * as guestModule from "../js/team-guest.js";
import { readFileSync } from "node:fs";
import { jsonResponse } from "./s2-helpers.js";
import { decodeResponse } from "../js/api.js";
import { createGuestFeature, guestGroups, guestKey, guestStateText, guestErrorText, guestEntryHTML, guestMessageHTML, guestHexKey } from "../js/team-guest.js";

const base = { server: "s", server_name: "Server", owner: "o", owner_name: "Owner", chat: "c", generation: "g", title: "Chat", access: "read", state: "live", mirror_last: 1, seen_mirror: 0, comments_last: 0, seen_comments: 0, unread: false, first_seen: "1" };
const list = (subscriptions = [], servers = [], truncated = false) => ({ subscriptions: subscriptions.map(view => ({ ...view })), servers, truncated });

test("shared actions are wired and have touch-sized buttons", () => {
  const app = readFileSync(new URL("../js/app.js", import.meta.url), "utf8");
  const css = readFileSync(new URL("../css/chat.css", import.meta.url), "utf8");
  assert.match(app, /uiMutate:\s*op\s*=>\s*uiMutate\(op\)/);
  assert.match(app, /bookmarks:\s*\(\)\s*=>\s*getUI\(\)\.bookmarks/);
  assert.match(app, /openSendTo:\s*opts\s*=>\s*bookmarksFeature\.openSendTo\(opts\)/);
  assert.match(css, /#msgs[^{}]*\.bubactions\s+button[^{}]*\{[^}]*min-height:\s*44px/);
});
function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

test("guest keys identify the subscription", () => {
  assert.equal(guestKey(base), "s/o/c/g");
});

test("gate off is retried on a bounded cadence and hides the tab", async () => {
  let calls = 0;
  const f = nativeViewFixture(async () => { calls++; throw Object.assign(new Error("not found"), { status: 404 }); });
  f.feature.tick(); await f.feature.settled();
  assert.equal(f.feature.tab.tabHTML(false), "");
  for (let i = 0; i < 10; i++) { f.setTime((i + 1) * 10000); f.feature.tick(); }
  await f.feature.settled(); assert.equal(calls, 2);
});

test("list polling throttles, ignores equal answers and retains the last list on failure", async () => {
  const pending = deferred();
  const answers = [list(), list([base]), list([base]), new Error("offline"), pending.promise];
  let calls = 0;
  const f = nativeViewFixture(async () => { const a = answers[calls++]; if (a instanceof Error) throw a; return a; });
  f.feature.tick(); await f.feature.settled();
  assert.equal(f.invalidations(), 0); assert.equal(f.feature.tab.tabHTML(false), "");
  f.setTime(9999); f.feature.tick(); assert.equal(calls, 1);
  f.setTime(10000); f.feature.tick(); await f.feature.settled(); assert.equal(f.invalidations(), 1);
  assert.match(f.feature.tab.tabHTML(false), /Shared/); assert.doesNotMatch(f.feature.tab.tabHTML(false), /tabbadge/);
  f.setTime(20000); f.feature.tick(); await f.feature.settled(); assert.equal(f.invalidations(), 1);
  f.setTime(30000); f.feature.tick(); await f.feature.settled(); assert.equal(f.invalidations(), 1); assert.match(f.feature.tab.tabHTML(false), /Shared/);
  f.setTime(40000); f.feature.tick(); f.setTime(60000); f.feature.tick(); assert.equal(calls, 5);
  pending.resolve(list([base, { ...base, chat: "two", unread: true }, { ...base, chat: "three", unread: true }])); await f.feature.settled();
  assert.equal(f.invalidations(), 2); assert.match(f.feature.tab.tabHTML(true), /class="on"/); assert.match(f.feature.tab.tabHTML(false), /aria-label="2 unread">2/);
});

test("groups order and label subscriptions", () => {
  const a = { ...base, server: "a", owner: "a", owner_name: "ALPHA", server_name: "A" };
  const b = { ...base, server: "b", owner: "b", owner_name: "Beta", server_name: "B" };
  const groups = guestGroups([b, a], [{ id: "a", delivery: "offline" }, { id: "b", delivery: "changed" }]);
  assert.equal(groups.length, 2);
  assert.match(JSON.stringify(groups[0]), /offline/); assert.match(JSON.stringify(groups[1]), /not the same server/);
  assert.match(JSON.stringify(guestGroups([{ ...base, owner_name: "" }], [{ id: "s", delivery: "refused" }])), /Unknown person.*access ended/);
  assert.doesNotMatch(JSON.stringify(guestGroups([base], [])), /offline|access ended|not the same server/);
});


test("groups sort by owner name, server name, owner id and server id", () => {
  const v = (owner_name, server_name, owner, server) => ({ ...base, owner_name, server_name, owner, server });
  for (const [first, second] of [
    [v("alpha", "Z", "a", "a"), v("Zulu", "A", "b", "b")],
    [v("Alpha", "A", "a", "a"), v("alpha", "Z", "b", "b")],
    [v("Alpha", "A", "a", "a"), v("alpha", "A", "b", "b")],
    [v("Alpha", "A", "a", "a"), v("alpha", "A", "a", "b")],
  ]) {
    assert.deepEqual(guestGroups([second, first], []).map(g => g.key), [first.server + "/" + first.owner, second.server + "/" + second.owner]);
  }
});

test("items sort live first, then case-insensitive title and first seen", () => {
  const rows = [
    { ...base, chat: "ended", state: "ended", title: "A" },
    { ...base, chat: "z", title: "Z" },
    { ...base, chat: "late", title: "a", first_seen: "2" },
    { ...base, chat: "early", title: "A", first_seen: "1" },
  ];
  const groups = guestGroups(rows, []);
  assert.equal(groups.length, 1);
  assert.deepEqual(groups[0].items.map(v => v.chat), ["early", "late", "z", "ended"]);
});


// Exercises the same native roots wired by app.js, with selection routed through
// the panel callback just as an Activities card routes it in the application.
function nativeViewFixture(api, commentsApi = async () => ({ entries: [], names: {}, next: 0, more: false }), randomBytes = n => new Uint8Array(n), extra = {}) {
  let time = 0, invalidations = 0, selected = "";
  const elements = new Map();
  const doc = { activeElement: null, querySelector: selector => elements.get(selector) || null };
  for (const id of ["cardlist", "cardtabs", "chattitle", "chatmission", "gueststate", "msgs", "guestmore", "guestnote", "guestremove"]) {
    const listeners = new Map();
    const el = { id, innerHTML: "", textContent: "", value: "", disabled: false, hidden: false, dataset: {}, scrollTop: 0, scrollHeight: 1000, clientHeight: 200, listeners,
      addEventListener(type, fn) { listeners.set(type, fn); }, removeEventListener(type) { listeners.delete(type); },
      focus() { doc.activeElement = el; }, contains(node) { return node === el; },
      querySelector() { return null; }, querySelectorAll() { return []; }, insertAdjacentHTML(_where, html) { this.innerHTML += html; } };
    elements.set("#" + id, el);
  }
  const body = elements.get("#msgs");
  Object.defineProperty(body, "children", { get() {
    const pieces = this.innerHTML.split(/(?=<div class="(?:turn |previewtrunc|bubactions))/).filter(Boolean);
    return pieces.map((piece, index) => {
      const read = name => piece.match(new RegExp(`data-${name}="([^"]*)"`))?.[1];
      return { dataset: { seq: read("seq"), cseq: read("cseq"), anchor: read("anchor"), gactions: piece.includes("data-gactions") ? "" : undefined },
        insertAdjacentHTML(_where, html) { pieces.splice(index, 0, html); body.innerHTML = pieces.join(""); } };
    });
  } });
  const routed = (path, opts) => path.includes("/comments?") ? commentsApi(path, opts) : api(path, opts);
  let feature;
  const callbacks = extra.panel || {};
  const panel = { promptIcon:"", preparePrompt() {}, ...callbacks,
    loaded: callbacks.loaded || (() => {}), changed: callbacks.changed || (() => {}), ownerChanged: callbacks.ownerChanged || (() => {}),
    removed() { selected = "local"; callbacks.removed?.(); },
    select(id) { selected = id; callbacks.select?.(id); feature.selectActivity(id); } };
  feature = createGuestFeature({ api: routed, doc, now: () => time, invalidateCards: () => invalidations++, randomBytes, selectedNode: () => "", selectedUID: () => "c", localAgentAvailable: () => true, confirmRemove: extra.confirm || (() => true), ...extra, panel });
  const get = id => elements.get("#" + id);
  feature.bind();
  return { feature, doc, get, actionRoot: "msgs", setTime: n => { time = n; }, invalidations: () => invalidations, selected: () => selected,
    open(id = "team/" + guestKey(base)) { feature.tab.open(id); },
    navigate(id = "local") { selected = id; feature.selectActivity(id); },
    click(id, target = get(id)) { get(id).listeners.get("click")({ target }); } };
}

test("native reader fixture selects a received activity and uses only normal roots", async () => {
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer());
  assert.equal(f.doc.querySelector("#sharedview"), null);
  assert.equal(f.doc.querySelector("#sv_text"), null);
  assert.equal("render" in f.feature.tab, false, "Activities owns the shared list renderer");
  for (const id of ["chattitle", "chatmission", "gueststate", "msgs", "guestmore", "guestnote", "guestremove"]) assert.equal(f.doc.querySelector("#" + id), f.get(id));
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  assert.equal(f.selected(), "team/s/o/c/g"); assert.equal(f.feature.tab.selected(), f.selected());
  assert.equal(f.get("chattitle").textContent, "Chat");
  f.navigate(); assert.equal(f.feature.tab.selected(), ""); assert.equal(f.get("msgs").innerHTML, "");
  f.feature.destroy();
});

test("native reader teardown drops pending data and removes native listeners", async () => {
  const held = deferred();
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : held.promise);
  f.feature.tick(); await f.feature.settled(); f.open();
  assert.equal(f.get("msgs").listeners.size, 2);
  assert.equal(f.get("guestmore").listeners.size, 1);
  assert.equal(f.get("guestremove").listeners.size, 1);
  f.feature.destroy();
  assert.equal(f.get("msgs").listeners.size, 0);
  assert.equal(f.get("guestmore").listeners.size, 0);
  assert.equal(f.get("guestremove").listeners.size, 0);
  held.resolve(readAnswer([{t:"user",seq:1,text:"stale"}],1)); await f.feature.settled();
  assert.doesNotMatch(f.get("msgs").innerHTML, /stale/);
});

test("native reader ignores repeated binding and activation with no selection", async () => {
  let calls = 0;
  const f = nativeViewFixture(async () => { calls++; return list(); });
  f.feature.bind();
  f.click("guestmore");
  const keydown = f.get("msgs").listeners.get("keydown");
  keydown({key:"Enter"});
  const turn = {dataset:{seq:"1"}};
  const bubble = {closest: selector => selector === ".bubble" ? bubble : selector === "[data-seq], [data-cseq]" ? turn : null};
  keydown({key:"Enter", target:bubble, preventDefault() {}});
  f.feature.tick(); await f.feature.settled(); assert.equal(calls, 1);
  f.feature.destroy();
});

test("native reader keeps a held comment page while a People send completes", async () => {
  const held = deferred(), paths = [], view = {...base, access:"write"};
  let posts = 0;
  const f = nativeViewFixture(async (path, opts) => {
    if (path === "/api/team/subscriptions") return list([view]);
    if (opts?.method === "POST") { posts++; return {seq:3}; }
    return readAnswer([], 0, false, view);
  }, async path => {
    paths.push(path);
    if (paths.length === 1) return commentsAnswer([{seq:1,anchor:0,author:"a",text:"first"}], 1, true, {a:"Ada"});
    return held.promise;
  });
  f.feature.tick(); await f.feature.settled(); f.open();
  for (let i = 0; i < 20 && paths.length < 2; i++) await Promise.resolve();
  assert.equal(paths.length, 2); assert.match(paths[1], /after=1$/);
  await f.feature.sendPeople("s/o/c/g", "hello");
  assert.equal(posts, 1); assert.equal(paths.length, 2, "sending must not restart a busy reader");
  held.resolve(commentsAnswer([{seq:2,anchor:0,author:"a",text:"second"}], 2, false, {a:"Ada"}));
  await f.feature.settled();
  assert.match(f.get("msgs").innerHTML, /first/); assert.match(f.get("msgs").innerHTML, /second/);
  f.feature.destroy();
});

test("native reader ignores a late error after selecting another received chat", async () => {
  const held = deferred(), other = {...base, chat:"d", title:"Other"};
  const f = nativeViewFixture(async path => {
    if (path === "/api/team/subscriptions") return list([base, other]);
    if (path.includes("/c/g?")) return held.promise;
    return readAnswer([{t:"assistant",seq:1,text:"new chat"}], 1, false, other);
  });
  f.feature.tick(); await f.feature.settled(); f.open(); f.open("team/" + guestKey(other));
  held.reject(new Error('{"error":"copy_damaged"}')); await f.feature.settled();
  assert.equal(f.feature.tab.selected(), "team/" + guestKey(other));
  assert.equal(f.get("chattitle").textContent, "Other");
  assert.match(f.get("msgs").innerHTML, /new chat/); assert.equal(f.get("guestnote").textContent, "");
  f.feature.destroy();
});

test("native reader preserves history and clear seams in the merged timeline", async () => {
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) :
    readAnswer([{t:"start",seq:1,scope:"from_now"},{t:"user",seq:2,text:"prompt"},{t:"source",seq:3,reason:"clear"}], 3),
    async () => commentsAnswer([{seq:1,anchor:2,author:"p",text:"reply"}], 1, false, {p:"Person"}));
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  const html = f.get("msgs").innerHTML;
  assert.match(html, /Earlier messages are not shared/); assert.match(html, /owner cleared/);
  assert.deepEqual([...html.matchAll(/data-(seq|cseq)="(\d+)"/g)].map(m => m[1] + m[2]), ["seq1", "seq2", "cseq1", "seq3"]);
  assert.match(html, /Person/); assert.match(html, /reply/); f.feature.destroy();
});

const readAnswer = (entries = [], next = 0, more = false, over = {}) => ({ view: { ...base, delivery: "ok", ...over }, entries, last: next, next, more });

test("shared capture retains author, provenance and address", () => {
  const capture = guestModule.guestCapture;
  assert.equal(typeof capture, "function");
  const address = { server: "s", owner: "o", chat: "c", generation: "g", log: "mirror", seq: 1 };
  const entry = { t: "user", seq: 1, text: "hello [asset](scimux-asset:abc)", time: "time", prov: true };
  assert.deepEqual(capture(entry, base, {}), { text: "hello asset", role: "person", who: "Owner", seat: "o", prov: true, from: "Chat", turnTime: "time", team: address });
  assert.deepEqual(capture(entry, { ...base, access: "owner" }, {}), { text: "hello asset", role: "user", prov: true, from: "Chat", turnTime: "time", team: address });
  assert.deepEqual(capture({ t: "assistant", seq: 1, text: "answer", agent: "Agent", time: "time" }, base, {}), { text: "answer", role: "assistant", agent: "Agent", from: "Chat", turnTime: "time", team: address });
  assert.deepEqual(capture({ t: "assistant", seq: 1, text: "answer" }, { ...base, title: "" }, {}), { text: "answer", role: "assistant", from: "Untitled chat", turnTime: undefined, team: address });
  assert.equal(capture({ ...entry, prov: false }, { ...base, owner_name: "" }, {}).who, "Unknown person");
  const comment = { seq: 2, text: "message", at: "then", author: "seat", mine: false };
  const commentAddress = { ...address, log: "comments", seq: 2 };
  assert.deepEqual(capture(comment, base, { seat: "Writer" }), { text: "message", role: "person", who: "Writer", seat: "seat", from: "Chat", turnTime: "then", team: commentAddress });
  assert.equal(capture(comment, base, {}).who, "Unknown person");
  assert.deepEqual(capture({ ...comment, mine: true }, { ...base, access: "owner" }, {}), { text: "message", role: "user", from: "Chat", turnTime: "then", team: commentAddress });
  for (const invalid of [{ t: "start", text: "x" }, { t: "source", text: "x" }, { t: "other", text: "x" }, { t: "user", omitted: "too_large" }, { t: "user" }, { t: "user", text: "" }]) assert.equal(capture(invalid, base, {}), null);
});

test("shared header describes each delivery and access state", () => {
  for (const [v, text] of [
    [{ state: "ended" }, "Sharing ended · saved copy"],
    [{ state: "live", delivery: "ok", access: "read" }, "Can view"],
    [{ state: "live", delivery: "ok", access: "write" }, "Can comment"],
    [{ state: "live", delivery: "offline", access: "read" }, "Can view · offline, showing the saved copy"],
    [{ state: "live", delivery: "offline", access: "write" }, "Can comment · offline, showing the saved copy"],
    [{ state: "live", delivery: "refused" }, "The server ended this computer's access · saved copy"],
    [{ state: "live", delivery: "changed" }, "Not the same server · saved copy"],
  ]) assert.equal(guestStateText(v), text);
});

for (const [access, delivery, from, state] of [
  ["owner", "ok", "Your chat · Server", "Shared by you"],
  ["owner", "offline", "Your chat · Server", "Shared by you · offline, showing the saved copy"],
  ["read", "ok", "From Owner · Server", "Can view"],
  ["write", "offline", "From Owner · Server", "Can comment · offline, showing the saved copy"],
]) {
  if (access !== "owner") test(`shared header for ${access} with ${delivery}`, async () => {
    const view = { ...base, access, delivery };
    const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([view]) : readAnswer([], 0, false, view));
    f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
    assert.equal(f.get("chatmission").textContent, from);
    assert.equal(f.get("gueststate").textContent, state);
  });
  test(`shared state for ${access} with ${delivery}`, () => {
    assert.equal(guestStateText({ state: "live", access, delivery }), state);
  });
}

test("read errors have safe fixed messages", () => {
  for (const [code, text] of [["not_found", "This chat is no longer in your list."], ["copy_damaged", "The saved copy of this chat is damaged and cannot be shown."], ["storage_full", "This computer's storage is full. Free some space to load more."], ["other", "That did not work. It will be tried again."]]) assert.equal(guestErrorText(code), text);
});

test("shared entries render all kinds and escape untrusted content", () => {
  const e = (t, extra = {}) => ({ t, seq: 1, time: "2026-01-01T00:00:00Z", ...extra });
  assert.match(guestEntryHTML(e("start", { scope: "from_now" }), "Owner"), /Earlier messages are not shared/);
  assert.match(guestEntryHTML(e("start"), "Owner"), /Shared with its history/);
  assert.match(guestEntryHTML(e("source", { reason: "clear" }), "Owner"), /owner cleared/);
  assert.match(guestEntryHTML(e("source"), "Owner"), /new session/);
  assert.equal(guestEntryHTML(e("unknown"), "Owner"), "");
  assert.match(guestEntryHTML(e("user", { text: '<&"', prov: true }), '<&"'), /includes forwarded AI text/);
  assert.match(guestEntryHTML(e("user", { text: '<&"' }), '<&"'), /&lt;&amp;&quot;/);
  assert.match(guestEntryHTML(e("user", { omitted: "too_large" }), ""), /Unknown person.*This message was too large to share/);
  assert.match(guestEntryHTML(e("assistant", { omitted: "too_large" }), "Owner"), /Agent.*This message was too large to share/);
  assert.match(guestEntryHTML(e("assistant", { agent: '<&"', text: '```\n<&"\n```\nscimux-asset:abc' }), "Owner"), /&lt;&amp;&quot;/);
  assert.doesNotMatch(guestEntryHTML(e("assistant", { text: "[asset](scimux-asset:abc)" }), "Owner"), /scimux-asset:/);
});

test("native reader: open pages, header and navigation away", async () => {
  const paths = [];
  const f = nativeViewFixture(async (path) => { paths.push(path); if (path === "/api/team/subscriptions") return list([base]); if (path.endsWith("after=0")) return readAnswer([{ t: "user", seq: 1, text: "synthetic", time: "" }], 1, true); if (path.endsWith("after=1")) return readAnswer([], 2, true); if (path.endsWith("after=2")) return readAnswer([], 3); return ""; });
  f.feature.tick(); await f.feature.settled(); paths.length = 0;
  f.open(); assert.equal(f.selected(), "team/s/o/c/g"); assert.equal(f.feature.tab.selected(), f.selected());
  await f.feature.settled();
  assert.deepEqual(paths.slice(0, 3), ["/api/team/subscriptions/s/o/c/g?after=0", "/api/team/subscriptions/s/o/c/g?after=1", "/api/team/subscriptions/s/o/c/g?after=2"]);
  assert.equal(f.get("chattitle").textContent, "Chat"); assert.equal(f.get("chatmission").textContent, "From Owner · Server"); assert.equal(f.get("gueststate").textContent, "Can view");
  assert.match(f.get("msgs").innerHTML, /synthetic/);
  f.navigate(); assert.equal(f.feature.tab.selected(), ""); assert.equal(f.get("msgs").innerHTML, "");
});

test("native reader: empty view, paging cap and load more", async () => {
  let reads = 0;
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); if (path.endsWith("/seen")) return ""; reads++; if (reads === 41) return readAnswer([], 41, false); return readAnswer([], reads, true); });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  assert.equal(reads, 40); assert.equal(f.get("guestmore").hidden, false); assert.match(f.get("msgs").innerHTML, /Nothing has been shared yet/);
  f.click("guestmore"); await f.feature.settled(); assert.equal(reads, 41); assert.equal(f.get("guestmore").hidden, true);
});

test("native reader: late read after navigation is dropped", async () => {
  const pending = deferred();
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); return pending.promise; });
  f.feature.tick(); await f.feature.settled(); f.open();
  f.navigate(); assert.equal(f.feature.tab.selected(), "");
  pending.resolve(readAnswer([{ t: "user", text: "late", seq: 1 }], 1)); await f.feature.settled();
  assert.equal(f.get("msgs").innerHTML, "");
});

test("native reader: read failures show fixed notes and a later success clears them", async () => {
  for (const [code, expected] of [["not_found", "This chat is no longer in your list."], ["copy_damaged", "The saved copy of this chat is damaged and cannot be shown."], ["storage_full", "This computer's storage is full. Free some space to load more."], ["bad-json", "That did not work. It will be tried again."]]) {
    let reads = 0;
    const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); reads++; if (reads === 1) { if (code === "bad-json") throw new Error("not json"); throw new Error(JSON.stringify({ error: code })); } return readAnswer(); });
    f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled(); assert.equal(f.get("guestnote").textContent, expected);
    if (code !== "not_found") { f.setTime(5000); f.feature.tick(); await f.feature.settled(); assert.equal(f.get("guestnote").textContent, ""); }
  }
});


test("native reader: opening another chat drops the first pending answer", async () => {
  const old = deferred(), second = { ...base, chat: "d" };
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([base, second]); if (path.includes("/c/g?")) return old.promise; return readAnswer([{ t: "user", text: "second", seq: 1 }], 1, false, second); });
  f.feature.tick(); await f.feature.settled(); f.open();
  f.open("team/" + guestKey(second)); assert.equal(f.feature.tab.selected(), "team/" + guestKey(second)); old.resolve(readAnswer([{ t: "user", text: "stale", seq: 1 }], 1)); await f.feature.settled();
  assert.match(f.get("msgs").innerHTML, /second/); assert.doesNotMatch(f.get("msgs").innerHTML, /stale/);
});

test("native reader: following waits five seconds and never moves focus while reading", async () => {
  let reads = 0;
  const pending = deferred();
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); if (path.endsWith("/seen")) return ""; reads++; if (reads === 2) return pending.promise; return readAnswer(); });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  f.setTime(4999); f.feature.tick(); assert.equal(reads, 1);
  const focused = f.doc.activeElement; f.setTime(5000); f.feature.tick(); assert.equal(reads, 2);
  f.setTime(10000); f.feature.tick(); assert.equal(reads, 2); assert.equal(f.doc.activeElement, focused);
  pending.resolve(readAnswer()); await f.feature.settled(); assert.equal(f.doc.activeElement, focused);
});

test("native reader: seen advances after successful reads and updates badge", async () => {
  const seen = [];
  const f = nativeViewFixture(async (path, opts) => { if (path === "/api/team/subscriptions") return list([{ ...base, unread: true }]); if (path.endsWith("/seen")) { seen.push(opts); return ""; } return readAnswer([{ t: "user", seq: 1, text: "x" }], 1); });
  f.feature.tick(); await f.feature.settled(); const before = f.invalidations(); assert.match(f.feature.tab.tabHTML(false), /1 unread/);
  f.open(); await f.feature.settled();
  assert.equal(seen.length, 1); assert.equal(seen[0].method, "PUT"); assert.deepEqual(JSON.parse(seen[0].body), { mirror: 1 });
  assert.doesNotMatch(f.feature.tab.tabHTML(false), /unread/); assert.equal(f.invalidations(), before + 2); // selection and the seen update each invalidate cards
  f.setTime(5000); f.feature.tick(); await f.feature.settled(); assert.equal(seen.length, 1);
});

test("native reader: failed seen is retried after another successful run", async () => {
  let puts = 0;
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([{ ...base, unread: true }]); if (path.endsWith("/seen")) { puts++; if (puts === 1) throw new Error("failed"); return ""; } return readAnswer([], 1); });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  assert.match(f.feature.tab.tabHTML(false), /1 unread/); assert.equal(puts, 1);
  f.setTime(5000); f.feature.tick(); await f.feature.settled(); assert.equal(puts, 2); assert.doesNotMatch(f.feature.tab.tabHTML(false), /unread/);
});

test("native reader: a failed read is not marked and not_found stops following", async () => {
  let reads = 0, puts = 0;
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); if (path.endsWith("/seen")) { puts++; return ""; } reads++; throw new Error('{"error":"not_found"}'); });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled(); f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.equal(reads, 1); assert.equal(puts, 0);
});

test("native reader: ended subscriptions remain readable", async () => {
  let reads = 0;
  const ended = { ...base, state: "ended" };
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([ended]); if (path.endsWith("/seen")) return ""; reads++; return readAnswer([{ t: "user", seq: 1, text: "saved" }], 1, false, ended); });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  assert.equal(reads, 1); assert.match(f.get("msgs").innerHTML, /saved/); assert.equal(f.get("gueststate").textContent, "Sharing ended · saved copy");
});


test("native reader: following an initially empty chat replaces the empty message", async () => {
  let reads = 0;
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); if (path.endsWith("/seen")) return ""; reads++; if (reads === 1) return readAnswer(); return readAnswer([{ t: "user", seq: 1, text: "new entry" }], 1); });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled(); assert.match(f.get("msgs").innerHTML, /Nothing has been shared yet/);
  f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.match(f.get("msgs").innerHTML, /new entry/); assert.doesNotMatch(f.get("msgs").innerHTML, /Nothing has been shared yet/);
});

test("native reader: first unread is scrolled into view and later appends respect scroll position", async () => {
  let reads = 0, scrolls = 0;
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); if (path.endsWith("/seen")) return ""; reads++; return readAnswer([{ t: "user", seq: reads, text: "entry" }], reads); });
  const body = f.get("msgs"); body.querySelector = () => ({ scrollIntoView(opts) { assert.deepEqual(opts, { block: "start" }); scrolls++; } });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled(); assert.equal(scrolls, 1);
  body.scrollTop = 0; f.setTime(5000); f.feature.tick(); await f.feature.settled(); assert.equal(body.scrollTop, 0);
  body.scrollTop = 750; f.setTime(10000); f.feature.tick(); await f.feature.settled(); assert.equal(body.scrollTop, body.scrollHeight);
});

test("list sorting handles reversed states and writing access", async () => {
  const groups = guestGroups([{ ...base, state: "live" }, { ...base, chat: "ended", state: "ended" }], []);
  assert.deepEqual(groups[0].items.map(v => v.state), ["live", "ended"]);
  const f = nativeViewFixture(async () => list([{ ...base, access: "write" }]));
  f.feature.tick(); await f.feature.settled(); assert.equal(f.feature.tab.activities()[0].access, "write");
});

test("destroy drops an in-flight list answer", async () => {
  const pending = deferred(); const f = nativeViewFixture(() => pending.promise);
  f.feature.tick(); f.feature.destroy(); pending.resolve(list([base])); await f.feature.settled();
  assert.equal(f.invalidations(), 0); assert.equal(f.feature.tab.tabHTML(false), "");
});

test("native reader: empty header fields use their fallbacks", async () => {
  const view = { ...base, title: "", owner_name: "" };
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([view]); return readAnswer([], 0, false, view); });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  assert.equal(f.get("chattitle").textContent, "Untitled chat"); assert.equal(f.get("chatmission").textContent, "From Unknown person · Server");
});

test("an initial empty list does not invalidate cards even with server status", async () => {
  const f = nativeViewFixture(async () => list([], [{ id: "s", delivery: "offline" }], true));
  f.feature.tick(); await f.feature.settled(); assert.equal(f.invalidations(), 0); assert.equal(f.feature.tab.tabHTML(false), "");
});


test("native reader: destroy drops pending read and list errors", async () => {
  const read = deferred();
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); return read.promise; });
  f.feature.tick(); await f.feature.settled(); f.open(); f.feature.destroy(); read.resolve(readAnswer([{ t: "user", text: "late", seq: 1 }], 1)); await f.feature.settled();
  assert.doesNotMatch(f.get("msgs").innerHTML, /late/);
  const pending = deferred(), g = nativeViewFixture(() => pending.promise);
  g.feature.tick(); g.feature.destroy(); pending.reject(Object.assign(new Error("off"), { status: 404 })); await g.feature.settled(); assert.equal(g.invalidations(), 0);
});

test("native reader: a partial read mark keeps a newer live entry unread", async () => {
  const view = { ...base, unread: true, mirror_last: 3 };
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([view]); if (path.endsWith("/seen")) return ""; return readAnswer([{ t: "user", seq: 1, text: "x" }], 1, false, view); });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled(); assert.match(f.feature.tab.tabHTML(false), /1 unread/);
});

test("native reader: load more does not overlap a read or retry a missing chat", async () => {
  const pending = deferred(); let reads = 0;
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); reads++; return pending.promise; });
  f.feature.tick(); await f.feature.settled(); f.open(); f.click("guestmore"); assert.equal(reads, 1);
  pending.reject(new Error('{"error":"not_found"}')); await f.feature.settled(); f.click("guestmore"); assert.equal(reads, 1);
});

test("native reader: gate turning off during a read hides the tab and sends no read mark", async () => {
  const read = deferred(); let lists = 0, puts = 0;
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") { lists++; if (lists === 1) return list([base]); throw Object.assign(new Error("off"), { status: 404 }); } if (path.endsWith("/seen")) { puts++; return ""; } return read.promise; });
  f.feature.tick(); await f.feature.settled(); f.open();
  const before = f.invalidations(); f.setTime(10000); f.feature.tick();
  await Promise.resolve(); await Promise.resolve(); read.resolve(readAnswer([{ t: "user", seq: 1, text: "x" }], 1)); await f.feature.settled();
  assert.equal(puts, 0); assert.equal(f.feature.tab.tabHTML(false), ""); assert.equal(f.invalidations(), before + 1);
});

test("native reader: late read errors and late seen responses do not update a closed view", async () => {
  const read = deferred();
  const a = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); return read.promise; });
  a.feature.tick(); await a.feature.settled(); a.open(); a.navigate(); read.reject(new Error('{"error":"copy_damaged"}')); await a.feature.settled(); assert.equal(a.get("guestnote").textContent, "");
  const seen = deferred();
  const b = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([{ ...base, unread: true }]); if (path.endsWith("/seen")) return seen.promise; return readAnswer([], 1); });
  b.feature.tick(); await b.feature.settled(); b.open(); await Promise.resolve(); b.navigate(); seen.resolve(""); await b.feature.settled(); assert.match(b.feature.tab.tabHTML(false), /1 unread/);
});

test("native reader: a completed read mark tolerates its subscription disappearing", async () => {
  const seen = deferred(); let lists = 0;
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") { lists++; if (lists === 1) return list([base]); return list(); } if (path.endsWith("/seen")) return seen.promise; return readAnswer([], 1); });
  f.feature.tick(); await f.feature.settled(); f.open(); await Promise.resolve(); f.setTime(10000); f.feature.tick(); await Promise.resolve(); await Promise.resolve(); seen.resolve(""); await f.feature.settled();
  assert.equal(f.feature.tab.tabHTML(false), "");
});


/* Condition table (condition | test for each deciding half):
 * guestEntryHTML: user || assistant | shared entries render all kinds: user and assistant.
 * cards tab Archived class, carried from the original cardTabsHTML | archived
 * tab active class follows the selected tab: archived and current.
 * cards waiting prefix conditional | shared cards path keeps the list (one waiting),
 * shared cards title has no waiting prefix when nobody is waiting (zero waiting).
 * A newer list request cannot overtake an older one: tick refuses a second load
 * while loading, and destroy drops the pending response.
 */

test("native reader: load more cannot issue requests after the gate turns off", async () => {
  let lists = 0, reads = 0;
  const f = nativeViewFixture(async path => {
    if (path === "/api/team/subscriptions") {
      lists++;
      if (lists === 1) return list([base]);
      return decodeResponse(jsonResponse('{"error":"not_found"}', { status: 404, etag: "" }));
    }
    if (path.endsWith("/seen")) return "";
    reads++; return readAnswer([], 0, true);
  });
  f.feature.tick(); await f.feature.settled(); f.setTime(9000); f.open(); await f.feature.settled();
  assert.equal(reads, 40); f.setTime(10000); f.feature.tick(); await f.feature.settled();
  const before = reads; f.click("guestmore"); await f.feature.settled(); assert.equal(reads, before);
});

test("an omitted field displays the omission message even when empty", () => {
  for (const kind of ["user", "assistant"]) {
    assert.match(guestEntryHTML({ t: kind, seq: 1, omitted: "" }, "Owner"), /This message was too large to share\./);
  }
});

test("removal cancel leaves the copy and returns focus", async () => {
  let calls=0;
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); calls++; return readAnswer(); }, undefined, undefined, {confirm:()=>false});
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  f.click("guestremove"); assert.equal(calls,1); assert.equal(f.doc.activeElement,f.get("guestremove"));
  assert.equal(f.feature.tab.selected(),"team/s/o/c/g");
});

test("native removal blocks duplicate deletes and read runs, then calls the panel", async () => {
  const pending = deferred(); let deletes = 0, reads = 0;
  const other = { ...base, chat: "d" };
  const f = nativeViewFixture(async (path, opts) => {
    if (path === "/api/team/subscriptions") return list([base, other]);
    if (opts) { if (opts.method === "DELETE") { deletes++; return pending.promise; } }
    reads++; return readAnswer();
  });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  const before = f.invalidations(); f.get("guestremove").focus(); f.click("guestremove");
  assert.equal(deletes, 1); assert.equal(f.get("guestremove").disabled, true); f.click("guestremove");
  f.setTime(5000); f.feature.tick(); assert.equal(reads, 1); assert.equal(deletes, 1);
  pending.resolve(""); await f.feature.settled();
  assert.equal(f.feature.tab.selected(), ""); assert.equal(f.selected(), "local");
  assert.equal(f.invalidations(), before + 1); assert.match(f.feature.tab.tabHTML(false), /Shared/);
  assert.equal(f.feature.hasActivity("team/s/o/c/g"), false);
});

test("native removal treats not found as success and calls the panel", async () => {
  const f = nativeViewFixture(async (path, opts) => {
    if (path === "/api/team/subscriptions") return list([base]);
    if (opts) { if (opts.method === "DELETE") throw Object.assign(new Error('{"error":"not_found"}'), { status: 404 }); }
    return readAnswer();
  });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  const before = f.invalidations(); f.click("guestremove"); await f.feature.settled();
  assert.equal(f.feature.tab.selected(), ""); assert.equal(f.selected(), "local");
  assert.equal(f.feature.tab.tabHTML(false), ""); assert.equal(f.invalidations(), before + 1);
});

test("removal failures reset the button and keep their note across ticks", async () => {
  for (const [code, expected] of [["unreachable", "The server could not be reached. Try again later."], ["other", "That did not work. Try again."]]) {
    let deletes = 0;
    const f = nativeViewFixture(async (path, opts) => {
      if (path === "/api/team/subscriptions") return list([base]);
      if (opts) { if (opts.method === "DELETE") { deletes++; throw new Error(JSON.stringify({ error: code })); } }
      return readAnswer();
    });
    f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
    f.click("guestremove"); await f.feature.settled();
    assert.equal(deletes, 1); assert.equal(f.get("guestremove").disabled, false); assert.equal(f.doc.activeElement, f.get("guestremove"));
    assert.equal(f.get("guestnote").textContent, expected); f.setTime(5000); f.feature.tick(); await f.feature.settled(); assert.equal(f.get("guestnote").textContent, expected);
  }
});

test("cancelled removal stays selected across a poll", async () => {
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer(), undefined, undefined, {confirm:()=>false});
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled(); f.click("guestremove");
  f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.equal(f.feature.tab.selected(),"team/s/o/c/g"); assert.equal(f.get("guestnote").textContent, "");
});

test("late successful removal drops its item without changing a different open view", async () => {
  for (const status of [204, 404]) {
    const pending = deferred(), other = { ...base, chat: "d", title: "Other" };
    const f = nativeViewFixture(async (path, opts) => {
      if (path === "/api/team/subscriptions") return list([base, other]);
      if (opts) { if (opts.method === "DELETE") return pending.promise; }
      if (path.includes("/d/g?")) return readAnswer([], 0, false, other);
      return readAnswer();
    });
    f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled(); f.click("guestremove");
    f.open("team/" + guestKey(other)); await Promise.resolve();
    if (status === 404) pending.reject(Object.assign(new Error('{"error":"not_found"}'), { status: 404 }));
    else pending.resolve("");
    await f.feature.settled(); assert.equal(f.feature.tab.selected(), "team/" + guestKey(other)); assert.equal(f.get("chattitle").textContent, "Other");
    assert.equal(f.feature.hasActivity("team/s/o/c/g"), false);
  }
});

test("late failed removal does not alter the newly opened view", async () => {
  const pending = deferred(), other = { ...base, chat: "d", title: "Other" };
  let deletes = 0;
  const f = nativeViewFixture(async (path, opts) => {
    if (path === "/api/team/subscriptions") return list([base, other]);
    if (opts) { if (opts.method === "DELETE") { deletes++; return pending.promise; } }
    return readAnswer([], 0, false, other);
  });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled(); f.click("guestremove");
  assert.equal(deletes, 1); f.open("team/" + guestKey(other)); pending.reject(new Error('{"error":"unreachable"}')); await f.feature.settled();
  assert.equal(f.feature.tab.selected(), "team/" + guestKey(other)); assert.equal(f.get("chattitle").textContent, "Other"); assert.equal(f.get("guestnote").textContent, "");
});

test("late failed removal does not affect a local selection", async () => {
  const pending = deferred(); let deletes = 0;
  const f = nativeViewFixture(async (path, opts) => {
    if (path === "/api/team/subscriptions") return list([base]);
    if (opts?.method === "DELETE") { deletes++; return pending.promise; }
    return readAnswer();
  });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  f.click("guestremove"); f.navigate("local");
  const note = f.get("guestnote").textContent;
  pending.reject(new Error('{"error":"unreachable"}')); await f.feature.settled();
  assert.equal(deletes, 1); assert.equal(f.feature.tab.selected(), ""); assert.equal(f.selected(), "local");
  assert.equal(f.get("guestnote").textContent, note); assert.equal(f.feature.hasActivity("team/s/o/c/g"), true);
  f.feature.destroy();
});

test("native removal cannot select an owner copy or delete its chat", async () => {
  const owned = {...base, access:"owner", chat:"owned"}; let deletes = 0, selected = 0;
  const f = nativeViewFixture(async (path, opts) => {
    if (opts?.method === "DELETE") deletes++;
    return path === "/api/team/subscriptions" ? list([owned]) : readAnswer([], 0, false, owned);
  }, undefined, undefined, {panel:{select(){selected++}}});
  f.feature.tick(); await f.feature.settled(); f.open("team/" + guestKey(owned));
  f.click("guestremove"); await f.feature.settled();
  assert.equal(selected, 0); assert.equal(deletes, 0); assert.equal(f.feature.tab.selected(), "");
  f.feature.destroy();
});

test("late removal after closing drops the chat without reopening the view", async () => {
  for (const status of [204, 404]) {
    const pending = deferred();
    const f = nativeViewFixture(async (path, opts) => {
      if (path === "/api/team/subscriptions") return list([base]);
      if (opts) { if (opts.method === "DELETE") return pending.promise; }
      return readAnswer();
    });
    f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
    f.click("guestremove"); f.navigate();
    const before = f.invalidations();
    if (status === 404) pending.reject(Object.assign(new Error("not found"), { status: 404 }));
    else pending.resolve("");
    await f.feature.settled(); assert.equal(f.feature.tab.selected(), ""); assert.equal(f.feature.tab.tabHTML(false), ""); assert.equal(f.invalidations(), before + 1);
  }
});

test("a non-JSON removal error uses the generic message", async () => {
  const f = nativeViewFixture(async (path, opts) => {
    if (path === "/api/team/subscriptions") return list([base]);
    if (opts) { if (opts.method === "DELETE") throw new Error("not json"); }
    return readAnswer();
  });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled(); f.click("guestremove"); await f.feature.settled();
  assert.equal(f.get("guestnote").textContent, "That did not work. Try again.");
});

test("late removal after destroy has no effects", async () => {
  const pending = deferred(); let deletes = 0;
  const f = nativeViewFixture(async (path, opts) => {
    if (path === "/api/team/subscriptions") return list([base]);
    if (opts) { if (opts.method === "DELETE") { deletes++; return pending.promise; } }
    return readAnswer();
  });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled(); f.click("guestremove");
  assert.equal(deletes, 1); const before = f.invalidations(); f.feature.destroy(); pending.resolve(""); await f.feature.settled();
  assert.equal(f.invalidations(), before);
});

test("removal controls do nothing when no view is open", async () => {
  let calls = 0;
  const f = nativeViewFixture(async () => { calls++; return list(); });
  f.click("guestremove"); f.click("msgs", f.get("msgs"));
  f.feature.tick(); await f.feature.settled(); assert.equal(calls, 1);
});

/* Wave 4c condition table:
 * | condition | test for each half |
 * | none | Wave 4c adds no &&, ||, or ?: condition. |
 */

test("a failed follow read after cancelled removal reports its error", async () => {
  let reads = 0;
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); if (++reads === 2) throw new Error('{"error":"copy_damaged"}'); return readAnswer(); }, undefined, undefined, {confirm:()=>false});
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  f.click("guestremove"); f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.equal(reads,2); assert.match(f.get("guestnote").textContent,/damaged/);
});


test("native reader: first shared read without unread entries scrolls to the end", async () => {
  const view = { ...base, seen_mirror: 3, mirror_last: 3 };
  const f = nativeViewFixture(async path => {
    if (path === "/api/team/subscriptions") return list([view]);
    return readAnswer([{ t: "user", seq: 1, text: "" }], 1, false, view);
  });
  const body = f.get("msgs"); let scrolls = 0;
  body.querySelector = () => ({ scrollIntoView() { scrolls++; } });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  assert.equal(body.scrollTop, body.scrollHeight); assert.equal(scrolls, 0);
});

test("native reader: shared follow scrolls at 80 pixels but not 81", async () => {
  for (const distance of [80, 81]) {
    let reads = 0;
    const f = nativeViewFixture(async path => {
      if (path === "/api/team/subscriptions") return list([base]);
      if (path.endsWith("/seen")) return "";
      reads++;
      if (reads === 1) return readAnswer();
      return readAnswer([{ t: "user", seq: 1, text: "" }], 1);
    });
    f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
    const body = f.get("msgs");
    body.scrollTop = body.scrollHeight - body.clientHeight - distance;
    const before = body.scrollTop;
    f.setTime(5000); f.feature.tick(); await f.feature.settled();
    if (distance === 80) assert.equal(body.scrollTop, body.scrollHeight);
    if (distance === 81) assert.equal(body.scrollTop, before);
  }
});

test("owner groups lead shared groups by server name and id", () => {
  const mineA = { ...base, server: "a", server_name: "Lab", owner: "own-a", owner_name: "Self", access: "owner" };
  const mineB = { ...base, server: "b", server_name: "lab", owner: "own-b", owner_name: "Self", access: "owner" };
  const guest = { ...base, server: "g", server_name: "A", owner_name: "Aaron" };
  const groups = guestGroups([guest, mineB, mineA, { ...mineA, owner: "own-again", chat: "two" }], [{ id: "a", delivery: "offline" }, { id: "b", delivery: "refused" }]);
  assert.equal(groups.length, 3);
  assert.match(groups[0].label, /^Mine · Lab · offline$/);
  assert.match(groups[1].label, /^Mine · lab · access ended$/);
  assert.equal(groups[0].items.length, 2);
  assert.equal(groups[2].ownerName, "Aaron");
  assert.match(guestGroups([mineA, guest], [])[0].label, /^Mine/);
  const later = { ...mineA, server: "z", server_name: "Zulu" };
  assert.deepEqual(guestGroups([later, mineA], []).map(group => group.server), ["a", "z"]);
});

test("owner subscriptions do not create a Shared with me tab", async () => {
  const owned = { ...base, access: "owner", unread: true };
  const f = nativeViewFixture(async () => list([owned, { ...owned, chat: "ended", state: "ended" }]));
  f.feature.tick(); await f.feature.settled();
  assert.equal(f.feature.tab.tabHTML(false), "");
  assert.equal(f.feature.tab.mineUnread(), true);
  assert.deepEqual(f.feature.tab.activities(), []);
});

const commentsAnswer = (entries = [], next = 0, more = false, names = {}) => ({ entries, names, last: next, next, more });

test("guest messages escape text names and time and show authorship", () => {
  const entry = { seq: 3, anchor: 1, author: "a", mine: false, at: '<&"', text: '```\n<&"\n```' };
  const other = guestMessageHTML(entry, { a: '<&"' });
  assert.ok(other.includes('class="turn assistant gmsg"'));
  assert.ok(other.includes('data-cseq="3"') && other.includes('data-anchor="1"'));
  assert.ok(other.includes("&lt;&amp;&quot;"));
  assert.ok(other.includes("<pre><code>"));
  assert.ok(guestMessageHTML(entry, {}).includes("Unknown person"));
  const mine = guestMessageHTML({ ...entry, mine: true }, { a: "Name" });
  assert.ok(mine.includes('class="turn user gmsg"'));
  assert.match(mine, /You · People/);
});

test("guest mirror prompts name their human source", () => {
  assert.ok(guestEntryHTML({ t:"user", seq:1, text:"x" }, "Owner").includes("Owner · prompt"));
  assert.ok(guestEntryHTML({ t:"user", seq:1, text:"x", prov:true }, "Owner").includes("Owner · prompt · includes forwarded AI text"));
});

test("native reader: shared view merges both logs by anchor and keeps names", async () => {
  const paths = [], puts = [];
  const view = { ...base, mirror_last:4, comments_last:3, unread:true };
  const f = nativeViewFixture(async (path, opts) => {
    if (path === "/api/team/subscriptions") return list([view]);
    if (path.endsWith("/seen")) { puts.push(JSON.parse(opts.body)); return ""; }
    paths.push(path);
    if (path.endsWith("after=0")) return readAnswer([{ t:"user", seq:1, text:"x" }, { t:"assistant", seq:3, text:"y" }], 3, false, view);
    return readAnswer([{ t:"assistant", seq:4, text:"z" }], 4, false, view);
  }, async path => {
    paths.push(path);
    if (path.endsWith("after=0")) return commentsAnswer([{ seq:1, anchor:1, author:"a", text:"x" }, { seq:2, anchor:1, author:"b", text:"y" }], 2, true, { a:"Ada" });
    if (path.endsWith("after=2")) return commentsAnswer([{ seq:3, anchor:4, author:"a", text:"z" }], 3, false, { b:"Bea" });
    return commentsAnswer([], 3);
  });
  const label = { dataset: { seat: "b" }, textContent: "" };
  f.get("msgs").querySelectorAll = sel => sel === "[data-seat]" ? [label] : [];
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  const order = () => [...f.get("msgs").innerHTML.matchAll(/data-(seq|cseq)="(\d+)"/g)].map(m => m[1] + m[2]);
  assert.deepEqual(order(), ["seq1", "cseq1", "cseq2", "seq3", "cseq3"]);
  assert.ok(f.get("msgs").innerHTML.includes("Ada")); assert.equal(label.textContent, "Bea");
  assert.deepEqual(puts[0], { mirror:3, comments:3 });
  assert.ok(paths[0].endsWith("?after=0")); assert.ok(paths[1].endsWith("/comments?after=0")); assert.ok(paths[2].endsWith("/comments?after=2"));
  f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.deepEqual(order(), ["seq1", "cseq1", "cseq2", "seq3", "seq4", "cseq3"]);
});

test("native reader: comment paging has its own forty-page cap", async () => {
  let calls = 0;
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); if (path.endsWith("/seen")) return ""; return readAnswer(); }, async () => { calls++; return commentsAnswer([], calls, true); });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  assert.equal(calls, 40); assert.equal(f.get("guestmore").hidden, false);
});

test("native reader: first unread comment scrolls into view", async () => {
  let selector = "", scrolls = 0;
  const view = { ...base, seen_mirror:1, seen_comments:0, comments_last:1 };
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([view]); if (path.endsWith("/seen")) return ""; return readAnswer([{ t:"user", seq:1, text:"x" }], 1, false, view); }, async () => commentsAnswer([{ seq:1, anchor:1, author:"a", text:"x" }], 1));
  f.get("msgs").querySelector = sel => { selector = sel; return { scrollIntoView() { scrolls++; } }; };
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  assert.equal(selector, '[data-cseq="1"]'); assert.equal(scrolls, 1);
});


test("native reader: read marks include only advanced received logs", async () => {
  for (const tc of [
    { mirror:1, comments:0, body:{mirror:1}, access:"read", unread:false },
    { mirror:0, comments:1, body:{comments:1}, access:"read", unread:false },
    { mirror:1, comments:1, body:{mirror:1,comments:1}, access:"read", unread:false },
    { mirror:0, comments:0, body:null, access:"read", unread:false },
    { mirror:1, comments:0, body:{mirror:1}, access:"read", unread:true, pendingComments:2 },
  ]) {
    const seen = [];
    const view = { ...base, access:tc.access, mirror_last:tc.mirror, comments_last:tc.pendingComments || tc.comments, unread:true };
    const f = nativeViewFixture(async (path, opts) => { if (path === "/api/team/subscriptions") return list([view]); if (path.endsWith("/seen")) { seen.push(JSON.parse(opts.body)); return ""; } return readAnswer([], tc.mirror, false, view); }, async () => commentsAnswer([], tc.comments));
    f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
    assert.deepEqual(seen, tc.body ? [tc.body] : []);
    if (tc.body) assert.equal(f.feature.tab.tabHTML(false).includes("unread"), tc.unread);
  }
});

test("native reader: following an empty view replaces it with a new comment", async () => {
  let calls = 0;
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); if (path.endsWith("/seen")) return ""; return readAnswer(); }, async () => { calls++; if (calls === 1) return commentsAnswer(); return commentsAnswer([{ seq:1, anchor:0, author:"a", text:"x" }], 1); });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  assert.ok(f.get("msgs").innerHTML.includes("Nothing has been shared yet."));
  f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.ok(f.get("msgs").innerHTML.includes('data-cseq="1"'));
  assert.ok(!f.get("msgs").innerHTML.includes("Nothing has been shared yet."));
});

test("native reader: late combined seen answer does not mark a closed view", async () => {
  const seen = deferred(), started = deferred();
  const f = nativeViewFixture(async path => { if (path === "/api/team/subscriptions") return list([{ ...base, unread:true }]); if (path.endsWith("/seen")) { started.resolve(); return seen.promise; } return readAnswer([], 1); });
  f.feature.tick(); await f.feature.settled(); f.open(); await started.promise;
  f.navigate(); seen.resolve(""); await f.feature.settled();
  assert.ok(f.feature.tab.tabHTML(false).includes("1 unread"));
});

test("guest comment keys use eight bytes in lower-case hex", () => {
  assert.equal(guestHexKey(new Uint8Array([0, 1, 15, 16, 127, 128, 254, 255])), "00010f107f80feff");
});














/* Wave 5b condition table:
 * | condition | test for each half |
 * | !morePages && !moreComments | empty view, paging cap and load more (mirror); comment paging has its own forty-page cap (comments) |
 * Mutating either half was caught by the corresponding test.
 */

test("capturable shared bubbles are keyboard reachable", () => {
  assert.match(guestEntryHTML({ t: "user", seq: 1, text: "x" }, "Owner"), /class="bubble"[^>]*tabindex="0"/);
  assert.doesNotMatch(guestEntryHTML({ t: "user", seq: 1, omitted: "too_large" }, "Owner"), /tabindex/);
  assert.doesNotMatch(guestEntryHTML({ t: "start", seq: 1 }, "Owner"), /tabindex/);
  assert.match(guestMessageHTML({ seq: 2, anchor: 1, text: "x", at: "", author: "a" }, {}), /class="bubble"[^>]*tabindex="0"/);
});

function sharedActionBubble(seq) {
  const turn = { dataset: { seq: String(seq) }, nextElementSibling: null };
  const bubble = { closest(sel) {
    if (sel === ".bubble") return bubble;
    if (sel === "[data-seq], [data-cseq]") return turn;
    return null;
  } };
  turn.insertAdjacentHTML = (_where, html) => {
    turn.rowHTML = html;
    turn.nextElementSibling = { dataset: { gactions: "" }, remove() { turn.nextElementSibling = null; } };
  };
  return { turn, bubble };
}

test("shared actions open, close, move, bookmark and send", async () => {
  const stored = [], ops = [], sends = [];
  const entries = [{ t: "user", seq: 1, text: "one", time: "then" }, { t: "assistant", seq: 2, text: "two", agent: "A", time: "then" }];
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer(entries, 2), undefined, undefined,
    { bookmarks: () => stored, uiMutate: op => { ops.push(op); stored.push(op.bookmark); }, openSendTo: opts => sends.push(opts) });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  const first = sharedActionBubble(1), second = sharedActionBubble(2);
  const click = target => f.click("msgs", target);
  click(first.bubble); assert.match(first.turn.rowHTML, /data-gactions/); assert.ok(first.turn.nextElementSibling);
  click(first.bubble); assert.equal(first.turn.nextElementSibling, null);
  click(first.bubble); click(second.bubble); assert.equal(first.turn.nextElementSibling, null); assert.ok(second.turn.nextElementSibling);
  const action = name => ({ dataset: { gact: name }, closest(sel) { return sel === "[data-gact]" ? this : null; } });
  click(action("bookmark")); assert.equal(ops.length, 1); assert.equal(ops[0].bookmark.role, "assistant"); assert.equal(ops[0].bookmark.t, new Date(0).toISOString()); assert.equal(f.get("guestnote").textContent, "Bookmarked.");
  click(action("bookmark")); assert.equal(ops.length, 1); assert.equal(f.get("guestnote").textContent, "Already bookmarked.");
  click(action("sendto")); assert.equal(f.feature.tab.selected(), "team/s/o/c/g"); assert.deepEqual(sends, [{ text: "two", exceptId: "", title: "Send to…", role: "assistant", who: undefined, prov: undefined, source: null }]);
});

test("shared action keyboard, links and ticks keep focus and the row", async () => {
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer([{ t: "user", seq: 1, text: "x" }], 1), undefined, undefined, { bookmarks: () => [], uiMutate() {}, openSendTo() {} });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  const first = sharedActionBubble(1), missing = sharedActionBubble(99);
  const keydown = f.get("msgs").listeners.get("keydown");
  let prevented = 0;
  keydown({ key: "Enter", target: first.bubble, preventDefault() { prevented++; } });
  assert.equal(prevented, 1); assert.ok(first.turn.nextElementSibling);
  const focus = { focus() { f.doc.activeElement = focus; } }; focus.focus();
  f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.equal(f.doc.activeElement, focus); assert.ok(first.turn.nextElementSibling);
  const link = { closest(sel) { if (sel === "a") return this; if (sel === ".bubble") return first.bubble; return null; } };
  f.click("msgs", link); assert.ok(first.turn.nextElementSibling);
  keydown({ key: " ", target: first.bubble, preventDefault() { prevented++; } });
  assert.equal(prevented, 2); assert.equal(first.turn.nextElementSibling, null);
  f.click("msgs", missing.bubble); assert.equal(missing.turn.nextElementSibling, null);
  keydown({ key: "Enter", target: { closest() { return null; } }, preventDefault() { assert.fail(); } });
});

test("shared bookmark duplicates compare all six address fields", async () => {
  const address = { server: "s", owner: "o", chat: "c", generation: "g", log: "mirror", seq: 1 };
  for (const field of Object.keys(address)) {
    const stored = [{ team: { ...address, [field]: "different" } }], ops = [];
    const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer([{ t: "user", seq: 1, text: "x" }], 1), undefined, undefined, { bookmarks: () => stored, uiMutate: op => ops.push(op), openSendTo() {} });
    f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
    const bubble = sharedActionBubble(1); f.click("msgs", bubble.bubble);
    const action = { dataset: { gact: "bookmark" }, closest(sel) { return sel === "[data-gact]" ? this : null; } };
    f.click("msgs", action); assert.equal(ops.length, 1, field);
    assert.deepEqual(ops[0].bookmark, { t: new Date(0).toISOString(), text: "x", role: "person", who: "Owner", seat: "o", from: "Chat", turnTime: undefined, team: address });
  }
});

test("shared actions ignore invalid targets and capture comments", async () => {
  const sends = [];
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer([{ t: "user", seq: 1, omitted: "too_large" }], 1),
    async () => ({ entries: [{ seq: 2, anchor: 1, text: "message", at: "then", author: "seat", mine: false }], names: { seat: "Ann" }, last: 2, next: 2, more: false }), undefined,
    { bookmarks: () => [], uiMutate() {}, openSendTo: opts => sends.push(opts) });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  const action = { dataset: { gact: "bookmark" }, closest(sel) { return sel === "[data-gact]" ? this : null; } };
  f.click("msgs", action);
  const orphan = { closest(sel) { return sel === ".bubble" ? this : null; } }; f.click("msgs", orphan);
  const omitted = sharedActionBubble(1); f.click("msgs", omitted.bubble); assert.equal(omitted.turn.nextElementSibling, null);
  const message = sharedActionBubble(0); message.turn.dataset = { cseq: "2" }; f.click("msgs", message.bubble);
  action.dataset.gact = "sendto"; f.click("msgs", action);
  assert.deepEqual(sends, [{ text: "message", exceptId: "", title: "Send to…", role: "person", who: "Ann", prov: undefined, source: null }]);
  const keydown = f.get("msgs").listeners.get("keydown");
  keydown({ key: "a" }); keydown({ key: "Enter", target: message.bubble, preventDefault() {} });
});

test("a later shared entry does not split the action row", async () => {
  let reads = 0;
  const f = nativeViewFixture(async path => {
    if (path === "/api/team/subscriptions") return list([base]);
    if (path.endsWith("/seen")) return "";
    return ++reads === 1 ? readAnswer([{ t: "user", seq: 1, text: "first" }, { t: "user", seq: 3, text: "third" }], 3) : readAnswer([{ t: "user", seq: 2, text: "second" }], 3);
  });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  const body = f.get("msgs");
  body.innerHTML = body.innerHTML.replace('<div class="turn assistant" data-seq="3"', '<div class="bubactions" data-gactions><button>bookmark</button></div><div class="turn assistant" data-seq="3"');
  f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.match(body.innerHTML, /data-seq="1"[\s\S]*data-gactions[\s\S]*data-seq="2"[\s\S]*data-seq="3"/);
});

test("shared send keeps focus and ignores unrelated bookmarks", async () => {
  const ops = [], sends = [];
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer([{ t: "user", seq: 1, text: "synthetic" }], 1), undefined, undefined,
    { bookmarks: () => [{ text: "old" }], uiMutate: op => ops.push(op), openSendTo: opts => sends.push(opts) });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  const item = sharedActionBubble(1); f.click("msgs", item.bubble);
  const act = { dataset: { gact: "bookmark" }, closest(sel) { return sel === "[data-gact]" ? this : null; } };
  f.click("msgs", act); assert.equal(ops.length, 1);
  const focus = { focus() { f.doc.activeElement = this; } }; focus.focus();
  act.dataset.gact = "sendto"; f.click("msgs", act);
  assert.equal(f.doc.activeElement, focus); assert.equal(sends.length, 1); assert.equal(sends[0].who, "Owner");
});

/* Wave 6 condition table: condition | each half decides the result.
 * entry.t !== user && entry.t !== assistant | capture: user; assistant and invalid divider.
 * Enter || Space | shared action keyboard: Enter and Space; unrelated key in shared actions ignore.
 * item.team && keys.every | shared send ignores unrelated bookmarks; duplicate fields test matches and differs.
 * comment ? at : time; comment ? comments : mirror | shared capture: comment and mirror.
 * names[author] || Unknown; owner_name || Unknown | shared capture: known and unknown for each.
 * title || Untitled | shared capture: named and empty title.
 * bookmarks who || another person; prov === true ? AI : empty | quoted shared text: named/empty, true/false.
 * bookmark/notes who and from source choices | shared source-line tests: each, both, neither.
 * nt && nt.from || empty; nd ? title : shared | shared snapshot tests: null, local, shared and empty.
 * snapshot && role === person ? speaker : empty | reference sends: null, person, and assistant.
 * snapshot && prov | reference sends: null and person provenance.
 * nt.who || empty in send paths | bookmark pane and notes inbox: named and legacy unnamed.
 */

test("comment author classification holds for guests and owners", () => {
  const message = { seq: 2, text: "x", author: "seat", mine: false };
  for (const access of ["read", "write", "owner"]) {
    assert.equal(guestModule.guestCapture(message, { ...base, access }, { seat: "Ann" }).role, "person");
    assert.equal(guestModule.guestCapture({ ...message, mine: true }, { ...base, access }, {}).role, "user");
  }
});


test("guestName selects local, server and unknown names", () => {
  const guestName = guestModule.guestName;
  assert.equal(typeof guestName, "function");
  assert.equal(guestName("s", "a", "Server", { "s/a": "Local" }), "Local");
  assert.equal(guestName("s", "a", "Server", { "s/a": "" }), "Server");
  assert.equal(guestName("s", "a", "Server", { "s/a": 4 }), "Server");
  assert.equal(guestName("s", "a", "", {}), "Unknown person");
  assert.equal(guestName("s", "a", "Server", undefined), "Server");
});

test("shared labels and capture use local names without renaming own entries", () => {
  const people = { "s/o": "Local owner", "s/a": "Local writer" };
  const prompt = { t: "user", seq: 1, text: "x" }, message = { seq: 2, anchor: 1, text: "y", author: "a", mine: false };
  assert.match(guestModule.guestGroups([base], [], people)[0].label, /Local owner/);
  assert.match(guestModule.guestGroups([{ ...base, access: "owner" }], [], people)[0].label, /^Mine/);
  assert.match(guestModule.guestEntryHTML(prompt, "Owner", "s", "o", people, "read"), /data-seat="o"[^>]*>Local owner/);
  assert.doesNotMatch(guestModule.guestEntryHTML(prompt, "Owner", "s", "o", people, "owner"), /Local owner/);
  assert.match(guestModule.guestMessageHTML(message, { a: "Writer" }, "s", people), /data-seat="a"[^>]*>Local writer/);
  assert.doesNotMatch(guestModule.guestMessageHTML({ ...message, mine: true }, { a: "Writer" }, "s", people), /Local writer/);
  assert.equal(guestModule.guestCapture(prompt, base, {}, people).who, "Local owner");
  assert.equal(guestModule.guestCapture(message, base, { a: "Writer" }, people).who, "Local writer");
  assert.equal(guestModule.guestCapture(prompt, { ...base, access: "owner" }, {}, people).role, "user");
});

test("native shared header and groups use the current local owner name", async () => {
  const people = {"s/o":"Local owner"};
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer(), undefined, undefined, {people:()=>people});
  f.feature.tick(); await f.feature.settled(); assert.match(f.feature.tab.groups()[0].label, /Local owner/);
  f.open(); await f.feature.settled(); assert.equal(f.get("chatmission").textContent, "From Local owner · Server");
});

test("a local name change appears when a new answer arrives", async () => {
  const people = { "s/o": "First" };
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer(), undefined, undefined, { people: () => people });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  assert.equal(f.get("chatmission").textContent, "From First · Server");
  people["s/o"] = "Second";
  assert.equal(f.get("chatmission").textContent, "From First · Server");
  f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.equal(f.get("chatmission").textContent, "From Second · Server");
});

test("shared view receives local people lazily", () => {
  const app = readFileSync(new URL("../js/app.js", import.meta.url), "utf8");
  assert.match(app, /people:\s*\(\)\s*=>\s*getUI\(\)\.people/);
});

test("a read answer invalidates the list when local names changed", async () => {
  const people = { "s/o": "First" };
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer(), undefined, undefined, { people: () => people });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  const before = f.invalidations(); people["s/o"] = "Second";
  f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.equal(f.invalidations(), before + 1);
});

test("native shared header and group retain the owner's identity", async () => {
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer(), undefined, undefined, {people:()=>({"s/o":"Local"})});
  f.feature.tick(); await f.feature.settled(); assert.equal(f.feature.tab.groups()[0].owner, "o");
  f.open(); await f.feature.settled(); assert.equal(f.get("chatmission").dataset.seat, "o");
});

function sharedRenameBubble(seq, fixture) {
  const turn = { dataset: { seq: String(seq) }, nextElementSibling: null };
  const bubble = { closest(sel) { if (sel === ".bubble") return bubble; if (sel === "[data-seq], [data-cseq]" || (sel === "[data-cseq]" && turn.dataset.cseq !== undefined)) return turn; return null; } };
  turn.insertAdjacentHTML = (_where, html) => {
    let inner = html, field = null;
    const renameButton = { focus() { fixture.doc.activeElement = renameButton; } };
    const row = { dataset: { gactions: "" }, remove() { turn.nextElementSibling = null; }, querySelector(sel) {
      if (sel === "[data-grename]") return field;
      if (sel === '[data-gact="rename"]') return renameButton;
      return null;
    } };
    Object.defineProperty(row, "innerHTML", { get() { return inner; }, set(value) {
      inner = value;
      const match = /value="([^"]*)"/.exec(value);
      field = match ? { dataset: { grename: "" }, value: match[1].replaceAll("&quot;", '"').replaceAll("&lt;", "<").replaceAll("&gt;", ">").replaceAll("&amp;", "&"), focus() { fixture.doc.activeElement = this; } } : null;
    } });
    turn.nextElementSibling = row;
  };
  return { turn, bubble };
}
function sharedNameAction(f, name) {
  const node = { dataset: { gact: name }, closest(sel) { return sel === "[data-gact]" ? this : null; } };
  f.click("msgs", node);
}

test("person rename edits local names and updates labels without closing the row", async () => {
  for (const [choice, typed, expected] of [["save", " New <&\" ", "New <&\""], ["save", "", ""], ["save", "Owner", ""], ["use", "ignored", ""]]) {
    const people = { "s/o": "Local" }, ops = [];
    const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer([{ t: "user", seq: 1, text: "x", prov: true }], 1), undefined, undefined, {
      people: () => people, bookmarks: () => [], openSendTo() {}, uiMutate(op) { ops.push(op); if (op.name) people[op.key] = op.name; else delete people[op.key]; },
    });
    const label = { dataset: { seat: "o", prompt: "1", prov: "true" }, textContent: "" };
    f.get("msgs").querySelectorAll = sel => sel === "[data-seat]" ? [label] : [];
    f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
    const bubble = sharedRenameBubble(1, f); f.click("msgs", bubble.bubble);
    assert.match(bubble.turn.nextElementSibling.innerHTML, /data-gact="rename"/);
    sharedNameAction(f, "rename"); const row = bubble.turn.nextElementSibling, field = row.querySelector("[data-grename]");
    assert.ok(field); assert.equal(field.value, "Local"); assert.equal(f.doc.activeElement, field); field.value = typed;
    f.setTime(5000); f.feature.tick(); await f.feature.settled(); assert.equal(row.querySelector("[data-grename]"), field); assert.equal(field.value, typed);
    const before = f.invalidations(); sharedNameAction(f, choice === "save" ? "name-save" : "name-use");
    assert.deepEqual(ops, [{ k: "person-name", key: "s/o", name: expected }]); assert.equal(f.invalidations(), before + 1);
    const shown = expected || "Owner"; assert.equal(f.get("chatmission").textContent, "From " + shown + " · Server");
    assert.equal(label.textContent, shown + " · prompt · includes forwarded AI text");
    assert.equal(f.doc.activeElement, row.querySelector('[data-gact="rename"]'));
  }
});

test("owner comments offer rename only for other people and Escape cancels", async () => {
  const view = { ...base, access: "owner" }, ops = [];
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([view]) : readAnswer([], 0, false, view),
    async () => commentsAnswer([{seq:1,anchor:0,text:"mine",author:"self",mine:true}, {seq:2,anchor:0,text:"other",author:"a",mine:false}], 2, false, {a:"Writer"}), undefined,
    {selectedNode:()=>"n1", people:()=>({}), uiMutate:op=>ops.push(op), bookmarks:()=>[], openSendTo() {}});
  f.feature.tick(); await f.feature.settled();
  const mine = sharedRenameBubble(0, f); mine.turn.dataset = {cseq:"1",gkey:guestKey(view)};
  f.click("msgs", mine.bubble); assert.doesNotMatch(mine.turn.nextElementSibling.innerHTML, /data-gact="rename"/); f.click("msgs", mine.bubble);
  const other = sharedRenameBubble(0, f); other.turn.dataset = {cseq:"2",gkey:guestKey(view)};
  f.click("msgs", other.bubble); sharedNameAction(f, "rename");
  const row=other.turn.nextElementSibling, field=row.querySelector("[data-grename]"); assert.equal(field.value,"Writer");
  let prevented=false, stopped=false;
  f.get("msgs").listeners.get("keydown")({key:"Escape",target:field,preventDefault(){prevented=true},stopPropagation(){stopped=true}});
  assert.equal(prevented,true); assert.equal(stopped,true); assert.deepEqual(ops,[]);
  assert.equal(f.doc.activeElement,row.querySelector('[data-gact="rename"]')); f.feature.destroy();
});


test("comments refresh can introduce a local-name change", async () => {
  const pending = deferred(), people = { "s/o": "First" }; let reads = 0;
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer(),
    async () => ++reads === 1 ? pending.promise : commentsAnswer(), undefined, { people: () => people });
  f.feature.tick(); await f.feature.settled(); f.open();
  await Promise.resolve(); await Promise.resolve();
  const before = f.invalidations(); people["s/o"] = "Second"; pending.resolve(commentsAnswer()); await f.feature.settled();
  assert.equal(f.invalidations(), before + 1); assert.equal(f.get("chatmission").textContent, "From Second · Server");
});

test("incremental comments rename an earlier message in the open view", async () => {
  let reads = 0;
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer(),
    async () => ++reads === 1 ? commentsAnswer([{ seq: 1, anchor: 0, author: "a", text: "synthetic" }], 1, false, { a: "Bob" }) : commentsAnswer([], 1, false, { a: "Bobby" }));
  const label = { dataset: { seat: "a" }, textContent: "Bob" };
  f.get("msgs").querySelectorAll = sel => sel === "[data-seat]" ? [label] : [];
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  assert.match(f.get("msgs").innerHTML, /Bob/);
  f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.equal(label.textContent, "Bobby");
  assert.equal(f.feature.tab.selected(), "team/s/o/c/g");
});

test("rename actions ignore closed, missing and non-person rows", async () => {
  const view = base;
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([view]) : readAnswer([{ t: "assistant", seq: 1, text: "x" }], 1, false, view), undefined, undefined,
    { people: () => ({}), uiMutate() {}, bookmarks: () => [], openSendTo() {} });
  const key = f.get("msgs").listeners.get("keydown");
  const event = { key: "Escape", target: { dataset: { grename: "" } }, preventDefault() {}, stopPropagation() {} };
  key(event);
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled(); key(event);
  const bubble = sharedRenameBubble(1, f); f.click("msgs", bubble.bubble);
  sharedNameAction(f, "rename"); sharedNameAction(f, "name-save"); key(event);
  assert.equal(f.feature.tab.selected(), "team/s/o/c/g");
});

test("comment rename can restore its server name", async () => {
  const people = { "s/a": "Local" }, ops = [];
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer(),
    async () => commentsAnswer([{ seq: 1, anchor: 0, text: "x", author: "a", mine: false }], 1, false, { a: "Writer" }), undefined,
    { people: () => people, uiMutate(op) { ops.push(op); if (op.name) people[op.key] = op.name; else delete people[op.key]; }, bookmarks: () => [], openSendTo() {} });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  const bubble = sharedRenameBubble(0, f); bubble.turn.dataset = { cseq: "1" }; f.click("msgs", bubble.bubble);
  sharedNameAction(f, "rename"); bubble.turn.nextElementSibling.querySelector("[data-grename]").value = "Writer"; sharedNameAction(f, "name-save");
  assert.deepEqual(ops, [{ k: "person-name", key: "s/a", name: "" }]);
});

test("new bookmark and send-to use the renamed person's name", async () => {
  const people = {}, ops = [], sends = [];
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer([{ t: "user", seq: 1, text: "synthetic" }], 1), undefined, undefined, {
    people: () => people, bookmarks: () => [], openSendTo: opts => sends.push(opts),
    uiMutate(op) { ops.push(op); if (op.k === "person-name") people[op.key] = op.name; },
  });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  const bubble = sharedRenameBubble(1, f); f.click("msgs", bubble.bubble); sharedNameAction(f, "rename");
  bubble.turn.nextElementSibling.querySelector("[data-grename]").value = 'New <&"'; sharedNameAction(f, "name-save");
  sharedNameAction(f, "bookmark"); assert.equal(ops[1].bookmark.who, 'New <&"'); assert.equal(ops[1].bookmark.role, "person"); assert.equal(ops[1].bookmark.seat, "o");
  sharedNameAction(f, "sendto"); assert.equal(sends[0].who, 'New <&"'); assert.equal(sends[0].role, "person");
  assert.match(guestModule.guestMessageHTML({ seq: 1, anchor: 0, text: "x", author: "o", mine: false }, { o: "Owner" }, "s", people), /New &lt;&amp;&quot;/);
});

test("a changed local name updates existing message labels on the next answer", async () => {
  const people = { "s/a": "First" };
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer(), async () => commentsAnswer([], 0, false, { a: "Writer" }), undefined, { people: () => people });
  const label = { dataset: { seat: "a" }, textContent: "" };
  f.get("msgs").querySelectorAll = sel => sel === "[data-seat]" ? [label] : [];
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled(); assert.equal(label.textContent, "First");
  people["s/a"] = 'Second <&"'; assert.equal(label.textContent, "First");
  f.setTime(5000); f.feature.tick(); await f.feature.settled(); assert.equal(label.textContent, 'Second <&"');
});

test("non-Escape keys leave the shared name editor open", async () => {
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer([{ t: "user", seq: 1, text: "x" }], 1), undefined, undefined,
    { people: () => ({}), uiMutate() {}, bookmarks: () => [], openSendTo() {} });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  const bubble = sharedRenameBubble(1, f); f.click("msgs", bubble.bubble); sharedNameAction(f, "rename");
  const row = bubble.turn.nextElementSibling, field = row.querySelector("[data-grename]"); field.closest = () => null;
  let prevented = false; f.get("msgs").listeners.get("keydown")({ key: "Enter", target: field, preventDefault() { prevented = true; } });
  assert.equal(prevented, false); assert.equal(row.querySelector("[data-grename]"), field);
});

test("stale local names do not change the empty shared list", async () => {
  const f = nativeViewFixture(async () => list(), undefined, undefined, { people: () => ({ "s/o": "Local" }) });
  f.feature.tick(); await f.feature.settled(); assert.equal(f.invalidations(), 0); assert.equal(f.feature.tab.tabHTML(false), "");
});

test("renaming a person never sends the local name to team routes", async () => {
  const calls = [], people = {};
  const f = nativeViewFixture(async (path, opts) => { calls.push({ path, body: opts?.body || "" }); if (path === "/api/team/subscriptions") return list([base]); return readAnswer([{ t: "user", seq: 1, text: "x" }], 1); }, undefined, undefined,
    { people: () => people, uiMutate(op) { people[op.key] = op.name; }, bookmarks: () => [], openSendTo() {} });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  const bubble = sharedRenameBubble(1, f); f.click("msgs", bubble.bubble); sharedNameAction(f, "rename");
  bubble.turn.nextElementSibling.querySelector("[data-grename]").value = "SyntheticLocalName"; sharedNameAction(f, "name-save");
  f.setTime(10000); f.feature.tick(); await f.feature.settled();
  assert.equal(calls.some(call => (call.path + call.body).includes("SyntheticLocalName")), false);
});



test("native reader: review: a failed later comment page does not discard the first page", async()=>{
 const view={...base,access:'write'};let reads=0; const paths=[];
 const first={seq:1,at:'2026-01-01T00:00:00Z',author:'other',text:'first page message',anchor:0};
 const second={...first,seq:2,text:'second page message'};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),async path=>{
  paths.push(path); reads++; if(reads===1)return {entries:[first],names:{other:'Other'},last:2,next:1,more:true};
  if(reads===2)throw new Error('{"error":"unreachable"}');
  return {entries:[second],names:{other:'Other'},last:2,next:2,more:false};
 });
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
 f.setTime(5000);f.feature.tick();await f.feature.settled();
 assert.match(f.get('msgs').innerHTML,/first page message/);
 assert.match(f.get('msgs').innerHTML,/second page message/);
 assert.match(paths[1], /after=1$/); assert.match(paths[2], /after=1$/, 'retry resumes from the last rendered page');
});







test("phase 1: subscriptions recover after a gate-off response without a hot loop", async () => {
  let calls = 0;
  const f = nativeViewFixture(async () => { calls++; if (calls === 1) throw Object.assign(new Error("off"), {status:404}); return list([base]); });
  f.feature.tick(); await f.feature.settled();
  f.setTime(9999); f.feature.tick(); await f.feature.settled(); assert.equal(calls, 1);
  f.setTime(10000); f.feature.tick(); await f.feature.settled();
  assert.equal(calls, 2); assert.match(f.feature.tab.tabHTML(false), /Shared/);
});

test("phase 1: explicit refresh queues behind an in-flight request and immediately recovers", async () => {
  const pending = deferred(); let calls = 0;
  const f = nativeViewFixture(async () => { calls++; if (calls === 1) return pending.promise; return list([base]); });
  f.feature.tick(); f.feature.refresh(); f.feature.refresh(); assert.equal(calls,1);
  pending.reject(Object.assign(new Error("off"),{status:404})); await f.feature.settled();
  assert.equal(calls,2); assert.match(f.feature.tab.tabHTML(false),/Shared/);
  f.feature.refresh(); await f.feature.settled(); assert.equal(calls,3);
});

test("activity adapter opens only received subscriptions and uses native navigation", async () => {
  const view = {...base, access:"write"}, owner = {...base,chat:"owned",access:"owner"};
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([view,owner]) : readAnswer([],0,false,view));
  f.feature.tick(); await f.feature.settled();
  assert.deepEqual(f.feature.tab.activities().map(x=>x.chat),[view.chat]);
  assert.equal(f.feature.tab.mineUnread(),false);
  assert.equal(f.feature.tab.groups().length,1);
  assert.equal(f.feature.tab.selected(),"");
  f.feature.tab.open("team/no/such/chat/g"); assert.equal(f.feature.tab.selected(),"");
  f.feature.tab.open("team/"+guestKey(owner)); assert.equal(f.feature.tab.selected(),"");
  f.feature.tab.open("team/"+guestKey(view)); await f.feature.settled();
  assert.equal(f.feature.tab.selected(),"team/"+guestKey(view));
  assert.equal(f.get("chattitle").textContent,"Chat");
  f.navigate(); assert.equal(f.feature.tab.selected(),"");
});

test("shared list omits owner copies even when a received item exists", async () => {
 const owned = {...base,access:"owner",chat:"owner-only",title:"Owner copy"};
 const f=nativeViewFixture(async()=>list([owned,base])); f.feature.tick(); await f.feature.settled();
 assert.deepEqual(f.feature.tab.activities().map(view=>view.chat), ["c"]);
});

test("panel adapter pages mirror and comments without showing overlay or sending a forged comment", async () => {
  const paths = []; const selected = []; const view = {...base, access:"write"};
  const panel = {ids:{}, loaded(){}, select:id=>selected.push(id), removed(){}};
  const f = nativeViewFixture(async (path, opts) => {
    paths.push([path,opts?.method || "GET"]);
    if (path === "/api/team/subscriptions") return list([view]);
    if (path.endsWith("after=0")) return readAnswer([{t:"assistant",seq:1,text:"answer"}],1,true,view);
    if (path.endsWith("after=1")) return readAnswer([{t:"user",seq:2,text:"question"}],2,false,view);
    return {};
  }, async () => ({entries:[{seq:1,anchor:1,text:"reply",author:"p",at:"T"}],names:{p:"Person"},next:1,more:false}), undefined, {panel});
  f.feature.tick(); await f.feature.settled();
  f.feature.tab.open("team/s/o/c/g"); assert.deepEqual(selected,["team/s/o/c/g"]);
  f.feature.selectActivity("team/s/o/c/g"); await f.feature.settled();
  assert.equal(f.doc.querySelector("#sharedview"),null);
  assert.match(f.get("msgs").innerHTML,/answer/);
  assert.match(f.get("msgs").innerHTML,/question/);
  assert.match(f.get("msgs").innerHTML,/reply/);
  assert.match(f.get("msgs").innerHTML,/Person/);
  assert.equal(f.doc.querySelector("#sv_send"), null); await f.feature.settled();
  assert.equal(paths.filter(([path,method])=>path.endsWith("/comments") && method === "POST").length,0);
  f.feature.destroy();
});

test("panel send-to keeps the received conversation selected", async () => {
  const sends=[]; const panel={ids:{},loaded(){},select(){},removed(){}};
  const f=nativeViewFixture(async path=>path==="/api/team/subscriptions" ? list([base]) : readAnswer([{t:"assistant",seq:1,text:"answer"}],1), undefined, undefined, {panel,openSendTo: value=>sends.push(value)});
  f.feature.tick(); await f.feature.settled(); f.feature.selectActivity("team/s/o/c/g"); await f.feature.settled();
  const bubble=sharedActionBubble(1); f.click("msgs",bubble.bubble);
  const action={dataset:{gact:"sendto"},closest:selector=>selector==="[data-gact]" ? action : null};
  f.click("msgs",action);
  assert.equal(sends.length,1); assert.equal(f.feature.tab.selected(),"team/s/o/c/g");
  f.feature.destroy();
});

test("panel People send posts bounded text with a stable retry key and no controller dispatch", async () => {
  const calls=[]; let fail=true; const store=new Map();
  const storage={getItem:k=>store.get(k)||null,setItem:(k,v)=>store.set(k,v),removeItem:k=>store.delete(k)};
  const view={...base,access:"write"};
  const panel={ids:{},loaded(){},select(){},removed(){},changed(){}};
  const f=nativeViewFixture(async(path,opts)=>{
    if(path==="/api/team/subscriptions") return list([view]);
    if(path.endsWith("/comments") && opts?.method==="POST") { calls.push(JSON.parse(opts.body)); if(fail) throw Error("offline"); return {seq:1}; }
    return readAnswer([],3,false,view);
  },undefined,()=>new Uint8Array([1,2,3,4,5,6,7,8]),{panel,storage});
  f.feature.tick(); await f.feature.settled(); f.feature.selectActivity("team/s/o/c/g"); await f.feature.settled();
  assert.equal(f.feature.peopleContext().writable,true);
  await assert.rejects(f.feature.sendPeople("s/o/c/g","hello"),/offline/);
  fail=false; await f.feature.sendPeople("s/o/c/g","hello");
  assert.deepEqual(calls,[{text:"hello",anchor:3,key:"0102030405060708"},{text:"hello",anchor:3,key:"0102030405060708"}]);
  await assert.rejects(f.feature.sendPeople("wrong","hello"));
  f.feature.destroy();
});

test("native People send enforces the UTF-8 byte boundary before posting", async () => {
  const view = {...base, access:"write"}, posts = [];
  const f = nativeViewFixture(async (path, opts) => {
    if (path === "/api/team/subscriptions") return list([view]);
    if (opts?.method === "POST") { posts.push(JSON.parse(opts.body)); return {seq:1}; }
    return readAnswer([], 0, false, view);
  });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  await assert.rejects(f.feature.sendPeople("s/o/c/g", " \n "), /unavailable/);
  await assert.rejects(f.feature.sendPeople("s/o/c/g", "a".repeat(16383) + "é"), /unavailable/);
  assert.equal(posts.length, 0);
  const allowed = "a".repeat(16382) + "é";
  await f.feature.sendPeople("s/o/c/g", allowed);
  assert.equal(posts.length, 1); assert.equal(posts[0].text, allowed); f.feature.destroy();
});

test("owned comments bind only to stable local record addresses, including duplicate text and seams", () => {
  const turns=[{uid:"u",segment:0,record:4,text:"same",time:"T"},{uid:"u",segment:1,record:9,text:"same",time:"T"}];
  const mirrors=[{seq:1,local_segment:0,local_record:4},{seq:2,local_segment:1,local_record:9}];
  const comments=[{seq:1,anchor:1,text:"first"},{seq:2,anchor:2,text:"second"}];
  assert.deepEqual(guestModule.ownerCommentAnchors("u",turns,mirrors,comments),[{turn:0,comment:comments[0]},{turn:1,comment:comments[1]}]);
  assert.deepEqual(guestModule.ownerCommentAnchors("other",turns,mirrors,comments),[]);
  assert.deepEqual(guestModule.ownerCommentAnchors("u",turns,[],comments),[]);
});

test("owner subscription is selected only by verified share identity and retains local anchors", async () => {
  const owned={...base,access:"owner"}; const changes=[];
  const panel={ids:{},loaded(){},select(){},removed(){},changed(){},ownerChanged(){changes.push(true)}};
  const f=nativeViewFixture(async path=>path==="/api/team/subscriptions"?list([owned]):readAnswer([{t:"assistant",seq:1,local_segment:0,local_record:4,text:"mirror"}],1,false,owned),async()=>({entries:[{seq:1,anchor:1,text:"reply",author:"p",at:"T"}],names:{p:"Person"},next:1,more:false}),undefined,{panel,selectedNode:()=>"n1"});
  f.feature.tick(); await f.feature.settled();
  f.feature.setOwnerShare("n1",{node:"n1",uid:"c",server:"s",generation:"g",state:"active"}); await f.feature.settled();
  const data=f.feature.ownerSnapshot();
  assert.equal(data.uid,"c"); assert.equal(data.comments[0].text,"reply"); assert.equal(data.mirrors[0].local_record,4);
  assert.equal(f.feature.peopleContext().owner,true); assert.ok(changes.length);
  f.feature.setOwnerShare("n1",{node:"n1",uid:"c",server:"s",generation:"wrong",state:"active"});
  assert.equal(f.feature.ownerSnapshot(),null);
  f.feature.destroy();
});

test("owner comment bubble keeps author identity in the normal-panel Send-to action", async () => {
  const view={...base,access:'owner'}, sends=[];
  const panel={ids:{},loaded(){},select(){},removed(){},changed(){},ownerChanged(){}};
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([{t:'assistant',seq:1,local_segment:0,local_record:4,text:'answer'}],1,false,view),async()=>({entries:[{seq:1,anchor:1,text:'reply',author:'p',at:'T'}],names:{p:'Person'},next:1,more:false}),undefined,{panel,selectedNode:()=>"n1",openSendTo: opts=>sends.push(opts)});
  f.feature.tick(); await f.feature.settled(); f.feature.setOwnerShare('n1',{node:'n1',uid:'c',server:'s',generation:'g',state:'active'}); await f.feature.settled();
  const turn={dataset:{cseq:'1',gkey:'s/o/c/g'},nextElementSibling:null};
  const bubble={closest:selector=>selector==='.bubble'?bubble:(selector==='[data-seq], [data-cseq]' || selector==='[data-cseq]')?turn:null};
  turn.insertAdjacentHTML=()=>{turn.nextElementSibling={remove(){}}};
  f.click('msgs',bubble);
  const action={dataset:{gact:'sendto'},closest:selector=>selector==='[data-gact]'?action:null}; f.click('msgs',action);
  assert.equal(sends.length,1); assert.equal(sends[0].who,'Person'); assert.equal(sends[0].role,'person');
  f.feature.destroy();
});

test("People comment without a confirmed sequence remains retryable", async () => {
  const store=new Map(), calls=[]; let confirmed=false;
  const storage={getItem:k=>store.get(k)||null,setItem:(k,v)=>store.set(k,v),removeItem:k=>store.delete(k)};
  const view={...base,access:'write'}, panel={ids:{},loaded(){},select(){},removed(){},changed(){}};
  const f=nativeViewFixture(async(path,opts)=>{
    if(path==='/api/team/subscriptions') return list([view]);
    if(path.endsWith('/comments') && opts?.method==='POST'){ calls.push(JSON.parse(opts.body)); return confirmed?{seq:1}:{}; }
    return readAnswer([],1,false,view);
  },undefined,()=>new Uint8Array([1,2,3,4,5,6,7,8]),{panel,storage});
  f.feature.tick(); await f.feature.settled(); f.feature.selectActivity('team/s/o/c/g'); await f.feature.settled();
  await assert.rejects(f.feature.sendPeople('s/o/c/g','text')); confirmed=true; await f.feature.sendPeople('s/o/c/g','text');
  assert.equal(calls.length,2); assert.deepEqual(calls[0],calls[1]); f.feature.destroy();
});

test("owner start anchor uses the verified share cursor including zero anchor and clear seam", () => {
  const turns=[{uid:'u',segment:0,record:2},{uid:'u',segment:1,record:8}];
  const mirrors=[{seq:1,t:'start',local_segment:0,local_record:1},{seq:2,t:'source',local_segment:0,local_record:6}];
  const comments=[{seq:1,anchor:0,text:'early'},{seq:2,anchor:2,text:'after clear'}];
  assert.deepEqual(guestModule.ownerCommentAnchors('u',turns,mirrors,comments),[{turn:-1,comment:comments[0]},{turn:-1,comment:comments[1]}]);
});

test("unchanged owner polls do not rebuild the local timeline", async () => {
  const view={...base,access:'owner',seen_comments:0}; let renders=0, after=0;
  const panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){renders++}};
  const f=nativeViewFixture(async path=>{
    if(path==='/api/team/subscriptions') return list([view]);
    if(path.includes('?after=')){ after++; return readAnswer([],0,false,view); }
    return {};
  },async()=>({entries:[],names:{},next:0,more:false}),undefined,{panel,selectedNode:()=>"n1"});
  f.feature.tick(); await f.feature.settled(); f.feature.setOwnerShare('n1',{node:'n1',uid:'c',server:'s',generation:'g',state:'active'}); await f.feature.settled();
  const first=renders; f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.ok(after>=2); assert.equal(renders,first); f.feature.destroy();
});

test('owner reads retained and current generations independently after restart', async () => {
  const old={...base,access:'owner',state:'ended',generation:'old',local_node:'n1'}, live={...base,access:'owner',generation:'new',local_node:'n1'};
  const panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){}};
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([old,live]):readAnswer([{t:'assistant',seq:1,local_segment:0,local_record:4,text:'mirror'}],1,false,path.includes('/old?')?old:live),async path=>({entries:[{seq:1,anchor:1,text:path.includes('/old/')?'old reply':'new reply',author:'p',at:'T'}],names:{p:'Person'},next:1,more:false}),undefined,{panel,selectedNode:()=>"n1"});
  f.feature.tick(); await f.feature.settled(); f.feature.setOwnerShare('n1',{node:'n1',uid:'c',server:'s',generation:'new',state:'active'}); await f.feature.settled();
  assert.deepEqual(f.feature.ownerSnapshots().map(x=>x.comments[0].text).sort(),['new reply','old reply']);
  assert.equal(f.feature.peopleContext().key,'s/o/c/new');
  f.feature.setOwnerShare('n1',null); await f.feature.settled();
  assert.equal(f.feature.ownerSnapshots().length,2); assert.equal(f.feature.peopleContext(),null); f.feature.destroy();
});

test('empty current segment places comments at an explicitly identified clear seam', () => {
  const mirrors=[{seq:1,t:'source',local_segment:0,local_record:6}];
  const comments=[{seq:1,anchor:1,text:'after clear'}];
  assert.deepEqual(guestModule.ownerCommentAnchors('u',[],mirrors,comments,{uid:'u',segment:1}),[{turn:-1,comment:comments[0]}]);
  assert.deepEqual(guestModule.ownerCommentAnchors('u',[],mirrors,comments,{uid:'u',segment:2}),[]);
});

test('owner mirror and comments paging never duplicate entries across polls', async () => {
 const view={...base,access:'owner',seen_comments:0}, panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){}};
 const f=nativeViewFixture(async path=>{
  if(path==='/api/team/subscriptions') return list([view]);
  if(path.endsWith('after=0')) return readAnswer([{seq:1,t:'assistant',local_segment:0,local_record:1,text:'one'}],1,true,view);
  if(path.endsWith('after=1')) return readAnswer([{seq:2,t:'assistant',local_segment:0,local_record:2,text:'two'}],2,false,view);
  return readAnswer([],2,false,view);
 },async path=>path.endsWith('after=0')?{entries:[{seq:1,anchor:1,text:'first',author:'p'}],names:{p:'Person'},next:1,more:true}:path.endsWith('after=1')?{entries:[{seq:2,anchor:2,text:'second',author:'p'}],names:{},next:2,more:false}:{entries:[],names:{},next:2,more:false},undefined,{panel,selectedNode:()=>"n1"});
 f.feature.tick(); await f.feature.settled(); f.feature.setOwnerShare('n1',{node:'n1',uid:'c',server:'s',generation:'g',state:'active'}); await f.feature.settled();
 f.setTime(5000); f.feature.tick(); await f.feature.settled();
 const snapshot=f.feature.ownerSnapshots()[0]; assert.deepEqual(snapshot.mirrors.map(e=>e.seq),[1,2]); assert.deepEqual(snapshot.comments.map(e=>e.seq),[1,2]); f.feature.destroy();
});

test('stale seen counters in unchanged owner polls do not repaint discussion', async () => {
 const view={...base,access:'owner',seen_comments:0}; let renders=0;
 const panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){renders++}};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):path.endsWith('/seen')?{}:readAnswer([],0,false,view),async path=>path.endsWith('after=0')?{entries:[{seq:1,anchor:0,text:'hello',author:'p'}],names:{p:'Person'},next:1,more:false}:{entries:[],names:{p:'Person'},next:1,more:false},undefined,{panel,selectedNode:()=>"n1"});
 f.feature.tick(); await f.feature.settled(); f.feature.setOwnerShare('n1',{node:'n1',uid:'c',server:'s',generation:'g',state:'active'}); await f.feature.settled();
 const before=renders; f.setTime(5000); f.feature.tick(); await f.feature.settled(); assert.equal(renders,before); f.feature.destroy();
});

test('late owner mirror and comment pages cannot repaint a different local chat', async () => {
 for (const phase of ['mirror','comments']) {
  const pending=deferred(), view={...base,access:'owner',local_node:'n1'}, renders=[]; let selected='n1', reached=false;
  const panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){renders.push(selected)}};
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):phase==='mirror'?(reached=true,pending.promise):readAnswer([],0,false,view),async()=>phase==='comments'?(reached=true,pending.promise):{entries:[],names:{},next:0,more:false},undefined,{panel,selectedNode:()=>selected,selectedUID:()=>selected==='n1'?'c':''});
  f.feature.tick(); for(let i=0;i<20 && !reached;i++) await Promise.resolve(); assert.equal(reached,true);
  selected='n2'; f.feature.setOwnerShare('n2',null); const before=renders.length;
  pending.resolve(phase==='mirror'?readAnswer([{seq:1,t:'assistant',text:'old'}],1,false,view):{entries:[{seq:1,anchor:0,text:'old',author:'p'}],names:{p:'Person'},next:1,more:false});
  await f.feature.settled(); assert.equal(renders.length,before); assert.deepEqual(f.feature.ownerSnapshots(),[]); f.feature.destroy();
 }
});

test('owner permission loss prevents a later comment post', async () => {
 let ended=false, posts=0; const view={...base,access:'owner'}, panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){}};
 const f=nativeViewFixture(async(path,opts)=>{
  if(path==='/api/team/subscriptions') return list([view]);
  if(path.endsWith('/comments') && opts?.method==='POST'){posts++; return {seq:1};}
  return readAnswer([],0,false,ended?{...view,state:'ended'}:view);
 },async()=>({entries:[],names:{},next:0,more:false}),undefined,{panel,selectedNode:()=>"n1"});
 f.feature.tick(); await f.feature.settled(); f.feature.setOwnerShare('n1',{node:'n1',uid:'c',server:'s',generation:'g',state:'active'}); await f.feature.settled();
 assert.equal(f.feature.peopleContext().writable,true); ended=true; f.setTime(5000); f.feature.tick(); await f.feature.settled();
 assert.equal(f.feature.peopleContext().writable,false); await assert.rejects(f.feature.sendPeople('s/o/c/g','forged')); assert.equal(posts,0); f.feature.destroy();
});

test('retained generations order by persisted owner subscription order, not names or timestamps', async () => {
 const old={...base,access:'owner',state:'ended',generation:'zz',local_node:'n1',local_order:1,first_seen:'same'};
 const recent={...base,access:'owner',state:'ended',generation:'aa',local_node:'n1',local_order:2,first_seen:'same'};
 const panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){}};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([recent,old]):readAnswer([],0,false,path.includes('/zz?')?old:recent),undefined,undefined,{panel,selectedNode:()=>"n1"});
 f.feature.tick(); await f.feature.settled(); assert.deepEqual(f.feature.ownerSnapshots().map(s=>s.view.generation),['zz','aa']); f.feature.destroy();
});

test('owner comment anchored at zero uses durable frontier before mirror start arrives', async () => {
 const view={...base,access:'owner',local_node:'n1',local_frontier:{node:'n1',segment:0,record:1}}, panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){}};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),async()=>({entries:[{seq:1,anchor:0,text:'early',author:'p'}],names:{p:'Person'},next:1,more:false}),undefined,{panel,selectedNode:()=>"n1"});
 f.feature.tick(); await f.feature.settled(); const data=f.feature.ownerSnapshots()[0];
 assert.deepEqual(guestModule.ownerCommentAnchors('c',[{uid:'c',segment:0,record:1},{uid:'c',segment:0,record:2}],data.mirrors,data.comments),[{turn:0,comment:data.comments[0]}]); f.feature.destroy();
});

test('panel subscription removal calls the panel callback and clears its saved selection', async () => {
 const removed=[]; const panel={ids:{},loaded(){},select(){},removed(){removed.push(true)}};
 const f=nativeViewFixture(async(path,opts)=>path==='/api/team/subscriptions'?list([base]):opts?.method==='DELETE'?{}:readAnswer(),undefined,undefined,{panel});
 f.feature.tick(); await f.feature.settled(); f.feature.selectActivity('team/s/o/c/g'); await f.feature.settled();
 f.click('guestremove'); await f.feature.settled(); assert.deepEqual(removed,[true]); assert.equal(f.feature.tab.selected(),''); f.feature.destroy();
});

test('panel selection ignores a repeated id and closes when navigating to a local chat', async () => {
 const panel={ids:{},loaded(){},select(){},removed(){}};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer(),undefined,undefined,{panel});
 f.feature.tick(); await f.feature.settled(); f.feature.selectActivity('team/s/o/c/g'); await f.feature.settled();
 f.feature.selectActivity('team/s/o/c/g'); assert.equal(f.feature.tab.selected(),'team/s/o/c/g');
 f.feature.selectActivity(''); assert.equal(f.feature.tab.selected(),''); f.feature.destroy();
});

test('comment retries replace damaged retry metadata with a fresh key', async () => {
 const data=new Map([['scimux-comment-retry:s/o/c/g','{']]); const storage={getItem:k=>data.get(k)||null,setItem:(k,v)=>data.set(k,v),removeItem:k=>data.delete(k)};
 const view={...base,access:'write'}, panel={ids:{},loaded(){},select(){},removed(){}}; let posted;
 const f=nativeViewFixture(async(path,opts)=>{if(path==='/api/team/subscriptions')return list([view]); if(path.endsWith('/comments') && opts?.method==='POST'){posted=JSON.parse(opts.body);return {seq:1}} return readAnswer([],0,false,view)},undefined,()=>new Uint8Array([1,2,3,4,5,6,7,8]),{panel,storage});
 f.feature.tick(); await f.feature.settled(); f.feature.selectActivity('team/s/o/c/g'); await f.feature.settled(); await f.feature.sendPeople('s/o/c/g','hello');
 assert.equal(posted.key,'0102030405060708'); f.feature.destroy();
});


test('owner reader retries transient failures without painting an error as a comment', async () => {
 const view={...base,access:'owner',local_node:'n1'}, panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){}}; let attempts=0;
 const f=nativeViewFixture(async path=>{if(path==='/api/team/subscriptions')return list([view]); attempts++; if(attempts===1) throw Error('offline'); return readAnswer([{seq:1,t:'assistant',local_segment:0,local_record:1,text:'answer'}],1,false,view)},undefined,undefined,{panel,selectedNode:()=>"n1"});
 f.feature.tick(); await f.feature.settled(); assert.equal(f.feature.ownerSnapshots()[0].mirrors.length,0);
 f.setTime(5000); f.feature.tick(); await f.feature.settled(); assert.equal(f.feature.ownerSnapshots()[0].mirrors.length,1); f.feature.destroy();
});

test('People send without a selected subscription is rejected before any request', async () => {
 let calls=0; const panel={ids:{},loaded(){},select(){},removed(){}};
 const f=nativeViewFixture(async()=>{calls++;return list()},undefined,undefined,{panel});
 await assert.rejects(f.feature.sendPeople('s/o/c/g','forged'),/unavailable/); assert.equal(calls,0); f.feature.destroy();
});

test('owner anchors preserve record zero as a valid identity', () => {
 const turns=[{uid:'u',segment:0,record:0}], mirror=[{seq:1,t:'user',local_segment:0,local_record:0}], comments=[{seq:1,anchor:1,text:'reply'}];
 assert.deepEqual(guestModule.ownerCommentAnchors('u',turns,mirror,comments),[{turn:0,comment:comments[0]}]);
});

test('a forged owner comment generation cannot select a different retained copy', async () => {
 const view={...base,access:'owner',local_node:'n1'}, panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){}};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),async()=>({entries:[{seq:1,anchor:0,text:'reply',author:'p'}],names:{p:'Person'},next:1,more:false}),undefined,{panel,selectedNode:()=>"n1"});
 f.feature.tick(); await f.feature.settled(); f.feature.setOwnerShare('n1',{node:'n1',uid:'c',server:'s',generation:'g',state:'active'}); await f.feature.settled();
 let opened=false; const turn={dataset:{cseq:'1',gkey:'forged'},insertAdjacentHTML(){opened=true}};
 const bubble={closest:selector=>selector==='.bubble'?bubble:(selector==='[data-cseq]'||selector==='[data-seq], [data-cseq]')?turn:null};
 f.click('msgs',bubble); assert.equal(opened,false); f.feature.destroy();
});

test('normal panel keyboard opens a received bubble action without treating Escape as back', async () => {
 const panel={ids:{},loaded(){},select(){},removed(){}};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([{t:'assistant',seq:1,text:'answer'}],1),undefined,undefined,{panel});
 f.feature.tick(); await f.feature.settled(); f.feature.selectActivity('team/s/o/c/g'); await f.feature.settled();
 const {bubble,turn}=sharedActionBubble(1); const handler=f.get('msgs').listeners.get('keydown');
 handler({key:'Enter',target:bubble,preventDefault(){},stopPropagation(){}}); assert.match(turn.rowHTML,/data-gactions/);
 handler({key:'Escape',target:bubble,preventDefault(){},stopPropagation(){}}); assert.equal(f.feature.tab.selected(),'team/s/o/c/g'); f.feature.destroy();
});

test('normal panel Escape restores the rename action without leaving the conversation', async () => {
 const panel={ids:{},loaded(){},select(){},removed(){}};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([{t:'user',seq:1,text:'words'}],1),undefined,undefined,{panel});
 f.feature.tick(); await f.feature.settled(); f.feature.selectActivity('team/s/o/c/g'); await f.feature.settled();
 const item=sharedRenameBubble(1,f); f.click('msgs',item.bubble);
 const action={dataset:{gact:'rename'},closest:s=>s==='[data-gact]'?action:null}; f.click('msgs',action);
 const field=item.turn.nextElementSibling.querySelector('[data-grename]'); assert.ok(field);
 f.get('msgs').listeners.get('keydown')({key:'Escape',target:field,preventDefault(){},stopPropagation(){}});
 assert.equal(f.feature.tab.selected(),'team/s/o/c/g'); assert.match(item.turn.nextElementSibling.innerHTML,/data-gact="rename"/); f.feature.destroy();
});

test('forged keyboard activation for an unknown owner generation cannot open an action', async () => {
 const view={...base,access:'owner',local_node:'n1'}, panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){}};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),async()=>({entries:[{seq:1,anchor:0,text:'reply',author:'p'}],names:{p:'Person'},next:1,more:false}),undefined,{panel,selectedNode:()=>"n1"});
 f.feature.tick(); await f.feature.settled(); f.feature.setOwnerShare('n1',{node:'n1',uid:'c',server:'s',generation:'g',state:'active'}); await f.feature.settled();
 let opened=false; const turn={dataset:{cseq:'1',gkey:'forged'},insertAdjacentHTML(){opened=true}};
 const bubble={closest:selector=>selector==='.bubble'?bubble:selector==='[data-seq], [data-cseq]'?turn:null};
 f.get('msgs').listeners.get('keydown')({key:'Enter',target:bubble,preventDefault(){},stopPropagation(){}}); assert.equal(opened,false); f.feature.destroy();
});

test('sending an owner comment while its reader is busy does not launch a second read', async () => {
 const view={...base,access:'owner',local_node:'n1'}, panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){}}; const pending=deferred(); let reads=0,posts=0;
 const f=nativeViewFixture(async(path,opts)=>{if(path==='/api/team/subscriptions')return list([view]);if(path.endsWith('/comments') && opts?.method==='POST'){posts++;return {seq:1}}reads++;return pending.promise},undefined,()=>new Uint8Array(8),{panel,selectedNode:()=>"n1"});
 f.feature.tick(); for(let i=0;i<20 && reads===0;i++)await Promise.resolve(); f.feature.setOwnerShare('n1',{node:'n1',uid:'c',server:'s',generation:'g',state:'active'});
 await f.feature.sendPeople('s/o/c/g','hello'); assert.equal(posts,1); assert.equal(reads,1); pending.resolve(readAnswer([],0,false,view)); await f.feature.settled(); f.feature.destroy();
});

test('owner renders retained progress when a later comment page fails', async () => {
 const view={...base,access:'owner',local_node:'n1'}, panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){renders++}}; let renders=0, phase=0;
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):phase===0?readAnswer([],0,false,view):readAnswer([{seq:1,t:'assistant',local_segment:0,local_record:1,text:'answer'}],1,false,view),async()=>{if(phase)throw Error('offline');return {entries:[{seq:1,anchor:1,text:'reply',author:'p'}],names:{p:'Person'},next:1,more:false}},undefined,{panel,selectedNode:()=>"n1"});
 f.feature.tick(); await f.feature.settled(); const before=renders; phase=1; f.setTime(5000); f.feature.tick(); await f.feature.settled();
 assert.ok(renders>before); assert.equal(f.feature.ownerSnapshots()[0].mirrors.length,1); f.feature.destroy();
});

test('legacy retained owner copies without frontier metadata match only the current log UID', async () => {
 const view={...base,access:'owner',state:'ended'}, panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){}};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([{seq:1,t:'assistant',local_segment:0,local_record:4,text:'answer'}],1,false,view),async()=>({entries:[{seq:1,anchor:1,text:'retained',author:'p'}],names:{p:'Person'},next:1,more:false}),undefined,{panel,selectedNode:()=>"n1",selectedUID:()=>"c"});
 f.feature.tick();await f.feature.settled();assert.equal(f.feature.ownerSnapshots().length,1);assert.equal(f.feature.ownerSnapshots()[0].comments[0].text,'retained');f.feature.destroy();
});

test('legacy comments lacking a durable frontier are identified as unplaced', () => {
 const mirrors=[{seq:1,t:'start'},{seq:2,t:'assistant',local_segment:0,local_record:5}];
 const comments=[{seq:1,anchor:0,text:'zero'},{seq:2,anchor:1,text:'start'},{seq:3,anchor:2,text:'located'}];
 assert.deepEqual(guestModule.ownerUnplacedComments(mirrors,comments),comments.slice(0,2));
});

test('owner prompt capability requires current UID and exact retained copy identity', async () => {
 const view={...base,access:'owner',local_node:'n1'}; let uid='c';
 const panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){}};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),async()=>({entries:[{seq:1,anchor:0,text:'reply',author:'p'}],names:{p:'Person'},next:1,more:false}),undefined,{panel,selectedNode:()=>"n1",selectedUID:()=>uid});
 f.feature.tick(); await f.feature.settled();
 const local={uid:'c',segment:0,record:1}; const shared={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:1};
 assert.equal(f.feature.canPreparePrompt(local),true); assert.equal(f.feature.canPreparePrompt(shared),true);
 assert.equal(f.feature.canPreparePrompt({...shared,generation:'other'}),false);
 assert.equal(f.feature.canPreparePrompt({...shared,seq:2}),false);
 uid='other'; assert.equal(f.feature.canPreparePrompt(local),false); assert.equal(f.feature.canPreparePrompt(shared),false);
 f.feature.destroy();
});

test('owner comment prompt action prepares attributed source and recipient forgery cannot prepare', async () => {
 const owner={...base,access:'owner',local_node:'n1'}; const prepared=[];
 const panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){},promptIcon:'I',preparePrompt:(...args)=>prepared.push(args)};
 const comments=async()=>({entries:[{seq:1,anchor:0,text:'reply',author:'p',prov:true}],names:{p:'Alex'},next:1,more:false});
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([owner]):readAnswer([],0,false,owner),comments,undefined,{panel,selectedNode:()=>"n1"});
 f.feature.tick(); await f.feature.settled();
 const turn={dataset:{cseq:'1',gkey:'s/o/c/g'},nextElementSibling:null,insertAdjacentHTML(_where,html){this.rowHTML=html; this.nextElementSibling={remove(){}}}};
 const bubble={closest:s=>s==='.bubble'?bubble:(s==='[data-cseq]'||s==='[data-seq], [data-cseq]')?turn:null};
 f.click('msgs',bubble); assert.match(turn.rowHTML,/data-gact="prompt"/);
 const action={dataset:{gact:'prompt'},closest:s=>s==='[data-gact]'?action:null}; f.click('msgs',action);
 assert.equal(prepared.length,1); assert.equal(prepared[0][0].who,'Alex'); assert.equal(prepared[0][0].prov,true);
 assert.deepEqual(prepared[0][1],{server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:1});
 f.feature.destroy();
 const g=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([],0),comments,undefined,{panel});
 g.feature.tick(); await g.feature.settled(); g.feature.selectActivity('team/s/o/c/g'); await g.feature.settled();
 const gt={dataset:{cseq:'1'},nextElementSibling:null,insertAdjacentHTML(_where,html){this.rowHTML=html;this.nextElementSibling={remove(){}}}};
 const gb={closest:s=>s==='.bubble'?gb:s==='[data-seq], [data-cseq]'?gt:null};
 g.click('msgs',gb); assert.doesNotMatch(gt.rowHTML,/data-gact="prompt"/); g.click('msgs',action); assert.equal(prepared.length,1); g.feature.destroy();
});

test('owner prompt capability requires a selected local chat and known UID', () => {
 const f=nativeViewFixture(async()=>list(),undefined,undefined,{selectedNode:()=>'',selectedUID:()=>''});
 assert.equal(f.feature.canPreparePrompt({uid:'c',segment:0,record:1}),false); f.feature.destroy();
 const g=nativeViewFixture(async()=>list(),undefined,undefined,{selectedNode:()=>'n1',selectedUID:()=>''});
 assert.equal(g.feature.canPreparePrompt({uid:'c',segment:0,record:1}),false); g.feature.destroy();
});

test('native reader: new received comments keep the active name editor and its scroll anchor', async () => {
 let next=false;
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([{t:'user',seq:1,text:'owner words'}],1),async()=>next?commentsAnswer([{seq:1,anchor:1,text:'new comment',author:'p'}],1,false,{p:'Person'}):commentsAnswer());
 f.feature.tick(); await f.feature.settled(); f.feature.selectActivity('team/s/o/c/g'); await f.feature.settled();
 const item=sharedRenameBubble(1,f), body=f.get('msgs');
 item.turn.getBoundingClientRect=()=>({top:900-body.scrollTop});
 f.click('msgs',item.bubble); const action={dataset:{gact:'rename'},closest:s=>s==='[data-gact]'?action:null}; f.click('msgs',action);
 const field=item.turn.nextElementSibling.querySelector('[data-grename]'); field.value='Editing';
 body.scrollTop=800; next=true; f.setTime(5000); f.feature.tick(); await f.feature.settled();
 assert.equal(body.scrollTop,800); assert.equal(item.turn.nextElementSibling.querySelector('[data-grename]'),field); assert.equal(f.doc.activeElement,field); assert.equal(field.value,'Editing');
 assert.match(body.innerHTML,/new comment/); f.feature.destroy();
});

test('native reader: coordinator user scroll during held received poll is not reversed', async () => {
 let next=false; const held=deferred();
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer(path.endsWith('after=0')?[{t:'user',seq:1,text:'owner words'}]:[],1),async()=>next?held.promise:commentsAnswer());
 f.feature.tick();await f.feature.settled();f.feature.selectActivity('team/s/o/c/g');await f.feature.settled();
 const item=sharedRenameBubble(1,f),body=f.get('msgs');item.turn.getBoundingClientRect=()=>({top:900-body.scrollTop});
 f.click('msgs',item.bubble);const action={dataset:{gact:'rename'},closest:s=>s==='[data-gact]'?action:null};f.click('msgs',action);
 body.scrollTop=800;next=true;f.setTime(5000);f.feature.tick();
 body.scrollTop=600; // user scrolls while the real reader awaits network data
 held.resolve(commentsAnswer([{seq:1,anchor:1,text:'new comment after user scroll',author:'p'}],1,false,{p:'Person'}));
 await f.feature.settled();assert.equal(body.scrollTop,600,'a completed poll must preserve the newer user scroll');f.feature.destroy();
});

test('native reader: received multi-page polling preserves the latest scroll before each insertion', async () => {
 const first=deferred(), second=deferred();let phase=0, reads=0;
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer(path.endsWith('after=0')?[{t:'user',seq:1,text:'owner words'}]:[],1),async()=>{if(!phase)return commentsAnswer();return ++reads===1?first.promise:second.promise});
 f.feature.tick();await f.feature.settled();f.feature.selectActivity('team/s/o/c/g');await f.feature.settled();
 const item=sharedRenameBubble(1,f),body=f.get('msgs');item.turn.getBoundingClientRect=()=>({top:900-body.scrollTop});
 f.click('msgs',item.bubble);const action={dataset:{gact:'rename'},closest:s=>s==='[data-gact]'?action:null};f.click('msgs',action);
 const input=item.turn.nextElementSibling.querySelector('[data-grename]');input.value='still editing';
 body.scrollTop=800;phase=1;f.setTime(5000);f.feature.tick();body.scrollTop=600;
 first.resolve(commentsAnswer([{seq:1,anchor:1,text:'page one',author:'p'}],1,true,{p:'Person'}));
 for(let i=0;i<20 && reads<2;i++)await Promise.resolve();assert.equal(reads,2);assert.equal(body.scrollTop,600);
 body.scrollTop=400;second.resolve(commentsAnswer([{seq:2,anchor:1,text:'page two',author:'p'}],2,false,{p:'Person'}));
 await f.feature.settled();assert.equal(body.scrollTop,400);assert.equal(f.doc.activeElement,input);assert.equal(input.value,'still editing');assert.match(body.innerHTML,/page one/);assert.match(body.innerHTML,/page two/);f.feature.destroy();
});

test('native reader: received editor opened while a poll waits keeps its current position', async () => {
 const gate=deferred();let waiting=false;
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer(path.endsWith('after=0')?[{t:'user',seq:1,text:'owner words'}]:[],1),async()=>waiting?gate.promise:commentsAnswer());
 f.feature.tick();await f.feature.settled();f.feature.selectActivity('team/s/o/c/g');await f.feature.settled();
 waiting=true;f.setTime(5000);f.feature.tick();const item=sharedRenameBubble(1,f),body=f.get('msgs');item.turn.getBoundingClientRect=()=>({top:900-body.scrollTop});
 f.click('msgs',item.bubble);const action={dataset:{gact:'rename'},closest:s=>s==='[data-gact]'?action:null};f.click('msgs',action);body.scrollTop=600;
 gate.resolve(commentsAnswer([{seq:1,anchor:1,text:'arrived',author:'p'}],1,false,{p:'Person'}));await f.feature.settled();
 assert.equal(body.scrollTop,600);assert.equal(f.doc.activeElement,item.turn.nextElementSibling.querySelector('[data-grename]'));assert.match(body.innerHTML,/arrived/);f.feature.destroy();
});

test('native reader: opening an editor during initial multi-page load keeps its position', async () => {
 const gate=deferred();let requested=false;
 const f=nativeViewFixture(async path=>{if(path==='/api/team/subscriptions')return list([base]);if(path.endsWith('after=0'))return readAnswer([{t:'user',seq:1,text:'owner words'}],1,true);requested=true;return gate.promise},undefined);
 f.feature.tick();await f.feature.settled();f.feature.selectActivity('team/s/o/c/g');
 for(let i=0;i<30&&!requested;i++)await Promise.resolve();assert.ok(requested);
 const item=sharedRenameBubble(1,f),body=f.get('msgs');item.turn.getBoundingClientRect=()=>({top:900-body.scrollTop});
 f.click('msgs',item.bubble);const action={dataset:{gact:'rename'},closest:s=>s==='[data-gact]'?action:null};f.click('msgs',action);body.scrollTop=600;
 const input=item.turn.nextElementSibling.querySelector('[data-grename]');gate.resolve(readAnswer([{t:'assistant',seq:2,text:'later page'}],2,false));await f.feature.settled();
 assert.equal(body.scrollTop,600);assert.equal(f.doc.activeElement,input);assert.match(body.innerHTML,/later page/);f.feature.destroy();
});

test('native reader: canceling a received editor during a poll restores normal bottom following', async () => {
 const gate=deferred();let waiting=false;
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer(path.endsWith('after=0')?[{t:'user',seq:1,text:'owner words'}]:[],1),async()=>waiting?gate.promise:commentsAnswer());
 f.feature.tick();await f.feature.settled();f.feature.selectActivity('team/s/o/c/g');await f.feature.settled();
 const item=sharedRenameBubble(1,f),body=f.get('msgs');f.click('msgs',item.bubble);const action={dataset:{gact:'rename'},closest:s=>s==='[data-gact]'?action:null};f.click('msgs',action);
 const input=item.turn.nextElementSibling.querySelector('[data-grename]');body.scrollTop=800;waiting=true;f.setTime(5000);f.feature.tick();
 body.listeners.get('keydown')({key:'Escape',target:input,preventDefault(){},stopPropagation(){}});
 gate.resolve(commentsAnswer([{seq:1,anchor:1,text:'arrived',author:'p'}],1,false,{p:'Person'}));await f.feature.settled();
 assert.equal(body.scrollTop,body.scrollHeight);assert.match(body.innerHTML,/arrived/);assert.equal(f.doc.activeElement,item.turn.nextElementSibling.querySelector('[data-gact="rename"]'));f.feature.destroy();
});

test('coordinator selected received reader recovers after gate loss during an in-flight read', async () => {
 let phase=0,reads=0;const held=deferred();
 const f=nativeViewFixture(async path=>{
  if(path==='/api/team/subscriptions'){
   if(phase===1)throw Object.assign(new Error('temporary unavailable'),{status:404});
   return list([base]);
  }
  if(path.endsWith('/seen'))return {};
  reads++;if(phase===1)return held.promise;
  return readAnswer([],0);
 });
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();assert.equal(reads,1);
 phase=1;f.setTime(10000);f.feature.tick();
 for(let step=0;step<12;step++)await Promise.resolve();
 held.resolve(readAnswer([],0));await f.feature.settled();assert.equal(reads,2);
 phase=2;f.setTime(20000);f.feature.tick();await f.feature.settled();
 f.setTime(25000);f.feature.tick();await f.feature.settled();
 assert.equal(f.feature.tab.selected(),'team/s/o/c/g');
 assert.ok(reads>=3,'the selected received reader must resume after availability recovers, without switching chats');
 f.feature.destroy();
});

for (const phase of ['mirror', 'comments', 'seen']) for (const outcome of ['error', 'success']) {
  test(`received recovery reserves the new reader after a stale ${phase} ${outcome}`, async () => {
    const view = {...base, access:'write', unread:true, mirror_last:2, comments_last:1};
    const old = deferred(), newer = deferred(); let gate = false, hold = false, recovered = false;
    let reads = 0, comments = 0, seen = 0; const paths = [], marks = [];
    const f = nativeViewFixture(async (path, opts) => {
      if (path === '/api/team/subscriptions') {
        if (gate) throw Object.assign(new Error('off'), {status:404});
        return list([view]);
      }
      if (path.endsWith('/seen')) {
        seen++; marks.push(JSON.parse(opts.body));
        if (hold && phase === 'seen') return old.promise;
        return {};
      }
      paths.push(path); reads++;
      if (recovered) return newer.promise;
      if (hold && phase === 'mirror') return old.promise;
      return readAnswer(hold && phase === 'seen' ? [{t:'user',seq:2,text:'rendered before gate'}] : [{t:'user',seq:1,text:'initial'}], hold && phase === 'seen' ? 2 : 1, false, view);
    }, async path => {
      comments++;
      if (hold && phase === 'comments') return old.promise;
      return commentsAnswer();
    });
    f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
    assert.equal(f.feature.peopleContext().writable, true);
    hold = true; f.setTime(5000); f.feature.tick();
    for (let i=0;i<20;i++) await Promise.resolve();
    gate = true; f.setTime(10000); f.feature.tick();
    for (let i=0;i<20;i++) await Promise.resolve();
    assert.equal(f.feature.peopleContext().writable, false);
    await assert.rejects(f.feature.sendPeople('s/o/c/g','draft'), /unavailable/);
    gate = false; hold = false; f.setTime(20000); f.feature.tick();
    for (let i=0;i<20;i++) await Promise.resolve();
    assert.equal(f.feature.peopleContext().writable, true);
    recovered = true; f.setTime(25000); f.feature.tick();
    for (let i=0;i<20;i++) await Promise.resolve();
    const count = reads;
    if (outcome === 'error') old.reject(new Error('{"error":"not_found"}'));
    else if (phase === 'mirror') old.resolve(readAnswer([{t:'user',seq:99,text:'stale content'}],99,false,view));
    else if (phase === 'comments') old.resolve(commentsAnswer([{seq:99,anchor:1,author:'p',text:'stale content'}],99));
    else old.resolve({});
    for (let i=0;i<20;i++) await Promise.resolve();
    f.setTime(30000); f.feature.tick();
    for (let i=0;i<20;i++) await Promise.resolve();
    assert.equal(reads, count, 'stale completion must not release the newer reader');
    assert.equal(f.feature.peopleContext().writable, true);
    assert.equal(f.feature.tab.selected(), 'team/s/o/c/g');
    assert.doesNotMatch(f.get('guestnote').textContent, /no longer/);
    assert.doesNotMatch(f.get('msgs').innerHTML, /stale content/);
    const cursor = phase === 'seen' ? 2 : 1;
    assert.match(paths.at(-1), new RegExp(`after=${cursor}$`));
    newer.resolve(readAnswer([{t:'assistant',seq:cursor+1,text:'new content'}],cursor+1,false,view));
    await f.feature.settled();
    assert.match(f.get('msgs').innerHTML, /new content/);
    assert.equal((f.get('msgs').innerHTML.match(/new content/g)||[]).length, 1);
    assert.equal(marks.at(-1).mirror,cursor+1);
    assert.ok(comments >= 2); assert.ok(seen >= 2);
    f.feature.destroy();
  });
}

test('received recovery respects missing, ended, and read-only list metadata', async () => {
  let views = [{...base, access:'write'}], gate = false, reads = 0;
  const f = nativeViewFixture(async path => {
    if (path === '/api/team/subscriptions') {
      if (gate) throw Object.assign(new Error('off'), {status:404});
      return list(views);
    }
    if (path.endsWith('/seen')) return {};
    reads++; return readAnswer([],0,false,{...base,access:'write'});
  });
  f.feature.tick(); await f.feature.settled(); f.open(); await f.feature.settled();
  for (const next of [[], [{...base,access:'read'}], [{...base,access:'write',state:'ended'}]]) {
    gate=true; f.feature.refresh(); await f.feature.settled();
    assert.equal(f.feature.peopleContext().writable,false);
    gate=false; views=next; f.feature.refresh(); await f.feature.settled();
    assert.equal(f.feature.peopleContext().writable,false);
    await assert.rejects(f.feature.sendPeople('s/o/c/g','draft'),/unavailable/);
  }
  views=[]; f.feature.refresh(); await f.feature.settled();
  const before=reads; f.setTime(100000); f.feature.tick(); await f.feature.settled(); assert.equal(reads,before);
  f.feature.destroy();
});

test('received recovery renders new comments once and marks only rendered cursors', async () => {
  const view={...base,access:'write',unread:true,mirror_last:1,comments_last:1};
  const held=deferred(); let gate=false, hold=false, delivered=false; const marks=[], commentPaths=[];
  const f=nativeViewFixture(async (path,opts)=>{
    if(path==='/api/team/subscriptions') { if(gate) throw Object.assign(new Error('off'),{status:404}); return list([view]); }
    if(path.endsWith('/seen')) {marks.push(JSON.parse(opts.body));return {};}
    return readAnswer([{t:'user',seq:1,text:'owner'}],1,false,view);
  },async path=>{
    commentPaths.push(path); if(hold)return held.promise;
    if(delivered)return commentsAnswer([{seq:1,anchor:1,author:'p',text:'new human comment'}],1,false,{p:'Person'});
    return commentsAnswer();
  });
  f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
  hold=true;f.setTime(5000);f.feature.tick();for(let i=0;i<10;i++)await Promise.resolve();
  gate=true;f.setTime(10000);f.feature.tick();for(let i=0;i<10;i++)await Promise.resolve();
  held.resolve(commentsAnswer([{seq:8,anchor:1,author:'p',text:'stale'}],8));await f.feature.settled();
  assert.deepEqual(marks,[{mirror:1}]);
  gate=false;hold=false;delivered=true;f.setTime(20000);f.feature.tick();await f.feature.settled();
  f.setTime(25000);f.feature.tick();await f.feature.settled();
  assert.equal(f.feature.tab.selected(),'team/s/o/c/g');
  assert.match(commentPaths.at(-1),/after=0$/);
  assert.equal((f.get('msgs').innerHTML.match(/new human comment/g)||[]).length,1);
  assert.doesNotMatch(f.get('msgs').innerHTML,/stale/);
  assert.deepEqual(marks,[{mirror:1},{comments:1}]);
  f.feature.destroy();
});

test('received removal asks for local-copy confirmation and restores focus on cancel', async () => {
  let deletes=0, message='';
  const f=nativeViewFixture(async (path,opts)=>{if(path==='/api/team/subscriptions')return list([base]);if(opts?.method==='DELETE')deletes++;return readAnswer();},undefined,undefined,{confirm:text=>{message=text;return false;}});
  f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
  f.click('guestremove');assert.match(message,/leav/i);assert.match(message,/copy/i);assert.equal(deletes,0);assert.equal(f.doc.activeElement,f.get('guestremove'));
  f.feature.destroy();
});

test('owner People comments identify their human destination', () => {
  const html=guestMessageHTML({seq:1,anchor:0,text:'human message',author:'o',mine:true},{},'s',{},'s/o/c/g');
  assert.match(html,/You · People/);assert.match(html,/human message/);
});

test('removal confirmation cannot delete a different chat selected during the dialog', async () => {
  const other={...base,chat:'other'}; let deletes=0; let f;
  f=nativeViewFixture(async (path,opts)=>{if(path==='/api/team/subscriptions')return list([base,other]);if(opts?.method==='DELETE')deletes++;return readAnswer();},undefined,undefined,{confirm:()=>{f.open('team/'+guestKey(other));return true;}});
  f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();f.click('guestremove');await f.feature.settled();
  assert.equal(deletes,0);assert.equal(f.feature.tab.selected(),'team/'+guestKey(other));f.feature.destroy();
});

test('received shared click ignores a target without closest', async () => {
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer());
  f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();f.click('msgs',{});
  assert.equal(f.feature.tab.selected(),'team/s/o/c/g');f.feature.destroy();
});

test('owner prompt capability follows the local agent, independent of sharing state', async () => {
 const view={...base,access:'owner',state:'ended',delivery:'offline',local_node:'n1'};let available=false;
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),async()=>commentsAnswer([{seq:1,anchor:0,text:'retained',author:'p'}],1),undefined,{selectedNode:()=> 'n1',selectedUID:()=> 'c',localAgentAvailable:()=>available});
 f.feature.tick();await f.feature.settled();const source={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:1};
 assert.equal(f.feature.canPreparePrompt(source),false,'closed or missing local agent cannot accept a prompt');
 available=true;assert.equal(f.feature.canPreparePrompt(source),true,'ended sharing and offline relay do not close the local agent');
 f.feature.destroy();
});
