import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
const { verifiedCoverageEntry } = createRequire(import.meta.url)('./testdata/team-e2e/coverage-source.cjs');

test('coverage verifies each browser entry against its own source snapshot', () => {
  const root = mkdtempSync(join(tmpdir(), 'scimux-coverage-source-'));
  try {
    const current = join(root, 'current'), legacy = join(root, 'legacy');
    for (const dir of [current, legacy]) mkdirSync(dir);
    writeFileSync(join(current, 'chat.js'), 'current');
    writeFileSync(join(legacy, 'chat.js'), 'legacy');
    const entry = { url: 'http://localhost/js/chat.js', source: 'legacy', functions: [] };
    assert.deepEqual(verifiedCoverageEntry(entry, 'old', legacy, 'legacy'), {page:'old', path:'/js/chat.js', source:'legacy', functions:[], sourceVersion:'legacy'});
    assert.throws(() => verifiedCoverageEntry(entry, 'old', current, 'current'), /browser source matches checkout/);
    assert.equal(verifiedCoverageEntry({url:'http://localhost/other.js'}, 'old', legacy, 'legacy'), null);
  } finally { rmSync(root, {recursive:true, force:true}); }
});
