import test from 'node:test';
import assert from 'node:assert/strict';
import {retire} from './tmux-cleanup.cjs';

test('owned live tmux stays controllable when teardown reports failure', async () => {
  const fs = await import('node:fs');
  const os = await import('node:os');
  const path = await import('node:path');
  const net = await import('node:net');
  const {execFileSync} = await import('node:child_process');
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'scimux-cleanup-test-'));
  const socket=path.join(root,'socket');
  const env={...process.env,HOME:root,XDG_CONFIG_HOME:root,TMUX_TMPDIR:root};delete env.TMUX;delete env.TMUX_PANE;
  const probe=p=>new Promise(resolve=>{const c=net.createConnection(p);c.once('connect',()=>{c.destroy();resolve('live');});c.once('error',e=>resolve(e.code==='ECONNREFUSED'||e.code==='ENOENT'?'dead':'unknown'));});
  try {
    execFileSync('tmux',['-f','/dev/null','-S',socket,'new-session','-d','cat'],{env});
    const outcome=await retire(socket,{exists:fs.existsSync,kill:async()=>{throw Error('injected failure');},probe,unlink:fs.unlinkSync});
    assert.equal(outcome.removed,false);
    assert.equal(await probe(socket),'live');
    execFileSync('tmux',['-S',socket,'has-session'],{env,stdio:'ignore'});
  } finally {
    try {execFileSync('tmux',['-S',socket,'kill-server'],{env,stdio:'ignore'});} catch {}
    try {fs.unlinkSync(socket);} catch(e) {if(e.code!=='ENOENT')throw e;}
    fs.rmSync(root,{recursive:true,force:true});
  }
});
