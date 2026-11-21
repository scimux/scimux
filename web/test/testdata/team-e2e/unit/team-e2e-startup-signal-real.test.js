import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {spawn} from 'node:child_process';
import {fileURLToPath} from 'node:url';

const enabled=process.env.SCIMUX_E2E_STARTUP_SIGNAL_TEST==='1';
const fixture=fileURLToPath(new URL('../synthetic-startup-signal.cjs',import.meta.url));
for(const [signal,code] of [['SIGINT',130],['SIGTERM',143]]) test(`real ${signal} during held startup is recorded before launch`,{skip:!enabled,timeout:10000},async t=>{
  const base=await fs.mkdtemp(path.join(os.tmpdir(),'startup-signal-synthetic-'));t.after(()=>fs.rm(base,{recursive:true,force:true}));
  const runtime=path.join(base,'runtime'),evidence=path.join(base,'evidence');await fs.mkdir(runtime,{mode:0o700});await fs.mkdir(evidence,{mode:0o700});
  const child=spawn(process.execPath,[fixture,runtime,evidence,signal],{stdio:['pipe','pipe','pipe']});let output='',error='';
  child.stdout.on('data',chunk=>{output+=chunk;});child.stderr.on('data',chunk=>{error+=chunk;});
  const closed=new Promise((resolve,reject)=>{child.once('error',reject);child.once('close',(exit,term)=>resolve({exit,term}));});
  const ready=new Promise((resolve,reject)=>{child.stdout.once('data',resolve);child.once('error',reject);child.once('close',()=>reject(Error('synthetic entry closed before ready')));});
  const bounded=async promise=>{let timer;try {return await Promise.race([promise,new Promise((_,reject)=>{timer=setTimeout(()=>reject(Error('synthetic entry timeout')),3000);})]);}finally{clearTimeout(timer);}};
  try {await bounded(ready);assert.equal(output,'ready\n');
    const observed=new Promise((resolve,reject)=>{child.stdout.once('data',resolve);child.once('close',()=>reject(Error('signal was not observed')));});
    assert.equal(child.kill(signal),true);await bounded(observed);assert.equal(output,'ready\nobserved\n');child.stdin.end();
    assert.deepEqual(await bounded(closed),{exit:code,term:null});
    const result=JSON.parse(output.slice('ready\nobserved\n'.length));assert.equal(result.outcome,'interrupted');assert.equal(result.signal,signal);assert.equal(result.removed,false);assert.equal(result.unstarted,true);
    assert.equal(error,'');assert.deepEqual(await fs.readdir(runtime),[]);assert.deepEqual(await fs.readdir(evidence),[]);
  } finally {if(child.exitCode===null&&child.signalCode===null){child.kill('SIGKILL');await bounded(closed);}}
});
