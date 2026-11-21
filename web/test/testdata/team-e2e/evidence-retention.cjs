'use strict';

// All store operations are serialized by the registry lock. A failed or
// ambiguous operation refuses admission; incomplete entries remain protected.
const amount=n=>Number.isSafeInteger(n)&&n>=0;
const validID=id=>typeof id==='string' && /^[A-Za-z0-9][A-Za-z0-9_-]*$/.test(id);
const validIdentity=value=>value==='matched'||(typeof value==='string'&&/^m[A-Za-z0-9_-]{16}$/.test(value));
function createRetention({store,clock,limits}) {
  async function reserve({id}) {
    try {
      return await store.locked(async()=>{
        const entries=await store.read();
        const now=clock();
        if(!validID(id) || !amount(now) || !amount(limits.age) || !amount(limits.total) || !amount(limits.reservation) || entries.some(e=>e.id===id || !validID(e.id) || !amount(e.created) || !e.registered || !validIdentity(e.identity) || !['active','complete'].includes(e.state) || !amount(e.state==='active'?e.reserved:e.bytes)))return {accepted:false};
        if(!amount(store.fixedBytes))return {accepted:false};
        let used=entries.reduce((sum,e)=>sum+(e.state==='active'?e.reserved:e.bytes),store.fixedBytes);
        if(!amount(used))return {accepted:false};
        const eligible=entries.filter(e=>e.state==='complete'&&!e.protected&&now-e.created>=limits.age).sort((a,b)=>a.created-b.created);
        const retired=[];
        let extra=await store.extra(entries);
        while(eligible.length) {
          const e=eligible.shift();retired.push(e);used-=e.bytes;
          extra=await store.extra(entries.filter(candidate=>!retired.includes(candidate)));
        }
        if(!amount(extra) || used+extra+limits.reservation>limits.total)return {accepted:false};
        const next=entries.filter(e=>!retired.includes(e));
        for(const e of retired)await store.retire(e,limits);
        const registration=await store.prepare(id);
        next.push({id,state:'active',bytes:0,reserved:limits.reservation,created:now,registered:true,identity:'matched',...registration});
        await store.admit(next,limits);
        await store.write(next,limits,id);
        return {accepted:true,witness:{id,stageDev:registration.stageDev,stageIno:registration.stageIno,identity:registration.identity}};
      });
    } catch { return {accepted:false}; }
  }
  async function publish({id,evidence}) {
    try {
      return await store.locked(async()=>{
        const entries=await store.read();
        const entry=entries.find(e=>e.id===id && e.state==='active');
        if(!entry || !amount(entry.reserved) || !amount(limits.diagnostics) || !amount(evidence.staging) || !amount(evidence.metadata) || !amount(evidence.diagnostics) || evidence.diagnostics>limits.diagnostics || evidence.staging+evidence.metadata+evidence.diagnostics>entry.reserved)return {published:false};
        for(const phase of ['stage-write','file-sync','rename','parent-sync','registry-commit'])await store.step(phase);
        const bytes=evidence.staging+evidence.metadata+evidence.diagnostics;
        try {await store.publish(id,{...entry,state:'complete',bytes},limits,evidence.expected);}
        catch(err) {
          await store.write(entries.map(e=>e.id===id?{...e,state:'incomplete',bytes}:e),limits,id);
          throw err;
        }
        return {published:true};
      });
    } catch { return {published:false}; }
  }
  async function recover() {
    try {return await store.locked(async()=>({recovered:!(await store.read()).some(e=>e.state==='incomplete')}));}
    catch {return {recovered:false};}
  }
  async function finalize({witness}) {
    try {
      return await store.locked(async()=>{
        const entries=await store.read(),entry=entries.find(e=>e.id===witness?.id);
        if(!entry||entry.state!=='active'||entry.protected||!entry.registered||!validIdentity(witness.identity)||witness.identity==='matched'||entry.identity!==witness.identity||entry.stageDev!==witness.stageDev||entry.stageIno!==witness.stageIno)return {finalized:false};
        await store.finalize(entry,limits);
        return {finalized:true};
      });
    }catch{return {finalized:false};}
  }
  return {reserve,publish,recover,finalize};
}
// The registry is scoped to an already private directory; a changed directory
// or ambiguous lock is never adopted or broken automatically.
function createFileStore({parent}) {
  const fs=require('node:fs');
  const fsp=fs.promises;
  const path=require('node:path');
  const crypto=require('node:crypto');
  const initial=fs.lstatSync(parent);
  if(!initial.isDirectory() || (initial.mode&0o777)!==0o700 || initial.uid!==process.getuid())throw Error('registry parent invalid');
  const same=(a,b)=>a.dev===b.dev && a.ino===b.ino && a.uid===b.uid && a.isDirectory() && (a.mode&0o777)===0o700;
  async function digest(file) {
    const hash=crypto.createHash('sha256'),buffer=Buffer.alloc(65536);
    for(;;) {const {bytesRead}=await file.read(buffer,0,buffer.length,null);if(!bytesRead)break;hash.update(buffer.subarray(0,bytesRead));}
    return hash.digest('hex');
  }
  let lockIdentity;
  const prepared=new Map();
  let transitioning;
  let retiring;
  let retired;
  async function verify() {
    if(!same(await fsp.lstat(parent),initial))throw Error('registry parent changed');
    if(!lockIdentity || !same(await fsp.lstat(path.join(parent,'lock')),lockIdentity))throw Error('registry lock changed');
  }
  async function read() {
    await verify();
    let file,value=[];
    try {file=await fsp.open(path.join(parent,'registry.json'),fs.constants.O_RDONLY|fs.constants.O_NOFOLLOW);}
    catch(err){if(err.code!=='ENOENT')throw err;}
    if(file) {
      try {
        const st=await file.stat();
        if(!st.isFile() || st.size>1048576)throw Error('registry invalid');
        value=JSON.parse(await file.readFile('utf8'));
        if(!Array.isArray(value))throw Error('registry invalid');
      } finally {await file.close();}
    }
    const expected=new Map(prepared);
    for(const entry of value) {
      if(!validID(entry.id) || !['active','complete'].includes(entry.state))throw Error('registry entry invalid');
      const isBundle=entry.state==='complete'||entry.id===transitioning;
      if(entry.id!==retired)expected.set((entry.id===retiring?'retire-':isBundle?'bundle-':'stage-')+entry.id,{dev:entry.stageDev,ino:entry.stageIno});
    }
    const allowed=new Set(['lock','registry.json',...expected.keys()]);
    for(const name of await fsp.readdir(parent))if(!allowed.has(name))throw Error('unknown registry entry');
    for(const [name,identity] of expected) {
      const st=await fsp.lstat(path.join(parent,name));
      if(!st.isDirectory() || st.uid!==process.getuid() || (st.mode&0o777)!==0o700 || st.dev!==identity.dev || st.ino!==identity.ino)throw Error('registry entry changed');
    }
    return value;
  }
  async function physicalBytes(dir) {
    let total=0;
    for(const name of await fsp.readdir(dir)) {
      const child=path.join(dir,name),st=await fsp.lstat(child);
      if(st.uid!==initial.uid || st.dev!==initial.dev || st.isSymbolicLink())throw Error('unknown evidence');
      if(st.isDirectory())total+=await physicalBytes(child);
      else if(st.isFile())total+=st.size;
      else throw Error('unknown evidence');
      if(!Number.isSafeInteger(total))throw Error('evidence overflow');
    }
    return total;
  }
  async function write(entries,limits,id) {
    await verify();
    await read(); // Do not replace an existing symlink or malformed registry.
    const encoded=Buffer.from(JSON.stringify(entries));
    if(encoded.length>1048576)throw Error('registry invalid');
    if(!limits || !amount(limits.total) || (await physicalBytes(parent))+encoded.length>limits.total)throw Error('registry peak limit');
    const active=entries.find(e=>e.id===id);
    if(active) {
      let oldBytes=0;
      try {oldBytes=(await fsp.lstat(path.join(parent,'registry.json'))).size;}catch(err){if(err.code!=='ENOENT')throw err;}
      const stage=path.join(parent,(id===transitioning?'bundle-':'stage-')+id);
      const payload=await physicalBytes(stage);
      if(!amount(active.reserved) || payload+oldBytes+encoded.length>active.reserved)throw Error('reservation peak limit');
    }
    const temporary=path.join(parent,'write-'+crypto.randomBytes(12).toString('hex'));
    const file=await fsp.open(temporary,fs.constants.O_WRONLY|fs.constants.O_CREAT|fs.constants.O_EXCL|fs.constants.O_NOFOLLOW,0o600);
    try {await file.writeFile(encoded);await file.sync();} finally {await file.close();}
    await verify();
    await fsp.rename(temporary,path.join(parent,'registry.json'));
    const dir=await fsp.open(parent,fs.constants.O_RDONLY|fs.constants.O_DIRECTORY|fs.constants.O_NOFOLLOW);
    try {await dir.sync();} finally {await dir.close();}
    prepared.clear();
  }
  async function locked(fn) {
    if(!same(await fsp.lstat(parent),initial))throw Error('registry parent changed');
    const lock=path.join(parent,'lock');
    await fsp.mkdir(lock,{mode:0o700});
    lockIdentity=await fsp.lstat(lock);
    try {return await fn();}
    finally {
      try {await verify();await fsp.rmdir(lock);}
      finally {prepared.clear();lockIdentity=undefined;}
    }
  }
  async function prepare(id) {
    await verify();
    const stage=path.join(parent,'stage-'+id);
    await fsp.mkdir(stage,{mode:0o700});
    const info=await fsp.lstat(stage);
    prepared.set('stage-'+id,{dev:info.dev,ino:info.ino});
    const dir=await fsp.open(parent,fs.constants.O_RDONLY|fs.constants.O_DIRECTORY|fs.constants.O_NOFOLLOW);
    try {await dir.sync();} finally {await dir.close();}
    return {stageDev:info.dev,stageIno:info.ino,identity:'m'+crypto.randomBytes(12).toString('base64url')};
  }
  async function extra(entries) {
    await verify();
    return Buffer.byteLength(JSON.stringify(entries))-2;
  }
  async function admit(entries,limits) {
    await verify();
    let used=2;
    for(const entry of entries) {
      const metadata=Buffer.byteLength(JSON.stringify(entry))+1;
      const charge=entry.state==='active'?entry.reserved:entry.bytes;
      if(!amount(charge) || metadata>charge)throw Error('registry accounting invalid');
      used+=charge;
    }
    if(!Number.isSafeInteger(used) || used>limits.total)throw Error('registry limit reached');
  }
  async function publish(id,entry,limits,expected) {
    await verify();
    const stage=path.join(parent,'stage-'+id), bundle=path.join(parent,'bundle-'+id);
    const info=await fsp.lstat(stage);
    if(!info.isDirectory() || info.dev!==entry.stageDev || info.ino!==entry.stageIno || info.uid!==process.getuid() || (info.mode&0o777)!==0o700)throw Error('staging changed');
    let total=0;const artifacts=[];
    for(const name of (await fsp.readdir(stage)).sort()) {
      if(name==='.' || name==='..' || name.includes('/') || name.includes('\\'))throw Error('invalid artifact');
      const file=await fsp.open(path.join(stage,name),fs.constants.O_RDONLY|fs.constants.O_NOFOLLOW);
      try {const st=await file.stat();if(!st.isFile() || st.dev!==info.dev || st.uid!==info.uid)throw Error('invalid artifact');total+=st.size;artifacts.push({name,dev:st.dev,ino:st.ino,size:st.size,sha256:await digest(file)});await file.sync();}
      finally {await file.close();}
    }
    if(expected!==undefined && (!Array.isArray(expected)||expected.length!==artifacts.length||artifacts.some((item,index)=>{
      const prior=expected[index];
      return !prior||prior.name!==item.name||prior.dev!==item.dev||prior.ino!==item.ino||prior.size!==item.size||prior.sha256!==item.sha256;
    })))throw Error('audited artifacts changed');
    if(total!==entry.bytes)throw Error('artifact size changed');
    const committed={...entry,identity:'matched',artifacts};
    let charge=total;
    for(;;) {
      committed.bytes=charge;
      const updated=total+Buffer.byteLength(JSON.stringify(committed))+1;
      if(updated===charge)break;
      charge=updated;
    }
    if(!amount(charge) || charge>entry.reserved)throw Error('reservation limit reached');
    const stageDir=await fsp.open(stage,fs.constants.O_RDONLY|fs.constants.O_DIRECTORY|fs.constants.O_NOFOLLOW);
    try {await stageDir.sync();} finally {await stageDir.close();}
    try {await fsp.lstat(bundle);throw Error('bundle exists');}catch(err){if(err.code!=='ENOENT')throw err;}
    await verify();
    const again=await fsp.lstat(stage);
    if(!same(again,info))throw Error('staging changed');
    await fsp.rename(stage,bundle);
    transitioning=id;
    try {
      const dir=await fsp.open(parent,fs.constants.O_RDONLY|fs.constants.O_DIRECTORY|fs.constants.O_NOFOLLOW);
      try {await dir.sync();} finally {await dir.close();}
      const entries=await read();
      await write(entries.map(e=>e.id===id?committed:e),limits,id);
    } finally {transitioning=undefined;}
  }
  async function retire(entry,limits) {
    const entries=await read();
    const current=entries.find(e=>e.id===entry.id && e.state==='complete' && !e.protected);
    if(!current || current.stageDev!==entry.stageDev || current.stageIno!==entry.stageIno)throw Error('retirement unregistered');
    const bundle=path.join(parent,'bundle-'+entry.id), tomb=path.join(parent,'retire-'+entry.id);
    const info=await fsp.lstat(bundle);
    if(!info.isDirectory() || info.dev!==entry.stageDev || info.ino!==entry.stageIno || info.uid!==process.getuid() || (info.mode&0o777)!==0o700)throw Error('retirement changed');
    const names=(await fsp.readdir(bundle)).sort();
    if(!Array.isArray(current.artifacts) || current.artifacts.length!==names.length)throw Error('retirement contents unknown');
    for(let i=0;i<names.length;i++) {
      const name=names[i],record=current.artifacts[i];
      if(!record || record.name!==name)throw Error('retirement contents unknown');
      const file=await fsp.open(path.join(bundle,name),fs.constants.O_RDONLY|fs.constants.O_NOFOLLOW);
      try {
        const st=await file.stat();
        if(!st.isFile() || st.dev!==info.dev || st.uid!==info.uid || st.dev!==record.dev || st.ino!==record.ino || st.size!==record.size || await digest(file)!==record.sha256)throw Error('retirement contents changed');
      } finally {await file.close();}
    }
    try {await fsp.lstat(tomb);throw Error('retirement exists');}catch(err){if(err.code!=='ENOENT')throw err;}
    await verify();
    if(!same(await fsp.lstat(bundle),info))throw Error('retirement changed');
    await fsp.rename(bundle,tomb);
    retiring=entry.id;
    try {
      const dir=await fsp.open(parent,fs.constants.O_RDONLY|fs.constants.O_DIRECTORY|fs.constants.O_NOFOLLOW);
      try {await dir.sync();} finally {await dir.close();}
      for(const name of names) {
        const file=path.join(tomb,name), record=current.artifacts.find(e=>e.name===name);
        const handle=await fsp.open(file,fs.constants.O_RDONLY|fs.constants.O_NOFOLLOW);
        try {
          const st=await handle.stat();
          if(!record || !st.isFile() || st.dev!==info.dev || st.uid!==info.uid || st.dev!==record.dev || st.ino!==record.ino || st.size!==record.size || await digest(handle)!==record.sha256)throw Error('retirement contents changed');
          const last=await fsp.lstat(file);
          if(!last.isFile() || last.isSymbolicLink() || last.dev!==st.dev || last.ino!==st.ino || last.size!==st.size)throw Error('retirement contents changed');
          await fsp.unlink(file);
        } finally {await handle.close();}
      }
      if(!same(await fsp.lstat(tomb),info))throw Error('retirement changed');
      await fsp.rmdir(tomb);
      retired=entry.id;
      await write(entries.filter(e=>e.id!==entry.id),limits);
    } finally {retiring=undefined;retired=undefined;}
  }
  async function finalize(entry,limits) {
    await verify();
    const entries=await read(),current=entries.find(e=>e.id===entry.id);
    if(!current||current.state!=='active'||current.protected||current.identity!==entry.identity||current.stageDev!==entry.stageDev||current.stageIno!==entry.stageIno)throw Error('finalization unregistered');
    const stage=path.join(parent,'stage-'+entry.id),tomb=path.join(parent,'retire-'+entry.id);
    const info=await fsp.lstat(stage);
    if(!info.isDirectory()||info.isSymbolicLink()||info.uid!==initial.uid||info.dev!==entry.stageDev||info.ino!==entry.stageIno||(info.mode&0o777)!==0o700||(await fsp.readdir(stage)).length)throw Error('finalization unsafe');
    try {await fsp.lstat(tomb);throw Error('finalization exists');}catch(err){if(err.code!=='ENOENT')throw err;}
    await verify();if(!same(await fsp.lstat(stage),info))throw Error('finalization changed');
    await fsp.rename(stage,tomb);retiring=entry.id;
    try {
      const dir=await fsp.open(parent,fs.constants.O_RDONLY|fs.constants.O_DIRECTORY|fs.constants.O_NOFOLLOW);
      try {await dir.sync();}finally{await dir.close();}
      if(!same(await fsp.lstat(tomb),info))throw Error('finalization changed');
      await fsp.rmdir(tomb);retired=entry.id;
      await write(entries.filter(e=>e.id!==entry.id),limits);
    }finally{retiring=undefined;retired=undefined;}
  }
  async function step() {await verify();}
  return {fixedBytes:2,locked,read,write,prepare,admit,publish,step,retire,finalize,extra};
}
module.exports = {createRetention,createFileStore};
