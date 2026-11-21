import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import os from 'node:os';
import {EventEmitter} from 'node:events';
import {configureRunner,startRunner,limits} from '../runner-bootstrap.cjs';
import {createRunIO} from '../runtime-io.cjs';
import {removeOwned} from '../owned-remove.cjs';
import {createFileStore,createRetention} from '../evidence-retention.cjs';

async function fixture(t) {
  const base=await fs.mkdtemp(path.join(os.tmpdir(),'reservation-finalize-synthetic-')),identity=await fs.lstat(base);
  t.after(async()=>{const st=await fs.lstat(base);assert.equal(st.dev,identity.dev);assert.equal(st.ino,identity.ino);assert.equal(st.uid,identity.uid);await fs.rm(base,{recursive:true});});
  const runtime=path.join(base,'runtime'),evidence=path.join(base,'evidence');await fs.mkdir(runtime,{mode:0o700});await fs.mkdir(evidence,{mode:0o700});
  const env={SCIMUX_E2E_RUNTIME_PARENT:runtime,SCIMUX_E2E_EVIDENCE_PARENT:evidence,SCIMUX_E2E_SUPERVISOR:'/synthetic/supervisor'};
  return {base,runtime,evidence,env,policy:()=>createRetention({store:createFileStore({parent:evidence}),clock:()=>Date.now(),limits})};
}

async function cancel(f,{afterAllocate=async()=>{},removeChild=removeOwned,id='synthetic',createPolicy}={}) {
  const events=new EventEmitter();let launches=0;
  const createIO=options=>{const io=createRunIO({...options,removeChild}),allocate=io.allocate;io.allocate=async()=>{const root=await allocate();await afterAllocate();events.emit('SIGINT');return root;};return io;};
  const start=options=>startRunner({...options,entry:async()=>{launches++;throw Error('unexpected launch');}});
  const result=await configureRunner({env:f.env,runnerPath:'/synthetic/runner',events,createIO,start,verifySupervisor:async()=>{},id,...(createPolicy?{createPolicy}:{})});
  return {result,launches};
}

for(const variant of ['nonempty','replaced','protected']) test(`unstarted cancellation preserves ${variant} reservation and reports failure`,async t=>{
  const f=await fixture(t),stage=path.join(f.evidence,'stage-synthetic');
  const afterAllocate=async()=>{
    if(variant==='nonempty')await fs.writeFile(path.join(stage,'partial'),'synthetic');
    if(variant==='replaced'){await fs.rename(stage,path.join(f.base,'original-stage'));await fs.mkdir(stage,{mode:0o700});await fs.writeFile(path.join(stage,'marker'),'synthetic');}
    if(variant==='protected'){const registry=path.join(f.evidence,'registry.json'),entries=JSON.parse(await fs.readFile(registry,'utf8'));entries[0].protected=true;await fs.writeFile(registry,JSON.stringify(entries));}
  };
  const {result,launches}=await cancel(f,{afterAllocate});assert.equal(launches,0);assert.equal(result.ok,false);assert.equal(result.outcome,'fail');
  if(variant==='nonempty')assert.equal(await fs.readFile(path.join(stage,'partial'),'utf8'),'synthetic');
  if(variant==='replaced')assert.equal(await fs.readFile(path.join(stage,'marker'),'utf8'),'synthetic');
  if(variant==='protected')assert.equal(JSON.parse(await fs.readFile(path.join(f.evidence,'registry.json'),'utf8'))[0].protected,true);
  assert.equal((await f.policy().reserve({id:'synthetic'})).accepted,false);
});

for(const fault of ['stage-remove','registry-rename','registry-write','parent-sync']) test(`finalization ${fault} failure remains visible and refuses ambiguous admission`,async t=>{
  const f=await fixture(t),original={rmdir:fs.rmdir,rename:fs.rename,open:fs.open};let injected=false;
  const removeChild=async options=>{
    const result=await removeOwned(options);
    if(result.removed) {
      fs.rmdir=async (name,...rest)=>{if(fault==='stage-remove'&&String(name).startsWith(f.evidence+path.sep)&&path.basename(name)!=='lock'){injected=true;throw Error('synthetic removal failure');}return original.rmdir(name,...rest);};
      fs.rename=async (from,to,...rest)=>{if(fault==='registry-rename'&&to===path.join(f.evidence,'registry.json')){injected=true;throw Error('synthetic rename failure');}return original.rename(from,to,...rest);};
      fs.open=async (name,...rest)=>{
        if(fault==='registry-write'&&String(name).startsWith(path.join(f.evidence,'write-'))){injected=true;throw Error('synthetic registry write failure');}
        const handle=await original.open(name,...rest);
        if(fault==='parent-sync'&&name===f.evidence)return new Proxy(handle,{get(target,key){if(key==='sync')return async()=>{injected=true;throw Error('synthetic sync failure');};const value=target[key];return typeof value==='function'?value.bind(target):value;}});
        return handle;
      };
    }
    return result;
  };
  let result;
  try {({result}=await cancel(f,{removeChild}));}finally{fs.rmdir=original.rmdir;fs.rename=original.rename;fs.open=original.open;}
  assert.equal(result.ok,false);assert.equal(result.outcome,'fail');assert.equal(injected,true);
  assert.equal((await f.policy().reserve({id:'synthetic'})).accepted,false);
});

test('unproven supervisor retains the active reservation and runtime',async t=>{
  const f=await fixture(t);let launched=0;
  const createIO=options=>createRunIO({...options,removeChild:removeOwned});
  const start=options=>startRunner({...options,entry:async()=>{launched++;return {ok:false,started:true,proof:''};}});
  const result=await configureRunner({env:f.env,runnerPath:'/synthetic/runner',events:new EventEmitter(),createIO,start,verifySupervisor:async()=>{},id:'synthetic'});
  assert.equal(launched,1);assert.equal(result.outcome,'unproven');assert.equal(result.removed,false);
  assert.equal((await fs.readdir(f.runtime)).length,1);assert.equal((await f.policy().reserve({id:'synthetic'})).accepted,false);
});

test('stale cancellation cannot finalize a recreated reservation with the same id',async t=>{
  const f=await fixture(t),registry=path.join(f.evidence,'registry.json'),stage=path.join(f.evidence,'stage-synthetic');let recreated=false;
  const removeChild=async options=>{
    const result=await removeOwned(options);
    if(result.removed){await fs.rmdir(stage);await fs.writeFile(registry,'[]');recreated=(await f.policy().reserve({id:'synthetic'})).accepted;}
    return result;
  };
  const {result}=await cancel(f,{removeChild});assert.equal(recreated,true);assert.equal(result.outcome,'fail');
  assert.equal((await f.policy().reserve({id:'synthetic'})).accepted,false);
  assert.equal((await fs.stat(stage)).isDirectory(),true);
});

test('finalizing one cancellation preserves an independently admitted reservation',async t=>{
  const f=await fixture(t);let admitted=false;
  const testLimits={...limits,reservation:512*1024*1024};
  const policy=()=>createRetention({store:createFileStore({parent:f.evidence}),clock:()=>Date.now(),limits:testLimits});
  const createPolicy=options=>createRetention({...options,limits:testLimits});
  const afterAllocate=async()=>{admitted=(await policy().reserve({id:'neighbor'})).accepted;};
  const {result}=await cancel(f,{afterAllocate,createPolicy});assert.equal(admitted,true);assert.equal(result.outcome,'interrupted');
  assert.equal((await policy().reserve({id:'synthetic'})).accepted,true);
  assert.equal((await policy().reserve({id:'neighbor'})).accepted,false);
});

test('default aggregate accounting refuses a second full active reservation when metadata does not fit',async t=>{
  const f=await fixture(t),policy=f.policy();assert.equal((await policy.reserve({id:'first'})).accepted,true);
  assert.equal((await policy.reserve({id:'second'})).accepted,false);
  assert.equal((await fs.stat(path.join(f.evidence,'stage-first'))).isDirectory(),true);
  assert.equal(await fs.stat(path.join(f.evidence,'stage-second')).then(()=>true,()=>false),false);
});

test('late duplicate witness cannot finalize a new incarnation with the same id',async t=>{
  const f=await fixture(t),policy=f.policy(),first=await policy.reserve({id:'synthetic'});assert.equal(first.accepted,true);
  assert.equal((await policy.finalize({witness:first.witness})).finalized,true);
  assert.equal((await policy.finalize({witness:first.witness})).finalized,false);
  const second=await policy.reserve({id:'synthetic'});assert.equal(second.accepted,true);
  assert.notEqual(second.witness.identity,first.witness.identity);
  assert.equal((await policy.finalize({witness:first.witness})).finalized,false);
  assert.equal((await f.policy().reserve({id:'synthetic'})).accepted,false);
  assert.equal((await fs.stat(path.join(f.evidence,'stage-synthetic'))).isDirectory(),true);
});

test('post-replacement sync failure stays failed; a new fully synced admission uses a fresh witness',async t=>{
  const f=await fixture(t),open=fs.open,parent=await fs.lstat(f.evidence);let synced=0,injected=false,witness;
  const removeChild=async options=>{
    const result=await removeOwned(options);
    if(result.removed)fs.open=async(name,...rest)=>{
      const handle=await open(name,...rest);
      if(name!==f.evidence)return handle;
      return new Proxy(handle,{get(target,key){if(key==='sync')return async()=>{synced++;if(synced===2){injected=true;throw Error('synthetic post-replacement sync failure');}return target.sync();};const value=target[key];return typeof value==='function'?value.bind(target):value;}});
    };
    return result;
  };
  let result,next;
  try {
    ({result}=await cancel(f,{removeChild,afterAllocate:async()=>{const [entry]=JSON.parse(await fs.readFile(path.join(f.evidence,'registry.json'),'utf8'));witness={id:entry.id,stageDev:entry.stageDev,stageIno:entry.stageIno,identity:entry.identity};}}));
    assert.equal(injected,true);assert.equal(result.outcome,'fail');
    next=await f.policy().reserve({id:'synthetic'});assert.equal(next.accepted,true);assert.ok(synced>=4,'new stage and registry barriers must complete');
    const after=await fs.lstat(f.evidence);assert.equal(after.dev,parent.dev);assert.equal(after.ino,parent.ino);assert.equal(after.uid,parent.uid);
    assert.notEqual(next.witness.identity,witness.identity);assert.equal((await f.policy().finalize({witness})).finalized,false);
  }finally{fs.open=open;}
});

test('persistent post-replacement sync failure refuses subsequent admission',async t=>{
  const f=await fixture(t),open=fs.open;let synced=0;
  const removeChild=async options=>{
    const result=await removeOwned(options);
    if(result.removed)fs.open=async(name,...rest)=>{const handle=await open(name,...rest);if(name!==f.evidence)return handle;
      return new Proxy(handle,{get(target,key){if(key==='sync')return async()=>{synced++;if(synced>=2)throw Error('synthetic persistent sync failure');return target.sync();};const value=target[key];return typeof value==='function'?value.bind(target):value;}});};
    return result;
  };
  try {const {result}=await cancel(f,{removeChild});assert.equal(result.outcome,'fail');assert.equal((await f.policy().reserve({id:'synthetic'})).accepted,false);assert.ok(synced>=3);}
  finally {fs.open=open;}
});

test('first retirement sync failure preserves tomb and refuses fresh admission',async t=>{
  const f=await fixture(t),open=fs.open;let injected=false;
  const removeChild=async options=>{
    const result=await removeOwned(options);
    if(result.removed)fs.open=async(name,...rest)=>{const handle=await open(name,...rest);if(name!==f.evidence)return handle;
      return new Proxy(handle,{get(target,key){if(key==='sync')return async()=>{injected=true;throw Error('synthetic first sync failure');};const value=target[key];return typeof value==='function'?value.bind(target):value;}});};
    return result;
  };
  let result;try {({result}=await cancel(f,{removeChild}));}finally{fs.open=open;}
  assert.equal(injected,true);assert.equal(result.outcome,'fail');assert.equal((await fs.stat(path.join(f.evidence,'retire-synthetic'))).isDirectory(),true);
  assert.equal((await f.policy().reserve({id:'another'})).accepted,false);
});

for(const state of ['old-registry-tomb','old-registry-missing','empty-registry-tomb']) test(`synthetic post-crash ${state} refuses admission and preserves state`,async t=>{
  const f=await fixture(t),stage=path.join(f.evidence,'stage-synthetic'),tomb=path.join(f.evidence,'retire-synthetic'),registry=path.join(f.evidence,'registry.json');
  await fs.mkdir(stage,{mode:0o700});const st=await fs.lstat(stage);
  const entry={id:'synthetic',state:'active',bytes:0,reserved:limits.reservation,created:1,registered:true,identity:'mABCDEFGHIJKLMNOP',stageDev:st.dev,stageIno:st.ino};
  if(state==='old-registry-missing')await fs.rmdir(stage);else await fs.rename(stage,tomb);
  const encoded=JSON.stringify(state==='empty-registry-tomb'?[]:[entry]);await fs.writeFile(registry,encoded,{mode:0o600});
  assert.equal((await f.policy().reserve({id:'another'})).accepted,false);assert.equal(await fs.readFile(registry,'utf8'),encoded);
  if(state!=='old-registry-missing'){const after=await fs.lstat(tomb);assert.equal(after.dev,st.dev);assert.equal(after.ino,st.ino);}
  assert.equal(await fs.stat(stage).then(()=>true,()=>false),false);
});

test('synthetic fully retired view admits only after barriers with a fresh witness',async t=>{
  const f=await fixture(t),registry=path.join(f.evidence,'registry.json'),parent=await fs.lstat(f.evidence),open=fs.open;
  await fs.writeFile(registry,'[]',{mode:0o600});let syncs=0;
  const old={id:'synthetic',stageDev:parent.dev,stageIno:123,identity:'mABCDEFGHIJKLMNOP'};
  fs.open=async(name,...rest)=>{const handle=await open(name,...rest);if(name!==f.evidence)return handle;
    return new Proxy(handle,{get(target,key){if(key==='sync')return async()=>{await target.sync();syncs++;};const value=target[key];return typeof value==='function'?value.bind(target):value;}});};
  try {
    const fresh=await f.policy().reserve({id:'synthetic'});assert.equal(fresh.accepted,true);assert.ok(syncs>=2);
    assert.notEqual(fresh.witness.identity,old.identity);assert.equal((await f.policy().finalize({witness:old})).finalized,false);
    const after=await fs.lstat(f.evidence);assert.equal(after.dev,parent.dev);assert.equal(after.ino,parent.ino);assert.equal(after.uid,parent.uid);
  }finally{fs.open=open;}
});

test('synthetic fully retired view refuses new admission when its parent barrier fails',async t=>{
  const f=await fixture(t),open=fs.open;await fs.writeFile(path.join(f.evidence,'registry.json'),'[]',{mode:0o600});let injected=false;
  fs.open=async(name,...rest)=>{const handle=await open(name,...rest);if(name!==f.evidence)return handle;
    return new Proxy(handle,{get(target,key){if(key==='sync')return async()=>{injected=true;throw Error('synthetic admission barrier failure');};const value=target[key];return typeof value==='function'?value.bind(target):value;}});};
  try {assert.equal((await f.policy().reserve({id:'synthetic'})).accepted,false);assert.equal(injected,true);}
  finally{fs.open=open;}
  assert.equal((await f.policy().reserve({id:'another'})).accepted,false);
});

test('store finalization refuses an unregistered incarnation under lock',async t=>{
  const f=await fixture(t),store=createFileStore({parent:f.evidence}),policy=createRetention({store,clock:()=>Date.now(),limits});
  const reservation=await policy.reserve({id:'synthetic'});assert.equal(reservation.accepted,true);
  await assert.rejects(store.locked(async()=>{const [entry]=await store.read();await store.finalize({...entry,identity:'mABCDEFGHIJKLMNOP'},limits);}));
  assert.equal((await fs.stat(path.join(f.evidence,'stage-synthetic'))).isDirectory(),true);
});

for(const failure of ['tomb-exists','tomb-stat-error','stage-replaced','tomb-replaced']) test(`finalization preserves state on ${failure}`,async t=>{
  const f=await fixture(t),policy=f.policy(),reservation=await policy.reserve({id:'synthetic'});assert.equal(reservation.accepted,true);
  const stage=path.join(f.evidence,'stage-synthetic'),tomb=path.join(f.evidence,'retire-synthetic'),lstat=fs.lstat,open=fs.open;let injected=false;
  fs.lstat=async(name,...rest)=>{
    if(name===tomb&&!injected&&failure!=='tomb-replaced'){
      injected=true;
      if(failure==='tomb-exists')await fs.mkdir(tomb,{mode:0o700});
      if(failure==='tomb-stat-error'){const error=Error('synthetic stat error');error.code='EIO';throw error;}
      if(failure==='stage-replaced'){await fs.rename(stage,path.join(f.base,'original-stage'));await fs.mkdir(stage,{mode:0o700});}
    }
    return lstat(name,...rest);
  };
  fs.open=async(name,...rest)=>{
    if(name===f.evidence&&failure==='tomb-replaced'&&!injected){injected=true;await fs.rename(tomb,path.join(f.base,'original-tomb'));await fs.mkdir(tomb,{mode:0o700});}
    return open(name,...rest);
  };
  try {assert.equal((await policy.finalize({witness:reservation.witness})).finalized,false);assert.equal(injected,true);}
  finally{fs.lstat=lstat;fs.open=open;}
  assert.equal((await f.policy().reserve({id:'synthetic'})).accepted,false);
  if(failure==='tomb-exists'||failure==='tomb-replaced')assert.equal((await fs.stat(tomb)).isDirectory(),true);
  if(failure==='stage-replaced')assert.equal((await fs.stat(path.join(f.base,'original-stage'))).isDirectory(),true);
});
