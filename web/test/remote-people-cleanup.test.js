import test from 'node:test';
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {mkdtempSync,writeFileSync,readFileSync,rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {fileURLToPath} from 'node:url';
const runner=fileURLToPath(new URL('./testdata/team-e2e/remote-people-tunnel.cjs',import.meta.url));
for(const [signal,code] of [['SIGINT',130],['SIGTERM',143]]) test(`remote browser fixture closes its owned browser on ${signal}`,{timeout:10000},async t=>{
 const dir=mkdtempSync(join(tmpdir(),'scimux-remote-cleanup-'));t.after(()=>rmSync(dir,{recursive:true,force:true}));
 const fake=join(dir,'playwright.cjs'),marker=join(dir,'closed');
 writeFileSync(fake,`const fs=require('node:fs');let timer;module.exports={devices:{},chromium:{launch:async()=>({newContext:async()=>{timer=setInterval(()=>{},1000);console.log('synthetic browser ready');return new Promise(()=>{})},close:async()=>{clearInterval(timer);fs.writeFileSync(process.env.FAKE_CLOSE_MARKER,'closed')}})}}`);
 const child=spawn(process.execPath,[runner],{env:{...process.env,SCIMUX_E2E_PLAYWRIGHT:fake,SCIMUX_TUNNEL_MODE:'desktop',SCIMUX_TUNNEL_URL:'http://127.0.0.1',SCIMUX_TUNNEL_ARTIFACTS:dir,FAKE_CLOSE_MARKER:marker},stdio:['ignore','pipe','pipe']});
 t.after(()=>{if(child.exitCode===null&&child.signalCode===null)child.kill('SIGKILL')});
 await new Promise((resolve,reject)=>{child.stdout.on('data',b=>{if(String(b).includes('synthetic browser ready'))resolve()});child.once('error',reject);child.once('exit',(n,s)=>reject(Error(`exited early ${n}/${s}`)))});
 child.kill(signal);const [status,sig]=await new Promise(resolve=>child.once('close',(n,s)=>resolve([n,s])));
 assert.equal(sig,null);assert.equal(status,code);assert.equal(readFileSync(marker,'utf8'),'closed');
});
