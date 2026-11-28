import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { createShareFeature, laneCountLine, shareErrorText, shareStatusText } from "../js/team-share.js";
import { createSheetsFeature } from "../js/sheets.js";

const north = { id: "server-north", name: "North", url: "https://north.example/team", seat_name: "Blue" };
const south = { id: "server-south", name: "South", url: "https://south.example/team", seat_name: "Red" };
const selfId = "seat-self";
const blue = { id: "seat-blue", name: "Blue" };
const red = { id: "seat-red", name: "Red" };
const marked = { id: "seat-mark", name: 'A <b> & "q"' };

const ERRORS = [
  ["unreachable", "The server could not be reached."],
  ["bad_server", "That address no longer answers as this server."],
  ["refused", "The server ended this computer's access."],
  ["unknown_member", "Someone in the list has left the server. Open the sheet again."],
  ["server_full", "The server has no room for another shared chat."],
  ["too_many", "This computer shares too many chats. Stop one first."],
  ["no_history", "This chat has nothing to share yet."],
  ["not_active", "Sharing of this chat has ended. Stop it, then share again."],
  ["share_fixed", "Stop sharing first to change the server or the scope."],
  ["share_changed", "Sharing changed. Close this sheet and review it again."],
  ["store_unavailable", "This computer could not save the change. Try again."],
  ["lane_share", "This chat is shared through its lane."],
  ["nope", "That did not work. Try again."],
];

function viewOf(over = {}) {
  return {
    uid: "share-1",
    node: over.node || "node-1",
    server: north.id,
    scope: "from_now",
    state: "active",
    delivery: "ok",
    members: [{ seat: blue.id, access: "read" }],
    shared_at: "2026-11-14T09:30:00Z",
    ...over,
  };
}

function members(n) {
  const all = [
    { seat: blue.id, access: "read" },
    { seat: red.id, access: "write" },
    { seat: "seat-third", access: "read" },
  ];
  return all.slice(0, n);
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((ok, bad) => {
    resolve = ok;
    reject = bad;
  });
  return { promise, resolve, reject };
}

function escapeHTML(s) {
  return String(s)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

function makeEl(tag) {
  const listeners = new Map();
  const node = {
    tagName: String(tag || "div").toUpperCase(),
    id: "",
    hidden: false,
    disabled: false,
    value: "",
    name: "",
    type: "",
    checked: false,
    selected: false,
    dataset: {},
    children: [],
    parentElement: null,
    ariaLabel: "",
    _text: "",
    _html: "",
    listeners,
    addEventListener(type, fn) {
      if (!listeners.has(type)) listeners.set(type, new Set());
      listeners.get(type).add(fn);
    },
    removeEventListener(type, fn) {
      const set = listeners.get(type);
      if (set) set.delete(fn);
    },
    focus() {
      node.ownerDoc.activeElement = node;
    },
    click() {
      bubble(node, "click");
    },
    emit(type) {
      bubble(node, type);
    },
  };
  Object.defineProperty(node, "textContent", {
    get() {
      if (node.children.length) {
        return node._text + node.children.map((child) => child.textContent).join("");
      }
      return node._text;
    },
    set(value) {
      node._text = String(value ?? "");
      node._html = escapeHTML(node._text);
      node.children = [];
    },
  });
  Object.defineProperty(node, "innerHTML", {
    get() {
      return node._html;
    },
    set(value) {
      node._html = String(value);
      node._text = "";
      node.children = parseHTML(node._html);
      link(node);
      settleSelects(node);
    },
  });
  node.querySelector = (sel) => queryAll(node, sel)[0] || null;
  node.querySelectorAll = (sel) => queryAll(node, sel);
  return node;
}

function bubble(origin, type) {
  const ev = { type, target: origin };
  let current = origin;
  while (current) {
    const set = current.listeners && current.listeners.get(type);
    if (set) for (const fn of [...set]) fn(ev);
    current = current.parentElement;
  }
}

function link(parent) {
  for (const child of parent.children) {
    child.parentElement = parent;
    child.ownerDoc = parent.ownerDoc;
    link(child);
  }
}

function settleSelects(node) {
  if (node.tagName === "SELECT") {
    let found = false;
    let chosen = "";
    for (const child of node.children) {
      if (child.tagName === "OPTION" && child.selected) {
        chosen = child.value;
        found = true;
      }
    }
    if (!found) {
      for (const child of node.children) {
        if (child.tagName === "OPTION") {
          chosen = child.value;
          break;
        }
      }
    }
    node.value = chosen;
  }
  for (const child of node.children) settleSelects(child);
}

function parseHTML(html) {
  const root = { children: [], _text: "" };
  const stack = [root];
  const re = /<\/?([a-zA-Z0-9]+)([^>]*)>|([^<]+)/g;
  let match;
  while ((match = re.exec(html))) {
    if (match[3]) {
      stack[stack.length - 1]._text += match[3];
      continue;
    }
    const tag = match[1];
    const closing = html[match.index + 1] === "/";
    if (closing) {
      if (stack.length > 1) stack.pop();
      continue;
    }
    const node = makeEl(tag);
    applyAttrs(node, match[2] || "");
    stack[stack.length - 1].children.push(node);
    const selfClose = /\/\s*$/.test(match[2] || "");
    if (tag !== "input" && !selfClose) stack.push(node);
  }
  return root.children;
}

function applyAttrs(node, raw) {
  const re = /([:@\w-]+)(?:="([^"]*)")?/g;
  let match;
  while ((match = re.exec(raw))) {
    const key = match[1];
    const val = match[2] == null ? "" : match[2];
    if (key === "id") node.id = val;
    else if (key === "value") node.value = val;
    else if (key === "name") node.name = val;
    else if (key === "type") node.type = val;
    else if (key === "checked") node.checked = true;
    else if (key === "selected") node.selected = true;
    else if (key === "hidden") node.hidden = true;
    else if (key === "aria-label") node.ariaLabel = val;
    else if (key.startsWith("data-")) node.dataset[key.slice(5)] = val;
  }
}

function match(node, sel) {
  if (sel.startsWith("#")) return node.id === sel.slice(1);
  if (sel === "input") return node.tagName === "INPUT";
  if (sel === "select") return node.tagName === "SELECT";
  if (sel === "button") return node.tagName === "BUTTON";
  if (sel === "p") return node.tagName === "P";
  return false;
}

function queryAll(root, sel) {
  const out = [];
  const stack = [...(root.children || [])];
  while (stack.length) {
    const node = stack.shift();
    if (match(node, sel)) out.push(node);
    if (node.children) stack.push(...node.children);
  }
  return out;
}

function slot(doc, id, tag) {
  const node = makeEl(tag || "div");
  node.id = id;
  node.ownerDoc = doc;
  doc.byId.set(id, node);
  return node;
}

function mount() {
  const doc = { activeElement: null, byId: new Map() };
  const btn = slot(doc, "sharebtn", "button");
  const status = slot(doc, "sharestatus", "button");
  const sheet = slot(doc, "sharesheet", "div");
  const body = slot(doc, "sh_body", "div");
  const note = slot(doc, "sh_note", "div");
  const apply = slot(doc, "sh_apply", "button");
  const stop = slot(doc, "sh_stop", "button");
  const live = slot(doc, "sh_live", "div");
  btn.hidden = true;
  status.hidden = true;
  note.hidden = true;
  stop.hidden = true;
  for (const child of [body, note, apply, stop, live]) {
    child.parentElement = sheet;
    sheet.children.push(child);
  }
  doc.querySelector = (sel) => {
    if (typeof sel === "string" && sel.startsWith("#")) {
      const known = doc.byId.get(sel.slice(1));
      if (known) return known;
    }
    for (const root of doc.byId.values()) {
      if (match(root, sel)) return root;
      const found = queryAll(root, sel)[0];
      if (found) return found;
    }
    return null;
  };
  return { doc, btn, status, sheet, body, note, apply, stop, live };
}

function rejected(status, message) {
  const err = new Error(message);
  err.status = status;
  return Promise.reject(err);
}

async function until(pred) {
  for (let i = 0; i < 40; i++) {
    if (pred()) return;
    await Promise.resolve();
  }
  throw new Error("timed out waiting for " + pred);
}

function setup(handler, extra) {
  const calls = [];
  const opened = [];
  const closed = [];
  const time = { now: 0 };
  const ui = mount();
  const backdrop = slot(ui.doc, "backdrop", "div");
  ui.sheet.className = "sheet";
  const sheets = createSheetsFeature({
    document: ui.doc,
    querySelectorAll: () => [ui.sheet],
  });
  const api = async (path, opts) => {
    const call = {
      path,
      method: (opts && opts.method) || "GET",
      body: opts && opts.body ? JSON.parse(opts.body) : null,
    };
    calls.push(call);
    return handler(call, calls);
  };
  const feature = createShareFeature({
    api,
    doc: ui.doc,
    openSheet: (id) => { opened.push(id); sheets.openSheet(id); },
    closeSheets: () => { closed.push("closed"); sheets.closeSheets(); },
    now: () => time.now,
    ...(extra || {}),
  });
  feature.bind();
  return { ...ui, calls, opened, closed, time, feature, sheets, backdrop };
}

test("share controls open an interactive sheet through the real sheet controller", async () => {
  for (const control of ["btn", "status"]) {
    const ui = setup((call) => {
      if (call.path === "/api/team/servers") return { servers: [north] };
      if (call.path.endsWith("/seats")) return { self: selfId, seats: [blue] };
      return viewOf();
    });
    ui.feature.select("node-1");
    await ui.feature.settled();
    assert.equal(ui[control].hidden, false);
    ui[control].click();
    await ui.feature.settled();
    assert.match(ui.sheet.className, /\bopen\b/);
    assert.equal(Boolean(ui.sheet.inert), false);
    assert.match(ui.backdrop.className, /\bon\b/);
    assert.equal(selects(ui.body)[0].disabled, false);
    ui.sheets.closeSheets();
    assert.doesNotMatch(ui.sheet.className, /\bopen\b/);
    assert.equal(ui.sheet.inert, true);
    assert.doesNotMatch(ui.backdrop.className, /\bon\b/);
  }
});

function peopleOf(body) {
  return body.querySelector("#sh_people");
}

function inputs(body, name) {
  return body.querySelectorAll("input").filter((input) => input.name === name);
}

function selects(body) {
  return body.querySelectorAll("input").filter(input => input.type === "checkbox" && input.dataset.seat);
}

test("shareStatusText covers every row for one member and for three", () => {
  const rows = [
    [{ state: "active", delivery: "ok" }, (n, name) => "Shared with " + n + " · " + name],
    [{ state: "active", delivery: "offline" }, (n, name) => "Shared with " + n + " · " + name + " · offline, will retry"],
    [{ state: "active", delivery: "refused" }, (_n, name) => "Sharing stopped · " + name + " ended this computer's access"],
    [{ state: "active", delivery: "changed" }, (_n, name) => "Sharing paused · " + name + " is not the same server"],
    [{ state: "stopping", delivery: "ok" }, () => "Stopping sharing…"],
    [{ state: "lost", delivery: "ok" }, (_n, name) => "Sharing ended on " + name],
    [{ state: "full", delivery: "ok" }, (_n, name) => "Sharing paused · " + name + " is full"],
  ];
  for (const [base, text] of rows) {
    for (const n of [1, 3]) {
      const view = { ...base, members: members(n) };
      assert.equal(shareStatusText(view, "North"), text(n, "North"), base.state + " " + base.delivery + " " + n);
    }
  }
  assert.equal(shareStatusText({ state: "active", delivery: "ok" }, "North"), "Shared with 0 · North");
  assert.equal(
    shareStatusText(viewOf({ server: "gone" }), "a server you left"),
    "Shared with 1 · a server you left",
  );
});

test("shareErrorText covers every code and an unknown one", () => {
  for (const [code, text] of ERRORS) assert.equal(shareErrorText(code), text);
  assert.equal(shareErrorText(""), "That did not work. Try again.");
});

test("changing the selection probes even while the gate is off", async () => {
  const { feature, calls, btn, status } = setup(() => rejected(404, JSON.stringify({ error: "not_found" })));
  feature.select("node-1");
  await feature.settled();
  assert.deepEqual(calls.map((call) => call.path), ["/api/team/servers"]);
  assert.equal(btn.hidden, true);
  assert.equal(status.hidden, true);
  for (let i = 0; i < 10; i++) feature.select("node-" + (i + 2));
  for (let i = 0; i < 10; i++) feature.tick();
  await feature.settled();
  assert.equal(calls.length, 11);
  assert.equal(btn.hidden, true);
  assert.equal(status.hidden, true);
});

test("no joined server hides both controls", async () => {
  const { feature, btn, status } = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [] };
    return rejected(404, JSON.stringify({ error: "not_found" }));
  });
  btn.hidden = false;
  status.hidden = false;
  feature.select("node-1");
  assert.equal(btn.hidden, true);
  assert.equal(status.hidden, true);
  await feature.settled();
  assert.equal(btn.hidden, true);
  assert.equal(status.hidden, true);
});

test("a servers payload that is not a list hides both controls", async () => {
  for (const body of [null, { servers: "no" }]) {
    const { feature, btn, status, calls } = setup((call) => {
      if (call.path === "/api/team/servers") return body;
      return rejected(404, JSON.stringify({ error: "not_found" }));
    });
    feature.select("node-1");
    await feature.settled();
    assert.equal(btn.hidden, true, JSON.stringify(body));
    assert.equal(status.hidden, true);
    assert.equal(calls.some((call) => call.path.startsWith("/api/team/shares/")), true);
  }
});

test("one server and no share shows only the button", async () => {
  const { feature, btn, status } = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    return rejected(404, JSON.stringify({ error: "not_found" }));
  });
  feature.select("node-1");
  assert.equal(btn.hidden, true);
  await feature.settled();
  assert.equal(btn.hidden, false);
  assert.equal(status.hidden, true);
  assert.match(btn.innerHTML, /<svg/);
  assert.match(btn.innerHTML, /currentColor/);
  assert.doesNotMatch(btn.innerHTML, /url\(|href=/);
});

test("one server and a share shows the status line", async () => {
  const { feature, btn, status } = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    return viewOf();
  });
  feature.select("node-1");
  await feature.settled();
  assert.equal(btn.hidden, false);
  assert.equal(status.hidden, false);
  assert.equal(status.textContent, "Shared with 1 · North");
});

test("the status line shows every row, both counts, and a server that was left", async () => {
  const cases = [
    [viewOf({ members: members(1) }), "Shared with 1 · North"],
    [viewOf({ members: members(3) }), "Shared with 3 · North"],
    [viewOf({ delivery: "offline", members: members(1) }), "Shared with 1 · North · offline, will retry"],
    [viewOf({ delivery: "offline", members: members(3) }), "Shared with 3 · North · offline, will retry"],
    [viewOf({ delivery: "refused", members: members(1) }), "Sharing stopped · North ended this computer's access"],
    [viewOf({ delivery: "refused", members: members(3) }), "Sharing stopped · North ended this computer's access"],
    [viewOf({ delivery: "changed", members: members(1) }), "Sharing paused · North is not the same server"],
    [viewOf({ delivery: "changed", members: members(3) }), "Sharing paused · North is not the same server"],
    [viewOf({ state: "stopping", members: members(1) }), "Stopping sharing…"],
    [viewOf({ state: "stopping", members: members(3) }), "Stopping sharing…"],
    [viewOf({ state: "lost", members: members(1) }), "Sharing ended on North"],
    [viewOf({ state: "lost", members: members(3) }), "Sharing ended on North"],
    [viewOf({ state: "full", members: members(1) }), "Sharing paused · North is full"],
    [viewOf({ state: "full", members: members(3) }), "Sharing paused · North is full"],
    [viewOf({ server: "server-gone", members: members(1) }), "Shared with 1 · a server you left"],
  ];
  for (const [view, text] of cases) {
    const { feature, status } = setup((call) => {
      if (call.path === "/api/team/servers") return { servers: [north] };
      return view;
    });
    feature.select("node-1");
    await feature.settled();
    assert.equal(status.hidden, false, text);
    assert.equal(status.textContent, text);
  }
});

test("a servers failure other than 404 hides both and does not turn the gate off", async () => {
  let fail = true;
  const { feature, calls, btn, status } = setup((call) => {
    if (call.path === "/api/team/servers") {
      if (fail) return rejected(500, JSON.stringify({ error: "store_unavailable" }));
      return { servers: [north] };
    }
    return rejected(404, JSON.stringify({ error: "not_found" }));
  });
  feature.select("node-1");
  await feature.settled();
  assert.equal(btn.hidden, true);
  assert.equal(status.hidden, true);
  assert.equal(calls.length, 1);
  fail = false;
  feature.select("node-1");
  await feature.settled();
  assert.equal(calls.filter((call) => call.path === "/api/team/servers").length, 2);
  assert.equal(btn.hidden, false);
  assert.equal(status.hidden, true);
});

test("a share failure other than 404 hides both and the next select tries again", async () => {
  let fail = true;
  const { feature, btn, status, calls } = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (fail) return rejected(502, JSON.stringify({ error: "unreachable" }));
    return rejected(404, JSON.stringify({ error: "not_found" }));
  });
  feature.select("node-1");
  await feature.settled();
  assert.equal(btn.hidden, true);
  assert.equal(status.hidden, true);
  fail = false;
  feature.select("node-1");
  await feature.settled();
  assert.equal(btn.hidden, false);
  assert.equal(status.hidden, true);
  assert.equal(calls.filter((call) => call.path === "/api/team/shares/node-1").length, 2);
});

test("a share payload that is not a view shows the button without a status line", async () => {
  const { feature, btn, status } = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    return "";
  });
  feature.select("node-1");
  await feature.settled();
  assert.equal(btn.hidden, false);
  assert.equal(status.hidden, true);
});

test("an answer for a node that is no longer selected is dropped", async () => {
  const gates = [];
  const { feature, calls, status } = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    const gate = deferred();
    gates.push(gate);
    return gate.promise;
  });
  feature.select("node-a");
  await until(() => calls.some((call) => call.path === "/api/team/shares/node-a"));
  feature.select("node-b");
  gates[0].resolve(viewOf({ members: members(1), delivery: "ok" }));
  await until(() => calls.some((call) => call.path === "/api/team/shares/node-b"));
  assert.equal(status.hidden, true);
  assert.equal(status.textContent, "");
  gates[1].resolve(viewOf({ members: members(3), delivery: "offline" }));
  await feature.settled();
  assert.equal(status.textContent, "Shared with 3 · North · offline, will retry");
});

test("a servers answer that arrives after a newer select is dropped", async () => {
  const gates = [];
  const { feature, calls, status } = setup((call) => {
    if (call.path === "/api/team/servers") {
      const gate = deferred();
      gates.push(gate);
      return gate.promise;
    }
    return viewOf({ delivery: "changed", members: members(3) });
  });
  feature.select("node-a");
  feature.select("node-b");
  gates[0].resolve({ servers: [north] });
  await until(() => gates.length === 2);
  for (let i = 0; i < 8; i++) await Promise.resolve();
  assert.equal(calls.some((call) => call.path === "/api/team/shares/node-a"), false);
  gates[1].resolve({ servers: [north] });
  await feature.settled();
  assert.equal(calls.some((call) => call.path === "/api/team/shares/node-a"), false);
  assert.equal(status.textContent, "Sharing paused · North is not the same server");
});

test("tick waits, skips a load that is in flight, and skips a missing node", async () => {
  const gate = deferred();
  const { feature, calls, time, body, note, doc } = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (calls.filter((item) => item.path.startsWith("/api/team/shares/")).length === 0) return gate.promise;
    return viewOf();
  });
  feature.tick();
  await feature.settled();
  assert.equal(calls.length, 0);
  feature.select("node-1");
  await until(() => calls.some((call) => call.path === "/api/team/shares/node-1"));
  time.now = 10000;
  feature.tick();
  assert.equal(calls.filter((call) => call.path === "/api/team/shares/node-1").length, 1);
  gate.resolve(viewOf());
  await feature.settled();
  const before = calls.length;
  body.innerHTML = "<p>keep</p>";
  note.textContent = "keep";
  note.hidden = false;
  body.focus();
  time.now = 9999;
  feature.tick();
  await feature.settled();
  assert.equal(calls.length, before);
  time.now = 10000;
  feature.tick();
  await feature.settled();
  assert.equal(calls.filter((call) => call.path === "/api/team/shares/node-1").length, 2);
  assert.equal(body.innerHTML, "<p>keep</p>");
  assert.equal(note.textContent, "keep");
  assert.equal(note.hidden, false);
  assert.equal(doc.activeElement, body);
});

test("tick updates the header and leaves the sheet, the note, and the focus", async () => {
  let delivery = "ok";
  const { feature, time, body, note, status, doc, btn, opened } = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return { self: selfId, seats: [selfSeat(), blue] };
    return viewOf({ delivery });
  });
  feature.select("node-1");
  await feature.settled();
  btn.click();
  await feature.settled();
  const seat = selects(body)[0];
  seat.focus();
  const html = body.innerHTML;
  const peopleHTML = peopleOf(body).innerHTML;
  const focused = doc.activeElement;
  note.textContent = "keep";
  note.hidden = false;
  delivery = "offline";
  time.now = 10000;
  feature.tick();
  await feature.settled();
  assert.equal(status.textContent, "Shared with 1 · North · offline, will retry");
  assert.equal(body.innerHTML, html);
  assert.equal(peopleOf(body).innerHTML, peopleHTML);
  assert.equal(note.textContent, "keep");
  assert.equal(note.hidden, false);
  assert.equal(doc.activeElement, focused);
  assert.deepEqual(opened, ["#sharesheet"]);
});

test("a server name with markup is text on the status line", async () => {
  const named = { ...north, name: 'A <b> & "q"' };
  const { feature, status } = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [named] };
    return viewOf();
  });
  feature.select("node-1");
  await feature.settled();
  assert.equal(status.textContent, 'Shared with 1 · A <b> & "q"');
  assert.equal(status.innerHTML, "Shared with 1 · A &lt;b&gt; &amp; &quot;q&quot;");
  assert.doesNotMatch(status.innerHTML, /<b>/);
});

function selfSeat() {
  return { id: selfId, name: "Self" };
}

function roster(seats) {
  return { self: selfId, seats };
}

test("opening and choosing send no share request", async () => {
  const { feature, calls, btn, body, opened, apply } = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north, south] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue, red]);
    return rejected(404, JSON.stringify({ error: "not_found" }));
  });
  feature.select("node-1");
  await feature.settled();
  const before = calls.length;
  btn.click();
  await feature.settled();
  assert.deepEqual(opened, ["#sharesheet"]);
  const chosen = calls.slice(before);
  assert.deepEqual(chosen.map((call) => call.path), ["/api/team/servers/server-north/seats"]);
  assert.equal(calls.some((call) => call.method === "PUT"), false);
  assert.match(body.innerHTML, /<legend>Server<\/legend>/);
  assert.match(body.innerHTML, /<legend>What to share<\/legend>/);
  assert.match(body.innerHTML, /Prompts and answers only\./);
  assert.match(body.innerHTML, /Tool details stay on this computer\./);
  const servers = inputs(body, "sh_server");
  assert.equal(servers[0].checked, true);
  assert.equal(servers[1].checked, false);
  const scopes = inputs(body, "sh_scope");
  assert.equal(scopes[0].checked, true);
  assert.equal(scopes[0].value, "from_now");
  servers[0].checked = false;
  servers[1].checked = true;
  servers[1].emit("change");
  await feature.settled();
  scopes[0].checked = false;
  scopes[1].checked = true;
  scopes[1].emit("change");
  const choice = selects(body)[0];
  choice.checked = true;
  choice.emit("change");
  await feature.settled();
  assert.equal(calls.some((call) => call.method === "PUT"), false);
  assert.equal(apply.textContent, "Share chat");
  assert.equal(apply.disabled, false);
});

test("the put body covers nobody, read, read and write, entire history, and the second server", async () => {
  const cases = [
    { choose: () => {}, members: [] },
    { choose: (body) => { selects(body)[0].checked = true; }, members: [{ seat: blue.id, access: "write" }] },
    {
      choose: (body) => {
        selects(body)[0].checked = true;
        selects(body)[1].checked = true;
      },
      members: [{ seat: blue.id, access: "write" }, { seat: red.id, access: "write" }],
    },
    {
      scope: "all",
      choose: () => {},
      members: [],
    },
    {
      server: south.id,
      choose: () => {},
      members: [],
    },
  ];
  for (const item of cases) {
    const seen = [];
    const { feature, btn, body, apply, calls } = setup((call) => {
      if (call.path === "/api/team/servers") return { servers: [north, south] };
      if (call.path.endsWith("/seats")) return roster([selfSeat(), blue, red]);
      if (call.method === "PUT") {
        seen.push(call.body);
        return viewOf({ server: call.body.server, scope: call.body.scope, members: call.body.members });
      }
      return rejected(404, JSON.stringify({ error: "not_found" }));
    });
    feature.select("node-1");
    await feature.settled();
    btn.click();
    await feature.settled();
    if (item.server) {
      const radios = inputs(body, "sh_server");
      radios[0].checked = false;
      radios[1].checked = true;
      radios[1].emit("change");
      await feature.settled();
    }
    if (item.scope === "all") {
      const radios = inputs(body, "sh_scope");
      radios[0].checked = false;
      radios[1].checked = true;
    }
    item.choose(body);
    apply.click();
    await feature.settled();
    assert.equal(calls.filter((call) => call.method === "PUT").length, 1);
    assert.deepEqual(seen[0], {
      server: item.server || north.id,
      scope: item.scope || "from_now",
      members: item.members,
    });
  }
});

test("a share with nothing chosen uses the first server and from now on", async () => {
  const { feature, btn, body, apply, calls } = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north, south] };
    if (call.path.endsWith("/seats")) return roster([]);
    if (call.method === "PUT") return viewOf({ members: [] });
    return rejected(404, JSON.stringify({ error: "not_found" }));
  });
  feature.select("node-1");
  await feature.settled();
  btn.click();
  await feature.settled();
  for (const input of body.querySelectorAll("input")) input.checked = false;
  apply.click();
  await feature.settled();
  const put = calls.find((call) => call.method === "PUT");
  assert.equal(put.body.server, north.id);
  assert.equal(put.body.scope, "from_now");
  assert.deepEqual(put.body.members, []);
});

test("self is not a row, an empty roster says so, and a failed roster shows the error", async () => {
  const emptyBodies = [roster([]), roster([selfSeat()]), null, { seats: "no" }, { seats: [blue] }];
  for (const body of emptyBodies) {
    const { feature, btn, body: sheet } = setup((call) => {
      if (call.path === "/api/team/servers") return { servers: [north] };
      if (call.path.endsWith("/seats")) return body;
      return rejected(404, JSON.stringify({ error: "not_found" }));
    });
    feature.select("node-1");
    await feature.settled();
    btn.click();
    await feature.settled();
    const host = peopleOf(sheet);
    if (body && Array.isArray(body.seats) && typeof body.self === "string") {
      assert.equal(host.textContent, "Nobody else has joined this server yet.");
    } else {
      assert.equal(host.textContent, "That did not work. Try again.");
    }
    assert.equal(selects(sheet).length, 0);
  }
  const { feature, btn, body } = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue, marked]);
    return rejected(404, JSON.stringify({ error: "not_found" }));
  });
  feature.select("node-1");
  await feature.settled();
  btn.click();
  await feature.settled();
  const rows = selects(body);
  assert.deepEqual(rows.map((row) => row.dataset.seat), [blue.id, marked.id]);
  assert.equal(rows.some((row) => row.dataset.seat === selfId), false);
  assert.match(peopleOf(body).innerHTML, /A &lt;b&gt; &amp; &quot;q&quot;/);
  assert.doesNotMatch(peopleOf(body).innerHTML, /A <b>/);
  assert.match(peopleOf(body).innerHTML, /<label><input type="checkbox"/);
  const failed = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return rejected(502, JSON.stringify({ error: "unreachable" }));
    return rejected(404, JSON.stringify({ error: "not_found" }));
  });
  failed.feature.select("node-1");
  await failed.feature.settled();
  failed.btn.click();
  await failed.feature.settled();
  assert.equal(peopleOf(failed.body).textContent, "The server could not be reached.");
  assert.equal(selects(failed.body).length, 0);
});

test("a late roster for the previous server is dropped", async () => {
  const gates = [];
  const { feature, btn, body, calls } = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north, south] };
    if (call.path.endsWith("/seats")) {
      const gate = deferred();
      gates.push(gate);
      return gate.promise;
    }
    return rejected(404, JSON.stringify({ error: "not_found" }));
  });
  feature.select("node-1");
  await feature.settled();
  btn.click();
  await until(() => calls.some((call) => call.path.endsWith("/seats")));
  const radios = inputs(body, "sh_server");
  radios[0].checked = false;
  radios[1].checked = true;
  radios[1].emit("change");
  await until(() => calls.filter((call) => call.path.endsWith("/seats")).length === 2);
  gates[0].resolve(roster([selfSeat(), { id: "seat-late", name: "Late" }]));
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(peopleOf(body).textContent.includes("Late"), false);
  gates[1].resolve(roster([selfSeat(), red]));
  await feature.settled();
  assert.equal(selects(body)[0].dataset.seat, red.id);
  assert.equal(peopleOf(body).textContent.includes("Late"), false);
});

test("every put error is shown and the sheet stays as it was", async () => {
  const failures = [
    ...ERRORS.map(([code, text]) => ({ message: JSON.stringify({ error: code }), text, status: 409 })),
    { message: "nope", text: "That did not work. Try again.", status: 500 },
    { message: "null", text: "That did not work. Try again.", status: 500 },
    { message: JSON.stringify({ error: 1 }), text: "That did not work. Try again.", status: 500 },
  ];
  for (const item of failures) {
    const { feature, btn, body, apply, note, status, calls } = setup((call) => {
      if (call.path === "/api/team/servers") return { servers: [north] };
      if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
      if (call.method === "PUT") return rejected(item.status, item.message);
      return rejected(404, JSON.stringify({ error: "not_found" }));
    });
    feature.select("node-1");
    await feature.settled();
    btn.click();
    await feature.settled();
    const html = body.innerHTML;
    apply.click();
    await feature.settled();
    assert.equal(note.hidden, false, item.message);
    assert.equal(note.textContent, item.text);
    assert.equal(body.innerHTML, html);
    assert.equal(apply.textContent, "Share chat");
    assert.equal(status.hidden, true);
    assert.equal(calls.filter((call) => call.method === "PUT").length, 1);
  }
});

test("a second click while sharing sends nothing", async () => {
  const gate = deferred();
  const { feature, btn, apply, stop, calls } = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([]);
    if (call.method === "PUT") return gate.promise;
    return rejected(404, JSON.stringify({ error: "not_found" }));
  });
  feature.select("node-1");
  await feature.settled();
  btn.click();
  await feature.settled();
  apply.click();
  assert.equal(apply.disabled, true);
  assert.equal(stop.disabled, true);
  apply.click();
  assert.equal(calls.filter((call) => call.method === "PUT").length, 1);
  gate.resolve(viewOf({ members: [] }));
  await feature.settled();
  assert.equal(calls.filter((call) => call.method === "PUT").length, 1);
});

test("a missing existing member disables save rather than dropping the grant", async () => {
  const ui = setup(call => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue, red]);
    return viewOf({ members: [{seat:blue.id,access:"read"},{seat:"seat-missing",access:"write"},{seat:red.id,access:"other"}] });
  });
  ui.feature.select("node-1"); await ui.feature.settled(); ui.btn.click(); await ui.feature.settled();
  assert.equal(ui.apply.disabled, true);
  assert.match(peopleOf(ui.body).textContent, /left the server/);
  ui.apply.click(); assert.equal(ui.calls.some(call => call.method === "PUT"), false);
});

test("save people stays disabled until a choice differs and again when it is put back", async () => {
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    return viewOf();
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  const row = selects(ui.body)[0];
  assert.equal(ui.apply.disabled, true);
  ui.apply.click();
  assert.equal(ui.calls.some((call) => call.method === "PUT"), false);
  row.checked = false;
  row.emit("change");
  assert.equal(ui.apply.disabled, false);
  row.checked = true;
  row.emit("change");
  assert.equal(ui.apply.disabled, true);
  ui.apply.click();
  assert.equal(ui.calls.some((call) => call.method === "PUT"), false);
});

test("an active share whose scope is the whole history is shown as text", async () => {
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    return viewOf({ scope: "all", members: null });
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  assert.match(ui.body.innerHTML, /Entire history/);
  assert.equal(selects(ui.body)[0].checked, false);
  assert.equal(ui.apply.disabled, true);
});

test("stop arms, a click elsewhere disarms, and the second click deletes", async () => {
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    if (call.method === "DELETE") return "";
    return viewOf();
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  ui.stop.click();
  assert.equal(ui.stop.textContent, "Confirm stop");
  assert.equal(ui.calls.some((call) => call.method === "DELETE"), false);
  ui.body.click();
  assert.equal(ui.stop.textContent, "Stop sharing");
  ui.stop.click();
  ui.body.click();
  assert.equal(ui.calls.some((call) => call.method === "DELETE"), false);
  ui.stop.click();
  ui.stop.click();
  await ui.feature.settled();
  assert.equal(ui.calls.filter((call) => call.method === "DELETE").length, 1);
  assert.equal(ui.calls.find((call) => call.method === "DELETE").path, "/api/team/shares/node-1");
});

test("delete answers 204 and 404 as stopped, 202 as stopping, and a failure as a note", async () => {
  async function start(onDelete) {
    const ui = setup((call) => {
      if (call.path === "/api/team/servers") return { servers: [north] };
      if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
      if (call.method === "DELETE") return onDelete();
      return viewOf();
    });
    ui.feature.select("node-1");
    await ui.feature.settled();
    ui.btn.click();
    await ui.feature.settled();
    ui.stop.click();
    ui.stop.click();
    await ui.feature.settled();
    return ui;
  }
  const gone = await start(() => "");
  assert.equal(gone.live.textContent, "Sharing stopped.");
  assert.deepEqual(gone.closed, ["closed"]);
  assert.equal(gone.status.hidden, true);
  assert.equal(gone.btn.hidden, false);
  const missing = await start(() => rejected(404, JSON.stringify({ error: "not_found" })));
  assert.equal(missing.live.textContent, "Sharing stopped.");
  assert.deepEqual(missing.closed, ["closed"]);
  assert.equal(missing.status.hidden, true);
  const stopping = await start(() => viewOf({ state: "stopping", delivery: "offline" }));
  assert.equal(stopping.closed.length, 0);
  assert.match(stopping.body.textContent, /Stopping sharing…/);
  assert.equal(stopping.apply.hidden, true);
  assert.equal(stopping.stop.hidden, true);
  assert.equal(stopping.status.textContent, "Stopping sharing…");
  assert.notEqual(stopping.live.textContent, "Sharing stopped.");
  const failed = await start(() => rejected(500, JSON.stringify({ error: "store_unavailable" })));
  assert.equal(failed.closed.length, 0);
  assert.equal(failed.note.hidden, false);
  assert.equal(failed.note.textContent, "This computer could not save the change. Try again.");
  assert.equal(failed.stop.textContent, "Confirm stop");
  assert.equal(failed.status.textContent, "Shared with 1 · North");
  assert.equal(failed.body.innerHTML.includes("North"), true);
});

test("a second click while stopping sends nothing", async () => {
  const gate = deferred();
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([]);
    if (call.method === "DELETE") return gate.promise;
    return viewOf();
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  ui.stop.click();
  ui.stop.click();
  assert.equal(ui.stop.disabled, true);
  assert.equal(ui.apply.disabled, true);
  ui.stop.click();
  assert.equal(ui.calls.filter((call) => call.method === "DELETE").length, 1);
  gate.resolve("");
  await ui.feature.settled();
});

test("lost and full offer stop, and stopping offers nothing", async () => {
  for (const state of ["lost", "full", "stopping"]) {
    const ui = setup((call) => {
      if (call.path === "/api/team/servers") return { servers: [north] };
      if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
      return viewOf({ state, delivery: "ok", members: members(1) });
    });
    ui.feature.select("node-1");
    await ui.feature.settled();
    const before = ui.calls.length;
    ui.btn.click();
    await ui.feature.settled();
    assert.equal(ui.calls.length, before, state);
    assert.equal(ui.body.querySelectorAll("select").length, 0, state);
    if (state === "stopping") {
      assert.match(ui.body.textContent, /Stopping sharing…/);
      assert.equal(ui.apply.hidden, true);
      assert.equal(ui.stop.hidden, true);
    } else {
      assert.match(ui.body.textContent, new RegExp(shareStatusText(viewOf({ state }), "North").replace(/[.]/g, "\\.")));
      assert.match(ui.body.innerHTML, /Members keep what they already received\./);
      assert.equal(ui.apply.hidden, true);
      assert.equal(ui.stop.hidden, false);
      assert.equal(ui.stop.textContent, "Stop sharing");
      ui.apply.click();
      assert.equal(ui.calls.some((call) => call.method === "PUT"), false);
    }
  }
});

test("focus returns to share, to save, and to the armed stop control", async () => {
  function watch(el) {
    const orig = el.focus.bind(el);
    let n = 0;
    el.focus = () => {
      n += 1;
      orig();
    };
    return () => n;
  }
  const shared = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    if (call.method === "PUT") return viewOf({ members: call.body.members });
    return rejected(404, JSON.stringify({ error: "not_found" }));
  });
  shared.feature.select("node-1");
  await shared.feature.settled();
  shared.btn.click();
  await shared.feature.settled();
  const shareFocus = watch(shared.apply);
  shared.apply.focus();
  const shareBefore = shareFocus();
  shared.apply.click();
  await shared.feature.settled();
  assert.equal(shared.live.textContent, "Sharing started.");
  assert.equal(shared.apply.textContent, "Save people");
  assert.equal(shared.doc.activeElement, shared.apply);
  assert.equal(shareFocus() > shareBefore, true);

  const saving = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    if (call.method === "PUT") return viewOf({ members: call.body.members });
    return viewOf();
  });
  saving.feature.select("node-1");
  await saving.feature.settled();
  saving.btn.click();
  await saving.feature.settled();
  const row = selects(saving.body)[0];
  row.checked = false;
  row.emit("change");
  const saveFocus = watch(saving.apply);
  saving.apply.focus();
  const saveBefore = saveFocus();
  saving.apply.click();
  await saving.feature.settled();
  assert.equal(saving.live.textContent, "People updated.");
  assert.equal(saving.apply.textContent, "Save people");
  assert.equal(saving.doc.activeElement, saving.apply);
  assert.equal(saveFocus() > saveBefore, true);

  const stopping = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    return viewOf();
  });
  stopping.feature.select("node-1");
  await stopping.feature.settled();
  stopping.btn.click();
  await stopping.feature.settled();
  const stopFocus = watch(stopping.stop);
  stopping.stop.focus();
  const stopBefore = stopFocus();
  stopping.stop.click();
  assert.equal(stopping.stop.textContent, "Confirm stop");
  assert.equal(stopping.doc.activeElement, stopping.stop);
  assert.equal(stopFocus() > stopBefore, true);
  assert.equal(stopping.calls.some((call) => call.method === "DELETE"), false);
});

test("selecting another node closes the sheet and selecting the same node leaves it", async () => {
  const gates = [];
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) {
      const gate = deferred();
      gates.push(gate);
      return gate.promise;
    }
    return viewOf();
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await until(() => gates.length === 1);
  ui.feature.select("node-1");
  await Promise.resolve();
  assert.equal(ui.closed.length, 0);
  ui.feature.select("node-2");
  assert.deepEqual(ui.closed, ["closed"]);
  gates[0].resolve(roster([selfSeat(), { id: "seat-late", name: "Late" }]));
  await ui.feature.settled();
  assert.equal(ui.body.textContent.includes("Late"), false);
});

test("the status button opens the same sheet", async () => {
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    return viewOf();
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.status.click();
  await ui.feature.settled();
  assert.deepEqual(ui.opened, ["#sharesheet"]);
  assert.equal(ui.apply.textContent, "Save people");
});

test("destroy removes every listener", async () => {
  const ui = mount();
  let added = 0;
  let removed = 0;
  for (const el of [ui.btn, ui.status, ui.sheet]) {
    const add = el.addEventListener.bind(el);
    const rem = el.removeEventListener.bind(el);
    el.addEventListener = (type, fn) => {
      added += 1;
      add(type, fn);
    };
    el.removeEventListener = (type, fn) => {
      removed += 1;
      rem(type, fn);
    };
  }
  const opened = [];
  const feature = createShareFeature({
    api: async () => ({ servers: [] }),
    doc: ui.doc,
    openSheet: (id) => opened.push(id),
    closeSheets: () => {},
    now: () => 0,
  });
  feature.bind();
  assert.equal(added, 4);
  feature.destroy();
  assert.equal(removed, added);
  ui.btn.click();
  ui.status.click();
  ui.sheet.click();
  assert.deepEqual(opened, []);
});

test("share controls keep a 44px target and hidden still hides them", () => {
  const chat = fs.readFileSync(new URL("../css/chat.css", import.meta.url), "utf8");
  const sheets = fs.readFileSync(new URL("../css/sheets.css", import.meta.url), "utf8");
  const chatShare = chat.slice(chat.indexOf("#sharebtn"));
  const sheetShare = sheets.slice(sheets.indexOf("#sharesheet"));
  const shareRule = chatShare.match(/^([^{}]+)\{([^{}]*)\}/);
  assert.ok(shareRule, "share control rule exists");
  assert.ok(shareRule[1].split(",").map(selector => selector.trim()).includes("#sharebtn.headtoggle"));
  for (const property of ["width", "height", "min-height"]) {
    assert.match(shareRule[2], new RegExp(`(?:^|;)\\s*${property}\\s*:\\s*44px\\s*(?:;|$)`));
  }
  assert.match(chat, /#chathead\s+\.headtoggle\[hidden\]\s*\{[^}]*display:\s*none/);
  assert.match(chatShare, /#sharestatus\s*\{[^}]*min-height:\s*44px/);
  assert.match(chatShare, /#sharestatus\[hidden\]\s*\{[^}]*display:\s*none/);
  assert.match(sheetShare, /#sharesheet label\s*\{[^}]*min-height:\s*44px/);
  assert.match(sheetShare, /#sharesheet input\[type="radio"\],\s*#sharesheet input\[type="checkbox"\]\s*\{[^}]*width:\s*auto/);
  assert.match(sheetShare, /#sharesheet #sh_apply,\s*#sharesheet #sh_stop\s*\{[^}]*min-height:\s*44px/);
  assert.doesNotMatch(chatShare, /@import|url\(/);
  assert.doesNotMatch(sheetShare, /@import|url\(/);
});

const CHANGED = "Sharing of this chat changed. Check and try again.";

test("a save after the share ended elsewhere sends no put and shows the unshared sheet", async () => {
  let shared = true;
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [south, north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    if (call.method === "PUT") return viewOf({ server: call.body.server, members: call.body.members });
    if (shared) return viewOf({ server: north.id });
    return rejected(404, JSON.stringify({ error: "not_found" }));
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  const row = selects(ui.body)[0];
  row.checked = false;
  row.emit("change");
  assert.equal(ui.apply.textContent, "Save people");
  assert.match(ui.body.innerHTML, />North</);
  shared = false;
  ui.time.now = 10000;
  ui.feature.tick();
  await ui.feature.settled();
  assert.equal(ui.apply.textContent, "Save people");
  assert.match(ui.body.innerHTML, />North</);
  assert.equal(ui.note.textContent, "");
  ui.apply.click();
  await ui.feature.settled();
  assert.equal(ui.calls.some((call) => call.method === "PUT"), false);
  assert.equal(ui.note.hidden, false);
  assert.equal(ui.note.textContent, CHANGED);
  assert.equal(ui.apply.textContent, "Share chat");
  assert.equal(ui.apply.hidden, false);
  const servers = inputs(ui.body, "sh_server");
  assert.equal(servers[0].value, south.id);
  assert.equal(servers[0].checked, true);
  assert.equal(servers[1].value, north.id);
});

test("share chat sends no put when a tick already found the share", async () => {
  let shared = false;
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    if (call.method === "PUT") return viewOf({ members: call.body.members });
    if (!shared) return rejected(404, JSON.stringify({ error: "not_found" }));
    return viewOf();
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  assert.equal(ui.apply.textContent, "Share chat");
  shared = true;
  ui.time.now = 10000;
  ui.feature.tick();
  await ui.feature.settled();
  assert.equal(ui.apply.textContent, "Share chat");
  assert.equal(inputs(ui.body, "sh_server").length, 1);
  ui.apply.click();
  await ui.feature.settled();
  assert.equal(ui.calls.some((call) => call.method === "PUT"), false);
  assert.equal(ui.note.textContent, CHANGED);
  assert.equal(ui.apply.textContent, "Save people");
  assert.equal(inputs(ui.body, "sh_server").length, 0);
  assert.match(ui.body.innerHTML, />North</);
});

test("save people sends no put when a tick finds the share lost", async () => {
  let state = "active";
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    if (call.method === "PUT") return viewOf({ members: call.body.members });
    return viewOf({ state });
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  const row = selects(ui.body)[0];
  row.checked = false;
  row.emit("change");
  state = "lost";
  ui.time.now = 10000;
  ui.feature.tick();
  await ui.feature.settled();
  assert.equal(ui.apply.textContent, "Save people");
  assert.equal(ui.apply.disabled, false);
  ui.apply.click();
  await ui.feature.settled();
  assert.equal(ui.calls.some((call) => call.method === "PUT"), false);
  assert.equal(ui.note.textContent, CHANGED);
  assert.match(ui.body.textContent, /Sharing ended on North/);
  assert.equal(ui.apply.hidden, true);
  assert.equal(ui.stop.hidden, false);
  assert.equal(ui.stop.textContent, "Stop sharing");
});

test("the confirming stop sends no delete until the sheet is drawn again", async () => {
  let state = "active";
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    if (call.method === "DELETE") return "";
    return viewOf({ state });
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  state = "lost";
  ui.time.now = 10000;
  ui.feature.tick();
  await ui.feature.settled();
  assert.equal(ui.stop.textContent, "Stop sharing");
  ui.stop.click();
  assert.equal(ui.stop.textContent, "Confirm stop");
  assert.equal(ui.note.textContent, "");
  assert.equal(ui.calls.some((call) => call.method === "DELETE"), false);
  ui.stop.click();
  await ui.feature.settled();
  assert.equal(ui.calls.filter((call) => call.method === "DELETE").length, 0);
  assert.equal(ui.note.textContent, CHANGED);
  assert.equal(ui.stop.textContent, "Stop sharing");
  assert.match(ui.body.textContent, /Sharing ended on North/);
  ui.stop.click();
  ui.stop.click();
  await ui.feature.settled();
  assert.equal(ui.calls.filter((call) => call.method === "DELETE").length, 1);
  assert.equal(ui.live.textContent, "Sharing stopped.");
});

test("share chat sends no put when the chosen server has left", async () => {
  let servers = [south, north];
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    if (call.method === "PUT") return viewOf({ server: call.body.server });
    return rejected(404, JSON.stringify({ error: "not_found" }));
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  assert.equal(inputs(ui.body, "sh_server")[0].value, south.id);
  assert.equal(inputs(ui.body, "sh_server")[0].checked, true);
  servers = [north];
  ui.time.now = 10000;
  ui.feature.tick();
  await ui.feature.settled();
  assert.equal(ui.apply.textContent, "Share chat");
  assert.equal(inputs(ui.body, "sh_server")[0].value, south.id);
  ui.apply.click();
  await ui.feature.settled();
  assert.equal(ui.calls.some((call) => call.method === "PUT"), false);
  assert.equal(ui.note.textContent, CHANGED);
  const again = inputs(ui.body, "sh_server");
  assert.equal(again.length, 1);
  assert.equal(again[0].value, north.id);
  assert.equal(again[0].checked, true);
});

test("share chat with no joined server closes the sheet and a later share works", async () => {
  let servers = [north];
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    if (call.method === "PUT") return viewOf({ members: call.body.members });
    return rejected(404, JSON.stringify({ error: "not_found" }));
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  servers = [];
  ui.time.now = 10000;
  ui.feature.tick();
  await ui.feature.settled();
  assert.equal(ui.apply.textContent, "Share chat");
  assert.equal(ui.closed.length, 0);
  assert.doesNotThrow(() => ui.apply.click());
  await ui.feature.settled();
  assert.equal(ui.calls.some((call) => call.method === "PUT"), false);
  assert.deepEqual(ui.closed, ["closed"]);
  servers = [north];
  ui.time.now = 20000;
  ui.feature.tick();
  await ui.feature.settled();
  assert.equal(ui.btn.hidden, false);
  ui.btn.click();
  await ui.feature.settled();
  ui.apply.click();
  await ui.feature.settled();
  assert.equal(ui.calls.filter((call) => call.method === "PUT").length, 1);
  assert.equal(ui.apply.textContent, "Save people");
  assert.equal(ui.live.textContent, "Sharing started.");
});

test("a click on a sheet drawn for another node sends no put", async () => {
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    if (call.method === "PUT") return viewOf({ members: call.body.members });
    return rejected(404, JSON.stringify({ error: "not_found" }));
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  ui.feature.select("node-2");
  await ui.feature.settled();
  assert.deepEqual(ui.closed, ["closed"]);
  assert.equal(ui.apply.textContent, "Share chat");
  ui.apply.click();
  await ui.feature.settled();
  assert.equal(ui.calls.some((call) => call.method === "PUT"), false);
  assert.equal(ui.note.textContent, CHANGED);
  assert.equal(ui.apply.textContent, "Share chat");
});

test("a put answer for the previous chat is not painted on the next one", async () => {
  const gate = deferred();
  let reads = 0;
  const real = viewOf({ delivery: "offline", members: members(3) });
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    if (call.method === "PUT") return gate.promise;
    if (call.path === "/api/team/shares/node-2") return rejected(500, JSON.stringify({ error: "store_unavailable" }));
    if (call.path === "/api/team/shares/node-1") {
      reads += 1;
      if (reads === 1) return rejected(404, JSON.stringify({ error: "not_found" }));
      return real;
    }
    return rejected(404, JSON.stringify({ error: "not_found" }));
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  ui.apply.click();
  ui.feature.select("node-2");
  const seats = ui.calls.filter((call) => call.path.endsWith("/seats")).length;
  const html = ui.body.innerHTML;
  gate.resolve(viewOf());
  await ui.feature.settled();
  assert.equal(ui.calls.some((call) => call.path === "/api/team/shares/node-2"), true);
  assert.equal(ui.status.hidden, true);
  assert.equal(ui.btn.hidden, true);
  assert.equal(ui.status.textContent, "");
  assert.equal(ui.live.textContent, "");
  assert.equal(ui.body.innerHTML, html);
  assert.equal(ui.calls.filter((call) => call.path.endsWith("/seats")).length, seats);
  assert.equal(ui.calls.filter((call) => call.method === "PUT").length, 1);
  ui.feature.select("node-1");
  await ui.feature.settled();
  assert.equal(ui.btn.hidden, false);
  assert.equal(ui.status.textContent, shareStatusText(real, "North"));
});

test("a delete answer of 204 for the previous chat closes nothing on the next one", async () => {
  const gate = deferred();
  let reads = 0;
  const real = viewOf({ delivery: "refused" });
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    if (call.method === "DELETE") return gate.promise;
    if (call.path === "/api/team/shares/node-2") return rejected(500, JSON.stringify({ error: "store_unavailable" }));
    if (call.path === "/api/team/shares/node-1") {
      reads += 1;
      if (reads === 1) return viewOf();
      return real;
    }
    return viewOf();
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  ui.stop.click();
  ui.stop.click();
  ui.feature.select("node-2");
  const closed = ui.closed.length;
  const html = ui.body.innerHTML;
  gate.resolve("");
  await ui.feature.settled();
  assert.equal(ui.calls.some((call) => call.path === "/api/team/shares/node-2"), true);
  assert.equal(ui.closed.length, closed);
  assert.equal(ui.live.textContent, "");
  assert.equal(ui.status.hidden, true);
  assert.equal(ui.btn.hidden, true);
  assert.equal(ui.body.innerHTML, html);
  ui.feature.select("node-1");
  await ui.feature.settled();
  assert.equal(ui.btn.hidden, false);
  assert.equal(ui.status.textContent, shareStatusText(real, "North"));
});

test("a delete answer of 202 for the previous chat is not painted on the next one", async () => {
  const gate = deferred();
  let reads = 0;
  const real = viewOf({ state: "lost" });
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    if (call.method === "DELETE") return gate.promise;
    if (call.path === "/api/team/shares/node-2") return rejected(500, JSON.stringify({ error: "store_unavailable" }));
    if (call.path === "/api/team/shares/node-1") {
      reads += 1;
      if (reads === 1) return viewOf();
      return real;
    }
    return viewOf();
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  ui.stop.click();
  ui.stop.click();
  ui.feature.select("node-2");
  const closed = ui.closed.length;
  const seats = ui.calls.filter((call) => call.path.endsWith("/seats")).length;
  const html = ui.body.innerHTML;
  gate.resolve(viewOf({ state: "stopping" }));
  await ui.feature.settled();
  assert.equal(ui.calls.some((call) => call.path === "/api/team/shares/node-2"), true);
  assert.equal(ui.closed.length, closed);
  assert.equal(ui.status.hidden, true);
  assert.equal(ui.btn.hidden, true);
  assert.notEqual(ui.status.textContent, shareStatusText(viewOf({ state: "stopping" }), "North"));
  assert.equal(ui.body.innerHTML, html);
  assert.equal(ui.calls.filter((call) => call.path.endsWith("/seats")).length, seats);
  ui.feature.select("node-1");
  await ui.feature.settled();
  assert.equal(ui.btn.hidden, false);
  assert.equal(ui.status.textContent, shareStatusText(real, "North"));
});

test("a late roster after stopping changes nothing and settled resolves", async () => {
  const seatsGate = deferred();
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return seatsGate.promise;
    if (call.method === "DELETE") return viewOf({ state: "stopping" });
    return viewOf();
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await until(() => ui.calls.some((call) => call.path.endsWith("/seats")));
  ui.stop.click();
  ui.stop.click();
  await until(() => ui.body.textContent.includes("Stopping sharing"));
  assert.equal(ui.body.querySelector("#sh_people"), null);
  seatsGate.resolve(roster([selfSeat(), blue]));
  await assert.doesNotReject(() => ui.feature.settled());
  assert.match(ui.body.textContent, /Stopping sharing…/);
  assert.equal(ui.body.querySelector("#sh_people"), null);
});

test("save people sends no put when the share was started again", async () => {
  let again = false;
  const first = viewOf({ server: north.id, scope: "from_now" });
  const next = viewOf({ server: south.id, scope: "all", shared_at: "2026-11-14T11:00:00Z" });
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north, south] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    if (call.method === "PUT") return first;
    return again ? next : first;
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  const row = selects(ui.body)[0];
  row.checked = false;
  row.emit("change");
  assert.equal(ui.apply.disabled, false);
  assert.match(ui.body.innerHTML, />North</);
  assert.match(ui.body.innerHTML, /From now on/);
  again = true;
  ui.time.now = 10000;
  ui.feature.tick();
  await ui.feature.settled();
  assert.match(ui.body.innerHTML, />North</);
  assert.match(ui.body.innerHTML, /From now on/);
  assert.equal(ui.note.textContent, "");
  ui.apply.click();
  await ui.feature.settled();
  assert.equal(ui.calls.some((call) => call.method === "PUT"), false);
  assert.equal(ui.note.hidden, false);
  assert.equal(ui.note.textContent, CHANGED);
  assert.match(ui.body.innerHTML, />South</);
  assert.match(ui.body.innerHTML, /Entire history/);
  assert.doesNotMatch(ui.body.innerHTML, /From now on/);
});

test("save people sends no put when only the scope differs", async () => {
  let scope = "from_now";
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    if (call.method === "PUT") return viewOf({ scope: "from_now" });
    return viewOf({ scope });
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  const row = selects(ui.body)[0];
  row.checked = false;
  row.emit("change");
  scope = "all";
  ui.time.now = 10000;
  ui.feature.tick();
  await ui.feature.settled();
  assert.match(ui.body.innerHTML, /From now on/);
  ui.apply.click();
  await ui.feature.settled();
  assert.equal(ui.calls.some((call) => call.method === "PUT"), false);
  assert.equal(ui.note.textContent, CHANGED);
  assert.match(ui.body.innerHTML, />North</);
  assert.match(ui.body.innerHTML, /Entire history/);
  assert.doesNotMatch(ui.body.innerHTML, /From now on/);
});

test("save people sends no put when only the shared time differs", async () => {
  let at = "2026-11-14T09:30:00Z";
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north, south] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    if (call.method === "PUT") return viewOf({ server: south.id });
    return viewOf({ shared_at: at });
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  const row = selects(ui.body)[0];
  row.checked = false;
  row.emit("change");
  at = "2026-11-14T11:00:00Z";
  ui.time.now = 10000;
  ui.feature.tick();
  await ui.feature.settled();
  assert.equal(row.checked, false);
  ui.apply.click();
  await ui.feature.settled();
  assert.equal(ui.calls.some((call) => call.method === "PUT"), false);
  assert.equal(ui.note.textContent, CHANGED);
  assert.match(ui.body.innerHTML, />North</);
  assert.match(ui.body.innerHTML, /From now on/);
  assert.doesNotMatch(ui.body.innerHTML, />South</);
  assert.equal(selects(ui.body)[0].checked, true);
});

test("save people detects concurrent membership changes", async () => {
  let changed = false;
  const seen = [];
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    if (call.method === "PUT") {
      seen.push(call.body);
      return viewOf({ members: call.body.members, delivery: "offline" });
    }
    if (!changed) return viewOf({ delivery: "ok" });
    return viewOf({
      delivery: "offline",
      members: [{ seat: blue.id, access: "write" }],
    });
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  const row = selects(ui.body)[0];
  row.checked = false;
  row.emit("change");
  changed = true;
  ui.time.now = 10000;
  ui.feature.tick();
  await ui.feature.settled();
  assert.equal(ui.apply.disabled, false);
  assert.match(ui.body.innerHTML, /From now on/);
  ui.apply.click();
  await ui.feature.settled();
  assert.equal(seen.length, 0);
  assert.equal(ui.note.textContent, CHANGED);
});

test("phase 1: unavailable sharing retries on cadence and selection can recover", async () => {
  let online = false;
  const ui = setup(call => {
    if (call.path === "/api/team/servers") return online ? { servers: [north] } : rejected(404, "off");
    return viewOf();
  });
  ui.feature.select("node-1"); await ui.feature.settled();
  online = true;
  ui.time.now = 9999; ui.feature.tick(); await ui.feature.settled();
  assert.equal(ui.calls.length, 1);
  ui.time.now = 10000; ui.feature.tick(); await ui.feature.settled();
  assert.equal(ui.btn.hidden, false);
  assert.equal(ui.status.textContent, "Shared with 1 · North");
});

test("phase 1: membership checkboxes preserve read and add write", async () => {
  const ui = setup(call => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue, red]);
    if (call.method === "PUT") return viewOf({ members: call.body.members });
    return viewOf();
  });
  ui.feature.select("node-1"); await ui.feature.settled(); ui.btn.click(); await ui.feature.settled();
  const choices = ui.body.querySelectorAll("input").filter(x => x.type === "checkbox");
  assert.equal(ui.body.querySelectorAll("select").length, 0);
  assert.deepEqual(choices.map(x => x.checked), [true, false]);
  choices[1].checked = true; choices[1].emit("change");
  assert.equal(ui.apply.disabled, false); ui.apply.click(); await ui.feature.settled();
  assert.deepEqual(ui.calls.find(c => c.method === "PUT").body.members, [{seat:blue.id,access:"read"},{seat:red.id,access:"write"}]);
});

test("phase 1: failed roster cannot submit a destructive membership update", async () => {
  const ui = setup(call => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path.endsWith("/seats")) return rejected(502, "offline");
    return viewOf();
  });
  ui.feature.select("node-1"); await ui.feature.settled(); ui.btn.click(); await ui.feature.settled();
  assert.equal(ui.apply.disabled, true); ui.apply.click();
  assert.equal(ui.calls.some(c => c.method === "PUT"), false);
});

test("phase 1: malformed access is unchecked, removal and revert preserve Apply state", async () => {
  const ui = setup(call => {
    if (call.path === "/api/team/servers") return {servers:[north]};
    if (call.path.endsWith("/seats")) return roster([selfSeat(),blue,red]);
    if (call.method === "PUT") return viewOf({members:call.body.members});
    return viewOf({members:[{seat:blue.id,access:"read"},{seat:red.id,access:"invalid"},null]});
  });
  ui.feature.select("node-1"); await ui.feature.settled(); ui.btn.click(); await ui.feature.settled();
  const rows = selects(ui.body);
  assert.deepEqual(rows.map(row => row.checked), [true,false]);
  assert.equal(ui.apply.disabled,true);
  rows[0].checked = false; rows[0].emit("change"); assert.equal(ui.apply.disabled,false);
  rows[0].checked = true; rows[0].emit("change"); assert.equal(ui.apply.disabled,true);
  rows[0].checked = false; rows[0].emit("change"); ui.apply.click(); await ui.feature.settled();
  assert.deepEqual(ui.calls.find(c => c.method === "PUT").body.members, []);
});

test("phase 1: a new share cannot submit while its roster fails or is malformed", async () => {
  for (const answer of [null, {self:selfId,seats:[null]}, {self:selfId,seats:[{id:"",name:"bad"}]}, {self:selfId,seats:[{id:"bad"}]}]) {
    const ui = setup(call => {
      if (call.path === "/api/team/servers") return {servers:[north]};
      if (call.path.endsWith("/seats")) return answer;
      return rejected(404,"not found");
    });
    ui.feature.select("node-1"); await ui.feature.settled(); ui.btn.click(); await ui.feature.settled();
    assert.equal(ui.apply.disabled,true); ui.apply.click(); assert.equal(ui.calls.some(c => c.method === "PUT"),false);
  }
});

test("phase 1: clearing selection cancels a pending header read", async () => {
  const gate = deferred(); const ui = setup(() => gate.promise);
  ui.feature.select("node-1"); ui.feature.select(""); gate.resolve({servers:[north]});
  await ui.feature.settled(); ui.time.now = 20000; ui.feature.tick(); await ui.feature.settled();
  assert.equal(ui.calls.length,1); assert.equal(ui.btn.hidden,true);
});

test("phase 1: null stored members still allow stopping without a false drift", async () => {
  const ui = setup(call => {
    if (call.path === "/api/team/servers") return {servers:[north]};
    if (call.path.endsWith("/seats")) return roster([]);
    if (call.method === "DELETE") return "";
    return viewOf({members:null});
  });
  ui.feature.select("node-1"); await ui.feature.settled(); ui.btn.click(); await ui.feature.settled();
  ui.stop.click(); ui.stop.click(); await ui.feature.settled();
  assert.equal(ui.calls.some(c=>c.method === "DELETE"),true);
});

test("retired sharing feature cannot repaint the successor after a delayed header read", async () => {
  const gate = deferred();
  const ui = setup(call => call.path === "/api/team/servers" ? gate.promise : viewOf());
  ui.feature.select("node-1"); ui.feature.destroy();
  ui.btn.hidden = true; ui.status.hidden = false; ui.status.textContent = "successor";
  gate.resolve({servers:[north]}); await ui.feature.settled();
  assert.equal(ui.btn.hidden,true); assert.equal(ui.status.textContent,"successor");
  assert.equal(ui.calls.length,1);
  ui.btn.innerHTML = "successor icon";
  ui.feature.select("node-2"); ui.feature.ensureSelected("node-3"); ui.time.now = 20000; ui.feature.tick(); ui.feature.bind(); ui.feature.destroy();
  assert.equal(ui.calls.length,1); assert.equal(ui.btn.hidden,true); assert.equal(ui.btn.innerHTML,"successor icon");
});

test("retirement prevents late share and roster reads from altering the successor", async () => {
  for (const late of ["share", "roster"]) {
    const gate = deferred();
    const ui = setup(call => {
      if (call.path === "/api/team/servers") return {servers:[north]};
      if (call.path.endsWith("/seats")) return late === "roster" ? gate.promise : roster([selfSeat(),blue]);
      return late === "share" ? gate.promise : viewOf();
    });
    ui.feature.select("node-1");
    if (late === "share") await until(() => ui.calls.some(c=>c.path === "/api/team/shares/node-1"));
    else { await ui.feature.settled(); ui.btn.click(); }
    ui.feature.destroy(); ui.btn.hidden = true; ui.status.textContent = "successor";
    if (late === "roster") peopleOf(ui.body).textContent = "successor people";
    gate.resolve(late === "share" ? viewOf() : roster([selfSeat(),blue]));
    await ui.feature.settled();
    assert.equal(ui.btn.hidden,true); assert.equal(ui.status.textContent,"successor");
    if (late === "roster") assert.equal(peopleOf(ui.body).textContent,"successor people");
  }
});

test("retirement prevents late share mutations from changing successor controls", async () => {
  for (const method of ["PUT","DELETE"]) for (const fail of [false,true]) {
    const gate = deferred();
    const ui = setup(call => {
      if (call.path === "/api/team/servers") return {servers:[north]};
      if (call.path.endsWith("/seats")) return roster([selfSeat(),blue]);
      if (call.method === method) return gate.promise;
      return viewOf();
    });
    ui.feature.select("node-1"); await ui.feature.settled(); ui.btn.click(); await ui.feature.settled();
    if (method === "PUT") { const row=selects(ui.body)[0]; row.checked=false; row.emit("change"); ui.apply.click(); }
    else { ui.stop.click(); ui.stop.click(); }
    ui.feature.destroy();
    ui.status.textContent="successor"; ui.note.textContent="successor note"; ui.live.textContent="successor live";
    ui.apply.disabled=false; ui.stop.disabled=false; const calls=ui.calls.length;
    if (fail) gate.reject(Object.assign(new Error("offline"),{status:502}));
    else gate.resolve(method === "PUT" ? viewOf() : "");
    await ui.feature.settled();
    assert.equal(ui.status.textContent,"successor"); assert.equal(ui.note.textContent,"successor note");
    assert.equal(ui.live.textContent,"successor live"); assert.equal(ui.apply.disabled,false); assert.equal(ui.stop.disabled,false);
    assert.equal(ui.calls.length,calls); assert.equal(ui.closed.length,0);
  }
});

test('reselect retains sharing controls while the real share refresh is pending', async () => {
 let hold=false, release; const delayed=new Promise(resolve=>{release=resolve});
 const ui=setup(call=>{
  if(call.path==='/api/team/servers') return {servers:[north]};
  if(hold) return delayed;
  return viewOf();
 });
 ui.feature.select('node-1'); await ui.feature.settled();
 assert.equal(ui.btn.hidden,false); assert.equal(ui.status.hidden,false);
 hold=true; ui.feature.select('node-1');
 assert.equal(ui.btn.hidden,false); assert.equal(ui.status.hidden,false);
 release(viewOf()); await ui.feature.settled();
 assert.equal(ui.status.textContent,'Shared with 1 · North');
});

test("membership-only save carries the viewed share identity and generation", async () => {
  const ui = setup(call => {
    if (call.path === "/api/team/servers") return {servers:[north]};
    if (call.path.endsWith("/seats")) return roster([selfSeat(),blue,red]);
    if (call.method === "PUT") return viewOf({generation:"generation-1",members:call.body.members});
    return viewOf({generation:"generation-1",members:[{seat:blue.id,access:"write"}]});
  });
  ui.feature.select("node-1"); await ui.feature.settled();ui.btn.click();await ui.feature.settled();
  const choices=ui.body.querySelectorAll("input").filter(x=>x.type==="checkbox");
  choices[1].checked=true;choices[1].emit("change");ui.apply.click();await ui.feature.settled();
  const sent=ui.calls.find(c=>c.method==="PUT").body;
  assert.deepEqual(sent.membership_only,{uid:"share-1",generation:"generation-1",baseline:[blue.id]});
});

test("a stale generation conflict reports review without discarding choices", async () => {
  const ui=setup(call=>{
    if(call.path==="/api/team/servers") return {servers:[north]};
    if(call.path.endsWith("/seats")) return roster([selfSeat(),blue,red]);
    if(call.method==="PUT") return rejected(409, JSON.stringify({error:"share_changed"}));
    return viewOf({generation:"generation-1"});
  });
  ui.feature.select("node-1");await ui.feature.settled();ui.btn.click();await ui.feature.settled();
  const choices=ui.body.querySelectorAll("input").filter(x=>x.type==="checkbox");
  choices[1].checked=true;choices[1].emit("change");ui.apply.click();await ui.feature.settled();
  assert.equal(ui.note.textContent,"Sharing changed. Close this sheet and review it again.");
  assert.equal(choices[1].checked,true);
});

test("stale write snapshot accepts current read returned by membership save", async () => {
  let current=[{seat:blue.id,access:"write"}];
  const puts=[];
  const ui=setup(call=>{
    if(call.path==="/api/team/servers")return {servers:[north]};
    if(call.path.endsWith("/seats"))return roster([selfSeat(),blue,red]);
    if(call.method==="PUT"){
      puts.push(call.body);
      current=call.body.members.map(m=>current.find(x=>x.seat===m.seat)||{seat:m.seat,access:"write"});
      return viewOf({generation:"generation-1",members:current});
    }
    return viewOf({generation:"generation-1",members:current});
  });
  ui.feature.select("node-1");await ui.feature.settled();ui.btn.click();await ui.feature.settled();
  let choices=ui.body.querySelectorAll("input").filter(x=>x.type==="checkbox");
  current=[{seat:blue.id,access:"read"}];
  choices[1].checked=true;choices[1].emit("change");ui.apply.click();await ui.feature.settled();
  assert.deepEqual(puts[0].members,[{seat:blue.id,access:"write"},{seat:red.id,access:"write"}]);
  assert.deepEqual(puts[0].membership_only.baseline,[blue.id]);
  choices=ui.body.querySelectorAll("input").filter(x=>x.type==="checkbox");
  choices[1].checked=false;choices[1].emit("change");ui.apply.click();await ui.feature.settled();
  assert.deepEqual(puts[1].members,[{seat:blue.id,access:"read"}]);
});

test("generation change while a sheet is open requires review before saving", async () => {
 let generation="generation-1";
 const ui=setup(call=>{
  if(call.path==="/api/team/servers")return {servers:[north]};
  if(call.path.endsWith("/seats"))return roster([selfSeat(),blue,red]);
  if(call.method==="PUT")return viewOf({generation,members:call.body.members});
  return viewOf({generation});
 });
 ui.feature.select("node-1");await ui.feature.settled();ui.btn.click();await ui.feature.settled();
 const choices=ui.body.querySelectorAll("input").filter(x=>x.type==="checkbox");
 choices[1].checked=true;choices[1].emit("change");
 generation="generation-2";ui.time.now=10000;ui.feature.tick();await ui.feature.settled();
 ui.apply.click();await ui.feature.settled();
 assert.equal(ui.calls.some(c=>c.method==="PUT"),false);
 assert.equal(ui.note.textContent,CHANGED);
});

function laneOf(over = {}) {
  return {
    lane: "north",
    id: "index-1",
    server: north.id,
    scope: "from_now",
    name: "North",
    color: "#1a2b3c",
    members: [{ seat: blue.id, access: "read" }],
    state: "active",
    delivery: "ok",
    chats: 1,
    excluded: 0,
    shared_at: "2026-11-14T09:30:00Z",
    generation: "generation-1",
    ...over,
  };
}

test("laneCountLine names archived chats only when some are archived", () => {
  assert.equal(
    laneCountLine(2, 1),
    "Shares 2 chats now · 1 archived chats stay private · new chats in this lane are shared automatically",
  );
  assert.equal(
    laneCountLine(2, 0),
    "Shares 2 chats now · new chats in this lane are shared automatically",
  );
});

test("a new lane sheet shares the lane and lowercases its color", async () => {
  const facts = { name: "North", color: "#AABBCC", open: 2, archived: ["arch-1"] };
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path === "/api/team/lane-shares") return { lane_shares: [] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    if (call.method === "PUT") return laneOf({ color: call.body.color, name: call.body.name, members: call.body.members });
    return rejected(404, JSON.stringify({ error: "not_found" }));
  }, { laneFacts: () => facts });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.feature.openLane("north");
  await ui.feature.settled();
  assert.equal(ui.apply.textContent, "Share lane");
  assert.match(ui.body.innerHTML, /Shares 2 chats now · 1 archived chats stay private/);
  assert.equal(ui.stop.hidden, true);
  const row = selects(ui.body)[0];
  row.checked = true;
  row.emit("change");
  ui.apply.click();
  await ui.feature.settled();
  const put = ui.calls.find((call) => call.method === "PUT");
  assert.equal(put.path, "/api/team/lane-shares/north");
  assert.equal(put.body.color, "#aabbcc");
  assert.equal(put.body.name, "North");
  assert.deepEqual(put.body.excluded, ["arch-1"]);
  assert.equal(put.body.membership_only, undefined);
});

test("saving lane people sends membership_only for the index", async () => {
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path === "/api/team/lane-shares") return { lane_shares: [laneOf()] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue, red]);
    if (call.method === "PUT") return laneOf({ members: call.body.members });
    if (call.path === "/api/team/lane-shares/north") return laneOf();
    return viewOf();
  }, { laneFacts: () => ({ name: "North", color: "#1a2b3c", open: 1, archived: [] }) });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.feature.openLane("north");
  await ui.feature.settled();
  assert.match(ui.body.innerHTML, /Shares 1 chats now/);
  assert.doesNotMatch(ui.body.innerHTML, /archived chats stay private/);
  const row = selects(ui.body)[1];
  row.checked = true;
  row.emit("change");
  ui.apply.click();
  await ui.feature.settled();
  const put = ui.calls.find((call) => call.method === "PUT");
  assert.equal(put.body.membership_only.uid, "index-1");
  assert.deepEqual(put.body.membership_only.baseline, [blue.id]);
  assert.equal(put.body.name, "North");
});

test("a lane-owned chat offers the lane sheet and no member controls", async () => {
  const nasty = "<img src=x onerror=1>";
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path === "/api/team/lane-shares") return { lane_shares: [laneOf({ name: nasty })] };
    if (call.path === "/api/team/lane-shares/north") return laneOf({ name: nasty });
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    return viewOf({ lane: "index-1" });
  }, { laneFacts: () => ({ name: nasty, color: "#1a2b3c", open: 0, archived: [] }) });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  assert.match(ui.body.innerHTML, /Shared through lane &lt;img src=x onerror=1&gt;/);
  assert.doesNotMatch(ui.body.innerHTML, /<img/);
  assert.equal(ui.stop.hidden, true);
  assert.equal(ui.apply.hidden, true);
  assert.equal(ui.body.querySelector("#sh_people"), null);
  ui.body.querySelector("#sh_lane").click();
  await ui.feature.settled();
  assert.equal(ui.apply.textContent, "Save people");
  assert.match(ui.body.innerHTML, /Shares 0 chats now/);
});

test("the share tick renames a lane once and skips a put already in flight", async () => {
  let release;
  const held = new Promise((ok) => { release = ok; });
  let puts = 0;
  const shares = [
    laneOf({ lane: "north", name: "Old", color: "#aabbcc" }),
    laneOf({ lane: "south", id: "index-2", name: "Old", color: "#aabbcc" }),
  ];
  const facts = {
    north: { name: "New", color: "#AABBCC", open: 1, archived: ["a"] },
    south: { name: "Also", color: "#00ff00", open: 0, archived: [] },
  };
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path === "/api/team/lane-shares" && call.method === "GET") return { lane_shares: shares };
    if (call.method === "PUT") {
      puts += 1;
      const lane = call.path.split("/").pop();
      const found = shares.find((item) => item.lane === lane);
      if (found) {
        found.name = call.body.name;
        found.color = call.body.color;
      }
      if (puts === 1) return held;
      return laneOf({ lane, name: call.body.name, color: call.body.color, members: call.body.members });
    }
    return viewOf();
  }, { laneFacts: (id) => facts[id] });
  ui.feature.select("node-1");
  await until(() => puts === 1);
  const first = ui.calls.find((call) => call.method === "PUT");
  assert.equal(first.path, "/api/team/lane-shares/north");
  assert.equal(first.body.name, "New");
  assert.equal(first.body.color, "#aabbcc");
  assert.deepEqual(first.body.members, shares[0].members);
  assert.deepEqual(first.body.excluded, ["a"]);
  ui.time.now = 10000;
  ui.feature.tick();
  for (let i = 0; i < 20; i++) await Promise.resolve();
  assert.equal(ui.calls.filter((call) => call.method === "PUT").length, 1);
  release(laneOf({ name: "New", color: "#aabbcc" }));
  await ui.feature.settled();
  ui.time.now = 20000;
  ui.feature.tick();
  await ui.feature.settled();
  const second = ui.calls.filter((call) => call.method === "PUT")[1];
  assert.equal(second.path, "/api/team/lane-shares/south");
  assert.equal(second.body.color, "#00ff00");
});

test("the share tick leaves a lane whose name and color already match", async () => {
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path === "/api/team/lane-shares") return { lane_shares: [laneOf({ name: "North", color: "#aabbcc" })] };
    return viewOf();
  }, { laneFacts: () => ({ name: "North", color: "#AABBCC", open: 1, archived: [] }) });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.time.now = 10000;
  ui.feature.tick();
  await ui.feature.settled();
  assert.equal(ui.calls.some((call) => call.method === "PUT"), false);
  assert.equal(ui.feature.laneShare("north").name, "North");
  assert.equal(ui.feature.serverName(north.id), "North");
  assert.equal(ui.feature.laneShare("missing"), null);
  assert.equal(ui.feature.serverName("gone"), "a server you left");
});

test("a late lane list does not replace a newer one", async () => {
  const first = deferred();
  let laneGets = 0;
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path === "/api/team/lane-shares") {
      laneGets += 1;
      if (laneGets === 1) return first.promise;
      return { lane_shares: [laneOf({ name: "New" })] };
    }
    return viewOf();
  }, { laneFacts: () => ({ name: "North", color: "#aabbcc", open: 0, archived: [] }) });
  ui.feature.select("node-1");
  await until(() => laneGets === 1);
  ui.feature.select("node-2");
  await until(() => ui.feature.laneShare("north") && ui.feature.laneShare("north").name === "New");
  first.resolve({ lane_shares: [laneOf({ name: "Old" })] });
  await ui.feature.settled();
  assert.equal(ui.feature.laneShare("north").name, "New");
});

test("a broken or failed lane list leaves no lane", async () => {
  for (const answer of [{ lane_shares: null }, null]) {
    const ui = setup((call) => {
      if (call.path === "/api/team/servers") return { servers: [north] };
      if (call.path === "/api/team/lane-shares") return answer;
      return viewOf();
    }, { laneFacts: () => ({ name: "North", color: "#aabbcc", open: 0, archived: [] }) });
    ui.feature.select("node-1");
    await ui.feature.settled();
    assert.equal(ui.feature.laneShare("north"), null);
  }
  const failed = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path === "/api/team/lane-shares") return rejected(500, "no");
    return viewOf();
  }, { laneFacts: () => ({ name: "North", color: "#aabbcc", open: 0, archived: [] }) });
  failed.feature.select("node-1");
  await failed.feature.settled();
  assert.equal(failed.feature.laneShare("north"), null);
});

test("the share tick skips a lane that is quiet or gone and renames when only the color differs", async () => {
  const shares = [
    null,
    laneOf({ lane: "stop", state: "stopping", name: "Stop", color: "#aabbcc" }),
    laneOf({ lane: "gone", name: "Gone", color: "#aabbcc" }),
    laneOf({ lane: "north", name: "North", color: "#112233" }),
  ];
  const facts = {
    gone: null,
    north: { name: "North", color: "#AABBCC", open: 0, archived: [] },
  };
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path === "/api/team/lane-shares" && call.method === "GET") return { lane_shares: shares };
    if (call.method === "PUT") return laneOf({ name: call.body.name, color: call.body.color });
    return viewOf();
  }, { laneFacts: (id) => facts[id] });
  ui.feature.select("node-1");
  await ui.feature.settled();
  const put = ui.calls.find((call) => call.method === "PUT");
  assert.equal(put.path, "/api/team/lane-shares/north");
  assert.equal(put.body.color, "#aabbcc");
  assert.equal(put.body.name, "North");
  assert.equal(ui.calls.filter((call) => call.method === "PUT").length, 1);
});

test("the sheet title follows the chat or the lane", async () => {
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path === "/api/team/lane-shares") return { lane_shares: [] };
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    return rejected(404, JSON.stringify({ error: "not_found" }));
  }, { laneFacts: () => ({ name: "North", color: "#AABBCC", open: 0, archived: [] }) });
  const title = slot(ui.doc, "sh_title", "h3");
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  assert.equal(title.textContent, "Share chat");
  ui.feature.openLane("north");
  await ui.feature.settled();
  assert.equal(title.textContent, "Share lane");
});

test("a lane-owned chat names the lane from the list when the page has no local name", async () => {
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path === "/api/team/lane-shares") {
      return { lane_shares: [null, { id: "other", lane: "south", name: "South" }, laneOf({ name: "Listed" })] };
    }
    return viewOf({ lane: "index-1" });
  }, { laneFacts: () => ({ color: "#aabbcc", open: 0, archived: [] }) });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  assert.match(ui.body.innerHTML, /Shared through lane Listed/);
});

test("a lane-owned chat with no lane list shows an empty name", async () => {
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    return viewOf({ lane: "missing" });
  });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.btn.click();
  await ui.feature.settled();
  assert.match(ui.body.innerHTML, /Shared through lane <\/p>/);
  assert.equal(ui.body.querySelector("#sh_people"), null);
});

test("a lane sheet shows a note when the lane cannot be read", async () => {
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path === "/api/team/lane-shares") return { lane_shares: [] };
    if (call.path === "/api/team/lane-shares/north") return rejected(500, JSON.stringify({ error: "store_unavailable" }));
    return viewOf();
  }, { laneFacts: () => ({ name: "North", color: "#aabbcc", open: 0, archived: [] }) });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.feature.openLane("north");
  await ui.feature.settled();
  assert.equal(ui.note.hidden, false);
  assert.equal(ui.note.textContent, "This computer could not save the change. Try again.");
});

test("a slower lane sheet does not replace the lane opened after it", async () => {
  const first = deferred();
  let gets = 0;
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path === "/api/team/lane-shares") return { lane_shares: [] };
    if (call.path === "/api/team/lane-shares/north") { gets += 1; return first.promise; }
    if (call.path === "/api/team/lane-shares/south") return laneOf({ lane: "south", id: "index-2", name: "South" });
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    return viewOf();
  }, { laneFacts: (id) => ({ name: id === "south" ? "South" : "North", color: "#aabbcc", open: 1, archived: [] }) });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.feature.openLane("north");
  await until(() => gets === 1);
  ui.feature.openLane("south");
  first.resolve(laneOf({ name: "North", server: "server-old" }));
  await ui.feature.settled();
  assert.match(ui.body.innerHTML, /<p>North<\/p>/);
  assert.doesNotMatch(ui.body.innerHTML, /a server you left/);
});

test("stopping a lane deletes that lane and a missing lane stays a note", async () => {
  const facts = { name: "North", color: "#1a2b3c", open: 1, archived: [] };
  const ui = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path === "/api/team/lane-shares") return { lane_shares: [laneOf()] };
    if (call.method === "DELETE") return laneOf({ state: "stopping" });
    if (call.path === "/api/team/lane-shares/north") return laneOf();
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    return viewOf();
  }, { laneFacts: () => facts });
  ui.feature.select("node-1");
  await ui.feature.settled();
  ui.feature.openLane("north");
  await ui.feature.settled();
  ui.stop.click();
  ui.stop.click();
  await ui.feature.settled();
  const del = ui.calls.find((call) => call.method === "DELETE");
  assert.equal(del.path, "/api/team/lane-shares/north");
  assert.match(ui.body.innerHTML, /Stopping sharing/);
  assert.equal(ui.status.hidden, false);

  const missed = setup((call) => {
    if (call.path === "/api/team/servers") return { servers: [north] };
    if (call.path === "/api/team/lane-shares") return { lane_shares: [laneOf()] };
    if (call.method === "DELETE") return rejected(404, JSON.stringify({ error: "store_unavailable" }));
    if (call.path === "/api/team/lane-shares/north") return laneOf();
    if (call.path.endsWith("/seats")) return roster([selfSeat(), blue]);
    return viewOf();
  }, { laneFacts: () => facts });
  missed.feature.select("node-1");
  await missed.feature.settled();
  missed.feature.openLane("north");
  await missed.feature.settled();
  missed.stop.click();
  missed.stop.click();
  await missed.feature.settled();
  assert.equal(missed.note.textContent, "This computer could not save the change. Try again.");
  assert.equal(missed.status.hidden, false);
});
