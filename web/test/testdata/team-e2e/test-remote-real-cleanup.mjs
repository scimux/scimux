import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import {spawn,execFileSync} from 'node:child_process';
import {mkdtempSync,readFileSync,rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {fileURLToPath} from 'node:url';
const runner=fileURLToPath(new URL('./remote-people-tunnel.cjs',import.meta.url));
const enabled=process.env.SCIMUX_REMOTE_CLEANUP_REAL==='1' && !!process.env.SCIMUX_E2E_PLAYWRIGHT;
if(enabled && process.platform!=='linux') throw Error('real remote cleanup process inspection requires Linux /proc');
function descendants(pid){const lines=execFileSync('ps',['-eo','pid=,ppid='],{encoding:'utf8'}).trim().split('\n').map(line=>line.trim().split(/\s+/).map(Number));const visit=id=>lines.filter(([,parent])=>parent===id).flatMap(([child])=>[child,...visit(child)]);return visit(pid)}
function running(pid){try {return readFileSync(`/proc/${pid}/stat`,'utf8').split(' ')[2]!=='Z'}catch{return false}}
for(const [mode,signal,exit] of [['desktop','SIGINT',130],['phone','SIGTERM',143]]) test(`real remote browser ${mode} cleans up on ${signal}`,{skip:!enabled,timeout:30000},async t=>{
 let ready;const held=new Promise(resolve=>ready=resolve);const server=http.createServer((req,res)=>{if(req.url==='/status'){ready();res.end('waiting');return}res.setHeader('Content-Type','text/html');res.end('<!doctype html><button id="load">Load</button><span id="ready">ready</span>')});
 await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));t.after(()=>new Promise(resolve=>server.close(resolve)));
 const dir=mkdtempSync(join(tmpdir(),'scimux-remote-real-cleanup-'));t.after(()=>rmSync(dir,{recursive:true,force:true}));
 const child=spawn(process.execPath,[runner],{env:{...process.env,SCIMUX_TUNNEL_MODE:mode,SCIMUX_TUNNEL_URL:`http://127.0.0.1:${server.address().port}`,SCIMUX_TUNNEL_ARTIFACTS:dir},stdio:['ignore','pipe','pipe']});
 t.after(async()=>{if(child.exitCode===null&&child.signalCode===null){child.kill('SIGTERM');await new Promise(resolve=>child.once('close',resolve))}});
 await Promise.race([held,new Promise((_,reject)=>child.once('exit',(n,s)=>reject(Error(`browser exited early ${n}/${s}`)) ))]);
 const owned=descendants(child.pid);assert.ok(owned.length>0,'fixture launched an owned browser process');child.kill(signal);
 const [status,sig]=await new Promise(resolve=>child.once('close',(n,s)=>resolve([n,s])));assert.equal(sig,null);assert.equal(status,exit);
 for(let i=0;i<40&&owned.some(running);i++)await new Promise(resolve=>setTimeout(resolve,50));
 assert.deepEqual(owned.filter(running),[],'browser descendants stopped');
});

test('real remote browser timeout closes owned browser processes',{skip:!enabled,timeout:30000},async t=>{
 let ready;const held=new Promise(resolve=>ready=resolve);const server=http.createServer((req,res)=>{if(req.url==='/status'){ready();res.end('held');return}res.setHeader('Content-Type','text/html');res.end('<!doctype html><span id="ready">ready</span><button id="load">Load</button><button id="poll">Poll</button><span id="polled">polled</span><button id="release">Release</button>')});
 await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));t.after(()=>new Promise(resolve=>server.close(resolve)));
 const dir=mkdtempSync(join(tmpdir(),'scimux-remote-timeout-'));t.after(()=>rmSync(dir,{recursive:true,force:true}));
 const child=spawn(process.execPath,[runner],{env:{...process.env,SCIMUX_TUNNEL_MODE:'desktop',SCIMUX_TUNNEL_URL:`http://127.0.0.1:${server.address().port}`,SCIMUX_TUNNEL_ARTIFACTS:dir},stdio:['ignore','pipe','pipe']});let errors='';child.stderr.on('data',b=>{errors+=b});
 t.after(async()=>{if(child.exitCode===null&&child.signalCode===null){child.kill('SIGTERM');await new Promise(resolve=>child.once('close',resolve))}});
 await Promise.race([held,new Promise((_,reject)=>child.once('exit',(n,s)=>reject(Error(`browser exited early ${n}/${s}`)) ))]);
 const owned=descendants(child.pid);assert.ok(owned.length>0);const [status,sig]=await new Promise(resolve=>child.once('close',(n,s)=>resolve([n,s])));
 assert.equal(sig,null);assert.equal(status,1);assert.match(errors,/Timeout/);for(let i=0;i<40&&owned.some(running);i++)await new Promise(resolve=>setTimeout(resolve,50));assert.deepEqual(owned.filter(running),[]);
});
