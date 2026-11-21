'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
function verifyLocalBudget(manifest) {
  assert.ok(Array.isArray(manifest.assertions), 'assertion manifest is required');
  for (const mode of ['desktop', 'phone']) {
    for (const id of ['people-local-budget-refusal-', 'people-local-budget-preserved-', 'people-local-budget-explicit-retry-', 'people-local-budget-zero-agent-']) {
      assert.ok(manifest.assertions.some(entry => entry.id === id + mode && entry.result === 'pass'), id + mode + ' missing');
    }
    const evidence = manifest['people-local-budget-' + mode];
    assert.equal(evidence?.status, 507, 'actual local refusal required');
    assert.equal(evidence?.code, 'storage_full', 'local storage error required');
    assert.equal(evidence?.commentCount, 1, 'exactly one explicit publication required');
  }
}
module.exports = {verifyLocalBudget};
if (require.main === module) verifyLocalBudget(JSON.parse(fs.readFileSync(process.argv[2], 'utf8')));
