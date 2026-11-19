import test from 'node:test';
import assert from 'node:assert/strict';
import {createRequire} from 'node:module';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {spawnSync} from 'node:child_process';
const require = createRequire(import.meta.url);
const {compare, readFlat, png, audit, bindRoot, runCLI, supported} = require('./testdata/team-e2e/audit-evidence.cjs');

test('diagnostic auditor rejects non-object manifests',()=>{
  const {auditDiagnostic}=require('./testdata/team-e2e/audit-evidence.cjs');
  for(const value of [null,[],42])assert.deepEqual(auditDiagnostic(value),{ok:false});
});

test('counter comparison rejects an unexpected agent dispatch', () => {
  const before = {attempted: 2, accepted: 2, controls: 0};
  const after = {attempted: 3, accepted: 3, controls: 0};
  assert.equal(compare(before, after, [0, 0, 0]), false);
  assert.equal(compare(before, after, [1, 1, 0]), true);
});

test('counter comparison checks exact types, decreases and controls', () => {
  const base = {attempted: 2, accepted: 1, controls: 0};
  for (const bad of [
    {attempted: '2', accepted: 1, controls: 0},
    {attempted: 1, accepted: 1, controls: 0},
    {attempted: 2, accepted: 1, controls: 1},
    {attempted: 2, accepted: 3, controls: 0},
    {attempted: 2, accepted: 1},
  ]) assert.equal(compare(base, bad, [0, 0, 0]), false);
  assert.equal(compare(base, {attempted: 3, accepted: 1, controls: 0}, [1, 0, 0]), true);
});


test('bounded reads refuse traversal, symlinks and non-regular files', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'scimux-audit-synthetic-'));
  const previous = process.cwd();
  try {
    fs.writeFileSync(path.join(root, 'synthetic'), 'small');
    fs.symlinkSync('synthetic', path.join(root, 'link'));
    fs.mkdirSync(path.join(root, 'directory'));
    process.chdir(root);
    assert.equal(readFlat('synthetic', 5).toString(), 'small');
    for (const name of ['../synthetic', '/synthetic', 'link', 'directory', 'missing']) {
      assert.throws(() => readFlat(name, 5));
    }
    assert.throws(() => readFlat('synthetic', 4));
  } finally {
    process.chdir(previous);
    fs.rmSync(root, {recursive: true, force: true});
  }
});


test('PNG validation checks a signature and the minimal IHDR structure', () => {
  const valid = Buffer.from('89504e470d0a1a0a0000000d494844520000000100000001080600000000000000', 'hex');
  assert.equal(png(valid), true);
  for (const bad of [Buffer.alloc(0), Buffer.from('not a png'), valid.subarray(0, 29), Buffer.concat([valid.subarray(0, 8), Buffer.alloc(25)])]) {
    assert.equal(png(bad), false);
  }
});

test('complete profile does not accept an empty or stripped manifest', () => {
  assert.throws(() => audit({version: 1, result: 'pass', assertions: [], artifacts: [], counters: {}}, 'people-attachments'));
  assert.throws(() => audit({}, 'owner-agent-attachments'));
  assert.throws(() => audit({}, 'unknown'));
});

test('a nonempty incomplete manifest cannot pass as a full profile', () => {
  const manifest = {version: 1, result: 'pass', assertions: [{id: 'one', result: 'pass'}], artifacts: ['one.png'], counters: {}};
  assert.throws(() => audit(manifest, 'owner-agent-attachments'));
  assert.throws(() => audit(manifest, 'people-attachments'));
});

// A small handwritten synthetic run; its values are unrelated to recorded runs.
const {synthetic} = require('./testdata/team-e2e/synthetic-audit-manifest.cjs');

test('source-defined profiles accept a complete synthetic run and reject tampering', () => {
  for (const profile of ['owner-agent-attachments', 'people-attachments']) {
    const m = synthetic(profile);
    assert.equal(audit(m, profile).screenshots, 1);
    const missing = structuredClone(m);
    delete missing['stage1-unread-phone'].afterReader;
    assert.throws(() => audit(missing, profile));
    const badControl = structuredClone(m);
    badControl['stage1-unread-phone'].afterReader.controls++;
    assert.throws(() => audit(badControl, profile));
    const badSend = structuredClone(m);
    badSend['trace-address-desktop'].afterSend.accepted = 0;
    assert.throws(() => audit(badSend, profile));
    const duplicate = structuredClone(m);
    duplicate.assertions.push({...duplicate.assertions[0]});
    assert.throws(() => audit(duplicate, profile));
    if (profile === 'people-attachments') {
      const stripped = structuredClone(m);
      delete stripped['people-files-desktop'];
      assert.throws(() => audit(stripped, profile));
    }
  }
});

test('unknown counter windows and stripped scenario assertions fail closed', () => {
  const base = synthetic('people-attachments');
  const missingFeature = structuredClone(base);
  missingFeature.assertions = missingFeature.assertions.filter(x => x.id !== 'people-image-prepare-no-dispatch-desktop');
  assert.throws(() => audit(missingFeature, 'people-attachments'));
  const quota = structuredClone(base);
  quota.assertions = quota.assertions.filter(x => x.id !== 'people-quota-no-implicit-dispatch-phone');
  assert.throws(() => audit(quota, 'people-attachments'));
  const extra = structuredClone(base);
  extra['unrecognized-desktop'] = {before: {attempted: 0, accepted: 0, controls: 0}, after: {attempted: 1, accepted: 1, controls: 0}};
  assert.throws(() => audit(extra, 'people-attachments'));
  const absentAssertion = structuredClone(base);
  absentAssertion.assertions = absentAssertion.assertions.filter(x => x.id !== 'native-authoritative-response');
  assert.throws(() => audit(absentAssertion, 'people-attachments'));
});


test('binding refuses a root symlink and remains attached after root replacement', () => {
  const parent = fs.mkdtempSync(path.join(os.tmpdir(), 'scimux-audit-root-'));
  const old = process.cwd();
  try {
    const root = path.join(parent, 'root');
    const target = path.join(parent, 'target');
    fs.mkdirSync(root); fs.mkdirSync(target);
    fs.writeFileSync(path.join(root, 'manifest.json'), 'original');
    fs.writeFileSync(path.join(target, 'manifest.json'), 'different');
    fs.symlinkSync(target, path.join(parent, 'link'));
    assert.throws(() => bindRoot(path.join(parent, 'link')));
    bindRoot(root);
    fs.renameSync(root, path.join(parent, 'moved'));
    fs.symlinkSync(target, root);
    assert.equal(readFlat('manifest.json', 20).toString(), 'original');
    fs.unlinkSync('manifest.json');
    fs.symlinkSync(path.join(target, 'manifest.json'), 'manifest.json');
    assert.throws(() => readFlat('manifest.json', 20));
  } finally {
    process.chdir(old);
    fs.rmSync(parent, {recursive: true, force: true});
  }
});


test('CLI sanitizes usage, path, read and parse failures', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'scimux-audit-cli-'));
  const old = process.cwd();
  try {
    for (const args of [[], ['--profile', 'poison-secret', root], ['--profile', 'people-attachments', '/missing-poison-path']]) {
      let output = '';
      assert.equal(runCLI(args, text => { output += text; }), 1);
      assert.equal(output, 'FAIL\n');
    }
    fs.writeFileSync(path.join(root, 'manifest.json'), '{poison-secret');
    let output = '';
    assert.equal(runCLI(['--profile', 'people-attachments', root], text => { output += text; }), 1);
    assert.equal(output, 'FAIL\n');
  } finally { process.chdir(old); fs.rmSync(root, {recursive: true, force: true}); }
});

test('CLI counts synthetic screenshots and refuses damaged references without leaking input', () => {
  const parent = fs.mkdtempSync(path.join(os.tmpdir(), 'scimux-audit-output-'));
  const old = process.cwd();
  const screenshot = Buffer.from('89504e470d0a1a0a0000000d494844520000000100000001080600000000000000', 'hex');
  try {
    const m = synthetic('people-attachments');
    fs.writeFileSync(path.join(parent, 'manifest.json'), JSON.stringify(m));
    fs.writeFileSync(path.join(parent, 'screen.png'), screenshot);
    fs.writeFileSync(path.join(parent, 'fixture.png'), screenshot);
    let output = '';
    assert.equal(runCLI(['--profile', 'people-attachments', parent], text => { output += text; }), 0);
    assert.match(output, /^assertions \d+\nscreenshots 1\nhistorical comparisons \d+\nadditional comparisons \d+\nafterNative seats \d+\nPASS\n$/);
    for (const artifacts of [['screen.png', 'screen.png'], ['missing-poison.png'], ['../poison.png'], ['link.png'], ['screen.png']]) {
      process.chdir(old);
      m.artifacts = artifacts;
      fs.writeFileSync(path.join(parent, 'manifest.json'), JSON.stringify(m));
      if (artifacts[0] === 'link.png') fs.symlinkSync('screen.png', path.join(parent, 'link.png'));
      if (artifacts.length === 1 && artifacts[0] === 'screen.png') fs.writeFileSync(path.join(parent, 'screen.png'), 'poison-secret');
      output = '';
      assert.equal(runCLI(['--profile', 'people-attachments', parent], text => { output += text; }), 1);
      assert.equal(output, 'FAIL\n');
    }
  } finally { process.chdir(old); fs.rmSync(parent, {recursive: true, force: true}); }
});

test('profile rejects malformed evidence and handles additional zero windows', () => {
  const base = synthetic('people-attachments');
  const cases = [
    m => {m.assertions[0].result = 'fail';},
    m => {m.assertions[0].id = '';},
    m => {m.assertions = m.assertions.filter(a => a.id !== 'native-exact-one-prompt');},
    m => {m.artifacts = [null];},
    m => {m.artifacts = ['not-a-screenshot.txt'];},
    m => {m.artifacts = ['../bad.png'];},
    m => {delete m['stage1-unread-desktop'];},
    m => {delete m['stale-membership-desktop'].after.observer;},
    m => {delete m.counters.afterNative;},
    m => {delete m.counters.afterNative.observer;},
    m => {m.counters.afterNative.observer.controls = 1;},
    m => {m['extra'] = {before: {role: {attempted: 0, accepted: 0, controls: 0}}, after: {other: {attempted: 0, accepted: 0, controls: 0}}};},
    m => {m['extra'] = {before: {role: {attempted: 0, accepted: 0, controls: 0}}, after: {role: {attempted: 0, accepted: 0, controls: 1}}};},
  ];
  for (const mutate of cases) { const changed = structuredClone(base); mutate(changed); assert.throws(() => audit(changed, 'people-attachments')); }
  const extra = structuredClone(base);
  extra.unrelated = {before: {role: {attempted: 0, accepted: 0, controls: 0}}, after: {role: {attempted: 0, accepted: 0, controls: 0}}};
  assert.equal(audit(extra, 'people-attachments').additional, audit(base, 'people-attachments').additional + 1);
});

test('directory swap while binding is rejected', () => {
  const parent = fs.mkdtempSync(path.join(os.tmpdir(), 'scimux-audit-race-'));
  const old = process.cwd();
  const lstat = fs.lstatSync;
  try {
    const root = path.join(parent, 'root');
    fs.mkdirSync(root); fs.mkdirSync(path.join(parent, 'replacement'));
    fs.lstatSync = (...args) => {
      const result = lstat(...args);
      fs.renameSync(root, path.join(parent, 'moved'));
      fs.renameSync(path.join(parent, 'replacement'), root);
      return result;
    };
    assert.throws(() => bindRoot(root));
  } finally {
    fs.lstatSync = lstat;
    process.chdir(old);
    fs.rmSync(parent, {recursive: true, force: true});
  }
});

test('unknown snapshots distinguish ordinary fields, nested mismatches and malformed counts', () => {
  const base = synthetic('owner-agent-attachments');
  const plain = structuredClone(base);
  plain.extra = {before: {number: 4}, after: {number: 5}};
  assert.doesNotThrow(() => audit(plain, 'owner-agent-attachments'));
  const mismatch = structuredClone(base);
  mismatch.extra = {before: {role: {attempted: 0, accepted: 0, controls: 0}}, after: {}};
  assert.throws(() => audit(mismatch, 'owner-agent-attachments'));
  const malformed = structuredClone(base);
  malformed.extra = {before: null, after: {attempted: 0, accepted: 0, controls: 0}};
  assert.throws(() => audit(malformed, 'owner-agent-attachments'));
  const missing = structuredClone(base);
  missing.assertions = missing.assertions.filter(x => x.id !== 'refusal-no-retry-phone-true');
  assert.throws(() => audit(missing, 'owner-agent-attachments'));
});

test('a file that grows after its size check is bounded', () => {
  const parent = fs.mkdtempSync(path.join(os.tmpdir(), 'scimux-audit-grow-'));
  const old = process.cwd(); const stat = fs.fstatSync;
  try {
    fs.writeFileSync(path.join(parent, 'small'), 'larger');
    process.chdir(parent);
    fs.fstatSync = fd => { const value = stat(fd); value.size = 0; return value; };
    assert.throws(() => readFlat('small', 2));
  } finally {fs.fstatSync = stat; process.chdir(old); fs.rmSync(parent, {recursive:true, force:true});}
});

test('unknown counter windows under counters cannot hide dispatches', () => {
  const m = synthetic('people-attachments');
  m.counters.surprise = {before: {attempted: 0, accepted: 0, controls: 0}, after: {attempted: 1, accepted: 1, controls: 0}};
  assert.throws(() => audit(m, 'people-attachments'));
});

test('unknown unpaired counter observations fail closed', () => {
  const m = synthetic('people-attachments');
  m.surprise = {before: {attempted: 0, accepted: 0, controls: 0}};
  assert.throws(() => audit(m, 'people-attachments'));
});


test('unsupported platforms and missing no-follow primitives fail closed', () => {
  assert.equal(supported('win32', 1, 1), false);
  assert.equal(supported('linux', undefined, 1), false);
  assert.equal(supported('linux', 1, 0), false);
  assert.equal(supported('linux', 1, 1), true);
});

test('unknown unpaired after counts and missing setup snapshots fail closed', () => {
  const base = synthetic('people-attachments');
  base.counters.afterExtraSetup = {'owner-a': {attempted: 0, accepted: 0, controls: 0}, 'owner-b': {attempted: 0, accepted: 0, controls: 0}, recipient: {attempted: 0, accepted: 0, controls: 0}, observer: {attempted: 0, accepted: 0, controls: 0}};
  for (const mode of ['desktop', 'phone']) base['native-start-failure-'+mode] = {counters: {attempted: 0, accepted: 0, controls: 0}};
  assert.doesNotThrow(() => audit(base, 'people-attachments'));
  const surprise = structuredClone(base); surprise.surprise = {after: {attempted: 1, accepted: 1, controls: 0}};
  assert.throws(() => audit(surprise, 'people-attachments'));
  const missing = structuredClone(base); delete missing.counters.afterExtraSetup;
  assert.throws(() => audit(missing, 'people-attachments'));
});


test('CLI subprocess reports only fixed counts or sanitized FAIL', () => {
  const parent = fs.mkdtempSync(path.join(os.tmpdir(), 'scimux-audit-process-'));
  const executable = path.resolve('web/test/testdata/team-e2e/audit-evidence.cjs');
  try {
    fs.writeFileSync(path.join(parent, 'manifest.json'), JSON.stringify(synthetic('owner-agent-attachments')));
    fs.writeFileSync(path.join(parent, 'screen.png'), Buffer.from('89504e470d0a1a0a0000000d494844520000000100000001080600000000000000', 'hex'));
    const good = spawnSync(process.execPath, [executable, '--profile', 'owner-agent-attachments', parent], {encoding: 'utf8'});
    assert.ifError(good.error); assert.equal(good.status, 0); assert.equal(good.stderr, '');
    assert.match(good.stdout, /^assertions \d+\nscreenshots 1\nhistorical comparisons \d+\nadditional comparisons \d+\nafterNative seats \d+\nPASS\n$/);
    for (const args of [[], ['--profile', 'poison-secret', parent]]) {
      const bad = spawnSync(process.execPath, [executable, ...args], {encoding: 'utf8'});
      assert.ifError(bad.error); assert.equal(bad.status, 1); assert.equal(bad.stdout, 'FAIL\n'); assert.equal(bad.stderr, '');
    }
    fs.writeFileSync(path.join(parent, 'manifest.json'), '{poison-secret');
    const bad = spawnSync(process.execPath, [executable, '--profile', 'owner-agent-attachments', parent], {encoding: 'utf8'});
    assert.ifError(bad.error); assert.equal(bad.status, 1); assert.equal(bad.stdout, 'FAIL\n'); assert.equal(bad.stderr, '');
  } finally { fs.rmSync(parent, {recursive: true, force: true}); }
});

test('unsupported runtime returns a fixed failure before opening evidence', () => {
  const original = Object.getOwnPropertyDescriptor(process, 'platform');
  try {
    Object.defineProperty(process, 'platform', {value: 'win32', configurable: true});
    let output = '';
    assert.equal(runCLI(['--profile', 'owner-agent-attachments', '/private-poison'], text => {output += text;}), 1);
    assert.equal(output, 'FAIL\n');
  } finally { Object.defineProperty(process, 'platform', original); }
});

test('startup failure snapshot must have a valid zero-control counter', () => {
  const base = synthetic('owner-agent-attachments');
  for (const change of [
    m => {delete m['native-start-failure-phone'];},
    m => {m['native-start-failure-phone'].counters = null;},
    m => {m['native-start-failure-phone'].counters.controls = 1;},
  ]) { const m = structuredClone(base); change(m); assert.throws(() => audit(m, 'owner-agent-attachments')); }
});

test('required counter intervals reject nonzero controls even when unchanged', () => {
  for (const profile of ['owner-agent-attachments', 'people-attachments']) {
    const manifest = synthetic(profile);
    assert.doesNotThrow(() => audit(manifest, profile));
    manifest['stage1-unread-desktop'].beforeOwner.controls = 1;
    manifest['stage1-unread-desktop'].afterOwner.controls = 1;
    assert.throws(() => audit(manifest, profile));
  }
});

test('unknown nested array windows cannot hide a dispatch', () => {
  for (const profile of ['owner-agent-attachments', 'people-attachments']) {
    const manifest = synthetic(profile);
    assert.doesNotThrow(() => audit(manifest, profile));
    manifest.unrecognized = {before: {items: [{attempted: 0, accepted: 0, controls: 0}]}, after: {items: [{attempted: 1, accepted: 1, controls: 0}]}};
    assert.throws(() => audit(manifest, profile));
  }
});

// These expectations come from explicit actions in the public scenario source.
test('rework: zero, explicit send, refusal and three-send rules are exact', () => {
  const m = synthetic('people-attachments');
  assert.doesNotThrow(() => audit(m, 'people-attachments'));
  for (const mutate of [
    x => {x['people-quota-desktop'].after.recipient.attempted = 1;},
    x => {x['people-quota-desktop'].after.recipient.accepted = 1;},
    x => {x['trace-address-desktop'].afterSend.attempted = 2;},
    x => {x['trace-address-desktop'].afterSend.accepted = 0;},
    x => {x['refusal-phone-false'].after.accepted = 1;},
    x => {x['refusal-phone-false'].after.attempted = 0;},
    x => {x['people-files-phone'].after.owner.attempted = 2;},
    x => {x['people-files-phone'].after.owner.accepted = 2;},
    x => {x['stage2-shared-phone'].afterReader.accepted = 0;},
    x => {x['stage2-shared-phone'].afterOther.attempted = 1;},
    x => {x['dropped-send-phone'].final.attempted = 1;},
    x => {x['held-evidence-desktop'].during.attempted = 0;},
    x => {x['held-evidence-desktop'].after.attempted = 2;},
  ]) { const changed = structuredClone(m); mutate(changed); assert.throws(() => audit(changed, 'people-attachments')); }
});

test('rework: required roles, additional intervals and profile assertions cannot be stripped', () => {
  const m = synthetic('people-attachments');
  assert.doesNotThrow(() => audit(m, 'people-attachments'));
  for (const mutate of [
    x => {delete x['people-files-desktop'].before.recipient;},
    x => {delete x['people-cross-channel-phone'].after.second;},
    x => {x['stale-membership-phone'].before = [];},
    x => {delete x['trace-choices-desktop'].secondCounts;},
    x => {delete x['recreated-phone-true'].afterCreate;},
    x => {delete x.counters['recovery-phone'].after.observer;},
    x => {delete x.counters.afterNative['old-reader-phone'];},
    x => {delete x['native-start-failure-desktop'];},
    x => {x.assertions = x.assertions.filter(a => a.id !== 'people-retained-file-explicit-delivery-phone');},
    x => {x.assertions.push({...x.assertions[0]});},
    x => {x.assertions[0].result = 'fail';},
  ]) { const changed = structuredClone(m); mutate(changed); assert.throws(() => audit(changed, 'people-attachments')); }
  const old = synthetic('owner-agent-attachments');
  assert.doesNotThrow(() => audit(old, 'owner-agent-attachments'));
  assert.throws(() => audit(old, 'people-attachments'));
});

test('rework: every observable synthetic counter forbids absolute controls', () => {
  for (const profile of ['owner-agent-attachments', 'people-attachments']) {
    const manifest = synthetic(profile), paths = [];
    function visit(value, path = []) {
      if (!value || typeof value !== 'object') return;
      if (Object.keys(value).sort().join(',') === 'accepted,attempted,controls') { paths.push(path); return; }
      for (const [key, child] of Object.entries(value)) visit(child, [...path, key]);
    }
    visit(manifest);
    assert.ok(paths.length > 0);
    for (const path of paths) {
      const changed = structuredClone(manifest);
      let target = changed; for (const key of path) target = target[key];
      target.controls = 1;
      assert.throws(() => audit(changed, profile));
    }
  }
});

test('rework: malformed counters and unknown nested arrays cannot evade validation', () => {
  const base = synthetic('people-attachments');
  assert.doesNotThrow(() => audit(base, 'people-attachments'));
  const invalid = [null, [], {attempted: 0, accepted: 0}, {attempted: '0', accepted: 0, controls: 0},
    {attempted: -1, accepted: 0, controls: 0}, {attempted: 0, accepted: 1, controls: 0},
    {attempted: 0, accepted: 0, controls: 0, extra: 0}];
  for (const bad of invalid) {const m = structuredClone(base); m['stage1-unread-desktop'].afterOwner = bad; assert.throws(() => audit(m, 'people-attachments'));}
  for (const item of [
    {before: [{attempted: 0, accepted: 0, controls: 0}], after: [{attempted: 1, accepted: 1, controls: 0}]},
    {before: {nested: [[{attempted: 0, accepted: 0, controls: 1}]]}, after: {nested: [[{attempted: 0, accepted: 0, controls: 1}]]}},
    {before: {nested: [{attempted: 0, accepted: 0}]}, after: {nested: [{attempted: 0, accepted: 0}]}},
  ]) {const m = structuredClone(base); m.unrecognized = item; assert.throws(() => audit(m, 'people-attachments'));}
});

const reworkCases = [
  ['zero implicit attempt', m => {m['people-quota-desktop'].after.recipient.attempted = 1;}],
  ['zero implicit acceptance', m => {m['people-quota-desktop'].after.recipient.accepted = 1;}],
  ['one explicit attempt', m => {m['trace-address-desktop'].afterSend.attempted = 2;}],
  ['one explicit acceptance', m => {m['trace-address-desktop'].afterSend.accepted = 0;}],
  ['refusal remains unaccepted', m => {m['refusal-phone-false'].after.accepted = 1;}],
  ['refusal has one attempt', m => {m['refusal-phone-false'].after.attempted = 0;}],
  ['three explicit attempts', m => {m['people-files-phone'].after.owner.attempted = 2;}],
  ['three explicit acceptances', m => {m['people-files-phone'].after.owner.accepted = 2;}],
  ['recipient local fork', m => {m['stage2-shared-phone'].afterReader.accepted = 0;}],
  ['other local agent stays unchanged', m => {m['stage2-shared-phone'].afterOther.attempted = 1;}],
  ['reviewed retry interval', m => {m['dropped-send-phone'].final.attempted = 1;}],
  ['held delivery interim interval', m => {m['held-evidence-desktop'].during.attempted = 0;}],
  ['held delivery final interval', m => {m['held-evidence-desktop'].after.attempted = 2;}],
  ['missing recipient', m => {delete m['people-files-desktop'].before.recipient;}],
  ['missing second owner', m => {delete m['people-cross-channel-phone'].after.second;}],
  ['array in required seat map', m => {m['stale-membership-phone'].before = [];}],
  ['missing second send snapshot', m => {delete m['trace-choices-desktop'].secondCounts;}],
  ['missing recreation snapshot', m => {delete m['recreated-phone-true'].afterCreate;}],
  ['missing seat from recovery', m => {delete m.counters['recovery-phone'].after.observer;}],
  ['missing final seat', m => {delete m.counters.afterNative['old-reader-phone'];}],
  ['missing startup snapshot', m => {delete m['native-start-failure-desktop'];}],
  ['missing feature assertion', m => {m.assertions = m.assertions.filter(a => a.id !== 'people-retained-file-explicit-delivery-phone');}],
  ['duplicate assertion', m => {m.assertions.push({...m.assertions[0]});}],
  ['failing assertion', m => {m.assertions[0].result = 'fail';}],
  ['null counter', m => {m['stage1-unread-desktop'].afterOwner = null;}],
  ['array counter', m => {m['stage1-unread-desktop'].afterOwner = [];}],
  ['missing counter field', m => {delete m['stage1-unread-desktop'].afterOwner.controls;}],
  ['string counter field', m => {m['stage1-unread-desktop'].afterOwner.attempted = '0';}],
  ['negative counter field', m => {m['stage1-unread-desktop'].afterOwner.attempted = -1;}],
  ['excess accepted', m => {m['stage1-unread-desktop'].afterOwner.accepted = 1;}],
  ['extra counter field', m => {m['stage1-unread-desktop'].afterOwner.extra = 0;}],
  ['unchanged nonzero controls', m => {m['stage1-unread-desktop'].beforeOwner.controls = 1; m['stage1-unread-desktop'].afterOwner.controls = 1;}],
  ['nested array dispatch', m => {m.unrecognized = {before: {items: [{attempted: 0, accepted: 0, controls: 0}]}, after: {items: [{attempted: 1, accepted: 1, controls: 0}]}};}],
  ['nested array controls', m => {m.unrecognized = {before: {nested: [[{attempted: 0, accepted: 0, controls: 1}]]}, after: {nested: [[{attempted: 0, accepted: 0, controls: 1}]]}};}],
  ['nested malformed array counter', m => {m.unrecognized = {before: [{attempted: 0, accepted: 0}], after: [{attempted: 0, accepted: 0}]};}],
];
for (const [name, mutate] of reworkCases) {
  test('matrix RED: ' + name, () => {
    const m = synthetic('people-attachments');
    assert.doesNotThrow(() => audit(m, 'people-attachments'));
    mutate(m);
    assert.throws(() => audit(m, 'people-attachments'));
  });
}
test('matrix RED: independent profile selection cannot promote old evidence', () => {
  const old = synthetic('owner-agent-attachments');
  assert.doesNotThrow(() => audit(old, 'owner-agent-attachments'));
  assert.throws(() => audit(old, 'people-attachments'));
});

test('matrix RED: standalone unknown counter snapshots reject nonzero controls', () => {
  const m = synthetic('owner-agent-attachments');
  assert.doesNotThrow(() => audit(m, 'owner-agent-attachments'));
  m.observation = {snapshot: {attempted: 0, accepted: 0, controls: 1}};
  assert.throws(() => audit(m, 'owner-agent-attachments'));
});
test('matrix RED: unknown scalar windows require zero change', () => {
  const m = synthetic('owner-agent-attachments');
  assert.doesNotThrow(() => audit(m, 'owner-agent-attachments'));
  m.observation = {before: {attempted: 0, accepted: 0, controls: 0}, after: {attempted: 1, accepted: 1, controls: 0}};
  assert.throws(() => audit(m, 'owner-agent-attachments'));
});

test('standalone nested malformed counter arrays are not ignored', () => {
  const base = synthetic('owner-agent-attachments');
  assert.doesNotThrow(() => audit(base, 'owner-agent-attachments'));
  for (const snapshot of [{controls: 1}, {attempted: 1}, {accepted: 1}]) {
    const m = structuredClone(base); m.extra = {snapshots: [[snapshot]]};
    assert.throws(() => audit(m, 'owner-agent-attachments'));
  }
});

test('standalone unknown arrays cannot hide counter snapshots', () => {
  const m = synthetic('people-attachments');
  assert.doesNotThrow(() => audit(m, 'people-attachments'));
  m.extra = {snapshots: [[{attempted: 1, accepted: 1, controls: 0}]]};
  assert.throws(() => audit(m, 'people-attachments'));
});

test('nested object windows reject an implicit send', () => {
  const m = synthetic('people-attachments');
  m.observation = {nested: {before: {attempted: 0, accepted: 0, controls: 0}, after: {attempted: 1, accepted: 1, controls: 0}}};
  assert.throws(() => audit(m, 'people-attachments'));
});

test('unknown record arrays reject implicit sends', () => {
  const m = synthetic('people-attachments');
  m.observation = [{before: {attempted: 0, accepted: 0, controls: 0}, after: {attempted: 1, accepted: 1, controls: 0}}];
  assert.throws(() => audit(m, 'people-attachments'));
});

test('deep unknown zero windows inside record arrays count once', () => {
  const m = synthetic('people-attachments');
  const base = audit(m, 'people-attachments').additional;
  m.observation = [{nested: {window: {before: {role: {attempted: 0, accepted: 0, controls: 0}}, after: {role: {attempted: 0, accepted: 0, controls: 0}}}}}];
  assert.equal(audit(m, 'people-attachments').additional, base + 1);
});

test('deep unknown counter values are compared through wrappers', () => {
  const m = synthetic('owner-agent-attachments');
  m.observation = {nested: {before: {roles: [{attempted: 0, accepted: 0, controls: 0}]}, after: {roles: [{attempted: 1, accepted: 1, controls: 0}]}}};
  assert.throws(() => audit(m, 'owner-agent-attachments'));
});

test('deep unknown before and after snapshots require a matching endpoint', () => {
  for (const field of ['before', 'after']) {
    const m = synthetic('owner-agent-attachments');
    m.observation = [{nested: {[field]: {attempted: 0, accepted: 0, controls: 0}}}];
    assert.throws(() => audit(m, 'owner-agent-attachments'));
  }
});

test('unknown matching zero arrays are compared without adding unique sends', () => {
  const m = synthetic('owner-agent-attachments');
  const base = audit(m, 'owner-agent-attachments').additional;
  m.observation = {nested: {before: {roles: [{attempted: 0, accepted: 0, controls: 0}]}, after: {roles: [{attempted: 0, accepted: 0, controls: 0}]}}};
  assert.equal(audit(m, 'owner-agent-attachments').additional, base + 1);
});

test('unknown nested window rejects role-key substitution on either side', () => {
  const m = synthetic('people-attachments');
  m.observation = {nested: {before: {unrelated: 'x', removed: 'y'}, after: {unrelated: 'x', added: {attempted: 1, accepted: 1, controls: 0}}}};
  assert.throws(() => audit(m, 'people-attachments'));
});

for(const [scope,result] of [['focused','pass'],['failure','fail'],['interrupted','interrupted']]) {
  test(`diagnostic ${scope} remains distinct from full acceptance`,()=>{
    const {auditDiagnostic}=require('./testdata/team-e2e/audit-evidence.cjs');
    const manifest={version:1,scope,result,assertions:[],artifacts:[],counters:{before:{attempted:0,accepted:0,controls:0},after:{attempted:0,accepted:0,controls:0}}};
    if(scope==='interrupted')manifest.signal='SIGINT';
    assert.deepEqual(auditDiagnostic(manifest),{ok:true,scope,result,acceptance:false});
    assert.throws(()=>audit(manifest,'people-attachments'));
  });
}

test('diagnostic evidence refuses unsafe content and invalid outcome',()=>{
  const {auditDiagnostic}=require('./testdata/team-e2e/audit-evidence.cjs');
  const base={version:1,scope:'failure',result:'fail',assertions:[],artifacts:[],counters:{before:{attempted:0,accepted:0,controls:0},after:{attempted:0,accepted:0,controls:0}}};
  for(const mutation of [m=>{m.path='/synthetic/private';},m=>{m.result='pass';},m=>{m.artifacts=['../synthetic.png'];}]) {
    const m=structuredClone(base);mutation(m);assert.deepEqual(auditDiagnostic(m),{ok:false});
  }
});

test('interrupted diagnostic requires its first signal',()=>{
  const {auditDiagnostic}=require('./testdata/team-e2e/audit-evidence.cjs');
  const manifest={version:1,scope:'interrupted',result:'interrupted',assertions:[],artifacts:[],counters:{before:{attempted:0,accepted:0,controls:0},after:{attempted:0,accepted:0,controls:0}}};
  assert.deepEqual(auditDiagnostic(manifest),{ok:false});
});

test('diagnostic scope does not invent counters absent from a focused probe',()=>{
  const {auditDiagnostic}=require('./testdata/team-e2e/audit-evidence.cjs');
  const manifest={version:1,scope:'focused',result:'pass',assertions:[{id:'browser-lifecycle-retains-coverage-desktop',result:'pass'}],artifacts:['lifecycle-desktop.png'],counters:{}};
  assert.deepEqual(auditDiagnostic(manifest),{ok:true,scope:'focused',result:'pass',acceptance:false});
  assert.throws(()=>audit(manifest,'people-attachments'));
});

test('failed diagnostic may retain the first signal as evidence of intent',()=>{
  const {auditDiagnostic}=require('./testdata/team-e2e/audit-evidence.cjs');
  const manifest={version:1,scope:'failure',result:'fail',signal:'SIGINT',assertions:[],artifacts:[],counters:{}};
  assert.deepEqual(auditDiagnostic(manifest),{ok:true,scope:'failure',result:'fail',acceptance:false});
});

test('diagnostic audit accepts only fixed sanitized cleanup categories',()=>{
  const {auditDiagnostic}=require('./testdata/team-e2e/audit-evidence.cjs');
  const manifest={version:1,scope:'failure',result:'fail',assertions:[],artifacts:[],counters:{},cleanup:{failures:['coverage','socket']}};
  assert.deepEqual(auditDiagnostic(manifest),{ok:true,scope:'failure',result:'fail',acceptance:false});
  manifest.cleanup.failures.push('synthetic private detail');assert.deepEqual(auditDiagnostic(manifest),{ok:false});
});

test('diagnostic audit accepts bounded positive cleanup observations and fixed failure identity',()=>{
  const {auditDiagnostic}=require('./testdata/team-e2e/audit-evidence.cjs');
  const manifest={version:1,scope:'failure',result:'fail',assertions:[],artifacts:[],counters:{},cleanup:{failures:[],childrenStopped:true,socketRemoved:true},failureCode:'controlled-native-cleanup'};
  assert.equal(auditDiagnostic(manifest).ok,true);
  manifest.failureCode='synthetic private detail';assert.equal(auditDiagnostic(manifest).ok,false);
});

test('diagnostic audit rejects unsupported failure codes, invalid positives and unknown cleanup keys',()=>{
  const {auditDiagnostic}=require('./testdata/team-e2e/audit-evidence.cjs');
  const base={version:1,scope:'failure',result:'fail',assertions:[],artifacts:[],counters:{}};
  for(const failureCode of ['other',false,null])assert.equal(auditDiagnostic({...base,failureCode}).ok,false);
  for(const value of [false,'true',1,null])for(const key of ['childrenStopped','socketRemoved'])assert.equal(auditDiagnostic({...base,cleanup:{failures:[],[key]:value}}).ok,false);
  assert.equal(auditDiagnostic({...base,cleanup:{failures:[],other:true}}).ok,false);
  assert.equal(auditDiagnostic({...base,other:true}).ok,false);
  assert.equal(auditDiagnostic({...base,scope:'interrupted',result:'interrupted',signal:'SIGINT',failureCode:'controlled-native-cleanup'}).ok,false);
  assert.equal(auditDiagnostic({...base,cleanup:{failures:['children','socket'],childrenStopped:true,socketRemoved:true}}).ok,true);
});

test('diagnostic audit validates cleanup container shape and assertion entries',()=>{
 const {auditDiagnostic}=require('./testdata/team-e2e/audit-evidence.cjs');
 const base={version:1,scope:'failure',result:'fail',assertions:[],artifacts:[],counters:{}};
 for(const cleanup of [null,1,[],{}, {failures:'bad'}, {failures:['children','children']}])assert.equal(auditDiagnostic({...base,cleanup}).ok,false);
 for(const assertions of [[null],[{id:'synthetic',result:'pass',other:true}],[{id:'synthetic',result:'pass'},{id:'synthetic',result:'pass'}]])assert.equal(auditDiagnostic({...base,assertions}).ok,false);
});

test('partial diagnostic retains anomalous observed counts without accepting incomplete evidence',()=>{
 const {auditDiagnostic}=require('./testdata/team-e2e/audit-evidence.cjs');
 const manifest={version:1,scope:'failure',result:'fail',assertions:[],artifacts:[],counters:{before:{attempted:2,accepted:1,controls:0},after:{attempted:3,accepted:2,controls:1}},'people-files-desktop':{beforeSend:{attempted:2,accepted:1,controls:0},afterSend:{attempted:3,accepted:2,controls:1}}};
 const original=structuredClone(manifest);assert.deepEqual(auditDiagnostic(manifest),{ok:true,scope:'failure',result:'fail',acceptance:false});assert.deepEqual(manifest,original);assert.throws(()=>audit(manifest,'people-attachments'));
});

test('diagnostic audit rejects unsafe or unknown counter shapes',()=>{
 const {auditDiagnostic}=require('./testdata/team-e2e/audit-evidence.cjs');
 const base={version:1,scope:'failure',result:'fail',assertions:[],artifacts:[],counters:{}};
 for(const counters of [[],{before:'synthetic path'}, {before:[1]}, {before:{attempted:-1}}, {before:{attempted:1.5}}, {before:{attempted:Number.MAX_SAFE_INTEGER+1}}, {unexpected:{attempted:1}}, {before:{accepted:['synthetic']}}])assert.equal(auditDiagnostic({...base,counters}).ok,false);
 assert.equal(auditDiagnostic({...base,'unknown-desktop':{before:{attempted:1}}}).ok,false);
 assert.equal(auditDiagnostic({...base,'people-files-desktop':{before:'synthetic'}}).ok,false);
});

test('diagnostic audit rejects incomplete or mixed counter snapshots',()=>{
 const {auditDiagnostic}=require('./testdata/team-e2e/audit-evidence.cjs');
 const base={version:1,scope:'failure',result:'fail',assertions:[],artifacts:[],counters:{}};
 for(const value of [{attempted:1},{accepted:1,controls:0},{attempted:1,accepted:1,controls:0,before:{attempted:0,accepted:0,controls:0}}])assert.equal(auditDiagnostic({...base,counters:{before:value}}).ok,false);
});
