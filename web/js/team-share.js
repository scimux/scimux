/* Share button, share sheet, and status line.
 *
 * api, doc, openSheet, closeSheets, and now are injected. This file does not
 * touch document, window, or fetch. Server and share reads run on one queue.
 * A newer select drops an older answer. Seats reads are tracked beside the
 * queue so a later choice can drop an earlier roster. settled() waits for both.
 * A click that sends a request starts it before the handler returns.
 */
import { esc } from "./format.js";

const SERVERS = "/api/team/servers";
const INTERVAL = 10000;
const LEFT = "a server you left";

const SHARE_ICON = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="6" cy="12" r="2"></circle><circle cx="18" cy="6" r="2"></circle><circle cx="18" cy="18" r="2"></circle><path d="M8 11.2 15.2 7.2"></path><path d="M8 12.8 15.2 16.8"></path></svg>';

const ERROR_TEXT = {
  unreachable: "The server could not be reached.",
  bad_server: "That address no longer answers as this server.",
  refused: "The server ended this computer's access.",
  unknown_member: "Someone in the list has left the server. Open the sheet again.",
  server_full: "The server has no room for another shared chat.",
  too_many: "This computer shares too many chats. Stop one first.",
  no_history: "This chat has nothing to share yet.",
  not_active: "Sharing of this chat has ended. Stop it, then share again.",
  share_fixed: "Stop sharing first to change the server or the scope.",
  store_unavailable: "This computer could not save the change. Try again.",
};

export function shareStatusText(view, serverName) {
  const count = (view.members || []).length;
  const name = serverName;
  if (view.state === "stopping") return "Stopping sharing…";
  if (view.state === "lost") return "Sharing ended on " + name;
  if (view.state === "full") return "Sharing paused · " + name + " is full";
  if (view.delivery === "offline") {
    return "Shared with " + count + " · " + name + " · offline, will retry";
  }
  if (view.delivery === "refused") {
    return "Sharing stopped · " + name + " ended this computer's access";
  }
  if (view.delivery === "changed") {
    return "Sharing paused · " + name + " is not the same server";
  }
  return "Shared with " + count + " · " + name;
}

export function shareErrorText(code) {
  return ERROR_TEXT[code] || "That did not work. Try again.";
}

export function createShareFeature({ api, doc, openSheet, closeSheets, now }) {
  const $ = (sel) => doc.querySelector(sel);

  let off = false;
  let joined = [];
  let selected = "";
  let view = null;
  let sheetNode = "";
  let armed = false;
  let sheetBusy = false;
  let generation = 0;
  let pendingLoads = 0;
  let lastLoadAt = 0;
  let seatsGen = 0;
  let drawnNode = "";
  let drawnShare = "";
  let chain = Promise.resolve();
  const side = new Set();
  const cleanups = [];

  function enqueue(fn) {
    chain = chain.then(fn);
    return chain;
  }

  function track(pending) {
    side.add(pending);
    pending.finally(() => {
      side.delete(pending);
    });
  }

  function stale(gen) {
    return gen !== generation;
  }

  function serverList(body) {
    if (body && Array.isArray(body.servers)) return body.servers;
    return [];
  }

  function nameOf(id) {
    for (const server of joined) {
      if (server.id === id) return server.name;
    }
    return LEFT;
  }

  function hideControls() {
    $("#sharebtn").hidden = true;
    $("#sharestatus").hidden = true;
  }

  function paintHeader() {
    const btn = $("#sharebtn");
    const status = $("#sharestatus");
    if (joined.length === 0) {
      btn.hidden = true;
      status.hidden = true;
      return;
    }
    btn.hidden = false;
    if (!view) {
      status.hidden = true;
      return;
    }
    status.hidden = false;
    status.textContent = shareStatusText(view, nameOf(view.server));
  }

  function beginLoad(node) {
    const gen = ++generation;
    pendingLoads += 1;
    lastLoadAt = now();
    const listed = api(SERVERS);
    return enqueue(async () => {
      try {
        await runLoad(gen, node, listed);
      } finally {
        pendingLoads -= 1;
      }
    });
  }

  async function runLoad(gen, node, listedPromise) {
    let listed = null;
    let serverErr = null;
    try {
      listed = await listedPromise;
    } catch (err) {
      serverErr = err;
    }
    if (stale(gen)) return;
    if (serverErr) {
      if (serverErr.status === 404) off = true;
      return;
    }
    let shareBody = null;
    let shareErr = null;
    try {
      shareBody = await api("/api/team/shares/" + node);
    } catch (err) {
      shareErr = err;
    }
    if (stale(gen)) return;
    joined = serverList(listed);
    if (shareErr) {
      if (shareErr.status === 404) {
        view = null;
        paintHeader();
      }
      return;
    }
    view = shareBody && typeof shareBody === "object" ? shareBody : null;
    paintHeader();
  }

  function select(node) {
    const shown = sheetNode;
    selected = node;
    hideControls();
    if (shown && shown !== node) {
      sheetNode = "";
      closeSheets();
    }
    if (off) return;
    return beginLoad(node);
  }

  function tick() {
    if (off) return;
    if (!selected) return;
    if (pendingLoads > 0) return;
    if (now() - lastLoadAt < INTERVAL) return;
    return beginLoad(selected);
  }

  function clearNote() {
    const note = $("#sh_note");
    note.textContent = "";
    note.hidden = true;
  }

  function showNote(code) {
    const note = $("#sh_note");
    note.textContent = shareErrorText(code);
    note.hidden = false;
  }

  function codeOf(err) {
    let body;
    try {
      body = JSON.parse(err.message);
    } catch {
      return "";
    }
    if (!body || typeof body.error !== "string") return "";
    return body.error;
  }

  function setApply(text, hidden, disabled) {
    const apply = $("#sh_apply");
    apply.hidden = hidden;
    apply.disabled = disabled;
    apply.textContent = text;
  }

  function setStop(hidden) {
    const stop = $("#sh_stop");
    stop.hidden = hidden;
    stop.disabled = false;
    stop.textContent = "Stop sharing";
  }

  function serverFields() {
    let html = "<fieldset><legend>Server</legend>";
    for (let i = 0; i < joined.length; i++) {
      const server = joined[i];
      const checked = i === 0 ? " checked" : "";
      html += '<label><input type="radio" name="sh_server" value="' + esc(server.id) + '"' +
        checked + ">" + esc(server.name) + "</label>";
    }
    html += "</fieldset>";
    return html;
  }

  function scopeFields() {
    return '<fieldset><legend>What to share</legend>' +
      '<label><input type="radio" name="sh_scope" value="from_now" checked>From now on</label>' +
      '<label><input type="radio" name="sh_scope" value="all">Entire history</label>' +
      "</fieldset>" +
      "<p>Prompts and answers only.</p>" +
      "<p>Tool details stay on this computer.</p>";
  }

  function accessOf(seatId) {
    if (!view || !Array.isArray(view.members)) return "";
    for (const member of view.members) {
      if (member.seat !== seatId) continue;
      if (member.access === "write") return "write";
      if (member.access === "read") return "read";
      return "";
    }
    return "";
  }

  function option(value, label, on) {
    return '<option value="' + value + '"' + (on ? " selected" : "") + ">" + label + "</option>";
  }

  function personRow(seat) {
    const access = accessOf(seat.id);
    return "<label>" + esc(seat.name) +
      '<select data-seat="' + esc(seat.id) + '" aria-label="' + esc(seat.name) + '">' +
      option("", "No access", access === "") +
      option("read", "Can view", access === "read") +
      option("write", "Can comment", access === "write") +
      "</select></label>";
  }

  function renderNew(body) {
    body.innerHTML = serverFields() + scopeFields() + '<div id="sh_people">Loading people…</div>';
    setApply("Share chat", false, false);
    setStop(true);
  }

  function renderActive(body) {
    const scope = view.scope === "all" ? "Entire history" : "From now on";
    body.innerHTML =
      "<p>" + esc(nameOf(view.server)) + "</p>" +
      "<p>" + scope + "</p>" +
      "<p>To change the server or the scope, stop sharing and share again.</p>" +
      '<div id="sh_people">Loading people…</div>' +
      "<p>Members keep what they already received.</p>";
    setApply("Save people", false, true);
    setStop(false);
  }

  function renderEnded(body) {
    body.innerHTML =
      "<p>" + esc(shareStatusText(view, nameOf(view.server))) + "</p>" +
      "<p>Members keep what they already received.</p>";
    setApply("", true, true);
    setStop(false);
  }

  function renderStopping(body) {
    body.innerHTML = "<p>" + esc(shareStatusText(view, nameOf(view.server))) + "</p>";
    setApply("", true, true);
    setStop(true);
  }

  function renderSheet() {
    armed = false;
    const body = $("#sh_body");
    if (!view) renderNew(body);
    else if (view.state === "active") renderActive(body);
    else if (view.state === "lost" || view.state === "full") renderEnded(body);
    else renderStopping(body);
    rememberDrawn();
  }

  function shareToken(v) {
    if (!v) return "";
    return String(v.uid) + "\n" + String(v.state);
  }

  function rememberDrawn() {
    drawnNode = selected;
    drawnShare = shareToken(view);
  }

  function wantsPeople() {
    if (!view) return true;
    return view.state === "active";
  }

  function checkedValue(name, fallback) {
    for (const input of $("#sh_body").querySelectorAll("input")) {
      if (input.name === name && input.checked) return input.value;
    }
    return fallback;
  }

  function firstServerId() {
    for (const server of joined) return server.id;
    return "";
  }

  function seatServer() {
    if (view) return view.server;
    return checkedValue("sh_server", firstServerId());
  }

  function chosenMembers() {
    const members = [];
    for (const sel of $("#sh_body").querySelectorAll("select")) {
      if (sel.value === "read" || sel.value === "write") {
        members.push({ seat: sel.dataset.seat, access: sel.value });
      }
    }
    return members;
  }

  function putBody() {
    if (!view) {
      return {
        server: checkedValue("sh_server", firstServerId()),
        scope: checkedValue("sh_scope", "from_now"),
        members: chosenMembers(),
      };
    }
    return { server: view.server, scope: view.scope, members: chosenMembers() };
  }

  function chosenServerJoined() {
    const id = checkedValue("sh_server", firstServerId());
    for (const server of joined) {
      if (server.id === id) return true;
    }
    return false;
  }

  function sheetDrifted() {
    if (drawnNode !== selected) return true;
    if (shareToken(view) !== drawnShare) return true;
    if (drawnShare === "" && !chosenServerJoined()) return true;
    return false;
  }

  function abandonSheet() {
    if (joined.length === 0) {
      sheetNode = "";
      closeSheets();
      return;
    }
    showSheet();
    const note = $("#sh_note");
    note.textContent = "Sharing of this chat changed. Check and try again.";
    note.hidden = false;
  }

  function sameAsView() {
    for (const sel of $("#sh_body").querySelectorAll("select")) {
      if (sel.value !== accessOf(sel.dataset.seat)) return false;
    }
    return true;
  }

  function syncApply() {
    const apply = $("#sh_apply");
    if (!view) {
      apply.disabled = false;
      return;
    }
    apply.disabled = sameAsView();
  }

  function applyShouldDisable() {
    if ($("#sh_apply").hidden) return true;
    if (!view) return false;
    return sameAsView();
  }

  function loadSeats(serverId) {
    const gen = ++seatsGen;
    const node = selected;
    $("#sh_people").textContent = "Loading people…";
    const pending = api(SERVERS + "/" + serverId + "/seats").then(
      (body) => applySeats(gen, node, body, null),
      (err) => applySeats(gen, node, null, err),
    );
    track(pending);
  }

  function applySeats(gen, node, body, err) {
    if (gen !== seatsGen) return;
    if (node !== selected) return;
    const host = $("#sh_people");
    if (err) {
      host.textContent = shareErrorText(codeOf(err));
      syncApply();
      return;
    }
    const seats = body && Array.isArray(body.seats) ? body.seats : [];
    const selfId = body && typeof body.self === "string" ? body.self : "";
    const others = [];
    for (const seat of seats) {
      if (seat.id !== selfId) others.push(seat);
    }
    if (others.length === 0) {
      host.textContent = "Nobody else has joined this server yet.";
      syncApply();
      return;
    }
    host.innerHTML = others.map(personRow).join("");
    syncApply();
  }

  function showSheet() {
    clearNote();
    renderSheet();
    if (wantsPeople()) loadSeats(seatServer());
  }

  function onOpen() {
    sheetNode = selected;
    showSheet();
    openSheet("sharesheet");
  }

  function onSheetChange(ev) {
    const target = ev.target;
    if (target.name === "sh_server") {
      disarmStop();
      loadSeats(target.value);
      return;
    }
    if (target.tagName === "SELECT") syncApply();
  }

  function onSheetClick(ev) {
    if (ev.target === $("#sh_stop")) {
      onStop();
      return;
    }
    disarmStop();
    if (ev.target === $("#sh_apply")) onApply();
  }

  function disarmStop() {
    if (!armed) return;
    armed = false;
    $("#sh_stop").textContent = "Stop sharing";
  }

  function onStop() {
    if (sheetBusy) return;
    const stop = $("#sh_stop");
    if (!armed) {
      armed = true;
      stop.textContent = "Confirm stop";
      if (doc.activeElement === stop) stop.focus();
      return;
    }
    if (sheetDrifted()) {
      abandonSheet();
      return;
    }
    sheetBusy = true;
    $("#sh_apply").disabled = true;
    stop.disabled = true;
    const pending = api("/api/team/shares/" + selected, { method: "DELETE" });
    enqueue(() => finishDelete(pending));
  }

  function onApply() {
    if (sheetBusy) return;
    const apply = $("#sh_apply");
    if (apply.hidden) return;
    if (apply.disabled) return;
    if (sheetDrifted()) {
      abandonSheet();
      return;
    }
    const restore = doc.activeElement === apply;
    const hadView = Boolean(view);
    const body = JSON.stringify(putBody());
    sheetBusy = true;
    apply.disabled = true;
    $("#sh_stop").disabled = true;
    const pending = api("/api/team/shares/" + selected, {
      method: "PUT",
      body,
    });
    enqueue(() => finishPut(pending, hadView, restore));
  }

  async function finishPut(pending, hadView, restore) {
    try {
      view = await pending;
      paintHeader();
      showSheet();
      $("#sh_live").textContent = hadView ? "People updated." : "Sharing started.";
      if (restore) $("#sh_apply").focus();
    } catch (err) {
      showNote(codeOf(err));
      $("#sh_stop").disabled = false;
      $("#sh_apply").disabled = applyShouldDisable();
    } finally {
      sheetBusy = false;
    }
  }

  function markStopped() {
    view = null;
    paintHeader();
    sheetNode = "";
    closeSheets();
    $("#sh_live").textContent = "Sharing stopped.";
  }

  async function finishDelete(pending) {
    try {
      const body = await pending;
      if (typeof body === "object") {
        view = body;
        paintHeader();
        showSheet();
      } else {
        markStopped();
      }
    } catch (err) {
      if (err.status === 404) markStopped();
      else showNote(codeOf(err));
    } finally {
      sheetBusy = false;
      $("#sh_stop").disabled = false;
      $("#sh_apply").disabled = applyShouldDisable();
    }
  }

  function listen(el, type, fn) {
    el.addEventListener(type, fn);
    cleanups.push(() => el.removeEventListener(type, fn));
  }

  function bind() {
    const btn = $("#sharebtn");
    btn.innerHTML = SHARE_ICON;
    listen(btn, "click", onOpen);
    listen($("#sharestatus"), "click", onOpen);
    listen($("#sharesheet"), "click", onSheetClick);
    listen($("#sharesheet"), "change", onSheetChange);
  }

  function destroy() {
    while (cleanups.length) cleanups.pop()();
  }

  async function settled() {
    let prev;
    do {
      prev = chain;
      const pending = [...side];
      await prev;
      await Promise.all(pending);
    } while (chain !== prev || side.size > 0);
  }

  return { select, tick, bind, destroy, settled };
}
