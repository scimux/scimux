/* Coverage gate for the Claude interaction task.

   Go's cover tool counts statements and basic blocks. It does not measure
   branches. This module never reports a Go block count as branch coverage.
   JavaScript uses V8 function ranges and block ranges. Offsets are UTF-16.

   The evaluator is pure. The file loader only normalizes checkout-root
   prefixes and refuses missing inputs. Test files are outside the Go
   production denominator; this module does not add them.
*/
import assert from "node:assert/strict";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, basename } from "node:path";
import { retainedExecutableCoverage } from "./menu-coverage-ranges.mjs";

export const GO_COVERAGE_KIND = "go-statement-blocks";

export function crossRatioFalls(beforeNum, beforeDen, afterNum, afterDen) {
  if (beforeDen < 0 || afterDen < 0 || beforeNum < 0 || afterNum < 0) return true;
  if (beforeDen === 0 || afterDen === 0) return beforeNum !== 0 || afterNum !== 0 || beforeDen !== afterDen;
  return beforeNum * afterDen > afterNum * beforeDen;
}

export function parseGoCover(text) {
  const blocks = [];
  const lines = String(text || "").split(/\n/);
  if (!lines[0] || !lines[0].startsWith("mode:")) {
    throw new Error("go coverage profile is missing its mode line");
  }
  for (const line of lines.slice(1)) {
    if (!line.trim()) continue;
    const m = /^(.+):(\d+)\.(\d+),(\d+)\.(\d+) (\d+) (\d+)$/.exec(line);
    if (!m) throw new Error(`unreadable go coverage line: ${line}`);
    blocks.push({
      file: m[1],
      startLine: Number(m[2]),
      startCol: Number(m[3]),
      endLine: Number(m[4]),
      endCol: Number(m[5]),
      statements: Number(m[6]),
      count: Number(m[7]),
    });
  }
  return blocks;
}

export function goTotals(blocks) {
  const byFile = new Map();
  const byPkg = new Map();
  let num = 0;
  let den = 0;
  for (const block of blocks) {
    den += block.statements;
    if (block.count > 0) num += block.statements;
    const file = byFile.get(block.file) || { num: 0, den: 0 };
    file.den += block.statements;
    if (block.count > 0) file.num += block.statements;
    byFile.set(block.file, file);
    const pkg = block.file.split("/").slice(0, -1).join("/") || ".";
    const pkgTot = byPkg.get(pkg) || { num: 0, den: 0 };
    pkgTot.den += block.statements;
    if (block.count > 0) pkgTot.num += block.statements;
    byPkg.set(pkg, pkgTot);
  }
  return { num, den, byFile, byPkg, kind: GO_COVERAGE_KIND };
}

function normalizePath(file, roots) {
  let out = file;
  for (const root of roots || []) {
    if (root && out.startsWith(root)) out = out.slice(root.length).replace(/^\/+/, "");
  }
  return out;
}

export function normalizeGoBlocks(blocks, roots) {
  return blocks.map(block => ({ ...block, file: normalizePath(block.file, roots) }));
}

export function loadGoProfile(path, roots) {
  return normalizeGoBlocks(parseGoCover(readFileSync(path, "utf8")), roots);
}

export function blockKey(block) {
  return `${block.file}:${block.startLine}.${block.startCol},${block.endLine}.${block.endCol}`;
}

/* A -coverpkg profile repeats every block once per test package. Sum and max
   are both reported. Last-copy-wins is not a result. Copies whose statement
   counts disagree are rejected rather than added together. */
export function aggregateGoCopies(blocks) {
  if (!Array.isArray(blocks) || blocks.length === 0) {
    throw new Error("aggregateGoCopies requires cover blocks");
  }
  const groups = new Map();
  for (const block of blocks) {
    if (!block || !Number.isInteger(block.statements) || block.statements < 0 || !Number.isInteger(block.count) || block.count < 0) {
      throw new Error("aggregateGoCopies rejected a block");
    }
    const key = blockKey(block);
    const prev = groups.get(key);
    if (!prev) {
      groups.set(key, { ...block, copies: 1, sum: block.count, max: block.count });
      continue;
    }
    if (prev.statements !== block.statements) {
      throw new Error("aggregateGoCopies rejected mismatched statements");
    }
    prev.copies += 1;
    prev.sum += block.count;
    if (block.count > prev.max) prev.max = block.count;
  }
  return [...groups.values()];
}

/* go test -coverpkg writes one full profile per test package, in package
   order, under a single mode line. A repeated block key starts the next copy.
   The owning package's copy is one of those stanzas, not the max across them. */
export function splitCoverpkgStanzas(blocks) {
  if (!Array.isArray(blocks) || blocks.length === 0) {
    throw new Error("splitCoverpkgStanzas requires cover blocks");
  }
  const stanzas = [];
  let current = [];
  const seen = new Set();
  for (const block of blocks) {
    if (!block || block.file == null) throw new Error("splitCoverpkgStanzas rejected a block");
    const key = blockKey(block);
    if (seen.has(key)) {
      stanzas.push(current);
      current = [];
      seen.clear();
    }
    seen.add(key);
    current.push(block);
  }
  stanzas.push(current);
  return stanzas;
}

/* A changed instrumentation boundary is reported, not dropped. Callers pass
   an explicit note when a block split or merged and the mapping is known. */
export function evaluateCoverage(model) {
  const failures = [];
  const notes = [];
  if (!model || typeof model !== "object") {
    return { ok: false, failures: ["missing coverage model"], notes, kind: GO_COVERAGE_KIND };
  }
  if (model.goKind && model.goKind !== GO_COVERAGE_KIND) {
    failures.push("go coverage was labeled as branch coverage");
  }
  if (!model.baseline || !model.after) failures.push("missing baseline or after evidence");
  if (model.discoveredCases === 0) failures.push("zero discovered test cases");
  if (model.missingPackages) {
    for (const name of model.missingPackages) failures.push(`absent package ${name}`);
  }
  if (model.missingScripts) {
    for (const name of model.missingScripts) failures.push(`absent script ${name}`);
  }
  const baseGo = model.baseline && model.baseline.go;
  const afterGo = model.after && model.after.go;
  if (model.baseline && model.after && (!baseGo || !afterGo)) {
    failures.push("missing baseline or after go profile");
  }
  if (baseGo && afterGo) {
    const before = goTotals(baseGo);
    const after = goTotals(afterGo);
    if (crossRatioFalls(before.num, before.den, after.num, after.den)) {
      failures.push(`go aggregate ratio fell ${before.num}/${before.den} -> ${after.num}/${after.den}`);
    }
    for (const [file, prev] of before.byFile) {
      const next = after.byFile.get(file);
      if (!next) {
        failures.push(`missing go source graph ${file}`);
        continue;
      }
      if (crossRatioFalls(prev.num, prev.den, next.num, next.den)) {
        failures.push(`go file ratio fell ${file} ${prev.num}/${prev.den} -> ${next.num}/${next.den}`);
      }
    }
    for (const [pkg, prev] of before.byPkg) {
      const next = after.byPkg.get(pkg);
      if (!next) {
        failures.push(`missing go package ${pkg}`);
        continue;
      }
      if (crossRatioFalls(prev.num, prev.den, next.num, next.den)) {
        failures.push(`go package ratio fell ${pkg} ${prev.num}/${prev.den} -> ${next.num}/${next.den}`);
      }
    }
    const afterByKey = new Map(afterGo.map(block => [blockKey(block), block]));
    for (const block of baseGo) {
      if (block.count <= 0) continue;
      if ((model.changedGo || new Set()).has(blockKey(block))) continue;
      const next = afterByKey.get(blockKey(block));
      if (!next || next.count <= 0) {
        failures.push(`lost unchanged covered go block ${blockKey(block)}`);
      }
    }
    for (const block of afterGo) {
      const required = (model.newOrChangedGo || new Set()).has(blockKey(block));
      if (required && block.count <= 0) {
        failures.push(`uncovered new or changed go block ${blockKey(block)}`);
      }
    }
    const oldNum = [...before.byFile.entries()].reduce((sum, [file, tot]) => {
      if ((model.newGoFiles || new Set()).has(file)) return sum;
      return sum + tot.num;
    }, 0);
    const newFiles = model.newGoFiles || new Set();
    let newNum = 0;
    let oldAfter = 0;
    for (const [file, tot] of after.byFile) {
      if (newFiles.has(file)) newNum += tot.num;
      else oldAfter += tot.num;
    }
    if (newNum > 0 && oldAfter < oldNum) {
      failures.push("new covered go code masks a loss on old code");
    }
  }
  const baseJS = model.baseline && model.baseline.js;
  const afterJS = model.after && model.after.js;
  if (model.requireJS && (!baseJS || !afterJS)) failures.push("missing baseline or after javascript coverage");
  if (baseJS && afterJS) {
    const graphs = new Map();
    for (const graph of afterJS) graphs.set(graph.id, graph);
    for (const graph of baseJS) {
      const next = graphs.get(graph.id);
      if (!next) {
        const renamed = afterJS.find(item => basename(item.id) === basename(graph.id) && item.id !== graph.id);
        if ((model.renamedGraphs || {})[graph.id]) {
          continue;
        }
        if (renamed) failures.push(`renamed javascript graph ${graph.id} -> ${renamed.id}`);
        else failures.push(`missing javascript source graph ${graph.id}`);
        continue;
      }
      if (crossRatioFalls(graph.num, graph.den, next.num, next.den)) {
        failures.push(`javascript graph ratio fell ${graph.id} ${graph.num}/${graph.den} -> ${next.num}/${next.den}`);
      }
      if (graph.text != null && next.text != null && graph.mapping && graph.functions && next.functions) {
        const retained = retainedExecutableCoverage(graph.functions, next.functions, graph.text, next.text, graph.mapping);
        for (const failure of retained.failures) failures.push(`${graph.id}: ${failure}`);
      }
    }
    for (const graph of afterJS) {
      if (!(model.newOrChangedJS || new Set()).has(graph.id)) continue;
      for (const range of graph.uncovered || []) {
        failures.push(`uncovered new or changed javascript ${range.kind || "range"} ${graph.id} ${range.start}-${range.end}`);
      }
      if (graph.uncoveredBranchOnCoveredLine) {
        failures.push(`uncovered javascript branch on a covered line ${graph.id}`);
      }
    }
    const oldBefore = baseJS.reduce((sum, graph) => sum + (model.newOrChangedJS && model.newOrChangedJS.has(graph.id) ? 0 : graph.num), 0);
    const oldAfter = afterJS.reduce((sum, graph) => sum + (model.newOrChangedJS && model.newOrChangedJS.has(graph.id) ? 0 : graph.num), 0);
    const added = afterJS.reduce((sum, graph) => sum + (model.newOrChangedJS && model.newOrChangedJS.has(graph.id) ? graph.num : 0), 0);
    if (added > 0 && oldAfter < oldBefore) failures.push("new covered javascript masks a loss on old code");
  }
  for (const note of model.boundaryNotes || []) notes.push(note);
  if ((model.boundaryNotes || []).length === 0 && (model.unexplainedBoundaries || 0) > 0) {
    failures.push("unexplained instrumentation-block boundary change");
  }
  return { ok: failures.length === 0, failures, notes, kind: GO_COVERAGE_KIND };
}

function rawRanges(functions) {
  const raw = [];
  for (const fn of functions || []) {
    const ranges = Array.isArray(fn && fn.ranges) ? fn.ranges : [];
    ranges.forEach((range, index) => {
      const item = range || {};
      raw.push({
        start: Number(item.startOffset) || 0,
        end: Number(item.endOffset) || 0,
        count: Number(item.count) || 0,
        root: index === 0,
      });
    });
  }
  return raw;
}

function strictlyInside(inner, outer) {
  return inner.start >= outer.start && inner.end <= outer.end &&
    (inner.start !== outer.start || inner.end !== outer.end);
}

function hasPositiveTwin(raw, range, index) {
  return raw.some((other, otherIndex) =>
    otherIndex !== index &&
    other.start === range.start &&
    other.end === range.end &&
    other.count > 0
  );
}

/* A coarse zero is an alternate instrumentation boundary only when positive
   children tile it exactly. A cold child, a gap, or a mere enclosing count
   leaves the zero uncovered. */
function partitionedByChildren(raw, range) {
  const inside = raw.filter(other => other !== range && strictlyInside(other, range));
  if (inside.some(other => other.count <= 0)) return false;
  const positive = inside.filter(other => other.count > 0);
  const top = positive.filter(part => !positive.some(other => other !== part && strictlyInside(part, other)));
  if (top.length < 2) return false;
  const sorted = [...top].sort((a, b) => a.start - b.start || a.end - b.end);
  if (sorted[0].start !== range.start || sorted[sorted.length - 1].end !== range.end) return false;
  for (let i = 1; i < sorted.length; i++) {
    if (sorted[i].start !== sorted[i - 1].end) return false;
  }
  return true;
}

export function analyzeRawV8(functions) {
  const raw = rawRanges(functions);
  const positive = raw.filter(range => range.count > 0);
  const uncovered = [];
  let alternateBoundaries = 0;
  let num = 0;
  let uncoveredBranchOnCoveredLine = false;
  raw.forEach((range, index) => {
    if (range.count > 0) {
      num++;
      return;
    }
    if (hasPositiveTwin(raw, range, index) || partitionedByChildren(raw, range)) {
      num++;
      alternateBoundaries++;
      return;
    }
    const kind = range.root ? "function" : "branch";
    uncovered.push({ kind, start: range.start, end: range.end });
    if (kind === "branch" && positive.some(other => strictlyInside(range, other))) {
      uncoveredBranchOnCoveredLine = true;
    }
  });
  return {
    num,
    den: raw.length,
    uncovered,
    uncoveredBranchOnCoveredLine,
    alternateBoundaries,
  };
}

/* Several Node test processes observe the same file. A range is covered when
   any observation counted it. Identical function shapes keep the maximum
   count. A cold child stays cold: a positive parent does not raise it. */
export function mergeV8Observations(entries) {
  if (!Array.isArray(entries)) return [];
  const groups = new Map();
  for (const entry of entries) {
    if (!entry) continue;
    const url = entry.url || "";
    const functions = Array.isArray(entry.functions) ? entry.functions : [];
    let bucket = groups.get(url);
    if (!bucket) {
      bucket = new Map();
      groups.set(url, bucket);
    }
    for (const fn of functions) {
      const body = fn || {};
      const ranges = Array.isArray(body.ranges) ? body.ranges : [];
      const normalized = ranges.map(range => {
        const item = range || {};
        return {
          startOffset: Number(item.startOffset) || 0,
          endOffset: Number(item.endOffset) || 0,
          count: Number(item.count) || 0,
        };
      });
      const signature = normalized.map(range => `${range.startOffset}:${range.endOffset}`).join(",");
      const name = body.functionName || "";
      const key = `${name}\n${signature}`;
      const prev = bucket.get(key);
      if (!prev) {
        bucket.set(key, {
          functionName: name,
          isBlockCoverage: !!body.isBlockCoverage,
          ranges: normalized,
        });
        continue;
      }
      if (body.isBlockCoverage) prev.isBlockCoverage = true;
      normalized.forEach((range, index) => {
        if (range.count > prev.ranges[index].count) prev.ranges[index].count = range.count;
      });
    }
  }
  return [...groups.entries()].map(([url, bucket]) => ({
    url,
    functions: [...bucket.values()],
  }));
}

/* Half-open UTF-16 ranges, the same offsets V8 reports. A range is on a
   1-based line only when the two intervals intersect. */
export function rangesOnLines(text, ranges, lineNumbers) {
  if (!Array.isArray(ranges) || !Array.isArray(lineNumbers)) return [];
  const lines = String(text || "").split(/(?<=\n)/);
  const spans = [];
  let offset = 0;
  for (const line of lines) {
    spans.push({ start: offset, end: offset + line.length });
    offset += line.length;
  }
  const wanted = new Set(lineNumbers);
  return ranges.filter(range => {
    const start = Number(range && range.start) || 0;
    const end = Number(range && range.end) || 0;
    for (let i = 0; i < spans.length; i++) {
      if (!wanted.has(i + 1)) continue;
      const span = spans[i];
      if (start < span.end && end > span.start) return true;
    }
    return false;
  });
}

export function lineSpans(text) {
  const lines = String(text ?? "").split(/(?<=\n)/);
  const spans = [];
  let offset = 0;
  for (const line of lines) {
    spans.push({ line: spans.length + 1, start: offset, end: offset + line.length });
    offset += line.length;
  }
  return spans;
}

function overlappingLines(spans, range) {
  const start = Number(range && range.start) || 0;
  const end = Number(range && range.end) || 0;
  const lines = [];
  for (const span of spans) {
    if (start < span.end && end > span.start) lines.push(span.line);
  }
  return lines;
}

function rangeCounts(range) {
  return Number(range && range.count) || 0;
}

function rangeBounds(range) {
  return { start: Number(range && range.start) || 0, end: Number(range && range.end) || 0 };
}

function boundsStrictlyInside(inner, outer) {
  return inner.start >= outer.start && inner.end <= outer.end &&
    (inner.start !== outer.start || inner.end !== outer.end);
}

/* executableSpans ignores only source it can classify safely as whitespace
   or comments. Strings/templates retain their executable contents. A slash
   that is not already a comment or inside a string could start a regex or
   division expression. Keep that entire suffix executable instead of guessing
   its grammar and accidentally treating regex contents as comments. This is
   deliberately conservative, including across multiline templates. */
export function executableSpans(text) {
  const src = String(text ?? "");
  const spans = [];
  let run = -1;
  const mark = (index, executable) => {
    if (executable) {
      if (run < 0) run = index;
      return;
    }
    if (run >= 0) {
      spans.push({ start: run, end: index });
      run = -1;
    }
  };
  const stack = [{ mode: "code", braces: 0, interp: false }];
  const top = () => stack[stack.length - 1];
  const push = (frame) => stack.push(frame);
  const pop = () => {
    if (stack.length > 1) stack.pop();
  };
  let i = 0;
  while (i < src.length) {
    const c = src[i];
    const n = src[i + 1];
    const mode = top().mode;
    if (mode === "line") {
      mark(i, false);
      if (c === "\n") pop();
      i++;
      continue;
    }
    if (mode === "block") {
      mark(i, false);
      if (c === "*" && n === "/") {
        mark(i + 1, false);
        i += 2;
        pop();
        continue;
      }
      i++;
      continue;
    }
    if (mode === "squote" || mode === "dquote" || mode === "template") {
      mark(i, true);
      if (c === "\\") {
        if (i + 1 < src.length) {
          mark(i + 1, true);
          i += 2;
        } else {
          i++;
        }
        continue;
      }
      if ((mode === "squote" && c === "'") || (mode === "dquote" && c === '"') || (mode === "template" && c === "`")) {
        pop();
        i++;
        continue;
      }
      if (mode === "template" && c === "$" && n === "{") {
        mark(i + 1, true);
        i += 2;
        push({ mode: "code", braces: 0, interp: true });
        continue;
      }
      i++;
      continue;
    }
    if (c === "/" && n === "/") {
      mark(i, false);
      mark(i + 1, false);
      i += 2;
      push({ mode: "line", braces: 0, interp: false });
      continue;
    }
    if (c === "/" && n === "*") {
      mark(i, false);
      mark(i + 1, false);
      i += 2;
      push({ mode: "block", braces: 0, interp: false });
      continue;
    }
    if (c === "/") {
      while (i < src.length) {
        mark(i, true);
        i++;
      }
      continue;
    }
    if (c === "'") {
      mark(i, true);
      i++;
      push({ mode: "squote", braces: 0, interp: false });
      continue;
    }
    if (c === '"') {
      mark(i, true);
      i++;
      push({ mode: "dquote", braces: 0, interp: false });
      continue;
    }
    if (c === "`") {
      mark(i, true);
      i++;
      push({ mode: "template", braces: 0, interp: false });
      continue;
    }
    if (c === "{") {
      mark(i, true);
      if (top().interp) top().braces++;
      i++;
      continue;
    }
    if (c === "}") {
      mark(i, true);
      if (top().interp && top().braces === 0) {
        pop();
      } else {
        if (top().interp) top().braces--;
      }
      i++;
      continue;
    }
    if (c === " " || c === "\t" || c === "\n" || c === "\r" || c === "\f" || c === "\v") {
      mark(i, false);
      i++;
      continue;
    }
    mark(i, true);
    i++;
  }
  if (run >= 0) spans.push({ start: run, end: src.length });
  return spans;
}

function executableMask(text) {
  const mask = new Array(text.length).fill(false);
  for (const span of executableSpans(text)) {
    for (let i = span.start; i < span.end && i < text.length; i++) mask[i] = true;
  }
  return mask;
}

function offsetCovered(offset, outer, observed) {
  return observed.some(positive => {
    if (positive.count <= 0 || offset < positive.start || offset >= positive.end) return false;
    if (!boundsStrictlyInside(positive, outer)) return false;
    return !observed.some(cold =>
      cold.count <= 0 &&
      offset >= cold.start && offset < cold.end &&
      boundsStrictlyInside(cold, positive)
    );
  });
}

/* A coarse uncovered range is explained only when it also overlaps unchanged
   source and every executable offset on each overlapping changed line has a
   positive range strictly inside the coarse range. A positive intersection
   with part of a line does not cover the rest of that line. A zero range
   strictly inside that positive range still leaves its offsets uncovered.
   Same-bounds observations are not inner coverage. Whitespace and comments
   are not executable, including a comment-only changed line. A range that
   sits only on changed lines is a hole. */
export function auditChangedRanges(text, changedLines, uncovered, ranges) {
  if (typeof text !== "string" || !Array.isArray(changedLines) || !Array.isArray(uncovered) || !Array.isArray(ranges)) {
    throw new Error("auditChangedRanges requires source text, changed lines, uncovered ranges, and observed ranges");
  }
  const changed = new Set(changedLines.map(Number).filter(line => Number.isInteger(line) && line > 0));
  const spans = lineSpans(text);
  const mask = executableMask(text);
  const observed = ranges.filter(Boolean).map(range => ({ ...rangeBounds(range), count: rangeCounts(range) }));
  const explained = [];
  const holes = [];
  for (const range of uncovered) {
    if (!range) continue;
    const outer = rangeBounds(range);
    const lines = overlappingLines(spans, outer);
    const changedHit = lines.filter(line => changed.has(line));
    if (changedHit.length === 0) continue;
    const unchangedHit = lines.some(line => !changed.has(line));
    const coveredChanged = changedHit.filter(line => {
      const span = spans[line - 1];
      for (let offset = span.start; offset < span.end; offset++) {
        if (!mask[offset]) continue;
        if (!offsetCovered(offset, outer, observed)) return false;
      }
      return true;
    });
    if (unchangedHit && coveredChanged.length === changedHit.length) {
      explained.push({ start: outer.start, end: outer.end, changedLines: changedHit });
      continue;
    }
    holes.push({
      start: outer.start,
      end: outer.end,
      changedLines: changedHit,
      reason: unchangedHit ? "changed line lacks its own positive range" : "uncovered range is only changed lines",
    });
  }
  return { explained, holes };
}

export function graphsFromRawV8(entries) {
  return (entries || []).map(entry => {
    const analysis = analyzeRawV8(entry.functions || []);
    return {
      id: entry.url || "",
      num: analysis.num,
      den: analysis.den || 1,
      functions: entry.functions || [],
      uncovered: analysis.uncovered,
      uncoveredBranchOnCoveredLine: analysis.uncoveredBranchOnCoveredLine,
      alternateBoundaries: analysis.alternateBoundaries,
    };
  });
}

export function v8FunctionRanges(coverage) {
  const out = [];
  for (const entry of coverage || []) {
    const functions = entry.functions || [];
    out.push({
      id: entry.url || "",
      functions: functions.map(fn => ({
        ranges: fn.ranges || [],
        isBlockCoverage: !!fn.isBlockCoverage,
      })),
    });
  }
  return out;
}

export function readV8Directory(dir) {
  const files = readdirSync(dir).filter(name => name.endsWith(".json"));
  if (files.length === 0) throw new Error(`no v8 coverage in ${dir}`);
  const merged = [];
  for (const name of files) {
    const full = join(dir, name);
    if (!statSync(full).isFile()) continue;
    const parsed = JSON.parse(readFileSync(full, "utf8"));
    const list = Array.isArray(parsed) ? parsed : parsed.result || [];
    merged.push(...list);
  }
  if (merged.length === 0) throw new Error(`empty v8 coverage in ${dir}`);
  return merged;
}

export function compareEvidence(input) {
  assert.ok(input && input.baselineGo && input.afterGo, "missing baseline or after go profile path");
  const baselineGo = loadGoProfile(input.baselineGo, input.roots || []);
  const afterGo = loadGoProfile(input.afterGo, input.roots || []);
  if (baselineGo.length === 0 || afterGo.length === 0) {
    return evaluateCoverage({ discoveredCases: 0, baseline: { go: baselineGo }, after: { go: afterGo } });
  }
  return evaluateCoverage({
    goKind: GO_COVERAGE_KIND,
    discoveredCases: input.discoveredCases,
    missingPackages: input.missingPackages || [],
    missingScripts: input.missingScripts || [],
    baseline: { go: baselineGo, js: input.baselineJS || null },
    after: { go: afterGo, js: input.afterJS || null },
    requireJS: !!input.requireJS,
    newOrChangedGo: new Set(input.newOrChangedGo || []),
    newGoFiles: new Set(input.newGoFiles || []),
    changedGo: new Set(input.changedGo || []),
    newOrChangedJS: new Set(input.newOrChangedJS || []),
    renamedGraphs: input.renamedGraphs || {},
    boundaryNotes: input.boundaryNotes || [],
    unexplainedBoundaries: input.unexplainedBoundaries || 0,
  });
}
