import test from 'node:test';
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {readFile} from 'node:fs/promises';
import {fileURLToPath} from 'node:url';

const runner=fileURLToPath(new URL('../../team-e2e.cjs',import.meta.url));
for(const [probe,signal,exitCode] of [['overlap','SIGINT',130],['overlap','SIGTERM',143],['capture-overlap','SIGINT',130],['capture-overlap','SIGTERM',143]])test(`active browser and native ${probe}: ${signal}`,{timeout:120000},async t=>{
 const env={...process.env,SCIMUX_E2E_CLEANUP_PROBE:probe};delete env.SCIMUX_E2E_SCENARIO;delete env.SCIMUX_E2E_FAIL_AFTER_NATIVE;delete env.SCIMUX_E2E_FAIL_AFTER_START;
 const child=spawn(process.execPath,[runner],{env,stdio:['ignore','pipe','pipe']});let output='',errors='';
 t.after(()=>{if(child.exitCode===null&&child.signalCode===null)child.kill('SIGTERM')});
 child.stdout.on('data',chunk=>{output+=chunk});child.stderr.on('data',chunk=>{errors+=chunk});
 await new Promise((resolve,reject)=>{
  const timer=setTimeout(()=>reject(Error('cleanup overlap probe did not become ready')),60000);
  child.stdout.on('data',()=>{if(output.includes('Cleanup overlap probe ready')){clearTimeout(timer);resolve()}});
  child.once('exit',(code,sig)=>{clearTimeout(timer);reject(Error(`probe exited before ready: ${code}/${sig}: ${errors}`))});
 });
 assert.equal(child.kill(signal),true);
 const [code,sig]=await new Promise(resolve=>child.once('close',(a,b)=>resolve([a,b])));
 assert.equal(sig,null);assert.equal(code,exitCode);
 const match=output.match(/Artifacts: (.+)/);assert.ok(match,`missing artifact path: ${output} ${errors}`);
 t.diagnostic('Artifacts: '+match[1]);
 const manifest=JSON.parse(await readFile(match[1]+'/manifest.json','utf8'));const coverage=JSON.parse(await readFile(match[1]+'/browser-coverage.json','utf8'));
 assert.equal(manifest.result,'interrupted');assert.equal(manifest.signal,signal);assert.ok(coverage.some(item=>item.page==='cleanup-overlap'&&item.path==='/js/chat.js'));
 const paths=coverage.filter(item=>item.page==='cleanup-overlap').map(item=>item.path);assert.equal(new Set(paths).size,paths.length,'page coverage captured once');
 assert.ok(manifest.cleanup.some(item=>item.childrenStopped===true));assert.ok(manifest.cleanup.some(item=>item.socketRemoved===true));
 assert.ok(manifest.cleanup.every(item=>item.socketRemoved!==false&&(item.remainingWorkerLocators??0)===0&&(item.remainingNativeHelpers??0)===0&&!item.socketError&&!item.deleteError&&!item.fatal&&!item.browserCloseError&&item.coverage!=='failed'));
});

test('failure cleanup retains the failure and removes owned resources',{timeout:120000},async t=>{
 const env={...process.env,SCIMUX_E2E_CLEANUP_PROBE:'failure'};delete env.SCIMUX_E2E_SCENARIO;delete env.SCIMUX_E2E_FAIL_AFTER_NATIVE;delete env.SCIMUX_E2E_FAIL_AFTER_START;
 const child=spawn(process.execPath,[runner],{env,stdio:['ignore','pipe','pipe']});let output='',errors='';
 t.after(()=>{if(child.exitCode===null&&child.signalCode===null)child.kill('SIGTERM')});child.stdout.on('data',chunk=>{output+=chunk});child.stderr.on('data',chunk=>{errors+=chunk});
 const [code,sig]=await new Promise(resolve=>child.once('close',(a,b)=>resolve([a,b])));assert.equal(sig,null);assert.equal(code,1);
 const match=output.match(/Artifacts: (.+)/);assert.ok(match,`missing artifact path: ${output} ${errors}`);t.diagnostic('Artifacts: '+match[1]);
 const manifest=JSON.parse(await readFile(match[1]+'/manifest.json','utf8'));assert.equal(manifest.result,'fail');assert.match(manifest.error,/controlled native cleanup failure/);
 assert.ok(manifest.cleanup.some(item=>item.childrenStopped===true));assert.ok(manifest.cleanup.some(item=>item.socketRemoved===true));
 assert.ok(manifest.cleanup.every(item=>item.socketRemoved!==false&&(item.remainingWorkerLocators??0)===0&&(item.remainingNativeHelpers??0)===0&&!item.socketError&&!item.deleteError&&!item.fatal&&!item.browserCloseError&&item.coverage!=='failed'));
});
