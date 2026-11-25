'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');

function verifyMidbodyWitness(manifest) {
  assert.ok(Array.isArray(manifest.assertions), 'assertion manifest is required');
  for (const mode of ['desktop', 'phone']) {
    for (const id of ['people-midbody-muxer-body-read-', 'people-midbody-refused-before-body-', 'people-midbody-frontend-refused-before-body-']) {
      assert.ok(manifest.assertions.some(entry => entry.id === id + mode && entry.result === 'pass'), id + mode + ' missing');
    }
    const evidence = manifest['people-midbody-permission-' + mode];
    assert.equal(evidence?.muxerContinue, 1, 'direct muxer body-read witness missing');
    assert.equal(evidence?.refusedContinue, 0, 'refused request must not enter body read');
    assert.equal(evidence?.refusedStatus, 403, 'read-only request must be refused');
    assert.equal(evidence?.frontContinue, 0, 'frontend must not send Continue before muxer admission');
    assert.equal(evidence?.frontBytes, 0, 'frontend must not read body before muxer admission');
    assert.equal(evidence?.frontStatus, 403, 'frontend request must relay read-only refusal');
  }
}
module.exports = {verifyMidbodyWitness};
if (require.main === module) verifyMidbodyWitness(JSON.parse(fs.readFileSync(process.argv[2], 'utf8')));
