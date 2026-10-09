// Run with Node 22's --expose-internals: use the same merger and counters as
// --experimental-test-coverage, retaining integer numerators/denominators.
// No exclusions or ignore directives are added. Query URL graphs remain in
// Node's aggregate; physical-file unions are used only for retained-line and
// changed-range checks (a function need not run in every imported graph).
import assert from 'node:assert/strict';
import { readFileSync, writeFileSync } from 'node:fs';
import { basename, join } from 'node:path';
import { createRequire } from 'node:module';
import { retainedExecutableCoverage } from './menu-coverage-ranges.mjs';
const { TestCoverage } = createRequire(import.meta.url)('internal/test_runner/coverage');
const [evidence, base] = process.argv.slice(2);
const diffText = readFileSync(join(evidence, 'js.diff'), 'utf8');
assert.ok(evidence && base, 'usage: node --expose-internals web/test/menu-coverage.mjs EVIDENCE BASE');
function load(phase) {
  const coverage = new TestCoverage(join(evidence, `${phase}-v8`), undefined, process.cwd(), [], ['web/js/*.js'], false, {});
  const original = coverage.getLines.bind(coverage);
  if (phase === 'before') coverage.getLines = url => original(url, readFileSync(join(evidence, 'before-js', basename(new URL(url).pathname)), 'utf8'));
  const summary = coverage.summary();
  const raw = coverage.getCoverageFromDirectory();
  const graphs = new Map();
  for (const script of raw) {
    const path = new URL(script.url).pathname;
    if (!graphs.has(path)) graphs.set(path, []);
    graphs.get(path).push(script.url);
  }
  for (const file of summary.files) file.url = graphs.get(file.path).shift();
  writeFileSync(join(evidence, `${phase}-browser.json`), JSON.stringify(summary, null, 2));
  return { summary, raw };
}
const before = load('before'), after = load('after');
const metrics = ['Line', 'Branch', 'Function'];
const failures = [], comparisons = [];
const requireGate = (ok, message) => { if (!ok) failures.push(message); };
function compare(a, b, label) {
  const row = { file: label };
  for (const metric of metrics) {
    const covered = `covered${metric}Count`, total = `total${metric}Count`;
    const ratio = s => s[total] ? s[covered] / s[total] : 1;
    row[metric.toLowerCase()] = { before: [a[covered], a[total]], after: [b[covered], b[total]] };
    requireGate(ratio(b) >= ratio(a), `${label}: ${metric} coverage regressed`);
  }
  comparisons.push(row);
}
// Match exact script URLs, including query imports, regardless of raw-file order.
const seen = new Map();
for (const file of before.summary.files) {
  const graph = seen.get(file.path) || 0; seen.set(file.path, graph + 1);
  const next = after.summary.files.find(f => f.url === file.url);
  requireGate(!!next, `missing coverage: ${file.path} graph ${graph}`);
  if (next) compare(file, next, `${basename(file.path)} [graph ${graph}]`);
}
compare(before.summary.totals, after.summary.totals, 'aggregate');
const novel = after.summary.files.filter(f => !before.summary.files.some(b => b.path === f.path));
for (const file of novel) for (const metric of metrics) requireGate(file[`covered${metric}Count`] === file[`total${metric}Count`], `${basename(file.path)}: new module ${metric} coverage below 100%`);

function lineUnion(summary, path) {
  const lines = new Map();
  for (const file of summary.files.filter(f => f.path === path)) for (const line of file.lines) lines.set(line.line, Math.max(lines.get(line.line) || 0, line.count));
  return lines;
}
function scripts(raw, path) { return raw.filter(s => new URL(s.url).pathname === path); }
function mergedFunctions(raw, path) {
  const merged = new Map();
  for (const script of scripts(raw, path)) for (const f of script.functions) {
    const root = f.ranges[0], key = `${root.startOffset}:${root.endOffset}`;
    if (!merged.has(key)) merged.set(key, []);
    merged.get(key).push(f);
  }
  return [...merged.values()];
}
function mapLines(path, oldText, newText) {
  const diff = diffText.split('diff --git ').find(part => part.startsWith(`a/${path} b/${path}\n`)) || '';
  const mapping = new Map(), changed = new Set();
  let oldLine = 1, newLine = 1;
  for (const match of diff.matchAll(/^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/gm)) {
    const oCount = Number(match[2] ?? 1), nCount = Number(match[4] ?? 1);
    const oStart = Number(match[1]) + (oCount === 0 ? 1 : 0), nStart = Number(match[3]) + (nCount === 0 ? 1 : 0);
    while (oldLine < oStart) mapping.set(oldLine++, newLine++);
    for (let i = 0; i < nCount; i++) changed.add(nStart + i);
    oldLine = oStart + oCount; newLine = nStart + nCount;
  }
  while (oldLine <= oldText.split('\n').length) mapping.set(oldLine++, newLine++);
  for (const [old, next] of mapping) requireGate(oldText.split('\n')[old - 1] === newText.split('\n')[next - 1], `invalid unchanged-line mapping ${path}:${old}`);
  return { mapping, changed };
}
const changedProof = [], retainedProof = [];
for (const path of new Set(before.summary.files.map(f => f.path))) {
  const name = basename(path), oldText = readFileSync(join(evidence, 'before-js', name), 'utf8'), newText = readFileSync(path, 'utf8');
  const rel = `web/js/${name}`, { mapping, changed } = mapLines(rel, oldText, newText);
  const oldLines = lineUnion(before.summary, path), newLines = lineUnion(after.summary, path);
  for (const [old, next] of mapping) if (oldLines.get(old) > 0) requireGate(newLines.get(next) > 0, `lost covered unchanged line ${rel}:${old} -> ${next}`);
  const retained = retainedExecutableCoverage(
    scripts(before.raw, path).flatMap(s => s.functions),
    scripts(after.raw, path).flatMap(s => s.functions),
    oldText, newText, mapping,
  );
  for (const failure of retained.failures) requireGate(false, `${rel}: ${failure}`);
  retainedProof.push({ file: rel, changed: changed.size > 0, ranges: retained.proof });
  if (!changed.size) continue;
  for (const line of changed) requireGate(newLines.get(line) > 0, `uncovered changed line ${rel}:${line}`);
  // Functions enclosing changed callbacks also own unrelated code. Audit
  // their changed ranges; audit the full body of each innermost changed
  // function. This discovers composition boundaries from V8's offsets.
  const offsets = [], lines = newText.split(/(?<=\n)/); let offset = 0;
  lines.forEach((text, i) => { if (changed.has(i + 1)) offsets.push([offset, offset + text.length]); offset += text.length; });
  const intersects = range => offsets.some(([s, e]) => s < range.endOffset && e > range.startOffset);
  const functions = mergedFunctions(after.raw, path).filter(copies => intersects(copies[0].ranges[0]));
  for (const copies of functions) {
    const root = copies[0].ranges[0];
    requireGate(copies.some(f => f.ranges[0].count > 0), `uncovered changed function ${rel}:${root.startOffset}`);
    const encloses = functions.some(other => {
      const child = other[0].ranges[0];
      return child.startOffset > root.startOffset && child.endOffset < root.endOffset;
    });
    const branchSet = new Map();
    for (const f of copies) for (const r of f.ranges) {
      if (encloses && !intersects(r)) continue;
      const key = `${r.startOffset}:${r.endOffset}`;
      branchSet.set(key, Math.max(branchSet.get(key) || 0, r.count));
    }
    for (const [range, count] of branchSet) requireGate(count > 0, `uncovered changed function branch ${rel}:${range}`);
    changedProof.push({ file: rel, function: copies[0].functionName || '(callback)', start: root.startOffset, end: root.endOffset, scope: encloses ? 'changed ranges in enclosing function' : 'full function', branches: [...branchSet] });
  }
}
// Go statement counts, per package and overall, using identical atomic profiles.
function goProfile(path) {
  const packages = new Map();
  for (const match of readFileSync(path, 'utf8').matchAll(/^(\S+):\S+ (\d+) (\d+)$/gm)) {
    const pkg = match[1].slice(0, match[1].lastIndexOf('/'));
    const row = packages.get(pkg) || { covered: 0, total: 0 };
    row.total += Number(match[2]); if (Number(match[3]) > 0) row.covered += Number(match[2]); packages.set(pkg, row);
  }
  packages.set('aggregate', [...packages.values()].reduce((a, b) => ({ covered: a.covered + b.covered, total: a.total + b.total }), { covered: 0, total: 0 }));
  return packages;
}
const goBefore = goProfile('/tmp/scimux-menu-before.cover'), goAfter = goProfile('/tmp/scimux-menu-after.cover'), goComparisons = [];
for (const [pkg, b] of goBefore) {
  const a = goAfter.get(pkg);
  requireGate(!!a && a.total === b.total, `Go statement denominator changed: ${pkg}`);
  requireGate(!!a && a.covered >= b.covered, `Go coverage regressed: ${pkg}: ${b.covered}/${b.total} -> ${a?.covered}/${a?.total}`);
  goComparisons.push({ package: pkg, before: b, after: a });
}
writeFileSync(join(evidence, 'coverage-comparison.json'), JSON.stringify({ comparisons, newModules: novel, changedProof, retainedProof, goComparisons, failures }, null, 2));
console.log(JSON.stringify({ comparisons: comparisons.filter(r => ['aggregate', 'app.js [graph 0]', 'app.js [graph 1]', 'harness.js [graph 0]'].includes(r.file)), newModules: novel.map(f => basename(f.path)), changedFunctions: changedProof.length, failures }, null, 2));
assert.deepEqual(failures, [], 'coverage gates failed');
