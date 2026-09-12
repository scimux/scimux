/* Offering the Home Screen, once.
 *
 * A device paired through Safari works, but it works inside a browser: a
 * tab among tabs, with an address bar over it. iOS will make it a real
 * icon, and Apple lets no page do that on the user's behalf — Share,
 * then Add to Home Screen, is theirs to tap.
 *
 * What scimux owns is the offer, and the offer's manners. It is made
 * once, quietly, and never again; it lives on in the menu for whoever
 * wants it later; and it is never made where it could not be taken —
 * to a browser with no Home Screen to be added to, to a session already
 * running as an icon, or to a browser sitting on the computer's own
 * local page, where there is no rendezvous to move anything to.
 *
 * The third condition was a viewport test until 2026-09-10, and that was
 * the bug: an iPad in landscape is wider than the phone breakpoint, so
 * iPadOS — which partitions an installed icon's storage exactly as iOS
 * does, and therefore needs the move exactly as much — was told nothing
 * and shown no menu entry. What the offer depends on is whether this
 * browser has a Home Screen at all, and a width has never answered that.
 */
import test from "node:test";
import assert from "node:assert/strict";

import {
  createHomeScreenControl,
  homeScreenInstallable,
  homeScreenReachable,
} from "../js/pairing-ui.js";

const REMOTE = "blob:https://my.scimux.com/6b1e-…";
const LOCAL = "http://127.0.0.1:8765/js/app.js";

test("the move is offered only where it could be taken", () => {
  const reachable = { moduleURL: REMOTE, standalone: false, installable: true };
  assert.equal(homeScreenReachable(reachable), true);

  assert.equal(
    homeScreenReachable({ ...reachable, moduleURL: LOCAL }),
    false,
    "a browser on the computer itself has nothing to move anywhere",
  );
  assert.equal(
    homeScreenReachable({ ...reachable, standalone: true }),
    false,
    "it is already the icon it would be asked to become",
  );
  assert.equal(
    homeScreenReachable({ ...reachable, installable: false }),
    false,
    "a browser with no Home Screen cannot be told to add one",
  );
  assert.equal(homeScreenReachable({}), false, "knowing nothing offers nothing");
});

/* The capability, read off the browser rather than off its window size.
 *
 * `navigator.standalone` is WebKit's own non-standard answer to "am I an
 * installed icon", and only an iOS-family browser has an opinion at all —
 * so the property *existing* is the marker, whichever way it reads. The
 * second clause is iPadOS asking for desktop-class pages, where the UA and
 * platform say Macintosh: a Mac reports no touch points, an iPad reports
 * several, and that difference is the only honest way to tell them apart.
 */
test("the Home Screen capability is a property of the browser, not of its width", () => {
  const iphone = { standaloneProp: false, maxTouchPoints: 5, platform: "iPhone" };
  const ipad = { standaloneProp: false, maxTouchPoints: 5, platform: "iPad" };

  assert.equal(homeScreenInstallable(iphone), true);
  assert.equal(
    homeScreenInstallable(ipad),
    true,
    "an iPad partitions an installed icon's storage exactly as an iPhone does",
  );
  assert.equal(
    homeScreenInstallable({ standaloneProp: true, maxTouchPoints: 5, platform: "iPad" }),
    true,
    "already being an icon is a different question, asked elsewhere",
  );
  assert.equal(
    homeScreenInstallable({ maxTouchPoints: 5, platform: "MacIntel" }),
    true,
    "iPadOS in desktop-class mode is still an iPad: a Mac has no touch points",
  );
  assert.equal(
    homeScreenInstallable({ maxTouchPoints: 0, platform: "MacIntel" }),
    false,
    "a Mac has no Home Screen to add anything to",
  );
  assert.equal(
    homeScreenInstallable({ maxTouchPoints: 0, platform: "Linux x86_64" }),
    false,
  );
  assert.equal(
    homeScreenInstallable({ maxTouchPoints: 5, platform: "Linux armv8l" }),
    false,
    "Android installs through its own prompt and partitions no storage; this move is an iOS workaround",
  );
  assert.equal(homeScreenInstallable({}), false, "knowing nothing offers nothing");
  assert.equal(homeScreenInstallable(), false);
});

/* ------------------------------------------------------------ wiring */

function makeEl(id) {
  return { id, hidden: true };
}

function makeDoc() {
  const els = new Map([
    ["#m_homescreen", makeEl("m_homescreen")],
    ["#hsoffer", makeEl("hsoffer")],
  ]);
  return { el: s => els.get(s), querySelector: s => els.get(s) || null };
}

function makeStorage(seed = {}) {
  const m = new Map(Object.entries(seed));
  return {
    m,
    getItem: k => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => void m.set(k, String(v)),
  };
}

function makeWin() {
  const win = {
    reloads: 0,
    location: { hash: "", pathname: "/p", reload: () => void (win.reloads += 1) },
  };
  return win;
}

function setup({ moduleURL = REMOTE, standalone = false, installable = true, storage } = {}) {
  const doc = makeDoc();
  const win = makeWin();
  const store = storage || makeStorage();
  const f = createHomeScreenControl({ doc, win, storage: store, moduleURL, standalone, installable });
  f.bind();
  return { f, doc, win, store, menu: () => doc.el("#m_homescreen"), offer: () => doc.el("#hsoffer") };
}

test("a paired iPhone or iPad in Safari is shown the offer, and keeps the menu entry", () => {
  const s = setup();
  assert.equal(s.offer().hidden, false);
  assert.equal(s.menu().hidden, false);
});

test("the offer is made once and never again", () => {
  const store = makeStorage();
  setup({ storage: store });

  const again = setup({ storage: store });
  assert.equal(again.offer().hidden, true, "it asked a second time");
  assert.equal(again.menu().hidden, false, "but it must still be findable on purpose");
});

/* Showing it is what spends it. Someone who scrolls past an offer they
 * did not read has still been asked, and asking again is nagging. */
test("showing the offer spends it, whether or not it is answered", () => {
  const s = setup();
  assert.equal(s.store.m.size, 1, "the offer left no record that it was made");
});

test("a Home Screen app is offered nothing, and told about nothing", () => {
  const s = setup({ standalone: true });
  assert.equal(s.offer().hidden, true);
  assert.equal(s.menu().hidden, true, "there is no move left to make");
});

test("a browser on the computer itself is offered nothing", () => {
  const s = setup({ moduleURL: LOCAL });
  assert.equal(s.offer().hidden, true);
  assert.equal(s.menu().hidden, true);
});

/* The regression this file was rewritten for: the offer must not be spent,
 * or withheld, on a device that could have taken it. */
test("a browser with no Home Screen is offered nothing and spends nothing", () => {
  const s = setup({ installable: false });
  assert.equal(s.offer().hidden, true);
  assert.equal(s.menu().hidden, true);
  assert.equal(s.store.m.size, 0, "an offer that was never made must not count as made");
});

test("taking the offer hands the page to the rendezvous", () => {
  const s = setup();
  s.f.move();
  assert.equal(s.win.location.hash, "move");
  assert.equal(s.win.reloads, 1, "the fragment alone does not re-run the page that reads it");
});

test("declining the offer only closes it", () => {
  const s = setup();
  s.f.dismiss();
  assert.equal(s.offer().hidden, true);
  assert.equal(s.win.reloads, 0);
  assert.equal(s.menu().hidden, false, "declining once is not declining forever");
});

/* Private Browsing throws on write. Losing the once-only record costs a
 * repeated offer, which is a manners problem; refusing to offer at all
 * costs the feature. */
test("storage that refuses to remember still lets the offer be made", () => {
  const hostile = {
    getItem() {
      throw new Error("denied");
    },
    setItem() {
      throw new Error("denied");
    },
  };
  const s = setup({ storage: hostile });
  assert.equal(s.offer().hidden, false);
});
