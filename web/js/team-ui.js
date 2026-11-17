/* Sharing section of the burger menu.
 *
 * Owned root: #m_sharing. api and doc are injected. This file does not touch
 * document, window, or fetch.
 *
 * List, join, leave, and remove run on one queue so their answers cannot pass
 * each other. Status reads start during a refresh and are not awaited there,
 * so a newer refresh can drop an older answer. settled() waits for the queue
 * and for those reads. A click that sends a request starts it before the
 * handler returns, then applies the answer on the queue.
 */
import { esc } from "./format.js";

const STATUS_TEXT = {
  connected: "connected",
  offline: "offline",
  refused: "access ended",
  changed: "not the same server",
};

const ERROR_TEXT = {
  invalid_link: "That is not a join link.",
  bad_request: "Check the name: 1 to 64 characters.",
  unreachable: "The server could not be reached.",
  bad_server: "That address is not a scimux team server.",
  already_joined: "This computer has already joined that server.",
  join_refused: "The server refused the join code. Ask for a new one.",
  already_registered: "This computer is still registered on that server. Ask its operator to remove the old seat.",
  server_full: "The server has no free seat.",
  joined_but_not_saved: "Joined, but this computer could not save it. Ask for a new join code.",
  left_but_not_removed: "Left the server, but this computer could not remove the entry. Try again.",
  name_refused: "The server did not accept that name.",
};

const LEAVING_NOTE = "Leaving ends everything you share on this server.";
const KEEP_NOTE = "The server keeps your seat until its operator removes it.";

export function teamStatusText(status) {
  return STATUS_TEXT[status] || "unknown";
}

export function teamErrorText(code) {
  return ERROR_TEXT[code] || "That did not work. Try again.";
}

export function createSharingSection({ api, doc, onJoined = () => {} }) {
  const $ = (sel) => doc.querySelector(sel);

  let rows = [];
  let byId = new Map();
  let states = new Map();
  let rowMessage = new Map();
  let armed = "";
  let removeFor = "";
  let leaving = false;
  let busyRemove = false;
  let nameEdit = null;
  const nameVersions = new Map();
  let generation = 0;
  let chain = Promise.resolve();
  const inflight = new Set();
  const cleanups = [];

  function enqueue(fn) {
    chain = chain.then(fn);
    return chain;
  }

  function refresh() {
    return enqueue(refreshNow);
  }

  async function refreshNow() {
    generation += 1;
    if (!$("#m_sharing")) return;
    await load();
  }

  async function load() {
    let body;
    const beforeNames = new Map(nameVersions);
    try {
      body = await api("/api/team/servers");
    } catch {
      return;
    }
    if (!body) return;
    if (typeof body !== "object") return;
    if (!Array.isArray(body.servers)) return;
    armed = "";
    removeFor = "";
    rowMessage.clear();
    rows = body.servers.map(server => {
      if (nameVersions.get(server.id) !== beforeNames.get(server.id)) {
        return { ...server, seat_name: byId.get(server.id).seat_name };
      }
      return server;
    });
    byId = new Map();
    states = new Map();
    for (const server of rows) byId.set(server.id, server);
    if (nameEdit && !byId.has(nameEdit.id)) nameEdit = null;
    $("#m_sharing").hidden = false;
    render();
    for (const server of rows) trackStatus(generation, server.id);
  }

  function hostOf(url) {
    try {
      const host = new URL(String(url)).host;
      if (host) return host;
    } catch {
      /* The text is not a URL, so the row shows it unchanged. */
    }
    return String(url);
  }

  function rowHTML(server) {
    const id = esc(server.id);
    const state = states.get(server.id) || "checking";
    const confirm = armed === server.id;
    let notes = "";
    if (confirm) notes += '<p class="teamnote">' + LEAVING_NOTE + "</p>";
    if (rowMessage.has(server.id)) notes += '<p class="teamnote">' + esc(rowMessage.get(server.id)) + "</p>";
    let remove = "";
    if (removeFor === server.id) {
      remove = '<p class="teamnote">' + KEEP_NOTE + "</p>" +
        '<button type="button" data-act="remove" data-id="' + id + '">Remove from this computer</button>';
    }
    let name = '<span class="teamseat">Your name: ' + esc(server.seat_name) + '</span>' +
      '<button type="button" data-act="name-edit" data-id="' + id + '">Change name</button>';
    if (nameEdit && nameEdit.id === server.id) {
      const disabled = nameEdit.busy ? ' disabled' : '';
      name = '<input data-name-input aria-label="Your name" value="' + esc(nameEdit.text) + '">' +
        '<button type="button" data-act="name-save" data-id="' + id + '"' + disabled + '>Save</button>' +
        '<button type="button" data-act="name-cancel" data-id="' + id + '"' + disabled + '>Cancel</button>';
    }
    return '<div class="teamrow">' +
      '<span class="teamname">' + esc(server.name) + "</span>" +
      '<span class="teammeta">' + esc(hostOf(server.url)) + "</span>" + name +
      '<span class="teamstate" data-state="' + id + '">' + esc(state) + "</span>" +
      '<button type="button" class="' + (confirm ? "cta danger" : "cta") + '" data-act="leave" data-id="' + id + '">' +
      (confirm ? "Confirm leave" : "Leave") + "</button>" +
      notes + remove +
      "</div>";
  }

  function render() {
    const root = $("#m_team_servers");
    if (rows.length === 0) {
      root.innerHTML = '<p class="teamnote">No server joined.</p>';
      return;
    }
    root.innerHTML = rows.map(rowHTML).join("");
  }

  function statusValue(body) {
    if (!body) return "";
    if (typeof body.status !== "string") return "";
    return body.status;
  }

  function applyStatus(gen, id, status) {
    if (gen !== generation) return;
    states.set(id, teamStatusText(status));
    paint(id);
  }

  function paint(id) {
    const node = $("#m_team_servers").querySelector('[data-state="' + id + '"]');
    if (!node) return;
    node.textContent = states.get(id);
  }

  function trackStatus(gen, id) {
    const path = "/api/team/servers/" + id + "/status";
    const pending = api(path).then(
      (body) => applyStatus(gen, id, statusValue(body)),
      () => applyStatus(gen, id, ""),
    );
    inflight.add(pending);
    pending.finally(() => inflight.delete(pending));
  }

  function setNote(text) {
    const note = $("#m_team_note");
    note.textContent = text;
    note.hidden = text === "";
  }

  function hideForm() {
    $("#m_team_form").hidden = true;
    $("#m_team_add").hidden = false;
    $("#m_team_link").value = "";
    $("#m_team_name").value = "";
    setNote("");
  }

  function onAdd() {
    $("#m_team_form").hidden = false;
    $("#m_team_add").hidden = true;
  }

  function errorCode(message) {
    let body;
    try {
      body = JSON.parse(message);
    } catch {
      return "";
    }
    if (!body) return "";
    if (typeof body.error !== "string") return "";
    return body.error;
  }

  function onJoin() {
    const link = $("#m_team_link").value.trim();
    const name = $("#m_team_name").value.trim();
    if (!link) {
      setNote(teamErrorText("invalid_link"));
      return;
    }
    if (!name) {
      setNote(teamErrorText("bad_request"));
      return;
    }
    const button = $("#m_team_join");
    if (button.disabled) return;
    button.disabled = true;
    const pending = api("/api/team/servers", {
      method: "POST",
      body: JSON.stringify({ link, name }),
    });
    enqueue(() => finishJoin(pending, name));
  }

  async function finishJoin(pending, typedName) {
    try {
      const body = await pending;
      let joined = typedName;
      if (body) {
        if (typeof body.name === "string") joined = body.name;
      }
      $("#m_team_live").textContent = "Joined " + joined + ".";
      hideForm();
      await refreshNow();
      onJoined();
    } catch (err) {
      setNote(teamErrorText(errorCode(err.message)));
    } finally {
      $("#m_team_join").disabled = false;
    }
  }

  function actionNode(node) {
    let current = node;
    while (current) {
      if (current.dataset.act) return current;
      current = current.parentElement;
    }
    return null;
  }

  function onListClick(ev) {
    const node = actionNode(ev.target);
    if (!node) return;
    if (node.dataset.act === "leave") onLeave(node.dataset.id);
    else if (node.dataset.act === "remove") onRemove(node.dataset.id);
    else if (node.dataset.act === "name-edit") onNameEdit(node.dataset.id);
    else if (node.dataset.act === "name-save") onNameSave(node.dataset.id);
    else if (node.dataset.act === "name-cancel") onNameCancel();
  }

  function focusName(id) {
    const buttons = $("#m_team_servers").querySelectorAll('[data-act="name-edit"]');
    const button = [...buttons].find(item => item.dataset.id === id);
    if (button) button.focus();
  }
  function onNameEdit(id) {
    if (nameEdit && nameEdit.busy) return;
    const server = byId.get(id);
    if (!server) return;
    nameEdit = { id, text: server.seat_name, busy: false };
    setNote(""); render();
    const field = $("#m_team_servers").querySelector("[data-name-input]");
    if (field) field.focus();
  }
  function onNameInput(ev) {
    if (!nameEdit) return;
    if (ev.target.dataset.nameInput === undefined) return;
    nameEdit.text = ev.target.value;
  }
  function onNameKey(ev) {
    if (!nameEdit) return;
    if (ev.target.dataset.nameInput === undefined) return;
    if (ev.key !== "Escape") return;
    ev.preventDefault(); ev.stopPropagation(); onNameCancel();
  }
  function onNameCancel() {
    if (!nameEdit) return;
    if (nameEdit.busy) return;
    const id = nameEdit.id;
    nameEdit = null; setNote(""); render(); focusName(id);
  }
  function onNameSave(id) {
    if (!nameEdit) return;
    if (nameEdit.id !== id) return;
    if (nameEdit.busy) return;
    const edit = nameEdit;
    edit.busy = true; render();
    const pending = api("/api/team/servers/" + id + "/seat", { method: "PUT", body: JSON.stringify({ name: edit.text }) });
    const work = finishName(edit, pending);
    inflight.add(work);
    work.finally(() => inflight.delete(work));
  }
  async function finishName(edit, pending) {
    try {
      const server = await pending;
      if (byId.has(edit.id)) {
        rows = rows.map(item => item.id === edit.id ? server : item);
        byId.set(edit.id, server);
        nameVersions.set(edit.id, (nameVersions.get(edit.id) || 0) + 1);
        if (nameEdit === edit) nameEdit = null;
        render(); focusName(edit.id);
      }
    } catch (err) {
      if (nameEdit === edit) {
        edit.busy = false; setNote(teamErrorText(errorCode(err.message))); render();
      }
    }
  }

  function onLeave(id) {
    if (leaving) return;
    if (armed !== id) {
      armed = id;
      removeFor = "";
      rowMessage.clear();
      render();
      return;
    }
    const server = byId.get(id);
    armed = "";
    leaving = true;
    render();
    const pending = api("/api/team/servers/" + id + "/leave", { method: "POST" });
    enqueue(() => finishLeave(server, pending));
  }

  async function finishLeave(server, pending) {
    try {
      await pending;
      $("#m_team_live").textContent = "Left " + server.name + ".";
      await refreshNow();
    } catch (err) {
      const code = errorCode(err.message);
      rowMessage.set(server.id, teamErrorText(code));
      if (code === "unreachable" || code === "bad_server") removeFor = server.id;
      render();
    } finally {
      leaving = false;
    }
  }

  function onRemove(id) {
    if (busyRemove) return;
    busyRemove = true;
    const pending = api("/api/team/servers/" + id, { method: "DELETE" });
    enqueue(() => finishRemove(id, pending));
  }

  async function finishRemove(id, pending) {
    try {
      await pending;
      await refreshNow();
    } catch (err) {
      if (err.status === 404) {
        await refreshNow();
        return;
      }
      rowMessage.set(id, teamErrorText(errorCode(err.message)));
      removeFor = id;
      render();
    } finally {
      busyRemove = false;
    }
  }

  function listen(el, type, fn) {
    el.addEventListener(type, fn);
    cleanups.push(() => el.removeEventListener(type, fn));
  }

  function bind() {
    const add = $("#m_team_add");
    const cancel = $("#m_team_cancel");
    const join = $("#m_team_join");
    const list = $("#m_team_servers");
    if ([add, cancel, join, list].some((el) => !el)) return;
    listen(add, "click", onAdd);
    listen(cancel, "click", hideForm);
    listen(join, "click", onJoin);
    listen(list, "click", onListClick);
    listen(list, "input", onNameInput);
    listen(list, "keydown", onNameKey);
  }

  function destroy() {
    while (cleanups.length) cleanups.pop()();
    const link = $("#m_team_link");
    if (link) link.value = "";
  }

  async function settled() {
    let prev;
    do {
      prev = chain;
      const pending = [...inflight];
      await prev;
      await Promise.all(pending);
    } while (chain !== prev);
  }

  return { refresh, bind, destroy, settled };
}
