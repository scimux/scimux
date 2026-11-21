import test from 'node:test';
import assert from 'node:assert/strict';
import {createRequire} from 'node:module';
const {createCleanupOnce}=createRequire(import.meta.url)('./testdata/team-e2e/cleanup-once.cjs');

test('overlapping cleanup callers share the same unfinished teardown', async()=>{
 let release,calls=0,finished=false;const gate=new Promise(resolve=>release=resolve);
 const cleanup=createCleanupOnce(async()=>{calls++;await gate;finished=true});
 const first=cleanup(),second=cleanup();let secondFinished=false;second.then(()=>{secondFinished=true});await new Promise(resolve=>setImmediate(resolve));assert.equal(secondFinished,false,'second caller must wait for teardown');assert.equal(first,second);assert.equal(calls,1);assert.equal(finished,false);
 release();await Promise.all([first,second]);assert.equal(finished,true);assert.equal(cleanup(),first);assert.equal(calls,1);
});

test('cleanup failure is shared and is never silently retried',async()=>{
 let calls=0;const cleanup=createCleanupOnce(()=>{calls++;throw Error('synthetic teardown failure')});const first=cleanup(),second=cleanup();
 assert.equal(first,second);await assert.rejects(first,/synthetic teardown failure/);await assert.rejects(second,/synthetic teardown failure/);assert.equal(cleanup(),first);assert.equal(calls,1);
});
