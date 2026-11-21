'use strict';
import test from 'node:test';
import assert from 'node:assert/strict';
import {retire} from './tmux-cleanup.cjs';

test('a failed kill cannot discard a live private server control socket', async () => {
  let exists = true, unlinks = 0;
  const ops = {exists:()=>exists, kill:async()=>{throw Error('synthetic kill failure');},
    probe:async()=> 'live', unlink:()=>{exists=false;unlinks++;}};
  const result = await retire('/owned/private/socket',ops);
  assert.equal(result.removed,false);
  assert.equal(exists,true);
  assert.equal(unlinks,0);
  assert.match(result.error,/kill failure/);
});


test('only a confirmed dead private server socket is removed', async () => {
  let exists=true;
  const result=await retire('/owned/private/socket',{exists:()=>exists,kill:async()=>{},probe:async()=> 'dead',unlink:()=>{exists=false;}});
  assert.equal(result.removed,true);
  assert.equal(exists,false);
});

test('an indeterminate probe retains control and reports failure', async () => {
  let unlinked=false;
  const result=await retire('/owned/private/socket',{exists:()=>true,kill:async()=>{},probe:async()=> 'unknown',unlink:()=>{unlinked=true;}});
  assert.equal(result.removed,false);
  assert.equal(unlinked,false);
  assert.match(result.error,/unknown/);
});

test('a failed redundant kill is harmless after the private socket is proved dead',async()=>{
  let exists=true;
  const result=await retire('/synthetic/socket',{exists:()=>exists,kill:async()=>{throw Error('synthetic');},probe:async()=> 'dead',unlink:()=>{exists=false;}});
  assert.deepEqual(result,{removed:true});assert.equal(exists,false);
});

test('absent socket needs no kill or probe',async()=>{
  let called=false;const result=await retire('/synthetic/socket',{exists:()=>false,kill:async()=>{called=true;},probe:async()=>{called=true;},unlink:()=>{called=true;}});
  assert.deepEqual(result,{removed:true});assert.equal(called,false);
});

test('probe exception retains the socket',async()=>{
  let unlinked=false;const result=await retire('/synthetic/socket',{exists:()=>true,kill:async()=>{},probe:async()=>{throw Error('synthetic');},unlink:()=>{unlinked=true;}});
  assert.equal(result.removed,false);assert.match(result.error,/probe failed/);assert.equal(unlinked,false);
});

for(const code of ['ENOENT','EIO']) test(`socket unlink ${code} is reported truthfully`,async()=>{
  let exists=true;const result=await retire('/synthetic/socket',{exists:()=>exists,kill:async()=>{},probe:async()=> 'dead',unlink:()=>{if(code==='ENOENT')exists=false;const error=Error('synthetic');error.code=code;throw error;}});
  assert.equal(result.removed,code==='ENOENT');if(code==='EIO')assert.match(result.error,/unlink failed/);
});

test('socket that remains live for all retries is retained',async()=>{
  const timer=globalThis.setTimeout;let probes=0,unlinked=false;
  globalThis.setTimeout=callback=>{queueMicrotask(callback);return 0;};
  try {
    const result=await retire('/synthetic/socket',{exists:()=>true,kill:async()=>{},probe:async()=>{probes++;return 'live';},unlink:()=>{unlinked=true;}});
    assert.equal(result.removed,false);assert.match(result.error,/remains reachable/);assert.equal(probes,30);assert.equal(unlinked,false);
  }finally{globalThis.setTimeout=timer;}
});
