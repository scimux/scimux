'use strict';
const fs=require('node:fs');
const path=require('node:path');
const {auditDiagnostic,bindRoot,png,readFlat,runCLI}=require('./audit-evidence.cjs');
const {bootstrapRunner,signalIntent,watchSignals}=require('./run-ownership.cjs');
const {createEvidenceIO}=require('./evidence-io.cjs');
const {projectEvidence}=require('./evidence-project.cjs');
const {verifiedCoverageEntry}=require('./coverage-source.cjs');
const limits={age:7*24*60*60*1000,total:2*1024*1024*1024,reservation:1024*1024*1024,diagnostics:1024*1024};

function validMidbodyProbe(probe,scenario) {
  return !probe || (scenario==='people-midbody-permission' && ['failure','signal','forward-error'].includes(probe));
}

async function auditStage({stage,kind,profile,runAudit=runCLI}) {
  const previous=process.cwd();
  try {
    if(kind==='full') {
      let text='';
      if(runAudit(['--profile',profile,stage],value=>{text+=value;})!==0)return {ok:false};
      const match=/^assertions (\d+)\nscreenshots (\d+)\nhistorical comparisons (\d+)\nadditional comparisons (\d+)\nafterNative seats (\d+)\nPASS\n$/.exec(text);
      if(!match)return {ok:false};
      return {ok:true,acceptance:true,figures:{assertions:Number(match[1]),screenshots:Number(match[2]),historical:Number(match[3]),additional:Number(match[4]),seats:Number(match[5])}};
    }
    bindRoot(stage);
    const manifest=JSON.parse(readFlat('manifest.json',16*1024*1024).toString('utf8'));
    const result=auditDiagnostic(manifest);
    if(!result.ok||result.scope!==kind)return {ok:false};
    for(const name of manifest.artifacts)if(name.endsWith('.png')&&!png(readFlat(name,32*1024*1024)))return {ok:false};
    return {...result,figures:{assertions:manifest.assertions.length,screenshots:manifest.artifacts.filter(name=>name.endsWith('.png')).length}};
  }catch{return {ok:false};}finally{process.chdir(previous);}
}
function verifyCoverage({bytes},env) {
  const entries=JSON.parse(bytes.toString('utf8'));
  if(!Array.isArray(entries))throw Error('invalid coverage');
  for(const entry of entries) {
    if(!entry||!['current','legacy'].includes(entry.sourceVersion)||typeof entry.path!=='string')throw Error('invalid coverage');
    const dir=entry.sourceVersion==='current'?path.resolve(__dirname,'../../../js'):path.join(env.SCIMUX_E2E_LEGACY_SOURCE,'web','js');
    if(!verifiedCoverageEntry({url:'http://synthetic.invalid'+entry.path,source:entry.source,functions:entry.functions},entry.page,dir,entry.sourceVersion))throw Error('invalid coverage');
  }
}
async function startRunner({runtimeParent,evidenceParent,io,store,retention,id,entry,evidenceIO=createEvidenceIO,project=projectEvidence,events=process,runnerPath,supervisorPath,env,streams,profile='people-attachments',intent:sharedIntent,finalize}) {
  const intent=sharedIntent||signalIntent(),dispose=sharedIntent?()=>{}:watchSignals(events,intent);
  try {
    const doProject=async({runtime,proof})=>{
      const work=await io.inspect('work');
      const record=(await store.locked(()=>store.read())).find(item=>item.id===id&&item.state==='active');
      if(!record)return {ok:false};
      const stage=path.join(evidenceParent.path,'stage-'+id);
      const loaded=await evidenceIO({runtime:runtime.work,stage,runtimeIdentity:work,stageIdentity:{uid:evidenceParent.uid,dev:record.stageDev,ino:record.stageIno},publish:details=>retention.publish({id,evidence:{staging:details.stagedBytes,metadata:0,diagnostics:0,expected:details.expected}})}).load();
      if(!loaded)return {ok:false};
      const result=loaded.source.manifest.result;
      const kind=result==='fail'?'failure':result==='interrupted'?'interrupted':env.SCIMUX_E2E_SCENARIO?'focused':'full';
      return project({...loaded,kind,proof:{waited:true,clean:true},limits:{run:limits.reservation,aggregate:limits.total,diagnostics:limits.diagnostics,registry:2*1024*1024},usage:{staging:0,registryCopies:0,diagnostics:0,otherRuns:0},auditor:()=>auditStage({stage,kind,profile}),verifyCoverage:file=>verifyCoverage(file,env)});
    };
    return await bootstrapRunner({runtimeParent,evidenceParent,io,project:doProject,intent,streams,entry,supervisorPath,runnerPath,env,finalize});
  } finally {dispose();}
}
module.exports={startRunner,auditStage,verifyCoverage,limits,validMidbodyProbe};
async function configureRunner({env,runnerPath,events=process,streams={stdin:process.stdin,stdout:process.stdout,stderr:process.stderr},
  createStore=require('./evidence-retention.cjs').createFileStore,
  createPolicy=require('./evidence-retention.cjs').createRetention,
  createIO=require('./runtime-io.cjs').createRunIO,start=startRunner,
  id=require('node:crypto').randomBytes(12).toString('hex'),
  verifySupervisor=file=>fs.promises.access(file,fs.constants.X_OK)}) {
  const intent=signalIntent(),dispose=watchSignals(events,intent);
  try {
    const runtimePath=env.SCIMUX_E2E_RUNTIME_PARENT,evidencePath=env.SCIMUX_E2E_EVIDENCE_PARENT,supervisorPath=env.SCIMUX_E2E_SUPERVISOR;
    if(![runtimePath,evidencePath,supervisorPath,runnerPath].every(value=>typeof value==='string'&&path.isAbsolute(value)))return {ok:false,outcome:'prerequisite',removed:false};
    await verifySupervisor(supervisorPath);
    if(intent.current())return {ok:false,outcome:'interrupted',signal:intent.current(),removed:false,unstarted:true};
    const store=createStore({parent:evidencePath}),retention=createPolicy({store,clock:()=>Date.now(),limits});
    let witness;
    const io=createIO({runtimeParent:runtimePath,evidenceParent:evidencePath,reserve:async()=>{const result=await retention.reserve({id});if(result.accepted)witness=result.witness;return result;}});
    const runtimeParent=await io.inspect('runtimeParent'),evidenceParent=await io.inspect('evidenceParent');
    if(intent.current())return {ok:false,outcome:'interrupted',signal:intent.current(),removed:false,unstarted:true};
    return await start({runtimeParent,evidenceParent,io,store,retention,id,events,streams,runnerPath,supervisorPath,env,intent,finalize:async()=>!!witness&&(await retention.finalize({witness})).finalized===true});
  } catch {return {ok:false,outcome:'prerequisite',removed:false};}finally {dispose();}
}
module.exports.configureRunner=configureRunner;
function runnerExitCode(result) {
  if(result?.ok&&result.outcome==='pass'&&result.removed)return 0;
  if(result?.outcome==='prerequisite')return 2;
  if(result?.outcome==='interrupted'&&(result.removed||result.unstarted)) {
    if(result.signal==='SIGINT')return 130;
    if(result.signal==='SIGTERM')return 143;
  }
  return 1;
}
module.exports.runnerExitCode=runnerExitCode;

function formatRunnerSummary(result) {
  const outcomes=['pass','fail','prerequisite','invalid-parent','invalid-runtime','allocation-failed','interrupted','launch-failed','unproven','timeout','evidence-failed'];
  const outcome=outcomes.includes(result?.outcome)?result.outcome:'unknown';
  const runnerExit=Number.isSafeInteger(result?.runnerExit)&&result.runnerExit>=0&&result.runnerExit<=255?String(result.runnerExit):'unknown';
  const removed=typeof result?.removed==='boolean'?String(result.removed):'unknown';
  const signal=['SIGINT','SIGTERM'].includes(result?.signal)?' signal='+result.signal:'';
  return `E2E outcome=${outcome} runnerExit=${runnerExit} removed=${removed}${signal}`;
}
module.exports.formatRunnerSummary=formatRunnerSummary;
