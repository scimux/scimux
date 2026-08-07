/* Activities card feature: #cardtabs + #cardlist rendering and card-local events.
 *
 * Packet 7A ownership inventory
 * -----------------------------
 * Owned roots:
 *   - #cardtabs  (tab strip: scope / optional Pinned / Archived)
 *   - #cardlist  (fold, adoptable sessions, activity cards, empty states)
 *
 * Inputs (explicit getters / pure args — never implicit globals):
 *   - nodes, unadopted, selectedId (sel)
 *   - cardTab, laneFilter, attnFoldOpen, expanded
 *   - actionCard, editingDesc, editingTitle, editingTitleScope
 *   - cardsSig, pinDragging (feature-local during drag)
 *   - UI.pinned, UI.archived, UI.lanes, UI.bookmarks length
 *   - icons, agentLogo, laneSelectHTML / syncLanePicker / readLaneChoice
 *   - laneColor / laneName / laneList, CSS.escape
 *
 * Outputs:
 *   - HTML into #cardtabs / #cardlist
 *   - document.title attention prefix
 *   - age textContent refresh on signature equality
 *   - uiMutate pin/unpin/arch/unarch/pin-order; PATCH description+lane; DELETE node
 *   - callbacks: exitThread, openAdopt, selectNode, setLevel, setLaneFilter,
 *     renderChatHead, renderMap, updateLocalNode, scheduleTick
 *
 * Events owned here after one idempotent bind():
 *   - #cardtabs click (tab switch + clear-scope)
 *   - #cardlist click (fold/save/pin/exit/arch/trash/expand/adopt/open/actions-dismiss)
 *   - #cardlist touchstart/touchend (swipe-to-actions)
 *   - #cardlist dragstart/dragover/drop/dragend (pinned reorder)
 *   - longpress on #cardlist for title / folded-card actions / summary edit
 *   - CARD_TIME_SWAP_MS interval for age flip
 *
 * Not owned (stay shell / other features):
 *   - shell/spatial document gestures, emptyTap fold-back on #cardlist
 *   - document change for [data-lane-select] (shared with sheets)
 *   - setLaneFilter implementation + map chip callers
 *   - longpress helper implementation; chat/map/bookmark longpresses
 *   - startTitleEdit / commitTitleEdit / cancelTitleEdit (shared with chat)
 *   - document keydown/focusout title-edit handlers
 *   - laneOptionsHTML / fillLaneSelect / makeLane pure wrappers (shared sheets)
 *   - map, chat, composer, bookmarks, notes, search, sheets, polling
 *
 * Reuses (no algorithm duplication):
 *   format.js: esc, ageText
 *   map-model.js: hardAttention, cardState, statusText, orderedNodes,
 *     pinnedOrder, isArchived, isPinned, inLaneScope, visibleCardLists
 *   lanes.js pure functions only via injected laneColor/laneName/laneList
 *   state.js only via injected uiMutate
 *
 * Module size: intentionally above the ~500-line soft guide — one factory owns
 * the complete Activities card surface (render + edit preservation + events).
 */

import { esc, ageText } from "./format.js";
import {
  hardAttention,
  cardState,
  statusText,
  orderedNodes,
  isArchived as isArchivedMod,
  isPinned as isPinnedMod,
  inLaneScope,
  visibleCardLists,
} from "./map-model.js";

export const CARD_TIME_SWAP_MS = 30000;

/* ---------- pure card time / meta / config ---------- */

export function cardConfigText(n){
  const parts = [(n && n.model) || "default"];
  if (n && n.effort) parts.push(n.effort);
  return parts.join("/");
}

export function cardTimeItems(n){
  const you = (n && n.last_interaction) || 0;
  const seen = (n && n.last_activity) || 0;
  const items = [];
  if (you) items.push({ key: "you", ms: you });
  if ((!you && seen) || (you && seen > you + 9000)) items.push({ key: "seen", ms: seen });
  return items;
}

export function cardTimeTitle(n, ageFn = ageText){
  const you = (n && n.last_interaction) || 0;
  const seen = (n && n.last_activity) || 0;
  if (you && seen) return `Last interaction ${ageFn(you)} · last seen ${ageFn(seen)}`;
  if (you) return `Last interaction ${ageFn(you)}`;
  return "Last seen";
}

export function cardTimeText(n, flip = 0, ageFn = ageText){
  const items = cardTimeItems(n);
  if (!items.length) return "";
  const it = items[flip % items.length];
  return `${it.key} ${ageFn(it.ms)}`;
}

export function cardTimeHTML(n, flip = 0, { escape = esc, ageFn = ageText } = {}){
  const txt = cardTimeText(n, flip, ageFn);
  if (!txt) {
    return `<span class="time" title=""></span>`;
  }
  return `<span class="time" title="${escape(cardTimeTitle(n, ageFn))}">${escape(txt)}</span>`;
}

export function cardMetaHTML(n, flip = 0, deps = {}){
  const escape = deps.escape || esc;
  const ageFn = deps.ageFn || ageText;
  const status = deps.statusText || statusText;
  const state = status(n).toLowerCase();
  return `${escape(cardConfigText(n))} · ${escape(state)} · ${cardTimeHTML(n, flip, { escape, ageFn })}`;
}

/* ---------- pure signature / tab / count / empty / fold decisions ---------- */

export function resolvePinnedTabFallback(cardTab, pinnedCount){
  /* If the last pin disappears while Pinned is open, fall back to All. */
  if (cardTab === "pinned" && !pinnedCount) return "current";
  return cardTab;
}

export function pinnedLiveCount(pinned, nodes){
  const byId = new Set((nodes || []).map(n => n.id));
  return (pinned || []).filter(id => byId.has(id)).length;
}

export function computeCardsSignature({
  list, foldList, unadopted, sel, expanded, cardTab, laneFilter,
  attnFoldOpen, bookmarksLen, lanes, pinned, actionCard, editingDesc, editingTitle,
}){
  const main = list || [];
  const fold = foldList || [];
  return JSON.stringify(main.concat(fold).map(n => [
    n.id, n.title, n.description, n.lane_id, n.ended_at || "", n.live, n.attention,
    n.model, n.effort, !!n.last_interaction,
  ]))
    + "|" + JSON.stringify(unadopted || [])
    + "|" + (sel || "")
    + "|" + [...(expanded || [])].join(",")
    + "|" + cardTab
    + "|" + laneFilter
    + "|" + fold.length
    + "|" + attnFoldOpen
    + "|" + (bookmarksLen || 0)
    + "|" + JSON.stringify(lanes || [])
    + "|" + JSON.stringify(pinned || [])
    + "|" + actionCard
    + "|" + editingDesc
    + "|" + editingTitle;
}

export function cardsRenderDecision(sig, cardsSig, pinDragging){
  if (pinDragging) return "skip";
  if (sig === cardsSig) return "ages";
  return "rebuild";
}

/* Mirrors the tabList/inScope filtering used for badges (not fold/adoptable). */
export function cardTabCount(nodes, { archivedTab, laneFilter, selectedId, archived } = {}){
  return orderedNodes(nodes).filter(n =>
    (isArchivedMod(n.id, archived) === archivedTab || (!archivedTab && hardAttention(n))) &&
    inLaneScope(n, { laneFilter, selectedId })).length;
}

export function emptyCardsHTML({ laneFilter, cardTab, hasAdoptCards }){
  if (laneFilter) {
    return `<div class="empty">No ${cardTab === "archived" ? "archived " : ""}activities on this journey.</div>`;
  }
  if (cardTab === "archived") {
    return `<div class="empty">Nothing archived yet — long-press a card to archive it.</div>`;
  }
  if (hasAdoptCards) return "";
  return `<div class="empty">No activities yet.<br><br><button class="plus" style="width:auto;padding:0 16px" onclick="document.getElementById('plusbtn').click()">start the first one</button></div>`;
}

export function adoptCardsHTML(unadopted, { cardTab, laneFilter, escape = esc } = {}){
  if (cardTab !== "current" || laneFilter) return "";
  return (unadopted || []).map(s => `
    <div class="card adoptable">
      <div class="row1"><span class="title">${escape(s)}</span></div>
      <div class="status">unadopted tmux session</div>
      <button class="go" data-adopt="${escape(s)}" aria-label="adopt ${escape(s)}"></button>
    </div>`).join("");
}

export function attnFoldButtonHTML(foldLen, attnFoldOpen){
  if (!foldLen) return "";
  return `
    <button class="attnfold ${attnFoldOpen ? "open" : ""}" data-attnfold
      aria-expanded="${attnFoldOpen}">
      <span class="attndot"></span>
      <span class="foldlabel">Input needed (${foldLen})</span>
      <span class="foldchev">&#8250;</span>
    </button>`;
}

export function cardTabsHTML({
  cardTab, laneFilter, laneColor, laneName, currentCount, archivedCount, pinnedCount,
  escape = esc,
}){
  const scope = laneFilter
    ? `<span class="tabdot" style="background:${escape(laneColor)}"></span>${escape(laneName)} (${currentCount})<span class="x" data-clear-scope aria-label="show all activities">&#10005;</span>`
    : "All";
  const pc = pinnedCount || 0;
  return `<button data-tab="current" class="${cardTab === "current" ? "on" : ""}">${scope}</button>` +
    (pc ? `<button data-tab="pinned" class="${cardTab === "pinned" ? "on" : ""}">Pinned (${pc})</button>` : "") +
    `<button data-tab="archived" class="${cardTab === "archived" ? "on" : ""}">Archived (${archivedCount})</button>`;
}

/* Drag end: persist DOM order, then append any off-screen survivors. */
export function pinOrderAfterDrag(domOrderIds, storedPinned){
  const order = (domOrderIds || []).filter(Boolean);
  const rest = (storedPinned || []).filter(id => !order.includes(id));
  return order.concat(rest);
}

export function cardSwipeOpensActions(dx, dy){
  return dx < -46 && Math.abs(dx) > Math.abs(dy) * 1.6;
}

/* ---------- edit-preservation pure key + restore loop ---------- */

export function cardEditKey(el, editingDesc){
  if (!el || !el.dataset) return "";
  if (el.dataset.titleInput != null) return "title:" + el.dataset.titleInput;
  /* every card carries a (hidden) descbox; only the one being edited may
     shadow a fresh server value */
  if (el.dataset.descInput != null)
    return editingDesc === el.dataset.descInput ? "desc:" + el.dataset.descInput : "";
  if (el.dataset.laneNew != null) return "lanenew:" + el.dataset.laneNew;
  if (typeof el.matches === "function" && el.matches("[data-lane-select]"))
    return "lanesel:" + (el.closest?.("[data-card-lane]")?.dataset?.cardLane || "");
  return "";
}

/* Snapshot edit fields, run render(), restore values/focus/selection.
   Root must expose querySelectorAll; document provides activeElement. */
export function withCardEditsPreserved(root, editingDesc, render, {
  document: doc = typeof document !== "undefined" ? document : null,
  syncLanePicker,
} = {}){
  if (!root) { render(); return; }
  const saved = {};
  let focusKey = "", selStart = 0, selEnd = 0;
  const fields = root.querySelectorAll ? root.querySelectorAll("input, textarea, select") : [];
  for (const el of fields){
    const k = cardEditKey(el, editingDesc);
    if (!k) continue;
    saved[k] = el.value;
    if (doc && el === doc.activeElement){
      focusKey = k;
      if (el.setSelectionRange){
        selStart = el.selectionStart ?? el.value.length;
        selEnd = el.selectionEnd ?? el.value.length;
      }
    }
  }
  render();
  if (!Object.keys(saved).length) return;
  const after = root.querySelectorAll ? root.querySelectorAll("input, textarea, select") : [];
  for (const el of after){
    const k = cardEditKey(el, editingDesc);
    if (!k || !(k in saved)) continue;
    if (el.value !== saved[k]) el.value = saved[k];
    if (typeof el.matches === "function" && el.matches("[data-lane-select]") && syncLanePicker)
      syncLanePicker(el);
    if (k === focusKey){
      el.focus?.();
      if (el.setSelectionRange && el.type !== "select-one"){
        try { el.setSelectionRange(selStart, selEnd); } catch { /* ignore */ }
      }
    }
  }
}

/* ---------- factory ---------- */

export function createCardsFeature(deps){
  const d = deps || {};
  const tabs = d.roots?.tabs;
  const list = d.roots?.list;
  const doc = d.document || (typeof document !== "undefined" ? document : null);
  const CSSRef = d.CSS || (typeof CSS !== "undefined" ? CSS : { escape: s => s });
  const setIntervalFn = d.setInterval || setInterval;
  const clearIntervalFn = d.clearInterval || clearInterval;

  let cardTimeFlip = 0;
  let pinDragging = false;
  let pinDragEl = null;
  let cardSwipe = null;
  let ageTimer = null;
  let bound = false;
  const cleanups = [];

  const icons = d.icons || {};
  const escape = d.esc || esc;
  const ageFn = d.ageText || ageText;

  function g(name, fallback){
    const v = d[name];
    return typeof v === "function" ? v() : (v !== undefined ? v : fallback);
  }
  function set(name, value){
    const fn = d["set" + name[0].toUpperCase() + name.slice(1)];
    if (typeof fn === "function") fn(value);
  }

  function nodeById(id){
    if (typeof d.nodeById === "function") return d.nodeById(id);
    return (g("nodes", []) || []).find(n => n.id === id);
  }

  function isArchived(id){ return isArchivedMod(id, g("archived", [])); }
  function isPinned(id){ return isPinnedMod(id, g("pinned", [])); }
  function pinnedCount(){ return pinnedLiveCount(g("pinned", []), g("nodes", [])); }

  function railboxHTML(n){
    const color = d.laneColor ? d.laneColor(n.lane_id) : "";
    const name = d.laneName ? d.laneName(n.lane_id) : (n.lane_id || "");
    return `<div class="railbox">
      <div class="railrow"><span class="k">Journey:</span>
        <span class="laneswatch" style="background:${escape(color)}"></span>
        <span>${escape(name)}</span></div>
    </div>`;
  }

  function cardHTML(n){
    const desc = n.description || n.prompt || "";
    const archived = isArchived(n.id);
    const pinned = isPinned(n.id);
    const sel = g("sel", "");
    const expanded = g("expanded", new Set());
    const actionCard = g("actionCard", "");
    const editingDesc = g("editingDesc", "");
    const editingTitle = g("editingTitle", "");
    const editingTitleScope = g("editingTitleScope", "");
    const cardTab = g("cardTab", "current");
    const agentLogo = d.agentLogo || (() => "");
    const laneSelectHTML = d.laneSelectHTML || (() => "");
    const flip = cardTimeFlip;
    return `
    <div class="card ${cardState(n)} ${sel === n.id ? "selected" : ""} ${expanded.has(n.id) ? "expanded" : ""} ${!n.lane_id ? "lane-edit" : ""} ${actionCard === n.id ? "actions-open" : ""} ${editingDesc === n.id ? "editing" : ""} ${editingTitle === n.id && editingTitleScope === "cards" ? "title-editing" : ""}" ${cardTab === "pinned" ? 'draggable="true" ' : ""}id="card-${escape(n.id)}">
      <span class="stripe" style="background:${escape(d.laneColor ? d.laneColor(n.lane_id) : "")}"></span>
      <button class="go" data-open="${escape(n.id)}" aria-label="open ${escape(n.title)}"></button>
      <div class="row1">
        <span class="agent-logo" title="${escape(n.agent || "agent")}">${agentLogo(n.agent)}</span>
        ${editingTitle === n.id && editingTitleScope === "cards"
          ? `<input class="title-edit" data-title-input="${escape(n.id)}" value="${escape(n.title)}">`
          : `<span class="title" data-title="${escape(n.id)}">${escape(n.title)}</span>`}
        <button class="save-desc" data-save-desc="${escape(n.id)}" aria-label="save description">${icons.ICON_CHECK || ""}</button>
        ${pinned ? `<span class="pinflag" title="pinned" aria-label="pinned">${icons.ICON_PIN || ""}</span>` : ""}
        <button class="chev" data-x="${escape(n.id)}" aria-label="summary">&#8250;</button>
      </div>
      <div class="status">${cardState(n) === "working" ? '<span class="workdot"></span>' : ""}<span class="st">${cardMetaHTML(n, flip, { escape, ageFn, statusText })}</span></div>
      ${!n.lane_id ? `<div class="lanebox" data-card-lane="${escape(n.id)}">
        <div class="lanepick">
          <span class="laneswatch"></span>
          ${laneSelectHTML("", true)}
        </div>
        <input data-lane-new="${escape(n.id)}" hidden placeholder="New lane name">
      </div>` : railboxHTML(n)}
      <div class="summary" data-desc="${escape(n.id)}">${escape(desc || "No description yet.")}</div>
      <textarea class="descbox" data-desc-input="${escape(n.id)}">${escape(desc)}</textarea>
      <div class="actions roundactions">
        <button class="pin" data-pin-action="${escape(n.id)}" aria-label="${pinned ? "unpin" : "pin"}">${pinned ? (icons.ICON_UNPIN || "") : (icons.ICON_PIN || "")}</button>
        ${n.ended_at ? "" : `<button class="endthread" data-exit="${escape(n.id)}" aria-label="close thread (mark ended)">${icons.ICON_END || ""}</button>`}
        <button class="archive" data-arch-action="${escape(n.id)}" aria-label="${archived ? "restore" : "archive"}">${archived ? (icons.ICON_RESTORE || "") : (icons.ICON_ARCHIVE || "")}</button>
        <button class="trash" data-trash="${escape(n.id)}" aria-label="remove">${icons.ICON_TRASH || ""}</button>
      </div>
    </div>`;
  }

  function renderCardTabs(){
    if (!tabs) return;
    const laneFilter = g("laneFilter", "");
    const cardTab = g("cardTab", "current");
    const nodes = g("nodes", []);
    const archived = g("archived", []);
    const sel = g("sel", "");
    const currentCount = cardTabCount(nodes, {
      archivedTab: false, laneFilter, selectedId: sel, archived,
    });
    const archivedCount = cardTabCount(nodes, {
      archivedTab: true, laneFilter, selectedId: sel, archived,
    });
    const color = laneFilter && d.laneColor ? d.laneColor(laneFilter) : "";
    const name = laneFilter && d.laneName ? d.laneName(laneFilter) : "";
    tabs.innerHTML = cardTabsHTML({
      cardTab,
      laneFilter,
      laneColor: color,
      laneName: name,
      currentCount,
      archivedCount,
      pinnedCount: pinnedCount(),
      escape,
    });
  }

  function updateCardAges(animate = false){
    const nodes = g("nodes", []) || [];
    for (const n of nodes){
      const el = doc?.getElementById?.("card-" + CSSRef.escape(n.id).replace(/\\/g, ""));
      const card = doc?.getElementById?.("card-" + n.id) || el;
      if (!card) continue;
      const time = card.querySelector?.(".time");
      if (time) {
        const next = cardTimeText(n, cardTimeFlip, ageFn);
        if (time.textContent !== next) {
          time.textContent = next;
          if (animate) {
            time.classList.remove("roll-dn", "roll-up");
            void time.offsetWidth;
            time.classList.add("roll-dn");
          }
        }
        time.title = cardTimeTitle(n, ageFn);
      }
    }
  }

  function renderCards(){
    if (pinDragging) return;
    let cardTab = g("cardTab", "current");
    const nextTab = resolvePinnedTabFallback(cardTab, pinnedCount());
    if (nextTab !== cardTab){
      cardTab = nextTab;
      set("cardTab", cardTab);
    }
    const nodes = g("nodes", []) || [];
    const pinned = g("pinned", []);
    const archived = g("archived", []);
    const laneFilter = g("laneFilter", "");
    const sel = g("sel", "");
    let attnFoldOpen = g("attnFoldOpen", false);
    const vis = visibleCardLists(nodes, {
      cardTab, pinned, archived, laneFilter, selectedId: sel,
    });
    const cardArr = vis.list;
    const foldList = vis.foldList;
    if (!foldList.length){
      attnFoldOpen = false;
      set("attnFoldOpen", false);
    }
    const expanded = g("expanded", new Set());
    const unadopted = g("unadopted", []);
    const sig = computeCardsSignature({
      list: cardArr, foldList, unadopted, sel, expanded, cardTab, laneFilter,
      attnFoldOpen,
      bookmarksLen: (g("bookmarks", []) || []).length,
      lanes: g("lanes", []),
      pinned,
      actionCard: g("actionCard", ""),
      editingDesc: g("editingDesc", ""),
      editingTitle: g("editingTitle", ""),
    });
    const cardsSig = g("cardsSig", "");
    const decision = cardsRenderDecision(sig, cardsSig, false);
    if (decision === "ages"){ updateCardAges(); return; }
    set("cardsSig", sig);
    renderCardTabs();
    const adoptCards = adoptCardsHTML(unadopted, { cardTab, laneFilter, escape });
    const foldHtml = !foldList.length ? "" : (
      attnFoldButtonHTML(foldList.length, attnFoldOpen) +
      (attnFoldOpen ? foldList.map(cardHTML).join("") : "")
    );
    const editingDesc = g("editingDesc", "");
    withCardEditsPreserved(list, editingDesc, () => {
      if (!list) return;
      list.innerHTML = foldHtml + adoptCards + (cardArr.length
        ? cardArr.map(cardHTML).join("")
        : emptyCardsHTML({
          laneFilter,
          cardTab,
          hasAdoptCards: !!adoptCards,
        }));
    }, { document: doc, syncLanePicker: d.syncLanePicker });
    const waiting = nodes.filter(hardAttention).length;
    if (doc) doc.title = (waiting ? `(${waiting}) ` : "") + "scimux";
    if (list && d.syncLanePicker) {
      list.querySelectorAll?.("[data-lane-select]")?.forEach?.(d.syncLanePicker);
    }
  }

  function invalidate(){ set("cardsSig", ""); }

  function onTabsClick(e){
    if (e.target.closest("[data-clear-scope]")){
      if (typeof d.setLaneFilter === "function") d.setLaneFilter("");
      return;
    }
    const b = e.target.closest("[data-tab]");
    if (!b) return;
    set("cardTab", b.dataset.tab);
    set("actionCard", "");
    set("editingDesc", "");
    if (tabs) {
      tabs.querySelectorAll("button").forEach(x => x.classList.toggle("on", x === b));
    }
    invalidate();
    renderCards();
  }

  async function onListClick(e){
    if (e.target.closest("[data-attnfold]")){
      set("attnFoldOpen", !g("attnFoldOpen", false));
      invalidate();
      renderCards();
      return;
    }
    const save = e.target.closest("[data-save-desc]");
    if (save){
      const id = save.dataset.saveDesc;
      const ta = doc?.querySelector?.(`[data-desc-input="${CSSRef.escape(id)}"]`);
      const description = ta ? ta.value.trim() : "";
      const laneBox = doc?.querySelector?.(`[data-card-lane="${CSSRef.escape(id)}"]`);
      const laneChoice = laneBox && d.readLaneChoice
        ? d.readLaneChoice(laneBox.querySelector("[data-lane-select]"), laneBox.querySelector("[data-lane-new]"))
        : {};
      if (laneChoice.error){
        if (typeof d.fieldError === "function")
          d.fieldError(laneBox.querySelector("[data-lane-new]"), laneChoice.error);
        laneBox.querySelector("[data-lane-new]")?.focus();
        return;
      }
      const body = { description };
      if (laneChoice.laneID) body.lane_id = laneChoice.laneID;
      try {
        const n = await d.api(`/api/nodes/${encodeURIComponent(id)}`, {
          method: "PATCH",
          body: JSON.stringify(body),
        });
        if (laneChoice.lane && typeof d.uiMutate === "function") {
          const lanes = typeof d.laneList === "function" ? d.laneList() : g("lanes", []);
          d.uiMutate({ k: "lanes", lanes: [...lanes, laneChoice.lane] });
        }
        if (typeof d.updateLocalNode === "function") d.updateLocalNode(n);
        set("editingDesc", "");
        invalidate();
        if (typeof d.invalidateChatSig === "function") d.invalidateChatSig();
        if (typeof d.invalidateMapSig === "function") d.invalidateMapSig();
        renderCards();
        if (typeof d.renderChatHead === "function") d.renderChatHead();
        if (typeof d.renderMap === "function") d.renderMap();
      } catch (err) {
        if (typeof d.alert === "function") d.alert(err.message);
      }
      return;
    }
    const pinBtn = e.target.closest("[data-pin-action]");
    if (pinBtn){
      const id = pinBtn.dataset.pinAction;
      if (typeof d.uiMutate === "function")
        d.uiMutate({ k: isPinned(id) ? "unpin" : "pin", id });
      set("actionCard", "");
      invalidate();
      renderCards();
      return;
    }
    const endt = e.target.closest("[data-exit]");
    if (endt){
      set("actionCard", "");
      if (typeof d.exitThread === "function") d.exitThread(endt.dataset.exit);
      return;
    }
    const arch = e.target.closest("[data-arch-action]");
    if (arch){
      const id = arch.dataset.archAction;
      if (typeof d.uiMutate === "function")
        d.uiMutate({ k: isArchived(id) ? "unarch" : "arch", id });
      set("actionCard", "");
      invalidate();
      renderCards();
      return;
    }
    const trash = e.target.closest("[data-trash]");
    if (trash){
      const id = trash.dataset.trash;
      const n = nodeById(id);
      if (!n) return;
      const msg = n.adopted
        ? `Remove "${n.title}" from scimux? The adopted tmux session will keep running.`
        : `Remove "${n.title}" and close its running session/link?`;
      if (typeof d.confirm === "function" ? !d.confirm(msg) : !confirm(msg)) return;
      try {
        await d.api(`/api/nodes/${encodeURIComponent(id)}`, { method: "DELETE" });
        if (isArchived(id) && typeof d.uiMutate === "function") d.uiMutate({ k: "unarch", id });
        if (isPinned(id) && typeof d.uiMutate === "function") d.uiMutate({ k: "unpin", id });
        if (typeof d.removeNode === "function") d.removeNode(id);
        else {
          /* shell mutates nodes array */
        }
        if (g("sel", "") === id) set("sel", "");
        set("actionCard", "");
        set("editingDesc", "");
        if (typeof d.invalidateStateEtag === "function") d.invalidateStateEtag();
        invalidate();
        if (typeof d.invalidateChatSig === "function") d.invalidateChatSig();
        if (typeof d.invalidateMapSig === "function") d.invalidateMapSig();
        renderCards();
        if (typeof d.renderChatHead === "function") d.renderChatHead();
        if (typeof d.renderMap === "function") d.renderMap();
        if (typeof d.scheduleTick === "function") d.scheduleTick();
      } catch (err) {
        if (typeof d.alert === "function") d.alert(err.message);
      }
      return;
    }
    const activeCard = e.target.closest(".card.actions-open");
    if (activeCard){
      const id = activeCard.querySelector(".go")?.dataset.open;
      if (id && g("actionCard", "") === id){
        set("actionCard", "");
        invalidate();
        renderCards();
        return;
      }
    }
    const x = e.target.closest("[data-x]");
    if (x){
      const id = x.dataset.x;
      const expanded = g("expanded", new Set());
      if (expanded.has(id)) expanded.delete(id); else expanded.add(id);
      set("actionCard", "");
      invalidate();
      renderCards();
      return;
    }
    const ad = e.target.closest("[data-adopt]");
    if (ad){
      if (typeof d.openAdopt === "function") d.openAdopt(ad.dataset.adopt);
      return;
    }
    const go = e.target.closest("[data-open]");
    if (go){
      set("actionCard", "");
      set("editingDesc", "");
      if (typeof d.selectNode === "function") d.selectNode(go.dataset.open);
      if (typeof d.isDesktop === "function" ? !d.isDesktop() : false) {
        if (typeof d.setLevel === "function") d.setLevel(1);
      }
    }
  }

  function onTouchStart(e){
    const card = e.target.closest(".card");
    const id = card?.querySelector(".go")?.dataset.open;
    if (!id || card.classList.contains("expanded") || card.classList.contains("editing")) return;
    const t = e.touches[0];
    cardSwipe = { id, x: t.clientX, y: t.clientY };
  }
  function onTouchEnd(e){
    if (!cardSwipe) return;
    const t = e.changedTouches[0], dx = t.clientX - cardSwipe.x, dy = t.clientY - cardSwipe.y;
    const id = cardSwipe.id;
    cardSwipe = null;
    if (cardSwipeOpensActions(dx, dy)){
      set("actionCard", id);
      set("editingDesc", "");
      invalidate();
      renderCards();
    }
  }

  function onDragStart(e){
    if (g("cardTab", "current") !== "pinned") return;
    const card = e.target.closest(".card");
    if (!card || card.classList.contains("editing") || card.classList.contains("title-editing")) return;
    pinDragging = true;
    pinDragEl = card;
    card.classList.add("dragging");
    try { e.dataTransfer.effectAllowed = "move"; e.dataTransfer.setData("text/plain", ""); } catch { /* ignore */ }
  }
  function onDragOver(e){
    if (!pinDragging || !pinDragEl) return;
    e.preventDefault();
    const over = e.target.closest(".card");
    if (!over || over === pinDragEl) return;
    const r = over.getBoundingClientRect();
    const after = e.clientY > r.top + r.height / 2;
    over.parentNode.insertBefore(pinDragEl, after ? over.nextSibling : over);
  }
  function onDrop(e){ if (pinDragging) e.preventDefault(); }
  function onDragEnd(){
    if (!pinDragging) return;
    const order = [...(list?.querySelectorAll?.(".card") || [])]
      .map(c => c.querySelector(".go")?.dataset.open).filter(Boolean);
    pinDragEl?.classList.remove("dragging");
    pinDragEl = null;
    pinDragging = false;
    const next = pinOrderAfterDrag(order, g("pinned", []));
    if (typeof d.uiMutate === "function") d.uiMutate({ k: "pin-order", pinned: next });
    invalidate();
    renderCards();
  }

  function bind(){
    if (bound) return;
    bound = true;
    if (tabs) {
      tabs.addEventListener("click", onTabsClick);
      cleanups.push(() => tabs.removeEventListener("click", onTabsClick));
    }
    if (list) {
      list.addEventListener("click", onListClick);
      cleanups.push(() => list.removeEventListener("click", onListClick));
      list.addEventListener("touchstart", onTouchStart, { passive: true });
      cleanups.push(() => list.removeEventListener("touchstart", onTouchStart));
      list.addEventListener("touchend", onTouchEnd, { passive: true });
      cleanups.push(() => list.removeEventListener("touchend", onTouchEnd));
      list.addEventListener("dragstart", onDragStart);
      cleanups.push(() => list.removeEventListener("dragstart", onDragStart));
      list.addEventListener("dragover", onDragOver);
      cleanups.push(() => list.removeEventListener("dragover", onDragOver));
      list.addEventListener("drop", onDrop);
      cleanups.push(() => list.removeEventListener("drop", onDrop));
      list.addEventListener("dragend", onDragEnd);
      cleanups.push(() => list.removeEventListener("dragend", onDragEnd));
    }
    if (typeof d.longpress === "function" && list) {
      const titleCleanup = d.longpress(list, ".card .title", el => {
        if (typeof d.startTitleEdit === "function") d.startTitleEdit(el.dataset.title, "cards");
      });
      if (typeof titleCleanup === "function") cleanups.push(titleCleanup);
      const cardCleanup = d.longpress(list, ".card", (el, ev) => {
        if (el.classList.contains("adoptable")) return;
        if (el.classList.contains("expanded") || el.closest(".editing")) return;
        if (ev?.target?.closest(".title,input,textarea,select,button")) return;
        const id = el.querySelector(".go")?.dataset.open;
        const n = id && nodeById(id);
        if (!n) return;
        set("actionCard", g("actionCard", "") === id ? "" : id);
        set("editingDesc", "");
        invalidate();
        renderCards();
      });
      if (typeof cardCleanup === "function") cleanups.push(cardCleanup);
      const summaryCleanup = d.longpress(list, ".summary", el => {
        const id = el.dataset.desc;
        if (!id) return;
        const expanded = g("expanded", new Set());
        expanded.add(id);
        set("actionCard", "");
        set("editingDesc", id);
        invalidate();
        renderCards();
        const t = d.setTimeout || setTimeout;
        t(() => doc?.querySelector?.(`[data-desc-input="${CSSRef.escape(id)}"]`)?.focus(), 0);
      });
      if (typeof summaryCleanup === "function") cleanups.push(summaryCleanup);
    }
    ageTimer = setIntervalFn(() => {
      cardTimeFlip++;
      updateCardAges(true);
    }, CARD_TIME_SWAP_MS);
    cleanups.push(() => { if (ageTimer != null) clearIntervalFn(ageTimer); ageTimer = null; });
  }

  function destroy(){
    while (cleanups.length) {
      try { cleanups.pop()(); } catch { /* ignore */ }
    }
    bound = false;
    pinDragging = false;
    pinDragEl = null;
    cardSwipe = null;
  }

  return {
    render: renderCards,
    updateAges: updateCardAges,
    bind,
    destroy,
    invalidate,
  };
}
