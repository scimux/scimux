import test from 'node:test';
import assert from 'node:assert/strict';
import {runOwned,signalIntent} from '../run-ownership.cjs';

// All identities and processes here are synthetic values, not host resources.
const runtime={uid:42,dev:7,ino:11,mode:0o700,kind:'directory'};
const runtimeParent={uid:42,dev:7,ino:10,mode:0o700,kind:'directory'};
const evidence={uid:42,dev:7,ino:12,mode:0o700,kind:'directory'};
function fixture(options={}) {
  const calls=[];
  let current={...runtime};
  const io={
    inspect:async name=>name==='runtime'?{...current}:name==='runtimeParent'?{...runtimeParent}:{...evidence},
    allocate:async()=>{calls.push('allocate');return {...runtime};},
    remove:async identity=>{calls.push(['remove',identity]);},
    signal:async target=>{calls.push(['signal',target]);},
  };
  const supervisor={launch:async()=>{calls.push('launch');return {wait:async()=>{calls.push('wait');return {exit:0,proof:'status clean\nrunner_exit 0\nreaped 0\n'};}};}};
  const project=async bound=>{calls.push(['project',bound]);return {ok:true};};
  return {args:{runtimeParent,evidenceParent:evidence,io,supervisor,project,...options},calls,replace:x=>{current=x;}};
}
const clean=exit=>`status clean\nrunner_exit ${exit}\nreaped 0\n`;
function withWait(f, exit, proof, waitError) {
  f.args.supervisor.launch=async()=>{f.calls.push('launch');return {wait:async()=>{f.calls.push('wait');if(waitError)throw Error('synthetic wait');return {exit:0,proof};}};};
}
for(const exit of [0,7]) test(`waited clean proof permits cleanup with runner exit ${exit}`,async()=>{
  const f=fixture();withWait(f,exit,clean(exit));
  const result=await runOwned(f.args);
  assert.deepEqual(result,{ok:exit===0,outcome:exit===0?'pass':'fail',runnerExit:exit,removed:true});
  assert.deepEqual(f.calls,['allocate','launch','wait',['project',{runtime,proof:{exit:0,proof:clean(exit)}}],['remove',runtime]]);
});

test('unstarted launch failure removes only the allocated runtime',async()=>{
  const f=fixture();f.args.supervisor.launch=async()=>{f.calls.push('launch');throw Error('synthetic launch');};
  assert.deepEqual(await runOwned(f.args),{ok:false,outcome:'launch-failed',removed:true});
  assert.deepEqual(f.calls,['allocate','launch',['remove',runtime]]);
});

for(const [label,proof,err] of [['missing','',false],['malformed','status clean\nrunner_exit 0\n',false],['uncertain','status timeout\nrunner_exit 0\nreaped 0\n',false],['wait-error','',true]]) {
  test(`${label} waited proof fails closed and never removes or signals a neighbor`,async()=>{
    const f=fixture();withWait(f,0,proof,err);
    const result=await runOwned(f.args);
    assert.equal(result.ok,false);assert.equal(result.removed,false);
    assert.equal(f.calls.some(x=>Array.isArray(x)),false);
    assert.ok(f.calls.includes('wait'),'launched supervisor must be waited');
  });
}
for(const [label,mutation] of [
  ['uid',x=>({...x,uid:43})],['device',x=>({...x,dev:8})],['inode',x=>({...x,ino:13})],
  ['mode',x=>({...x,mode:0o755})],['symlink',x=>({...x,kind:'symlink'})],
]) test(`changed runtime ${label} is never adopted or removed`,async()=>{
  const f=fixture();withWait(f,0,clean(0));
  f.args.supervisor.launch=async()=>{f.calls.push('launch');return {wait:async()=>{f.calls.push('wait');f.replace(mutation(runtime));return {exit:0,proof:clean(0)};}};};
  const result=await runOwned(f.args);
  assert.equal(result.ok,false);assert.equal(result.removed,false);
  assert.ok(f.calls.includes('wait'));assert.equal(f.calls.some(x=>Array.isArray(x)),false);
});

test('runtime and evidence parents must be distinct registered identities',async()=>{
  const f=fixture({evidenceParent:runtimeParent});
  const result=await runOwned(f.args);
  assert.equal(result.ok,false);assert.deepEqual(f.calls,[]);
});

test('signal while validating parents stops before runtime allocation',async()=>{
  const f=fixture();let release,entered;const gate=new Promise(resolve=>{release=resolve}),ready=new Promise(resolve=>{entered=resolve});
  const inspect=f.args.io.inspect;f.args.io.inspect=async name=>{if(name==='runtimeParent'){entered();await gate;}return inspect(name);};
  const intent=signalIntent();f.args.intent=intent;const pending=runOwned(f.args);
  await ready;intent.receive('SIGINT');release();const result=await pending;
  assert.equal(result.outcome,'interrupted');assert.equal(result.signal,'SIGINT');assert.deepEqual(f.calls,[]);
  assert.equal(result.removed,false);assert.equal(result.unstarted,true);
});

test('finalization exception cannot turn an unstarted cancellation into success',async()=>{
  const f=fixture(),intent=signalIntent();f.args.intent=intent;
  const allocate=f.args.io.allocate;f.args.io.allocate=async()=>{const value=await allocate();intent.receive('SIGINT');return value;};
  f.args.finalize=async()=>{throw Error('synthetic finalization failure');};
  const result=await runOwned(f.args);assert.equal(result.outcome,'fail');assert.equal(result.removed,true);assert.equal(result.ok,false);
  assert.deepEqual(f.calls,['allocate',['remove',runtime]]);
});

test('invalid allocated runtime is retained without launching',async()=>{
  const f=fixture();f.args.io.allocate=async()=>{f.calls.push('allocate');return {...runtime,mode:0o755};};
  const result=await runOwned(f.args);assert.equal(result.outcome,'invalid-runtime');assert.equal(result.removed,false);assert.deepEqual(f.calls,['allocate']);
});

test('out-of-range runner exit cannot certify cleanup',async()=>{
  const f=fixture();withWait(f,256,clean(256));const result=await runOwned(f.args);
  assert.equal(result.ok,false);assert.equal(result.removed,false);assert.equal(result.outcome,'unproven');
});
for(const [first,second] of [['SIGINT','SIGINT'],['SIGINT','SIGTERM'],['SIGTERM','SIGINT']]) test(`first ${first} intent survives later ${second}`,()=>{
  const intent=signalIntent();intent.receive(first);intent.receive(second);
  assert.equal(intent.current(),first);
});

test('teardown remains pending through repeated signals until its wait completes',async()=>{
  const f=fixture();let release;const blocked=new Promise(resolve=>{release=resolve});
  f.args.supervisor.launch=async()=>{f.calls.push('launch');return {wait:async()=>{f.calls.push('wait');await blocked;return {exit:0,proof:clean(0)};}};};
  const intent=signalIntent();f.args.intent=intent;
  let settled=false;const pending=runOwned(f.args).then(x=>{settled=true;return x;});
  await new Promise(resolve=>setImmediate(resolve));intent.receive('SIGINT');intent.receive('SIGTERM');
  try {assert.equal(settled,false,'repeated signal must not end a blocked wait');}
  finally {release();}
  const result=await pending;
  assert.equal(result.outcome,'interrupted');assert.equal(result.signal,'SIGINT');assert.equal(result.removed,true);
});

test('waited supervisor timeout retains runtime and does not project or signal neighbors',async()=>{
  const f=fixture();
  f.args.supervisor.launch=async()=>{f.calls.push('launch');return {wait:async()=>{f.calls.push('wait');return {exit:1,proof:'status timeout\nrunner_exit 0\nreaped 0\n'};}};};
  const result=await runOwned(f.args);
  assert.equal(result.outcome,'timeout');assert.equal(result.removed,false);
  assert.deepEqual(f.calls,['allocate','launch','wait']);
});

test('entry forwards only explicit standard streams and separates supervisor proof',async()=>{
  const f=fixture();const streams={stdin:{id:'input'},stdout:{id:'output'},stderr:{id:'error'}};
  f.args.streams=streams;
  f.args.supervisor.launch=async options=>{f.calls.push(options);return {wait:async()=>({exit:0,proof:clean(0)})};};
  const result=await runOwned(f.args);
  assert.equal(result.ok,true);
  const launched=f.calls.find(x=>x && x.streams);
  assert.deepEqual(launched.streams,streams);
  assert.deepEqual(Object.keys(launched.streams).sort(),['stderr','stdin','stdout']);
  assert.equal(launched.streams.stdout,streams.stdout);
});

test('controlled runner failure remains failure when teardown also receives a signal',async()=>{
  const f=fixture();const intent=signalIntent();f.args.intent=intent;
  f.args.supervisor.launch=async()=>{f.calls.push('launch');return {wait:async()=>{intent.receive('SIGINT');return {exit:0,proof:clean(7)};}};};
  const result=await runOwned(f.args);
  assert.equal(result.outcome,'fail');assert.equal(result.runnerExit,7);assert.equal(result.removed,true);
});

test('evidence parent symlink is refused before launch',async()=>{
  const f=fixture();f.args.io.inspect=async name=>name==='runtime'?runtime:{...evidence,kind:'symlink'};
  const result=await runOwned(f.args);assert.equal(result.ok,false);
  assert.deepEqual(f.calls,[]);
});

test('cleanup failure retains a genuine controlled runner failure',async()=>{
  const f=fixture();withWait(f,7,clean(7));
  f.args.io.remove=async()=>{f.calls.push('remove-failed');throw Error('synthetic remove');};
  const result=await runOwned(f.args);
  assert.equal(result.ok,false);assert.equal(result.outcome,'fail');assert.equal(result.runnerExit,7);assert.equal(result.removed,false);
});

test('nonzero supervisor exit cannot certify a clean-looking proof',async()=>{
  const f=fixture();f.args.supervisor.launch=async()=>{f.calls.push('launch');return {wait:async()=>{f.calls.push('wait');return {exit:1,proof:clean(0)};}};};
  const result=await runOwned(f.args);assert.equal(result.ok,false);assert.equal(result.removed,false);
  assert.ok(f.calls.includes('wait'));assert.equal(f.calls.some(x=>Array.isArray(x)),false);
});

for(const exit of [0,7]) test(`projection failure cleans proven runtime and preserves runner exit ${exit}`,async()=>{
  const f=fixture();withWait(f,exit,clean(exit));
  f.args.project=async bound=>{f.calls.push(['project',bound]);return {ok:false,category:'audit-failure'};};
  const result=await runOwned(f.args);
  assert.equal(result.ok,false);assert.equal(result.removed,true);assert.equal(result.runnerExit,exit);
  assert.equal(result.outcome,exit===0?'evidence-failed':'fail');
  assert.deepEqual(f.calls,['allocate','launch','wait',['project',{runtime,proof:{exit:0,proof:clean(exit)}}],['remove',runtime]]);
});

test('projection exception still permits removal after waited clean proof',async()=>{
  const f=fixture();withWait(f,0,clean(0));
  f.args.project=async bound=>{f.calls.push(['project',bound]);throw Error('synthetic audit failure');};
  const result=await runOwned(f.args);
  assert.equal(result.ok,false);assert.equal(result.removed,true);
  assert.deepEqual(f.calls,['allocate','launch','wait',['project',{runtime,proof:{exit:0,proof:clean(0)}}],['remove',runtime]]);
});

test('runtime remains until projection, audit and publication finish',async()=>{
  const f=fixture();withWait(f,0,clean(0));let release;
  const blocked=new Promise(resolve=>{release=resolve});
  f.args.project=async bound=>{f.calls.push(['project',bound]);await blocked;f.calls.push('published');return {ok:true};};
  let settled=false;const pending=runOwned(f.args).then(value=>{settled=true;return value;});
  await new Promise(resolve=>setImmediate(resolve));
  try {
    assert.equal(settled,false);assert.deepEqual(f.calls,['allocate','launch','wait',['project',{runtime,proof:{exit:0,proof:clean(0)}}]]);
  } finally {release();}
  const result=await pending;
  assert.equal(result.removed,true);assert.deepEqual(f.calls.slice(-2),['published',['remove',runtime]]);
});

test('only the first signal is forwarded even when it arrives before supervisor attachment',()=>{
  const sent=[];const intent=signalIntent();
  intent.receive('SIGINT');intent.attach(signal=>sent.push(signal));
  intent.receive('SIGINT');intent.receive('SIGTERM');
  assert.equal(intent.current(),'SIGINT');assert.deepEqual(sent,['SIGINT']);
});

test('entry transports runner streams independently of waited supervisor proof',async()=>{
  const {EventEmitter}=await import('node:events');const {PassThrough}=await import('node:stream');
  const {runSupervisorEntry}=await import('../run-ownership.cjs');
  let options;
  const spawnProcess=(_binary,_args,opts)=>{
    options=opts;const child=new EventEmitter();child.stdin=new PassThrough();child.stdout=new PassThrough();child.stderr=new PassThrough();
    child.stdio=[child.stdin,child.stdout,child.stderr,new PassThrough(),new PassThrough()];
    queueMicrotask(()=>{child.stdout.end(clean(7));child.stdio[3].end(clean(0));child.stdio[4].end('synthetic stderr');child.emit('close',0,null);});
    return child;
  };
  const result=await runSupervisorEntry({supervisor:'synthetic',command:['runner'],stdin:'synthetic',spawnProcess});
  assert.equal(result.ok,true);assert.equal(result.runnerExit,7);assert.equal(result.proof,clean(7));
  assert.equal(result.stdout,clean(0));assert.equal(result.stderr,'synthetic stderr');
  assert.deepEqual(options.stdio,['pipe','pipe','pipe','pipe','pipe']);
  assert.equal(options.env.TESTOWNED_BROWSER_TEARDOWN,'1');
});

test('entry forwards live standard streams while supervisor is still running',async()=>{
  const {EventEmitter}=await import('node:events');const {PassThrough}=await import('node:stream');
  const {runSupervisorEntry}=await import('../run-ownership.cjs');
  const input=new PassThrough(),output=new PassThrough(),error=new PassThrough();
  let out='',err='';output.on('data',x=>{out+=x;});error.on('data',x=>{err+=x;});
  let child;
  const spawnProcess=()=>{child=new EventEmitter();child.stdin=new PassThrough();child.stdout=new PassThrough();child.stderr=new PassThrough();child.stdio=[child.stdin,child.stdout,child.stderr,new PassThrough(),new PassThrough()];return child;};
  const pending=runSupervisorEntry({supervisor:'synthetic',command:['runner'],streams:{stdin:input,stdout:output,stderr:error},spawnProcess});
  let received='';child.stdin.on('data',x=>{received+=x;});
  input.write('input');child.stdio[3].write('visible');child.stdio[4].write('diagnostic');
  try {assert.equal(received,'input');assert.equal(out,'visible');assert.equal(err,'diagnostic');}
  finally {child.stdout.end(clean(0));child.stdio[3].end();child.stdio[4].end();child.emit('close',0,null);input.end();}
  assert.equal((await pending).ok,true);
});

for(const failure of ['proof-overflow','signal-exit','stdin-error','forward-refused','string-chunk']) test(`supervisor entry handles ${failure}`,async()=>{
  const {EventEmitter}=await import('node:events');const {PassThrough}=await import('node:stream');
  const {runSupervisorEntry}=await import('../run-ownership.cjs');
  const child=new EventEmitter();child.pid=42;child.stdin=new PassThrough();child.stdout=new PassThrough();child.stderr=new PassThrough();
  child.stdio=[child.stdin,child.stdout,child.stderr,new PassThrough(),new PassThrough()];child.kill=()=>false;
  if(failure==='string-chunk')child.stdio[3].setEncoding('utf8');
  const intent=signalIntent();if(failure==='forward-refused')intent.receive('SIGINT');
  const pending=runSupervisorEntry({supervisor:'synthetic',command:[],spawnProcess:()=>child,intent});
  if(failure==='stdin-error')child.stdin.emit('error',Error('synthetic stdin error'));
  child.stdout.end(failure==='proof-overflow'?clean(0)+'x'.repeat(1024):clean(0));child.stdio[3].end('synthetic');child.stdio[4].end();
  child.emit('close',0,failure==='signal-exit'?'SIGTERM':null);const result=await pending;
  if(failure==='proof-overflow'||failure==='signal-exit'){assert.equal(result.ok,false);assert.equal(result.category,'unproven');}
  else {assert.equal(result.ok,true);assert.equal(result.stdout,'synthetic');if(failure==='forward-refused')assert.equal(result.forwardFailed,true);}
});

for(const exit of [0,7]) test(`rejected staged evidence cleans proven runtime and preserves runner exit ${exit}`,async()=>{
  const {projectEvidence}=await import('../evidence-project.cjs');
  const f=fixture();withWait(f,exit,clean(exit));const staged=new Map(),published=[];
  const source={manifest:{version:1,scope:'synthetic',result:'pass',assertions:[{id:'synthetic',result:'pass'}],artifacts:['screen.png'],counters:{}},files:{'screen.png':{kind:'file',identity:'screen',captureIdentity:'screen',bytes:Buffer.from('synthetic')}}};
  let audited=false;
  f.args.project=async()=>projectEvidence({source,proof:{waited:true,clean:true},limits:{run:1024,aggregate:2048,diagnostics:16,registry:32},usage:{staging:0,registryCopies:0,diagnostics:0,otherRuns:0},io:{read:async name=>source.files[name],write:async(name,bytes)=>staged.set(name,Buffer.from(bytes)),snapshot:async()=>[],publish:async value=>published.push(value)},auditor:async()=>{audited=true;return {ok:false};}});
  const result=await runOwned(f.args);
  assert.equal(result.ok,false);assert.equal(result.removed,true);assert.equal(result.runnerExit,exit);assert.equal(result.outcome,exit===0?'evidence-failed':'fail');
  assert.ok(staged.has('manifest.json'));assert.equal(audited,true);assert.equal(published.length,0);
});

test('bootstrap delegates the registered runtime to the inner runner under the supervisor',async()=>{
  const {bootstrapRunner}=await import('../run-ownership.cjs');
  const f=fixture();const root={...runtime,path:'/synthetic/owned',work:'/synthetic/owned/work',tmp:'/synthetic/owned/tmp',workIdentity:{uid:42,dev:7,ino:15}};
  f.args.io.allocate=async()=>{f.calls.push('allocate');return root;};
  f.args.io.inspect=async name=>name==='runtime'?root:name==='runtimeParent'?runtimeParent:evidence;
  let launch;
  const entry=async options=>{launch=options;f.calls.push('entry');return {ok:true,supervisorExit:0,runnerExit:0,proof:clean(0)};};
  const result=await bootstrapRunner({...f.args,entry,supervisorPath:'/synthetic/supervisor',runnerPath:'/synthetic/runner',env:{SYNTHETIC:'1'}});
  assert.equal(result.ok,true);assert.equal(result.removed,true);
  assert.deepEqual(launch.command,[process.execPath,'/synthetic/runner']);
  assert.equal(launch.supervisor,'/synthetic/supervisor');
  assert.equal(launch.env.SCIMUX_E2E_INNER,'1');assert.equal(launch.env.SCIMUX_E2E_RUNTIME,root.work);assert.equal(launch.env.TMPDIR,root.tmp);
  assert.equal(launch.env.SCIMUX_E2E_RUNTIME_IDENTITY,JSON.stringify(root.workIdentity));
  assert.equal(launch.env.SYNTHETIC,'1');
  assert.deepEqual(f.calls,['allocate','entry',['project',{runtime:root,proof:{exit:0,proof:clean(0)}}],['remove',root]]);
});

for(const started of [false,true]) test(`bootstrap distinguishes ${started?'launched uncertainty':'unstarted failure'} for cleanup`,async()=>{
  const {bootstrapRunner}=await import('../run-ownership.cjs');const f=fixture();
  const entry=async()=>({ok:false,started,category:'unproven',proof:''});
  const result=await bootstrapRunner({...f.args,entry,supervisorPath:'/synthetic/supervisor',runnerPath:'/synthetic/runner',env:{}});
  assert.equal(result.ok,false);assert.equal(result.removed,!started);
  assert.equal(f.calls.some(x=>Array.isArray(x)&&x[0]==='remove'),!started);
  assert.equal(f.calls.some(x=>Array.isArray(x)&&x[0]==='project'),false);
});

test('a refused removal cannot be reported as removed',async()=>{
  const f=fixture();withWait(f,0,clean(0));
  f.args.io.remove=async()=>{f.calls.push('remove-refused');return false;};
  const result=await runOwned(f.args);
  assert.equal(result.ok,false);assert.equal(result.removed,false);
});

test('launched supervisor error cannot settle before close and preserves clean waited proof',async()=>{
  const {EventEmitter}=await import('node:events');const {PassThrough}=await import('node:stream');
  const {runSupervisorEntry}=await import('../run-ownership.cjs');
  const child=new EventEmitter();child.pid=42;child.stdin=new PassThrough();child.stdout=new PassThrough();child.stderr=new PassThrough();
  child.stdio=[child.stdin,child.stdout,child.stderr,new PassThrough(),new PassThrough()];
  let settled=false;const pending=runSupervisorEntry({supervisor:'synthetic',command:[],spawnProcess:()=>child}).then(result=>{settled=true;return result;});
  child.emit('error',Error('synthetic post-launch signal error'));
  await new Promise(resolve=>setImmediate(resolve));
  try {assert.equal(settled,false,'error is not a close receipt');}
  finally {child.stdout.end(clean(7));child.stdio[3].end();child.stdio[4].end();child.emit('close',0,null);}
  const result=await pending;assert.equal(result.ok,true);assert.equal(result.runnerExit,7);assert.equal(result.started,true);
});

for(const kind of ['sync','async']) test(`${kind} pre-launch supervisor failure is reported unstarted`,async()=>{
  const {EventEmitter}=await import('node:events');const {PassThrough}=await import('node:stream');
  const {runSupervisorEntry}=await import('../run-ownership.cjs');
  const spawnProcess=()=>{
    if(kind==='sync')throw Error('synthetic spawn failure');
    const child=new EventEmitter();child.stdin=new PassThrough();child.stdout=new PassThrough();child.stderr=new PassThrough();
    child.stdio=[child.stdin,child.stdout,child.stderr,new PassThrough(),new PassThrough()];
    queueMicrotask(()=>{child.emit('error',Error('synthetic spawn failure'));child.emit('close',-2,null);});return child;
  };
  const result=await runSupervisorEntry({supervisor:'synthetic',command:[],spawnProcess});
  assert.equal(result.ok,false);assert.equal(result.started,false);
});

test('outer signal listeners preserve and forward only the first intent until disposed',async()=>{
  const {EventEmitter}=await import('node:events');const {watchSignals}=await import('../run-ownership.cjs');
  const events=new EventEmitter(),intent=signalIntent(),sent=[];intent.attach(signal=>sent.push(signal));
  const dispose=watchSignals(events,intent);
  events.emit('SIGTERM');events.emit('SIGINT');
  assert.equal(intent.current(),'SIGTERM');assert.deepEqual(sent,['SIGTERM']);
  dispose();assert.equal(events.listenerCount('SIGTERM'),0);assert.equal(events.listenerCount('SIGINT'),0);
});

test('signal forwarding failure cannot end a launched supervisor wait early',async()=>{
  const {EventEmitter}=await import('node:events');const {PassThrough}=await import('node:stream');
  const {runSupervisorEntry}=await import('../run-ownership.cjs');
  const child=new EventEmitter();child.pid=42;child.stdin=new PassThrough();child.stdout=new PassThrough();child.stderr=new PassThrough();
  child.stdio=[child.stdin,child.stdout,child.stderr,new PassThrough(),new PassThrough()];child.kill=()=>{throw Error('synthetic signal error');};
  const intent=signalIntent();intent.receive('SIGINT');let settled=false;
  const pending=runSupervisorEntry({supervisor:'synthetic',command:[],spawnProcess:()=>child,intent}).then(value=>{settled=true;return value;},()=>{settled=true;return null;});
  await new Promise(resolve=>setImmediate(resolve));
  try {assert.equal(settled,false,'forwarding failure is not a close receipt');}
  finally {child.stdout.end(clean(7));child.stdio[3].end();child.stdio[4].end();child.emit('close',0,null);}
  const result=await pending;assert.equal(result?.started,true);assert.equal(result?.runnerExit,7);assert.equal(result?.forwardFailed,true);
});

test('bootstrap does not report success when forwarding the first signal failed',async()=>{
  const {bootstrapRunner}=await import('../run-ownership.cjs');const f=fixture();
  const entry=async()=>({ok:true,started:true,supervisorExit:0,runnerExit:0,proof:clean(0),forwardFailed:true});
  const result=await bootstrapRunner({...f.args,entry,supervisorPath:'/synthetic/supervisor',runnerPath:'/synthetic/runner',env:{}});
  assert.equal(result.ok,false);assert.equal(result.removed,true);assert.equal(result.outcome,'fail');
});

test('unstarted runtime is cleaned after a transient identity inspection failure',async()=>{
  const f=fixture();const inspect=f.args.io.inspect;let attempts=0,launches=0;
  f.args.io.inspect=async name=>{if(name==='runtime'&&attempts++===0)throw Error('synthetic inspect failure');return inspect(name);};
  f.args.supervisor.launch=async()=>{launches++;};
  const result=await runOwned(f.args);
  assert.equal(result.outcome,'allocation-failed');assert.equal(result.removed,true);assert.equal(launches,0);
  assert.deepEqual(f.calls,['allocate',['remove',runtime]]);
});

test('unstarted runtime is preserved if identity remains uncertain after inspection failure',async()=>{
  const f=fixture();const inspect=f.args.io.inspect;let launches=0;
  f.args.io.inspect=async name=>{if(name==='runtime')throw Error('synthetic inspect failure');return inspect(name);};
  f.args.supervisor.launch=async()=>{launches++;};
  const result=await runOwned(f.args);
  assert.equal(result.outcome,'allocation-failed');assert.equal(result.removed,false);assert.equal(launches,0);
  assert.deepEqual(f.calls,['allocate']);
});

test('unstarted runtime is preserved if its identity changes after an inspection error',async()=>{
  const f=fixture();const inspect=f.args.io.inspect;let attempts=0,launches=0;
  f.args.io.inspect=async name=>{if(name==='runtime'){if(attempts++===0)throw Error('synthetic inspection error');return {...runtime,ino:99};}return inspect(name);};
  f.args.supervisor.launch=async()=>{launches++;};
  const result=await runOwned(f.args);
  assert.equal(result.outcome,'allocation-failed');assert.equal(result.removed,false);assert.equal(launches,0);assert.deepEqual(f.calls,['allocate']);
});

for(const failure of ['refuse','throw']) test(`unstarted runtime remains when retry cleanup ${failure}s`,async()=>{
  const f=fixture();const inspect=f.args.io.inspect;let attempts=0;
  f.args.io.inspect=async name=>{if(name==='runtime'&&attempts++===0)throw Error('synthetic inspection error');return inspect(name);};
  f.args.io.remove=async()=>{f.calls.push('remove-attempt');if(failure==='throw')throw Error('synthetic removal error');return false;};
  const result=await runOwned(f.args);
  assert.equal(result.outcome,'allocation-failed');assert.equal(result.removed,false);assert.deepEqual(f.calls,['allocate','remove-attempt']);
});

for(const channel of [3,4]) test(`supervisor entry reports runner stream ${channel} overflow separately from clean proof`,async()=>{
  const {EventEmitter}=await import('node:events');const {PassThrough}=await import('node:stream');
  const {runSupervisorEntry}=await import('../run-ownership.cjs');
  const child=new EventEmitter();child.pid=42;child.stdin=new PassThrough();child.stdout=new PassThrough();child.stderr=new PassThrough();
  child.stdio=[child.stdin,child.stdout,child.stderr,new PassThrough(),new PassThrough()];
  const pending=runSupervisorEntry({supervisor:'synthetic',command:[],spawnProcess:()=>child});
  child.stdout.end(clean(0));child.stdio[channel].write('x'.repeat(1048577));
  child.stdio[3].end();child.stdio[4].end();child.emit('close',0,null);
  const result=await pending;
  assert.equal(result.ok,true);assert.equal(result.started,true);assert.equal(result.runnerExit,0);assert.equal(result.outputOverflow,true);
});

for(const channel of [3,4]) test(`supervisor entry counts UTF-8 bytes on runner stream ${channel}`,async()=>{
  const {EventEmitter}=await import('node:events');const {PassThrough}=await import('node:stream');
  const {runSupervisorEntry}=await import('../run-ownership.cjs');
  const child=new EventEmitter();child.pid=42;child.stdin=new PassThrough();child.stdout=new PassThrough();child.stderr=new PassThrough();
  child.stdio=[child.stdin,child.stdout,child.stderr,new PassThrough(),new PassThrough()];
  const pending=runSupervisorEntry({supervisor:'synthetic',command:[],spawnProcess:()=>child});
  child.stdout.end(clean(0));child.stdio[channel].write(Buffer.from('€'.repeat(400000)));
  child.stdio[3].end();child.stdio[4].end();child.emit('close',0,null);
  const result=await pending;assert.equal(result.ok,true);assert.equal(result.outputOverflow,true);
  assert.ok(Buffer.byteLength(channel===3?result.stdout:result.stderr)<=1048576);
});

for(const channel of [3,4]) test(`supervisor entry decodes split UTF-8 exactly at stream ${channel} byte limit`,async()=>{
  const {EventEmitter}=await import('node:events');const {PassThrough}=await import('node:stream');
  const {runSupervisorEntry}=await import('../run-ownership.cjs');
  const child=new EventEmitter();child.pid=42;child.stdin=new PassThrough();child.stdout=new PassThrough();child.stderr=new PassThrough();
  child.stdio=[child.stdin,child.stdout,child.stderr,new PassThrough(),new PassThrough()];
  const output=new PassThrough();let forwarded=[];output.on('data',data=>forwarded.push(data));
  const streams=channel===3?{stdout:output}:{stderr:output};
  const pending=runSupervisorEntry({supervisor:'synthetic',command:[],streams,spawnProcess:()=>child});
  const value='€'.repeat(349525)+'a',bytes=Buffer.from(value);child.stdout.end(clean(0));
  child.stdio[channel].write(bytes.subarray(0,1));child.stdio[channel].write(bytes.subarray(1));
  child.stdio[3].end();child.stdio[4].end();child.emit('close',0,null);
  const result=await pending;assert.equal(result.ok,true);assert.equal(result.outputOverflow,undefined);
  assert.equal(channel===3?result.stdout:result.stderr,value);assert.deepEqual(Buffer.concat(forwarded),bytes);
});

for(const proof of [clean(0),'status timeout\nrunner_exit 0\nreaped 0\n']) test(`bootstrap handles output overflow with ${proof.startsWith('status clean')?'clean':'unknown'} proof`,async()=>{
  const {bootstrapRunner,runSupervisorEntry}=await import('../run-ownership.cjs');
  const {EventEmitter}=await import('node:events');const {PassThrough}=await import('node:stream');const f=fixture();
  const entry=async options=>runSupervisorEntry({...options,spawnProcess:()=>{
    const child=new EventEmitter();child.pid=42;child.stdin=new PassThrough();child.stdout=new PassThrough();child.stderr=new PassThrough();
    child.stdio=[child.stdin,child.stdout,child.stderr,new PassThrough(),new PassThrough()];
    queueMicrotask(()=>{child.stdout.end(proof);child.stdio[3].end('x'.repeat(1048577));child.stdio[4].end();child.emit('close',0,null);});return child;
  }});
  const result=await bootstrapRunner({...f.args,entry,supervisorPath:'/synthetic/supervisor',runnerPath:'/synthetic/runner',env:{}});
  assert.equal(result.ok,false);assert.equal(result.removed,proof===clean(0));
  if(proof===clean(0))assert.equal(result.outcome,'fail');
  assert.equal(f.calls.some(x=>Array.isArray(x)&&x[0]==='remove'),proof===clean(0));
  assert.equal(f.calls.some(x=>Array.isArray(x)&&x[0]==='project'),false,'overflow must not publish acceptance evidence');
});

for(const channel of [3,4]) test(`supervisor entry accepts runner stream ${channel} exactly at capture bound`,async()=>{
  const {EventEmitter}=await import('node:events');const {PassThrough}=await import('node:stream');
  const {runSupervisorEntry}=await import('../run-ownership.cjs');
  const child=new EventEmitter();child.pid=42;child.stdin=new PassThrough();child.stdout=new PassThrough();child.stderr=new PassThrough();
  child.stdio=[child.stdin,child.stdout,child.stderr,new PassThrough(),new PassThrough()];
  const pending=runSupervisorEntry({supervisor:'synthetic',command:[],spawnProcess:()=>child});
  child.stdout.end(clean(0));child.stdio[channel].write('x'.repeat(1048576));
  child.stdio[3].end();child.stdio[4].end();child.emit('close',0,null);
  const result=await pending;
  assert.equal(result.ok,true);assert.equal(result.runnerExit,0);
  assert.equal((channel===3?result.stdout:result.stderr).length,1048576);
});

test('bootstrap retains runtime when entry throws with unknown launch state',async()=>{
  const {bootstrapRunner}=await import('../run-ownership.cjs');const f=fixture();
  const entry=async()=>{f.calls.push('entry');throw Error('synthetic unknown launch state');};
  const result=await bootstrapRunner({...f.args,entry,supervisorPath:'/synthetic/supervisor',runnerPath:'/synthetic/runner',env:{}});
  assert.equal(result.removed,false);assert.equal(result.ok,false);assert.equal(result.outcome,'unproven');
  assert.deepEqual(f.calls,['allocate','entry']);
});
