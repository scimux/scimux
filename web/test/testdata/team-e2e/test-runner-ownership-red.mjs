import test from 'node:test';
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {mkdir, readFile, readdir, writeFile} from 'node:fs/promises';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {bounded,track,stopOwned,waitForMarker} from './probe-process.cjs';

// Opt in only inside a fresh, externally supervised process namespace. The
// namespace owner removes its registered private root after all children exit.
const enabled=process.env.SCIMUX_E2E_OWNERSHIP_RED==='1';
const runner=fileURLToPath(new URL('../../team-e2e.cjs',import.meta.url));
const tmp=process.env.SCIMUX_E2E_PROBE_TMP;

async function start(t, probe, dir) {
  await mkdir(dir,{mode:0o700});
  const runtimeParent=path.join(dir,'r'),evidenceParent=path.join(tmp,'e');
  await mkdir(runtimeParent,{mode:0o700});await mkdir(evidenceParent,{mode:0o700,recursive:true});
  const before=new Set(await readdir(evidenceParent));
  const env={...process.env,TMPDIR:dir,SCIMUX_E2E_RUNTIME_PARENT:runtimeParent,SCIMUX_E2E_EVIDENCE_PARENT:evidenceParent,SCIMUX_E2E_SCENARIO:'browser-lifecycle'};
  if(probe)env.SCIMUX_E2E_CLEANUP_PROBE=probe;
  else delete env.SCIMUX_E2E_CLEANUP_PROBE;
  if(probe==='overlap')env.SCIMUX_E2E_HOLD_CLEANUP='1';
  const child=spawn(process.execPath,[runner],{env,stdio:['pipe','pipe','pipe']});
  let output='', errors='';
  child.stdout.on('data',x=>{output+=x});child.stderr.on('data',x=>{errors+=x});
  const exited=track(child);
  t.after(async()=>{await stopOwned(child,exited);});
  const waitFor=(text,count=1)=>waitForMarker(child,()=>output,text,count);
  return {child,exited,waitFor,before,output:()=>output,errors:()=>errors};
}

async function assertCleanEvidence(dir,before,expected,signal) {
  const evidenceParent=path.join(tmp,'e');
  const roots=(await readdir(evidenceParent)).filter(x=>x.startsWith('bundle-')&&!before.has(x));
  assert.equal(roots.length,1,'expected one evidence bundle');
  const root=path.join(evidenceParent,roots[0]);
  const manifest=JSON.parse(await readFile(path.join(root,'manifest.json'),'utf8'));
  assert.equal(manifest.result,expected);
  assert.equal(manifest.scope,expected==='pass'?'focused':expected==='fail'?'failure':'interrupted');
  if(signal)assert.equal(manifest.signal,signal,'first interruption remains recorded');
  if(expected==='pass')assert.ok(manifest.assertions.some(x=>x.id==='browser-lifecycle-retains-coverage-desktop'&&x.result==='pass'),'real browser lifecycle and zero-dispatch check completed');
  const entries=await readdir(root);
  const runtime=entries.filter(x=>['bin','relay','owner-a','owner-b','recipient','observer'].includes(x));
  assert.deepEqual(runtime,[],'retained evidence must not include runtime homes, binaries or relay state');
  assert.deepEqual(await readdir(path.join(dir,'r')),[],'proven runtime must be removed');
}

for(const [label,probe,code,result] of [['success','',0,'pass'],['failure','failure',1,'fail']]) {
  test(`runner ${label} keeps runtime out of retained evidence`,{skip:!enabled,timeout:300000},async t=>{
    const dir=path.join(tmp,label);const run=await start(t,probe,dir);
    const outcome=await run.exited;
    assert.equal(outcome.signal,null);assert.equal(outcome.code,code,'runner reached its expected outcome');
    await assertCleanEvidence(dir,run.before,result);
  });
}

for(const [label,first,second] of [['same','SIGINT','SIGINT'],['mixed','SIGINT','SIGTERM']]) {
  test(`runner survives ${label} signals during blocked cleanup`,{skip:!enabled,timeout:300000},async t=>{
    const dir=path.join(tmp,label);const neighbor=path.join(tmp,label+'-neighbor');
    await mkdir(neighbor,{mode:0o700});await writeFile(path.join(neighbor,'marker'),'synthetic');
    const other=spawn(process.execPath,['-e',"process.stdin.on('data',()=>process.stdout.write('alive\\n'));process.stdin.resume()"],{stdio:['pipe','pipe','ignore']});
    const neighborClosed=track(other);
    t.after(async()=>{await stopOwned(other,neighborClosed);});
    const run=await start(t,'overlap',dir);
    await run.waitFor('Cleanup overlap probe ready');
    assert.equal(run.child.kill(first),true);await run.waitFor('Cleanup signal received');
    assert.equal(run.child.kill(second),true);
    await new Promise(resolve=>setTimeout(resolve,100));
    // The unrelated process and directory must remain usable.
    const alive=new Promise(resolve=>other.stdout.once('data',resolve));other.stdin.write('probe');await bounded(alive,5000,'neighbor did not answer');
    assert.equal(await readFile(path.join(neighbor,'marker'),'utf8'),'synthetic');
    if(run.child.exitCode===null&&run.child.signalCode===null)run.child.stdin.write('release');
    const outcome=await run.exited;
    assert.equal(outcome.signal,null,'repeated signal must not terminate during cleanup');
    assert.equal(outcome.code,130,'first interruption remains authoritative');
    await assertCleanEvidence(dir,run.before,'interrupted',first);
  });
}
