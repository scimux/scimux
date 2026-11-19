import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {createDiagnosticBudget} from '../diagnostic-log.cjs';
import {PassThrough} from 'node:stream';

test('diagnostic log seam preserves existing append and permissions behavior',async t=>{
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'diagnostic-log-synthetic-'));t.after(()=>fs.rmSync(root,{recursive:true,force:true}));
  const previous=name=>fs.openSync(path.join(root,name+'.log'),'a',0o600);
  for(const text of ['first','second']) {const fd=previous('before');try {fs.writeSync(fd,text);}finally {fs.closeSync(fd);}}
  const budget=createDiagnosticBudget(root),stream=new PassThrough();budget.attach('seam',stream);stream.write('first');stream.end('second');
  assert.deepEqual(await budget.finish(),{bytes:11,failed:false});
  for(const name of ['before','seam']) {assert.equal(fs.readFileSync(path.join(root,name+'.log'),'utf8'),'firstsecond');assert.equal(fs.statSync(path.join(root,name+'.log')).mode&0o777,0o600);}
});

test('concurrent diagnostic streams share a byte budget and drain after overflow',async t=>{
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'diagnostic-budget-synthetic-'));t.after(()=>fs.rmSync(root,{recursive:true,force:true}));
  const budget=createDiagnosticBudget(root),a=new PassThrough(),b=new PassThrough();budget.attach('a',a);budget.attach('b',b);
  a.write(Buffer.alloc(600000,0x61));b.write(Buffer.alloc(448576,0x62));a.write('€');b.end('after overflow');a.end();
  const result=await budget.finish();assert.equal(result.bytes,1048576);assert.equal(result.failed,true);
  assert.equal(fs.statSync(path.join(root,'a.log')).size+fs.statSync(path.join(root,'b.log')).size,1048576);
});

test('diagnostic byte boundary accepts UTF-8 and append and waits for late stream close',async t=>{
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'diagnostic-budget-synthetic-'));t.after(()=>fs.rmSync(root,{recursive:true,force:true}));
  const budget=createDiagnosticBudget(root),first=new PassThrough();budget.attach('a',first);first.end('€');
  await new Promise(resolve=>first.once('close',resolve));
  const second=new PassThrough();budget.attach('a',second);second.write(Buffer.alloc(1048573,0x61));let settled=false;
  const pending=budget.finish().then(value=>{settled=true;return value;});await new Promise(resolve=>setImmediate(resolve));
  try {assert.equal(settled,false);}finally{second.end();}
  const result=await pending;assert.deepEqual(result,{bytes:1048576,failed:false});
  assert.equal(fs.statSync(path.join(root,'a.log')).size,1048576);assert.equal(fs.readFileSync(path.join(root,'a.log')).subarray(0,3).toString(),'€');
});

test('diagnostic budget reports write and stream failures while draining',async t=>{
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'diagnostic-budget-synthetic-'));t.after(()=>fs.rmSync(root,{recursive:true,force:true}));
  const budget=createDiagnosticBudget(root),a=new PassThrough(),b=new PassThrough();budget.attach('a',a);budget.attach('b',b);
  const write=fs.writeSync;fs.writeSync=()=>{throw Error('synthetic write failure');};
  try {a.write('synthetic');}finally{fs.writeSync=write;}
  b.emit('error',Error('synthetic stream failure'));a.end('drained');b.end('drained');
  const result=await budget.finish();assert.equal(result.failed,true);assert.ok(result.bytes<=1048576);
});

test('diagnostic budget tolerates close failure after rejecting an opened log',async t=>{
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'diagnostic-budget-synthetic-'));t.after(()=>fs.rmSync(root,{recursive:true,force:true}));
  const file=path.join(root,'unsafe.log');fs.writeFileSync(file,'synthetic');fs.chmodSync(file,0o644);
  const budget=createDiagnosticBudget(root),stream=new PassThrough(),close=fs.closeSync;
  fs.closeSync=fd=>{close(fd);throw Error('synthetic close failure');};
  try {budget.attach('unsafe',stream);}finally{fs.closeSync=close;}
  stream.end('discard');const result=await budget.finish();assert.equal(result.failed,true);assert.equal(fs.readFileSync(file,'utf8'),'synthetic');
});

test('diagnostic budget refuses symlinks and reports path replacement after attachment',async t=>{
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'diagnostic-budget-synthetic-'));t.after(()=>fs.rmSync(root,{recursive:true,force:true}));
  const neighbor=path.join(root,'neighbor');fs.writeFileSync(neighbor,'untouched');fs.symlinkSync(neighbor,path.join(root,'bad.log'));
  const budget=createDiagnosticBudget(root),a=new PassThrough(),b=new PassThrough();budget.attach('bad',a);budget.attach('good',b);
  fs.renameSync(path.join(root,'good.log'),path.join(root,'old'));fs.writeFileSync(path.join(root,'good.log'),'replacement');
  a.end('discard');b.end('synthetic');const result=await budget.finish();assert.equal(result.failed,true);
  assert.equal(fs.readFileSync(neighbor,'utf8'),'untouched');
});

test('diagnostic path replacement alone is a capture failure',async t=>{
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'diagnostic-budget-synthetic-'));t.after(()=>fs.rmSync(root,{recursive:true,force:true}));
  const budget=createDiagnosticBudget(root),stream=new PassThrough();budget.attach('good',stream);
  fs.renameSync(path.join(root,'good.log'),path.join(root,'old'));fs.writeFileSync(path.join(root,'good.log'),'replacement');
  stream.end('synthetic');const result=await budget.finish();assert.equal(result.failed,true);
});

for(const kind of ['invalid-root','changed-root','invalid-name','wrong-mode','existing-over-cap','zero-write','close-error']) test(`diagnostic budget fails safely on ${kind}`,async t=>{
  const base=fs.mkdtempSync(path.join(os.tmpdir(),'diagnostic-errors-synthetic-'));t.after(()=>fs.rmSync(base,{recursive:true,force:true}));
  const root=path.join(base,'runtime');fs.mkdirSync(root,{mode:0o700});
  if(kind==='invalid-root')fs.chmodSync(root,0o755);
  if(kind==='wrong-mode'){fs.writeFileSync(path.join(root,'synthetic.log'),'x');fs.chmodSync(path.join(root,'synthetic.log'),0o644);}
  if(kind==='existing-over-cap'){const fd=fs.openSync(path.join(root,'synthetic.log'),'w',0o600);fs.ftruncateSync(fd,1048577);fs.closeSync(fd);}
  const budget=createDiagnosticBudget(root),stream=new PassThrough();
  if(kind==='changed-root'){fs.renameSync(root,root+'-old');fs.mkdirSync(root,{mode:0o700});}
  budget.attach(kind==='invalid-name'?'../unsafe':'synthetic',stream);
  const write=fs.writeSync,close=fs.closeSync;
  if(kind==='zero-write')fs.writeSync=()=>0;
  try {stream.write('synthetic');}finally{fs.writeSync=write;}
  if(kind==='close-error')fs.closeSync=fd=>{close(fd);throw Error('synthetic close failure');};
  try {stream.end();const result=await budget.finish();assert.equal(result.failed,true);}
  finally {fs.closeSync=close;}
});

test('active synthetic child cannot grow runtime diagnostic logs beyond the aggregate bound', {timeout:10000},async t=>{
  const {spawn}=await import('node:child_process');
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'diagnostic-flood-synthetic-'));t.after(()=>fs.rmSync(root,{recursive:true,force:true}));
  const budget=createDiagnosticBudget(root);
  const child=spawn(process.execPath,['-e',"require('node:fs').writeSync(2,Buffer.alloc(1048577,120));process.stdout.write('ready');process.stdin.resume();process.stdin.on('end',()=>process.exit(0));"],{stdio:['pipe','pipe','pipe']});
  budget.attach('synthetic',child.stderr);
  const closed=new Promise((resolve,reject)=>{child.once('error',reject);child.once('close',(code,signal)=>resolve({code,signal}));});
  const ready=new Promise((resolve,reject)=>{child.stdout.once('data',resolve);child.once('error',reject);child.once('close',()=>reject(Error('synthetic writer closed before ready')));});
  try {
    await ready;
    assert.equal(child.exitCode,null,'writer remains active');
    const bytes=fs.statSync(path.join(root,'synthetic.log')).size;
    assert.ok(bytes<=1024*1024,`active diagnostic bytes ${bytes} exceed aggregate bound`);
  } finally {child.stdin.end();assert.deepEqual(await closed,{code:0,signal:null});}
  assert.deepEqual(await budget.finish(),{bytes:1048576,failed:true});
});
