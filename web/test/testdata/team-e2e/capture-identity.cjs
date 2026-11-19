'use strict';
const fs=require('node:fs');
const crypto=require('node:crypto');
const same=(a,b)=>a.isFile()&&b.isFile()&&a.uid===b.uid&&a.dev===b.dev&&a.ino===b.ino&&a.size===b.size&&a.mtimeMs===b.mtimeMs&&a.ctimeMs===b.ctimeMs;
async function captureIdentity(file,{limit=32*1024*1024}={}) {
  let handle;
  try {
    const initial=await fs.promises.lstat(file);
    if(!Number.isSafeInteger(limit)||limit<0||initial.isSymbolicLink()||!initial.isFile()||initial.uid!==process.getuid()||initial.size>limit)return null;
    handle=await fs.promises.open(file,fs.constants.O_RDONLY|fs.constants.O_NOFOLLOW|fs.constants.O_NONBLOCK);
    const opened=await handle.stat();if(!same(initial,opened))return null;
    const hash=crypto.createHash('sha256'),buffer=Buffer.alloc(65536);let total=0;
    for(;;) {
      const {bytesRead}=await handle.read(buffer,0,buffer.length,null);
      if(!bytesRead)break;
      total+=bytesRead;if(total>initial.size)return null;
      hash.update(buffer.subarray(0,bytesRead));
    }
    const after=await handle.stat(),last=await fs.promises.lstat(file);
    if(total!==initial.size||last.isSymbolicLink()||!same(initial,after)||!same(initial,last))return null;
    return [initial.uid,initial.dev,initial.ino,initial.size,hash.digest('hex')].join(':');
  } catch {return null;}finally {if(handle)await handle.close().catch(()=>{});}
}
module.exports={captureIdentity};
function createCaptureRegistry({identity=captureIdentity,write}) {
  const entries=new Map();
  return {register:async file=>{
    try {
      const name=require('node:path').basename(file);
      if(!/^[a-z0-9][a-z0-9-]*\.png$/.test(name)||entries.has(name))return null;
      const value=await identity(file);if(!value)return null;
      const next=new Map(entries);next.set(name,value);
      await write(JSON.stringify(Object.fromEntries(next)));
      entries.set(name,value);return value;
    } catch {return null;}
  }};
}
module.exports.createCaptureRegistry=createCaptureRegistry;
