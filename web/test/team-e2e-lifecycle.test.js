import test from 'node:test';
import assert from 'node:assert/strict';
import {createRequire} from 'node:module';
const {retireCompletedContexts}=createRequire(import.meta.url)('./testdata/team-e2e/browser-lifecycle.cjs');

test('completed browser contexts retire only after their coverage is captured', async () => {
 const events=[],pages=[{id:'a'},{id:'b'}],contexts=[{async close(){events.push('close a')}},{async close(){events.push('close b')}}];
 await retireCompletedContexts(pages,contexts,async()=>{assert.equal(pages.length,2);events.push('coverage')});
 assert.deepEqual(events,['coverage','close a','close b']);assert.equal(pages.length,0);assert.equal(contexts.length,0);
});

test('coverage failure keeps completed browser contexts available for cleanup', async () => {
 const pages=[{}],contexts=[{async close(){assert.fail('must not close before coverage')}}];
 await assert.rejects(retireCompletedContexts(pages,contexts,async()=>{throw Error('synthetic coverage failure')}),/synthetic coverage failure/);
 assert.equal(pages.length,1);assert.equal(contexts.length,1);
});
