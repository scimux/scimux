/* Search overlay feature: open/close, shortcuts, debounced /api/search,
 * recents, and the grouped hit feed.
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
 *   - Chat preview surface (#previewview, openPreview body) — shell
 *   - Generic sheets / new-node / fork — shell (this module no longer forks)
 *   - Top-level document touch gestures and immutable overlayOwned snapshot
 *   - cards, map, chat, composer, Bookmarks, Notes, polling, app.js
 *   - format.js esc (reused, not duplicated)
 *
 * Ephemeral state (never ui.json, never localStorage except recents):
 *   - searchReturnFocus
 *   - searchSeq (monotonic; stale-response suppression)
 *   - searchAbort (AbortController for in-flight fetch)
 *   - searchDebounce (180ms timer)
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
 *   - setBookmarksOpen, openPreview
 *   - toast, isDesktop
 *   - now (clock), storage, fetch, AbortController
 *   - setTimeout / clearTimeout / requestAnimationFrame
 *   - document, activeElement, focus
 *   - esc (default from format.js)
 *
 * Document shortcut ownership (bind):
 *   - Cmd/Ctrl-K → open (anywhere)
 *   - "/" → open when not in text field and overlay closed
 *   - #searchoverlay Escape + Tab focus trap (input ↔ close)
 *
 * Lifecycle:
 *   - bind() idempotent; owns every Search root listener + document shortcut
 *   - destroy() removes listeners, clears debounce, aborts in-flight
 *
 * Contracts preserved:
 *   - SEARCH_MIN=2, SEARCH_MAX=128
 *   - Exact 180ms debounce; short query aborts + blank
 *   - Superseding requests abort older; stale seq never renders
 *   - AbortError silent; current-request failure → empty message
 *   - Results survive close; blank refreshes recents on open when field short
 *   - One gesture: a hit opens the preview; unaddressed bookmark hits open the
 *     pane; an addressless live hit jumps; nothing else is offered here
 *
 * Reuses (no algorithm duplication):
 *   format.js: esc
 * Does not import sheets.js, polling.js, app.js, or later features.
 */

import { esc as escDefault } from "./format.js";

/* ---------- public constants ---------- */

export const SEARCH_MIN = 2;
export const SEARCH_MAX = 128;
export const SEARCH_DEBOUNCE_MS = 180;
export const RECENTS_KEY = "scimux-search-recents";
export const RECENTS_CAP = 8;
export const RECENTS_SHOW = 5;

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
    ` data-role="${e(h.role || "")}" data-bookmark-id="${e(h.bookmark_id || "")}"` +
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
  /* A bookmark carries its own stamped address and the Bookmarks bucket has no
     group address to lend, so the hit's uid wins where it has one. */
  const hits = (g.hits || []).map(h =>
    searchHitHTML(Object.assign({ _kind: g.kind, _id: g.id, _uid: h.uid || g.uid,
      _lane: g.lane_id }, h), e)).join("");
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

/* ---------- pure: the jump address ---------- */

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

/* ---------- factory ---------- */

export function createSearchFeature(deps){
  const d = deps || {};
  const roots = d.roots || {};
  const doc = d.document || (typeof document !== "undefined" ? document : null);
  const storage = d.storage || null;
  const esc = d.esc || escDefault;
  const setTimeoutFn = d.setTimeout || setTimeout;
  const clearTimeoutFn = d.clearTimeout || clearTimeout;
  const rAF = d.requestAnimationFrame || (typeof requestAnimationFrame !== "undefined"
    ? requestAnimationFrame : fn => setTimeoutFn(fn, 0));
  const fetchFn = typeof d.fetch === "function" ? d.fetch
    : (typeof fetch !== "undefined" ? fetch.bind(globalThis) : null);
  const AbortCtrl = d.AbortController || (typeof AbortController !== "undefined" ? AbortController : null);

  let searchReturnFocus = null;
  let searchSeq = 0;
  let searchAbort = null;
  let searchDebounce = null;
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
    const feed = root("searchfeed");
    if (feed) feed.innerHTML = searchBlankHTML(storage, esc);
  }

  function renderSearchFeed(resp){
    const feed = root("searchfeed");
    if (!feed) return;
    feed.innerHTML = searchFeedHTML(resp, htmlDeps());
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

  /* The only thing a result row does. Every hit opens its surrounding
     conversation in the preview — the same gesture, the same destination,
     whether the chat is still running or was deleted months ago, because the
     row itself gives the user no way to tell those apart and the old adaptive
     action bar made them find out by tapping.

     Two hits cannot be previewed, and both fall back to something true rather
     than to a disabled control. A bookmark filed as free-standing commentary
     points at no turn in any log, so it opens in the pane where it lives. A
     live chat whose log predates the meta header has no address to resolve, but
     the chat itself is right there — so the tap goes into it, which is what
     every live hit used to do. */
  function openHitPreview(hit){
    if (!hit) return;
    const input = root("searchinput");
    recordRecent(input ? input.value : "", storage);
    const uid = hit.dataset.uid;
    const id = hit.dataset.id;
    const turn = hit.dataset.turn;
    if (uid){
      close();
      if (typeof d.openPreview === "function")
        d.openPreview(uid, hit.dataset.segment, hit.dataset.record, turn);
      return;
    }
    if (hit.dataset.kind === "bookmarks"){
      close();
      if (typeof d.setBookmarksOpen === "function") d.setBookmarksOpen(true);
      return;
    }
    if (id && nodeById(id)){
      jumpToHitChat(id, turn, { uid: "", segment: hit.dataset.segment, record: hit.dataset.record });
      return;
    }
    if (typeof d.toast === "function") d.toast("This chat is unavailable.");
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
    const hit = typeof t.closest === "function" ? t.closest(".searchhit") : null;
    if (hit) openHitPreview(hit);
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
