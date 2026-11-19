'use strict';

async function bounded(promise, ms, label) {
  let timer;
  try { return await Promise.race([promise,new Promise((_,reject)=>{timer=setTimeout(()=>reject(Error(label)),ms);})]); }
  finally { clearTimeout(timer); }
}
function track(child) {
  let spawnError;
  child.on('error',err=>{spawnError=err;});
  child.stdin?.on('error',()=>{});
  const closed=new Promise(resolve=>child.once('close',(code,signal)=>resolve({code,signal,spawnError})));
  return closed;
}
async function stopOwned(child,closed) {
  if(child.exitCode===null&&child.signalCode===null) {
    if(child.stdin?.writable)child.stdin.end('release');
    child.kill('SIGTERM');
  }
  try { return await bounded(closed,220000,'owned child did not close'); }
  catch {
    if(child.exitCode===null&&child.signalCode===null)child.kill('SIGKILL');
    return bounded(closed,5000,'owned child did not close after kill');
  }
}
function waitForMarker(child,output,text,count=1) {
  return new Promise((resolve,reject)=>{
    if(output().split(text).length-1>=count)return resolve();
    if(child.exitCode!==null||child.signalCode!==null)return reject(Error('runner exited before probe boundary'));
    const timer=setTimeout(()=>finish(Error('runner did not reach probe boundary')),60000);
    function finish(err){clearTimeout(timer);child.stdout.off('data',check);child.off('exit',exit);child.off('error',failed);err?reject(err):resolve();}
    function check(){if(output().split(text).length-1>=count)finish();}
    function exit(){finish(Error('runner exited before probe boundary'));}
    function failed(){finish(Error('runner could not start'));}
    child.stdout.on('data',check);child.once('exit',exit);child.once('error',failed);
  });
}
module.exports={bounded,track,stopOwned,waitForMarker};
