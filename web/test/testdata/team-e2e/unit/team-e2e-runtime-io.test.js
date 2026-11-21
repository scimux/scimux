import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {createRunIO} from '../runtime-io.cjs';

async function fixture(t) {
  const base=await fs.mkdtemp(path.join(os.tmpdir(),'runio-synthetic-'));
  t.after(()=>fs.rm(base,{recursive:true,force:true}));
  const runtimeParent=path.join(base,'runtime'),evidenceParent=path.join(base,'evidence'),neighbor=path.join(base,'neighbor');
  for(const p of [runtimeParent,evidenceParent,neighbor])await fs.mkdir(p,{mode:0o700});
  await fs.writeFile(path.join(neighbor,'marker'),'synthetic');
  return {base,runtimeParent,evidenceParent,neighbor};
}

test('registered runtime allocation precedes launch and removes only its owned root',async t=>{
  const f=await fixture(t),events=[];
  const {removeOwned}=await import('../owned-remove.cjs');
  const io=createRunIO({...f,reserve:async()=>{events.push('reserve');return {accepted:true};},removeChild:removeOwned});
  const runtime=await io.allocate();assert.ok(runtime?.path);assert.deepEqual(events,['reserve']);
  assert.equal((await fs.lstat(runtime.path)).isDirectory(),true);
  assert.deepEqual(await io.inspect('runtime'),runtime);
  assert.equal(await io.remove(runtime),true);
  assert.equal(await fs.stat(runtime.path).then(()=>true,()=>false),false);
  assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

test('reservation refusal creates no runtime directory',async t=>{
  const f=await fixture(t);const io=createRunIO({...f,reserve:async()=>({accepted:false})});
  await assert.rejects(io.allocate());assert.deepEqual(await fs.readdir(f.runtimeParent),[]);
});

test('symlink runtime parent is not adopted',async t=>{
  const f=await fixture(t);const link=path.join(f.base,'link');await fs.symlink(f.runtimeParent,link);
  assert.throws(()=>createRunIO({...f,runtimeParent:link,reserve:async()=>({accepted:true})}));
  assert.deepEqual(await fs.readdir(f.runtimeParent),[]);
});

test('runtime parent replacement blocks allocation and preserves both parents',async t=>{
  const f=await fixture(t);const io=createRunIO({...f,reserve:async()=>({accepted:true})});
  await fs.rename(f.runtimeParent,path.join(f.base,'old-runtime'));await fs.mkdir(f.runtimeParent,{mode:0o700});
  await assert.rejects(io.allocate());assert.deepEqual(await fs.readdir(f.runtimeParent),[]);
  assert.deepEqual(await fs.readdir(path.join(f.base,'old-runtime')),[]);
});

test('runtime I/O refuses unsafe, overlapping and non-private parents',async t=>{
  const f=await fixture(t),reserve=async()=>({accepted:true});
  assert.throws(()=>createRunIO({...f,runtimeParent:'relative',reserve}));
  assert.throws(()=>createRunIO({...f,evidenceParent:f.runtimeParent,reserve}));
  const nested=path.join(f.runtimeParent,'nested');await fs.mkdir(nested,{mode:0o700});
  assert.throws(()=>createRunIO({...f,evidenceParent:nested,reserve}));
  const otherNested=path.join(f.evidenceParent,'nested');await fs.mkdir(otherNested,{mode:0o700});
  assert.throws(()=>createRunIO({...f,runtimeParent:otherNested,reserve}));
  await fs.chmod(f.evidenceParent,0o755);assert.throws(()=>createRunIO({...f,reserve}));
});

test('runtime I/O refuses unknown inspections and wrong-root removal',async t=>{
  const f=await fixture(t),io=createRunIO({...f,reserve:async()=>({accepted:true})});
  await assert.rejects(io.inspect('runtime'));await assert.rejects(io.inspect('unknown'));
  const runtime=await io.allocate();assert.equal(await io.remove({...runtime,path:f.neighbor}),false);
  await fs.chmod(runtime.path,0o755);await assert.rejects(io.inspect('runtime'));
});

test('removal delegates to a child bound to the registered runtime cwd',async t=>{
  const f=await fixture(t);let request;
  const removeChild=async value=>{request=value;return {removed:true};};
  const io=createRunIO({...f,reserve:async()=>({accepted:true}),removeChild});
  const runtime=await io.allocate();assert.equal(await io.remove(runtime),true);
  assert.equal(request.cwd,runtime.path);assert.equal(request.rootIdentity.ino,runtime.ino);
  assert.deepEqual(Object.keys(request.registered).sort(),['tmp','work']);
});

test('removal child waits for explicit removal receipt',async()=>{
  const {EventEmitter}=await import('node:events');const {PassThrough}=await import('node:stream');
  const {removeInChild}=await import('../owned-remove.cjs');let options;
  const spawnProcess=(_bin,_args,opts)=>{
    options=opts;const child=new EventEmitter();child.stdin=new PassThrough();child.stdout=new PassThrough();
    queueMicrotask(()=>{child.stdout.end('removed\n');child.emit('close',0,null);});return child;
  };
  const result=await removeInChild({cwd:'/synthetic/root',registered:{},rootIdentity:{uid:1,dev:2,ino:3}},spawnProcess);
  assert.deepEqual(result,{removed:true});assert.equal(options.cwd,'/synthetic/root');
  assert.deepEqual(options.stdio,['pipe','pipe','ignore']);
});

test('removal child rejects an unbound cwd before touching files',async()=>{
  const {removalChildEntry}=await import('../owned-remove.cjs');let called=false;
  const result=await removalChildEntry({input:{rootIdentity:{uid:1,dev:2,ino:3}},cwdStat:async()=>({uid:1,dev:2,ino:4,isDirectory:()=>true}),remove:async()=>{called=true;return {removed:true};}});
  assert.deepEqual(result,{removed:false});assert.equal(called,false);
});

test('removal child uses a matching bound cwd before traversing',async()=>{
  const {removalChildEntry}=await import('../owned-remove.cjs');let called=false;
  const input={rootIdentity:{uid:1,dev:2,ino:3}};
  const result=await removalChildEntry({input,cwdStat:async()=>({uid:1,dev:2,ino:3,isDirectory:()=>true}),remove:async value=>{called=value===input;return {removed:true};}});
  assert.deepEqual(result,{removed:true});assert.equal(called,true);
});

test('removal child error is not a close receipt',async()=>{
  const {EventEmitter}=await import('node:events');const {PassThrough}=await import('node:stream');
  const {removeInChild}=await import('../owned-remove.cjs');
  const child=new EventEmitter();child.pid=42;child.stdin=new PassThrough();child.stdout=new PassThrough();
  let settled=false;const pending=removeInChild({cwd:'/synthetic/root'},()=>child).then(result=>{settled=true;return result;});
  child.emit('error',Error('synthetic error'));await new Promise(resolve=>setImmediate(resolve));
  try {assert.equal(settled,false);}finally{child.stdout.end();child.emit('close',1,null);}
  assert.deepEqual(await pending,{removed:false});
});

for(const failure of ['spawn','stdin','overflow','exit','signal']) test(`removal child refuses ${failure} without a successful receipt`,async()=>{
  const {EventEmitter}=await import('node:events');const {PassThrough}=await import('node:stream');
  const {removeInChild}=await import('../owned-remove.cjs');
  const spawn=()=>{
    if(failure==='spawn')throw Error('synthetic spawn failure');
    const child=new EventEmitter();child.stdin=new PassThrough();child.stdout=new PassThrough();
    queueMicrotask(()=>{
      if(failure==='stdin')child.stdin.emit('error',Error('synthetic stdin error'));
      child.stdout.end(failure==='overflow'?'x'.repeat(65):'removed\n');
      child.emit('close',failure==='exit'?1:0,failure==='signal'?'SIGTERM':null);
    });return child;
  };
  assert.deepEqual(await removeInChild({cwd:'/synthetic/root'},spawn),{removed:false});
});

test('removal child entry refuses inspection errors without removal',async()=>{
  const {removalChildEntry}=await import('../owned-remove.cjs');let called=false;
  const result=await removalChildEntry({input:{},cwdStat:async()=>{throw Error('synthetic stat failure');},remove:async()=>{called=true;}});
  assert.deepEqual(result,{removed:false});assert.equal(called,false);
});

test('registered runtime removal uses the actual bound cleanup child',{skip:process.env.SCIMUX_E2E_SOCKET_TEST!=='1',timeout:10000},async t=>{
  const f=await fixture(t);const io=createRunIO({...f,reserve:async()=>({accepted:true})});
  const runtime=await io.allocate();await fs.writeFile(path.join(runtime.path,'work','synthetic.txt'),'synthetic');
  assert.equal(await io.remove(runtime),true);
  assert.equal(await fs.stat(runtime.path).then(()=>true,()=>false),false);
  assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

test('work directory identity remains bound to its registered allocation',async t=>{
  const f=await fixture(t);const io=createRunIO({...f,reserve:async()=>({accepted:true})});
  const runtime=await io.allocate();const first=await io.inspect('work').catch(()=>null);assert.ok(first,'registered work identity is available');
  assert.equal(first.path,runtime.work);
  await fs.rename(runtime.work,runtime.work+'-old');await fs.mkdir(runtime.work,{mode:0o700});
  await assert.rejects(io.inspect('work'));
});

test('registered parent identities retain their exact paths for staging',async t=>{
  const f=await fixture(t);const io=createRunIO({...f,reserve:async()=>({accepted:true})});
  assert.equal((await io.inspect('runtimeParent')).path,f.runtimeParent);
  assert.equal((await io.inspect('evidenceParent')).path,f.evidenceParent);
});

test('runtime allocation carries the registered work-directory identity',async t=>{
  const f=await fixture(t);const io=createRunIO({...f,reserve:async()=>({accepted:true})});
  const runtime=await io.allocate(),work=await fs.lstat(runtime.work);
  assert.deepEqual(runtime.workIdentity,{uid:work.uid,dev:work.dev,ino:work.ino});
});

test('inner runner adopts only the exact registered work-directory identity',async()=>{
  const {validateInnerRuntime}=await import('../runtime-io.cjs');
  const env={SCIMUX_E2E_RUNTIME:'/synthetic/work',SCIMUX_E2E_RUNTIME_IDENTITY:JSON.stringify({uid:42,dev:7,ino:11})};
  const stat={uid:42,dev:7,ino:11,mode:0o700,isDirectory:()=>true,isSymbolicLink:()=>false};
  assert.equal(validateInnerRuntime(env,()=>stat,()=>42),'/synthetic/work');
  assert.equal(validateInnerRuntime(env,()=>({...stat,ino:12}),()=>42),null);
  assert.equal(validateInnerRuntime(env,()=>({...stat,isSymbolicLink:()=>true}),()=>42),null);
  assert.equal(validateInnerRuntime({...env,SCIMUX_E2E_RUNTIME:'relative'},()=>stat,()=>42),null);
  assert.equal(validateInnerRuntime({...env,SCIMUX_E2E_RUNTIME_IDENTITY:'invalid'},()=>stat,()=>42),null);
});

for(const failedName of ['work','tmp']) test(`unstarted partial runtime is removed when ${failedName} allocation fails`,async t=>{
  const f=await fixture(t);const {runOwned}=await import('../run-ownership.cjs');
  const {removeOwned}=await import('../owned-remove.cjs');
  let reserved=0,launched=0;
  const io=createRunIO({...f,reserve:async()=>{reserved++;return {accepted:true};},removeChild:removeOwned});
  const runtimeParent=await io.inspect('runtimeParent'),evidenceParent=await io.inspect('evidenceParent');
  const original=fs.mkdir;let injected=false;
  fs.mkdir=async function(name,...args){if(path.basename(name)===failedName){injected=true;throw Object.assign(Error('synthetic allocation failure'),{code:'EIO'});}return original.call(this,name,...args);};
  let result;
  try {result=await runOwned({runtimeParent,evidenceParent,io,supervisor:{launch:async()=>{launched++;}},project:async()=>({ok:true})});}
  finally {fs.mkdir=original;}
  assert.equal(injected,true);assert.equal(reserved,1);assert.equal(launched,0);
  assert.equal(result.outcome,'allocation-failed');assert.equal(result.removed,true);
  assert.deepEqual(await fs.readdir(f.runtimeParent),[]);
  assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

test('changed partial root is preserved when unstarted allocation fails',async t=>{
  const f=await fixture(t);const {runOwned}=await import('../run-ownership.cjs');
  const {removeOwned}=await import('../owned-remove.cjs');let launched=0;
  const io=createRunIO({...f,reserve:async()=>({accepted:true}),removeChild:removeOwned});
  const runtimeParent=await io.inspect('runtimeParent'),evidenceParent=await io.inspect('evidenceParent');
  const original=fs.mkdir;let replacement;
  fs.mkdir=async function(name,...args){
    if(path.basename(name)==='work') {
      const root=path.dirname(name);await fs.rename(root,root+'-old');await original.call(this,root,{mode:0o700});
      replacement=path.join(root,'marker');await fs.writeFile(replacement,'synthetic replacement');
      throw Object.assign(Error('synthetic allocation failure'),{code:'EIO'});
    }
    return original.call(this,name,...args);
  };
  let result;
  try {result=await runOwned({runtimeParent,evidenceParent,io,supervisor:{launch:async()=>{launched++;}},project:async()=>({ok:true})});}
  finally {fs.mkdir=original;}
  assert.equal(result.outcome,'allocation-failed');assert.equal(result.removed,false);assert.equal(launched,0);
  assert.equal(await fs.readFile(replacement,'utf8'),'synthetic replacement');
  assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

for(const failure of ['refuse','throw']) test(`unstarted partial runtime remains registered when cleanup child ${failure}s`,async t=>{
  const f=await fixture(t);const {runOwned}=await import('../run-ownership.cjs');let cleanupCalls=0,launches=0;
  const io=createRunIO({...f,reserve:async()=>({accepted:true}),removeChild:async()=>{cleanupCalls++;if(failure==='throw')throw Error('synthetic cleanup failure');return {removed:false};}});
  const runtimeParent=await io.inspect('runtimeParent'),evidenceParent=await io.inspect('evidenceParent');
  const original=fs.mkdir;
  fs.mkdir=async function(name,...args){if(path.basename(name)==='work')throw Object.assign(Error('synthetic failure'),{code:'EIO'});return original.call(this,name,...args);};
  let result;
  try {result=await runOwned({runtimeParent,evidenceParent,io,supervisor:{launch:async()=>{launches++;}},project:async()=>({ok:true})});}
  finally {fs.mkdir=original;}
  assert.equal(result.outcome,'allocation-failed');assert.equal(result.removed,false);
  assert.equal(cleanupCalls,1);assert.equal(launches,0);assert.equal((await fs.readdir(f.runtimeParent)).length,1);
  assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

test('unstarted runtime is removed when its mode confirmation fails',async t=>{
  const f=await fixture(t);const {runOwned}=await import('../run-ownership.cjs');const {removeOwned}=await import('../owned-remove.cjs');
  const io=createRunIO({...f,reserve:async()=>({accepted:true}),removeChild:removeOwned});
  const runtimeParent=await io.inspect('runtimeParent'),evidenceParent=await io.inspect('evidenceParent');
  const original=fs.chmod;let launched=0;
  fs.chmod=async()=>{throw Object.assign(Error('synthetic chmod failure'),{code:'EIO'});};
  let result;try{result=await runOwned({runtimeParent,evidenceParent,io,supervisor:{launch:async()=>{launched++;}},project:async()=>({ok:true})});}finally{fs.chmod=original;}
  assert.equal(result.outcome,'allocation-failed');assert.equal(result.removed,true);assert.equal(launched,0);
  assert.deepEqual(await fs.readdir(f.runtimeParent),[]);assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

test('failed root creation preserves the reservation and never launches',async t=>{
  const f=await fixture(t);const {runOwned}=await import('../run-ownership.cjs');let reserved=0,launched=0,removed=0;
  const io=createRunIO({...f,reserve:async()=>{reserved++;return {accepted:true};},removeChild:async()=>{removed++;return {removed:true};}});
  const runtimeParent=await io.inspect('runtimeParent'),evidenceParent=await io.inspect('evidenceParent');
  const original=fs.mkdtemp;fs.mkdtemp=async()=>{throw Error('synthetic allocation failure');};
  let result;try{result=await runOwned({runtimeParent,evidenceParent,io,supervisor:{launch:async()=>{launched++;}},project:async()=>({ok:true})});}finally{fs.mkdtemp=original;}
  assert.equal(result.outcome,'allocation-failed');assert.equal(result.removed,false);assert.equal(reserved,1);assert.equal(launched,0);assert.equal(removed,0);
  assert.deepEqual(await fs.readdir(f.runtimeParent),[]);
});

for(const change of ['kind','symlink','uid','device','mode','unreadable']) test(`unverified new runtime ${change} is preserved`,async t=>{
  const f=await fixture(t);const {runOwned}=await import('../run-ownership.cjs');let removed=0,launched=0;
  const io=createRunIO({...f,reserve:async()=>({accepted:true}),removeChild:async()=>{removed++;return {removed:true};}});
  const runtimeParent=await io.inspect('runtimeParent'),evidenceParent=await io.inspect('evidenceParent');const original=fs.lstat;
  fs.lstat=async function(name,...args){
    const st=await original.call(this,name,...args);
    if(path.dirname(name)!==f.runtimeParent)return st;
    if(change==='unreadable')throw Error('synthetic lstat failure');
    const replacement=Object.create(st);
    if(change==='kind')replacement.isDirectory=()=>false;
    if(change==='symlink')replacement.isSymbolicLink=()=>true;
    if(change==='uid')replacement.uid=st.uid+1;
    if(change==='device')replacement.dev=st.dev+1;
    if(change==='mode')replacement.mode=(st.mode&~0o777)|0o755;
    return replacement;
  };
  let result;try{result=await runOwned({runtimeParent,evidenceParent,io,supervisor:{launch:async()=>{launched++;}},project:async()=>({ok:true})});}finally{fs.lstat=original;}
  assert.equal(result.outcome,'allocation-failed');assert.equal(result.removed,false);assert.equal(removed,0);assert.equal(launched,0);
  assert.equal((await fs.readdir(f.runtimeParent)).length,1);assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

test('partial allocation failure uses the actual identity-bound cleanup child',{skip:process.env.SCIMUX_E2E_SOCKET_TEST!=='1',timeout:10000},async t=>{
  const f=await fixture(t);const {runOwned}=await import('../run-ownership.cjs');let launched=0;
  const io=createRunIO({...f,reserve:async()=>({accepted:true})});
  const runtimeParent=await io.inspect('runtimeParent'),evidenceParent=await io.inspect('evidenceParent');
  const original=fs.mkdir;
  fs.mkdir=async function(name,...args){if(path.basename(name)==='work')throw Object.assign(Error('synthetic failure'),{code:'EIO'});return original.call(this,name,...args);};
  let result;try{result=await runOwned({runtimeParent,evidenceParent,io,supervisor:{launch:async()=>{launched++;}},project:async()=>({ok:true})});}finally{fs.mkdir=original;}
  assert.equal(result.outcome,'allocation-failed');assert.equal(result.removed,true);assert.equal(launched,0);
  assert.deepEqual(await fs.readdir(f.runtimeParent),[]);assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});
