import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { retainedExecutableCoverage } from './menu-coverage-ranges.mjs';

const range = (startOffset, endOffset, count = 1) => ({ startOffset, endOffset, count });
const fn = (start, end, branches = [], count = 1, isBlockCoverage = true) => ({ functionName: 'synthetic', isBlockCoverage, ranges: [range(start, end, count), ...branches] });
const oldText = 'function a(){\n  if (x) yes();\n  else no();\n}\nfunction b(){ return 1; }\n';
const newText = '// added\n' + oldText;
const mapping = new Map([[1, 2], [2, 3], [3, 4], [4, 5], [5, 6], [6, 7]]);
const endA = oldText.indexOf('function b');
const shift = r => range(r.startOffset + 9, r.endOffset + 9, r.count);
const b = fn(endA, oldText.length - 1);
const a = fn(0, endA - 1, [range(oldText.indexOf('else'), oldText.indexOf('no();') + 5)]);
const shifted = f => ({ ...f, ranges: f.ranges.map(shift) });

test('retains shifted functions and branches inside changed files', () => {
  const result = retainedExecutableCoverage([a, b], [shifted(a), shifted(b)], oldText, newText, mapping);
  assert.deepEqual(result.failures, []);
  assert.equal(result.proof.filter(r => r.kind === 'function').length, 2);
  assert.equal(result.proof.filter(r => r.kind === 'branch').length, 3);
});

test('rejects an unchanged branch losing coverage even while its line remains covered', () => {
  const lost = shifted(a); lost.ranges[1].count = 0;
  const result = retainedExecutableCoverage([a, b], [lost, shifted(b)], oldText, newText, mapping);
  assert.equal(result.failures.length, 1);
  assert.match(result.failures[0], /branch/);
});

test('rejects an unchanged function losing coverage or disappearing', () => {
  const lost = shifted(b); lost.ranges[0].count = 0;
  const result = retainedExecutableCoverage([b], [lost], oldText, newText, mapping);
  assert.ok(result.failures.some(f => /function/.test(f)));
  assert.ok(retainedExecutableCoverage([b], [], oldText, newText, mapping).failures.some(f => /function/.test(f)));
});

test('checks surviving branches inside edited functions and exempts only edited text', () => {
  const edited = oldText.replace('yes()', 'different()');
  const oldStart = oldText.indexOf('else'), oldEnd = oldText.indexOf('no();') + 5;
  const newStart = edited.indexOf('else'), newEnd = edited.indexOf('no();') + 5;
  const partial = new Map([[1, 1], [3, 3], [4, 4], [5, 5], [6, 6]]);
  const before = fn(0, endA - 1, [range(oldStart, oldEnd)]);
  const after = fn(0, edited.indexOf('function b') - 1, [range(newStart, newEnd, 0)]);
  const result = retainedExecutableCoverage([before], [after], oldText, edited, partial);
  assert.equal(result.failures.length, 1);
  assert.match(result.failures[0], /branch/);
  assert.equal(result.proof.filter(r => r.kind === 'function').length, 0, 'edited body is checked by the changed-range gate');
});

test('uncovered baseline paths need no retention, function-only records remain functions, and graphs combine', () => {
  const uncovered = fn(0, endA - 1, [], 0);
  const functionOnly = fn(endA, oldText.length - 1, [], 1, false);
  const after = shifted(functionOnly);
  const absent = { ...after, ranges: after.ranges.map(r => ({ ...r, count: 0 })) };
  const result = retainedExecutableCoverage([uncovered, functionOnly, functionOnly], [absent, after], oldText, newText, mapping);
  assert.deepEqual(result.failures, []);
  assert.deepEqual(result.proof.map(r => r.kind), ['function']);
});

test('does not map across insertions within ranges and rejects inaccurate source mappings', () => {
  const inserted = oldText.replace('  else', '// inserted\n  else');
  const insertedMap = new Map([[1, 1], [2, 2], [3, 4], [4, 5], [5, 6], [6, 7]]);
  const result = retainedExecutableCoverage([fn(0, endA - 1, [], 1, false)], [], oldText, inserted, insertedMap);
  assert.deepEqual(result.failures, []);
  assert.deepEqual(result.proof, []);
  assert.throws(() => retainedExecutableCoverage([], [], oldText, newText, new Map([[1, 1]])), /unchanged source/);
});

test('maps UTF-16 offsets and end-exclusive boundaries, including a range at EOF', () => {
  const old = 'const symbol = "🛰";\nfunction retained() {}';
  const next = '// prefix\n' + old;
  const start = old.indexOf('function'), before = fn(start, old.length, [], 1, false);
  const after = fn(start + 10, next.length, [], 1, false);
  const result = retainedExecutableCoverage([before], [after], old, next, new Map([[1, 2], [2, 3]]));
  assert.deepEqual(result.failures, []);
  assert.equal(result.proof.length, 1);
});

test('the verifier applies executable retention to every production file before checking changed ranges', () => {
  const src = readFileSync(new URL('./menu-coverage.mjs', import.meta.url), 'utf8');
  const call = src.indexOf('retainedExecutableCoverage(\n');
  assert.ok(call > src.indexOf('for (const path of new Set('));
  assert.ok(call < src.indexOf('if (!changed.size)'));
});
