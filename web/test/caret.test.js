/* P3 — caret at the end, never select-all.
 *
 * Pure caret.js unit cases (1–4) plus the app.js source pin (case 5), then
 * Phase 3 focusAtEnd branch coverage for missing targets, thrown selection,
 * contenteditable fallbacks, and sparse DOM hosts. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const appSrc = readFileSync(join(__dirname, "../js/app.js"), "utf8");

async function loadCaret(){
  return import("../js/caret.js");
}

/* ---------- cases 1–2: pure caretTarget ---------- */

test("P3 1: caretTarget(12) is a collapsed range at the end", async () => {
  const caret = await loadCaret();
  assert.equal(typeof caret.caretTarget, "function", "caretTarget must be exported");
  assert.deepEqual(caret.caretTarget(12), { start: 12, end: 12 });
});

test("P3 2: caretTarget(0) is a collapsed range at zero", async () => {
  const caret = await loadCaret();
  assert.equal(typeof caret.caretTarget, "function", "caretTarget must be exported");
  assert.deepEqual(caret.caretTarget(0), { start: 0, end: 0 });
});

/* ---------- cases 3–4: focusAtEnd on input / contenteditable ---------- */

function fakeInput(value){
  const el = {
    tagName: "INPUT",
    value: value || "",
    selectionStart: 0,
    selectionEnd: 0,
    _focused: false,
    _selected: false,
    _range: null,
    focus(){ el._focused = true; },
    select(){ el._selected = true; },
    setSelectionRange(a, b){
      el.selectionStart = a;
      el.selectionEnd = b;
      el._range = [a, b];
    },
  };
  return el;
}

function fakeContentEditable(text, { nested = false } = {}){
  const textNode = {
    nodeType: 3,
    textContent: text || "",
    get lastChild(){ return null; },
  };
  const ranges = [];
  const selection = {
    ranges,
    removeAllRanges(){ ranges.length = 0; },
    addRange(r){ ranges.push(r); },
  };
  const doc = {
    createRange(){
      return {
        startContainer: null, startOffset: 0,
        endContainer: null, endOffset: 0,
        collapsed: false,
        setStart(n, o){ this.startContainer = n; this.startOffset = o; },
        setEnd(n, o){ this.endContainer = n; this.endOffset = o; this.collapsed = this.startContainer === n && this.startOffset === o; },
        selectNodeContents(n){
          this.startContainer = n;
          this.startOffset = 0;
          this.endContainer = n;
          this.endOffset = (n.textContent || "").length;
        },
        collapse(toStart){
          if (toStart){
            this.endContainer = this.startContainer;
            this.endOffset = this.startOffset;
          } else {
            this.startContainer = this.endContainer;
            this.startOffset = this.endOffset;
          }
          this.collapsed = true;
        },
      };
    },
    getSelection(){ return selection; },
  };
  let lastChild = text ? textNode : null;
  let childNodes = text ? [textNode] : [];
  if (nested && text){
    const inner = {
      nodeType: 1,
      textContent: text,
      childNodes: [textNode],
      lastChild: textNode,
    };
    textNode.parentNode = inner;
    lastChild = inner;
    childNodes = [inner];
  }
  const el = {
    tagName: "DIV",
    isContentEditable: true,
    contentEditable: "true",
    textContent: text || "",
    childNodes,
    lastChild,
    ownerDocument: doc,
    _focused: false,
    focus(){ el._focused = true; },
  };
  if (!nested && textNode) textNode.parentNode = el;
  return { el, selection, textNode, doc };
}

test("P3 3: focusAtEnd on an input sets a collapsed range at len; never select()", async () => {
  const caret = await loadCaret();
  assert.equal(typeof caret.focusAtEnd, "function", "focusAtEnd must be exported");
  const el = fakeInput("hello world");
  caret.focusAtEnd(el);
  assert.equal(el._focused, true, "must focus");
  assert.deepEqual(el._range, [11, 11], "caret at end, nothing selected");
  assert.equal(el.selectionStart, 11);
  assert.equal(el.selectionEnd, 11);
  assert.equal(el._selected, false, "select() must never be called");
});

test("P3 4: focusAtEnd on contenteditable places a collapsed range at the last text node", async () => {
  const caret = await loadCaret();
  assert.equal(typeof caret.focusAtEnd, "function", "focusAtEnd must be exported");
  const { el, selection, textNode } = fakeContentEditable("merged draft");
  caret.focusAtEnd(el);
  assert.equal(el._focused, true, "must focus");
  assert.equal(selection.ranges.length, 1, "exactly one range");
  const r = selection.ranges[0];
  assert.equal(r.collapsed, true, "range is collapsed (nothing selected)");
  assert.equal(r.startContainer, textNode, "caret sits on the last text node");
  assert.equal(r.startOffset, "merged draft".length, "caret at end of text");
  assert.equal(r.endOffset, "merged draft".length);
});

/* ---------- case 5: app.js focusTitleEditorNow (source-level; app is not importable) ---------- */

test("P3 5: focusTitleEditorNow uses focusAtEnd; only copyText may call .select()", () => {
  /* Established idiom: usage.test.js, returnto.test.js, map.test.js read app.js
     as source text. Behavioural focusAtEnd coverage is cases 3–4 above. */
  assert.match(appSrc, /function focusTitleEditorNow\b/, "focusTitleEditorNow still exists");
  assert.match(appSrc, /from "\.\/caret\.js"/, "app imports the caret helper");
  assert.match(appSrc, /focusAtEnd\s*\(/, "app calls focusAtEnd");

  /* Isolate copyText — its ta.select() is the sanctioned clipboard fallback and
     must survive. Every other .select() in app.js is the bug this phase fixes. */
  const withoutCopy = appSrc.replace(
    /function copyText\s*\([^)]*\)\s*\{[\s\S]*?\n\}/,
    "function copyText(){/*stripped*/}",
  );
  assert.doesNotMatch(withoutCopy, /\.select\s*\(/,
    "no .select() outside copyText — focusTitleEditorNow must not select-all");
  assert.match(appSrc, /function copyText[\s\S]*?\.select\s*\(/,
    "copyText still selects for the execCommand clipboard fallback");
});

/* ---------- Phase 3: focusAtEnd branch coverage ---------- */

test("focusAtEnd: missing or null target is a safe no-op", async () => {
  const caret = await loadCaret();
  assert.doesNotThrow(() => caret.focusAtEnd(null));
  assert.doesNotThrow(() => caret.focusAtEnd(undefined));
  assert.doesNotThrow(() => caret.focusAtEnd(false));
});

test("focusAtEnd: textarea uses setSelectionRange at value length", async () => {
  const caret = await loadCaret();
  const el = fakeInput("abc");
  el.tagName = "TEXTAREA";
  caret.focusAtEnd(el);
  assert.equal(el._focused, true);
  assert.deepEqual(el._range, [3, 3]);
  assert.equal(el._selected, false);
});

test("focusAtEnd: setSelectionRange throw is swallowed; still focused", async () => {
  const caret = await loadCaret();
  const el = fakeInput("typed");
  el.setSelectionRange = () => { throw new Error("non-text input"); };
  assert.doesNotThrow(() => caret.focusAtEnd(el));
  assert.equal(el._focused, true);
  assert.equal(el._selected, false, "must not fall through to select()");
});

test("focusAtEnd: missing ownerDocument is a focused no-op for contenteditable", async () => {
  const caret = await loadCaret();
  const el = {
    tagName: "DIV",
    isContentEditable: true,
    ownerDocument: null,
    _focused: false,
    focus(){ el._focused = true; },
  };
  assert.doesNotThrow(() => caret.focusAtEnd(el));
  assert.equal(el._focused, true);
});

test("focusAtEnd: missing createRange is a focused no-op", async () => {
  const caret = await loadCaret();
  const el = {
    tagName: "DIV",
    isContentEditable: true,
    ownerDocument: {},
    _focused: false,
    focus(){ el._focused = true; },
  };
  assert.doesNotThrow(() => caret.focusAtEnd(el));
  assert.equal(el._focused, true);
});

test("focusAtEnd: contenteditable without a text node selects contents and collapses to end", async () => {
  const caret = await loadCaret();
  const { el, selection } = fakeContentEditable("");
  el.childNodes = [];
  el.lastChild = null;
  el.textContent = "";
  caret.focusAtEnd(el);
  assert.equal(el._focused, true);
  assert.equal(selection.ranges.length, 1);
  const r = selection.ranges[0];
  assert.equal(r.collapsed, true);
  assert.equal(r.startContainer, el);
  assert.equal(r.startOffset, 0);
  assert.equal(r.endOffset, 0);
});

test("focusAtEnd: selection from document.defaultView.getSelection when doc has none", async () => {
  const caret = await loadCaret();
  const ranges = [];
  const selection = {
    ranges,
    removeAllRanges(){ ranges.length = 0; },
    addRange(r){ ranges.push(r); },
  };
  const textNode = { nodeType: 3, textContent: "via-view", lastChild: null };
  const el = {
    tagName: "DIV",
    isContentEditable: true,
    textContent: "via-view",
    childNodes: [textNode],
    lastChild: textNode,
    _focused: false,
    focus(){ el._focused = true; },
    ownerDocument: {
      createRange(){
        return {
          startContainer: null, startOffset: 0,
          endContainer: null, endOffset: 0,
          collapsed: false,
          setStart(n, o){ this.startContainer = n; this.startOffset = o; },
          setEnd(n, o){ this.endContainer = n; this.endOffset = o; this.collapsed = true; },
          selectNodeContents(){},
          collapse(){ this.collapsed = true; },
        };
      },
      defaultView: { getSelection(){ return selection; } },
    },
  };
  textNode.parentNode = el;
  caret.focusAtEnd(el);
  assert.equal(el._focused, true);
  assert.equal(selection.ranges.length, 1);
  assert.equal(selection.ranges[0].startContainer, textNode);
  assert.equal(selection.ranges[0].startOffset, "via-view".length);
});

test("focusAtEnd: missing selection is a focused no-op after building the range", async () => {
  const caret = await loadCaret();
  const textNode = { nodeType: 3, textContent: "x", lastChild: null };
  let rangeBuilt = false;
  const el = {
    tagName: "DIV",
    isContentEditable: true,
    textContent: "x",
    childNodes: [textNode],
    lastChild: textNode,
    _focused: false,
    focus(){ el._focused = true; },
    ownerDocument: {
      createRange(){
        rangeBuilt = true;
        return {
          setStart(){}, setEnd(){}, selectNodeContents(){}, collapse(){},
        };
      },
      /* neither getSelection nor defaultView */
    },
  };
  textNode.parentNode = el;
  assert.doesNotThrow(() => caret.focusAtEnd(el));
  assert.equal(el._focused, true);
  assert.equal(rangeBuilt, true);
});

test("focusAtEnd: selection lacking removeAllRanges still adds the range", async () => {
  const caret = await loadCaret();
  const ranges = [];
  const selection = {
    ranges,
    addRange(r){ ranges.push(r); },
  };
  const textNode = { nodeType: 3, textContent: "keep", lastChild: null };
  const el = {
    tagName: "DIV",
    isContentEditable: true,
    textContent: "keep",
    childNodes: [textNode],
    lastChild: textNode,
    _focused: false,
    focus(){ el._focused = true; },
    ownerDocument: {
      createRange(){
        return {
          startContainer: null, startOffset: 0,
          endContainer: null, endOffset: 0,
          collapsed: false,
          setStart(n, o){ this.startContainer = n; this.startOffset = o; },
          setEnd(n, o){ this.endContainer = n; this.endOffset = o; this.collapsed = true; },
        };
      },
      getSelection(){ return selection; },
    },
  };
  textNode.parentNode = el;
  caret.focusAtEnd(el);
  assert.equal(selection.ranges.length, 1);
  assert.equal(selection.ranges[0].startOffset, 4);
});

test("focusAtEnd: selection lacking addRange clears without throwing", async () => {
  const caret = await loadCaret();
  let cleared = false;
  const selection = {
    removeAllRanges(){ cleared = true; },
  };
  const textNode = { nodeType: 3, textContent: "solo", lastChild: null };
  const el = {
    tagName: "DIV",
    isContentEditable: true,
    textContent: "solo",
    childNodes: [textNode],
    lastChild: textNode,
    _focused: false,
    focus(){ el._focused = true; },
    ownerDocument: {
      createRange(){
        return {
          setStart(){}, setEnd(){},
        };
      },
      getSelection(){ return selection; },
    },
  };
  textNode.parentNode = el;
  assert.doesNotThrow(() => caret.focusAtEnd(el));
  assert.equal(el._focused, true);
  assert.equal(cleared, true);
});

test("focusAtEnd: nested child nodes place caret on the deepest last text node", async () => {
  const caret = await loadCaret();
  const { el, selection, textNode } = fakeContentEditable("nested end", { nested: true });
  caret.focusAtEnd(el);
  assert.equal(el._focused, true);
  assert.equal(selection.ranges.length, 1);
  const r = selection.ranges[0];
  assert.equal(r.startContainer, textNode);
  assert.equal(r.startOffset, "nested end".length);
  assert.equal(r.collapsed, true);
});

test("focusAtEnd: childNodes-only fake without lastChild still finds the text node", async () => {
  const caret = await loadCaret();
  const textNode = { nodeType: 3, textContent: "sparse", get lastChild(){ return null; } };
  const ranges = [];
  const selection = {
    ranges,
    removeAllRanges(){ ranges.length = 0; },
    addRange(r){ ranges.push(r); },
  };
  const el = {
    tagName: "DIV",
    isContentEditable: true,
    textContent: "sparse",
    /* lastChild absent / null — force the childNodes walk fallback */
    lastChild: null,
    childNodes: [textNode],
    _focused: false,
    focus(){ el._focused = true; },
    ownerDocument: {
      createRange(){
        return {
          startContainer: null, startOffset: 0,
          endContainer: null, endOffset: 0,
          collapsed: false,
          setStart(n, o){ this.startContainer = n; this.startOffset = o; },
          setEnd(n, o){ this.endContainer = n; this.endOffset = o; this.collapsed = true; },
        };
      },
      getSelection(){ return selection; },
    },
  };
  textNode.parentNode = el;
  caret.focusAtEnd(el);
  assert.equal(selection.ranges.length, 1);
  assert.equal(selection.ranges[0].startContainer, textNode);
  assert.equal(selection.ranges[0].startOffset, 6);
});
