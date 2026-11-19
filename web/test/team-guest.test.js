import test from "node:test";
import assert from "node:assert/strict";
import * as guestModule from "../js/team-guest.js";
import { readFileSync } from "node:fs";
import { jsonResponse } from "./s2-helpers.js";
import { decodeResponse } from "../js/api.js";
import { bubbleActionsHTML } from '../js/chat.js';
import { createBubbleActionController } from '../js/menu.js';
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
  const routed = (path, opts) => path.includes("/comments?") && opts?.method !== "POST" ? commentsApi(path, opts) : api(path, opts);
  let feature;
  const callbacks = extra.panel || {};
  const panel = { bubbleActions:bubbleActionsHTML, promptIcon:"", preparePrompt() {}, ...callbacks,
    loaded: callbacks.loaded || (() => {}), changed: callbacks.changed || (() => {}), ownerChanged: callbacks.ownerChanged || (() => {}),
    removed() { selected = "local"; callbacks.removed?.(); },
    select(id) { selected = id; callbacks.select?.(id); if (!extra.skipPanelSelect) feature.selectActivity(id); } };
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

test('shared read cannot paint after the common visit changes away and back', async () => {
 const held=deferred(), controller=createBubbleActionController();controller.selectVisit('team/s/o/c/g');
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):held.promise,undefined,undefined,{bubbleController:controller});
 f.feature.tick();await f.feature.settled();f.open();
 controller.selectVisit('n1');controller.selectVisit('team/s/o/c/g');
 held.resolve(readAnswer([{t:'user',text:'old answer',seq:1}],1));await f.feature.settled();
 assert.doesNotMatch(f.get('msgs').innerHTML,/old answer/);f.feature.destroy();
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

test('shared speaker labels identify humans and agent harness without destination suffixes', () => {
  const mine=guestMessageHTML({seq:1,anchor:0,author:'me',mine:true,text:'hello'},{});
  assert.match(mine,/>You<\/div>/);assert.doesNotMatch(mine,/You · People/);
  const agent=guestEntryHTML({t:'assistant',seq:2,text:'answer',agent:'Codex'},'Owner');
  assert.match(agent,/>Codex · agent<\/div>/);
});

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
  assert.match(mine, />You<\/div>/);
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

test('shared bubble actions reveal the stored sent time and omit absent time', async () => {
 const entries=[{t:'user',seq:1,text:'dated',time:'2026-01-02T03:04:00Z'},{t:'user',seq:2,text:'undated'}];
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer(entries,2),undefined,undefined,{bookmarks:()=>[],uiMutate(){},openSendTo(){}});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
 const dated=sharedActionBubble(1);f.click('msgs',dated.bubble);assert.match(dated.turn.rowHTML,/<div class="bubwhen">[^<]+<\/div>[^]*data-gact=/);
 const undated=sharedActionBubble(2);f.click('msgs',undated.bubble);assert.doesNotMatch(undated.turn.rowHTML,/bubwhen/);f.feature.destroy();
});

test('shared bubbles expose the ordinary action order and icons', async () => {
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([{t:'assistant',seq:1,text:'answer'}],1),undefined,undefined,{bookmarks:()=>[],uiMutate(){},openSendTo(){},panel:{promptIcon:'INTO',branchIcon:'FORK',copyIcon:'COPY'}});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();const turn=sharedActionBubble(1);f.click('msgs',turn.bubble);
 const html=turn.turn.rowHTML,actions=[...html.matchAll(/data-gact="(fork|sendto|desc|bookmark|copy)"/g)].map(m=>m[1]);
 assert.deepEqual(actions,['fork','sendto','desc','bookmark','copy']);assert.match(html,/FORK/);assert.match(html,/INTO/);assert.match(html,/COPY/);f.feature.destroy();
});

test('shared file bookmark failures do not commit a misleading bookmark',async()=>{
 for(const mode of ['missing capture','capture error']) {
  const entry={seq:1,anchor:0,author:'a',text:'synthetic',attachments:[{id:'0123456789abcdef',sha256:'a'.repeat(64),name:'synthetic.png',mime:'image/png',size:1}]};
  const mutations=[],panel=mode==='capture error'?{captureBookmark:async()=>{throw Error('synthetic capture failure')}}:{};
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer(),async()=>commentsAnswer([entry],1),undefined,{panel,bookmarks:()=>[],uiMutate:change=>mutations.push(change)});
  f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
  const item=sharedActionBubble(0);item.turn.dataset={cseq:'1'};f.click('msgs',item.bubble);
  const action={dataset:{gact:'bookmark'},closest:selector=>selector==='[data-gact]'?action:null};f.click('msgs',action);
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(f.get('guestnote').textContent,'Bookmark capture failed — try again.');assert.deepEqual(mutations,[]);f.feature.destroy();
 }
});

test('shared file copy explains whether text can be copied',async()=>{
 for(const text of ['', 'synthetic caption']) {
  const entry={seq:1,anchor:0,author:'a',text,attachments:[{id:'0123456789abcdef',sha256:'a'.repeat(64),name:'synthetic.txt',mime:'text/plain',size:1}]};
  const notices=[],copied=[];
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer(),async()=>commentsAnswer([entry],1),undefined,{toast:value=>notices.push(value),panel:{copy:value=>copied.push(value)}});
  f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
  const item=sharedActionBubble(0);item.turn.dataset={cseq:'1'};f.click('msgs',item.bubble);
  const action={dataset:{gact:'copy'},closest:selector=>selector==='[data-gact]'?action:null};f.click('msgs',action);
  assert.deepEqual(copied,text?[text]:[]);
  assert.deepEqual(notices,[text?'This action uses the text; attached files are not included.':'This action does not support files without text.']);f.feature.destroy();
 }
});

test('shared action commands pass captured text to local capability adapters', async () => {
 const calls=[], entry={t:'assistant',seq:1,text:'answer',agent:'Codex',time:'then'};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([entry],1),undefined,undefined,{bookmarks:()=>[],uiMutate(){},openSendTo(){},panel:{fork:c=>calls.push(['fork',c]),describe:c=>calls.push(['desc',c]),copy:t=>calls.push(['copy',t])}});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();const item=sharedActionBubble(1);f.click('msgs',item.bubble);
 for(const name of ['fork','desc','copy'])f.click('msgs',{dataset:{gact:name},closest:s=>s==='[data-gact]'?{dataset:{gact:name}}:null});
 assert.deepEqual(calls.map(c=>c[0]),['fork','desc','copy']);assert.equal(calls[0][1].team.seq,1);assert.equal(calls[1][1].role,'assistant');assert.equal(calls[2][1],'answer');f.feature.destroy();
});

test('description selection offers only live local nodes and revalidates a chosen target', async () => {
 const chosen=[],nodes=[{id:'local',title:'Local',instance_id:'local-instance'},{id:'closed',title:'Closed',ended_at:'then'}];let menu;
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):path==='/api/state'?{nodes}:readAnswer([{t:'user',seq:1,text:'human'}],1),undefined,undefined,{localNodes:()=>nodes,bookmarks:()=>[],uiMutate(){},openSendTo(){},panel:{chooseDescription:options=>{menu=options},editDescription:(id,c)=>chosen.push([id,c])}});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();const item=sharedActionBubble(1);f.click('msgs',item.bubble);
 const action={dataset:{gact:'desc'},closest:s=>s==='[data-gact]'?action:null};f.click('msgs',action);
 assert.deepEqual(menu.choices.map(x=>x.id),['local']);menu.onChoose({id:'closed'});assert.deepEqual(chosen,[]);
 menu.onChoose({id:'local'});await new Promise(resolve=>setImmediate(resolve));assert.equal(chosen[0][0],'local');assert.equal(chosen[0][1].text,'human');
  nodes[0].ended_at='then';menu.onChoose({id:'local'});assert.equal(chosen.length,1);
  nodes[0].ended_at='';f.navigate('elsewhere');menu.onChoose({id:'local'});assert.equal(chosen.length,1);f.feature.destroy();
});

test('shared send-to carries its exact source and permits existing local targets only', async () => {
 const sends=[],entry={t:'assistant',seq:1,text:'answer',agent:'Codex'};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([entry],1),undefined,undefined,{bookmarks:()=>[],uiMutate(){},openSendTo:x=>sends.push(x)});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();const item=sharedActionBubble(1);f.click('msgs',item.bubble);
 const action={dataset:{gact:'sendto'},closest:s=>s==='[data-gact]'?action:null};f.click('msgs',action);
 assert.deepEqual(sends[0].source,{server:'s',owner:'o',chat:'c',generation:'g',log:'mirror',seq:1});assert.equal(sends[0].allowNew,false);f.feature.destroy();
});

test('received bubbles render the common marker for their exact source', async () => {
 const calls=[],f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([{t:'assistant',seq:1,text:'answer'}],1),async()=>commentsAnswer([{seq:2,anchor:1,text:'comment',author:'a'}],2),undefined,{panel:{sharedMarkers:source=>{calls.push(source);return '<span data-marker-proof></span>'}}});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
 assert.equal((f.get('msgs').innerHTML.match(/data-marker-proof/g)||[]).length,2);assert.deepEqual(calls.map(x=>[x.log,x.seq]),[['mirror',1],['comments',2]]);f.feature.destroy();
});

test('an omitted shared comment never acquires a source marker', async () => {
 let markerCalls=0;
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer(),async()=>commentsAnswer([{seq:1,anchor:0,author:'a',omitted:'too_large'}],1),undefined,{panel:{sharedMarkers:()=>{markerCalls++;return 'marker'}}});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();assert.equal(markerCalls,0);assert.doesNotMatch(f.get('msgs').innerHTML,/marker/);f.feature.destroy();
});

test('received marker activation delegates the exact shared identity', async () => {
 const calls=[],f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([{t:'assistant',seq:1,text:'answer'}],1),undefined,undefined,{panel:{openMarker:(source,kind)=>calls.push({source,kind})}});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
 const turn={dataset:{seq:'1'}},marker={dataset:{bmarker:'bookmark'},closest:s=>s==='[data-bmarker]'?marker:s==='[data-seq], [data-cseq]'?turn:null};f.click('msgs',marker);
 assert.deepEqual(calls,[{source:{server:'s',owner:'o',chat:'c',generation:'g',log:'mirror',seq:1},kind:'bookmark'}]);f.feature.destroy();
});

test('received marker refresh updates its existing bubble without losing focused marker', async () => {
 let html='<div class="bubblemarkers"><button data-bmarker="bookmark"></button></div>',focused=0;
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([{t:'assistant',seq:1,text:'answer'}],1),undefined,undefined,{panel:{sharedMarkers:()=>html}});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
 const active={dataset:{bmarker:'bookmark'}},old={contains:x=>x===active,remove(){}},replacement={focus(){focused++}},bubble={querySelector:s=>s==='.bubblemarkers'?old:s==='[data-bmarker="bookmark"]'?replacement:null,insertAdjacentHTML(){}};
 f.doc.activeElement=active;f.get('msgs').querySelectorAll=s=>s==='[data-seq], [data-cseq]'?[{dataset:{seq:'1'},querySelector:()=>bubble}]:[];
 html='<div class="bubblemarkers"><button data-bmarker="bookmark">new</button></div>';f.feature.refreshMarkers();assert.equal(focused,1);f.feature.destroy();
});

test('marker refresh ignores unrelated or missing bubbles and updates a comment without stealing focus', async () => {
 let html='<span data-bmarker="bookmark"></span>',inserted=[];
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([{t:'assistant',seq:1,text:'answer'}],1),async()=>commentsAnswer([{seq:2,anchor:1,text:'comment',author:'a'}],2),undefined,{panel:{sharedMarkers:()=>html}});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
 const bubble={querySelector:()=>null,insertAdjacentHTML:(_where,value)=>inserted.push(value)};
 f.get('msgs').querySelectorAll=()=>[{dataset:{seq:'99'},querySelector:()=>bubble},{dataset:{seq:'1'},querySelector:()=>null},{dataset:{cseq:'2'},querySelector:()=>bubble}];
 html='<span data-bmarker="sendto"></span>';f.feature.refreshMarkers();assert.deepEqual(inserted,[html]);
 f.feature.refreshMarkers();assert.deepEqual(inserted,[html]);f.feature.destroy();
});

test('shared bookmark jump selects the exact sequence even when text is duplicated', async () => {
 const view={...base,access:'write'};const hits=[],scrolls=[];
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),async()=>commentsAnswer([{seq:1,anchor:0,text:'same',author:'a'},{seq:2,anchor:0,text:'same',author:'a'}],2,false,{a:'Writer'}));
 f.get('msgs').querySelector=selector=>{hits.push(selector);return selector.startsWith('[data-cseq=')?{scrollIntoView(){scrolls.push(selector)},classList:{add(){},remove(){}}}:null};
 f.feature.tick();await f.feature.settled();const source={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:2};
 assert.equal(f.feature.jumpToSource(source),true);await f.feature.settled();assert.ok(hits.includes('[data-cseq="2"]'));assert.equal(scrolls.at(-1),'[data-cseq="2"]');
 assert.equal(f.feature.jumpToSource({...source,generation:'stale'}),false);f.feature.destroy();
});

test('jumping to a shared source retires the previous action row', async () => {
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([{t:'assistant',seq:1,text:'answer'}],1));
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();const item=sharedActionBubble(1);f.click('msgs',item.bubble);assert.ok(item.turn.nextElementSibling);
 assert.equal(f.feature.jumpToSource({server:'s',owner:'o',chat:'c',generation:'g',log:'mirror',seq:1}),true);assert.equal(item.turn.nextElementSibling,null);f.feature.destroy();
});

test('an owner bookmark resolves only through its verified local node and exact shared identity', async () => {
 const view={...base,access:'owner',local_node:'owned'},jumps=[];
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),undefined,undefined,{panel:{ownerJump:(node,source)=>{jumps.push({node,source});return true}}});
 f.feature.tick();await f.feature.settled();const source={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:2};
 assert.equal(f.feature.jumpToSource(source),true);assert.equal(f.selected(),'owned');assert.deepEqual(jumps,[{node:'owned',source}]);
 assert.equal(f.feature.jumpToSource({...source,generation:'stale'}),false);f.feature.destroy();
});

test('bookmark navigation rejects invalid identities and owners without a verified local destination', async () => {
 const source={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:1};
 for (const [local_node,ownerJump] of [['',()=>true],['team/s/o/c/g',()=>true],['owned',undefined],['owned',()=>false]]) {
  const view={...base,access:'owner',local_node};
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),undefined,undefined,{panel:{ownerJump}});
  f.feature.tick();await f.feature.settled();
  assert.equal(f.feature.jumpToSource(source),false);
  assert.equal(f.feature.jumpToSource(null),false);
  assert.equal(f.feature.jumpToSource({...source,log:'other'}),false);
  assert.equal(f.feature.jumpToSource({...source,seq:0}),false);
  f.feature.destroy();
 }
});

test('bookmark navigation reports a missing mirror instead of matching another bubble', async () => {
 const view={...base,access:'write',unread:false};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view));
 f.feature.tick();await f.feature.settled();
 assert.equal(f.feature.jumpToSource({server:'s',owner:'o',chat:'c',generation:'g',log:'mirror',seq:8}),true);
 await f.feature.settled();assert.match(f.get('guestnote').textContent,/unavailable/i);f.feature.destroy();
});

test('bookmark navigation cannot act when the host declines selection', async () => {
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer(),undefined,undefined,{skipPanelSelect:true});
 f.feature.tick();await f.feature.settled();
 assert.equal(f.feature.jumpToSource({server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:1}),false);
 assert.equal(f.feature.tab.selected(),'');f.feature.destroy();
});

test('a stalled mirror page cannot keep a bookmark jump polling forever', async () => {
 let reads=0;
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):(reads++,readAnswer([],0,true)));
 f.feature.tick();await f.feature.settled();assert.equal(f.feature.jumpToSource({server:'s',owner:'o',chat:'c',generation:'g',log:'mirror',seq:9}),true);await f.feature.settled();
 assert.ok(reads<=40);assert.match(f.get('guestnote').textContent,/unavailable/i);f.feature.destroy();
});

test('an already loaded mirror jumps by sequence and reports an omitted record truthfully', async () => {
 const source={server:'s',owner:'o',chat:'c',generation:'g',log:'mirror',seq:1};
 for (const omitted of [false,true]) {
  const record={t:'assistant',seq:1,text:'answer',...(omitted?{omitted:'too_large'}:{})};let hit='';
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([record],1));
  f.get('msgs').querySelector=selector=>({scrollIntoView(){hit=selector},classList:{add(){},remove(){}}});
  f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();hit='';
  assert.equal(f.feature.jumpToSource(source),true);
  if(omitted){assert.equal(hit,'');assert.match(f.get('guestnote').textContent,/unavailable/i);}
  else assert.equal(hit,'[data-seq="1"]');
  f.feature.destroy();
 }
});

test('owner mirror bookmark uses a loaded copy with the exact sequence', async () => {
 const view={...base,access:'owner',local_node:'n1',unread:false},source={server:'s',owner:'o',chat:'c',generation:'g',log:'mirror',seq:1};let changed=0;
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([{t:'assistant',seq:1,text:'answer'}],1,false,view),undefined,undefined,{selectedNode:()=> 'n1',selectedUID:()=> 'c',panel:{ownerJump:()=>true,ownerChanged:()=>changed++}});
 f.feature.tick();await f.feature.settled();const before=changed;assert.equal(f.feature.jumpToSource(source),true);assert.equal(changed,before+1);f.feature.destroy();
});

test('an owner mirror jump resolves when its requested page arrives', async () => {
 const view={...base,access:'owner',local_node:'n1',unread:false},source={server:'s',owner:'o',chat:'c',generation:'g',log:'mirror',seq:9};let selected='',unavailable=[];
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([{t:'assistant',seq:9,text:'answer',local_segment:0,local_record:2}],9,false,view),undefined,undefined,{selectedNode:()=>selected,selectedUID:()=>selected?'c':'',panel:{select:id=>{selected=id},ownerJump:()=>true,ownerUnavailable:s=>unavailable.push(s)}});
 f.feature.tick();await f.feature.settled();assert.equal(f.feature.jumpToSource(source),true);f.feature.localIdentityChanged();await f.feature.settled();
 assert.ok(f.feature.ownerSnapshots()[0].mirrors.some(x=>x.seq===9));assert.deepEqual(unavailable,[]);f.feature.destroy();
});

test('owner bookmark loading continues past the page bound until the exact comment arrives', async () => {
 const view={...base,access:'owner',local_node:'n1',unread:false,comments_last:41},source={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:41};let selected='';
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),async path=>{const seq=Number(path.split('after=')[1])+1;return commentsAnswer([{seq,anchor:0,text:'same',author:'a'}],seq,seq<41,{a:'Writer'});},undefined,{selectedNode:()=>selected,selectedUID:()=>selected?'c':'',panel:{select:id=>{selected=id},ownerJump:()=>true}});
 f.feature.tick();await f.feature.settled();assert.equal(f.feature.jumpToSource(source),true);f.feature.localIdentityChanged();await f.feature.settled();
 assert.ok(f.feature.ownerSnapshots()[0].comments.some(x=>x.seq===41));f.feature.destroy();
});

test('owner bookmark reports an unavailable source when its saved copy cannot be read', async () => {
 const view={...base,access:'owner',local_node:'n1',unread:false},source={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:9};let selected='',unavailable=[];
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),async()=>{throw Error('offline')},undefined,{selectedNode:()=>selected,selectedUID:()=>selected?'c':'',panel:{select:id=>{selected=id},ownerJump:()=>true,ownerUnavailable:s=>unavailable.push(s)}});
 f.feature.tick();await f.feature.settled();f.feature.jumpToSource(source);f.feature.localIdentityChanged();await f.feature.settled();assert.deepEqual(unavailable,[source]);f.feature.destroy();
});

test('owner bookmark stops stalled mirror and comment pages without inventing a target', async () => {
 for(const log of ['mirror','comments']) {
  const view={...base,access:'owner',local_node:'n1',unread:false},source={server:'s',owner:'o',chat:'c',generation:'g',log,seq:9};let selected='',unavailable=[];
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,log==='mirror',view),async()=>commentsAnswer([],0,log==='comments'),undefined,{selectedNode:()=>selected,selectedUID:()=>selected?'c':'',panel:{select:id=>{selected=id},ownerJump:()=>true,ownerUnavailable:s=>unavailable.push(s)}});
  f.feature.tick();await f.feature.settled();assert.equal(f.feature.jumpToSource(source),true);f.feature.localIdentityChanged();await f.feature.settled();
  assert.deepEqual(unavailable,[source]);f.feature.destroy();
 }
});

test('an owner bookmark loading in an old chat is retired on navigation', async () => {
 const held=deferred(),view={...base,access:'owner',local_node:'n1',unread:false},source={server:'s',owner:'o',chat:'c',generation:'g',log:'mirror',seq:9};let selected='',unavailable=[];
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):held.promise,undefined,undefined,{selectedNode:()=>selected,selectedUID:()=>selected==='n1'?'c':'',panel:{select:id=>{selected=id},ownerJump:()=>true,ownerUnavailable:s=>unavailable.push(s)}});
 f.feature.tick();await f.feature.settled();assert.equal(f.feature.jumpToSource(source),true);f.feature.localIdentityChanged();selected='other';f.feature.localIdentityChanged();
 held.resolve(readAnswer([],0,false,view));await f.feature.settled();assert.deepEqual(unavailable,[]);f.feature.destroy();
});

test('owner bookmark uses an already loaded copy without a fresh network read', async () => {
 const view={...base,access:'owner',local_node:'n1',unread:false},source={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:1};let reads=0,unavailable=0;
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),async()=>{reads++;if(reads>1)throw Error('offline');return commentsAnswer([{seq:1,anchor:0,author:'a',text:'saved'}],1,false)},undefined,{selectedNode:()=> 'n1',selectedUID:()=> 'c',panel:{ownerJump:()=>true,ownerUnavailable:()=>unavailable++}});
 f.feature.tick();await f.feature.settled();assert.equal(f.feature.jumpToSource(source),true);await f.feature.settled();assert.equal(reads,1);assert.equal(unavailable,0);f.feature.destroy();
});

test('shared bookmark jump continues across bounded comment pages', async () => {
 const view={...base,access:'write',unread:false,comments_last:41,seen_comments:0};const scrolls=[];
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),async path=>{
  const seq=Number(path.split('after=')[1])+1;return commentsAnswer([{seq,anchor:0,text:'same',author:'a'}],seq,seq<41,{a:'Writer'});
 });
 f.get('msgs').querySelector=selector=>selector==='[data-cseq="41"]'?{scrollIntoView(){scrolls.push(selector)},classList:{add(){},remove(){}}}:null;
 f.feature.tick();await f.feature.settled();assert.equal(f.feature.jumpToSource({server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:41}),true);
 await f.feature.settled();assert.deepEqual(scrolls,['[data-cseq="41"]']);f.feature.destroy();
});

test('a jump in an already open chat continues the remaining mirror pages', async () => {
 let scrolled=false;
 const f=nativeViewFixture(async path=>{if(path==='/api/team/subscriptions')return list([base]);const seq=Number(path.split('after=')[1])+1;return readAnswer([{seq,t:'assistant',text:'answer'}],seq,seq<41)});
 f.get('msgs').querySelector=selector=>selector==='[data-seq="41"]'?{scrollIntoView(){scrolled=true},classList:{add(){},remove(){}}}:null;
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
 assert.equal(f.feature.jumpToSource({server:'s',owner:'o',chat:'c',generation:'g',log:'mirror',seq:41}),true);await f.feature.settled();assert.equal(scrolled,true);f.feature.destroy();
});

test('a delayed bookmark page cannot scroll or replace a later selection', async () => {
 const held=deferred(),view={...base,access:'write',unread:false};let scrolls=0;
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),()=>held.promise);
 f.get('msgs').querySelector=()=>({scrollIntoView(){scrolls++},classList:{add(){},remove(){}}});
 f.feature.tick();await f.feature.settled();
 assert.equal(f.feature.jumpToSource({server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:2}),true);
 f.navigate();held.resolve(commentsAnswer([{seq:2,text:'late',author:'a'}],2));await f.feature.settled();
 assert.equal(scrolls,0);assert.doesNotMatch(f.get('msgs').innerHTML,/late/);assert.equal(f.selected(),'local');f.feature.destroy();
});

test('shared bookmark jump stops if a paginated source makes no progress', async () => {
 let reads=0;const view={...base,access:'write',unread:false};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),async()=>{if(++reads>45)throw Error('too many');return commentsAnswer([],0,true);});
 f.feature.tick();await f.feature.settled();f.feature.jumpToSource({server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:9});await f.feature.settled();
 assert.ok(reads<=40);assert.match(f.get('guestnote').textContent,/unavailable/i);f.feature.destroy();
});

test('renaming a person is offered from a secondary action menu', async () => {
 let menu;const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([{t:'user',seq:1,text:'human'}],1),undefined,undefined,{bookmarks:()=>[],uiMutate(){},openSendTo(){},panel:{choosePersonAction:opts=>{menu=opts}}});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();const item=sharedActionBubble(1);f.click('msgs',item.bubble);
 assert.match(item.turn.rowHTML,/data-gact="more"/);assert.doesNotMatch(item.turn.rowHTML,/data-gact="rename"/);
 const action={dataset:{gact:'more'},closest:s=>s==='[data-gact]'?action:null};f.click('msgs',action);assert.deepEqual(menu.choices.map(x=>x.id),['rename']);f.feature.destroy();
});

test('description chooser with no local destinations offers no implicit target', async () => {
 let choices;
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([{t:'user',seq:1,text:'human'}],1),undefined,undefined,{panel:{chooseDescription:menu=>{choices=menu.choices}}});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();const item=sharedActionBubble(1);f.click('msgs',item.bubble);
 const action={dataset:{gact:'desc'},closest:s=>s==='[data-gact]'?action:null};f.click('msgs',action);assert.deepEqual(choices,[]);f.feature.destroy();
});

test('description chooser labels an untitled local destination', async () => {
 let choices;
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([{t:'user',seq:1,text:'human'}],1),undefined,undefined,{localNodes:()=>[{id:'n1',instance_id:'one'}],panel:{chooseDescription:menu=>{choices=menu.choices}}});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();const item=sharedActionBubble(1);f.click('msgs',item.bubble);
 const action={dataset:{gact:'desc'},closest:s=>s==='[data-gact]'?action:null};f.click('msgs',action);assert.deepEqual(choices,[{id:'n1',label:'Untitled chat'}]);f.feature.destroy();
});

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
  click(action("sendto")); assert.equal(f.feature.tab.selected(), "team/s/o/c/g"); assert.deepEqual(sends, [{ text: "two", exceptId: "", title: "Send to…", role: "assistant", who: undefined, prov: undefined, source: {server:'s',owner:'o',chat:'c',generation:'g',log:'mirror',seq:2}, allowNew:false }]);
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
  assert.deepEqual(sends, [{ text: "message", exceptId: "", title: "Send to…", role: "person", who: "Ann", prov: undefined, source: {server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:2}, allowNew:false }]);
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
      if (sel === '[data-gact="more"]') return renameButton;
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
    assert.match(bubble.turn.nextElementSibling.innerHTML, /data-gact="more"/);
    sharedNameAction(f, "rename"); const row = bubble.turn.nextElementSibling, field = row.querySelector("[data-grename]");
    assert.ok(field); assert.equal(field.value, "Local"); assert.equal(f.doc.activeElement, field); field.value = typed;
    f.setTime(5000); f.feature.tick(); await f.feature.settled(); assert.equal(row.querySelector("[data-grename]"), field); assert.equal(field.value, typed);
    const before = f.invalidations(); sharedNameAction(f, choice === "save" ? "name-save" : "name-use");
    assert.deepEqual(ops, [{ k: "person-name", key: "s/o", name: expected }]); assert.equal(f.invalidations(), before + 1);
    const shown = expected || "Owner"; assert.equal(f.get("chatmission").textContent, "From " + shown + " · Server");
    assert.equal(label.textContent, shown + " · prompt · includes forwarded AI text");
    assert.equal(f.doc.activeElement, row.querySelector('[data-gact="more"]'));
  }
});

test('secondary person menu renames only the still-selected discussion', async () => {
 let menu;
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([{t:'user',seq:1,text:'human'}],1),undefined,undefined,{panel:{choosePersonAction:options=>{menu=options}}});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
 const first=sharedRenameBubble(1,f);f.click('msgs',first.bubble);sharedNameAction(f,'more');menu.onChoose({id:'other'});
 assert.equal(first.turn.nextElementSibling.querySelector('[data-grename]'),null);menu.onChoose({id:'rename'});
 assert.ok(first.turn.nextElementSibling.querySelector('[data-grename]'));
 const second=sharedRenameBubble(1,f);f.click('msgs',second.bubble);sharedNameAction(f,'more');const stale=menu;f.navigate();stale.onChoose({id:'rename'});
 assert.equal(f.feature.tab.selected(),'');f.feature.destroy();
});

test("owner comments offer rename only for other people and Escape cancels", async () => {
  const view = { ...base, access: "owner" }, ops = [];
  const f = nativeViewFixture(async path => path === "/api/team/subscriptions" ? list([view]) : readAnswer([], 0, false, view),
    async () => commentsAnswer([{seq:1,anchor:0,text:"mine",author:"self",mine:true}, {seq:2,anchor:0,text:"other",author:"a",mine:false}], 2, false, {a:"Writer"}), undefined,
    {selectedNode:()=>"n1", people:()=>({}), uiMutate:op=>ops.push(op), bookmarks:()=>[], openSendTo() {}});
  f.feature.tick(); await f.feature.settled();
  const mine = sharedRenameBubble(0, f); mine.turn.dataset = {cseq:"1",gkey:guestKey(view)};
  f.click("msgs", mine.bubble); assert.doesNotMatch(mine.turn.nextElementSibling.innerHTML, /data-gact="more"/); f.click("msgs", mine.bubble);
  const other = sharedRenameBubble(0, f); other.turn.dataset = {cseq:"2",gkey:guestKey(view)};
  f.click("msgs", other.bubble); sharedNameAction(f, "rename");
  const row=other.turn.nextElementSibling, field=row.querySelector("[data-grename]"); assert.equal(field.value,"Writer");
  let prevented=false, stopped=false;
  f.get("msgs").listeners.get("keydown")({key:"Escape",target:field,preventDefault(){prevented=true},stopPropagation(){stopped=true}});
  assert.equal(prevented,true); assert.equal(stopped,true); assert.deepEqual(ops,[]);
  assert.equal(f.doc.activeElement,row.querySelector('[data-gact="more"]')); f.feature.destroy();
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

test('owner unread excludes own comments without hiding an earlier unseen collaborator comment', async () => {
  const view={...base,access:'owner',local_node:'local',unread:true,comments_last:2,seen_comments:0};
  let comments=[{seq:1,anchor:0,author:'o',mine:true,text:'mine'},{seq:2,anchor:0,author:'o',mine:true,text:'mine too'}];
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer(),
    async()=>commentsAnswer(comments,comments.length,false));
  f.feature.tick(); await f.feature.settled();
  assert.equal(f.feature.tab.ownerUnread('local'),false,'own comments are not incoming');
  comments.push({seq:3,anchor:0,author:'other',mine:false,text:'incoming'},{seq:4,anchor:0,author:'o',mine:true,text:'mine later'});
  view.comments_last=4;
  f.feature.refresh(); await f.feature.settled();
  assert.equal(f.feature.tab.ownerUnread('local'),true,'later own comment must not hide incoming');
  f.feature.destroy();
});

test('unchanged classified comment ranges are reused while new sequences are inspected', async () => {
  const view={...base,access:'owner',local_node:'local',unread:true,comments_last:1,seen_comments:0};let reads=0;
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer(),async()=>{reads++;return commentsAnswer([{seq:1,mine:true,text:'mine'}],1,false);});
  f.feature.tick();await f.feature.settled();const invalidations=f.invalidations();f.feature.refresh();await f.feature.settled();assert.equal(reads,1);
  assert.equal(f.invalidations(),invalidations,'unchanged classified state does not rebuild cards');
  f.feature.destroy();
});

test('unavailable comment authorship remains unknown rather than inventing an unread message', async () => {
  const view={...base,access:'owner',local_node:'local',unread:true,comments_last:1,seen_comments:0};
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer(),async()=>{throw Error('offline');});
  f.feature.tick(); await f.feature.settled();
  assert.equal(f.feature.tab.ownerUnread('local'),false);
  assert.equal(f.feature.tab.ownerUnreadStatus('local'),'unknown');
  f.feature.destroy();
});

test('received comment unread excludes self while an unseen owner mirror remains unread', async () => {
  const view={...base,access:'write',unread:true,mirror_last:1,seen_mirror:1,comments_last:1,seen_comments:0};
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer(),
    async()=>commentsAnswer([{seq:1,anchor:0,author:'self',mine:true,text:'self'}],1,false));
  f.feature.tick(); await f.feature.settled();
  assert.equal(f.feature.tab.activities()[0].unread,false);
  view.mirror_last=2; f.feature.refresh(); await f.feature.settled();
  assert.equal(f.feature.tab.activities()[0].unread,true);
  f.feature.destroy();
});

test('comment classification detects incoming across pages and never marks them seen', async () => {
  const view={...base,access:'owner',local_node:'local',unread:true,comments_last:3,seen_comments:0};
  const reads=[],writes=[];
  const f=nativeViewFixture(async (path,opts)=>{if(path==='/api/team/subscriptions')return list([view]); if(opts?.method) writes.push(path); return readAnswer();},async path=>{
    reads.push(path);
    if(path.endsWith('after=0')) return commentsAnswer([{seq:1,mine:true,text:'own'}],1,true);
    return commentsAnswer([{seq:2,mine:false,text:'incoming'},{seq:3,mine:true,text:'own'}],3,false);
  });
  f.feature.tick(); await f.feature.settled();
  assert.equal(f.feature.tab.ownerUnread('local'),true);
  assert.deepEqual(reads.map(path=>path.split('?')[1]),['after=0','after=1']); assert.deepEqual(writes,[]);
  f.feature.destroy();
});

test('a gap in projected comments is unknown and a later proved incoming record still counts', async () => {
  const view={...base,access:'owner',local_node:'local',unread:true,comments_last:3,seen_comments:0};
  let incoming=false;
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer(),async()=>commentsAnswer([{seq:2,mine:!incoming,text:'entry'},{seq:3,mine:true,text:'own'}],3,false));
  f.feature.tick(); await f.feature.settled(); assert.equal(f.feature.tab.ownerUnreadStatus('local'),'unknown');
  incoming=true; f.feature.refresh(); await f.feature.settled(); assert.equal(f.feature.tab.ownerUnreadStatus('local'),'unread');
  f.feature.destroy();
});

test('stale classification cannot overwrite a newer list after a subscription disappears', async () => {
  const gate=deferred(),view={...base,access:'owner',local_node:'local',unread:true,comments_last:1,seen_comments:0}; let present=true;
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list(present?[view]:[]):readAnswer(),async()=>gate.promise);
  f.feature.tick(); await new Promise(r=>setImmediate(r)); present=false; f.feature.refresh();
  gate.resolve(commentsAnswer([{seq:1,mine:false,text:'incoming'}],1,false)); await f.feature.settled();
  assert.equal(f.feature.tab.ownerUnread('local'),false); f.feature.destroy();
});

test('repeated list refreshes do not start unbounded duplicate comment classifiers', async () => {
  const gate=deferred(),view={...base,access:'owner',local_node:'local',unread:true,comments_last:1,seen_comments:0}; let reads=0;
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer(),async()=>{reads++;return gate.promise;});
  f.feature.tick(); await new Promise(r=>setImmediate(r));assert.equal(f.feature.tab.ownerUnreadStatus('local'),'unknown');
  for(let i=0;i<8;i++){f.feature.refresh();await new Promise(r=>setImmediate(r));}
  assert.equal(reads,1);
  gate.resolve(commentsAnswer([{seq:1,mine:false,text:'incoming'}],1,false)); await f.feature.settled();
  assert.equal(f.feature.tab.ownerUnread('local'),true); f.feature.destroy();
});

test('destroy releases queued comment classification work without starting it', async () => {
  const gate=deferred(), views=Array.from({length:6},(_,i)=>({...base,chat:'c'+i,access:'owner',local_node:'n'+i,unread:true,comments_last:1,seen_comments:0})); let reads=0;
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list(views):readAnswer(),async()=>{reads++;return gate.promise;});
  f.feature.tick(); await new Promise(r=>setImmediate(r)); assert.equal(reads,4); f.feature.destroy();
  gate.resolve(commentsAnswer([{seq:1,mine:true,text:'own'}],1,false));
  await Promise.race([f.feature.settled(),new Promise((_r,reject)=>setTimeout(()=>reject(Error('queued work did not retire')),100))]);
  assert.equal(reads,4);
});

test('stalled comment reads time out so a later subscription can classify', async () => {
  const views=Array.from({length:5},(_,i)=>({...base,chat:'c'+i,access:'owner',local_node:'n'+i,unread:true,comments_last:1,seen_comments:0}));
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list(views):readAnswer(),
    async path=>path.includes('/c4/')?commentsAnswer([{seq:1,mine:false,text:'incoming'}],1,false):new Promise(()=>{}),undefined,{classificationTimeoutMs:10});
  f.feature.tick();await Promise.race([f.feature.settled(),new Promise((_r,reject)=>setTimeout(()=>reject(Error('classification queue stalled')),200))]);
  assert.equal(f.feature.tab.ownerUnreadStatus('n0'),'unknown');assert.equal(f.feature.tab.ownerUnread('n4'),true);f.feature.destroy();
});

test('classification retains proven incoming evidence when a newer page is malformed', async () => {
  const gate=deferred(), view={...base,access:'owner',local_node:'local',unread:true,comments_last:1,seen_comments:0}; let calls=0;
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer(),async()=>{
    calls++;if(calls===1)return gate.promise;
    if(calls===2)return commentsAnswer([{seq:1,mine:true,text:'own'},{seq:2,mine:false,text:'incoming'}],2,false);
    return {entries:[{seq:4,mine:true}],next:2,more:false};
  });
  f.feature.tick();await new Promise(r=>setImmediate(r));view.comments_last=2;f.feature.refresh();await new Promise(r=>setImmediate(r));
  gate.resolve(commentsAnswer([{seq:1,mine:true,text:'own'}],1,false));await f.feature.settled();
  assert.equal(calls,2);assert.equal(f.feature.tab.ownerUnreadStatus('local'),'unread');
  view.comments_last=3;f.feature.refresh();await f.feature.settled();assert.equal(f.feature.tab.ownerUnreadStatus('local'),'unread');
  f.feature.destroy();
});

test('comment classification treats malformed or incomplete page evidence as unknown', async () => {
  const cases=[
    {entries:null,next:1}, {entries:[],next:0},
    {entries:[{seq:0,mine:false}],next:1}, {entries:[{seq:2,mine:false}],next:1},
    {entries:[{seq:1}],next:1}, {entries:[],next:1},
  ];
  for (const page of cases) {
    const view={...base,access:'owner',local_node:'local',unread:true,comments_last:1,seen_comments:0};
    const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer(),async()=>page);
    f.feature.tick();await f.feature.settled();assert.equal(f.feature.tab.ownerUnreadStatus('local'),'unknown');f.feature.destroy();
  }
});

test('comment classification caps catch-up pages and reports unknown until complete', async () => {
  const view={...base,access:'owner',local_node:'local',unread:true,comments_last:41,seen_comments:0};let calls=0;
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer(),async()=>{calls++;return commentsAnswer([{seq:calls,mine:true,text:'own'}],calls,true);});
  f.feature.tick();await f.feature.settled();assert.equal(calls,40);assert.equal(f.feature.tab.ownerUnreadStatus('local'),'unknown');f.feature.destroy();
});

test('bounded classification resumes after forty pages instead of rereading the same prefix', async () => {
  const view={...base,access:'owner',local_node:'local',unread:true,comments_last:41,seen_comments:0};let calls=0;
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer(),async path=>{
    calls++;const after=Number(path.split('after=')[1]);const seq=after+1;return commentsAnswer([{seq,mine:seq!==41,text:'entry'}],seq,seq<41);
  });
  f.feature.tick();await f.feature.settled();assert.equal(f.feature.tab.ownerUnreadStatus('local'),'unknown');
  f.feature.refresh();await f.feature.settled();assert.equal(calls,41);assert.equal(f.feature.tab.ownerUnreadStatus('local'),'unread');f.feature.destroy();
});

test('received unknown status is announced in its tab and group without an unread claim', async () => {
  const view={...base,access:'write',unread:true,mirror_last:1,seen_mirror:1,comments_last:1,seen_comments:0};
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer(),async()=>{throw Error('offline');});
  f.feature.tick();await f.feature.settled();assert.match(f.feature.tab.tabHTML(false),/unread status unavailable/);
  assert.equal(f.feature.tab.activities()[0].unread,false);assert.equal(f.feature.tab.groups()[0].items[0].unreadUnknown,true);f.feature.destroy();
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
  const view={...base,access:"write",people_copy_instance:"1".repeat(32)};
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

test('late owner file retention cannot apply to a different local chat', async () => {
 const view={...base,access:'owner',local_node:'n1',people_copy_instance:'1'.repeat(32)}, pending=deferred();
 const file={id:'0123456789abcdef',sha256:'a'.repeat(64),name:'synthetic.txt',mime:'text/plain',size:1};
 const entry={seq:1,anchor:0,text:'synthetic',author:'p',attachments:[file]};
 let selected='n1',started=false,seen=0,renders=0;
 const panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){renders++}};
 const f=nativeViewFixture(async(path,opts)=>{
  if(path==='/api/team/subscriptions')return {...list([view],[{id:'s',delivery:'ok',people_files:'supported'}]),people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216}};
  if(path.includes('/seen')&&opts?.method==='PUT'){seen++;return {}}
  return readAnswer([],0,false,view);
 },async()=>({entries:[entry],names:{p:'Person'},next:1,more:false}),undefined,{panel,selectedNode:()=>selected,selectedUID:()=>selected==='n1'?'c':'',retainPeople:async()=>{started=true;return pending.promise}});
 f.feature.tick();for(let i=0;i<40&&!started;i++)await Promise.resolve();assert.equal(started,true);
 selected='n2';f.feature.setOwnerShare('n2',null);const before=renders;
 pending.resolve({files:{'1/0123456789abcdef':true}});await f.feature.settled();
 assert.equal(renders,before);assert.deepEqual(f.feature.ownerSnapshots(),[]);assert.equal(entry._peopleRetained,undefined);assert.equal(seen,0);
 f.feature.destroy();
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

test('received preview authority accepts only the current associated raster',async()=>{
 const view={...base,people_copy_instance:'1'.repeat(32),access:'read',state:'ended'};
 const file={id:'0123456789abcdef',sha256:'a'.repeat(64),name:'synthetic.png',mime:'image/png',size:1};
 const message={seq:1,anchor:0,author:'a',text:'',attachments:[file]};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),async()=>commentsAnswer([message],1,false));
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
 const path='/api/team/subscriptions/s/o/c/g/attachments/1/'+file.id+'?copy_instance='+view.people_copy_instance+'&preview=1';
 const turn={dataset:{cseq:'1'}},anchor={dataset:{tkey:file.id},closest:()=>turn,getAttribute:()=>path};
 assert.deepEqual(f.feature.sharedPreview(anchor),{url:path,name:file.name});
 anchor.getAttribute=()=> 'https://example.invalid/forged';assert.equal(f.feature.sharedPreview(anchor),null);
 anchor.getAttribute=()=>path;anchor.dataset.tkey='fedcba9876543210';assert.equal(f.feature.sharedPreview(anchor),null);
 anchor.dataset.tkey=file.id;turn.dataset.cseq='0';assert.equal(f.feature.sharedPreview(anchor),null);
 turn.dataset.cseq='9007199254740992';assert.equal(f.feature.sharedPreview(anchor),null);turn.dataset.cseq='1';
 message._peopleRetained={[file.id]:false};assert.equal(f.feature.sharedPreview(anchor),null);delete message._peopleRetained;
 file.mime='text/html';assert.equal(f.feature.sharedPreview(anchor),null);file.mime='image/png';
 delete anchor.dataset.tkey;assert.equal(f.feature.sharedPreview(anchor),null);anchor.dataset.tkey=file.id;
 assert.equal(f.feature.sharedPreview({dataset:{tkey:file.id},closest:()=>null}),null);
 f.navigate();assert.equal(f.feature.sharedPreview(anchor),null);f.feature.destroy();
});

test('received preview authority refuses a copy without its local identity',async()=>{
 const view={...base,access:'read'},file={id:'0123456789abcdef',sha256:'a'.repeat(64),name:'synthetic.png',mime:'image/png',size:1};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),async()=>commentsAnswer([{seq:1,anchor:0,author:'a',attachments:[file]}],1,false));
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
 assert.equal(f.feature.sharedPreview({dataset:{tkey:file.id},closest:()=>({dataset:{cseq:'1'}})}),null);f.feature.destroy();
});

test('received preview authority rejects unavailable assets and damaged descriptors',async()=>{
 const view={...base,people_copy_instance:'1'.repeat(32),access:'read'};let available=false;
 const file={id:'0123456789abcdef',sha256:'a'.repeat(64),name:'synthetic.png',mime:'image/png',size:1};
 const message={seq:1,anchor:0,author:'a',text:'',attachments:[file]};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),async()=>commentsAnswer([message],1,false),undefined,{assetURL:path=>available?path:''});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
 const path='/api/team/subscriptions/s/o/c/g/attachments/1/'+file.id+'?copy_instance='+view.people_copy_instance+'&preview=1';
 const turn={dataset:{cseq:'1'}},anchor={dataset:{tkey:file.id},closest:()=>turn,getAttribute:()=>path};
 assert.equal(f.feature.sharedPreview(anchor),null);available=true;assert.ok(f.feature.sharedPreview(anchor));
 file.sha256='damaged';assert.equal(f.feature.sharedPreview(anchor),null);f.feature.destroy();
});

test('removing a People copy carries its captured local identity', async () => {
 const instance='a'.repeat(32),sub={...base,people_copy_instance:instance};let removed='';
 const f=nativeViewFixture(async(path,opts)=>{if(path==='/api/team/subscriptions')return list([sub]);if(opts?.method==='DELETE'){removed=path;return {}}return readAnswer([],0,false,{people_copy_instance:instance});});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();f.click('guestremove');await f.feature.settled();
 assert.equal(new URL(removed,'http://localhost').searchParams.get('copy_instance'),instance);f.feature.destroy();
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
 assert.equal(f.feature.tab.selected(),'team/s/o/c/g'); assert.match(item.turn.nextElementSibling.innerHTML,/data-gact="more"/); f.feature.destroy();
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
 assert.equal(prepared[0][2](),true);
 const copy={dataset:{gact:'copy'},closest:s=>s==='[data-gact]'?copy:null};f.click('msgs',copy);
 assert.equal(prepared[0][2](),false,'a later action must retire the preparation callback');
 f.feature.destroy();
 const g=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([],0),comments,undefined,{panel});
 g.feature.tick(); await g.feature.settled(); g.feature.selectActivity('team/s/o/c/g'); await g.feature.settled();
 const gt={dataset:{cseq:'1'},nextElementSibling:null,insertAdjacentHTML(_where,html){this.rowHTML=html;this.nextElementSibling={remove(){}}}};
 const gb={closest:s=>s==='.bubble'?gb:s==='[data-seq], [data-cseq]'?gt:null};
 g.click('msgs',gb); assert.doesNotMatch(gt.rowHTML,/data-gact="prompt"/); g.click('msgs',action); assert.equal(prepared.length,1); g.feature.destroy();
});

test('preparing an owner-authored People message retains its own authorship', async () => {
 const owner={...base,access:'owner',local_node:'n1'},prepared=[];
 const panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){},promptIcon:'I',preparePrompt:(...args)=>prepared.push(args)};
 const comments=async()=>({entries:[{seq:1,anchor:0,text:'my own note',author:'self',mine:true,prov:true}],names:{self:'Owner'},next:1,more:false});
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([owner]):readAnswer([],0,false,owner),comments,undefined,{panel,selectedNode:()=>'n1'});
 f.feature.tick();await f.feature.settled();
 const turn={dataset:{cseq:'1',gkey:'s/o/c/g'},nextElementSibling:null,insertAdjacentHTML(){this.nextElementSibling={remove(){}}}};
 const bubble={closest:s=>s==='.bubble'?bubble:(s==='[data-cseq]'||s==='[data-seq], [data-cseq]')?turn:null};f.click('msgs',bubble);
 const action={dataset:{gact:'prompt'},closest:s=>s==='[data-gact]'?action:null};f.click('msgs',action);
 assert.equal(prepared[0][0].role,'user');assert.equal(prepared[0][0].who,undefined);assert.equal(prepared[0][0].prov,true);f.feature.destroy();
});

test('owner marker and unavailable action adapters cannot activate another action', async () => {
 const owner={...base,access:'owner',local_node:'n1'};
 const panel={ids:{},loaded(){},select(){},removed(){},ownerChanged(){},fork:null,preparePrompt:null};
 const comments=async()=>({entries:[{seq:1,anchor:0,text:'reply',author:'p'}],names:{p:'Alex'},next:1,more:false});
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([owner]):readAnswer([],0,false,owner),comments,undefined,{panel,selectedNode:()=>'n1'});
 f.feature.tick();await f.feature.settled();
 const turn={dataset:{cseq:'1',gkey:'s/o/c/g'},nextElementSibling:null,insertAdjacentHTML(){this.nextElementSibling={remove(){}}}};
 const bubble={closest:s=>s==='.bubble'?bubble:(s==='[data-cseq]'||s==='[data-seq], [data-cseq]')?turn:null};f.click('msgs',bubble);
 const marker={closest:s=>s==='[data-bmarker]'?marker:null};f.click('msgs',marker);
 for(const name of ['fork','prompt']) {const action={dataset:{gact:name},closest:s=>s==='[data-gact]'?action:null};assert.doesNotThrow(()=>f.click('msgs',action));}
 f.feature.destroy();
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
 assert.equal(body.scrollTop,body.scrollHeight);assert.match(body.innerHTML,/arrived/);assert.equal(f.doc.activeElement,item.turn.nextElementSibling.querySelector('[data-gact="more"]'));f.feature.destroy();
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
  assert.match(html,/>You<\/div>/);assert.match(html,/human message/);
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

test('coordinator: known incoming unread survives a later own append while tail classification is pending', async () => {
 let last=1, reads=0,release;
 const held=new Promise(resolve=>release=resolve);
 const incoming={seq:1,anchor:0,author:'other',mine:false,text:'synthetic incoming'};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([{...base,access:'owner',local_node:'local',mirror_last:0,seen_mirror:0,unread:true,comments_last:last,seen_comments:0}]):readAnswer(),async()=>{if(++reads>1)await held;return commentsAnswer(last===1?[incoming]:[incoming,{seq:2,anchor:0,author:'o',mine:true,text:'synthetic own append'}],last,false);});
 try{
  f.feature.tick();await f.feature.settled();assert.equal(f.feature.tab.ownerUnread('local'),true);
  last=2;f.feature.refresh();await new Promise(r=>setImmediate(r));
  assert.equal(f.feature.tab.ownerUnread('local'),true,'a later append cannot revoke already proven incoming unread evidence');
 }finally{release();await f.feature.settled();f.feature.destroy();}
});

test('known incoming evidence is retired when the read position advances past it', async () => {
 let last=1,seen=0;
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([{...base,access:'owner',local_node:'local',unread:true,comments_last:last,seen_comments:seen}]):readAnswer(),async path=>{
  const after=Number(path.split('after=')[1]);return after===0?commentsAnswer([{seq:1,mine:false,text:'incoming'}],1,false):commentsAnswer([{seq:2,mine:true,text:'own'}],2,false);
 });
 f.feature.tick();await f.feature.settled();assert.equal(f.feature.tab.ownerUnread('local'),true);
 last=2;seen=1;f.feature.refresh();await f.feature.settled();assert.equal(f.feature.tab.ownerUnreadStatus('local'),'read');f.feature.destroy();
});


test('coordinator: shared bubble selection uses the ordinary drag-selection guard', async () => {
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):readAnswer([{t:'assistant',seq:1,text:'Synthetic selectable text'}],1),undefined,undefined,{bookmarks:()=>[],uiMutate(){},openSendTo(){}});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
 f.doc.getSelection=()=>({isCollapsed:false,toString:()=> 'Synthetic selectable'});
 const item=sharedActionBubble(1);f.click('msgs',item.bubble);
 assert.equal(item.turn.rowHTML,undefined,'selecting text must not toggle bubble actions');
 f.feature.destroy();
});

test('description verification reports unavailable destinations and preserves the current visit', async () => {
 let menu,mode='missing';const notices=[],applied=[],nodes=[{id:'local',instance_id:'one',title:'Local'}];
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):path==='/api/state'?(mode==='error'?Promise.reject(Error('offline')):{}):readAnswer([{t:'user',seq:1,text:'human'}],1),undefined,undefined,{toast:value=>notices.push(value),localNodes:()=>nodes,panel:{chooseDescription:value=>menu=value,editDescription:id=>applied.push(id)}});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();const item=sharedActionBubble(1);f.click('msgs',item.bubble);
 const action={dataset:{gact:'desc'},closest:s=>s==='[data-gact]'?action:null};f.click('msgs',action);
 menu.onChoose(menu.choices[0]);await new Promise(resolve=>setImmediate(resolve));assert.equal(notices.length,1);assert.deepEqual(applied,[]);
 mode='error';menu.onChoose(menu.choices[0]);await new Promise(resolve=>setImmediate(resolve));assert.equal(notices.length,2);assert.deepEqual(applied,[]);f.feature.destroy();
});

test('another shared bubble action retires pending description verification', async () => {
 let menu,finish;const selected=[],copies=[],nodes=[{id:'local',instance_id:'one',title:'Local'}];
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([base]):path==='/api/state'?new Promise(resolve=>finish=resolve):readAnswer([{t:'user',seq:1,text:'human'}],1),undefined,undefined,{localNodes:()=>nodes,panel:{chooseDescription:value=>menu=value,editDescription:id=>selected.push(id),copy:text=>copies.push(text)}});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();const item=sharedActionBubble(1);f.click('msgs',item.bubble);
 const action=name=>({dataset:{gact:name},closest:s=>s==='[data-gact]'?{dataset:{gact:name}}:null});
 f.click('msgs',action('desc'));menu.onChoose(menu.choices[0]);await Promise.resolve();f.click('msgs',action('copy'));finish({nodes});await new Promise(resolve=>setImmediate(resolve));
 assert.deepEqual(selected,[]);assert.deepEqual(copies,['human']);f.feature.destroy();
});

test("People file publication keeps an uncertain manifest immutable across edits", async () => {
 const calls=[],paths=[];let fail=true;const values=new Map();const storage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};const view={...base,access:"write",people_copy_instance:"1".repeat(32)};
 const panel={ids:{},loaded(){},select(){},removed(){},changed(){}};
 const f=nativeViewFixture(async(path,opts)=>{
  if(path==="/api/team/subscriptions")return {...list([view],[{id:"s",delivery:"ok",people_files:"supported"}]),people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216}};
  if(path.split("?")[0].endsWith("/comments")&&opts?.method==="POST"){paths.push(path);calls.push(JSON.parse(opts.body));if(fail)throw Error("offline");return {seq:1}}
  return readAnswer([],3,false,view);
 },undefined,()=>new Uint8Array([1,2,3,4,5,6,7,8]),{panel,storage});
 f.feature.tick();await f.feature.settled();f.feature.selectActivity("team/s/o/c/g");await f.feature.settled();
 assert.equal(f.feature.peopleContext().attachments,true);
 await assert.rejects(f.feature.sendPeople("s/o/c/g","",["0123456789abcdef"]),/offline/);
 await assert.rejects(f.feature.sendPeople("s/o/c/g","newer text",["fedcba9876543210"]),/unconfirmed/i);assert.equal(calls.length,1);
 assert.equal(f.feature.pendingPeople().text, "");
 fail=false;const receipt=await f.feature.retryPeople();
 assert.deepEqual(calls,[{text:"",anchor:3,key:"0102030405060708",attachments:["0123456789abcdef"]},{text:"",anchor:3,key:"0102030405060708",attachments:["0123456789abcdef"]}]);
 assert.equal(paths.length,2);for(const path of paths)assert.equal(new URL(path,'http://local.invalid').searchParams.get('copy_instance'),view.people_copy_instance);
 assert.ok([...values.keys()].some(key=>key.startsWith("scimux-people-file-pending:")),"confirmed response cleared pending before durable browser acknowledgement");
 assert.equal(f.feature.finishPeople(receipt),true);
 assert.equal([...values.keys()].some(key=>key.startsWith("scimux-people-file-pending:")),false);
 f.feature.destroy();
});

test("People file capability requires the selected local copy identity", async () => {
 let view={...base,access:"write"};
 const f=nativeViewFixture(async path=>path==="/api/team/subscriptions"?{...list([view],[{id:"s",delivery:"ok",people_files:"supported"}]),people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216}}:readAnswer([],0,false,view));
 f.feature.tick();await f.feature.settled();f.feature.selectActivity("team/s/o/c/g");await f.feature.settled();
 assert.equal(f.feature.peopleContext().attachments,false);
 f.feature.destroy();
});

test("damaged People pending data never posts or deletes a retry record", async () => {
 const view={...base,access:'write',people_copy_instance:'1'.repeat(32)};
 const values=new Map(),storage={persistent:true,getItem:key=>values.get(key)??null,setItem:(key,value)=>values.set(key,value),removeItem:key=>values.delete(key)};
 const key='scimux-people-file-pending:s/o/c/g::copy:'+view.people_copy_instance;let posts=0;
 const f=nativeViewFixture(async(path,opts)=>{
  if(path==='/api/team/subscriptions')return {...list([view],[{id:'s',delivery:'ok',people_files:'supported'}]),people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216}};
  if(opts?.method==='POST'){posts++;return {seq:0}}
  return readAnswer([],0,false,view);
 },undefined,undefined,{storage});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
 for(const raw of ['{',JSON.stringify({instance:'',copyInstance:view.people_copy_instance,request:{text:'synthetic',anchor:0,key:'0123456789abcdef',attachments:['0123456789abcdef','0123456789abcdef']}})]) {
  values.set(key,raw);
  assert.throws(()=>f.feature.pendingPeople(),/recovery is unavailable/i);
  await assert.rejects(f.feature.retryPeople(),/recovery is unavailable/i);
  assert.equal(f.feature.finishPeople({peoplePending:{key,value:'different'}}),false);
  assert.equal(values.get(key),raw);
 }
 assert.equal(posts,0);
 const valid=JSON.stringify({instance:'',copyInstance:view.people_copy_instance,request:{text:'synthetic',anchor:0,key:'0123456789abcdef',attachments:['0123456789abcdef']}});
 values.set(key,valid);await assert.rejects(f.feature.retryPeople(),/delivery is unconfirmed/i);
 assert.equal(posts,1);assert.equal(values.get(key),valid);
 assert.equal(f.feature.finishPeople(null),false);
 assert.equal(f.feature.finishPeople({peoplePending:{key:'unrelated',value:valid}}),false);
 storage.persistent=false;assert.equal(f.feature.pendingPeople(),null);
 await assert.rejects(f.feature.retryPeople(),/recovery is unavailable/i);
 assert.equal(values.get(key),valid);assert.equal(posts,1);f.feature.destroy();
});

test('owner People retry preserves copy, configuration and native instance binding',async()=>{
 const view={...base,access:'owner',local_node:'n1',people_copy_instance:'1'.repeat(32)};
 const values=new Map(),storage={persistent:true,getItem:key=>values.get(key)??null,setItem:(key,value)=>values.set(key,value),removeItem:key=>values.delete(key)};
 let native='synthetic-instance',sent;
 const f=nativeViewFixture(async(path,opts)=>{
  if(path==='/api/team/subscriptions')return {...list([view],[{id:'s',delivery:'ok',people_files:'supported',people_file_binding:'binding'}]),people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216}};
  if(opts?.method==='POST'){sent={path,body:JSON.parse(opts.body)};return {seq:3}}
  return readAnswer([],0,false,view);
 },undefined,undefined,{storage,selectedNode:()=> 'n1',selectedUID:()=> 'c',localNodes:()=>[{id:'n1',instance_id:native}]});
 f.feature.tick();await f.feature.settled();f.feature.setOwnerShare('n1',{node:'n1',uid:'c',server:'s',generation:'g',state:'active'});await f.feature.settled();
 const key='scimux-people-file-pending:s/o/c/g:'+native+':binding:copy:'+view.people_copy_instance;
 const request={text:'synthetic',anchor:0,key:'0123456789abcdef',attachments:['fedcba9876543210']};
 values.set(key,JSON.stringify({instance:native,copyInstance:view.people_copy_instance,request}));
 const receipt=await f.feature.retryPeople();
 const url=new URL(sent.path,'http://local.invalid');
 assert.equal(url.searchParams.get('copy_instance'),view.people_copy_instance);assert.equal(url.searchParams.get('chat_instance'),native);assert.deepEqual(sent.body,request);
 assert.ok(values.has(key));assert.equal(f.feature.finishPeople(receipt),true);assert.equal(values.has(key),false);
 native='';assert.equal(f.feature.pendingPeople(),null);f.feature.destroy();
});

test('owner People file publication requires the current native instance',async()=>{
 const view={...base,access:'owner',local_node:'n1',people_copy_instance:'1'.repeat(32)};
 const values=new Map(),storage={persistent:true,getItem:key=>values.get(key)??null,setItem:(key,value)=>values.set(key,value),removeItem:key=>values.delete(key)};let posts=0;
 const f=nativeViewFixture(async(path,opts)=>{
  if(path==='/api/team/subscriptions')return {...list([view],[{id:'s',delivery:'ok',people_files:'supported',people_file_binding:'binding'}]),people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216}};
  if(opts?.method==='POST'){posts++;return {seq:1}}return readAnswer([],0,false,view);
 },undefined,undefined,{storage,selectedNode:()=> 'n1',selectedUID:()=> 'c',localNodes:()=>[{id:'other',instance_id:'unrelated'},{id:'n1',instance_id:'current'}]});
 f.feature.tick();await f.feature.settled();f.feature.setOwnerShare('n1',{node:'n1',uid:'c',server:'s',generation:'g',state:'active'});await f.feature.settled();
 await assert.rejects(f.feature.sendPeople('s/o/c/g','draft',['0123456789abcdef'],'old'),/identity is unavailable/i);assert.equal(posts,0);assert.equal(values.size,0);
 const receipt=await f.feature.sendPeople('s/o/c/g','draft',['0123456789abcdef'],'current');assert.equal(posts,1);assert.ok(receipt.peoplePending);f.feature.destroy();
});

test("file-only shared comment keeps its exact source and common attachment tile", () => {
 const entry={seq:7,anchor:0,author:"a",at:"2026-01-01T00:00:00Z",text:"",attachments:[{id:"0123456789abcdef",sha256:"a".repeat(64),name:"synthetic.png",mime:"image/png",size:12}]};
 const capture=guestModule.guestCapture(entry,base,{a:"Ada"});
 assert.ok(capture);assert.equal(capture.text,"");assert.equal(capture.team.seq,7);assert.deepEqual(capture.attachments.map(file=>file.id),["0123456789abcdef"]);
 const html=guestMessageHTML(entry,{a:"Ada"},"s",{},"","","s/o/c/g");
 assert.match(html,/class="attthumb"/);assert.match(html,/\/api\/team\/subscriptions\/s\/o\/c\/g\/attachments\/7\/0123456789abcdef\?preview=1/);assert.match(html,/tabindex="0"/);
});

test("shared attachment tiles use the remote asset transport for downloads and previews", () => {
 const entry={seq:7,anchor:0,author:"a",text:"",attachments:[{id:"0123456789abcdef",sha256:"a".repeat(64),name:"synthetic.png",mime:"image/png",size:12}]};
 const html=guestModule.guestMessageHTML(entry,{},"s",{},"","","s/o/c/g",path=>"/remote-asset?path="+encodeURIComponent(path));
 assert.match(html,/href="\/remote-asset\?path=/);assert.match(html,/src="\/remote-asset\?path=/);
});

test("shared attachment with unavailable remote bytes does not link to the current page", () => {
 const entry={seq:7,anchor:0,author:"a",text:"",attachments:[{id:"0123456789abcdef",sha256:"a".repeat(64),name:"synthetic.png",mime:"image/png",size:12}]};
 const html=guestModule.guestMessageHTML(entry,{},"s",{},"","","s/o/c/g",()=>"");
 assert.doesNotMatch(html,/href=""|src=""/);assert.match(html,/unavailable/i);
});

test("a missing local copy identity never creates an unbound file link", () => {
 const entry={seq:7,anchor:0,author:"a",text:"",attachments:[{id:"0123456789abcdef",sha256:"a".repeat(64),name:"synthetic.txt",mime:"text/plain",size:1}]};
 const html=guestModule.guestMessageHTML(entry,{},"s",{},"","","s/o/c/g",path=>path,"");
 assert.match(html,/unavailable/i);assert.doesNotMatch(html,/href="\/api\/team\/subscriptions/);assert.doesNotMatch(html,/data-attachment-retry=/);
});

test("a People file that could not be retained is labelled truthfully", () => {
 const entry={seq:7,anchor:0,author:"a",text:"",_peopleRetained:{"0123456789abcdef":false},attachments:[{id:"0123456789abcdef",sha256:"a".repeat(64),name:"synthetic.txt",mime:"text/plain",size:12}]};
 const html=guestModule.guestMessageHTML(entry,{},"s",{},"","","s/o/c/g");
 assert.match(html,/not retained/i);assert.match(html,/retry download/i);
});

test('invalid shared attachment manifests never produce file links',()=>{
 const file={id:'0123456789abcdef',sha256:'a'.repeat(64),name:'synthetic.txt',mime:'text/plain',size:4194304};
 const oversized=Array.from({length:5},(_,i)=>({...file,id:i.toString(16).padStart(16,'0')}));
 for(const attachments of [null,[],[{...file,name:'../escape'}],[file,file],oversized]) {
  let calls=0;const entry={seq:7,anchor:0,author:'a',text:'synthetic',attachments};
  const html=guestModule.guestMessageHTML(entry,{},'s',{},'','','s/o/c/g',()=>{calls++;return 'blob:unexpected'});
  assert.match(html,/Attachments unavailable/);assert.doesNotMatch(html,/blob:unexpected/);assert.equal(calls,0);
 }
});

test('a malformed shared attachment source key cannot produce a file URL',()=>{
 const entry={seq:7,anchor:0,author:'a',text:'synthetic',attachments:[{id:'0123456789abcdef',sha256:'a'.repeat(64),name:'synthetic.txt',mime:'text/plain',size:1}]};
 let calls=0;const html=guestModule.guestMessageHTML(entry,{},'s',{},'','',{invalid:true},()=>{calls++;return 'blob:unexpected'});
 assert.equal(calls,0);assert.doesNotMatch(html,/blob:unexpected|data-attachment-retry/);
});

test("received People files are handed to bounded retention before their page renders", async()=>{
 const view={...base,access:"write",people_copy_instance:"1".repeat(32)};const retained=[];
 const message={seq:1,anchor:0,author:"a",text:"",attachments:[{id:"0123456789abcdef",sha256:"a".repeat(64),name:"synthetic.txt",mime:"text/plain",size:1}]};
 const f=nativeViewFixture(async path=>path==="/api/team/subscriptions"?{...list([view],[{id:"s",delivery:"ok",people_files:"supported"}]),people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216}}:readAnswer([],0,false,view),async()=>commentsAnswer([message],1,false),undefined,{retainPeople:async(key,entries)=>{retained.push({key,entries});}});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
 assert.equal(retained.length,1);assert.equal(retained[0].key,"s/o/c/g");assert.equal(retained[0].entries[0].attachments[0].name,"synthetic.txt");f.feature.destroy();
});

test('received retention failure preserves the discussion without inventing availability',async()=>{
 const view={...base,access:'read',people_copy_instance:'1'.repeat(32)};
 const message={seq:1,anchor:0,author:'a',text:'synthetic retained caption',attachments:[{id:'0123456789abcdef',sha256:'a'.repeat(64),name:'synthetic.txt',mime:'text/plain',size:1}]};
 let attempts=0;const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?{...list([view],[{id:'s',delivery:'ok',people_files:'supported'}]),people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216}}:readAnswer([],0,false,view),async()=>commentsAnswer([message],1,false),undefined,{retainPeople:async()=>{attempts++;throw Error('synthetic retention failure')}});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
 assert.equal(attempts,1);assert.match(f.get('msgs').innerHTML,/synthetic retained caption/);assert.equal(message._peopleRetained,undefined);f.feature.destroy();
});

test('closing or destroying a received panel aborts its in-flight file retention',async()=>{
 for(const action of ['close','destroy']) {
  const view={...base,access:'read',people_copy_instance:'1'.repeat(32)};
  const message={seq:1,anchor:0,author:'a',text:'synthetic stale caption',attachments:[{id:'0123456789abcdef',sha256:'a'.repeat(64),name:'synthetic.txt',mime:'text/plain',size:1}]};
  let entered;const started=new Promise(resolve=>{entered=resolve});
  const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?{...list([view],[{id:'s',delivery:'ok',people_files:'supported'}]),people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216}}:readAnswer([],0,false,view),async()=>commentsAnswer([message],1,false),undefined,{retainPeople:async(_key,_entries,signal)=>{entered(signal);return new Promise((_resolve,reject)=>signal.addEventListener('abort',()=>reject(Error('synthetic abort')),{once:true}))}});
  f.feature.tick();await f.feature.settled();f.open();const signal=await started;
  if(action==='close')f.navigate();else f.feature.destroy();
  assert.equal(signal.aborted,true);await f.feature.settled();assert.doesNotMatch(f.get('msgs').innerHTML,/synthetic stale caption/);
  if(action==='close')f.feature.destroy();
 }
});

test("retention results stay scoped to each comment and show missing bytes", async()=>{
 const view={...base,access:"write",people_copy_instance:"1".repeat(32)},file={id:"0123456789abcdef",sha256:"a".repeat(64),name:"synthetic.txt",mime:"text/plain",size:1};
 const first={seq:1,anchor:0,author:"a",text:"",attachments:[file]},second={seq:2,anchor:0,author:"a",text:"",attachments:[{...file,name:"second.txt"}]};
 const f=nativeViewFixture(async path=>path==="/api/team/subscriptions"?{...list([view],[{id:"s",delivery:"ok",people_files:"supported"}]),people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216}}:readAnswer([],0,false,view),async()=>commentsAnswer([first,second],2,false),undefined,{retainPeople:async()=>({files:{"1/0123456789abcdef":true,"2/0123456789abcdef":false}})});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
 assert.equal(first._peopleRetained[file.id],true);assert.equal(second._peopleRetained[file.id],false);
 assert.doesNotMatch(guestModule.guestMessageHTML(first,{},"s",{},"","","s/o/c/g"),/not retained/i);
 assert.match(guestModule.guestMessageHTML(second,{},"s",{},"","","s/o/c/g"),/not retained/i);
 f.feature.destroy();
});

test('retention results cannot authorize a malformed neighboring comment',async()=>{
 const view={...base,access:'write',people_copy_instance:'1'.repeat(32)},file={id:'0123456789abcdef',sha256:'a'.repeat(64),name:'synthetic.txt',mime:'text/plain',size:1};
 const valid={seq:1,anchor:0,author:'a',text:'valid',attachments:[file]},invalid={seq:2,anchor:0,author:'a',text:'invalid',attachments:[{...file,name:'../escape'}]};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?{...list([view],[{id:'s',delivery:'ok',people_files:'supported'}]),people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216}}:readAnswer([],0,false,view),async()=>commentsAnswer([valid,invalid],2,false),undefined,{retainPeople:async()=>({files:{'1/0123456789abcdef':true,'2/0123456789abcdef':true}})});
 f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();
 assert.equal(valid._peopleRetained[file.id],true);assert.equal(invalid._peopleRetained,undefined);
 assert.match(guestModule.guestMessageHTML(invalid,{},'s',{},'','','s/o/c/g'),/Attachments unavailable/);f.feature.destroy();
});

test("shared attachment capture does not carry arbitrary paths or URLs", () => {
 const entry={seq:7,anchor:0,author:"a",text:"",attachments:[{id:"0123456789abcdef",name:"synthetic.png",mime:"image/png",size:12,sha256:"a".repeat(64),path:"/synthetic/private",url:"https://example.invalid/file"}]};
 const capture=guestModule.guestCapture(entry,base,{a:"Ada"});
 assert.ok(capture);assert.equal("path" in capture.attachments[0],false);assert.equal("url" in capture.attachments[0],false);assert.equal(capture.attachments[0].id,"0123456789abcdef");
});

test('a verified remote download stays available when raster preview is unavailable',()=>{
 const file={id:'0123456789abcdef',sha256:'a'.repeat(64),name:'synthetic.png',mime:'image/png',size:12};
 const entry={seq:7,anchor:0,author:'a',text:'',attachments:[file]};
 const html=guestModule.guestMessageHTML(entry,{},'s',{},'','','s/o/c/g',path=>path.endsWith('?preview=1')?'':'blob:retained');
 assert.match(html,/synthetic\.png/);assert.match(html,/preview unavailable/i);assert.match(html,/href="blob:retained"/);assert.match(html,/download=/);
});

test('remote unavailable association offers a scoped explicit retry control',()=>{
 const file={id:'0123456789abcdef',sha256:'a'.repeat(64),name:'synthetic.txt',mime:'text/plain',size:1};const entry={seq:7,anchor:0,author:'a',text:'',_peopleRetained:{[file.id]:false},attachments:[file]};
 const html=guestModule.guestMessageHTML(entry,{},'s',{},'','','s/o/c/g',()=> '');
 assert.match(html,/data-attachment-retry="\/api\/team\/subscriptions\/s\/o\/c\/g\/attachments\/7\/0123456789abcdef"/);assert.match(html,/>Retry download<\/button>/);
});

test('retained owner file authority binds exact copy, descriptor and native instance',async()=>{
 const view={...base,access:'owner',state:'ended',local_node:'n1',people_copy_instance:'1'.repeat(32)};const file={id:'0123456789abcdef',sha256:'a'.repeat(64),name:'synthetic.txt',mime:'text/plain',size:1};let subscriptions=[view],binding='one',instance='original';
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list(subscriptions,[{id:'s',people_file_binding:binding}]):readAnswer([],0,false,view),async()=>commentsAnswer([{seq:1,anchor:0,text:'retained',author:'p',attachments:[file]}],1),undefined,{selectedNode:()=> 'n1',selectedUID:()=> 'c',localAgentAvailable:()=>true,localNodes:()=>[{id:'n1',instance_id:instance}]});
 f.feature.tick();await f.feature.settled();const source={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:1};
 const scope=f.feature.capturePromptAuthority(source,[file]);assert.ok(scope);assert.equal(scope.current(),true);
 instance='';assert.equal(f.feature.capturePromptAuthority(source,[file]),null);assert.equal(scope.current(),false);instance='original';
 assert.equal(f.feature.capturePromptAuthority(source,[{...file,name:'different'}]),null);assert.equal(f.feature.capturePromptAuthority({...source,generation:'other'},[file]),null);
 instance='replacement';assert.equal(scope.current(),false);instance='original';binding='two';f.setTime(10000);f.feature.tick();await f.feature.settled();assert.equal(scope.current(),false);
 binding='one';subscriptions=[];f.setTime(20000);f.feature.tick();await f.feature.settled();subscriptions=[view];f.setTime(30000);f.feature.tick();await f.feature.settled();assert.equal(scope.current(),false);f.feature.destroy();
});

test('owner preparation authority requires every ordered retained descriptor',async()=>{
 const view={...base,access:'owner',state:'ended',local_node:'n1',people_copy_instance:'1'.repeat(32)};
 const first={id:'0123456789abcdef',sha256:'a'.repeat(64),name:'first.txt',mime:'text/plain',size:1},second={id:'fedcba9876543210',sha256:'b'.repeat(64),name:'second.txt',mime:'text/plain',size:2};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),async()=>commentsAnswer([{seq:1,anchor:0,text:'retained',author:'p',attachments:[first,second]}],1),undefined,{selectedNode:()=> 'n1',selectedUID:()=> 'c',localAgentAvailable:()=>true,localNodes:()=>[{id:'n1',instance_id:'synthetic-instance'}]});
 f.feature.tick();await f.feature.settled();const source={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:1};
 const valid=f.feature.capturePromptAuthority(source,[first,second]);assert.ok(valid);assert.equal(valid.current(),true);
 for(const attempted of [null,[],[first],[second,first],[first,{...second,sha256:'c'.repeat(64)}],[first,{...second,size:1}],[first,{...second,mime:'image/png'}]]) assert.equal(f.feature.capturePromptAuthority(source,attempted),null);
 f.feature.destroy();
});

test('owner file preparation refuses a retained copy without its local identity',async()=>{
 const view={...base,access:'owner',state:'ended',local_node:'n1'};
 const file={id:'0123456789abcdef',sha256:'a'.repeat(64),name:'synthetic.txt',mime:'text/plain',size:1};
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),async()=>commentsAnswer([{seq:1,anchor:0,text:'retained',author:'p',attachments:[file]}],1),undefined,{selectedNode:()=> 'n1',selectedUID:()=> 'c',localAgentAvailable:()=>true,localNodes:()=>[{id:'n1',instance_id:'synthetic-instance'}]});
 f.feature.tick();await f.feature.settled();
 assert.equal(f.feature.capturePromptAuthority({server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:1},[file]),null);
 f.feature.destroy();
});

test('retained file authority stops on feature disable, selection or source retirement',async()=>{
 const view={...base,access:'owner',state:'ended',local_node:'n1',people_copy_instance:'1'.repeat(32)};const file={id:'0123456789abcdef',sha256:'a'.repeat(64),name:'synthetic.txt',mime:'text/plain',size:1};let enabled=true,selected='n1',uid='c',agent=true;
 const f=nativeViewFixture(async path=>{if(path==='/api/team/subscriptions'){if(!enabled)throw Object.assign(Error('disabled'),{status:404});return list([view])}return readAnswer([],0,false,view)},async()=>commentsAnswer([{seq:1,anchor:0,text:'retained',author:'p',attachments:[file]}],1),undefined,{selectedNode:()=>selected,selectedUID:()=>uid,localAgentAvailable:()=>agent,localNodes:()=>[{id:'n1',instance_id:'original'}]});
 f.feature.tick();await f.feature.settled();const source={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:1};const scope=f.feature.capturePromptAuthority(source,[file]);assert.ok(scope);assert.equal(scope.current(),true);
 selected='n2';assert.equal(scope.current(),false);selected='n1';uid='other';assert.equal(scope.current(),false);uid='c';agent=false;assert.equal(scope.current(),false);agent=true;
 enabled=false;f.setTime(10000);f.feature.tick();await f.feature.settled();assert.equal(scope.current(),false);assert.equal(f.feature.capturePromptAuthority(source,[file]),null);f.feature.destroy();
});

test('a replacement local copy must retire the failed prior panel even under the same durable chat address', async()=>{
 let phase=0;const original={...base,access:'write',mirror_last:0,seen_mirror:0,people_copy_instance:'a'.repeat(32)},replacement={...original,people_copy_instance:'b'.repeat(32)};
 const capabilities=view=>({...list([view],[{id:base.server,people_files:'supported',people_file_binding:'stable'}]),people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216}});
 const f=nativeViewFixture(async(path,opts)=>{
  if(path==='/api/team/subscriptions')return capabilities(phase===2?replacement:original);
  if(opts?.method==='PUT')return {};
  if(phase===1)throw Object.assign(new Error('{"error":"not_found"}'),{status:404});
  return readAnswer([],0,false,phase===2?replacement:original);
 });
 try{
  f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();assert.equal(f.feature.peopleContext().attachments,true);
  phase=1;f.setTime(5000);f.feature.tick();await f.feature.settled();assert.equal(f.feature.peopleContext().writable,false);
  phase=2;f.feature.refresh();await f.feature.settled();f.open();await f.feature.settled();
  assert.equal(f.feature.peopleContext().writable,true,'explicitly opening the listed replacement must not retain prior not-found failure');
  assert.equal(f.feature.peopleContext().attachments,true,'replacement must use its current copy identity for attachment capability');
  assert.equal(f.get('guestnote').textContent,'');
 }finally{f.feature.destroy();}
});

test('owner copy replacement retires a delayed old history response',async()=>{
 const original={...base,access:'owner',local_node:'n1',people_copy_instance:'a'.repeat(32)},replacement={...original,people_copy_instance:'b'.repeat(32)};
 let view=original,release,entered;const started=new Promise(resolve=>{entered=resolve});
 const f=nativeViewFixture(async path=>{
  if(path==='/api/team/subscriptions')return list([view]);
  if(path.includes('copy_instance='+original.people_copy_instance)){entered();return new Promise(resolve=>{release=()=>resolve(readAnswer([{t:'user',seq:1,text:'old retained'}],1,false,original))})}
  return readAnswer([{t:'user',seq:1,text:'replacement retained'}],1,false,replacement);
 },undefined,undefined,{selectedNode:()=> 'n1'});
 try {
  f.feature.tick();await started;view=replacement;f.feature.refresh();await new Promise(resolve=>setImmediate(resolve));
  release();await f.feature.settled();const snapshots=f.feature.ownerSnapshots();assert.equal(snapshots.length,1);assert.equal(snapshots[0].view.people_copy_instance,replacement.people_copy_instance);
  assert.deepEqual(snapshots[0].mirrors.map(item=>item.text),['replacement retained']);
 } finally {if(release)release();f.feature.destroy()}
});

test('unread evidence from an earlier copy is not reused by its replacement',async()=>{
 const original={...base,access:'write',state:'live',unread:true,comments_last:1,seen_comments:0,mirror_last:0,seen_mirror:0,people_copy_instance:'a'.repeat(32)},replacement={...original,people_copy_instance:'b'.repeat(32)};let view=original;
 const f=nativeViewFixture(async path=>path==='/api/team/subscriptions'?list([view]):readAnswer([],0,false,view),async()=>commentsAnswer([{seq:1,anchor:0,text:'synthetic',author:'p',mine:view===replacement}],1,false));
 try {f.feature.tick();await f.feature.settled();assert.equal(f.feature.tab.activities()[0].unread,true);view=replacement;f.feature.refresh();await f.feature.settled();assert.equal(f.feature.tab.activities()[0].unread,false)} finally {f.feature.destroy()}
});

test('a delayed received-copy response cannot replace the current panel',async()=>{
 const original={...base,access:'write',people_copy_instance:'a'.repeat(32)},replacement={...original,people_copy_instance:'b'.repeat(32)};let view=original,release,entered;
 const started=new Promise(resolve=>{entered=resolve});
 const f=nativeViewFixture(async path=>{
  if(path==='/api/team/subscriptions')return list([view]);
  if(path.includes('copy_instance='+original.people_copy_instance)){entered();return new Promise(resolve=>{release=()=>resolve(readAnswer([{t:'user',seq:1,text:'old response'}],1,false,original))})}
  return readAnswer([{t:'user',seq:1,text:'replacement response'}],1,false,replacement);
 });
 try {f.feature.tick();await f.feature.settled();f.open();await started;view=replacement;f.feature.refresh();await new Promise(resolve=>setImmediate(resolve));release();await f.feature.settled();assert.match(f.get('msgs').innerHTML,/replacement response/);assert.doesNotMatch(f.get('msgs').innerHTML,/old response/)} finally {if(release)release();f.feature.destroy()}
});

test('refreshing and reselecting an unchanged copy preserves its panel',async()=>{
 const view={...base,access:'write',people_copy_instance:'a'.repeat(32)};let reads=0;
 const f=nativeViewFixture(async path=>{if(path==='/api/team/subscriptions')return list([{...view}]);reads++;return readAnswer([{t:'user',seq:1,text:'stable response'}],1,false,{...view})});
 try {f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();const before=f.get('msgs').innerHTML,count=reads;f.feature.refresh();await f.feature.settled();f.open();await f.feature.settled();assert.equal(f.get('msgs').innerHTML,before);assert.equal(reads,count)} finally {f.feature.destroy()}
});

test('read-position updates carry the selected copy identity',async()=>{
 const view={...base,access:'write',people_copy_instance:'a'.repeat(32),seen_mirror:0,mirror_last:1};const seen=[];
 const f=nativeViewFixture(async(path,opts)=>{if(path==='/api/team/subscriptions')return list([view]);if(opts?.method==='PUT'){seen.push(path);return {}}return readAnswer([{t:'user',seq:1,text:'synthetic'}],1,false,view)});
 try {f.feature.tick();await f.feature.settled();f.open();await f.feature.settled();assert.equal(seen.length,1);assert.equal(new URL(seen[0],'http://local.invalid').searchParams.get('copy_instance'),view.people_copy_instance)} finally {f.feature.destroy()}
});
