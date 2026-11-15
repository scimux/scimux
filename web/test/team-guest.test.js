import test from "node:test";
import assert from "node:assert/strict";
import { createGuestFeature, guestGroups, guestKey } from "../js/team-guest.js";

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
  const doc = { activeElement: null, querySelector(sel) { if (sel === "#cardlist") return el; return null; } };
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
