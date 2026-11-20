import test from 'node:test';
import assert from 'node:assert/strict';
import {loadPeopleLocalInventory as load} from '../js/people-local-review.js';

const key = ['1'.repeat(32), '2'.repeat(32), '3'.repeat(16), '4'.repeat(16)].join('/');
const copyInstance = 'a'.repeat(32);
const row = (kind, id = '5'.repeat(16)) => ({kind, id, revision:'6'.repeat(64)});
const args = api => ({api, key, copyInstance, current:()=>true});

test('follows empty pages and returns validated records through local GET only', async () => {
  const calls = [], signal = new AbortController().signal;
  const pages = [{entries:[],next_cursor:'p'+'1'.repeat(16)}, {entries:[row('pending')],next_cursor:'s'+'1'.repeat(16)}, {entries:[row('stage')]}];
  const result = await load({...args(async (path, options)=>{calls.push({path,options}); return pages.shift();}), signal});
  assert.deepEqual(result, [row('pending'), row('stage')]);
  assert.equal(calls.length, 3);
  assert.equal(calls[0].path, `/api/team/subscriptions/${key}/local?copy_instance=${copyInstance}`);
  assert.equal(calls[1].path, calls[0].path + '&cursor=p'+'1'.repeat(16));
  assert.equal(calls[2].path, calls[0].path + '&cursor=s'+'1'.repeat(16));
  for (const call of calls) { assert.equal(call.options.method,'GET'); assert.equal(call.options.signal,signal); }
});

test('rejects invalid inputs before dispatch', async () => {
  let calls = 0; const base = args(async()=>{calls++; return {entries:[]};});
  for (const change of [{key:''},{key:'A'.repeat(32) + key.slice(32)},{key:null},{copyInstance:''},{copyInstance:'A'.repeat(32)},{copyInstance:null},{api:null},{current:null}])
    await assert.rejects(load({...base,...change}));
  assert.equal(calls,0);
});

test('rejects malformed pages and duplicate identities', async () => {
  const bad = [null, {}, {entries:null}, {entries:[null]}, {entries:[row('other')]}, {entries:[{...row('stage'),id:'X'}]}, {entries:[{...row('stage'),revision:'X'}]}, {entries:[row('stage'),row('stage')]}, {entries:[],next_cursor:'bad'}, {entries:[],next_cursor:null}, {entries:Array.from({length:9},(_,i)=>row('stage',i.toString(16).padStart(16,'0')))}];
  for (const page of bad) await assert.rejects(load(args(async()=>page)));
  for (const cursor of ['p'+'1'.repeat(16), 'p'+'0'.repeat(16)]) {
    let n=0; await assert.rejects(load(args(async()=>++n===1?{entries:[],next_cursor:'p'+'1'.repeat(16)}:{entries:[],next_cursor:cursor})));
  }
});

test('bounds traversal and preserves API errors', async () => {
  let count=0;
  await assert.rejects(load(args(async()=>({entries:[],next_cursor:'p'+(++count).toString(16).padStart(16,'0')}))));
  assert.equal(count,1024);
  const failure = Error('synthetic failure');
  await assert.rejects(load(args(async()=>{throw failure;})), e=>e===failure);
});

test('cancels before and after await without exposing partial records', async () => {
  let calls=0, active=false;
  await assert.rejects(load({...args(async()=>{calls++;}),current:()=>active}));
  assert.equal(calls,0);
  active=true;
  await assert.rejects(load({...args(async()=>{calls++; active=false; return {entries:[row('pending')],next_cursor:'p'+'1'.repeat(16)};}),current:()=>active}));
  assert.equal(calls,1);
  const controller = new AbortController(); controller.abort(); calls=0;
  await assert.rejects(load({...args(async()=>{calls++;}),signal:controller.signal}));
  assert.equal(calls,0);
  const later = new AbortController();
  await assert.rejects(load({...args(async()=>{calls++; later.abort(); return {entries:[row('pending')]};}),signal:later.signal}));
  assert.equal(calls,1);
});

test('checks current again before a later dispatch and after rejected requests', async () => {
  let checks=0, calls=0;
  await assert.rejects(load({...args(async()=>{calls++; return {entries:[row('pending')],next_cursor:'p'+'1'.repeat(16)};}),current:()=>++checks < 3}), /cancelled/);
  assert.equal(calls,1);
  let active=true;
  await assert.rejects(load({...args(async()=>{active=false; throw Error('network');}),current:()=>active}), /cancelled/);
});

test('allows the maximum bounded envelope and captures input values', async () => {
  let count=0; const input=args(async(path)=>{
    count++;
    assert.ok(path.startsWith(`/api/team/subscriptions/${key}/local?copy_instance=${copyInstance}`));
    input.key='changed'; input.copyInstance='changed';
    return {entries:Array.from({length:8},(_,i)=>row('stage',((count-1)*8+i).toString(16).padStart(16,'0'))), ...(count < 1024 ? {next_cursor:'s'+count.toString(16).padStart(16,'0')} : {})};
  });
  assert.equal((await load(input)).length,8192);
  assert.equal(count,1024);
});

test('discard uses the exact reviewed record and validates truthful outcomes', async () => {
  const mod=await import('../js/people-local-review.js'), discard=mod.discardPeopleLocalPending || (async()=>null);const calls=[];
  const record=row('pending'); const base={...args(async(path,opts)=>{calls.push({path,opts});return {state:'locally_discarded'};}),record};
  assert.deepEqual(await discard(base),{state:'locally_discarded'});
  assert.deepEqual(calls,[{path:`/api/team/subscriptions/${key}/pending/${record.id}?copy_instance=${copyInstance}&revision=${record.revision}`,opts:{method:'DELETE'}}]);
  assert.deepEqual(await discard({...base,api:async()=>({state:'already_completed',seq:42})}),{state:'already_completed',seq:42});
  for (const result of [null,{}, {state:'other'}, {state:'locally_discarded',seq:4}, {state:'already_completed'}, {state:'already_completed',seq:0}, {state:'already_completed',seq:1.5}, {state:'already_completed',seq:Number.MAX_SAFE_INTEGER+1}]) await assert.rejects(discard({...base,api:async()=>result}));
});

test('discard refuses invalid identity, record and stale work before dispatch', async () => {
  const mod=await import('../js/people-local-review.js'), discard=mod.discardPeopleLocalPending || (async()=>null);let calls=0;const base={...args(async()=>{calls++;return {state:'locally_discarded'};}),record:row('pending')};
  for (const change of [{api:null},{current:null},{key:null},{key:'bad'},{copyInstance:null},{copyInstance:'bad'},{record:null},{record:row('stage')},{record:{...row('pending'),id:null}},{record:{...row('pending'),id:'bad'}},{record:{...row('pending'),revision:null}},{record:{...row('pending'),revision:'bad'}},{current:()=>false}]) await assert.rejects(discard({...base,...change}));
  assert.equal(calls,0);
});

test('discard fences delayed success and failure and preserves current API failure', async () => {
  const mod=await import('../js/people-local-review.js'), discard=mod.discardPeopleLocalPending || (async()=>null);let active=true;const base={...args(async()=>({state:'locally_discarded'})),current:()=>active,record:row('pending')};
  await assert.rejects(discard({...base,api:async()=>{active=false;return {state:'locally_discarded'};}}));active=true;
  await assert.rejects(discard({...base,api:async()=>{active=false;throw Error('synthetic');}}));active=true;
  const failure=Error('offline');await assert.rejects(discard({...base,api:async()=>{throw failure;}}),error=>error===failure);
});
