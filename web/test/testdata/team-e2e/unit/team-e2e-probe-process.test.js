import test from 'node:test';
import assert from 'node:assert/strict';
import {EventEmitter} from 'node:events';
import {bounded,track,stopOwned,waitForMarker} from '../probe-process.cjs';

function childFixture() {
  const child=new EventEmitter(),signals=[],input=[];
  child.exitCode=null;child.signalCode=null;child.stdout=new EventEmitter();child.stdin=new EventEmitter();child.stdin.writable=true;
  child.stdin.end=value=>input.push(value);child.kill=signal=>{signals.push(signal);return true;};
  return {child,signals,input};
}
async function fakeTimers(fn) {
  const originalSet=globalThis.setTimeout,originalClear=globalThis.clearTimeout,pending=new Map();let next=0;
  globalThis.setTimeout=(callback,ms)=>{const id=++next;pending.set(id,{callback,ms});return id;};
  globalThis.clearTimeout=id=>pending.delete(id);
  try {await fn({pending,fire:ms=>{const entry=[...pending].find(([,value])=>value.ms===ms);assert.ok(entry,`missing timer ${ms}`);pending.delete(entry[0]);entry[1].callback();}});}
  finally {globalThis.setTimeout=originalSet;globalThis.clearTimeout=originalClear;}
}

test('bounded wait returns its result and cancels its timer',async()=>fakeTimers(async({pending})=>{
  assert.equal(await bounded(Promise.resolve('synthetic'),25,'timeout'),'synthetic');assert.equal(pending.size,0);
}));

test('bounded wait propagates rejection and timeout with no remaining timer',async()=>fakeTimers(async({pending,fire})=>{
  await assert.rejects(bounded(Promise.reject(Error('synthetic')),25,'timeout'),/synthetic/);assert.equal(pending.size,0);
  const waiting=bounded(new Promise(()=>{}),30,'synthetic timeout');fire(30);await assert.rejects(waiting,/synthetic timeout/);assert.equal(pending.size,0);
}));

test('child tracking waits for close after spawn error and records it',async()=>{
  const {child}=childFixture(),failure=Error('synthetic spawn error');let settled=false;
  const closed=track(child).then(value=>{settled=true;return value;});child.stdin.emit('error',Error('synthetic stdin'));
  child.emit('error',failure);await Promise.resolve();assert.equal(settled,false);
  child.emit('close',null,'SIGTERM');assert.deepEqual(await closed,{code:null,signal:'SIGTERM',spawnError:failure});
});

test('stopping an already exited child only waits for its close receipt',async()=>{
  const {child,signals,input}=childFixture();child.exitCode=7;
  const outcome={code:7,signal:null};assert.deepEqual(await stopOwned(child,Promise.resolve(outcome)),outcome);
  assert.deepEqual(signals,[]);assert.deepEqual(input,[]);
});

test('stopping a live child releases stdin and waits for close after TERM',async()=>{
  const {child,signals,input}=childFixture();let release;const closed=new Promise(resolve=>{release=resolve;});
  const result=stopOwned(child,closed);assert.deepEqual(input,['release']);assert.deepEqual(signals,['SIGTERM']);
  release({code:0,signal:null});assert.deepEqual(await result,{code:0,signal:null});
});

test('stopping a child escalates after timeout and still waits for close',async()=>fakeTimers(async({fire,pending})=>{
  const {child,signals}=childFixture();let release;const closed=new Promise(resolve=>{release=resolve;});
  const result=stopOwned(child,closed);fire(220000);await new Promise(resolve=>setImmediate(resolve));
  assert.deepEqual(signals,['SIGTERM','SIGKILL']);release({code:null,signal:'SIGKILL'});
  assert.deepEqual(await result,{code:null,signal:'SIGKILL'});assert.equal(pending.size,0);
}));

test('unconfirmed close after escalation remains an error',async()=>fakeTimers(async({fire,pending})=>{
  const {child,signals}=childFixture();const result=stopOwned(child,new Promise(()=>{}));
  fire(220000);await new Promise(resolve=>setImmediate(resolve));fire(5000);
  await assert.rejects(result,/after kill/);assert.deepEqual(signals,['SIGTERM','SIGKILL']);assert.equal(pending.size,0);
}));

test('a child that exits before timeout does not receive KILL',async()=>fakeTimers(async({fire,pending})=>{
  const {child,signals}=childFixture();const result=stopOwned(child,new Promise(()=>{}));child.exitCode=1;
  fire(220000);await new Promise(resolve=>setImmediate(resolve));fire(5000);
  await assert.rejects(result,/after kill/);assert.deepEqual(signals,['SIGTERM']);assert.equal(pending.size,0);
}));

test('signal delivery exception is not treated as a close receipt',async()=>{
  const {child}=childFixture();child.kill=()=>{throw Error('synthetic signal failure');};
  await assert.rejects(stopOwned(child,new Promise(()=>{})),/synthetic signal failure/);
});

test('a rejected close wait cannot be mistaken for a successful receipt',async()=>{
  const {child,signals,input}=childFixture();child.stdin.writable=false;
  await assert.rejects(stopOwned(child,Promise.reject(Error('synthetic close uncertainty'))),/synthetic close uncertainty/);
  assert.deepEqual(signals,['SIGTERM','SIGKILL']);assert.deepEqual(input,[]);
});

test('readiness resolves immediately only after the required repeated markers',async()=>{
  const {child}=childFixture();assert.equal(await waitForMarker(child,()=> 'ready ready','ready',2),undefined);
  assert.equal(child.stdout.listenerCount('data'),0);
});

test('readiness tracks repeated markers and disposes listeners on success',async()=>fakeTimers(async({pending})=>{
  const {child}=childFixture();let output='';const waiting=waitForMarker(child,()=>output,'ready',2);
  output='ready';child.stdout.emit('data');output+=' ready';child.stdout.emit('data');await waiting;
  assert.equal(pending.size,0);assert.equal(child.stdout.listenerCount('data'),0);assert.equal(child.listenerCount('exit'),0);assert.equal(child.listenerCount('error'),0);
}));

for(const failure of ['already-exited','exit','error','timeout']) test(`readiness reports ${failure} and removes its listeners`,async()=>fakeTimers(async({fire,pending})=>{
  const {child}=childFixture();if(failure==='already-exited')child.exitCode=1;
  const waiting=waitForMarker(child,()=>'', 'ready');
  if(failure==='exit')child.emit('exit',1,null);
  if(failure==='error')child.emit('error',Error('synthetic spawn error'));
  if(failure==='timeout')fire(60000);
  await assert.rejects(waiting,/runner (exited|could not start|did not reach)/);
  assert.equal(pending.size,0);assert.equal(child.stdout.listenerCount('data'),0);assert.equal(child.listenerCount('exit'),0);assert.equal(child.listenerCount('error'),0);
}));
