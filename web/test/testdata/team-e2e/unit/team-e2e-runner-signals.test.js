import test from 'node:test';
import assert from 'node:assert/strict';
import {EventEmitter} from 'node:events';
import {installRunnerSignals} from '../runner-signals.cjs';

for(const [first,second,code] of [['SIGINT','SIGINT',130],['SIGINT','SIGTERM',130],['SIGTERM','SIGINT',143]])test(`runner holds ${first} intent across ${second} during cleanup`,async()=>{
  const events=new EventEmitter(),manifest={result:'pass'},exits=[],messages=[];let release,count=0;
  const gate=new Promise(resolve=>{release=resolve});
  const installed=installRunnerSignals({events,manifest,cleanup:async()=>{count++;await gate;},record:()=>{},exit:code=>exits.push(code),notify:value=>messages.push(value)});
  events.emit(first);events.emit(second);await new Promise(resolve=>setImmediate(resolve));
  try {assert.deepEqual(exits,[]);assert.equal(installed.current(),first);assert.equal(manifest.signal,first);assert.equal(count,1);}
  finally {release();}
  await new Promise(resolve=>setImmediate(resolve));
  assert.deepEqual(exits,[code]);assert.equal(manifest.result,'interrupted');installed.dispose();
});

test('a controlled failure remains a failure if interrupted during cleanup',async()=>{
  const events=new EventEmitter(),manifest={result:'fail'},exits=[];
  const installed=installRunnerSignals({events,manifest,cleanup:async()=>{},record:()=>{},exit:code=>exits.push(code),notify:()=>{}});
  events.emit('SIGINT');await new Promise(resolve=>setImmediate(resolve));
  assert.equal(manifest.result,'fail');assert.equal(manifest.signal,'SIGINT');assert.deepEqual(exits,[1]);installed.dispose();
});

test('cleanup rejection produces a fixed failure outcome',async()=>{
  const events=new EventEmitter(),manifest={result:'pass'},exits=[],messages=[];
  const installed=installRunnerSignals({events,manifest,cleanup:async()=>{throw Error('synthetic private detail');},record:()=>{},exit:code=>exits.push(code),notify:value=>messages.push(value)});
  events.emit('SIGTERM');await new Promise(resolve=>setImmediate(resolve));
  assert.equal(manifest.result,'fail');assert.deepEqual(exits,[1]);assert.equal(JSON.stringify(messages).includes('synthetic private detail'),false);installed.dispose();
});

test('cleanup failure remains reported when recording the failure also throws',async()=>{
  const events=new EventEmitter(),manifest={result:'pass'},exits=[],messages=[];
  const installed=installRunnerSignals({events,manifest,cleanup:async()=>{throw Error('synthetic cleanup');},record:()=>{throw Error('synthetic record');},exit:code=>exits.push(code),notify:value=>messages.push(value)});
  events.emit('SIGTERM');await new Promise(resolve=>setImmediate(resolve));
  assert.equal(manifest.result,'fail');assert.deepEqual(exits,[1]);assert.deepEqual(messages,['received','cleanup-failed']);installed.dispose();
});

test('a scenario completion cannot overwrite an earlier signal or failure',async()=>{
  const {finishScenario}=await import('../runner-signals.cjs');
  for(const [before,signal,expected] of [['running',null,'pass'],['interrupted','SIGINT','interrupted'],['fail',null,'fail']]) {
    const manifest={result:before};finishScenario(manifest,signal);assert.equal(manifest.result,expected);
  }
});

test('controlled failure classification uses only the deliberately thrown object identity',async()=>{
 const mod=await import('../runner-signals.cjs');
 assert.equal(typeof mod.controlledFailureCode,'function');
 const deliberate=Error('synthetic');
 assert.equal(mod.controlledFailureCode(deliberate,deliberate),'controlled-native-cleanup');
 assert.equal(mod.controlledFailureCode(Error('synthetic'),deliberate),undefined);
 assert.equal(mod.controlledFailureCode('synthetic',deliberate),undefined);
 assert.equal(mod.controlledFailureCode(undefined,undefined),undefined);
 assert.equal(mod.controlledFailureCode('synthetic','synthetic'),undefined);
});
