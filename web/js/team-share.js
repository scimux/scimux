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

export const SHARE_ICON = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="6" cy="12" r="2"></circle><circle cx="18" cy="6" r="2"></circle><circle cx="18" cy="18" r="2"></circle><path d="M8 11.2 15.2 7.2"></path><path d="M8 12.8 15.2 16.8"></path></svg>';

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
  share_changed: "Sharing changed. Close this sheet and review it again.",
  store_unavailable: "This computer could not save the change. Try again.",
  lane_share: "This chat is shared through its lane.",
};

export function laneCountLine(open, archived){
  const head = "Shares " + (open || 0) + " chats now";
  const tail = "new chats in this lane are shared automatically";
  if (!archived) return head + " · " + tail;
  return head + " · " + archived + " archived chats stay private · " + tail;
}

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

export function createShareFeature({ api, doc, openSheet, closeSheets, now, onChange = () => {}, laneFacts, onLanes = () => {} }) {
  const $ = (sel) => doc.querySelector(sel);

  let destroyed = false;
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
  let drawnMembers = [];
  let seatsReady = false;
  let chain = Promise.resolve();
  let sheetKind = "chat";
  let laneTarget = "";
  let laneView = null;
  let laneShares = [];
  let laneFlight = false;
  let laneTicket = 0;
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
      if (serverErr.status === 404) onChange(node, null);
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
        onChange(node, null);
        paintHeader();
      }
      refreshLanes(gen);
      return;
    }
    view = shareBody && typeof shareBody === "object" ? shareBody : null;
    onChange(node, view);
    paintHeader();
    refreshLanes(gen);
  }

  function refreshLanes(gen) {
    if (typeof laneFacts !== "function") return;
    const pending = api("/api/team/lane-shares").then(
      (body) => {
        if (stale(gen)) return;
        laneShares = body && Array.isArray(body.lane_shares) ? body.lane_shares : [];
        onLanes();
        reconcileLane();
      },
      () => {},
    );
    track(pending);
  }

  function reconcileLane() {
    if (laneFlight) return;
    for (const share of laneShares) {
      if (!share || share.state !== "active") continue;
      const facts = laneFacts(share.lane);
      if (!facts) continue;
      const color = String(facts.color).toLowerCase();
      if (facts.name === share.name && color === share.color) continue;
      laneFlight = true;
      const pending = api("/api/team/lane-shares/" + share.lane, {
        method: "PUT",
        body: JSON.stringify({
          server: share.server,
          scope: share.scope,
          members: share.members,
          name: facts.name,
          color,
          excluded: facts.archived,
        }),
      });
      const done = () => { laneFlight = false; };
      track(pending.then(done, done));
      return;
    }
  }

  function select(node) {
    if (destroyed) return;
    const shown = sheetNode;
    if (node !== selected) { view = null; hideControls(); }
    selected = node;
    if (shown && shown !== node) {
      sheetNode = "";
      closeSheets();
    }
    if (!node) { generation++; return; }
    return beginLoad(node);
  }

  function ensureSelected(node) {
    if (node !== selected) return select(node);
  }

  function tick() {
    if (destroyed) return;
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
    for (const member of drawnMembers) {
      if (!member || member.seat !== seatId) continue;
      if (member.access === "write") return "write";
      if (member.access === "read") return "read";
      return "";
    }
    return "";
  }

  function personRow(seat) {
    const checked = accessOf(seat.id) ? " checked" : "";
    return '<label><input type="checkbox" data-seat="' + esc(seat.id) + '"' + checked + '>' + esc(seat.name) + '</label>';
  }

  function activeView() {
    return sheetKind === "lane" ? laneView : view;
  }

  function paintTitle() {
    const title = $("#sh_title");
    if (!title) return;
    title.textContent = sheetKind === "lane" ? "Share lane" : "Share chat";
  }

  function countHTML() {
    if (sheetKind !== "lane") return "";
    const facts = laneFacts(laneTarget);
    return "<p>" + esc(laneCountLine(facts.open || 0, facts.archived.length)) + "</p>";
  }

  function renderNew(body) {
    body.innerHTML = serverFields() + scopeFields() + countHTML() + '<div id="sh_people">Loading people…</div>';
    setApply(sheetKind === "lane" ? "Share lane" : "Share chat", false, true);
    setStop(true);
    paintTitle();
  }

  function renderActive(body) {
    const current = activeView();
    const scope = current.scope === "all" ? "Entire history" : "From now on";
    body.innerHTML =
      "<p>" + esc(nameOf(current.server)) + "</p>" +
      "<p>" + scope + "</p>" +
      "<p>To change the server or the scope, stop sharing and share again.</p>" +
      countHTML() +
      '<div id="sh_people">Loading people…</div>' +
      "<p>Members keep what they already received.</p>";
    setApply("Save people", false, true);
    setStop(false);
    paintTitle();
  }

  function renderEnded(body) {
    const current = activeView();
    body.innerHTML =
      "<p>" + esc(shareStatusText(current, nameOf(current.server))) + "</p>" +
      "<p>Members keep what they already received.</p>";
    setApply("", true, true);
    setStop(false);
    paintTitle();
  }

  function renderStopping(body) {
    const current = activeView();
    body.innerHTML = "<p>" + esc(shareStatusText(current, nameOf(current.server))) + "</p>";
    setApply("", true, true);
    setStop(true);
    paintTitle();
  }

  function ownedLane() {
    for (const item of laneShares) {
      if (item && item.id === view.lane) return item;
    }
    return null;
  }

  function renderThrough(body) {
    const owned = ownedLane() || {};
    const facts = typeof laneFacts === "function" ? laneFacts(owned.lane) : null;
    const name = (facts && facts.name) || owned.name || "";
    body.innerHTML = "<p>Shared through lane " + esc(name) + "</p>" +
      '<button type="button" id="sh_lane">Open lane</button>';
    setApply("", true, true);
    setStop(true);
    paintTitle();
  }

  function renderSheet() {
    seatsGen += 1;
    seatsReady = false;
    const current = activeView();
    drawnMembers = current && Array.isArray(current.members) ? current.members.map(member => member && { ...member }) : [];
    armed = false;
    const body = $("#sh_body");
    if (sheetKind === "chat" && view && view.lane) renderThrough(body);
    else if (!current) renderNew(body);
    else if (current.state === "active") renderActive(body);
    else if (current.state === "lost" || current.state === "full") renderEnded(body);
    else renderStopping(body);
    rememberDrawn();
  }

  function shareToken(v) {
    if (!v) return "";
    return String(v.uid) + "\n" + String(v.generation) + "\n" + String(v.state) + "\n" + String(v.server) + "\n" + String(v.scope) + "\n" + String(v.shared_at);
  }

  function sheetNodeKey() {
    return sheetKind === "lane" ? laneTarget : selected;
  }

  function rememberDrawn() {
    drawnNode = sheetNodeKey();
    drawnShare = shareToken(activeView());
  }

  function wantsPeople() {
    if (sheetKind === "chat" && view && view.lane) return false;
    const current = activeView();
    if (!current) return true;
    return current.state === "active";
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
    const current = activeView();
    if (current) return current.server;
    return checkedValue("sh_server", firstServerId());
  }

  function chosenMembers() {
    const members = [];
    for (const input of $("#sh_body").querySelectorAll("input")) {
      if (input.type === "checkbox" && input.dataset.seat && input.checked) {
        members.push({ seat: input.dataset.seat, access: accessOf(input.dataset.seat) || "write" });
      }
    }
    return members;
  }

  function laneExtra() {
    if (sheetKind !== "lane") return null;
    const facts = laneFacts(laneTarget);
    return {
      name: facts.name,
      color: String(facts.color).toLowerCase(),
      excluded: facts.archived,
    };
  }

  function putBody() {
    const current = activeView();
    const extra = laneExtra() || {};
    if (!current) {
      return {
        server: checkedValue("sh_server", firstServerId()),
        scope: checkedValue("sh_scope", "from_now"),
        members: chosenMembers(),
        ...extra,
      };
    }
    const baseline = Array.from($("#sh_body").querySelectorAll("input"))
      .filter(input => input.type === "checkbox" && input.dataset.seat && accessOf(input.dataset.seat))
      .map(input => input.dataset.seat);
    return {
      server: current.server,
      scope: current.scope,
      members: chosenMembers(),
      membership_only: { uid: current.uid, generation: current.generation, baseline },
      ...extra,
    };
  }

  function chosenServerJoined() {
    const id = checkedValue("sh_server", firstServerId());
    for (const server of joined) {
      if (server.id === id) return true;
    }
    return false;
  }

  function sheetDrifted() {
    const current = activeView();
    if (drawnNode !== sheetNodeKey()) return true;
    if (shareToken(current) !== drawnShare) return true;
    if (current && JSON.stringify(current.members || []) !== JSON.stringify(drawnMembers)) return true;
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
    for (const input of $("#sh_body").querySelectorAll("input")) {
      if (input.type === "checkbox" && input.dataset.seat && input.checked !== Boolean(accessOf(input.dataset.seat))) return false;
    }
    return true;
  }

  function syncApply() {
    $("#sh_apply").disabled = !seatsReady || (Boolean(activeView()) && sameAsView());
  }

  function applyShouldDisable() {
    return $("#sh_apply").hidden || !seatsReady || (Boolean(activeView()) && sameAsView());
  }

  function loadSeats(serverId) {
    const gen = ++seatsGen;
    seatsReady = false;
    syncApply();
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
    if (!body || !Array.isArray(body.seats) || typeof body.self !== "string" || !body.self ||
        body.seats.some(seat => !seat || typeof seat.id !== "string" || !seat.id || typeof seat.name !== "string")) {
      host.textContent = shareErrorText("");
      syncApply();
      return;
    }
    const others = body.seats.filter(seat => seat.id !== body.self);
    if (drawnMembers.some(member => (member && (member.access === "read" || member.access === "write")) && !others.some(seat => seat.id === member.seat))) {
      host.textContent = shareErrorText("unknown_member");
      syncApply();
      return;
    }
    seatsReady = true;
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
    sheetKind = "chat";
    sheetNode = selected;
    showSheet();
    openSheet("#sharesheet");
  }

  function onSheetChange(ev) {
    const target = ev.target;
    if (target.name === "sh_server") {
      disarmStop();
      loadSeats(target.value);
      return;
    }
    if (target.type === "checkbox" && target.dataset.seat) syncApply();
  }

  function onSheetClick(ev) {
    if (ev.target.id === "sh_lane") {
      openLane(ownedLane().lane);
      return;
    }
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
    const node = selected;
    const kind = sheetKind;
    const pending = api(targetPath(), { method: "DELETE" });
    enqueue(() => finishDelete(pending, node, kind));
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
    const hadView = Boolean(activeView());
    const body = JSON.stringify(putBody());
    sheetBusy = true;
    apply.disabled = true;
    $("#sh_stop").disabled = true;
    const node = selected;
    const kind = sheetKind;
    const lane = laneTarget;
    const pending = api(targetPath(), {
      method: "PUT",
      body,
    });
    enqueue(() => finishPut(pending, hadView, restore, node, kind, lane));
  }

  function targetPath() {
    if (sheetKind === "lane") return "/api/team/lane-shares/" + laneTarget;
    return "/api/team/shares/" + selected;
  }

  async function finishPut(pending, hadView, restore, node, kind, lane) {
    let body = null;
    let err = null;
    try {
      body = await pending;
    } catch (e) {
      err = e;
    }
    sheetBusy = false;
    if (destroyed || node !== selected || (kind === "lane" && lane !== laneTarget)) return;
    if (err) {
      showNote(codeOf(err));
      $("#sh_stop").disabled = false;
      $("#sh_apply").disabled = applyShouldDisable();
      return;
    }
    if (kind === "lane") laneView = { ...body, uid: body.id };
    else {
      view = body;
      onChange(selected, view);
      paintHeader();
    }
    showSheet();
    $("#sh_live").textContent = hadView ? "People updated." : "Sharing started.";
    if (restore) $("#sh_apply").focus();
  }

  function markStopped() {
    view = null;
    onChange(selected, null);
    paintHeader();
    sheetNode = "";
    closeSheets();
    $("#sh_live").textContent = "Sharing stopped.";
  }

  async function finishDelete(pending, node, kind) {
    let body = null;
    let err = null;
    try {
      body = await pending;
    } catch (e) {
      err = e;
    }
    sheetBusy = false;
    if (destroyed || node !== selected) return;
    if (err) {
      if (kind === "lane" || err.status !== 404) showNote(codeOf(err));
      else markStopped();
    } else if (kind === "lane") {
      laneView = { ...body, uid: body.id };
      showSheet();
    } else if (typeof body === "object") {
      view = body;
      onChange(selected, view);
      paintHeader();
      showSheet();
    } else {
      markStopped();
    }
    $("#sh_stop").disabled = false;
    $("#sh_apply").disabled = applyShouldDisable();
  }

  function listen(el, type, fn) {
    el.addEventListener(type, fn);
    cleanups.push(() => el.removeEventListener(type, fn));
  }

  function bind() {
    if (destroyed) return;
    const btn = $("#sharebtn");
    btn.innerHTML = SHARE_ICON;
    listen(btn, "click", onOpen);
    listen($("#sharestatus"), "click", onOpen);
    listen($("#sharesheet"), "click", onSheetClick);
    listen($("#sharesheet"), "change", onSheetChange);
  }

  function destroy() {
    destroyed = true;
    generation++;
    seatsGen++;
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

  function showLane() {
    clearNote();
    const id = laneTarget;
    const ticket = ++laneTicket;
    const pending = api("/api/team/lane-shares/" + id);
    enqueue(async () => {
      let body = null;
      let err = null;
      try {
        body = await pending;
      } catch (e) {
        err = e;
      }
      if (ticket !== laneTicket || destroyed) return;
      if (err) {
        if (err.status === 404) laneView = null;
        else {
          showNote(codeOf(err));
          return;
        }
      } else laneView = { ...body, uid: body.id };
      showSheet();
    });
  }

  function openLane(id) {
    sheetKind = "lane";
    laneTarget = id;
    if (!sheetNode) sheetNode = selected;
    showLane();
    openSheet("#sharesheet");
  }

  function laneShare(id) {
    for (const share of laneShares) {
      if (share.lane === id) return share;
    }
    return null;
  }

  function serverName(id) {
    return nameOf(id);
  }

  return { select, ensureSelected, tick, bind, destroy, settled, openLane, laneShare, serverName };
}
