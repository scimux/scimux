/* P4 — the paired-device list, which is the other half of a grant.
 *
 * Pairing hands a device SSH-equivalent authority on this computer. A grant
 * nobody can see and nobody can take back is not a grant, it is a leak, so
 * the list and its revoke are part of the same feature rather than a
 * later nicety.
 *
 * Three properties carry the weight here and none is cosmetic:
 *   - revoking is two taps, and the second one is worded, because the row
 *     next to the one you meant is the phone you are holding.
 *   - a failed read never blanks the list, because an empty list reads as
 *     "nothing is paired" — the single most dangerous thing this surface
 *     can say when it is not true.
 *   - the device identity stays on the row. The name is whatever the
 *     device called itself, or whatever the operator renamed it to; the
 *     hash is the only part of the row a device cannot choose.
 */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import { createDeviceList } from "../js/pairing-ui.js";

const ICONS = { ICON_PENCIL: `<svg data-icon="pencil"></svg>`, ICON_LINK_SLASH: `<svg data-icon="unlink"></svg>` };

function makeEl(id) {
  const listeners = {};
  return {
    id,
    innerHTML: "",
    textContent: "",
    input: null,
    addEventListener(type, fn) {
      (listeners[type] || (listeners[type] = [])).push(fn);
    },
    removeEventListener(type, fn) {
      listeners[type] = (listeners[type] || []).filter((f) => f !== fn);
    },
    listenerCount(type) {
      return (listeners[type] || []).length;
    },
    fire(type, ev) {
      for (const fn of listeners[type] || []) fn(ev);
    },
    /* The rename field, as the focus call sees it. */
    querySelector(sel) {
      if (sel !== ".devnameedit" || !this.innerHTML.includes("devnameedit")) return null;
      if (!this.input) {
        this.input = { focused: 0, selected: 0, focus() { this.focused++; }, select() { this.selected++; } };
      }
      return this.input;
    },
    /* A click on a descendant carrying data-dev, as delegation sees it.
       data-act says which of the row's controls was hit; a bare id means
       the revoke, which is what every pre-rename test tapped. */
    click(id_, act) {
      const target = {
        closest(sel) {
          return sel === "[data-dev]" && id_ ? { dataset: { dev: id_, act: act || "revoke" } } : null;
        },
      };
      for (const fn of listeners.click || []) fn({ type: "click", target, preventDefault() {} });
    },
    /* A keystroke in the rename field. The handler reads the value off
       the element it matched, exactly as it would in a browser. */
    key(id_, key, value) {
      const el = { dataset: { devname: id_ }, value, closest(sel) { return sel === "[data-devname]" ? el : null; } };
      for (const fn of listeners.keydown || []) {
        fn({ type: "keydown", key, target: el, preventDefault() {}, stopPropagation() {} });
      }
    },
    /* Focus leaving the rename field — a tap anywhere else on a phone. */
    blurInput(id_, value) {
      const el = { dataset: { devname: id_ }, value, closest(sel) { return sel === "[data-devname]" ? el : null; } };
      for (const fn of listeners.focusout || []) fn({ type: "focusout", target: el });
    },
  };
}

function makeDoc() {
  const els = new Map([["#m_devices", makeEl("m_devices")]]);
  return { el: (s) => els.get(s), querySelector: (s) => els.get(s) || null };
}

function makeApi(routes) {
  const calls = [];
  const bodies = [];
  const fn = async (path, opts = {}) => {
    const method = (opts.method || "GET").toUpperCase();
    const key = `${method} ${path}`;
    calls.push(key);
    bodies.push(opts.body);
    const h = routes[key];
    if (!h) throw new Error(`unrouted ${key}`);
    return typeof h === "function" ? h() : h;
  };
  fn.calls = calls;
  fn.bodies = bodies;
  return fn;
}

const LIST = "GET /api/remote/devices";
const REVOKE_A = "DELETE /api/remote/devices/dev-a";
const REVOKE_B = "DELETE /api/remote/devices/dev-b";
const RENAME_A = "PATCH /api/remote/devices/dev-a";

const TWO = {
  devices: [
    { id: "dev-a", label: "Christian's <b>iPhone</b>", paired_at: "2026-08-20T09:00:00Z" },
    { id: "dev-b", label: "iPad", paired_at: "2026-08-21T09:00:00Z" },
  ],
};

/* An armed row asks in words before anything happens. */
const ARMED = /revoke access\?/i;

function setup(routes) {
  const doc = makeDoc();
  const api = makeApi(routes);
  const f = createDeviceList({ api, doc, icons: ICONS });
  f.bind();
  return { f, doc, api, host: () => doc.el("#m_devices"), html: () => doc.el("#m_devices").innerHTML };
}

test("the list names every paired device and offers each one a revoke", async () => {
  const h = setup({ [LIST]: TWO });
  await h.f.refresh();
  assert.match(h.html(), /Christian's &lt;b&gt;iPhone&lt;\/b&gt;/,
    "a device label the phone chose was rendered as markup, or not rendered at all");
  assert.match(h.html(), /iPad/);
  assert.match(h.html(), /data-dev="dev-a"/);
  assert.match(h.html(), /data-dev="dev-b"/);
});

test("each row carries a rename and a revoke, both named for a screen reader", async () => {
  /* Two round buttons, the same pair a note card carries in the notes
     workspace. An icon with no text needs an accessible name: HIG reserves
     the unlabelled glyph for metaphors every reader already shares, and
     "the link is severed" is not one of them. */
  const h = setup({ [LIST]: TWO });
  await h.f.refresh();
  assert.match(h.html(), /data-act="rename"/);
  assert.match(h.html(), /data-act="revoke"/);
  assert.match(h.html(), /class="roundactions[^"]*"|class="[^"]*roundactions/,
    "the cluster is not the shared round-button shape");
  assert.match(h.html(), /aria-label="rename this device"/);
  assert.match(h.html(), /aria-label="revoke this device's access"/);
  assert.match(h.html(), /data-icon="pencil"/, "the rename button has no glyph");
  assert.match(h.html(), /data-icon="unlink"/, "the revoke button has no glyph");
});

test("a labelled device still shows the identity it cannot choose", async () => {
  /* The label came from the device, or from the operator's own typing.
     Neither is proof of which phone this row is. The hash is. */
  const id = "f2b7eacb4e9665d78162f467b60e23aa4badc43510d422f5d6a97ace33dc2c08";
  const h = setup({ [LIST]: { devices: [{ id, label: "my iPhone" }] } });
  await h.f.refresh();
  assert.match(h.html(), /my iPhone/);
  assert.match(h.html(), /f2b7eacb…2c08/, "a renamed row hides which device it is");
});

test("a device with no label is still identifiable", async () => {
  /* A row that says nothing is a row nobody dares revoke. */
  const h = setup({ [LIST]: { devices: [{ id: "dev-a" }] } });
  await h.f.refresh();
  assert.match(h.html(), /dev-a/);
});

test("a browser device without a label has a compact name and keeps its full identity", async () => {
  const id = "f2b7eacb4e9665d78162f467b60e23aa4badc43510d422f5d6a97ace33dc2c08";
  const h = setup({ [LIST]: { devices: [{ id }] } });
  await h.f.refresh();
  assert.match(h.html(), /Device f2b7eacb…2c08/);
  assert.match(h.html(), new RegExp(`data-dev="${id}"`));
  assert.doesNotMatch(h.html(), new RegExp(`>${id}<`));
});

test("a long paired-device name cannot push its controls out of view", () => {
  const css = readFileSync(new URL("../css/sheets.css", import.meta.url), "utf8");
  assert.match(css, /#m_devices \.devname\s*\{[^}]*overflow:\s*hidden[^}]*text-overflow:\s*ellipsis[^}]*white-space:\s*nowrap/s);
  assert.match(css, /#m_devices \.devtext\s*\{[^}]*min-width:\s*0/s);
  /* The cluster floats over the row's trailing edge, so the row keeps that
     edge clear rather than letting a long name run under it. */
  assert.match(css, /#m_devices \.devrow\s*\{[^}]*padding-right:/s);
});

test("an empty list says so rather than showing nothing", async () => {
  const h = setup({ [LIST]: { devices: [] } });
  await h.f.refresh();
  assert.match(h.html(), /no devices/i);
  assert.doesNotMatch(h.html(), /data-dev=/);
});

test("the first tap on revoke reaches the network for nothing", async () => {
  /* The row next to the one you meant is the phone you are holding. */
  const h = setup({ [LIST]: TWO });
  await h.f.refresh();
  h.host().click("dev-a");
  await h.f.settled();
  assert.deepEqual(h.api.calls, [LIST], "one tap revoked a device");
  assert.match(h.html(), ARMED, "the armed row does not say what the next tap does");
});

test("the armed row asks in words, and the confirming tap carries the verb", async () => {
  /* An icon may arm a destructive action; it may not be the whole of it.
     The confirming control says Revoke, in a word, next to a way out. */
  const h = setup({ [LIST]: TWO });
  await h.f.refresh();
  h.host().click("dev-a");
  await h.f.settled();
  assert.match(h.html(), />Revoke</, "the confirming control is not worded");
  assert.match(h.html(), /data-act="cancel"/, "the armed row has no way back");
});

test("the second tap revokes that device and re-reads the list", async () => {
  const h = setup({ [LIST]: TWO, [REVOKE_A]: () => "" });
  await h.f.refresh();
  h.host().click("dev-a");
  await h.f.settled();
  h.host().click("dev-a");
  await h.f.settled();
  assert.deepEqual(h.api.calls, [LIST, REVOKE_A, LIST], "the revoke did not happen, or the list went stale after it");
});

test("cancelling an armed row leaves the grant alone", async () => {
  const h = setup({ [LIST]: TWO, [REVOKE_A]: () => "" });
  await h.f.refresh();
  h.host().click("dev-a");
  await h.f.settled();
  h.host().click("dev-a", "cancel");
  await h.f.settled();
  assert.deepEqual(h.api.calls, [LIST], "cancel revoked the device");
  assert.doesNotMatch(h.html(), ARMED, "cancel left the row armed");
});

test("arming one row disarms any other", async () => {
  /* Two armed rows means one stray tap revokes a device the human armed
   * a minute ago and then thought better of. */
  const h = setup({ [LIST]: TWO, [REVOKE_B]: () => "" });
  await h.f.refresh();
  h.host().click("dev-a");
  await h.f.settled();
  h.host().click("dev-b");
  await h.f.settled();
  assert.deepEqual(h.api.calls, [LIST], "arming a second row fired the first");

  h.host().click("dev-a");
  await h.f.settled();
  assert.deepEqual(h.api.calls, [LIST], "the first row was still armed");
});

test("a successful revoke leaves nothing armed", async () => {
  const h = setup({ [LIST]: TWO, [REVOKE_A]: () => "" });
  await h.f.refresh();
  h.host().click("dev-a");
  await h.f.settled();
  h.host().click("dev-a");
  await h.f.settled();
  assert.doesNotMatch(h.html(), ARMED, "a row stayed armed across a re-read");
});

test("a failed read never blanks the devices already on screen", async () => {
  /* An empty list reads as "nothing is paired". That is the most
   * dangerous sentence this surface can say when it is not true. */
  let fail = false;
  const h = setup({
    [LIST]: () => {
      if (fail) throw new Error("network down");
      return TWO;
    },
  });
  await h.f.refresh();
  fail = true;
  await h.f.refresh();

  assert.match(h.html(), /data-dev="dev-a"/, "a failed read emptied the list of grants");
  assert.match(h.html(), /could not/i, "the failure was invisible, so the list looked current");
});

test("a first read that fails says so instead of claiming nothing is paired", async () => {
  const h = setup({ [LIST]: () => { throw new Error("network down"); } });
  await h.f.refresh();
  assert.doesNotMatch(h.html(), /no devices/i, "an unreachable list claimed the computer had no paired devices");
  assert.match(h.html(), /could not/i);
});

test("a revoke that fails says so and leaves the device listed", async () => {
  const h = setup({ [LIST]: TWO, [REVOKE_A]: () => { throw new Error("nope"); } });
  await h.f.refresh();
  h.host().click("dev-a");
  await h.f.settled();
  h.host().click("dev-a");
  await h.f.settled();
  assert.match(h.html(), /data-dev="dev-a"/, "a device vanished from the list without being revoked");
  assert.match(h.html(), /could not/i);
});

test("destroy releases the delegation", async () => {
  const h = setup({ [LIST]: TWO });
  assert.ok(h.host().listenerCount("click") > 0);
  assert.ok(h.host().listenerCount("keydown") > 0, "the rename field's keys are not delegated");
  assert.ok(h.host().listenerCount("focusout") > 0, "leaving the rename field is not delegated");
  h.f.destroy();
  assert.equal(h.host().listenerCount("click"), 0);
  assert.equal(h.host().listenerCount("keydown"), 0);
  assert.equal(h.host().listenerCount("focusout"), 0);
});

test("an unlabelled device is named by its id, not by nothing", async () => {
  /* `dev-a` also appears in data-dev, so this asserts the part a human
     reads rather than the part the click handler reads. */
  const h = setup({ [LIST]: { devices: [{ id: "dev-a" }] } });
  await h.f.refresh();
  assert.match(h.html(), /<span class="devname">dev-a<\/span>/,
    "the row's visible name is empty, so the human is asked to revoke a blank");
});

test("a device id is escaped into the attribute that carries it", async () => {
  /* The id is server-minted today. It lands in an HTML attribute, and the
     escape is what keeps that true of tomorrow's id too. */
  const h = setup({ [LIST]: { devices: [{ id: 'a"b', label: "Phone" }] } });
  await h.f.refresh();
  assert.doesNotMatch(h.html(), /data-dev="a"b"/, "a device id broke out of its attribute");
  assert.match(h.html(), /data-dev="a&quot;b"/);
});

test("the armed row is marked destructive, not merely relabelled", async () => {
  /* Colour is the part a thumb reads before the word does. */
  const h = setup({ [LIST]: TWO });
  await h.f.refresh();
  h.host().click("dev-a");
  await h.f.settled();
  assert.match(h.html(), /class="danger"/, "the armed revoke looks like every other button");
});

test("a read that succeeds after a failure clears the failure", async () => {
  /* A stale "could not read" sitting under a current list tells the human
     the list is stale when it is not. */
  let fail = true;
  const h = setup({
    [LIST]: () => {
      if (fail) throw new Error("network down");
      return TWO;
    },
  });
  await h.f.refresh();
  assert.match(h.html(), /could not/i);
  fail = false;
  await h.f.refresh();
  assert.doesNotMatch(h.html(), /could not/i, "a healed list still reports the old failure");
});

test("re-reading the list disarms whatever was armed", async () => {
  /* The menu is closed and reopened; nobody should find a loaded revoke
     waiting under their thumb from a minute ago. */
  const h = setup({ [LIST]: TWO, [REVOKE_A]: () => "" });
  await h.f.refresh();
  h.host().click("dev-a");
  await h.f.settled();
  await h.f.refresh();
  assert.doesNotMatch(h.html(), ARMED, "a row stayed armed across a fresh read");

  h.host().click("dev-a");
  await h.f.settled();
  assert.deepEqual(h.api.calls, [LIST, LIST], "the stale arming fired a revoke");
});

test("a tap that misses every row does nothing at all", async () => {
  const h = setup({ [LIST]: TWO });
  await h.f.refresh();
  h.host().click("");
  await h.f.settled();
  assert.deepEqual(h.api.calls, [LIST]);
  assert.doesNotMatch(h.html(), ARMED);
});

/* ---------- renaming ----------
 *
 * The name on a row is a claim the device made about itself in its
 * pair-offer. Renaming is how the operator replaces that claim with
 * something they asserted. It is direct manipulation, HIG's own idiom for
 * naming: the name becomes editable in place, Return or leaving the field
 * commits, Escape puts it back.
 */

test("the pencil turns the name into a field holding the name it had", async () => {
  const h = setup({ [LIST]: TWO });
  await h.f.refresh();
  h.host().click("dev-a", "rename");
  await h.f.settled();
  assert.match(h.html(), /class="devnameedit"/, "the pencil did not open an editable name");
  assert.match(h.html(), /data-devname="dev-a"/);
  assert.match(h.html(), /value="Christian's &lt;b&gt;iPhone&lt;\/b&gt;"/,
    "the field opened empty, or opened with unescaped markup in it");
  assert.deepEqual(h.api.calls, [LIST], "opening a rename talked to the computer");
  assert.equal(h.host().input.focused, 1, "the field opened without focus, so no keyboard appears");
});

test("Return commits the new name and the row keeps what the computer stored", async () => {
  const h = setup({ [LIST]: TWO, [RENAME_A]: () => ({ id: "dev-a", label: "my iPhone" }) });
  await h.f.refresh();
  h.host().click("dev-a", "rename");
  await h.f.settled();
  h.host().key("dev-a", "Enter", "  my iPhone  ");
  await h.f.settled();
  assert.deepEqual(h.api.calls, [LIST, RENAME_A]);
  assert.deepEqual(JSON.parse(h.api.bodies[1]), { label: "my iPhone" },
    "the typed name was not trimmed before it was sent");
  assert.match(h.html(), /my iPhone/);
  assert.doesNotMatch(h.html(), /devnameedit/, "the field stayed open after the name was committed");
});

test("leaving the field commits too, which is the only way out on a phone", async () => {
  const h = setup({ [LIST]: TWO, [RENAME_A]: () => ({ id: "dev-a", label: "hotel phone" }) });
  await h.f.refresh();
  h.host().click("dev-a", "rename");
  await h.f.settled();
  h.host().blurInput("dev-a", "hotel phone");
  await h.f.settled();
  assert.deepEqual(h.api.calls, [LIST, RENAME_A]);
  assert.match(h.html(), /hotel phone/);
});

test("Escape leaves the name exactly as it was, and says nothing to the computer", async () => {
  const h = setup({ [LIST]: TWO });
  await h.f.refresh();
  h.host().click("dev-a", "rename");
  await h.f.settled();
  h.host().key("dev-a", "Escape", "something else");
  await h.f.settled();
  assert.deepEqual(h.api.calls, [LIST], "Escape sent the edit anyway");
  assert.match(h.html(), /Christian's &lt;b&gt;iPhone&lt;\/b&gt;/);
  assert.doesNotMatch(h.html(), /devnameedit/);
});

test("a rename that changes nothing is not a request", async () => {
  const h = setup({ [LIST]: TWO });
  await h.f.refresh();
  h.host().click("dev-a", "rename");
  await h.f.settled();
  h.host().key("dev-a", "Enter", "Christian's <b>iPhone</b>");
  await h.f.settled();
  assert.deepEqual(h.api.calls, [LIST], "an unchanged name still went to the computer");
});

test("a rename that fails says so and leaves the old name standing", async () => {
  const h = setup({ [LIST]: TWO, [RENAME_A]: () => { throw new Error("nope"); } });
  await h.f.refresh();
  h.host().click("dev-a", "rename");
  await h.f.settled();
  h.host().key("dev-a", "Enter", "my iPhone");
  await h.f.settled();
  assert.match(h.html(), /could not rename/i);
  assert.match(h.html(), /Christian's &lt;b&gt;iPhone&lt;\/b&gt;/, "the row kept a name the computer never stored");
});

test("a name the computer shortened is what the row shows", async () => {
  /* The computer bounds every label it stores. A row that kept showing the
     untrimmed typing would disagree with the next read of the list. */
  const h = setup({ [LIST]: TWO, [RENAME_A]: () => ({ id: "dev-a", label: "short" }) });
  await h.f.refresh();
  h.host().click("dev-a", "rename");
  await h.f.settled();
  h.host().key("dev-a", "Enter", "a much longer name than that");
  await h.f.settled();
  assert.match(h.html(), />short</);
  assert.doesNotMatch(h.html(), /a much longer name than that/);
});

test("opening a rename disarms a revoke, and arming a revoke closes a rename", async () => {
  /* One row, one pending decision. */
  const h = setup({ [LIST]: TWO, [REVOKE_A]: () => "" });
  await h.f.refresh();
  h.host().click("dev-a");
  await h.f.settled();
  h.host().click("dev-a", "rename");
  await h.f.settled();
  assert.doesNotMatch(h.html(), ARMED, "the revoke stayed armed under an open rename");

  h.host().click("dev-b");
  await h.f.settled();
  assert.doesNotMatch(h.html(), /devnameedit/, "an open rename survived arming a revoke elsewhere");
  assert.deepEqual(h.api.calls, [LIST]);
});

test("a stray keystroke outside the rename field does nothing", async () => {
  const h = setup({ [LIST]: TWO });
  await h.f.refresh();
  h.host().fire("keydown", { type: "keydown", key: "Enter", target: { closest: () => null } });
  h.host().fire("focusout", { type: "focusout", target: { closest: () => null } });
  await h.f.settled();
  assert.deepEqual(h.api.calls, [LIST]);
});

test("the shell hands the list its two glyphs", () => {
  /* The icons live in app.js with every other inlined glyph; the list is
     given them rather than reaching for a global. */
  const app = readFileSync(new URL("../js/app.js", import.meta.url), "utf8");
  assert.match(app, /const ICON_LINK_SLASH = /, "there is no revoke glyph to hand over");
  assert.match(app, /createDeviceList\(\{[^}]*icons:\s*\{[^}]*ICON_PENCIL[^}]*ICON_LINK_SLASH/s,
    "the device list is constructed without its glyphs");
});

test("a page without the list slot binds and reads without throwing", async () => {
  /* Every write is guarded, which is right for a browser: a renamed id
     must degrade to an empty section, never to a dead module that takes
     the rest of the shell down with it. */
  const doc = { querySelector: () => null };
  const api = makeApi({ [LIST]: TWO });
  const f = createDeviceList({ api, doc });
  f.bind();
  await f.refresh();
  f.destroy();
});
