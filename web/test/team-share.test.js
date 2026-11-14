import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { createShareFeature, shareErrorText, shareStatusText } from "../js/team-share.js";

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
  ["store_unavailable", "This computer could not save the change. Try again."],
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

function setup(handler) {
  const calls = [];
  const opened = [];
  const closed = [];
  const time = { now: 0 };
  const ui = mount();
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
    openSheet: (id) => opened.push(id),
    closeSheets: () => closed.push("closed"),
    now: () => time.now,
  });
  feature.bind();
  return { ...ui, calls, opened, closed, time, feature };
}

function peopleOf(body) {
  return body.querySelector("#sh_people");
}

function inputs(body, name) {
  return body.querySelectorAll("input").filter((input) => input.name === name);
}

function selects(body) {
  return body.querySelectorAll("select");
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

test("a gate that is off sends one probe and then nothing", async () => {
  const { feature, calls, btn, status } = setup(() => rejected(404, JSON.stringify({ error: "not_found" })));
  feature.select("node-1");
  await feature.settled();
  assert.deepEqual(calls.map((call) => call.path), ["/api/team/servers"]);
  assert.equal(btn.hidden, true);
  assert.equal(status.hidden, true);
  for (let i = 0; i < 10; i++) feature.select("node-" + (i + 2));
  for (let i = 0; i < 10; i++) feature.tick();
  await feature.settled();
  assert.equal(calls.length, 1);
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
  assert.deepEqual(opened, ["sharesheet"]);
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
  choice.value = "read";
  choice.emit("change");
  await feature.settled();
  assert.equal(calls.some((call) => call.method === "PUT"), false);
  assert.equal(apply.textContent, "Share chat");
  assert.equal(apply.disabled, false);
});

test("the put body covers nobody, read, read and write, entire history, and the second server", async () => {
  const cases = [
    { choose: () => {}, members: [] },
    { choose: (body) => { selects(body)[0].value = "read"; }, members: [{ seat: blue.id, access: "read" }] },
    {
      choose: (body) => {
        selects(body)[0].value = "read";
        selects(body)[1].value = "write";
      },
      members: [{ seat: blue.id, access: "read" }, { seat: red.id, access: "write" }],
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
    if (body && Array.isArray(body.seats) && body.seats.some((seat) => seat.id === blue.id) && body.self == null) {
      assert.equal(selects(sheet).length, 1);
      assert.equal(selects(sheet)[0].dataset.seat, blue.id);
    } else {
      assert.equal(host.textContent, "Nobody else has joined this server yet.");
      assert.equal(selects(sheet).length, 0);
    }
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
  assert.match(peopleOf(body).innerHTML, /aria-label="A &lt;b&gt; &amp; &quot;q&quot;"/);
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

