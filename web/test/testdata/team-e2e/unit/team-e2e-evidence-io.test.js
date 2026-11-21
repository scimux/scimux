import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import os from 'node:os';
import {captureIdentity} from '../capture-identity.cjs';
import {createEvidenceIO} from '../evidence-io.cjs';

const id=st=>({uid:st.uid,dev:st.dev,ino:st.ino});
async function fixture(t) {
  const base=await fs.mkdtemp(path.join(os.tmpdir(),'evidence-io-synthetic-'));t.after(()=>fs.rm(base,{recursive:true,force:true}));
  const runtime=path.join(base,'runtime'),stage=path.join(base,'stage'),neighbor=path.join(base,'neighbor');
  for(const p of [runtime,stage,neighbor])await fs.mkdir(p,{mode:0o700});
  await fs.writeFile(path.join(neighbor,'marker'),'synthetic');
  const screenshot=path.join(runtime,'screen.png');await fs.writeFile(screenshot,'synthetic screenshot');
  await fs.writeFile(path.join(runtime,'capture-identities.private.json'),JSON.stringify({'screen.png':await captureIdentity(screenshot)}));
  await fs.writeFile(path.join(runtime,'manifest.json'),JSON.stringify({version:1,scope:'synthetic',result:'pass',assertions:[],artifacts:['screen.png'],counters:{}}));
  return {runtime,stage,neighbor,runtimeIdentity:id(await fs.lstat(runtime)),stageIdentity:id(await fs.lstat(stage))};
}

test('evidence adapter reads only registered captures and writes into bound staging',async t=>{
  const f=await fixture(t);const loaded=await createEvidenceIO(f).load();
  assert.ok(loaded,'registered evidence adapter loads');
  assert.equal(loaded.source.manifest.result,'pass');
  const screen=await loaded.io.read('screen.png');assert.equal(screen.captureIdentity,screen.identity);
  assert.equal(screen.bytes.toString(),'synthetic screenshot');
  await loaded.io.write('manifest.json',Buffer.from('synthetic staged'));
  assert.equal(await fs.readFile(path.join(f.stage,'manifest.json'),'utf8'),'synthetic staged');
  assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

test('evidence adapter refuses a source capture replaced after registration',async t=>{
  const f=await fixture(t);await fs.writeFile(path.join(f.runtime,'screen.png'),'changed');
  const loaded=await createEvidenceIO(f).load();assert.ok(loaded,'registered evidence adapter loads');const file=await loaded.io.read('screen.png');
  assert.notEqual(file.captureIdentity,file.identity);
  assert.deepEqual(await fs.readdir(f.stage),[]);
});

test('evidence adapter rejects a replaced staging root before writing',async t=>{
  const f=await fixture(t);const loaded=await createEvidenceIO(f).load();
  assert.ok(loaded,'registered evidence adapter loads');
  await fs.rename(f.stage,f.stage+'-old');await fs.mkdir(f.stage,{mode:0o700});
  await assert.rejects(loaded.io.write('manifest.json',Buffer.from('synthetic')));
  assert.deepEqual(await fs.readdir(f.stage),[]);assert.deepEqual(await fs.readdir(f.stage+'-old'),[]);
});

test('evidence adapter refuses symlinked runtime records',async t=>{
  const f=await fixture(t);await fs.unlink(path.join(f.runtime,'manifest.json'));
  await fs.symlink(path.join(f.neighbor,'marker'),path.join(f.runtime,'manifest.json'));
  assert.equal(await createEvidenceIO(f).load(),null);
  assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

test('evidence file reader rejects an unsafe name before filesystem access',async t=>{
  const f=await fixture(t),adapter=createEvidenceIO(f);await assert.rejects(adapter.read('../neighbor/marker',1024));
  assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

test('failed run without screenshots loads without inventing a capture index',async t=>{
  const f=await fixture(t);
  await fs.unlink(path.join(f.runtime,'screen.png'));await fs.unlink(path.join(f.runtime,'capture-identities.private.json'));
  await fs.writeFile(path.join(f.runtime,'manifest.json'),JSON.stringify({version:1,scope:'cleanup probe only',result:'fail',assertions:[],artifacts:[],counters:{}}));
  const loaded=await createEvidenceIO(f).load();
  assert.ok(loaded);assert.deepEqual(loaded.source.manifest.artifacts,[]);assert.deepEqual(loaded.source.files,{});
});

test('staging snapshot binds each file identity, size and digest',async t=>{
  const crypto=await import('node:crypto');const f=await fixture(t);const loaded=await createEvidenceIO(f).load();assert.ok(loaded);
  await loaded.io.write('manifest.json',Buffer.from('synthetic'));
  const info=await fs.lstat(path.join(f.stage,'manifest.json'));
  assert.deepEqual(await loaded.io.snapshot(),[{name:'manifest.json',dev:info.dev,ino:info.ino,size:info.size,sha256:crypto.createHash('sha256').update('synthetic').digest('hex')}]);
});

test('evidence adapter accounts for raw manifest and capture-index bytes',async t=>{
  const f=await fixture(t);const loaded=await createEvidenceIO(f).load();assert.ok(loaded);
  const manifest=await fs.stat(path.join(f.runtime,'manifest.json')),index=await fs.stat(path.join(f.runtime,'capture-identities.private.json'));
  assert.equal(loaded.source.rawMetadataBytes,manifest.size+index.size);
});

test('manifest growth after inspection cannot cause an unbounded evidence read',async t=>{
  const f=await fixture(t);const manifest=path.join(f.runtime,'manifest.json');
  const originalOpen=fs.open;let bytesRead=0,injected=false;
  fs.open=async (...args)=>{
    const handle=await originalOpen(...args);
    if(args[0]!==manifest)return handle;
    return new Proxy(handle,{get(target,property){
      if(property==='stat')return async (...values)=>{
        const stat=await target.stat(...values);
        if(!injected){injected=true;await fs.appendFile(manifest,Buffer.alloc(16*1024*1024+8192,0x20));}
        return stat;
      };
      if(property==='readFile')return async (...values)=>{const bytes=await target.readFile(...values);bytesRead+=bytes.length;return bytes;};
      if(property==='read')return async (...values)=>{const result=await target.read(...values);bytesRead+=result.bytesRead;return result;};
      const value=target[property];return typeof value==='function'?value.bind(target):value;
    }});
  };
  try {
    assert.equal(await createEvidenceIO(f).load(),null);
    assert.equal(injected,true);
    assert.ok(bytesRead<=16*1024*1024+1,`read ${bytesRead} bytes beyond the manifest bound`);
  } finally {fs.open=originalOpen;}
});

for(const size of [65537,16*1024*1024]) test(`valid manifest of ${size} bytes is read completely`,async t=>{
  const f=await fixture(t);const file=path.join(f.runtime,'manifest.json');
  const json=JSON.stringify({version:1,scope:'synthetic',result:'pass',assertions:[],artifacts:[],counters:{}});
  await fs.writeFile(file,json+' '.repeat(size-Buffer.byteLength(json)));
  const loaded=await createEvidenceIO(f).load();assert.ok(loaded);
  assert.equal(loaded.source.rawMetadataBytes,size+(await fs.stat(path.join(f.runtime,'capture-identities.private.json'))).size);
});

test('empty registered capture is read without inventing bytes',async t=>{
  const f=await fixture(t);const file=path.join(f.runtime,'screen.png');await fs.writeFile(file,'');
  await fs.writeFile(path.join(f.runtime,'capture-identities.private.json'),JSON.stringify({'screen.png':await captureIdentity(file)}));
  const loaded=await createEvidenceIO(f).load();assert.ok(loaded);
  const capture=await loaded.io.read('screen.png');assert.equal(capture.bytes.length,0);assert.equal(capture.identity,capture.captureIdentity);
});

for(const failure of ['truncate','read-error']) test(`evidence read rejects ${failure} after opening the manifest`,async t=>{
  const f=await fixture(t);const manifest=path.join(f.runtime,'manifest.json'),originalOpen=fs.open;let injected=false;
  fs.open=async (...args)=>{
    const handle=await originalOpen(...args);if(args[0]!==manifest)return handle;
    return new Proxy(handle,{get(target,property){
      if(property==='read')return async (...values)=>{
        if(!injected){injected=true;if(failure==='read-error')throw Error('synthetic read failure');await fs.truncate(manifest,0);}
        return target.read(...values);
      };
      const value=target[property];return typeof value==='function'?value.bind(target):value;
    }});
  };
  try {assert.equal(await createEvidenceIO(f).load(),null);assert.equal(injected,true);}
  finally {fs.open=originalOpen;}
});

for(const change of ['opened-change','post-read-timestamp','post-read-replacement']) test(`evidence read rejects ${change}`,async t=>{
  const f=await fixture(t);const manifest=path.join(f.runtime,'manifest.json'),originalOpen=fs.open;let injected=false;
  fs.open=async (...args)=>{
    const handle=await originalOpen(...args);if(args[0]!==manifest)return handle;
    if(change==='opened-change'){injected=true;await fs.appendFile(manifest,' ');}
    return new Proxy(handle,{get(target,property){
      if(property==='read')return async (...values)=>{
        const result=await target.read(...values);
        if(!result.bytesRead&&!injected){
          injected=true;
          if(change==='post-read-timestamp')await fs.utimes(manifest,1,1);
          else {await fs.rename(manifest,manifest+'-old');await fs.writeFile(manifest,'synthetic replacement');}
        }
        return result;
      };
      const value=target[property];return typeof value==='function'?value.bind(target):value;
    }});
  };
  try {assert.equal(await createEvidenceIO(f).load(),null);assert.equal(injected,true);}
  finally {fs.open=originalOpen;}
});

test('runtime diagnostic log bytes are accounted or evidence loading refuses them',async t=>{
  const f=await fixture(t);const log=path.join(f.runtime,'synthetic.log');
  const handle=await fs.open(log,'w',0o600);
  try {await handle.truncate(1024*1024+1);} finally {await handle.close();}
  const loaded=await createEvidenceIO(f).load();
  if(loaded)assert.ok(loaded.source.rawMetadataBytes>=1024*1024+1,'raw runtime log bytes must count toward evidence capacity');
});

for(const sizes of [[17,23],[512*1024,512*1024]]) test(`diagnostic logs totaling ${sizes[0]+sizes[1]} bytes are admitted and counted once`,async t=>{
  const f=await fixture(t);let expected=0;
  for(const [i,size] of sizes.entries()) {const handle=await fs.open(path.join(f.runtime,`log-${i}.log`),'w',0o600);try {await handle.truncate(size);}finally {await handle.close();}expected+=size;}
  const originalOpen=fs.open;
  fs.open=async (...args)=>{
    const handle=await originalOpen(...args);if(!String(args[0]).endsWith('.log'))return handle;
    return new Proxy(handle,{get(target,property){if(property==='read'||property==='readFile')throw Error('diagnostic content must remain unread');const value=target[property];return typeof value==='function'?value.bind(target):value;}});
  };
  try {
    const loaded=await createEvidenceIO(f).load();assert.ok(loaded);assert.equal(loaded.source.rawDiagnosticBytes,expected);
    const manifest=await fs.stat(path.join(f.runtime,'manifest.json')),index=await fs.stat(path.join(f.runtime,'capture-identities.private.json'));
    assert.equal(loaded.source.rawMetadataBytes,manifest.size+index.size+expected);
  } finally {fs.open=originalOpen;}
});

for(const kind of ['symlink','directory','unsafe-name','open-error','replacement']) test(`diagnostic admission refuses ${kind}`,async t=>{
  const f=await fixture(t),file=path.join(f.runtime,kind==='unsafe-name'?'Bad.log':'synthetic.log');
  if(kind==='symlink')await fs.symlink(path.join(f.neighbor,'marker'),file);
  else if(kind==='directory')await fs.mkdir(file);
  else await fs.writeFile(file,'synthetic');
  const originalOpen=fs.open;
  if(kind==='open-error'||kind==='replacement')fs.open=async (...args)=>{
    if(args[0]===file&&kind==='open-error')throw Error('synthetic open error');
    const handle=await originalOpen(...args);
    if(args[0]===file&&kind==='replacement'){await fs.rename(file,file+'-old');await fs.writeFile(file,'synthetic');}
    return handle;
  };
  try {assert.equal(await createEvidenceIO(f).load(),null);}
  finally {fs.open=originalOpen;}
  assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

test('admitted raw diagnostics contribute to projection peak without being staged',async t=>{
  const {projectEvidence}=await import('../evidence-project.cjs');const f=await fixture(t);
  await fs.writeFile(path.join(f.runtime,'synthetic.log'),Buffer.alloc(400,0x78));
  const loaded=await createEvidenceIO({...f,publish:async()=>({published:true})}).load();assert.ok(loaded);
  const options={...loaded,proof:{waited:true,clean:true},limits:{run:400,aggregate:4096,diagnostics:1024,registry:0},usage:{staging:0,registryCopies:0,diagnostics:0,otherRuns:0},auditor:async()=>({ok:true})};
  const refused=await projectEvidence(options);assert.equal(refused.ok,false);assert.equal(refused.category,'capacity');assert.deepEqual(await fs.readdir(f.stage),[]);
  options.limits.run=4096;
  const admitted=await projectEvidence(options);assert.equal(admitted.ok,true);
  assert.deepEqual((await fs.readdir(f.stage)).sort(),['manifest.json','screen.png']);
});

for(const [label,manifest,index] of [
  ['invalid manifest',{},'{}'],['invalid index',{artifacts:[]},'[]'],
  ['missing screenshot index',{artifacts:['screen.png']},null],['invalid name',{artifacts:['unsafe.txt']},'{}'],
  ['duplicate artifact',{artifacts:['screen.png','screen.png']},'{}'],['missing artifact',{artifacts:['missing.png']},'{}'],
]) test(`evidence adapter rejects ${label}`,async t=>{
  const f=await fixture(t);await fs.writeFile(path.join(f.runtime,'manifest.json'),JSON.stringify(manifest));
  const file=path.join(f.runtime,'capture-identities.private.json');if(index===null)await fs.unlink(file);else await fs.writeFile(file,index);
  assert.equal(await createEvidenceIO(f).load(),null);
});

test('evidence adapter accepts and reads registered coverage and rejects unregistered operations',async t=>{
  const f=await fixture(t);await fs.writeFile(path.join(f.runtime,'browser-coverage.json'),'[]');
  await fs.writeFile(path.join(f.runtime,'manifest.json'),JSON.stringify({version:1,scope:'synthetic',result:'pass',assertions:[],artifacts:['browser-coverage.json'],counters:{}}));
  const loaded=await createEvidenceIO(f).load();assert.ok(loaded);assert.equal((await loaded.io.read('browser-coverage.json')).bytes.toString(),'[]');
  await assert.rejects(loaded.io.read('unknown.png'));await assert.rejects(loaded.io.write('unsafe.txt',Buffer.from('x')));
  await assert.rejects(loaded.io.write('manifest.json','not bytes'));
  await fs.writeFile(path.join(f.stage,'unsafe.txt'),'synthetic');await assert.rejects(loaded.io.snapshot());
});

test('evidence adapter snapshot rejects staged symlink',async t=>{
  const f=await fixture(t),loaded=await createEvidenceIO(f).load();assert.ok(loaded);
  await fs.symlink(path.join(f.neighbor,'marker'),path.join(f.stage,'screen.png'));await assert.rejects(loaded.io.snapshot());
  assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

test('evidence adapter refuses a capture changed between reading and re-identification',async t=>{
  const f=await fixture(t),loaded=await createEvidenceIO(f).load();assert.ok(loaded);
  const file=path.join(f.runtime,'screen.png'),open=fs.open;let opens=0,injected=false;
  fs.open=async(...args)=>{
    const handle=await open(...args);if(args[0]!==file||++opens!==2)return handle;
    return new Proxy(handle,{get(target,key){if(key==='close')return async()=>{await target.close();await fs.writeFile(file,'changed screenshot!!');injected=true;};const value=target[key];return typeof value==='function'?value.bind(target):value;}});
  };
  try {await assert.rejects(loaded.io.read('screen.png'));assert.equal(injected,true);}
  finally{fs.open=open;}
});

test('staging snapshot refuses a file on a different synthetic device',async t=>{
  const f=await fixture(t),loaded=await createEvidenceIO(f).load();assert.ok(loaded);const file=path.join(f.stage,'screen.png');
  await fs.writeFile(file,'synthetic');const lstat=fs.lstat,open=fs.open;
  const changed=st=>new Proxy(st,{get(target,key){if(key==='dev')return target.dev+1;const value=target[key];return typeof value==='function'?value.bind(target):value;}});
  fs.lstat=async(name,...args)=>{const st=await lstat(name,...args);return name===file?changed(st):st;};
  fs.open=async(...args)=>{const handle=await open(...args);if(args[0]!==file)return handle;
    return new Proxy(handle,{get(target,key){if(key==='stat')return async(...values)=>changed(await target.stat(...values));const value=target[key];return typeof value==='function'?value.bind(target):value;}});};
  try {await assert.rejects(loaded.io.snapshot());}finally{fs.lstat=lstat;fs.open=open;}
  assert.equal(await fs.readFile(file,'utf8'),'synthetic');
});

test('capture registry rejects unidentifiable and unwritable registrations',async()=>{
  const {createCaptureRegistry}=await import('../capture-identity.cjs');
  const missing=createCaptureRegistry({identity:async()=>null,write:async()=>{throw Error('unexpected write');}});
  assert.equal(await missing.register('/synthetic/screen.png'),null);
  const failed=createCaptureRegistry({identity:async()=> 'synthetic',write:async()=>{throw Error('synthetic write failure');}});
  assert.equal(await failed.register('/synthetic/screen.png'),null);assert.equal(await failed.register('/synthetic/screen.png'),null);
});

test('capture identity refuses a missing file before opening a handle',async t=>{
  const base=await fs.mkdtemp(path.join(os.tmpdir(),'capture-missing-synthetic-'));t.after(()=>fs.rm(base,{recursive:true,force:true}));
  assert.equal(await captureIdentity(path.join(base,'missing.png')),null);
});

test('capture identity propagates synchronous handle close failure',async t=>{
  const base=await fs.mkdtemp(path.join(os.tmpdir(),'capture-close-synthetic-'));t.after(()=>fs.rm(base,{recursive:true,force:true}));
  const file=path.join(base,'screen.png');await fs.writeFile(file,'synthetic');const open=fs.open;
  fs.open=async(...args)=>{const handle=await open(...args);if(args[0]!==file)return handle;
    return new Proxy(handle,{get(target,key){if(key==='close')return ()=>{target.close();throw Error('synthetic synchronous close failure');};const value=target[key];return typeof value==='function'?value.bind(target):value;}});};
  try {await assert.rejects(captureIdentity(file),/synthetic synchronous close failure/);}finally{fs.open=open;}
});

for(const change of ['open','grow','truncate','read-error','close-error']) test(`capture identity refuses ${change} race or failure`,async t=>{
  const base=await fs.mkdtemp(path.join(os.tmpdir(),'capture-race-synthetic-'));t.after(()=>fs.rm(base,{recursive:true,force:true}));
  const file=path.join(base,'screen.png');await fs.writeFile(file,'synthetic');const open=fs.open;let injected=false;
  fs.open=async (...args)=>{
    const handle=await open(...args);if(args[0]!==file)return handle;
    if(change==='open')await fs.appendFile(file,'x');
    return new Proxy(handle,{get(target,property){
      if(property==='read')return async (...values)=>{
        if(!injected){injected=true;if(change==='grow')await fs.appendFile(file,'x');if(change==='truncate')await fs.truncate(file,0);if(change==='read-error')throw Error('synthetic read error');}
        return target.read(...values);
      };
      if(property==='close'&&change==='close-error')return async()=>{await target.close();throw Error('synthetic close error');};
      const value=target[property];return typeof value==='function'?value.bind(target):value;
    }});
  };
  try {const result=await captureIdentity(file);if(change==='close-error')assert.equal(typeof result,'string');else assert.equal(result,null);}
  finally {fs.open=open;}
});
