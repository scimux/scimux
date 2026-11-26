/* Fixed, versioned People reaction model and inline presentation. */
import { esc } from "./format.js";

export const REACTIONS = Object.freeze([
  Object.freeze({id:"celebration", emoji:"🎉"}),
  Object.freeze({id:"thinking", emoji:"🤔"}),
  Object.freeze({id:"laughing", emoji:"😂"}),
  Object.freeze({id:"heart", emoji:"❤️"}),
  Object.freeze({id:"thumbs_down", emoji:"👎"}),
  Object.freeze({id:"thumbs_up", emoji:"👍"}),
]);

const byID = new Map(REACTIONS.map(item => [item.id, item.emoji]));
const faceSmile = '<svg class="reaction-trigger-glyph" viewBox="0 0 512 512" aria-hidden="true" focusable="false"><path fill="currentColor" d="M464 256A208 208 0 1 0 48 256a208 208 0 1 0 416 0zM0 256a256 256 0 1 1 512 0A256 256 0 1 1 0 256zm177.6 62.1C192.8 334.5 218.8 352 256 352s63.2-17.5 78.4-33.9c9-9.7 24.2-10.4 33.9-1.4s10.4 24.2 1.4 33.9c-22 23.8-60 49.4-113.6 49.4s-91.7-25.5-113.6-49.4c-9.7-9.7-8.4-24.9 1.4-33.9s24.9-8.4 33.9 1.4zM144.4 208a32 32 0 1 1 64 0 32 32 0 1 1 -64 0zm192-32a32 32 0 1 1 0 64 32 32 0 0 1 0-64z"/></svg>';

export function reactionEligible({shared, type, author, viewer, mine, access, state, online, text, attachments} = {}) {
  const anotherPerson = typeof mine === "boolean" ? mine === false : typeof author === "string" && author !== "" && typeof viewer === "string" && viewer !== "" && author !== viewer;
  return shared === true && type === "people-comment" && typeof author === "string" && author !== "" && anotherPerson &&
    (text !== "" || (Array.isArray(attachments) && attachments.length > 0)) &&
    access !== "read" && state === "live" && online === true;
}

/* The retained records that may change display state, in sequence order. Replay
   and pending settlement both use this one validation, so an unvalidated record
   can neither add a chip nor settle a local operation. */
export function acceptedReactionEvents(events, comments) {
  if (!Array.isArray(events) || !(comments instanceof Map)) return [];
  const accepted = [];
  let previous = 0;
  for (const event of events) {
    if (!event || !Number.isSafeInteger(event.seq) || event.seq <= previous) continue;
    previous = event.seq;
    if (Object.keys(event).some(key => !["v", "t", "seq", "key", "target", "target_author", "emoji", "active", "author", "mine"].includes(key))) continue;
    if (event.v !== 1 || event.t !== "reaction" || !Number.isSafeInteger(event.target) || event.target < 1 || event.target >= event.seq ||
        typeof event.emoji !== "string" || !byID.has(event.emoji) || typeof event.active !== "boolean" ||
        typeof event.author !== "string" || !event.author || typeof event.target_author !== "string" || !event.target_author || !/^r\/[0-9a-f]{16}$/.test(event.key || "")) continue;
    const target = comments.get(event.target);
    if (!target || target.valid !== true || target.author !== event.target_author || target.author === event.author) continue;
    accepted.push(event);
  }
  return accepted;
}

export function replayReactionEvents(events, comments, viewer = "") {
  const active = new Map();
  for (const event of acceptedReactionEvents(events, comments)) {
    active.set(`${event.target}\0${event.author}\0${event.emoji}`, {active:event.active, mine:event.mine === true});
  }
  const grouped = new Map();
  for (const [key, enabled] of active) {
    if (!enabled.active) continue;
    const [target, author, emoji] = key.split("\0");
    const groupKey = `${target}\0${emoji}`;
    if (!grouped.has(groupKey)) grouped.set(groupKey, {target:Number(target), emoji, glyph:byID.get(emoji), participants:new Map()});
    grouped.get(groupKey).participants.set(author, enabled.mine);
  }
  const order = new Map(REACTIONS.map((item, index) => [item.id, index]));
  return [...grouped.values()].map(group => ({
    target:group.target, emoji:group.emoji, glyph:group.glyph,
    count:group.participants.size, mine:[...group.participants.values()].some(Boolean) || group.participants.has(viewer),
  })).sort((a, b) => a.target - b.target || order.get(a.emoji) - order.get(b.emoji));
}

const UNCONFIRMED_LABEL = {pending:", pending confirmation", uncertain:", not confirmed"};
const UNCONFIRMED_TITLE = {pending:"Reaction pending confirmation", uncertain:"Reaction not confirmed; choose it again in the reaction menu to retry"};
const operationKey = (target, emoji) => target + "\0" + emoji;

/* Derives the sender's displayed reactions: the latest confirmed summary with
   the sender's local operations applied. Pure: neither argument is mutated, and
   operations are a presentation overlay, never retained events. An add counts the
   sender only if the confirmed summary does not already contain them; a removal
   subtracts only that confirmed contribution. Zero-count chips are dropped. */
export function projectReactionIntent(confirmed, operations = []) {
  const chips = new Map();
  for (const chip of Array.isArray(confirmed) ? confirmed : []) {
    chips.set(operationKey(chip.target, chip.emoji), {target:chip.target, emoji:chip.emoji, glyph:chip.glyph, count:chip.count, mine:chip.mine === true});
  }
  for (const operation of Array.isArray(operations) ? operations : []) {
    const key = operationKey(operation.target, operation.emoji);
    const chip = chips.get(key) || {target:operation.target, emoji:operation.emoji, glyph:byID.get(operation.emoji), count:0, mine:false};
    if (operation.active && !chip.mine) { chip.count += 1; chip.mine = true; }
    else if (!operation.active && chip.mine) { chip.count -= 1; chip.mine = false; }
    chip.unconfirmed = operation.state === "uncertain" ? "uncertain" : "pending";
    chips.set(key, chip);
  }
  const order = new Map(REACTIONS.map((item, index) => [item.id, index]));
  return [...chips.values()].filter(chip => chip.count > 0)
    .sort((a, b) => a.target - b.target || order.get(a.emoji) - order.get(b.emoji));
}

/* The active participants of one chip, as the disclosure lists them. Confirmed
   participants are the same validated records replay counts (latest state per
   author, in activation order). The sender's own operations then follow the same
   sequential rule as projectReactionIntent, so the roster length and its own flag
   always agree with the chip. A pending or uncertain state labels only the
   sender's contribution. Pure: nothing is mutated. */
export function reactionParticipants(events, comments, {target, emoji, operations = [], writable = true} = {}) {
  const latest = new Map();
  for (const event of acceptedReactionEvents(events, comments)) {
    if (event.target !== target || event.emoji !== emoji) continue;
    latest.set(event.author, {active:event.active, mine:event.mine === true, seq:event.seq});
  }
  const entries = [...latest].filter(([, state]) => state.active).sort((a, b) => a[1].seq - b[1].seq)
    .map(([author, state]) => ({author, mine:state.mine, state:"confirmed"}));
  if (!writable) return entries;
  let label = null;
  for (const operation of Array.isArray(operations) ? operations : []) {
    if (operation.target !== target || operation.emoji !== emoji) continue;
    const index = entries.findIndex(entry => entry.mine);
    if (operation.active && index < 0) entries.push({author:"", mine:true, state:"pending"});
    else if (!operation.active && index >= 0) entries.splice(index, 1);
    label = operation.state === "uncertain" ? "uncertain" : "pending";
  }
  const own = entries.find(entry => entry.mine);
  if (own && label) own.state = label;
  return entries;
}

export function reactionGlyph(emoji) {
  return byID.get(emoji) || "";
}

const joinNames = parts => parts.length === 1 ? parts[0] : parts.length === 2 ? `${parts[0]} and ${parts[1]}` : `${parts.slice(0, -1).join(", ")} and ${parts[parts.length - 1]}`;

/* Plain sentence for a disclosure. Up to four participants are named; a larger
   roster names three and counts the rest. Your own participation reads "you". A
   pending or uncertain own contribution is stated after the sentence. */
export function reactionPreviewText(entries, glyph) {
  if (!Array.isArray(entries) || entries.length === 0) return "";
  /* Other people's names are shown exactly as chosen; only the own word takes a capital at the start. */
  const names = entries.map((entry, index) => entry.mine ? (index === 0 ? "You" : "you") : entry.name);
  const parts = names.length <= 4 ? names : [...names.slice(0, 3), `${names.length - 3} others`];
  const text = `${joinNames(parts)} reacted with ${glyph}`;
  const own = entries.find(entry => entry.mine);
  if (own?.state === "pending") return `${text}. Your reaction is pending confirmation.`;
  if (own?.state === "uncertain") return `${text}. Your reaction is not confirmed.`;
  return text;
}

const rosterLabel = entry => {
  if (!entry.mine) return entry.name;
  if (entry.state === "pending") return "You — pending confirmation";
  if (entry.state === "uncertain") return "You — not confirmed";
  return "You";
};

/* Escaped markup for a disclosure. "hover" is the brief preview; "wide" is the
   expanded roster for a small anchored panel; "sheet" adds the explicit close
   control of the compact sheet. */
export function reactionRosterHTML(entries, glyph, mode = "wide") {
  const list = Array.isArray(entries) ? entries : [];
  const summary = `<p class="reaction-disclosure-summary">${esc(reactionPreviewText(list, glyph))}</p>`;
  if (mode === "hover") return summary;
  const items = list.map(entry => `<li>${esc(rosterLabel(entry))}</li>`).join("");
  const close = mode === "sheet" ? '<button type="button" class="reaction-disclosure-close" data-reaction-close>Close</button>' : "";
  return `${summary}<ul class="reaction-roster">${items}</ul>${close}`;
}

const DISCLOSURE_MARGIN = 8;
const DISCLOSURE_MAX_WIDTH = "18rem";
/* A chat pane at or under this width is compact: pinned disclosures are sheets. Same breakpoint as the chooser. */
export const REACTION_COMPACT_WIDTH = 480;
/* The width cap is the chat's, never the bubble's, so a readable roster can extend past a short bubble. */
export function reactionDisclosureMaxWidth(bounds) {
  return `min(${DISCLOSURE_MAX_WIDTH}, ${Math.max(0, bounds.width - 2 * DISCLOSURE_MARGIN)}px)`;
}
/* Where a disclosure goes relative to its chip, in the same viewport coordinates
   as the rects. A panel goes above the chip when that side has room, or else on
   the side with more room. A pinned panel is a wide panel only in a non-compact
   chat, when it fits the chat's width and one vertical side has room for it;
   otherwise it becomes a sheet. */
export function reactionDisclosureSpot(chip, bounds, size) {
  const roomAbove = chip.top - bounds.top;
  const roomBelow = bounds.bottom - chip.bottom;
  const above = roomAbove >= size.height + DISCLOSURE_MARGIN || roomAbove >= roomBelow;
  const fitsWidth = size.width <= bounds.width - 2 * DISCLOSURE_MARGIN;
  const fitsHeight = Math.max(roomAbove, roomBelow) >= size.height + DISCLOSURE_MARGIN;
  const compact = bounds.width <= REACTION_COMPACT_WIDTH;
  const x = Math.max(bounds.left + DISCLOSURE_MARGIN, Math.min(chip.left, bounds.right - DISCLOSURE_MARGIN - size.width));
  return {above, fits:!compact && fitsWidth && fitsHeight, x, y:above ? chip.top : chip.bottom};
}

export function reactionChipsHTML(chips, target = null) {
  if (!Array.isArray(chips)) return "";
  return chips.filter(chip => chip && (target === null || chip.target === target) && Number.isSafeInteger(chip.count) && chip.count > 0 && Number.isSafeInteger(chip.target) && chip.target > 0 && byID.get(chip.emoji) === chip.glyph)
    .map(chip => {
      const state = ["pending", "uncertain"].includes(chip.unconfirmed) ? chip.unconfirmed : "";
      const classes = "reaction-chip" + (chip.mine ? " mine" : "") + (state ? " reaction-pending" : "");
      const label = chip.glyph + " " + chip.count + (chip.mine ? ", you reacted" : "") + (state ? UNCONFIRMED_LABEL[state] : "");
      const title = state ? ` title="${UNCONFIRMED_TITLE[state]}"` : "";
      /* The label keeps the state contract; the action is its description, and the chip is a disclosure. */
      return `<button class="${classes}" data-reaction="${chip.emoji}" aria-label="${label}"${title} aria-description="Show who reacted" type="button" aria-haspopup="dialog" aria-expanded="false" data-rtarget="${chip.target}"><span aria-hidden="true">${chip.glyph}</span><span>${chip.count}</span></button>`;
    }).join("");
}

export function reactionControlHTML({target = 0, key = "", copyInstance = ""} = {}) {
  const choices = REACTIONS.map(item => `<button type="button" class="reaction-choice" data-reaction-choice="${item.id}" aria-label="${item.emoji}">${item.emoji}</button>`).join("");
  const copy = /^[0-9a-f]{32}$/.test(copyInstance) ? copyInstance : "";
  const safeKey = /^[0-9a-f]{32}\/[0-9a-f]{32}\/[0-9a-f]{16}\/[0-9a-f]{16}$/.test(key) ? key : "";
  const safeTarget = Number.isSafeInteger(target) && target > 0 ? target : 0;
  return `<span class="reaction-entry" data-reaction-entry data-rkey="${safeKey}" data-rtarget="${safeTarget}" data-copy-instance="${copy}"><button type="button" class="reaction-trigger" data-reaction-open aria-label="React with emoji." title="React with emoji" aria-expanded="false">${faceSmile}</button><span class="reaction-chooser" role="group" aria-label="Choose a reaction" hidden>${choices}</span></span>`;
}

/* One interaction owner for the smile picker and the participant disclosure.
   The picker is unchanged. The disclosure is either a hover preview (pointer only,
   never focus, never a keyboard listener) or a pinned panel from a click or tap.
   Only one of the two exists at a time, and neither writes anything. */
export function createReactionInteraction({doc, onChoose, roster = () => null}) {
  let opened = null;
  let active = null;
  function closePicker() {
    if (!opened) return;
    const trigger = opened.querySelector("[data-reaction-open]");
    const chooser = opened.querySelector(".reaction-chooser");
    if (chooser) chooser.hidden = true;
    if (trigger) trigger.setAttribute("aria-expanded", "false");
    opened.classList.remove("open");
    opened.classList.remove("below");
    opened.classList.remove("rightward");
    opened.classList.remove("compact");
    opened.classList.remove("stacked");
    opened.classList.remove("detached");
    opened = null;
  }
  function onKeydown(event) {
    sync();
    if (event.key !== "Escape" || !active) return;
    event.preventDefault();
    event.stopPropagation();
    closeDisclosure({restore:true});
  }
  function closeDisclosure({restore = false} = {}) {
    if (!active) return;
    const state = active;
    active = null;
    const focusInside = state.panel.contains(doc?.activeElement);
    state.panel.remove();
    if (state.pinned) {
      state.chip.setAttribute("aria-expanded", "false");
      doc.removeEventListener("keydown", onKeydown, true);
      if (restore && focusInside) state.chip.focus({preventScroll:true});
    }
  }
  function stop(event) {
    event.preventDefault();
    event.stopPropagation();
  }
  function disclosureData(chip) {
    const key = chip.closest("[data-gkey]")?.dataset.gkey || "";
    const target = Number(chip.dataset.rtarget);
    const emoji = chip.dataset.reaction;
    const entries = roster(key, target, emoji);
    return Array.isArray(entries) && entries.length > 0 ? {key, target, emoji, entries, glyph:reactionGlyph(emoji)} : null;
  }
  /* Renders the current state into its panel: content only when it changed, the
     side and offsets from the live rects, and a sheet when a pinned panel cannot
     fit. A detached panel is reattached inside the chip's current bubble. */
  function paint(state) {
    const bubble = state.chip.closest(".bubble");
    const mode = !state.pinned ? "hover" : state.sheet ? "sheet" : "wide";
    const html = reactionRosterHTML(state.data.entries, state.data.glyph, mode);
    /* Replacing the markup destroys any focused descendant. Focus that was inside the panel moves to the
       rebuilt close control when there is one, else to the panel; focus outside the panel is never taken. */
    const focusInside = state.panel.contains(doc?.activeElement);
    if (html !== state.html) { state.panel.innerHTML = html; state.html = html; }
    if (focusInside && !state.panel.contains(doc?.activeElement)) (state.panel.querySelector?.("[data-reaction-close]") || state.panel).focus({preventScroll:true});
    state.panel.classList.remove("reaction-hover", "reaction-sheet", "above");
    if (mode === "hover") state.panel.classList.add("reaction-hover");
    if (mode === "sheet") state.panel.classList.add("reaction-sheet");
    if (!bubble.contains(state.panel)) bubble.appendChild(state.panel);
    if (mode === "sheet") { state.panel.style.left = ""; state.panel.style.top = ""; state.panel.style.maxWidth = ""; return; }
    const bounds = doc?.querySelector?.("#msgs")?.getBoundingClientRect?.() || {left:0, top:0, right:0, bottom:0, width:0};
    state.panel.style.maxWidth = reactionDisclosureMaxWidth(bounds);
    const spot = reactionDisclosureSpot(state.chip.getBoundingClientRect(), bounds, state.panel.getBoundingClientRect());
    if (state.pinned && !spot.fits) { state.sheet = true; paint(state); return; }
    const frame = bubble.getBoundingClientRect();
    if (spot.above) state.panel.classList.add("above");
    state.panel.style.left = `${spot.x - frame.left}px`;
    state.panel.style.top = `${spot.y - frame.top}px`;
  }
  function pin(state) {
    state.pinned = true;
    state.chip.setAttribute("aria-expanded", "true");
    paint(state);
    state.panel.setAttribute("role", "dialog");
    state.panel.setAttribute("aria-label", `People who reacted with ${state.data.glyph}`);
    state.panel.setAttribute("tabindex", "-1");
    state.panel.focus({preventScroll:true});
    doc.addEventListener("keydown", onKeydown, true);
  }
  function openDisclosure(chip, data, pinned) {
    const bubble = chip.closest(".bubble");
    if (!bubble) return;
    const panel = doc.createElement("div");
    panel.classList.add("reaction-disclosure");
    panel.setAttribute("role", "tooltip");
    active = {chip, data, pinned:false, sheet:false, panel, html:null};
    paint(active);
    if (pinned) pin(active);
  }
  /* A chip click: a pinned disclosure on this chip closes; a preview on this chip
     becomes pinned in place; any other chip opens its own pinned disclosure. */
  function pinChip(chip) {
    const current = active?.chip === chip ? active : null;
    if (current?.pinned) { closeDisclosure({restore:true}); return; }
    const data = disclosureData(chip);
    if (!data) { closeDisclosure(); return; }
    if (current) { current.data = data; pin(current); return; }
    closePicker();
    closeDisclosure();
    openDisclosure(chip, data, true);
  }
  function findChip(state) {
    const candidates = [...(doc?.querySelectorAll?.(".reaction-chip") || [])];
    return candidates.find(chip => chip.dataset.reaction === state.data.emoji && chip.dataset.rtarget === String(state.data.target) &&
      chip.closest("[data-gkey]")?.dataset.gkey === state.data.key) || null;
  }
  /* Keeps an open disclosure true to the live chip and its data. A replaced chip
     is re-anchored; a chip that is gone, or data that is no longer available,
     closes it. Called before each pointer or click is interpreted. */
  function sync() {
    if (!active) return;
    const state = active;
    if (!state.chip.isConnected) {
      const replacement = findChip(state);
      if (!replacement) { closeDisclosure(); return; }
      state.chip = replacement;
      replacement.setAttribute("aria-expanded", state.pinned ? "true" : "false");
    }
    const data = disclosureData(state.chip);
    if (!data) { closeDisclosure(); return; }
    state.data = data;
    paint(state);
  }
  function inside(node) {
    return !!active && !!node && (active.chip.contains(node) || active.panel.contains(node));
  }
  /* A hover preview belongs to its message bubble, which holds its chip and its panel. A diagonal
     pointer path crosses the bubble's padding and row before it reaches the panel, so the whole
     bubble is the zone; leaving the bubble is what dismisses a preview. */
  function hoverZone(node) {
    if (!active || !node) return false;
    return !!active.chip.closest?.(".bubble")?.contains(node);
  }
  function hover(event) {
    sync();
    if (event.pointerType !== "mouse") return;
    if (event.type === "pointerover") {
      if (opened) return;
      const chip = event.target?.closest?.(".reaction-chip");
      if (!chip || active?.pinned || active?.chip === chip) return;
      const data = disclosureData(chip);
      if (!data) return;
      closeDisclosure();
      openDisclosure(chip, data, false);
      return;
    }
    if (event.type === "pointerout" && active && !active.pinned && hoverZone(event.target) && !hoverZone(event.relatedTarget)) closeDisclosure();
  }
  function handle(event) {
    sync();
    const target = event.target;
    const pick = target?.closest?.("[data-reaction-open], [data-reaction-choice]");
    if (!pick) {
      if (target?.closest?.(".reaction-disclosure, .reaction-sheet")) {
        stop(event);
        if (target.closest("[data-reaction-close]")) closeDisclosure({restore:true});
        return true;
      }
      const chip = target?.closest?.(".reaction-chip");
      if (chip) { stop(event); pinChip(chip); return true; }
      if (opened && !opened.contains(target)) closePicker();
      if (active && !inside(target)) closeDisclosure();
      return false;
    }
    stop(event);
    closeDisclosure();
    const entry = pick.closest("[data-reaction-entry]");
    if (!entry) return true;
    if (pick.matches("[data-reaction-choice]")) {
      const selected = REACTIONS.find(item => item.id === pick.dataset.reactionChoice);
      const destination = {key:entry.dataset.rkey, target:Number(entry.dataset.rtarget), copyInstance:entry.dataset.copyInstance, emoji:selected?.id || ""};
      closePicker();
      if (selected) onChoose?.(destination);
      return true;
    }
    if (opened === entry) { closePicker(); return true; }
    closePicker();
    const trigger = entry.querySelector("[data-reaction-open]");
    const chooser = entry.querySelector(".reaction-chooser");
    if (!trigger || !chooser) return true;
    opened = entry;
    chooser.hidden = false;
    trigger.setAttribute("aria-expanded", "true");
    entry.classList.add("open");
    const rect = trigger.getBoundingClientRect?.();
    const chooserRect = chooser.getBoundingClientRect?.();
    const chatRect = doc?.querySelector?.("#msgs")?.getBoundingClientRect?.();
    if (chooserRect && chatRect && rect && Number.isFinite(chatRect.left) && Number.isFinite(chatRect.right) &&
        Number.isFinite(rect.left) && Number.isFinite(rect.right) && Number.isFinite(chooserRect.width)) {
      const gap = 4;
      const leftRoom = Math.max(0, rect.left - chatRect.left - gap);
      const rightRoom = Math.max(0, chatRect.right - rect.right - gap);
      const room = Math.max(leftRoom, rightRoom);
      if (chooserRect.width > room) {
        entry.classList.add("compact");
        const compactRect = chooser.getBoundingClientRect?.();
        if (compactRect && compactRect.width > room && compactRect.width > chatRect.right - chatRect.left) entry.classList.add("stacked");
      }
      const finalRect = chooser.getBoundingClientRect?.();
      const finalWidth = finalRect?.width || 0;
      if (leftRoom >= finalWidth) entry.classList.remove("rightward");
      else if (rightRoom >= finalWidth) entry.classList.add("rightward");
      else {
        entry.classList.add("detached");
        const entryRect = entry.getBoundingClientRect();
        entry.style.setProperty("--reaction-chat-center", `${chatRect.left + (chatRect.right - chatRect.left) / 2 - entryRect.left}px`);
      }
    }
    const finalRect = chooser.getBoundingClientRect?.();
    const height = finalRect?.height || chooserRect?.height || 0;
    const viewport = doc?.defaultView?.innerHeight || 0;
    const topEdge = Number.isFinite(chatRect?.top) ? chatRect.top : 0;
    const bottomEdge = Number.isFinite(chatRect?.bottom) ? chatRect.bottom : viewport;
    if (rect && rect.top - topEdge < height + 8 && bottomEdge - rect.bottom >= height + 8) entry.classList.add("below");
    return true;
  }
  function close() {
    closePicker();
    closeDisclosure();
  }
  return {handle, hover, sync, close, current:() => opened, disclosure:() => active && {chip:active.chip, pinned:active.pinned, sheet:active.sheet}};
}
