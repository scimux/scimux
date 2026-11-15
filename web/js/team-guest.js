/* Shared subscription list and read-only guest view. */
import { esc, md, stripAssetRefs, bubbleTitle } from "./format.js";

const LIST = "/api/team/subscriptions";

export function guestKey(view) {
  return [view.server, view.owner, view.chat, view.generation].join("/");
}

function compare(a, b) {
  return a.localeCompare(b, undefined, { sensitivity: "base" });
}

export function guestGroups(subs, servers) {
  const groups = new Map();
  for (const view of subs) {
    const mine = view.access === "owner";
    let key = view.server + "/" + view.owner;
    if (mine) key = view.server + "/@mine";
    if (!groups.has(key)) {
      let ownerName = view.owner_name;
      if (!ownerName) ownerName = "Unknown person";
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

export function createGuestFeature({ api, doc, now, invalidateCards }) {
  let off = false;
  let destroyed = false;
  let answer = { subscriptions: [], servers: [], truncated: false };
  let loading = false;
  let lastLoad = null;
  let hasAnswer = false;
  const pending = new Set();
  const collapsed = new Set();
  let rendered = "";
  let renderedDOM = "";
  let listEl = null;
  let bound = false;
  let open = null;
  let readGeneration = 0;

  function track(promise) {
    pending.add(promise);
    promise.finally(() => pending.delete(promise));
  }
  function tabHTML(active) {
    if (off) return "";
    if (!answer.subscriptions.length) return "";
    let cls = "";
    if (active) cls = "on";
    const count = answer.subscriptions.filter(item => item.unread).length;
    let badge = "";
    if (count) badge = `<span class="tabbadge" aria-label="${count} unread">${count}</span>`;
    return `<button data-tab="shared" class="${cls}">Shared${badge}</button>`;
  }
  function html() {
    let out = '<div class="glist">';
    for (const group of guestGroups(answer.subscriptions, answer.servers)) {
      const hidden = collapsed.has(group.key);
      out += `<button class="ghead" data-group="${esc(group.key)}" aria-expanded="${!hidden}">${esc(group.label)}</button>`;
      if (hidden) continue;
      for (const item of group.items) {
        let title = item.title;
        if (!title) title = "Untitled chat";
        let state = "Can view";
        if (item.access === "write") state = "Can comment";
        if (item.access === "owner") state = "Shared by you";
        if (item.state === "ended") state = "Sharing ended";
        let cls = "gitem";
        let unread = "";
        if (item.unread) { cls += " unread"; unread = '<span class="sr-only">unread</span>'; }
        out += `<button class="${cls}" data-sub="${esc(guestKey(item))}"><span>${esc(title)}</span><span>${state}</span>${unread}</button>`;
      }
    }
    if (answer.truncated) out += '<div>Some shared chats are not listed.</div>';
    return out + "</div>";
  }
  function render(el) {
    if (!tabHTML(false)) return false;
    listEl = el;
    const content = html();
    if (content === rendered) {
      if (el.innerHTML === renderedDOM) return true;
    }
    let selector = "";
    const focus = doc.activeElement;
    if (focus) {
      if (el.contains(focus)) {
        if (focus.dataset.group) selector = `[data-group="${focus.dataset.group}"]`;
        if (focus.dataset.sub) selector = `[data-sub="${focus.dataset.sub}"]`;
      }
    }
    el.innerHTML = content;
    rendered = content;
    renderedDOM = el.innerHTML;
    if (selector) {
      const replacement = el.querySelector(selector);
      if (replacement) replacement.focus();
    }
    return true;
  }
  async function load() {
    try {
      const next = await api(LIST);
      if (destroyed) return;
      let changed = JSON.stringify(next) !== JSON.stringify(answer);
      if (!hasAnswer) {
        if (!next.subscriptions.length) changed = false;
      }
      hasAnswer = true;
      answer = next;
      if (changed) invalidateCards();
    } catch (error) {
      if (destroyed) return;
      if (error.status === 404) {
        off = true;
        readGeneration++;
        if (answer.subscriptions.length) invalidateCards();
      }
    } finally {
      loading = false;
    }
  }
  function tick() {
    if (destroyed) return;
    if (off) return;
    const time = now();
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
  function click(event) {
    const group = event.target.closest("[data-group]");
    if (group) {
      const key = group.dataset.group;
      if (collapsed.has(key)) collapsed.delete(key);
      else collapsed.add(key);
      if (listEl) render(listEl);
      return;
    }
    const item = event.target.closest("[data-sub]");
    if (!item) return;
    const view = answer.subscriptions.find(v => guestKey(v) === item.dataset.sub);
    if (view) openView(view);
  }
  const $ = id => doc.querySelector("#" + id);
  function header(view) {
    let title = view.title;
    if (!title) title = "Untitled chat";
    let owner = view.owner_name;
    if (!owner) owner = "Unknown person";
    $("sv_title").textContent = title;
    $("sv_from").textContent = "From " + owner + " · " + view.server_name;
    $("sv_state").textContent = guestStateText(view);
  }
  function openView(view) {
    readGeneration++;
    open = { key: guestKey(view), view, next: 0, cnext: 0, first: true, seenAtOpen: view.seen_mirror, commentsAtOpen: view.seen_comments, seen: view.seen_mirror, commentsSeen: view.seen_comments, names: {}, lastRead: 0, busy: false, failed: false, empty: false, armed: false, removing: false, removeNote: false };
    header(view);
    $("sv_body").innerHTML = '<div class="previewtrunc">Loading…</div>';
    $("sv_note").textContent = "";
    $("sv_remove").textContent = "Remove from my list";
    $("sv_remove").disabled = false;
    $("sv_more").hidden = true;
    $("sharedview").hidden = false;
    $("sv_back").focus();
    startRead();
  }
  function close() {
    if (!open) return;
    const key = open.key;
    open = null;
    readGeneration++;
    $("sharedview").hidden = true;
    $("sv_body").innerHTML = "";
    let focus = $("cardlist").querySelector(`[data-sub="${key}"]`);
    if (!focus) focus = $("cardtabs").querySelector('[data-tab="shared"]');
    if (focus) focus.focus();
  }
  function keydown(event) {
    if (event.key !== "Escape") return;
    event.stopPropagation();
    if (open) {
      if (open.armed) { disarm(open); return; }
    }
    close();
  }
  function more() { startRead(); }
  function startRead() {
    if (off) return;
    if (!open) return;
    if (open.busy) return;
    if (open.removing) return;
    if (open.failed) return;
    const current = open;
    current.busy = true;
    current.lastRead = now();
    const gen = ++readGeneration;
    track(readRun(current, gen));
  }
  function currentRead(current, gen) {
    if (destroyed) return false;
    if (open !== current) return false;
    return gen === readGeneration;
  }
  function insertOrdered(body, html, key) {
    for (const element of body.children) {
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
    const body = $("sv_body");
    const first = current.first;
    let hasEntries = false;
    let morePages = false;
    let moreComments = false;
    const nearEnd = body.scrollHeight - body.scrollTop - body.clientHeight <= 80;
    try {
      for (let n = 0; n < 40; n++) {
        const result = await api(LIST + "/" + current.key + "?after=" + current.next);
        if (!currentRead(current, gen)) return;
        current.view = result.view;
        header(result.view);
        if (!current.removeNote) $("sv_note").textContent = "";
        if (current.first) { body.innerHTML = ""; current.first = false; }
        for (const entry of result.entries) {
          const rendered = guestEntryHTML(entry, result.view.owner_name);
          if (rendered) {
            if (current.empty) { body.innerHTML = ""; current.empty = false; }
            insertOrdered(body, rendered, [entry.seq, 0, 0]);
            hasEntries = true;
          }
        }
        current.next = result.next;
        morePages = result.more;
        if (!morePages) break;
      }
      const messages = [];
      for (let n = 0; n < 40; n++) {
        const result = await api(LIST + "/" + current.key + "/comments?after=" + current.cnext);
        if (!currentRead(current, gen)) return;
        Object.assign(current.names, result.names);
        messages.push(...result.entries);
        current.cnext = result.next;
        moreComments = result.more;
        if (!moreComments) break;
      }
      for (const message of messages) {
        if (current.empty) { body.innerHTML = ""; current.empty = false; }
        insertOrdered(body, guestMessageHTML(message, current.names), [message.anchor, 1, message.seq]);
        hasEntries = true;
      }
      if (first) {
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
      } else if (nearEnd) body.scrollTop = body.scrollHeight;
      $("sv_more").hidden = !morePages && !moreComments;
      const marks = {};
      if (current.next > current.seen) marks.mirror = current.next;
      if (current.cnext > current.commentsSeen) marks.comments = current.cnext;
      if (Object.keys(marks).length) await markSeen(current, gen, marks);
    } catch (error) {
      if (!currentRead(current, gen)) return;
      let code = "";
      try { code = JSON.parse(error.message).error; } catch {}
      if (!current.removeNote) $("sv_note").textContent = guestErrorText(code);
      if (code === "not_found") current.failed = true;
    } finally {
      if (currentRead(current, gen)) current.busy = false;
    }
  }
  async function markSeen(current, gen, marks) {
    try {
      await api(LIST + "/" + current.key + "/seen", { method: "PUT", body: JSON.stringify(marks) });
      if (!currentRead(current, gen)) return;
      if (marks.mirror !== undefined) current.seen = marks.mirror;
      if (marks.comments !== undefined) current.commentsSeen = marks.comments;
      const view = answer.subscriptions.find(item => guestKey(item) === current.key);
      if (!view) return;
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
  function disarm(current) {
    current.armed = false;
    current.removeNote = false;
    $("sv_remove").textContent = "Remove from my list";
    $("sv_note").textContent = "";
  }
  function removeClick() {
    if (!open) return;
    if (open.removing) return;
    if (!open.armed) {
      open.armed = true;
      open.removeNote = true;
      $("sv_remove").textContent = "Confirm remove";
      $("sv_note").textContent = "This leaves the chat and deletes the saved copy from this computer.";
      return;
    }
    const current = open;
    current.armed = false;
    current.removing = true;
    $("sv_remove").disabled = true;
    track(remove(current));
  }
  function sharedClick(event) {
    if (!open) return;
    if (event.target === $("sv_remove")) return;
    if (open.armed) disarm(open);
  }
  async function remove(current) {
    try {
      await api(LIST + "/" + current.key, { method: "DELETE" });
    } catch (error) {
      if (error.status !== 404) {
        if (open !== current) return;
        current.removing = false;
        current.armed = false;
        current.removeNote = true;
        $("sv_remove").disabled = false;
        $("sv_remove").textContent = "Remove from my list";
        let code = "";
        try { code = JSON.parse(error.message).error; } catch {}
        let text = "That did not work. Try again.";
        if (code === "unreachable") text = "The server could not be reached. Try again later.";
        $("sv_note").textContent = text;
        return;
      }
    }
    if (destroyed) return;
    answer.subscriptions = answer.subscriptions.filter(view => guestKey(view) !== current.key);
    invalidateCards();
    if (open !== current) return;
    open = null;
    readGeneration++;
    $("sharedview").hidden = true;
    $("sv_body").innerHTML = "";
    let focus = null;
    if (tabHTML(false)) focus = $("cardtabs").querySelector('[data-tab="shared"]');
    if (!focus) focus = $("cardlist");
    focus.focus();
  }
  function bind() {
    if (bound) return;
    bound = true;
    doc.querySelector("#cardlist").addEventListener("click", click);
    doc.querySelector("#sv_back").addEventListener("click", close);
    doc.querySelector("#sv_scrim").addEventListener("click", close);
    doc.querySelector("#sv_more").addEventListener("click", more);
    doc.querySelector("#sharedview").addEventListener("keydown", keydown);
    doc.querySelector("#sharedview").addEventListener("click", sharedClick);
    doc.querySelector("#sv_remove").addEventListener("click", removeClick);
  }
  function destroy() {
    if (bound) {
      doc.querySelector("#cardlist").removeEventListener("click", click);
      doc.querySelector("#sv_back").removeEventListener("click", close);
      doc.querySelector("#sv_scrim").removeEventListener("click", close);
      doc.querySelector("#sv_more").removeEventListener("click", more);
      doc.querySelector("#sharedview").removeEventListener("keydown", keydown);
      doc.querySelector("#sharedview").removeEventListener("click", sharedClick);
      doc.querySelector("#sv_remove").removeEventListener("click", removeClick);
    }
    open = null;
    readGeneration++;
    bound = false;
    destroyed = true;
  }
  async function settled() {
    while (pending.size) await Promise.all([...pending]);
  }
  return { tick, bind, destroy, settled, tab: { tabHTML, render } };
}

export function guestStateText(view) {
  if (view.state === "ended") return "Sharing ended · saved copy";
  if (view.delivery === "refused") return "The server ended this computer's access · saved copy";
  if (view.delivery === "changed") return "Not the same server · saved copy";
  let text = "Can view";
  if (view.access === "write") text = "Can comment";
  if (view.delivery === "offline") text += " · offline, showing the saved copy";
  return text;
}
export function guestErrorText(code) {
  if (code === "not_found") return "This chat is no longer in your list.";
  if (code === "copy_damaged") return "The saved copy of this chat is damaged and cannot be shown.";
  if (code === "storage_full") return "This computer's storage is full. Free some space to load more.";
  return "That did not work. It will be tried again.";
}
export function guestMessageHTML(entry, names) {
  let cls = "turn assistant gmsg";
  let who = names[entry.author];
  if (!who) who = "Unknown person";
  let name = `<div class="gwho">${esc(who)}</div>`;
  if (entry.mine) { cls = "turn user gmsg"; name = ""; }
  return `<div class="${cls}" data-cseq="${esc(entry.seq)}" data-anchor="${esc(entry.anchor)}"><div class="bubble" title="${esc(entry.at)}">${name}${md(entry.text)}</div></div>`;
}
export function guestEntryHTML(entry, ownerName) {
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
    if (entry.t === "user") {
      who += " · prompt";
      if (entry.prov === true) who += " · includes forwarded AI text";
    } else {
      who = entry.agent;
      if (!who) who = "Agent";
    }
    let content = "This message was too large to share.";
    if (entry.omitted === undefined) content = md(stripAssetRefs(entry.text));
    return `<div class="turn assistant" data-seq="${esc(entry.seq)}"><div class="bubble" title="${esc(bubbleTitle(entry.t, entry.time))}"><div class="gwho">${esc(who)}</div>${content}</div></div>`;
  } else return "";
  return `<div class="previewtrunc" data-seq="${esc(entry.seq)}">${line}</div>`;
}
