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

test("test runner is active", () => assert.equal(1 + 1, 2));
