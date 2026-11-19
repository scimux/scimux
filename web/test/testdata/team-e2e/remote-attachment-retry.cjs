'use strict';
const fs=require('node:fs'),path=require('node:path'),http=require('node:http'),assert=require('node:assert/strict');
const {chromium,devices}=require(process.env.SCIMUX_E2E_PLAYWRIGHT);
(async()=>{
 const source=process.argv[2],out=process.argv[3];fs.mkdirSync(out,{recursive:true,mode:0o700});
 const key=['a'.repeat(32),'b'.repeat(32),'c'.repeat(16),'d'.repeat(16)].join('/'),root='/api/team/subscriptions',base=root+'/'+key,id='0123456789abcdef',copyInstance='f'.repeat(32);let fileGets=0,dispatches=0;
 const [seat,owner,chat,generation]=key.split('/');const caps={people_attachments:{version:1,max_file_bytes:4194304,max_files:16,max_total_bytes:16777216},servers:[{id:seat,people_files:'supported'}],subscriptions:[{server:seat,owner,chat,generation,people_copy_instance:copyInstance}]};
 const entry={seq:1,anchor:0,author:'b'.repeat(32),at:'2026-01-01T00:00:00Z',text:'',attachments:[{id,sha256:'e'.repeat(64),size:1,name:'synthetic.txt',mime:'text/plain'}],_peopleRetained:{[id]:false}};
 const server=http.createServer((req,res)=>{const url=new URL(req.url,'http://fixture'),name=url.pathname;if(req.method!=='GET'){dispatches++;res.writeHead(403);res.end();return}
  if(name==='/'){res.setHeader('Content-Type','text/html');res.end(`<!doctype html><section id="chat"><div id="msgs"></div></section><script type="module">
import {createRemoteAssetTransport} from '/js/bootstrap.js';import {createChatFeature} from '/js/chat.js';import {guestMessageHTML} from '/js/team-guest.js';
const root=${JSON.stringify(root)},base=${JSON.stringify(base)},key=${JSON.stringify(key)},entry=${JSON.stringify(entry)},copyInstance=${JSON.stringify(copyInstance)};
const transport=createRemoteAssetTransport({channel:fetch,assets:new Map(),createObjectURL:blob=>URL.createObjectURL(blob)});
await (await transport.fetchImpl(root)).json();await (await transport.fetchImpl(base+'/comments?copy_instance='+copyInstance)).json();
const msgs=document.querySelector('#msgs');const chat=createChatFeature({document,window,roots:{msgs},assetURL:transport.assetURL,retryAsset:transport.retryAsset,sel:()=> 'team/'+key,selGen:()=>1});chat.bind();msgs.innerHTML=guestMessageHTML(entry,{},'a'.repeat(32),{},'','',key,transport.assetURL,copyInstance);window.ready=true;</script>`);return}
  if(/^\/js\/[a-z0-9-]+\.js$/.test(name)){res.setHeader('Content-Type','text/javascript');res.end(fs.readFileSync(path.join(source,'web',name)));return}
  if(name===root){res.setHeader('Content-Type','application/json');res.end(JSON.stringify(caps));return}
  if(name===base+'/comments'){res.setHeader('Content-Type','application/json');res.end(JSON.stringify({entries:[entry]}));return}
  if(name===base+'/attachments/1/'+id&&url.searchParams.get('copy_instance')===copyInstance){fileGets++;if(fileGets%2===1){res.writeHead(503);res.end();return}res.writeHead(200,{'Content-Type':'application/octet-stream'});res.end('x');return}
  res.writeHead(404);res.end();
 });
 await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));let browser;try{browser=await chromium.launch();for(const mode of ['desktop','phone']){const context=await browser.newContext(mode==='phone'?devices['iPhone 13']:{viewport:{width:1280,height:900}});try{const page=await context.newPage();await page.goto('http://127.0.0.1:'+server.address().port,{waitUntil:'networkidle'});await page.waitForFunction(()=>window.ready===true);const retry=page.getByRole('button',{name:'Retry download'});await retry.waitFor();const downloaded=page.waitForEvent('download');if(mode==='phone'){await retry.focus();await retry.press('Enter')}else await retry.click();const download=await downloaded;assert.equal(fs.readFileSync(await download.path(),'utf8'),'x');assert.equal(dispatches,0);await page.screenshot({path:path.join(out,'remote-retry-'+mode+'.png'),fullPage:true});}finally{await context.close()}}assert.equal(fileGets,4);console.log('PASS desktop and phone: real retry interaction through the scoped remote asset adapter, exact bytes, zero controls');}finally{if(browser)await browser.close();await new Promise(resolve=>server.close(resolve));}
})().catch(err=>{console.error(err);process.exitCode=1});
