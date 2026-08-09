/* Repo-wide smoke checks. Keep these cheap — they run on every suite and
 * catch structural failure classes that per-feature regex CSS tests cannot see. */
import test from "node:test";
import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = dirname(fileURLToPath(import.meta.url));
const cssDir = join(__dirname, "../css");

// Strip CSS block comments so braces inside them do not affect balance.
function stripCssComments(src){
  return src.replace(/\/\*[\s\S]*?\*\//g, "");
}

/** Walk { / } after comment strip. depth ends at 0 and never goes negative. */
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

test("every web/css file has balanced braces (ends at 0, never negative)", () => {
  /* Structural guard: a stray } truncates an @media block and orphans later
     rules at global scope. Rule-body regexes (css.match(/\.foo\s*\{([^}]+)\}/))
     match happily across a truncated block, so this failure class is invisible
     to every other CSS test in the suite. P1a. */
  const files = readdirSync(cssDir).filter(f => f.endsWith(".css")).sort();
  assert.ok(files.length > 0, "web/css has .css files");
  for (const name of files){
    const raw = readFileSync(join(cssDir, name), "utf8");
    const { depth, minDepth } = braceWalk(stripCssComments(raw));
    assert.equal(depth, 0,
      `${name}: brace depth ends at ${depth}, expected 0`);
    assert.ok(minDepth >= 0,
      `${name}: brace depth went negative (min ${minDepth})`);
  }
});

/** Every @keyframes body in a file, brace-matched (stop blocks nest). */
function keyframeBlocks(src){
  const out = [];
  const re = /@(?:-\w+-)?keyframes\s+([\w-]+)\s*\{/g;
  let m;
  while ((m = re.exec(src)) !== null){
    let depth = 1, i = re.lastIndex;
    for (; i < src.length && depth > 0; i++){
      if (src[i] === "{") depth++;
      else if (src[i] === "}") depth--;
    }
    out.push({ name: m[1], body: src.slice(re.lastIndex, i - 1) });
  }
  return out;
}

test("no keyframe animates a paint-only property", () => {
  /* The attention pulses run infinite, at 60fps, for as long as any node is
     asking — on a phone that is the app's whole idle cost. opacity and
     transform hand the animation to the compositor and cost nothing per frame;
     box-shadow, filter and backdrop-filter cannot be composited, so each frame
     repaints a blurred shadow. will-change does not change that. Pulse by
     animating the opacity of a layer that carries a *static* shadow. */
  const banned = /(^|[;{\s])(box-shadow|filter|backdrop-filter|-webkit-backdrop-filter)\s*:/;
  const files = readdirSync(cssDir).filter(f => f.endsWith(".css")).sort();
  for (const name of files){
    for (const { name: kf, body } of keyframeBlocks(stripCssComments(readFileSync(join(cssDir, name), "utf8")))){
      const hit = body.match(banned);
      assert.ok(!hit, `${name}: @keyframes ${kf} animates ${hit && hit[2]}`);
    }
  }
});

test("keyframeBlocks brace-matches nested stop blocks", () => {
  const blocks = keyframeBlocks("@keyframes a { 0% { opacity: 0 } 50%,100% { opacity: 1 } } .x { box-shadow: 0 0 1px red; }");
  assert.equal(blocks.length, 1);
  assert.equal(blocks[0].name, "a");
  assert.match(blocks[0].body, /50%,100%/);
  assert.doesNotMatch(blocks[0].body, /\.x/, "the block must end at its own closing brace");
});

test("test runner is active", () => assert.equal(1 + 1, 2));
