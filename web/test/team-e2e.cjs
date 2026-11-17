#!/usr/bin/env node
// Opt-in synthetic real-browser acceptance. No test dependency ships with scimux.
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const net = require('node:net');
const assert = require('node:assert/strict');
const { spawn, execFileSync } = require('node:child_process');
const { retire } = require('./testdata/team-e2e/tmux-cleanup.cjs');

function prerequisite(name) {
  const value = process.env[name];
  if (!value || !path.isAbsolute(value)) throw Error(`${name} must name an absolute path`);
  return fs.realpathSync(value);
}
let scimux, relayBin, playwright;
try {
  scimux = prerequisite('SCIMUX_E2E_BINARY'); relayBin = prerequisite('SCIMUX_TEAM_E2E_BINARY');
  if (scimux === relayBin) throw Error('scimux and relay must be distinct binaries');
  for (const p of [scimux, relayBin]) fs.accessSync(p, fs.constants.X_OK);
  playwright = require(prerequisite('SCIMUX_E2E_PLAYWRIGHT'));
  if (!playwright.chromium || !playwright.devices) throw Error('Playwright Chromium is required');
  execFileSync('tmux', ['-V'], {stdio:'ignore'});
} catch (e) { console.error('E2E prerequisite error:', e.message); process.exit(2); }

const fixtureDir = path.join(__dirname, 'testdata', 'team-e2e');
const root = fs.mkdtempSync(path.join(os.tmpdir(), 'scimux-team-e2e-'));
fs.chmodSync(root, 0o700);
const manifest = {version:1, scope:'implemented synthetic browser acceptance scenarios', assertions:[], artifacts:[], counters:{}, cleanup:[]};
const seats = [], children = [], pages = [], contexts = [];
const pending = new Set();
const heldReleases = new Set();
let browser, relay, shutting = false, signalCode = 0;
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
const assertID = (id, condition) => { assert.ok(condition, id); manifest.assertions.push({id, result:'pass'}); };
const record = () => fs.writeFileSync(path.join(root, 'manifest.json'), JSON.stringify(manifest, null, 2));
const artifacts = name => path.join(root, name);
const jsonLines = p => fs.existsSync(p) ? fs.readFileSync(p, 'utf8').trim().split('\n').filter(Boolean).map(JSON.parse) : [];
const logFile = name => fs.openSync(artifacts(name + '.log'), 'a', 0o600);
const fixtureBin = artifacts('bin'); fs.mkdirSync(fixtureBin, {mode:0o700});
const names = ['claude','codex','pi','pi-acp','opencode','grok','cursor-agent','dsh','muse','vibe-acp'];
for (const name of names) {
  const dest = path.join(fixtureBin,name);
  if (name === 'claude' || name === 'codex') fs.copyFileSync(path.join(fixtureDir,name),dest);
  else fs.writeFileSync(dest, '#!/bin/sh\nexit 127\n');
  fs.chmodSync(dest,0o755);
}
const testPath = fixtureBin + ':/usr/bin:/bin';
for (const name of names) {
  const resolved = execFileSync('/bin/sh',['-c','command -v "$1"','fixture',name],{env:{PATH:testPath},encoding:'utf8'}).trim();
  if (resolved !== path.join(fixtureBin,name)) throw Error('unsafe fixture resolution: ' + name);
}
const baseEnv = {...process.env, PATH:testPath, HOME:root, SCIMUX_ENABLE_EXPERIMENTAL_FEATURE_TEAM:'0'};
delete baseEnv.TMUX;
delete baseEnv.TMUX_PANE;
function start(binary,args,env,label,stdout='ignore') {
  if(shutting)throw Error('runner shutting down');
  const log = logFile(label);
  const c = spawn(binary,args,{env,stdio:['ignore',stdout,log]});
  fs.closeSync(log); children.push(c); return c;
}
async function until(fn, ms, label) {
  const end = Date.now() + ms;
  while (Date.now() < end) { if(shutting)throw Error('runner shutting down'); const v = await fn(); if (v) return v; await delay(100); }
  throw Error('timeout: ' + label);
}
async function freePort() {
  const server=net.createServer(); await new Promise((resolve,reject)=>server.once('error',reject).listen(0,'127.0.0.1',resolve));
  const port=server.address().port; await new Promise(resolve=>server.close(resolve)); return port;
}
async function launchRelay() {
  const dir=artifacts('relay'); fs.mkdirSync(dir,{mode:0o700});
  const c=start(relayBin,['-state-dir',dir,'-addr','127.0.0.1:0','-name','Synthetic relay'],baseEnv,'relay','pipe');
  let out=''; c.stdout.on('data',x=>{out+=x;});
  const url=await until(()=>{if(c.exitCode!==null)throw Error('relay exited');return out.match(/scimux-team listening on (http:\/\/127\.0\.0\.1:\d+)/)?.[1];},20000,'relay startup');
  return {dir,url,child:c};
}
async function restartRelay() {
  const c=start(relayBin,['-state-dir',relay.dir,'-addr',new URL(relay.url).host,'-name','Synthetic relay'],baseEnv,'relay-restart','pipe');
  let out='';c.stdout.on('data',x=>{out+=x;});relay.child=c;
  await until(()=>{if(c.exitCode!==null)throw Error('restarted relay exited');return out.includes('scimux-team listening on '+relay.url);},20000,'relay restart');
}
async function makeSeat(name) {
  const data=artifacts(name); fs.mkdirSync(data,{mode:0o700});
  const tmuxDir=path.join(data,'tmux');fs.mkdirSync(tmuxDir,{mode:0o700});
  const socket='e2e-'+name+'-'+process.pid;
  const env={...baseEnv, HOME:data, TMUX_TMPDIR:tmuxDir, SCIMUX_SYNTHETIC_COUNTER:path.join(data,'counter')};
  const s={name,data,env,socket,socketPath:path.join(tmuxDir,'tmux-'+process.getuid(),socket), nodes:[]}; seats.push(s);
  const port=await freePort(); s.base='http://127.0.0.1:'+port;
  s.child=start(scimux,['-data',data,'-addr','127.0.0.1:'+port,'-socket',socket],env,name);
  const html=await until(async()=>{if(s.child.exitCode!==null)throw Error(name+' exited');try{const r=await fetch(s.base);return r.ok?await r.text():null;}catch{return null;}},20000,name+' startup');
  const match=html.match(/name="scimux-csrf" content="([^"]+)"/);assert.ok(match,'csrf meta');const token=match[1];
  s.api=(route,method='GET',body,cleanupRequest=false)=>{
    if(shutting&&!cleanupRequest)return Promise.reject(Error('runner shutting down'));
    const request=(async()=>{const r=await fetch(s.base+route,{method,headers:{'Content-Type':'application/json','X-Scimux-CSRF':token,Origin:s.base},body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(10000)});const text=await r.text();if(!r.ok)throw Error(`${name} ${method} ${route}: HTTP ${r.status}`);return text?JSON.parse(text):null;})();
    pending.add(request);request.then(()=>pending.delete(request),()=>pending.delete(request));return request;
  };
  s.status=(route,body)=>{if(shutting)return Promise.reject(Error('runner shutting down'));const request=(async()=>{const r=await fetch(s.base+route,{method:'POST',headers:{'Content-Type':'application/json','X-Scimux-CSRF':token,Origin:s.base},body:JSON.stringify(body),signal:AbortSignal.timeout(10000)});return r.status;})();pending.add(request);request.then(()=>pending.delete(request),()=>pending.delete(request));return request;};
  s.uiSetup=async value=>{const r=await fetch(s.base+'/api/ui',{method:'PUT',headers:{'Content-Type':'application/json','X-Scimux-CSRF':token,Origin:s.base,'If-Match':'*'},body:JSON.stringify(value)});if(!r.ok)throw Error('synthetic UI setup failed: '+r.status);};
  return s;
}
async function node(s,agent,title,prompt) {
  const n=await s.api('/api/nodes','POST',{agent,title,prompt,dir:s.data});s.nodes.push(n.id);
  await until(async()=>{const c=await s.api('/api/nodes/'+encodeURIComponent(n.id)+'/chat');return c.turns?.some(t=>t.role==='assistant');},30000,'initial '+agent+' response');return n;
}
function counts(s) { const p=path.join(s.data,'counter');return {attempted:jsonLines(p+'.attempts').length,accepted:jsonLines(p).length,controls:jsonLines(p+'.controls').length}; }
async function pageFor(s,mode,label) {
  if(shutting)throw Error('runner shutting down');
  const width={w320:320,w390:390,w768:768}[mode];
  const context=await browser.newContext(mode==='phone'?playwright.devices['iPhone 13']:{viewport:{width:width||1280,height:900}});contexts.push(context);
  const page=await context.newPage();pages.push({page,label});
  await page.coverage.startJSCoverage({resetOnNavigation:false});
  await page.goto(s.base,{waitUntil:'networkidle'});return page;
}
async function screenshot(p,name) { await p.screenshot({path:artifacts(name),fullPage:true});manifest.artifacts.push(name); }
async function open(p,id) {await p.locator('[data-open="'+id+'"]').click({position:{x:20,y:12}});}
async function staleMembership(mode,a,b,r,observer) {
  const sharePath='/api/team/shares/'+b.node.id;
  await b.api(sharePath,'PUT',{server:b.serverID,scope:'all',members:[{seat:r.self,access:'write'},{seat:observer.self,access:'write'}]});
  const before=Object.fromEntries(seats.map(s=>[s.name,counts(s)]));
  const owner=await pageFor(b,mode,'stale-membership-'+mode);await open(owner,b.node.id);await owner.locator('#sharebtn').click();
  const existing=owner.locator('#sh_body input[data-seat="'+r.self+'"]'),invite=owner.locator('#sh_body input[data-seat="'+a.self+'"]');
  await existing.waitFor();assert.equal(await existing.isChecked(),true);assert.equal(await invite.isChecked(),false);
  let release;const held=new Promise(resolve=>release=resolve);heldReleases.add(release);let sent;
  await owner.route('**'+sharePath,async route=>{
    if(route.request().method()==='GET'){const response=await route.fetch();await held;if(!owner.isClosed())await route.fulfill({response});}
    else {if(route.request().method()==='PUT')sent=route.request().postDataJSON();await route.continue();}
  });
  try {
    await b.api(sharePath,'PUT',{server:b.serverID,scope:'all',members:[{seat:r.self,access:'read'},{seat:observer.self,access:'write'}]});
    assertID('stale-read-before-save-'+mode,(await b.api(sharePath)).members.some(x=>x.seat===r.self&&x.access==='read'));
    await invite.check();await owner.locator('#sh_apply').click();
    const saved=await until(async()=>{const share=await b.api(sharePath);return share.members.some(x=>x.seat===a.self)?share:null;},20000,'stale membership save');
    assertID('stale-keeps-current-read-'+mode,saved.members.some(x=>x.seat===r.self&&x.access==='read'));
    assertID('stale-new-write-and-unrelated-'+mode,saved.members.some(x=>x.seat===a.self&&x.access==='write')&&saved.members.some(x=>x.seat===observer.self&&x.access==='write'));
    const sub=await until(async()=>{const list=(await r.api('/api/team/subscriptions')).subscriptions;return list.find(x=>x.chat===saved.uid&&x.owner===b.self&&x.access==='read');},30000,'stale read subscription');
    const id=[sub.server,sub.owner,sub.chat,sub.generation].join('/');
    assertID('stale-read-comment-403-'+mode,await r.status('/api/team/subscriptions/'+id+'/comments',{text:'Synthetic denied',anchor:0,key:mode==='desktop'?'abcdefff23456781':'abcdefff23456782'})===403);
    const after=Object.fromEntries(seats.map(s=>[s.name,counts(s)]));assert.deepEqual(after,before);
    manifest['stale-membership-'+mode]={submitted:sent?.membership_only,grants:saved.members.map(x=>({seat:x.seat,access:x.access})),before,after};
    await screenshot(owner,'stale-membership-'+mode+'.png');
  } finally {release();heldReleases.delete(release);await owner.unroute('**'+sharePath);}
  await b.api(sharePath,'PUT',{server:b.serverID,scope:'all',members:[{seat:r.self,access:'write'},{seat:observer.self,access:'write'}]});
}
async function staleRemovedMembership(mode,a,b,r,observer) {
  const sharePath='/api/team/shares/'+b.node.id;
  await b.api(sharePath,'PUT',{server:b.serverID,scope:'all',members:[{seat:r.self,access:'read'},{seat:observer.self,access:'write'}]});
  const before=Object.fromEntries(seats.map(s=>[s.name,counts(s)]));
  const owner=await pageFor(b,mode,'stale-removed-membership-'+mode);await open(owner,b.node.id);await owner.locator('#sharebtn').click();
  const existing=owner.locator('#sh_body input[data-seat="'+r.self+'"]'),invite=owner.locator('#sh_body input[data-seat="'+a.self+'"]');
  await existing.waitFor();assert.equal(await existing.isChecked(),true);assert.equal(await invite.isChecked(),false);
  let release;const held=new Promise(resolve=>release=resolve);heldReleases.add(release);let sent;
  await owner.route('**'+sharePath,async route=>{
    if(route.request().method()==='GET'){const response=await route.fetch();await held;if(!owner.isClosed())await route.fulfill({response});}
    else {if(route.request().method()==='PUT')sent=route.request().postDataJSON();await route.continue();}
  });
  try {
    await b.api(sharePath,'PUT',{server:b.serverID,scope:'all',members:[{seat:observer.self,access:'write'}]});
    assertID('stale-removal-before-save-'+mode,!(await b.api(sharePath)).members.some(x=>x.seat===r.self));
    await invite.check();await owner.locator('#sh_apply').click();
    const saved=await until(async()=>{const share=await b.api(sharePath);return share.members.some(x=>x.seat===a.self)?share:null;},20000,'stale removed membership save');
    assertID('stale-removed-stays-absent-'+mode,!saved.members.some(x=>x.seat===r.self));
    assertID('stale-removed-new-write-and-unrelated-'+mode,saved.members.some(x=>x.seat===a.self&&x.access==='write')&&saved.members.some(x=>x.seat===observer.self&&x.access==='write'));
    assertID('stale-removed-baseline-submitted-'+mode,sent?.membership_only?.baseline.includes(r.self));
    const after=Object.fromEntries(seats.map(s=>[s.name,counts(s)]));assert.deepEqual(after,before);
    manifest['stale-removed-membership-'+mode]={submitted:sent?.membership_only,grants:saved.members.map(x=>({seat:x.seat,access:x.access})),before,after};
    await screenshot(owner,'stale-removed-membership-'+mode+'.png');
  } finally {release();heldReleases.delete(release);await owner.unroute('**'+sharePath);}
  await b.api(sharePath,'PUT',{server:b.serverID,scope:'all',members:[{seat:r.self,access:'write'},{seat:observer.self,access:'write'}]});
}
async function earlyNativeComment(mode, ownerSeat, readerSeat) {
  const native=await node(ownerSeat,'claude','Early native '+mode,'Synthetic native initial');
  await ownerSeat.api('/api/team/shares/'+native.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+native.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.chat===share.uid&&x.owner===ownerSeat.self),30000,'early native share');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/');
  const owner=await pageFor(ownerSeat,mode,'early-owner-'+mode),reader=await pageFor(readerSeat,mode,'early-reader-'+mode);
  await open(owner,ownerSeat.node.id);
  if(mode==='phone'){await owner.locator('#chatback').click();await owner.waitForFunction(()=>document.querySelector('#cards').getBoundingClientRect().left>=-1);}
  await owner.locator('[data-tab=current]').click();
  const mine=owner.locator('[data-activity-group=mine]');await mine.waitFor({timeout:30000});
  if(await mine.getAttribute('aria-expanded')==='true')await mine.click();
  assertID('early-native-mine-collapsed-'+mode,(await mine.getAttribute('aria-expanded'))==='false');
  let release,ready,released=false;const held=new Promise(resolve=>release=resolve),requested=new Promise(resolve=>ready=resolve);heldReleases.add(release);
  await reader.route('**/api/team/subscriptions/'+key+'?after=*',async route=>{try{const response=await route.fetch();ready();await held;if(!reader.isClosed())await route.fulfill({response});}catch(e){if(!released&&!shutting&&!reader.isClosed())throw e;}});
  try {
    await open(reader,'team/'+key);await Promise.race([requested,delay(20000).then(()=>{throw Error('initial mirror not requested');})]);
    await reader.locator('#prompt[contenteditable=true]').waitFor({timeout:15000});
    const before=Object.fromEntries(seats.map(x=>[x.name,counts(x)]));
    const text='Synthetic early native comment '+mode;const posted=reader.waitForResponse(x=>x.request().method()==='POST'&&x.url().endsWith('/comments'),{timeout:15000});
    await reader.locator('#prompt').fill(text);await reader.locator('#sendbtn').click();const response=await posted;
    const own=await until(async()=>(await ownerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.chat===share.uid&&x.access==='owner'),30000,'owner subscription');
    const ownKey=[own.server,own.owner,own.chat,own.generation].join('/');
    const copied=await until(async()=>(await ownerSeat.api('/api/team/subscriptions/'+ownKey+'/comments?after=0')).entries.find(x=>x.text===text),20000,'early comment copied');
    const normal=await ownerSeat.api('/api/nodes/'+native.id+'/chat');
    assertID('early-native-precondition-'+mode,response.status()===200&&copied.anchor===0&&own.local_frontier.segment===0&&own.local_frontier.record===-1&&normal.segment_index===1&&normal.prior_turns===0);
    const unread=await until(async()=>{const v=(await ownerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.chat===share.uid&&x.access==='owner');return v?.unread&&v.comments_last>=copied.seq&&v.seen_comments<copied.seq?v:null;},30000,'unselected owner unread');
    await mine.locator('[aria-label=unread]').waitFor({timeout:30000});await delay(2500);
    const stillUnread=(await ownerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.chat===share.uid&&x.access==='owner');
    assertID('early-native-collapsed-unread-'+mode,stillUnread.unread&&stillUnread.seen_comments<copied.seq&&await mine.locator('[aria-label=unread]').count()===1);
    await mine.click();await open(owner,native.id);
    const bubble=owner.locator('#msgs .gmsg').filter({hasText:text});await bubble.waitFor({timeout:30000});
    assertID('early-native-author-source-once-'+mode,await bubble.count()===1&&await bubble.getAttribute('data-gkey')===ownKey&&Number(await bubble.getAttribute('data-cseq'))===copied.seq&&(await bubble.innerText()).includes(readerSeat.name));
    const seen=await until(async()=>{const v=(await ownerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.chat===share.uid&&x.access==='owner');return v&&v.seen_comments>=copied.seq&&!v.unread?v:null;},30000,'opened owner marked seen');
    assertID('early-native-opened-seen-'+mode,!!seen);
    const after=Object.fromEntries(seats.map(x=>[x.name,counts(x)]));assert.deepEqual(after,before);
    assertID('early-native-visible-no-agent-dispatch-'+mode,true);
    manifest['early-native-'+mode]={copied,ownerView:own,unread,seen,before,after};await screenshot(owner,'early-native-'+mode+'.png');
  } finally {released=true;release();heldReleases.delete(release);await reader.unrouteAll({behavior:'wait'});}
}
async function receivedOfflineRetry(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Offline received '+mode,'Synthetic offline initial');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+chat.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid),30000,'offline subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/');const owner=await pageFor(ownerSeat,mode,'offline-owner-'+mode),reader=await pageFor(readerSeat,mode,'offline-reader-'+mode);
  await open(owner,chat.id);await owner.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});await open(reader,'team/'+key);
  await reader.getByText('Synthetic response 1',{exact:true}).waitFor({timeout:30000});
  const seed='Synthetic owner discussion '+mode;await owner.locator('#prompt').fill(seed);await owner.locator('#sendbtn').click();await reader.getByText(seed,{exact:true}).waitFor({timeout:30000});
  const human=reader.locator('#msgs .gmsg').filter({hasText:seed});const preAuthor=(await human.locator('.gwho').innerText()).trim();
  const preComments=await readerSeat.api('/api/team/subscriptions/'+key+'/comments?after=0');const preHuman=preComments.entries.find(x=>x.text===seed);
  const preMirror=await readerSeat.api('/api/team/subscriptions/'+key+'?after=0');const preAssistant=preMirror.entries.find(x=>x.t==='assistant'&&x.text==='Synthetic response 1');
  const preAgent=reader.locator('#msgs [data-seq]').filter({hasText:'Synthetic response 1'}).first();const preAgentAuthor=(await preAgent.locator('.gwho').innerText()).trim();
  assert.ok(preAuthor&&preHuman&&preHuman.author===sub.owner&&(await human.locator('.gwho').getAttribute('data-seat'))===preHuman.author&&Number(await human.getAttribute('data-cseq'))===preHuman.seq&&preAssistant&&preAgentAuthor&&Number(await preAgent.getAttribute('data-seq'))===preAssistant.seq,'pre-outage identities');
  const draft='Retry human after offline '+mode;await reader.locator('#prompt').fill(draft);const before=counts(ownerSeat);
  await terminate(relay.child);assert.ok(relay.child.exitCode!==null||relay.child.signalCode!==null,'owned relay stopped');
  try {
    if(mode==='phone')await reader.locator('#chatback').click();await open(reader,readerSeat.node.id);
    await reader.reload({waitUntil:'networkidle'});if(mode==='phone'&&await reader.locator('#cards').evaluate(el=>el.getBoundingClientRect().right<=1))await reader.locator('#chatback').click();
    await open(reader,'team/'+key);await reader.getByText(seed,{exact:true}).waitFor({timeout:30000});await reader.getByText('Synthetic response 1',{exact:true}).waitFor({timeout:30000});
    const author=reader.locator('#msgs .gmsg').filter({hasText:seed}).locator('.gwho');
    const saved=await readerSeat.api('/api/team/subscriptions/'+key+'?after=0');
    const assistant=saved.entries.find(x=>x.t==='assistant'&&x.text==='Synthetic response 1');
    const savedComments=await readerSeat.api('/api/team/subscriptions/'+key+'/comments?after=0');const savedHuman=savedComments.entries.find(x=>x.text===seed);
    const agentBubble=reader.locator('#msgs [data-seq]').filter({hasText:'Synthetic response 1'}).first();
    assertID('received-offline-saved-copy-and-draft-'+mode,(await reader.locator('#prompt').innerText())===draft&&(await author.innerText()).trim()===preAuthor&&(await author.getAttribute('data-seat'))===preHuman.author&&!!savedHuman&&savedHuman.author===preHuman.author&&savedHuman.seq===preHuman.seq&&savedHuman.anchor===preHuman.anchor&&Number(await author.locator('..').locator('..').getAttribute('data-cseq'))===preHuman.seq&&!!assistant&&assistant.seq===preAssistant.seq&&assistant.agent===preAssistant.agent&&(await agentBubble.locator('.gwho').innerText()).trim()===preAgentAuthor&&Number(await agentBubble.getAttribute('data-seq'))===preAssistant.seq&&[saved.view.server,saved.view.owner,saved.view.chat,saved.view.generation].join('/')===key);
    const dialog=reader.waitForEvent('dialog');await reader.locator('#sendbtn').click();const failure=await dialog;await failure.accept();
    assertID('received-offline-failed-send-keeps-draft-'+mode,(await reader.locator('#prompt').innerText())===draft&&JSON.stringify(counts(ownerSeat))===JSON.stringify(before));
  } finally {await restartRelay();}
  await reader.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
  let attemptedKey, retryKey;const pattern='**/api/team/subscriptions/'+key+'/comments';
  await reader.route(pattern,async route=>{if(route.request().method()!=='POST'){await route.continue();return;}attemptedKey=route.request().postDataJSON().key;await route.fetch();await route.abort('failed');});
  try {
    const dialog=reader.waitForEvent('dialog');await reader.locator('#sendbtn').click();const failure=await dialog;await failure.accept();
    assertID('received-retry-uncertain-keeps-draft-'+mode,(await reader.locator('#prompt').innerText())===draft);
  } finally {await reader.unroute(pattern);}
  await reader.route(pattern,route=>{if(route.request().method()==='POST')retryKey=route.request().postDataJSON().key;return route.continue();});
  try {await reader.locator('#sendbtn').click();await reader.getByText(draft,{exact:true}).waitFor({timeout:30000});} finally {await reader.unroute(pattern);}
  const entries=await readerSeat.api('/api/team/subscriptions/'+key+'/comments?after=0');
  assertID('received-explicit-retry-idempotent-'+mode,!!attemptedKey&&attemptedKey===retryKey&&entries.entries.filter(x=>x.text===draft).length===1&&JSON.stringify(counts(ownerSeat))===JSON.stringify(before));
  manifest['received-offline-'+mode]={key,preAuthor,preAgentAuthor,human:{author:preHuman.author,seq:preHuman.seq,anchor:preHuman.anchor},assistant:{seq:preAssistant.seq,agent:preAssistant.agent||'Agent'},before,after:counts(ownerSeat),sequence:entries.entries.find(x=>x.text===draft)?.seq};await screenshot(reader,'received-offline-retry-'+mode+'.png');
}
async function heldChatLoading(mode,seat) {
  const chat=await node(seat,'codex','Held chat loading '+mode,'Synthetic loading initial');
  const page=await pageFor(seat,mode,'loading-'+mode);await page.emulateMedia({reducedMotion:'reduce'});
  let release,ready,released=false;const held=new Promise(resolve=>release=resolve),requested=new Promise(resolve=>ready=resolve);heldReleases.add(release);
  await page.route('**/api/nodes/'+chat.id+'/chat*',async route=>{try{const response=await route.fetch();ready();await held;if(!page.isClosed())await route.fulfill({response});}catch(e){if(!released&&!shutting&&!page.isClosed())throw e;}});
  try {
    await open(page,chat.id);await Promise.race([requested,delay(20000).then(()=>{throw Error('held chat not requested');})]);
    const spinner=page.locator('#chatloading .spin');await spinner.waitFor({timeout:10000});
    const animation=await spinner.evaluate(el=>({name:getComputedStyle(el).animationName,state:getComputedStyle(el).animationPlayState,first:getComputedStyle(el).transform}));
    await delay(250);animation.second=await spinner.evaluate(el=>getComputedStyle(el).transform);
    assertID('held-chat-loading-reduced-motion-'+mode,animation.name!=='none'&&animation.state==='running'&&animation.first!==animation.second);
    manifest['held-chat-animation-'+mode]=animation;await screenshot(page,'held-chat-loading-'+mode+'.png');
    released=true;release();heldReleases.delete(release);await page.unrouteAll({behavior:'wait'});await page.getByText('Synthetic response 1',{exact:true}).waitFor({timeout:30000});
  } finally {released=true;release();heldReleases.delete(release);await page.unrouteAll({behavior:'wait'});}
}
async function unsharedChat(mode,seat) {
  const first=await node(seat,'codex','Unshared first '+mode,'Synthetic unshared first');const second=await node(seat,'codex','Unshared second '+mode,'Synthetic unshared second');
  const page=await pageFor(seat,mode,'unshared-'+mode);await open(page,first.id);await page.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
  const prompt='Synthetic unshared draft '+mode;await page.locator('#prompt').fill(prompt);
  if(mode==='phone')await page.locator('#chatback').click();await open(page,second.id);
  if(mode==='phone')await page.locator('#chatback').click();await open(page,first.id);
  assertID('unshared-navigation-preserves-draft-'+mode,(await page.locator('#prompt').innerText())===prompt);
  await until(()=>page.locator('#sendbtn').isEnabled(),30000,'unshared send ready');
  const before=counts(seat);if(mode==='phone'){await page.locator('#prompt').focus();await page.locator('#prompt').press('Enter');assertID('unshared-phone-enter-no-send',JSON.stringify(counts(seat))===JSON.stringify(before));await page.locator('#prompt').fill(prompt);await page.locator('#sendbtn').click();}else{await page.locator('#prompt').focus();await page.locator('#prompt').press('Enter');}
  await until(()=>counts(seat).accepted===before.accepted+1,20000,'unshared explicit send');
  const after=counts(seat);assertID('unshared-send-once-'+mode,after.attempted===before.attempted+1&&after.accepted===before.accepted+1);
  await page.getByText('Synthetic response 2',{exact:true}).waitFor({timeout:30000});
  const bubble=page.locator('#msgs .turn.user:not(.echo) .bubble').filter({hasText:prompt}).first();await bubble.waitFor({timeout:30000});await bubble.click();await page.locator('[data-bact=bookmark]').click();
  await until(async()=>(await seat.api('/api/ui')).bookmarks?.some(x=>x.text===prompt),20000,'unshared bookmark');
  if(!await page.locator('[data-bact=sendto]').isVisible())await bubble.click();
  await page.locator('[data-bact=sendto]').click();await page.locator('#sendto_list [data-fwd="'+second.id+'"]').click();
  assertID('unshared-bookmark-sendto-'+mode,(await page.locator('#chattitle').innerText())===second.title&&(await page.locator('#prompt').innerText()).includes(prompt)&&JSON.stringify(counts(seat))===JSON.stringify(after));
}
async function mediaContrast(mode,scheme,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Media contrast '+mode+' '+scheme,'Synthetic media initial');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const page=await pageFor(ownerSeat,mode,'media-'+mode+'-'+scheme);await page.emulateMedia({colorScheme:scheme});await open(page,chat.id);
  await page.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});await page.locator('#destinationagent').click();
  const png=Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aX6sAAAAASUVORK5CYII=','base64');
  await page.locator('#attfile').setInputFiles({name:'synthetic-pixel.png',mimeType:'image/png',buffer:png});await page.locator('#attstage .stagechip.done').waitFor({timeout:30000});
  await page.locator('#prompt').fill('');await page.locator('#sendbtn').click();
  const caption=page.locator('#msgs .turn.user.media .gwho').last();await caption.waitFor({timeout:30000});
  const colors=await caption.evaluate(el=>({text:getComputedStyle(el).color,background:getComputedStyle(document.querySelector('#chathead')).backgroundColor}));
  const luminance=color=>{const rgb=color.match(/[\d.]+/g).slice(0,3).map(x=>Number(x)/255).map(v=>v<=0.04045?v/12.92:((v+0.055)/1.055)**2.4);return rgb[0]*0.2126+rgb[1]*0.7152+rgb[2]*0.0722;};
  const a=luminance(colors.text),b=luminance(colors.background),contrast=(Math.max(a,b)+0.05)/(Math.min(a,b)+0.05);
  assertID('media-caption-contrast-'+mode+'-'+scheme,(await caption.innerText())==='You · Agent'&&contrast>=4.5);
  manifest['media-contrast-'+mode+'-'+scheme]={...colors,contrast};await screenshot(page,'media-'+mode+'-'+scheme+'.png');
}
async function reshareStale(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Reshare '+mode,'Synthetic reshare initial');const sharePath='/api/team/shares/'+chat.id;
  await ownerSeat.api(sharePath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const initial=await ownerSeat.api(sharePath);const old=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===initial.uid&&x.generation===initial.generation),30000,'old generation');
  const oldKey=[old.server,old.owner,old.chat,old.generation].join('/');const page=await pageFor(readerSeat,mode,'reshare-reader-'+mode);await open(page,'team/'+oldKey);
  await page.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});const prior='Old generation comment '+mode;await page.locator('#prompt').fill(prior);await page.locator('#sendbtn').click();await page.getByText(prior,{exact:true}).waitFor({timeout:30000});
  let release,ready,released=false;const held=new Promise(resolve=>release=resolve),requested=new Promise(resolve=>ready=resolve);heldReleases.add(release);
  await page.route('**/api/team/subscriptions/'+oldKey+'?after=*',async route=>{try{const response=await route.fetch();ready();await held;if(!page.isClosed())await route.fulfill({response});}catch(e){if(!released&&!shutting&&!page.isClosed())throw e;}});
  try {
    await Promise.race([requested,delay(20000).then(()=>{throw Error('old generation mirror not requested');})]);const before=counts(ownerSeat);
    await ownerSeat.api(sharePath,'DELETE');await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.some(x=>x.chat===old.chat&&x.generation===old.generation&&x.state==='ended'),30000,'old generation ended');
    await ownerSeat.api(sharePath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});const next=await ownerSeat.api(sharePath);
    assert.notEqual(next.generation,old.generation);const fresh=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===old.chat&&x.generation===next.generation),30000,'new generation');
    const freshKey=[fresh.server,fresh.owner,fresh.chat,fresh.generation].join('/');if(mode==='phone')await page.locator('#chatback').click();await open(page,'team/'+freshKey);await page.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
    const draft='New generation draft '+mode;await page.locator('#prompt').fill(draft);released=true;release();heldReleases.delete(release);await page.unrouteAll({behavior:'wait'});
    await delay(1000);assertID('reshare-stale-response-keeps-new-generation-'+mode,await page.evaluate(id=>localStorage.getItem('scimux-sel')===id,'team/'+freshKey)&&(await page.locator('#prompt').innerText())===draft&&JSON.stringify(counts(ownerSeat))===JSON.stringify(before));
    const subscriptions=(await readerSeat.api('/api/team/subscriptions')).subscriptions;assertID('reshare-retains-old-copy-'+mode,subscriptions.some(x=>x.chat===old.chat&&x.generation===old.generation&&x.state==='ended')&&subscriptions.some(x=>x.chat===old.chat&&x.generation===fresh.generation));
    await page.locator('#sendbtn').click();await page.getByText(draft,{exact:true}).waitFor({timeout:30000});
    assertID('reshare-new-discussion-once-'+mode,await page.getByText(draft,{exact:true}).count()===1&&JSON.stringify(counts(ownerSeat))===JSON.stringify(before));
    manifest['reshare-'+mode]={oldKey,freshKey,before,after:counts(ownerSeat)};
  } finally {released=true;release();heldReleases.delete(release);await page.unrouteAll({behavior:'wait'});}
}
async function droppedSend(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Dropped response '+mode,'Synthetic dropped response initial');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+chat.id);const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid),30000,'dropped subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/');const owner=await pageFor(ownerSeat,mode,'dropped-owner-'+mode),reader=await pageFor(readerSeat,mode,'dropped-reader-'+mode);
  await open(owner,chat.id);await open(reader,'team/'+key);await reader.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
  const comment='Synthetic dropped source '+mode;await reader.locator('#prompt').fill(comment);await reader.locator('#sendbtn').click();
  const source=owner.locator('#msgs .bubble').filter({hasText:comment}).first();await source.waitFor({timeout:30000});await source.click();await owner.locator('[data-gact=prompt]').click();
  const prompt='Synthetic dropped prompt '+mode+' [delay-dispatch]';await owner.locator('#prompt').fill(prompt);
  let release,ready,released=false;const held=new Promise(resolve=>release=resolve),requested=new Promise(resolve=>ready=resolve);heldReleases.add(release);
  await owner.route('**/api/nodes/'+chat.id+'/send',async route=>{try{await route.fetch();ready();await held;if(!owner.isClosed())await route.abort('failed');}catch(e){if(!released&&!shutting&&!owner.isClosed())throw e;}});
  try {
    const before=counts(ownerSeat);await owner.locator('#sendbtn').click();await Promise.race([requested,delay(20000).then(()=>{throw Error('real send not received');})]);
    await owner.locator('#destinationpeople').click();await owner.locator('#prompt').fill('People after dropped response '+mode);
    await owner.locator('#destinationagent').click();await owner.locator('#prompt').fill('New agent after dropped response '+mode);
    await owner.locator('#attfile').setInputFiles({name:'newer-after-send.txt',mimeType:'text/plain',buffer:Buffer.from('newer synthetic attachment')});
    await owner.locator('#attstage .stagechip.done').waitFor({timeout:30000});
    released=true;release();heldReleases.delete(release);await owner.unrouteAll({behavior:'wait'});await delay(4500);
    const after=counts(ownerSeat);assertID('dropped-response-no-automatic-retry-'+mode,after.attempted===before.attempted+1&&after.accepted===before.accepted+1&&after.controls===before.controls);
    assertID('dropped-response-not-marked-sent-'+mode,await owner.locator('[aria-label="Sent as prompt"]').count()===0);
    const agentDraft=await owner.locator('#prompt').innerText();
    assertID('dropped-response-keeps-newer-drafts-'+mode,agentDraft.includes('New agent after dropped response '+mode)&&agentDraft.includes(prompt)&&(await owner.locator('#attstage').innerText()).includes('newer-after-send.txt'));
    await owner.locator('#destinationpeople').click();assertID('dropped-response-keeps-people-draft-'+mode,(await owner.locator('#prompt').innerText())==='People after dropped response '+mode);
    await owner.locator('#destinationagent').click();await owner.locator('#attstage .sx').click();const retry='Explicit reviewed retry '+mode;await owner.locator('#prompt').fill(retry);await owner.locator('#sendbtn').click();
    await until(()=>counts(ownerSeat).accepted===after.accepted+1,20000,'explicit reviewed retry accepted');const final=counts(ownerSeat);
    const exact=jsonLines(path.join(ownerSeat.data,'counter')).filter(x=>x.text===retry);
    assertID('dropped-response-explicit-retry-once-'+mode,final.attempted===after.attempted+1&&final.accepted===after.accepted+1&&final.controls===after.controls&&exact.length===1);
    manifest['dropped-send-'+mode]={before,after,final};
  } finally {released=true;release();heldReleases.delete(release);await owner.unrouteAll({behavior:'wait'});}
}
async function viewportKeyboard(mode,readerSeat) {
  const ui=await readerSeat.api('/api/ui');await readerSeat.uiSetup({...ui,pinned:[readerSeat.node.id]});
  const page=await pageFor(readerSeat,mode,'keyboard-'+mode);await page.locator('[data-tab=shared]').waitFor({timeout:30000});
  for(const tab of ['current','pinned','archived','shared'])for(const key of ['Enter','Space']){
    const button=page.locator('[data-tab='+tab+']');await button.focus();await button.press(key);
    assertID('viewport-keyboard-'+mode+'-'+tab+'-'+key,(await button.getAttribute('aria-pressed'))==='true'&&await button.evaluate(el=>document.activeElement===el));
  }
  const selected=page.locator('[data-tab=shared]');await selected.focus();const scroll=await page.locator('#cardtabs').evaluate(el=>el.scrollLeft);
  const before=await page.evaluate(()=>['#cards','#chat'].map(q=>{const r=document.querySelector(q).getBoundingClientRect();return [r.left,r.top,r.width,r.height];}));
  const poll=page.waitForResponse(r=>new URL(r.url()).pathname==='/api/state'&&[200,304].includes(r.status()),{timeout:20000});
  await readerSeat.api('/api/nodes/'+readerSeat.node.id,'PATCH',{title:'Keyboard updated '+mode});await poll;await delay(200);
  const after=await page.evaluate(()=>['#cards','#chat'].map(q=>{const r=document.querySelector(q).getBoundingClientRect();return [r.left,r.top,r.width,r.height];}));
  assertID('viewport-keyboard-poll-focus-'+mode,await selected.evaluate((el,x)=>document.activeElement===el&&el.parentElement.scrollLeft===x,scroll)&&JSON.stringify(before)===JSON.stringify(after));
  assertID('viewport-no-page-overflow-'+mode,await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
  await screenshot(page,'keyboard-'+mode+'.png');
  await page.locator('[data-tab=current]').click();await open(page,readerSeat.node.id);
  if(page.viewportSize().width<900)await page.waitForFunction(()=>document.querySelector('#cards').getBoundingClientRect().right<=1);
  for(const id of ['sharebtn','sendbtn','chatback']){
    const button=page.locator('#'+id);if(!await button.isVisible())continue;
    const rect=await button.boundingBox();const hits=await button.evaluate(el=>{const r=el.getBoundingClientRect();return [[1,1],[r.width-1,1],[1,r.height-1],[r.width-1,r.height-1]].every(([x,y])=>{const hit=document.elementFromPoint(r.left+x,r.top+y);return hit===el||el.contains(hit);});});
    assertID('viewport-control-target-'+mode+'-'+id,!!rect&&rect.width>=44&&rect.height>=44&&hits);
  }
  await screenshot(page,'chat-controls-'+mode+'.png');
}
async function heldEvidence(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Held evidence '+mode,'Synthetic evidence initial');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+chat.id);const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid),30000,'evidence subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/');const owner=await pageFor(ownerSeat,mode,'evidence-owner-'+mode),reader=await pageFor(readerSeat,mode,'evidence-reader-'+mode);
  await open(owner,chat.id);await open(reader,'team/'+key);await reader.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
  const comment='Synthetic held evidence source '+mode;await reader.locator('#prompt').fill(comment);await reader.locator('#sendbtn').click();
  const source=owner.locator('#msgs .bubble').filter({hasText:comment}).first();await source.waitFor({timeout:30000});await source.click();await owner.locator('[data-gact=prompt]').click();
  const prompt='Synthetic held evidence prompt '+mode+' [delay-dispatch]';await owner.locator('#prompt').fill(prompt);
  const before=counts(ownerSeat);await owner.locator('#sendbtn').click();await owner.locator('[aria-label="Awaiting prompt delivery"]').waitFor({timeout:15000});
  assert.equal(counts(ownerSeat).accepted,before.accepted,'synthetic agent is still delaying acceptance');
  let release,ready,released=false;const held=new Promise(resolve=>release=resolve),requested=new Promise(resolve=>ready=resolve);heldReleases.add(release);
  await owner.route('**/api/nodes/'+chat.id+'/chat*',async route=>{try{const response=await route.fetch();ready();await held;if(!owner.isClosed())await route.fulfill({response});}catch(e){if(!released&&!shutting&&!owner.isClosed())throw e;}});
  try {
    await until(()=>counts(ownerSeat).accepted===before.accepted+1,15000,'held evidence accepted');
    await Promise.race([requested,delay(20000).then(()=>{throw Error('later evidence not requested');})]);
    const during=counts(ownerSeat);assertID('held-evidence-awaiting-not-sent-'+mode,await owner.locator('[aria-label="Sent as prompt"]').count()===0&&during.attempted===before.attempted+1&&during.accepted===before.accepted+1&&during.controls===before.controls);
    await owner.locator('#destinationpeople').click();await owner.locator('#prompt').fill('New people while awaiting '+mode);
    released=true;release();heldReleases.delete(release);await owner.unrouteAll({behavior:'wait'});
    await owner.locator('[aria-label="Sent as prompt"]').waitFor({timeout:30000});
    assertID('held-evidence-confirmed-after-release-'+mode,(await owner.locator('#prompt').innerText())==='New people while awaiting '+mode&&JSON.stringify(counts(ownerSeat))===JSON.stringify(during));
    manifest['held-evidence-'+mode]={before,during,after:counts(ownerSeat)};
  } finally {released=true;release();heldReleases.delete(release);await owner.unrouteAll({behavior:'wait'});}
}
async function recipientLeaveStale(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Delayed leave '+mode,'Synthetic delayed leave initial');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+chat.id);const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid),30000,'delayed leave subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/');const page=await pageFor(readerSeat,mode,'delayed-leave-'+mode);await open(page,'team/'+key);
  let release,ready,released=false;const held=new Promise(resolve=>release=resolve),requested=new Promise(resolve=>ready=resolve);heldReleases.add(release);
  await page.route('**/api/team/subscriptions/'+key,async route=>{if(route.request().method()!=='DELETE'){await route.continue();return;}try{const response=await route.fetch();ready();await held;if(!page.isClosed())await route.fulfill({response});}catch(e){if(!released&&!shutting&&!page.isClosed())throw e;}});
  try {
    page.once('dialog',dialog=>dialog.accept());await page.locator('#guestremove').click();await Promise.race([requested,delay(20000).then(()=>{throw Error('leave not requested');})]);
    if(mode==='phone')await page.locator('#chatback').click();await open(page,readerSeat.node.id);
    await page.locator('#prompt[contenteditable=true]').focus();release();released=true;heldReleases.delete(release);
    await until(async()=>!(await readerSeat.api('/api/team/subscriptions')).subscriptions.some(x=>x.chat===share.uid),30000,'delayed copy removed');
    await delay(500);assertID('leave-delayed-keeps-new-selection-'+mode,(await page.locator('#chattitle').innerText())===readerSeat.name+' chat'&&await page.locator('#prompt').evaluate(el=>document.activeElement===el));
  } finally {released=true;release();heldReleases.delete(release);await page.unrouteAll({behavior:'wait'});}
}
async function recipientLeaveEmpty(mode,ownerSeat) {
  const seat=await makeSeat('empty-'+mode);
  await seat.api('/api/settings','PUT',{experimental_feature_team:true});
  const issue=JSON.parse(execFileSync(relayBin,['-state-dir',relay.dir,'-url',relay.url,'-json','issue'],{env:baseEnv,encoding:'utf8'}));
  await seat.api('/api/team/servers','POST',{link:issue.link,name:'empty '+mode});
  seat.serverID=(await seat.api('/api/team/servers')).servers[0].id;seat.self=(await seat.api('/api/team/servers/'+seat.serverID+'/seats')).self;
  const chat=await node(ownerSeat,'codex','Empty recipient '+mode,'Synthetic empty recipient initial');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:seat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+chat.id);const sub=await until(async()=>(await seat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid),30000,'empty recipient subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/');const page=await pageFor(seat,mode,'empty-reader-'+mode);
  await open(page,'team/'+key);await page.locator('#guestremove').waitFor({timeout:30000});
  page.once('dialog',dialog=>dialog.accept());await page.locator('#guestremove').click();
  await until(async()=>!(await seat.api('/api/team/subscriptions')).subscriptions.some(x=>x.chat===share.uid),30000,'empty copy removed');
  await page.waitForFunction(()=>document.activeElement===document.querySelector('#plusbtn'),null,{timeout:30000});
  assertID('leave-empty-activities-fallback-'+mode,(await seat.api('/api/state')).nodes.length===0&&(await ownerSeat.api('/api/state')).nodes.some(x=>x.id===chat.id));
}
async function permissionLifecycle(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Permission lifecycle '+mode,'Synthetic lifecycle initial');
  const sharePath='/api/team/shares/'+chat.id;
  await ownerSeat.api(sharePath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api(sharePath);const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid),30000,'lifecycle subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/');const owner=await pageFor(ownerSeat,mode,'permission-owner-'+mode);await open(owner,chat.id);
  await owner.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});const page=await pageFor(readerSeat,mode,'permission-reader-'+mode);
  await open(page,'team/'+key);await page.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
  const comment='Synthetic retained human '+mode;await page.locator('#prompt').fill(comment);await page.locator('#sendbtn').click();await page.getByText(comment,{exact:true}).waitFor({timeout:30000});
  const before=counts(ownerSeat),draft='Preserve draft on read '+mode;await page.locator('#prompt').fill(draft);
  await ownerSeat.api(sharePath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'read'}]});
  await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.some(x=>x.chat===share.uid&&x.access==='read'),30000,'read permission propagated');
  await page.waitForFunction(()=>document.querySelector('#prompt').getAttribute('contenteditable')!=='true',null,{timeout:30000});
  const denied=await readerSeat.status('/api/team/subscriptions/'+key+'/comments',{text:'Synthetic denied',anchor:0,key:'abcdefaa00000001'});
  assertID('permission-read-draft-and-denial-'+mode,denied===403&&(await page.locator('#prompt').innerText())===draft);
  await ownerSeat.api(sharePath,'DELETE');
  await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.some(x=>x.chat===share.uid&&x.state==='ended'),30000,'ended copy propagated');
  await page.getByText(comment,{exact:true}).waitFor({timeout:30000});
  manifest['permission-ended-debug-'+mode]={count:await page.getByText(comment,{exact:true}).count(),before,after:counts(ownerSeat)};
  assertID('permission-ended-copy-retained-'+mode,await page.getByText(comment,{exact:true}).count()===1&&JSON.stringify(counts(ownerSeat))===JSON.stringify(before));
  const source=owner.locator('#msgs .bubble').filter({hasText:comment}).first();await source.waitFor({timeout:30000});await source.click();
  await owner.locator('[data-gact=prompt]').click();
  assertID('permission-ended-owner-prompt-no-dispatch-'+mode,(await owner.locator('#prompt').innerText()).includes(comment)&&JSON.stringify(counts(ownerSeat))===JSON.stringify(before));
  await ownerSeat.api('/api/nodes/'+chat.id+'/exit','POST',{});
  await until(async()=>(await ownerSeat.api('/api/state')).nodes.find(x=>x.id===chat.id)?.ended_at,30000,'owner node closed');
  await owner.reload({waitUntil:'networkidle'});await owner.getByText(comment,{exact:true}).waitFor({timeout:30000});
  await owner.locator('#msgs .bubble').filter({hasText:comment}).first().click();
  assertID('permission-closed-owner-no-prompt-'+mode,await owner.locator('[data-gact=prompt]').count()===0);
  manifest['permission-'+mode]={key,denied,before,after:counts(ownerSeat)};
}
async function recipientLeave(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Leave source '+mode,'Synthetic leave initial');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+chat.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid),30000,'leave subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/');const page=await pageFor(readerSeat,mode,'leave-reader-'+mode);
  await open(page,'team/'+key);const button=page.locator('#guestremove');await button.waitFor({timeout:30000});
  if(page.viewportSize().width<900)await page.waitForFunction(()=>document.querySelector('#cards').getBoundingClientRect().right<=1);
  const before=counts(ownerSeat);
  const hit=await button.evaluate(el=>{const r=el.getBoundingClientRect();return r.width>=44&&r.height>=44&&[[1,1],[r.width-1,r.height-1]].every(([x,y])=>{const target=document.elementFromPoint(r.left+x,r.top+y);return target===el||el.contains(target);});});
  assertID('leave-control-hit-target-'+mode,hit);
  page.once('dialog',dialog=>dialog.dismiss());await button.click();
  assertID('leave-cancel-retains-focus-'+mode,await button.evaluate(el=>document.activeElement===el)&&await page.locator('#chattitle').innerText()===chat.title);
  await page.route('**/api/team/subscriptions/'+key,route=>route.request().method()==='DELETE'?route.abort('failed'):route.continue());
  page.once('dialog',dialog=>dialog.accept());await button.click();await page.locator('#guestnote').getByText('That did not work. Try again.').waitFor({timeout:30000});
  assertID('leave-error-retains-copy-'+mode,await button.isEnabled()&&await button.evaluate(el=>document.activeElement===el)&&(await readerSeat.api('/api/team/subscriptions')).subscriptions.some(x=>x.chat===share.uid));
  await page.unroute('**/api/team/subscriptions/'+key);
  page.once('dialog',dialog=>dialog.accept());await button.click();
  await until(async()=>!(await readerSeat.api('/api/team/subscriptions')).subscriptions.some(x=>x.chat===share.uid),30000,'left local copy');
  const localNodes=(await readerSeat.api('/api/state')).nodes;
  await page.waitForFunction(ids=>ids.includes(localStorage.getItem('scimux-sel')),localNodes.map(x=>x.id),{timeout:30000});
  const selectedID=await page.evaluate(()=>localStorage.getItem('scimux-sel'));const selected=localNodes.find(x=>x.id===selectedID);
  assertID('leave-success-owner-intact-'+mode,!!selected&&(await page.locator('#chattitle').innerText())===selected.title&&(await ownerSeat.api('/api/state')).nodes.some(x=>x.id===chat.id)&&JSON.stringify(counts(ownerSeat))===JSON.stringify(before));
  manifest['leave-'+mode]={subscription:key,before,after:counts(ownerSeat)};
}
async function aiSourcePrompt(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','AI quote '+mode,'Synthetic AI quote initial');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+chat.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid),30000,'AI quote subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/');
  const owner=await pageFor(ownerSeat,mode,'ai-owner-'+mode),reader=await pageFor(readerSeat,mode,'ai-reader-'+mode);
  await open(owner,chat.id);await open(reader,'team/'+key);await owner.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});
  const local=await ownerSeat.api('/api/nodes/'+chat.id+'/chat');const ai=local.turns.find(t=>t.role==='assistant'&&t.text==='Synthetic response 1');assert.ok(ai);
  const before=counts(ownerSeat);await owner.getByText('Synthetic response 1',{exact:true}).first().click();await owner.locator('[data-bact=prompt]').click();
  const draft=await owner.locator('#prompt').innerText();assertID('ai-source-disclosure-no-autosend-'+mode,draft.includes('[AI-generated, quoted from another chat — not written by me:]')&&draft.includes(ai.text)&&JSON.stringify(counts(ownerSeat))===JSON.stringify(before));
  await owner.locator('#sendbtn').click();await owner.locator('[aria-label="Sent as prompt"]').waitFor({timeout:30000});
  const link=await until(async()=>{const ui=await ownerSeat.api('/api/ui');return (ui.forward_links||[]).find(x=>x.kind==='prompt'&&x.destination?.node===chat.id&&x.source?.uid===ai.uid&&x.source.record===ai.record);},30000,'AI source exact link');
  const turn=await until(async()=>{const c=await ownerSeat.api('/api/nodes/'+chat.id+'/chat');return c.turns.find(t=>t.uid===link.destination.uid&&(t.segment??0)===(link.destination.segment??0)&&t.record===link.destination.record);},30000,'AI prompt local turn');
  const mirrored=await until(async()=>{const m=await readerSeat.api('/api/team/subscriptions/'+key+'?after=0');return m.entries.find(x=>x.t==='user'&&x.text===turn.text);},30000,'AI disclosure mirror');
  const after=counts(ownerSeat);assertID('ai-source-explicit-exact-mirrored-'+mode,link.source.role==='assistant'&&after.attempted===before.attempted+1&&after.accepted===before.accepted+1&&after.controls===before.controls&&turn.text.includes('[AI-generated, quoted from another chat — not written by me:]')&&mirrored.text===turn.text);
  await reader.getByText('AI-generated, quoted from another chat',{exact:false}).first().waitFor({timeout:30000});
  manifest['ai-source-'+mode]={source:link.source,destination:link.destination,turn,mirrored,before,after};
}
async function recipientBoundary(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Boundary '+mode,'Synthetic boundary initial');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+chat.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid),30000,'boundary subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/');
  const owner=await pageFor(ownerSeat,mode,'boundary-owner-'+mode);await open(owner,chat.id);
  await owner.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});
  const page=await pageFor(readerSeat,mode,'boundary-reader-'+mode);await open(page,'team/'+key);
  await page.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
  assertID('boundary-no-agent-control-ui-'+mode,!(await page.locator('#destinationagent').isVisible())&&!(await page.locator('#autoapprove').isVisible()));
  const lease=await ownerSeat.api('/api/nodes/'+chat.id+'/auto-approve','POST',{enabled:true});assert.ok(lease.enabled);
  const before=counts(ownerSeat),history=await ownerSeat.api('/api/nodes/'+chat.id+'/chat');const statuses=[];
  const actions=['send','key','send/resolve','send/interrupt','claude-interaction','auto-approve','exit'];
  for(const id of [chat.id,encodeURIComponent('team/'+key),encodeURIComponent('../'+chat.id)])for(const action of actions){
    const status=await readerSeat.status('/api/nodes/'+id+'/'+action,{text:'Synthetic forged prompt',key:'Enter',enabled:true});statuses.push({id,action,status});
    assert.ok(status>=400,`${id}/${action}: ${status}`);assert.deepEqual(counts(ownerSeat),before);
  }
  for(const field of ['agent','destination','control']){
    const status=await readerSeat.status('/api/team/subscriptions/'+key+'/comments',{text:'Synthetic rejected control',anchor:0,key:'abcdefff00000001',[field]:'agent'});
    assert.equal(status,400);assert.deepEqual(counts(ownerSeat),before);
  }
  const recipientBefore=(await readerSeat.api('/api/state')).nodes.map(x=>x.id).sort(),recipientCounts=counts(readerSeat);
  const clearStatus=await readerSeat.status('/api/nodes/'+encodeURIComponent(chat.id)+'/send',{text:'/clear'});
  const forkStatuses=[];for(const parent of [chat.id,'team/'+key])forkStatuses.push(await readerSeat.status('/api/nodes',{title:'Synthetic forbidden fork',agent:'codex',dir:readerSeat.data,parent,prompt:'Synthetic fork'}));
  const recipientAfter=(await readerSeat.api('/api/state')).nodes.map(x=>x.id).sort();
  assertID('boundary-real-clear-and-fork-rejected-'+mode,clearStatus>=400&&forkStatuses.every(x=>x>=400)&&JSON.stringify(recipientBefore)===JSON.stringify(recipientAfter)&&JSON.stringify(counts(readerSeat))===JSON.stringify(recipientCounts)&&JSON.stringify(counts(ownerSeat))===JSON.stringify(before));
  manifest['boundary-real-controllers-'+mode]={clearStatus,forkStatuses};
  const credential=JSON.parse(fs.readFileSync(path.join(readerSeat.data,'team.json'),'utf8')).servers.find(x=>x.id===sub.server).token;
  for(const action of actions){
    const response=await fetch(relay.url+'/v1/channels/'+sub.owner+'/'+sub.chat+'/'+action,{method:'POST',headers:{Authorization:'Bearer '+credential,'Content-Type':'application/json'},body:JSON.stringify({text:'Synthetic forged prompt'})});
    assert.ok(response.status>=400,`relay ${action}: ${response.status}`);assert.deepEqual(counts(ownerSeat),before);
  }
  const comment='Synthetic boundary discussion '+mode;await page.locator('#prompt').fill(comment);await page.locator('#sendbtn').click();
  await until(async()=>{try{return (await ownerSeat.api('/api/team/subscriptions/'+key+'/comments?after=0')).entries.some(x=>x.text===comment);}catch{return false;}},30000,'owner received boundary discussion');
  assert.deepEqual(counts(ownerSeat),before);
  await page.reload({waitUntil:'networkidle'});await page.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
  const afterReload='Synthetic reconnect discussion '+mode;await page.locator('#prompt').fill(afterReload);await page.locator('#sendbtn').click();
  await until(async()=>{try{return (await ownerSeat.api('/api/team/subscriptions/'+key+'/comments?after=0')).entries.some(x=>x.text===afterReload);}catch{return false;}},30000,'reconnected discussion');
  assert.deepEqual(counts(ownerSeat),before);
  assertID('boundary-armed-owner-comments-no-dispatch-'+mode,true);
  const after=await ownerSeat.api('/api/nodes/'+chat.id+'/chat');
  assertID('boundary-forgeries-rejected-'+mode,JSON.stringify(after.turns)===JSON.stringify(history.turns));
  manifest['boundary-'+mode]={statuses,before,after:counts(ownerSeat),subscription:key};
}
async function nativeStartupFailure(mode,seat) {
  const page=await pageFor(seat,mode,'startup-failure-'+mode);
  const prompt='Synthetic recoverable startup prompt '+mode,title='Synthetic startup failure '+mode;
  const marker=path.join(seat.data,'.scimux-e2e-no-hook');fs.writeFileSync(marker,'');
  let createError='';
  try {await seat.api('/api/nodes','POST',{agent:'claude',title,prompt,dir:seat.data});}
  catch(e){createError=String(e);} finally {fs.unlinkSync(marker);}
  const created=await until(async()=>(await seat.api('/api/state')).nodes.find(n=>n.title===title),20000,'owned failed node exists');
  seat.nodes.push(created.id);await open(page,created.id);
  const evidence=await until(async()=>{const c=await seat.api('/api/nodes/'+created.id+'/chat');return c.restore_draft===prompt?c:null;},30000,'actual native startup restore');
  await page.waitForFunction(text=>document.querySelector('#prompt').innerText===text,prompt,{timeout:30000});
  assertID('native-start-failure-restores-draft-'+mode,!!evidence.error&&evidence.restore_draft===prompt);
  manifest['native-start-failure-'+mode]={createError,error:evidence.error,counters:counts(seat)};
  await screenshot(page,'native-start-failure-'+mode+'.png');
}
async function conversationTrace(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','trace '+mode,'Synthetic trace initial');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+chat.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid&&x.generation===share.generation),30000,'trace subscription');
  const received='team/'+[sub.server,sub.owner,sub.chat,sub.generation].join('/');
  const owner=await pageFor(ownerSeat,mode,'trace-owner-'+mode),reader=await pageFor(readerSeat,mode,'trace-reader-'+mode);
  await open(owner,chat.id);await open(reader,received);
  await reader.getByText('Synthetic response 1',{exact:true}).waitFor({timeout:30000});
  await owner.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});
  assertID('trace-normal-panel-'+mode,await reader.locator('#chat').isVisible()&&await reader.locator('#prompt').isVisible()&&await reader.locator('#prompt').count()===1&&await reader.locator('#sharedview').count()===0);
  assertID('trace-people-next-auto-'+mode,await owner.evaluate(()=>document.querySelector('#autoapprove').nextElementSibling?.id==='destination'));
  assertID('trace-recipient-people-only-'+mode,!(await reader.locator('#destinationagent').isVisible()));
  const before=Object.fromEntries(seats.map(s=>[s.name,counts(s)]));
  const agentBubble=owner.locator('#msgs .bubble').filter({hasText:'Synthetic response 1'}).first();await agentBubble.click();
  assertID('trace-ai-prompt-icon-rotated-'+mode,await owner.locator('[data-bact=prompt] .rot90r').count()===1&&await owner.locator('[data-bact=desc] .rot90l').count()===1);
  await owner.locator('[data-bact=prompt]').click();assertID('trace-ai-quote-editable-'+mode,(await owner.locator('#destinationagent').getAttribute('aria-pressed'))==='true'&&(await owner.locator('#prompt').innerText()).includes('Synthetic response 1'));
  for(const s of seats)assert.deepEqual(counts(s),before[s.name]);
  await owner.locator('#prompt').fill('');await owner.locator('#destinationpeople').click();
  await reader.locator('#msgs .bubble').filter({hasText:'Synthetic response 1'}).first().click();
  assertID('trace-recipient-ai-no-prompt-'+mode,await reader.locator('[data-gact=prompt]').count()===0);
  const discussion='Synthetic trace human '+mode;
  await reader.locator('#prompt').fill(discussion);await reader.locator('#sendbtn').click();
  await owner.getByText(discussion,{exact:true}).waitFor({timeout:40000});
  assertID('trace-original-once-'+mode,(await owner.getByText(discussion,{exact:true}).count())===1&&(await owner.locator('#chattitle').innerText())==='trace '+mode);
  assertID('trace-agent-turn-alongside-'+mode,await owner.getByText('Synthetic response 1',{exact:true}).count()===1);
  const log=fs.readFileSync(path.join(ownerSeat.data,'sessions',chat.id+'.jsonl'),'utf8');
  assertID('trace-human-not-agent-log-'+mode,!log.includes(discussion));
  for(const s of seats)assert.deepEqual(counts(s),before[s.name]);
  const readerSource=reader.locator('#msgs .bubble').filter({hasText:discussion}).first();await readerSource.click();await reader.locator('[data-gact=bookmark]').click();
  const bookmark=await until(async()=>{const u=await readerSeat.api('/api/ui');return (u.bookmarks||[]).find(x=>x.text===discussion&&x.team?.chat===sub.chat&&x.team?.generation===sub.generation);},20000,'received bookmark saved');
  assertID('trace-bookmark-full-address-'+mode,bookmark.team.server===sub.server&&bookmark.team.owner===sub.owner&&bookmark.team.log==='comments'&&bookmark.team.seq>0&&bookmark.role==='user');
  const ownerComment='Owner human for rename '+mode;await owner.locator('#prompt').fill(ownerComment);await owner.locator('#sendbtn').click();
  await owner.getByText(ownerComment,{exact:true}).waitFor();await owner.locator('#msgs .gwho').filter({hasText:'You · People'}).first().waitFor({timeout:40000});assertID('trace-owner-people-provenance-'+mode,true);
  const otherPerson=reader.locator('#msgs .bubble').filter({hasText:ownerComment}).first();await otherPerson.waitFor({timeout:40000});await otherPerson.click();await reader.locator('[data-gact=rename]').click();await reader.locator('[data-grename]').fill('Synthetic renamed person');await reader.locator('[data-gact=name-save]').click();
  assertID('trace-rename-person-'+mode,await reader.getByText('Synthetic renamed person',{exact:false}).count()>0);
  await readerSource.click();await reader.locator('[data-gact=sendto]').click();await reader.locator('#sendto_list [data-fwd="'+readerSeat.node.id+'"]').click();
  assertID('trace-sendto-own-agent-draft-'+mode,(await reader.locator('#chattitle').innerText())===readerSeat.name+' chat'&&(await reader.locator('#prompt').innerText()).includes(discussion));
  for(const s of seats)assert.deepEqual(counts(s),before[s.name]);
  await reader.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});await reader.locator('#prompt').fill('');if(mode==='phone')await reader.locator('#chatback').click();await open(reader,received);
  const peopleDraft='Editing people during new data '+mode;await owner.locator('#prompt').fill(peopleDraft);
  await owner.locator('#prompt').evaluate(el=>{window.__tracePrompt=el;el.focus();const range=document.createRange();range.setStart(el.firstChild,5);range.collapse(true);const selection=window.getSelection();selection.removeAllRanges();selection.addRange(range);});
  const incoming='Incoming while editing '+mode;await reader.locator('#prompt').fill(incoming);await reader.locator('#sendbtn').click();
  await owner.getByText(incoming,{exact:true}).waitFor({timeout:40000});
  assertID('trace-new-comment-keeps-focus-caret-'+mode,await owner.locator('#prompt').evaluate(el=>el===window.__tracePrompt&&document.activeElement===el&&el.innerText.startsWith('Editing people')&&window.getSelection().anchorNode===el.firstChild&&window.getSelection().anchorOffset===5));
  assertID('trace-people-labels-'+mode,(await owner.locator('#prompt').getAttribute('data-placeholder'))==='Message people…'&&(await owner.locator('#sendbtn').getAttribute('aria-label'))==='Send message to people');
  await owner.locator('#prompt').fill('');
  await owner.locator('#destinationagent').click();await owner.locator('#prompt').fill('Unrelated agent draft '+mode);await owner.locator('#destinationpeople').click();
  const source=owner.locator('#msgs .bubble').filter({hasText:discussion}).first();
  await source.click();await owner.locator('[data-gact=prompt]').click();
  assertID('trace-prepare-selects-agent-'+mode,(await owner.locator('#destinationagent').getAttribute('aria-pressed'))==='true'&&(await owner.locator('#prompt').innerText()).includes(discussion)&&(await owner.locator('#prompt').innerText()).includes('Unrelated agent draft '+mode));
  assertID('trace-agent-send-label-'+mode,(await owner.locator('#sendbtn').getAttribute('aria-label'))==='Send message to agent');
  await owner.locator('[aria-label="Prepared as prompt"]').waitFor();
  for(const s of seats)assert.deepEqual(counts(s),before[s.name]);
  await owner.locator('#prompt').fill('Partial trace edit '+mode);
  assertID('trace-partial-retains-marker-'+mode,await owner.locator('[aria-label="Prepared as prompt"]').count()===1);
  await owner.reload({waitUntil:'networkidle'});await owner.locator('[aria-label="Prepared as prompt"]').waitFor({timeout:30000});
  assertID('trace-reload-people-default-'+mode,(await owner.locator('#destinationpeople').getAttribute('aria-pressed'))==='true');
  await owner.locator('#destinationagent').click();assertID('trace-reload-draft-'+mode,(await owner.locator('#prompt').innerText())==='Partial trace edit '+mode);
  await owner.locator('#prompt').fill('');await owner.waitForFunction(()=>!document.querySelector('[aria-label="Prepared as prompt"]'));
  await owner.locator('#prompt').fill('Unrelated later text');assertID('trace-erased-association-'+mode,await owner.locator('[aria-label="Prepared as prompt"]').count()===0);
  await owner.locator('#prompt').fill('');await owner.locator('#destinationpeople').click();await owner.locator('#prompt').fill('Independent people draft '+mode);
  await source.click();await owner.locator('[data-gact=prompt]').click();
  const edited='Edited prompt α '+mode;await owner.locator('#prompt').fill(edited);
  const beforeSend=counts(ownerSeat);await owner.locator('#sendbtn').click();
  await owner.locator('[aria-label="Sent as prompt"]').waitFor({timeout:30000});
  const afterSend=counts(ownerSeat);assertID('trace-owner-agent-provenance-'+mode,await owner.getByText('You · Agent',{exact:true}).count()>0);
  assertID('trace-explicit-one-dispatch-'+mode,afterSend.attempted===beforeSend.attempted+1&&afterSend.accepted===beforeSend.accepted+1&&afterSend.controls===beforeSend.controls);
  const link=await until(async()=>{const ui=await ownerSeat.api('/api/ui');return (ui.forward_links||[]).find(x=>x.kind==='prompt'&&x.destination?.node===chat.id);},20000,'confirmed source link persisted');
  const local=await ownerSeat.api('/api/nodes/'+chat.id+'/chat');
  assert.ok(link,'confirmed source link');
  const turn=local.turns.find(x=>x.uid===link.destination.uid&&(x.segment??0)===(link.destination.segment??0)&&x.record===link.destination.record);
  assertID('trace-confirmed-exact-address-'+mode,!!turn&&turn.text===edited&&local.turns.some((x,i)=>i>local.turns.indexOf(turn)&&x.role==='assistant'));
  manifest['trace-address-'+mode]={source:link.source,destination:link.destination,preview:link.preview,beforeSend,afterSend};
  await owner.reload({waitUntil:'networkidle'});await owner.locator('[aria-label="Sent as prompt"]').waitFor();
  await owner.locator('#destinationagent').click();await owner.locator('#prompt').fill('temporary');await owner.locator('#prompt').fill('');
  assertID('trace-confirmed-survives-empty-'+mode,await owner.locator('[aria-label="Sent as prompt"]').count()===1);
  await owner.locator('#destinationpeople').click();assertID('trace-people-draft-independent-'+mode,(await owner.locator('#prompt').innerText())==='Independent people draft '+mode);
  await owner.locator('#msgs .bubble').filter({hasText:'Synthetic response 1'}).first().click();await owner.locator('[data-bact=prompt]').click();
  assertID('trace-ai-after-reload-'+mode,(await owner.locator('#prompt').innerText()).includes('Synthetic response 1'));
  await owner.locator('#prompt').fill('');await owner.locator('#destinationpeople').click();
  const mirror=await readerSeat.api('/api/team/subscriptions/'+received.slice(5)+'?after=0');
  for(let i=0;i<14;i++)await readerSeat.api('/api/team/subscriptions/'+received.slice(5)+'/comments','POST',{text:'Trace filler '+mode+' '+i+': '+('synthetic scrolling context '.repeat(45)),anchor:mirror.next,key:(mode==='desktop'?'feed1000':'feed2000')+i.toString(16).padStart(8,'0')});
  await owner.getByText('Trace filler '+mode+' 13:',{exact:false}).waitFor({timeout:40000});
  await source.click();await owner.locator('[data-gact=prompt]').click();
  const editedAgain='Second meaningful prompt 🧪 漢 '+mode+' <&" 漢 '.repeat(24);await owner.locator('#prompt').fill(editedAgain);
  const beforeSecond=counts(ownerSeat);await owner.locator('#sendbtn').click();
  await until(async()=>{const u=await ownerSeat.api('/api/ui');return (u.forward_links||[]).filter(x=>x.kind==='prompt'&&x.destination?.node===chat.id).length===2;},30000,'second exact prompt link');
  const secondCounts=counts(ownerSeat);assertID('trace-second-explicit-dispatch-'+mode,secondCounts.attempted===beforeSecond.attempted+1&&secondCounts.accepted===beforeSecond.accepted+1&&secondCounts.controls===beforeSecond.controls);
  const links=(await ownerSeat.api('/api/ui')).forward_links.filter(x=>x.kind==='prompt'&&x.destination?.node===chat.id);
  assertID('trace-two-distinct-addresses-'+mode,links.length===2&&links[0].destination.record!==links[1].destination.record&&links.every(x=>x.source.seq===link.source.seq));
  const marker=owner.locator('[aria-label="Sent as prompt"]').first();await marker.waitFor();await marker.focus();await marker.press('Enter');
  const choices=owner.locator('#sendto_list [data-choice]');await choices.first().waitFor();
  const labels=await choices.allInnerTexts(),preview=Array.from(editedAgain.replace(/\s+/g,' ').trim()).slice(0,100).join('');
  assertID('trace-chooser-meaningful-unicode-'+mode,labels.length===2&&labels[0].includes(edited)&&labels[1].includes(preview)&&await owner.locator('#sendto_list [data-choice] img').count()===0);
  await delay(600);assertID('trace-chooser-visible-'+mode,await choices.first().evaluate(el=>{const r=el.getBoundingClientRect();return r.top>=0&&r.bottom<=innerHeight;}));
  await screenshot(owner,'trace-chooser-'+mode+'.png');await choices.first().press('Escape');
  assertID('trace-chooser-escape-focus-'+mode,await marker.evaluate(el=>document.activeElement===el));
  await owner.locator('#destinationpeople').click();await owner.locator('#prompt').fill('Independent people draft '+mode);
  await owner.locator('#msgs .gmsg .bubble').last().click();await owner.locator('#msgs [data-gact=rename]').click();
  const editor=owner.locator('#msgs [data-grename]');await editor.fill('In progress person edit');await editor.evaluate(el=>{el.setSelectionRange(4,9);window.__traceEditor=el;});
  const editorAnchor=await editor.evaluate(el=>el.getBoundingClientRect().top);
  await readerSeat.api('/api/team/subscriptions/'+received.slice(5)+'/comments','POST',{text:'Poll while editing '+mode,anchor:mirror.next,key:(mode==='desktop'?'fa110000':'fa220000')+'00000001'});
  await owner.getByText('Poll while editing '+mode,{exact:true}).waitFor({timeout:30000});
  const latePoll=owner.waitForResponse(r=>new URL(r.url()).pathname==='/api/nodes/'+chat.id+'/chat'&&[200,304].includes(r.status()),{timeout:20000});
  await ownerSeat.api('/api/nodes/'+chat.id,'PATCH',{title:'Synthetic concurrent title '+mode});
  const late=await latePoll;await delay(250);
  const editorPoll=await editor.evaluate((el,top)=>({same:el===window.__traceEditor,focused:document.activeElement===el,value:el.value,start:el.selectionStart,end:el.selectionEnd,top:el.getBoundingClientRect().top,previousTop:top}),editorAnchor);manifest['trace-editor-poll-'+mode]=editorPoll;
  editorPoll.status=late.status();
  assertID('trace-editor-focused-during-poll-'+mode,editorPoll.same&&editorPoll.focused&&editorPoll.value==='In progress person edit'&&editorPoll.start===4&&editorPoll.end===9&&Math.abs(editorPoll.top-editorPoll.previousTop)<2);
  await marker.scrollIntoViewIfNeeded();
  const current=await ownerSeat.api('/api/nodes/'+chat.id+'/chat');const targetLink=links[1];
  const index=current.turns.findIndex(x=>x.uid===targetLink.destination.uid&&(x.segment??0)===(targetLink.destination.segment??0)&&x.record===targetLink.destination.record);
  assert.ok(index>=0,'exact target found by address');const target=owner.locator('#msgs .turn[data-bk="i:'+index+'"]');
  const prior=await target.boundingBox(),viewport=owner.viewportSize();
  assertID('trace-jump-target-offscreen-'+mode,!!prior&&(prior.y>=viewport.height||prior.y+prior.height<=0));
  await marker.click();await choices.nth(1).waitFor();await choices.nth(1).focus();await choices.nth(1).press('Enter');
  await owner.waitForFunction(index=>document.querySelector('#msgs .turn[data-bk="i:'+index+'"]')?.classList.contains('jumpflash'),index,{timeout:10000});
  const afterRect=await target.boundingBox();assertID('trace-jump-exact-onscreen-'+mode,!!afterRect&&afterRect.y<viewport.height&&afterRect.y+afterRect.height>0);
  assertID('trace-jump-preserves-editor-'+mode,await editor.evaluate(el=>el===window.__traceEditor&&el.value==='In progress person edit'&&el.selectionStart===4&&el.selectionEnd===9));
  assertID('trace-jump-preserves-people-draft-'+mode,(await owner.locator('#prompt').innerText())==='Independent people draft '+mode);
  manifest['trace-choices-'+mode]={links:links.map(x=>({source:x.source,destination:x.destination,preview:x.preview})),labels,prior,after:afterRect,beforeSecond,secondCounts};
  await screenshot(owner,'trace-editor-jump-'+mode+'.png');
  await screenshot(owner,'trace-owner-'+mode+'.png');await screenshot(reader,'trace-reader-'+mode+'.png');
  await owner.locator('#destinationagent').click();await owner.locator('#attfile').setInputFiles({name:'synthetic-note.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic attachment')});
  await owner.locator('#attstage .stagechip.done').waitFor({timeout:30000});await owner.locator('#destinationpeople').click();
  assertID('trace-agent-attachment-hidden-people-'+mode,!(await owner.locator('#attstage').isVisible()));await owner.locator('#destinationagent').click();
  assertID('trace-agent-attachment-retained-toggle-'+mode,(await owner.locator('#attstage').innerText()).includes('synthetic-note.txt'));
  await owner.locator('#attstage .sx').click();
  await owner.emulateMedia({reducedMotion:'reduce'});
  let releaseUpload,uploadReady,releasedUpload=false;const uploadHeld=new Promise(resolve=>releaseUpload=resolve),uploadRequested=new Promise(resolve=>uploadReady=resolve);heldReleases.add(releaseUpload);
  await owner.route('**/api/nodes/'+chat.id+'/attachments',async route=>{try{const response=await route.fetch();uploadReady();await uploadHeld;if(!owner.isClosed())await route.fulfill({response});}catch(e){if(!releasedUpload&&!shutting&&!owner.isClosed())throw e;}});
  try {
    await owner.locator('#attfile').setInputFiles({name:'held-synthetic.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic held upload')});
    await Promise.race([uploadRequested,delay(20000).then(()=>{throw Error('upload not requested');})]);
    const spinner=owner.locator('#attstage .stagechip.up .spin i');await spinner.waitFor({timeout:10000});
    const animation=await spinner.evaluate(el=>({name:getComputedStyle(el).animationName,state:getComputedStyle(el).animationPlayState,first:getComputedStyle(el).transform}));
    await delay(250);animation.second=await spinner.evaluate(el=>getComputedStyle(el).transform);
    assertID('trace-held-upload-reduced-motion-'+mode,animation.name!=='none'&&animation.state==='running'&&animation.first!==animation.second);
    manifest['held-upload-animation-'+mode]=animation;await screenshot(owner,'held-upload-'+mode+'.png');
    await owner.locator('#destinationpeople').click();await owner.locator('#prompt').fill('People during upload '+mode);
    await owner.locator('#destinationagent').click();await owner.locator('#prompt').fill('New agent edit during upload '+mode);
    releasedUpload=true;releaseUpload();heldReleases.delete(releaseUpload);await owner.locator('#attstage .stagechip.done').waitFor({timeout:30000});
    assertID('trace-held-upload-newer-agent-draft-'+mode,(await owner.locator('#prompt').innerText())==='New agent edit during upload '+mode&&(await owner.locator('#attstage').innerText()).includes('held-synthetic.txt'));
    await owner.locator('#destinationpeople').click();assertID('trace-held-upload-people-draft-'+mode,(await owner.locator('#prompt').innerText())==='People during upload '+mode);
    await owner.locator('#destinationagent').click();await owner.locator('#attstage .sx').click();await owner.locator('#prompt').fill('');
  } finally {releasedUpload=true;releaseUpload();heldReleases.delete(releaseUpload);await owner.unrouteAll({behavior:'wait'});await owner.emulateMedia({reducedMotion:'no-preference'});}
  await owner.route('**/api/nodes/'+chat.id+'/attachments',route=>route.abort('failed'));
  try {
    await owner.locator('#prompt').fill('Agent draft after failed upload '+mode);
    await owner.locator('#attfile').setInputFiles({name:'failed-synthetic.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic failed upload')});
    await owner.locator('#attstage .stagechip.err').waitFor({timeout:30000});await owner.locator('#destinationpeople').click();
    assertID('trace-failed-upload-people-draft-'+mode,(await owner.locator('#prompt').innerText())==='People during upload '+mode);
    await owner.locator('#destinationagent').click();
    assertID('trace-failed-upload-agent-draft-'+mode,(await owner.locator('#prompt').innerText())==='Agent draft after failed upload '+mode&&(await owner.locator('#attstage').innerText()).includes('failed-synthetic.txt'));
    await owner.locator('#attstage .sx').click();await owner.locator('#prompt').fill('');
  } finally {await owner.unrouteAll({behavior:'wait'});}
  await owner.locator('#destinationagent').click();await owner.locator('#prompt').fill('/clear');await owner.locator('#sendbtn').click();
  await owner.locator('.histload').waitFor({timeout:30000});await owner.locator('.histload').click();
  await owner.getByText(discussion,{exact:true}).waitFor({timeout:30000});
  const history=await ownerSeat.api('/api/nodes/'+chat.id+'/chat?history=1');
  assertID('trace-clear-history-seam-'+mode,(history.segments.length+(history.empty_segments||[]).length)>=2&&(await owner.getByText(discussion,{exact:true}).count())===1);
  manifest['trace-history-'+mode]={segments:history.segments.map(x=>({uid:x.log_uid,index:x.segment_index,seam:x.seam,reason:x.reason}))};await screenshot(owner,'trace-history-'+mode+'.png');
}
async function nativeTrace(mode,ownerSeat,readerSeat) {
  const native=await node(ownerSeat,'claude','native trace '+mode,'Synthetic native initial');
  await ownerSeat.api('/api/team/shares/'+native.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+native.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid&&x.generation===share.generation),30000,'native trace subscription');
  const received='team/'+[sub.server,sub.owner,sub.chat,sub.generation].join('/');
  const owner=await pageFor(ownerSeat,mode,'native-trace-owner-'+mode),reader=await pageFor(readerSeat,mode,'native-trace-reader-'+mode);
  await open(owner,native.id);await open(reader,received);await reader.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
  await reader.getByText('Synthetic response 1',{exact:true}).waitFor({timeout:30000});
  const comment='Native source α '+mode+'\nSecond line 漢';await reader.locator('#prompt').fill(comment);await reader.locator('#sendbtn').click();
  const source=owner.locator('#msgs .bubble').filter({hasText:'Native source α '+mode}).first();await source.waitFor({timeout:40000});
  const before=counts(ownerSeat);await source.click();await owner.locator('[data-gact=prompt]').click();
  const quote=await owner.locator('#prompt').innerText();assertID('native-trace-attributed-multiline-'+mode,quote.includes(comment)&&quote.includes('recipient'));
  assert.deepEqual(counts(ownerSeat),before);const edited=quote+'\nEdited suffix Ω '+mode;await owner.locator('#prompt').fill(edited);const submitted=(await owner.locator('#prompt').innerText()).replace(/\u00a0/g,' ').trim();await owner.locator('#sendbtn').click();
  await owner.locator('[aria-label="Sent as prompt"]').waitFor({timeout:30000});
  const after=counts(ownerSeat),attempts=jsonLines(path.join(ownerSeat.data,'counter.attempts')).filter(x=>x.text===submitted),accepted=jsonLines(path.join(ownerSeat.data,'counter')).filter(x=>x.text===submitted);
  manifest['native-trace-debug-'+mode]={edited,submitted,before,after,attempts,accepted};
  assertID('native-trace-one-exact-multiline-'+mode,after.attempted===before.attempted+1&&after.accepted===before.accepted+1&&after.controls===before.controls&&attempts.length===1&&accepted.length===1);
  const link=await until(async()=>{const ui=await ownerSeat.api('/api/ui');return (ui.forward_links||[]).find(x=>x.kind==='prompt'&&x.destination?.node===native.id);},30000,'persisted native prompt link');
  const evidence=await until(async()=>{const chat=await ownerSeat.api('/api/nodes/'+native.id+'/chat?history=1');const turns=chat.segments.flatMap(x=>x.turns||[]);const turn=turns.find(x=>x.uid===link.destination.uid&&(x.segment??0)===(link.destination.segment??0)&&x.record===link.destination.record);return turn&&turn.text===submitted&&turns.some((x,i)=>i>turns.indexOf(turn)&&x.role==='assistant')?{turn}:null;},30000,'native addressed prompt and assistant');
  assertID('native-trace-confirmed-address-'+mode,!!evidence);

  manifest['native-trace-'+mode]={source:link.source,destination:link.destination,before,after,submitted};await screenshot(owner,'native-trace-'+mode+'.png');
}
async function refusalTrace(mode,ownerSeat,readerSeat,newer) {
  const chat=await node(ownerSeat,'codex','refusal '+mode+' '+newer,'Synthetic refusal initial');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+chat.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid&&x.generation===share.generation),30000,'refusal subscription');
  const received='team/'+[sub.server,sub.owner,sub.chat,sub.generation].join('/');
  const owner=await pageFor(ownerSeat,mode,'refusal-owner-'+mode+'-'+newer),reader=await pageFor(readerSeat,mode,'refusal-reader-'+mode+'-'+newer);
  await open(owner,chat.id);await open(reader,received);await reader.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
  const comment='Refusal source '+mode+' '+newer;await reader.locator('#prompt').fill(comment);await reader.locator('#sendbtn').click();
  const source=owner.locator('#msgs .bubble').filter({hasText:comment}).first();await source.waitFor({timeout:40000});
  await source.click();await owner.locator('[data-gact=prompt]').click();
  const rejected='Refused '+mode+' '+newer+' [delay-dispatch] [reject-dispatch]';await owner.locator('#prompt').fill(rejected);
  const observed=[];manifest['refusal-responses-'+mode+'-'+newer]=observed;owner.on('response',async r=>{if(r.url().endsWith('/api/nodes/'+chat.id+'/chat')&&r.status()===200){try{const j=await r.json();observed.push({error:j.error,restore_draft:j.restore_draft,trace_outcome:j.trace_outcome,pending:j.pending});}catch{}}});
  const restored=owner.waitForResponse(async r=>{if(!r.url().endsWith('/api/nodes/'+chat.id+'/chat')||r.status()!==200)return false;try{return Boolean((await r.json()).error);}catch{return false;}},{timeout:30000});
  const before=counts(ownerSeat);await owner.locator('#sendbtn').click();await owner.locator('[aria-label="Awaiting prompt delivery"]').waitFor({timeout:10000});
  assertID('refusal-awaiting-not-sent-'+mode+'-'+newer,await owner.locator('[aria-label="Sent as prompt"]').count()===0);
  await owner.locator('#destinationpeople').click();const people='People editing '+mode+' '+newer;await owner.locator('#prompt').fill(people);
  if(newer){await owner.locator('#destinationagent').click();await owner.locator('#prompt').fill('Newer agent draft '+mode);await owner.locator('#destinationpeople').click();}
  const restoredResponse=await restored;const restoredBody=await restoredResponse.json();await delay(200);assertID('refusal-people-preserved-'+mode+'-'+newer,(await owner.locator('#prompt').innerText())===people);
  assertID('refusal-never-sent-'+mode+'-'+newer,await owner.locator('[aria-label="Sent as prompt"]').count()===0);
  await owner.locator('#destinationagent').click();assertID('refusal-agent-recovery-'+mode+'-'+newer,(await owner.locator('#prompt').innerText()).includes(newer?'Newer agent draft '+mode:rejected));
  const after=counts(ownerSeat);assertID('refusal-no-retry-'+mode+'-'+newer,after.attempted===before.attempted+1&&after.accepted===before.accepted&&after.controls===before.controls);
  manifest['refusal-'+mode+'-'+newer]={before,after,errorGET:restoredBody.error,restoreDraftGET:restoredBody.restore_draft||null};await screenshot(owner,'refusal-'+mode+'-'+newer+'.png');
}
async function captureCoverage() {
  const result=[];const sourceDir=path.resolve(__dirname,'../js');
  for (const {page,label} of pages) {
    if(page.isClosed()) continue;
    try {for(const entry of await page.coverage.stopJSCoverage()) {
      const url=new URL(entry.url);if(!/^\/js\/[a-z0-9-]+\.js$/.test(url.pathname))continue;
      const source=fs.readFileSync(path.join(sourceDir,path.basename(url.pathname)),'utf8');
      assert.equal(entry.source,source,'browser source matches checkout: '+url.pathname);
      result.push({page:label,path:url.pathname,source,functions:entry.functions});
    }} catch(e){manifest.coverageError=String(e);throw e;}
  }
  fs.writeFileSync(artifacts('browser-coverage.json'),JSON.stringify(result));manifest.artifacts.push('browser-coverage.json');
}
async function terminate(c) {
  if(!c || c.exitCode!==null || c.signalCode!==null)return;
  c.kill('SIGTERM');await Promise.race([new Promise(r=>c.once('exit',r)),delay(4000)]);
  if(c.exitCode===null && c.signalCode===null){c.kill('SIGKILL');await Promise.race([new Promise(r=>c.once('exit',r)),delay(2000)]);}
}
async function cleanup() {
  if(shutting)return;shutting=true;
  for(const release of heldReleases)release();
  if(browser){try{await captureCoverage();}catch(e){manifest.cleanup.push({coverage:'failed',error:String(e)});manifest.result='fail';process.exitCode=1;}try{await browser.close();}catch{}}
  await Promise.allSettled([...pending]);
  for(const s of seats) {
    if(s.api){try {
      // Enumerate owned data, including a node whose create response raced shutdown.
      const state=await s.api('/api/state','GET',undefined,true);
      for(const n of state.nodes || []) {await s.api('/api/nodes/'+encodeURIComponent(n.id),'DELETE',undefined,true);manifest.cleanup.push({seat:s.name,deleted:true});}
    }catch(e){manifest.cleanup.push({seat:s.name,deleteError:String(e)});manifest.result='fail';process.exitCode=1;}}
  }
  for(const c of children.slice().reverse()) await terminate(c);
  const stopped=children.every(c=>c.exitCode!==null||c.signalCode!==null);
  manifest.cleanup.push({childrenStopped:stopped});
  if(!stopped){manifest.result='fail';process.exitCode=1;}
  for(const s of seats){
    if(s.socketPath && s.socketPath.startsWith(s.env.TMUX_TMPDIR+'/tmux-'+process.getuid()+'/')) {
      const outcome=await retire(s.socketPath,{
        exists:fs.existsSync,
        kill:async socket=>execFileSync('tmux',['-S',socket,'kill-server'],{env:s.env,stdio:'ignore',timeout:5000}),
        probe:socket=>new Promise(resolve=>{const conn=net.createConnection(socket);conn.setTimeout(1000);conn.once('connect',()=>{conn.destroy();resolve('live');});conn.once('timeout',()=>{conn.destroy();resolve('unknown');});conn.once('error',e=>resolve(e.code==='ECONNREFUSED'||e.code==='ENOENT'?'dead':'unknown'));}),
        unlink:fs.unlinkSync
      });
      manifest.cleanup.push({seat:s.name,socketRemoved:outcome.removed,...(outcome.error?{socketError:outcome.error}:{}),...(outcome.killError?{killError:outcome.killError}:{})});
      if(!outcome.removed){manifest.result='fail';process.exitCode=1;}
    } else {manifest.cleanup.push({seat:s.name,socketError:'invalid owned socket path'});manifest.result='fail';process.exitCode=1;}
    const dir=path.join(s.data,'control','workers');
    const remaining=fs.existsSync(dir)?fs.readdirSync(dir).filter(n=>n.endsWith('.json')):[];
    manifest.cleanup.push({seat:s.name,remainingWorkerLocators:remaining.length});
    if(remaining.length){manifest.result='fail';process.exitCode=1;}
    const pidsPath=path.join(s.data,'counter.native-pids');
    const pids=fs.existsSync(pidsPath)?fs.readFileSync(pidsPath,'utf8').trim().split('\n').filter(Boolean).map(Number):[];
    const alive=pid=>{try{process.kill(pid,0);return true;}catch(e){return e.code!=='ESRCH';}};
    for(let i=0;i<50 && pids.some(alive);i++)await delay(100);
    const remainingNativeHelpers=pids.filter(alive).length;
    manifest.cleanup.push({seat:s.name,remainingNativeHelpers});
    if(remainingNativeHelpers){manifest.result='fail';process.exitCode=1;}
  }
  record();
}
for(const [sig,code] of [['SIGINT',130],['SIGTERM',143]])process.once(sig,()=>{signalCode=code;manifest.result='interrupted';manifest.signal=sig;cleanup().then(()=>{console.log('Artifacts: '+root);process.exit(code);}).catch(e=>{console.error(e);process.exit(code);});});

async function run() {
 const selected=process.env.SCIMUX_E2E_SCENARIO||'default';
 if(!['default','stale-membership','stale-removed-membership','conversation-trace','conversation-native','conversation-refusal','conversation-core','early-native','native-start-failure','recipient-boundary','ai-source','ui-join','recipient-leave','permission-lifecycle','leave-empty','leave-stale','held-evidence','viewport-keyboard','dropped-send','reshare-stale','media-contrast','unshared','held-chat-loading','received-offline'].includes(selected))throw Error('unknown SCIMUX_E2E_SCENARIO');
 if(selected!=='default')manifest.scope=selected+' scenario only';
 relay=await launchRelay();
 for(const name of ['owner-a','owner-b','recipient','observer']) await makeSeat(name);
 const [a,b,r,observer]=seats;
 for(const s of seats)s.node=await node(s,'codex',s.name+' chat','Synthetic initial request');
 if(selected==='native-start-failure'){
  browser=await playwright.chromium.launch();
  for(const mode of ['desktop','phone']){const before=counts(a);await nativeStartupFailure(mode,a);assert.deepEqual(counts(a),before);assertID('native-start-failure-zero-dispatch-'+mode,true);}return;
 }
 if(process.env.SCIMUX_E2E_FAIL_AFTER_START==='1')throw Error('controlled post-start failure');
 if(process.env.SCIMUX_E2E_CLEANUP_PROBE==='failure'||process.env.SCIMUX_E2E_CLEANUP_PROBE==='signal') {
  manifest.scope='cleanup probe only';
  await node(a,'claude','native cleanup probe','Synthetic native initial');
  if(process.env.SCIMUX_E2E_CLEANUP_PROBE==='failure')throw Error('controlled native cleanup failure');
  console.log('Native cleanup probe ready');
  await new Promise(()=>{});
 }
 if(shutting)throw Error('runner shutting down');
 browser=await playwright.chromium.launch();
 // Selected chat remains selected as Team becomes available and through reload.
 if(selected==='default'||selected==='ui-join') for(const mode of ['desktop','phone']) {
  const s=mode==='desktop'?a:b;
  const p=await pageFor(s,mode,'activation-'+mode);await open(p,s.node.id);
  await p.waitForFunction(()=>document.querySelector('#msgs').innerText.includes('Synthetic response'));
  assertID('team-off-icon-hidden-'+mode,!(await p.locator('#sharebtn').isVisible()));
  await s.api('/api/settings','PUT',{experimental_feature_team:true});
  const issue=JSON.parse(execFileSync(relayBin,['-state-dir',relay.dir,'-url',relay.url,'-json','issue'],{env:baseEnv,encoding:'utf8'}));
  await p.locator('#burger').click();await p.locator('#m_sharing summary').click();await p.locator('#m_team_add').click();
  await p.locator('#m_team_link').fill(issue.link);await p.locator('#m_team_name').fill('Same sharer');
  const joined=p.waitForResponse(resp=>resp.request().method()==='POST'&&resp.url().endsWith('/api/team/servers')&&resp.status()===201,{timeout:30000});
  await p.locator('#m_team_join').click();await joined;await p.locator('#m_team_form').waitFor({state:'hidden',timeout:30000});
  await p.locator('#m_team_link').evaluate(el=>{el.value='';});await p.locator('#backdrop').click({position:{x:5,y:5}});
  await p.locator('#sharebtn').waitFor({state:'visible',timeout:30000});
  assertID('selected-ui-team-join-'+mode,(await p.locator('#chattitle').innerText())===s.name+' chat');
  assertID('selected-'+mode,(await p.locator('#chattitle').innerText())===s.name+' chat');
  await p.locator('#sharebtn').click({trial:true});await p.reload({waitUntil:'networkidle'});
  await p.locator('#sharebtn').waitFor({state:'visible',timeout:20000});
  assertID('reload-'+mode,(await p.locator('#chattitle').innerText())===s.name+' chat');
  await p.locator('#prompt').click({trial:true});await screenshot(p,'activation-'+mode+'.png');
 }
 if(selected==='ui-join')return;
 for(const s of selected==='default'?[r,observer]:seats){await s.api('/api/settings','PUT',{experimental_feature_team:true});const issue=JSON.parse(execFileSync(relayBin,['-state-dir',relay.dir,'-url',relay.url,'-json','issue'],{env:baseEnv,encoding:'utf8'}));await s.api('/api/team/servers','POST',{link:issue.link,name:selected==='received-offline'&&s===a?'Synthetic sharer':s.name});}
 for(const s of seats){s.serverID=(await s.api('/api/team/servers')).servers[0].id;s.self=(await s.api('/api/team/servers/'+s.serverID+'/seats')).self;}
 for(const s of [a,b])await s.api('/api/team/shares/'+s.node.id,'PUT',{server:s.serverID,scope:'all',members:[{seat:r.self,access:'write'},...(s===b?[{seat:observer.self,access:'write'}]:[])]});
 if(selected==='default'||selected==='stale-membership')for(const mode of ['desktop','phone'])await staleMembership(mode,a,b,r,observer);
 if(selected==='default'||selected==='stale-removed-membership')for(const mode of ['desktop','phone'])await staleRemovedMembership(mode,a,b,r,observer);
 if(selected==='early-native'){for(const mode of ['desktop','phone'])await earlyNativeComment(mode,a,r);return;}
 if(selected==='recipient-boundary'){for(const mode of ['desktop','phone'])await recipientBoundary(mode,a,r);return;}
 if(selected==='ai-source'){for(const mode of ['desktop','phone'])await aiSourcePrompt(mode,a,r);return;}
 if(selected==='recipient-leave'){for(const mode of ['desktop','phone'])await recipientLeave(mode,a,r);return;}
 if(selected==='permission-lifecycle'){for(const mode of ['desktop','phone'])await permissionLifecycle(mode,a,r);return;}
 if(selected==='leave-empty'){for(const mode of ['desktop','phone'])await recipientLeaveEmpty(mode,a);return;}
 if(selected==='leave-stale'){for(const mode of ['desktop','phone'])await recipientLeaveStale(mode,a,r);return;}
 if(selected==='held-evidence'){for(const mode of ['desktop','phone'])await heldEvidence(mode,a,r);return;}
 if(selected==='viewport-keyboard'){for(const mode of ['w320','w390','w768'])await viewportKeyboard(mode,r);return;}
 if(selected==='dropped-send'){for(const mode of ['desktop','phone'])await droppedSend(mode,a,r);return;}
 if(selected==='reshare-stale'){for(const mode of ['desktop','phone'])await reshareStale(mode,a,r);return;}
 if(selected==='media-contrast'){for(const mode of ['desktop','phone'])for(const scheme of ['light','dark'])await mediaContrast(mode,scheme,a,r);return;}
 if(selected==='unshared'){for(const mode of ['desktop','phone'])await unsharedChat(mode,a);return;}
 if(selected==='held-chat-loading'){for(const mode of ['desktop','phone'])await heldChatLoading(mode,a);return;}
 if(selected==='received-offline'){for(const mode of ['desktop','phone'])await receivedOfflineRetry(mode,a,r);return;}
 if(selected==='conversation-trace'){for(const mode of ['desktop','phone']){await conversationTrace(mode,a,r);await nativeTrace(mode,a,r);await refusalTrace(mode,a,r,false);await refusalTrace(mode,a,r,true);}return;}
 if(selected==='conversation-core'){for(const mode of ['desktop','phone'])await conversationTrace(mode,a,r);return;}
 if(selected==='conversation-native'){for(const mode of ['desktop','phone'])await nativeTrace(mode,a,r);return;}
 if(selected==='conversation-refusal'){for(const mode of ['desktop','phone']){await refusalTrace(mode,a,r,false);await refusalTrace(mode,a,r,true);}return;}
 if(selected!=='default')return;
 const subscriptions=await until(async()=>{const list=(await r.api('/api/team/subscriptions')).subscriptions.filter(x=>x.access!=='owner');return list.length===2?list:null;},40000,'two shares');
 assertID('two-sharers-and-recipient-local',subscriptions.length===2 && !!r.node.id);
 const shared=subscriptions.find(x=>x.owner===a.self);assert.ok(shared);
 const received='team/'+[shared.server,shared.owner,shared.chat,shared.generation].join('/');
 const other=subscriptions.find(x=>x.owner===b.self);assert.ok(other);
 const otherID='team/'+[other.server,other.owner,other.chat,other.generation].join('/');
 for(const mode of ['desktop','phone']) {
  const beforeMembership=Object.fromEntries(seats.map(s=>[s.name,counts(s)]));
  const owner=await pageFor(b,mode,'sheet-'+mode),reader=await pageFor(r,mode,'readonly-'+mode);
  await open(owner,b.node.id);await open(reader,otherID);
  await reader.locator('#prompt[contenteditable=true]').waitFor({timeout:20000});
  const draft='Retain permission draft '+mode;await reader.locator('#prompt').fill(draft);
  const ownerRead=owner.waitForResponse(async response=>{
   if(!response.url().endsWith('/api/team/shares/'+b.node.id)||response.status()!==200)return false;
   try{return (await response.json()).members?.some(x=>x.seat===r.self&&x.access==='read');}catch{return false;}
  },{timeout:30000});
  await b.api('/api/team/shares/'+b.node.id,'PUT',{server:b.serverID,scope:'all',members:[{seat:r.self,access:'read'},{seat:observer.self,access:'write'}]});
  await until(async()=>{const list=(await r.api('/api/team/subscriptions')).subscriptions;return list.find(x=>x.chat===other.chat&&x.access==='read');},30000,'read grant');
  await reader.waitForFunction(()=>document.querySelector('#prompt').getAttribute('contenteditable')!=='true',null,{timeout:30000});
  await ownerRead;
  assertID('read-disabled-preserves-draft-'+mode,(await reader.locator('#prompt').innerText())===draft);
  const status=await r.status('/api/team/subscriptions/'+otherID.slice(5)+'/comments',{text:'Denied synthetic comment',anchor:0,key:mode==='desktop'?'abcdef0123456701':'abcdef0123456702'});
  assertID('read-backend-403-'+mode,status===403);
  await owner.locator('#sharebtn').click();
  const picker=owner.locator('#sh_body input[data-seat="'+a.self+'"]');await picker.waitFor({timeout:20000});
  assertID('sheet-no-permission-dropdown-'+mode,(await owner.locator('#sh_body select').count())===0);
  const controls=await owner.locator('#sh_body input[type=radio], #sh_body input[data-seat]').evaluateAll(xs=>xs.map(x=>({width:x.getBoundingClientRect().width,height:x.getBoundingClientRect().height,target:x.closest('label')?.getBoundingClientRect().height})));
  manifest['sheetGeometry-'+mode]=controls;
  assertID('sheet-indicators-targets-'+mode,controls.length>=2 && controls.every(x=>x.width>=12&&x.width<=14&&x.height>=12&&x.height<=14&&x.target>=43.9));
  await owner.waitForFunction(()=>Math.abs(document.querySelector('#sharesheet').getBoundingClientRect().bottom-innerHeight)<2);
  if(mode==='desktop')assertID('sheet-centered-desktop',await owner.locator('#sharesheet').evaluate(el=>Math.abs((el.getBoundingClientRect().left+el.getBoundingClientRect().right)/2-innerWidth/2)<2));
  assertID('sheet-no-overflow-'+mode,await owner.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
  await picker.locator('..').click({trial:true});await screenshot(owner,'share-sheet-'+mode+'.png');
  await picker.check();await owner.locator('#sh_apply').click();
  await until(async()=>{const share=await b.api('/api/team/shares/'+b.node.id);return share.members.some(x=>x.seat===a.self&&x.access==='write');},20000,'saved new write grant');
  const grants=(await b.api('/api/team/shares/'+b.node.id)).members;
  manifest['membershipGrants-'+mode]=grants.map(x=>({seat:x.seat,access:x.access}));
  assertID('old-read-new-write-'+mode,grants.some(x=>x.seat===r.self&&x.access==='read')&&grants.some(x=>x.seat===a.self&&x.access==='write')&&grants.some(x=>x.seat===observer.self&&x.access==='write'));
  await owner.locator('#backdrop').click({position:{x:5,y:5}});
  const invited=await until(async()=>{const list=(await a.api('/api/team/subscriptions')).subscriptions;return list.find(x=>x.chat===other.chat&&x.owner===b.self&&x.access==='write');},30000,'new write subscription');
  const invitedID='team/'+[invited.server,invited.owner,invited.chat,invited.generation].join('/');
  const guest=await pageFor(a,mode,'new-writer-'+mode);await open(guest,invitedID);
  await guest.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
  const human='New writer human '+mode;await guest.locator('#prompt').fill(human);await guest.locator('#sendbtn').click();
  await owner.getByText(human,{exact:true}).waitFor({timeout:40000});
  assertID('new-writer-human-comment-'+mode,(await owner.getByText(human,{exact:true}).count())===1);
  const ownerWrite=owner.waitForResponse(async response=>{if(!response.url().endsWith('/api/team/shares/'+b.node.id)||response.status()!==200)return false;try{return (await response.json()).members?.some(x=>x.seat===r.self&&x.access==='write');}catch{return false;}},{timeout:30000});
  await b.api('/api/team/shares/'+b.node.id,'PUT',{server:b.serverID,scope:'all',members:[{seat:r.self,access:'write'},{seat:a.self,access:'write'},{seat:observer.self,access:'write'}]});
  await reader.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
  await ownerWrite;
  assertID('write-restored-draft-'+mode,(await reader.locator('#prompt').innerText())===draft);
  // Remove the newly added seat through the picker so the next viewport tests
  // a genuinely new grant too; retain the recipient's existing write grant.
  await owner.locator('#sharebtn').click();
  const remove=owner.locator('#sh_body input[data-seat="'+a.self+'"]');await remove.waitFor();
  await remove.uncheck();await owner.locator('#sh_apply').click();
  await until(async()=>!(await b.api('/api/team/shares/'+b.node.id)).members.some(x=>x.seat===a.self),20000,'remove added member');
  const afterMembership=Object.fromEntries(seats.map(s=>[s.name,counts(s)]));
  assert.deepEqual(afterMembership,beforeMembership);manifest.counters['membership-'+mode]={before:beforeMembership,after:afterMembership};
 }
 const unshared=await node(r,'codex','recipient unshared','Synthetic separate local request');
 for(const mode of ['desktop','phone']) {
  const p=await pageFor(r,mode,'new-share-'+mode);await open(p,unshared.id);await p.locator('#sharebtn').click();
  await p.locator('#sh_body input[name=sh_scope]').first().waitFor();
  const scopes=await p.locator('#sh_body input[name=sh_scope]').evaluateAll(xs=>xs.map(x=>({w:x.getBoundingClientRect().width,h:x.getBoundingClientRect().height,target:x.closest('label').getBoundingClientRect().height})));
  assertID('scope-radio-targets-'+mode,scopes.length===2&&scopes.every(x=>x.w>=12&&x.w<=14&&x.h>=12&&x.h<=14&&x.target>=43.9));
  await p.locator('#sh_body input[name=sh_scope]').first().locator('..').click({trial:true});
  await p.waitForFunction(()=>Math.abs(document.querySelector('#sharesheet').getBoundingClientRect().bottom-innerHeight)<2);
  await screenshot(p,'new-share-'+mode+'.png');await p.keyboard.press('Escape');
 }
 await r.api('/api/team/shares/'+r.node.id,'PUT',{server:r.serverID,scope:'all',members:[{seat:a.self,access:'write'}]});
 manifest.counters.afterExtraSetup=Object.fromEntries(seats.map(s=>[s.name,counts(s)]));
 for(const mode of ['desktop','phone']) {
  const beforeGroups=Object.fromEntries(seats.map(s=>[s.name,counts(s)]));
  const p=await pageFor(r,mode,'activities-'+mode);
  const goBack=async()=>{if(mode==='phone'){await delay(600);if(await p.locator('#cards').evaluate(el=>el.getBoundingClientRect().right<=1)){await p.locator('#chatback').click();await p.waitForFunction(()=>document.querySelector('#cards').getBoundingClientRect().left>=-1);}}};
  await goBack();await p.locator('[data-tab=current]').click();
  const groupA='owner/'+shared.server+'/'+shared.owner,groupB='owner/'+other.server+'/'+other.owner;
  manifest['activityIdentities-'+mode]={groupA,groupB,received,otherID,ownShared:r.node.id,ownUnshared:unshared.id};
  await p.locator('[data-activity-group="'+groupA+'"]').waitFor({timeout:30000});
  await p.locator('[data-activity-group="'+groupB+'"]').waitFor({timeout:30000});
  assertID('mine-first-distinct-identities-'+mode,(await p.locator('[data-activity-group]').first().getAttribute('data-activity-group'))==='mine'&&groupA!==groupB);
  const labels=[await p.locator('[data-activity-group="'+groupA+'"] .foldlabel').innerText(),await p.locator('[data-activity-group="'+groupB+'"] .foldlabel').innerText()];
  manifest['activityLabels-'+mode]=labels;
  assertID('same-name-distinct-groups-'+mode,labels[0]===labels[1]&&labels[0].includes('Same sharer'));
  for(const [group,id] of [[groupA,received],[groupB,otherID]])assertID('owner-group-card-'+mode+'-'+(group===groupA?'a':'b'),await p.locator('[data-activity-group="'+group+'"]').evaluate((el,id)=>{for(let row=el.nextElementSibling;row&&!row.hasAttribute('data-activity-group');row=row.nextElementSibling)if(row.querySelector('[data-open="'+id+'"]'))return true;return false;},id));
  assertID('own-shared-no-duplicate-'+mode,(await p.locator('[data-open="'+r.node.id+'"]').count())===1&&(await p.locator('[data-open="'+unshared.id+'"]').count())===1);
  await screenshot(p,'groups-all-'+mode+'.png');
  // Read A, switch to local, then fold A before new content arrives.
  if(await p.locator('[data-activity-group="'+groupA+'"]').getAttribute('aria-expanded')==='false')await p.locator('[data-activity-group="'+groupA+'"]').click();
  await open(p,received);await p.waitForFunction(()=>document.querySelector('#chattitle').innerText==='owner-a chat');
  await p.locator('#prompt[contenteditable=true]').waitFor({timeout:20000});await delay(2500);
  await goBack();await open(p,unshared.id);await goBack();
  await until(async()=>await p.locator('[data-activity-group="'+groupA+'"] [aria-label=unread]').count()===0,15000,'selected shared chat read');
  await p.locator('[data-activity-group="'+groupA+'"]').click();
  assertID('folded-'+mode,(await p.locator('[data-activity-group="'+groupA+'"]').getAttribute('aria-expanded'))==='false');
  assertID('folded-read-before-new-'+mode,await p.locator('[data-activity-group="'+groupA+'"] [aria-label=unread]').count()===0);
  const own=await until(async()=>{const list=(await a.api('/api/team/subscriptions')).subscriptions;return list.find(x=>x.chat===shared.chat&&x.access==='owner');},20000,'owner group subscription');
  const ownKey=[own.server,own.owner,own.chat,own.generation].join('/');
  await a.api('/api/team/subscriptions/'+ownKey+'/comments','POST',{text:'Unread folded '+mode,anchor:0,key:mode==='desktop'?'abcdef0123456711':'abcdef0123456712'});
  await p.locator('[data-activity-group="'+groupA+'"] [aria-label=unread]').waitFor({timeout:40000});
  await delay(2500);assertID('folded-unread-poll-'+mode,await p.locator('[data-activity-group="'+groupA+'"] [aria-label=unread]').count()===1);
  await screenshot(p,'folded-unread-'+mode+'.png');
  await p.reload({waitUntil:'networkidle'});await goBack();
  assertID('fold-reload-'+mode,(await p.locator('[data-activity-group="'+groupA+'"]').getAttribute('aria-expanded'))==='false');
  await p.context().setOffline(true);await delay(3000);await p.context().setOffline(false);await delay(3000);
  assertID('fold-reconnect-'+mode,(await p.locator('[data-activity-group="'+groupA+'"]').getAttribute('aria-expanded'))==='false');
  await p.locator('[data-tab=shared]').click();
  assertID('shared-excludes-own-'+mode,await p.locator('[data-activity-group=mine]').count()===0&&await p.locator('[data-open="'+r.node.id+'"]').count()===0&&await p.locator('[data-activity-group]').count()===2);
  await screenshot(p,'groups-shared-'+mode+'.png');
  await p.locator('[data-activity-group="'+groupA+'"]').click();
  await open(p,received);await p.getByText('Unread folded '+mode,{exact:true}).waitFor({timeout:30000});await delay(2500);await goBack();
  await until(async()=>await p.locator('[data-activity-group="'+groupA+'"] [aria-label=unread]').count()===0,15000,'opened unread marked read');
  assertID('open-marks-unread-read-'+mode,true);
  const pin=async id=>{
   const card=p.locator('.card:has([data-open="'+id+'"])');await card.scrollIntoViewIfNeeded();
   if(mode==='desktop')await card.hover();else {
    const box=await card.boundingBox(),cdp=await p.context().newCDPSession(p);
    await cdp.send('Input.dispatchTouchEvent',{type:'touchStart',touchPoints:[{x:box.x+box.width-20,y:box.y+box.height/2}]});
    await cdp.send('Input.dispatchTouchEvent',{type:'touchMove',touchPoints:[{x:box.x+box.width-120,y:box.y+box.height/2}]});
    await cdp.send('Input.dispatchTouchEvent',{type:'touchEnd',touchPoints:[]});await cdp.detach();
   }
   await p.locator('[data-pin-action="'+id+'"]').click();
  };
  await pin(received);await p.locator('[data-tab=current]').click();await pin(r.node.id);
  const groupContains=async()=>p.locator('[data-activity-group="'+groupA+'"]').evaluate((el,id)=>{for(let row=el.nextElementSibling;row&&!row.hasAttribute('data-activity-group');row=row.nextElementSibling)if(row.querySelector('[data-open="'+id+'"]'))return true;return false;},received);
  assertID('pinned-in-owner-group-all-'+mode,await groupContains());
  await p.locator('[data-tab=shared]').click();assertID('pinned-in-owner-group-shared-'+mode,await groupContains());
  await p.locator('[data-tab=pinned]').click();
  const pinned=await p.locator('.card .go').evaluateAll(xs=>xs.map(x=>x.dataset.open));
  assertID('mixed-pinned-'+mode,JSON.stringify(pinned)===JSON.stringify([r.node.id,received]));
  await screenshot(p,'pinned-'+mode+'.png');
  await p.reload({waitUntil:'networkidle'});await goBack();await p.locator('[data-tab=pinned]').click();
  assertID('pinned-reload-'+mode,(await p.locator('[data-open="'+received+'"]').count())===1);
  for(const tab of ['current','pinned','archived','shared'])for(const key of ['Enter','Space']) {
    const button=p.locator('[data-tab='+tab+']');await button.focus();await button.press(key);
    assertID('tab-keyboard-'+mode+'-'+tab+'-'+key,(await button.getAttribute('aria-pressed'))==='true'&&await button.evaluate(el=>document.activeElement===el));
  }
  await p.locator('[data-tab=current]').click();
  assertID('pinned-stays-owner-group-'+mode,await groupContains());
  await p.locator('[data-tab=pinned]').click();
  await pin(received);await pin(r.node.id);
  await p.locator('[data-tab=current]').click();
  for(const tab of ['current','archived','shared']) {const t=p.locator('[data-tab='+tab+']');await t.focus();await t.click();assertID('tab-reachable-'+mode+'-'+tab,(await t.getAttribute('aria-pressed'))==='true');}
  const selected=p.locator('[data-tab=shared]');await selected.focus();
  const scrollBefore=await p.locator('#cardtabs').evaluate(el=>el.scrollLeft);
  const panelsBefore=await p.evaluate(()=>['#cards','#chat'].map(q=>{const x=document.querySelector(q).getBoundingClientRect();return [x.left,x.top,x.width,x.height];}));
  manifest['tabsGeometry-'+mode]=await p.evaluate(()=>{const strip=document.querySelector('#cardtabs'),tab=strip.querySelector('[data-tab=shared]'),r=x=>{const b=x.getBoundingClientRect();return {left:b.left,right:b.right,top:b.top,bottom:b.bottom};};return {strip:r(strip),tab:r(tab),scroll:strip.scrollLeft,viewport:innerWidth};});
  assertID('selected-tab-visible-in-strip-'+mode,await selected.evaluate(el=>{const a=el.getBoundingClientRect(),b=el.parentElement.getBoundingClientRect();return a.left>=b.left-1&&a.right<=b.right+1;}));
  await r.api('/api/nodes/'+unshared.id,'PATCH',{title:'recipient updated '+mode});await delay(3000);
  assertID('tab-focus-scroll-through-poll-'+mode,await selected.evaluate((el,scroll)=>document.activeElement===el&&el.parentElement.scrollLeft===scroll,scrollBefore));
  assertID('tab-poll-no-panel-movement-'+mode,JSON.stringify(panelsBefore)===JSON.stringify(await p.evaluate(()=>['#cards','#chat'].map(q=>{const x=document.querySelector(q).getBoundingClientRect();return [x.left,x.top,x.width,x.height];}))));
  assertID('tabs-no-overflow-'+mode,await p.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
  const afterGroups=Object.fromEntries(seats.map(s=>[s.name,counts(s)]));assert.deepEqual(afterGroups,beforeGroups);manifest.counters['activities-'+mode]={before:beforeGroups,after:afterGroups};
  // Do not leave this reader on A while the next viewport tests unread.
  await p.locator('[data-tab=current]').click();await open(p,unshared.id);
  await p.waitForFunction(t=>document.querySelector('#chattitle').innerText===t,'recipient updated '+mode);
 }
 const before=Object.fromEntries(seats.map(s=>[s.name,counts(s)]));
 for(const mode of ['desktop','phone']) {
  const op=await pageFor(a,mode,'owner-'+mode),rp=await pageFor(r,mode,'recipient-'+mode);
  await open(op,a.node.id);await open(rp,received);
  await rp.waitForFunction(()=>document.querySelector('#msgs').innerText.includes('Synthetic response'),null,{timeout:30000});
  assertID('normal-panel-'+mode,await rp.locator('#prompt').isVisible() && await rp.locator('#sharedview').count()===0);
  assertID('recipient-no-agent-'+mode,!(await rp.locator('#destinationagent').isVisible()));
  await rp.locator('#msgs .bubble').first().click();
  assertID('recipient-no-use-as-prompt-'+mode,await rp.locator('[data-gact="prompt"]').count()===0);
  await rp.locator('#prompt').click({trial:true});await rp.locator('#sendbtn').click({trial:true});
  const comment='Synthetic human '+mode;await rp.locator('#prompt').fill(comment);await rp.locator('#sendbtn').click();
  await op.waitForFunction(t=>document.querySelector('#msgs').innerText.includes(t),comment,{timeout:40000});
  assertID('owner-original-timeline-'+mode,(await op.locator('#chattitle').innerText())==='owner-a chat');
  const counterBefore=Object.fromEntries(seats.map(s=>[s.name,counts(s)]));
  const draft='Offline retained '+mode;await op.locator('#prompt').fill(draft);
  let failedRequests=0;op.on('requestfailed',request=>{if(request.url().startsWith(a.base+'/api/'))failedRequests++;});
  await op.context().setOffline(true);
  await until(()=>failedRequests>0,15000,'actual offline request failure');
  const offlineComment='Offline new human '+mode;
  await rp.locator('#prompt').fill(offlineComment);await rp.locator('#sendbtn').click();
  const restoredFetch=op.waitForResponse(response=>response.url().startsWith(a.base+'/api/')&&response.status()===200,{timeout:30000});
  await op.context().setOffline(false);
  await restoredFetch;assertID('offline-valid-fetch-after-online-'+mode,true);
  await op.getByText(offlineComment,{exact:true}).waitFor({timeout:40000});
  assertID('offline-new-content-once-'+mode,(await op.getByText(offlineComment,{exact:true}).count())===1);
  assertID('offline-selection-draft-'+mode,(await op.locator('#chattitle').innerText())==='owner-a chat' && (await op.locator('#prompt').innerText())===draft);
  await op.locator('#prompt').click({trial:true});await op.locator('#sharebtn').click({trial:true});
  if(mode==='phone')await op.locator('#chatback').click({trial:true});
  const recoveryDraft='Recovery retained '+mode;await rp.locator('#prompt').fill(recoveryDraft);
  let releaseOld, releaseNew, seenOld;
  const oldHeld=new Promise(resolve=>releaseOld=resolve),newHeld=new Promise(resolve=>releaseNew=resolve),oldReady=new Promise(resolve=>seenOld=resolve);
  heldReleases.add(releaseOld);heldReleases.add(releaseNew);
  let reads=0;
  try {
   await rp.route('**/api/team/subscriptions/*/*/*/*?after=*',async route=>{
    const response=await route.fetch();reads++;
    if(reads===1){seenOld();await oldHeld;}else if(reads===2)await newHeld;
    await route.fulfill({response});
   });
   await Promise.race([oldReady,delay(20000).then(()=>{throw Error('no real held mirror response');})]);
   const off=rp.waitForResponse(x=>x.url().endsWith('/api/team/subscriptions')&&x.status()===404,{timeout:20000});
   await r.api('/api/settings','PUT',{experimental_feature_team:false});await off;
   await rp.waitForFunction(()=>document.querySelector('#prompt').getAttribute('contenteditable')!=='true');
   assertID('recovery-disabled-draft-'+mode,(await rp.locator('#prompt').innerText())===recoveryDraft);
   const on=rp.waitForResponse(x=>x.url().endsWith('/api/team/subscriptions')&&x.status()===200,{timeout:20000});
   await r.api('/api/settings','PUT',{experimental_feature_team:true});await on;
   await rp.locator('#prompt[contenteditable=true]').waitFor({timeout:15000});
   await until(()=>reads>=2,15000,'replacement mirror read');
   releaseOld();await delay(6200);
   assertID('recovery-old-cannot-unlock-'+mode,reads===2);
   releaseNew();
   const own=await until(async()=>{const ss=(await a.api('/api/team/subscriptions')).subscriptions;return ss.find(x=>x.chat===shared.chat&&x.access==='owner');},15000,'owner subscription');
   const key=[own.server,own.owner,own.chat,own.generation].join('/');
   const recoveryComment='Recovery new human '+mode;
   await a.api('/api/team/subscriptions/'+key+'/comments','POST',{text:recoveryComment,anchor:0,key:mode==='desktop'?'abcdef0123456789':'abcdef0123456790'});
   await rp.getByText(recoveryComment,{exact:true}).waitFor({timeout:30000});
   assertID('recovery-once-selection-draft-'+mode,(await rp.getByText(recoveryComment,{exact:true}).count())===1 && (await rp.locator('#chattitle').innerText())==='owner-a chat' && (await rp.locator('#prompt').innerText())===recoveryDraft && await rp.evaluate(id=>localStorage.getItem('scimux-sel')===id,received));
   await rp.locator('#prompt').click({trial:true});
   manifest.counters['recovery-'+mode]={before:counterBefore,after:Object.fromEntries(seats.map(s=>[s.name,counts(s)])),offlineFailedRequests:failedRequests,heldReads:2};
   for(const s of seats)assert.deepEqual(counts(s),counterBefore[s.name]);
  } finally {releaseOld();releaseNew();heldReleases.delete(releaseOld);heldReleases.delete(releaseNew);await rp.unroute('**/api/team/subscriptions/*/*/*/*?after=*');}
  await screenshot(op,'owner-'+mode+'.png');await screenshot(rp,'recipient-'+mode+'.png');
 }
 for(const s of seats)assert.deepEqual(counts(s),before[s.name],s.name+' counters unchanged by comments');
 manifest.counters.beforeComments=before;manifest.counters.afterComments=Object.fromEntries(seats.map(s=>[s.name,counts(s)]));
 assertID('comments-zero-agent-attempts',true);
 // Native chat uses the same private tmux socket and synthetic hook transcript.
 const native=await node(a,'claude','native chat','Synthetic native initial');
 const text='  quoted α\nsecond line  ';
 // The browser composer trims outer whitespace; use the controller API here
 // to verify the native delivery boundary preserves the submitted bytes.
 await a.api('/api/nodes/'+native.id+'/send','POST',{text});
 await until(()=>jsonLines(path.join(a.data,'counter')).some(x=>x.text===text),30000,'native exact prompt');
 const rows=jsonLines(path.join(a.data,'counter')).filter(x=>x.text===text);
 const attempts=jsonLines(path.join(a.data,'counter.attempts')).filter(x=>x.text===text);
 assertID('native-exact-one-prompt',rows.length===1 && attempts.length===1);
 await until(async()=>{const c=await a.api('/api/nodes/'+native.id+'/chat');return c.turns?.some(t=>t.role==='assistant' && String(t.text||'').includes('Synthetic response 2'));},30000,'native authoritative response');
 for(const mode of ['desktop','phone'])await earlyNativeComment(mode,a,r);
 for(const mode of ['desktop','phone']){await conversationTrace(mode,a,r);await nativeTrace(mode,a,r);await refusalTrace(mode,a,r,false);await refusalTrace(mode,a,r,true);}
 for(const mode of ['desktop','phone']){const before=counts(a);await nativeStartupFailure(mode,a);assert.deepEqual(counts(a),before);assertID('native-start-failure-zero-dispatch-'+mode,true);}
 for(const mode of ['desktop','phone'])await recipientBoundary(mode,a,r);
 for(const mode of ['desktop','phone'])await aiSourcePrompt(mode,a,r);
 for(const mode of ['desktop','phone'])await recipientLeave(mode,a,r);
 for(const mode of ['desktop','phone'])await permissionLifecycle(mode,a,r);
 for(const mode of ['desktop','phone'])await recipientLeaveEmpty(mode,a);
 for(const mode of ['desktop','phone'])await recipientLeaveStale(mode,a,r);
 for(const mode of ['desktop','phone'])await heldEvidence(mode,a,r);
 for(const mode of ['w320','w390','w768'])await viewportKeyboard(mode,r);
 for(const mode of ['desktop','phone'])await droppedSend(mode,a,r);
 for(const mode of ['desktop','phone'])await reshareStale(mode,a,r);
 for(const mode of ['desktop','phone'])for(const scheme of ['light','dark'])await mediaContrast(mode,scheme,a,r);
 for(const mode of ['desktop','phone'])await unsharedChat(mode,a);
 for(const mode of ['desktop','phone'])await heldChatLoading(mode,a);
 for(const mode of ['desktop','phone'])await receivedOfflineRetry(mode,a,r);
 manifest.counters.afterNative=Object.fromEntries(seats.map(s=>[s.name,counts(s)]));
 assertID('native-authoritative-response',true);
 if(process.env.SCIMUX_E2E_FAIL_AFTER_NATIVE==='1')throw Error('controlled failure with native node');
}
(async()=>{try{await run();manifest.result='pass';}catch(e){manifest.result='fail';manifest.error=String(e);console.error(e.stack||e);process.exitCode=1;for(const {page,label} of pages){try{if(!page.isClosed())await screenshot(page,'failure-'+label+'.png');}catch{}}}finally{await cleanup();console.log('Artifacts: '+root);if(signalCode)process.exitCode=signalCode;}})();
