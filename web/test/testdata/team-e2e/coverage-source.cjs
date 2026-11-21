'use strict';
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');

function verifiedCoverageEntry(entry, label, sourceDir, sourceVersion) {
  const url = new URL(entry.url);
  if (!/^\/js\/[a-z0-9-]+\.js$/.test(url.pathname)) return null;
  const source = fs.readFileSync(path.join(sourceDir, path.basename(url.pathname)), 'utf8');
  assert.equal(entry.source, source, 'browser source matches checkout: ' + url.pathname);
  return {page:label, path:url.pathname, source, functions:entry.functions, sourceVersion};
}
module.exports = { verifiedCoverageEntry };
