import test from 'node:test';
import assert from 'node:assert/strict';
import * as review from '../js/people-local-review.js';

// Missing exports act as no-ops so the initial failure exercises behavior.
const read = review.readPeopleLocalPublication || (async () => undefined);
const retire = review.retireAbsentPeopleLocalPublication || (async () => undefined);
const key = ['1'.repeat(32), '2'.repeat(32), '3'.repeat(16), '4'.repeat(16)].join('/');
const copyInstance = 'a'.repeat(32), publicationKey = 'b'.repeat(16);
const revision = 'c'.repeat(64), stage = 'd'.repeat(16);
const base = api => ({api, key, copyInstance, publicationKey, current:()=>true});
const full = state => ({state, revision, stage_ids:[stage]});
const path = `/api/team/subscriptions/${key}/pending/${publicationKey}`;

test('read projects all actual states and sends one exact safe GET', async () => {
  for (const value of [{state:'missing'}, {state:'absent_retired'}, full('pending'), {...full('retired'),text:'hello'}, {...full('completed'),seq:2}, {...full('completed'),text:'',seq:Number.MAX_SAFE_INTEGER,pending_pinned:true}]) {
    const calls=[], signal=new AbortController().signal;
    const result=await read({...base(async (...a)=>{calls.push(a);return value;}),signal});
    assert.deepEqual(result, {...value, ...(value.revision && value.text === undefined ? {text:''} : {})});
    assert.deepEqual(calls, [[`${path}?copy_instance=${copyInstance}`,{method:'GET',signal}]]);
    if (value.stage_ids) { assert.notEqual(result.stage_ids,value.stage_ids); value.stage_ids.push('e'.repeat(16)); assert.equal(result.stage_ids.length,1); }
  }
});

test('read rejects malformed and contradictory state projections', async () => {
  const bad = [null, [], {}, {state:'unknown'}, {state:'missing',extra:'unexpected'}, {...full('pending'),extra:'unexpected'}, {state:'missing',revision}, {state:'absent_retired',seq:1}, {state:'missing',text:''}, {state:'missing',stage_ids:[]}, {state:'missing',pending_pinned:true},
    {state:'pending'}, {...full('pending'),revision:null}, {...full('pending'),revision:'A'.repeat(64)}, {...full('pending'),stage_ids:null}, {...full('pending'),stage_ids:[]}, {...full('pending'),stage_ids:[stage,stage]}, {...full('pending'),stage_ids:Array(17).fill(stage)}, {...full('pending'),stage_ids:['A'.repeat(16)]}, {...full('pending'),stage_ids:[3]},
    {...full('pending'),text:null}, {...full('pending'),text:'é'.repeat(8193)}, {...full('pending'),seq:1}, {...full('retired'),pending_pinned:true}, {...full('pending'),pending_pinned:false},
    {...full('completed')}, ...[0,-1,1.5,Number.MAX_SAFE_INTEGER+1,'1',null].map(seq=>({...full('completed'),seq})), ...[null, 1, 'true'].map(pending_pinned=>({...full('completed'),seq:1,pending_pinned}))];
  for (const value of bad) await assert.rejects(read(base(async()=>value)), undefined, JSON.stringify(value));
  const maximum={...full('pending'),stage_ids:Array.from({length:16},(_,i)=>i.toString(16).padStart(16,'0'))};
  assert.deepEqual(await read(base(async()=>maximum)), {...maximum,text:''});
  const limit={...full('pending'),text:'é'.repeat(8192)};
  assert.equal((await read(base(async()=>limit))).text,limit.text);
});

test('retire sends bodyless POST and accepts only truthful success', async () => {
  const calls=[], signal=new AbortController().signal;
  assert.deepEqual(await retire({...base(async (...a)=>{calls.push(a);return {state:'locally_discarded'};}),signal}), {state:'locally_discarded'});
  assert.deepEqual(calls, [[`${path}/retire-absent?copy_instance=${copyInstance}`,{method:'POST',signal}]]);
  for (const value of [null,{}, {state:'missing'}, {state:'locally_discarded',seq:1}, {state:'locally_discarded',revision}, {state:'locally_discarded',extra:1}, {state:'locally_discarded',pending_pinned:true}]) await assert.rejects(retire(base(async()=>value)));
});

test('both adapters reject invalid identity or missing authority before any request', async () => {
  for (const fn of [read,retire]) {
    let calls=0; const input=base(async()=>{calls++;});
    for (const change of [{api:null},{current:null},{key:null},{key:'bad'},{key:key.toUpperCase().replace('1','A')},{copyInstance:null},{copyInstance:'bad'},{publicationKey:null},{publicationKey:'bad'},{publicationKey:'B'.repeat(16)},{current:()=>false}]) await assert.rejects(fn({...input,...change}));
    const controller=new AbortController(); controller.abort(); await assert.rejects(fn({...input,signal:controller.signal}));
    assert.equal(calls,0);
  }
});

test('both adapters fence delayed successes and errors and preserve current failures', async () => {
  for (const [fn,value] of [[read,{state:'missing'}],[retire,{state:'locally_discarded'}]]) {
    for (const reject of [false,true]) for (const mode of ['current','abort']) {
      let active=true,calls=0; const controller=new AbortController();
      await assert.rejects(fn({...base(async()=>{calls++; if(mode==='current') active=false; else controller.abort(); if(reject) throw Error('network'); return value;}),current:()=>active,signal:controller.signal}), /cancelled/);
      assert.equal(calls,1);
    }
    const failure=Object.assign(Error('review_required'),{status:409,body:{error:'review_required'}}); let calls=0;
    await assert.rejects(fn(base(async()=>{calls++;throw failure;})),error=>error===failure && error.status===409 && error.body.error==='review_required');
    assert.equal(calls,1);
  }
});

test('both adapters capture caller identity and callbacks before dispatch', async () => {
  for (const [fn,value,suffix] of [[read,{state:'missing'},''],[retire,{state:'locally_discarded'},'/retire-absent']]) {
    const calls=[]; let resolve; const input=base((...a)=>{calls.push(a); return new Promise(r=>{resolve=r;});});
    const promise=fn(input); input.key='changed'; input.publicationKey='changed'; input.copyInstance='changed'; input.current=()=>false; input.api=()=>{throw Error('changed');};
    resolve(value); assert.deepEqual(await promise,value);
    assert.equal(calls.length,1); assert.equal(calls[0][0],`${path}${suffix}?copy_instance=${copyInstance}`);
  }
});

test('completed publication accepts an explicit false pending pin', async () => {
  const value={...full('completed'),seq:7,pending_pinned:false};
  assert.deepEqual(await read(base(async()=>value)), {...full('completed'),text:'',seq:7});
});
