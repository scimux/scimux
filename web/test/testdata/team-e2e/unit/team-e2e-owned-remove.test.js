import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {removeOwned} from '../owned-remove.cjs';

const identity=st=>({uid:st.uid,dev:st.dev,ino:st.ino});
async function fixture(t) {
  const parent=await fs.mkdtemp(path.join(os.tmpdir(),'owned-synthetic-'));
  t.after(()=>fs.rm(parent,{recursive:true,force:true}));
  const root=path.join(parent,'run'),neighbor=path.join(parent,'neighbor');
  await fs.mkdir(root,{mode:0o700});await fs.mkdir(neighbor,{mode:0o700});
  await fs.writeFile(path.join(neighbor,'marker'),'synthetic');
  await fs.mkdir(path.join(root,'home'),{mode:0o700});await fs.writeFile(path.join(root,'home','sample'),'synthetic');
  const parentIdentity=identity(await fs.lstat(parent)),rootIdentity=identity(await fs.lstat(root)),homeIdentity=identity(await fs.lstat(path.join(root,'home')));
  return {parent,root,neighbor,args:{parent,name:'run',parentIdentity,rootIdentity,registered:{home:homeIdentity}}};
}

test('registered runtime removal preserves unrelated neighbor',async t=>{
  const f=await fixture(t);assert.deepEqual(await removeOwned(f.args),{removed:true});
  assert.equal(await fs.stat(f.root).then(()=>true,()=>false),false);
  assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

test('unregistered root entry refuses removal without modifying either tree',async t=>{
  const f=await fixture(t);await fs.writeFile(path.join(f.root,'unknown'),'synthetic');
  assert.deepEqual(await removeOwned(f.args),{removed:false});
  assert.equal(await fs.readFile(path.join(f.root,'unknown'),'utf8'),'synthetic');
  assert.equal(await fs.readFile(path.join(f.root,'home','sample'),'utf8'),'synthetic');
  assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

test('runtime removal rejects invalid requests without touching a neighbor',async t=>{
  const f=await fixture(t);
  for(const args of [{...f.args,parent:'relative'},{...f.args,name:'../neighbor'},{...f.args,registered:null}])assert.deepEqual(await removeOwned(args),{removed:false});
  assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

for(const input of ['invalid-json','overflow','wrong-root']) test(`actual cleanup child refuses ${input}`,{skip:process.env.SCIMUX_E2E_SOCKET_TEST!=='1',timeout:10000},async t=>{
  const {spawn}=await import('node:child_process');const {fileURLToPath}=await import('node:url');const f=await fixture(t);
  const script=fileURLToPath(new URL('../owned-remove.cjs',import.meta.url));
  const child=spawn(process.execPath,[script,'--child'],{cwd:f.root,stdio:['pipe','pipe','pipe']});let output='',error='';
  child.stdout.on('data',x=>{output+=x;});child.stderr.on('data',x=>{error+=x;});child.stdin.on('error',()=>{});
  const closed=new Promise((resolve,reject)=>{child.once('error',reject);child.once('close',(code,signal)=>resolve({code,signal}));});
  const data=input==='invalid-json'?'{':input==='overflow'?' '.repeat(1048577):JSON.stringify({rootIdentity:{uid:0,dev:0,ino:0}});
  try {child.stdin.end(data);assert.deepEqual(await closed,{code:0,signal:null});assert.equal(output,'refused\n');assert.equal(error,'');}
  finally {if(child.exitCode===null&&child.signalCode===null){child.kill('SIGKILL');await closed;}}
  assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');assert.equal(await fs.readFile(path.join(f.root,'home','sample'),'utf8'),'synthetic');
});

for(const mode of ['duplicate-event','timeout']) test(`socket inspection preserves an owned socket on ${mode}`,{skip:process.env.SCIMUX_E2E_SOCKET_TEST!=='1',timeout:5000},async t=>{
  const net=(await import('node:net')).default;const {EventEmitter}=await import('node:events');const f=await fixture(t);
  const socket=path.join(f.root,'home','active.sock'),server=net.createServer();
  await new Promise((resolve,reject)=>server.once('error',reject).listen(socket,resolve));
  const original=net.createConnection;net.createConnection=()=>{
    const fake=new EventEmitter();fake.destroy=()=>{};
    if(mode==='duplicate-event')queueMicrotask(()=>{fake.emit('connect');const error=Error('synthetic');error.code='ECONNREFUSED';fake.emit('error',error);});
    return fake;
  };
  try {assert.deepEqual(await removeOwned(f.args),{removed:false});assert.equal((await fs.lstat(socket)).isSocket(),true);}
  finally {net.createConnection=original;await new Promise(resolve=>server.close(resolve));}
});

test('a replaced registered root is not adopted or removed',async t=>{
  const f=await fixture(t);await fs.rename(f.root,path.join(f.parent,'old'));
  await fs.mkdir(f.root,{mode:0o700});await fs.writeFile(path.join(f.root,'replacement'),'synthetic');
  assert.deepEqual(await removeOwned(f.args),{removed:false});
  assert.equal(await fs.readFile(path.join(f.root,'replacement'),'utf8'),'synthetic');
  assert.equal(await fs.readFile(path.join(f.parent,'old','home','sample'),'utf8'),'synthetic');
});

test('a symlink replacing a registered entry is not traversed or removed',async t=>{
  const f=await fixture(t);await fs.rename(path.join(f.root,'home'),path.join(f.parent,'old-home'));
  await fs.symlink(f.neighbor,path.join(f.root,'home'));
  assert.deepEqual(await removeOwned(f.args),{removed:false});
  assert.equal((await fs.lstat(path.join(f.root,'home'))).isSymbolicLink(),true);
  assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

test('registered runtime can contain synthetic browser filenames with spaces',async t=>{
  const f=await fixture(t);await fs.writeFile(path.join(f.root,'home','Local State'),'synthetic');
  assert.deepEqual(await removeOwned(f.args),{removed:true});
  assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

test('registered runtime removal handles hidden state and ordinary filenames',async t=>{
  const f=await fixture(t);const dir=path.join(f.root,'home','.scimux');
  await fs.mkdir(dir,{mode:0o700});await fs.writeFile(path.join(dir,'.storage.lock'),'synthetic');
  await fs.writeFile(path.join(f.root,'home','synthetic file.txt'),'synthetic');
  assert.deepEqual(await removeOwned(f.args),{removed:true});
  assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

test('registered runtime removal handles a closed owned Unix socket',{skip:process.env.SCIMUX_E2E_SOCKET_TEST!=='1',timeout:5000},async t=>{
  const net=await import('node:net');const f=await fixture(t);
  const source=path.join(f.root,'home','source.sock'),dead=path.join(f.root,'home','worker.sock');
  const server=net.createServer();
  await new Promise((resolve,reject)=>server.once('error',reject).listen(source,resolve));
  await fs.rename(source,dead);
  await new Promise((resolve,reject)=>server.close(error=>error?reject(error):resolve()));
  assert.equal((await fs.lstat(dead)).isSocket(),true);
  assert.deepEqual(await removeOwned(f.args),{removed:true});
  assert.equal(await fs.readFile(path.join(f.neighbor,'marker'),'utf8'),'synthetic');
});

test('a live Unix socket in a registered runtime is never unlinked',{skip:process.env.SCIMUX_E2E_SOCKET_TEST!=='1',timeout:5000},async t=>{
  const net=await import('node:net');const f=await fixture(t);const socket=path.join(f.root,'home','live.sock');
  const server=net.createServer();
  await new Promise((resolve,reject)=>server.once('error',reject).listen(socket,resolve));
  try {assert.deepEqual(await removeOwned(f.args),{removed:false});assert.equal((await fs.lstat(socket)).isSocket(),true);}
  finally {await new Promise(resolve=>server.close(resolve));}
});

test('a renamed live Unix socket is preserved',{skip:process.env.SCIMUX_E2E_SOCKET_TEST!=='1',timeout:5000},async t=>{
  const net=await import('node:net');const f=await fixture(t);
  const original=path.join(f.root,'home','source.sock'),moved=path.join(f.root,'home','moved.sock');
  const server=net.createServer(socket=>socket.destroy());
  await new Promise((resolve,reject)=>server.once('error',reject).listen(original,resolve));await fs.rename(original,moved);
  try {assert.deepEqual(await removeOwned(f.args),{removed:false});assert.equal((await fs.lstat(moved)).isSocket(),true);}
  finally {await new Promise(resolve=>server.close(resolve));}
});
