'use strict';
// Synthetic source-defined audit example, independent of recorded runs.
function synthetic(profile) {
  const m = {version: 1, result: 'pass', assertions: [
    {id: 'comments-zero-agent-attempts', result: 'pass'},
    {id: 'native-exact-one-prompt', result: 'pass'},
    {id: 'native-authoritative-response', result: 'pass'},
  ], artifacts: ['screen.png'], counters: {}};
  const z = () => ({attempted: 0, accepted: 0, controls: 0});
  const counts = n => ({attempted: n, accepted: n, controls: 0});
  const seats = () => ({'owner-a': z(), 'owner-b': z(), recipient: z(), observer: z()});
  const ordinary = ['boundary', 'leave', 'permission-ended-debug', 'permission', 'reshare',
    'coordinator-header-focus', 'coordinator-description-save', 'description-recreated-choice',
    'description-choice-order', 'received-offline', 'stage2-pages', 'draft-recovery',
    'delete-recreation', 'legacy-closed'];
  const intentional = ['native-trace-debug', 'native-trace', 'ai-source', 'held-evidence',
    'dropped-send', 'stage1-scanner', 'stage1-owner-files', 'no-selection'];
  const discussion = ['people-uncertain-retry', 'people-upload-permission', 'people-midbody-permission',
    'people-image-bookmark', 'people-quota', 'people-local-budget', 'people-file-unread',
    'people-recovery', 'people-relay-missing', 'people-relay-corrupt', 'people-unavailable',
    'people-retired-draft', 'people-copy-replacement', 'people-stale-preparation', 'people-real-tunnel'];
  const local = ['owner-a', 'owner-b', 'recipient', 'observer', 'empty-desktop', 'empty-phone',
    'legacy-closed-desktop', 'legacy-closed-phone', 'no-selection'];
  for (const mode of ['desktop', 'phone']) {
    const required = ['early-native-visible-no-agent-dispatch', 'no-selection-explicit-send-only', 'stage1-scanner-explicit-dispatch-only', 'stage1-shared-owner-idle', 'stage1-discussion-zero-agent-dispatch', 'stage1-owner-file-delivered', 'stage2-fork-fresh-local', 'stage2-prompt-one-dispatch', 'dropped-response-explicit-retry-once', 'held-evidence-confirmed-after-release', 'ai-source-explicit-exact-mirrored', 'trace-explicit-one-dispatch', 'trace-second-explicit-dispatch', 'native-trace-one-exact-multiline'];
    if (profile === 'people-attachments') required.push('people-image-prepare-no-dispatch', 'people-quota-no-implicit-dispatch', 'people-copy-replacement-zero-agent-dispatch', 'people-files-no-implicit-dispatch', 'people-owner-prepare-no-dispatch', 'people-image-no-implicit-dispatch', 'people-retained-prepare-no-dispatch', 'people-stale-source-no-dispatch', 'people-retained-missing-no-dispatch', 'people-retired-review-no-dispatch', 'people-recovery-zero-agent-dispatch', 'people-relay-missing-zero-dispatch', 'people-relay-corrupt-zero-dispatch', 'people-cross-channel-zero-controls', 'people-real-tunnel-no-agent-dispatch', 'people-file-unread-no-agent-dispatch', 'people-local-budget-zero-agent', 'people-uncertain-no-agent-dispatch', 'people-held-upload-no-agent-dispatch', 'people-midbody-no-agent-dispatch', 'people-image-bookmark-no-agent-dispatch', 'legacy-reader-zero-implicit-dispatch', 'old-relay-no-blob-put', 'people-source-cancel-keeps-unrelated-file', 'people-retained-file-explicit-delivery');
    for (const id of required) m.assertions.push({id: id+'-'+mode, result: 'pass'});
    for (const value of ['false', 'true']) m.assertions.push({id: 'refusal-no-retry-'+mode+'-'+value, result: 'pass'});
    m['native-start-failure-'+mode] = {counters: z()};
    for (const key of ['stale-membership', 'stale-removed-membership', 'early-native']) m[key+'-'+mode] = {before: seats(), after: seats()};
    for (const key of ordinary) m[key+'-'+mode] = {before: z(), after: z()};
    for (const key of intentional) m[key+'-'+mode] = {before: z(), after: counts(1)};
    for (const newer of ['false', 'true']) m['refusal-'+mode+'-'+newer] = {before: z(), after: {attempted: 1, accepted: 0, controls: 0}};
    for (const name of ['membership', 'activities', 'recovery']) m.counters[name+'-'+mode] = {before: seats(), after: seats()};
    m['stage1-scanner-owner-'+mode] = {beforeOwner: z(), afterOwner: counts(1)};
    m['stage1-unread-'+mode] = {beforeOwner: z(), afterOwner: z(), beforeReader: z(), afterReader: z()};
    m['stage2-shared-'+mode] = {beforeOwner: z(), afterOwner: counts(1), beforeReader: z(), afterReader: counts(1), beforeOther: z(), afterOther: z()};
    m['trace-address-'+mode] = {beforeSend: z(), afterSend: counts(1)};
    m['trace-choices-'+mode] = {beforeSecond: z(), secondCounts: counts(1)};
    m['dropped-send-'+mode].final = counts(2);
    m['held-evidence-'+mode].during = counts(1);
    for (const pending of ['false', 'true']) m['recreated-'+mode+'-'+pending] = {afterCreate: z(), after: z()};
    if (profile === 'people-attachments') {
      for (const key of discussion) m[key+'-'+mode] = {before: {owner: z(), recipient: z()}, after: {owner: z(), recipient: z()}};
      m['people-files-'+mode] = {before: {owner: z(), recipient: z()}, after: {owner: counts(3), recipient: z()}};
      m['people-source-cancel-'+mode] = {before: {owner: z(), recipient: z()}, after: {owner: counts(1), recipient: z()}};
      m['people-cross-channel-'+mode] = {before: {first: z(), second: z(), recipient: z()}, after: {first: z(), second: z(), recipient: z()}};
      m['legacy-reader-'+mode] = {before: {owner: z(), legacy: z()}, after: {owner: z(), legacy: z()}};
      m['old-relay-'+mode] = {before: z(), after: z()};
      local.push('legacy-reader-'+mode, 'old-owner-'+mode, 'old-reader-'+mode);
    }
  }
  m.counters.beforeComments = seats(); m.counters.afterComments = seats(); m.counters.afterExtraSetup = seats();
  m.counters.afterNative = Object.fromEntries(local.map(name => [name, z()]));
  return m;
}

module.exports={synthetic};
