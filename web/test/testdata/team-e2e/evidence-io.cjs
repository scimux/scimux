'use strict';
const fs=require('node:fs');
const fsp=fs.promises;
const path=require('node:path');
const {captureIdentity}=require('./capture-identity.cjs');
const same=(st,id)=>st.uid===id.uid&&st.dev===id.dev&&st.ino===id.ino;
const validName=name=>typeof name==='string'&&(/^[a-z0-9][a-z0-9-]*\.png$/.test(name)||['manifest.json','browser-coverage.json','capture-identities.private.json'].includes(name));
function createEvidenceIO({runtime,stage,runtimeIdentity,stageIdentity,publish}) {
  async function verify(dir,identity) {
    const st=await fsp.lstat(dir);
    if(!st.isDirectory()||st.isSymbolicLink()||!same(st,identity)||(st.mode&0o777)!==0o700)throw Error('identity');
  }
  async function read(name,limit) {
    if(!validName(name))throw Error('name');
    await verify(runtime,runtimeIdentity);
    const file=path.join(runtime,name);
    const initial=await fsp.lstat(file);
    if(!initial.isFile()||initial.isSymbolicLink()||initial.uid!==runtimeIdentity.uid||initial.dev!==runtimeIdentity.dev||initial.size>limit)throw Error('identity');
    const handle=await fsp.open(file,fs.constants.O_RDONLY|fs.constants.O_NOFOLLOW|fs.constants.O_NONBLOCK);
    try {
      const unchanged=st=>same(st,initial)&&st.isFile()&&st.size===initial.size&&st.mtimeMs===initial.mtimeMs&&st.ctimeMs===initial.ctimeMs;
      const opened=await handle.stat();if(!unchanged(opened))throw Error('identity');
      const chunks=[];let total=0;
      while(total<=initial.size) {
        const buffer=Buffer.alloc(Math.min(65536,initial.size+1-total));
        const {bytesRead}=await handle.read(buffer,0,buffer.length,null);
        if(!bytesRead)break;
        total+=bytesRead;if(total>initial.size)throw Error('identity');
        chunks.push(buffer.subarray(0,bytesRead));
      }
      const bytes=Buffer.concat(chunks,total);const last=await fsp.lstat(file),after=await handle.stat();
      if(total!==initial.size||last.isSymbolicLink()||!unchanged(last)||!unchanged(after))throw Error('identity');
      await verify(runtime,runtimeIdentity);return bytes;
    } finally {await handle.close();}
  }
  async function diagnosticBytes() {
    await verify(runtime,runtimeIdentity);
    let total=0;
    for(const name of await fsp.readdir(runtime)) {
      if(!name.toLowerCase().endsWith('.log'))continue;
      if(!/^[a-z0-9][a-z0-9-]*\.log$/.test(name))throw Error('diagnostic name');
      const file=path.join(runtime,name),initial=await fsp.lstat(file);
      if(!initial.isFile()||initial.isSymbolicLink()||initial.uid!==runtimeIdentity.uid||initial.dev!==runtimeIdentity.dev||!Number.isSafeInteger(initial.size)||initial.size<0)throw Error('diagnostic identity');
      total+=initial.size;if(!Number.isSafeInteger(total)||total>1024*1024)throw Error('diagnostic capacity');
      const handle=await fsp.open(file,fs.constants.O_RDONLY|fs.constants.O_NOFOLLOW|fs.constants.O_NONBLOCK);
      try {
        const unchanged=st=>st.isFile()&&same(st,initial)&&st.size===initial.size&&st.mtimeMs===initial.mtimeMs&&st.ctimeMs===initial.ctimeMs;
        const opened=await handle.stat(),last=await fsp.lstat(file);
        if(!unchanged(opened)||last.isSymbolicLink()||!unchanged(last))throw Error('diagnostic identity');
      } finally {await handle.close();}
    }
    await verify(runtime,runtimeIdentity);return total;
  }
  async function load() {
    try {
      await verify(runtime,runtimeIdentity);await verify(stage,stageIdentity);
      const manifestBytes=await read('manifest.json',16*1024*1024);
      const manifest=JSON.parse(manifestBytes.toString());
      if(!manifest||!Array.isArray(manifest.artifacts))return null;
      let captures,indexBytes=Buffer.alloc(0);
      try {indexBytes=await read('capture-identities.private.json',1024*1024);captures=JSON.parse(indexBytes.toString());}
      catch(error) {if(error.code==='ENOENT'&&!manifest.artifacts.some(name=>typeof name==='string'&&name.endsWith('.png')))captures={};else throw error;}
      if(!captures||typeof captures!=='object'||Array.isArray(captures))return null;
      const files={};
      for(const name of manifest.artifacts) {
        if(!validName(name)||name==='manifest.json'||name==='capture-identities.private.json'||Object.hasOwn(files,name))return null;
        const limit=name==='browser-coverage.json'?512*1024*1024:32*1024*1024;
        const identity=await captureIdentity(path.join(runtime,name),{limit});
        if(!identity)return null;
        files[name]={identity,captureIdentity:captures[name],size:Number(identity.split(':')[3])};
      }
      const io={
        read:async name=>{
          if(!Object.hasOwn(files,name))throw Error('name');
          const limit=name==='browser-coverage.json'?512*1024*1024:32*1024*1024;
          const before=await captureIdentity(path.join(runtime,name),{limit});
          const bytes=await read(name,limit);
          const after=await captureIdentity(path.join(runtime,name),{limit});
          if(!before||before!==after)throw Error('identity');
          return {kind:'file',identity:before,captureIdentity:files[name].captureIdentity,bytes};
        },
        write:async(name,bytes)=>{
          if(!validName(name)||name==='capture-identities.private.json'||!Buffer.isBuffer(bytes))throw Error('name');
          await verify(stage,stageIdentity);
          const handle=await fsp.open(path.join(stage,name),fs.constants.O_WRONLY|fs.constants.O_CREAT|fs.constants.O_EXCL|fs.constants.O_NOFOLLOW,0o600);
          try {await handle.writeFile(bytes);}finally{await handle.close();}
          await verify(stage,stageIdentity);
        },
        snapshot:async()=>{
          await verify(stage,stageIdentity);
          const result=[];
          for(const name of (await fsp.readdir(stage)).sort()) {
            if(!validName(name)||name==='capture-identities.private.json')throw Error('name');
            const token=await captureIdentity(path.join(stage,name),{limit:512*1024*1024});
            if(!token)throw Error('identity');
            const [uid,dev,ino,size,sha256]=token.split(':');
            if(Number(uid)!==stageIdentity.uid||Number(dev)!==stageIdentity.dev)throw Error('identity');
            result.push({name,dev:Number(dev),ino:Number(ino),size:Number(size),sha256});
          }
          await verify(stage,stageIdentity);return result;
        },
        publish,
      };
      const rawDiagnosticBytes=await diagnosticBytes();
      return {source:{manifest,files,rawDiagnosticBytes,rawMetadataBytes:manifestBytes.length+indexBytes.length+rawDiagnosticBytes},io};
    } catch {return null;}
  }
  return {load,read};
}
module.exports={createEvidenceIO};
