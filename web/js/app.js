"use strict";
/* Packet 7J: sole browser entry (composition + shell).
 *
 * Composition/shell ownership inventory
 * -------------------------------------
 * This module is the only browser entry point and the only composition root.
 * It owns the instance lifecycle (the window slot it claims, the teardown
 * ledger every owned thing is registered in, the timer books), shared
 * application state, feature construction and lazy cross-feature
 * dependency injection, selection/title-edit coordination, shared lane-picker
 * DOM sync, status/usage rendering and existing timers, node rail helpers, station
 * bookmark/toast/longpress helpers, feature bind order, spatial navigation and
 * document gestures, the read-only chat preview, update/license menu
 * actions, polling construction/bind, and final boot order.
 *
 * Feature modules under ./ retain all feature HTML templates and feature-owned
 * listeners. polling.js retains state/UI tick, ETag, visibility, pagehide, and
 * timer ownership.
 *
 * Soft line-count guide: app.js is the deliberate composition/shell exception
 * when above ~500 lines. It must not re-implement extracted feature bodies or
 * the polling algorithm. No window/global application bridge, no second entry,
 * no bundler. The single window property is lifecycle.js's APP_SLOT, and it is
 * not that bridge: it holds one retire handle, exposes no state and no API, and
 * exists because a handover's fresh module graph has no other way to find the
 * instance it is replacing. Read lifecycle.js before removing it.
 *
 * What the exception covers, and what it does not
 * -----------------------------------------------
 * Tests import and execute createApp over file: URLs with injected
 * fetchImpl/assetURL/document/window (see s2-helpers / AT-FR-42). Module-scope
 * auto-boot runs only when import.meta.url is https?: — so Node imports do not
 * touch a real DOM. Coverage therefore includes this file's executed paths;
 * the deliberate exception is composition-only wiring (bind order, listener
 * attachment, timer plumbing, feature factory construction), not "the whole
 * file is untested."
 *
 * The exception covers wiring: construction, injection, bind order, listener
 * registration, DOM reads and writes. It does not cover pure logic. A function
 * here that takes values and returns a value or a string — arc geometry,
 * colour thresholds, HTML assembly, escaping, date formatting, reading a form
 * choice — is extractable and belongs in a module under ./ with Node tests.
 * Escaping is the clearest case: no source-text regex can assert it, so it
 * stays unasserted for exactly as long as it lives in here.
 *
 * Extract on sight, one feature at a time. usage.js is the worked example:
 * decision half and presentation half together, agent logo / metric / phase
 * helpers, clock and locale injected so the module stays pure. Do not answer a
 * coverage finding by adding assertions over this file's source text — they
 * catch deletion, not breakage, and a test that reads a comment proves nothing.
 */
import {
  esc, mdInline, md, ageText, fmtDur, fmtStamp, fmtBubbleTime, fmtWhen,
  fmtNoteMeta, bubbleTitle, cssRGB as cssRGBMod,
  contrastText as contrastTextMod,
} from "./format.js";
import {
  usageBadge,
  agentLogo as agentLogoMod,
  sysMetricHTML,
  statusPhaseAt,
  setStatusUnreachable,
  STATUS_PHASES,
} from "./usage.js";
import {
  hashStr, laneList as laneListMod, sortedLaneList as sortedLaneListMod,
  laneById as laneByIdMod, laneName as laneNameMod, laneColor as laneColorMod,
  uniqueLaneID as uniqueLaneIDMod, nextLaneColor as nextLaneColorMod, makeLane as makeLaneMod,
  laneModel as laneModelMod, stopsOf, stopLabel,
} from "./lanes.js";
import {
  NEW_LANE_VALUE,
  laneOptionsHTML as laneOptionsHTMLMod,
  laneSelectHTML as laneSelectHTMLMod,
  readLaneChoiceFromValues,
} from "./lane-picker.js";
import {
  hardAttention, orderedNodes as orderedNodesMod,
  isArchived as isArchivedMod, stationLatestTurn,
  ackReadySeen, pruneReadySeen, loadReadySeen, saveReadySeen,
  cardTabForVisibleNode,
} from "./map-model.js";
import {
  clampLevel, scrimStep, captureTouchStart, documentSwipeDecision,
  captureNotesTouchStart, notesSwipeBackDecision, journeyToggleState,
  bootLevel, loadStoredLevel, saveLevel,
} from "./navigation.js";
import {
  withCsrf as withCsrfMod, api as apiMod,
  apiConditionalGet as apiConditionalGetMod,
} from "./api.js";
import { createCardsFeature } from "./cards.js";
import { createMapFeature, escapeDockStep } from "./map.js";
import { createChatFeature, splitAssetRefs } from "./chat.js";
import { createComposerFeature } from "./composer.js";
import {
  createBookmarksFeature,
  stampAddress as stampAddressMod,
  bookmarkLaneId as bookmarkLaneIdMod,
  bookmarkSortKey as bookmarkSortKeyMod,
  bookmarkClampState as bookmarkClampStateMod,
} from "./bookmarks.js";
import { createNotesFeature } from "./notes.js";
import { harnessRowsHTML, harnessCheckNote, createSettingsController } from "./harness.js";
import { createSearchFeature, buildPendingJump } from "./search.js";
import {
  makeReturnContext, returnAfterSelection, chatBackState, returnPillState,
  mapJumpReturnKind,
  RETURN_MAP, RETURN_SEARCH, RETURN_NOTE,
} from "./returnto.js";
import { createSheetsFeature } from "./sheets.js";
import { createPairingFeature, createDeviceList, createUnlinkControl, createPairControl, createHomeScreenControl, homeScreenInstallable } from "./pairing-ui.js";
import { createPollingFeature } from "./polling.js";
import { installInsetRefresh } from "./insets.js";
import { claimAppSlot, createTeardown, createTimerBook } from "./lifecycle.js";
import { focusAtEnd } from "./caret.js";


/* FR-42 transport seam: the composition root takes fetchImpl and assetURL as
   required suppliers. No module-global default — a missing supplier throws,
   so a future caller cannot silently acquire the global. On the computer origin
   the browser entry below injects identity assetURL and the platform fetch. */
export async function createApp({ fetchImpl, assetURL, document, window } = {}) {
  if (typeof fetchImpl !== "function") {
    throw new Error("createApp: fetchImpl is required");
  }
  if (typeof assetURL !== "function") {
    throw new Error("createApp: assetURL is required");
  }
  if (!document) {
    throw new Error("createApp: document is required");
  }
  if (!window) {
    throw new Error("createApp: window is required");
  }
  /* One app per window, claimed before anything is registered and before the
     first await. A remote handover (FR-40) installs a new head/body and calls
     createApp again over a brand-new module graph; the predecessor's listeners
     on document/window/matchMedia/visualViewport and its timers all survive
     that swap, so the successor is the only thing that can retire it.
     Everything below which outlives an element goes into the ledger. */
  const teardown = createTeardown();
  const instance = { retire: teardown.run };
  claimAppSlot(window, instance);
  const own = teardown.own;
  /* Prefer the injected window's timers; unref under Node so a test that
     boots the composition root is not kept alive by the 2s poll / 30s usage
     cadence. Browser timers have no unref and are unchanged. */
  const hostSetTimeout = typeof window.setTimeout === "function"
    ? window.setTimeout.bind(window) : globalThis.setTimeout.bind(globalThis);
  const hostClearTimeout = typeof window.clearTimeout === "function"
    ? window.clearTimeout.bind(window) : globalThis.clearTimeout.bind(globalThis);
  const hostSetInterval = typeof window.setInterval === "function"
    ? window.setInterval.bind(window) : globalThis.setInterval.bind(globalThis);
  const hostClearInterval = typeof window.clearInterval === "function"
    ? window.clearInterval.bind(window) : globalThis.clearInterval.bind(globalThis);
  /* Every timer this instance starts is booked, so retirement can stop a
     cadence nobody kept a handle to — the 8s status phase and the 30s usage
     read are the two with no other owner. These wrappers stay the single
     funnel: features receive them, never the host's. */
  const timeouts = createTimerBook({ set: hostSetTimeout, clear: hostClearTimeout, once: true });
  const intervals = createTimerBook({ set: hostSetInterval, clear: hostClearInterval, once: false });
  teardown.add(() => timeouts.clearAll());
  teardown.add(() => intervals.clearAll());
  function setTimeout(fn, ms, ...rest) {
    const id = timeouts.set(fn, ms, ...rest);
    if (id && typeof id.unref === "function") id.unref();
    return id;
  }
  function clearTimeout(id) { return timeouts.clear(id); }
  function setInterval(fn, ms, ...rest) {
    const id = intervals.set(fn, ms, ...rest);
    if (id && typeof id.unref === "function") id.unref();
    return id;
  }
  function clearInterval(id) { return intervals.clear(id); }
  /* Rendering discipline (the port contract):
     1. The prompt bar and key row are singletons outside every render region —
        no render call may rebuild them, so typing is never interrupted.
     2. Card list HTML is rebuilt only when the structural signature changes
        (membership, order, live/attention states, titles); volatile text
        (relative ages) updates in place via textContent.
     3. Chat turns re-render only when their content signature changes; scroll
        is preserved unless pinned to the bottom.
     4. Polling pauses when the tab is hidden and fires immediately on return. */

  const $ = s => document.querySelector(s);

  let nodes = [];              // last /api/state nodes
  let unadopted = [];
  let sel = localStorage.getItem("scimux-sel") || "";
  let readySeen = loadReadySeen(localStorage);
  /* Where the user left off: a sleep/wake re-dial re-runs createApp() in a
     fresh module graph, so the pane comes back from storage or not at all. */
  let level = bootLevel({
    stored: loadStoredLevel(localStorage),
    isDesktop: matchMedia("(min-width: 900px)").matches,
    hasSelection: !!sel,
  });
  /* termOpen lives inside createChatFeature (Packet 7C). */
  let cardTab = "current";
  let laneFilter = localStorage.getItem("scimux-lanefilter") || "";
  let attnFoldOpen = false;   /* "Input needed" disclosure; folded on scope change */
  /* mapTab / mapFold / mapFull / fare / mapSel live inside createMapFeature (Packet 7B). */
  const expanded = new Set();
  let actionCard = "";
  let editingDesc = "";
  let editingTitle = "";
  let editingTitleScope = "";   /* "cards" | "chat" — the editor renders only where it was opened */
  /* chatDetailsOpen / editingChatDesc / chatDetailSavedUntil / chatCtxPct /
     sentEcho / suppressAttentionUntil / chatSig / lastTurns / bubbleTurns
     live inside createChatFeature (Packet 7C). */
  let cardsSig = "";
  let sysPrev = null;
  /* pollingFeature owns stateEtag + UI document sync (Packet 7I). Declared early
     so feature factories can inject lazy uiMutate/tick/getUI wrappers. */
  let pollingFeature;

  const isDesktop = () => matchMedia("(min-width: 900px)").matches;
  /* The Notes workspace shows its zones side by side from 768px up (notes.css);
     below that it is one zone at a time, which is what decides whether the
     Bookmarks pane still offers "use in note" (review 2, item 4). Mirrors the
     workspace's own breakpoint, not the 900px shell/desktop one. */
  const SINGLE_ZONE_QUERY = "(max-width: 767px)";
  const singleZone = () => matchMedia(SINGLE_ZONE_QUERY).matches;
  const isPhoneTouch = () => !isDesktop() && matchMedia("(hover: none) and (pointer: coarse)").matches;

  /* Per-muxer-lifetime CSRF token, embedded in the page by the server. Every unsafe
     request echoes it back in X-Scimux-CSRF; a cross-origin page can neither read
     it nor forge the custom header, so this is what stops a web page the operator
     happens to open from driving the local API (send/approve/interrupt/update). */
  const CSRF = document.querySelector('meta[name="scimux-csrf"]')?.content || "";
  const withCsrf = (opts = {}) => withCsrfMod(opts, CSRF);
  const api = (path, opts) => apiMod(path, opts, { fetchImpl, csrf: CSRF });
  const apiConditionalGet = (path, etag) =>
    apiConditionalGetMod(path, etag, { fetchImpl, csrf: CSRF });

  /* UI document + state tick live in createPollingFeature (Packet 7I).
     Shell reaches the shared document via getUI() / uiMutate / isUILoaded. */
  const uiMutate = op => pollingFeature.uiMutate(op);
  const getUI = () => pollingFeature.getUI();
  const uiLoaded = () => pollingFeature.isUILoaded();

  /* copy glyph: fa-regular fa-clone (two offset rounded squares), inlined */
  const ICON_COPY = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2" stroke-linejoin="round" aria-hidden="true">
    <rect x="2" y="8" width="14" height="14" rx="3"/>
    <path d="M8 4.5A2.5 2.5 0 0 1 10.5 2h9A2.5 2.5 0 0 1 22 4.5v9a2.5 2.5 0 0 1-2.5 2.5H19"/>
  </svg>`;
  /* trash glyph: fa-regular fa-trash (lidded can), inlined like the copy icon */
  const ICON_TRASH = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M3 6h18"/>
    <path d="M8 6V4.5A1.5 1.5 0 0 1 9.5 3h5A1.5 1.5 0 0 1 16 4.5V6"/>
    <path d="M5 6l1.2 14a2 2 0 0 0 2 1.8h7.6a2 2 0 0 0 2-1.8L19 6"/>
    <path d="M10 11v6M14 11v6"/>
  </svg>`;
  const ICON_ARCHIVE = `<svg width="17" height="17" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M4 7h16l-1 13H5L4 7Z"/>
    <path d="M3 4h18v3H3z"/>
    <path d="M9 11h6"/>
  </svg>`;
  const ICON_RESTORE = `<svg width="17" height="17" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M9 14l-4-4 4-4"/>
    <path d="M5 10h9a5 5 0 1 1 0 10H8"/>
  </svg>`;
  const ICON_CHECK = `<svg width="17" height="17" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <circle cx="12" cy="12" r="9"/>
    <path d="M8 12.5l2.5 2.5L16 9"/>
  </svg>`;
  /* buffer-stop / dead-end: a track meeting a terminating cap bar (the ⊣ glyph) */
  const ICON_END = `<svg width="16" height="16" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M4 12h11"/>
    <path d="M18 6v12"/>
  </svg>`;
  /* thumbtack / thumbtack-slash — Font Awesome Free 7 (classic solid), CC BY 4.0.
     Inlined like every other glyph (no webfont dependency); filled, so these use
     fill="currentColor" over the project's usual stroke icons. */
  const ICON_PIN = `<svg width="13" height="13" viewBox="0 0 640 640" fill="currentColor" aria-hidden="true">
    <path d="M160 96C160 78.3 174.3 64 192 64L448 64C465.7 64 480 78.3 480 96C480 113.7 465.7 128 448 128L418.5 128L428.8 262.1C465.9 283.3 494.6 318.5 507 361.8L510.8 375.2C513.6 384.9 511.6 395.2 505.6 403.3C499.6 411.4 490 416 480 416L160 416C150 416 140.5 411.3 134.5 403.3C128.5 395.3 126.5 384.9 129.3 375.2L133 361.8C145.4 318.5 174 283.3 211.2 262.1L221.5 128L192 128C174.3 128 160 113.7 160 96zM288 464L352 464L352 576C352 593.7 337.7 608 320 608C302.3 608 288 593.7 288 576L288 464z"/></svg>`;
  const ICON_UNPIN = `<svg width="13" height="13" viewBox="0 0 640 640" fill="currentColor" aria-hidden="true">
    <path d="M73 39.1C63.6 29.7 48.4 29.7 39.1 39.1C29.8 48.5 29.7 63.7 39 73.1L567 601.1C576.4 610.5 591.6 610.5 600.9 601.1C610.2 591.7 610.3 576.5 600.9 567.2L449.8 416L480 416C490 416 499.5 411.3 505.5 403.3C511.5 395.3 513.5 384.9 510.7 375.2L507 361.8C494.6 318.5 466 283.3 428.8 262.1L418.5 128L448 128C465.7 128 480 113.7 480 96C480 78.3 465.7 64 448 64L192 64C184.6 64 177.9 66.5 172.5 70.6L222.1 120.3L217.3 183.4L73 39.1zM314.2 416L181.7 283.6C159 304.1 141.9 331 133 361.9L129.2 375.3C126.4 385 128.4 395.3 134.4 403.4C140.4 411.5 150 416 160 416L314.2 416zM288 576C288 593.7 302.3 608 320 608C337.7 608 352 593.7 352 576L352 464L288 464L288 576z"/></svg>`;
  const ICON_SEND = `<svg width="17" height="17" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M4 11.5L20 4l-7.5 16-2-6.5L4 11.5Z"/>
    <path d="M10.5 13.5L20 4"/>
  </svg>`;
  const ICON_PENCIL = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M4 20h4L18.5 9.5a2.1 2.1 0 0 0-3-3L5 17v3Z"/><path d="M13.5 6.5l3 3"/></svg>`;
  /* a broken chain link: revoking a paired device severs the link between
     it and this computer, which is what happened — nothing is deleted, and
     the same device can pair again tomorrow. Drawn here in the house 24×24
     stroke style so it sits beside the pencil rather than being lifted from
     an icon set with its own licence. */
  const ICON_LINK_SLASH = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M10 14l-2 2a3.5 3.5 0 0 1-5-5l2-2"/>
    <path d="M14 10l2-2a3.5 3.5 0 0 1 5 5l-2 2"/>
    <path d="M4 4l16 16"/></svg>`;
  /* dog-eared document glyph for non-image attachment chips */
  const ICON_FILE = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M6 2h8l4 4v14a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2Z"/>
    <path d="M13 2v5h5"/></svg>`;
  const ICON_PLUS = `<svg width="20" height="20" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true">
    <path d="M12 5v14M5 12h14"/></svg>`;
  /* picture glyph (mountain + sun) for the "Choose image…" menu item */
  const ICON_IMAGE = `<svg width="18" height="18" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <rect x="3" y="4" width="18" height="16" rx="2.5"/>
    <circle cx="8.5" cy="9.5" r="1.6"/><path d="M4 17l5-5 4 4 3-3 4 4"/></svg>`;
  /* stop glyph: fa-regular fa-circle-stop */
  const ICON_STOP = `<svg width="38" height="38" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <circle cx="12" cy="12" r="9"/>
    <rect x="9" y="9" width="6" height="6" rx="1"/>
  </svg>`;
  /* arrow-into-bracket glyph (fa arrow-right-to-bracket style), inlined like
     the copy icon. The bubble-action buttons reuse it in three rotations —
     the visible labels carry the meaning, the shared glyph just says "this
     row sends the message somewhere". */
  const ICON_INTO = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M15 4h3a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2h-3"/>
    <path d="M3 12h12"/>
    <path d="M11 8l4 4-4 4"/>
  </svg>`;
  /* branch glyph: fa-regular fa-code-branch (exact FA 7.3.1 path), inlined
     like the copy icon. Used by the fork action, unrotated. */
  const ICON_BRANCH = `<svg width="15" height="15" viewBox="0 0 640 640" fill="currentColor" aria-hidden="true">
    <path d="M176 168C189.3 168 200 157.3 200 144C200 130.7 189.3 120 176 120C162.7 120 152 130.7 152 144C152 157.3 162.7 168 176 168zM256 144C256 176.8 236.3 205 208 217.3L208 288L384 288C410.5 288 432 266.5 432 240L432 217.3C403.7 205 384 176.8 384 144C384 99.8 419.8 64 464 64C508.2 64 544 99.8 544 144C544 176.8 524.3 205 496 217.3L496 240C496 301.9 445.9 352 384 352L208 352L208 422.7C236.3 435 256 463.2 256 496C256 540.2 220.2 576 176 576C131.8 576 96 540.2 96 496C96 463.2 115.7 435 144 422.7L144 217.4C115.7 205 96 176.8 96 144C96 99.8 131.8 64 176 64C220.2 64 256 99.8 256 144zM488 144C488 130.7 477.3 120 464 120C450.7 120 440 130.7 440 144C440 157.3 450.7 168 464 168C477.3 168 488 157.3 488 144zM176 520C189.3 520 200 509.3 200 496C200 482.7 189.3 472 176 472C162.7 472 152 482.7 152 496C152 509.3 162.7 520 176 520z"/>
  </svg>`;
  /* comment glyph: fa-regular fa-comment style speech bubble — deliberately not
     a reply arrow, which would collide with the leftward jump-to-source icon */
  const ICON_COMMENT = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M21 11.3c0 3.9-4 7-9 7-1.2 0-2.4-.2-3.4-.5L4 20l1.2-3.2C3.8 15.4 3 13.4 3 11.3c0-3.9 4-7 9-7s9 3.1 9 7Z"/>
  </svg>`;
  /* jump-to-source glyph: fa share-from-square mirrored along its y-axis so the
     arrow leaves to the left — "back into the activity", inlined stroke style */
  const ICON_JUMP = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M13 6h4a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2v-4"/>
    <path d="M9 3H3v6"/>
    <path d="M3 3l9 9"/>
  </svg>`;
  /* terminal glyph: fa-regular fa-window-maximize (framed window w/ title bar) */
  const ICON_TERM = `<svg width="17" height="17" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2" stroke-linejoin="round" aria-hidden="true">
    <rect x="3" y="4" width="18" height="16" rx="2.5"/>
    <path d="M3 8.5h18"/>
  </svg>`;
  /* scroll-to-end glyph: fa-regular fa-angles-down (double chevron) */
  const ICON_DOWNALL = `<svg width="18" height="18" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M6 6l6 5.5 6-5.5"/>
    <path d="M6 13l6 5.5 6-5.5"/>
  </svg>`;
  /* Auto-approve toggle pair + warning — Font Awesome Free classic solid
     (CC BY 4.0), inlined like every other glyph (no webfont/CDN). Paths:
     toggle-off, toggle-on, triangle-exclamation. */
  const ICON_TOGGLE_OFF = `<svg width="18" height="18" viewBox="0 0 576 512" fill="currentColor" aria-hidden="true">
    <path d="M384 128c70.7 0 128 57.3 128 128s-57.3 128-128 128l-192 0c-70.7 0-128-57.3-128-128s57.3-128 128-128l192 0zM576 256c0-106-86-192-192-192L192 64C86 64 0 150 0 256S86 448 192 448l192 0c106 0 192-86 192-192zM192 352a96 96 0 1 0 0-192 96 96 0 1 0 0 192z"/>
  </svg>`;
  const ICON_TOGGLE_ON = `<svg width="18" height="18" viewBox="0 0 576 512" fill="currentColor" aria-hidden="true">
    <path d="M192 64C86 64 0 150 0 256S86 448 192 448l192 0c106 0 192-86 192-192s-86-192-192-192L192 64zM384 352a96 96 0 1 0 0-192 96 96 0 1 0 0 192z"/>
  </svg>`;
  const ICON_WARN = `<svg width="15" height="15" viewBox="0 0 512 512" fill="currentColor" aria-hidden="true">
    <path d="M256 32c14.2 0 27.3 7.5 34.5 19.8l216 368c7.3 12.4 7.3 27.7 .2 40.1S486.3 480 472 480L40 480c-14.3 0-27.6-7.7-34.7-20.1s-7-27.8 .2-40.1l216-368C228.7 39.5 241.8 32 256 32zm0 128c-13.3 0-24 10.7-24 24l0 112c0 13.3 10.7 24 24 24s24-10.7 24-24l0-112c0-13.3-10.7-24-24-24zm32 224a32 32 0 1 0 -64 0 32 32 0 1 0 64 0z"/>
  </svg>`;
  const ICON_CHEV_DOWN = `<svg width="18" height="18" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M6 9l6 6 6-6"/>
  </svg>`;
  const ICON_CHEV_UP = `<svg width="18" height="18" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M6 15l6-6 6 6"/>
  </svg>`;
  const ICON_MOVE_UP = ICON_CHEV_UP, ICON_MOVE_DOWN = ICON_CHEV_DOWN;
  const ICON_CHEV_RIGHT = `<svg width="18" height="18" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M9 6l6 6-6 6"/>
  </svg>`;
  /* overflow/kebab glyph: three vertical dots */
  const ICON_MENU_DOTS = `<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
    <circle cx="12" cy="5" r="1.7"/><circle cx="12" cy="12" r="1.7"/><circle cx="12" cy="19" r="1.7"/>
  </svg>`;
  /* Use-in-note glyph: paperclip (fa-paperclip in spirit) — "attach this bookmark
     to a Note". Feather-style stroke path, matching the copy/trash siblings. */
  const ICON_CLIP = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M21.44 11.05l-9.19 9.19a6 6 0 0 1-8.49-8.49l9.19-9.19a4 4 0 0 1 5.66 5.66l-9.2 9.19a2 2 0 0 1-2.83-2.83l8.49-8.48"/>
  </svg>`;
  /* Map full-screen pair — Font Awesome Free 7.3.1 classic solid (CC BY 4.0).
     Expand: up-right-and-down-left-from-center (arrows out). Contract:
     down-left-and-up-right-to-center (arrows in). Distinct geometries, not a
     rotated twin of one glyph. */
  const ICON_MAP_EXPAND = `<svg width="16" height="16" viewBox="0 0 512 512" fill="currentColor" aria-hidden="true">
    <path d="M344 0L488 0c13.3 0 24 10.7 24 24l0 144c0 9.7-5.8 18.5-14.8 22.2s-19.3 1.7-26.2-5.2l-39-39-87 87c-9.4 9.4-24.6 9.4-33.9 0l-32-32c-9.4-9.4-9.4-24.6 0-33.9l87-87-39-39c-6.9-6.9-8.9-17.2-5.2-26.2S334.3 0 344 0zM168 512L24 512c-13.3 0-24-10.7-24-24L0 344c0-9.7 5.8-18.5 14.8-22.2S34.1 320.2 41 327l39 39 87-87c9.4-9.4 24.6-9.4 33.9 0l32 32c9.4 9.4 9.4 24.6 0 33.9l-87 87 39 39c6.9 6.9 8.9 17.2 5.2 26.2S177.7 512 168 512z"/>
  </svg>`;
  const ICON_MAP_CONTRACT = `<svg width="16" height="16" viewBox="0 0 512 512" fill="currentColor" aria-hidden="true">
    <path d="M439.5 7c9.4-9.4 24.6-9.4 33.9 0l32 32c9.4 9.4 9.4 24.6 0 33.9l-87 87 39 39c6.9 6.9 8.9 17.2 5.2 26.2S450.2 240 440.5 240l-144 0c-13.3 0-24-10.7-24-24l0-144c0-9.7 5.8-18.5 14.8-22.2s19.3-1.7 26.2 5.2l39 39 87-87zM72.5 272l144 0c13.3 0 24 10.7 24 24l0 144c0 9.7-5.8 18.5-14.8 22.2s-19.3 1.7-26.2-5.2l-39-39-87 87c-9.4 9.4-24.6 9.4-33.9 0l-32-32c-9.4-9.4-9.4-24.6 0-33.9l87-87-39-39c-6.9-6.9-8.9-17.2-5.2-26.2S62.8 272 72.5 272z"/>
  </svg>`;
  /* Layers control — Font Awesome Free 6.7.2 classic solid layer-group path
     (CC BY 4.0), inlined like every other glyph (no webfont classes;
     see TestPinnedIcons). One stable glyph; Fare on/off lives in the menu. */
  const ICON_LAYERS = `<svg width="16" height="16" viewBox="0 0 576 512" fill="currentColor" data-icon="layers" aria-hidden="true"><path d="M264.5 5.2c14.9-6.9 32.1-6.9 47 0l218.6 101c8.5 3.9 13.9 12.4 13.9 21.8s-5.4 17.9-13.9 21.8l-218.6 101c-14.9 6.9-32.1 6.9-47 0L45.9 149.8C37.4 145.8 32 137.3 32 128s5.4-17.9 13.9-21.8L264.5 5.2zM476.9 209.6l53.2 24.6c8.5 3.9 13.9 12.4 13.9 21.8s-5.4 17.9-13.9 21.8l-218.6 101c-14.9 6.9-32.1 6.9-47 0L45.9 277.8C37.4 273.8 32 265.3 32 256s5.4-17.9 13.9-21.8l53.2-24.6 152 70.2c23.4 10.8 50.4 10.8 73.8 0l152-70.2zm-152 198.2l152-70.2 53.2 24.6c8.5 3.9 13.9 12.4 13.9 21.8s-5.4 17.9-13.9 21.8l-218.6 101c-14.9 6.9-32.1 6.9-47 0L45.9 405.8C37.4 401.8 32 393.3 32 384s5.4-17.9 13.9-21.8l53.2-24.6 152 70.2c23.4 10.8 50.4 10.8 73.8 0z"/></svg>`;
  /* One-arg wrapper: call sites and injected deps stay agentLogo(agent);
     the pure builder in usage.js takes the FR-42 assetURL supplier explicitly. */
  function agentLogo(agent){
    return agentLogoMod(agent, assetURL);
  }

  /* clipboard: the async API needs a secure context (HTTPS/localhost) — over
     plain LAN http fall back to execCommand */
  function copyText(s){
    if (navigator.clipboard && window.isSecureContext){
      navigator.clipboard.writeText(s); return;
    }
    const ta = document.createElement("textarea");
    ta.value = s;
    ta.style.cssText = "position:fixed;top:0;left:0;opacity:0";
    ta.setAttribute("readonly", "");            /* no keyboard flash on iOS */
    document.body.appendChild(ta);
    ta.select(); ta.setSelectionRange(0, s.length);
    try { document.execCommand("copy"); } finally { ta.remove(); }
  }

  /* esc / mdInline / md imported from /js/format.js */

  /* Attachment/session-asset tile builders + splitAssetRefs live in chat.js
     (Packet 7C). The read-only chat preview imports splitAssetRefs for the
     same inert-missing-chip projection. */

  /* ---------- statusbar ---------- */
  function renderSys(sys){
    if (!sys || sys.mem_pct == null) { $("#sysmetrics").textContent = ""; return; }
    const loadPct = sys.ncpu ? 100 * sys.load1 / sys.ncpu : 0;
    const prev = sysPrev || {};
    if (sysPrev && sys.mem_pct === prev.mem_pct && sys.swap_pct === prev.swap_pct
        && sys.load1 === prev.load1) return;   /* unchanged: don't re-animate */
    const lbl = (f, a) => `<span class="Lfull">${f}</span><span class="Labbr">${a}</span>`;
    $("#sysmetrics").innerHTML =
      sysMetricHTML(lbl("MEM","M"),  sys.mem_pct.toFixed(0) + "%",  sys.mem_pct,  prev.mem_pct) +
      sysMetricHTML(lbl("SWAP","S"), sys.swap_pct.toFixed(0) + "%", sys.swap_pct, prev.swap_pct) +
      sysMetricHTML(lbl("LOAD","L"), sys.load1.toFixed(2),
             loadPct, prev.ncpu ? 100 * prev.load1 / prev.ncpu : null);
    sysPrev = sys;
  }

  /* ---------- subscription usage (status slot, phase 2) ----------
     The slot flips between system metrics and agent budgets on a local timer;
     the flip is presentation-only and never triggers a server fetch. Budget
     data comes from /api/usage, which is a cache-only read (no provider I/O),
     read at most every 30s while the tab is visible. Numbers are *remaining*
     percentages — runway, the value a user needs before starting work. */
  let usageSnap = null;
  /* The slot cycles: system metrics → Claude → Codex → Grok budgets → repeat.
     Each phase is a fixed-shape row so a flip never reflows the bar.
     Phase ids live in usage.js (STATUS_PHASES); decision via statusPhaseAt. */
  let statusPhaseIdx = 0;

  function renderUsage(snap){
    const a = (snap && snap.agents) || {};
    $("#sysclaude").innerHTML = snap ? usageBadge("claude", a.claude, { agentLogo }) : "";
    $("#syscodex").innerHTML  = snap ? usageBadge("codex",  a.codex,  { agentLogo }) : "";
    const grokEl = $("#sysgrok");
    if (grokEl) grokEl.innerHTML = snap ? usageBadge("grok", a.grok, { agentLogo }) : "";
  }
  function applyStatusPhase(){
    const phase = statusPhaseAt(statusPhaseIdx, usageSnap);
    $("#sysmetrics").classList.toggle("on", phase === "metrics");
    $("#sysclaude").classList.toggle("on",  phase === "claude");
    $("#syscodex").classList.toggle("on",   phase === "codex");
    const grokEl = $("#sysgrok");
    if (grokEl) grokEl.classList.toggle("on", phase === "grok");
  }
  async function readUsage(){
    if (document.hidden) return;
    try {
      usageSnap = await api("/api/usage", { cache: "no-store" });
      renderUsage(usageSnap);
      applyStatusPhase();
    } catch { /* cache-only read; a miss just leaves the last snapshot */ }
  }
  setInterval(() => {
    statusPhaseIdx = (statusPhaseIdx + 1) % STATUS_PHASES.length;
    applyStatusPhase();
  }, 8000);
  setInterval(readUsage, 30000);

  /* ---------- cards ---------- */
  /* Card render/time/signature/events live in /js/cards.js (Packet 7A).
     Lane picker HTML/choice live in lane-picker.js; DOM sync stays here. */
  const orderedNodes = () => orderedNodesMod(nodes);
  const laneList = () => laneListMod(getUI().lanes);
  const sortedLaneList = () => sortedLaneListMod(getUI().lanes);
  const laneById = id => laneByIdMod(id, getUI().lanes);
  const laneName = id => laneNameMod(id, getUI().lanes);
  /* resolveColor for non-hex CSS colors via getComputedStyle (browser only). */
  function resolveCssColor(c){
    if (typeof document === "undefined" || !document.body) return null;
    const el = document.createElement("span");
    el.style.color = c;
    if (!el.style.color) return null;
    el.style.display = "none";
    document.body.appendChild(el);
    const m = getComputedStyle(el).color.match(/rgba?\((\d+),\s*(\d+),\s*(\d+)/);
    el.remove();
    return m ? [parseInt(m[1], 10), parseInt(m[2], 10), parseInt(m[3], 10)] : null;
  }
  const cssRGB = c => cssRGBMod(c, resolveCssColor);
  const contrastText = c => contrastTextMod(c, resolveCssColor);
  const laneColor = id => laneColorMod(id, getUI().lanes);
  const uniqueLaneID = name => uniqueLaneIDMod(name, getUI().lanes);
  const nextLaneColor = () => nextLaneColorMod(getUI().lanes);
  const makeLane = name => makeLaneMod(name, getUI().lanes);
  function laneOptionsHTML(selected = "", includeUnset = true, disabled = false){
    return laneOptionsHTMLMod(selected, sortedLaneList(), {
      includeUnset, disabled,
    });
  }
  function laneSelectHTML(selected = "", includeUnset = true, disabled = false){
    return laneSelectHTMLMod(selected, sortedLaneList(), {
      includeUnset, disabled,
    });
  }
  function fillLaneSelect(select, selected = "", includeUnset = true, disabled = false){
    if (!select) return;
    select.innerHTML = laneOptionsHTML(selected, includeUnset, disabled);
    select.disabled = !!disabled;
    /* authoritative disable (inherited lane): survive the note-open re-enable */
    delete select.dataset.sheetDisabledByClose;
    select.value = selected;
    syncLanePicker(select);
  }
  function syncLanePicker(select){
    if (!select) return;
    const pick = select.closest(".lanepick");
    const input = pick?.nextElementSibling?.matches("input") ? pick.nextElementSibling : null;
    const swatch = pick?.querySelector(".laneswatch");
    if (input) input.hidden = select.value !== NEW_LANE_VALUE;
    if (swatch) swatch.style.background = select.value === NEW_LANE_VALUE ? nextLaneColor() : laneColor(select.value);
  }
  function readLaneChoice(select, input){
    return readLaneChoiceFromValues(select?.value || "", input?.value || "", { makeLane });
  }
  const laneModel = () => laneModelMod(nodes, getUI().lanes);
  const isArchived = id => isArchivedMod(id, getUI().archived);

  /* Activities card feature factory — owns #cardtabs/#cardlist only. */
  const cardsFeature = own(createCardsFeature({
    roots: { tabs: $("#cardtabs"), list: $("#cardlist") },
    document,
    CSS,
    setInterval,
    clearInterval,
    setTimeout,
    esc,
    ageText,
    nodes: () => nodes,
    unadopted: () => unadopted,
    sel: () => sel,
    setSel: v => { sel = v; },
    cardTab: () => cardTab,
    setCardTab: v => { cardTab = v; },
    laneFilter: () => laneFilter,
    attnFoldOpen: () => attnFoldOpen,
    setAttnFoldOpen: v => { attnFoldOpen = v; },
    expanded: () => expanded,
    actionCard: () => actionCard,
    setActionCard: v => { actionCard = v; },
    editingDesc: () => editingDesc,
    setEditingDesc: v => { editingDesc = v; },
    editingTitle: () => editingTitle,
    editingTitleScope: () => editingTitleScope,
    cardsSig: () => cardsSig,
    setCardsSig: v => { cardsSig = v; },
    readySeen: () => readySeen,
    pinned: () => getUI().pinned,
    archived: () => getUI().archived,
    lanes: () => getUI().lanes,
    bookmarks: () => getUI().bookmarks,
    agentLogo,
    laneSelectHTML,
    syncLanePicker,
    readLaneChoice,
    laneColor,
    laneName,
    laneList,
    icons: {
      ICON_CHECK, ICON_PIN, ICON_UNPIN, ICON_END,
      ICON_ARCHIVE, ICON_RESTORE, ICON_TRASH,
    },
    api,
    uiMutate,
    updateLocalNode: n => updateLocalNode(n),
    nodeById: id => nodeById(id),
    exitThread: id => exitThread(id),
    openAdopt: s => sheetsFeature.openAdopt(s),
    selectNode: id => select(id),
    setLevel,
    isDesktop,
    setLaneFilter,
    renderChatHead: () => renderChatHead(),
    renderMap: () => renderMap(),
    fieldError: (el, msg) => sheetsFeature.fieldError(el, msg),
    alert: msg => alert(msg),
    confirm: msg => confirm(msg),
    removeNode: id => { nodes = nodes.filter(x => x.id !== id); },
    invalidateChatSig: () => { chatFeature.invalidate(); },
    invalidateMapSig: () => { mapFeature.invalidate(); },
    invalidateStateEtag: () => pollingFeature.invalidateStateEtag(),
    scheduleTick: () => setTimeout(() => pollingFeature.tick(), 200),
    startTitleEdit: (id, scope) => startTitleEdit(id, scope),
    longpress: (container, selector, fn) => longpress(container, selector, fn),
  }));
  function renderCards(){ cardsFeature.render(); }
  function updateCardAges(animate=false){ cardsFeature.updateAges(animate); }

  function setLaneFilter(id){
    laneFilter = id || "";
    attnFoldOpen = false;      /* a new scope starts with the fold closed */
    if (laneFilter) localStorage.setItem("scimux-lanefilter", laneFilter);
    else localStorage.removeItem("scimux-lanefilter");
    cardsSig = ""; mapFeature.invalidate();
    renderCards(); renderMap();
  }

  teardown.on(document, "change", e => {
    const sel = e.target.closest("[data-lane-select]");
    if (sel) syncLanePicker(sel);
  });
  /* one PATCH per rail-property change; the poll's re-render draws the result */
  async function patchRail(id, body){
    try {
      const n = await api(`/api/nodes/${encodeURIComponent(id)}`, {
        method: "PATCH", body: JSON.stringify(body),
      });
      updateLocalNode(n);
      cardsSig = ""; mapFeature.invalidate();
      renderCards(); renderMap();
    } catch (err) { alert(err.message); }
  }

  /* /exit: mark a thread ended (dead-end cap, stays on the map) and stop its
     process. Shared by the card action and the map toolbar. */
  async function exitThread(id){
    const n = nodeById(id);
    if (!n) return;
    /* Truth in advertising: scimux never kills a tmux session it only adopted, so
       for those "close" means "marked closed here", not "process stopped". Owned
       sessions/links are torn down. The confirm reflects which one this is; the
       server then reports whether the stop actually happened (adopted / kill
       failure both keep the agent alive). */
    const prompt = n.adopted
      ? `Close "${n.title}"? It stays on the map with a dead-end cap and is marked closed in scimux — but the adopted tmux session keeps running (scimux never kills a session it didn't start).`
      : `Close "${n.title}"? It stays on the map with a dead-end cap, and its running session/link is stopped.`;
    if (!confirm(prompt)) return;
    try {
      const res = await api(`/api/nodes/${encodeURIComponent(id)}/exit`, { method: "POST" });
      updateLocalNode(res.node);
      cardsSig = ""; chatFeature.invalidate(); mapFeature.invalidate();
      renderCards(); renderChatHead(); renderMap();
      if (res.closed && !res.stopped && res.reason === "kill_failed")
        alert(`"${n.title}" is marked closed, but scimux could not stop its process — it may still be running.`);
    } catch (err) { alert(err.message); }
  }

  /* Journeys map feature factory — owns tabs/chips/wrap/toolbar/full/fare + map sheet. */
  const mapFeature = own(createMapFeature({
    roots: {
      maptabs: $("#maptabs"),
      lanechips: $("#lanechips"),
      mapwrap: $("#mapwrap"),
      mapscroll: $("#mapscroll"),
      maptoolbar: $("#maptoolbar"),
      mappill: $("#mappill"),
      mapfullbtn: $("#mapfullbtn"),
      layersbtn: $("#layersbtn"),
      fareticket: $("#fare_ticket"),
      map: $("#map"),
      mapdivider: $("#mapdivider"),
      tabHead: $("#tab_head"),
      tabName: $("#tab_name"),
      tabLanes: $("#tab_lanes"),
      tabSave: $("#tab_save"),
      tabDel: $("#tab_del"),
    },
    document,
    window,
    CSS,
    storage: localStorage,
    setTimeout,
    getComputedStyle,
    esc,
    ageText,
    fmtStamp,
    fmtDur,
    contrastText,
    nodes: () => nodes,
    sel: () => sel,
    readySeen: () => readySeen,
    groups: () => getUI().groups,
    laneFilter: () => laneFilter,
    uiLoaded: () => uiLoaded(),
    level: () => level,
    isDesktop,
    mapOpen: () => document.body.classList.contains("map-open"),
    laneModel,
    laneColor,
    laneName,
    laneList,
    laneById,
    nodeById: id => nodeById(id),
    agentLogo,
    icons: { ICON_PENCIL, ICON_END, ICON_MAP_EXPAND, ICON_MAP_CONTRACT, ICON_LAYERS },
    uiMutate,
    selectNode: (id, how) => {
      /* a wall-map "Open chat" is a jump: remember the map so #chatback leads
         back into it (UI review item 18) — but only when the map actually left.
         Asked of isFull(), not isDock(): map.js calls selectNode *before*
         setMapDock(true), so isDock() is still false here on the first open and
         the pill appeared every time. On the desktop wall a jump always keeps
         the map (full → dock), so isFull() is the honest predicate. */
      const mapStays = isDesktop() && mapFeature.isFull();
      const kind = mapJumpReturnKind({ how, mapStays });
      if (kind) setReturnContext(kind, { node: id });
      select(id, how);
    },
    setLevel,
    setLaneFilter,
    loadChatHistory: (id, st) => loadChatHistory(id, st),
    jumpChatToNow: () => jumpChatToNow(),
    openActivityEditor: (id, stop) => sheetsFeature.openActivityEditor(id, stop),
    forkFromStation: id => sheetsFeature.forkFromStation(id),
    addStationBookmark: (id, stop) => addStationBookmark(id, stop),
    exitThread: id => exitThread(id),
    openSheet: id => sheetsFeature.openSheet(id),
    closeSheets: () => sheetsFeature.closeSheets(),
    renderCards: () => renderCards(),
    invalidateCardsSig: () => { cardsSig = ""; },
    longpress: (container, selector, fn) => longpress(container, selector, fn),
    prompt: (msg, def) => prompt(msg, def),
  }));
  function renderMap(){ mapFeature.render(); }
  function renderMapTabs(){ mapFeature.renderTabs(); }
  function setMapFull(on){ mapFeature.setFull(on); }

  /* Chat feature factory — owns head/details, msgs, terminal, keys, workpulse. */
  let pendingJump = null;    // jump-to-source in flight (set by bookmarks/search; consumed by chat)
  /* composerFeature / bookmarksFeature / notesFeature / searchFeature /
     sheetsFeature are assigned after createChatFeature; chat/bookmarks/cards/map
     inject lazy wrappers so effects resolve after construction (Bookmarks must
     not import notes.js; sheets is Packet 7H). */
  let composerFeature;
  let bookmarksFeature;
  let notesFeature;
  let searchFeature;
  let sheetsFeature;
  const chatFeature = own(createChatFeature({
    roots: {
      chathead: $("#chathead"),
      chattitle: $("#chattitle"),
      chatmission: $("#chatmission"),
      chatdetails: $("#chatdetails"),
      chatdesc: $("#chatdesc"),
      chatdescinput: $("#chatdescinput"),
      chatdot: $("#chatdot"),
      chatmeta: $("#chatmeta"),
      chatagentlogo: $("#chatagentlogo"),
      infobtn: $("#infobtn"),
      msgs: $("#msgs"),
      msgwrap: $("#msgwrap"),
      chatloading: $("#chatloading"),
      gauge: $("#gauge"),
      gaugefill: $("#gaugefill"),
      keyrow: $("#keyrow"),
      convtools: $("#convtools"),
      termtoggle: $("#termtoggle"),
      autoapprove: $("#autoapprove"),
      scrollend: $("#scrollend"),
      workpulse: $("#workpulse"),
    },
    document,
    window,
    CSS,
    setTimeout,
    clearTimeout,
    esc,
    md,
    fmtWhen,
    fmtBubbleTime,
    bubbleTitle,
    hashStr,
    api,
    apiConditionalGet,
    assetURL,
    nodes: () => nodes,
    sel: () => sel,
    selGen: () => selGen,
    nodeById: id => nodeById(id),
    editingTitle: () => editingTitle,
    editingTitleScope: () => editingTitleScope,
    hardAttention,
    laneColor,
    agentLogo,
    icons: {
      ICON_TERM, ICON_CHECK, ICON_CHEV_UP, ICON_CHEV_DOWN,
      ICON_BRANCH, ICON_INTO, ICON_COPY, ICON_DOWNALL, ICON_FILE,
      ICON_TOGGLE_OFF, ICON_TOGGLE_ON, ICON_WARN,
    },
    bookmarks: () => getUI().bookmarks,
    uiMutate,
    stampAddress: (b, t) => stampAddressMod(b, t),
    copyText: s => copyText(s),
    forkFromTurn: (text, parent) => sheetsFeature.forkFromTurn(text, parent),
    /* bookmarks owns #sendto; the bubble action reuses that one dialogue */
    openSendTo: opts => bookmarksFeature.openSendTo(opts),
    setComposerBusy: v => composerFeature.setComposerBusy(v),
    setComposerClosed: v => composerFeature.setComposerClosed(v),
    setAttachAvail: v => composerFeature.setAttachAvail(v),
    storage: localStorage,
    composerText: () => (composerFeature && typeof composerFeature.promptText === "function")
      ? (composerFeature.promptText() || "") : "",
    restoreDraft: (id, text) => {
      try { localStorage.setItem("scimux-draft:" + id, text); } catch { /* ignore */ }
      if (sel === id && composerFeature && typeof composerFeature.setPromptText === "function"){
        composerFeature.setPromptText(text);
      }
    },
    setBookmarksBackLabel: t => bookmarksFeature.setBackLabel(t),
    updateLocalNode: n => updateLocalNode(n),
    invalidateCardsSig: () => { cardsSig = ""; },
    invalidateMapSig: () => { mapFeature.invalidate(); },
    renderCards: () => renderCards(),
    renderMap: () => renderMap(),
    getPendingJump: () => pendingJump,
    setPendingJump: v => { pendingJump = v; },
    toast: msg => toast(msg),
    alert: msg => alert(msg),
    tick: () => pollingFeature.tick(),
    scheduleTick: ms => setTimeout(() => pollingFeature.tick(), ms),
    isDesktop,
    startTitleEdit: (id, scope) => startTitleEdit(id, scope),
    longpress: (container, selector, fn) => longpress(container, selector, fn),
  }));
  function renderChatHead(){ chatFeature.renderHead(); }
  async function refreshChat(){ return chatFeature.render(); }
  function loadChatHistory(id, st){ return chatFeature.loadHistory(id, st); }
  function jumpChatToNow(){ chatFeature.jumpToNow(); }
  function restartWorkPulse(){ chatFeature.restartWorkPulse(); }

  /* Composer singleton — prompt bar, drafts, staging, attachments, send/interrupt.
     Outside every polled render region (invariant). */
  composerFeature = own(createComposerFeature({
    roots: {
      promptbar: $("#promptbar"),
      attstage: $("#attstage"),
      prompt: $("#prompt"),
      sendbtn: $("#sendbtn"),
      attadd: $("#attadd"),
      attmenu: $("#attmenu"),
      attimg: $("#attimg"),
      attfile: $("#attfile"),
    },
    document,
    storage: localStorage,
    sel: () => sel,
    nodeById: id => nodeById(id),
    isPhoneTouch,
    esc,
    icons: { ICON_PLUS, ICON_IMAGE, ICON_FILE, ICON_SEND, ICON_STOP },
    api,
    withCsrf,
    fetchImpl,
    FormData,
    URL,
    setInterval,
    clearInterval,
    setTimeout,
    setSentEcho: e => chatFeature.setSentEcho(e),
    clearSentEchoFor: id => chatFeature.clearSentEchoFor(id),
    paintEcho: dest => chatFeature.paintEcho(dest),
    getChatSig: () => chatFeature.getChatSig(),
    getLastTurns: () => chatFeature.getLastTurns(),
    invalidateChat: () => chatFeature.invalidate(),
    refreshChat: () => chatFeature.render(),
    tick: () => pollingFeature.tick(),
    scheduleTick: ms => setTimeout(() => pollingFeature.tick(), ms),
    alert: msg => alert(msg),
  }));

  /* Bookmarks feature — pane, flags, clamp, reply composer, jump, send-to.
     openNotes is lazy so Notes workspace (Packet 7F) is never imported here. */
  bookmarksFeature = own(createBookmarksFeature({
    roots: {
      bookmarkspane: $("#bookmarkspane"),
      bookmarktabs: $("#bookmarktabs"),
      bookmarklist: $("#bookmarklist"),
      bookmarkflags: $("#bookmarkflags"),
      bookmarksbtn: $("#bookmarksbtn"),
      bookmarksback: $("#bookmarksback"),
      notesbtn: $("#notesbtn"),
      bookmarkpeek: $("#bookmarkpeek"),
      bookmarkbar: $("#bookmarkbar"),
      bookmarkchip: $("#bookmarkchip"),
      bookmarkchiptext: $("#bookmarkchiptext"),
      bookmarkchipx: $("#bookmarkchipx"),
      bookmarkprompt: $("#bookmarkprompt"),
      bookmarksend: $("#bookmarksend"),
    },
    document,
    storage: localStorage,
    setTimeout,
    clearTimeout,
    CSS,
    esc,
    md,
    fmtWhen,
    hashStr,
    icons: {
      ICON_JUMP, ICON_COMMENT, ICON_COPY, ICON_CLIP, ICON_TRASH, ICON_SEND,
      ICON_INTO, ICON_MENU_DOTS,
    },
    bookmarks: () => getUI().bookmarks,
    uiMutate,
    nodeById: id => nodeById(id),
    laneList: () => laneList(),
    laneColor: id => laneColor(id),
    agentLogo,
    laneModel: () => laneModel(),
    orderedNodes: () => orderedNodes(),
    pinned: () => getUI().pinned,
    openNotes: () => notesFeature.open(),
    startPlacement: nt => notesFeature.startPlacement(nt),
    toast: msg => toast(msg),
    copyText: s => copyText(s),
    select: (id, how) => select(id, how),
    setLevel: n => setLevel(n),
    openSheet: id => sheetsFeature.openSheet(id),
    closeSheets: () => sheetsFeature.closeSheets(),
    openNewActivity: opts => sheetsFeature.openNewActivity(opts),
    setPendingJump: v => { pendingJump = v; },
    openPreview: (uid, seg, rec, at) => openPreview(uid, seg, rec, at),
    invalidateChat: () => chatFeature.invalidate(),
    wsOpen: () => notesFeature.isOpen(),
    closeWorkspace: () => notesFeature.close(),
    isDesktop,
    singleZone,
    restartWorkPulse: () => restartWorkPulse(),
    /* P6: longpress send-to removed; send-to is an explicit bar button. */
  }));
  function renderBookmarksPane(){ bookmarksFeature.render(); }
  function setBookmarksOpen(open){ bookmarksFeature.setOpen(open); }
  function stampAddress(bookmark, src){ stampAddressMod(bookmark, src); }
  function bookmarkLaneId(nt, byT){ return bookmarkLaneIdMod(nt, byT, nodeById); }
  function bookmarkSortKey(nt, byT){ return bookmarkSortKeyMod(nt, byT); }
  function bookmarkClampState(naturalPx, expanded){
    return bookmarkClampStateMod(naturalPx, expanded);
  }
  function jumpToChatAddress(a){ return bookmarksFeature.jumpToChatAddress(a); }

  /* Notes workspace — synthesis overlay; owns every #ws* root/listener.
     Bookmarks reaches it only via the lazy openNotes/startPlacement closures above. */
  notesFeature = own(createNotesFeature({
    roots: {
      notesworkspace: $("#notesworkspace"),
      wsscrim: $("#wsscrim"),
      wspanel: $("#wspanel"),
      wstitle: $("#wstitle"),
      wsback: $("#wsback"),
      wsclose: $("#wsclose"),
      wsplacetext: $("#wsplacetext"),
      wsplacecancel: $("#wsplacecancel"),
      wszones: $("#wszones"),
      wsinbox: $("#wsinbox"),
      wsnav: $("#wsnav"),
      wsinboxtabs: $("#wsinboxtabs"),
      wsinboxlist: $("#wsinboxlist"),
      wscards: $("#wscards"),
      wsnewnote: $("#wsnewnote"),
      wsnote: $("#wsnote"),
      wssections: $("#wssections"),
      wsnoteempty: $("#wsnoteempty"),
    },
    document,
    storage: localStorage,
    setTimeout,
    clearTimeout,
    requestAnimationFrame,
    CSS,
    esc,
    md,
    fmtWhen,
    fmtNoteMeta,
    hashStr,
    icons: {
      ICON_PLUS, ICON_MENU_DOTS, ICON_CLIP, ICON_JUMP, ICON_COPY, ICON_TRASH,
      ICON_CHEV_RIGHT, ICON_CHEV_DOWN, ICON_PENCIL, ICON_MOVE_UP, ICON_MOVE_DOWN,
      ICON_INTO,
    },
    api,
    bookmarks: () => getUI().bookmarks,
    nodeById: id => nodeById(id),
    laneList: () => laneList(),
    laneColor: id => laneColor(id),
    toast: msg => toast(msg),
    copyText: s => copyText(s),
    /* bookmarks owns #sendto; inbox + reference bars reuse the one dialogue */
    openSendTo: opts => bookmarksFeature.openSendTo(opts),
    onVisibilityChange: () => { bookmarksFeature.invalidate(); renderBookmarksPane(); },
    jumpToChatAddress: a => {
      setReturnContext(RETURN_NOTE, { title: notesFeature.activeTitle() });
      const ok = jumpToChatAddress(a);
      if (!ok){ returnCtx = null; renderChatBack(); }
      return ok;
    },
    isNarrow: () => window.matchMedia("(max-width: 767px)").matches,
    /* P5: inbox flat-vs-overflow bar follows the same desktop breakpoint. */
    isDesktop,
    uiMutate,
    notesbtn: () => $("#notesbtn"),
    captureNotesTouchStart,
    notesSwipeBackDecision,
  }));
  function openWorkspace(){ notesFeature.open(); }
  function closeWorkspace(){ notesFeature.close(); }
  function wsOpen(){ return notesFeature.isOpen(); }
  function renderWsInbox(){ notesFeature.renderInbox(); }
  function startPlacement(nt){ notesFeature.startPlacement(nt); }

  /* Search overlay — Packet 7G; owns #search* roots, shortcuts, recents, feed. */
  searchFeature = own(createSearchFeature({
    roots: {
      searchoverlay: $("#searchoverlay"),
      searchscrim: $("#searchscrim"),
      searchinput: $("#searchinput"),
      searchclose: $("#searchclose"),
      searchfeed: $("#searchfeed"),
      searchbtn: $("#searchbtn"),
    },
    document,
    storage: localStorage,
    setTimeout,
    clearTimeout,
    requestAnimationFrame,
    fetch: fetchImpl,
    AbortController,
    createElement: tag => document.createElement(tag),
    esc,
    stampAddress: stampAddressMod,
    nodeById: id => nodeById(id),
    laneColor: id => laneColor(id),
    agentLogo: a => agentLogo(a),
    select: id => { setReturnContext(RETURN_SEARCH); select(id, "jump"); },
    setPendingJump: v => { pendingJump = v; },
    invalidateChat: () => chatFeature.invalidate(),
    setBookmarksOpen: o => setBookmarksOpen(o),
    /* the overlay hides itself to show this, so the modal owes it a way back */
    openPreview: (uid, seg, rec, at) => openPreview(uid, seg, rec, at, RETURN_SEARCH),
    forkFromTurn: (text, parent) => sheetsFeature.forkFromTurn(text, parent),
    uiMutate: op => uiMutate(op),
    toast: msg => toast(msg),
    prompt: msg => prompt(msg),
    isDesktop: () => isDesktop(),
  }));
  function searchOpen(){ return searchFeature.isOpen(); }
  function openSearch(opts){ searchFeature.open(opts); }
  function closeSearch(){ searchFeature.close(); }

  /* Generic sheets + new-activity + adoption — Packet 7H. */
  sheetsFeature = own(createSheetsFeature({
    roots: {
      backdrop: $("#backdrop"),
      burger: $("#burger"),
      plusbtn: $("#plusbtn"),
      newchat: $("#newchat"),
      nc_head: $("#nc_head"),
      nc_start: $("#nc_start"),
      nc_title: $("#nc_title"),
      nc_prompt: $("#nc_prompt"),
      nc_agent: $("#nc_agent"),
      nc_model: $("#nc_model"),
      nc_effort: $("#nc_effort"),
      nc_dir: $("#nc_dir"),
      nc_lane: $("#nc_lane"),
      nc_lane_new: $("#nc_lane_new"),
      nc_lane_hint: $("#nc_lane_hint"),
      adopt: $("#adopt"),
      ad_title: $("#ad_title"),
      ad_agent: $("#ad_agent"),
      ad_sid: $("#ad_sid"),
      ad_path: $("#ad_path"),
      ad_titlein: $("#ad_titlein"),
      ad_prompt: $("#ad_prompt"),
      ad_go: $("#ad_go"),
    },
    document,
    storage: localStorage,
    setTimeout,
    clearTimeout,
    createElement: tag => document.createElement(tag),
    esc,
    api,
    nodeById: id => nodeById(id),
    sel: () => sel,
    laneFilter: () => laneFilter,
    stopsOf,
    stopLabel,
    fillLaneSelect: (select, selected, includeUnset, disabled) =>
      fillLaneSelect(select, selected, includeUnset, disabled),
    readLaneChoice: (select, input) => readLaneChoice(select, input),
    syncLanePicker: select => syncLanePicker(select),
    laneList: () => laneList(),
    uiMutate: op => uiMutate(op),
    updateLocalNode: n => updateLocalNode(n),
    select: id => select(id),
    /* the launch prompt's optimistic echo — the composer owns every later turn */
    setSentEcho: e => chatFeature.setSentEcho(e),
    now: () => Date.now(),
    setLevel,
    isDesktop,
    tick: () => pollingFeature.tick(),
    invalidateStateEtag: () => pollingFeature.invalidateStateEtag(),
    invalidateCardsSig: () => { cardsSig = ""; },
    invalidateChat: () => chatFeature.invalidate(),
    invalidateMap: () => mapFeature.invalidate(),
    renderCards: () => renderCards(),
    renderChatHead: () => renderChatHead(),
    renderMap: () => renderMap(),
    alert: msg => alert(msg),
  }));

  /* Device pairing + the list of who holds a grant — Packet S8.
     Both take the generic api() and the document; pairing-ui.js owns every
     #pair_* root and #m_devices, and nothing else here writes into them. */
  const pairingFeature = own(createPairingFeature({ api, doc: document }));
  const deviceList = own(createDeviceList({
    api,
    doc: document,
    icons: { ICON_PENCIL, ICON_LINK_SLASH },
  }));
  /* Whether "Pair a device" is offered at all. A computer with no
     enrollment mints a perfectly well-formed code that no device can ever
     meet, so the button is hidden until the status says otherwise. */
  const pairControl = own(createPairControl({ api, doc: document }));
  /* The offer to become a Home Screen app, on a device that has one and
     that reached this page over the tunnel. import.meta.url is the
     evidence for the second: the loader mints a blob: URL per verified
     module, and a local page never has one. The move itself belongs to
     the rendezvous page, which is the side holding the pairing record.

     The capability is homeScreenInstallable()'s to judge, not a viewport
     query's: isPhoneTouch() used to stand in for it and silently excluded
     every iPad in landscape, which needs the move exactly as much as a
     phone does. */
  const homeScreen = own(createHomeScreenControl({
    doc: document,
    win: window,
    storage: window.localStorage,
    moduleURL: import.meta.url,
    standalone:
      window.navigator.standalone === true ||
      matchMedia("(display-mode: standalone)").matches,
    installable: homeScreenInstallable({
      standaloneProp: window.navigator.standalone,
      maxTouchPoints: window.navigator.maxTouchPoints,
      platform: window.navigator.platform,
    }),
  }));

  /* Unlinking this computer (F1). It shares the burger-open refresh with the
     device list, and re-reads that list afterwards because an unlink ends
     every grant on it at once — and takes the pair button with it, which is
     the whole point of asking again here. */
  const unlinkControl = own(createUnlinkControl({
    api,
    doc: document,
    onUnlinked: () => { deviceList.refresh(); pairControl.refresh(); },
  }));

  /* Shell navigation into/out of Journeys (not map-local chrome). */
  /* the chevron must describe the tap: on desktop the button is a toggle, so it
     flips with the pane; on the phone Journeys is a forward level and never
     reports open (UI review item 18). */
  function renderJourneyToggle(){
    const btn = $("#journeybtn");
    if (!btn) return;
    const st = journeyToggleState(isDesktop() && document.body.classList.contains("map-open"));
    btn.innerHTML = st.innerHTML;
    btn.setAttribute("aria-label", st.ariaLabel);
  }
  $("#journeybtn").addEventListener("click", () => {
    if (isDesktop()){
      document.body.classList.toggle("map-open");
      renderJourneyToggle();
      mapFeature.invalidate(); renderMap();
      restartWorkPulse();
    } else setLevel(3);
  });
  $("#mapclose").addEventListener("click", () => setLevel(2));

  /* tapping empty space folds back a level — but a scroll-drag (>6px) or a
     tap on anything interactive must never trigger it */
  function emptyTap(container, interactiveSelector, fn){
    let start = null;
    container.addEventListener("pointerdown", e => { start = { x: e.clientX, y: e.clientY }; });
    container.addEventListener("click", e => {
      if (e.target.closest(interactiveSelector)) return;
      if (start && Math.abs(e.clientX - start.x) + Math.abs(e.clientY - start.y) > 6) return;
      fn(e);
    });
  }
  emptyTap($("#cardlist"), ".card,button,input,textarea,select,a,[data-adopt],[data-x],[data-open]", () => {
    if (isDesktop()){
      /* iPad/desktop columns: tapping the Activity gutter folds the Journey
         column back, mirroring the same gesture on the map itself */
      document.body.classList.remove("map-open");
      renderJourneyToggle();
      mapFeature.invalidate(); renderMap();
      restartWorkPulse();
    } else setLevel(1);
  });
  emptyTap($("#mapscroll"), ".strow,.lanechip,[data-nid],[data-chip],button,a,input,textarea,select", () => {
    /* in full screen a stray tap must never exit the mode (HIG: no accidental
       mode exit — only ⤡ / Escape / a station jump leave) */
    if (mapFeature.isFull()) return;
    if (isDesktop()){
      document.body.classList.remove("map-open");
      renderJourneyToggle();
      mapFeature.invalidate(); renderMap();
      restartWorkPulse();
    } else setLevel(2);
  });

  /* ---------- chat ---------- */
  function nodeById(id){ return nodes.find(n => n.id === id); }
  function updateLocalNode(next){
    const cur = next?.id && nodeById(next.id);
    if (cur) Object.assign(cur, next);
  }
  function titleEditorRoot(){
    return editingTitleScope === "chat" ? "#chathead " : "#cardlist ";
  }
  function focusTitleEditorNow(id){
    /* scope the lookup: on iPhone the card list is hidden while the chat is
       open — focusing its (invisible) input would edit into the void */
    const input = document.querySelector(`${titleEditorRoot()}[data-title-input="${CSS.escape(id)}"]`)
      || document.querySelector(`[data-title-input="${CSS.escape(id)}"]`);
    if (input) focusAtEnd(input);
  }
  function focusTitleEditor(id){ setTimeout(() => focusTitleEditorNow(id), 0); }
  function startTitleEdit(id, scope){
    if (!id || !nodeById(id)) return;
    editingTitle = id;
    editingTitleScope = scope || "cards";
    actionCard = "";
    editingDesc = "";
    cardsSig = ""; chatFeature.invalidate(); mapFeature.invalidate();
    renderCards(); renderChatHead(); renderMap();
    focusTitleEditor(id);
  }
  function cancelTitleEdit(id){
    if (editingTitle !== id) return;
    editingTitle = "";
    editingTitleScope = "";
    cardsSig = ""; chatFeature.invalidate(); mapFeature.invalidate();
    renderCards(); renderChatHead(); renderMap();
  }
  async function commitTitleEdit(id, raw){
    if (editingTitle !== id) return;
    const title = (raw || "").trim();
    if (!title){
      /* an emptied title is a cancel, not an error: an alert here loops with
         the blur that triggered the commit (no escape on touch keyboards) */
      cancelTitleEdit(id);
      return;
    }
    const cur = nodeById(id);
    editingTitle = "";
    editingTitleScope = "";
    cardsSig = ""; chatFeature.invalidate(); mapFeature.invalidate();
    renderCards(); renderChatHead(); renderMap();
    if (cur && cur.title === title) return;
    try {
      const n = await api(`/api/nodes/${encodeURIComponent(id)}`, {
        method: "PATCH",
        body: JSON.stringify({ title }),
      });
      updateLocalNode(n);
      cardsSig = ""; chatFeature.invalidate(); mapFeature.invalidate();
      renderCards(); renderChatHead(); renderMap();
    } catch (err) { alert(err.message); }
  }

  /* selGen guards async work: any await that outlives a selection change must
     not touch the DOM for the node the user has already left. */
  let selGen = 0;
  /* hashStr imported from /js/lanes.js */

  /* Contextual return (UI review item 18) — a jump out of the wall map, the
     search overlay or a note remembers where it came from so #chatback can lead
     back there. Selecting a chat any other way is a deviation from that
     trajectory and drops the context (returnAfterSelection). */
  let returnCtx = null;
  function setReturnContext(kind, payload){ returnCtx = makeReturnContext(kind, payload); }
  function renderChatBack(){
    const btn = $("#chatback");
    if (!btn) return;
    const st = chatBackState(returnCtx);
    btn.innerHTML = st.html;
    btn.setAttribute("aria-label", st.ariaLabel);
    btn.classList.toggle("hasreturn", st.hasReturn);
  }
  function returnFromChat(){
    const ctx = returnCtx;
    returnCtx = null;
    renderChatBack();
    if (!ctx){ setLevel(2); return; }
    if (ctx.kind === RETURN_MAP){ setMapFull(true); return; }
    if (ctx.kind === RETURN_SEARCH){ openSearch(); return; }
    if (ctx.kind === RETURN_NOTE){ openWorkspace(); return; }
    setLevel(2);
  }

  function select(id, how){
    returnCtx = returnAfterSelection(returnCtx, how);
    renderChatBack();
    if (sel !== id){ chatFeature.onSelectChange(); mapFeature.invalidate(); selGen++; }   /* history is per-visit, reloaded on demand */
    else {
      /* same node re-select still discards a pending "use as description" */
      chatFeature.onReselect();
    }
    sel = id;
    localStorage.setItem("scimux-sel", sel);
    composerFeature.onSelect(); /* draft + stage restore; hide + until chat has turns */
    /* the selected node's card must be visible: opening an archived chat (map
       toolbar, notes jump) switches the Activities tab to where the card lives —
       same visibility rule as the renderCards filter */
    const seln = nodeById(id);
    if (seln){
      cardTab = cardTabForVisibleNode(seln, cardTab, getUI().archived);
      const nextSeen = ackReadySeen(readySeen, seln);
      if (nextSeen !== readySeen){
        readySeen = nextSeen;
        saveReadySeen(localStorage, readySeen);
      }
    }
    cardsSig = "";
    renderCards();
    renderChatHead();
    renderMap();   /* full-screen wall map: move the selection marker + action bar */
    refreshChat();
  }
  teardown.on(document, "keydown", e => {
    const titleInput = e.target.closest("[data-title-input]");
    if (titleInput){
      if (e.key === "Enter"){
        e.preventDefault();
        commitTitleEdit(titleInput.dataset.titleInput, titleInput.value);
      } else if (e.key === "Escape"){
        e.preventDefault();
        cancelTitleEdit(titleInput.dataset.titleInput);
      }
      return;
    }
    if (e.target === $("#chatdescinput") && e.key === "Escape"){
      e.preventDefault();
      chatFeature.cancelDescEdit();
      return;
    }
    /* Escape walks the dock ladder (keyboard parity — .backbtn is hidden ≥900px):
       dock open → peek → undock → exit full screen. Ignored while a text field
       is focused so it can't hijack a field's own Escape; the earlier edit-field
       branches already returned. Search/notes/bookmarks/composer own Escape in
       their scopes and stopPropagation — do not reorder or replace this site. */
    if (e.key === "Escape" && mapFeature.isFull() &&
        !/^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName) && !e.target.isContentEditable){
      e.preventDefault();
      const step = escapeDockStep({
        mapFull: mapFeature.isFull(),
        mapDock: mapFeature.isDock(),
      });
      if (step === "undock") mapFeature.setDock(false);
      else if (step === "exit-full") setMapFull(false);
    }
  });
  teardown.on(document, "focusout", e => {
    const titleInput = e.target.closest("[data-title-input]");
    if (titleInput) commitTitleEdit(titleInput.dataset.titleInput, titleInput.value);
  });

  /* Chat head/details, loading chrome, terminal, keyrow, msgs rebuild, bubble
     actions, attention suppression, sent-echo, and scroll live in chat.js
     (Packet 7C). Shell keeps document title-edit key handlers above. */

  /* forkFromTurn / forkFromStation / openActivityEditor / openAdopt / openSheet /
     closeSheets live in createSheetsFeature (Packet 7H). Map/chat/search/cards
     reach them through injected sheetsFeature methods. */
  /* wall map: "Add note" files a sticky note for this station. Notes are
     lane-scoped in the store (no per-node anchor), so it lands in the station's
     lane tab of the notes pane — same record the in-chat/notes composer writes. */
  async function addStationBookmark(id, stopTime){
    const n = nodeById(id);
    if (!n) return;
    /* item 13: no dialog. The station stands for a chat, so the bookmark is that
       chat's latest turn — the head stop takes the live tail, an earlier stop the
       last turn of the segment it marks. The durable address is stamped so the
       bookmark's "Open chat" lands back on the turn it was taken from. */
    let turn = null;
    try {
      const q = stopTime ? "?history=1" : "";
      const data = await api(`/api/nodes/${encodeURIComponent(id)}/chat${q}`);
      turn = stationLatestTurn(data.segments, data.turns, stopTime);
    } catch (err) { turn = null; }
    const clean = ((turn && turn.text) || "").trim();
    if (!clean){ toast("Nothing to bookmark here yet."); return; }
    const bookmark = { t: new Date().toISOString(), text: clean, node: id };
    if (turn.time) bookmark.turnTime = turn.time;
    /* Same reason as the bubble bookmark in chat.js: role is unrecoverable
       later and send-to's AI disclosure keys on it. */
    if (turn.role === "user" || turn.role === "assistant")
      bookmark.role = turn.role;
    if (n.lane_id) bookmark.lane = n.lane_id;
    stampAddress(bookmark, turn);
    uiMutate({ k: "bookmark-add", bookmark });
    bookmarksFeature.pinBottom();
    renderBookmarksPane();
    toast("Bookmark added" + (n.lane_id ? " to " + laneName(n.lane_id) : ""));
  }
  /* ---------- sticky-notes / Bookmarks pane ----------
     Packet 7E: rendering, clamp, flags, reply composer, jump, and longpress
     send-to live in createBookmarksFeature (web/js/bookmarks.js). Pure helpers
     (stampAddress, bookmarkLaneId, bookmarkSortKey, bookmarkClampState) are
     re-exported via shell wrappers for Notes inbox / snapshot until Packet 7F. */
  /* pendingJump is declared with the chat feature factory (Packet 7C). */

  /* transient one-liner, fixed above the composer; outside every polled render
     region so a rebuild cannot wipe it mid-display */
  let toastTimer = null;
  function toast(msg){
    let el = $("#toast");
    if (!el){
      el = document.createElement("div");
      el.id = "toast";
      document.body.appendChild(el);
    }
    el.textContent = msg;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.remove(), 3000);
  }

  /* pending-jump scroll/flash consumption lives in chat.js (Packet 7C). */

  /* ---- long-press (HIG context-menu idiom): delegated helper ----
     A long-press may be followed by a synthesized click on the same element;
     lpFired swallows exactly that one click so tap and hold stay distinct. */
  let lpFired = false;
  teardown.on(document, "click", e => {
    if (lpFired){ e.stopPropagation(); e.preventDefault(); lpFired = false; }
  }, true);
  function longpress(container, selector, fn){
    let timer = null;
    const listeners = [];
    const on = (event, handler, options) => {
      container.addEventListener(event, handler, options);
      listeners.push([event, handler, options]);
    };
    const cancel = () => clearTimeout(timer);
    const fire = (el, ev) => {
      lpFired = true;
      setTimeout(() => { lpFired = false; }, 700);  /* don't swallow later taps */
      fn(el, ev);
    };
    /* all listeners are passive: preventDefault() here would suppress native
       panning for the whole touch, turning every long-pressable title into a
       scroll dead zone. The iOS text-selection callout is suppressed by CSS
       instead (.no-callout on the long-press targets), so taps, scrolling,
       and the native click all stay fully native. */
    on("touchstart", e => {
      const el = e.target.closest(selector);
      if (!el) return;
      /* touches inside form controls stay fully native (cursor placement in
         an open title editor); long-press means nothing there */
      if (e.target.closest("input, textarea, select, button")) return;
      timer = setTimeout(() => fire(el, e), 500);
    }, { passive: true });
    ["touchmove","touchend","touchcancel"].forEach(ev => on(ev, cancel, { passive: true }));
    /* press-and-hold with a pointer (mouse, iPad trackpad) — synthesized mouse
       events after a touch tap arrive as a quick down/up pair, so the timer
       never fires for those; moving > 6px (text selection, drag) cancels */
    let mstart = null;
    on("mousedown", e => {
      const el = e.target.closest(selector);
      if (!el) return;
      mstart = { x: e.clientX, y: e.clientY };
      timer = setTimeout(() => fire(el, e), 500);
    });
    on("mousemove", e => {
      if (mstart && Math.abs(e.clientX - mstart.x) + Math.abs(e.clientY - mstart.y) > 6)
        clearTimeout(timer);
    });
    const cancelMouse = () => { mstart = null; clearTimeout(timer); };
    ["mouseup","mouseleave"].forEach(ev => on(ev, cancelMouse));
    on("contextmenu", e => {   /* desktop right-click */
      const el = e.target.closest(selector);
      if (el){ e.preventDefault(); fire(el, e); }
    });
    return () => {
      clearTimeout(timer);
      for (const [event, handler, options] of listeners)
        container.removeEventListener(event, handler, options);
    };
  }

  /* Card-local listeners (tabs/list/swipe/drag/longpress/age flip). */
  cardsFeature.bind();
  /* Map-local listeners (tabs/chips/wrap/toolbar/full/layers/scroll/resize). */
  mapFeature.bind();
  /* Chat-local listeners (head/details, msgs, keys, terminal, scroll, longpress). */
  chatFeature.bind();
  /* Composer singleton listeners (prompt, send, attach menu/drop, drafts). */
  composerFeature.bind();
  /* Bookmarks pane/flags/composer/peek/longpress (Packet 7E). */
  bookmarksFeature.bind();
  /* Notes workspace listeners (Packet 7F). */
  notesFeature.bind();
  /* Search overlay listeners + document shortcuts (Packet 7G). */
  searchFeature.bind();
  /* Generic sheets + new-activity + adoption (Packet 7H). */
  sheetsFeature.bind();
  /* Pairing sheet + paired-device list (Packet S8). */
  pairingFeature.bind();
  deviceList.bind();
  pairControl.bind();
  homeScreen.bind();
  unlinkControl.bind();
  const refocusTitleEditor = () => {
    if (editingTitle) focusTitleEditorNow(editingTitle);
  };
  for (const ev of ["touchend", "pointerup"]) {
    teardown.on(document, ev, refocusTitleEditor, { passive: true });
  }


  /* long-press send-to lives in bookmarksFeature.bind (Packet 7E). */

  /* Prompt bar / drafts / staging / send live in createComposerFeature
     (Packet 7D). Shell calls composerFeature.onSelect on selection change;
     chat injects setComposerBusy / setComposerClosed / setAttachAvail. */

  /* ---------- navigation (phone: cards slide over chat) ---------- */
  function setLevel(n){
    level = clampLevel(n);
    if (isDesktop()) return;
    saveLevel(localStorage, level);
    document.body.classList.toggle("cards-open", level >= 2);
    $("#cards").classList.toggle("open", level >= 2);
    $("#map").classList.toggle("open", level >= 3);
    $("#scrim").classList.toggle("on", level >= 2);
    if (level === 3){ mapFeature.invalidate(); renderMap(); mapFeature.scrollMapToTop(); }
  }
  function applyNavAction(action){
    if (!action || action.type === "suppress") return;
    if (action.type === "setLevel") setLevel(action.level);
    else if (action.type === "setBookmarksOpen") setBookmarksOpen(action.open);
    else if (action.type === "openWorkspace") openWorkspace();
    else if (action.type === "cancelEditors"){
      if (editingTitle === sel){ editingTitle = ""; editingTitleScope = ""; }
      chatFeature.cancelEditorsForNav();
      cardsSig = "";
      renderCards(); renderChatHead();
    }
  }
  $("#chatback").addEventListener("click", returnFromChat);
  /* The dock's × — visible only under body.map-full.map-dock. Bound here rather
     than in chatFeature because the state it changes belongs to the map. */
  $("#dockclose").addEventListener("click", () => mapFeature.setDock(false));
  /* the scrim is the left panes' peek made tappable: a tap steps ONE pane back
     (level 3→2, 2→1), mirroring the swipe — never a jump straight to chat. */
  $("#scrim").addEventListener("click", () => applyNavAction(scrimStep(level)));
  /* #bookmarkpeek listener lives in bookmarksFeature.bind (Packet 7E). */
  let touch = null;
  teardown.on(document, "touchstart", e => {
    const t = e.touches[0];
    /* snapshot overlay ownership at START: an overlay's own touchend listener
       bubbles first and can close itself (wsOpen()→false) before this handler
       runs, so a live re-check would let the SAME L→R swipe be re-processed as
       "Bookmarks → back → Chat" — the Notes→back overshoot. (pane-gap-design.md P2) */
    touch = captureTouchStart({
      x: t.clientX, y: t.clientY, clientWidth: innerWidth,
      localCard: !!e.target.closest("#cardlist .card"),
      overlayOwned: wsOpen() || searchOpen() || previewOpen(),
    });
  }, { passive: true });
  teardown.on(document, "touchend", e => {
    const start = touch; touch = null;
    if (!start) return;
    const t = e.changedTouches[0];
    const action = documentSwipeDecision(start, { x: t.clientX, y: t.clientY }, {
      level, bookmarksOpen: bookmarksFeature.isOpen(), isDesktop: isDesktop(),
      editingChatDesc: chatFeature.isEditingDesc(), editingTitle, sel,
    });
    applyNavAction(action);
  }, { passive: true });

  /* ---------- sheets (Packet 7H) ----------
     Generic backdrop/open/close, #newchat create/fork/edit, and #adopt live in
     createSheetsFeature (web/js/sheets.js). Construction and bind are next to
     searchFeature above. Feature-specific #sendto/#cardact/#tabsheet contents
     remain in bookmarks/cards/map; #menu update/license contents stay shell. */

  /* Notes workspace implementation lives in /js/notes.js (Packet 7F).
     Shell wrappers openWorkspace/closeWorkspace/wsOpen/renderWsInbox/
     startPlacement are defined next to notesFeature construction above. */

  /* Search overlay implementation lives in /js/search.js (Packet 7G).
     Shell wrappers searchOpen/openSearch/closeSearch and searchFeature
     construction/bind are defined next to notesFeature above. */

  /* ---- the chat preview: what a search hit opens ----
     Every hit, live or deleted, opens the same read-only window of turns around
     itself (GET /api/preview — never the live chat endpoint, no composer, no
     polling). The overlay used to branch here, and the branch was invisible until
     after the tap; now the one fact that differs — whether a chat still exists —
     is a control in the head rather than a rule the user has to learn. */
  let previewNode = null;        /* the live node behind this log, "" when deleted */
  let previewReturnFocus = null; /* focus restore on close */
  /* Which surface presented this modal, when one did. The search overlay hides
     itself to show the preview — and shows it in the overlay's own panel
     geometry, so the results appear to have *become* the chat — which makes a
     plain ✕ a trapdoor out of the search. The Bookmarks pane, the other caller,
     stays open behind and needs none of this. */
  let previewReturn = null;
  /* The address the window was opened at, so "Open chat" can land the real chat on
     the very turn the user was reading rather than at the bottom. */
  let previewAddr = null;
  function previewOpen(){ return !$("#previewview").hidden; }
  async function openPreview(uid, seg, rec, at, from){
    previewReturnFocus = document.activeElement;
    previewReturn = makeReturnContext(from);
    previewAddr = { uid, segment: seg, record: rec, turnTime: at || "" };
    renderPreviewBack();
    $("#previewtitle").textContent = "Chat";
    $("#previewsub").textContent = "";
    $("#previewopen").hidden = true;
    previewNode = null;
    $("#previewbody").innerHTML = `<div class="previewtrunc">Loading…</div>`;
    $("#previewnote").textContent = "";
    $("#previewview").hidden = false;
    try {
      /* anchor by the hit's stable (seg, rec) ordinal — duplicate/empty timestamps
         can't mis-anchor the window; `at` rides along only as a server-side fallback */
      renderPreview(await api("/api/preview?uid=" + encodeURIComponent(uid) +
        "&seg=" + encodeURIComponent(seg || 0) + "&rec=" + encodeURIComponent(rec || 0) +
        "&at=" + encodeURIComponent(at || "")));
    } catch {
      $("#previewbody").innerHTML = `<div class="previewtrunc">This chat could not be loaded.</div>`;
    }
  }
  function renderPreview(d){
    previewNode = d.node || "";
    $("#previewtitle").textContent = d.title || "Deleted chat";
    $("#previewsub").textContent = [d.agent, d.model].filter(Boolean).join(" · ");
    /* The only asymmetry left, and it is stated rather than discovered: a chat
       that still exists gets a way into it; one that does not, simply has no
       button where the button would be. */
    const openBtn = $("#previewopen");
    openBtn.hidden = !previewNode;
    openBtn.innerHTML = ICON_JUMP + " Open chat";
    openBtn.setAttribute("aria-label", "open chat");
    const turns = d.turns || [];
    const rows = turns.map((t, i) => {
      /* a live node's blobs are still served, so its markers resolve exactly as in
         the chat; a deleted node's were archived with it and degrade to an inert
         "unavailable" chip */
      const s = splitAssetRefs(t.text || "", previewNode, d.assets || {}, { iconFile: ICON_FILE, assetURL });
      const role = t.role === "user" ? "user" : "assistant";
      return `<div class="turn ${role} ${i === d.anchor ? "anchor" : ""}">` +
        `<div class="bubble" title="${esc(bubbleTitle(t.role, t.time))}">${md(s.clean)}${s.html}</div></div>`;
    }).join("");
    const before = d.before_truncated ? `<div class="previewtrunc">Earlier turns not shown</div>` : "";
    const after = d.after_truncated ? `<div class="previewtrunc">Later turns not shown</div>` : "";
    $("#previewbody").innerHTML = before + rows + after;
    $("#previewbody").querySelector(".turn.anchor")?.scrollIntoView({ block: "center" });
    /* Say why this is read-only, and say the true reason — "the chat was deleted"
       on a live chat would be a lie the Open chat button immediately contradicts. */
    $("#previewnote").textContent = previewNode
      ? "Read-only excerpt — open the chat to reply, fork, or bookmark."
      : "Read-only — this chat was deleted. Its history is preserved here.";
  }
  function renderPreviewBack(){
    const btn = $("#previewback");
    if (!btn) return;
    const st = returnPillState(previewReturn);
    btn.hidden = !st.hasReturn;
    btn.innerHTML = st.html;
    btn.setAttribute("aria-label", st.ariaLabel);
  }
  /* Every exit is the same exit: the button, the ✕, the scrim and Escape all
     dismiss a pushed modal, and a pushed modal dismisses back to its presenter.
     Labelling only one of them would make the other three the trapdoor again.

     Reopening the overlay suppresses its entry animation for one frame: the panel
     the user is looking at never moved, so popping the results back in would
     announce a new surface where nothing new happened. */
  function closePreview(){
    if (!previewOpen()) return;
    $("#previewview").hidden = true;
    const to = previewReturn;
    const back = previewReturnFocus;
    previewReturn = null;
    previewReturnFocus = null;
    previewAddr = null;
    /* Quietly: the panel underneath never moved, so neither of the overlay's
       entry animations may run. See search.js open(). */
    if (to && to.kind === RETURN_SEARCH){ openSearch({ nopop: true }); return; }  /* it focuses its own field */
    if (back && document.contains(back) && typeof back.focus === "function") back.focus();
  }
  /* The way onward. This is a jump, not a dismissal: the preview is a photo, and
     everything you might want to *do* with a turn — reply, fork, bookmark, copy —
     lives in the real chat. The address rides along so the chat opens on the turn
     that was being read, and the chat's own back button inherits the search trail
     so the trip is still reversible one control at a time. */
  function openPreviewChat(){
    const node = previewNode;
    if (!node) return;
    const addr = previewAddr || {};
    const from = previewReturn;
    previewReturn = null;              /* leaving, not returning: the overlay stays shut */
    closePreview();
    pendingJump = buildPendingJump(node, addr.turnTime || "", addr, Date.now());
    chatFeature.invalidate();
    if (!isDesktop()) setBookmarksOpen(false);
    /* Only the search trail survives the hop. The Bookmarks pane — the other
       presenter — stays open behind the preview and is already where its own ✕
       lands, so a chat reached from there needs no second promise. */
    if (from && from.kind === RETURN_SEARCH) setReturnContext(RETURN_SEARCH, from);
    select(node, "jump");
  }
  $("#previewback").addEventListener("click", closePreview);
  $("#previewclose").addEventListener("click", closePreview);
  $("#previewscrim").addEventListener("click", closePreview);
  $("#previewopen").addEventListener("click", openPreviewChat);
  $("#previewview").addEventListener("keydown", e => {
    if (e.key === "Escape"){ e.preventDefault(); e.stopPropagation(); closePreview(); }
  });

  /* ---- burger menu: remote access ----
     The device list is read when the menu opens and after a revoke, never
     polled: a grant does not change behind the human's back, and a list on
     a one-second timer would be a second poller for a value that moves
     perhaps twice a year. #burger's open transition belongs to sheets.js;
     this listener only asks for fresh data. */
  $("#burger").addEventListener("click", () => {
    deviceList.refresh();
    pairControl.refresh();
    unlinkControl.refresh();
    loadHarnesses();
  });
  /* A dark Claude gauge that the user could switch on is the only badge worth
     tapping, so tapping it goes where the switch lives rather than explaining
     itself in a place with no control. usage.js decides which badges carry the
     marker; this only routes. */
  $("#sys").addEventListener("click", (e) => {
    const badge = e.target && e.target.closest ? e.target.closest("[data-usage-off]") : null;
    if (!badge) return;
    $("#burger").click();
    const sec = $("#m_harnesses");
    if (sec && sec.scrollIntoView) sec.scrollIntoView({ block: "center" });
  });
  $("#m_homescreen").addEventListener("click", () => {
    sheetsFeature.closeSheets();
    homeScreen.move();
  });
  $("#hsoffer_go").addEventListener("click", () => homeScreen.move());
  $("#hsoffer_no").addEventListener("click", () => homeScreen.dismiss());
  $("#m_pair").addEventListener("click", () => {
    sheetsFeature.closeSheets();
    pairingFeature.open();
  });
  /* The pairing overlay is not a `.sheet` and so carries its own scrim
     rather than the shared #backdrop, whose one listener closes every
     sheet unconditionally. Dismissal is the feature's call: a live pairing
     code says no, because a scrim tap that abandoned it would leave a
     minted credential outstanding with no cancel behind it. ✕ is the way out. */
  $("#pairscrim").addEventListener("click", () => {
    if (pairingFeature.dismissable()) pairingFeature.close();
  });

  /* ---- burger menu: manual update check + self-update + about ----
     The check and the download run only on an explicit tap; the server never
     phones home on its own. The install is confirm-first, and the button walks
     through honest states (checking → available → updating → switching). */
  let updateInfo = null;
  $("#m_check").addEventListener("click", async () => {
    const btn = $("#m_check"), tag = $("#m_version");
    btn.disabled = true;
    tag.textContent = "checking…";
    try {
      updateInfo = await api("/api/update/check");
      tag.textContent = updateInfo.current;
      $("#m_result").hidden = false;
      $("#m_reltext").textContent = updateInfo.available
        ? updateInfo.latest + " available" : "up to date (" + updateInfo.latest + ")";
      $("#m_relurl").href = updateInfo.url || "https://codeberg.org/chrberger/scimux/releases";
      $("#m_apply").hidden = !updateInfo.available;
      $("#m_notes").innerHTML = updateInfo.available ? md(updateInfo.notes || "") : "";
      $("#m_notes").hidden = !updateInfo.available || !updateInfo.notes;
    } catch {
      updateInfo = null;
      tag.textContent = "check failed";
    } finally {
      btn.disabled = false;
    }
  });
  $("#m_apply").addEventListener("click", async () => {
    if (!updateInfo || !updateInfo.available) return;
    if (!confirm(`Download ${updateInfo.latest}, verify its checksum, and install the update?\n\n` +
      "Running agent sessions stay connected to their session workers while scimux switches over.")) return;
    const btn = $("#m_apply");
    btn.disabled = true;
    btn.textContent = "updating…";
    try {
      await api("/api/update", { method: "POST", body: JSON.stringify({ expected_tag: updateInfo.latest }) });
      btn.textContent = "switching…";
      const want = updateInfo.latest, t0 = Date.now();
      while (Date.now() - t0 < 60000){
        await new Promise(res => setTimeout(res, 1000));
        try {
          const st = await api("/api/state", { cache: "no-store" });
          if (st.version === want){ location.reload(); return; }
        } catch {}
      }
      btn.textContent = "switch timed out — reload the page";
    } catch (e) {
      alert("Update failed: " + (e.message || e));
      btn.disabled = false;
      btn.textContent = "Install update";
    }
  });
  /* ---- burger menu: which agent harnesses this computer has ----
     The inventory is local and read when the menu opens; the server probes
     `--version` once per process, so this is a cached answer and the panel is
     populated before the human reads down to it. The upstream check is a
     separate tap for the same reason the scimux one is: opening a menu must
     not call six registries, and "up to date" is a claim only a check makes. */
  let harnessRows = null, harnessLatest = null, usageChecks = false, museConsent = false;
  function renderHarnesses(){
    if (!harnessRows) return;
    $("#m_harnesses").innerHTML = harnessRowsHTML(harnessRows, harnessLatest, {
      agentLogo, usageChecks, museConsent,
    });
    const note = harnessCheckNote(harnessRows, harnessLatest);
    $("#m_hnote").textContent = note;
    $("#m_hnote").hidden = !note;
  }
  const settingsCtl = createSettingsController({
    read: () => api("/api/settings"),
    write: body => api("/api/settings", { method: "PUT", body: JSON.stringify(body) }),
    render: state => {
      usageChecks = !!state.claude_usage_checks;
      museConsent = !!state.muse_approval_judge_consent;
      renderHarnesses();
    },
  });
  async function loadHarnesses(){
    /* Consent flags are re-read on every menu open even when the inventory
       is already cached: they are the values here another device can change,
       and a switch showing the wrong state is worse than no switch. */
    await settingsCtl.load();
    if (!harnessRows){
      try {
        const d = await api("/api/harnesses");
        harnessRows = Array.isArray(d.harnesses) ? d.harnesses : [];
      } catch { return; }
    }
    renderHarnesses();
  }
  /* Switches write through the tested controller and re-render from confirmed
     server state, never from the checkbox. */
  $("#m_harnesses").addEventListener("change", (e) => {
    const usageBox = e.target && e.target.closest ? e.target.closest("[data-usage-check]") : null;
    const museBox = e.target && e.target.closest ? e.target.closest("[data-muse-consent]") : null;
    if (usageBox){
      settingsCtl.setClaudeUsage(!!usageBox.checked);
      return;
    }
    if (museBox) settingsCtl.setMuseConsent(!!museBox.checked);
  });
  $("#m_hcheck").addEventListener("click", async () => {
    const btn = $("#m_hcheck");
    btn.disabled = true;
    btn.textContent = "checking…";
    try {
      const d = await api("/api/harnesses/latest");
      harnessLatest = (d && d.latest) || {};
      renderHarnesses();
      btn.textContent = "Check for harness updates";
    } catch {
      btn.textContent = "check failed";
    } finally {
      btn.disabled = false;
    }
  });

  /* license texts: fetched once on first tap, folded like everything else */
  let licenseTexts = null;
  document.querySelectorAll("#menu .lic").forEach(b => b.addEventListener("click", async () => {
    const pre = $("#m_lictext");
    if (!licenseTexts){
      try { licenseTexts = await api("/api/licenses"); } catch { return; }
    }
    if (!pre.hidden && pre.dataset.lic === b.dataset.lic){ pre.hidden = true; return; }
    pre.textContent = licenseTexts[b.dataset.lic] || "";
    pre.dataset.lic = b.dataset.lic;
    pre.hidden = false;
  }));

  /* termtoggle + scrollend listeners live in chat.js bind (Packet 7C). */

  /* New activity / adopt / agents probe / fieldError / launch-config / plus /
     nc_start / ad_go live in createSheetsFeature (Packet 7H). */

  /* ---------- polling + UI-sync (Packet 7I) ----------
     createPollingFeature owns /api/state tick, generation/cancellation,
     visibility pause/resume, UI document mutate/load/flush/poll, and pagehide.
     Usage badge HTML and the 30s cadence stay shell-owned; only the prime is
     injected as onStartPolling. */
  pollingFeature = own(createPollingFeature({
    document,
    window,
    storage: localStorage,
    fetchImpl,
    csrf: CSRF,
    setTimeout,
    clearTimeout,
    syncwarnEl: $("#syncwarn"),
    setHostOnline: ok => {
      $("#host").classList.toggle("online", ok);
      $("#host").classList.toggle("offline", !ok);
      /* The dot and the words are one status; a recovery that moved only the
         dot left "server unreachable" standing over a working tunnel. */
      if (ok) setStatusUnreachable($("#sys"), false);
    },
    setServerUnreachable: () => setStatusUnreachable($("#sys"), true),
    setHostname: h => { $("#host").textContent = h; },
    setVersion: v => { $("#m_version").textContent = v; },
    publishState: st => {
      nodes = st.nodes || [];
      unadopted = st.unadopted || [];
      let nextSeen = pruneReadySeen(readySeen, nodes);
      const open = nodeById(sel);
      if (open && open.turn_done) nextSeen = ackReadySeen(nextSeen, open);
      if (nextSeen !== readySeen){
        readySeen = nextSeen;
        saveReadySeen(localStorage, readySeen);
      }
    },
    renderSys,
    ensureSelection: () => {
      /* a stored sel of "NOTES" (the retired pseudo-activity) falls through
         to the first real node here */
      if (!sel || !nodeById(sel)){
        const first = orderedNodes()[0];
        if (first) select(first.id);
        /* The chat pane is showing a *substitute* now — a conversation the user
           never left off in. Only level 1 lies about it; cards and map are
           already honest, and desktop has no pane stack. */
        if (!isDesktop() && level === 1) setLevel(2);
      }
    },
    updateCardAges: () => updateCardAges(),
    renderCards: () => renderCards(),
    renderMap: () => renderMap(),
    renderMapTabs: () => renderMapTabs(),
    renderBookmarksPane: () => renderBookmarksPane(),
    renderChatHead: () => renderChatHead(),
    refreshChat: () => refreshChat(),
    renderWsInbox: () => renderWsInbox(),
    invalidateCardsSig: () => { cardsSig = ""; },
    invalidateMap: () => mapFeature.invalidate(),
    invalidateBookmarks: () => bookmarksFeature.invalidate(),
    invalidateChat: () => chatFeature.invalidate(),
    onStartPolling: () => readUsage(), /* prime; 30s cadence stays shell-owned */
    /* A phone that sleeps mid-pairing must not keep polling a code that
       expired while the screen was off; the reducer owns what that means.
       Routed through polling.js because the document's visibilitychange
       has exactly one owner. */
    onPageVisibility: visible => pairingFeature.setVisible(visible),
  }));
  pollingFeature.bind();
  /* iOS leaves the layout viewport at the landscape height after rotating back to
     portrait, pinning the fixed shell above the visible area and sliding the
     status bar under the OS clock. Feed the measured shortfall back as --vvtop. */
  teardown.add(installInsetRefresh({
    win: window,
    doc: document,
    apply: px => document.documentElement.style.setProperty("--vvtop", px + "px"),
    raf: fn => requestAnimationFrame(fn),
    setTimeout: (fn, ms) => setTimeout(fn, ms),
  }));
  setLevel(level);
  renderJourneyToggle();
  renderChatBack();
  /* the Journeys chevron is breakpoint-dependent: the desktop toggle becomes a
     phone forward-arrow when the pane stops being a toggle */
  teardown.on(window, "resize", renderJourneyToggle);
  /* crossing the workspace's zone breakpoint changes which actions a bookmark
     row offers, so re-render the pane on the edge rather than on every resize */
  teardown.on(matchMedia(SINGLE_ZONE_QUERY), "change", () => {
    bookmarksFeature.invalidate();
    renderBookmarksPane();
  });
  /* P5/P5b: crossing isDesktop() flips flat bar ↔ overflow more-menu and
     re-clamps a persisted too-narrow inboxW to the flat-row floor. */
  teardown.on(matchMedia("(min-width: 900px)"), "change", () => {
    bookmarksFeature.invalidate();
    renderBookmarksPane();
    if (typeof notesFeature.applyLayout === "function") notesFeature.applyLayout();
    if (typeof notesFeature.invalidateInbox === "function") notesFeature.invalidateInbox();
    if (typeof notesFeature.renderInbox === "function") notesFeature.renderInbox();
  });
  /* restore persisted full-screen / fare chrome (map feature owns keys + ARIA) */
  mapFeature.restoreChrome();
  renderMapTabs();
  pollingFeature.loadUI();
  pollingFeature.startPolling();
  /* The handle a successor's claimAppSlot retires, and the one a caller
     that owns the page can retire itself. */
  return instance;
}

/* Browser entry: index.html loads this module as the sole script. Tests import
   createApp over file: URLs and pass their own suppliers — never auto-boot. */
if (/^https?:/i.test(import.meta.url)) {
  createApp({
    fetchImpl: globalThis.fetch.bind(globalThis),
    assetURL: path => path,
    document: globalThis.document,
    window: globalThis.window,
  });
}
