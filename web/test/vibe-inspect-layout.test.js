/* The Vibe inspection choice sits outside #m_harnesses. This checks the
   production cascade against that label, the attachment switch, and a
   harness-row switch. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const dir = dirname(fileURLToPath(import.meta.url));
const indexSrc = readFileSync(join(dir, "../index.html"), "utf8");

test("the Vibe inspection label has its own 44px wrapping rule", () => {
  const root = parseHTML(indexSrc);
  const input = findById(root, "m_vibe_inspect");
  assert.ok(input, "index.html has no #m_vibe_inspect");
  const label = input.parent;
  assert.equal(label && label.tag, "label");
  assert.ok(label.classes.includes("hswitch"));
  assert.equal(ancestor(label, "m_harnesses"), null);
  assert.ok(ancestorClass(label, "sheet"), "the inspection label is in the menu sheet");

  const attachment = findById(root, "m_external_attachments");
  assert.ok(attachment && attachment.parent && attachment.parent.tag === "label");
  const harnesses = findById(root, "m_harnesses");
  assert.ok(harnesses);
  const row = element("label", "", ["hswitch"], harnesses);
  const rowInput = element("input", "", [], row);

  const rules = parseCSS(cascade(indexSrc));
  assert.equal(matches(label, "#m_harnesses .hswitch"), false);
  assert.equal(matches(label, "label:has(> #m_vibe_inspect)"), true);
  assert.equal(matches(input, "label:has(> #m_vibe_inspect) > input"), true);
  assert.equal(matches(row, "#m_harnesses .hswitch"), true);
  assert.equal(matches(attachment.parent, "label:has(> #m_vibe_inspect)"), false);
  assert.equal(matches(row, "label:has(> #m_vibe_inspect)"), false);

  const rowStyle = apply(row, rules);
  assert.equal(rowStyle["min-height"] && rowStyle["min-height"].value, "44px");
  assert.equal(rowStyle["min-height"].selector, "#m_harnesses .hswitch");
  const rowBox = apply(rowInput, rules);
  assert.equal(rowBox.width && rowBox.width.value, "18px");
  assert.match(rowBox.width.selector, /#m_harnesses \.hswitch input/);

  const style = apply(label, rules);
  assert.ok(style["min-height"], "the inspection label has no min-height");
  assert.equal(style["min-height"].value, "44px");
  assert.match(style["min-height"].selector, /#m_vibe_inspect/);
  assert.equal(style.display && style.display.value, "flex");
  assert.match(style.display.selector, /#m_vibe_inspect/);
  assert.equal(style.gap && style.gap.value, "8px");
  assert.equal(style["flex-wrap"] && style["flex-wrap"].value, "wrap");
  assert.match(style["flex-wrap"].selector, /#m_vibe_inspect/);
  assert.equal(style["overflow-wrap"] && style["overflow-wrap"].value, "break-word");

  const box = apply(input, rules);
  assert.equal(box.width && box.width.value, "18px");
  assert.equal(box.height && box.height.value, "18px");
  assert.match(box.width.selector, /#m_vibe_inspect/);
  assert.doesNotMatch(box.width.selector, /#m_harnesses \.hswitch/);

  const attachmentStyle = apply(attachment.parent, rules);
  if (attachmentStyle["min-height"]) {
    assert.doesNotMatch(attachmentStyle["min-height"].selector, /m_vibe_inspect/);
  }
  for (const decl of Object.values(attachmentStyle)) {
    assert.doesNotMatch(decl.selector, /m_vibe_inspect/);
  }
});

function cascade(html) {
  const hrefs = [...html.matchAll(/<link\b[^>]*rel="stylesheet"[^>]*href="([^"]+)"/g)].map(m => m[1]);
  return hrefs.map(href => readFileSync(join(dir, "..", href), "utf8")).join("\n");
}

function parseHTML(html) {
  const root = element("#document", "", [], null);
  const stack = [root];
  const voids = new Set(["area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "source", "track", "wbr"]);
  const src = html.replace(/<!--[\s\S]*?-->/g, "");
  const re = /<\/?([A-Za-z][\w:-]*)([^>]*)>/g;
  let match;
  while ((match = re.exec(src))) {
    const tag = match[1].toLowerCase();
    if (match[0].startsWith("</")) {
      for (let i = stack.length - 1; i > 0; i--) {
        if (stack[i].tag === tag) {
          stack.length = i;
          break;
        }
      }
      continue;
    }
    const el = element(tag, attr(match[2], "id"), classes(match[2]), stack[stack.length - 1]);
    if (!voids.has(tag) && !/\/\s*$/.test(match[2])) stack.push(el);
  }
  return root;
}

function element(tag, id, classList, parent) {
  const el = { tag, id: id || "", classes: classList || [], parent, children: [] };
  if (parent) parent.children.push(el);
  return el;
}

function attr(raw, name) {
  const found = new RegExp(`(?:^|\\s)${name}="([^"]*)"`).exec(raw);
  return found ? found[1] : "";
}

function classes(raw) {
  return attr(raw, "class").split(/\s+/).filter(Boolean);
}

function findById(el, id) {
  if (el.id === id) return el;
  for (const child of el.children) {
    const found = findById(child, id);
    if (found) return found;
  }
  return null;
}

function ancestor(el, id) {
  for (let cur = el.parent; cur; cur = cur.parent) if (cur.id === id) return cur;
  return null;
}

function ancestorClass(el, name) {
  for (let cur = el; cur; cur = cur.parent) if (cur.classes.includes(name)) return cur;
  return null;
}

function parseCSS(css) {
  const src = css.replace(/\/\*[\s\S]*?\*\//g, "");
  const rules = [];
  consume(src, 0, rules);
  return rules;
}

function consume(src, start, rules) {
  let i = start;
  while (i < src.length) {
    while (i < src.length && /\s/.test(src[i])) i++;
    if (i >= src.length) return i;
    if (src[i] === "}") return i + 1;
    if (src.startsWith("@keyframes", i) || src.startsWith("@-webkit-keyframes", i)) {
      i = skipBlock(src, src.indexOf("{", i) + 1);
      continue;
    }
    if (src[i] === "@") {
      const brace = src.indexOf("{", i);
      i = consume(src, brace + 1, rules);
      continue;
    }
    const brace = src.indexOf("{", i);
    if (brace < 0) return src.length;
    const selector = src.slice(i, brace).trim();
    const end = skipBlock(src, brace + 1);
    const body = src.slice(brace + 1, end - 1);
    if (selector) rules.push({ selector, body, order: rules.length });
    i = end;
  }
  return i;
}

function skipBlock(src, i) {
  let depth = 1;
  while (i < src.length && depth) {
    if (src[i] === "{") depth++;
    else if (src[i] === "}") depth--;
    i++;
  }
  return i;
}

function apply(el, rules) {
  const out = {};
  for (const rule of rules) {
    for (const alt of splitCommas(rule.selector)) {
      if (!matches(el, alt)) continue;
      const spec = specificity(alt);
      for (const [prop, value] of declarations(rule.body)) {
        const prev = out[prop];
        if (!prev || specCompare(spec, prev.spec) > 0 || (specCompare(spec, prev.spec) === 0 && rule.order >= prev.order)) {
          out[prop] = { value, spec, selector: alt.trim(), order: rule.order };
        }
      }
    }
  }
  return out;
}

function declarations(body) {
  const out = [];
  for (const part of body.split(";")) {
    const cut = part.indexOf(":");
    if (cut < 0) continue;
    const prop = part.slice(0, cut).trim().toLowerCase();
    const value = part.slice(cut + 1).trim();
    if (prop && value) out.push([prop, value]);
  }
  return out;
}

function matches(el, selector) {
  const parts = tokenize(selector);
  if (!parts.length) return false;
  return matchFrom(el, parts, parts.length - 1);
}

function matchFrom(el, parts, index) {
  if (index < 0) return true;
  const part = parts[index];
  if (part.kind === "compound") {
    if (!el || !matchCompound(el, part.text)) return false;
    if (index === 0) return true;
    const combinator = parts[index - 1];
    if (combinator.kind === "child") {
      return matchFrom(el.parent, parts, index - 2);
    }
    if (combinator.kind === "descendant") {
      for (let cur = el.parent; cur; cur = cur.parent) {
        if (matchFrom(cur, parts, index - 2)) return true;
      }
      return false;
    }
    return false;
  }
  return false;
}

function matchCompound(el, text) {
  let i = 0;
  let saw = false;
  if (/^[A-Za-z]/.test(text)) {
    const tag = /^[A-Za-z][\w-]*/.exec(text)[0].toLowerCase();
    if (el.tag !== tag) return false;
    i += tag.length;
    saw = true;
  }
  while (i < text.length) {
    saw = true;
    if (text[i] === ".") {
      const name = /^[\w-]+/.exec(text.slice(i + 1));
      if (!name || !el.classes.includes(name[0])) return false;
      i += 1 + name[0].length;
    } else if (text[i] === "#") {
      const name = /^[\w-]+/.exec(text.slice(i + 1));
      if (!name || el.id !== name[0]) return false;
      i += 1 + name[0].length;
    } else if (text.startsWith(":has(", i)) {
      const inner = group(text, i + 4);
      if (inner == null || !hasMatch(el, inner)) return false;
      i += 5 + inner.length + 1;
    } else if (text[i] === ":" || text[i] === "[") {
      return false;
    } else {
      return false;
    }
  }
  return saw;
}

function hasMatch(el, inner) {
  const rel = inner.trim();
  if (rel.startsWith(">")) {
    const sel = rel.slice(1).trim();
    return el.children.some(child => matches(child, sel));
  }
  return walk(el, node => node !== el && matches(node, rel));
}

function walk(el, pred) {
  for (const child of el.children) {
    if (pred(child) || walk(child, pred)) return true;
  }
  return false;
}

function tokenize(selector) {
  const parts = [];
  let i = 0;
  const src = selector.trim();
  while (i < src.length) {
    if (/\s/.test(src[i])) {
      while (i < src.length && /\s/.test(src[i])) i++;
      if (i < src.length && !/[>+~]/.test(src[i]) && parts.length && parts[parts.length - 1].kind === "compound") {
        parts.push({ kind: "descendant" });
      }
      continue;
    }
    if (src[i] === ">") {
      parts.push({ kind: "child" });
      i++;
      continue;
    }
    if (/[+~]/.test(src[i])) return [];
    const start = i;
    if (/^[A-Za-z]/.test(src.slice(i))) i += /^[A-Za-z][\w-]*/.exec(src.slice(i))[0].length;
    while (i < src.length) {
      if (src[i] === "." || src[i] === "#") {
        i += 1 + (/^[\w-]+/.exec(src.slice(i + 1)) || [""])[0].length;
      } else if (src.startsWith(":has(", i)) {
        const inner = group(src, i + 4);
        if (inner == null) return [];
        i += 5 + inner.length + 1;
      } else break;
    }
    if (i === start) return [];
    parts.push({ kind: "compound", text: src.slice(start, i) });
  }
  return parts;
}

function group(src, openParen) {
  let depth = 1;
  let i = openParen + 1;
  const start = i;
  while (i < src.length && depth) {
    if (src[i] === "(") depth++;
    else if (src[i] === ")") depth--;
    if (depth) i++;
  }
  if (depth) return null;
  return src.slice(start, i);
}

function splitCommas(selector) {
  const out = [];
  let depth = 0;
  let start = 0;
  for (let i = 0; i < selector.length; i++) {
    if (selector[i] === "(") depth++;
    else if (selector[i] === ")") depth--;
    else if (selector[i] === "," && depth === 0) {
      out.push(selector.slice(start, i));
      start = i + 1;
    }
  }
  out.push(selector.slice(start));
  return out;
}

function specificity(selector) {
  const ids = (selector.match(/#[\w-]+/g) || []).length;
  const classes = (selector.match(/\.[\w-]+|:has\(/g) || []).length;
  const stripped = selector.replace(/#[\w-]+|\.[\w-]+|:has\([^)]*\)/g, " ").replace(/[>+~]/g, " ");
  const elements = (stripped.match(/[A-Za-z][\w-]*/g) || []).length;
  return [ids, classes, elements];
}

function specCompare(a, b) {
  for (let i = 0; i < 3; i++) if (a[i] !== b[i]) return a[i] - b[i];
  return 0;
}
