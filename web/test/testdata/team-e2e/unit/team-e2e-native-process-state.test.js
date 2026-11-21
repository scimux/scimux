import test from 'node:test';
import assert from 'node:assert/strict';
import {nativeProcessState} from '../native-process-state.cjs';

for(const [state,expected] of [['Z','zombie'],['S','live'],['R','live']]) test(`owned synthetic native ${state} state is classified`,()=>{
 const result=nativeProcessState(12,{probe:()=>{},read:()=>`12 (synthetic) ${state} 1 2 3`});
 assert.equal(result,expected);
});

test('missing synthetic pid is gone and uninspectable state remains uncertain',()=>{
 assert.equal(nativeProcessState(12,{probe:()=>{const e=Error('synthetic');e.code='ESRCH';throw e;},read:()=>''}),'gone');
 assert.equal(nativeProcessState(12,{probe:()=>{},read:()=>{throw Error('synthetic');}}),'unknown');
 assert.equal(nativeProcessState(12,{probe:()=>{},read:()=> 'malformed'}),'unknown');
});

test('native summary distinguishes pending zombies from live or uncertain helpers',async()=>{
 const {summarizeNative}=await import('../native-process-state.cjs');
 const states={1:'zombie',2:'live',3:'gone',4:'unknown'};
 assert.deepEqual(summarizeNative([1,2,3,4],pid=>states[pid]),{active:2,zombies:1});
});

test('native inspection handles invalid pid, unexpected probe errors and invalid states',()=>{
 assert.equal(nativeProcessState(0),'unknown');
 assert.equal(nativeProcessState(12,{probe:()=>{const e=Error('synthetic');e.code='EPERM';throw e;}}),'unknown');
 assert.equal(nativeProcessState(12,{probe:()=>{},read:()=> '12 (synthetic) ? 1'}),'unknown');
 assert.equal(nativeProcessState(12,{probe:()=>{},read:()=> '13 (synthetic) S 1'}),'unknown');
 assert.equal(nativeProcessState(process.pid),process.platform==='linux'?'live':'unknown');
});
