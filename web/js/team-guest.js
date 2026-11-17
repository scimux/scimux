/* Shared subscription list and read-only guest view. */
import { esc, md, stripAssetRefs, bubbleTitle } from "./format.js";
import { pinnedOrder } from "./map-model.js";
import { promptSourceKey } from "./storage.js";

const LIST = "/api/team/subscriptions";

export function guestKey(view) {
  return [view.server, view.owner, view.chat, view.generation].join("/");
}

export function guestName(server, seat, serverName, people = {}) {
  const local = people?.[server + "/" + seat];
  if (typeof local === "string" && local) return local;
  return serverName || "Unknown person";
}

export function guestCapture(entry, view, names, people = {}) {
  if (entry.omitted !== undefined) return null;
  if (!entry.text) return null;
  const comment = entry.t === undefined;
  if (!comment) {
    if (entry.t !== "user" && entry.t !== "assistant") return null;
  }
  let role = "user";
  if (entry.t === "assistant") role = "assistant";
  else if (comment) {
    if (!entry.mine) role = "person";
  } else if (view.access !== "owner") role = "person";
  const capture = { text: stripAssetRefs(entry.text), role };
  if (role === "assistant") {
    if (entry.agent) capture.agent = entry.agent;
  }
  if (role === "person") {
    if (comment) {
      capture.who = guestName(view.server, entry.author, names[entry.author], people);
      capture.seat = entry.author;
    } else {
      capture.who = guestName(view.server, view.owner, view.owner_name, people);
      capture.seat = view.owner;
    }
  }
  if (entry.prov === true) capture.prov = true;
  capture.from = view.title || "Untitled chat";
  capture.turnTime = comment ? entry.at : entry.time;
  capture.team = { server: view.server, owner: view.owner, chat: view.chat, generation: view.generation, log: comment ? "comments" : "mirror", seq: entry.seq };
  return capture;
}

function compare(a, b) {
  return a.localeCompare(b, undefined, { sensitivity: "base" });
}

export function guestGroups(subs, servers, people = {}) {
  const groups = new Map();
  for (const view of subs) {
    const mine = view.access === "owner";
    let key = view.server + "/" + view.owner;
    if (mine) key = view.server + "/@mine";
    if (!groups.has(key)) {
      let ownerName = guestName(view.server, view.owner, view.owner_name, people);
      let label = ownerName + " · " + view.server_name;
      if (mine) label = "Mine · " + view.server_name;
      const server = servers.find(item => item.id === view.server);
      if (server) {
        if (server.delivery === "offline") label += " · offline";
        if (server.delivery === "refused") label += " · access ended";
        if (server.delivery === "changed") label += " · not the same server";
      }
      groups.set(key, { key, label, ownerName, serverName: view.server_name, owner: view.owner, server: view.server, mine, items: [] });
    }
    groups.get(key).items.push(view);
  }
  const result = [...groups.values()];
  result.sort((a, b) => {
    if (a.mine !== b.mine) {
      if (a.mine) return -1;
      return 1;
    }
    if (a.mine) {
      const order = compare(a.serverName, b.serverName);
      if (order) return order;
      return a.server.localeCompare(b.server);
    }
    let order = compare(a.ownerName, b.ownerName);
    if (order) return order;
    order = compare(a.serverName, b.serverName);
    if (order) return order;
    order = a.owner.localeCompare(b.owner);
    if (order) return order;
    return a.server.localeCompare(b.server);
  });
  for (const group of result) {
    group.items.sort((a, b) => {
      if (a.state !== b.state) {
        if (a.state === "live") return -1;
        return 1;
      }
      const order = compare(a.title, b.title);
      if (order) return order;
      return a.first_seen.localeCompare(b.first_seen);
    });
  }
  return result;
}

export function createGuestFeature({ api, doc, now, invalidateCards, randomBytes, uiMutate, bookmarks, openSendTo, people = () => ({}), panel, storage = null, selectedNode, selectedUID, localAgentAvailable, confirmRemove }) {
  let off = false;
  let destroyed = false;
  let answer = { subscriptions: [], servers: [], truncated: false };
  let loading = false;
  let lastLoad = null;
  let refreshPending = false;
  let hasAnswer = false;
  const pending = new Set();
  let bound = false;
  let open = null;
  let owner = null;
  const ownerCopies = new Map();
  let actionOwner = null;
  let ownerShare = null;
  let readGeneration = 0;
  let knownPeople = {};

  function track(promise) {
    pending.add(promise);
    promise.finally(() => pending.delete(promise));
  }
  function tabHTML(active) {
    if (off) return "";
    const received = answer.subscriptions.filter(item => item.access !== "owner");
    if (!received.length) return "";
    let cls = "";
    if (active) cls = "on";
    const count = received.filter(item => item.unread).length;
    let badge = "";
    if (count) badge = `<span class="tabbadge" aria-label="${count} unread">${count}</span>`;
    return `<button data-tab="shared" class="${cls}" aria-pressed="${active}">Shared with me${badge}</button>`;
  }
  async function load() {
    try {
      const next = await api(LIST);
      if (destroyed) return;
      const wasOff = off;
      off = false;
      let changed = wasOff || JSON.stringify(next) !== JSON.stringify(answer);
      if (!hasAnswer) {
        if (!next.subscriptions.length) changed = false;
      }
      hasAnswer = true;
      answer = next;
      if (open && !next.subscriptions.some(view => guestKey(view) === open.key)) {
        readGeneration++;
        open.busy = false;
      }
      panel.changed();
      panel.loaded();
      reconcileOwner();
      if (refreshPeople() && next.subscriptions.length) changed = true;
      if (changed) invalidateCards();
    } catch (error) {
      if (destroyed) return;
      if (error.status === 404) {
        off = true;
        readGeneration++;
        if (open) open.busy = false;
        panel.changed();
        if (answer.subscriptions.length) invalidateCards();
      }
    } finally {
      loading = false;
      if (refreshPending && !destroyed) { refreshPending = false; refresh(); }
    }
  }
  function tick() {
    if (destroyed) return;
    const time = now();
    for (const copy of ownerCopies.values()) if (!copy.busy && time - copy.lastRead >= 5000) startOwnerRead(copy);
    if (open) {
      if (!open.busy) {
        if (!open.failed) {
          if (time - open.lastRead >= 5000) startRead();
        }
      }
    }
    if (loading) return;
    if (lastLoad !== null) {
      if (time - lastLoad < 10000) return;
    }
    lastLoad = time;
    loading = true;
    track(load());
  }
  function refresh() {
    if (loading) { refreshPending = true; return; }
    lastLoad = null;
    tick();
  }
  const $ = id => doc.querySelector("#" + id);
  function refreshPeople() {
    const next = { ...people() };
    const changed = JSON.stringify(next) !== JSON.stringify(knownPeople);
    knownPeople = next;
    if (open) {
      header(open.view);
      for (const node of $("msgs").querySelectorAll("[data-seat]")) {
        let serverName = open.names[node.dataset.seat];
        if (node.dataset.prompt !== undefined) serverName = open.view.owner_name;
        let name = guestName(open.view.server, node.dataset.seat, serverName, knownPeople);
        if (node.dataset.prompt !== undefined) {
          name += " · prompt";
          if (node.dataset.prov === "true") name += " · includes forwarded AI text";
        }
        node.textContent = name;
      }
      if (open.row) open.row.capture = guestCapture(open.row.entry, open.view, open.names, knownPeople);
    }
    return changed;
  }
  function header(view) {
    let title = view.title;
    if (!title) title = "Untitled chat";
    let owner = guestName(view.server, view.owner, view.owner_name, knownPeople);
    $("chattitle").textContent = title;
    $("chatmission").dataset.seat = view.owner;
    $("chatmission").textContent = "From " + owner + " · " + view.server_name;
    $("gueststate").textContent = guestStateText(view);
  }
  function openView(view) {
    readGeneration++;
    const key = guestKey(view);
    open = { key, view, next: 0, cnext: 0, first: true, seenAtOpen: view.seen_mirror, commentsAtOpen: view.seen_comments, seen: view.seen_mirror, commentsSeen: view.seen_comments, names: {}, entries: new Map(), row: null, lastRead: 0, busy: false, failed: false, empty: false, removing: false, removeNote: false };
    header(view);
    $("msgs").innerHTML = '<div class="previewtrunc">Loading…</div>';
    $("guestnote").textContent = "";
    $("guestremove").disabled = false;
    $("guestmore").hidden = true;
    startRead();
  }
  function close() {
    open = null;
    readGeneration++;
    $("msgs").innerHTML = "";
    invalidateCards();
  }
  function keydown(event) {
    if (event.key === "Escape" && event.target?.dataset?.grename !== undefined) {
      event.preventDefault(); event.stopPropagation(); restoreNameRow(); return;
    }
    if (event.key === "Enter" || event.key === " ") {
      if (!event.target) return;
      const bubble = event.target.closest(".bubble");
      if (bubble === event.target) {
        event.preventDefault();
        toggleAction(bubble);
      }
      return;
    }

  }
  function more() { startRead(); }
  function startRead() {
    if (off) return;
    if (!open) return;
    if (!answer.subscriptions.some(view => guestKey(view) === open.key)) return;
    if (open.busy) return;
    if (open.removing) return;
    if (open.failed) return;
    const current = open;
    current.lastRead = now();
    const gen = ++readGeneration;
    current.busy = gen;
    track(readRun(current, gen));
  }
  function currentRead(current, gen) {
    if (destroyed) return false;
    if (open !== current) return false;
    return gen === readGeneration;
  }
  function insertOrdered(body, html, key) {
    for (const element of body.children) {
      if (element.dataset.gactions !== undefined) continue;
      let other = [Number(element.dataset.seq), 0, 0];
      if (element.dataset.cseq !== undefined) other = [Number(element.dataset.anchor), 1, Number(element.dataset.cseq)];
      for (let i = 0; i < key.length; i++) {
        if (key[i] < other[i]) { element.insertAdjacentHTML("beforebegin", html); return; }
        if (key[i] > other[i]) break;
      }
    }
    body.insertAdjacentHTML("beforeend", html);
  }
  async function readRun(current, gen) {
    const body = $("msgs");
    const first = current.first;
    let hasEntries = false;
    let morePages = false;
    let moreComments = false;
    function insertUpdate(html, key) {
      const row = current.row;
      const editing = !!row?.renaming;
      const top = editing ? row.turn.getBoundingClientRect?.().top : undefined;
      const nearEnd = body.scrollHeight - body.scrollTop - body.clientHeight <= 80;
      insertOrdered(body, html, key);
      if (editing && current.row === row && row.renaming) {
        if (typeof top === "number") body.scrollTop += row.turn.getBoundingClientRect().top - top;
      } else if (!first && nearEnd) body.scrollTop = body.scrollHeight;
    }
    try {
      for (let n = 0; n < 40; n++) {
        const result = await api(LIST + "/" + current.key + "?after=" + current.next);
        if (!currentRead(current, gen)) return;
        current.view = result.view;
        panel.changed();
        if (refreshPeople()) invalidateCards();
        header(result.view);
        if (!current.removeNote) {
          $("guestnote").textContent = "";
        }
        if (current.first) { body.innerHTML = ""; current.first = false; }
        for (const entry of result.entries) {
          current.entries.set("mirror/" + entry.seq, entry);
          const rendered = guestEntryHTML(entry, result.view.owner_name, result.view.server, result.view.owner, knownPeople, result.view.access);
          if (rendered) {
            if (current.empty) { body.innerHTML = ""; current.empty = false; }
            insertUpdate(rendered, [entry.seq, 0, 0]);
            hasEntries = true;
          }
        }
        current.next = result.next;
        morePages = result.more;
        if (!morePages) break;
      }
      for (let n = 0; n < 40; n++) {
        const result = await api(LIST + "/" + current.key + "/comments?after=" + current.cnext);
        if (!currentRead(current, gen)) return;
        Object.assign(current.names, result.names);
        if (refreshPeople()) invalidateCards();
        for (const message of result.entries) {
          if (current.empty) { body.innerHTML = ""; current.empty = false; }
          insertUpdate(guestMessageHTML(message, current.names, current.view.server, knownPeople), [message.anchor, 1, message.seq]);
          current.entries.set("comments/" + message.seq, message);
          hasEntries = true;
        }
        current.cnext = result.next;
        moreComments = result.more;
        if (!moreComments) break;
      }
      if (first && !current.row?.renaming) {
        if (!hasEntries) {
          body.innerHTML = '<div class="previewtrunc">Nothing has been shared yet.</div>';
          current.empty = true;
        }
        let selector = "";
        for (const element of body.children) {
          if (element.dataset.seq !== undefined) {
            if (Number(element.dataset.seq) > current.seenAtOpen) { selector = `[data-seq="${element.dataset.seq}"]`; break; }
          } else if (element.dataset.cseq !== undefined) {
            if (Number(element.dataset.cseq) > current.commentsAtOpen) { selector = `[data-cseq="${element.dataset.cseq}"]`; break; }
          }
        }
        if (selector) {
          const unread = body.querySelector(selector);
          if (unread) unread.scrollIntoView({ block: "start" });
        } else body.scrollTop = body.scrollHeight;
      }
      $("guestmore").hidden = !morePages && !moreComments;
      const marks = {};
      if (current.next > current.seen) marks.mirror = current.next;
      if (current.cnext > current.commentsSeen) marks.comments = current.cnext;
      if (Object.keys(marks).length) await markSeen(current, gen, marks);
    } catch (error) {
      if (!currentRead(current, gen)) return;
      let code = "";
      try { code = JSON.parse(error.message).error; } catch {}
      if (!current.removeNote) {
        $("guestnote").textContent = guestErrorText(code);
      }
      if (code === "not_found") { current.failed = true; panel.changed(); }
    } finally {
      if (current.busy === gen) current.busy = false;
    }
  }
  async function markSeen(current, gen, marks) {
    try {
      await api(LIST + "/" + current.key + "/seen", { method: "PUT", body: JSON.stringify(marks) });
      if (!currentRead(current, gen)) return;
      if (marks.mirror !== undefined) current.seen = marks.mirror;
      if (marks.comments !== undefined) current.commentsSeen = marks.comments;
      // Removing the listing invalidates this generation before a seen reply can apply.
      const view = answer.subscriptions.find(item => guestKey(item) === current.key);
      const before = JSON.stringify(view);
      if (marks.mirror !== undefined) view.seen_mirror = marks.mirror;
      if (marks.comments !== undefined) view.seen_comments = marks.comments;
      view.unread = false;
      if (view.state === "live") {
        if (view.comments_last > view.seen_comments) view.unread = true;
        if (view.access !== "owner") {
          if (view.mirror_last > view.seen_mirror) view.unread = true;
        }
      }
      if (JSON.stringify(view) !== before) invalidateCards();
    } catch {}
  }
  function removeClick() {
    if (!open || open.removing) return;
    const current = open;
    if (!confirmRemove("Leave this shared chat and remove its saved copy from this computer? The owner's chat will not be deleted.")) {
      $("guestremove").focus();
      return;
    }
    if (open !== current) return;
    current.removing = true;
    current.removeNote = true;
    $("guestremove").disabled = true;
    track(remove(current));
  }
  function actionView() { return open || (actionOwner && ownerCurrent(actionOwner) ? actionOwner : owner); }
  function sharedClick(event) {
    if (!open && event.target.closest) {
      const turn = event.target.closest("[data-cseq]");
      if (turn?.dataset.gkey) {
        actionOwner = ownerCopies.get(turn.dataset.gkey) || null;
        if (!actionOwner) return;
      }
    }
    const current = actionView();
    if (!current) return;
    if (!event.target.closest) return;
    const action = event.target.closest("[data-gact]");
    if (action) { runAction(action.dataset.gact); return; }
    if (event.target.closest("a") || event.target.closest("[data-bmarker]")) return;
    const bubble = event.target.closest(".bubble");
    if (bubble) toggleAction(bubble);
  }
  function toggleAction(bubble) {
    const turn = bubble.closest("[data-seq], [data-cseq]");
    if (!turn) return;
    if (!open && turn.dataset.gkey) {
      actionOwner = ownerCopies.get(turn.dataset.gkey) || null;
      if (!actionOwner) return;
    }
    const current = actionView();
    if (!current) return;
    let key = "mirror/" + turn.dataset.seq;
    if (turn.dataset.cseq !== undefined) key = "comments/" + turn.dataset.cseq;
    const entry = current.entries.get(key);
    if (!entry) return;
    const capture = guestCapture(entry, current.view, current.names, knownPeople);
    if (!capture) return;
    const previous = current.row;
    if (previous) {
      previous.turn.nextElementSibling?.remove();
      current.row = null;
      if (previous.turn === turn) return;
    }
    turn.insertAdjacentHTML("afterend", '<div class="bubactions" data-gactions>' + actionButtons(capture) + '</div>');
    current.row = { turn, capture, entry, renaming: false };
  }
  function actionButtons(capture) {
    let buttons = '<button data-gact="bookmark">bookmark</button><button data-gact="sendto">send to&#8230;</button>';
    if (actionView() && !open && canPreparePrompt(capture.team)) buttons += '<button data-gact="prompt"><span class="rot90r">' + panel.promptIcon + '</span>use as prompt</button>';
    if (capture.role === "person") buttons += '<button data-gact="rename">rename person</button>';
    return buttons;
  }
  function restoreNameRow() {
    const current = actionView();
    if (!current) return;
    if (!current.row) return;
    if (!current.row.renaming) return;
    const row = current.row.turn.nextElementSibling;
    current.row.renaming = false;
    row.innerHTML = actionButtons(current.row.capture);
    row.querySelector('[data-gact="rename"]').focus();
  }
  function editPerson() {
    const current = actionView();
    if (current.row.capture.role !== "person") return;
    const row = current.row.turn.nextElementSibling;
    current.row.renaming = true;
    row.innerHTML = '<input data-grename maxlength="64" aria-label="Name for this person" value="' + esc(current.row.capture.who) + '">' +
      '<button data-gact="name-save">Save</button><button data-gact="name-use">Use their name</button>';
    row.querySelector("[data-grename]").focus();
  }
  function savePerson(useServerName) {
    const current = actionView();
    if (!current.row.renaming) return;
    const row = current.row.turn.nextElementSibling;
    const entry = current.row.entry;
    const seat = current.row.capture.seat;
    let serverName = current.names[seat];
    if (entry.t === "user") serverName = current.view.owner_name;
    let name = row.querySelector("[data-grename]").value.trim();
    if (useServerName || name === serverName) name = "";
    uiMutate({ k: "person-name", key: current.view.server + "/" + seat, name });
    refreshPeople();
    invalidateCards();
    restoreNameRow();
  }
  function runAction(action) {
    const current = actionView();
    if (!current.row) return;
    const capture = current.row.capture;
    if (action === "prompt") {
      if (!open && ownerCurrent(current) && canPreparePrompt(capture.team)) {
        const entry = current.row.entry;
        panel.preparePrompt({...capture, role:"person", who:guestName(current.view.server, entry.author, current.names[entry.author], knownPeople)}, capture.team);
      }
      return;
    }
    if (action === "rename") { editPerson(); return; }
    if (action === "name-save") { savePerson(false); return; }
    if (action === "name-use") { savePerson(true); return; }
    if (action === "bookmark") {
      const keys = ["server", "owner", "chat", "generation", "log", "seq"];
      const duplicate = bookmarks().some(item => item.team && keys.every(key => item.team[key] === capture.team[key]));
      if (duplicate) { $("guestnote").textContent = "Already bookmarked."; return; }
      uiMutate({ k: "bookmark-add", bookmark: { t: new Date(now()).toISOString(), ...capture } });
      $("guestnote").textContent = "Bookmarked.";
    }
    if (action === "sendto") {
      openSendTo({ text: capture.text, exceptId: "", title: "Send to…", role: capture.role, who: capture.who, prov: capture.prov, source: null });
    }
  }
  async function remove(current) {
    try {
      await api(LIST + "/" + current.key, { method: "DELETE" });
    } catch (error) {
      if (error.status !== 404) {
        if (open !== current) return;
        current.removing = false;
        current.removeNote = true;
        $("guestremove").disabled = false;
        let code = "";
        try { code = JSON.parse(error.message).error; } catch {}
        let text = "That did not work. Try again.";
        if (code === "unreachable") text = "The server could not be reached. Try again later.";
        $("guestnote").textContent = text;
        $("guestremove").focus();
        return;
      }
    }
    if (destroyed) return;
    answer.subscriptions = answer.subscriptions.filter(view => guestKey(view) !== current.key);
    invalidateCards();
    if (open !== current) return;
    open = null;
    readGeneration++;
    panel.removed();
  }
  function panelKeydown(event) {
    if (event.key === "Escape" && event.target?.dataset?.grename === undefined) return;
    keydown(event);
  }
  function bind() {
    if (bound) return;
    bound = true;
    $("guestmore").addEventListener("click", more);
    $("msgs").addEventListener("keydown", panelKeydown);
    $("msgs").addEventListener("click", sharedClick);
    $("guestremove").addEventListener("click", removeClick);
  }
  function destroy() {
    if (bound) {
      $("guestmore").removeEventListener("click", more);
      $("msgs").removeEventListener("keydown", panelKeydown);
      $("msgs").removeEventListener("click", sharedClick);
      $("guestremove").removeEventListener("click", removeClick);
    }
    open = null; owner = null; ownerShare = null; ownerCopies.clear(); actionOwner = null;
    readGeneration++;
    bound = false;
    destroyed = true;
  }
  async function settled() {
    while (pending.size) await Promise.all([...pending]);
  }
  function activities() { return answer.subscriptions.filter(view => view.access !== "owner"); }
  function mineUnread() { return answer.subscriptions.some(view => view.access === "owner" && view.unread); }
  function groups() { return sharedGroups(); }
  function sharedGroups() { return guestGroups(activities(), answer.servers, knownPeople); }
  function openActivity(id) {
    const view = activities().find(item => "team/" + guestKey(item) === id);
    if (!view) return;
    panel.select(id);
    invalidateCards();
  }
  function selectActivity(id) {
    if (open && "team/" + open.key === id) return;
    if (open) close();
    const view = activities().find(item => "team/" + guestKey(item) === id);
    if (view) openView(view);
  }
  function localIdentityChanged() { reconcileOwner(); }
  function setOwnerShare(node, share) {
    ownerShare = share && share.node === node ? {node, ...share} : null;
    reconcileOwner();
  }
  function reconcileOwner() {
    const share = ownerShare;
    const node = selectedNode();
    const uid = selectedUID();
    const relevant = answer.subscriptions.filter(v => !!node && !!uid && v.access === "owner" && v.chat === uid);
    const keep = new Set(relevant.map(guestKey));
    let changed = false;
    for (const [key, item] of ownerCopies) if (item.node !== node || !keep.has(key)) { ownerCopies.delete(key); changed = true; }
    let active = null;
    for (const view of relevant) {
      const key = guestKey(view);
      let current = ownerCopies.get(key);
      if (!current) {
        current = {key, node, view, order: view.local_order, share: null, next:0, cnext:0, mirrors:[], comments:[], entries:new Map(), row:null, names:{}, busy:false, lastRead:0};
        ownerCopies.set(key,current); changed = true;
        startOwnerRead(current);
      } else { current.view = {...view, delivery: current.view.delivery}; current.order = view.local_order; }
      if (share && share.node === node && view.chat === share.uid && view.server === share.server && view.generation === share.generation && share.state === "active") { current.share = share; active = current; }
      else current.share = null;
    }
    if (owner !== active) { owner = active; changed = true; }
    if (changed) panel.ownerChanged();
  }
  function startOwnerRead(current = owner) {
    if (!current || current.busy || destroyed) return;
    current.busy = true; current.lastRead = now();
    track(readOwner(current));
  }
  function ownerCurrent(current) { return !destroyed && selectedNode() === current.node && ownerCopies.get(current.key) === current; }
  function ownerRenderSignature(current) {
    const view = current.view;
    return JSON.stringify([view.title, view.owner_name, view.access, view.state, view.delivery, current.mirrors, current.comments, current.names]);
  }
  async function readOwner(current) {
    const before = ownerRenderSignature(current);
    let painted = false;
    try {
      for (let n = 0; n < 40; n++) {
        const page = await api(LIST + "/" + current.key + "?after=" + current.next);
        if (!ownerCurrent(current)) return;
        current.view = page.view; current.mirrors.push(...page.entries); current.next = page.next;
        if (!page.more) break;
      }
      for (let n = 0; n < 40; n++) {
        const page = await api(LIST + "/" + current.key + "/comments?after=" + current.cnext);
        if (!ownerCurrent(current)) return;
        Object.assign(current.names, page.names); current.comments.push(...page.entries);
        for (const entry of page.entries) current.entries.set("comments/" + entry.seq, entry);
        current.cnext = page.next;
        if (!page.more) break;
      }
      if (before !== ownerRenderSignature(current)) { panel.ownerChanged(); painted = true; }
      if (current.cnext > current.view.seen_comments) {
        await api(LIST + "/" + current.key + "/seen", {method:"PUT",body:JSON.stringify({comments:current.cnext})});
        if (ownerCurrent(current)) {
          current.view.seen_comments = current.cnext; current.view.unread = false;
          const listed = answer.subscriptions.find(view => guestKey(view) === current.key);
          if (listed) { listed.seen_comments = current.cnext; listed.unread = listed.comments_last > current.cnext; }
          invalidateCards();
        }
      }
    } catch {
      if (!painted && ownerCurrent(current) && before !== ownerRenderSignature(current)) panel.ownerChanged();
    } finally { if (ownerCurrent(current)) current.busy = false; }
  }
  function ownerSnapshots() {
    return [...ownerCopies.values()].filter(copy => ownerCurrent(copy)).sort((a,b) => (Number.isInteger(a.order) ? a.order : Infinity) - (Number.isInteger(b.order) ? b.order : Infinity) || a.key.localeCompare(b.key)).map(copy => {
      const frontier = copy.view.local_frontier;
      const missingStart = !copy.mirrors.some(entry => entry.t === "start");
      const mirrors = frontier && missingStart ? [{seq:0,t:"start",local_segment:frontier.segment,local_record:frontier.record}, ...copy.mirrors] : copy.mirrors;
      return { uid:copy.view.chat, key:copy.key, view:copy.view, mirrors, comments:copy.comments, names:copy.names, people:knownPeople };
    });
  }
  function canPreparePrompt(source) {
    if (!promptSourceKey(source) || !selectedNode() || selectedNode().startsWith("team/") || !localAgentAvailable()) return false;
    const uid = selectedUID();
    if (!uid) return false;
    const copies = [...ownerCopies.values()].filter(copy => ownerCurrent(copy) && copy.view.chat === uid && copy.view.access === "owner");
    if (source.uid) return source.uid === uid && copies.length > 0;
    return copies.some(copy => copy.view.server === source.server && copy.view.owner === source.owner &&
      copy.view.chat === source.chat && copy.view.generation === source.generation &&
      source.log === "comments" && copy.entries.has("comments/" + source.seq));
  }
  function ownerSnapshot() {
    return ownerSnapshots().find(copy => owner && copy.key === owner.key) || null;
  }
  function peopleContext() {
    const current = open || (owner && selectedNode() === owner.share.node ? owner : null);
    if (!current) return null;
    const view = current.view;
    const listed = answer.subscriptions.find(item => guestKey(item) === current.key);
    const writable = item => item && item.state === "live" && (item.access === "write" || item.access === "owner");
    return { key: current.key, owner: current === owner, writable: !off && !current.failed && !!writable(view) && !!writable(listed) && (!current.share || current.share.state === "active") };
  }
  async function sendPeople(key, text) {
    const context = peopleContext();
    const current = open || (context?.owner ? owner : null);
    if (!current || !context || context.key !== key || !context.writable || !text.trim() || new TextEncoder().encode(text).length > 16384) throw new Error("Comments are unavailable");
    const storageKey = "scimux-comment-retry:" + key;
    let pending = null;
    try { pending = JSON.parse(storage?.getItem(storageKey) || "null"); } catch {}
    if (!pending || pending.text !== text || typeof pending.key !== "string") {
      pending = { text, anchor: current.next, key: guestHexKey(randomBytes(8)) };
      storage?.setItem(storageKey, JSON.stringify(pending));
    }
    const result = await api(LIST + "/" + key + "/comments", { method: "POST", body: JSON.stringify(pending) });
    if (!Number.isSafeInteger(result?.seq) || result.seq <= 0) throw new Error("Comment delivery is unconfirmed");
    if (storage?.getItem(storageKey) === JSON.stringify(pending)) storage.removeItem(storageKey);
    if (open === current) { current.lastRead = 0; startRead(); }
    if (owner === current) { current.lastRead = 0; startOwnerRead(); }
    return result;
  }
  function selectedActivity() { return open ? "team/" + open.key : ""; }
  return { tick, refresh, bind, destroy, settled, selectActivity, setOwnerShare, localIdentityChanged, ownerSnapshot, ownerSnapshots, canPreparePrompt, peopleContext, sendPeople, loaded: () => hasAnswer, hasActivity: id => activities().some(view => "team/" + guestKey(view) === id), tab: { tabHTML, activities, groups, mineUnread, open: openActivity, selected: selectedActivity } };
}

export function guestStateText(view) {
  if (view.state === "ended") return "Sharing ended · saved copy";
  if (view.delivery === "refused") return "The server ended this computer's access · saved copy";
  if (view.delivery === "changed") return "Not the same server · saved copy";
  let text = "Can view";
  if (view.access === "write") text = "Can comment";
  if (view.access === "owner") text = "Shared by you";
  if (view.delivery === "offline") text += " · offline, showing the saved copy";
  return text;
}
export function guestErrorText(code) {
  if (code === "not_found") return "This chat is no longer in your list.";
  if (code === "copy_damaged") return "The saved copy of this chat is damaged and cannot be shown.";
  if (code === "storage_full") return "This computer's storage is full. Free some space to load more.";
  return "That did not work. It will be tried again.";
}
export function guestMessageHTML(entry, names, server = "", people = {}, ownerKey = "", markers = "") {
  let cls = "turn assistant gmsg";
  let who = guestName(server, entry.author, names[entry.author], people);
  let name = `<div class="gwho" data-seat="${esc(entry.author)}">${esc(who)}</div>`;
  if (entry.mine) { cls = "turn user gmsg"; name = `<div class="gwho">You · People</div>`; }
  let tab = "";
  if (entry.text) tab = ' tabindex="0"';
  return `<div class="${cls}" data-cseq="${esc(entry.seq)}" data-anchor="${esc(entry.anchor)}"${ownerKey ? ` data-gkey="${esc(ownerKey)}"` : ""}><div class="bubble" title="${esc(entry.at)}"${tab}>${markers}${name}${md(entry.text)}</div></div>`;
}
export function guestHexKey(bytes) {
  return Array.from(bytes, byte => byte.toString(16).padStart(2, "0")).join("");
}
export function guestEntryHTML(entry, ownerName, server = "", seat = "", people = {}, access = "") {
  let line = "";
  if (entry.t === "start") {
    line = "Shared with its history.";
    if (entry.scope === "from_now") line = "Sharing started here. Earlier messages are not shared.";
  } else if (entry.t === "source") {
    line = "The conversation continued in a new session.";
    if (entry.reason === "clear") line = "The owner cleared the conversation.";
  } else if (entry.t === "user" || entry.t === "assistant") {
    let who = ownerName;
    if (!who) who = "Unknown person";
    let seatAttrs = "";
    if (entry.t === "user") {
      if (access !== "owner") {
        who = guestName(server, seat, ownerName, people);
        seatAttrs = ` data-seat="${esc(seat)}" data-prompt="1" data-prov="${entry.prov === true}"`;
      }
      who += " · prompt";
      if (entry.prov === true) who += " · includes forwarded AI text";
    } else {
      who = entry.agent;
      if (!who) who = "Agent";
    }
    let content = "This message was too large to share.";
    if (entry.omitted === undefined) content = md(stripAssetRefs(entry.text));
    let tab = "";
    if (entry.omitted === undefined) { if (entry.text) tab = ' tabindex="0"'; }
    return `<div class="turn assistant" data-seq="${esc(entry.seq)}"><div class="bubble" title="${esc(bubbleTitle(entry.t, entry.time))}"${tab}><div class="gwho"${seatAttrs}>${esc(who)}</div>${content}</div></div>`;
  } else return "";
  return `<div class="previewtrunc" data-seq="${esc(entry.seq)}">${line}</div>`;
}

export const ACTIVITY_FOLDS_KEY = "scimux-activity-folds";
export const SHARED_ACTIVITY_PREFIX = "team/";
export function sharedActivityId(view) { return SHARED_ACTIVITY_PREFIX + guestKey(view); }
export function isSharedActivityId(id) { return typeof id === "string" && id.startsWith(SHARED_ACTIVITY_PREFIX); }
export function sharedActivityGroups(views, servers, people) {
  return guestGroups((views || []).filter(view => view.access !== "owner"), servers || [], people || {});
}
export function sharedPinnedOrder(nodes, views, pinned) {
  const shares = (views || []).filter(view => view.access !== "owner").map(view => ({id:sharedActivityId(view), shared:view}));
  return pinnedOrder([...(nodes || []), ...shares].filter(item => (pinned || []).includes(item.id)), pinned);
}
export function readActivityFolds(storage) {
  try {
    const values = JSON.parse(storage?.getItem(ACTIVITY_FOLDS_KEY) || "[]");
    return new Set(Array.isArray(values) ? values.filter(value => typeof value === "string") : []);
  } catch { return new Set(); }
}
export function toggleActivityFold(storage, folds, key) {
  const next = new Set(folds);
  if (next.has(key)) next.delete(key); else next.add(key);
  try { storage?.setItem(ACTIVITY_FOLDS_KEY, JSON.stringify([...next])); } catch { /* memory still works */ }
  return next;
}

/* Resolve the anchor through the owner's mirror identity, never message text. */
export function ownerCommentAnchors(uid, turns, mirrors, comments, emptySegment = null) {
  const result = [];
  for (const comment of comments) {
    let anchor = null;
    for (const entry of mirrors) {
      if (entry.seq <= comment.anchor && Number.isInteger(entry.local_segment) && Number.isInteger(entry.local_record) && (!anchor || entry.seq > anchor.seq)) anchor = entry;
    }
    if (!anchor && comment.anchor === 0) anchor = mirrors.find(entry => entry.t === "start" && Number.isInteger(entry.local_segment) && Number.isInteger(entry.local_record));
    if (!anchor) continue;
    let position = -1, segmentFound = false;
    const segment = anchor.local_segment + (anchor.t === "source" ? 1 : 0);
    for (let i = 0; i < turns.length; i++) {
      const turn = turns[i];
      if (turn.uid !== uid || (turn.segment || 0) !== segment) continue;
      segmentFound = true;
      if ((turn.record || 0) <= anchor.local_record) position = i;
    }
    if (segmentFound || (!turns.length && emptySegment?.uid === uid && emptySegment.segment === segment)) result.push({turn: position, comment});
  }
  return result;
}

/* Old retained copies can predate saved start frontiers; never invent one. */
export function ownerUnplacedComments(mirrors, comments) {
  return comments.filter(comment => !mirrors.some(entry =>
    (entry.seq <= comment.anchor || (comment.anchor === 0 && entry.t === "start")) &&
    Number.isInteger(entry.local_segment) && Number.isInteger(entry.local_record)));
}
