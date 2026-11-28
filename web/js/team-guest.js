/* Shared subscription list and read-only guest view. */
import { esc, md, stripAssetRefs, bubbleTitle, bubbleSpeaker } from "./format.js";
import { fixedProviderVendor, sharingTermsAnchor } from "./harness.js";
import { usageAgentDisplayName } from "./usage.js";
import { pinnedOrder } from "./map-model.js";
import { promptSourceKey } from "./storage.js";
import { peoplePendingStorageKey, parsePeoplePendingSnapshot } from "./people-local-review.js";
import { attachmentTileHTML, attachmentUnavailableHTML } from "./attachment-tile.js";
import { activateBubbleKey, routeBubbleClick, renderBubbleTurn, renderActionRow, createBubbleActionController, dispatchBubbleAction } from "./menu.js";
import { REACTIONS, reactionEligible, reactionControlHTML, reactionChipsHTML, replayReactionEvents, acceptedReactionEvents, projectReactionIntent, reactionParticipants, createReactionInteraction } from "./reactions.js";

const LIST = "/api/team/subscriptions";

export function guestKey(view) {
  return [view.server, view.owner, view.chat, view.generation].join("/");
}

function copyQuery(view) {
  return /^[0-9a-f]{32}$/.test(view?.people_copy_instance || "") ? "&copy_instance=" + view.people_copy_instance : "";
}

function copyIdentity(view) { return view?.people_copy_instance || ""; }
function classificationKey(view) { return guestKey(view) + "\0" + copyIdentity(view); }

export function guestName(server, seat, serverName, people = {}) {
  const local = people?.[server + "/" + seat];
  if (typeof local === "string" && local) return local;
  return serverName || "Unknown person";
}

function evidencedAgents(entries) {
  const rows = [];
  for (const entry of entries) {
    if (entry.t !== "assistant" || typeof entry.agent !== "string" || !entry.agent) continue;
    rows.push(entry);
  }
  rows.sort((a, b) => a.seq - b.seq);
  const seen = new Set();
  const agents = [];
  for (const entry of rows) {
    if (seen.has(entry.agent)) continue;
    seen.add(entry.agent);
    agents.push(entry.agent);
  }
  return agents;
}

function joinAgents(parts) {
  if (parts.length === 1) return parts[0];
  if (parts.length === 2) return parts[0] + " and " + parts[1];
  return parts.slice(0, -1).join(", ") + ", and " + parts[parts.length - 1];
}

/* Presentation only. Callers pass mirror entries already read; this does not
 * fetch, and the result is not a turn. */
export function sharingNoticeHTML(owner, entries) {
  const agents = evidencedAgents(entries);
  const parts = [];
  const linked = [];
  let unspecified = agents.length === 0;
  for (const agent of agents) {
    const vendor = fixedProviderVendor(agent);
    const name = esc(usageAgentDisplayName(agent));
    if (!vendor) {
      unspecified = true;
      parts.push(name);
    } else {
      parts.push(name + " (" + esc(vendor) + ")");
      linked.push({ agent, vendor });
    }
  }
  const lead = agents.length
    ? "Shared by " + esc(owner) + " · Created with " + joinAgents(parts)
    : "Shared by " + esc(owner) + " · Includes AI-generated responses";
  let terms = "provider terms";
  if (linked.length === 1) terms = sharingTermsAnchor(linked[0].agent, "provider terms");
  else if (linked.length > 1) terms = "provider terms (" + linked.map(item => sharingTermsAnchor(item.agent, item.vendor)).join(", ") + ")";
  const caveat = unspecified ? " Terms depend on the configured provider." : "";
  const who = esc(owner);
  return `<div class="sharing-notice" role="note"><p class="sharing-notice-lead">${lead}</p><p class="sharing-notice-body">AI responses may be inaccurate. Verify before relying on them. Preserve attribution and respect applicable rights and ${terms}. Only ${who} can send to the agent.${caveat}</p></div>`;
}

export function guestCapture(entry, view, names, people = {}) {
  if (entry.omitted !== undefined) return null;
  const comment = entry.t === undefined;
  const files = comment ? sharedAttachmentDescriptors(entry) : [];
  if (!entry.text && !files?.length) return null;
  if (!comment) {
    if (entry.t !== "user" && entry.t !== "assistant") return null;
  }
  let role = "user";
  if (entry.t === "assistant") role = "assistant";
  else if (comment) {
    if (!entry.mine) role = "person";
  } else if (view.access !== "owner") role = "person";
  const capture = { text: stripAssetRefs(entry.text || ""), role };
  if (files?.length) capture.attachments = files;
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

function isLaneIndex(view) {
  const lane = view.lane;
  return !!lane && typeof lane === "object" && !Array.isArray(lane);
}

function laneLabel(index) {
  const name = index.lane.name;
  return typeof name === "string" ? name : "";
}

function sortedLaneIndexes(views) {
  return views.filter(isLaneIndex).sort((a, b) => {
    const order = compare(laneLabel(a), laneLabel(b));
    if (order) return order;
    return String(a.chat).localeCompare(String(b.chat));
  });
}

function indexListsChat(index, chat) {
  const chats = index.lane.chats;
  if (!Array.isArray(chats)) return false;
  for (const entry of chats) if (entry && entry.chat === chat) return true;
  return false;
}

export function laneForChat(view, subs) {
  if (!view || isLaneIndex(view)) return null;
  const same = (subs || []).filter(item => item.server === view.server && item.owner === view.owner);
  const index = sortedLaneIndexes(same).find(item => indexListsChat(item, view.chat));
  if (!index) return null;
  return {
    name: laneLabel(index),
    color: typeof index.lane.color === "string" ? index.lane.color : "",
    index: index.chat,
  };
}

function assignLanes(group) {
  const taken = new Set();
  const placed = new Set();
  group.lanes = sortedLaneIndexes(group.items).map(index => {
    const items = [];
    for (const item of group.items) {
      if (isLaneIndex(item) || taken.has(item.chat) || !indexListsChat(index, item.chat)) continue;
      taken.add(item.chat);
      placed.add(item);
      items.push(item);
    }
    return {
      key: group.key + "/lane/" + index.chat,
      label: laneLabel(index),
      color: typeof index.lane.color === "string" ? index.lane.color : "",
      items,
    };
  });
  group.items = group.items.filter(item => !isLaneIndex(item) && !placed.has(item));
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
    assignLanes(group);
  }
  return result;
}

export function createGuestFeature({ api, doc, now, invalidateCards, randomBytes, uiMutate, bookmarks, openSendTo, toast, retainPeople, assetURL = path => path, people = () => ({}), localNodes = () => [], panel, bubbleController = createBubbleActionController(), storage = null, peopleFileStore = null, selectedNode, selectedUID, localAgentAvailable, confirmRemove, classificationTimeoutMs = 8000 }) {
  let off = false;
  let destroyed = false;
  let answer = { subscriptions: [], servers: [], truncated: false };
  let loading = false;
  let lastLoad = null;
  let refreshPending = false;
  let hasAnswer = false;
  const pending = new Set();
  let bound = false;
  let removeBubbleListeners = null;
  let removeReactionListeners = null;
  let open = null;
  let owner = null;
  const ownerCopies = new Map();
  let actionOwner = null;
  let pendingOwnerJump = null;
  let ownerShare = null;
  let readGeneration = 0;
  let knownPeople = {};
  let disclosure = null;
  let disclosureCopy = null;
  let noticeHTML = "";
  let disclosureHTML = "";
  const classificationJobs = new Map();
  const classificationQueue = [];
  const classificationProgress = new Map();
  const classificationResults = new Map();
  const classificationCancels = new Set();
  const retentionCancels = new Set();
  let classificationActive = 0;
  const reactionUI = createReactionInteraction({doc, onChoose:chooseReaction, roster:reactionRoster});

  function reactionComments(current) {
    const comments = new Map();
    for (const [key, entry] of current.entries) {
      if (!key.startsWith("comments/") || !entry || !Number.isSafeInteger(entry.seq)) continue;
      const attachments = sharedAttachmentDescriptors(entry);
      comments.set(entry.seq, {author:entry.author, valid:!!entry.text || !!attachments?.length});
    }
    return comments;
  }
  function reactionSummary(current) {
    return replayReactionEvents(current.reactionEvents, reactionComments(current));
  }
  function reactionOnline(view) {
    const server = answer.servers.find(item => item.id === view.server);
    return view.state === "live" && !!server && !["offline", "refused", "changed"].includes(server.delivery);
  }
  function reactionWritable(current) {
    return reactionOnline(current.view) && current.view.access !== "read" && current.view.state === "live";
  }
  /* What the sender sees: confirmed reactions with local operations applied.
     Read-only, offline and ended copies show confirmed state only. */
  function reactionDisplay(current) {
    const confirmed = reactionSummary(current);
    if (!reactionWritable(current)) return confirmed;
    return projectReactionIntent(confirmed, [...current.reactionPending.values()]);
  }
  /* The copy a disclosure reads: the open shared view, or a current owner copy. */
  function reactionCopyFor(key) {
    if (open && open.key === key) return open;
    const copy = ownerCopies.get(key);
    return copy && ownerCurrent(copy) ? copy : null;
  }
  /* Participants of one chip for its disclosure. Confirmed participants come from
     the retained records; this viewer's pending operations apply only where the
     copy accepts writes, so read-only, offline and ended copies show confirmed
     state only. Names follow the guest rules; your own entries are labelled "You"
     by the presentation, so they never carry a name. */
  function reactionRoster(key, target, emoji) {
    const current = reactionCopyFor(key);
    if (!current || current.reactionRetired) return null;
    const operations = reactionWritable(current) ? [...current.reactionPending.values()] : [];
    return reactionParticipants(current.reactionEvents, reactionComments(current), {target, emoji, operations}).map(entry => ({
      name: entry.mine ? "" : guestName(current.view.server, entry.author, current.names[entry.author], knownPeople),
      mine: entry.mine, state: entry.state,
    }));
  }
  /* A pending operation settles only on a validated retained record for the same
     key, target, emoji and direction, written by this viewer. A removal settles
     even though it leaves no aggregate chip. Raw page records never settle one. */
  function settleReactionOperations(current) {
    if (current.reactionPending.size === 0) return;
    const accepted = acceptedReactionEvents(current.reactionEvents, reactionComments(current));
    for (const [key, pending] of current.reactionPending) {
      if (accepted.some(event => event.mine === true && event.key === "r/" + pending.key && event.target === pending.target && event.emoji === pending.emoji && event.active === pending.active)) {
        current.reactionPending.delete(key);
      }
    }
  }
  function syncReactionChips(current) {
    settleReactionOperations(current);
    const summaries = reactionDisplay(current);
    for (const turn of doc.querySelectorAll("#msgs [data-cseq]")) {
      if (turn.dataset.gkey !== current.key) continue;
      const container = turn.querySelector("[data-reaction-chips]");
      if (container) {
        const html = reactionChipsHTML(summaries, Number(turn.dataset.cseq));
        if (container.innerHTML !== html) container.innerHTML = html;
      }
      const entry = current.entries.get("comments/" + turn.dataset.cseq);
      if (!entry) continue;
      const attachments = sharedAttachmentDescriptors(entry);
      const eligible = reactionEligible({shared:true, type:"people-comment", author:entry.author, mine:entry.mine,
        access:current.view.access, state:current.view.state, online:reactionOnline(current.view), text:entry.text || "", attachments});
      const control = turn.querySelector("[data-reaction-entry]");
      if (!eligible || !/^[0-9a-f]{32}$/.test(current.reactionCopyInstance || "")) {
        control?.remove?.();
      } else if (control) {
        control.dataset.copyInstance = current.reactionCopyInstance;
      } else {
        turn.querySelector(".bubble")?.insertAdjacentHTML("beforeend", reactionControlHTML({target:entry.seq, key:current.key, copyInstance:current.reactionCopyInstance}));
      }
    }
    reactionUI.sync();
  }
  function rememberReactionEvents(current, events) {
    let changed = false;
    const seen = new Set(current.reactionEvents.map(event => event.seq));
    for (const event of events) {
      if (seen.has(event.seq)) continue;
      current.reactionEvents.push(event);
      seen.add(event.seq);
      changed = true;
    }
    return changed;
  }

  async function readReactionPages(current, isCurrent) {
    if (current.reactionRetired) return true;
    for (let pageNumber = 0; pageNumber < 40; pageNumber++) {
      const previous = current.rnext || 0;
      const instance = current.reactionCopyInstance || "";
      const suffix = instance ? "&copy_instance=" + encodeURIComponent(instance) : "";
      let page;
      try {
        page = await api(LIST + "/" + current.key + "/reactions?after=" + previous + suffix);
      } catch (error) {
        if (!isCurrent()) return false;
        let code = "";
        try { code = JSON.parse(error.message).error; } catch {}
        if ((error.status === 409 && code === "copy_changed") || (error.status === 404 && code === "not_found")) {
          retireReactionInteraction(current);
          return true;
        }
        throw error;
      }
      if (!isCurrent()) return false;
      if (!/^[0-9a-f]{32}$/.test(page.copy_instance || "")) return false;
      const observedCopy = current.view.people_copy_instance || "";
      if (/^[0-9a-f]{32}$/.test(observedCopy) && observedCopy !== page.copy_instance) {
        retireReactionInteraction(current);
        return true;
      }
      if (current.reactionCopyInstance && current.reactionCopyInstance !== page.copy_instance) {
        retireReactionInteraction(current);
        return true;
      }
      current.reactionCopyInstance = page.copy_instance;
      current.reactionsChanged = rememberReactionEvents(current, page.events) || current.reactionsChanged === true;
      current.rnext = page.next;
      if (!page.more || current.rnext <= previous) break;
    }
    return true;
  }

  function retireReactionInteraction(current) {
    current.reactionRetired = true;
    current.reactionCopyInstance = "";
    current.reactionEvents = [];
    current.reactionPending?.clear?.();
    current.rnext = 0;
    reactionUI.close();
    for (const turn of doc.querySelectorAll("#msgs [data-gkey]")) {
      if (turn.dataset.gkey !== current.key) continue;
      turn.querySelector("[data-reaction-entry]")?.remove?.();
      const chips = turn.querySelector("[data-reaction-chips]");
      if (chips) chips.innerHTML = "";
    }
    if (current === owner || ownerCopies.get(current.key) === current) panel.ownerChanged();
  }

  /* A local operation is sending, acknowledged (awaiting its retained event) or
     uncertain. Only the matching retained event removes it; an acknowledgment
     never does. An explicit recognized rejection removes it. Anything else keeps
     the exact body and key for a deliberate retry, which is never automatic. */
  function reactionRejected(error, code) {
    const status = error?.status;
    return [400, 403, 404, 409, 413].includes(status) || (status === 502 && code === "refused");
  }
  /* The write receipt is the appended comment's sequence, later than its target.
     A success without it is not evidence the record was stored. */
  function reactionReceiptUsable(receipt, target) {
    return !!receipt && Number.isSafeInteger(receipt.seq) && receipt.seq > target;
  }
  function sameLogicalView(view, other) {
    return guestKey(view) === guestKey(other) && copyIdentity(view) === copyIdentity(other);
  }
  /* Coalesces with a running read: a busy read sets one flag, and its settle
     path starts exactly one follow-up. No timer is involved. */
  function requestReactionRefresh(current) {
    if (current.busy) { current.reactionRefresh = true; return; }
    if (current === open) startRead();
    else startOwnerRead(current);
  }
  async function chooseReaction(destination) {
    const current = open || (owner && selectedNode() === owner.node && selectedUID() === owner.view.chat ? owner : null);
    if (!current || current.reactionRetired || current.key !== destination.key || current.reactionCopyInstance !== destination.copyInstance || !reactionOnline(current.view)) return;
    const comment = current.entries.get("comments/" + destination.target);
    if (!comment || comment.mine || current.view.access === "read" || current.view.state !== "live") return;
    const key = destination.target + "\0" + destination.emoji;
    const captured = {current, node:current.node, key:current.key, copy:destination.copyInstance, view:current.view, visit:open?.visit, access:current.view.access, state:current.view.state};
    const stillCurrent = () => !destroyed && (open === current || ownerCopies.get(captured.key) === current) && sameLogicalView(current.view, captured.view) && current.reactionCopyInstance === captured.copy && current.view.access === captured.access && current.view.state === captured.state && reactionOnline(current.view) && (open === current || (selectedNode() === captured.node && selectedUID() === captured.view.chat)) && (!captured.visit || bubbleController.isCurrentVisit(captured.visit));
    let pending = current.reactionPending.get(key);
    if (pending && pending.state !== "uncertain") return;
    const created = !pending;
    if (created) {
      const activeNow = reactionSummary(current).some(chip => chip.target === destination.target && chip.emoji === destination.emoji && chip.mine);
      const bytes = randomBytes(8);
      pending = {target:destination.target, emoji:destination.emoji, active:!activeNow, key:guestHexKey(bytes), body:null, state:"sending"};
      pending.body = JSON.stringify({target:pending.target, emoji:pending.emoji, active:pending.active, key:pending.key});
      current.reactionPending.set(key, pending);
    }
    /* A fenced request was never sent: a new operation is dropped, and an
       uncertain one keeps its state so the explicit retry remains available. */
    if (!stillCurrent()) {
      if (created) current.reactionPending.delete(key);
      return;
    }
    pending.state = "sending";
    const operationCurrent = () => stillCurrent() && current.reactionPending.get(key) === pending;
    syncReactionChips(current);
    let receipt;
    try {
      receipt = await api(LIST + "/" + current.key + "/reactions?copy_instance=" + pendingCopy(current), {method:"POST", body:pending.body});
    } catch (error) {
      let code = "";
      try { code = JSON.parse(error.message).error; } catch {}
      if (!operationCurrent()) {
        if (current.reactionPending.get(key) === pending && pending.state === "sending") pending.state = "uncertain";
        return;
      }
      if (error.status === 409 && code === "copy_changed") { retireReactionInteraction(current); return; }
      if (reactionRejected(error, code)) {
        current.reactionPending.delete(key);
        syncReactionChips(current);
        if (typeof toast === "function") toast("Reaction was not saved.");
        return;
      }
      pending.state = "uncertain";
      syncReactionChips(current);
      return;
    }
    if (!reactionReceiptUsable(receipt, destination.target)) {
      /* Ambiguous, not failed: the exact body and key stay for a deliberate retry. */
      pending.state = "uncertain";
      if (operationCurrent()) syncReactionChips(current);
      return;
    }
    if (current.reactionPending.get(key) === pending) pending.state = "acknowledged";
    if (operationCurrent()) requestReactionRefresh(current);
  }
  function pendingCopy(current) { return current.reactionCopyInstance; }

  // This is presentation evidence, not a read position. The durable comments
  // copy is populated by GET; only the existing /seen operation marks it read.
  function unreadState(view) {
    if (view.state !== "live") return "read";
    if (!view.unread) return "read";
    if (view.access !== "owner" && view.mirror_last > view.seen_mirror) return "unread";
    if (view.comments_last > view.seen_comments) return view.commentUnread || "unknown";
    return "unread";
  }
  function presented(view) {
    const state = unreadState(view);
    return {...view, unread: state === "unread", unreadUnknown: state === "unknown"};
  }
  function setCommentUnread(view, state) {
    Object.defineProperty(view, "commentUnread", {value:state, writable:true, configurable:true});
  }
  async function readClassificationPage(path) {
    const controller = new AbortController();
    let timer, cancel;
    const timeout = new Promise(resolve => {
      cancel = () => { controller.abort(); resolve(null); };
      timer = setTimeout(cancel, classificationTimeoutMs);
    });
    classificationCancels.add(cancel);
    try { return await Promise.race([api(path, {signal:controller.signal}), timeout]); }
    finally { clearTimeout(timer); classificationCancels.delete(cancel); }
  }
  async function classifyComments(view) {
    const key = classificationKey(view);
    const saved = classificationProgress.get(key);
    let cursor = saved && saved.seen === view.seen_comments && saved.next <= view.comments_last ? saved.next : view.seen_comments;
    if (cursor === view.seen_comments) classificationProgress.delete(key);
    let gap = false;
    try {
      for (let pageNo = 0; pageNo < 40 && cursor < view.comments_last; pageNo++) {
        const page = await readClassificationPage(LIST + "/" + guestKey(view) + "/comments?after=" + cursor + copyQuery(view));
        if (!page || !Array.isArray(page.entries) || !Number.isSafeInteger(page.next) || page.next <= cursor) return "unknown";
        const known = new Map();
        for (const entry of page.entries) {
          if (!Number.isSafeInteger(entry.seq) || entry.seq <= cursor || entry.seq > page.next || typeof entry.mine !== "boolean") return "unknown";
          known.set(entry.seq, {kind:"comment", entry});
        }
        for (const event of page.reactions || []) {
          if (!event || event.v !== 1 || event.t !== "reaction" || !Number.isSafeInteger(event.seq) || event.seq <= cursor || event.seq > page.next ||
              !Number.isSafeInteger(event.target) || event.target < 1 || event.target >= event.seq ||
              typeof event.emoji !== "string" || !REACTIONS.some(choice => choice.id === event.emoji) || typeof event.active !== "boolean" ||
              typeof event.key !== "string" || !/^r\/[0-9a-f]{16}$/.test(event.key) || typeof event.author !== "string" || !event.author ||
              typeof event.target_author !== "string" || !event.target_author || event.author === event.target_author || typeof event.mine !== "boolean") continue;
          known.set(event.seq, {kind:"reaction"});
        }
        for (let seq = cursor + 1; seq <= Math.min(page.next, view.comments_last); seq++) {
          const item = known.get(seq);
          if (!item) { gap = true; continue; }
          if (item.kind === "comment" && item.entry.mine === false) { classificationProgress.delete(key); return "unread"; }
        }
        cursor = page.next;
        if (!gap) classificationProgress.set(key, {seen:view.seen_comments, next:cursor});
      }
      if (gap || cursor >= view.comments_last) classificationProgress.delete(key);
      return cursor >= view.comments_last && !gap ? "read" : "unknown";
    } catch { return "unknown"; }
  }
  function pumpClassifications() {
    while (!destroyed && classificationActive < 4 && classificationQueue.length) {
      const job = classificationQueue.shift();
      const view = job.view;
      const seen = view.seen_comments, last = view.comments_last;
      classificationActive++;
      classifyComments(view).then(state => {
        classificationActive--;
        classificationJobs.delete(job.key);
        const current = answer.subscriptions.find(item => classificationKey(item) === job.key);
        if (!destroyed && current && current.seen_comments === seen && current.comments_last === last) {
          const known = classificationResults.get(job.key);
          if (known && known.state === "unread" && known.seen === seen && known.last < last) state = "unread";
          if (state !== "unknown") classificationResults.set(job.key, {seen, last, state});
          setCommentUnread(current, state);
          invalidateCards();
        } else if (!destroyed && current) classifyList([current]);
        job.done();
        pumpClassifications();
      });
    }
  }
  function classifyList(views) {
    for (const view of views) {
      if (isLaneIndex(view)) continue;
      if (view.state !== "live" || !view.unread || view.comments_last <= view.seen_comments || (view.access !== "owner" && view.mirror_last > view.seen_mirror)) continue;
      const key = classificationKey(view);
      const result = classificationResults.get(key);
      if (result && result.seen === view.seen_comments) {
        if (result.last === view.comments_last) { setCommentUnread(view, result.state); continue; }
        if (result.state === "unread" && result.last < view.comments_last) setCommentUnread(view, "unread");
      }
      const prior = classificationJobs.get(key);
      if (prior) { prior.view = view; continue; }
      let done;
      const promise = new Promise(resolve => { done = resolve; });
      const job = {key, view, done};
      classificationJobs.set(key, job);
      classificationQueue.push(job);
      track(promise);
    }
    pumpClassifications();
  }

  function track(promise) {
    pending.add(promise);
    promise.finally(() => pending.delete(promise));
  }
  function tabHTML(active) {
    if (off) return "";
    const received = activities();
    if (!received.length) return "";
    let cls = "";
    if (active) cls = "on";
    const count = received.filter(item => item.unread).length;
    let badge = "";
    if (count) badge = `<span class="tabbadge" aria-label="${count} unread">${count}</span>`;
    else if (received.some(item => item.unreadUnknown)) badge = '<span class="tabbadge" aria-label="unread status unavailable">?</span>';
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
      const present = new Set(next.subscriptions.map(classificationKey));
      for (const key of classificationProgress.keys()) if (!present.has(key)) classificationProgress.delete(key);
      for (const key of classificationResults.keys()) if (!present.has(key)) classificationResults.delete(key);
      classifyList(next.subscriptions);
      if (open && !next.subscriptions.some(view => guestKey(view) === open.key)) {
        readGeneration++;
        open.busy = false;
      }
      if (open) {
        const current = next.subscriptions.find(view => guestKey(view) === open.key);
        if (current && copyIdentity(current) !== copyIdentity(open.view)) {
          close();
          openView(current);
        }
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
    refreshMarkers();
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
  function mirrorEntries(current) {
    const rows = [];
    for (const [key, entry] of current.entries) {
      if (key.startsWith("mirror/")) rows.push(entry);
    }
    return rows;
  }
  function noticeAnchor(current) {
    const row = current.row;
    return row?.renaming ? row.turn : null;
  }
  function syncDisclosure(html) {
    const head = $("chathead");
    if (!head) return;
    if (!disclosure) {
      disclosure = doc.createElement("details");
      disclosure.className = "sharing-disclosure";
      const summary = doc.createElement("summary");
      summary.textContent = "Sharing notice";
      disclosure.appendChild(summary);
      disclosureCopy = doc.createElement("div");
      disclosureCopy.className = "sharing-notice-copy";
      disclosure.appendChild(disclosureCopy);
      head.appendChild(disclosure);
      disclosureCopy.innerHTML = html;
      disclosureHTML = html;
      return;
    }
    if (html === disclosureHTML) return;
    disclosureCopy.innerHTML = html;
    disclosureHTML = html;
  }
  function clearDisclosure() {
    noticeHTML = "";
    disclosureHTML = "";
    if (!disclosure) return;
    disclosure.remove();
    disclosure = null;
    disclosureCopy = null;
  }
  /* Replace only the notice node once it exists, and only when its text
     changed. Assigning #msgs.innerHTML would drop the reader's scroll position. */
  function syncNotice(current) {
    const body = $("msgs");
    const html = sharingNoticeHTML(guestName(current.view.server, current.view.owner, current.view.owner_name, knownPeople), mirrorEntries(current));
    const first = body.firstElementChild;
    if (!(first && first.classList.contains("sharing-notice"))) {
      body.innerHTML = html + body.innerHTML;
    } else if (html !== noticeHTML) {
      const anchor = noticeAnchor(current);
      const snapshot = bubbleController.captureViewport(body, { anchor });
      first.innerHTML = html.slice(html.indexOf(">") + 1, html.lastIndexOf("</div>"));
      bubbleController.restoreViewport(snapshot, { anchor: noticeAnchor(current), follow: false });
    }
    noticeHTML = html;
    syncDisclosure(html);
  }
  function refreshPeople() {
    const next = { ...people() };
    const changed = JSON.stringify(next) !== JSON.stringify(knownPeople);
    knownPeople = next;
    if (open) {
      header(open.view);
      syncNotice(open);
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
    reactionUI.sync();
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
    reactionUI.close();
    readGeneration++;
    const key = guestKey(view);
  open = { key, view, visit:bubbleController.captureVisit(), next: 0, cnext: 0, rnext:0, reactionCopyInstance:view.people_copy_instance || "", reactionEvents:[], reactionPending:new Map(), first: true, seenAtOpen: view.seen_mirror, commentsAtOpen: view.seen_comments, seen: view.seen_mirror, commentsSeen: view.seen_comments, names: {}, entries: new Map(), markerHTML:new Map(), row: null, lastRead: 0, busy: false, failed: false, empty: false, removing: false, removeNote: false };
    header(view);
    $("msgs").innerHTML = '<div class="previewtrunc">Loading…</div>';
    syncNotice(open);
    $("guestnote").textContent = "";
    $("guestremove").disabled = false;
    $("guestmore").hidden = true;
    startRead();
  }
  function close() {
    for (const cancel of retentionCancels) cancel();
    if (bubbleController.current()?.kind === "shared") bubbleController.clear();
    open = null;
    readGeneration++;
    reactionUI.close();
    $("msgs").innerHTML = "";
    clearDisclosure();
    invalidateCards();
  }
  function keydown(event) {
    if (event.key === "Escape" && event.target?.dataset?.grename !== undefined) {
      event.preventDefault(); event.stopPropagation(); restoreNameRow(); return;
    }
    activateBubbleKey(event, {doc, win:doc.defaultView, activate:toggleAction});

  }
  function more() { startRead(); }
  function refreshMarkers() {
    const current = open;
    if (!current || !panel.sharedMarkers) return;
    for (const turn of $("msgs").querySelectorAll("[data-seq], [data-cseq]")) {
      const key = turn.dataset.cseq !== undefined ? "comments/" + turn.dataset.cseq : "mirror/" + turn.dataset.seq;
      const entry = current.entries.get(key), capture = entry && guestCapture(entry, current.view, current.names, knownPeople);
      if (!capture) continue;
      const html = panel.sharedMarkers(capture.team) || "";
      if (current.markerHTML.get(key) === html) continue;
      const bubble = turn.querySelector(".bubble");
      if (!bubble) continue;
      const old = bubble.querySelector(".bubblemarkers"), active = doc.activeElement;
      const marker = old && old.contains(active) && active?.dataset?.bmarker;
      const restore = ["bookmark", "prompt", "sendto"].includes(marker);
      if (old) old.remove();
      if (html) bubble.insertAdjacentHTML("afterbegin", html);
      if (restore) bubble.querySelector(`[data-bmarker="${marker}"]`)?.focus({preventScroll:true});
      current.markerHTML.set(key, html);
    }
  }
  function resolveSharedJump(current, moreMirror, moreComments) {
    const target = current.jump;
    if (!target || open !== current) return false;
    const key = target.log + "/" + target.seq;
    const entry = current.entries.get(key);
    if (entry) {
      const selector = target.log === "comments" ? `[data-cseq="${target.seq}"]` : `[data-seq="${target.seq}"]`;
      const element = entry.omitted === undefined ? $("msgs").querySelector(selector) : null;
      if (element) {
        element.scrollIntoView({block:"center"});
        element.classList?.add("jumpflash");
        setTimeout(() => element.classList?.remove("jumpflash"), 1600);
      } else $("guestnote").textContent = "This shared message is unavailable — its saved bookmark remains.";
      current.jump = null;
      return false;
    }
    const hasMore = target.log === "comments" ? moreComments : moreMirror;
    if (!hasMore) { $("guestnote").textContent = "This shared message is unavailable — its saved bookmark remains."; current.jump = null; }
    return hasMore;
  }
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
    return gen === readGeneration && bubbleController.isCurrentVisit(current.visit);
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
    let continueJump = false;
    function insertUpdate(html, key) {
      const row = current.row;
      const editing = !!row?.renaming;
      const snapshot = bubbleController.captureViewport(body, {anchor:editing ? row.turn : null});
      insertOrdered(body, html, key);
      const anchor = editing && current.row === row && row.renaming ? row.turn : null;
      bubbleController.restoreViewport(snapshot, {anchor, follow:!first});
    }
    try {
      for (let n = 0; n < 40; n++) {
        const previous = current.next;
        const result = await api(LIST + "/" + current.key + "?after=" + current.next + copyQuery(current.view));
        if (!currentRead(current, gen)) return;
        current.view = result.view;
        panel.changed();
        if (refreshPeople()) invalidateCards();
        header(result.view);
        if (!current.removeNote) {
          $("guestnote").textContent = "";
        }
        if (current.first) { body.innerHTML = ""; current.first = false; syncNotice(current); }
        for (const entry of result.entries) {
          current.entries.set("mirror/" + entry.seq, entry);
          const capture = guestCapture(entry, result.view, current.names, knownPeople);
          const markers = capture ? panel.sharedMarkers?.(capture.team) || "" : "";
          if (capture) current.markerHTML.set("mirror/" + entry.seq, markers);
          const rendered = guestEntryHTML(entry, result.view.owner_name, result.view.server, result.view.owner, knownPeople, result.view.access, markers);
          if (rendered) {
            if (current.empty) { body.innerHTML = ""; current.empty = false; syncNotice(current); }
            insertUpdate(rendered, [entry.seq, 0, 0]);
            hasEntries = true;
          }
        }
        current.next = result.next;
        morePages = result.more;
        if (current.jump && morePages && current.next <= previous) { morePages = false; break; }
        if (!morePages) break;
      }
      for (let n = 0; n < 40; n++) {
        const previous = current.cnext;
        const result = await api(LIST + "/" + current.key + "/comments?after=" + current.cnext + copyQuery(current.view));
        if (!currentRead(current, gen)) return;
        const retained = await retainComments(current, result.entries);
        if (!currentRead(current, gen)) return;
        applyRetention(result.entries, retained);
        Object.assign(current.names, result.names);
        if (refreshPeople()) invalidateCards();
        for (const message of result.entries) {
          if (current.empty) { body.innerHTML = ""; current.empty = false; syncNotice(current); }
          current.entries.set("comments/" + message.seq, message);
          const capture = guestCapture(message, current.view, current.names, knownPeople);
          const markers = capture ? panel.sharedMarkers?.(capture.team) || "" : "";
          if (capture) current.markerHTML.set("comments/" + message.seq, markers);
          insertUpdate(guestMessageHTML({...message, access:current.view.access, state:current.view.state}, current.names, current.view.server, knownPeople, current.key, markers, current.key, assetURL, current.reactionCopyInstance || "", reactionDisplay(current), reactionOnline(current.view)), [message.anchor, 1, message.seq]);
          hasEntries = true;
        }
        current.cnext = result.next;
        moreComments = result.more;
        if (current.jump && moreComments && current.cnext <= previous) { moreComments = false; break; }
        if (!moreComments) break;
      }
      if (current.cnext > 0) {
        await readReactionPages(current, () => currentRead(current, gen));
        if (!currentRead(current, gen)) return;
      }
      // Access, state, and relay availability can change without a new event.
      // Reconcile only the reaction decorations after each ordinary comments
      // poll so stale entry controls never outlive current permissions.
      syncReactionChips(current);
      current.reactionsChanged = false;
      syncNotice(current);
      if (first && !current.row?.renaming) {
        if (!hasEntries) {
          body.innerHTML = '<div class="previewtrunc">Nothing has been shared yet.</div>';
          current.empty = true;
          syncNotice(current);
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
      current.moreMirror = morePages; current.moreComments = moreComments;
      continueJump = resolveSharedJump(current, morePages, moreComments);
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
      const refresh = current.reactionRefresh === true;
      current.reactionRefresh = false;
      if ((continueJump || refresh) && currentRead(current, gen)) startRead();
    }
  }
  async function markSeen(current, gen, marks) {
    try {
      const copy = copyQuery(current.view);
      await api(LIST + "/" + current.key + "/seen" + (copy ? "?" + copy.slice(1) : ""), { method: "PUT", body: JSON.stringify(marks) });
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
      delete view.commentUnread;
      classifyList([view]);
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
    const marker = event.target.closest("[data-bmarker]");
    if (marker && open) {
      const turn = marker.closest("[data-seq], [data-cseq]");
      const key = turn?.dataset?.cseq !== undefined ? "comments/" + turn.dataset.cseq : "mirror/" + turn?.dataset?.seq;
      const entry = current.entries.get(key);
      const capture = entry && guestCapture(entry, current.view, current.names, knownPeople);
      if (capture) panel.openMarker?.(capture.team, marker.dataset.bmarker);
      return;
    }
    if (event.target.closest("[data-bmarker]")) return;
    routeBubbleClick({target:event.target, actionSelector:"[data-gact]", doc, win:doc.defaultView,
      findTurn:target => target.closest(".bubble"), onAction:action => runAction(action.dataset.gact), onTurn:toggleAction});
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
    const selected = bubbleController.toggle({key:"shared/" + current.key + "/" + key, target:turn, kind:"shared", close:() => {
      turn.classList?.remove("bubble-selected");
      current.row?.turn.nextElementSibling?.remove();
      current.row = null;
    }});
    if (!selected) return;
    turn.classList?.add("bubble-selected");
    renderActionRow({doc, turn, html:actionButtons(capture), shared:true});
    current.row = { turn, capture, entry, renaming: false };
  }
  function actionButtons(capture) {
    let buttons = panel.bubbleActions({time:capture.turnTime}, {
      shared:true, iconBranch:panel.branchIcon || "", iconInto:panel.promptIcon || "", iconCopy:panel.copyIcon || "",
      prompt:!!(actionView() && !open && canPreparePrompt(capture.team)),
    });
    if (capture.role === "person") buttons += '<button data-gact="more" aria-label="more actions">' + (panel.moreIcon || "") + '</button>';
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
    row.querySelector('[data-gact="more"]').focus();
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
    bubbleController.cancelAction();
    const capture = current.row.capture;
    const attachmentFeedback = {attachmentNotice:() => toast?.("This action uses the text; attached files are not included."), unsupported:() => toast?.("This action does not support files without text.")};
    if (action === "fork") { dispatchBubbleAction("fork", capture, {...attachmentFeedback, fork:panel.fork ? text => panel.fork({...capture,text}) : null}); return; }
    if (action === "desc") {
      dispatchBubbleAction("desc",capture,{...attachmentFeedback,controller:bubbleController,choose:panel.chooseDescription,nodes:localNodes,
        isCurrent:() => actionView() === current && current.row?.capture === capture,
        verify:panel.chooseDescription ? async (id,instance) => {
          const state = await api("/api/state");
          return (state.nodes || []).some(n => n.id === id && n.instance_id === instance && !n.ended_at);
        } : null,
        unavailable:() => toast?.("That chat is no longer available. Choose a current local chat."),
        describe:panel.chooseDescription ? (id,value) => panel.editDescription?.(id,value) : (_id,value) => panel.describe?.(value)});
      return;
    }
    if (action === "copy") { dispatchBubbleAction("copy", capture, {...attachmentFeedback,copy:panel.copy}); return; }
    if (action === "more" && capture.role === "person") {
      panel.choosePersonAction?.({title:"More actions", choices:[{id:"rename",label:"Rename person"}], onChoose:choice => {
        if (choice?.id === "rename" && actionView() === current && current.row?.capture === capture) editPerson();
      }});
      return;
    }
    if (action === "prompt") {
      dispatchBubbleAction("prompt", {...capture,source:capture.team}, {
        controller:bubbleController,
        canPrepare:() => !open && ownerCurrent(current) && canPreparePrompt(capture.team),
        prepare:panel.preparePrompt ? (text,source,actionCurrent) => { const entry = current.row.entry;
          panel.preparePrompt({...capture,text,...(capture.role === "person" ? {who:guestName(current.view.server,entry.author,current.names[entry.author],knownPeople)} : {})},source,() => actionCurrent() && actionView() === current && current.row?.capture === capture); } : null,
      });
      return;
    }
    if (action === "rename") { editPerson(); return; }
    if (action === "name-save") { savePerson(false); return; }
    if (action === "name-use") { savePerson(true); return; }
    if (action === "bookmark") {
      const keys = ["server", "owner", "chat", "generation", "log", "seq"];
      dispatchBubbleAction("bookmark", capture, {
        exists:value => bookmarks().some(item => item.team && keys.every(key => item.team[key] === value.team[key])),
        isCurrent:() => actionView() === current && current.row?.capture === capture,
        duplicate:() => { $("guestnote").textContent = "Already bookmarked."; },
        capture:value => {
          const bookmark = {t:new Date(now()).toISOString(),...value};
          if (value.attachments?.length && typeof panel.captureBookmark !== "function") throw new Error("Attachment capture unavailable");
          return value.attachments?.length ? panel.captureBookmark(bookmark, value) : bookmark;
        },
        error:() => { $("guestnote").textContent = "Bookmark capture failed — try again."; },
        commit:value => { uiMutate({k:"bookmark-add",bookmark:value}); refreshMarkers(); $("guestnote").textContent = "Bookmarked."; },
      });
    }
    if (action === "sendto") {
      dispatchBubbleAction("sendto", {...capture, source:capture.team, shared:true}, {...attachmentFeedback,sendTo:openSendTo});
    }
  }
  async function remove(current) {
    try {
      const instance = current.view?.people_copy_instance || "";
      await api(LIST + "/" + current.key + (instance ? "?copy_instance=" + encodeURIComponent(instance) : ""), { method: "DELETE" });
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
    reactionUI.close();
    readGeneration++;
    clearDisclosure();
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
    removeBubbleListeners = bubbleController.listen($("msgs"), {click:sharedClick, keydown:panelKeydown});
    const messages = $("msgs");
    const reactionClick = event => reactionUI.handle(event);
    const reactionHover = event => reactionUI.hover(event);
    const reactionPointer = event => {
      const root = doc.body || doc.documentElement;
      if (!root?.classList || !event.pointerType) return;
      root.classList.toggle("reaction-input-mouse", event.pointerType === "mouse");
    };
    if (typeof doc.addEventListener === "function") {
      doc.addEventListener("click", reactionClick, true);
      doc.addEventListener("pointerover", reactionHover);
      doc.addEventListener("pointerout", reactionHover);
      messages.addEventListener("pointermove", reactionPointer, {passive:true});
      messages.addEventListener("pointerdown", reactionPointer, {passive:true});
    }
    removeReactionListeners = () => {
      if (typeof doc.removeEventListener === "function") {
        doc.removeEventListener("click", reactionClick, true);
        doc.removeEventListener("pointerover", reactionHover);
        doc.removeEventListener("pointerout", reactionHover);
        messages.removeEventListener("pointermove", reactionPointer);
        messages.removeEventListener("pointerdown", reactionPointer);
      }
      reactionUI.close();
      (doc.body || doc.documentElement)?.classList?.remove("reaction-input-mouse");
    };
    $("guestremove").addEventListener("click", removeClick);
  }
  function destroy() {
    reactionUI.close();
    if (bound) {
      $("guestmore").removeEventListener("click", more);
      removeBubbleListeners();
      removeBubbleListeners = null;
      removeReactionListeners?.();
      removeReactionListeners = null;
      $("guestremove").removeEventListener("click", removeClick);
    }
    open = null; owner = null; ownerShare = null; ownerCopies.clear(); actionOwner = null;
    clearDisclosure();
    readGeneration++;
    bound = false;
    destroyed = true;
    for (const flight of peopleFileFlights) cancelPeopleFileFlight(flight, "PEOPLE_FEATURE_CLOSED");
    for (const cancel of classificationCancels) cancel();
    for (const cancel of retentionCancels) cancel();
    classificationProgress.clear();
    classificationResults.clear();
    while (classificationQueue.length) {
      const job = classificationQueue.shift();
      classificationJobs.delete(job.key);
      job.done();
    }
  }
  async function settled() {
    while (pending.size) await Promise.all([...pending]);
  }
  function activities() { return answer.subscriptions.filter(view => view.access !== "owner" && !isLaneIndex(view)).map(presented); }
  function mineUnread() { return answer.subscriptions.some(view => view.access === "owner" && unreadState(view) === "unread"); }
  function mineUnknown() { return answer.subscriptions.some(view => view.access === "owner" && unreadState(view) === "unknown"); }
  function ownerUnreadStatus(id) {
    const states = answer.subscriptions.filter(view => view.access === "owner" && view.local_node === id).map(unreadState);
    return states.includes("unread") ? "unread" : states.includes("unknown") ? "unknown" : "read";
  }
  function ownerUnread(id) { return !!id && ownerUnreadStatus(id) === "unread"; }
  function groups() { return sharedGroups(); }
  function sharedGroups() { return guestGroups(activities(), answer.servers, knownPeople); }
  function openActivity(id) {
    const view = activities().find(item => "team/" + guestKey(item) === id);
    if (!view) return;
    panel.select(id);
    invalidateCards();
  }
  function jumpToSource(source) {
    if (!source || !promptSourceKey(source) || !["mirror", "comments"].includes(source.log)) return false;
    const key = guestKey(source);
    const view = answer.subscriptions.find(item => guestKey(item) === key);
    if (!view || isLaneIndex(view)) return false;
    bubbleController.clear();
    if (view.access === "owner") {
      if (!view.local_node || view.local_node.startsWith("team/") || !panel.ownerJump) return false;
      panel.select(view.local_node, "jump");
      pendingOwnerJump = {node:view.local_node, key, source};
      if (panel.ownerJump(view.local_node, source) === false) { pendingOwnerJump = null; return false; }
      const current = ownerCopies.get(key);
      const found = current && (source.log === "comments" ? current.entries.has("comments/" + source.seq) : current.mirrors.some(item => item.seq === source.seq));
      if (found) { pendingOwnerJump = null; panel.ownerChanged(); }
      else if (current && !current.busy) startOwnerRead(current);
      return true;
    }
    const id = "team/" + key;
    if (!open || open.key !== key) panel.select(id, "jump");
    if (!open || open.key !== key) return false;
    open.jump = {log:source.log, seq:source.seq};
    if (!open.busy) {
      if (resolveSharedJump(open, open.moreMirror, open.moreComments)) startRead();
    }
    return true;
  }
  function selectActivity(id) {
    const view = activities().find(item => "team/" + guestKey(item) === id);
    if (open && "team/" + open.key === id && view && copyIdentity(open.view) === copyIdentity(view)) return;
    if (open) close();
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
    if (pendingOwnerJump && pendingOwnerJump.node !== node) pendingOwnerJump = null;
    const uid = selectedUID();
    const relevant = answer.subscriptions.filter(v => !!node && !!uid && v.access === "owner" && v.chat === uid);
    const keep = new Set(relevant.map(guestKey));
    let changed = false;
    for (const [key, item] of ownerCopies) if (item.node !== node || !keep.has(key)) { ownerCopies.delete(key); changed = true; }
    let active = null;
    for (const view of relevant) {
      const key = guestKey(view);
      let current = ownerCopies.get(key);
      if (current && copyIdentity(current.view) !== copyIdentity(view)) {
        ownerCopies.delete(key);
        current = null;
        changed = true;
      }
      if (!current) {
        current = {key, node, view, order: view.local_order, share: null, next:0, cnext:0, rnext:0, reactionCopyInstance:view.people_copy_instance || "", reactionEvents:[], reactionPending:new Map(), mirrors:[], comments:[], entries:new Map(), row:null, names:{}, busy:false, lastRead:0};
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
    return JSON.stringify([view.title, view.owner_name, view.access, view.state, view.delivery, current.mirrors, current.comments, current.reactionEvents, current.names]);
  }
  async function readOwner(current) {
    const before = ownerRenderSignature(current);
    let painted = false;
    let moreMirror = false, moreComments = false, continueJump = false;
    try {
      for (let n = 0; n < 40; n++) {
        const previous = current.next;
        const page = await api(LIST + "/" + current.key + "?after=" + current.next + copyQuery(current.view));
        if (!ownerCurrent(current)) return;
        current.view = page.view; current.mirrors.push(...page.entries); current.next = page.next;
        moreMirror = page.more;
        if (pendingOwnerJump?.key === current.key && moreMirror && current.next <= previous) { moreMirror = false; break; }
        if (!moreMirror) break;
      }
      for (let n = 0; n < 40; n++) {
        const previous = current.cnext;
        const page = await api(LIST + "/" + current.key + "/comments?after=" + current.cnext + copyQuery(current.view));
        if (!ownerCurrent(current)) return;
        const retained = await retainComments(current, page.entries);
        if (!ownerCurrent(current)) return;
        applyRetention(page.entries, retained);
        Object.assign(current.names, page.names); current.comments.push(...page.entries);
        for (const entry of page.entries) current.entries.set("comments/" + entry.seq, entry);
        current.cnext = page.next;
        moreComments = page.more;
        if (pendingOwnerJump?.key === current.key && moreComments && current.cnext <= previous) { moreComments = false; break; }
        if (!moreComments) break;
      }
      if (current.cnext > 0) await readReactionPages(current, () => ownerCurrent(current));
      if (!ownerCurrent(current)) return;
      syncReactionChips(current);
      if (current.reactionsChanged) panel.ownerChanged();
      current.reactionsChanged = false;
      if (pendingOwnerJump?.key === current.key && pendingOwnerJump.node === current.node) {
        const source = pendingOwnerJump.source;
        const found = source.log === "comments" ? current.entries.has("comments/" + source.seq) : current.mirrors.some(item => item.seq === source.seq);
        const more = source.log === "comments" ? moreComments : moreMirror;
        if (found) pendingOwnerJump = null;
        else if (more) continueJump = true;
        else { pendingOwnerJump = null; panel.ownerUnavailable?.(source); }
      }
      if (before !== ownerRenderSignature(current)) { panel.ownerChanged(); painted = true; }
      if (current.cnext > current.view.seen_comments) {
        const copy = copyQuery(current.view);
        await api(LIST + "/" + current.key + "/seen" + (copy ? "?" + copy.slice(1) : ""), {method:"PUT",body:JSON.stringify({comments:current.cnext})});
        if (ownerCurrent(current)) {
          current.view.seen_comments = current.cnext; current.view.unread = false;
          const listed = answer.subscriptions.find(view => guestKey(view) === current.key);
          if (listed) { listed.seen_comments = current.cnext; listed.unread = listed.comments_last > current.cnext; }
          if (listed) { delete listed.commentUnread; classifyList([listed]); }
          invalidateCards();
        }
      }
    } catch {
      if (!painted && ownerCurrent(current) && before !== ownerRenderSignature(current)) panel.ownerChanged();
      if (ownerCurrent(current) && pendingOwnerJump?.key === current.key) {
        const source = pendingOwnerJump.source; pendingOwnerJump = null; panel.ownerUnavailable?.(source);
      }
    } finally {
      if (ownerCurrent(current)) {
        current.busy = false;
        const refresh = current.reactionRefresh === true;
        current.reactionRefresh = false;
        if ((continueJump && pendingOwnerJump?.key === current.key) || refresh) startOwnerRead(current);
      }
    }
  }
  function ownerSnapshots() {
    reactionUI.sync();
    return [...ownerCopies.values()].filter(copy => ownerCurrent(copy)).sort((a,b) => (Number.isInteger(a.order) ? a.order : Infinity) - (Number.isInteger(b.order) ? b.order : Infinity) || a.key.localeCompare(b.key)).map(copy => {
      const frontier = copy.view.local_frontier;
      const missingStart = !copy.mirrors.some(entry => entry.t === "start");
      const mirrors = frontier && missingStart ? [{seq:0,t:"start",local_segment:frontier.segment,local_record:frontier.record}, ...copy.mirrors] : copy.mirrors;
      const reactionState = reactionDisplay(copy);
      return { uid:copy.view.chat, key:copy.key, view:copy.view, mirrors, comments:copy.comments, reactionEvents:copy.reactionEvents, reactionState, reactionCopyInstance:copy.reactionRetired ? "" : copy.reactionCopyInstance, online:reactionOnline(copy.view), names:copy.names, people:knownPeople };
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
  function capturePromptAuthority(source, attachments) {
    if (off || !canPreparePrompt(source) || source?.log !== "comments" || !Array.isArray(attachments) || !attachments.length) return null;
    const node = selectedNode(), uid = selectedUID(), key = [source.server,source.owner,source.chat,source.generation].join("/");
    const instance = localNodes().find(item => item.id === node)?.instance_id;
    const copy = ownerCopies.get(key), entry = copy?.entries.get("comments/" + source.seq);
    const files = entry && sharedAttachmentDescriptors(entry);
    const fields = ["id", "sha256", "size", "name", "mime"];
    if (!instance || !copy || !/^[0-9a-f]{32}$/.test(copy.view.people_copy_instance || "") || !files || files.length !== attachments.length || !files.every((file, index) => fields.every(field => file[field] === attachments[index]?.[field]))) return null;
    const captured = {...source}, binding = answer.servers.find(item => item.id === source.server)?.people_file_binding || "";
    const copyInstance = copy.view.people_copy_instance;
    return {copyInstance, current:() => !off && selectedNode() === node && selectedUID() === uid && localNodes().find(item => item.id === node)?.instance_id === instance && ownerCopies.get(key) === copy && copy.view.people_copy_instance === copyInstance && copy.entries.get("comments/" + captured.seq) === entry && (answer.servers.find(item => item.id === captured.server)?.people_file_binding || "") === binding && canPreparePrompt(captured)};
  }
  function ownerSnapshot() {
    return ownerSnapshots().find(copy => owner && copy.key === owner.key) || null;
  }
  function localReviewContexts() {
    return ownerSnapshots().map(copy => ({key:copy.key, copyInstance:copy.view.people_copy_instance || "", binding:answer.servers.find(server => server.id === copy.view.server)?.people_file_binding || "", owner:true}));
  }
  function peopleContext() {
    const current = open || (owner && selectedNode() === owner.share.node ? owner : null);
    if (!current) return null;
    const view = current.view;
    const listed = answer.subscriptions.find(item => guestKey(item) === current.key);
    const writable = item => item && item.state === "live" && (item.access === "write" || item.access === "owner");
    const attachments = fileCapability(view);
    const binding = answer.servers.find(item => item.id === view.server)?.people_file_binding || "";
    return { key: current.key, binding, copyInstance:listed?.people_copy_instance || "", copyIdentityMissing:relayFileCapability(view) && !attachments, owner: current === owner, writable: !off && !current.failed && !!writable(view) && !!writable(listed) && (!current.share || current.share.state === "active"), attachments };
  }
  function relayFileCapability(view) {
    const limits = answer.people_attachments;
    const relay = answer.servers.find(item => item.id === view.server);
    return limits?.version === 1 && limits.max_file_bytes === 4194304 && limits.max_files === 16 && limits.max_total_bytes === 16777216 && relay?.people_files === "supported";
  }
  function fileCapability(view) {
    const listed = answer.subscriptions.find(item => guestKey(item) === guestKey(view));
    return relayFileCapability(view) && /^[0-9a-f]{32}$/.test(listed?.people_copy_instance || "") && view.people_copy_instance === listed.people_copy_instance;
  }
  async function retainComments(current, entries) {
    if (destroyed || current.view.state !== "live" || !fileCapability(current.view) || typeof retainPeople !== "function" || !Array.isArray(entries) || !entries.some(entry => sharedAttachmentDescriptors(entry)?.length)) return;
    const controller = new AbortController(); const cancel = () => controller.abort();
    retentionCancels.add(cancel);
    try { return await retainPeople(current.key, entries, controller.signal, current.view.people_copy_instance); } catch { return null; }
    finally { retentionCancels.delete(cancel); }
  }
  function applyRetention(entries, result) {
    if (!result?.files) return;
    for (const entry of entries) {
      const files = sharedAttachmentDescriptors(entry);
      if (!files?.length) continue;
      const state = Object.create(null);
      for (const file of files) {
        const mark = result.files[`${entry.seq}/${file.id}`];
        state[file.id] = mark === "unkept" ? "unkept" : mark === true;
      }
      entry._peopleRetained = state;
    }
  }
  const peopleFileFlights = new Set();
  const peopleLegacyDecisions = new Map();
  const peopleDurablePending = new Map();
  const peopleRetainedPending = new Map();
  const peopleTextPending = new Map();
  const peopleDurableText = new Map();
  const peopleRetainedText = new Map();
  const peopleLegacyDecisionKey = snapshot => JSON.stringify([snapshot.storageKey, snapshot.value]);
  const peopleRetainedKey = (key, copyInstance, instance) => JSON.stringify([key, copyInstance, instance]);
  function peopleRecoveryScope(context, instance) {
    const ownerIdentity = context.owner ? selectedNode() + "\0" + instance : "";
    return "people\0" + context.key + (context.binding ? "\0binding:" + context.binding : "") + (context.copyInstance ? "\0copy:" + context.copyInstance : "") + "\0" + ownerIdentity;
  }
  function peopleTransactionalSnapshot(row, scope) {
    if (!row || row.scope !== scope || row.kind !== "publication" || row.state !== "pending" || typeof row.publication !== "string" || !/^[0-9a-f]{32}:[1-9][0-9]*$/.test(row.publication) || typeof row.peopleKey !== "string" || typeof row.native !== "string" || typeof row.binding !== "string" || !Array.isArray(row.files)) throw new Error("People recovery is unavailable.");
    const identity = {key:row.peopleKey, copyInstance:row.copyInstance, binding:row.binding, instance:row.native};
    const value = JSON.stringify({instance:row.native, copyInstance:row.copyInstance, request:row.request});
    const parsed = parsePeoplePendingSnapshot(value, identity);
    if (row.files.length !== parsed.attachments.length || row.files.some((file, index) => file.id !== parsed.attachments[index])) throw new Error("People recovery is unavailable.");
    return {...parsed, format:"transactional", scope, publication:row.publication, head:row.head, version:row.version, storageKey:"idb:" + row.publication};
  }
  function pendingSnapshotIdentity(snapshot) {
    try {
      if (!snapshot || typeof snapshot.storageKey !== "string" || typeof snapshot.value !== "string" ||
          typeof snapshot.key !== "string" || !/^[^/:]+\/[^/:]+\/[^/:]+\/[^/:]+$/.test(snapshot.key) ||
          typeof snapshot.copyInstance !== "string" || !/^[0-9a-f]{32}$/.test(snapshot.copyInstance) ||
          typeof snapshot.instance !== "string" || snapshot.instance.includes(":") ||
          typeof snapshot.binding !== "string" || snapshot.binding.includes(":")) return null;
      const identity = {key:snapshot.key, copyInstance:snapshot.copyInstance, binding:snapshot.binding, instance:snapshot.instance};
      const parsed = parsePeoplePendingSnapshot(snapshot.value, identity);
      const storageKey = snapshot.format === "transactional" && typeof snapshot.publication === "string" && /^[0-9a-f]{32}:[1-9][0-9]*$/.test(snapshot.publication) && typeof snapshot.scope === "string" ? "idb:" + snapshot.publication : parsed.storageKey;
      if (storageKey !== snapshot.storageKey || parsed.publicationKey !== snapshot.publicationKey ||
          parsed.text !== snapshot.text || JSON.stringify(parsed.attachments) !== JSON.stringify(snapshot.attachments)) return null;
      const flightIdentity = JSON.stringify([snapshot.format === "transactional" ? "transactional" : "legacy", snapshot.key, snapshot.copyInstance, snapshot.binding, snapshot.instance, storageKey, parsed.value, ...(snapshot.format === "transactional" ? [snapshot.scope, snapshot.publication, snapshot.head, snapshot.version] : [])]);
      return {key:storageKey, value:parsed.value, flightIdentity};
    } catch { return null; }
  }
  async function clearPeoplePending(snapshot, options) {
    const pending = pendingSnapshotIdentity(snapshot);
    if (!pending || !options || typeof options.current !== "function") return false;
    if (snapshot.format === "transactional") {
      if (options.scope !== snapshot.scope || typeof peopleFileStore?.decidePeoplePublication !== "function") return false;
      const result = await peopleFileStore.decidePeoplePublication({scope:snapshot.scope, publication:snapshot.publication, expected:snapshot, proof:options.proof, current:options.current});
      if (!options.current()) return false;
      const entries = peopleDurablePending.get(snapshot.scope) || [];
      peopleDurablePending.set(snapshot.scope, entries.filter(item => item.publication !== snapshot.publication || item.value !== snapshot.value));
      const retainedKey = peopleRetainedKey(snapshot.key, snapshot.copyInstance, snapshot.instance);
      peopleRetainedPending.set(retainedKey, (peopleRetainedPending.get(retainedKey) || []).filter(item => item.publication !== snapshot.publication || item.value !== snapshot.value));
      return result;
    }
    if (typeof peopleFileStore?.recordLegacyPeopleDecision !== "function") return false;
    const result = await peopleFileStore.recordLegacyPeopleDecision({scope:options.scope, snapshot, proof:options.proof, current:options.current});
    if (!options.current()) return false;
    if (result?.state !== "retired" && result?.state !== "completed") return false;
    peopleLegacyDecisions.set(peopleLegacyDecisionKey(snapshot), {state:result.state, ...(result.state === "completed" ? {seq:options.proof.seq} : {})});
    return true;
  }
  async function refreshPeopleRecovery(scope) {
    if (typeof peopleFileStore?.listPeopleRecovery !== "function") throw new Error("People recovery is unavailable.");
    const context = peopleContext(), instance = context?.owner ? localNodes().find(item => item.id === selectedNode())?.instance_id : "";
    const result = await peopleFileStore.listPeopleRecovery(scope);
    if (destroyed) return;
    peopleDurablePending.set(scope, result.pending.map(row => peopleTransactionalSnapshot(row, scope)));
    peopleDurableText.set(scope, result.textPending);
    if (context && typeof context.copyInstance === "string" && /^[0-9a-f]{32}$/.test(context.copyInstance) && peopleRecoveryScope(context, instance) === scope && typeof peopleFileStore.listRetainedPeopleRecovery === "function") {
      const retained = await peopleFileStore.listRetainedPeopleRecovery({peopleKey:context.key, copyInstance:context.copyInstance, native:instance});
      if (destroyed) return;
      peopleRetainedPending.set(peopleRetainedKey(context.key, context.copyInstance, instance), retained.map(row => peopleTransactionalSnapshot(row, row.scope)));
    }
    for (const row of result.legacy) {
      if (pendingSnapshotIdentity(row.snapshot) && (row.state === "retired" || (row.state === "completed" && Number.isSafeInteger(row.seq) && row.seq > 0))) peopleLegacyDecisions.set(peopleLegacyDecisionKey(row.snapshot), {state:row.state, ...(row.state === "completed" ? {seq:row.seq} : {})});
    }
    return result;
  }
  function localTextIdentity(copy) {
    if (destroyed || !copy || typeof copy.owner !== "boolean" || typeof copy.binding !== "string" ||
        typeof copy.key !== "string" || !/^[0-9a-f]{32}\/[0-9a-f]{32}\/[0-9a-f]{16}\/[0-9a-f]{16}$/.test(copy.key) ||
        typeof copy.copyInstance !== "string" || !/^(?:[0-9a-f]{32})?$/.test(copy.copyInstance)) return null;
    const node = selectedNode(), active = peopleContext();
    const allowed = copy.owner ? localReviewContexts().some(item => item.key === copy.key && item.copyInstance === copy.copyInstance && item.binding === copy.binding) :
      node === "team/" + copy.key && active && !active.owner && active.key === copy.key && active.copyInstance === copy.copyInstance && active.binding === copy.binding;
    if (!allowed) return null;
    const native = copy.owner ? localNodes().find(item => item.id === node)?.instance_id : "";
    if (copy.owner && (typeof native !== "string" || !native)) return null;
    return {node, key:copy.key, copyInstance:copy.copyInstance, binding:copy.binding, owner:copy.owner, native};
  }
  function textIdentityKey(identity) {
    return JSON.stringify([identity.node, identity.key, identity.copyInstance, identity.binding, identity.owner, identity.native]);
  }
  async function refreshRetainedPeopleRecovery(copy) {
    if (typeof peopleFileStore?.listRetainedPeopleRecovery !== "function") throw new Error("People recovery is unavailable.");
    const identity = localTextIdentity(copy);
    if (!identity) throw new Error("People recovery is unavailable.");
    const key = textIdentityKey(identity), current = () => { const latest = localTextIdentity(copy); return !!latest && textIdentityKey(latest) === key; };
    if (copy.copyInstance) {
      const retained = await peopleFileStore.listRetainedPeopleRecovery({peopleKey:copy.key, copyInstance:copy.copyInstance, native:identity.native});
      if (!current()) return;
      peopleRetainedPending.set(peopleRetainedKey(copy.key, copy.copyInstance, identity.native), retained.map(row => peopleTransactionalSnapshot(row, row.scope)));
    }
    const text = await peopleFileStore.listRetainedPeopleRecovery({peopleKey:copy.key, copyInstance:copy.copyInstance, native:identity.native, kind:"text-publication"});
    if (!current()) return;
    peopleRetainedText.set(key, text);
  }
  function readRetainedPeopleTextPending(copy) {
    const identity = localTextIdentity(copy);
    if (!identity) return {entries:[], unavailable:true};
    const entries = peopleRetainedText.get(textIdentityKey(identity)) || [];
    return {entries:JSON.parse(JSON.stringify(entries)), unavailable:false};
  }
  async function retirePeopleText(snapshot, options) {
    if (!snapshot || typeof snapshot !== "object" || !options || typeof options.current !== "function" || typeof peopleFileStore?.retirePeopleTextPublication !== "function") throw new Error("People text recovery is unavailable.");
    const active = peopleContext();
    const copy = localReviewContexts().find(item => item.key === snapshot.peopleKey && item.copyInstance === snapshot.copyInstance) ||
      (active && !active.owner && active.key === snapshot.peopleKey && active.copyInstance === snapshot.copyInstance ? active : null);
    const identity = localTextIdentity(copy);
    if (!identity || identity.native !== snapshot.native) throw new Error("People text recovery identity is unavailable.");
    const key = textIdentityKey(identity), current = () => { const latest = localTextIdentity(copy); return options.current() === true && !!latest && textIdentityKey(latest) === key; };
    if (!current()) throw new Error("People publication operation cancelled.");
    const saved = JSON.parse(JSON.stringify(snapshot));
    const result = await peopleFileStore.retirePeopleTextPublication({scope:saved.scope, publication:saved.publication, expected:saved, current});
    if (current()) {
      peopleRetainedText.set(key, (peopleRetainedText.get(key) || []).filter(row => row.publication !== saved.publication));
      peopleDurableText.set(saved.scope, (peopleDurableText.get(saved.scope) || []).filter(row => row.publication !== saved.publication));
    }
    return result;
  }
  function cancelPeopleFileFlight(flight, code) {
    if (flight.cancelled) return;
    flight.cancelled = code;
    flight.resolveCancel();
    flight.controller.abort();
  }
  function fencePeoplePending(snapshot) {
    const pending = pendingSnapshotIdentity(snapshot);
    if (!pending) return {state:"unavailable", cancelled:0};
    let cancelled = 0;
    for (const flight of peopleFileFlights) {
      if (flight.identity !== pending.flightIdentity || flight.cancelled) continue;
      cancelled++; cancelPeopleFileFlight(flight, "PEOPLE_LOCAL_RETIRED");
    }
    return {state:"locally_retired", cancelled};
  }
  async function postPeopleFile(path, body, pending, onConfirmed) {
    const flight = {identity:pendingSnapshotIdentity(pending).flightIdentity, controller:new AbortController(), cancelled:"", resolveCancel:null};
    const cancellation = new Promise(resolve => { flight.resolveCancel = resolve; });
    peopleFileFlights.add(flight);
    const cancelled = () => Object.assign(new Error(flight.cancelled === "PEOPLE_LOCAL_RETIRED" ? "Local publication was retired." : "People feature was closed."), {code:flight.cancelled});
    try {
      let result;
      try { result = await Promise.race([api(path, {method:"POST", body, signal:flight.controller.signal}), cancellation]); }
      catch (err) { if (flight.cancelled) throw cancelled(); throw err; }
      if (flight.cancelled) throw cancelled();
      if (!Number.isSafeInteger(result?.seq) || result.seq <= 0) throw new Error("Comment delivery is unconfirmed");
      return onConfirmed(result);
    } finally { peopleFileFlights.delete(flight); }
  }
  async function sendPeople(key, text, attachments, instance = "", admission = null) {
    const context = peopleContext();
    const current = open || (context?.owner ? owner : null);
    const files = attachments !== undefined;
    if (!current || !context || context.key !== key || !context.writable || typeof text !== "string" || (!text.trim() && !files) || new TextEncoder().encode(text).length > 16384) throw new Error("Comments are unavailable");
    if (files) {
      if (!context.attachments || !Array.isArray(attachments) || attachments.length < 1 || attachments.length > 16 || attachments.some(id => typeof id !== "string" || !/^[0-9a-f]{16}$/.test(id)) || new Set(attachments).size !== attachments.length) throw new Error("People files are unavailable");
      if (context.owner) {
        const node = localNodes().find(item => item.id === selectedNode());
        if (!node || !instance || node.instance_id !== instance) throw new Error("Chat identity is unavailable");
      } else if (instance) throw new Error("Chat identity is unavailable");
      const scope = peopleRecoveryScope(context, instance);
      if (!admission || admission.scope !== scope || typeof admission.current !== "function" || typeof admission.onPrepared !== "function" || typeof peopleFileStore?.beginPeoplePublication !== "function") throw new Error("Pending message recovery is unavailable");
      const check = () => {
        const latest = peopleContext();
        if (!admission.current() || !latest || !latest.writable || !latest.attachments || latest.key !== key || latest.copyInstance !== context.copyInstance || latest.binding !== context.binding || latest.owner !== context.owner || (context.owner && localNodes().find(item => item.id === selectedNode())?.instance_id !== instance)) throw Object.assign(new Error("People publication was cancelled."), {code:"PEOPLE_FEATURE_CLOSED"});
      };
      check();
      const existing = await refreshPeopleRecovery(scope);
      check();
      const legacy = pendingPeople();
      if (existing.pending.length || (legacy && !legacy.locallyRetired)) throw new Error("Previous file message is unconfirmed. Review and retry it before sending another.");
      const request = {key:guestHexKey(randomBytes(8)), text, anchor:current.next, attachments:[...attachments]};
      const admitted = await peopleFileStore.beginPeoplePublication({scope, peopleKey:key, head:admission.head, version:admission.version, copyInstance:context.copyInstance, binding:context.binding || "", native:instance, request, files:admission.files, current:() => { try { check(); return true; } catch { return false; } }});
      const pending = {instance, copyInstance:context.copyInstance, request};
      const pendingValue = JSON.stringify(pending), storageKey = "idb:" + admitted.publication;
      const snapshot = {format:"transactional", scope, publication:admitted.publication, head:admission.head, version:admission.version, key, instance, copyInstance:context.copyInstance, binding:context.binding || "", storageKey, value:pendingValue, publicationKey:request.key, text, attachments:[...attachments]};
      peopleDurablePending.set(scope, [snapshot]);
      check(); admission.onPrepared(snapshot); check();
      const suffix = "?copy_instance=" + encodeURIComponent(context.copyInstance) + (instance ? "&chat_instance=" + encodeURIComponent(instance) : "");
      return postPeopleFile(LIST + "/" + key + "/comments" + suffix, JSON.stringify(pending.request), snapshot, result => {
        if (open === current) { current.lastRead = 0; startRead(); }
        if (owner === current) { current.lastRead = 0; startOwnerRead(); }
        return {...result, peoplePending:snapshot};
      });
    }
    const native = context.owner ? (localNodes().find(item => item.id === selectedNode())?.instance_id || "") : "";
    const scope = peopleRecoveryScope(context, native);
    let durable = null, storageUnavailable = false;
    if (admission?.requireDurableAdmission === true && (admission.scope !== scope || typeof admission.current !== "function" || typeof peopleFileStore?.beginPeopleTextPublication !== "function" || typeof peopleFileStore?.listPeopleRecovery !== "function")) throw new Error("People text recovery is unavailable.");
    if (admission && admission.scope === scope && typeof admission.current === "function" && typeof peopleFileStore?.beginPeopleTextPublication === "function") {
      const check = () => {
        const latest = peopleContext();
        if (destroyed || !admission.current() || (admission.preflightCurrent && !admission.preflightCurrent()) || !latest || !latest.writable || latest.key !== key || latest.copyInstance !== context.copyInstance || latest.binding !== context.binding || latest.owner !== context.owner || (context.owner && localNodes().find(item => item.id === selectedNode())?.instance_id !== native)) throw Object.assign(new Error("People publication was cancelled."), {code:"PEOPLE_FEATURE_CLOSED"});
      };
      try {
        check();
        const state = await refreshPeopleRecovery(scope);
        check();
        durable = state.textPending[0] || null;
        if (durable && durable.request.text !== text) throw new Error("Previous People message is unconfirmed. Review it before sending another.");
        if (!durable) {
          const request = {text, anchor:current.next, key:guestHexKey(randomBytes(8))};
          const identity = {scope, peopleKey:key, copyInstance:context.copyInstance || "", binding:context.binding || "", native, head:admission.head, version:admission.version, request};
          const created = await peopleFileStore.beginPeopleTextPublication({...identity, current:() => { try { check(); return true; } catch { return false; } }});
          durable = {...identity, publication:created.publication};
          peopleDurableText.set(scope, [durable]);
        }
      } catch (error) {
        if (admission.requireDurableAdmission === true || error?.code !== "PEOPLE_STORAGE_UNAVAILABLE") throw error;
        storageUnavailable = true;
      }
      check();
    }
    let pending = durable?.request || peopleTextPending.get(scope);
    if (pending && pending.text !== text) throw new Error("Previous People message is unconfirmed. Review it before sending another.");
    if (!pending) {
      pending = { text, anchor: current.next, key: guestHexKey(randomBytes(8)) };
      peopleTextPending.set(scope, pending);
    }
    if (!durable && typeof toast === "function") toast("People message retry is available only while this page remains open." + (storageUnavailable ? " Previous saved retries could not be checked." : ""));
    const result = await api(LIST + "/" + key + "/comments", { method: "POST", body: JSON.stringify(pending) });
    if (!Number.isSafeInteger(result?.seq) || result.seq <= 0) throw new Error("Comment delivery is unconfirmed");
    if (storageUnavailable) result.peopleTextStorageUnavailable = true;
    if (peopleTextPending.get(scope) === pending) peopleTextPending.delete(scope);
    if (durable) {
      result.peopleTextDecision = await peopleFileStore.finishPeopleTextPublication({scope, publication:durable.publication, expected:durable, seq:result.seq, current:admission.current});
      peopleDurableText.set(scope, (peopleDurableText.get(scope) || []).filter(item => item.publication !== durable.publication));
    }
    if (open === current) { current.lastRead = 0; startRead(); }
    if (owner === current) { current.lastRead = 0; startOwnerRead(); }
    return result;
  }
  function pendingPeople() {
    const context = peopleContext();
    if (!context?.key) return null;
    const node = context.owner ? localNodes().find(item => item.id === selectedNode()) : null;
    const instance = context.owner ? node?.instance_id : "";
    if (context.owner && !instance) return null;
    const durable = peopleDurablePending.get(peopleRecoveryScope(context, instance));
    if (durable?.length) return durable[0];
    if (!storage || storage.persistent === false) return null;
    const identity = {key:context.key, instance, binding:context.binding, copyInstance:context.copyInstance};
    const storageKey = peoplePendingStorageKey(identity);
    const raw = storage.getItem(storageKey);
    if (!raw) return null;
    const snapshot = parsePeoplePendingSnapshot(raw, identity);
    const decision = peopleLegacyDecisions.get(peopleLegacyDecisionKey(snapshot));
    return {...snapshot, format:"legacy", ...(decision ? {locallyRetired:true, decision} : {})};
  }
  function pendingPeopleText() {
    const context = peopleContext();
    if (!context?.key) return null;
    const native = context.owner ? (localNodes().find(item => item.id === selectedNode())?.instance_id || "") : "";
    const scope = peopleRecoveryScope(context, native);
    const durable = peopleDurableText.get(scope);
    return durable?.[0] || (peopleTextPending.has(scope) ? {request:peopleTextPending.get(scope)} : null);
  }
  function readRetainedPeoplePending(copy) {
    const unavailable = () => ({entries:[], unavailable:true});
    if (destroyed || !copy || typeof copy.owner !== "boolean" || typeof copy.binding !== "string" ||
        typeof copy.key !== "string" || !/^[0-9a-f]{32}\/[0-9a-f]{32}\/[0-9a-f]{16}\/[0-9a-f]{16}$/.test(copy.key) ||
        typeof copy.copyInstance !== "string" || !/^[0-9a-f]{32}$/.test(copy.copyInstance)) return unavailable();
    const active = peopleContext();
    const allowed = copy.owner ? localReviewContexts().some(item => item.key === copy.key && item.copyInstance === copy.copyInstance && item.binding === copy.binding) :
      selectedNode() === "team/" + copy.key && active && !active.owner && active.key === copy.key && active.copyInstance === copy.copyInstance && active.binding === copy.binding;
    if (!allowed) return unavailable();
    const instance = copy.owner ? localNodes().find(item => item.id === selectedNode())?.instance_id : "";
    if (copy.owner && (typeof instance !== "string" || !instance || instance.includes(":"))) return unavailable();
    const entries = [...(peopleRetainedPending.get(peopleRetainedKey(copy.key, copy.copyInstance, instance)) || [])];
    if (!storage || storage.persistent === false || typeof storage.keys !== "function") return {entries, unavailable:true};
    let keys;
    try { keys = storage.keys(); } catch { return {entries, unavailable:true}; }
    if (storage.persistent === false || !Array.isArray(keys) || keys.length > 4096) return {entries, unavailable:true};
    const prefix = 'scimux-people-file-pending:' + copy.key + ':' + instance;
    const suffix = ':copy:' + copy.copyInstance;
    let failed = false;
    for (const storageKey of keys) {
      if (typeof storageKey !== 'string' || !storageKey.startsWith(prefix + ':') || !storageKey.endsWith(suffix)) continue;
      const middle = storageKey.slice(prefix.length, -suffix.length);
      if (middle && (!middle.startsWith(':') || middle.length === 1 || middle.slice(1).includes(':'))) continue;
      const binding = middle ? middle.slice(1) : '';
      try {
        const value = storage.getItem(storageKey);
        if (storage.persistent === false || typeof value !== 'string') { failed = true; continue; }
        entries.push(parsePeoplePendingSnapshot(value, {key:copy.key, copyInstance:copy.copyInstance, binding, instance}));
      } catch { failed = true; }
    }
    return {entries, unavailable:failed};
  }
  async function retryPeople() {
    const context = peopleContext();
    let pending = pendingPeople();
    if (pending?.locallyRetired) throw Object.assign(new Error("This local publication was retired."), {code:"PEOPLE_LOCAL_RETIRED"});
    if (!context || !context.writable || !pending || !context.attachments) throw new Error("Pending message recovery is unavailable.");
    const original = pending, scope = pending.scope || peopleRecoveryScope(context, pending.instance);
    await refreshPeopleRecovery(scope);
    const latest = peopleContext();
    if (!latest || !latest.writable || !latest.attachments || latest.key !== context.key || latest.copyInstance !== context.copyInstance || latest.binding !== context.binding || latest.owner !== context.owner) throw new Error("Pending message recovery is unavailable.");
    pending = pendingPeople();
    if (!pending || pending.locallyRetired || pending.format !== original.format || pending.storageKey !== original.storageKey || pending.value !== original.value) throw Object.assign(new Error("This local publication is unavailable or retired."), {code:"PEOPLE_LOCAL_RETIRED"});
    const request = JSON.parse(pending.value).request;
    const suffix = "?copy_instance=" + encodeURIComponent(context.copyInstance) + (pending.instance ? "&chat_instance=" + encodeURIComponent(pending.instance) : "");
    return postPeopleFile(LIST + "/" + pending.key + "/comments" + suffix, JSON.stringify(request), pending, result => ({...result, peoplePending:pending}));
  }
  async function finishPeople(receipt, options = {}) {
    const pending = receipt?.peoplePending;
    if (!pending || !Number.isSafeInteger(receipt.seq) || receipt.seq < 1) return false;
    if (options.current !== undefined && typeof options.current !== "function") return false;
    const current = () => !destroyed && (options.current === undefined || options.current() === true);
    if (pending.format === "legacy") {
      const cleared = await clearPeoplePending(pending, {scope:options.scope, proof:{state:"completed", seq:receipt.seq, text:pending.text, stage_ids:pending.attachments}, current});
      return cleared && current() ? {state:"completed", seq:receipt.seq, filesConfirmed:false, draftCleared:false} : false;
    }
    if (pending.format !== "transactional" || typeof peopleFileStore?.decidePeoplePublication !== "function") return false;
    const result = await peopleFileStore.decidePeoplePublication({scope:pending.scope, publication:pending.publication, expected:pending, proof:{state:"completed", seq:receipt.seq, text:pending.text, stage_ids:pending.attachments}, current});
    if (!current()) return false;
    const entries = peopleDurablePending.get(pending.scope) || [];
    peopleDurablePending.set(pending.scope, entries.filter(item => item.publication !== pending.publication || item.value !== pending.value));
    const retainedKey = peopleRetainedKey(pending.key, pending.copyInstance, pending.instance);
    peopleRetainedPending.set(retainedKey, (peopleRetainedPending.get(retainedKey) || []).filter(item => item.publication !== pending.publication || item.value !== pending.value));
    return result;
  }
  function selectedActivity() { return open ? "team/" + open.key : ""; }
  function sharedPreview(anchor) {
    const current = open, turn = anchor?.closest?.('.turn'), rawSeq = turn?.dataset?.cseq, id = anchor?.dataset?.tkey;
    if (off || !current || !/^[1-9][0-9]*$/.test(rawSeq || '') || !/^[0-9a-f]{16}$/.test(id || '')) return null;
    const seq = Number(rawSeq), listed = answer.subscriptions.find(view => guestKey(view) === current.key);
    if (!Number.isSafeInteger(seq) || !listed || !/^[0-9a-f]{32}$/.test(current.view.people_copy_instance || '') || copyIdentity(listed) !== copyIdentity(current.view)) return null;
    const entry = current.entries.get('comments/' + seq), file = entry && sharedAttachmentDescriptors(entry)?.find(item => item.id === id);
    if (!file || !['image/png', 'image/jpeg'].includes(file.mime) || entry._peopleRetained?.[id] === false) return null;
    const path = '/api/team/subscriptions/' + current.key.split('/').map(encodeURIComponent).join('/') + '/attachments/' + seq + '/' + id + '?copy_instance=' + encodeURIComponent(current.view.people_copy_instance) + '&preview=1';
    const resolved = assetURL(path);
    if (!resolved || anchor.getAttribute?.('href') !== resolved) return null;
    return {url:anchor.href || resolved, name:file.name};
  }
  return { tick, refresh, refreshMarkers, bind, destroy, settled, selectActivity, setOwnerShare, localIdentityChanged, ownerSnapshot, ownerSnapshots, localReviewContexts, readRetainedPeoplePending, readRetainedPeopleTextPending, retirePeopleText, canPreparePrompt, capturePromptAuthority, sharedPreview, peopleContext, sendPeople, pendingPeople, pendingPeopleText, retryPeople, finishPeople, clearPeoplePending, fencePeoplePending, refreshPeopleRecovery, refreshRetainedPeopleRecovery, jumpToSource, loaded: () => hasAnswer, hasActivity: id => activities().some(view => "team/" + guestKey(view) === id), tab: { tabHTML, activities, groups, mineUnread, mineUnknown, ownerUnread, ownerUnreadStatus, open: openActivity, selected: selectedActivity } };
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
/* The owner timeline's rebuild signature. Derived pending chips are excluded so a
   local preview never forces a transcript rebuild; retained reaction events and
   relay availability stay in the signature, so confirmed changes still rebuild. */
export function ownerDiscussionSignature(discussion) {
  return JSON.stringify((discussion || []).map(({ reactionState, ...copy }) => copy));
}
export function guestMessageHTML(entry, names, server = "", people = {}, ownerKey = "", markers = "", attachmentKey = "", assetURL = path => path, copyInstance = null, reactionState = [], online = true) {
  let cls = "turn assistant gmsg";
  let who = guestName(server, entry.author, names[entry.author], people);
  let speakerAttributes = `data-seat="${esc(entry.author)}"`;
  if (entry.mine) { cls = "turn user gmsg"; who = bubbleSpeaker({mine:true}); speakerAttributes = ""; }
  let tab = "";
  if (entry.text || entry.attachments?.length) tab = ' tabindex="0"';
  const tiles = sharedAttachmentTiles(entry, attachmentKey || ownerKey, assetURL, copyInstance);
  const attachments = sharedAttachmentDescriptors(entry) || [];
  const eligible = reactionEligible({shared:!!ownerKey, type:"people-comment", author:entry.author, mine:entry.mine,
    access:entry.access || "write", state:entry.state || "live", online, text:entry.text || "", attachments});
  const controls = eligible && /^[0-9a-f]{32}$/.test(copyInstance || "") ? reactionControlHTML({target:entry.seq, key:ownerKey, copyInstance}) : "";
  const chips = reactionChipsHTML(reactionState, entry.seq);
  return renderBubbleTurn({turnClass:cls, turnAttributes:`data-cseq="${esc(entry.seq)}" data-anchor="${esc(entry.anchor)}"${ownerKey ? ` data-gkey="${esc(ownerKey)}"` : ""}`,
    bubbleAttributes:`title="${esc(entry.at)}"${tab}`, markers, speakerName:who, speakerAttributes, text:entry.text || "", formatText:md,
    suffix:tiles + (chips ? `<span class="reaction-chips" data-reaction-chips>${chips}</span>` : '<span class="reaction-chips" data-reaction-chips></span>') + controls});
}
function sharedAttachmentDescriptors(entry) {
  if (entry.attachments === undefined) return [];
  if (!Array.isArray(entry.attachments) || entry.attachments.length < 1 || entry.attachments.length > 16) return null;
  const seen = new Set(); let total = 0; const files = [];
  for (const file of entry.attachments) {
    if (!file || typeof file.id !== "string" || !/^[0-9a-f]{16}$/.test(file.id) || seen.has(file.id) || typeof file.sha256 !== "string" || !/^[0-9a-f]{64}$/.test(file.sha256) || !Number.isSafeInteger(file.size) || file.size < 1 || file.size > 4194304 || typeof file.name !== "string" || !file.name || new TextEncoder().encode(file.name).length > 255 || /[\\/\x00-\x1f\x7f]/.test(file.name) || file.name === "." || file.name === ".." || typeof file.mime !== "string" || !file.mime || file.mime.length > 255) return null;
    seen.add(file.id); total += file.size;
    if (total > 16777216) return null;
    files.push({id:file.id, sha256:file.sha256, size:file.size, name:file.name, mime:file.mime});
  }
  return files;
}

function sharedAttachmentTiles(entry, key, assetURL, copyInstance) {
  const parts = typeof key === "string" ? key.split("/") : [];
  const files = sharedAttachmentDescriptors(entry);
  if (files === null) return '<span role="status">Attachments unavailable.</span>';
  if (parts.length !== 4 || !Number.isSafeInteger(entry.seq) || entry.seq < 1 || !files.length) return "";
  const base = "/api/team/subscriptions/" + parts.map(encodeURIComponent).join("/") + "/attachments/" + entry.seq + "/";
  const tiles = files.map(file => {
    if (copyInstance !== null && !/^[0-9a-f]{32}$/.test(copyInstance)) return attachmentUnavailableHTML({name:file.name, download:"", status:"copy identity unavailable", escape:esc});
    const url = base + encodeURIComponent(file.id) + (copyInstance ? "?copy_instance=" + encodeURIComponent(copyInstance) : "");
    const image = file.mime === "image/png" || file.mime === "image/jpeg";
    const previewURL = url + (copyInstance ? "&preview=1" : "?preview=1");
    const download = assetURL(url), preview = image ? assetURL(previewURL) : "";
    const unkept = entry._peopleRetained?.[file.id] === "unkept";
    if (entry._peopleRetained?.[file.id] === false) return attachmentUnavailableHTML({name:file.name, download, retryPath:download ? "" : url, escape:esc});
    if (!download && !preview) return attachmentUnavailableHTML({name:file.name, download, retryPath:url, status:"unavailable", escape:esc});
    return attachmentTileHTML({name:file.name, url, previewURL, image:unkept ? false : image && !!preview, key:file.id, escape:esc, assetURL, fallbackURL:download, note:unkept ? "Not kept locally" : image && !preview ? "preview unavailable" : ""});
  }).join("");
  return `<div class="attrow">${tiles}</div>`;
}
export function guestHexKey(bytes) {
  return Array.from(bytes, byte => byte.toString(16).padStart(2, "0")).join("");
}
export function guestEntryHTML(entry, ownerName, server = "", seat = "", people = {}, access = "", markers = "") {
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
        seatAttrs = `data-seat="${esc(seat)}" data-prompt="1" data-prov="${entry.prov === true}"`;
      }
      who += " · prompt";
      if (entry.prov === true) who += " · includes forwarded AI text";
    } else {
      who = bubbleSpeaker({agent:entry.agent || "Agent"});
    }
    const text = entry.omitted === undefined ? stripAssetRefs(entry.text) : null;
    let tab = "";
    if (entry.omitted === undefined) { if (entry.text) tab = ' tabindex="0"'; }
    return renderBubbleTurn({turnClass:"turn assistant", turnAttributes:`data-seq="${esc(entry.seq)}"`, bubbleAttributes:`title="${esc(bubbleTitle(entry.t, entry.time))}"${tab}`,
      markers, speakerName:who, speakerAttributes:seatAttrs, text, formatText:value => value === null ? "This message was too large to share." : md(value)});
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
  const shares = (views || []).filter(view => view.access !== "owner" && !isLaneIndex(view)).map(view => ({id:sharedActivityId(view), shared:view}));
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
