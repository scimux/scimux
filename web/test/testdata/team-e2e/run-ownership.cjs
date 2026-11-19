'use strict';
function signalIntent() {
  let first=null,forward=null;
  return {
    receive(signal){if(first===null){first=signal;if(forward)forward(signal);}},
    current(){return first;},
    attach(send){forward=send;if(first!==null)forward(first);},
  };
}
const valid=identity=>identity && identity.kind==='directory' && identity.mode===0o700 && Number.isSafeInteger(identity.uid) && Number.isSafeInteger(identity.dev) && Number.isSafeInteger(identity.ino);
const same=(a,b)=>valid(a)&&valid(b)&&a.uid===b.uid&&a.dev===b.dev&&a.ino===b.ino;
function cleanProof(waited) {
  if(!waited || waited.exit!==0 || typeof waited.proof!=='string')return null;
  const match=/^status clean\nrunner_exit (0|[1-9][0-9]*)\nreaped (0|[1-9][0-9]*)\n$/.exec(waited.proof);
  if(!match)return null;
  const exit=Number(match[1]),reaped=Number(match[2]);
  return Number.isSafeInteger(exit)&&exit<=255&&Number.isSafeInteger(reaped)?exit:null;
}
async function runOwned({runtimeParent,evidenceParent,io,supervisor,project,intent,streams,finalize}) {
  async function finalizeSafe() {try{return !finalize||(await finalize())===true;}catch{return false;}}
  let runtime;
  try {
    if(!valid(runtimeParent)||!valid(evidenceParent)||same(runtimeParent,evidenceParent)||!same(await io.inspect('runtimeParent'),runtimeParent)||!same(await io.inspect('evidenceParent'),evidenceParent))return {ok:false,outcome:'invalid-parent',removed:false};
    if(intent?.current())return {ok:false,outcome:'interrupted',signal:intent.current(),removed:false,unstarted:true};
    runtime=await io.allocate();
    if(!valid(runtime)||!same(await io.inspect('runtime'),runtime))return {ok:false,outcome:'invalid-runtime',removed:false};
  } catch (error) {
    let removed=error?.cleanupRemoved===true;
    if(!removed&&valid(runtime)) {
      try {if(same(await io.inspect('runtime'),runtime))removed=(await io.remove(runtime))!==false;}catch {}
    }
    if(removed)await finalizeSafe();
    return {ok:false,outcome:'allocation-failed',removed};
  }
  if(intent?.current()) {
    let removed=false;
    try {if(same(await io.inspect('runtime'),runtime))removed=(await io.remove(runtime))!==false;}catch {}
    const finalized=removed&&await finalizeSafe();
    return finalized?{ok:false,outcome:'interrupted',signal:intent.current(),removed:true}:{ok:false,outcome:'fail',removed};
  }
  let child;
  try {child=await supervisor.launch({streams,runtime});}
  catch {
    let removed=false;
    try {if(same(await io.inspect('runtime'),runtime))removed=(await io.remove(runtime))!==false;} catch {}
    if(removed)await finalizeSafe();
    return {ok:false,outcome:'launch-failed',removed};
  }
  let waited;
  try {waited=await child.wait();} catch {return {ok:false,outcome:'unproven',removed:false};}
  const exit=cleanProof(waited);
  if(exit===null)return {ok:false,outcome:waited?.proof?.startsWith('status timeout\n')?'timeout':'unproven',removed:false};
  let projected=false;
  try {
    if(!same(await io.inspect('runtime'),runtime))return {ok:false,outcome:'invalid-runtime',removed:false};
    if(!waited.outputOverflow)projected=(await project({runtime,proof:waited}))?.ok===true;
  } catch {}
  let removed=false;
  try {if(same(await io.inspect('runtime'),runtime))removed=(await io.remove(runtime))!==false;} catch {}
  const finalized=!projected&&removed?await finalizeSafe():true;
  const signal=intent?.current()||null;
  const interrupted=signal && (exit===0 || exit===(signal==='SIGINT'?130:143));
  const outcome=!finalized||waited.forwardFailed||waited.outputOverflow||exit!==0&&!interrupted?'fail':!projected?'evidence-failed':signal?'interrupted':removed?'pass':'fail';
  return {ok:outcome==='pass'&&removed,outcome,runnerExit:exit,removed,...(outcome==='interrupted'?{signal}:{})};
}
async function runSupervisorEntry({supervisor,supervisorArgs,command,stdin='',streams,spawnProcess=require('node:child_process').spawn,env=process.env,intent}) {
  let child;
  try {
    child=spawnProcess(supervisor,supervisorArgs||['--',...command],{env:{...env,TESTOWNED_FORWARD_STREAMS:'1',TESTOWNED_BROWSER_TEARDOWN:'1'},stdio:['pipe','pipe','pipe','pipe','pipe']});
  } catch {return {ok:false,started:false,category:'unproven'};}
  const captures={proof:{chunks:[],size:0,limit:1024,overflow:false},stdout:{chunks:[],size:0,limit:1048576,overflow:false},stderr:{chunks:[],size:0,limit:1048576,overflow:false}};
  let forwardFailed=false;
  function collect(field,chunk) {
    const value=Buffer.isBuffer(chunk)?chunk:Buffer.from(chunk),capture=captures[field];
    const remaining=capture.limit-capture.size;
    if(value.length>remaining)capture.overflow=true;
    const part=value.subarray(0,remaining);if(part.length)capture.chunks.push(part);capture.size+=part.length;
  }
  child.stdout.on('data',chunk=>collect('proof',chunk));
  child.stderr.resume();
  child.stdio[3].on('data',chunk=>collect('stdout',chunk));
  child.stdio[4].on('data',chunk=>collect('stderr',chunk));
  if(streams?.stdout)child.stdio[3].pipe(streams.stdout,{end:false});
  if(streams?.stderr)child.stdio[4].pipe(streams.stderr,{end:false});
  child.stdin.on('error',()=>{});
  let spawnError=false;
  const waited=new Promise(resolve=>{
    child.once('error',()=>{spawnError=true;});
    child.once('close',(exit,signal)=>resolve({exit:signal?null:exit}));
  });
  if(intent)intent.attach(signal=>{try{if(!child.kill(signal))forwardFailed=true;}catch{forwardFailed=true;}});
  if(streams?.stdin)streams.stdin.pipe(child.stdin);
  else child.stdin.end(stdin);
  const {exit}=await waited;
  if(streams?.stdin)streams.stdin.unpipe(child.stdin);
  const {StringDecoder}=require('node:string_decoder');
  function decoded(capture) {const decoder=new StringDecoder('utf8');const value=decoder.write(Buffer.concat(capture.chunks,capture.size));return value+(capture.overflow?'':decoder.end());}
  const proof=decoded(captures.proof),stdout=decoded(captures.stdout),stderr=decoded(captures.stderr);
  const outputOverflow=captures.stdout.overflow||captures.stderr.overflow;
  const runnerExit=captures.proof.overflow?null:cleanProof({exit,proof});
  if(runnerExit===null)return {ok:false,started:child.pid!==undefined||(!spawnError&&exit!==null),category:'unproven',stdout,stderr,proof};
  return {ok:true,started:true,supervisorExit:exit,runnerExit,stdout,stderr,proof,...(outputOverflow?{outputOverflow:true}:{}),...(forwardFailed?{forwardFailed:true}:{})};
}
module.exports={runOwned,signalIntent,runSupervisorEntry};
async function bootstrapRunner({io,runtimeParent,evidenceParent,project,intent,streams,entry=runSupervisorEntry,supervisorPath,runnerPath,env,finalize}) {
  const supervisor={launch:async({runtime})=>{
    let result;
    try {result=await entry({supervisor:supervisorPath,command:[process.execPath,runnerPath],streams,intent,env:{...env,SCIMUX_E2E_INNER:'1',SCIMUX_E2E_RUNTIME:runtime.work,SCIMUX_E2E_RUNTIME_IDENTITY:JSON.stringify(runtime.workIdentity),TMPDIR:runtime.tmp}});}
    catch {return {wait:async()=>{throw Error('supervisor state unknown');}};}
    if(result.started===false)throw Error('supervisor did not start');
    return {wait:async()=>({exit:result.ok?result.supervisorExit:-1,proof:result.proof||'',...(result.forwardFailed?{forwardFailed:true}:{}),...(result.outputOverflow?{outputOverflow:true}:{})})};
  }};
  return runOwned({io,runtimeParent,evidenceParent,project,intent,streams,supervisor,finalize});
}
module.exports.bootstrapRunner=bootstrapRunner;
function watchSignals(events,intent) {
  const handlers=new Map();
  for(const signal of ['SIGINT','SIGTERM']) {
    const handler=()=>intent.receive(signal);
    handlers.set(signal,handler);events.on(signal,handler);
  }
  return ()=>{for(const [signal,handler] of handlers)events.off(signal,handler);};
}
module.exports.watchSignals=watchSignals;
