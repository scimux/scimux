/* P5 — shared popover menu module (pure refactor of notes .wsmenu).
 *
 * Cases 1, 2, 3, 5, 6 from ux-fixes.md P5, with Correction 1–3 applied:
 *   - case 1 driven at BOTH offsets (150 section menu, 40 delete confirm)
 *   - case 4 (Escape) dropped — not existing behaviour
 *   - plus a pin for delete-confirm placement specifically
 *
 * menu.js does not exist on the red commit: a static namespace import would
 * abort this whole file. Dynamic import keeps the file loadable so each
 * missing export fails as its own case. */
import test from "node:test";
import assert from "node:assert/strict";
import { readdirSync, readFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const cssDir = join(__dirname, "../css");
const menuJsPath = join(__dirname, "../js/menu.js");

async function loadMenu(){
  return import("../js/menu.js");
}

/* ---------- fakes ---------- */

function makeStyle(){
  return {};
}

function makeEl(tag = "div", props = {}){
  const listeners = {};
  const node = {
    tagName: tag.toUpperCase(),
    className: props.className || "",
    innerHTML: props.innerHTML || "",
    style: makeStyle(),
    children: [],
    parentNode: null,
    dataset: Object.assign({}, props.dataset || {}),
    getBoundingClientRect: props.getBoundingClientRect || (() => ({
      top: 0, left: 0, bottom: 0, right: 0, width: 0, height: 0,
    })),
    addEventListener(type, fn){
      (listeners[type] || (listeners[type] = [])).push(fn);
    },
    removeEventListener(type, fn){
      if (!listeners[type]) return;
      listeners[type] = listeners[type].filter(f => f !== fn);
    },
    dispatch(type, ev = {}){
      const e = Object.assign({
        type, target: node, currentTarget: node,
        preventDefault(){}, stopPropagation(){},
      }, ev);
      for (const fn of listeners[type] || []) fn(e);
    },
    appendChild(child){
      child.parentNode = node;
      node.children.push(child);
      return child;
    },
    remove(){
      if (!node.parentNode) return;
      const kids = node.parentNode.children;
      const i = kids.indexOf(node);
      if (i >= 0) kids.splice(i, 1);
      node.parentNode = null;
    },
    closest(sel){
      if (sel === ".popmenu" || sel === ".wsmenu"){
        if ((node.className || "").includes("popmenu") ||
            (node.className || "").includes("wsmenu")) return node;
      }
      if (sel === "[data-secmenu]" && node.dataset && node.dataset.secmenu != null)
        return node;
      return null;
    },
  };
  return node;
}

function makeDoc(){
  return {
    createElement(tag){ return makeEl(tag); },
  };
}

function makeHost(anchorRect, panelRect){
  const doc = makeDoc();
  const panel = makeEl("div");
  panel.getBoundingClientRect = () => ({
    top: 0, left: 0, width: 400, height: 600,
    ...panelRect,
  });
  const anchor = makeEl("button");
  anchor.getBoundingClientRect = () => ({
    top: 10, left: 200, bottom: 50, right: 244, width: 44, height: 40,
    ...anchorRect,
  });
  return { doc, panel, anchor };
}

/* Strip CSS block comments so braces inside them do not affect balance. */
function stripCssComments(src){
  return src.replace(/\/\*[\s\S]*?\*\//g, "");
}

function braceWalk(src){
  let depth = 0;
  let minDepth = 0;
  for (const ch of src){
    if (ch === "{") depth++;
    else if (ch === "}"){
      depth--;
      if (depth < minDepth) minDepth = depth;
    }
  }
  return { depth, minDepth };
}

/* ---------- case 1: pure placement at BOTH offsets ---------- */

test("P5 1a: menuPlacement with section-menu offset 150", async () => {
  const menu = await loadMenu();
  assert.equal(typeof menu.menuPlacement, "function", "menuPlacement must be exported");
  /* mid-panel: top = bottom - panelTop + 4; left = max(8, min(left - panelLeft - 150, width - 190)) */
  const pos = menu.menuPlacement(
    { bottom: 100, left: 200 },
    { top: 0, left: 0, width: 400 },
    { offset: 150 },
  );
  assert.deepEqual(pos, { top: 104, left: 50 });
});

test("P5 1b: menuPlacement with delete-confirm offset 40", async () => {
  const menu = await loadMenu();
  assert.equal(typeof menu.menuPlacement, "function", "menuPlacement must be exported");
  const pos = menu.menuPlacement(
    { bottom: 100, left: 200 },
    { top: 0, left: 0, width: 400 },
    { offset: 40 },
  );
  /* same anchor as 1a but offset 40 → left 160, not 50 (110px difference) */
  assert.deepEqual(pos, { top: 104, left: 160 });
});

test("P5 1c: menuPlacement clamps left edge to 8 and right to width-190", async () => {
  const menu = await loadMenu();
  const leftClamp = menu.menuPlacement(
    { bottom: 50, left: 20 },
    { top: 0, left: 0, width: 400 },
    { offset: 150 },
  );
  assert.deepEqual(leftClamp, { top: 54, left: 8 });

  const rightClamp = menu.menuPlacement(
    { bottom: 50, left: 380 },
    { top: 0, left: 0, width: 400 },
    { offset: 40 },
  );
  assert.deepEqual(rightClamp, { top: 54, left: 210 });
});

/* Pin the delete-confirm consumer specifically — the extraction is most likely
   to bake in the section-menu offset of 150 and silently move this popover. */
test("P5 1d: delete-confirm position pin (offset 40, real openDeleteConfirm rects)", async () => {
  const menu = await loadMenu();
  /* mirrors notes.test.js card-delete fixtures: anchor bottom 50 left 10,
     panel top 0 left 0 width 400 — the live openDeleteConfirm arithmetic */
  const pos = menu.menuPlacement(
    { bottom: 50, left: 10 },
    { top: 0, left: 0, width: 400 },
    { offset: 40 },
  );
  assert.deepEqual(pos, { top: 54, left: 8 });
  /* and open must apply those exact px strings for offset 40 */
  assert.equal(typeof menu.createPopoverMenu, "function");
  const { doc, panel, anchor } = makeHost(
    { bottom: 50, left: 10 },
    { top: 0, left: 0, width: 400 },
  );
  const ctl = menu.createPopoverMenu(doc);
  const el = ctl.open({
    panel, anchor, offset: 40,
    className: "popmenu wsconfirm",
    html: `<div class="wsconfirmmsg">Delete note?</div>`,
  });
  assert.equal(el.style.top, "54px");
  assert.equal(el.style.left, "8px");
  assert.match(el.className, /popmenu/);
  assert.match(el.className, /wsconfirm/);
});

/* ---------- case 2: open — exactly one menu; second open closes first ---------- */

test("P5 2: open leaves exactly one menu; second open replaces the first", async () => {
  const menu = await loadMenu();
  assert.equal(typeof menu.createPopoverMenu, "function");
  const { doc, panel, anchor } = makeHost();
  const ctl = menu.createPopoverMenu(doc);
  const first = ctl.open({
    panel, anchor, offset: 150,
    className: "popmenu",
    html: `<button data-mi="a">A</button>`,
  });
  assert.equal(panel.children.length, 1);
  assert.equal(ctl.element(), first);

  const second = ctl.open({
    panel, anchor, offset: 150,
    className: "popmenu",
    html: `<button data-mi="b">B</button>`,
  });
  assert.equal(panel.children.length, 1, "second open must close the first");
  assert.equal(ctl.element(), second);
  assert.notEqual(first, second);
  assert.equal(first.parentNode, null, "first menu removed from the panel");
  assert.match(second.innerHTML, /data-mi="b"/);
});

/* ---------- case 3: outside click closes (existing behaviour, no Escape) ---------- */

test("P5 3: outside click closes; click inside or on exclude does not", async () => {
  const menu = await loadMenu();
  const { doc, panel, anchor } = makeHost();
  const ctl = menu.createPopoverMenu(doc);
  ctl.open({
    panel, anchor, offset: 150,
    className: "popmenu",
    html: `<button data-mi="x">X</button>`,
  });
  assert.ok(ctl.element());
  assert.equal(typeof ctl.shouldCloseForClick, "function");

  /* outside: no .popmenu ancestor, no exclude match → dismiss */
  const outside = makeEl("div");
  assert.equal(ctl.shouldCloseForClick(outside, { exclude: "[data-secmenu]" }), true);
  if (ctl.shouldCloseForClick(outside, { exclude: "[data-secmenu]" })) ctl.close();
  assert.equal(ctl.element(), null, "outside click closes");
  assert.equal(panel.children.length, 0);

  /* re-open for the non-dismiss paths */
  const m = ctl.open({
    panel, anchor, offset: 150,
    className: "popmenu",
    html: `<button data-mi="x">X</button>`,
  });
  assert.equal(ctl.shouldCloseForClick(m, { exclude: "[data-secmenu]" }), false,
    "click on the menu itself must not dismiss");
  const insideBtn = makeEl("button");
  /* closest climbs; fake a child whose closest returns the menu for .popmenu */
  insideBtn.closest = sel => (sel === ".popmenu" ? m : null);
  assert.equal(ctl.shouldCloseForClick(insideBtn, { exclude: "[data-secmenu]" }), false);

  /* notes quirk preserved: click on [data-secmenu] does not close */
  const trigger = makeEl("button");
  trigger.dataset.secmenu = "";
  trigger.closest = sel => {
    if (sel === "[data-secmenu]") return trigger;
    return null;
  };
  assert.equal(ctl.shouldCloseForClick(trigger, { exclude: "[data-secmenu]" }), false);

  /* null target does not close (matches notes onDocClick short-circuit) */
  assert.equal(ctl.shouldCloseForClick(null, { exclude: "[data-secmenu]" }), false);
  assert.ok(ctl.element(), "menu still open after non-dismiss clicks");
});

/* ---------- case 5: destructive item carries .danger, last after .sep ---------- */

test("P5 5: destructive item is .danger and sits last after a .sep", async () => {
  const menu = await loadMenu();
  assert.equal(typeof menu.menuButtonHTML, "function");
  assert.equal(typeof menu.menuSepHTML, "function");

  const html =
    menu.menuButtonHTML({ attrs: 'data-mi="add"', label: "Add section below", icon: "+" }) +
    menu.menuButtonHTML({ attrs: 'data-mi="edit"', label: "Edit", icon: "P" }) +
    menu.menuSepHTML() +
    menu.menuButtonHTML({ attrs: 'data-mi="del"', label: "Delete section", icon: "T", danger: true });

  assert.match(html, /class="danger"/);
  assert.match(html, /data-mi="del"[^>]*class="danger"|class="danger"[^>]*data-mi="del"/);
  const sepIdx = html.indexOf('class="sep"');
  assert.ok(sepIdx >= 0, "separator present");
  const dangerIdx = html.indexOf("danger");
  assert.ok(dangerIdx > sepIdx, "danger follows the separator");
  /* danger is the last button */
  const lastBtn = html.lastIndexOf("<button");
  assert.ok(lastBtn > sepIdx);
  assert.match(html.slice(lastBtn), /class="danger"/);
});

/* ---------- case 6: menu.css enumerated; brace depth 0 ---------- */

test("P5 6: web/css/menu.css is enumerated and brace depth returns to 0", () => {
  /* smoke.test.js readdirSync-enumerates web/css; this case pins the new file
     exists and is structurally sound. Do not extend the smoke guard. */
  const files = readdirSync(cssDir).filter(f => f.endsWith(".css")).sort();
  assert.ok(files.includes("menu.css"), "menu.css must live under web/css");
  const raw = readFileSync(join(cssDir, "menu.css"), "utf8");
  const { depth, minDepth } = braceWalk(stripCssComments(raw));
  assert.equal(depth, 0, `menu.css: brace depth ends at ${depth}, expected 0`);
  assert.ok(minDepth >= 0, `menu.css: brace depth went negative (min ${minDepth})`);
  /* class rename: generic rules use .popmenu, not .wsmenu */
  assert.match(raw, /\.popmenu\s*\{/);
  assert.match(raw, /\.popmenu\s+button\.danger/);
  assert.match(raw, /\.popmenu\s+\.sep/);
  assert.doesNotMatch(raw, /\.wsmenu/);
  /* note-specific .wsconfirmmsg must NOT have been dragged into the generic file */
  assert.doesNotMatch(raw, /wsconfirmmsg/);
});

/* ---------- P6: Escape closes the open popover; focus returns to trigger ---------- */

test("P6 Escape: Escape closes the open popover", async () => {
  const menu = await loadMenu();
  const { doc, panel, anchor } = makeHost();
  /* doc must accept keydown listeners for Escape */
  const keyListeners = [];
  doc.addEventListener = (type, fn) => {
    if (type === "keydown") keyListeners.push(fn);
  };
  doc.removeEventListener = (type, fn) => {
    if (type === "keydown"){
      const i = keyListeners.indexOf(fn);
      if (i >= 0) keyListeners.splice(i, 1);
    }
  };
  const ctl = menu.createPopoverMenu(doc);
  const trigger = makeEl("button");
  trigger.focus = () => { trigger._focused = true; };
  const el = ctl.open({
    panel, anchor, offset: 150,
    className: "popmenu",
    html: `<button data-mi="x">X</button>`,
    trigger,
  });
  assert.ok(el);
  assert.equal(ctl.element(), el);
  assert.ok(keyListeners.length >= 1, "Escape keydown listener must be registered on open");

  for (const fn of keyListeners.slice()){
    fn({ key: "Escape", preventDefault(){}, stopPropagation(){} });
  }
  assert.equal(ctl.element(), null, "Escape must close the menu");
  assert.equal(panel.children.length, 0);
});

test("P6 Escape: focus returns to the trigger element", async () => {
  const menu = await loadMenu();
  const { doc, panel, anchor } = makeHost();
  const keyListeners = [];
  doc.addEventListener = (type, fn) => {
    if (type === "keydown") keyListeners.push(fn);
  };
  doc.removeEventListener = (type, fn) => {
    if (type === "keydown"){
      const i = keyListeners.indexOf(fn);
      if (i >= 0) keyListeners.splice(i, 1);
    }
  };
  const ctl = menu.createPopoverMenu(doc);
  const trigger = makeEl("button");
  let focused = false;
  trigger.focus = () => { focused = true; trigger._focused = true; };
  ctl.open({
    panel, anchor, offset: 150,
    className: "popmenu",
    html: `<button data-mi="x">X</button>`,
    trigger,
  });
  for (const fn of keyListeners.slice()){
    fn({ key: "Escape", preventDefault(){}, stopPropagation(){} });
  }
  assert.equal(focused, true, "focus must return to the trigger passed via open() options");
  /* trigger arrives through open() options — not document.activeElement at module scope */
  assert.doesNotMatch(
    readFileSync(menuJsPath, "utf8").replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/[^\n]*/g, ""),
    /document\.activeElement/,
  );
});

/* ---------- structural pin: menu.js is importable under bare Node ---------- */

test("P5: menu.js takes no implicit document global at module scope", async () => {
  /* enrolment puts menu.js under offline-import-smoke; it must not touch
     document/window at load time. Strip comments before scanning. */
  assert.ok(existsSync(menuJsPath), "web/js/menu.js must exist");
  const src = readFileSync(menuJsPath, "utf8");
  const code = src
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/\/\/[^\n]*/g, "");
  assert.doesNotMatch(code, /\bdocument\.(createElement|body|getElementById|querySelector)\b/);
  assert.doesNotMatch(code, /\bwindow\./);
  /* bare global `document` as an expression — allow the createPopoverMenu(doc) param */
  assert.doesNotMatch(code, /[^\w.]document[^\w]/);
  const menu = await loadMenu();
  assert.equal(typeof menu.createPopoverMenu, "function");
  assert.equal(typeof menu.menuPlacement, "function");
  assert.equal(typeof menu.menuButtonHTML, "function");
  assert.equal(typeof menu.menuSepHTML, "function");
});

/* ---------- P7: .popmenu button is a 44px touch target ----------
 * Match the rule BODY. A file-wide /min-height:\s*44px/ would pass if the
 * declaration landed on .popmenu .sep (a 44px-tall hairline). Mutation
 * check: put min-height on .sep and confirm this fails. */

const menuCssSrc = readFileSync(join(cssDir, "menu.css"), "utf8");

function popmenuButtonRuleBody(){
  const m = menuCssSrc.match(/\.popmenu\s+button\s*\{([^}]+)\}/);
  assert.ok(m, ".popmenu button rule present");
  return m[1];
}

test("P7: .popmenu button body has min-height: 44px", () => {
  const body = popmenuButtonRuleBody();
  assert.match(body, /min-height:\s*44px/,
    ".popmenu button must declare min-height: 44px");
});
