/* Shared subscription list and read-only guest view. */
import { esc } from "./format.js";

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
  let generation = 0;
  const pending = new Set();
  const collapsed = new Set();
  let rendered = "";
  let listEl = null;
  let bound = false;

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
  async function load(gen) {
    try {
      const next = await api(LIST);
      if (destroyed) return;
      if (gen !== generation) return;
      const changed = JSON.stringify(next) !== JSON.stringify(answer);
      answer = next;
      if (changed) invalidateCards();
    } catch (error) {
      if (destroyed) return;
      if (gen !== generation) return;
      if (error.status === 404) off = true;
    } finally {
      if (gen === generation) loading = false;
    }
  }
  function tick() {
    if (destroyed) return;
    if (off) return;
    if (loading) return;
    const time = now();
    if (lastLoad !== null) {
      if (time - lastLoad < 10000) return;
    }
    lastLoad = time;
    loading = true;
    generation++;
    track(load(generation));
  }
  function click(event) {
    const group = event.target.closest("[data-group]");
    if (!group) return;
    const key = group.dataset.group;
    if (collapsed.has(key)) collapsed.delete(key);
    else collapsed.add(key);
    if (listEl) render(listEl);
  }
  function bind() {
    if (bound) return;
    bound = true;
    doc.querySelector("#cardlist").addEventListener("click", click);
  }
  function destroy() {
    if (bound) doc.querySelector("#cardlist").removeEventListener("click", click);
    bound = false;
    destroyed = true;
    generation++;
  }
  async function settled() {
    while (pending.size) await Promise.all([...pending]);
  }
  return { tick, bind, destroy, settled, tab: { tabHTML, render } };
}

export function guestStateText() { return ""; }
export function guestErrorText() { return ""; }
export function guestEntryHTML() { return ""; }
