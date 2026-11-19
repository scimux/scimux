'use strict';
import test from 'node:test';
import assert from 'node:assert/strict';
import {createRetention,createFileStore} from '../evidence-retention.cjs';
import {mkdtemp, mkdir, readFile, rename, rm, stat, symlink, writeFile} from 'node:fs/promises';
import os from 'node:os';
import fs from 'node:fs';
import path from 'node:path';

// The store is deliberately synthetic: no paths or real runs enter this model.
const DAY=24*60*60*1000;
const limits={age:7*DAY,total:200,reservation:50,diagnostics:1};
const now=20*DAY;
class Store {
  constructor(entries=[],fault='') {
    this.entries=new Map(entries.map(e=>[e.id,structuredClone(e)]));
    this.fixedBytes=0;
    this.fault=fault;
    this.lock=Promise.resolve();
    this.neighbor='untouched';
  }
  async locked(fn) {
    if(this.fault==='lock')throw Error('synthetic lock ambiguity');
    const previous=this.lock;let release;
    this.lock=new Promise(resolve=>{release=resolve});
    await previous;
    try{return await fn();}finally{release();}
  }
  async read() { if(this.fault==='read')throw Error('synthetic read failure');return structuredClone([...this.entries.values()]); }
  async write(entries) { if(this.fault==='write')throw Error('synthetic write failure');this.entries=new Map(entries.map(e=>[e.id,structuredClone(e)])); }
  async remove(id) { if(this.fault==='remove')throw Error('synthetic remove failure');this.entries.delete(id); }
  async extra() {return 0;}
  async prepare() {return {};}
  async admit() {}
  async retire(entry) {
    const before=await this.read();
    await this.write(before.filter(e=>e.id!==entry.id));
    try {await this.remove(entry.id);}catch(err){await this.write(before);throw err;}
  }
  async step(phase) { if(this.fault===phase)throw Error('synthetic I/O failure'); }
  async publish(id,entry) { await this.step('publish');this.entries.set(id,structuredClone(entry)); }
  snapshot() {return structuredClone([...this.entries.values()].sort((a,b)=>a.id.localeCompare(b.id)));}
}
const completed=(id,bytes,age=0,extra={})=>({id,state:'complete',bytes,created:now-age,registered:true,identity:'matched',...extra});
const active=(id)=>({id,state:'active',bytes:0,reserved:50,created:now,registered:true,identity:'matched'});
const incomplete=(id,bytes)=>({id,state:'incomplete',bytes,created:now,registered:true,identity:'matched'});
const setup=(entries=[],fault='',clock=()=>now)=>{
  const store=new Store(entries,fault);
  return {store,policy:createRetention({store,clock,limits})};
};

for (const [name,entry] of [
  ['active',active('protected')],
  ['unknown state',{...completed('protected',160,20*DAY),state:'unknown'}],
  ['unregistered',completed('protected',160,20*DAY,{registered:false})],
  ['changed identity',completed('protected',160,20*DAY,{identity:'changed'})],
  ['historical protected',completed('protected',160,20*DAY,{protected:true})],
  ['incomplete staging',incomplete('protected',160)],
]) test(`admission preserves ${name} evidence`,async()=>{
  const {store,policy}=setup([entry,...(entry.state==='active'?[completed('occupied',110)]:[])]);const before=store.snapshot();
  const result=await policy.reserve({id:'new'});
  assert.equal(result.accepted,false);
  assert.deepEqual(store.snapshot(),before);
  assert.equal(store.neighbor,'untouched');
});

test('admission serializes competing reservations against aggregate cap',async()=>{
  const {store,policy}=setup([active('a'),active('b'),active('c')]);
  const results=await Promise.all([policy.reserve({id:'new-a'}),policy.reserve({id:'new-b'})]);
  assert.equal(results.filter(r=>r.accepted).length,1);
  assert.equal(store.snapshot().filter(e=>e.state==='active').length,4);
});

test('admission reserves full per-run budget including staging and metadata',async()=>{
  const {store,policy}=setup([completed('old',150)]);
  const result=await policy.reserve({id:'new'});
  assert.equal(result.accepted,true);
  const entry=store.snapshot().find(e=>e.id==='new');
  assert.equal(entry?.reserved,50);
  assert.equal((await policy.reserve({id:'extra'})).accepted,false);
});

test('incomplete staging and completed metadata count toward admission',async()=>{
  const {store,policy}=setup([incomplete('partial',51),completed('bundle',100)]);
  const before=store.snapshot();assert.equal((await policy.reserve({id:'new'})).accepted,false);
  assert.deepEqual(store.snapshot(),before);
});

test('eligible expiry occurs only after seven complete days and never touches neighbor',async()=>{
  const {store,policy}=setup([completed('young',80,7*DAY-1),completed('expired',80,7*DAY)]);
  assert.equal((await policy.reserve({id:'new'})).accepted,true);
  assert.deepEqual(store.snapshot().map(e=>e.id),['new','young']);
  assert.equal(store.neighbor,'untouched');
});

test('admission expires eligible evidence at seven days even with spare capacity',async()=>{
  const young=completed('young',10,7*DAY-1);
  const expired=completed('expired',10,7*DAY);
  const protectedEntry=completed('protected',10,10*DAY,{protected:true});
  const running=active('running');
  const {store,policy}=setup([young,expired,protectedEntry,running]);
  assert.equal((await policy.reserve({id:'new'})).accepted,true);
  assert.deepEqual(store.snapshot().find(e=>e.id==='young'),young);
  assert.deepEqual(store.snapshot().find(e=>e.id==='running'),running);
  assert.deepEqual(store.snapshot().find(e=>e.id==='protected'),protectedEntry);
  assert.equal(store.neighbor,'untouched');
  assert.deepEqual(store.snapshot().map(e=>e.id),['new','protected','running','young']);
});

for(const fault of ['lock','read','write','remove']) test(`admission fails closed on ${fault} failure`,async()=>{
  const entry=completed('expired',160,8*DAY);const {store,policy}=setup([entry],fault);const before=store.snapshot();
  const result=await policy.reserve({id:'new'});
  assert.equal(result.accepted,false);
  assert.deepEqual(store.snapshot(),before);
  assert.equal(store.neighbor,'untouched');
});

test('clock failure prevents expiration and admission',async()=>{
  const {store,policy}=setup([completed('old',160,8*DAY)],'',()=>{throw Error('synthetic clock failure')});
  const before=store.snapshot();assert.equal((await policy.reserve({id:'new'})).accepted,false);
  assert.deepEqual(store.snapshot(),before);
});

for(const [name,evidence] of [
  ['staging overflow',{staging:49,metadata:2,diagnostics:0}],
  ['diagnostic overflow',{staging:1,metadata:1,diagnostics:2}],
]) test(`publication rejects ${name} inside reservation`,async()=>{
  const {store,policy}=setup([active('run')]);const before=store.snapshot();
  assert.equal((await policy.publish({id:'run',evidence})).published,false);
  assert.deepEqual(store.snapshot(),before);
});

test('publication accounts for staging, metadata and failure diagnostics',async()=>{
  const {store,policy}=setup([active('run')]);
  assert.equal((await policy.publish({id:'run',evidence:{staging:48,metadata:1,diagnostics:1}})).published,true);
  assert.deepEqual(store.snapshot().map(e=>[e.id,e.state,e.bytes]),[['run','complete',50]]);
});

test('failed publication retains recoverable staging and fails closed',async()=>{
  const {store,policy}=setup([active('run')],'publish');
  const result=await policy.publish({id:'run',evidence:{staging:10,metadata:1,diagnostics:1}});
  assert.equal(result.published,false);
  assert.equal(store.snapshot().find(e=>e.id==='run')?.state,'incomplete');
  assert.equal((await policy.reserve({id:'other'})).accepted,false);
});

test('recovery leaves ambiguous incomplete publication and protected entries intact',async()=>{
  const entries=[incomplete('partial',11),completed('protected',20,30*DAY,{protected:true})];
  const {store,policy}=setup(entries);const before=store.snapshot();
  assert.equal((await policy.recover()).recovered,false);
  assert.deepEqual(store.snapshot(),before);
  assert.equal(store.neighbor,'untouched');
});

for(const phase of ['stage-write','file-sync','rename','parent-sync','registry-commit']) test(`publication fails closed on ${phase} fault`,async()=>{
  const protectedEntry=completed('protected',20,30*DAY,{protected:true});
  const {store,policy}=setup([active('run'),protectedEntry],phase);
  const result=await policy.publish({id:'run',evidence:{staging:10,metadata:1,diagnostics:1}});
  assert.equal(result.published,false);
  assert.deepEqual(store.snapshot().find(e=>e.id==='protected'),protectedEntry);
  assert.ok(['active','incomplete'].includes(store.snapshot().find(e=>e.id==='run')?.state));
  assert.equal(store.neighbor,'untouched');
});

test('failed evidence diagnostics count against both reservation and aggregate capacity',async()=>{
  const failed={...incomplete('failed',100),diagnostics:1};
  const {store,policy}=setup([failed,completed('retained',50)]);
  assert.equal((await policy.reserve({id:'new'})).accepted,false);
  assert.equal(store.snapshot().find(e=>e.id==='failed')?.diagnostics,1);
});

for(const fault of ['lock','read']) test(`recovery stops on ${fault} fault without deleting evidence`,async()=>{
  const {store,policy}=setup([incomplete('partial',11)],fault);const before=store.snapshot();
  assert.equal((await policy.recover()).recovered,false);
  assert.deepEqual(store.snapshot(),before);
});

for(const entry of [
  {...completed('bad',-1)},
  {...completed('bad',Number.MAX_SAFE_INTEGER+1)},
  {...completed('bad',1),created:NaN},
  {...active('bad'),reserved:-1},
  {...completed('bad',1),id:''},
]) test('invalid recorded accounting refuses admission',async()=>{
  const {store,policy}=setup([entry]);const before=store.snapshot();
  assert.equal((await policy.reserve({id:'new'})).accepted,false);
  assert.deepEqual(store.snapshot(),before);
});

for(const id of ['', '../other', 'a/b']) test('invalid reservation identity refuses admission',async()=>{
  const {store,policy}=setup();
  assert.equal((await policy.reserve({id})).accepted,false);
  assert.deepEqual(store.snapshot(),[]);
});

for(const evidence of [
  {staging:-1,metadata:0,diagnostics:0},
  {staging:1,metadata:NaN,diagnostics:0},
  {staging:1,metadata:0,diagnostics:-1},
]) test('invalid publication accounting refuses publication',async()=>{
  const {store,policy}=setup([active('run')]);const before=store.snapshot();
  assert.equal((await policy.publish({id:'run',evidence})).published,false);
  assert.deepEqual(store.snapshot(),before);
});

async function diskFixture(t) {
  const root=await mkdtemp(path.join(os.tmpdir(),'retention-unit-'));
  const parent=path.join(root,'registry');await mkdir(parent,{mode:0o700});
  t.after(async()=>{await rm(root,{recursive:true,force:true});});
  return {root,parent};
}
test('filesystem registry survives reopen without touching a neighbor',async t=>{
  const {root,parent}=await diskFixture(t);const marker=path.join(root,'neighbor');await writeFile(marker,'synthetic');
  const first=createFileStore({parent});let entries;
  await first.locked(async()=>{entries=[{...active('run'),...await first.prepare('run')}];await first.write(entries,{total:20000});});
  const reopened=createFileStore({parent});
  assert.deepEqual(await reopened.locked(()=>reopened.read()),entries);
  assert.equal(await readFile(marker,'utf8'),'synthetic');
});
test('filesystem registry refuses an ambiguous preexisting lock',async t=>{
  const {parent}=await diskFixture(t);await mkdir(path.join(parent,'lock'));
  const store=createFileStore({parent});
  await assert.rejects(store.locked(()=>store.read()));
});
test('filesystem registry refuses a symlink without changing its target',async t=>{
  const {root,parent}=await diskFixture(t);const target=path.join(root,'neighbor');await writeFile(target,'synthetic');
  await symlink(target,path.join(parent,'registry.json'));
  const store=createFileStore({parent});
  await assert.rejects(store.locked(()=>store.read()));
  assert.equal(await readFile(target,'utf8'),'synthetic');
});
test('filesystem registry refuses a replaced parent during a transaction',async t=>{
  const {root,parent}=await diskFixture(t);const store=createFileStore({parent});
  await assert.rejects(store.locked(async()=>{
    await rename(parent,path.join(root,'original'));await mkdir(parent,{mode:0o700});
    await store.write([active('run')],{total:20000});
  }));
  assert.deepEqual(await readFile(path.join(root,'original','registry.json'),'utf8').catch(()=>''),'');
});

test('filesystem-backed reservation and publication survive reopen',async t=>{
  const {parent}=await diskFixture(t);
  const diskLimits={...limits,total:20000,reservation:5000};
  const first=createRetention({store:createFileStore({parent}),clock:()=>now,limits:diskLimits});
  assert.equal((await first.reserve({id:'run'})).accepted,true);
  const stage=path.join(parent,'stage-run');
  assert.equal(await stat(stage).then(s=>s.isDirectory(),()=>false),true,'reservation creates registered staging');
  await writeFile(path.join(stage,'artifact'),'0123456789');
  assert.equal((await first.publish({id:'run',evidence:{staging:8,metadata:1,diagnostics:1}})).published,true);
  assert.equal(await readFile(path.join(parent,'bundle-run','artifact'),'utf8'),'0123456789');
  const reopened=createFileStore({parent});
  const [entry]=await reopened.locked(()=>reopened.read());
  assert.equal(entry.id,'run');assert.equal(entry.state,'complete');
  assert.equal(entry.bytes,10+Buffer.byteLength(JSON.stringify(entry))+1);
});
test('filesystem-backed corrupt registry fails admission without replacing it',async t=>{
  const {parent}=await diskFixture(t);const registry=path.join(parent,'registry.json');await writeFile(registry,'synthetic-invalid');
  const policy=createRetention({store:createFileStore({parent}),clock:()=>now,limits});
  assert.equal((await policy.reserve({id:'run'})).accepted,false);
  assert.equal(await readFile(registry,'utf8'),'synthetic-invalid');
});

test('filesystem publication refuses a replaced staging directory',async t=>{
  const {root,parent}=await diskFixture(t);const store=createFileStore({parent});
  const policy=createRetention({store,clock:()=>now,limits:{...limits,total:20000,reservation:5000}});
  assert.equal((await policy.reserve({id:'run'})).accepted,true);
  await rename(path.join(parent,'stage-run'),path.join(root,'original-stage'));
  await mkdir(path.join(parent,'stage-run'),{mode:0o700});await writeFile(path.join(parent,'stage-run','artifact'),'0123456789');
  assert.equal((await policy.publish({id:'run',evidence:{staging:8,metadata:1,diagnostics:1}})).published,false);
  assert.equal(await readFile(path.join(parent,'stage-run','artifact'),'utf8'),'0123456789');
});
test('filesystem publication refuses a misreported artifact size',async t=>{
  const {parent}=await diskFixture(t);const policy=createRetention({store:createFileStore({parent}),clock:()=>now,limits:{...limits,total:20000,reservation:5000}});
  assert.equal((await policy.reserve({id:'run'})).accepted,true);
  await writeFile(path.join(parent,'stage-run','artifact'),'too short');
  assert.equal((await policy.publish({id:'run',evidence:{staging:8,metadata:1,diagnostics:1}})).published,false);
});
test('filesystem admission refuses a replaced active staging directory',async t=>{
  const {root,parent}=await diskFixture(t);const policy=createRetention({store:createFileStore({parent}),clock:()=>now,limits:{...limits,total:20000,reservation:5000}});
  assert.equal((await policy.reserve({id:'first'})).accepted,true);
  await rename(path.join(parent,'stage-first'),path.join(root,'original-stage'));
  await mkdir(path.join(parent,'stage-first'),{mode:0o700});
  assert.equal((await policy.reserve({id:'second'})).accepted,false);
});
test('filesystem admission leaves unknown neighboring entries untouched',async t=>{
  const {parent}=await diskFixture(t);await mkdir(path.join(parent,'unknown'),{mode:0o700});
  await writeFile(path.join(parent,'unknown','marker'),'synthetic');
  const policy=createRetention({store:createFileStore({parent}),clock:()=>now,limits});
  assert.equal((await policy.reserve({id:'run'})).accepted,false);
  assert.equal(await readFile(path.join(parent,'unknown','marker'),'utf8'),'synthetic');
});

test('filesystem admission retires only the registered expired bundle',async t=>{
  const {root,parent}=await diskFixture(t);const marker=path.join(root,'neighbor');await writeFile(marker,'synthetic');
  const small={...limits,total:20000,reservation:2000};
  const store=createFileStore({parent});
  const old=createRetention({store,clock:()=>now-8*DAY,limits:small});
  assert.equal((await old.reserve({id:'old'})).accepted,true);
  await writeFile(path.join(parent,'stage-old','artifact'),Buffer.alloc(50));
  assert.equal((await old.publish({id:'old',evidence:{staging:48,metadata:1,diagnostics:1}})).published,true);
  const current=createRetention({store,clock:()=>now-7*DAY+1,limits:small});
  assert.equal((await current.reserve({id:'young'})).accepted,true);
  await writeFile(path.join(parent,'stage-young','artifact'),Buffer.alloc(50));
  assert.equal((await current.publish({id:'young',evidence:{staging:48,metadata:1,diagnostics:1}})).published,true);
  const retained=await store.locked(()=>store.read());
  const constrained={...small,total:retained.reduce((n,e)=>n+e.bytes,0)+small.reservation+1};
  const constrainedPolicy=createRetention({store,clock:()=>now,limits:constrained});
  assert.equal((await constrainedPolicy.reserve({id:'new'})).accepted,true,'expired registered bundle frees reservation');
  assert.equal(await stat(path.join(parent,'bundle-old')).then(()=>true,()=>false),false);
  assert.equal((await stat(path.join(parent,'bundle-young'))).isDirectory(),true);
  assert.equal(await readFile(marker,'utf8'),'synthetic');
});

test('filesystem retirement protects a bundle whose contents changed',async t=>{
  const {parent}=await diskFixture(t);const small={...limits,total:2002,reservation:2000};const store=createFileStore({parent});
  const old=createRetention({store,clock:()=>now-8*DAY,limits:small});
  assert.equal((await old.reserve({id:'old'})).accepted,true);
  await writeFile(path.join(parent,'stage-old','artifact'),Buffer.alloc(50));
  assert.equal((await old.publish({id:'old',evidence:{staging:48,metadata:1,diagnostics:1}})).published,true);
  await writeFile(path.join(parent,'bundle-old','unknown'),'synthetic');
  const current=createRetention({store,clock:()=>now,limits:small});
  assert.equal((await current.reserve({id:'new'})).accepted,false);
  assert.equal(await readFile(path.join(parent,'bundle-old','unknown'),'utf8'),'synthetic');
});

test('unchanged expired bundle retires under the same synthetic headroom',async t=>{
  const {parent}=await diskFixture(t);const small={...limits,total:2002,reservation:2000};const store=createFileStore({parent});
  const old=createRetention({store,clock:()=>now-8*DAY,limits:small});assert.equal((await old.reserve({id:'old'})).accepted,true);
  await writeFile(path.join(parent,'stage-old','artifact'),Buffer.alloc(50));
  assert.equal((await old.publish({id:'old',evidence:{staging:48,metadata:1,diagnostics:1}})).published,true);
  const current=createRetention({store,clock:()=>now,limits:small});assert.equal((await current.reserve({id:'new'})).accepted,true);
  assert.equal(await stat(path.join(parent,'bundle-old')).then(()=>true,()=>false),false);
  assert.equal((await stat(path.join(parent,'stage-new'))).isDirectory(),true);
});

test('original 500-byte reservation truthfully refuses oversized metadata without mutating staged bytes',async t=>{
  const {parent}=await diskFixture(t);const small={...limits,total:502,reservation:500};const store=createFileStore({parent});let peak=0;
  const original=fs.promises.rename;
  fs.promises.rename=async(from,to)=>{peak=Math.max(peak,await logicalBytes(parent));await original(from,to);peak=Math.max(peak,await logicalBytes(parent));};
  try {
    const policy=createRetention({store,clock:()=>now-8*DAY,limits:small});assert.equal((await policy.reserve({id:'old'})).accepted,true);
    const artifact=path.join(parent,'stage-old','artifact'),bytes=Buffer.alloc(50,7);await writeFile(artifact,bytes);peak=Math.max(peak,await logicalBytes(parent));
    const result=await policy.publish({id:'old',evidence:{staging:48,metadata:1,diagnostics:1}});
    assert.equal(result.published,false);
    const staged=await readFile(artifact).catch(()=>null),moved=await readFile(path.join(parent,'bundle-old','artifact')).catch(()=>null);
    assert.deepEqual(staged??moved,bytes);assert.equal((await policy.reserve({id:'another'})).accepted,false);
    assert.ok(peak<=small.total,'physical bytes remain within the aggregate cap');
  } finally {fs.promises.rename=original;}
});

test('filesystem metadata bytes count against the reservation',async t=>{
  const {parent}=await diskFixture(t);const tiny={...limits,total:1,reservation:1};
  const policy=createRetention({store:createFileStore({parent}),clock:()=>now,limits:tiny});
  const admitted=await policy.reserve({id:'run'});
  if(admitted.accepted) {
    const result=await policy.publish({id:'run',evidence:{staging:0,metadata:0,diagnostics:0}});
    assert.equal(result.published,false,'encoded registry metadata cannot fit in one byte');
  }
});

test('admission includes registry framing when selecting expired retirement',async t=>{
  const {parent}=await diskFixture(t);const store=createFileStore({parent});
  const broad={...limits,total:10000,reservation:500};
  const old=createRetention({store,clock:()=>now-8*DAY,limits:broad});
  assert.equal((await old.reserve({id:'old'})).accepted,true);
  await writeFile(path.join(parent,'stage-old','artifact'),Buffer.alloc(10));
  assert.equal((await old.publish({id:'old',evidence:{staging:8,metadata:1,diagnostics:1}})).published,true);
  const [entry]=await store.locked(()=>store.read());
  const exact={...broad,total:entry.bytes+broad.reservation+1};
  const current=createRetention({store,clock:()=>now,limits:exact});
  assert.equal((await current.reserve({id:'new'})).accepted,true,'framing pressure retires the eligible bundle');
  assert.equal(await stat(path.join(parent,'bundle-old')).then(()=>true,()=>false),false);
});

test('failed reservation never adopts an unregistered staged directory',async t=>{
  const {parent}=await diskFixture(t);const store=createFileStore({parent});
  const tiny=createRetention({store,clock:()=>now,limits:{...limits,total:3,reservation:1}});
  assert.equal((await tiny.reserve({id:'failed'})).accepted,false);
  const broad=createRetention({store,clock:()=>now,limits:{...limits,total:20000,reservation:5000}});
  assert.equal((await broad.reserve({id:'next'})).accepted,false);
  assert.equal((await stat(path.join(parent,'stage-failed'))).isDirectory(),true);
});

test('retirement preserves a file replaced after bundle rename',async t=>{
  const {root,parent}=await diskFixture(t);const limits2={age:7,total:5250,reservation:5000,diagnostics:1};let clock=100;
  const policy=createRetention({store:createFileStore({parent}),clock:()=>clock,limits:limits2});
  assert.equal((await policy.reserve({id:'owned'})).accepted,true);
  await writeFile(path.join(parent,'stage-owned','witness.txt'),Buffer.alloc(20,1));
  assert.equal((await policy.publish({id:'owned',evidence:{staging:20,metadata:0,diagnostics:0}})).published,true);
  const marker=path.join(root,'neighbor');await writeFile(marker,'synthetic neighbor');
  const original=fs.promises.rename;let swapped=false;
  try {
    fs.promises.rename=async(from,to)=>{
      await original(from,to);
      if(from===path.join(parent,'bundle-owned')) {
        await original(path.join(to,'witness.txt'),path.join(root,'original'));
        await writeFile(path.join(to,'witness.txt'),'synthetic replacement');swapped=true;
      }
    };
    clock+=10;
    const result=await policy.reserve({id:'next'});
    assert.equal(swapped,true,'replacement occurs after the bundle rename');
    assert.equal(result.accepted,false,'replacement must stop retirement');
    assert.equal(await readFile(path.join(parent,'retire-owned','witness.txt'),'utf8'),'synthetic replacement');
    assert.equal(await readFile(marker,'utf8'),'synthetic neighbor');
  } finally {fs.promises.rename=original;}
});

async function logicalBytes(dir) {
  let size=0;
  for(const entry of await fs.promises.readdir(dir,{withFileTypes:true})) {
    const name=path.join(dir,entry.name),info=await fs.promises.lstat(name);
    size+=info.isDirectory()?await logicalBytes(name):info.size;
  }
  return size;
}

test('atomic registry replacement stays within the aggregate peak budget',async t=>{
  const {parent}=await diskFixture(t);const caps={age:7,total:7002,reservation:3500,diagnostics:1};
  const policy=createRetention({store:createFileStore({parent}),clock:()=>100,limits:caps});
  const original=fs.promises.rename;let peak=0;
  try {
    fs.promises.rename=async(from,to)=>{
      if(to===path.join(parent,'registry.json'))peak=Math.max(peak,await logicalBytes(parent));
      await original(from,to);
      peak=Math.max(peak,await logicalBytes(parent));
    };
    assert.equal((await policy.reserve({id:'first'})).accepted,true);
    await writeFile(path.join(parent,'stage-first','artifact'),Buffer.alloc(3000,1));
    peak=Math.max(peak,await logicalBytes(parent));
    assert.equal((await policy.publish({id:'first',evidence:{staging:3000,metadata:0,diagnostics:0}})).published,true);
    const next=await policy.reserve({id:'second'});
    if(next.accepted) {
      await writeFile(path.join(parent,'stage-second','artifact'),Buffer.alloc(3000,2));
      peak=Math.max(peak,await logicalBytes(parent));
      const result=await policy.publish({id:'second',evidence:{staging:3000,metadata:0,diagnostics:0}});
      if(!result.published) {
        assert.deepEqual(result,{published:false});
        const staged=await readFile(path.join(parent,'stage-second','artifact')).catch(()=>null);
        const moved=await readFile(path.join(parent,'bundle-second','artifact')).catch(()=>null);
        assert.deepEqual(staged??moved,Buffer.alloc(3000,2),'refused publication preserves its bytes');
      }
    } else {
      assert.deepEqual(next,{accepted:false});
      assert.equal(await stat(path.join(parent,'stage-second')).then(()=>true,()=>false),false);
    }
    assert.deepEqual(await readFile(path.join(parent,'bundle-first','artifact')),Buffer.alloc(3000,1));
    assert.ok(peak<=caps.total,'peak physical file bytes must fit aggregate budget');
  } finally {fs.promises.rename=original;}
});

test('failed registry replacement preserves evidence across restart',async t=>{
  const {parent}=await diskFixture(t);const caps={age:7,total:12000,reservation:5000,diagnostics:1};
  const policy=createRetention({store:createFileStore({parent}),clock:()=>100,limits:caps});
  assert.equal((await policy.reserve({id:'first'})).accepted,true);
  await writeFile(path.join(parent,'stage-first','artifact'),Buffer.alloc(3000,1));
  assert.equal((await policy.publish({id:'first',evidence:{staging:3000,metadata:0,diagnostics:0}})).published,true);
  assert.equal((await policy.reserve({id:'second'})).accepted,true);
  await writeFile(path.join(parent,'stage-second','artifact'),Buffer.alloc(3000,2));
  const original=fs.promises.rename;let failed=false;
  try {
    fs.promises.rename=async(from,to)=>{
      if(to===path.join(parent,'registry.json')) {failed=true;throw Error('synthetic rename failure');}
      return original(from,to);
    };
    assert.equal((await policy.publish({id:'second',evidence:{staging:3000,metadata:0,diagnostics:0}})).published,false);
  } finally {fs.promises.rename=original;}
  assert.equal(failed,true);
  assert.deepEqual(await readFile(path.join(parent,'bundle-first','artifact')),Buffer.alloc(3000,1));
  const restarted=createRetention({store:createFileStore({parent}),clock:()=>100,limits:caps});
  assert.equal((await restarted.reserve({id:'third'})).accepted,false,'ambiguous publication cannot be adopted');
});

test('registry replacement stays within the per-run peak reservation',async t=>{
  const {parent}=await diskFixture(t);const caps={age:7,total:5000,reservation:600,diagnostics:1};
  const policy=createRetention({store:createFileStore({parent}),clock:()=>100,limits:caps});
  const original=fs.promises.rename;let peak=0;
  try {
    fs.promises.rename=async(from,to)=>{
      if(to===path.join(parent,'registry.json'))peak=Math.max(peak,await logicalBytes(parent));
      await original(from,to);peak=Math.max(peak,await logicalBytes(parent));
    };
    assert.equal((await policy.reserve({id:'only'})).accepted,true);
    await writeFile(path.join(parent,'stage-only','artifact'),Buffer.alloc(300,1));
    peak=Math.max(peak,await logicalBytes(parent));
    await policy.publish({id:'only',evidence:{staging:300,metadata:0,diagnostics:0}});
    assert.ok(peak<=caps.reservation,'one run including registry copies must fit its reservation');
  } finally {fs.promises.rename=original;}
});

test('invalid registry parent permissions are rejected',async t=>{
  const {parent}=await diskFixture(t);await fs.promises.chmod(parent,0o755);
  assert.throws(()=>createFileStore({parent}));
});
test('a replaced registry lock stops a transaction',async t=>{
  const {root,parent}=await diskFixture(t);const store=createFileStore({parent});
  await assert.rejects(store.locked(async()=>{
    await rename(path.join(parent,'lock'),path.join(root,'old-lock'));
    await mkdir(path.join(parent,'lock'),{mode:0o700});
    await store.read();
  }));
  assert.equal((await stat(path.join(parent,'lock'))).isDirectory(),true);
});
test('invalid registry data cannot be replaced',async t=>{
  const {parent}=await diskFixture(t);const file=path.join(parent,'registry.json');
  await writeFile(file,JSON.stringify({unexpected:true}));
  const policy=createRetention({store:createFileStore({parent}),clock:()=>now,limits:{...limits,total:20000,reservation:5000}});
  assert.equal((await policy.reserve({id:'run'})).accepted,false);
  assert.deepEqual(JSON.parse(await readFile(file,'utf8')),{unexpected:true});
});
test('a symlink within active staging blocks another admission',async t=>{
  const {root,parent}=await diskFixture(t);const caps={...limits,total:20000,reservation:5000};
  const policy=createRetention({store:createFileStore({parent}),clock:()=>now,limits:caps});
  assert.equal((await policy.reserve({id:'first'})).accepted,true);
  const target=path.join(root,'neighbor');await writeFile(target,'synthetic');
  await symlink(target,path.join(parent,'stage-first','link'));
  assert.equal((await policy.reserve({id:'second'})).accepted,false);
  assert.equal(await readFile(target,'utf8'),'synthetic');
});
test('publication rejects a filename containing a backslash',async t=>{
  const {parent}=await diskFixture(t);const caps={...limits,total:20000,reservation:5000};
  const policy=createRetention({store:createFileStore({parent}),clock:()=>now,limits:caps});
  assert.equal((await policy.reserve({id:'run'})).accepted,true);
  await writeFile(path.join(parent,'stage-run','bad\\\\name'),'synthetic');
  assert.equal((await policy.publish({id:'run',evidence:{staging:9,metadata:0,diagnostics:0}})).published,false);
});
test('retirement preserves a registered file changed in place',async t=>{
  const {parent}=await diskFixture(t);const caps={age:7,total:10002,reservation:10000,diagnostics:1};let clock=100;
  const policy=createRetention({store:createFileStore({parent}),clock:()=>clock,limits:caps});
  assert.equal((await policy.reserve({id:'old'})).accepted,true);
  await writeFile(path.join(parent,'stage-old','artifact'),'first');
  assert.equal((await policy.publish({id:'old',evidence:{staging:5,metadata:0,diagnostics:0}})).published,true);
  await writeFile(path.join(parent,'bundle-old','artifact'),'other');clock+=10;
  assert.equal((await policy.reserve({id:'new'})).accepted,false);
  assert.equal(await readFile(path.join(parent,'bundle-old','artifact'),'utf8'),'other');
});
test('corrupt fixed accounting fails closed without writing',async()=>{
  const {store,policy}=setup();store.fixedBytes=-1;
  assert.equal((await policy.reserve({id:'run'})).accepted,false);
  assert.deepEqual(store.snapshot(),[]);
});
test('duplicate registration fails without changing existing evidence',async()=>{
  const {store,policy}=setup([active('run')]);const before=store.snapshot();
  assert.equal((await policy.reserve({id:'run'})).accepted,false);
  assert.deepEqual(store.snapshot(),before);
});

test('multiple expired bundles retire on admission under pressure',async()=>{
  const entries=[completed('newer',80,8*DAY),completed('oldest',80,9*DAY)];
  const {store,policy}=setup(entries);
  assert.equal((await policy.reserve({id:'run'})).accepted,true);
  assert.deepEqual(store.snapshot().map(e=>e.id),['run']);
});

test('all eligible expired bundles retire on admission even when one would free enough capacity',async()=>{
  const entries=[completed('newer',80,8*DAY),completed('oldest',80,9*DAY)];
  const {store,policy}=setup(entries);
  assert.equal((await policy.reserve({id:'run'})).accepted,true);
  assert.deepEqual(store.snapshot().map(e=>e.id),['run']);
});
test('registry refuses oversized input without rewriting it',async t=>{
  const {parent}=await diskFixture(t);const registry=path.join(parent,'registry.json');
  const bytes=' '.repeat(1048577);await writeFile(registry,bytes);
  const store=createFileStore({parent});
  await assert.rejects(store.locked(()=>store.read()));
  assert.equal((await stat(registry)).size,bytes.length);
});
test('registry rejects a record without a valid identity',async t=>{
  const {parent}=await diskFixture(t);const registry=path.join(parent,'registry.json');
  const data=JSON.stringify([{...active('valid'),id:''}]);await writeFile(registry,data);
  const store=createFileStore({parent});
  await assert.rejects(store.locked(()=>store.read()));
  assert.equal(await readFile(registry,'utf8'),data);
});
test('registry peak refusal leaves existing registry untouched',async t=>{
  const {parent}=await diskFixture(t);const store=createFileStore({parent});
  await assert.rejects(store.locked(()=>store.write([],{total:1})));
  assert.equal(await stat(path.join(parent,'registry.json')).then(()=>true,()=>false),false);
});
test('a registry parent replaced before locking is not adopted',async t=>{
  const {root,parent}=await diskFixture(t);const store=createFileStore({parent});
  await rename(parent,path.join(root,'old-parent'));await mkdir(parent,{mode:0o700});
  await assert.rejects(store.locked(()=>store.read()));
  assert.deepEqual(await fs.promises.readdir(parent),[]);
});
test('registered staging replacement during publication is preserved',async t=>{
  const {root,parent}=await diskFixture(t);const caps={age:7,total:20000,reservation:5000,diagnostics:1};
  const store=createFileStore({parent});const policy=createRetention({store,clock:()=>100,limits:caps});
  assert.equal((await policy.reserve({id:'run'})).accepted,true);
  const stage=path.join(parent,'stage-run');await writeFile(path.join(stage,'artifact'),'synthetic');
  const step=store.step;let swapped=false;
  store.step=async phase=>{
    await step(phase);
    if(!swapped){await rename(stage,path.join(root,'original-stage'));await mkdir(stage,{mode:0o700});await writeFile(path.join(stage,'artifact'),'new bytes');swapped=true;}
  };
  assert.equal((await policy.publish({id:'run',evidence:{staging:9,metadata:0,diagnostics:0}})).published,false);
  assert.equal(swapped,true);assert.equal(await readFile(path.join(stage,'artifact'),'utf8'),'new bytes');
});
test('publication refuses unreserved registry metadata without moving staging',async t=>{
  const {parent}=await diskFixture(t);const caps={age:7,total:20000,reservation:350,diagnostics:1};
  const policy=createRetention({store:createFileStore({parent}),clock:()=>100,limits:caps});
  assert.equal((await policy.reserve({id:'run'})).accepted,true);
  await writeFile(path.join(parent,'stage-run','artifact'),Buffer.alloc(150));
  assert.equal((await policy.publish({id:'run',evidence:{staging:150,metadata:0,diagnostics:0}})).published,false);
  assert.equal((await stat(path.join(parent,'stage-run','artifact'))).size,150);
});

test('retirement preserves a file changed in place after bundle rename',async t=>{
  const {parent}=await diskFixture(t);const caps={age:7,total:5250,reservation:5000,diagnostics:1};let clock=100;
  const policy=createRetention({store:createFileStore({parent}),clock:()=>clock,limits:caps});
  assert.equal((await policy.reserve({id:'owned'})).accepted,true);
  await writeFile(path.join(parent,'stage-owned','witness.txt'),'initial');
  assert.equal((await policy.publish({id:'owned',evidence:{staging:7,metadata:0,diagnostics:0}})).published,true);
  const original=fs.promises.rename;let swapped=false;
  try {
    fs.promises.rename=async(from,to)=>{
      await original(from,to);
      if(from===path.join(parent,'bundle-owned')) {await writeFile(path.join(to,'witness.txt'),'changed');swapped=true;}
    };
    clock+=10;
    assert.equal((await policy.reserve({id:'next'})).accepted,false);
    assert.equal(swapped,true);
    assert.equal(await readFile(path.join(parent,'retire-owned','witness.txt'),'utf8'),'changed');
  } finally {fs.promises.rename=original;}
});

test('physical accounting rejects an unknown file type without removing it',async t=>{
  const {parent}=await diskFixture(t);const caps={age:7,total:20000,reservation:5000,diagnostics:1};
  const policy=createRetention({store:createFileStore({parent}),clock:()=>100,limits:caps});
  assert.equal((await policy.reserve({id:'first'})).accepted,true);
  const file=path.join(parent,'stage-first','artifact');await writeFile(file,'synthetic');
  const original=fs.promises.lstat;
  try {
    fs.promises.lstat=async p=>{
      const st=await original(p);
      return p===file?{...st,isDirectory:()=>false,isFile:()=>false,isSymbolicLink:()=>false}:st;
    };
    assert.equal((await policy.reserve({id:'next'})).accepted,false);
  } finally {fs.promises.lstat=original;}
  assert.equal(await readFile(file,'utf8'),'synthetic');
});

test('overflowing aggregate accounting refuses admission',async()=>{
  const {store,policy}=setup([active('run')]);store.fixedBytes=Number.MAX_SAFE_INTEGER;
  const before=store.snapshot();assert.equal((await policy.reserve({id:'next'})).accepted,false);
  assert.deepEqual(store.snapshot(),before);
});
test('unregistered retirement is refused without deleting a neighbor',async t=>{
  const {root,parent}=await diskFixture(t);const marker=path.join(root,'neighbor');await writeFile(marker,'synthetic');
  const store=createFileStore({parent});
  await assert.rejects(store.locked(()=>store.retire(completed('unknown',1),{total:20000})));
  assert.equal(await readFile(marker,'utf8'),'synthetic');
});
test('admission rejects a registered stage if registry overhead exhausts the budget',async t=>{
  const {parent}=await diskFixture(t);const store=createFileStore({parent});
  await assert.rejects(store.locked(async()=>{
    const identity=await store.prepare('run');
    await store.admit([{...active('run'),...identity,reserved:5000}],{total:1});
  }));
  assert.equal((await stat(path.join(parent,'stage-run'))).isDirectory(),true);
});
test('retirement refuses a manifest naming a different file',async t=>{
  const {parent}=await diskFixture(t);const caps={age:7,total:5250,reservation:5000,diagnostics:1};let clock=100;
  const store=createFileStore({parent}),policy=createRetention({store,clock:()=>clock,limits:caps});
  assert.equal((await policy.reserve({id:'old'})).accepted,true);
  await writeFile(path.join(parent,'stage-old','artifact'),'synthetic');
  assert.equal((await policy.publish({id:'old',evidence:{staging:9,metadata:0,diagnostics:0}})).published,true);
  const registry=path.join(parent,'registry.json');const records=JSON.parse(await readFile(registry,'utf8'));
  records[0].artifacts[0].name='different';await writeFile(registry,JSON.stringify(records));clock+=10;
  assert.equal((await policy.reserve({id:'new'})).accepted,false);
  assert.equal(await readFile(path.join(parent,'bundle-old','artifact'),'utf8'),'synthetic');
});
test('publication refuses a newly occupied bundle destination',async t=>{
  const {root,parent}=await diskFixture(t);const caps={age:7,total:20000,reservation:5000,diagnostics:1};
  const store=createFileStore({parent}),policy=createRetention({store,clock:()=>100,limits:caps});
  assert.equal((await policy.reserve({id:'run'})).accepted,true);
  await writeFile(path.join(parent,'stage-run','artifact'),'synthetic');
  const step=store.step;let created=false;
  store.step=async phase=>{await step(phase);if(!created){await mkdir(path.join(parent,'bundle-run'),{mode:0o700});await writeFile(path.join(parent,'bundle-run','neighbor'),'synthetic neighbor');created=true;}};
  assert.equal((await policy.publish({id:'run',evidence:{staging:9,metadata:0,diagnostics:0}})).published,false);
  assert.equal(created,true);assert.equal(await readFile(path.join(parent,'bundle-run','neighbor'),'utf8'),'synthetic neighbor');
  assert.equal((await stat(root)).isDirectory(),true);
});

test('physical size overflow refuses admission and preserves files',async t=>{
  const {parent}=await diskFixture(t);const caps={age:7,total:20000,reservation:5000,diagnostics:1};
  const policy=createRetention({store:createFileStore({parent}),clock:()=>100,limits:caps});
  assert.equal((await policy.reserve({id:'first'})).accepted,true);
  const file=path.join(parent,'stage-first','artifact');await writeFile(file,'synthetic');
  const original=fs.promises.lstat;
  try {
    fs.promises.lstat=async p=>{
      const st=await original(p);
      return p===file?{...st,size:Number.MAX_SAFE_INTEGER,isDirectory:()=>false,isFile:()=>true,isSymbolicLink:()=>false}:st;
    };
    assert.equal((await policy.reserve({id:'second'})).accepted,false);
  } finally {fs.promises.lstat=original;}
  assert.equal(await readFile(file,'utf8'),'synthetic');
});
test('a directory in staging cannot be published as an artifact',async t=>{
  const {parent}=await diskFixture(t);const caps={age:7,total:20000,reservation:5000,diagnostics:1};
  const policy=createRetention({store:createFileStore({parent}),clock:()=>100,limits:caps});
  assert.equal((await policy.reserve({id:'run'})).accepted,true);
  await mkdir(path.join(parent,'stage-run','unknown'),{mode:0o700});
  assert.equal((await policy.publish({id:'run',evidence:{staging:0,metadata:0,diagnostics:0}})).published,false);
  assert.equal((await stat(path.join(parent,'stage-run','unknown'))).isDirectory(),true);
});
test('a staging directory replaced after file sync is not published',async t=>{
  const {root,parent}=await diskFixture(t);const caps={age:7,total:20000,reservation:5000,diagnostics:1};
  const policy=createRetention({store:createFileStore({parent}),clock:()=>100,limits:caps});
  assert.equal((await policy.reserve({id:'run'})).accepted,true);
  const stage=path.join(parent,'stage-run');await writeFile(path.join(stage,'artifact'),'synthetic');
  const original=fs.promises.open;let swapped=false;
  try {
    fs.promises.open=async(name,flags,...rest)=>{
      const handle=await original(name,flags,...rest);
      if(name===stage && (flags&fs.constants.O_DIRECTORY)){
        await rename(stage,path.join(root,'old-stage'));await mkdir(stage,{mode:0o700});
        await writeFile(path.join(stage,'artifact'),'new bytes');swapped=true;
      }
      return handle;
    };
    assert.equal((await policy.publish({id:'run',evidence:{staging:9,metadata:0,diagnostics:0}})).published,false);
  } finally {fs.promises.open=original;}
  assert.equal(swapped,true);assert.equal(await readFile(path.join(stage,'artifact'),'utf8'),'new bytes');
});
test('a registry metadata read failure refuses a write',async t=>{
  const {parent}=await diskFixture(t);const caps={age:7,total:20000,reservation:5000,diagnostics:1};
  const store=createFileStore({parent}),policy=createRetention({store,clock:()=>100,limits:caps});
  assert.equal((await policy.reserve({id:'first'})).accepted,true);
  const registry=path.join(parent,'registry.json'),before=await readFile(registry,'utf8');
  const original=fs.promises.lstat;let seen=false,failed=false;
  try {
    fs.promises.lstat=async p=>{
      if(p===registry){if(seen){failed=true;const err=Error('synthetic I/O');err.code='EIO';throw err;}seen=true;}
      return original(p);
    };
    assert.equal((await policy.reserve({id:'second'})).accepted,false);
  } finally {fs.promises.lstat=original;}
  assert.equal(failed,true);assert.equal(await readFile(registry,'utf8'),before);
});

async function completedBundle(t) {
  const {root,parent}=await diskFixture(t);const caps={age:7,total:5250,reservation:5000,diagnostics:1};let clock=100;
  const store=createFileStore({parent}),policy=createRetention({store,clock:()=>clock,limits:caps});
  assert.equal((await policy.reserve({id:'old'})).accepted,true);
  await writeFile(path.join(parent,'stage-old','artifact'),'synthetic');
  assert.equal((await policy.publish({id:'old',evidence:{staging:9,metadata:0,diagnostics:0}})).published,true);
  return {root,parent,store,policy,advance:()=>{clock+=10;}};
}

test('retirement refuses a bundle replaced after registry validation',async t=>{
  const {root,parent,store}=await completedBundle(t),bundle=path.join(parent,'bundle-old');
  const [entry]=await store.locked(()=>store.read());
  const original=fs.promises.lstat;let swapped=false;
  try {
    fs.promises.lstat=async p=>{
      const st=await original(p);
      if(p===bundle&&!swapped){await rename(bundle,path.join(root,'original-bundle'));await mkdir(bundle,{mode:0o700});await writeFile(path.join(bundle,'marker'),'synthetic');swapped=true;}
      return st;
    };
    await assert.rejects(store.locked(()=>store.retire(entry,{total:5250})));
  } finally {fs.promises.lstat=original;}
  assert.equal(swapped,true);assert.equal(await readFile(path.join(bundle,'marker'),'utf8'),'synthetic');
});

test('retirement refuses a tomb that appears after bundle inspection',async t=>{
  const {parent,policy,advance}=await completedBundle(t),bundle=path.join(parent,'bundle-old'),tomb=path.join(parent,'retire-old');
  const original=fs.promises.readdir;let created=false;
  try {
    fs.promises.readdir=async(p,...args)=>{
      const names=await original(p,...args);
      if(p===bundle&&!created){await mkdir(tomb,{mode:0o700});await writeFile(path.join(tomb,'marker'),'synthetic');created=true;}
      return names;
    };
    advance();assert.equal((await policy.reserve({id:'new'})).accepted,false);
  } finally {fs.promises.readdir=original;}
  assert.equal(created,true);assert.equal(await readFile(path.join(tomb,'marker'),'utf8'),'synthetic');
  assert.equal(await readFile(path.join(bundle,'artifact'),'utf8'),'synthetic');
});

test('retirement refuses an unreadable tomb destination',async t=>{
  const {parent,policy,advance}=await completedBundle(t),tomb=path.join(parent,'retire-old');
  const original=fs.promises.lstat;let failed=false;
  try {
    fs.promises.lstat=async p=>{if(p===tomb){failed=true;const err=Error('synthetic I/O');err.code='EIO';throw err;}return original(p);};
    advance();assert.equal((await policy.reserve({id:'new'})).accepted,false);
  } finally {fs.promises.lstat=original;}
  assert.equal(failed,true);assert.equal(await readFile(path.join(parent,'bundle-old','artifact'),'utf8'),'synthetic');
});

test('retirement preserves a bundle replaced immediately before rename',async t=>{
  const {root,parent,policy,advance}=await completedBundle(t),bundle=path.join(parent,'bundle-old'),tomb=path.join(parent,'retire-old');
  const original=fs.promises.lstat;let swapped=false;
  try {
    fs.promises.lstat=async p=>{
      if(p===tomb&&!swapped){
        try{return await original(p);}catch(err){if(err.code!=='ENOENT')throw err;await rename(bundle,path.join(root,'original-bundle'));await mkdir(bundle,{mode:0o700});await writeFile(path.join(bundle,'marker'),'synthetic');swapped=true;throw err;}
      }
      return original(p);
    };
    advance();assert.equal((await policy.reserve({id:'new'})).accepted,false);
  } finally {fs.promises.lstat=original;}
  assert.equal(swapped,true);assert.equal(await readFile(path.join(bundle,'marker'),'utf8'),'synthetic');
});

test('retirement preserves a replacement at the final file identity check',async t=>{
  const {root,parent,policy,advance}=await completedBundle(t),file=path.join(parent,'retire-old','artifact');
  const original=fs.promises.lstat;let swapped=false;
  try {
    fs.promises.lstat=async p=>{
      if(p===file&&!swapped){await rename(file,path.join(root,'original-file'));await writeFile(file,'replacement');swapped=true;}
      return original(p);
    };
    advance();assert.equal((await policy.reserve({id:'new'})).accepted,false);
  } finally {fs.promises.lstat=original;}
  assert.equal(swapped,true);assert.equal(await readFile(file,'utf8'),'replacement');
});

test('retirement preserves a tomb replaced after its verified file removal',async t=>{
  const {root,parent,policy,advance}=await completedBundle(t),tomb=path.join(parent,'retire-old'),file=path.join(tomb,'artifact');
  const original=fs.promises.unlink;let swapped=false;
  try {
    fs.promises.unlink=async p=>{
      await original(p);
      if(p===file){await rename(tomb,path.join(root,'original-tomb'));await mkdir(tomb,{mode:0o700});await writeFile(path.join(tomb,'marker'),'synthetic');swapped=true;}
    };
    advance();assert.equal((await policy.reserve({id:'new'})).accepted,false);
  } finally {fs.promises.unlink=original;}
  assert.equal(swapped,true);assert.equal(await readFile(path.join(tomb,'marker'),'utf8'),'synthetic');
});

test('registry inspection recognizes the owned tomb during retirement',async t=>{
  const {parent,store,policy,advance}=await completedBundle(t),tomb=path.join(parent,'retire-old');
  const original=fs.promises.open;let inspected=false;
  try {
    fs.promises.open=async(name,flags,...args)=>{
      if(name===parent && (flags&fs.constants.O_DIRECTORY) && fs.existsSync(tomb) && !inspected){
        inspected=true;const records=await store.read();assert.equal(records[0].id,'old');
      }
      return original(name,flags,...args);
    };
    advance();assert.equal((await policy.reserve({id:'new'})).accepted,true);
  } finally {fs.promises.open=original;}
  assert.equal(inspected,true);
});

test('publication refuses a staged artifact changed after audit',async t=>{
  const crypto=await import('node:crypto');const {parent}=await diskFixture(t);
  const diskLimits={...limits,total:20000,reservation:5000};
  const policy=createRetention({store:createFileStore({parent}),clock:()=>now,limits:diskLimits});
  assert.equal((await policy.reserve({id:'run'})).accepted,true);
  const file=path.join(parent,'stage-run','artifact');await writeFile(file,'synthetic');
  const info=await stat(file);const expected=[{name:'artifact',dev:info.dev,ino:info.ino,size:info.size,sha256:crypto.createHash('sha256').update('synthetic').digest('hex')}];
  await writeFile(file,'changed!!');
  const result=await policy.publish({id:'run',evidence:{staging:9,metadata:0,diagnostics:0,expected}});
  assert.equal(result.published,false);assert.equal(await readFile(file,'utf8'),'changed!!');
  assert.equal(await stat(path.join(parent,'bundle-run')).then(()=>true,()=>false),false);
});

test('registry refuses an encoded replacement beyond its readable bound',async t=>{
  const {parent}=await diskFixture(t);const store=createFileStore({parent});
  await store.locked(async()=>{
    const prepared=await store.prepare('run');
    const entry={...active('run'),reserved:2*1024*1024,...prepared,syntheticPadding:'x'.repeat(1048576)};
    await assert.rejects(store.write([entry],{total:6*1024*1024},'run'));
  });
  assert.equal(await stat(path.join(parent,'registry.json')).then(()=>true,()=>false),false);
  assert.equal((await stat(path.join(parent,'stage-run'))).isDirectory(),true);
});
