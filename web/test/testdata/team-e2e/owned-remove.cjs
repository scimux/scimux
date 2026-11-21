'use strict';
const fs=require('node:fs/promises');
const path=require('node:path');
const match=(st,id)=>st.uid===id.uid&&st.dev===id.dev&&st.ino===id.ino;
const validName=name=>typeof name==='string'&&name!==''&&name!=='.'&&name!=='..'&&!name.includes('\0')&&!name.includes('/')&&!path.isAbsolute(name);

// Only a refused local stream connection proves a socket path is inactive.
// A live socket, another socket type, an error or a stalled connect is kept.
async function inactiveSocket(file) {
  const net=require('node:net');
  return new Promise(resolve=>{
    let done=false;
    const socket=net.createConnection({path:file});
    const finish=value=>{if(done)return;done=true;clearTimeout(timer);socket.destroy();resolve(value);};
    const timer=setTimeout(()=>finish(false),1000);
    socket.once('connect',()=>finish(false));
    socket.once('error',error=>finish(error.code==='ECONNREFUSED'));
  });
}

// The caller uses a dedicated child with cwd bound to the registered root.
// All names are inspected before the first removal and rechecked before use.
async function removeOwned({parent,name,parentIdentity,rootIdentity,registered}) {
  try {
    if(!path.isAbsolute(parent)||!validName(name)||!registered||typeof registered!=='object')return {removed:false};
    const root=path.join(parent,name);
    async function verifyRoot() {
      const p=await fs.lstat(parent),r=await fs.lstat(root);
      if(!p.isDirectory()||!r.isDirectory()||p.isSymbolicLink()||r.isSymbolicLink()||!match(p,parentIdentity)||!match(r,rootIdentity)||p.dev!==r.dev||(p.mode&0o777)!==0o700||(r.mode&0o777)!==0o700)throw Error('identity');
    }
    await verifyRoot();
    const top=await fs.readdir(root);
    if(top.length!==Object.keys(registered).length||top.some(name=>!validName(name)||!Object.hasOwn(registered,name)))return {removed:false};
    const entries=[];
    async function inspect(relative,expected) {
      const item=path.join(root,relative),st=await fs.lstat(item);
      if(st.isSymbolicLink()||(!st.isFile()&&!st.isDirectory()&&!st.isSocket())||st.uid!==rootIdentity.uid||st.dev!==rootIdentity.dev||(expected&&!match(st,expected)))throw Error('identity');
      if(st.isSocket()&&!await inactiveSocket(item))throw Error('socket active');
      if(st.isDirectory())for(const child of await fs.readdir(item)) {
        if(!validName(child))throw Error('name');
        await inspect(path.join(relative,child));
      }
      entries.push({relative,identity:{uid:st.uid,dev:st.dev,ino:st.ino},directory:st.isDirectory(),socket:st.isSocket()});
    }
    for(const item of top)await inspect(item,registered[item]);
    for(const item of entries) {
      await verifyRoot();
      const target=path.join(root,item.relative),st=await fs.lstat(target);
      if(st.isSymbolicLink()||!match(st,item.identity)||(item.directory?!st.isDirectory():item.socket?!st.isSocket():!st.isFile()))throw Error('identity');
      if(item.socket) {
        if(!await inactiveSocket(target))throw Error('socket active');
        const again=await fs.lstat(target);
        if(!again.isSocket()||!match(again,item.identity))throw Error('identity');
      }
      if(item.directory)await fs.rmdir(target);else await fs.unlink(target);
    }
    await verifyRoot();await fs.rmdir(root);
    return {removed:true};
  } catch {return {removed:false};}
}
module.exports={removeOwned};
async function removalChildEntry({input,cwdStat=()=>fs.stat('.'),remove=removeOwned}) {
  try {
    const st=await cwdStat();
    if(!st.isDirectory()||!input?.rootIdentity||!match(st,input.rootIdentity))return {removed:false};
    return await remove(input);
  } catch {return {removed:false};}
}
async function removeInChild(input,spawnProcess=require('node:child_process').spawn) {
  let child;
  try {
    child=spawnProcess(process.execPath,[__filename,'--child'],{cwd:input.cwd,stdio:['pipe','pipe','ignore'],signal:AbortSignal.timeout(210000),killSignal:'SIGKILL'});
  } catch {return {removed:false};}
  let output='',failed=false;
  child.stdout.on('data',chunk=>{output+=String(chunk);if(output.length>64)failed=true;output=output.slice(0,64);});
  child.stdin.on('error',()=>{failed=true;});
  const closed=new Promise(resolve=>{
    child.once('error',()=>{failed=true;});
    child.once('close',(code,signal)=>resolve({code,signal}));
  });
  child.stdin.end(JSON.stringify(input));
  const result=await closed;
  return {removed:!failed&&result.code===0&&result.signal===null&&output==='removed\n'};
}
module.exports={removeOwned,removeInChild,removalChildEntry};
if(require.main===module&&process.argv[2]==='--child') {
  let input='',overflow=false;
  process.stdin.setEncoding('utf8');
  process.stdin.on('data',chunk=>{input+=chunk;if(input.length>1048576)overflow=true;input=input.slice(0,1048576);});
  process.stdin.on('end',async()=>{
    let result={removed:false};
    try {if(!overflow)result=await removalChildEntry({input:JSON.parse(input)});}catch {}
    process.stdout.write(result.removed?'removed\n':'refused\n');
  });
}
