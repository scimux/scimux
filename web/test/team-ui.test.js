import test from "node:test";
import assert from "node:assert/strict";
import { createSharingSection, teamErrorText, teamStatusText } from "../js/team-ui.js";

const relay = {
  id: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  name: "Lab relay",
  url: "https://relay.example/join",
  seat_name: "Ada",
};
const other = {
  id: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  name: "Other relay",
  url: "https://other.example/join",
  seat_name: "Bea",
};

const STATUS = [
  ["connected", "connected"],
  ["offline", "offline"],
  ["refused", "access ended"],
  ["changed", "not the same server"],
  ["anything-else", "unknown"],
];

const ERRORS = [
  ["invalid_link", "That is not a join link."],
  ["bad_request", "Check the name: 1 to 64 characters."],
  ["unreachable", "The server could not be reached."],
  ["bad_server", "That address is not a scimux team server."],
  ["already_joined", "This computer has already joined that server."],
  ["join_refused", "The server refused the join code. Ask for a new one."],
  ["already_registered", "This computer is still registered on that server. Ask its operator to remove the old seat."],
  ["server_full", "The server has no free seat."],
  ["joined_but_not_saved", "Joined, but this computer could not save it. Ask for a new join code."],
  ["left_but_not_removed", "Left the server, but this computer could not remove the entry. Try again."],
  ["nope", "That did not work. Try again."],
];

let focusedNode = null;

function deferred() {
  let done = false;
  let resolve;
  let reject;
  const promise = new Promise((ok, bad) => {
    resolve = ok;
    reject = bad;
  });
  return {
    promise,
    resolve(value) {
      if (!done) {
        done = true;
        resolve(value);
      }
    },
    reject(err) {
      if (!done) {
        done = true;
        reject(err);
      }
    },
  };
}

function makeEl(tag) {
  const listeners = new Map();
  const classSet = new Set();
  const node = {
    tagName: String(tag || "div").toUpperCase(),
    id: "",
    hidden: false,
    disabled: false,
    value: "",
    textContent: "",
    dataset: {},
    children: [],
    parentElement: null,
    classList: {
      add(name) {
        classSet.add(name);
        node.className = [...classSet].join(" ");
      },
      remove(name) {
        classSet.delete(name);
        node.className = [...classSet].join(" ");
      },
      contains(name) {
        return classSet.has(name);
      },
    },
    addEventListener(type, fn) {
      if (!listeners.has(type)) listeners.set(type, new Set());
      listeners.get(type).add(fn);
    },
    focus() { focusedNode = node; },
    removeEventListener(type, fn) {
      listeners.get(type)?.delete(fn);
    },
    click() {
      const ev = { type: "click", target: node, preventDefault() {}, stopPropagation() {} };
      let current = node;
      while (current) {
        for (const fn of current.listeners?.get?.("click") || listenersOf(current, "click")) fn(ev);
        current = current.parentElement;
      }
    },
  };
  node.listeners = listeners;
  Object.defineProperty(node, "className", {
    get() {
      return [...classSet].join(" ");
    },
    set(value) {
      classSet.clear();
      for (const name of String(value || "").split(/\s+/)) if (name) classSet.add(name);
    },
  });
  Object.defineProperty(node, "innerHTML", {
    get() {
      return node._html || "";
    },
    set(value) {
      node._html = String(value);
      node.children = parseHTML(node._html);
      for (const child of node.children) child.parentElement = node;
      if (!node.children.length) node.textContent = node._html;
    },
  });
  node.querySelector = (sel) => queryAll(node, sel)[0] || null;
  node.querySelectorAll = (sel) => queryAll(node, sel);
  return node;
}

function listenersOf(node, type) {
  return node.listeners?.get(type) || [];
}

function parseHTML(html) {
  const out = [];
  html = html.replace(/<input\b([^>]*)>/g, "<input$1></input>");
  const re = /<([a-zA-Z0-9]+)([^>]*)>([\s\S]*?)<\/\1>/g;
  let match;
  while ((match = re.exec(html))) out.push(buildEl(match[1], match[2], match[3]));
  return out;
}

function buildEl(tag, attrs, inner) {
  const node = makeEl(tag);
  const cls = /class="([^"]*)"/.exec(attrs);
  if (cls) node.className = cls[1];
  const id = /data-id="([^"]*)"/.exec(attrs);
  if (id) node.dataset.id = id[1];
  const state = /data-state="([^"]*)"/.exec(attrs);
  if (state) node.dataset.state = state[1];
  const act = /data-act="([^"]*)"/.exec(attrs);
  if (act) node.dataset.act = act[1];
  if (/data-name-input/.test(attrs)) node.dataset.nameInput = "";
  const value = /value="([^"]*)"/.exec(attrs);
  if (value) node.value = value[1].replaceAll("&quot;", '"').replaceAll("&lt;", "<").replaceAll("&gt;", ">").replaceAll("&amp;", "&");
  if (/\sdisabled(?:\s|$)/.test(attrs)) node.disabled = true;
  node.innerHTML = inner;
  return node;
}

function matches(node, sel) {
  if (sel === "button") return node.tagName === "BUTTON";
  if (sel.startsWith("#") && !sel.includes(" ")) return node.id === sel.slice(1);
  if (sel.startsWith(".") && !sel.includes(" ")) return node.classList.contains(sel.slice(1));
  const state = /^\[data-state="([^"]*)"\]$/.exec(sel);
  if (state) return node.dataset.state === state[1];
  if (sel === "[data-state]") return node.dataset.state != null && node.dataset.state !== "";
  const act = /^\[data-act="([^"]*)"\]$/.exec(sel);
  if (act) return node.dataset.act === act[1];
  if (sel === "[data-act]") return Boolean(node.dataset.act);
  if (sel === "[data-name-input]") return node.dataset.nameInput !== undefined;
  return false;
}

function queryAll(root, sel) {
  const out = [];
  const stack = [...(root.children || [])];
  while (stack.length) {
    const node = stack.shift();
    if (matches(node, sel)) out.push(node);
    if (node.children) stack.push(...node.children);
  }
  return out;
}

function slot(id, tag) {
  const node = makeEl(tag || "div");
  node.id = id;
  return node;
}

function mount() {
  const byId = new Map();
  const sharing = slot("m_sharing");
  sharing.hidden = true;
  const servers = slot("m_team_servers");
  const add = slot("m_team_add", "button");
  const form = slot("m_team_form");
  form.hidden = true;
  const link = slot("m_team_link", "input");
  const name = slot("m_team_name", "input");
  const join = slot("m_team_join", "button");
  const cancel = slot("m_team_cancel", "button");
  const note = slot("m_team_note");
  note.hidden = true;
  const live = slot("m_team_live");
  for (const node of [sharing, servers, add, form, link, name, join, cancel, note, live]) byId.set(node.id, node);
  const doc = {
    querySelector(sel) {
      if (typeof sel === "string" && sel.startsWith("#") && !/[ .\[>:]/.test(sel.slice(1))) {
        return byId.get(sel.slice(1)) || null;
      }
      return queryAll(servers, sel)[0] || null;
    },
  };
  return { doc, sharing, servers, add, form, link, name, join, cancel, note, live };
}

function find(root, pred) {
  const stack = [...(root.children || [])];
  while (stack.length) {
    const node = stack.shift();
    if (pred(node)) return node;
    if (node.children) stack.push(...node.children);
  }
  return null;
}

function apiError(code, status) {
  const err = new Error(JSON.stringify({ error: code }));
  err.status = status;
  return err;
}

function harness(handler) {
  const calls = [];
  const api = async (path, opts) => {
    const call = { path, method: (opts && opts.method) || "GET", body: opts && opts.body };
    calls.push(call);
    return handler(call, calls);
  };
  const ui = mount();
  const section = createSharingSection({ api, doc: ui.doc });
  section.bind();
  return { ...ui, api, calls, section };
}

test("status and error text cover every listed value and an unknown one", () => {
  for (const [status, text] of STATUS) assert.equal(teamStatusText(status), text);
  assert.equal(teamStatusText(""), "unknown");
  for (const [code, text] of ERRORS) assert.equal(teamErrorText(code), text);
  assert.equal(teamErrorText(""), "That did not work. Try again.");
});

test("the section stays hidden until a server list arrives and stays visible after a later failure", async () => {
  const answers = [
    () => { throw new Error("down"); },
    () => null,
    () => "no",
    () => ({}),
    () => ({ servers: "no" }),
    () => ({ servers: [] }),
    () => { throw new Error("down"); },
  ];
  let n = 0;
  const { section, sharing, servers } = harness(() => answers[n++]());
  assert.equal(sharing.hidden, true);
  for (let i = 0; i < 5; i++) {
    await section.refresh();
    await section.settled();
    assert.equal(sharing.hidden, true, `answer ${i} revealed the section`);
  }
  await section.refresh();
  await section.settled();
  assert.equal(sharing.hidden, false);
  assert.match(servers.innerHTML, /No server joined\./);
  await section.refresh();
  await section.settled();
  assert.equal(sharing.hidden, false);
  assert.match(servers.innerHTML, /No server joined\./);
});

test("rows stay in answer order, escape markup, and show the host or the whole address", async (t) => {
  const marked = {
    ...relay,
    name: "Ada <b>",
    seat_name: "A&B",
    url: "https://relay.example/join",
  };
  const odd = { ...other, url: "not a url" };
  const file = {
    id: "cccccccccccccccccccccccccccccccc",
    name: "File relay",
    url: "file:///lab",
    seat_name: "Cy",
  };
  const gate = deferred();
  t.after(() => gate.resolve({ status: "offline" }));
  const { section, servers } = harness((call) => {
    if (call.path === "/api/team/servers") return { servers: [marked, odd, file] };
    return gate.promise;
  });
  await section.refresh();
  const html = servers.innerHTML;
  assert.ok(html.indexOf("Ada &lt;b&gt;") < html.indexOf("not a url"));
  assert.ok(html.indexOf("not a url") < html.indexOf("file:///lab"));
  assert.match(html, /Ada &lt;b&gt;/);
  assert.match(html, /A&amp;B/);
  assert.doesNotMatch(html, /Ada <b>/);
  assert.match(html, /relay\.example/);
  assert.match(html, /Your name: A&amp;B/);
  assert.equal(find(servers, (n) => n.dataset.state === marked.id).textContent, "checking");
  assert.equal(find(servers, (n) => n.dataset.act === "leave" && n.dataset.id === marked.id).textContent, "Leave");
  find(servers, (n) => n.classList.contains("teamname")).click();
  assert.equal(find(servers, (n) => n.dataset.act === "leave" && n.dataset.id === marked.id).textContent, "Leave");
});

test("status text arrives for every row at once and a late answer is dropped", async (t) => {
  const gates = [];
  const { section, servers, calls } = harness((call) => {
    if (call.path === "/api/team/servers") return { servers: [relay, other] };
    const gate = deferred();
    gates.push(gate);
    t.after(() => gate.resolve({ status: "offline" }));
    return gate.promise;
  });
  await section.refresh();
  assert.deepEqual(calls.filter((c) => c.path.endsWith("/status")).map((c) => c.path), [
    "/api/team/servers/" + relay.id + "/status",
    "/api/team/servers/" + other.id + "/status",
  ]);
  assert.equal(find(servers, (n) => n.dataset.state === relay.id).textContent, "checking");
  assert.equal(find(servers, (n) => n.dataset.state === other.id).textContent, "checking");
  const done = section.settled();
  await section.refresh();
  gates[2].resolve({ status: "offline" });
  gates[3].resolve({ status: "refused" });
  gates[0].resolve({ status: "connected" });
  gates[1].resolve({ status: "changed" });
  await done;
  assert.equal(find(servers, (n) => n.dataset.state === relay.id).textContent, "offline");
  assert.equal(find(servers, (n) => n.dataset.state === other.id).textContent, "access ended");
});

test("each status word is shown and a failed status read is unknown", async () => {
  for (const [status, text] of STATUS) {
    const { section, servers } = harness((call) => {
      if (call.path === "/api/team/servers") return { servers: [relay] };
      if (status === "anything-else") return { status: "nope" };
      return { status };
    });
    await section.refresh();
    await section.settled();
    const node = find(servers, (n) => n.dataset.state === relay.id);
    assert.ok(node, status);
    assert.equal(node.textContent, text, status);
  }
  for (const body of [{}, null]) {
    const { section, servers } = harness((call) => {
      if (call.path === "/api/team/servers") return { servers: [relay] };
      return body;
    });
    await section.refresh();
    await section.settled();
    const node = find(servers, (n) => n.dataset.state === relay.id);
    assert.ok(node);
    assert.equal(node.textContent, "unknown");
  }
  const { section, servers } = harness((call) => {
    if (call.path === "/api/team/servers") return { servers: [relay] };
    throw new Error("down");
  });
  await section.refresh();
  await section.settled();
  const node = find(servers, (n) => n.dataset.state === relay.id);
  assert.ok(node);
  assert.equal(node.textContent, "unknown");
});

test("a status answer for a row that disappeared is ignored", async (t) => {
  const gate = deferred();
  t.after(() => gate.resolve({ status: "connected" }));
  const { section, servers, calls } = harness((call) => {
    if (call.path === "/api/team/servers") return { servers: [relay] };
    return gate.promise;
  });
  await section.refresh();
  servers.children = [];
  servers.querySelector = () => null;
  gate.resolve({ status: "connected" });
  await section.settled();
  assert.equal(calls.filter((c) => c.path.endsWith("/status")).length, 1);
});

test("add, cancel, and join send the trimmed link and name once", async (t) => {
  const gate = deferred();
  t.after(() => gate.resolve({ name: "Lab relay" }));
  const { section, add, form, link, name, join, cancel, note, live, calls, servers, sharing } = harness((call) => {
    if (call.method === "POST") return gate.promise;
    if (call.path === "/api/team/servers") return { servers: [{ ...relay }] };
    return { status: "connected" };
  });
  add.click();
  assert.equal(form.hidden, false);
  assert.equal(add.hidden, true);
  link.value = "   ";
  name.value = "Ada";
  join.click();
  await section.settled();
  assert.equal(note.textContent, "That is not a join link.");
  assert.equal(note.hidden, false);
  assert.equal(calls.length, 0);

  link.value = "  https://relay.example/join#CODE  ";
  name.value = "   ";
  join.click();
  await section.settled();
  assert.equal(note.textContent, "Check the name: 1 to 64 characters.");
  assert.equal(calls.length, 0);

  name.value = "  Ada  ";
  join.click();
  join.click();
  assert.equal(join.disabled, true);
  assert.equal(calls.filter((c) => c.method === "POST").length, 1);
  assert.equal(calls[0].path, "/api/team/servers");
  assert.deepEqual(JSON.parse(calls[0].body), { link: "https://relay.example/join#CODE", name: "Ada" });
  gate.resolve({ name: "Lab relay" });
  await section.settled();
  assert.equal(form.hidden, true);
  assert.equal(add.hidden, false);
  assert.equal(link.value, "");
  assert.equal(name.value, "");
  assert.equal(note.textContent, "");
  assert.equal(live.textContent, "Joined Lab relay.");
  assert.equal(sharing.hidden, false);
  assert.match(servers.innerHTML, /Lab relay/);
  assert.equal(join.disabled, false);

  add.click();
  link.value = "https://relay.example/join#CODE";
  name.value = "Ada";
  note.textContent = "old";
  cancel.click();
  assert.equal(form.hidden, true);
  assert.equal(add.hidden, false);
  assert.equal(link.value, "");
  assert.equal(name.value, "");
  assert.equal(note.textContent, "");
  assert.equal(note.hidden, true);
});

test("join keeps the fields and shows every error, including an answer that is not JSON", async () => {
  const cases = [
    ...ERRORS.map(([code, text]) => ({ message: JSON.stringify({ error: code }), text })),
    { message: "nope", text: "That did not work. Try again." },
    { message: "{}", text: "That did not work. Try again." },
    { message: "null", text: "That did not work. Try again." },
  ];
  for (const item of cases) {
    const { add, form, link, name, join, note, section, calls } = harness(() => {
      throw new Error(item.message);
    });
    add.click();
    link.value = " https://relay.example/join#CODE ";
    name.value = " Ada ";
    join.click();
    await section.settled();
    assert.equal(note.textContent, item.text, item.message);
    assert.equal(link.value, " https://relay.example/join#CODE ");
    assert.equal(name.value, " Ada ");
    assert.equal(form.hidden, false);
    assert.equal(calls.filter((c) => c.method === "POST").length, 1);
    assert.equal(join.disabled, false);
  }
});

test("a join answer without a server name uses the name that was sent", async () => {
  for (const body of [{}, null]) {
    const { add, link, name, join, live, section } = harness((call) => {
      if (call.method === "POST") return body;
      return { servers: [] };
    });
    add.click();
    link.value = "https://relay.example/join#CODE";
    name.value = "Ada";
    join.click();
    await section.settled();
    assert.equal(live.textContent, "Joined Ada.");
  }
});

test("leave arms, disarms, and reports success", async () => {
  const { section, servers, live, calls } = harness((call) => {
    if (call.method === "POST") return null;
    if (call.path === "/api/team/servers") return { servers: [relay, other] };
    return { status: "connected" };
  });
  await section.refresh();
  await section.settled();
  const leave = (id) => find(servers, (n) => n.dataset.act === "leave" && n.dataset.id === id);
  const first = leave(relay.id);
  assert.ok(first);
  first.click();
  assert.equal(leave(relay.id).textContent, "Confirm leave");
  assert.equal(leave(relay.id).classList.contains("danger"), true);
  assert.match(servers.innerHTML, /Leaving ends everything you share on this server\./);
  leave(other.id).click();
  assert.equal(leave(relay.id).textContent, "Leave");
  assert.equal(leave(relay.id).classList.contains("danger"), false);
  assert.equal(leave(other.id).textContent, "Confirm leave");
  await section.refresh();
  await section.settled();
  assert.equal(leave(other.id).textContent, "Leave");
  assert.doesNotMatch(servers.innerHTML, /Leaving ends everything/);
  const before = calls.filter((c) => c.path.endsWith("/leave")).length;
  leave(relay.id).click();
  const confirm = leave(relay.id);
  confirm.click();
  confirm.click();
  await section.settled();
  const sent = calls.filter((c) => c.path.endsWith("/leave"));
  assert.equal(sent.length, before + 1);
  assert.equal(sent[0].method, "POST");
  assert.equal(sent[0].path, "/api/team/servers/" + relay.id + "/leave");
  assert.equal(sent[0].body, undefined);
  assert.equal(live.textContent, "Left Lab relay.");
});

test("leave shows remove only when the server cannot be left", async () => {
  for (const code of ["unreachable", "bad_server", "left_but_not_removed"]) {
    const { section, servers } = harness((call) => {
      if (call.method === "POST") throw apiError(code, 502);
      if (call.path === "/api/team/servers") return { servers: [relay] };
      return { status: "offline" };
    });
    await section.refresh();
    await section.settled();
    const leave = () => find(servers, (n) => n.dataset.act === "leave");
    const armed = leave();
    assert.ok(armed, code);
    armed.click();
    const confirm = leave();
    assert.ok(confirm, code);
    confirm.click();
    await section.settled();
    const remove = find(servers, (n) => n.dataset.act === "remove");
    if (code === "left_but_not_removed") {
      assert.equal(remove, null);
      assert.doesNotMatch(servers.innerHTML, /Remove from this computer/);
    } else {
      assert.equal(remove.textContent, "Remove from this computer");
      assert.match(servers.innerHTML, /The server keeps your seat until its operator removes it\./);
    }
    assert.match(servers.innerHTML, new RegExp(teamErrorText(code).replace(/[.]/g, "\\.")));
  }
});

test("remove forgets the server on 204 and on 404, and stays on another failure", async () => {
  const outcomes = [
    { status: 204, throw: false, gone: true },
    { status: 404, throw: true, gone: true },
    { status: 500, throw: true, gone: false },
  ];
  for (const outcome of outcomes) {
    let removed = false;
    const gate = deferred();
    const { section, servers, calls } = harness((call) => {
      if (call.method === "DELETE") return gate.promise;
      if (call.method === "POST") throw apiError("unreachable", 502);
      if (removed) return { servers: [] };
      return { servers: [relay] };
    });
    await section.refresh();
    await section.settled();
    const leave = () => find(servers, (n) => n.dataset.act === "leave");
    const armed = leave();
    assert.ok(armed);
    armed.click();
    const confirm = leave();
    assert.ok(confirm);
    confirm.click();
    await section.settled();
    const remove = find(servers, (n) => n.dataset.act === "remove");
    assert.ok(remove);
    remove.click();
    remove.click();
    assert.equal(calls.filter((c) => c.method === "DELETE").length, 1);
    assert.equal(calls.find((c) => c.method === "DELETE").path, "/api/team/servers/" + relay.id);
    if (outcome.throw) gate.reject(apiError("store_unavailable", outcome.status));
    else gate.resolve(null);
    if (outcome.gone) removed = true;
    await section.settled();
    if (outcome.gone) assert.match(servers.innerHTML, /No server joined\./);
    else {
      assert.match(servers.innerHTML, /Remove from this computer/);
      assert.match(servers.innerHTML, /Lab relay/);
      assert.match(servers.innerHTML, /That did not work\. Try again\./);
    }
  }
});

test("destroy removes the listeners and clears the link field", async () => {
  const { section, add, form, link } = harness(() => ({ servers: [] }));
  link.value = "https://relay.example/join#CODE";
  section.destroy();
  assert.equal(link.value, "");
  add.click();
  await section.settled();
  assert.equal(form.hidden, true);
});

test("missing slots do nothing", async () => {
  const calls = [];
  const section = createSharingSection({
    api: async (path) => {
      calls.push(path);
      return { servers: [] };
    },
    doc: { querySelector() { return null; } },
  });
  section.bind();
  await section.refresh();
  await section.settled();
  section.destroy();
  assert.deepEqual(calls, []);
});

function teamNameAction(ui, id, act) {
  return find(ui.servers, node => node.dataset.act === act && node.dataset.id === id);
}
function teamNameField(ui) { return find(ui.servers, node => node.dataset.nameInput !== undefined); }
function teamNameInput(ui, text) {
  const field = teamNameField(ui); assert.ok(field); field.value = text;
  for (const fn of ui.servers.listeners.get("input") || []) fn({ target: field });
  return field;
}
function teamNameKey(ui, key) {
  const field = teamNameField(ui); let stopped = false, prevented = false;
  for (const fn of ui.servers.listeners.get("keydown") || []) fn({ target: field, key, stopPropagation() { stopped = true; }, preventDefault() { prevented = true; } });
  return { stopped, prevented };
}

test("server name edit saves once, survives refresh and restores focus", async () => {
  const pending = deferred(); let count = 0;
  const ui = harness(call => {
    if (call.path === "/api/team/servers") return { servers: [relay, other] };
    if (call.method === "PUT") { count++; return pending.promise; }
    return { status: "connected" };
  });
  await ui.section.refresh(); await ui.section.settled();
  assert.match(ui.servers.innerHTML, /Your name: Ada/); assert.match(ui.servers.innerHTML, /Your name: Bea/);
  teamNameAction(ui, relay.id, "name-edit").click();
  assert.equal(teamNameField(ui).value, "Ada"); teamNameInput(ui, "Synthetic <&\"");
  await ui.section.refresh(); await ui.section.settled(); assert.equal(teamNameField(ui).value, "Synthetic <&\"");
  teamNameAction(ui, relay.id, "name-save").click();
  assert.equal(count, 1); assert.equal(teamNameAction(ui, relay.id, "name-save").disabled, true); assert.equal(teamNameAction(ui, relay.id, "name-cancel").disabled, true);
  teamNameAction(ui, relay.id, "name-save").click(); assert.equal(count, 1);
  const sent = ui.calls.find(call => call.method === "PUT"); assert.equal(sent.path, "/api/team/servers/" + relay.id + "/seat"); assert.deepEqual(JSON.parse(sent.body), { name: "Synthetic <&\"" });
  pending.resolve({ ...relay, seat_name: "Synthetic <&\"" }); await ui.section.settled();
  assert.match(ui.servers.innerHTML, /Your name: Synthetic &lt;&amp;&quot;/); assert.equal(focusedNode, teamNameAction(ui, relay.id, "name-edit"));
});

test("server name cancel, escape and failure retain the right state", async () => {
  let puts = 0;
  const ui = harness(call => { if (call.path === "/api/team/servers") return { servers: [relay] }; if (call.method === "PUT") { puts++; throw apiError("name_refused", 422); } return { status: "connected" }; });
  await ui.section.refresh(); await ui.section.settled();
  assert.equal(teamErrorText("name_refused"), "The server did not accept that name.");
  teamNameAction(ui, relay.id, "name-edit").click(); teamNameInput(ui, "Synthetic"); teamNameAction(ui, relay.id, "name-save").click(); await ui.section.settled();
  assert.equal(puts, 1); assert.equal(teamNameField(ui).value, "Synthetic"); assert.equal(ui.note.textContent, "The server did not accept that name.");
  teamNameAction(ui, relay.id, "name-cancel").click(); assert.equal(teamNameField(ui), null); assert.equal(focusedNode, teamNameAction(ui, relay.id, "name-edit"));
  teamNameAction(ui, relay.id, "name-edit").click(); teamNameInput(ui, "Other"); assert.deepEqual(teamNameKey(ui, "Escape"), { stopped: true, prevented: true });
  assert.equal(teamNameField(ui), null); assert.equal(focusedNode, teamNameAction(ui, relay.id, "name-edit")); assert.equal(puts, 1);
});

test("late seat rename follows a rebuilt row and is dropped if it is gone", async () => {
  for (const exists of [true, false]) {
    const pending = deferred(); let loads = 0, done = false;
    const ui = harness(call => {
      if (call.path === "/api/team/servers") { loads++; return { servers: loads === 2 && !exists ? [] : [relay] }; }
      if (call.method === "PUT") return pending.promise;
      return { status: "connected" };
    });
    await ui.section.refresh(); await ui.section.settled();
    teamNameAction(ui, relay.id, "name-edit").click(); teamNameInput(ui, "Synthetic"); teamNameAction(ui, relay.id, "name-save").click();
    const refresh = ui.section.refresh().then(() => { done = true; });
    await new Promise(resolve => setTimeout(resolve, 0));
    try {
      assert.equal(done, true, "refresh must finish while save is pending");
      if (exists) assert.equal(teamNameField(ui).value, "Synthetic");
      else assert.match(ui.servers.innerHTML, /No server joined/);
    } finally { pending.resolve({ ...relay, seat_name: "Synthetic" }); await refresh; await ui.section.settled(); }
    if (exists) assert.match(ui.servers.innerHTML, /Your name: Synthetic/);
    else {
      assert.match(ui.servers.innerHTML, /No server joined/);
      await ui.section.refresh(); await ui.section.settled();
      assert.match(ui.servers.innerHTML, /Your name: Ada/); assert.equal(teamNameField(ui), null);
    }
  }
});
