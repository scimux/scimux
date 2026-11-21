import test from 'node:test';
import assert from 'node:assert/strict';
import {createRequire} from 'node:module';
const {createPageCapture}=createRequire(import.meta.url)('./testdata/team-e2e/capture-once.cjs');

test('overlapping page capture waits for one actual capture',async()=>{
 let release,calls=0;const page={},gate=new Promise(resolve=>release=resolve);
 const capture=createPageCapture(async()=>{calls++;await gate});const a=capture(page),b=capture(page);await Promise.resolve();assert.equal(calls,1);release();await Promise.all([a,b]);await capture(page);assert.equal(calls,1);
});
test('capture failures remain visible to every caller',async()=>{
 let calls=0;const page={},capture=createPageCapture(()=>{calls++;throw Error('synthetic capture failure')});
 await assert.rejects(capture(page),/synthetic capture failure/);await assert.rejects(capture(page),/synthetic capture failure/);assert.equal(calls,1);
});
