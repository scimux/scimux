import test from "node:test";
import assert from "node:assert/strict";
import { jsonResponse } from "./s2-helpers.js";
import { decodeResponse } from "../js/api.js";
import { createGuestFeature, guestGroups, guestKey, guestStateText, guestErrorText, guestEntryHTML } from "../js/team-guest.js";

const base = { server: "s", server_name: "Server", owner: "o", owner_name: "Owner", chat: "c", generation: "g", title: "Chat", access: "read", state: "live", mirror_last: 1, seen_mirror: 0, unread: false, first_seen: "1" };
const list = (subscriptions = [], servers = [], truncated = false) => ({ subscriptions: subscriptions.map(view => ({ ...view })), servers, truncated });
function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
function fixture(api) {
  let time = 0, invalidations = 0;
  const listeners = new Map();
  const el = { innerHTML: "", addEventListener(type, fn) { listeners.set(type, fn); }, removeEventListener(type) { listeners.delete(type); }, querySelector() { return null; }, contains() { return false; } };
  const doc = { activeElement: null, querySelector(sel) { if (sel === "#cardlist") return el; return { addEventListener() {}, removeEventListener() {} }; } };
  const feature = createGuestFeature({ api, doc, now: () => time, invalidateCards: () => invalidations++ });
  return { feature, el, doc, listeners, setTime(n) { time = n; }, invalidations: () => invalidations };
}

test("guest keys identify the subscription", () => {
  assert.equal(guestKey(base), "s/o/c/g");
});

test("gate off is probed once and hides the tab", async () => {
  let calls = 0;
  const f = fixture(async () => { calls++; throw Object.assign(new Error("not found"), { status: 404 }); });
  f.feature.tick(); await f.feature.settled();
  assert.equal(f.feature.tab.tabHTML(false), "");
  assert.equal(f.feature.tab.render(f.el), false);
  for (let i = 0; i < 10; i++) { f.setTime((i + 1) * 10000); f.feature.tick(); }
  await f.feature.settled(); assert.equal(calls, 1);
});

test("list polling throttles, ignores equal answers and retains the last list on failure", async () => {
  const pending = deferred();
  const answers = [list(), list([base]), list([base]), new Error("offline"), pending.promise];
  let calls = 0;
  const f = fixture(async () => { const a = answers[calls++]; if (a instanceof Error) throw a; return a; });
  f.feature.tick(); await f.feature.settled();
  assert.equal(f.invalidations(), 0); assert.equal(f.feature.tab.tabHTML(false), ""); assert.equal(f.feature.tab.render(f.el), false);
  f.setTime(9999); f.feature.tick(); assert.equal(calls, 1);
  f.setTime(10000); f.feature.tick(); await f.feature.settled(); assert.equal(f.invalidations(), 1);
  assert.match(f.feature.tab.tabHTML(false), /Shared with me/); assert.doesNotMatch(f.feature.tab.tabHTML(false), /tabbadge/);
  f.setTime(20000); f.feature.tick(); await f.feature.settled(); assert.equal(f.invalidations(), 1);
  f.setTime(30000); f.feature.tick(); await f.feature.settled(); assert.equal(f.invalidations(), 1); assert.match(f.feature.tab.tabHTML(false), /Shared with me/);
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

test("list renders escaped labels, unread, ended, empty title, and truncation", async () => {
  const f = fixture(async () => list([{ ...base, owner_name: '<&"', title: '<&"', unread: true }, { ...base, chat: "d", owner: "other", title: "", state: "ended" }], [], true));
  f.feature.bind(); f.feature.tick(); await f.feature.settled();
  assert.equal(f.feature.tab.render(f.el), true);
  for (const text of ["&lt;&amp;&quot;", "unread", "Untitled chat", "Sharing ended", "Some shared chats are not listed.", 'aria-expanded="true"']) assert.ok(f.el.innerHTML.includes(text), text);
  const group = { dataset: { group: "s/o" }, closest(sel) { if (sel === "[data-group]") return this; return null; } };
  f.listeners.get("click")({ target: group });
  assert.match(f.el.innerHTML, /aria-expanded="false"/);
  f.listeners.get("click")({ target: group });
  assert.match(f.el.innerHTML, /aria-expanded="true"/);
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

test("render avoids needless writes and restores list focus after rebuilding", async () => {
  const f = fixture(async () => list([base]));
  let value = "", writes = 0, focused = 0;
  Object.defineProperty(f.el, "innerHTML", { get() { return value; }, set(v) { value = v; writes++; } });
  const old = { dataset: { sub: "s/o/c/g" }, closest() { return this; } };
  const replacement = { focus() { focused++; } };
  f.el.contains = node => node === old;
  f.el.querySelector = () => replacement;
  f.feature.tick(); await f.feature.settled();
  f.feature.tab.render(f.el); f.feature.tab.render(f.el); assert.equal(writes, 1);
  value = ""; f.doc.activeElement = old; f.feature.tab.render(f.el); assert.equal(writes, 2); assert.equal(focused, 1);
  old.dataset = { group: "s/o" }; value = ""; f.feature.tab.render(f.el); assert.equal(focused, 2);
});

function viewFixture(api) {
  let time = 0, invalidations = 0;
  const elements = new Map();
  const doc = { activeElement: null, querySelector(sel) { return elements.get(sel); } };
  for (const id of ["cardlist", "cardtabs", "sharedview", "sv_scrim", "sv_back", "sv_title", "sv_from", "sv_state", "sv_body", "sv_more", "sv_note", "sv_remove"]) {
    const listeners = new Map();
    const el = { id, innerHTML: "", textContent: "", hidden: false, dataset: {}, scrollTop: 0, scrollHeight: 1000, clientHeight: 200, listeners,
      addEventListener(type, fn) { listeners.set(type, fn); }, removeEventListener(type) { listeners.delete(type); },
      focus() { doc.activeElement = el; }, contains(node) { return node === el; },
      querySelector() { return null; }, insertAdjacentHTML(_where, html) { this.innerHTML += html; } };
    elements.set("#" + id, el);
  }
  const feature = createGuestFeature({ api, doc, now: () => time, invalidateCards: () => invalidations++ });
  const get = id => elements.get("#" + id);
  const click = (id, target = get(id)) => get(id).listeners.get("click")({ target });
  const item = { dataset: { sub: guestKey(base) }, closest(sel) { if (sel === "[data-sub]") return this; return null; }, focus() { doc.activeElement = item; } };
  get("cardlist").querySelector = () => item;
  get("cardtabs").querySelector = () => get("cardtabs");
  feature.bind();
  return { feature, get, click, item, doc, setTime(v) { time = v; }, invalidations: () => invalidations };
}

const readAnswer = (entries = [], next = 0, more = false, over = {}) => ({ view: { ...base, delivery: "ok", ...over }, entries, last: next, next, more });

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

test("open loads pages, updates the header and closes to the item", async () => {
  const paths = [];
  const f = viewFixture(async (path) => { paths.push(path); if (path === "/api/team/subscriptions") return list([base]); if (path.endsWith("after=0")) return readAnswer([{ t: "user", seq: 1, text: "synthetic", time: "" }], 1, true); if (path.endsWith("after=1")) return readAnswer([], 2, true); if (path.endsWith("after=2")) return readAnswer([], 3); return ""; });
  f.feature.tick(); await f.feature.settled(); paths.length = 0;
  f.click("cardlist", f.item); assert.equal(f.doc.activeElement, f.get("sv_back")); assert.equal(f.get("sharedview").hidden, false);
  await f.feature.settled();
  assert.deepEqual(paths.slice(0, 3), ["/api/team/subscriptions/s/o/c/g?after=0", "/api/team/subscriptions/s/o/c/g?after=1", "/api/team/subscriptions/s/o/c/g?after=2"]);
  assert.equal(f.get("sv_title").textContent, "Chat"); assert.equal(f.get("sv_from").textContent, "From Owner · Server"); assert.equal(f.get("sv_state").textContent, "Can view");
  assert.match(f.get("sv_body").innerHTML, /synthetic/);
  f.click("sv_back"); assert.equal(f.get("sharedview").hidden, true); assert.equal(f.doc.activeElement, f.item); assert.equal(f.get("sv_body").innerHTML, "");
});

test("empty view, paging cap and load more", async () => {
  let reads = 0;
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); if (path.endsWith("/seen")) return ""; reads++; if (reads === 41) return readAnswer([], 41, false); return readAnswer([], reads, true); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  assert.equal(reads, 40); assert.equal(f.get("sv_more").hidden, false); assert.match(f.get("sv_body").innerHTML, /Nothing has been shared yet/);
  f.click("sv_more"); await f.feature.settled(); assert.equal(reads, 41); assert.equal(f.get("sv_more").hidden, true);
});

test("late read after closing is dropped and escape closes", async () => {
  const pending = deferred();
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); return pending.promise; });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item);
  let stopped = false;
  f.get("sharedview").listeners.get("keydown")({ key: "Escape", stopPropagation() { stopped = true; } });
  assert.equal(stopped, true); assert.equal(f.get("sharedview").hidden, true);
  pending.resolve(readAnswer([{ t: "user", text: "late", seq: 1 }], 1)); await f.feature.settled();
  assert.equal(f.get("sv_body").innerHTML, "");
});

test("read failures show fixed notes and a later success clears them", async () => {
  for (const [code, expected] of [["not_found", "This chat is no longer in your list."], ["copy_damaged", "The saved copy of this chat is damaged and cannot be shown."], ["storage_full", "This computer's storage is full. Free some space to load more."], ["bad-json", "That did not work. It will be tried again."]]) {
    let reads = 0;
    const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); reads++; if (reads === 1) { if (code === "bad-json") throw new Error("not json"); throw new Error(JSON.stringify({ error: code })); } return readAnswer(); });
    f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled(); assert.equal(f.get("sv_note").textContent, expected);
    if (code !== "not_found") { f.setTime(5000); f.feature.tick(); await f.feature.settled(); assert.equal(f.get("sv_note").textContent, ""); }
  }
});

test("scrim closes and falls back to the shared tab if the item vanished", async () => {
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); return readAnswer(); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  f.get("cardlist").querySelector = () => null; f.click("sv_scrim");
  assert.equal(f.get("sharedview").hidden, true); assert.equal(f.doc.activeElement, f.get("cardtabs"));
});

test("opening another chat drops the first pending answer", async () => {
  const old = deferred(), second = { ...base, chat: "d" };
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base, second]); if (path.includes("/c/g?")) return old.promise; return readAnswer([{ t: "user", text: "second", seq: 1 }], 1, false, second); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item);
  const other = { dataset: { sub: guestKey(second) }, closest(sel) { if (sel === "[data-sub]") return this; return null; } };
  f.click("cardlist", other); old.resolve(readAnswer([{ t: "user", text: "stale", seq: 1 }], 1)); await f.feature.settled();
  assert.match(f.get("sv_body").innerHTML, /second/); assert.doesNotMatch(f.get("sv_body").innerHTML, /stale/);
});

test("following waits five seconds and never moves focus while reading", async () => {
  let reads = 0;
  const pending = deferred();
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); if (path.endsWith("/seen")) return ""; reads++; if (reads === 2) return pending.promise; return readAnswer(); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  f.setTime(4999); f.feature.tick(); assert.equal(reads, 1);
  const focused = f.doc.activeElement; f.setTime(5000); f.feature.tick(); assert.equal(reads, 2);
  f.setTime(10000); f.feature.tick(); assert.equal(reads, 2); assert.equal(f.doc.activeElement, focused);
  pending.resolve(readAnswer()); await f.feature.settled(); assert.equal(f.doc.activeElement, focused);
});

test("seen advances after successful reads and updates badge", async () => {
  const seen = [];
  const f = viewFixture(async (path, opts) => { if (path === "/api/team/subscriptions") return list([{ ...base, unread: true }]); if (path.endsWith("/seen")) { seen.push(opts); return ""; } return readAnswer([{ t: "user", seq: 1, text: "x" }], 1); });
  f.feature.tick(); await f.feature.settled(); const before = f.invalidations(); assert.match(f.feature.tab.tabHTML(false), /1 unread/);
  f.click("cardlist", f.item); await f.feature.settled();
  assert.equal(seen.length, 1); assert.equal(seen[0].method, "PUT"); assert.deepEqual(JSON.parse(seen[0].body), { mirror: 1 });
  assert.doesNotMatch(f.feature.tab.tabHTML(false), /unread/); assert.equal(f.invalidations(), before + 1);
  f.setTime(5000); f.feature.tick(); await f.feature.settled(); assert.equal(seen.length, 1);
});

test("failed seen is retried after another successful run", async () => {
  let puts = 0;
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([{ ...base, unread: true }]); if (path.endsWith("/seen")) { puts++; if (puts === 1) throw new Error("failed"); return ""; } return readAnswer([], 1); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  assert.match(f.feature.tab.tabHTML(false), /1 unread/); assert.equal(puts, 1);
  f.setTime(5000); f.feature.tick(); await f.feature.settled(); assert.equal(puts, 2); assert.doesNotMatch(f.feature.tab.tabHTML(false), /unread/);
});

test("a failed read is not marked and not_found stops following", async () => {
  let reads = 0, puts = 0;
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); if (path.endsWith("/seen")) { puts++; return ""; } reads++; throw new Error('{"error":"not_found"}'); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled(); f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.equal(reads, 1); assert.equal(puts, 0);
});

test("ended subscriptions remain readable", async () => {
  let reads = 0;
  const ended = { ...base, state: "ended" };
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([ended]); if (path.endsWith("/seen")) return ""; reads++; return readAnswer([{ t: "user", seq: 1, text: "saved" }], 1, false, ended); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  assert.equal(reads, 1); assert.match(f.get("sv_body").innerHTML, /saved/); assert.equal(f.get("sv_state").textContent, "Sharing ended · saved copy");
});

test("destroy removes all guest listeners and prevents later requests", async () => {
  let calls = 0;
  const f = viewFixture(async path => { calls++; if (path === "/api/team/subscriptions") return list([base]); return readAnswer(); });
  for (const id of ["cardlist", "sv_back", "sv_scrim", "sv_more", "sv_remove"]) assert.equal(f.get(id).listeners.size, 1);
  assert.equal(f.get("sharedview").listeners.size, 2);
  f.feature.destroy();
  for (const id of ["cardlist", "sv_back", "sv_scrim", "sv_more", "sv_remove", "sharedview"]) assert.equal(f.get(id).listeners.size, 0);
  f.feature.tick(); await f.feature.settled(); assert.equal(calls, 0);
});

test("following an initially empty chat replaces the empty message", async () => {
  let reads = 0;
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); if (path.endsWith("/seen")) return ""; reads++; if (reads === 1) return readAnswer(); return readAnswer([{ t: "user", seq: 1, text: "new entry" }], 1); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled(); assert.match(f.get("sv_body").innerHTML, /Nothing has been shared yet/);
  f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.match(f.get("sv_body").innerHTML, /new entry/); assert.doesNotMatch(f.get("sv_body").innerHTML, /Nothing has been shared yet/);
});

test("first unread is scrolled into view and later appends respect scroll position", async () => {
  let reads = 0, scrolls = 0;
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); if (path.endsWith("/seen")) return ""; reads++; return readAnswer([{ t: "user", seq: reads, text: "entry" }], reads); });
  const body = f.get("sv_body"); body.querySelector = () => ({ scrollIntoView(opts) { assert.deepEqual(opts, { block: "start" }); scrolls++; } });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled(); assert.equal(scrolls, 1);
  body.scrollTop = 0; f.setTime(5000); f.feature.tick(); await f.feature.settled(); assert.equal(body.scrollTop, 0);
  body.scrollTop = 750; f.setTime(10000); f.feature.tick(); await f.feature.settled(); assert.equal(body.scrollTop, body.scrollHeight);
});

test("list sorting handles reversed states and writing access", async () => {
  const groups = guestGroups([{ ...base, state: "live" }, { ...base, chat: "ended", state: "ended" }], []);
  assert.deepEqual(groups[0].items.map(v => v.state), ["live", "ended"]);
  const f = fixture(async () => list([{ ...base, access: "write" }]));
  f.feature.tick(); await f.feature.settled(); f.feature.tab.render(f.el); assert.match(f.el.innerHTML, /Can comment/);
});

test("destroy drops an in-flight list answer", async () => {
  const pending = deferred(); const f = fixture(() => pending.promise);
  f.feature.tick(); f.feature.destroy(); pending.resolve(list([base])); await f.feature.settled();
  assert.equal(f.invalidations(), 0); assert.equal(f.feature.tab.tabHTML(false), "");
});

test("empty header fields use their fallbacks", async () => {
  const view = { ...base, title: "", owner_name: "" };
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([view]); return readAnswer([], 0, false, view); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  assert.equal(f.get("sv_title").textContent, "Untitled chat"); assert.equal(f.get("sv_from").textContent, "From Unknown person · Server");
});

test("an initial empty list does not invalidate cards even with server status", async () => {
  const f = fixture(async () => list([], [{ id: "s", delivery: "offline" }], true));
  f.feature.tick(); await f.feature.settled(); assert.equal(f.invalidations(), 0); assert.equal(f.feature.tab.tabHTML(false), "");
});

test("unrelated clicks and keys, and repeated binding, have no effect", async () => {
  let calls = 0;
  const f = viewFixture(async () => { calls++; return list([base]); });
  f.feature.bind();
  f.click("cardlist", { closest() { return null; } });
  f.click("sv_back"); f.click("sv_more");
  let stopped = false; f.get("sharedview").listeners.get("keydown")({ key: "Enter", stopPropagation() { stopped = true; } });
  f.feature.tick(); await f.feature.settled();
  assert.equal(stopped, false); assert.equal(calls, 1);
});

test("destroy drops pending read and list errors", async () => {
  const read = deferred();
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); return read.promise; });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); f.feature.destroy(); read.resolve(readAnswer([{ t: "user", text: "late", seq: 1 }], 1)); await f.feature.settled();
  assert.doesNotMatch(f.get("sv_body").innerHTML, /late/);
  const pending = deferred(), g = fixture(() => pending.promise);
  g.feature.tick(); g.feature.destroy(); pending.reject(Object.assign(new Error("off"), { status: 404 })); await g.feature.settled(); assert.equal(g.invalidations(), 0);
});

test("a partial read mark keeps a newer live entry unread", async () => {
  const view = { ...base, unread: true, mirror_last: 3 };
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([view]); if (path.endsWith("/seen")) return ""; return readAnswer([{ t: "user", seq: 1, text: "x" }], 1, false, view); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled(); assert.match(f.feature.tab.tabHTML(false), /1 unread/);
});

test("load more does not overlap a read or retry a missing chat", async () => {
  const pending = deferred(); let reads = 0;
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); reads++; return pending.promise; });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); f.click("sv_more"); assert.equal(reads, 1);
  pending.reject(new Error('{"error":"not_found"}')); await f.feature.settled(); f.click("sv_more"); assert.equal(reads, 1);
});

test("gate turning off during a read hides the tab and sends no read mark", async () => {
  const read = deferred(); let lists = 0, puts = 0;
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") { lists++; if (lists === 1) return list([base]); throw Object.assign(new Error("off"), { status: 404 }); } if (path.endsWith("/seen")) { puts++; return ""; } return read.promise; });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item);
  const before = f.invalidations(); f.setTime(10000); f.feature.tick();
  await Promise.resolve(); await Promise.resolve(); read.resolve(readAnswer([{ t: "user", seq: 1, text: "x" }], 1)); await f.feature.settled();
  assert.equal(puts, 0); assert.equal(f.feature.tab.tabHTML(false), ""); assert.equal(f.invalidations(), before + 1);
});

test("late read errors and late seen responses do not update a closed view", async () => {
  const read = deferred();
  const a = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); return read.promise; });
  a.feature.tick(); await a.feature.settled(); a.click("cardlist", a.item); a.click("sv_back"); read.reject(new Error('{"error":"copy_damaged"}')); await a.feature.settled(); assert.equal(a.get("sv_note").textContent, "");
  const seen = deferred();
  const b = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([{ ...base, unread: true }]); if (path.endsWith("/seen")) return seen.promise; return readAnswer([], 1); });
  b.feature.tick(); await b.feature.settled(); b.click("cardlist", b.item); await Promise.resolve(); b.click("sv_back"); seen.resolve(""); await b.feature.settled(); assert.match(b.feature.tab.tabHTML(false), /1 unread/);
});

test("a completed read mark tolerates its subscription disappearing", async () => {
  const seen = deferred(); let lists = 0;
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") { lists++; if (lists === 1) return list([base]); return list(); } if (path.endsWith("/seen")) return seen.promise; return readAnswer([], 1); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await Promise.resolve(); f.setTime(10000); f.feature.tick(); await Promise.resolve(); await Promise.resolve(); seen.resolve(""); await f.feature.settled();
  assert.equal(f.feature.tab.tabHTML(false), "");
});

test("browser-normalized HTML does not rewrite an unchanged shared list", async () => {
  const f = fixture(async () => list([{ ...base, title: 'Quoted "title"' }]));
  let value = "", writes = 0;
  Object.defineProperty(f.el, "innerHTML", { get() { return value; }, set(v) { value = v.replaceAll("&quot;", '"'); writes++; } });
  f.feature.tick(); await f.feature.settled(); f.feature.tab.render(f.el); f.feature.tab.render(f.el);
  assert.equal(writes, 1);
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

test("load more cannot issue requests after the gate turns off", async () => {
  let lists = 0, reads = 0;
  const f = viewFixture(async path => {
    if (path === "/api/team/subscriptions") {
      lists++;
      if (lists === 1) return list([base]);
      return decodeResponse(jsonResponse('{"error":"not_found"}', { status: 404, etag: "" }));
    }
    if (path.endsWith("/seen")) return "";
    reads++; return readAnswer([], 0, true);
  });
  f.feature.tick(); await f.feature.settled(); f.setTime(9000); f.click("cardlist", f.item); await f.feature.settled();
  assert.equal(reads, 40); f.setTime(10000); f.feature.tick(); await f.feature.settled();
  const before = reads; f.click("sv_more"); await f.feature.settled(); assert.equal(reads, before);
});

test("an omitted field displays the omission message even when empty", () => {
  for (const kind of ["user", "assistant"]) {
    assert.match(guestEntryHTML({ t: kind, seq: 1, omitted: "" }, "Owner"), /This message was too large to share\./);
  }
});

test("remove first click arms with a warning and another click disarms", async () => {
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); return readAnswer(); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  const button = f.get("sv_remove");
  assert.equal(button.textContent, "Remove from my list"); assert.equal(button.disabled, false);
  button.focus(); f.click("sv_remove");
  assert.equal(button.textContent, "Confirm remove");
  assert.equal(f.get("sv_note").textContent, "This leaves the chat and deletes the saved copy from this computer.");
  assert.equal(f.doc.activeElement, button);
  f.click("sharedview", button); assert.equal(button.textContent, "Confirm remove");
  f.click("sharedview", f.get("sv_body"));
  assert.equal(button.textContent, "Remove from my list"); assert.equal(f.get("sv_note").textContent, "");
});

test("escape first disarms removal and then closes the view", async () => {
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); return readAnswer(); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled(); f.click("sv_remove");
  let stopped = 0;
  const escape = () => f.get("sharedview").listeners.get("keydown")({ key: "Escape", stopPropagation() { stopped++; } });
  escape(); assert.equal(f.get("sharedview").hidden, false); assert.equal(f.get("sv_remove").textContent, "Remove from my list"); assert.equal(f.get("sv_note").textContent, "");
  escape(); assert.equal(f.get("sharedview").hidden, true); assert.equal(stopped, 2);
});

test("removal blocks duplicate deletes and read runs, then focuses the remaining tab", async () => {
  const pending = deferred(); let deletes = 0, reads = 0;
  const other = { ...base, chat: "d" };
  const f = viewFixture(async (path, opts) => {
    if (path === "/api/team/subscriptions") return list([base, other]);
    if (opts) { if (opts.method === "DELETE") { deletes++; return pending.promise; } }
    reads++; return readAnswer();
  });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  const before = f.invalidations(); f.get("sv_remove").focus(); f.click("sv_remove"); f.click("sv_remove");
  assert.equal(deletes, 1); assert.equal(f.get("sv_remove").disabled, true); f.click("sv_remove");
  f.setTime(5000); f.feature.tick(); assert.equal(reads, 1); assert.equal(deletes, 1);
  pending.resolve(""); await f.feature.settled();
  assert.equal(f.get("sharedview").hidden, true); assert.equal(f.doc.activeElement, f.get("cardtabs"));
  assert.equal(f.invalidations(), before + 1); assert.match(f.feature.tab.tabHTML(false), /Shared with me/);
  f.feature.tab.render(f.get("cardlist")); assert.equal(f.get("cardlist").innerHTML.includes('data-sub="s/o/c/g"'), false);
});

test("not found removal succeeds and focuses the card list when the tab is gone", async () => {
  const f = viewFixture(async (path, opts) => {
    if (path === "/api/team/subscriptions") return list([base]);
    if (opts) { if (opts.method === "DELETE") throw Object.assign(new Error('{"error":"not_found"}'), { status: 404 }); }
    return readAnswer();
  });
  f.get("cardtabs").querySelector = () => null;
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  const before = f.invalidations(); f.click("sv_remove"); f.click("sv_remove"); await f.feature.settled();
  assert.equal(f.get("sharedview").hidden, true); assert.equal(f.doc.activeElement, f.get("cardlist"));
  assert.equal(f.feature.tab.tabHTML(false), ""); assert.equal(f.invalidations(), before + 1);
});

test("removal failures reset the button and keep their note across ticks", async () => {
  for (const [code, expected] of [["unreachable", "The server could not be reached. Try again later."], ["other", "That did not work. Try again."]]) {
    let deletes = 0;
    const f = viewFixture(async (path, opts) => {
      if (path === "/api/team/subscriptions") return list([base]);
      if (opts) { if (opts.method === "DELETE") { deletes++; throw new Error(JSON.stringify({ error: code })); } }
      return readAnswer();
    });
    f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
    f.click("sv_remove"); f.click("sv_remove"); await f.feature.settled();
    assert.equal(deletes, 1); assert.equal(f.get("sv_remove").disabled, false); assert.equal(f.get("sv_remove").textContent, "Remove from my list");
    assert.equal(f.get("sv_note").textContent, expected); f.setTime(5000); f.feature.tick(); await f.feature.settled(); assert.equal(f.get("sv_note").textContent, expected);
  }
});

test("tick keeps the armed removal warning", async () => {
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); return readAnswer(); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled(); f.click("sv_remove");
  f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.equal(f.get("sv_remove").textContent, "Confirm remove"); assert.equal(f.get("sv_note").textContent, "This leaves the chat and deletes the saved copy from this computer.");
});

test("late successful removal drops its item without changing a different open view", async () => {
  for (const status of [204, 404]) {
    const pending = deferred(), other = { ...base, chat: "d", title: "Other" };
    const f = viewFixture(async (path, opts) => {
      if (path === "/api/team/subscriptions") return list([base, other]);
      if (opts) { if (opts.method === "DELETE") return pending.promise; }
      if (path.includes("/d/g?")) return readAnswer([], 0, false, other);
      return readAnswer();
    });
    f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled(); f.click("sv_remove"); f.click("sv_remove");
    const second = { dataset: { sub: guestKey(other) }, closest(sel) { if (sel === "[data-sub]") return this; return null; } };
    f.click("cardlist", second); await Promise.resolve();
    if (status === 404) pending.reject(Object.assign(new Error('{"error":"not_found"}'), { status: 404 }));
    else pending.resolve("");
    await f.feature.settled(); assert.equal(f.get("sharedview").hidden, false); assert.equal(f.get("sv_title").textContent, "Other");
    f.feature.tab.render(f.get("cardlist")); assert.equal(f.get("cardlist").innerHTML.includes('data-sub="s/o/c/g"'), false);
  }
});

test("late failed removal does not alter the newly opened view", async () => {
  const pending = deferred(), other = { ...base, chat: "d", title: "Other" };
  let deletes = 0;
  const f = viewFixture(async (path, opts) => {
    if (path === "/api/team/subscriptions") return list([base, other]);
    if (opts) { if (opts.method === "DELETE") { deletes++; return pending.promise; } }
    return readAnswer([], 0, false, other);
  });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled(); f.click("sv_remove"); f.click("sv_remove");
  const second = { dataset: { sub: guestKey(other) }, closest(sel) { if (sel === "[data-sub]") return this; return null; } };
  assert.equal(deletes, 1); f.click("cardlist", second); pending.reject(new Error('{"error":"unreachable"}')); await f.feature.settled();
  assert.equal(f.get("sharedview").hidden, false); assert.equal(f.get("sv_title").textContent, "Other"); assert.equal(f.get("sv_note").textContent, "");
});

test("late removal after closing drops the chat without reopening the view", async () => {
  for (const status of [204, 404]) {
    const pending = deferred();
    const f = viewFixture(async (path, opts) => {
      if (path === "/api/team/subscriptions") return list([base]);
      if (opts) { if (opts.method === "DELETE") return pending.promise; }
      return readAnswer();
    });
    f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
    f.click("sv_remove"); f.click("sv_remove"); f.click("sv_back");
    const before = f.invalidations();
    if (status === 404) pending.reject(Object.assign(new Error("not found"), { status: 404 }));
    else pending.resolve("");
    await f.feature.settled(); assert.equal(f.get("sharedview").hidden, true); assert.equal(f.feature.tab.tabHTML(false), ""); assert.equal(f.invalidations(), before + 1);
  }
});

test("a non-JSON removal error uses the generic message", async () => {
  const f = viewFixture(async (path, opts) => {
    if (path === "/api/team/subscriptions") return list([base]);
    if (opts) { if (opts.method === "DELETE") throw new Error("not json"); }
    return readAnswer();
  });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled(); f.click("sv_remove"); f.click("sv_remove"); await f.feature.settled();
  assert.equal(f.get("sv_note").textContent, "That did not work. Try again.");
});

test("late removal after destroy has no effects", async () => {
  const pending = deferred(); let deletes = 0;
  const f = viewFixture(async (path, opts) => {
    if (path === "/api/team/subscriptions") return list([base]);
    if (opts) { if (opts.method === "DELETE") { deletes++; return pending.promise; } }
    return readAnswer();
  });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled(); f.click("sv_remove"); f.click("sv_remove");
  assert.equal(deletes, 1); const before = f.invalidations(); f.feature.destroy(); pending.resolve(""); await f.feature.settled();
  assert.equal(f.invalidations(), before);
});

test("removal controls do nothing when no view is open", async () => {
  let calls = 0;
  const f = viewFixture(async () => { calls++; return list(); });
  f.click("sv_remove"); f.click("sharedview", f.get("sv_body"));
  f.feature.tick(); await f.feature.settled(); assert.equal(calls, 1);
});

/* Wave 4c condition table:
 * | condition | test for each half |
 * | none | Wave 4c adds no &&, ||, or ?: condition. |
 */

test("a failed follow read keeps the armed removal warning", async () => {
  let reads = 0;
  const f = viewFixture(async path => {
    if (path === "/api/team/subscriptions") return list([base]);
    reads++;
    if (reads === 2) throw new Error('{"error":"copy_damaged"}');
    return readAnswer();
  });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  f.click("sv_remove"); f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.equal(reads, 2);
  assert.equal(f.get("sv_remove").textContent, "Confirm remove");
  assert.equal(f.get("sv_note").textContent, "This leaves the chat and deletes the saved copy from this computer.");
});

test("unarmed dialog clicks and escape without an open view do nothing", async () => {
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); return readAnswer(); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  f.click("sharedview", f.get("sv_body")); assert.equal(f.get("sv_remove").textContent, "Remove from my list");
  f.click("sv_back"); let stopped = false;
  f.get("sharedview").listeners.get("keydown")({ key: "Escape", stopPropagation() { stopped = true; } });
  assert.equal(stopped, true); assert.equal(f.get("sharedview").hidden, true);
});

test("collapsed shared groups hide their items and expand again", async () => {
  const second = { ...base, owner: "other", chat: "d" };
  const f = fixture(async () => list([base, second]));
  f.feature.bind(); f.feature.tick(); await f.feature.settled(); f.feature.tab.render(f.el);
  const firstKey = 'data-sub="s/o/c/g"';
  const secondKey = 'data-sub="s/other/d/g"';
  assert.equal(f.el.innerHTML.includes(firstKey), true); assert.equal(f.el.innerHTML.includes(secondKey), true);
  const group = { dataset: { group: "s/o" }, closest(sel) { if (sel === "[data-group]") return this; return null; } };
  f.listeners.get("click")({ target: group });
  assert.equal(f.el.innerHTML.includes(firstKey), false); assert.equal(f.el.innerHTML.includes(secondKey), true);
  f.listeners.get("click")({ target: group });
  assert.equal(f.el.innerHTML.includes(firstKey), true); assert.equal(f.el.innerHTML.includes(secondKey), true);
});

test("first shared read without unread entries scrolls to the end", async () => {
  const view = { ...base, seen_mirror: 3, mirror_last: 3 };
  const f = viewFixture(async path => {
    if (path === "/api/team/subscriptions") return list([view]);
    return readAnswer([{ t: "user", seq: 1, text: "" }], 1, false, view);
  });
  const body = f.get("sv_body"); let scrolls = 0;
  body.querySelector = () => ({ scrollIntoView() { scrolls++; } });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  assert.equal(body.scrollTop, body.scrollHeight); assert.equal(scrolls, 0);
});

test("shared follow scrolls at 80 pixels but not 81", async () => {
  for (const distance of [80, 81]) {
    let reads = 0;
    const f = viewFixture(async path => {
      if (path === "/api/team/subscriptions") return list([base]);
      if (path.endsWith("/seen")) return "";
      reads++;
      if (reads === 1) return readAnswer();
      return readAnswer([{ t: "user", seq: 1, text: "" }], 1);
    });
    f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
    const body = f.get("sv_body");
    body.scrollTop = body.scrollHeight - body.clientHeight - distance;
    const before = body.scrollTop;
    f.setTime(5000); f.feature.tick(); await f.feature.settled();
    if (distance === 80) assert.equal(body.scrollTop, body.scrollHeight);
    if (distance === 81) assert.equal(body.scrollTop, before);
  }
});
