'use strict';

const fields = ['attempted', 'accepted', 'controls'];
function counter(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value) &&
    Object.keys(value).length === fields.length &&
    fields.every(key => Number.isSafeInteger(value[key]) && value[key] >= 0) &&
    value.accepted <= value.attempted && value.controls === 0;
}
function compare(before, after, expected) {
  return counter(before) && counter(after) &&
    fields.every((key, index) => after[key] - before[key] === expected[index]);
}
function readFlat(name, limit) {
  const fs = require('node:fs');
  if (typeof name !== 'string' || !name || name === '.' || name === '..' ||
      name.includes('/') || name.includes('\\') || name.includes('\0') ||
      typeof fs.constants.O_NOFOLLOW !== 'number') throw Error('invalid input');
  const fd = fs.openSync(name, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
  try {
    const stat = fs.fstatSync(fd);
    if (!stat.isFile() || stat.size > limit) throw Error('invalid input');
    const bytes = Buffer.alloc(limit + 1);
    let used = 0;
    while (used < bytes.length) {
      const read = fs.readSync(fd, bytes, used, bytes.length - used, null);
      if (read === 0) break;
      used += read;
    }
    if (used > limit) throw Error('invalid input');
    return bytes.subarray(0, used);
  } finally {
    fs.closeSync(fd);
  }
}
function png(bytes) {
  return Buffer.isBuffer(bytes) && bytes.length >= 33 &&
    bytes.subarray(0, 8).equals(Buffer.from('89504e470d0a1a0a', 'hex')) &&
    bytes.readUInt32BE(8) === 13 && bytes.toString('ascii', 12, 16) === 'IHDR' &&
    bytes.readUInt32BE(16) > 0 && bytes.readUInt32BE(20) > 0;
}
function audit(manifest, profile) {
  if (!['owner-agent-attachments', 'people-attachments', 'corrective'].includes(profile) ||
      manifest === null || typeof manifest !== 'object' || manifest.result !== 'pass' ||
      manifest.version !== 1 || !Array.isArray(manifest.assertions) ||
      !Array.isArray(manifest.artifacts) || !manifest.counters) throw Error('invalid evidence');
  return auditManifest(manifest, profile);
}

const modes = ['desktop', 'phone'];
const initialSeats = ['owner-a', 'owner-b', 'recipient', 'observer'];
const standard = [
  'native-trace-debug', 'native-trace', 'boundary', 'ai-source', 'leave',
  'permission-ended-debug', 'permission', 'held-evidence', 'dropped-send',
  'reshare', 'coordinator-header-focus', 'coordinator-description-save',
  'description-recreated-choice', 'description-choice-order', 'received-offline',
  'stage1-scanner', 'stage1-owner-files', 'stage2-pages', 'draft-recovery',
  'delete-recreation', 'legacy-closed', 'no-selection',
];
const people = [
  'people-files', 'people-uncertain-retry', 'people-upload-permission',
  'people-midbody-permission', 'people-image-bookmark', 'people-source-cancel',
  'people-quota', 'people-local-budget', 'people-file-unread', 'people-recovery',
  'people-relay-missing', 'people-relay-corrupt', 'people-unavailable',
  'people-retired-draft', 'people-copy-replacement', 'people-stale-preparation',
  'people-real-tunnel',
];
const expectedOne = new Set(['native-trace-debug', 'native-trace', 'ai-source',
  'held-evidence', 'dropped-send', 'stage1-scanner', 'stage1-owner-files', 'no-selection']);
function auditManifest(manifest, profile) {
  const assertions = new Set();
  for (const entry of manifest.assertions) {
    if (!entry || typeof entry.id !== 'string' || !entry.id || entry.result !== 'pass' || assertions.has(entry.id)) throw Error('invalid evidence');
    assertions.add(entry.id);
  }
  for (const id of ['comments-zero-agent-attempts', 'native-exact-one-prompt', 'native-authoritative-response']) {
    if (!assertions.has(id)) throw Error('invalid evidence');
  }
  for (const mode of modes) {
    const required = ['early-native-visible-no-agent-dispatch', 'no-selection-explicit-send-only',
      'stage1-scanner-explicit-dispatch-only', 'stage1-shared-owner-idle', 'stage1-discussion-zero-agent-dispatch',
      'stage1-owner-file-delivered', 'stage2-fork-fresh-local', 'stage2-prompt-one-dispatch',
      'dropped-response-explicit-retry-once', 'held-evidence-confirmed-after-release',
      'ai-source-explicit-exact-mirrored', 'trace-explicit-one-dispatch', 'trace-second-explicit-dispatch',
      'native-trace-one-exact-multiline'];
    if (profile !== 'owner-agent-attachments') required.push('people-image-prepare-no-dispatch', 'people-quota-no-implicit-dispatch', 'people-copy-replacement-zero-agent-dispatch', 'people-files-no-implicit-dispatch', 'people-owner-prepare-no-dispatch', 'people-image-no-implicit-dispatch', 'people-retained-prepare-no-dispatch', 'people-stale-source-no-dispatch', 'people-retained-missing-no-dispatch', 'people-retired-review-no-dispatch', 'people-recovery-zero-agent-dispatch', 'people-relay-missing-zero-dispatch', 'people-relay-corrupt-zero-dispatch', 'people-cross-channel-zero-controls', 'people-real-tunnel-no-agent-dispatch', 'people-file-unread-no-agent-dispatch', 'people-local-budget-zero-agent', 'people-uncertain-no-agent-dispatch', 'people-held-upload-no-agent-dispatch', 'people-midbody-no-agent-dispatch', 'people-image-bookmark-no-agent-dispatch', 'legacy-reader-zero-implicit-dispatch', 'old-relay-no-blob-put', 'people-source-cancel-keeps-unrelated-file', 'people-retained-file-explicit-delivery');
    if (profile === 'corrective') required.push('people-stage-competition-consistent', 'people-stage-competition-draft', 'people-stage-competition-zero-agent', 'people-stage-competition-one-append');
    for (const id of required) if (!assertions.has(id + '-' + mode)) throw Error('invalid evidence');
    for (const newer of ['false', 'true']) if (!assertions.has('refusal-no-retry-' + mode + '-' + newer)) throw Error('invalid evidence');
  }
  const screenshots = new Set();
  for (const name of manifest.artifacts) {
    if (typeof name !== 'string') throw Error('invalid evidence');
    if (!name.endsWith('.png')) continue;
    if (!name || name === '.' || name === '..' || name.includes('/') || name.includes('\\') || screenshots.has(name)) throw Error('invalid evidence');
    screenshots.add(name);
  }
  if (!screenshots.size) throw Error('invalid evidence');
  let historical = 0, additional = 0;
  const counted = new Map();
  function window(record, label, left, right, roles, expectations, primary) {
    if (!record || typeof record !== 'object') throw Error('invalid evidence');
    const before = record[left], after = record[right];
    const keys = roles === null ? [null] : roles;
    if (roles !== null && (!before || !after || typeof before !== 'object' || typeof after !== 'object' ||
      Object.keys(before).length !== roles.length || Object.keys(after).length !== roles.length)) throw Error('invalid evidence');
    for (const role of keys) {
      const a = role === null ? before : before[role];
      const b = role === null ? after : after[role];
      const wanted = expectations[role === null ? '' : role] || [0, 0, 0];
      if (!compare(a, b, wanted)) throw Error('invalid evidence');
      if (primary) historical++; else additional++;
    }
    if (!counted.has(record)) counted.set(record, []);
    counted.get(record).push([left, right]);
  }
  for (const mode of modes) {
    for (const name of ['stale-membership', 'stale-removed-membership', 'early-native']) {
      const label = name + '-' + mode;
      window(manifest[label], label, 'before', 'after', initialSeats, {}, true);
    }
    for (const name of standard) {
      const label = name + '-' + mode;
      window(manifest[label], label, 'before', 'after', null, expectedOne.has(name) ? {'': [1, 1, 0]} : {}, true);
    }
    for (const newer of ['false', 'true']) {
      const label = 'refusal-' + mode + '-' + newer;
      window(manifest[label], label, 'before', 'after', null, {'': [1, 0, 0]}, true);
    }
    if (profile !== 'owner-agent-attachments') {
      for (const name of people) {
        const label = name + '-' + mode;
        const expected = name === 'people-files' ? [3, 3, 0] : name === 'people-source-cancel' ? [1, 1, 0] : [0, 0, 0];
        window(manifest[label], label, 'before', 'after', ['owner', 'recipient'], {owner: expected}, true);
      }
      for (const [name, roles] of [['people-cross-channel', ['first', 'second', 'recipient']], ['legacy-reader', ['owner', 'legacy']]]) {
        const label = name + '-' + mode;
        window(manifest[label], label, 'before', 'after', roles, {}, true);
      }
      const label = 'old-relay-' + mode;
      window(manifest[label], label, 'before', 'after', null, {}, true);
    }
    if (profile === 'corrective') window(manifest['people-stage-competition-' + mode], 'people-stage-competition-' + mode, 'before', 'after', ['owner', 'recipient'], {}, true);
    for (const family of ['membership', 'activities', 'recovery']) {
      const label = family + '-' + mode;
      window(manifest.counters[label], 'counters/' + label, 'before', 'after', initialSeats, {}, true);
    }
    for (const [family, left, right, delta] of [
      ['stage1-scanner-owner', 'beforeOwner', 'afterOwner', [1, 1, 0]],
      ['stage1-unread', 'beforeOwner', 'afterOwner', [0, 0, 0]],
      ['stage1-unread', 'beforeReader', 'afterReader', [0, 0, 0]],
      ['stage2-shared', 'beforeOwner', 'afterOwner', [1, 1, 0]],
      ['stage2-shared', 'beforeReader', 'afterReader', [1, 1, 0]],
      ['stage2-shared', 'beforeOther', 'afterOther', [0, 0, 0]],
      ['trace-address', 'beforeSend', 'afterSend', [1, 1, 0]],
      ['trace-choices', 'beforeSecond', 'secondCounts', [1, 1, 0]],
      ['dropped-send', 'after', 'final', [1, 1, 0]],
      ['held-evidence', 'before', 'during', [1, 1, 0]],
      ['held-evidence', 'during', 'after', [0, 0, 0]],
    ]) {
      const label = family + '-' + mode;
      window(manifest[label], label, left, right, null, {'': delta}, false);
    }
    for (const pending of ['false', 'true']) {
      const label = 'recreated-' + mode + '-' + pending;
      window(manifest[label], label, 'afterCreate', 'after', null, {}, false);
    }
  }
  window(manifest.counters, 'counters', 'beforeComments', 'afterComments', initialSeats, {}, false);
  const seats = [...initialSeats, 'empty-desktop', 'empty-phone', 'legacy-closed-desktop', 'legacy-closed-phone', 'no-selection'];
  if (profile !== 'owner-agent-attachments') seats.push('legacy-reader-desktop', 'legacy-reader-phone', 'old-owner-desktop', 'old-reader-desktop', 'old-owner-phone', 'old-reader-phone');
  const setup = manifest.counters.afterExtraSetup;
  if (!setup || Object.keys(setup).length !== initialSeats.length || initialSeats.some(seat => !counter(setup[seat]) || setup[seat].controls !== 0)) throw Error('invalid evidence');
  for (const mode of modes) {
    const snapshot = manifest['native-start-failure-' + mode];
    if (!snapshot || !counter(snapshot.counters) || snapshot.counters.controls !== 0) throw Error('invalid evidence');
  }
  const afterNative = manifest.counters.afterNative;
  if (!afterNative || Object.keys(afterNative).length !== seats.length || seats.some(seat => !counter(afterNative[seat]) || afterNative[seat].controls !== 0)) throw Error('invalid evidence');
  function hasCounter(value) {
    if (!value || typeof value !== 'object') return false;
    if (!Array.isArray(value) && fields.some(key => Object.hasOwn(value, key))) return true;
    return Object.values(value).some(hasCounter);
  }
  function unknownPair(a, b) {
    if (!hasCounter(a) && !hasCounter(b)) return;
    if (!a || !b || typeof a !== 'object' || typeof b !== 'object' || Array.isArray(a) !== Array.isArray(b)) throw Error('invalid evidence');
    if (fields.some(key => Object.hasOwn(a, key) || Object.hasOwn(b, key))) {
      if (!compare(a, b, [0, 0, 0])) throw Error('invalid evidence');
      additional++;
      return;
    }
    const keys = Object.keys(a);
    if (keys.length !== Object.keys(b).length || keys.some(key => !Object.hasOwn(b, key))) throw Error('invalid evidence');
    for (const key of keys) unknownPair(a[key], b[key]);
  }
  function walk(value, inArray = false, endpoint = false) {
    if (!value || typeof value !== 'object') return;
    const array = Array.isArray(value);
    if (!array && fields.filter(key => Object.hasOwn(value, key)).length >= (inArray ? 1 : 2)) {
      if (!counter(value) || (inArray && !endpoint)) throw Error('invalid evidence');
      return;
    }
    const pairs = counted.get(value) || [];
    const endpoints = new Set(pairs.flat());
    if (!array) {
      for (const key of Object.keys(value)) {
        if (key.startsWith('after') && hasCounter(value[key])) {
          const matching = 'before' + key.slice(5);
          const standalone = value === manifest.counters && ['afterExtraSetup', 'afterNative'].includes(key);
          if (!standalone && !Object.hasOwn(value, matching) && !endpoints.has(key)) throw Error('invalid evidence');
        }
        if (!key.startsWith('before')) continue;
        const right = 'after' + key.slice(6);
        if (Object.hasOwn(value, right)) {
          if (!pairs.some(([left, end]) => left === key && end === right)) unknownPair(value[key], value[right]);
          endpoints.add(key); endpoints.add(right);
        } else if (hasCounter(value[key]) && !endpoints.has(key)) {
          throw Error('invalid evidence');
        }
      }
    }
    for (const [key, child] of Object.entries(value)) {
      walk(child, inArray || array, endpoint || endpoints.has(key));
    }
  }
  walk(manifest);
  return {assertions: assertions.size, screenshots: screenshots.size, historical, additional, seats: seats.length, screenshotNames: [...screenshots]};
}
function bindRoot(root) {
  const fs = require('node:fs');
  const before = fs.lstatSync(root);
  if (!before.isDirectory() || before.isSymbolicLink()) throw Error('invalid evidence');
  process.chdir(root);
  const after = fs.statSync('.');
  if (before.dev !== after.dev || before.ino !== after.ino) throw Error('invalid evidence');
}
function runCLI(args, write) {
  try {
    const flags = require('node:fs').constants;
    if (!supported(process.platform, flags.O_NOFOLLOW, flags.O_NONBLOCK)) throw Error('invalid evidence');
    if (args.length !== 3 || args[0] !== '--profile' ||
        !['owner-agent-attachments', 'people-attachments', 'corrective'].includes(args[1]) ||
        !args[2]) throw Error('invalid evidence');
    bindRoot(args[2]);
    const manifest = JSON.parse(readFlat('manifest.json', 16 * 1024 * 1024).toString('utf8'));
    const result = audit(manifest, args[1]);
    for (const name of result.screenshotNames) {
      if (!png(readFlat(name, 32 * 1024 * 1024))) throw Error('invalid evidence');
    }
    write('assertions ' + result.assertions + '\n' +
      'screenshots ' + result.screenshots + '\n' +
      'historical comparisons ' + result.historical + '\n' +
      'additional comparisons ' + result.additional + '\n' +
      'afterNative seats ' + result.seats + '\nPASS\n');
    return 0;
  } catch {
    write('FAIL\n');
    return 1;
  }
}
function supported(platform, nofollow, nonblock) {
  return ['linux', 'darwin', 'freebsd', 'openbsd', 'netbsd', 'aix', 'sunos'].includes(platform) &&
    Number.isInteger(nofollow) && nofollow > 0 && Number.isInteger(nonblock) && nonblock > 0;
}
function auditDiagnostic(manifest) {
  const {counters,observationKey,assertionID}=require('./evidence-project.cjs');
  if(!manifest||typeof manifest!=='object'||Array.isArray(manifest))return {ok:false};
  const outcomes={focused:'pass',failure:'fail',interrupted:'interrupted'};
  if(manifest.version!==1||!Object.hasOwn(outcomes,manifest.scope)||manifest.result!==outcomes[manifest.scope]||
     Object.keys(manifest).some(key=>!['version','scope','result','signal','assertions','artifacts','counters','cleanup','failureCode'].includes(key)&&!observationKey(key))||
     !Array.isArray(manifest.assertions)||!Array.isArray(manifest.artifacts)||!manifest.counters||
     (manifest.scope==='interrupted'?!['SIGINT','SIGTERM'].includes(manifest.signal):manifest.scope==='failure'&&Object.hasOwn(manifest,'signal')?!['SIGINT','SIGTERM'].includes(manifest.signal):Object.hasOwn(manifest,'signal')))return {ok:false};
  try {counters(manifest.counters,true);for(const [key,value] of Object.entries(manifest))if(observationKey(key))counters(value,true);}catch{return {ok:false};}
  if(Object.hasOwn(manifest,'failureCode')&&(manifest.scope!=='failure'||manifest.failureCode!=='controlled-native-cleanup'))return {ok:false};
  if(Object.hasOwn(manifest,'cleanup')) {
    const value=manifest.cleanup,allowed=['coverage','browser','seat','children','socket','worker','native','fatal'];
    if(!value||typeof value!=='object'||Array.isArray(value)||Object.keys(value).some(key=>!['failures','childrenStopped','socketRemoved'].includes(key))||(Object.hasOwn(value,'childrenStopped')&&value.childrenStopped!==true)||(Object.hasOwn(value,'socketRemoved')&&value.socketRemoved!==true)||!Array.isArray(value.failures)||value.failures.some(name=>!allowed.includes(name))||new Set(value.failures).size!==value.failures.length)return {ok:false};
  }
  const ids=new Set(),names=new Set();
  for(const item of manifest.assertions) {
    if(!item||Object.keys(item).sort().join(',')!=='id,result'||!assertionID(item.id)||item.result!=='pass'||ids.has(item.id))return {ok:false};
    ids.add(item.id);
  }
  for(const name of manifest.artifacts) {
    if(typeof name!=='string'||!(/^[a-z0-9][a-z0-9-]*\.png$/.test(name)||name==='browser-coverage.json')||names.has(name))return {ok:false};
    names.add(name);
  }
  return {ok:true,scope:manifest.scope,result:manifest.result,acceptance:false};
}
module.exports = {compare, readFlat, png, audit, auditDiagnostic, bindRoot, runCLI, supported};

if (require.main === module) process.exitCode = runCLI(process.argv.slice(2), text => process.stdout.write(text));
