import test from 'node:test';
import assert from 'node:assert/strict';
import {EventEmitter} from 'node:events';
import {spawnSync} from 'node:child_process';
import {startRunner} from '../runner-bootstrap.cjs';

const parent={uid:42,dev:7,ino:10,mode:0o700,kind:'directory'};
const evidence={uid:42,dev:7,ino:12,mode:0o700,kind:'directory',path:'/synthetic/evidence'};
const root={uid:42,dev:7,ino:11,mode:0o700,kind:'directory',path:'/synthetic/r',work:'/synthetic/r/work',tmp:'/synthetic/r/tmp'};
const clean='status clean\nrunner_exit 0\nreaped 0\n';
function fixture() {
 const events=[],emitter=new EventEmitter();let launch;
 const io={inspect:async name=>name==='runtimeParent'?parent:name==='evidenceParent'?evidence:name==='work'?{...root,path:root.work}:root,
   allocate:async()=>{events.push('reserve');return root;},remove:async()=>{events.push('remove');return true;}};
 const store={locked:async fn=>fn(),read:async()=>[{id:'synthetic',state:'active',stageDev:7,stageIno:13}]};
 const retention={publish:async()=>{events.push('publish');return {published:true};}};
 const entry=async opts=>{launch=opts;events.push('entry');return {ok:true,started:true,supervisorExit:0,runnerExit:0,proof:clean};};
 const evidenceIO=()=>({load:async()=>({source:{manifest:{result:'pass',scope:'synthetic'},files:{}},io:{publish:retention.publish}})});
 const project=async args=>{events.push('project');await args.io.publish({stagedBytes:1});return {ok:true};};
 return {events,emitter,io,store,retention,entry,evidenceIO,project,launch:()=>launch};
}

test('runner bootstrap reserves, supervises, projects and then removes the owned runtime',async()=>{
 const f=fixture();const result=await startRunner({runtimeParent:parent,evidenceParent:evidence,io:f.io,store:f.store,retention:f.retention,id:'synthetic',entry:f.entry,evidenceIO:f.evidenceIO,project:f.project,events:f.emitter,runnerPath:'/synthetic/runner',supervisorPath:'/synthetic/supervisor',env:{}});
 assert.equal(result.ok,true);assert.equal(result.removed,true);
 assert.deepEqual(f.events,['reserve','entry','project','publish','remove']);
 assert.equal(f.launch().env.SCIMUX_E2E_RUNTIME,root.work);assert.equal(f.launch().env.TMPDIR,root.tmp);
 assert.equal(f.emitter.listenerCount('SIGINT'),0);assert.equal(f.emitter.listenerCount('SIGTERM'),0);
});

test('bootstrap never publishes or removes runtime when supervisor proof is uncertain',async()=>{
 const f=fixture();f.entry=async()=>{f.events.push('entry');return {ok:false,started:true,proof:'status timeout\nrunner_exit 0\nreaped 0\n'};};
 const result=await startRunner({runtimeParent:parent,evidenceParent:evidence,io:f.io,store:f.store,retention:f.retention,id:'synthetic',entry:f.entry,evidenceIO:f.evidenceIO,project:f.project,events:f.emitter,runnerPath:'/synthetic/runner',supervisorPath:'/synthetic/supervisor',env:{}});
 assert.equal(result.ok,false);assert.equal(result.removed,false);assert.deepEqual(f.events,['reserve','entry']);
});

test('bootstrap audit reads staged full evidence and returns file-derived counts',async t=>{
 const fs=await import('node:fs/promises');const os=await import('node:os');const path=await import('node:path');
 const {synthetic}=await import('../synthetic-audit-manifest.cjs');
 const {audit}=await import('../audit-evidence.cjs');const {auditStage}=await import('../runner-bootstrap.cjs');
 const root=await fs.mkdtemp(path.join(os.tmpdir(),'stage-audit-synthetic-'));t.after(()=>fs.rm(root,{recursive:true,force:true}));
 const manifest=synthetic('owner-agent-attachments');
 await fs.writeFile(path.join(root,'manifest.json'),JSON.stringify(manifest));
 await fs.writeFile(path.join(root,'screen.png'),Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAEUlEQVR4nGP4z8DwH4QZYAwAR8oH+WdZbrcAAAAASUVORK5CYII=','base64'));
 const result=await auditStage({stage:root,kind:'full',profile:'owner-agent-attachments',manifest});const expected=audit(manifest,'owner-agent-attachments');
 assert.equal(result.ok,true);assert.deepEqual(result.figures,{assertions:expected.assertions,screenshots:expected.screenshots,historical:expected.historical,additional:expected.additional,seats:expected.seats});
 await fs.writeFile(path.join(root,'screen.png'),'corrupt');assert.equal((await auditStage({stage:root,kind:'full',profile:'owner-agent-attachments',manifest})).ok,false);
});

test('bootstrap audit reports focused diagnostics without full acceptance',async t=>{
 const fs=await import('node:fs/promises');const os=await import('node:os');const path=await import('node:path');
 const {auditStage}=await import('../runner-bootstrap.cjs');
 const root=await fs.mkdtemp(path.join(os.tmpdir(),'diagnostic-audit-synthetic-'));t.after(()=>fs.rm(root,{recursive:true,force:true}));
 const manifest={version:1,scope:'focused',result:'pass',assertions:[],artifacts:[],counters:{}};
 await fs.writeFile(path.join(root,'manifest.json'),JSON.stringify(manifest));
 const result=await auditStage({stage:root,kind:'focused',manifest});
 assert.equal(result.ok,true);assert.equal(result.acceptance,false);assert.equal(result.scope,'focused');
});

test('bootstrap audit refuses corrupt diagnostic screenshots and invalid manifests',async t=>{
 const fs=await import('node:fs/promises');const os=await import('node:os');const path=await import('node:path');
 const {auditStage}=await import('../runner-bootstrap.cjs');
 const dir=await fs.mkdtemp(path.join(os.tmpdir(),'diagnostic-audit-coverage-'));t.after(()=>fs.rm(dir,{recursive:true,force:true}));
 const manifest={version:1,scope:'focused',result:'pass',assertions:[],artifacts:['screen.png'],counters:{}};
 await fs.writeFile(path.join(dir,'manifest.json'),JSON.stringify(manifest));
 await fs.writeFile(path.join(dir,'screen.png'),Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAEUlEQVR4nGP4z8DwH4QZYAwAR8oH+WdZbrcAAAAASUVORK5CYII=','base64'));
 const valid=await auditStage({stage:dir,kind:'focused',manifest});assert.equal(valid.ok,true);assert.equal(valid.figures.screenshots,1);
 assert.equal((await auditStage({stage:dir,kind:'failure',manifest})).ok,false);
 await fs.writeFile(path.join(dir,'screen.png'),'corrupt');
 assert.equal((await auditStage({stage:dir,kind:'focused',manifest})).ok,false);
 await fs.writeFile(path.join(dir,'manifest.json'),'invalid json');assert.equal((await auditStage({stage:dir,kind:'focused'})).ok,false);
});

test('bootstrap refuses a successful full auditor with malformed output',async()=>{
 const {auditStage}=await import('../runner-bootstrap.cjs');
 const result=await auditStage({stage:'/synthetic/stage',kind:'full',profile:'synthetic',runAudit:(_args,write)=>{write('PASS\n');return 0;}});
 assert.equal(result.ok,false);
});

test('runner entry configures separate parents and reserves before inner launch',async()=>{
 const {configureRunner}=await import('../runner-bootstrap.cjs');const calls=[];
 const env={SCIMUX_E2E_RUNTIME_PARENT:'/synthetic/runtime',SCIMUX_E2E_EVIDENCE_PARENT:'/synthetic/evidence',SCIMUX_E2E_SUPERVISOR:'/synthetic/supervisor'};
 const policy={reserve:async({id})=>{calls.push(['reserve',id]);return {accepted:true};}};
 const createStore=options=>{calls.push(['store',options.parent]);return {};};
 const createPolicy=()=>policy;
 const createIO=options=>{calls.push(['io',options.runtimeParent,options.evidenceParent]);return {inspect:async name=>name==='runtimeParent'?parent:evidence,allocate:async()=>{await options.reserve();return root;}};};
 const start=async options=>{calls.push('start');await options.io.allocate();return {ok:true,outcome:'pass',removed:true};};
 const result=await configureRunner({env,runnerPath:'/synthetic/runner',createStore,createPolicy,createIO,start,id:'synthetic',verifySupervisor:async()=>{}});
 assert.equal(result.ok,true);assert.deepEqual(calls,[['store',env.SCIMUX_E2E_EVIDENCE_PARENT],['io',env.SCIMUX_E2E_RUNTIME_PARENT,env.SCIMUX_E2E_EVIDENCE_PARENT],'start',['reserve','synthetic']]);
});

test('runner entry refuses missing parents or supervisor before starting',async()=>{
 const {configureRunner}=await import('../runner-bootstrap.cjs');let started=false;
 const result=await configureRunner({env:{},runnerPath:'/synthetic/runner',start:async()=>{started=true;}});
 assert.equal(result.ok,false);assert.equal(started,false);
});

test('runner entry uses the default executable prerequisite check',async()=>{
 const {configureRunner}=await import('../runner-bootstrap.cjs');const f=fixture();let started=false;
 const env={SCIMUX_E2E_RUNTIME_PARENT:'/synthetic/runtime',SCIMUX_E2E_EVIDENCE_PARENT:'/synthetic/evidence',SCIMUX_E2E_SUPERVISOR:process.execPath};
 const result=await configureRunner({env,runnerPath:'/synthetic/runner',events:f.emitter,createStore:()=>f.store,createPolicy:()=>f.retention,createIO:()=>f.io,start:async()=>{started=true;return {ok:true,outcome:'pass',removed:true};},id:'synthetic'});
 assert.equal(result.ok,true);assert.equal(started,true);assert.equal(f.emitter.listenerCount('SIGINT'),0);assert.equal(f.emitter.listenerCount('SIGTERM'),0);
});

test('runner entry propagates a synthetic signal-listener disposal failure',async()=>{
 const {configureRunner}=await import('../runner-bootstrap.cjs');const events=new EventEmitter();
 events.off=()=>{throw Error('synthetic disposal failure');};
 await assert.rejects(configureRunner({env:{},runnerPath:'/synthetic/runner',events}),/synthetic disposal failure/);
});

test('runner exit status distinguishes pass, signal, failure and missing proof',async()=>{
 const {runnerExitCode}=await import('../runner-bootstrap.cjs');
 for(const [result,expected] of [
  [{ok:true,outcome:'pass',removed:true},0],
  [{ok:false,outcome:'prerequisite',removed:false},2],
  [{ok:false,outcome:'interrupted',signal:'SIGINT',removed:true},130],
  [{ok:false,outcome:'interrupted',signal:'SIGTERM',removed:true},143],
  [{ok:false,outcome:'interrupted',signal:'SIGINT',removed:false},1],
  [{ok:false,outcome:'fail',removed:true},1],
 ])assert.equal(runnerExitCode(result),expected);
});

test('bootstrap reserves space for both bounded registry copies during projection',async()=>{
 const f=fixture();let policy;
 f.project=async args=>{policy=args.limits;await args.io.publish({stagedBytes:1});return {ok:true};};
 const result=await startRunner({runtimeParent:parent,evidenceParent:evidence,io:f.io,store:f.store,retention:f.retention,id:'synthetic',entry:f.entry,evidenceIO:f.evidenceIO,project:f.project,events:f.emitter,runnerPath:'/synthetic/runner',supervisorPath:'/synthetic/supervisor',env:{}});
 assert.equal(result.ok,true);assert.equal(policy.registry,2*1024*1024);
});

test('bootstrap rejects missing registry record and missing loaded evidence',async()=>{
 for(const failure of ['record','load']){
  const f=fixture();if(failure==='record')f.store.read=async()=>[];else f.evidenceIO=()=>({load:async()=>null});
  const result=await startRunner({runtimeParent:parent,evidenceParent:evidence,io:f.io,store:f.store,retention:f.retention,id:'synthetic',entry:f.entry,evidenceIO:f.evidenceIO,project:f.project,events:f.emitter,runnerPath:'/synthetic/runner',supervisorPath:'/synthetic/supervisor',env:{}});
  assert.equal(result.ok,false);assert.equal(result.outcome,'evidence-failed');assert.equal(result.removed,true);assert.equal(f.events.includes('project'),false);
 }
});

for(const [resultValue,env,kind] of [['pass',{},'full'],['pass',{SCIMUX_E2E_SCENARIO:'browser-lifecycle'},'focused'],['fail',{},'failure'],['interrupted',{},'interrupted']]) test(`bootstrap derives ${kind} evidence scope`,async()=>{
 const f=fixture();f.evidenceIO=({publish})=>({load:async()=>({source:{manifest:{result:resultValue},files:{}},io:{publish}})});
 let captured;f.project=async args=>{captured=args;await args.io.publish({stagedBytes:3,expected:[]});return {ok:true};};
 const result=await startRunner({runtimeParent:parent,evidenceParent:evidence,io:f.io,store:f.store,retention:f.retention,id:'synthetic',entry:f.entry,evidenceIO:f.evidenceIO,project:f.project,events:f.emitter,runnerPath:'/synthetic/runner',supervisorPath:'/synthetic/supervisor',env});
 assert.equal(result.ok,true);assert.equal(captured.kind,kind);assert.ok(f.events.includes('publish'));
 assert.equal((await captured.auditor()).ok,false);
});

test('bootstrap verifies coverage against its source and rejects malformed entries',async()=>{
 const fs=await import('node:fs/promises');const f=fixture();let verify;
 f.project=async args=>{verify=args.verifyCoverage;return {ok:true};};
 const {fileURLToPath}=await import('node:url');const legacySource=fileURLToPath(new URL('../../../../../',import.meta.url));
 await startRunner({runtimeParent:parent,evidenceParent:evidence,io:f.io,store:f.store,retention:f.retention,id:'synthetic',entry:f.entry,evidenceIO:f.evidenceIO,project:f.project,events:f.emitter,runnerPath:'/synthetic/runner',supervisorPath:'/synthetic/supervisor',env:{SCIMUX_E2E_LEGACY_SOURCE:legacySource}});
 const source=await fs.readFile(new URL('../../../../js/chat.js',import.meta.url),'utf8');
 assert.doesNotThrow(()=>verify({bytes:Buffer.from(JSON.stringify([{sourceVersion:'current',path:'/js/chat.js',source,functions:[],page:'synthetic'}]))}));
 assert.doesNotThrow(()=>verify({bytes:Buffer.from(JSON.stringify([{sourceVersion:'legacy',path:'/js/chat.js',source,functions:[],page:'synthetic'}]))}));
 for(const data of [{},[{sourceVersion:'unknown',path:'/js/chat.js'}],[{sourceVersion:'current',path:'/not-js',source:'x'}]])assert.throws(()=>verify({bytes:Buffer.from(JSON.stringify(data))}));
});

test('signal during supervisor prerequisite remains effective through startup',async()=>{
 const {configureRunner}=await import('../runner-bootstrap.cjs');const f=fixture();let release,entered;
 const held=new Promise(resolve=>{release=resolve}),ready=new Promise(resolve=>{entered=resolve});
 const env={SCIMUX_E2E_RUNTIME_PARENT:'/synthetic/runtime',SCIMUX_E2E_EVIDENCE_PARENT:'/synthetic/evidence',SCIMUX_E2E_SUPERVISOR:'/synthetic/supervisor'};
 const start=options=>startRunner({...options,entry:f.entry,evidenceIO:f.evidenceIO,project:f.project});
 const pending=configureRunner({env,runnerPath:'/synthetic/runner',events:f.emitter,createStore:()=>f.store,createPolicy:()=>f.retention,createIO:()=>f.io,start,id:'synthetic',verifySupervisor:async()=>{entered();await held;}});
 await ready;f.emitter.emit('SIGINT');release();
 const result=await pending;
 assert.equal(result.ok,false);assert.equal(result.outcome,'interrupted');assert.equal(result.signal,'SIGINT');
});

for(const [first,second] of [['SIGINT','SIGINT'],['SIGINT','SIGTERM'],['SIGTERM','SIGTERM'],['SIGTERM','SIGINT']]) test(`startup retains first ${first} through ${second} before allocation`,async()=>{
 const {configureRunner}=await import('../runner-bootstrap.cjs');const f=fixture();let release,entered,allocations=0;
 const held=new Promise(resolve=>{release=resolve}),ready=new Promise(resolve=>{entered=resolve});
 const env={SCIMUX_E2E_RUNTIME_PARENT:'/synthetic/runtime',SCIMUX_E2E_EVIDENCE_PARENT:'/synthetic/evidence',SCIMUX_E2E_SUPERVISOR:'/synthetic/supervisor'};
 const allocate=f.io.allocate;f.io.allocate=async()=>{allocations++;return allocate();};
 const start=options=>startRunner({...options,entry:f.entry,evidenceIO:f.evidenceIO,project:f.project});
 const pending=configureRunner({env,runnerPath:'/synthetic/runner',events:f.emitter,createStore:()=>f.store,createPolicy:()=>f.retention,createIO:()=>f.io,start,id:'synthetic',verifySupervisor:async()=>{entered();await held;}});
 await ready;f.emitter.emit(first);f.emitter.emit(second);release();const result=await pending;
 assert.equal(result.outcome,'interrupted');assert.equal(result.signal,first);assert.equal(allocations,0);assert.equal(f.events.includes('entry'),false);
 assert.equal(result.removed,false);assert.equal(result.unstarted,true);
 assert.equal(f.emitter.listenerCount('SIGINT'),0);assert.equal(f.emitter.listenerCount('SIGTERM'),0);
});

for(const signal of ['SIGINT','SIGTERM']) test(`startup ${signal} during allocation removes unstarted runtime without launch`,async()=>{
 const {configureRunner}=await import('../runner-bootstrap.cjs');const f=fixture();let release,entered;
 const held=new Promise(resolve=>{release=resolve}),ready=new Promise(resolve=>{entered=resolve});
 const env={SCIMUX_E2E_RUNTIME_PARENT:'/synthetic/runtime',SCIMUX_E2E_EVIDENCE_PARENT:'/synthetic/evidence',SCIMUX_E2E_SUPERVISOR:'/synthetic/supervisor'};
 const allocate=f.io.allocate;f.io.allocate=async()=>{entered();await held;return allocate();};
 const witness={id:'synthetic',stageDev:7,stageIno:13,identity:'synthetic-incarnation'};let finalized;
 f.retention.reserve=async({id})=>{assert.equal(id,'synthetic');return {accepted:true,witness};};
 f.retention.finalize=async value=>{finalized=value;return {finalized:true};};
 const createIO=({reserve})=>({...f.io,allocate:async()=>{assert.equal((await reserve()).accepted,true);return f.io.allocate();}});
 const start=options=>startRunner({...options,entry:f.entry,evidenceIO:f.evidenceIO,project:f.project});
 const pending=configureRunner({env,runnerPath:'/synthetic/runner',events:f.emitter,createStore:()=>f.store,createPolicy:()=>f.retention,createIO,start,id:'synthetic',verifySupervisor:async()=>{}});
 await ready;f.emitter.emit(signal);release();const result=await pending;
 assert.equal(result.outcome,'interrupted');assert.equal(result.signal,signal);assert.equal(result.removed,true);
 assert.deepEqual(finalized,{witness});
 assert.deepEqual(f.events,['reserve','remove']);assert.equal(f.emitter.listenerCount('SIGINT'),0);assert.equal(f.emitter.listenerCount('SIGTERM'),0);
});

test('startup prerequisite failure stays failure after a signal and disposes listeners',async()=>{
 const {configureRunner}=await import('../runner-bootstrap.cjs');const f=fixture();let release,entered;
 const held=new Promise(resolve=>{release=resolve}),ready=new Promise(resolve=>{entered=resolve});
 const env={SCIMUX_E2E_RUNTIME_PARENT:'/synthetic/runtime',SCIMUX_E2E_EVIDENCE_PARENT:'/synthetic/evidence',SCIMUX_E2E_SUPERVISOR:'/synthetic/supervisor'};
 const pending=configureRunner({env,runnerPath:'/synthetic/runner',events:f.emitter,verifySupervisor:async()=>{entered();await held;throw Error('synthetic failure');}});
 await ready;f.emitter.emit('SIGTERM');release();const result=await pending;
 assert.equal(result.outcome,'prerequisite');assert.equal(result.ok,false);assert.equal(f.emitter.listenerCount('SIGINT'),0);assert.equal(f.emitter.listenerCount('SIGTERM'),0);
});

test('startup signal during parent inspection aborts before allocation',async()=>{
 const {configureRunner}=await import('../runner-bootstrap.cjs');const f=fixture();let release,entered;
 const held=new Promise(resolve=>{release=resolve}),ready=new Promise(resolve=>{entered=resolve});const inspect=f.io.inspect;
 f.io.inspect=async name=>{if(name==='runtimeParent'){entered();await held;}return inspect(name);};
 const env={SCIMUX_E2E_RUNTIME_PARENT:'/synthetic/runtime',SCIMUX_E2E_EVIDENCE_PARENT:'/synthetic/evidence',SCIMUX_E2E_SUPERVISOR:'/synthetic/supervisor'};
 const pending=configureRunner({env,runnerPath:'/synthetic/runner',events:f.emitter,createStore:()=>f.store,createPolicy:()=>f.retention,createIO:()=>f.io,id:'synthetic',verifySupervisor:async()=>{}});
 await ready;f.emitter.emit('SIGTERM');release();const result=await pending;
 assert.equal(result.outcome,'interrupted');assert.equal(result.signal,'SIGTERM');assert.deepEqual(f.events,[]);
 assert.equal(result.removed,false);assert.equal(result.unstarted,true);
 assert.equal(f.emitter.listenerCount('SIGTERM'),0);assert.equal(f.emitter.listenerCount('SIGINT'),0);
});

for(const failure of ['refused','throw','changed']) test(`startup interruption with ${failure} runtime cleanup remains failure`,async()=>{
 const {configureRunner}=await import('../runner-bootstrap.cjs');const f=fixture();let release,entered;
 const held=new Promise(resolve=>{release=resolve}),ready=new Promise(resolve=>{entered=resolve});
 const env={SCIMUX_E2E_RUNTIME_PARENT:'/synthetic/runtime',SCIMUX_E2E_EVIDENCE_PARENT:'/synthetic/evidence',SCIMUX_E2E_SUPERVISOR:'/synthetic/supervisor'};
 const allocate=f.io.allocate,inspect=f.io.inspect;f.io.allocate=async()=>{entered();await held;return allocate();};let inspections=0;
 f.io.inspect=async name=>{if(name==='runtime'&&failure==='changed'&&inspections++>0)return {...root,ino:99};return inspect(name);};
 f.io.remove=async()=>{if(failure==='throw')throw Error('synthetic removal error');return false;};
 const start=options=>startRunner({...options,entry:f.entry,evidenceIO:f.evidenceIO,project:f.project});
 const pending=configureRunner({env,runnerPath:'/synthetic/runner',events:f.emitter,createStore:()=>f.store,createPolicy:()=>f.retention,createIO:()=>f.io,start,id:'synthetic',verifySupervisor:async()=>{}});
 await ready;f.emitter.emit('SIGINT');release();const result=await pending;
 assert.equal(result.outcome,'fail');assert.equal(result.removed,false);assert.equal(f.events.includes('entry'),false);
 assert.equal(f.emitter.listenerCount('SIGINT'),0);assert.equal(f.emitter.listenerCount('SIGTERM'),0);
});

test('repeated proven unstarted cancellations leave capacity for a later legitimate reservation',async t=>{
 const fs=await import('node:fs/promises');const os=await import('node:os');const path=await import('node:path');
 const {createRunIO}=await import('../runtime-io.cjs');const {removeOwned}=await import('../owned-remove.cjs');
 const {createFileStore,createRetention}=await import('../evidence-retention.cjs');
 const {configureRunner,startRunner,limits}=await import('../runner-bootstrap.cjs');
 const base=await fs.mkdtemp(path.join(os.tmpdir(),'cancel-reservations-synthetic-'));const identity=await fs.lstat(base);
 t.after(async()=>{const current=await fs.lstat(base);assert.equal(current.dev,identity.dev);assert.equal(current.ino,identity.ino);assert.equal(current.uid,identity.uid);await fs.rm(base,{recursive:true});});
 const runtime=path.join(base,'runtime'),evidence=path.join(base,'evidence');await fs.mkdir(runtime,{mode:0o700});await fs.mkdir(evidence,{mode:0o700});
 const env={SCIMUX_E2E_RUNTIME_PARENT:runtime,SCIMUX_E2E_EVIDENCE_PARENT:evidence,SCIMUX_E2E_SUPERVISOR:'/synthetic/supervisor'};
 const results=[];let launches=0;
 for(let i=0;i<5;i++) {
   const events=new EventEmitter();
   const createIO=options=>{const io=createRunIO({...options,removeChild:removeOwned}),allocate=io.allocate;io.allocate=async()=>{const root=await allocate();events.emit('SIGINT');return root;};return io;};
   const start=options=>startRunner({...options,entry:async()=>{launches++;throw Error('unexpected synthetic launch');}});
   results.push(await configureRunner({env,runnerPath:'/synthetic/runner',events,createIO,start,verifySupervisor:async()=>{},id:`synthetic-${i}`}));
 }
 const later=await createRetention({store:createFileStore({parent:evidence}),clock:()=>Date.now(),limits}).reserve({id:'legitimate'});
 assert.equal(launches,0);assert.deepEqual(await fs.readdir(runtime),[]);
 assert.deepEqual({rows:results.map(result=>[result.outcome,result.removed]),later:later.accepted},{rows:Array.from({length:5},()=>['interrupted',true]),later:true});
});

test('cleanly waited output overflow does not strand reservations needed by later runs',async t=>{
 const fs=await import('node:fs/promises');const os=await import('node:os');const path=await import('node:path');
 const {createRunIO}=await import('../runtime-io.cjs');const {removeOwned}=await import('../owned-remove.cjs');
 const {createFileStore,createRetention}=await import('../evidence-retention.cjs');
 const {configureRunner,startRunner,limits}=await import('../runner-bootstrap.cjs');
 const base=await fs.mkdtemp(path.join(os.tmpdir(),'overflow-reservations-synthetic-'));const identity=await fs.lstat(base);
 t.after(async()=>{const current=await fs.lstat(base);assert.equal(current.dev,identity.dev);assert.equal(current.ino,identity.ino);assert.equal(current.uid,identity.uid);await fs.rm(base,{recursive:true});});
 const runtime=path.join(base,'runtime'),evidence=path.join(base,'evidence');await fs.mkdir(runtime,{mode:0o700});await fs.mkdir(evidence,{mode:0o700});
 const env={SCIMUX_E2E_RUNTIME_PARENT:runtime,SCIMUX_E2E_EVIDENCE_PARENT:evidence,SCIMUX_E2E_SUPERVISOR:'/synthetic/supervisor'};
 const results=[];let entries=0;
 for(let i=0;i<5;i++) {
   const createIO=options=>createRunIO({...options,removeChild:removeOwned});
   const start=options=>startRunner({...options,entry:async()=>{entries++;return {ok:true,started:true,supervisorExit:0,runnerExit:0,proof:clean,outputOverflow:true};}});
   results.push(await configureRunner({env,runnerPath:'/synthetic/runner',events:new EventEmitter(),createIO,start,verifySupervisor:async()=>{},id:`synthetic-${i}`}));
 }
 const later=await createRetention({store:createFileStore({parent:evidence}),clock:()=>Date.now(),limits}).reserve({id:'legitimate'});
 assert.deepEqual(await fs.readdir(runtime),[]);
 assert.deepEqual({rows:results.map(result=>[result.outcome,result.removed]),entries,later:later.accepted},{rows:Array.from({length:5},()=>['fail',true]),entries:5,later:true});
});

test('runner summary emits only validated fixed outcome fields',async()=>{
 const {formatRunnerSummary}=await import('../runner-bootstrap.cjs');
 assert.equal(typeof formatRunnerSummary,'function');
 assert.equal(formatRunnerSummary({outcome:'prerequisite',removed:false}), 'E2E outcome=prerequisite runnerExit=unknown removed=false');
 assert.equal(formatRunnerSummary({outcome:'interrupted',runnerExit:143,removed:true,signal:'SIGTERM'}), 'E2E outcome=interrupted runnerExit=143 removed=true signal=SIGTERM');
 assert.equal(formatRunnerSummary({outcome:'synthetic private detail',runnerExit:'secret',removed:'secret',signal:'secret'}), 'E2E outcome=unknown runnerExit=unknown removed=unknown');
});

test('runner CLI reports a fixed prerequisite classification',()=>{
 const result=spawnSync(process.execPath,['web/test/team-e2e.cjs'],{env:{PATH:process.env.PATH},encoding:'utf8'});
 assert.equal(result.status,2);assert.match(result.stderr,/^E2E outcome=prerequisite runnerExit=unknown removed=false\n$/);
});

test('runner summary validates exit, removal and signal without exposing arbitrary input',async()=>{
 const {formatRunnerSummary}=await import('../runner-bootstrap.cjs');
 for(const runnerExit of [-1,256,1.5,NaN,null])assert.match(formatRunnerSummary({runnerExit}),/runnerExit=unknown/);
 for(const removed of [0,'false',null])assert.match(formatRunnerSummary({removed}),/removed=unknown/);
 assert.equal(formatRunnerSummary({outcome:'fail',runnerExit:1,removed:true,signal:'SIGINT'}),'E2E outcome=fail runnerExit=1 removed=true signal=SIGINT');
 assert.equal(formatRunnerSummary(null),'E2E outcome=unknown runnerExit=unknown removed=unknown');
});

test('runner summary distinguishes failed evidence publication',async()=>{
 const {formatRunnerSummary}=await import('../runner-bootstrap.cjs');
 assert.equal(formatRunnerSummary({outcome:'evidence-failed',runnerExit:0,removed:true}),'E2E outcome=evidence-failed runnerExit=0 removed=true');
});

test('midbody probe configuration allows only selected supported modes',async()=>{
 const {validMidbodyProbe}=await import('../runner-bootstrap.cjs');
 for(const probe of [undefined,''])for(const scenario of [undefined,'default','people-midbody-permission'])assert.equal(validMidbodyProbe(probe,scenario),true);
 for(const probe of ['failure','signal','forward-error']) {
  assert.equal(validMidbodyProbe(probe,'people-midbody-permission'),true);
  for(const scenario of [undefined,'','default','another'])assert.equal(validMidbodyProbe(probe,scenario),false);
 }
 for(const scenario of [undefined,'default','people-midbody-permission'])assert.equal(validMidbodyProbe('unknown',scenario),false);
});

test('entry rejects invalid probe before runtime checks and accepts valid selection through prerequisites',()=>{
 for(const inner of [undefined,'1'])for(const [scenario,probe,invalid] of [
  ['default','signal',true],['people-midbody-permission','unknown',true],
  ['people-midbody-permission','failure',false],['people-midbody-permission','signal',false],['people-midbody-permission','forward-error',false],
  ['default','',false],['default',undefined,false],
 ]) {
  const env={PATH:process.env.PATH,SCIMUX_E2E_SCENARIO:scenario};
  if(inner!==undefined)env.SCIMUX_E2E_INNER=inner;
  if(probe!==undefined)env.SCIMUX_E2E_MIDBODY_PROBE=probe;
  const result=spawnSync(process.execPath,['web/test/team-e2e.cjs'],{env,encoding:'utf8'});
  assert.equal(result.status,2);
  if(invalid)assert.equal(result.stderr,'midbody cleanup probe requires its selected scenario\n');
  else assert.equal(result.stderr,inner?'E2E runtime invalid\n':'E2E outcome=prerequisite runnerExit=unknown removed=false\n');
 }
});
