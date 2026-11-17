'use strict';
// Never discard the only control path to a server that may still be alive.
async function retire(socket, ops) {
  if (!ops.exists(socket)) return {removed:true};
  let killError;
  try { await ops.kill(socket); } catch (e) { killError=String(e); }
  let state='unknown';
  for(let i=0;i<30;i++) {
    try { state=await ops.probe(socket); } catch(e) { return {removed:false,error:'socket probe failed: '+String(e),killError}; }
    if(state==='dead') break;
    if(state==='live' && killError) return {removed:false,error:killError,killError};
    if(state!=='live') return {removed:false,error:'socket status unknown',killError};
    await new Promise(resolve=>setTimeout(resolve,100));
  }
  if(state!=='dead') return {removed:false,error:killError || 'private tmux server remains reachable',killError};
  try { ops.unlink(socket); } catch(e) { if(e.code!=='ENOENT')return {removed:false,error:'socket unlink failed: '+String(e),killError}; }
  return {removed:!ops.exists(socket),killError};
}
module.exports = {retire};
