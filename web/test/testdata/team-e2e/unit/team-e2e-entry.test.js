import test from 'node:test';
import assert from 'node:assert/strict';
import {fileURLToPath} from 'node:url';
import {runSupervisorEntry} from '../run-ownership.cjs';

// A built supervisor is supplied only by an external containment owner.
const enabled=process.env.SCIMUX_E2E_ENTRY_RED==='1';
const binary=process.env.SCIMUX_E2E_SUPERVISOR_BIN;
const runner=fileURLToPath(new URL('../synthetic-entry-runner.cjs',import.meta.url));
test('actual supervisor entry keeps forwarded streams separate from proof', {skip:!enabled,timeout:10000},async()=>{
  assert.ok(binary,'externally built supervisor path required');
  const result=await runSupervisorEntry({supervisor:binary,command:[process.execPath,runner],stdin:'synthetic input\n'});
  assert.equal(result.ok,true);
  assert.equal(result.supervisorExit,0);
  assert.equal(result.runnerExit,7);
  assert.equal(result.stdout,'status clean\nrunner_exit 0\nreaped 0\ninput:synthetic input\n');
  assert.equal(result.stderr,'synthetic stderr');
  assert.equal(result.proof,'status clean\nrunner_exit 7\nreaped 0\n');
});

test('proof-shaped runner output cannot certify an unproven supervisor', {skip:!enabled,timeout:10000},async()=>{
  const fake=fileURLToPath(new URL('../synthetic-unproven-supervisor.cjs',import.meta.url));
  const result=await runSupervisorEntry({supervisor:process.execPath,supervisorArgs:[fake],command:[],stdin:''});
  assert.equal(result.ok,false);assert.equal(result.category,'unproven');
  assert.equal(result.stdout,'status clean\nrunner_exit 0\nreaped 0\n');
  assert.equal(result.proof,'status timeout\nrunner_exit 0\nreaped 0\n');
});

test('source-built supervisor forwards synthetic streams on separate descriptors', {skip:!enabled,timeout:10000},async t=>{
  const {spawn}=await import('node:child_process');
  const child=spawn(binary,['--',process.execPath,runner],{env:{...process.env,TESTOWNED_FORWARD_STREAMS:'1',TESTOWNED_BROWSER_TEARDOWN:'1'},stdio:['pipe','pipe','pipe','pipe','pipe']});
  let proof='',output='',errors='';
  child.stdout.setEncoding('utf8');child.stdout.on('data',s=>{proof+=s;});
  child.stdio[3].setEncoding('utf8');child.stdio[3].on('data',s=>{output+=s;});
  child.stdio[4].setEncoding('utf8');child.stdio[4].on('data',s=>{errors+=s;});
  child.stdin.on('error',()=>{});
  const closed=new Promise((resolve,reject)=>{child.once('error',reject);child.once('close',(code,signal)=>resolve({code,signal}));});
  async function bounded(promise) {
    let timer;
    try {return await Promise.race([promise,new Promise((_,reject)=>{timer=setTimeout(()=>reject(Error('synthetic supervisor did not close')),3000);})]);}
    finally {clearTimeout(timer);}
  }
  t.after(async()=>{if(child.exitCode===null&&child.signalCode===null)child.kill('SIGTERM');await bounded(closed);});
  child.stdin.end('synthetic input\n');
  assert.deepEqual(await bounded(closed),{code:0,signal:null});
  assert.equal(output,'status clean\nrunner_exit 0\nreaped 0\ninput:synthetic input\n');
  assert.equal(errors,'synthetic stderr');
  assert.equal(proof,'status clean\nrunner_exit 7\nreaped 0\n');
});

test('synthetic runner reports an intentionally inherited extra descriptor', {skip:!enabled,timeout:10000},async()=>{
  const {spawn}=await import('node:child_process');
  const child=spawn(process.execPath,[runner],{stdio:['pipe','pipe','pipe','pipe']});let output='',error='',extra='';
  child.stdout.on('data',chunk=>{output+=chunk;});child.stderr.on('data',chunk=>{error+=chunk;});child.stdio[3].on('data',chunk=>{extra+=chunk;});
  const closed=new Promise((resolve,reject)=>{child.once('error',reject);child.once('close',(code,signal)=>resolve({code,signal}));});
  try {child.stdin.end('synthetic input\n');assert.deepEqual(await closed,{code:7,signal:null});assert.equal(extra,'unexpected');assert.equal(error,'unexpected descriptor');assert.ok(output.includes('input:synthetic input\n'));}
  finally {if(child.exitCode===null&&child.signalCode===null){child.kill('SIGKILL');await closed;}}
});
