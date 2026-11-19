'use strict';
const fs=require('node:fs');
const fsp=fs.promises;
const path=require('node:path');
const {removeInChild}=require('./owned-remove.cjs');
const id=st=>({uid:st.uid,dev:st.dev,ino:st.ino});
const same=(st,identity)=>st.uid===identity.uid&&st.dev===identity.dev&&st.ino===identity.ino;
function checkPath(value) {
  if(typeof value!=='string'||!path.isAbsolute(value)||path.resolve(value)!==value)throw Error('invalid parent');
  let current=path.parse(value).root;
  for(const component of value.slice(current.length).split(path.sep).filter(Boolean)) {
    current=path.join(current,component);
    const info=fs.lstatSync(current);
    if(info.isSymbolicLink()||!info.isDirectory())throw Error('invalid parent');
  }
  const info=fs.lstatSync(value);
  if(info.uid!==process.getuid()||(info.mode&0o777)!==0o700)throw Error('invalid parent');
  return id(info);
}
function createRunIO({runtimeParent,evidenceParent,reserve,removeChild=removeInChild}) {
  const runtimeParentIdentity=checkPath(runtimeParent),evidenceParentIdentity=checkPath(evidenceParent);
  const a=path.relative(runtimeParent,evidenceParent),b=path.relative(evidenceParent,runtimeParent);
  if(!a||(!a.startsWith('..'+path.sep)&&a!=='..')||(!b.startsWith('..'+path.sep)&&b!=='..'))throw Error('invalid parents');
  let root,registered,allocatedIdentity;
  function parentInfo(name) {
    const parent=name==='runtimeParent'?runtimeParent:evidenceParent,expected=name==='runtimeParent'?runtimeParentIdentity:evidenceParentIdentity;
    const now=checkPath(parent);
    if(!same(now,expected))throw Error('parent changed');
    return {...now,kind:'directory',mode:0o700,path:parent};
  }
  async function inspect(name) {
    if(name==='runtimeParent'||name==='evidenceParent')return parentInfo(name);
    if(!['runtime','work'].includes(name)||!root)throw Error('runtime absent');
    parentInfo('runtimeParent');
    const stat=await fsp.lstat(root);
    if(!stat.isDirectory()||stat.isSymbolicLink()||(stat.mode&0o777)!==0o700)throw Error('runtime changed');
    if(name==='work') {
      if(!allocatedIdentity||!same(stat,allocatedIdentity))throw Error('runtime changed');
      const work=path.join(root,'work'),info=await fsp.lstat(work);
      if(!info.isDirectory()||info.isSymbolicLink()||!same(info,registered.work)||(info.mode&0o777)!==0o700)throw Error('work changed');
      return {...id(info),kind:'directory',mode:0o700,path:work};
    }
    return {...id(stat),kind:'directory',mode:0o700,path:root,work:path.join(root,'work'),tmp:path.join(root,'tmp'),workIdentity:registered?.work};
  }
  async function allocate() {
    parentInfo('runtimeParent');parentInfo('evidenceParent');
    if(!(await reserve())?.accepted)throw Error('reservation refused');
    parentInfo('runtimeParent');parentInfo('evidenceParent');
    try {
      root=await fsp.mkdtemp(path.join(runtimeParent,'r'));
      const initial=await fsp.lstat(root);
      if(!initial.isDirectory()||initial.isSymbolicLink()||initial.uid!==runtimeParentIdentity.uid||initial.dev!==runtimeParentIdentity.dev||(initial.mode&0o777)!==0o700)throw Error('runtime invalid');
      allocatedIdentity=id(initial);registered={};
      await fsp.chmod(root,0o700);
      for(const name of ['work','tmp']) {
        const p=path.join(root,name);await fsp.mkdir(p,{mode:0o700});registered[name]=id(await fsp.lstat(p));
      }
      return await inspect('runtime');
    } catch {
      let cleanupRemoved=false;
      if(root&&allocatedIdentity&&registered) {
        try {
          cleanupRemoved=(await removeChild({cwd:root,parent:runtimeParent,name:path.basename(root),parentIdentity:runtimeParentIdentity,rootIdentity:allocatedIdentity,registered}))?.removed===true;
        } catch {}
      }
      const failure=Error('runtime allocation failed');failure.cleanupRemoved=cleanupRemoved;throw failure;
    }
  }
  async function remove(runtime) {
    if(!root||runtime.path!==root)return false;
    const result=await removeChild({cwd:root,parent:runtimeParent,name:path.basename(root),parentIdentity:runtimeParentIdentity,rootIdentity:runtime,registered});
    return result.removed;
  }
  return {inspect,allocate,remove};
}
module.exports={createRunIO};
function validateInnerRuntime(env,lstat=fs.lstatSync,uid=process.getuid) {
  try {
    const root=env.SCIMUX_E2E_RUNTIME,expected=JSON.parse(env.SCIMUX_E2E_RUNTIME_IDENTITY);
    if(typeof root!=='string'||!path.isAbsolute(root)||path.resolve(root)!==root)return null;
    const stat=lstat(root);
    if(!stat.isDirectory()||stat.isSymbolicLink()||(stat.mode&0o777)!==0o700||stat.uid!==uid()||!same(stat,expected))return null;
    return root;
  } catch {return null;}
}
module.exports.validateInnerRuntime=validateInnerRuntime;
