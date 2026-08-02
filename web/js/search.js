/* Search overlay feature: open/close, shortcuts, debounced /api/search,
 * recents, grouped hit feed, and per-hit action bar.
 *
 * Packet 7G ownership inventory
 * -----------------------------
 * Owned DOM roots / controls:
 *   - #searchoverlay (dialog; hidden toggle)
 *   - #searchscrim (backdrop close)
 *   - #searchpanel / #searchbar (structure only; no listeners)
 *   - #searchinput (query field; debounced input)
 *   - #searchclose (✕)
 *   - #searchfeed (recents / groups / empty / error; delegated clicks)
 *   - #searchbtn (🔍 trigger; open)
 *
 * Explicit non-ownership:
 *   - Archived read-only surface (#archivedview, openArchived body) — shell
 *   - Generic sheets / new-node / forkFromTurn sheet body — shell (fork effect
 *     injected)
 *   - Top-level document touch gestures and immutable overlayOwned snapshot
 *   - cards, map, chat, composer, Bookmarks, Notes, polling, app.js
 *   - format.js / bookmarks stampAddress algorithms (reused, not duplicated)
 *
 * Ephemeral state (never ui.json, never localStorage except recents):
 *   - searchReturnFocus
 *   - searchSeq (monotonic; stale-response suppression)
 *   - searchAbort (AbortController for in-flight fetch)
 *   - searchDebounce (180ms timer)
 *   - openHit (transient hit-bar owner)
 *
 * Persistent state (localStorage, exact key):
 *   - "scimux-search-recents" — string[] most-recent-first, cap 8; blank
 *     surface shows 5; record only on hit action (never per keystroke)
 *
 * Server calls (via injected fetch):
 *   - GET /api/search?q=<normalized query>
 *
 * Injected effects / seams:
 *   - nodeById, laneColor, agentLogo
 *   - select, setPendingJump, invalidateChat
 *   - setBookmarksOpen, openArchived, forkFromTurn
 *   - uiMutate, toast, prompt, isDesktop
 *   - now / nowISO (clock), storage, fetch, AbortController
 *   - setTimeout / clearTimeout / requestAnimationFrame
 *   - document, activeElement, createElement, focus
 *   - esc, stampAddress (defaults from format.js / bookmarks.js)
 *
 * Document shortcut ownership (bind):
 *   - Cmd/Ctrl-K → open (anywhere)
 *   - "/" → open when not in text field and overlay closed
 *   - #searchoverlay Escape + Tab focus trap (input ↔ close)
 *
 * Lifecycle:
 *   - bind() idempotent; owns every Search root listener + document shortcut
 *   - destroy() removes listeners, clears debounce, aborts in-flight, closes bar
 *
 * Contracts preserved:
 *   - SEARCH_MIN=2, SEARCH_MAX=128
 *   - Exact 180ms debounce; short query aborts + blank
 *   - Superseding requests abort older; stale seq never renders
 *   - AbortError silent; current-request failure → empty message
 *   - Results survive close; blank refreshes recents on open when field short
 *   - Asset hits show-only; fork only live+forkable; bookmark non-terminal
 *   - Durable address stamping; live/Bookmarks/archived/unavailable routing
 *
 * Reuses (no algorithm duplication):
 *   format.js: esc
 *   bookmarks.js: stampAddress
 * Does not import sheets.js, polling.js, app.js, or later features.
 */

import { esc as escDefault } from "./format.js";
import { stampAddress as stampAddressDefault } from "./bookmarks.js";

/* ---------- public constants ---------- */

export const SEARCH_MIN = 2;
export const SEARCH_MAX = 128;
export const SEARCH_DEBOUNCE_MS = 180;
export const RECENTS_KEY = "scimux-search-recents";
export const RECENTS_CAP = 8;
export const RECENTS_SHOW = 5;
export const SACT_LABEL = {
  show: "Show to chat",
  fork: "Fork from here",
  bookmark: "Add bookmark",
};

/* ---------- pure: query ---------- */

export function normalizeSearchQuery(q){
  return (q || "").trim().slice(0, SEARCH_MAX);
}

export function isSearchQueryReady(q){
  return normalizeSearchQuery(q).length >= SEARCH_MIN;
}

/* ---------- pure: recents ---------- */

/* loadRecents reads storage; corrupt JSON / non-array / getItem throw → []. */
export function loadRecents(storage){
  try {
    const raw = storage && typeof storage.getItem === "function"
      ? storage.getItem(RECENTS_KEY) : null;
    const a = JSON.parse(raw || "[]");
    return Array.isArray(a) ? a.filter(s => typeof s === "string") : [];
  } catch { return []; }
}

/* recordRecent: clamp, gate, move-to-front dedupe, hard-cap; silent setItem fail. */
export function recordRecent(q, storage){
  const query = normalizeSearchQuery(q);
  if (query.length < SEARCH_MIN) return;
  const list = loadRecents(storage).filter(s => s !== query);
  list.unshift(query);
  try {
    if (storage && typeof storage.setItem === "function")
      storage.setItem(RECENTS_KEY, JSON.stringify(list.slice(0, RECENTS_CAP)));
  } catch {}
}

export function clearRecents(storage){
  try {
    if (storage && typeof storage.removeItem === "function")
      storage.removeItem(RECENTS_KEY);
  } catch {}
}

export function recentsHTML(storage, esc){
  const e = esc || escDefault;
  const list = loadRecents(storage).slice(0, RECENTS_SHOW);
  if (!list.length) return "";
  return `<div class="recents"><div class="recentshead">Recent searches` +
    `<button id="recentsclear" type="button">Clear</button></div>` +
    list.map(q => `<button class="recent" type="button" data-recent="${e(q)}">${e(q)}</button>`).join("") +
    `</div>`;
}

export function searchBlankHTML(storage, esc){
  return recentsHTML(storage, esc)
    || `<div class="empty">Search your current and past chats and notes.</div>`;
}

/* ---------- pure: hit / group HTML ---------- */

export function searchHitHTML(h, esc){
  const e = esc || escDefault;
  const before = e(h.before || ""), match = e(h.match || ""), after = e(h.after || "");
  const when = h.turn_time || h.time || "";
  return `<div class="searchhit" data-kind="${e(h._kind || "")}" data-id="${e(h._id || "")}"` +
    ` data-uid="${e(h._uid || "")}" data-segment="${h.segment || 0}" data-record="${h.record || 0}"` +
    ` data-role="${e(h.role || "")}" data-bookmark-id="${e(h.bookmark_id || "")}" data-forkable="${h._forkable ? 1 : 0}"` +
    ` data-lane="${e(h._lane || "")}" data-turn="${e(h.turn_time || h.time || "")}">` +
    `<span class="ex">${before}<mark>${match}</mark>${after}</span>` +
    (when ? `<span class="when">${e(when)}</span>` : "") +
    `</div>`;
}

export function searchGroupHTML(g, deps){
  const d = deps || {};
  const e = d.esc || escDefault;
  const laneColor = typeof d.laneColor === "function" ? d.laneColor : () => "";
  const agentLogo = typeof d.agentLogo === "function" ? d.agentLogo : () => "";
  const swatch = g.lane_id ? `<span class="tabdot" style="background:${e(laneColor(g.lane_id))}"></span>` : "";
  const kind = g.kind === "bookmarks" ? "bookmarks" : g.kind === "archived" ? "past" : "";
  const hits = (g.hits || []).map(h =>
    searchHitHTML(Object.assign({ _kind: g.kind, _id: g.id, _uid: g.uid,
      _forkable: g.forkable, _lane: g.lane_id }, h), e)).join("");
  return `<div class="searchgroup">` +
    `<div class="sghead">` +
      `<span class="agent-logo" title="${e(g.agent || "agent")}">${agentLogo(g.agent)}</span>` +
      swatch +
      `<span class="sgtitle">${e(g.title || "Untitled")}</span>` +
      (kind ? `<span class="sgkind">${kind}</span>` : "") +
    `</div>${hits}</div>`;
}

export function searchFeedHTML(resp, deps){
  const groups = (resp && resp.groups) || [];
  if (!groups.length)
    return `<div class="empty">No matches.</div>`;
  return groups.map(g => searchGroupHTML(g, deps)).join("")
    + (resp.partial ? `<div class="searchmore">Showing the most recent matches — refine to narrow.</div>` : "");
}

/* ---------- pure: actions ---------- */

/* Adaptive action list: every hit can be shown; asset → show only;
   fork only for live+forkable; bookmark for non-asset. */
export function searchHitActions(kind, forkable, role){
  const acts = [];
  acts.push("show");
  if (role === "asset") return acts;
  if (kind === "live" && forkable) acts.push("fork");
  acts.push("bookmark");
  return acts;
}

export function hitBarHTML(kind, forkable, role){
  return searchHitActions(kind, forkable, role)
    .map(a => `<button data-sact="${a}">${SACT_LABEL[a]}</button>`).join("");
}

export function buildPendingJump(id, turn, addr, now){
  return {
    node: id,
    turnTime: turn || "",
    text: "",
    ts: now,
    uid: (addr && addr.uid) || "",
    segment: addr && +addr.segment,
    record: addr && +addr.record,
  };
}

export function buildSearchBookmark(fields, stampAddress){
  const f = fields || {};
  const stamp = stampAddress || stampAddressDefault;
  const bookmark = { t: f.t, text: f.text };
  if (f.node) bookmark.node = f.node;
  if (f.turnTime) bookmark.turnTime = f.turnTime;
  if (f.lane) bookmark.lane = f.lane;
  stamp(bookmark, { uid: f.uid, segment: f.segment, record: f.record });
  return bookmark;
}

/* show destination for a hit given live node presence and kind. */
export function showActionDecision(kind, live){
  if (live) return "live";
  if (kind === "bookmarks") return "bookmarks";
  if (kind === "archived") return "archived";
  return "unavailable";
}

/* ---------- factory ---------- */

export function createSearchFeature(deps){
  const d = deps || {};
  const roots = d.roots || {};
  const doc = d.document || (typeof document !== "undefined" ? document : null);
  const storage = d.storage || null;
  const esc = d.esc || escDefault;
  const stampAddress = d.stampAddress || stampAddressDefault;
  const setTimeoutFn = d.setTimeout || setTimeout;
  const clearTimeoutFn = d.clearTimeout || clearTimeout;
  const rAF = d.requestAnimationFrame || (typeof requestAnimationFrame !== "undefined"
    ? requestAnimationFrame : fn => setTimeoutFn(fn, 0));
  const fetchFn = typeof d.fetch === "function" ? d.fetch
    : (typeof fetch !== "undefined" ? fetch.bind(globalThis) : null);
  const AbortCtrl = d.AbortController || (typeof AbortController !== "undefined" ? AbortController : null);
  const createEl = typeof d.createElement === "function" ? d.createElement
    : (tag => (doc && doc.createElement ? doc.createElement(tag) : null));

  let searchReturnFocus = null;
  let searchSeq = 0;
  let searchAbort = null;
  let searchDebounce = null;
  let openHit = null;
  let bound = false;
  const cleanups = [];

  function root(name){
    if (roots[name]) return roots[name];
    if (roots["#" + name]) return roots["#" + name];
    if (doc && typeof doc.getElementById === "function") return doc.getElementById(name);
    if (doc && typeof doc.querySelector === "function") return doc.querySelector("#" + name);
    return null;
  }

  function activeEl(){
    if (typeof d.activeElement === "function") return d.activeElement();
    return doc && doc.activeElement;
  }

  function docContains(el){
    if (typeof d.contains === "function") return d.contains(el);
    if (doc && typeof doc.contains === "function") return doc.contains(el);
    return !!el;
  }

  function now(){
    if (typeof d.now === "function") return d.now();
    return Date.now();
  }

  function nowISO(){
    if (typeof d.nowISO === "function") return d.nowISO();
    return new Date().toISOString();
  }

  function nodeById(id){
    return typeof d.nodeById === "function" ? d.nodeById(id) : null;
  }
  function laneColor(id){
    return typeof d.laneColor === "function" ? d.laneColor(id) : "";
  }
  function agentLogo(agent){
    return typeof d.agentLogo === "function" ? d.agentLogo(agent) : "";
  }
  function htmlDeps(){
    return { esc, laneColor, agentLogo };
  }

  function isOpen(){
    const ov = root("searchoverlay");
    return !!(ov && !ov.hidden);
  }

  function searchBlank(){
    openHit = null;
    const feed = root("searchfeed");
    if (feed) feed.innerHTML = searchBlankHTML(storage, esc);
  }

  function renderSearchFeed(resp){
    const feed = root("searchfeed");
    if (!feed) return;
    openHit = null;
    feed.innerHTML = searchFeedHTML(resp, htmlDeps());
  }

  function closeHitBar(){
    if (!openHit) return;
    if (openHit.classList) openHit.classList.remove("open");
    else if (openHit.className)
      openHit.className = String(openHit.className).replace(/\bopen\b/g, "").trim();
    if (typeof openHit.querySelector === "function")
      openHit.querySelector(".hitbar")?.remove();
    openHit = null;
  }

  function toggleHitBar(el){
    if (!el) return;
    if (openHit === el){ closeHitBar(); return; }
    closeHitBar();
    const bar = createEl("div");
    if (!bar) return;
    bar.className = "hitbar";
    bar.innerHTML = hitBarHTML(el.dataset.kind, el.dataset.forkable === "1", el.dataset.role);
    if (typeof el.appendChild === "function") el.appendChild(bar);
    if (el.classList) el.classList.add("open");
    else el.className = ((el.className || "") + " open").trim();
    openHit = el;
  }

  function focusInput(){
    const input = root("searchinput");
    if (input && typeof input.focus === "function") input.focus();
  }

  function open(){
    if (isOpen()){ focusInput(); return; }
    searchReturnFocus = activeEl();
    const ov = root("searchoverlay");
    if (ov) ov.hidden = false;
    const input = root("searchinput");
    const val = input ? (input.value || "") : "";
    if (normalizeSearchQuery(val).length < SEARCH_MIN) searchBlank();
    rAF(() => focusInput());
  }

  function close(){
    if (!isOpen()) return;
    const ov = root("searchoverlay");
    if (ov) ov.hidden = true;
    const back = searchReturnFocus;
    searchReturnFocus = null;
    if (back && docContains(back) && typeof back.focus === "function") back.focus();
    else {
      const btn = root("searchbtn");
      if (btn && typeof btn.focus === "function") btn.focus();
    }
  }

  async function runSearch(q){
    const query = normalizeSearchQuery(q);
    if (query.length < SEARCH_MIN){
      searchSeq++;
      if (searchAbort){ searchAbort.abort(); searchAbort = null; }
      searchBlank();
      return;
    }
    const seq = ++searchSeq;
    if (searchAbort) searchAbort.abort();
    if (!AbortCtrl || !fetchFn){
      const feed = root("searchfeed");
      if (feed) feed.innerHTML = `<div class="empty">Search is unavailable right now.</div>`;
      return;
    }
    searchAbort = new AbortCtrl();
    try {
      const r = await fetchFn("/api/search?q=" + encodeURIComponent(query), { signal: searchAbort.signal });
      if (!r.ok) throw new Error("search " + r.status);
      const resp = typeof r.json === "function" ? await r.json() : r.body;
      if (seq !== searchSeq) return;
      renderSearchFeed(resp);
    } catch (err) {
      if (err && err.name === "AbortError") return;
      if (seq === searchSeq){
        const feed = root("searchfeed");
        if (feed) feed.innerHTML = `<div class="empty">Search is unavailable right now.</div>`;
      }
    }
  }

  function jumpToHitChat(id, turn, addr){
    close();
    if (typeof d.setPendingJump === "function")
      d.setPendingJump(buildPendingJump(id, turn, addr, now()));
    if (typeof d.invalidateChat === "function") d.invalidateChat();
    const desktop = typeof d.isDesktop === "function" ? d.isDesktop() : false;
    if (!desktop && typeof d.setBookmarksOpen === "function") d.setBookmarksOpen(false);
    if (typeof d.select === "function") d.select(id);
  }

  function doSearchAction(a, hit){
    if (!hit) return;
    const input = root("searchinput");
    recordRecent(input ? input.value : "", storage);
    const kind = hit.dataset.kind, id = hit.dataset.id;
    const turn = hit.dataset.turn, lane = hit.dataset.lane;
    const live = id && nodeById(id);
    if (a === "show"){
      const dest = showActionDecision(kind, live);
      if (dest === "live")
        jumpToHitChat(id, turn, { uid: hit.dataset.uid, segment: hit.dataset.segment, record: hit.dataset.record });
      else if (dest === "bookmarks"){
        close();
        if (typeof d.setBookmarksOpen === "function") d.setBookmarksOpen(true);
      } else if (dest === "archived"){
        close();
        if (typeof d.openArchived === "function")
          d.openArchived(hit.dataset.uid, hit.dataset.segment, hit.dataset.record, turn);
      } else if (typeof d.toast === "function"){
        d.toast("This chat is unavailable.");
      }
      return;
    }
    if (a === "fork"){
      if (!live) return;
      const ex = (typeof hit.querySelector === "function"
        ? hit.querySelector(".ex")?.textContent : "") || "";
      close();
      if (typeof d.forkFromTurn === "function") d.forkFromTurn(ex, id);
      return;
    }
    if (a === "bookmark"){
      const text = typeof d.prompt === "function" ? d.prompt("Add a bookmark")
        : (typeof prompt !== "undefined" ? prompt("Add a bookmark") : null);
      if (text == null) return;
      const clean = String(text).trim();
      if (!clean) return;
      const bookmark = buildSearchBookmark({
        t: nowISO(),
        text: clean,
        node: live ? id : "",
        turnTime: turn || "",
        lane: lane || "",
        uid: hit.dataset.uid,
        segment: hit.dataset.segment,
        record: hit.dataset.record,
      }, stampAddress);
      if (typeof d.uiMutate === "function")
        d.uiMutate({ k: "bookmark-add", bookmark });
      if (typeof d.toast === "function") d.toast("Bookmark added");
      closeHitBar();
    }
  }

  function onDocumentKeydown(e){
    if ((e.metaKey || e.ctrlKey) && !e.altKey && (e.key === "k" || e.key === "K")){
      e.preventDefault(); open();
      return;
    }
    if (e.key === "/" && !e.metaKey && !e.ctrlKey && !e.altKey && !isOpen() &&
        !/^(INPUT|TEXTAREA|SELECT)$/.test(e.target && e.target.tagName) &&
        !(e.target && e.target.isContentEditable)){
      e.preventDefault(); open();
    }
  }

  function onOverlayKeydown(e){
    if (e.key === "Escape"){ e.preventDefault(); e.stopPropagation(); close(); return; }
    if (e.key !== "Tab") return;
    const stops = [root("searchinput"), root("searchclose")];
    const first = stops[0], last = stops[stops.length - 1];
    if (!first || !last) return;
    const cur = activeEl();
    if (e.shiftKey && cur === first){ e.preventDefault(); if (typeof last.focus === "function") last.focus(); }
    else if (!e.shiftKey && cur === last){ e.preventDefault(); if (typeof first.focus === "function") first.focus(); }
  }

  function onInput(e){
    clearTimeoutFn(searchDebounce);
    const q = e.target ? e.target.value : "";
    searchDebounce = setTimeoutFn(() => runSearch(q), SEARCH_DEBOUNCE_MS);
  }

  function onFeedClick(e){
    const t = e.target;
    if (!t) return;
    if (typeof t.closest === "function" && t.closest("#recentsclear")){
      clearRecents(storage); searchBlank(); return;
    }
    const rc = typeof t.closest === "function" ? t.closest("[data-recent]") : null;
    if (rc){
      const q = rc.dataset.recent;
      const input = root("searchinput");
      if (input) input.value = q;
      runSearch(q);
      return;
    }
    const btn = typeof t.closest === "function" ? t.closest("[data-sact]") : null;
    if (btn){
      doSearchAction(btn.dataset.sact, typeof btn.closest === "function" ? btn.closest(".searchhit") : null);
      return;
    }
    const hit = typeof t.closest === "function" ? t.closest(".searchhit") : null;
    if (hit) toggleHitBar(hit);
  }

  function on(el, type, fn, opts){
    if (!el || typeof el.addEventListener !== "function") return;
    el.addEventListener(type, fn, opts);
    cleanups.push(() => {
      if (typeof el.removeEventListener === "function") el.removeEventListener(type, fn, opts);
    });
  }

  function bind(){
    if (bound) return;
    bound = true;
    on(root("searchbtn"), "click", open);
    on(root("searchclose"), "click", close);
    on(root("searchscrim"), "click", close);
    on(doc, "keydown", onDocumentKeydown);
    on(root("searchoverlay"), "keydown", onOverlayKeydown);
    on(root("searchinput"), "input", onInput);
    on(root("searchfeed"), "click", onFeedClick);
  }

  function destroy(){
    if (!bound) return;
    bound = false;
    for (const c of cleanups.splice(0)) c();
    clearTimeoutFn(searchDebounce);
    searchDebounce = null;
    searchSeq++; /* invalidate a response even when an injected fetch ignores abort */
    if (searchAbort){ try { searchAbort.abort(); } catch {} searchAbort = null; }
    closeHitBar();
    searchReturnFocus = null;
  }

  return {
    bind,
    destroy,
    open,
    close,
    isOpen,
  };
}
