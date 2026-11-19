import test from 'node:test';
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {mkdir,readFile,readdir} from 'node:fs/promises';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {auditDiagnostic} from './audit-evidence.cjs';
import {bounded,track,stopOwned,waitForMarker} from './probe-process.cjs';

// Run only in a fresh externally supervised PID namespace with guarded agent PATH.
const enabled=process.env.SCIMUX_E2E_OVERLAP_TEST==='1';
const tmp=process.env.SCIMUX_E2E_PROBE_TMP;
const runner=fileURLToPath(new URL('../../team-e2e.cjs',import.meta.url));

async function start(t,label,probe) {
 const dir=path.join(tmp,label),runtime=path.join(dir,'r'),evidence=path.join(dir,'e');
 await mkdir(dir,{mode:0o700});await mkdir(runtime,{mode:0o700});await mkdir(evidence,{mode:0o700});
 const env={...process.env,TMPDIR:dir,SCIMUX_E2E_RUNTIME_PARENT:runtime,SCIMUX_E2E_EVIDENCE_PARENT:evidence,SCIMUX_E2E_CLEANUP_PROBE:probe};
 delete env.SCIMUX_E2E_SCENARIO;delete env.SCIMUX_E2E_FAIL_AFTER_NATIVE;delete env.SCIMUX_E2E_FAIL_AFTER_START;delete env.SCIMUX_E2E_HOLD_CLEANUP;
 const child=spawn(process.execPath,[runner],{env,stdio:['ignore','pipe','pipe']});let output='';
 child.stdout.on('data',chunk=>{output+=chunk});child.stderr.resume();
 const closed=track(child);t.after(async()=>{await stopOwned(child,closed);});
 return {child,closed,runtime,evidence,ready:()=>waitForMarker(child,()=>output,'Cleanup overlap probe ready')};
}
async function inspect(run,result,signal) {
 assert.deepEqual(await readdir(run.runtime),[],'registered runtime removed');
 const roots=(await readdir(run.evidence)).filter(x=>x.startsWith('bundle-'));
 assert.equal(roots.length,1,'one retained diagnostic bundle');
 const root=path.join(run.evidence,roots[0]);
 const manifest=JSON.parse(await readFile(path.join(root,'manifest.json'),'utf8'));
 assert.equal(auditDiagnostic(manifest).ok,true,'diagnostic passes public auditor');
 assert.equal(manifest.result,result);
 if(signal)assert.equal(manifest.signal,signal);
 return {manifest,root};
}
function assertCleanup(manifest) {
 assert.equal(manifest.cleanup.childrenStopped,true,'children positively stopped');
 assert.equal(manifest.cleanup.socketRemoved,true,'socket positively removed');
 assert.deepEqual(manifest.cleanup.failures,[],'no cleanup errors or remaining workers or native helpers');
}
for(const [probe,signal,exitCode] of [['overlap','SIGINT',130],['overlap','SIGTERM',143],['capture-overlap','SIGINT',130],['capture-overlap','SIGTERM',143]])test(`active browser and native ${probe}: ${signal}`,{skip:!enabled,timeout:300000},async t=>{
 const run=await start(t,`${probe}-${signal}`,probe);await run.ready();assert.equal(run.child.kill(signal),true);
 const outcome=await bounded(run.closed,220000,'runner did not close');assert.equal(outcome.signal,null);assert.equal(outcome.code,exitCode);
 const {manifest,root}=await inspect(run,'interrupted',signal);
 const coverage=JSON.parse(await readFile(path.join(root,'browser-coverage.json'),'utf8'));
 assert.ok(coverage.some(item=>item.page==='cleanup-overlap'&&item.path==='/js/chat.js'));
 const paths=coverage.filter(item=>item.page==='cleanup-overlap').map(item=>item.path);assert.equal(new Set(paths).size,paths.length,'page coverage captured once');
 assertCleanup(manifest);
});
test('failure cleanup retains the failure and removes owned resources',{skip:!enabled,timeout:300000},async t=>{
 const run=await start(t,'failure','failure');const outcome=await bounded(run.closed,220000,'runner did not close');
 assert.equal(outcome.signal,null);assert.equal(outcome.code,1);
 const {manifest}=await inspect(run,'fail');assert.equal(manifest.failureCode,'controlled-native-cleanup');assertCleanup(manifest);
});
