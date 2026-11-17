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
