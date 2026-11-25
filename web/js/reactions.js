/* Fixed, versioned People reaction model and inline presentation. */
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

export function replayReactionEvents(events, comments, viewer = "") {
  if (!Array.isArray(events) || !(comments instanceof Map)) return [];
  const active = new Map();
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

export function reactionChipsHTML(chips, target = null) {
  if (!Array.isArray(chips)) return "";
  return chips.filter(chip => chip && (target === null || chip.target === target) && Number.isSafeInteger(chip.count) && chip.count > 0 && byID.get(chip.emoji) === chip.glyph)
    .map(chip => `<span class="reaction-chip${chip.mine ? " mine" : ""}" data-reaction="${chip.emoji}"${chip.mine ? ' aria-label="' + chip.glyph + " " + chip.count + ', you reacted" aria-pressed="true"' : ' aria-label="' + chip.glyph + " " + chip.count + '" aria-pressed="false"'}><span aria-hidden="true">${chip.glyph}</span><span>${chip.count}</span></span>`).join("");
}

export function reactionControlHTML({target = 0, key = "", copyInstance = ""} = {}) {
  const choices = REACTIONS.map(item => `<button type="button" class="reaction-choice" data-reaction-choice="${item.id}" aria-label="${item.emoji}">${item.emoji}</button>`).join("");
  const copy = /^[0-9a-f]{32}$/.test(copyInstance) ? copyInstance : "";
  const safeKey = /^[0-9a-f]{32}\/[0-9a-f]{32}\/[0-9a-f]{16}\/[0-9a-f]{16}$/.test(key) ? key : "";
  const safeTarget = Number.isSafeInteger(target) && target > 0 ? target : 0;
  return `<span class="reaction-entry" data-reaction-entry data-rkey="${safeKey}" data-rtarget="${safeTarget}" data-copy-instance="${copy}"><span class="reaction-divider" aria-hidden="true">│</span><button type="button" class="reaction-trigger" data-reaction-open aria-label="React with emoji." title="React with emoji" aria-expanded="false">${faceSmile}</button><span class="reaction-chooser" role="group" aria-label="Choose a reaction" hidden>${choices}</span></span>`;
}

export function createReactionInteraction({doc, onChoose}) {
  let opened = null;
  function close() {
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
  function stop(event) {
    event.preventDefault();
    event.stopPropagation();
  }
  function handle(event) {
    const target = event.target?.closest?.("[data-reaction-open], [data-reaction-choice]");
    if (!target) {
      if (opened && !opened.contains(event.target)) close();
      return false;
    }
    stop(event);
    const entry = target.closest("[data-reaction-entry]");
    if (!entry) return true;
    if (target.matches("[data-reaction-choice]")) {
      const selected = REACTIONS.find(item => item.id === target.dataset.reactionChoice);
      const destination = {key:entry.dataset.rkey, target:Number(entry.dataset.rtarget), copyInstance:entry.dataset.copyInstance, emoji:selected?.id || ""};
      close();
      if (selected) onChoose?.(destination);
      return true;
    }
    if (opened === entry) { close(); return true; }
    close();
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
  return {handle, close, current:() => opened};
}
