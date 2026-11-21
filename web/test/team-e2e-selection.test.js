import test from 'node:test';
import assert from 'node:assert/strict';
import {createRequire} from 'node:module';
const {selectSharedSubscription}=createRequire(import.meta.url)('./testdata/team-e2e/share-select.cjs');

test('browser fixture selects the exact share among the same owner subscriptions', () => {
 const older={server:'relay',owner:'owner',chat:'other',generation:'old'};
 const wrongRelay={server:'other-relay',owner:'owner',chat:'target',generation:'current'};
 const stale={server:'relay',owner:'owner',chat:'target',generation:'old'};
 const wanted={server:'relay',owner:'owner',chat:'target',generation:'current'};
 assert.equal(selectSharedSubscription([older,wrongRelay,stale,wanted],wanted),wanted);
 assert.equal(selectSharedSubscription([older,wrongRelay,stale],wanted),undefined);
});
