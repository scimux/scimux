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
  const css = readFileSync(new URL("../css/layout.css", import.meta.url), "utf8");
  assert.match(app, /uiMutate:\s*op\s*=>\s*uiMutate\(op\)/);
  assert.match(app, /bookmarks:\s*\(\)\s*=>\s*getUI\(\)\.bookmarks/);
  assert.match(app, /openSendTo:\s*opts\s*=>\s*bookmarksFeature\.openSendTo\(opts\)/);
  assert.match(css, /#sharedview[^{}]*\.bubactions\s+button\s*\{[^}]*min-height:\s*44px/);
});
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

function viewFixture(api, commentsApi = async () => ({ entries: [], names: {}, last: 0, next: 0, more: false }), randomBytes = n => new Uint8Array(n), extra = {}) {
  let time = 0, invalidations = 0;
  const elements = new Map();
  const doc = { activeElement: null, querySelector(sel) { return elements.get(sel); } };
  for (const id of ["cardlist", "cardtabs", "sharedview", "sv_scrim", "sv_back", "sv_title", "sv_from", "sv_state", "sv_body", "sv_more", "sv_note", "sv_remove", "sv_compose", "sv_text", "sv_send", "sv_hint"]) {
    const listeners = new Map();
    const el = { id, innerHTML: "", textContent: "", value: "", selectionStart: 0, selectionEnd: 0, disabled: false, hidden: false, dataset: {}, scrollTop: 0, scrollHeight: 1000, clientHeight: 200, listeners,
      addEventListener(type, fn) { listeners.set(type, fn); }, removeEventListener(type) { listeners.delete(type); },
      focus() { doc.activeElement = el; }, contains(node) { return node === el; },
      querySelector() { return null; }, querySelectorAll() { return []; }, insertAdjacentHTML(_where, html) { this.innerHTML += html; } };
    elements.set("#" + id, el);
  }
  const body = elements.get("#sv_body");
  Object.defineProperty(body, "children", { get() {
    const pieces = this.innerHTML.split(/(?=<div class="(?:turn |previewtrunc|bubactions))/).filter(Boolean);
    return pieces.map((piece, index) => {
      const read = name => piece.match(new RegExp(`data-${name}="([^"]*)"`))?.[1];
      return { dataset: { seq: read("seq"), cseq: read("cseq"), anchor: read("anchor"), gactions: piece.includes("data-gactions") ? "" : undefined },
        insertAdjacentHTML(_where, html) { pieces.splice(index, 0, html); body.innerHTML = pieces.join(""); } };
    });
  } });
  const routed = (path, opts) => {
    if (path.includes("/comments?")) return commentsApi(path, opts);
    return api(path, opts);
  };
  const feature = createGuestFeature({ api: routed, doc, now: () => time, invalidateCards: () => invalidations++, randomBytes, ...extra });
  const get = id => elements.get("#" + id);
  const click = (id, target = get(id)) => get(id).listeners.get("click")({ target });
  const item = { dataset: { sub: guestKey(base) }, closest(sel) { if (sel === "[data-sub]") return this; return null; }, focus() { doc.activeElement = item; } };
  get("cardlist").querySelector = () => item;
  get("cardtabs").querySelector = () => get("cardtabs");
  feature.bind();
  return { feature, get, click, item, doc, setTime(v) { time = v; }, invalidations: () => invalidations };
}

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
  test(`shared header for ${access} with ${delivery}`, async () => {
    const view = { ...base, access, delivery };
    const f = viewFixture(async path => path === "/api/team/subscriptions" ? list([view]) : readAnswer([], 0, false, view));
    f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
    assert.equal(f.get("sv_from").textContent, from);
    assert.equal(f.get("sv_state").textContent, state);
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
  assert.equal(f.get("sv_text").listeners.size, 2);
  assert.equal(f.get("sv_send").listeners.size, 1);
  assert.equal(f.get("sharedview").listeners.size, 2);
  f.feature.destroy();
  for (const id of ["cardlist", "sv_back", "sv_scrim", "sv_more", "sv_remove", "sharedview", "sv_text", "sv_send"]) assert.equal(f.get(id).listeners.size, 0);
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
  assert.equal(f.invalidations(), before + 1); assert.match(f.feature.tab.tabHTML(false), /Shared/);
  f.feature.tab.render(f.get("cardlist")); assert.equal(f.get("cardlist").innerHTML.includes('data-sub="s/o/c/g"'), false);
});

test("not found removal succeeds and focuses the current tab when shared is gone", async () => {
  const f = viewFixture(async (path, opts) => {
    if (path === "/api/team/subscriptions") return list([base]);
    if (opts) { if (opts.method === "DELETE") throw Object.assign(new Error('{"error":"not_found"}'), { status: 404 }); }
    return readAnswer();
  });
  const current = { focus() { f.doc.activeElement = current; } };
  f.get("cardtabs").querySelector = sel => sel === '[data-tab="current"]' ? current : null;
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  const before = f.invalidations(); f.click("sv_remove"); f.click("sv_remove"); await f.feature.settled();
  assert.equal(f.get("sharedview").hidden, true); assert.equal(f.doc.activeElement, current);
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

test("owner items describe active and ended sharing", async () => {
  const owned = { ...base, access: "owner" };
  const f = fixture(async () => list([owned, { ...owned, chat: "ended", state: "ended" }]));
  f.feature.tick(); await f.feature.settled(); f.feature.tab.render(f.el);
  assert.match(f.feature.tab.tabHTML(false), />Shared</);
  assert.match(f.el.innerHTML, /Shared by you/);
  assert.match(f.el.innerHTML, /Sharing ended/);
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
  assert.ok(!mine.includes("gwho"));
});

test("guest mirror prompts name their human source", () => {
  assert.ok(guestEntryHTML({ t:"user", seq:1, text:"x" }, "Owner").includes("Owner · prompt"));
  assert.ok(guestEntryHTML({ t:"user", seq:1, text:"x", prov:true }, "Owner").includes("Owner · prompt · includes forwarded AI text"));
});

test("shared view merges both logs by anchor and keeps names", async () => {
  const paths = [], puts = [];
  const view = { ...base, mirror_last:4, comments_last:3, unread:true };
  const f = viewFixture(async (path, opts) => {
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
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  const order = () => [...f.get("sv_body").innerHTML.matchAll(/data-(seq|cseq)="(\d+)"/g)].map(m => m[1] + m[2]);
  assert.deepEqual(order(), ["seq1", "cseq1", "cseq2", "seq3", "cseq3"]);
  assert.ok(f.get("sv_body").innerHTML.includes("Ada")); assert.ok(f.get("sv_body").innerHTML.includes("Bea"));
  assert.deepEqual(puts[0], { mirror:3, comments:3 });
  assert.ok(paths[0].endsWith("?after=0")); assert.ok(paths[1].endsWith("/comments?after=0")); assert.ok(paths[2].endsWith("/comments?after=2"));
  f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.deepEqual(order(), ["seq1", "cseq1", "cseq2", "seq3", "seq4", "cseq3"]);
});

test("comment paging has its own forty-page cap", async () => {
  let calls = 0;
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); if (path.endsWith("/seen")) return ""; return readAnswer(); }, async () => { calls++; return commentsAnswer([], calls, true); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  assert.equal(calls, 40); assert.equal(f.get("sv_more").hidden, false);
});

test("first unread comment scrolls into view", async () => {
  let selector = "", scrolls = 0;
  const view = { ...base, seen_mirror:1, seen_comments:0, comments_last:1 };
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([view]); if (path.endsWith("/seen")) return ""; return readAnswer([{ t:"user", seq:1, text:"x" }], 1, false, view); }, async () => commentsAnswer([{ seq:1, anchor:1, author:"a", text:"x" }], 1));
  f.get("sv_body").querySelector = sel => { selector = sel; return { scrollIntoView() { scrolls++; } }; };
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  assert.equal(selector, '[data-cseq="1"]'); assert.equal(scrolls, 1);
});

test("read marks include only advanced logs and use owner unread rules", async () => {
  for (const tc of [
    { mirror:1, comments:0, body:{mirror:1}, access:"read", unread:false },
    { mirror:0, comments:1, body:{comments:1}, access:"read", unread:false },
    { mirror:1, comments:1, body:{mirror:1,comments:1}, access:"owner", unread:false },
    { mirror:0, comments:0, body:null, access:"read", unread:false },
    { mirror:1, comments:0, body:{mirror:1}, access:"read", unread:true, pendingComments:2 },
  ]) {
    const seen = [];
    const view = { ...base, access:tc.access, mirror_last:tc.mirror, comments_last:tc.pendingComments || tc.comments, unread:true };
    const f = viewFixture(async (path, opts) => { if (path === "/api/team/subscriptions") return list([view]); if (path.endsWith("/seen")) { seen.push(JSON.parse(opts.body)); return ""; } return readAnswer([], tc.mirror, false, view); }, async () => commentsAnswer([], tc.comments));
    f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
    assert.deepEqual(seen, tc.body ? [tc.body] : []);
    if (tc.body) assert.equal(f.feature.tab.tabHTML(false).includes("unread"), tc.unread);
  }
});

test("following an empty view replaces it with a new comment", async () => {
  let calls = 0;
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); if (path.endsWith("/seen")) return ""; return readAnswer(); }, async () => { calls++; if (calls === 1) return commentsAnswer(); return commentsAnswer([{ seq:1, anchor:0, author:"a", text:"x" }], 1); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  assert.ok(f.get("sv_body").innerHTML.includes("Nothing has been shared yet."));
  f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.ok(f.get("sv_body").innerHTML.includes('data-cseq="1"'));
  assert.ok(!f.get("sv_body").innerHTML.includes("Nothing has been shared yet."));
});

test("late combined seen answer does not mark a closed view", async () => {
  const seen = deferred(), started = deferred();
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([{ ...base, unread:true }]); if (path.endsWith("/seen")) { started.resolve(); return seen.promise; } return readAnswer([], 1); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await started.promise;
  f.click("sv_back"); seen.resolve(""); await f.feature.settled();
  assert.ok(f.feature.tab.tabHTML(false).includes("1 unread"));
});

function setGuestDraft(f, text) {
  const field = f.get("sv_text"); field.value = text;
  const listener = field.listeners.get("input"); assert.ok(listener, "missing draft listener"); listener({ target: field });
}

test("guest comment keys use eight bytes in lower-case hex", () => {
  assert.equal(guestHexKey(new Uint8Array([0, 1, 15, 16, 127, 128, 254, 255])), "00010f107f80feff");
});

test("composer follows live access and read updates", async () => {
  for (const tc of [
    { access:"read", state:"live", visible:false }, { access:"write", state:"live", visible:true },
    { access:"owner", state:"live", visible:true }, { access:"write", state:"ended", visible:false },
    { access:"owner", state:"ended", visible:false },
  ]) {
    const view = { ...base, ...tc };
    const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([view]); return readAnswer([], 0, false, view); });
    f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
    assert.equal(f.get("sv_compose").hidden, !tc.visible);
  }
  const writable = { ...base, access:"write" }, ended = { ...writable, state:"ended" };
  let reads = 0;
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([writable]); reads++; return readAnswer([], 0, false, reads === 1 ? writable : ended); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled(); assert.equal(f.get("sv_compose").hidden, false);
  f.setTime(5000); f.feature.tick(); await f.feature.settled(); assert.equal(f.get("sv_compose").hidden, true);
});

test("composer validates UTF-8 byte limit and in-flight sends", async () => {
  const pending = deferred(), view = { ...base, access:"write" };
  const f = viewFixture(async (path, opts) => { if (path === "/api/team/subscriptions") return list([view]); if (opts?.method === "POST") return pending.promise; return readAnswer([], 0, false, view); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  assert.equal(f.get("sv_send").disabled, true);
  setGuestDraft(f, " \n "); assert.equal(f.get("sv_send").disabled, true);
  setGuestDraft(f, "a".repeat(16382) + "é"); assert.equal(f.get("sv_send").disabled, false);
  setGuestDraft(f, "a".repeat(16383) + "é"); assert.equal(f.get("sv_send").disabled, true); assert.equal(f.get("sv_note").textContent, "That message is too long.");
  setGuestDraft(f, "synthetic"); assert.equal(f.get("sv_send").disabled, false);
  f.click("sv_send"); assert.equal(f.get("sv_send").disabled, true); pending.resolve({ seq:1 }); await f.feature.settled();
});

test("sending keeps a draft key on failure and changes it with the text", async () => {
  const posts = []; let serial = 0, reads = 0;
  const view = { ...base, access:"write" };
  const f = viewFixture(async (path, opts) => {
    if (path === "/api/team/subscriptions") return list([view]);
    if (opts?.method === "POST") { posts.push(JSON.parse(opts.body)); if (posts.length < 3) throw new Error('{"error":"unreachable"}'); return { seq:3 }; }
    if (path.endsWith("/seen")) return "";
    reads++; return readAnswer([], 7, false, view);
  }, undefined, n => { assert.equal(n, 8); serial++; return new Uint8Array(n).fill(serial); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  const field = f.get("sv_text"); field.focus(); setGuestDraft(f, " first ");
  f.click("sv_send"); await f.feature.settled();
  assert.ok(field.value === " first "); assert.equal(f.doc.activeElement, field); assert.equal(f.get("sv_note").textContent, "The server could not be reached. Your message is still here.");
  f.click("sv_send"); await f.feature.settled();
  assert.ok(JSON.stringify(posts.slice(0, 2)) === JSON.stringify([{ text:" first ", anchor:7, key:"0101010101010101" }, { text:" first ", anchor:7, key:"0101010101010101" }]));
  setGuestDraft(f, "second"); f.click("sv_send"); await f.feature.settled();
  assert.ok(JSON.stringify(posts[2]) === JSON.stringify({ text:"second", anchor:7, key:"0202020202020202" }));
  assert.ok(field.value === ""); assert.equal(f.doc.activeElement, field); assert.ok(reads >= 2);
});

test("composer keyboard sends only control or meta Enter", async () => {
  let posts = 0;
  const view = { ...base, access:"write" };
  const f = viewFixture(async (path, opts) => { if (path === "/api/team/subscriptions") return list([view]); if (opts?.method === "POST") { posts++; throw new Error("failed"); } return readAnswer([], 0, false, view); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled(); setGuestDraft(f, "synthetic");
  const key = f.get("sv_text").listeners.get("keydown"); assert.ok(key, "missing key listener");
  let prevented = 0;
  key({ key:"Enter", ctrlKey:false, metaKey:false, preventDefault() { prevented++; } }); assert.equal(posts, 0); assert.equal(prevented, 0);
  key({ key:"Enter", ctrlKey:true, metaKey:false, preventDefault() { prevented++; } }); await f.feature.settled();
  key({ key:"Enter", ctrlKey:false, metaKey:true, preventDefault() { prevented++; } }); await f.feature.settled();
  assert.equal(posts, 2); assert.equal(prevented, 2);
});

test("composer reports every send failure without losing the draft", async () => {
  for (const [code, expected] of [["read_only", "You can only view this chat."], ["not_active", "Sharing of this chat has ended."], ["unreachable", "The server could not be reached. Your message is still here."], ["refused", "The server ended this computer's access."], ["server_full", "The server has no room for more messages."], ["too_large", "That message is too long."], ["other", "That did not work. Your message is still here."]]) {
    const view = { ...base, access:"write" };
    const f = viewFixture(async (path, opts) => { if (path === "/api/team/subscriptions") return list([view]); if (opts?.method === "POST") throw new Error(JSON.stringify({ error:code })); return readAnswer([], 0, false, view); });
    f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
    const field = f.get("sv_text"); field.focus(); setGuestDraft(f, "synthetic"); f.click("sv_send"); await f.feature.settled();
    assert.ok(field.value === "synthetic"); assert.equal(f.doc.activeElement, field); assert.equal(f.get("sv_note").textContent, expected);
  }
});

test("drafts survive polling, selection, closing and changing chats", async () => {
  const first = { ...base, access:"write" }, second = { ...first, chat:"d", title:"Other" };
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([first, second]); if (path.includes("/d/g?")) return readAnswer([], 0, false, second); return readAnswer([], 0, false, first); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  const field = f.get("sv_text"); setGuestDraft(f, "first"); field.selectionStart = 1; field.selectionEnd = 3; field.focus();
  f.setTime(10000); f.feature.tick(); await f.feature.settled();
  assert.ok(field.value === "first"); assert.equal(field.selectionStart, 1); assert.equal(field.selectionEnd, 3); assert.equal(f.doc.activeElement, field);
  const other = { dataset:{ sub:guestKey(second) }, closest(sel) { if (sel === "[data-sub]") return this; return null; } };
  f.click("cardlist", other); await f.feature.settled(); assert.ok(field.value === ""); setGuestDraft(f, "second");
  f.click("sv_back"); f.click("cardlist", f.item); await f.feature.settled(); assert.ok(field.value === "first");
  f.click("cardlist", other); await f.feature.settled(); assert.ok(field.value === "second");
});

test("late send answers keep another view and update only their own draft", async () => {
  for (const success of [true, false]) {
    const pending = deferred(), first = { ...base, access:"write" }, second = { ...first, chat:"d", title:"Other" };
    let random = 0, posts = 0;
    const f = viewFixture(async (path, opts) => {
      if (path === "/api/team/subscriptions") return list([first, second]);
      if (opts?.method === "POST") { posts++; return pending.promise; }
      if (path.includes("/d/g?")) return readAnswer([], 0, false, second);
      return readAnswer([], 0, false, first);
    }, undefined, n => { random++; return new Uint8Array(n).fill(random); });
    f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
    setGuestDraft(f, "first"); f.click("sv_send");
    const other = { dataset:{ sub:guestKey(second) }, closest(sel) { if (sel === "[data-sub]") return this; return null; } };
    f.click("cardlist", other); setGuestDraft(f, "second"); f.get("sv_text").focus();
    if (success) pending.resolve({ seq:1 }); else pending.reject(new Error('{"error":"unreachable"}'));
    await f.feature.settled();
    assert.ok(f.get("sv_text").value === "second"); assert.equal(f.get("sv_title").textContent, "Other"); assert.equal(f.doc.activeElement, f.get("sv_text"));
    f.click("cardlist", f.item); await f.feature.settled();
    assert.ok(f.get("sv_text").value === (success ? "" : "first")); assert.equal(posts, 1); assert.equal(random, 1);
  }
});

test("composer ignores closed, empty, duplicate and unrelated keys", async () => {
  const pending = deferred(), view = { ...base, access:"write" }; let posts = 0;
  const f = viewFixture(async (path, opts) => { if (path === "/api/team/subscriptions") return list([view]); if (opts?.method === "POST") { posts++; return pending.promise; } return readAnswer([], 0, false, view); });
  const input = f.get("sv_text").listeners.get("input"), key = f.get("sv_text").listeners.get("keydown");
  assert.ok(input && key, "missing composer listeners"); input({ target:f.get("sv_text") }); key({ key:"a" }); f.click("sv_send");
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled(); f.click("sv_send"); assert.equal(posts, 0);
  setGuestDraft(f, "synthetic"); f.click("sv_send"); f.click("sv_send"); assert.equal(posts, 1); pending.resolve({ seq:1 }); await f.feature.settled();
});

test("editing a reopened draft after a late success creates a fresh draft", async () => {
  const pending = deferred(), view = { ...base, access:"write" };
  const f = viewFixture(async (path, opts) => { if (path === "/api/team/subscriptions") return list([view]); if (opts?.method === "POST") return pending.promise; return readAnswer([], 0, false, view); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled(); setGuestDraft(f, "first"); f.click("sv_send");
  f.click("cardlist", f.item); pending.resolve({ seq:1 }); await f.feature.settled(); setGuestDraft(f, "second");
  f.click("sv_back"); f.click("cardlist", f.item); await f.feature.settled(); assert.ok(f.get("sv_text").value === "second");
});

test("a send starts a fresh read even when an earlier read is pending", async () => {
  const pending = deferred(), view = { ...base, access:"write" }; let reads = 0;
  const f = viewFixture(async (path, opts) => {
    if (path === "/api/team/subscriptions") return list([view]);
    if (opts?.method === "POST") return { seq:1 };
    reads++; if (reads === 1) return pending.promise;
    return readAnswer([], 0, false, view);
  });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); setGuestDraft(f, "synthetic"); f.click("sv_send");
  await Promise.resolve(); await Promise.resolve();
  const observed = reads; pending.resolve(readAnswer([], 0, false, view)); await f.feature.settled();
  assert.equal(observed, 2);
});

test("an over-limit draft keeps its warning when a read fails", async () => {
  const view = { ...base, access:"write" }; let reads = 0;
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([view]); reads++; if (reads > 1) throw new Error('{"error":"unreachable"}'); return readAnswer([], 0, false, view); });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  setGuestDraft(f, "x".repeat(16385)); f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.equal(f.get("sv_note").textContent, "That message is too long.");
});

test("only a live owner hides the remove button", async () => {
  for (const tc of [
    { access:"read", state:"live", hidden:false }, { access:"write", state:"live", hidden:false },
    { access:"owner", state:"live", hidden:true }, { access:"owner", state:"ended", hidden:false },
    { access:"read", state:"ended", hidden:false }, { access:"write", state:"ended", hidden:false },
  ]) {
    const view = { ...base, ...tc };
    const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([view]); return readAnswer([], 0, false, view); });
    f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
    assert.equal(f.get("sv_remove").hidden, tc.hidden);
  }
  const owner = { ...base, access:"owner" }, ended = { ...owner, state:"ended" }, pending = deferred();
  const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([owner]); return pending.promise; });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); assert.equal(f.get("sv_remove").hidden, true);
  pending.resolve(readAnswer([], 0, false, ended)); await f.feature.settled(); assert.equal(f.get("sv_remove").hidden, false);
});

test("a late send after close updates only its stored draft", async () => {
  for (const success of [true, false]) {
    const pending = deferred(), view = { ...base, access:"write" }; let random = 0;
    const posts = [];
    const f = viewFixture(async (path, opts) => {
      if (path === "/api/team/subscriptions") return list([view]);
      if (opts?.method === "POST") { posts.push(JSON.parse(opts.body)); if (posts.length === 1) return pending.promise; throw new Error("not json"); }
      return readAnswer([], 0, false, view);
    }, undefined, n => { random++; return new Uint8Array(n).fill(random); });
    f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
    setGuestDraft(f, "synthetic"); f.click("sv_send"); f.click("sv_back");
    if (success) pending.resolve({ seq:1 }); else pending.reject(new Error("not json"));
    await f.feature.settled(); assert.equal(f.get("sharedview").hidden, true);
    f.click("cardlist", f.item); await f.feature.settled();
    assert.ok(f.get("sv_text").value === (success ? "" : "synthetic"));
    if (!success) {
      f.click("sv_send"); await f.feature.settled();
      assert.equal(posts[0].key, posts[1].key); assert.equal(random, 1);
      assert.equal(f.get("sv_note").textContent, "That did not work. Your message is still here.");
    }
  }
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
  const f = viewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer(entries, 2), undefined, undefined,
    { bookmarks: () => stored, uiMutate: op => { ops.push(op); stored.push(op.bookmark); }, openSendTo: opts => sends.push(opts) });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  const first = sharedActionBubble(1), second = sharedActionBubble(2);
  const click = target => f.click("sharedview", target);
  click(first.bubble); assert.match(first.turn.rowHTML, /data-gactions/); assert.ok(first.turn.nextElementSibling);
  click(first.bubble); assert.equal(first.turn.nextElementSibling, null);
  click(first.bubble); click(second.bubble); assert.equal(first.turn.nextElementSibling, null); assert.ok(second.turn.nextElementSibling);
  const action = name => ({ dataset: { gact: name }, closest(sel) { return sel === "[data-gact]" ? this : null; } });
  click(action("bookmark")); assert.equal(ops.length, 1); assert.equal(ops[0].bookmark.role, "assistant"); assert.equal(ops[0].bookmark.t, new Date(0).toISOString()); assert.equal(f.get("sv_note").textContent, "Bookmarked.");
  click(action("bookmark")); assert.equal(ops.length, 1); assert.equal(f.get("sv_note").textContent, "Already bookmarked.");
  click(action("sendto")); assert.equal(f.get("sharedview").hidden, true); assert.deepEqual(sends, [{ text: "two", exceptId: "", title: "Send to…", role: "assistant", who: undefined, prov: undefined, source: null }]);
});

test("shared action keyboard, links and ticks keep focus and the row", async () => {
  const f = viewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer([{ t: "user", seq: 1, text: "x" }], 1), undefined, undefined, { bookmarks: () => [], uiMutate() {}, openSendTo() {} });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  const first = sharedActionBubble(1), missing = sharedActionBubble(99);
  const keydown = f.get("sharedview").listeners.get("keydown");
  let prevented = 0;
  keydown({ key: "Enter", target: first.bubble, preventDefault() { prevented++; } });
  assert.equal(prevented, 1); assert.ok(first.turn.nextElementSibling);
  const focus = { focus() { f.doc.activeElement = focus; } }; focus.focus();
  f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.equal(f.doc.activeElement, focus); assert.ok(first.turn.nextElementSibling);
  const link = { closest(sel) { if (sel === "a") return this; if (sel === ".bubble") return first.bubble; return null; } };
  f.click("sharedview", link); assert.ok(first.turn.nextElementSibling);
  keydown({ key: " ", target: first.bubble, preventDefault() { prevented++; } });
  assert.equal(prevented, 2); assert.equal(first.turn.nextElementSibling, null);
  f.click("sharedview", missing.bubble); assert.equal(missing.turn.nextElementSibling, null);
  keydown({ key: "Enter", target: { closest() { return null; } }, preventDefault() { assert.fail(); } });
});

test("shared bookmark duplicates compare all six address fields", async () => {
  const address = { server: "s", owner: "o", chat: "c", generation: "g", log: "mirror", seq: 1 };
  for (const field of Object.keys(address)) {
    const stored = [{ team: { ...address, [field]: "different" } }], ops = [];
    const f = viewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer([{ t: "user", seq: 1, text: "x" }], 1), undefined, undefined, { bookmarks: () => stored, uiMutate: op => ops.push(op), openSendTo() {} });
    f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
    const bubble = sharedActionBubble(1); f.click("sharedview", bubble.bubble);
    const action = { dataset: { gact: "bookmark" }, closest(sel) { return sel === "[data-gact]" ? this : null; } };
    f.click("sharedview", action); assert.equal(ops.length, 1, field);
    assert.deepEqual(ops[0].bookmark, { t: new Date(0).toISOString(), text: "x", role: "person", who: "Owner", seat: "o", from: "Chat", turnTime: undefined, team: address });
  }
});

test("shared actions ignore invalid targets and capture comments", async () => {
  const sends = [];
  const f = viewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer([{ t: "user", seq: 1, omitted: "too_large" }], 1),
    async () => ({ entries: [{ seq: 2, anchor: 1, text: "message", at: "then", author: "seat", mine: false }], names: { seat: "Ann" }, last: 2, next: 2, more: false }), undefined,
    { bookmarks: () => [], uiMutate() {}, openSendTo: opts => sends.push(opts) });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  const action = { dataset: { gact: "bookmark" }, closest(sel) { return sel === "[data-gact]" ? this : null; } };
  f.click("sharedview", action);
  const orphan = { closest(sel) { return sel === ".bubble" ? this : null; } }; f.click("sharedview", orphan);
  const omitted = sharedActionBubble(1); f.click("sharedview", omitted.bubble); assert.equal(omitted.turn.nextElementSibling, null);
  const message = sharedActionBubble(0); message.turn.dataset = { cseq: "2" }; f.click("sharedview", message.bubble);
  action.dataset.gact = "sendto"; f.click("sharedview", action);
  assert.deepEqual(sends, [{ text: "message", exceptId: "", title: "Send to…", role: "person", who: "Ann", prov: undefined, source: null }]);
  const keydown = f.get("sharedview").listeners.get("keydown");
  keydown({ key: "a" }); keydown({ key: "Enter", target: message.bubble, preventDefault() {} });
});

test("a later shared entry does not split the action row", async () => {
  let reads = 0;
  const f = viewFixture(async path => {
    if (path === "/api/team/subscriptions") return list([base]);
    if (path.endsWith("/seen")) return "";
    return ++reads === 1 ? readAnswer([{ t: "user", seq: 1, text: "first" }, { t: "user", seq: 3, text: "third" }], 3) : readAnswer([{ t: "user", seq: 2, text: "second" }], 3);
  });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  const body = f.get("sv_body");
  body.innerHTML = body.innerHTML.replace('<div class="turn assistant" data-seq="3"', '<div class="bubactions" data-gactions><button>bookmark</button></div><div class="turn assistant" data-seq="3"');
  f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.match(body.innerHTML, /data-seq="1"[\s\S]*data-gactions[\s\S]*data-seq="2"[\s\S]*data-seq="3"/);
});

test("shared send keeps focus and ignores unrelated bookmarks", async () => {
  const ops = [], sends = [];
  const f = viewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer([{ t: "user", seq: 1, text: "synthetic" }], 1), undefined, undefined,
    { bookmarks: () => [{ text: "old" }], uiMutate: op => ops.push(op), openSendTo: opts => sends.push(opts) });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  const item = sharedActionBubble(1); f.click("sharedview", item.bubble);
  const act = { dataset: { gact: "bookmark" }, closest(sel) { return sel === "[data-gact]" ? this : null; } };
  f.click("sharedview", act); assert.equal(ops.length, 1);
  const focus = { focus() { f.doc.activeElement = this; } }; focus.focus();
  act.dataset.gact = "sendto"; f.click("sharedview", act);
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

test("composer shortcuts cannot send again while a message is in flight", async () => {
  const pending = deferred(), view = { ...base, access: "write" }; let posts = 0, prevented = 0;
  const f = viewFixture(async (path, opts) => {
    if (path === "/api/team/subscriptions") return list([view]);
    if (opts?.method === "POST") { posts++; return pending.promise; }
    return readAnswer([], 0, false, view);
  });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  setGuestDraft(f, "synthetic"); f.click("sv_send");
  const key = f.get("sv_text").listeners.get("keydown");
  key({ key: "Enter", ctrlKey: true, preventDefault() { prevented++; } });
  key({ key: "Enter", metaKey: true, preventDefault() { prevented++; } });
  assert.equal(posts, 1); assert.equal(prevented, 2);
  pending.resolve({ seq: 1 }); await f.feature.settled();
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

test("shared header and groups use the current local owner name", async () => {
  const people = { "s/o": "Local owner" };
  const f = viewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer(), undefined, undefined, { people: () => people });
  f.feature.tick(); await f.feature.settled(); f.feature.tab.render(f.get("cardlist"));
  assert.match(f.get("cardlist").innerHTML, /Local owner/);
  f.click("cardlist", f.item); await f.feature.settled(); assert.equal(f.get("sv_from").textContent, "From Local owner · Server");
});

test("a local name change appears when a new answer arrives", async () => {
  const people = { "s/o": "First" };
  const f = viewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer(), undefined, undefined, { people: () => people });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  assert.equal(f.get("sv_from").textContent, "From First · Server");
  people["s/o"] = "Second";
  assert.equal(f.get("sv_from").textContent, "From First · Server");
  f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.equal(f.get("sv_from").textContent, "From Second · Server");
});

test("shared view receives local people lazily", () => {
  const app = readFileSync(new URL("../js/app.js", import.meta.url), "utf8");
  assert.match(app, /people:\s*\(\)\s*=>\s*getUI\(\)\.people/);
});

test("a read answer invalidates the list when local names changed", async () => {
  const people = { "s/o": "First" };
  const f = viewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer(), undefined, undefined, { people: () => people });
  f.feature.tick(); await f.feature.settled(); f.click("cardlist", f.item); await f.feature.settled();
  const before = f.invalidations(); people["s/o"] = "Second";
  f.setTime(5000); f.feature.tick(); await f.feature.settled();
  assert.equal(f.invalidations(), before + 1);
});

test("shared group and header mark the person they show", async () => {
  const people = { "s/o": "Local" };
  const f = viewFixture(async path => path === "/api/team/subscriptions" ? list([base]) : readAnswer(), undefined, undefined, { people: () => people });
  f.feature.tick(); await f.feature.settled(); f.feature.tab.render(f.get("cardlist"));
  assert.match(f.get("cardlist").innerHTML, /data-seat="o"/);
  f.click("cardlist", f.item); await f.feature.settled(); assert.equal(f.get("sv_from").dataset.seat, "o");
  const owned = { ...base, access: "owner" };
  const g = viewFixture(async path => path === "/api/team/subscriptions" ? list([owned]) : readAnswer([], 0, false, owned), undefined, undefined, { people: () => people });
  g.feature.tick(); await g.feature.settled(); g.click("cardlist", g.item); await g.feature.settled();
  assert.equal(g.get("sv_from").dataset.seat, undefined); assert.equal(g.get("sv_from").textContent, "Your chat · Server");
});
