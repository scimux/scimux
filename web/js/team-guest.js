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
    const key = view.server + "/" + view.owner;
    if (!groups.has(key)) {
      let ownerName = view.owner_name;
      if (!ownerName) ownerName = "Unknown person";
      let label = ownerName + " · " + view.server_name;
      const server = servers.find(item => item.id === view.server);
      if (server) {
        if (server.delivery === "offline") label += " · offline";
        if (server.delivery === "refused") label += " · access ended";
        if (server.delivery === "changed") label += " · not the same server";
      }
      groups.set(key, { key, label, ownerName, serverName: view.server_name, owner: view.owner, server: view.server, items: [] });
    }
    groups.get(key).items.push(view);
  }
  const result = [...groups.values()];
  result.sort((a, b) => {
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
    return `<button data-tab="shared" class="${cls}">Shared with me${badge}</button>`;
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
      if (el.innerHTML === content) return true;
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
      if (error.status === 404) off = true;
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
    open = { key: guestKey(view), view, next: 0, first: true, seenAtOpen: view.seen_mirror, seen: view.seen_mirror, lastRead: 0, busy: false, failed: false, empty: false };
    header(view);
    $("sv_body").innerHTML = '<div class="previewtrunc">Loading…</div>';
    $("sv_note").textContent = "";
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
    close();
  }
  function more() { startRead(); }
  function startRead() {
    if (!open) return;
    if (open.busy) return;
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
  async function readRun(current, gen) {
    const body = $("sv_body");
    const first = current.first;
    let unreadSeq = null;
    let hasEntries = false;
    let morePages = false;
    const nearEnd = body.scrollHeight - body.scrollTop - body.clientHeight <= 80;
    try {
      for (let n = 0; n < 40; n++) {
        const result = await api(LIST + "/" + current.key + "?after=" + current.next);
        if (!currentRead(current, gen)) return;
        current.view = result.view;
        header(result.view);
        $("sv_note").textContent = "";
        if (current.first) { body.innerHTML = ""; current.first = false; }
        for (const entry of result.entries) {
          const rendered = guestEntryHTML(entry, result.view.owner_name);
          if (rendered) {
            if (current.empty) { body.innerHTML = ""; current.empty = false; }
            body.insertAdjacentHTML("beforeend", rendered);
            hasEntries = true;
            if (unreadSeq === null) {
              if (entry.seq > current.seenAtOpen) unreadSeq = entry.seq;
            }
          }
        }
        current.next = result.next;
        morePages = result.more;
        if (!morePages) break;
      }
      if (!currentRead(current, gen)) return;
      if (first) {
        if (!hasEntries) {
          body.innerHTML = '<div class="previewtrunc">Nothing has been shared yet.</div>';
          current.empty = true;
        }
        if (unreadSeq !== null) {
          const unread = body.querySelector(`[data-seq="${unreadSeq}"]`);
          if (unread) unread.scrollIntoView({ block: "start" });
        } else body.scrollTop = body.scrollHeight;
      } else if (nearEnd) body.scrollTop = body.scrollHeight;
      $("sv_more").hidden = !morePages;
      if (current.next > current.seen) await markSeen(current, gen);
    } catch (error) {
      if (!currentRead(current, gen)) return;
      let code = "";
      try { code = JSON.parse(error.message).error; } catch {}
      $("sv_note").textContent = guestErrorText(code);
      if (code === "not_found") current.failed = true;
    } finally {
      if (currentRead(current, gen)) current.busy = false;
    }
  }
  async function markSeen(current, gen) {
    const next = current.next;
    try {
      await api(LIST + "/" + current.key + "/seen", { method: "PUT", body: JSON.stringify({ mirror: next }) });
      if (!currentRead(current, gen)) return;
      current.seen = next;
      const view = answer.subscriptions.find(item => guestKey(item) === current.key);
      if (!view) return;
      const before = JSON.stringify(view);
      view.seen_mirror = next;
      view.unread = false;
      if (view.state === "live") {
        if (view.mirror_last > next) view.unread = true;
      }
      if (JSON.stringify(view) !== before) invalidateCards();
    } catch {}
  }
  function bind() {
    if (bound) return;
    bound = true;
    doc.querySelector("#cardlist").addEventListener("click", click);
    doc.querySelector("#sv_back").addEventListener("click", close);
    doc.querySelector("#sv_scrim").addEventListener("click", close);
    doc.querySelector("#sv_more").addEventListener("click", more);
    doc.querySelector("#sharedview").addEventListener("keydown", keydown);
  }
  function destroy() {
    if (bound) {
      doc.querySelector("#cardlist").removeEventListener("click", click);
      doc.querySelector("#sv_back").removeEventListener("click", close);
      doc.querySelector("#sv_scrim").removeEventListener("click", close);
      doc.querySelector("#sv_more").removeEventListener("click", more);
      doc.querySelector("#sharedview").removeEventListener("keydown", keydown);
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
      if (entry.prov === true) who += " · includes forwarded AI text";
    } else {
      who = entry.agent;
      if (!who) who = "Agent";
    }
    let content = "This message was too large to share.";
    if (!entry.omitted) content = md(stripAssetRefs(entry.text));
    return `<div class="turn assistant" data-seq="${esc(entry.seq)}"><div class="bubble" title="${esc(bubbleTitle(entry.t, entry.time))}"><div class="gwho">${esc(who)}</div>${content}</div></div>`;
  } else return "";
  return `<div class="previewtrunc" data-seq="${esc(entry.seq)}">${line}</div>`;
}
