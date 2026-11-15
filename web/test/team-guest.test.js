import test from "node:test";
import assert from "node:assert/strict";
import { createGuestFeature, guestGroups, guestKey, guestStateText, guestErrorText, guestEntryHTML } from "../js/team-guest.js";

const base = { server: "s", server_name: "Server", owner: "o", owner_name: "Owner", chat: "c", generation: "g", title: "Chat", access: "read", state: "live", mirror_last: 1, seen_mirror: 0, unread: false, first_seen: "1" };
const list = (subscriptions = [], servers = [], truncated = false) => ({ subscriptions, servers, truncated });
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
  for (const id of ["cardlist", "cardtabs", "sharedview", "sv_scrim", "sv_back", "sv_title", "sv_from", "sv_state", "sv_body", "sv_more", "sv_note"]) {
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
    const f = viewFixture(async path => { if (path === "/api/team/subscriptions") return list([base]); reads++; if (reads === 1) throw new Error(code === "bad-json" ? "not json" : JSON.stringify({ error: code })); return readAnswer(); });
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
