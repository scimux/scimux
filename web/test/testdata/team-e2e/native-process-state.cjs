'use strict';
function nativeProcessState(pid,{probe=process.kill,read=value=>require('node:fs').readFileSync('/proc/'+value+'/stat','utf8')}={}) {
  if(!Number.isSafeInteger(pid)||pid<=0)return 'unknown';
  try {probe(pid,0);}catch(error){return error.code==='ESRCH'?'gone':'unknown';}
  try {
    const raw=read(pid),end=raw.lastIndexOf(') ');
    if(end<0||!raw.startsWith(String(pid)+' ('))return 'unknown';
    const state=raw.slice(end+2, end+3);
    if(!'RSDZTWtXxKWPIN'.includes(state)||state.length!==1)return 'unknown';
    return state==='Z'?'zombie':'live';
  } catch {return 'unknown';}
}
module.exports={nativeProcessState};
function summarizeNative(pids,state=nativeProcessState) {
  let active=0,zombies=0;
  for(const pid of pids) {
    const result=state(pid);
    if(result==='zombie')zombies++;
    else if(result!=='gone')active++;
  }
  return {active,zombies};
}
module.exports.summarizeNative=summarizeNative;
