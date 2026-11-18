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
const { selectSharedSubscription } = require('./testdata/team-e2e/share-select.cjs');
const { retireCompletedContexts } = require('./testdata/team-e2e/browser-lifecycle.cjs');
const { contrastSample } = require('./testdata/team-e2e/contrast.cjs');
const { createCleanupOnce } = require('./testdata/team-e2e/cleanup-once.cjs');
const { createPageCapture } = require('./testdata/team-e2e/capture-once.cjs');

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
const coverageArchive = [];
const pending = new Set();
const heldReleases = new Set();
let browser, relay, shutting = false, signalCode = 0;
let cleanupProbeGate, cleanupProbeRelease;
let captureProbeGate, captureProbeRelease, captureProbeStarted=false;
const launchBrowser=()=>playwright.chromium.launch({handleSIGINT:false,handleSIGTERM:false,handleSIGHUP:false});
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
async function makeSeat(name, legacyClosed=false) {
  const data=artifacts(name); fs.mkdirSync(data,{mode:0o700});
  if(legacyClosed)fs.writeFileSync(path.join(data,'nodes.jsonl'),JSON.stringify({type:'node',node:{id:'Synthetic-legacy-closed',title:'Synthetic legacy closed',agent:'codex',dir:data,created_at:'1999-01-01T00:00:00Z',ended_at:'2000-01-01T00:00:00Z'}})+'\n',{mode:0o600});
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
  await reader.locator('#msgs').getByText('Synthetic response 1',{exact:true}).waitFor({timeout:30000});
  const seed='Synthetic owner discussion '+mode;await owner.locator('#prompt').fill(seed);await owner.locator('#sendbtn').click();await reader.locator('#msgs').getByText(seed,{exact:true}).waitFor({timeout:30000});
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
    await open(reader,'team/'+key);await reader.locator('#msgs').getByText(seed,{exact:true}).waitFor({timeout:30000});await reader.locator('#msgs').getByText('Synthetic response 1',{exact:true}).waitFor({timeout:30000});
    const author=reader.locator('#msgs .gmsg').filter({hasText:seed}).locator('.gwho');
    const saved=await readerSeat.api('/api/team/subscriptions/'+key+'?after=0');
    const assistant=saved.entries.find(x=>x.t==='assistant'&&x.text==='Synthetic response 1');
    const savedComments=await readerSeat.api('/api/team/subscriptions/'+key+'/comments?after=0');const savedHuman=savedComments.entries.find(x=>x.text===seed);
    const agentBubble=reader.locator('#msgs [data-seq]').filter({hasText:'Synthetic response 1'}).first();
    assertID('received-offline-saved-copy-and-draft-'+mode,(await reader.locator('#prompt').innerText())===draft&&(await author.innerText()).trim()===preAuthor&&(await author.getAttribute('data-seat'))===preHuman.author&&!!savedHuman&&savedHuman.author===preHuman.author&&savedHuman.seq===preHuman.seq&&savedHuman.anchor===preHuman.anchor&&Number(await author.locator('..').locator('..').getAttribute('data-cseq'))===preHuman.seq&&!!assistant&&assistant.seq===preAssistant.seq&&assistant.agent===preAssistant.agent&&(await agentBubble.locator('.gwho').innerText()).trim()===preAgentAuthor&&Number(await agentBubble.getAttribute('data-seq'))===preAssistant.seq&&[saved.view.server,saved.view.owner,saved.view.chat,saved.view.generation].join('/')===key);
    const dialog=reader.waitForEvent('dialog').then(failure=>failure.accept());await reader.locator('#sendbtn').click();await dialog;
    assertID('received-offline-failed-send-keeps-draft-'+mode,(await reader.locator('#prompt').innerText())===draft&&JSON.stringify(counts(ownerSeat))===JSON.stringify(before));
  } finally {await restartRelay();}
  await reader.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
  let attemptedKey, retryKey;const pattern='**/api/team/subscriptions/'+key+'/comments';
  await reader.route(pattern,async route=>{if(route.request().method()!=='POST'){await route.continue();return;}attemptedKey=route.request().postDataJSON().key;await route.fetch();await route.abort('failed');});
  try {
    const dialog=reader.waitForEvent('dialog').then(failure=>failure.accept());await reader.locator('#sendbtn').click();await dialog;
    assertID('received-retry-uncertain-keeps-draft-'+mode,(await reader.locator('#prompt').innerText())===draft);
  } finally {await reader.unroute(pattern);}
  await reader.route(pattern,route=>{if(route.request().method()==='POST')retryKey=route.request().postDataJSON().key;return route.continue();});
  try {await reader.locator('#sendbtn').click();await reader.locator('#msgs').getByText(draft,{exact:true}).waitFor({timeout:30000});} finally {await reader.unroute(pattern);}
  const entries=await readerSeat.api('/api/team/subscriptions/'+key+'/comments?after=0');
  assertID('received-explicit-retry-idempotent-'+mode,!!attemptedKey&&attemptedKey===retryKey&&entries.entries.filter(x=>x.text===draft).length===1&&JSON.stringify(counts(ownerSeat))===JSON.stringify(before));
  manifest['received-offline-'+mode]={key,preAuthor,preAgentAuthor,human:{author:preHuman.author,seq:preHuman.seq,anchor:preHuman.anchor},assistant:{seq:preAssistant.seq,agent:preAssistant.agent||'Agent'},before,after:counts(ownerSeat),sequence:entries.entries.find(x=>x.text===draft)?.seq};await screenshot(reader,'received-offline-retry-'+mode+'.png');
}
async function deleteRecreationIsolation(mode,seat) {
  const title='Delayed deletion '+mode,old=await node(seat,'codex',title,'Synthetic old deletion target');
  const page=await pageFor(seat,mode,'delete-recreation-'+mode);await open(page,old.id);if(mode==='phone')await page.locator('#chatback').click();
  let release,ready,released=false;const held=new Promise(resolve=>release=resolve),requested=new Promise(resolve=>ready=resolve);heldReleases.add(release);
  await page.route('**/api/nodes/'+old.id+'*',async route=>{if(route.request().method()!=='DELETE'||new URL(route.request().url()).pathname!=='/api/nodes/'+old.id){await route.continue();return;}try{ready();await held;if(!page.isClosed())await route.continue();}catch(e){if(!released&&!shutting&&!page.isClosed())throw e;}});
  try {
    const card=page.locator('#card-'+old.id);await card.scrollIntoViewIfNeeded();
    if(mode==='desktop')await card.hover();else {const box=await card.boundingBox(),cdp=await page.context().newCDPSession(page);await cdp.send('Input.dispatchTouchEvent',{type:'touchStart',touchPoints:[{x:box.x+box.width-20,y:box.y+box.height/2}]});await cdp.send('Input.dispatchTouchEvent',{type:'touchMove',touchPoints:[{x:box.x+box.width-120,y:box.y+box.height/2}]});await cdp.send('Input.dispatchTouchEvent',{type:'touchEnd',touchPoints:[]});await cdp.detach();}
    const confirmation=page.waitForEvent('dialog');await Promise.all([confirmation.then(dialog=>dialog.accept()),card.locator('[data-trash]').click()]);await Promise.race([requested,delay(15000).then(()=>{throw Error('old deletion request not held');})]);
    await open(page,seat.node.id);await seat.api('/api/nodes/'+old.id,'DELETE');seat.nodes=seat.nodes.filter(id=>id!==old.id);
    const fresh=await node(seat,'codex',title,'Synthetic replacement deletion target');assertID('delete-recreation-same-id-'+mode,fresh.id===old.id&&fresh.instance_id!==old.instance_id);
    const before=counts(seat),response=page.waitForResponse(r=>r.request().method()==='DELETE'&&new URL(r.url()).pathname==='/api/nodes/'+old.id,{timeout:30000});
    const feedback=page.waitForEvent('dialog');released=true;release();heldReleases.delete(release);const rejected=await response;assertID('delete-recreation-stale-rejected-'+mode,rejected.status()===409);const warning=await feedback;assertID('delete-recreation-stale-feedback-'+mode,/chat identity changed/i.test(warning.message()));await warning.accept();
    const live=await until(async()=>(await seat.api('/api/state')).nodes.find(n=>n.id===fresh.id&&n.instance_id===fresh.instance_id),30000,'replacement survives stale deletion');
    assertID('delete-recreation-selection-and-counter-'+mode,!!live&&(await page.evaluate(()=>localStorage.getItem('scimux-sel')))===seat.node.id&&JSON.stringify(before)===JSON.stringify(counts(seat)));
    manifest['delete-recreation-'+mode]={before,after:counts(seat),old:old.instance_id,fresh:fresh.instance_id};await screenshot(page,'delete-recreation-'+mode+'.png');
  } finally {released=true;release();heldReleases.delete(release);await page.unrouteAll({behavior:'wait'});}
}
async function legacyClosedRecreation(mode) {
  const seat=await makeSeat('legacy-closed-'+mode,true),id='Synthetic-legacy-closed',title='Synthetic legacy closed';
  const initial=(await seat.api('/api/state')).nodes.find(n=>n.id===id);
  assertID('legacy-closed-migrated-'+mode,!!initial?.instance_id&&!!initial.ended_at);
  const page=await pageFor(seat,mode,'legacy-closed-'+mode);await page.reload({waitUntil:'networkidle'});
  const reloaded=(await seat.api('/api/state')).nodes.find(n=>n.id===id);
  assertID('legacy-closed-identity-stable-'+mode,reloaded?.instance_id===initial.instance_id&&reloaded.ended_at===initial.ended_at);
  const card=page.locator('#card-'+id);await card.waitFor({timeout:30000});
  if(mode==='desktop')await card.hover();else {const box=await card.boundingBox(),cdp=await page.context().newCDPSession(page);await cdp.send('Input.dispatchTouchEvent',{type:'touchStart',touchPoints:[{x:box.x+box.width-20,y:box.y+box.height/2}]});await cdp.send('Input.dispatchTouchEvent',{type:'touchMove',touchPoints:[{x:box.x+box.width-120,y:box.y+box.height/2}]});await cdp.send('Input.dispatchTouchEvent',{type:'touchEnd',touchPoints:[]});await cdp.detach();}
  let held,ready,release;const requested=new Promise(resolve=>ready=resolve),hold=new Promise(resolve=>release=resolve);heldReleases.add(release);
  await page.route(url=>url.pathname==='/api/nodes/'+id,async route=>{if(route.request().method()!=='DELETE'){await route.continue();return;}held=route;ready();await hold;if(!page.isClosed())await route.continue();});
  try {
    const confirmation=page.waitForEvent('dialog');await Promise.all([confirmation.then(dialog=>dialog.accept()),card.locator('[data-trash]').click()]);await Promise.race([requested,delay(15000).then(()=>{throw Error('legacy deletion was not held');})]);
    const sent=new URL(held.request().url()).searchParams.get('chat_instance');await seat.api('/api/nodes/'+id,'DELETE');
    const replacement=await node(seat,'codex',title,'Synthetic replacement must survive');
    assertID('legacy-closed-reused-id-'+mode,replacement.id===id&&replacement.instance_id!==initial.instance_id);
    const before=counts(seat),response=page.waitForResponse(r=>r.request().method()==='DELETE'&&new URL(r.url()).pathname==='/api/nodes/'+id);
    const feedback=page.waitForEvent('dialog');release();heldReleases.delete(release);const stale=await response;const warning=await feedback;assertID('legacy-closed-stale-feedback-'+mode,/chat identity changed/i.test(warning.message()));await warning.accept();
    const state=await seat.api('/api/state'),chat=await seat.api('/api/nodes/'+id+'/chat');
    const survives=state.nodes.some(n=>n.id===id&&n.instance_id===replacement.instance_id)&&chat.turns?.some(t=>t.role==='assistant');
    assertID('legacy-closed-stale-delete-rejected-'+mode,sent===initial.instance_id&&stale.status()===409&&survives&&JSON.stringify(before)===JSON.stringify(counts(seat)));
    manifest['legacy-closed-'+mode]={before,after:counts(seat),status:stale.status(),replacementSurvives:survives};await screenshot(page,'legacy-closed-recreation-'+mode+'.png');
  } finally {release();heldReleases.delete(release);await page.unrouteAll({behavior:'wait'});}
}
async function noSelection(mode,seat) {
  const page=await pageFor(seat,mode,'no-selection-'+mode);const initial=counts(seat);
  assertID('no-selection-no-borrowed-activity-'+mode,(await page.evaluate(()=>localStorage.getItem('scimux-sel')||''))===''&&!(await page.locator('#workpulse').evaluate(el=>el.classList.contains('on')))&&!(await page.locator('#convtools').isVisible())&&!(await page.locator('#autoapprove').isVisible()));
  assertID('no-selection-zero-dispatch-'+mode,JSON.stringify(counts(seat))===JSON.stringify(initial)&&(mode!=='desktop'||(initial.attempted===0&&initial.accepted===0&&initial.controls===0)));
  const chat=await node(seat,'codex','Remove active '+mode,'Synthetic remove setup');await open(page,chat.id);
  const releasePath=path.join(seat.data,'counter.release-active');try{fs.unlinkSync(releasePath);}catch(e){if(e.code!=='ENOENT')throw e;}
  const before=counts(seat);await page.locator('#prompt').fill('Synthetic active removal '+mode+' [hold-active]');await page.locator('#sendbtn').click();await page.locator('#workpulse.on').waitFor({timeout:30000});
  let release,ready,released=false;const held=new Promise(resolve=>release=resolve),requested=new Promise(resolve=>ready=resolve);heldReleases.add(release);
  await page.route('**/api/nodes/'+chat.id+'/chat*',async route=>{try{const headers={...route.request().headers()};delete headers['if-none-match'];const response=await route.fetch({headers});ready();await held;if(!page.isClosed())await route.fulfill({response});}catch(e){if(!released&&!shutting&&!page.isClosed())throw e;}});
  try {
    await Promise.race([requested,delay(15000).then(()=>{throw Error('active removal response not held');})]);
    if(mode==='phone')await page.locator('#chatback').click();const card=page.locator('#card-'+chat.id);await card.scrollIntoViewIfNeeded();
    if(mode==='desktop')await card.hover();else {const box=await card.boundingBox(),cdp=await page.context().newCDPSession(page);await cdp.send('Input.dispatchTouchEvent',{type:'touchStart',touchPoints:[{x:box.x+box.width-20,y:box.y+box.height/2}]});await cdp.send('Input.dispatchTouchEvent',{type:'touchMove',touchPoints:[{x:box.x+box.width-120,y:box.y+box.height/2}]});await cdp.send('Input.dispatchTouchEvent',{type:'touchEnd',touchPoints:[]});await cdp.detach();}
    page.once('dialog',dialog=>dialog.accept());await card.locator('[data-trash]').click();await card.waitFor({state:'detached',timeout:30000});seat.nodes=seat.nodes.filter(id=>id!==chat.id);
    await page.locator('#workpulse.on').waitFor({state:'detached',timeout:10000});
    assertID('no-selection-active-retired-'+mode,!(await page.locator('#workpulse').evaluate(el=>el.classList.contains('on')))&&!(await page.locator('#convtools').isVisible())&&!(await page.locator('#autoapprove').isVisible()));
    released=true;release();heldReleases.delete(release);await page.unrouteAll({behavior:'wait'});await delay(500);
    assertID('no-selection-stale-response-isolated-'+mode,!(await page.locator('#workpulse').evaluate(el=>el.classList.contains('on')))&&!(await page.locator('#convtools').isVisible())&&!(await page.locator('#autoapprove').isVisible()));
    const after=counts(seat);assertID('no-selection-explicit-send-only-'+mode,after.attempted===before.attempted+1&&after.accepted===before.accepted+1&&after.controls===before.controls);manifest['no-selection-'+mode]={initial,before,after};await screenshot(page,'no-selection-'+mode+'.png');
  } finally {released=true;release();heldReleases.delete(release);fs.writeFileSync(releasePath,'release');await page.unrouteAll({behavior:'wait'});}
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
    released=true;release();heldReleases.delete(release);await page.unrouteAll({behavior:'wait'});await page.locator('#msgs').getByText('Synthetic response 1',{exact:true}).waitFor({timeout:30000});
  } finally {released=true;release();heldReleases.delete(release);await page.unrouteAll({behavior:'wait'});}
}
async function stage1Scanner(mode,ownerSeat,readerSeat) {
  const local=await node(readerSeat,'codex','Scanner local '+mode,'Synthetic scanner setup');
  const share=await ownerSeat.api('/api/team/shares/'+ownerSeat.node.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid),30000,'scanner received subscription');
  const received='team/'+[sub.server,sub.owner,sub.chat,sub.generation].join('/');
  const page=await pageFor(readerSeat,mode,'stage1-scanner-'+mode);await page.emulateMedia({reducedMotion:'reduce'});
  await open(page,local.id);await page.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
  const releasePath=path.join(readerSeat.data,'counter.release-active');try{fs.unlinkSync(releasePath);}catch(e){if(e.code!=='ENOENT')throw e;}
  const before=counts(readerSeat);await page.locator('#prompt').fill('Synthetic active '+mode+' [hold-active]');await page.locator('#sendbtn').click();
  await page.locator('#workpulse.on').waitFor({timeout:30000});
  let release,ready,released=false;const held=new Promise(resolve=>release=resolve),requested=new Promise(resolve=>ready=resolve);heldReleases.add(release);
  await page.route('**/api/nodes/'+local.id+'/chat*',async route=>{try{const headers={...route.request().headers()};delete headers['if-none-match'];const response=await route.fetch({headers});ready();await held;if(!page.isClosed())await route.fulfill({response});}catch(e){if(!released&&!shutting&&!page.isClosed())throw e;}});
  try {
    await Promise.race([requested,delay(15000).then(()=>{throw Error('active response not held');})]);
    if(mode==='phone')await page.locator('#chatback').click();await open(page,received);
    await page.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
    assertID('stage1-scanner-received-off-'+mode,!(await page.locator('#workpulse').evaluate(el=>el.classList.contains('on')))&&!(await page.locator('#convtools').isVisible()));
    released=true;release();heldReleases.delete(release);await page.unrouteAll({behavior:'wait'});await delay(500);
    assertID('stage1-stale-active-response-isolated-'+mode,!(await page.locator('#workpulse').evaluate(el=>el.classList.contains('on')))&&!(await page.locator('#autoapprove').isVisible()));
    await screenshot(page,'stage1-scanner-received-'+mode+'.png');
    if(mode==='phone')await page.locator('#chatback').click();await open(page,local.id);await page.locator('#workpulse.on').waitFor({timeout:20000});
    assertID('stage1-scanner-local-restored-'+mode,true);
    await page.reload({waitUntil:'networkidle'});await page.locator('#workpulse.on').waitFor({timeout:20000});
    assertID('stage1-scanner-active-reload-'+mode,true);
    let releaseOld,oldReady,oldDone,oldCaptured=false,oldReleased=false;
    const oldHeld=new Promise(resolve=>releaseOld=resolve),oldRequested=new Promise(resolve=>oldReady=resolve),oldSettled=new Promise(resolve=>oldDone=resolve);heldReleases.add(releaseOld);
    await page.route('**/api/nodes/'+local.id+'/chat*',async route=>{
      if(oldCaptured){await route.continue();return;}
      oldCaptured=true;
      try {const headers={...route.request().headers()};delete headers['if-none-match'];const response=await route.fetch({headers});oldReady();await oldHeld;if(!page.isClosed())await route.fulfill({response});}
      catch(e){if(!oldReleased&&!shutting&&!page.isClosed())throw e;}finally{oldDone();}
    });
    try {
      await Promise.race([oldRequested,delay(15000).then(()=>{throw Error('rapid-switch old chat response not held');})]);
      if(mode==='phone')await page.locator('#chatback').click();await open(page,received);
      fs.writeFileSync(releasePath,'release');await until(async()=>{const chat=await readerSeat.api('/api/nodes/'+local.id+'/chat');return chat.turn_in_flight===false&&(chat.turns||[]).some(turn=>turn.role==='assistant'&&turn.text==='Synthetic response 2');},30000,'active response completed');
      if(mode==='phone')await page.locator('#chatback').click();await open(page,local.id);
      await page.locator('#msgs .bubble').getByText('Synthetic response 2',{exact:true}).waitFor({timeout:30000});
      await page.locator('#workpulse.on').waitFor({state:'detached',timeout:30000});
      oldReleased=true;releaseOld();heldReleases.delete(releaseOld);await oldSettled;await delay(500);
      assertID('stage1-scanner-rapid-aba-stale-isolated-'+mode,!(await page.locator('#workpulse').evaluate(el=>el.classList.contains('on'))));
      await page.context().setOffline(true);await delay(1000);const online=page.waitForResponse(r=>new URL(r.url()).pathname==='/api/state'&&[200,304].includes(r.status()),{timeout:30000});await page.context().setOffline(false);await online;
      assertID('stage1-scanner-idle-reconnect-'+mode,!(await page.locator('#workpulse').evaluate(el=>el.classList.contains('on'))));
    } finally {oldReleased=true;releaseOld();heldReleases.delete(releaseOld);await page.unrouteAll({behavior:'wait'});}
    const after=counts(readerSeat);assertID('stage1-scanner-explicit-dispatch-only-'+mode,after.attempted===before.attempted+1&&after.accepted===before.accepted+1&&after.controls===before.controls);
    manifest['stage1-scanner-'+mode]={before,after};await screenshot(page,'stage1-scanner-idle-'+mode+'.png');
  } finally {released=true;release();heldReleases.delete(release);fs.writeFileSync(releasePath,'release');await page.unrouteAll({behavior:'wait'});}
  const owner=await pageFor(ownerSeat,mode,'stage1-scanner-owner-'+mode);await open(owner,ownerSeat.node.id);
  await owner.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});await owner.locator('#destinationagent').click();
  const ownerRelease=path.join(ownerSeat.data,'counter.release-active');try{fs.unlinkSync(ownerRelease);}catch(e){if(e.code!=='ENOENT')throw e;}
  try {
    const beforeOwner=counts(ownerSeat);await owner.locator('#prompt').fill('Synthetic owner active '+mode+' [hold-active]');await owner.locator('#sendbtn').click();
    await owner.locator('#workpulse.on').waitFor({timeout:30000});assertID('stage1-shared-owner-active-'+mode,(await owner.locator('#destinationpeople').getAttribute('aria-pressed'))==='true');
    fs.writeFileSync(ownerRelease,'release');await owner.locator('#workpulse.on').waitFor({state:'detached',timeout:30000});
    const afterOwner=counts(ownerSeat);assertID('stage1-shared-owner-idle-'+mode,afterOwner.attempted===beforeOwner.attempted+1&&afterOwner.accepted===beforeOwner.accepted+1&&afterOwner.controls===beforeOwner.controls);
    manifest['stage1-scanner-owner-'+mode]={beforeOwner,afterOwner};await screenshot(owner,'stage1-scanner-owner-'+mode+'.png');
  } finally {fs.writeFileSync(ownerRelease,'release');}
}
async function stage1Unread(mode,ownerSeat,readerSeat) {
  const chats=[];
  for(let i=0;i<5;i++){
    const chat=await node(ownerSeat,'codex','Unread owner '+mode+' '+i,'Synthetic unread setup');
    await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
    const share=await ownerSeat.api('/api/team/shares/'+chat.id);
    const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid),30000,'unread recipient subscription');
    await until(async()=>(await ownerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.chat===share.uid&&x.local_node===chat.id),30000,'verified owner node identity');
    chats.push({chat,share,sub,key:[sub.server,sub.owner,sub.chat,sub.generation].join('/')});
  }
  const beforeOwner=counts(ownerSeat),beforeReader=counts(readerSeat);
  const owner=await pageFor(ownerSeat,mode,'stage1-unread-owner-'+mode),reader=await pageFor(readerSeat,mode,'stage1-unread-reader-'+mode);
  await open(owner,ownerSeat.node.id);if(mode==='phone')await owner.locator('#chatback').click();
  for(let i=0;i<2;i++){
    if(i&&mode==='phone')await reader.locator('#chatback').click();
    await open(reader,'team/'+chats[i].key);await reader.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
    const text='Incoming '+mode+' '+i;await reader.locator('#prompt').fill(text);await reader.locator('#sendbtn').click();
    await reader.locator('#msgs .bubble').getByText(text,{exact:true}).waitFor({timeout:30000});
    const card=owner.locator('#card-'+chats[i].chat.id);await card.locator('.activity-unread').waitFor({timeout:40000});
    assertID('stage1-unread-specific-card-'+mode+'-'+i,await card.locator('.activity-unread').evaluate(el=>el.nextElementSibling?.matches('button.chev')));
  }
  assertID('stage1-unread-two-cards-'+mode,await owner.locator('#card-'+chats[0].chat.id+' .activity-unread').count()===1&&await owner.locator('#card-'+chats[1].chat.id+' .activity-unread').count()===1);
  await screenshot(owner,'stage1-unread-cards-'+mode+'.png');
  // A second owner view sends while the first view holds its tail classifier.
  // Blocking the second view's read acknowledgements keeps the earlier
  // collaborator message genuinely unseen in the shared installation.
  const watched=chats[0], writer=await pageFor(ownerSeat,mode,'stage1-unread-writer-'+mode);
  let releaseTail,tailReady,tailReleased=false;
  const tailHeld=new Promise(resolve=>releaseTail=resolve),tailRequested=new Promise(resolve=>tailReady=resolve);heldReleases.add(releaseTail);
  await writer.route('**/api/team/subscriptions/'+watched.key+'/seen',route=>route.abort());
  await owner.route('**/api/team/subscriptions/'+watched.key+'/comments?after=*',async route=>{try{const response=await route.fetch();tailReady();await tailHeld;if(!owner.isClosed())await route.fulfill({response});}catch(e){if(!tailReleased&&!shutting&&!owner.isClosed())throw e;}});
  try {
    await open(writer,watched.chat.id);await writer.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});
    const own='Later owner append '+mode;await writer.locator('#prompt').fill(own);await writer.locator('#sendbtn').click();
    await writer.locator('#msgs .bubble').getByText(own,{exact:true}).waitFor({timeout:30000});
    await until(async()=>{const v=(await ownerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.chat===watched.share.uid&&x.access==='owner');return v?.comments_last===2&&v.seen_comments===0;},40000,'own append with unread cursor unchanged');
    await Promise.race([tailRequested,delay(40000).then(()=>{throw Error('new tail classification not held');})]);
    assertID('stage1-known-incoming-survives-held-tail-'+mode,await owner.locator('#card-'+watched.chat.id+' .activity-unread').count()===1);
    await screenshot(owner,'stage1-unread-held-tail-'+mode+'.png');
  } finally {tailReleased=true;releaseTail();heldReleases.delete(releaseTail);await owner.unrouteAll({behavior:'wait'});await captureCoverage([{page:writer,label:'stage1-unread-writer-'+mode}]);await writer.close();}
  const afterWriter=(await ownerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.chat===watched.share.uid&&x.access==='owner');
  assertID('stage1-unread-writer-retirement-preserves-cursor-'+mode,afterWriter?.comments_last===2&&afterWriter.seen_comments===0);
  await owner.locator('[data-activity-group="mine"]').click();
  assertID('stage1-unread-folded-mine-'+mode,await owner.locator('[data-activity-group="mine"] .activity-unread').count()===1);
  await owner.reload({waitUntil:'networkidle'});
  if(mode==='phone'&&await owner.locator('#cards').evaluate(el=>el.getBoundingClientRect().right<=1))await owner.locator('#chatback').click();
  await owner.locator('[data-activity-group="mine"] .activity-unread').waitFor({timeout:40000});
  assertID('stage1-unread-fold-reload-'+mode,(await owner.locator('[data-activity-group="mine"]').getAttribute('aria-expanded'))==='false');
  await owner.context().setOffline(true);await delay(1500);await owner.context().setOffline(false);
  await owner.locator('[data-activity-group="mine"] .activity-unread').waitFor({timeout:40000});
  assertID('stage1-unread-fold-reconnect-'+mode,true);await owner.locator('[data-activity-group="mine"]').click();
  const pinnedCard=owner.locator('#card-'+chats[0].chat.id);await pinnedCard.scrollIntoViewIfNeeded();
  if(mode==='desktop')await pinnedCard.hover();else {const box=await pinnedCard.boundingBox(),cdp=await owner.context().newCDPSession(owner);await cdp.send('Input.dispatchTouchEvent',{type:'touchStart',touchPoints:[{x:box.x+box.width-20,y:box.y+box.height/2}]});await cdp.send('Input.dispatchTouchEvent',{type:'touchMove',touchPoints:[{x:box.x+box.width-120,y:box.y+box.height/2}]});await cdp.send('Input.dispatchTouchEvent',{type:'touchEnd',touchPoints:[]});await cdp.detach();}
  await owner.locator('[data-pin-action="'+chats[0].chat.id+'"]').click();await owner.locator('[data-tab=pinned]').click();
  assertID('stage1-unread-pinned-card-'+mode,await owner.locator('#card-'+chats[0].chat.id+' .activity-unread').count()===1);
  await screenshot(owner,'stage1-unread-pinned-'+mode+'.png');await owner.locator('[data-tab=current]').click();
  const third=chats[2],ownerView=await until(async()=>(await ownerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.chat===third.share.uid&&x.access==='owner'),30000,'self comment owner subscription');
  const ownerKey=[ownerView.server,ownerView.owner,ownerView.chat,ownerView.generation].join('/');
  await open(owner,third.chat.id);await owner.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});
  let release,ready,released=false;const held=new Promise(resolve=>release=resolve),requested=new Promise(resolve=>ready=resolve);heldReleases.add(release);
  await owner.route('**/api/team/subscriptions/'+ownerKey+'/comments',async route=>{try{const response=await route.fetch();ready();await held;if(!owner.isClosed())await route.fulfill({response});}catch(e){if(!released&&!shutting&&!owner.isClosed())throw e;}});
  try {
    await owner.locator('#prompt').fill('Own pending '+mode);await owner.locator('#sendbtn').click();
    await Promise.race([requested,delay(20000).then(()=>{throw Error('own comment not posted');})]);
    if(mode==='phone')await owner.locator('#chatback').click();await open(owner,ownerSeat.node.id);if(mode==='phone')await owner.locator('#chatback').click();
    const classified=owner.waitForResponse(async response=>{
      if(!response.url().includes('/api/team/subscriptions/'+ownerKey+'/comments?after=0')||response.status()!==200)return false;
      try{return (await response.json()).entries.some(entry=>entry.mine===true&&entry.text==='Own pending '+mode);}catch{return false;}
    },{timeout:40000});
    released=true;release();heldReleases.delete(release);await owner.unrouteAll({behavior:'wait'});
    await until(async()=>{const v=(await ownerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.chat===third.share.uid&&x.access==='owner');return v?.comments_last===1&&v.seen_comments===0;},40000,'own publication without read advance');
    await classified;
    const card=owner.locator('#card-'+third.chat.id);
    await until(async()=>await card.locator('[aria-label="Unread status unavailable"]').count()===0&&await card.locator('.activity-unread').count()===0,40000,'self comment classification');
    assertID('stage1-own-comment-no-unread-'+mode,true);
    assertID('stage1-discussion-zero-agent-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(beforeOwner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(beforeReader));
    manifest['stage1-unread-'+mode]={beforeOwner,afterOwner:counts(ownerSeat),beforeReader,afterReader:counts(readerSeat)};await screenshot(owner,'stage1-unread-own-'+mode+'.png');
  } finally {released=true;release();heldReleases.delete(release);await owner.unrouteAll({behavior:'wait'});}
  const fourth=chats[3],fourthView=await until(async()=>(await ownerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.chat===fourth.share.uid&&x.access==='owner'),30000,'concurrent owner subscription');
  const fourthKey=[fourthView.server,fourthView.owner,fourthView.chat,fourthView.generation].join('/');
  let releaseRead,readReady,readReleased=false,reads=0;const heldRead=new Promise(resolve=>releaseRead=resolve),requestedRead=new Promise(resolve=>readReady=resolve);heldReleases.add(releaseRead);
  await owner.route('**/api/team/subscriptions/'+fourthKey+'/comments?after=*',async route=>{try{if(++reads===1){const response=await route.fetch();readReady();await heldRead;if(!owner.isClosed())await route.fulfill({response});}else await route.continue();}catch(e){if(!readReleased&&!shutting&&!owner.isClosed())throw e;}});
  try {
    await open(owner,fourth.chat.id);await owner.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});
    await Promise.race([requestedRead,delay(20000).then(()=>{throw Error('owner comment read not held');})]);
    if(mode==='phone')await reader.locator('#chatback').click();await open(reader,'team/'+fourth.key);await reader.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
    const incoming='Concurrent incoming '+mode;await reader.locator('#prompt').fill(incoming);await reader.locator('#sendbtn').click();
    await reader.locator('#msgs .bubble').getByText(incoming,{exact:true}).waitFor({timeout:30000});
    const ownDone=owner.waitForResponse(response=>response.request().method()==='POST'&&response.url().endsWith('/api/team/subscriptions/'+fourthKey+'/comments')&&response.status()===200,{timeout:30000});
    await owner.locator('#prompt').fill('Own after incoming '+mode);await owner.locator('#sendbtn').click();await ownDone;
    if(mode==='phone')await owner.locator('#chatback').click();await open(owner,ownerSeat.node.id);if(mode==='phone')await owner.locator('#chatback').click();
    readReleased=true;releaseRead();heldReleases.delete(releaseRead);await owner.unrouteAll({behavior:'wait'});
    await until(async()=>{const v=(await ownerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.chat===fourth.share.uid&&x.access==='owner');return v?.comments_last===2&&v.seen_comments===0;},40000,'concurrent comments remain unseen');
    await owner.locator('#card-'+fourth.chat.id+' .activity-unread').waitFor({timeout:40000});
    assertID('stage1-incoming-before-own-stays-unread-'+mode,true);
    assertID('stage1-concurrent-discussion-zero-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(beforeOwner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(beforeReader));
    await screenshot(owner,'stage1-unread-concurrent-'+mode+'.png');
  } finally {readReleased=true;releaseRead();heldReleases.delete(releaseRead);await owner.unrouteAll({behavior:'wait'});}
  const fifth=chats[4],received='team/'+fifth.key,group='owner/'+fifth.sub.server+'/'+fifth.sub.owner;
  if(mode==='phone')await reader.locator('#chatback').click();await open(reader,received);await reader.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
  await until(async()=>{const v=(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.chat===fifth.share.uid&&x.generation===fifth.share.generation);return v&&!v.unread;},40000,'recipient initial copy read');
  if(mode==='phone')await reader.locator('#chatback').click();await open(reader,'team/'+fourth.key);if(mode==='phone')await reader.locator('#chatback').click();
  await open(owner,fifth.chat.id);await owner.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});
  const recipientIncoming='Owner discussion '+mode;await owner.locator('#prompt').fill(recipientIncoming);await owner.locator('#sendbtn').click();
  await owner.locator('#msgs .bubble').getByText(recipientIncoming,{exact:true}).waitFor({timeout:30000});
  await reader.locator('[data-tab=shared] [aria-label$="unread"]').waitFor({timeout:40000});
  await reader.locator('[data-tab=shared]').click();
  await reader.locator('[data-activity-group="'+group+'"] .activity-unread').waitFor({timeout:40000});
  const receivedCard=reader.locator('.card:has([data-open="'+received+'"])');await receivedCard.locator('.activity-unread').waitFor({timeout:40000});
  assertID('stage1-recipient-unread-tab-group-card-'+mode,true);
  await reader.locator('[data-activity-group="'+group+'"]').click();
  assertID('stage1-recipient-unread-folded-'+mode,await reader.locator('[data-activity-group="'+group+'"] .activity-unread').count()===1);
  await reader.reload({waitUntil:'networkidle'});
  if(mode==='phone'&&await reader.locator('#cards').evaluate(el=>el.getBoundingClientRect().right<=1))await reader.locator('#chatback').click();
  await reader.locator('[data-tab=shared]').click();await reader.locator('[data-activity-group="'+group+'"] .activity-unread').waitFor({timeout:40000});
  assertID('stage1-recipient-unread-fold-reload-'+mode,(await reader.locator('[data-activity-group="'+group+'"]').getAttribute('aria-expanded'))==='false');
  await reader.locator('[data-activity-group="'+group+'"]').click();await receivedCard.scrollIntoViewIfNeeded();
  if(mode==='desktop')await receivedCard.hover();else {const box=await receivedCard.boundingBox(),cdp=await reader.context().newCDPSession(reader);await cdp.send('Input.dispatchTouchEvent',{type:'touchStart',touchPoints:[{x:box.x+box.width-20,y:box.y+box.height/2}]});await cdp.send('Input.dispatchTouchEvent',{type:'touchMove',touchPoints:[{x:box.x+box.width-120,y:box.y+box.height/2}]});await cdp.send('Input.dispatchTouchEvent',{type:'touchEnd',touchPoints:[]});await cdp.detach();}
  await reader.locator('[data-pin-action="'+received+'"]').click();await reader.locator('[data-tab=pinned]').click();
  assertID('stage1-recipient-unread-pinned-'+mode,await receivedCard.locator('.activity-unread').count()===1);
  assertID('stage1-recipient-discussion-zero-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(beforeOwner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(beforeReader));
  await screenshot(reader,'stage1-recipient-unread-pinned-'+mode+'.png');
}
async function stage2SharedActions(mode,ownerSeat,otherSeat,readerSeat) {
  await otherSeat.api('/api/team/shares/'+otherSeat.node.id,'PUT',{server:otherSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+ownerSeat.node.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid&&x.generation===share.generation),30000,'stage2 shared subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),received='team/'+key;
  const reader=await pageFor(readerSeat,mode,'stage2-reader-'+mode),owner=await pageFor(ownerSeat,mode,'stage2-owner-'+mode);
  const beforeReader=counts(readerSeat),beforeOwner=counts(ownerSeat),beforeOther=counts(otherSeat);
  await reader.locator('[data-tab=shared]').click();await reader.locator('.shared-card .agent-logo svg[data-icon="users"]').first().waitFor({timeout:30000});
  const header=reader.locator('[data-activity-group]').first(),headerStyle=await header.evaluate(el=>({height:el.getBoundingClientRect().height,border:getComputedStyle(el).borderBottomStyle,expanded:el.getAttribute('aria-expanded')}));
  assertID('stage2-group-glyph-'+mode,headerStyle.height>=44&&headerStyle.border==='dashed'&&headerStyle.expanded==='true'&&await reader.locator('[data-activity-group^="owner/"]').count()>=2);
  await header.focus();await header.press('Enter');assertID('stage2-group-keyboard-'+mode,(await header.getAttribute('aria-expanded'))==='false');await header.press('Enter');
  for(const scheme of ['light','dark']){
    await reader.emulateMedia({colorScheme:scheme,contrast:'more'});
    const colors=await header.evaluate(el=>({text:getComputedStyle(el).color,background:getComputedStyle(el).backgroundColor}));
    const lum=color=>{const rgb=color.match(/[\d.]+/g).slice(0,3).map(x=>Number(x)/255).map(v=>v<=0.04045?v/12.92:((v+0.055)/1.055)**2.4);return rgb[0]*0.2126+rgb[1]*0.7152+rgb[2]*0.0722;};
    const a=lum(colors.text),b=lum(colors.background),ratio=(Math.max(a,b)+0.05)/(Math.min(a,b)+0.05);assertID('stage2-group-contrast-'+mode+'-'+scheme,ratio>=4.5);
    manifest['stage2-group-contrast-'+mode+'-'+scheme]={...colors,ratio};await screenshot(reader,'stage2-groups-'+mode+'-'+scheme+'.png');
  }
  await reader.emulateMedia({colorScheme:'light',contrast:'no-preference'});
  await screenshot(reader,'stage2-groups-'+mode+'.png');await open(reader,received);await reader.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
  const text='Identical shared message '+mode;
  for(let i=0;i<2;i++){await reader.locator('#prompt').fill(text);await reader.locator('#sendbtn').click();await until(async()=>await reader.locator('#msgs .gmsg .bubble').filter({hasText:text}).count()===i+1,30000,'duplicate publication');}
  const duplicate=reader.locator('#msgs .gmsg').filter({hasText:text}).last(),seq=Number(await duplicate.getAttribute('data-cseq'));const bubble=duplicate.locator('.bubble');
  await bubble.focus();await bubble.press('Enter');const actions=reader.locator('#msgs [data-gactions]');
  const order=await actions.locator('[data-gact]').evaluateAll(els=>els.map(el=>el.dataset.gact).filter(x=>['fork','sendto','desc','bookmark','copy'].includes(x)));
  assertID('stage2-shared-actions-time-'+mode,JSON.stringify(order)===JSON.stringify(['fork','sendto','desc','bookmark','copy'])&&await actions.locator('.bubwhen').count()===1&&await duplicate.locator('.gwho').innerText()==='You');
  await actions.locator('[data-gact=bookmark]').click();const saved=await until(async()=>{const ui=await readerSeat.api('/api/ui');return (ui.bookmarks||[]).find(x=>x.team?.chat===sub.chat&&x.team?.generation===sub.generation&&x.team?.log==='comments'&&x.team.seq===seq);},30000,'exact shared bookmark');
  assertID('stage2-bookmark-exact-identity-'+mode,saved.text===text&&seq>1);
  await duplicate.locator('[data-bmarker=bookmark]').waitFor({timeout:30000});assertID('stage2-bookmark-marker-'+mode,await reader.locator('#msgs .gmsg').filter({hasText:text}).first().locator('[data-bmarker=bookmark]').count()===0);
  await actions.locator('[data-gact=fork]').click();await reader.locator('#newchat.open').waitFor({timeout:10000});
  const draft=await reader.locator('#nc_prompt').inputValue();assertID('stage2-fork-explicit-send-'+mode,draft.includes(text)&&await reader.locator('#nc_start').getAttribute('aria-label')==='Start and send'&&JSON.stringify(counts(readerSeat))===JSON.stringify(beforeReader));
  await reader.locator('#nc_title').fill('Fresh shared fork '+mode);await reader.locator('#nc_agent').selectOption('codex');await reader.locator('#nc_dir').fill(readerSeat.data);
  const createdResponse=reader.waitForResponse(response=>response.request().method()==='POST'&&response.url().endsWith('/api/nodes')&&response.status()===200,{timeout:30000});
  await reader.locator('#nc_start').click();const response=await createdResponse,body=response.request().postDataJSON(),created=await response.json();readerSeat.nodes.push(created.id);
  await until(()=>counts(readerSeat).accepted===beforeReader.accepted+1,30000,'fresh local fork explicit send');
  const afterFork=counts(readerSeat);assertID('stage2-fork-fresh-local-'+mode,!body.parent&&body.prompt.includes(text)&&afterFork.attempted===beforeReader.attempted+1&&afterFork.controls===beforeReader.controls);
  const forwarded=await until(async()=>{const ui=await readerSeat.api('/api/ui');return (ui.forward_links||[]).find(x=>x.source?.server===sub.server&&x.source?.owner===sub.owner&&x.source?.chat===sub.chat&&x.source?.generation===sub.generation&&x.source?.log==='comments'&&x.source?.seq===seq&&x.destination?.node===created.id);},40000,'confirmed shared source link');
  assertID('stage2-fork-source-link-'+mode,forwarded.source.seq===seq);
  if(mode==='phone')await reader.locator('#chatback').click();await reader.locator('[data-tab=current]').click();await open(reader,readerSeat.node.id);await reader.locator('#bookmarksbtn').click();
  const bookmark=reader.locator('#bookmarklist .bookmark').filter({hasText:text}).last();await bookmark.locator('.nbubble').click();await bookmark.locator('[data-bmact=jump]').click();
  await reader.locator('#msgs [data-cseq="'+seq+'"].jumpflash').waitFor({timeout:30000});
  assertID('stage2-bookmark-exact-jump-'+mode,await reader.locator('#msgs [data-cseq="'+seq+'"].jumpflash').count()===1);
  await duplicate.locator('[data-bmarker=sendto]').waitFor({timeout:30000});assertID('stage2-source-marker-'+mode,await duplicate.locator('[data-bmarker=sendto]').count()===1);
  await screenshot(reader,'stage2-bookmark-jump-'+mode+'.png');
  await reader.context().grantPermissions(['clipboard-read','clipboard-write']);await duplicate.locator('.bubble').click();await reader.locator('[data-gact=copy]').click();
  assertID('stage2-copy-shared-'+mode,(await reader.evaluate(()=>navigator.clipboard.readText()))===text);
  const destination=(await readerSeat.api('/api/state')).nodes.find(n=>n.id===readerSeat.node.id&&n.instance_id===readerSeat.node.instance_id);
  assert.ok(destination?.title,'selected local destination instance still exists');
  await reader.locator('[data-gact=desc]').click();await reader.locator('#sendto_list').getByRole('button',{name:destination.title,exact:true}).click();
  await reader.locator('#chatdescinput').waitFor({state:'visible',timeout:30000});
  assertID('stage2-description-local-choice-'+mode,await reader.evaluate(()=>localStorage.getItem('scimux-sel'))===destination.id&&(await reader.locator('#chattitle').innerText())===destination.title&&(await reader.locator('#chatdescinput').inputValue()).includes(text)&&JSON.stringify(counts(readerSeat))===JSON.stringify(afterFork));
  if((await reader.locator('#bookmarksbtn').getAttribute('aria-label'))==='open bookmarks')await reader.locator('#bookmarksbtn').click();
  if(!await bookmark.locator('[data-bmact=jump]').isVisible())await bookmark.locator('.nbubble').click();await bookmark.locator('[data-bmact=jump]').click();await reader.locator('#msgs [data-cseq="'+seq+'"].jumpflash').waitFor({timeout:30000});
  const relayBefore=(await readerSeat.api('/api/team/subscriptions/'+key+'/comments?after=0')).entries.length;
  if((await reader.locator('#bookmarksbtn').getAttribute('aria-label'))==='open bookmarks')await reader.locator('#bookmarksbtn').click();
  if(!await bookmark.locator('[data-bmact=comment]').isVisible())await bookmark.locator('.nbubble').click();await bookmark.locator('[data-bmact=comment]').click();
  const annotation='Local annotation '+mode;await reader.locator('#bookmarkprompt').fill(annotation);await reader.locator('#bookmarksend').click();
  const localComment=await until(async()=>{const ui=await readerSeat.api('/api/ui');return (ui.bookmarks||[]).find(x=>x.text===annotation&&x.anchor===saved.t);},30000,'local bookmark annotation');
  assertID('stage2-bookmark-comment-local-'+mode,!!localComment&&(await readerSeat.api('/api/team/subscriptions/'+key+'/comments?after=0')).entries.length===relayBefore);
  await open(owner,ownerSeat.node.id);const source=owner.locator('#msgs .gmsg[data-cseq="'+seq+'"] .bubble');await source.waitFor({timeout:40000});await source.click();
  await owner.locator('[data-gact=bookmark]').click();const ownerSaved=await until(async()=>{const ui=await ownerSeat.api('/api/ui');return (ui.bookmarks||[]).find(x=>x.team?.chat===sub.chat&&x.team?.generation===sub.generation&&x.team?.log==='comments'&&x.team.seq===seq);},30000,'owner shared bookmark');
  await owner.locator('#bookmarksbtn').click();const ownerBookmark=owner.locator('#bookmarklist .bookmark').filter({hasText:text}).last();await ownerBookmark.locator('.nbubble').click();await ownerBookmark.locator('[data-bmact=jump]').click();
  await owner.locator('#msgs .gmsg[data-gkey="'+key+'"][data-cseq="'+seq+'"].jumpflash').waitFor({timeout:30000});assertID('stage2-owner-bookmark-exact-jump-'+mode,ownerSaved.team.seq===seq);
  await source.click();
  await owner.locator('[data-gact=prompt]').click();assertID('stage2-prompt-prepares-only-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(beforeOwner)&&(await owner.locator('#prompt').innerText()).includes(text));
  const submitted='Explicit shared prompt '+mode;await owner.locator('#prompt').fill(submitted);await owner.locator('#sendbtn').click();await until(()=>counts(ownerSeat).accepted===beforeOwner.accepted+1,30000,'explicit owner send');
  assertID('stage2-prompt-one-dispatch-'+mode,counts(ownerSeat).attempted===beforeOwner.attempted+1&&counts(ownerSeat).controls===beforeOwner.controls&&JSON.stringify(counts(readerSeat))===JSON.stringify(afterFork)&&JSON.stringify(counts(otherSeat))===JSON.stringify(beforeOther));
  await owner.locator('#destinationpeople').click();const ownText='Owner discussion source '+mode;
  await owner.locator('#prompt').fill(ownText);await owner.locator('#sendbtn').click();
  const ownTurn=owner.locator('#msgs .gmsg').filter({hasText:ownText}).last();await ownTurn.waitFor({timeout:30000});
  assertID('stage2-own-source-label-'+mode,(await ownTurn.locator('.gwho').innerText())==='You');
  const ownSeq=Number(await ownTurn.getAttribute('data-cseq'));await ownTurn.locator('.bubble').click();await owner.locator('[data-gact=prompt]').click();
  const ownSource=await owner.evaluate(id=>JSON.parse(localStorage.getItem('scimux-prompt-sources:'+id)||'[]'),ownerSeat.node.id);
  assertID('stage2-own-source-provenance-'+mode,ownSource.some(source=>source.seq===ownSeq&&source.role==='user'&&!source.who));
  assertID('stage2-own-source-no-dispatch-'+mode,counts(ownerSeat).attempted===beforeOwner.attempted+1&&counts(ownerSeat).controls===beforeOwner.controls);
  manifest['stage2-shared-'+mode]={key,seq,bookmark:saved.team,beforeReader,afterReader:counts(readerSeat),beforeOwner,afterOwner:counts(ownerSeat),beforeOther,afterOther:counts(otherSeat)};
}

async function stage2BookmarkPages(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Bookmark pages '+mode,'Synthetic shared pagination setup');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+chat.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid&&x.generation===share.generation),30000,'pagination subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/');
  for(let i=0;i<257;i++)await readerSeat.api('/api/team/subscriptions/'+key+'/comments','POST',{text:i===0||i===256?'Identical page boundary '+mode:'Synthetic page '+mode+' '+i,anchor:0,key:(mode==='desktop'?'aced1000':'aced2000')+i.toString(16).padStart(8,'0')});
  const reader=await pageFor(readerSeat,mode,'stage2-pages-'+mode),before=counts(readerSeat),pages=[];
  reader.on('response',response=>{if(response.url().includes('/api/team/subscriptions/'+key+'/comments?after='))pages.push(response.url());});
  await open(reader,'team/'+key);const target=reader.locator('#msgs .gmsg[data-cseq="257"]');await target.waitFor({timeout:60000});
  await target.locator('.bubble').click();await reader.locator('[data-gact=bookmark]').click();
  await until(async()=>(await readerSeat.api('/api/ui')).bookmarks?.some(x=>x.team?.chat===share.uid&&x.team?.generation===share.generation&&x.team?.log==='comments'&&x.team.seq===257),30000,'page boundary bookmark');
  if(mode==='phone')await reader.locator('#chatback').click();await open(reader,readerSeat.node.id);
  pages.length=0;await reader.reload({waitUntil:'networkidle'});await reader.locator('#bookmarksbtn').click();
  const bookmark=reader.locator('#bookmarklist .bookmark').filter({hasText:'Identical page boundary '+mode}).last();await bookmark.locator('.nbubble').click();await bookmark.locator('[data-bmact=jump]').click();
  await reader.locator('#msgs .gmsg[data-cseq="257"].jumpflash').waitFor({timeout:60000});
  assertID('stage2-bookmark-pagination-'+mode,pages.some(url=>url.includes('after=256'))&&await reader.locator('#msgs .gmsg[data-cseq="1"].jumpflash').count()===0&&JSON.stringify(counts(readerSeat))===JSON.stringify(before));
  await screenshot(reader,'stage2-bookmark-pagination-'+mode+'.png');manifest['stage2-pages-'+mode]={key,pages,before,after:counts(readerSeat)};
}

async function stage1OwnerFiles(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Owner files '+mode,'Synthetic owner file setup');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const page=await pageFor(ownerSeat,mode,'stage1-owner-files-'+mode);await open(page,chat.id);
  await page.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});await page.locator('#prompt').fill('People draft '+mode);
  await page.locator('#destinationagent').click();await page.locator('#attadd').waitFor({state:'visible',timeout:30000});
  await page.locator('#attadd').click();const picker=page.waitForEvent('filechooser');await page.locator('#attmenu [data-src=file]').click();
  await (await picker).setFiles({name:'owner-file.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic delivered file')});
  await page.locator('#attstage .stagechip.done').waitFor({timeout:30000});
  await page.locator('#prompt').evaluate(el=>{const data=new DataTransfer();const bytes=Uint8Array.from(atob('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aX6sAAAAASUVORK5CYII='),c=>c.charCodeAt(0));data.items.add(new File([bytes],'paste.png',{type:'image/png'}));el.dispatchEvent(new ClipboardEvent('paste',{bubbles:true,cancelable:true,clipboardData:data}));});
  await page.locator('#attstage .stagechip.done[title="paste.png"] img').waitFor({timeout:30000});
  await page.locator('#promptbar').evaluate(el=>{const data=new DataTransfer();data.items.add(new File(['drop'],'drop.txt',{type:'text/plain'}));el.dispatchEvent(new DragEvent('drop',{bubbles:true,cancelable:true,dataTransfer:data}));});
  await page.locator('#attstage .stagechip.done[title="drop.txt"]').waitFor({timeout:30000});
  assertID('stage1-owner-paste-drop-preview-'+mode,await page.locator('#attstage .stagechip.done').count()===3);
  await page.locator('#attstage .stagechip[title="paste.png"] .sx').click();await page.locator('#attstage .stagechip[title="drop.txt"] .sx').click();
  assertID('stage1-owner-attachment-removal-'+mode,await page.locator('#attstage .stagechip').count()===1);
  const uploadPath='**/api/nodes/'+chat.id+'/attachments?*';await page.route(uploadPath,route=>route.fulfill({status:413,body:'Synthetic upload limit'}));
  const beforeFailure=counts(ownerSeat);await page.locator('#attfile').setInputFiles({name:'rejected.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic rejection')});
  const rejected=page.locator('#attstage .stagechip.err[title="rejected.txt"]');await rejected.locator('[role=status]').waitFor({timeout:30000});
  assertID('stage1-upload-error-visible-'+mode,(await rejected.locator('[role=status]').innerText()).includes('Synthetic upload limit')&&JSON.stringify(counts(ownerSeat))===JSON.stringify(beforeFailure));
  await screenshot(page,'upload-error-'+mode+'.png');await rejected.locator('.sx').click();await page.unroute(uploadPath);
  assertID('stage1-upload-error-preserves-stage-'+mode,await page.locator('#attstage .stagechip.done[title="owner-file.txt"]').count()===1);
  await page.locator('#destinationpeople').click();let dialog=page.waitForEvent('dialog');
  const paste=page.locator('#prompt').evaluate(el=>{const data=new DataTransfer();data.items.add(new File(['ignore'],'people.txt',{type:'text/plain'}));el.dispatchEvent(new ClipboardEvent('paste',{bubbles:true,cancelable:true,clipboardData:data}));});
  const warning=await dialog;assertID('stage1-people-file-feedback-'+mode,/text only/i.test(warning.message()));await warning.accept();await paste;
  dialog=page.waitForEvent('dialog');const drop=page.locator('#promptbar').evaluate(el=>{const data=new DataTransfer();data.items.add(new File(['ignore'],'people-drop.txt',{type:'text/plain'}));el.dispatchEvent(new DragEvent('drop',{bubbles:true,cancelable:true,dataTransfer:data}));});
  const dropWarning=await dialog;assertID('stage1-people-drop-feedback-'+mode,/text only/i.test(dropWarning.message()));await dropWarning.accept();await drop;
  assertID('stage1-people-file-keeps-draft-'+mode,(await page.locator('#prompt').innerText())==='People draft '+mode);
  await page.locator('#destinationagent').click();assertID('stage1-owner-file-staged-'+mode,(await page.locator('#attstage').innerText()).includes('owner-file.txt'));
  await page.locator('#prompt').fill('Owner file prompt '+mode);
  const before=counts(ownerSeat),request=page.waitForRequest(req=>req.method()==='POST'&&new URL(req.url()).pathname==='/api/nodes/'+chat.id+'/send');
  await page.locator('#sendbtn').click();const body=(await request).postDataJSON();
  await until(()=>counts(ownerSeat).accepted===before.accepted+1,30000,'owner file delivered to fake transport');
  const after=counts(ownerSeat),records=jsonLines(path.join(ownerSeat.data,'counter'));const sent=records.at(-1)?.text||'';
  assertID('stage1-owner-file-delivered-'+mode,body.attachments?.length===1&&sent.includes(body.attachments[0].path)&&fs.readFileSync(body.attachments[0].path,'utf8')==='synthetic delivered file'&&after.attempted===before.attempted+1&&after.controls===before.controls);
  assertID('stage1-owner-file-disarms-'+mode,(await page.locator('#destinationpeople').getAttribute('aria-pressed'))==='true'&&(await page.locator('#prompt').innerText())==='People draft '+mode);
  manifest['stage1-owner-files-'+mode]={before,after};await screenshot(page,'stage1-owner-files-'+mode+'.png');
}
async function recreatedChatIsolation(mode,seat,readerSeat) {
 for(const pending of [false,true]) {
  const title='Synthetic recreated '+mode+' '+pending,old=await node(seat,'codex',title,'Synthetic old request');
  await seat.api('/api/team/shares/'+old.id,'PUT',{server:seat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const page=await pageFor(seat,mode,'recreated-'+mode+'-'+pending);await open(page,old.id);
  await page.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});await page.locator('#destinationagent').click();
  let held,finish;if(pending)await page.route('**/api/nodes/'+old.id+'/attachments?*',route=>{held=route;return new Promise(resolve=>{finish=resolve})});
  await page.locator('#attadd').waitFor({state:'visible'});await page.locator('#attadd').click();const picker=page.waitForEvent('filechooser');await page.locator('#attmenu [data-src=file]').click();
  await (await picker).setFiles({name:'old-synthetic.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic old file')});
  if(pending){await until(()=>!!held,10000,'held upload');await page.locator('#prompt').fill('Synthetic old pending prompt');await page.locator('#sendbtn').click();}
  else await page.locator('#attstage .stagechip.done').waitFor({timeout:30000});
  if(mode==='phone')await page.locator('#chatback').click();await open(page,seat.node.id);
  await seat.api('/api/nodes/'+old.id,'DELETE');seat.nodes=seat.nodes.filter(id=>id!==old.id);
  const fresh=await node(seat,'codex',title,'Synthetic replacement request');assertID('recreated-id-reused-'+mode+'-'+pending,fresh.id===old.id);
  await seat.api('/api/team/shares/'+fresh.id,'PUT',{server:seat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const afterCreate=counts(seat);
  if(pending){await held.continue();finish();await delay(2500);const current=await seat.api('/api/nodes/'+fresh.id+'/chat');
   assertID('recreated-no-old-dispatch-'+mode,JSON.stringify(counts(seat))===JSON.stringify(afterCreate)&&!(current.turns||[]).some(t=>t.role==='user'&&t.text.includes('Synthetic old pending prompt')));
  } else {
   if(mode==='phone')await page.locator('#chatback').click();await open(page,fresh.id);await page.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});await page.locator('#destinationagent').click();
   assertID('recreated-no-old-files-'+mode,!(await page.locator('#attstage').innerText()).includes('old-synthetic'));await screenshot(page,'recreated-attachment-'+mode+'.png');
  }
  manifest['recreated-'+mode+'-'+pending]={afterCreate,after:counts(seat)};
 }
}

async function bubbleSelection(mode,ownerSeat,readerSeat) {
 const drag=async(page,bubble,label)=>{const para=bubble.locator('p').first();await para.waitFor();
  await page.evaluate(()=>new Promise((resolve,reject)=>{let timer;const deadline=setTimeout(()=>{obs.disconnect();reject(Error('transcript did not settle'))},10000);const obs=new MutationObserver(()=>{clearTimeout(timer);timer=setTimeout(done,800)});function done(){clearTimeout(deadline);obs.disconnect();resolve()}obs.observe(document.querySelector('#msgs'),{childList:true,subtree:true});timer=setTimeout(done,800)}));
  await para.scrollIntoViewIfNeeded();
  const r=await until(async()=>{const box=await para.boundingBox(),view=page.viewportSize();if(!box||box.x<0||box.y<0||box.x+box.width>view.width||box.y+box.height>view.height)return null;const visible=await para.evaluate((el,b)=>[b.x+3,b.x+b.width-3].every(x=>{const hit=document.elementFromPoint(x,b.y+b.height/2);return hit===el||el.contains(hit)}),box);return visible?box:null},30000,'visible selection text bounds');
  assertID('bubble-selection-target-visible-'+mode+'-'+label,!!r);
  await page.mouse.move(r.x+3,r.y+r.height/2);await page.mouse.down();await page.mouse.move(r.x+r.width-3,r.y+r.height/2,{steps:12});await page.mouse.up();
  assertID('bubble-selection-real-pointer-'+mode+'-'+label,(await page.evaluate(()=>String(window.getSelection()))).trim().length>0);
  assertID('bubble-selection-no-actions-'+mode+'-'+label,await page.locator('#msgs .bubactions').count()===0);
 };
 const local=await pageFor(ownerSeat,mode,'bubble-selection-local-'+mode);await open(local,ownerSeat.node.id);
 await drag(local,local.locator('#msgs .turn.assistant .bubble').filter({hasText:'Synthetic response 1'}).first(),'local');
 const sub=(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(v=>v.owner===ownerSeat.self);assert.ok(sub,'synthetic share exists');
 const page=await pageFor(readerSeat,mode,'bubble-selection-shared-'+mode);await open(page,'team/'+[sub.server,sub.owner,sub.chat,sub.generation].join('/'));
 await drag(page,page.locator('#msgs .turn.assistant .bubble').filter({hasText:'Synthetic response 1'}).first(),'shared');await screenshot(page,'bubble-selection-shared-'+mode+'.png');
}

async function draftRecovery(mode,seat) {
 const page=await pageFor(seat,mode,'draft-recovery-'+mode);await open(page,seat.node.id);
 const chat=await node(seat,'codex','Synthetic recovery '+mode,'Synthetic recovery setup');
 const draft='Synthetic saved draft <review> '+mode;
 await page.evaluate(({id,text})=>{localStorage.removeItem('scimux-draft-owner:'+id);localStorage.setItem('scimux-draft:'+id,text)}, {id:chat.id,text:draft});
 const before=counts(seat);if(mode==='phone')await page.locator('#chatback').click();await open(page,chat.id);
 await page.locator('#draftrecovery summary').waitFor();
 assertID('recovery-does-not-prepare-'+mode,(await page.locator('#prompt').innerText())===''&&JSON.stringify(counts(seat))===JSON.stringify(before));
 await page.locator('#draftrecovery summary').click();const saved=page.locator('#draftrecovery textarea');
 assertID('recovery-explicit-review-'+mode,await saved.inputValue()===draft&&await saved.getAttribute('readonly')!==null);
 assertID('recovery-no-agent-dispatch-'+mode,JSON.stringify(counts(seat))===JSON.stringify(before));
 await screenshot(page,'draft-recovery-'+mode+'.png');manifest['draft-recovery-'+mode]={before,after:counts(seat)};
}

async function headerFocus(mode,seat) {
 const reader=seats[2];const targetNode=await node(seat,'codex','Synthetic focus target '+mode,'Synthetic shared focus target');
 await seat.api('/api/team/shares/'+targetNode.id,'PUT',{server:seat.serverID,scope:'all',members:[{seat:reader.self,access:'write'}]});
 const other=await node(seat,'codex','Synthetic focus holder '+mode,'Synthetic separate local chat');
 const page=await pageFor(seat,mode,'coordinator-header-focus-'+mode);await open(page,other.id);
 const header=page.locator('[data-activity-group="mine"]');await header.waitFor();await header.focus();
 assertID('coordinator-mine-header-focused-'+mode,await header.evaluate(el=>document.activeElement===el));
 const share=await seat.api('/api/team/shares/'+targetNode.id);
 const target={server:reader.serverID,owner:seat.self,chat:share.uid,generation:share.generation};
 const sub=await until(async()=>selectSharedSubscription((await reader.api('/api/team/subscriptions')).subscriptions,target),30000,'exact focus share');
 assert.equal(sub.access,'write','synthetic share is writable');const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/');
 const before=counts(seat);await reader.api('/api/team/subscriptions/'+key+'/comments','POST',{key:require('node:crypto').randomBytes(8).toString('hex'),anchor:0,text:'Synthetic incoming focus check'});
 await page.locator('[id="card-'+targetNode.id+'"] .activity-unread').waitFor({timeout:30000});
 const after=counts(seat);manifest['coordinator-header-focus-'+mode]={before,after,active:await page.evaluate(()=>({tag:document.activeElement.tagName,group:document.activeElement.dataset.activityGroup||''}))};
 await screenshot(page,'coordinator-header-focus-after-unread-'+mode+'.png');
 assertID('coordinator-mine-header-keeps-focus-after-unread-'+mode,await header.evaluate(el=>document.activeElement===el));
 assertID('coordinator-focus-comment-zero-dispatch-'+mode,JSON.stringify(before)===JSON.stringify(after));
 const received='team/'+key,readerPage=await pageFor(reader,mode,'sharer-header-focus-'+mode);
 await open(readerPage,received);await readerPage.locator('#msgs .gmsg').first().waitFor({timeout:30000});
 if(mode==='phone')await readerPage.locator('#chatback').click();await open(readerPage,reader.node.id);
 const sharer=readerPage.locator('[data-activity-group="owner/'+sub.server+'/'+sub.owner+'"]');await sharer.waitFor();await sharer.focus();
 assertID('sharer-header-focused-'+mode,await sharer.evaluate(el=>document.activeElement===el));
 const own=selectSharedSubscription((await seat.api('/api/team/subscriptions')).subscriptions,{...target,server:seat.serverID});
 assert.ok(own,'synthetic owner subscription exists');const ownKey=[own.server,own.owner,own.chat,own.generation].join('/');
 const beforeReader=counts(reader);await seat.api('/api/team/subscriptions/'+ownKey+'/comments','POST',{key:require('node:crypto').randomBytes(8).toString('hex'),anchor:0,text:'Synthetic sharer focus check '+mode});
 await readerPage.locator('[id="card-'+received+'"] .activity-unread').waitFor({timeout:30000});
 assertID('sharer-header-keeps-focus-after-unread-'+mode,await sharer.evaluate(el=>document.activeElement===el));
 assertID('sharer-focus-zero-dispatch-'+mode,JSON.stringify(beforeReader)===JSON.stringify(counts(reader))&&JSON.stringify(after)===JSON.stringify(counts(seat)));
 await screenshot(readerPage,'sharer-header-focus-after-unread-'+mode+'.png');
}

async function overlappingDescriptionChoices(mode,ownerSeat,readerSeat) {
 const a=await node(readerSeat,'codex','Choice A '+mode,'Synthetic destination A');
 const b=await node(readerSeat,'codex','Choice B '+mode,'Synthetic destination B');
 const share=await ownerSeat.api('/api/team/shares/'+ownerSeat.node.id);
 const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid&&x.generation===share.generation),30000,'choice-order subscription');
 const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),received='team/'+key,source='Synthetic choice-order source '+mode;
 await readerSeat.api('/api/team/subscriptions/'+key+'/comments','POST',{key:require('node:crypto').randomBytes(8).toString('hex'),anchor:0,text:source});
 const page=await pageFor(readerSeat,mode,'description-choice-order-'+mode);await open(page,received);
 const bubble=page.locator('#msgs .gmsg .bubble').filter({hasText:source}).first();await bubble.waitFor({timeout:30000});await bubble.click();
 await page.waitForResponse(r=>new URL(r.url()).pathname==='/api/state'&&r.request().headers()['if-none-match'],{timeout:30000});
 const held=[];await page.route('**/api/state',async route=>{
  if(route.request().headers()['if-none-match']) {await route.continue();return;}
  const response=await route.fetch();let release;const wait=new Promise(resolve=>release=resolve);heldReleases.add(release);held.push({release});
  try {await wait;if(!page.isClosed())await route.fulfill({response});}finally{heldReleases.delete(release)}
 });
 try {
  const before=counts(readerSeat);
  await page.locator('[data-gact=desc]').click();await page.locator('#sendto_list [data-choice]').filter({hasText:a.title}).click();await until(()=>held.length===1,10000,'first description verification');
  await page.locator('[data-gact=desc]').click();await page.locator('#sendto_list [data-choice]').filter({hasText:b.title}).click();await until(()=>held.length===2,10000,'second description verification');
  const first=page.waitForResponse(r=>new URL(r.url()).pathname==='/api/state'&&!r.request().headers()['if-none-match']);held[0].release();await first;await page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));
  assertID('choice-order-old-cannot-navigate-'+mode,await page.evaluate(()=>localStorage.getItem('scimux-sel'))===received&&await page.locator('#msgs .gmsg').filter({hasText:source}).count()===1);
  assertID('choice-order-no-old-editor-'+mode,!await page.locator('#chatdescinput').isVisible());await screenshot(page,'description-choice-order-pending-'+mode+'.png');
  const second=page.waitForResponse(r=>new URL(r.url()).pathname==='/api/state'&&!r.request().headers()['if-none-match']);held[1].release();await second;await page.locator('#chatdescinput').waitFor({state:'visible',timeout:30000});
  assertID('choice-order-new-destination-'+mode,await page.evaluate(()=>localStorage.getItem('scimux-sel'))===b.id&&(await page.locator('#chattitle').innerText())===b.title&&(await page.locator('#chatdescinput').inputValue()).includes(source));
  assertID('choice-order-zero-dispatch-'+mode,JSON.stringify(before)===JSON.stringify(counts(readerSeat)));manifest['description-choice-order-'+mode]={before,after:counts(readerSeat),a:a.id,b:b.id};await screenshot(page,'description-choice-order-applied-'+mode+'.png');
 } finally {for(const item of held)item.release();await page.unrouteAll({behavior:'wait'});}
}
async function recreatedDescriptionChoice(mode,ownerSeat,readerSeat) {
 const title='Synthetic description destination '+mode;
 const old=await node(readerSeat,'codex',title,'Synthetic original description');
 const share=await ownerSeat.api('/api/team/shares/'+ownerSeat.node.id);
 const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid&&x.generation===share.generation),30000,'description subscription');
 const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),received='team/'+key;
 await readerSeat.api('/api/team/subscriptions/'+key+'/comments','POST',{key:require('node:crypto').randomBytes(8).toString('hex'),anchor:0,text:'Synthetic description source '+mode});
 const page=await pageFor(readerSeat,mode,'description-recreated-choice-'+mode);await open(page,received);
 const bubble=page.locator('#msgs .gmsg .bubble').filter({hasText:'Synthetic description source '+mode}).first();await bubble.waitFor({timeout:30000});
 const clickURL=page.url(),events=[];const navigated=frame=>{if(frame===page.mainFrame())events.push({type:'navigation',url:frame.url()});};const dialog=d=>{events.push({type:'dialog',message:d.message()});d.dismiss().catch(()=>{});};page.on('framenavigated',navigated);page.on('dialog',dialog);
 let clickState;
 try {await bubble.click();} finally {
  page.off('framenavigated',navigated);page.off('dialog',dialog);
  try {clickState=await Promise.race([page.evaluate(mode=>{const turn=[...document.querySelectorAll('#msgs .gmsg')].find(el=>el.textContent.includes('Synthetic description source '+mode));return {selected:localStorage.getItem('scimux-sel'),source:turn?.dataset.cseq||'',action:!!turn?.nextElementSibling?.matches('[data-gactions]'),rows:document.querySelectorAll('#msgs [data-gactions]').length};},mode),delay(3000).then(()=>({error:'click state unavailable'}))]);}catch(e){clickState={error:String(e)};}
  manifest['description-choice-click-'+mode]={initialURL:clickURL,currentURL:page.url(),events,state:clickState};
 }
 assertID('description-choice-source-action-'+mode,page.url()===clickURL&&events.length===0&&clickState.selected===received&&!!clickState.source&&clickState.action&&clickState.rows===1);
 await page.locator('[data-gact=desc]').click();
 const choice=page.locator('#sendto_list [data-choice]').filter({hasText:title});await choice.waitFor();
 await readerSeat.api('/api/nodes/'+old.id,'DELETE');readerSeat.nodes=readerSeat.nodes.filter(id=>id!==old.id);
 const fresh=await node(readerSeat,'codex',title,'Synthetic replacement description');
 assertID('description-choice-reuses-id-'+mode,fresh.id===old.id&&fresh.instance_id!==old.instance_id);
 const before=counts(readerSeat);await choice.click();await page.locator('#sendto.open').waitFor({state:'hidden',timeout:10000});
 await page.waitForTimeout(500);const state=await readerSeat.api('/api/state');const replacement=state.nodes.find(n=>n.id===fresh.id);
 assertID('description-choice-refuses-replacement-'+mode,await page.evaluate(()=>localStorage.getItem('scimux-sel'))===received&&await page.locator('#msgs .gmsg').count()>0&&replacement.instance_id===fresh.instance_id&&replacement.description!==(await bubble.innerText()));
 assertID('description-choice-zero-dispatch-'+mode,JSON.stringify(before)===JSON.stringify(counts(readerSeat)));
 manifest['description-recreated-choice-'+mode]={before,after:counts(readerSeat),old:old.instance_id,fresh:fresh.instance_id};await screenshot(page,'description-recreated-choice-'+mode+'.png');
}
async function descriptionSaveIsolation(mode,seat) {
 const first=await node(seat,'codex','Synthetic old description '+mode,'Synthetic first source');
 const second=await node(seat,'codex','Synthetic new description '+mode,'Synthetic second source');
 const page=await pageFor(seat,mode,'coordinator-description-save-'+mode);await open(page,first.id);
 const a=page.locator('#msgs .turn.user:not(.echo) .bubble').first();await a.waitFor({timeout:30000});await a.click();await page.locator('[data-bact=desc]').click();
 await page.locator('#chatdescinput').fill('Synthetic old description text');
 let release,ready;const held=new Promise(resolve=>release=resolve),requested=new Promise(resolve=>ready=resolve);heldReleases.add(release);
 await page.route('**/api/nodes/'+first.id+'?*',async route=>{
  if(route.request().method()!=='PATCH'){await route.continue();return;}
  const response=await route.fetch();ready();await held;if(!page.isClosed())await route.fulfill({response});
 });
 try {
  const before=counts(seat);await page.locator('#infobtn').click();await Promise.race([requested,delay(20000).then(()=>{throw Error('description save was not requested');})]);
  if(mode==='phone')await page.locator('#chatback').click();await open(page,second.id);
  const b=page.locator('#msgs .turn.user:not(.echo) .bubble').first();await b.waitFor({timeout:30000});await b.click();await page.locator('[data-bact=desc]').click();
  const input=page.locator('#chatdescinput');await input.fill('Synthetic newer description text');
  assertID('coordinator-new-description-editor-ready-'+mode,await input.isVisible()&&(await input.inputValue())==='Synthetic newer description text');
  const done=page.waitForResponse(response=>response.request().method()==='PATCH'&&new URL(response.url()).pathname==='/api/nodes/'+first.id);release();heldReleases.delete(release);await done;
  await page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));
  const state={visible:await input.isVisible(),value:await input.inputValue(),before,after:counts(seat)};manifest['coordinator-description-save-'+mode]=state;
  await screenshot(page,'coordinator-description-after-late-save-'+mode+'.png');
  assertID('coordinator-description-late-response-isolated-'+mode,state.visible&&state.value==='Synthetic newer description text');
  assertID('coordinator-description-zero-dispatch-'+mode,JSON.stringify(state.before)===JSON.stringify(state.after));
 } finally {release();heldReleases.delete(release);await page.unrouteAll({behavior:'wait'});}
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
  await page.locator('#msgs').getByText('Synthetic response 2',{exact:true}).waitFor({timeout:30000});
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
  let lastSample,colors;
  try { colors=await until(async()=>{
    lastSample=await page.evaluate(()=>{const el=[...document.querySelectorAll('#msgs .turn.user.media .gwho')].at(-1),head=document.querySelector('#chathead');const rect=el?.getBoundingClientRect();return {connected:!!el?.isConnected&&!!head?.isConnected,visible:!!rect&&rect.width>0&&rect.height>0&&getComputedStyle(el).visibility==='visible',text:el?getComputedStyle(el).color:null,background:head?getComputedStyle(head).backgroundColor:null,caption:el?.innerText||'',selected:localStorage.getItem('scimux-sel')||'',title:document.querySelector('#chattitle')?.innerText||''};});
    const sample=contrastSample(lastSample);return sample&&sample.selected===chat.id&&sample.title===chat.title?sample:null;
  },10000,'connected media contrast sample'); } catch(e) {throw Error(`${e.message}; sample=${JSON.stringify(lastSample)}`);}
  assertID('media-caption-contrast-'+mode+'-'+scheme,colors.caption==='You'&&colors.contrast>=4.5);
  manifest['media-contrast-'+mode+'-'+scheme]=colors;await screenshot(page,'media-'+mode+'-'+scheme+'.png');
}
async function reshareStale(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Reshare '+mode,'Synthetic reshare initial');const sharePath='/api/team/shares/'+chat.id;
  await ownerSeat.api(sharePath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const initial=await ownerSeat.api(sharePath);const old=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===initial.uid&&x.generation===initial.generation),30000,'old generation');
  const oldKey=[old.server,old.owner,old.chat,old.generation].join('/');const page=await pageFor(readerSeat,mode,'reshare-reader-'+mode);await open(page,'team/'+oldKey);
  await page.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});const prior='Old generation comment '+mode;await page.locator('#prompt').fill(prior);await page.locator('#sendbtn').click();await page.locator('#msgs').getByText(prior,{exact:true}).waitFor({timeout:30000});
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
    await page.locator('#sendbtn').click();await page.locator('#msgs').getByText(draft,{exact:true}).waitFor({timeout:30000});
    assertID('reshare-new-discussion-once-'+mode,await page.locator('#msgs').getByText(draft,{exact:true}).count()===1&&JSON.stringify(counts(ownerSeat))===JSON.stringify(before));
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
  await owner.route('**/api/nodes/'+chat.id+'/send?*',async route=>{try{await route.fetch();ready();await held;if(!owner.isClosed())await route.abort('failed');}catch(e){if(!released&&!shutting&&!owner.isClosed())throw e;}});
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
  const releasePath=path.join(ownerSeat.data,'counter.release-dispatch');try{fs.unlinkSync(releasePath);}catch(e){if(e.code!=='ENOENT')throw e;}
  const releaseDispatch=()=>fs.writeFileSync(releasePath,'release');heldReleases.add(releaseDispatch);
  const prompt='Synthetic held evidence prompt '+mode+' [hold-dispatch]';await owner.locator('#prompt').fill(prompt);
  let releasePost,postReady,releaseChat,chatReady,postReleased=false,chatReleased=false,postCount=0,postStatus;
  const heldPost=new Promise(resolve=>releasePost=resolve),requestedPost=new Promise(resolve=>postReady=resolve);
  const heldChat=new Promise(resolve=>releaseChat=resolve),requestedChat=new Promise(resolve=>chatReady=resolve);
  heldReleases.add(releasePost);heldReleases.add(releaseChat);
  const sendPath='/api/nodes/'+chat.id+'/send',chatPath='/api/nodes/'+chat.id+'/chat';
  await owner.route(url=>url.pathname===sendPath,async route=>{if(route.request().method()!=='POST'){await route.continue();return;}try{postCount++;const response=await route.fetch();postStatus=response.status();postReady();await heldPost;if(!owner.isClosed())await route.fulfill({response});}catch(e){if(!postReleased&&!shutting&&!owner.isClosed())throw e;}});
  const before=counts(ownerSeat);
  try {
    await owner.locator('#sendbtn').click();
    await Promise.race([requestedPost,delay(20000).then(()=>{throw Error('send response not held');})]);
    await source.locator('[aria-label="Awaiting prompt delivery"]').waitFor({timeout:15000});
    assert.equal(counts(ownerSeat).accepted,before.accepted,'synthetic agent is still delaying acceptance');
    assertID('held-evidence-http-pending-'+mode,postCount===1&&postStatus===200&&await owner.locator('#destinationpeople[aria-pressed=true]').count()===1);
    await screenshot(owner,'held-evidence-pending-'+mode+'.png');
    await owner.route(url=>url.pathname===chatPath,async route=>{try{const response=await route.fetch();chatReady();await heldChat;if(!owner.isClosed())await route.fulfill({response});}catch(e){if(!chatReleased&&!shutting&&!owner.isClosed())throw e;}});
    releaseDispatch();heldReleases.delete(releaseDispatch);
    await until(()=>counts(ownerSeat).accepted===before.accepted+1,15000,'held evidence accepted');
    await Promise.race([requestedChat,delay(20000).then(()=>{throw Error('later evidence not requested');})]);
    await owner.locator('#destinationpeople').click();await owner.locator('#prompt').fill('New people while awaiting '+mode);
    postReleased=true;releasePost();heldReleases.delete(releasePost);
    await source.locator('[aria-label="Accepted; awaiting confirmation"]').waitFor({timeout:10000});
    assertID('held-evidence-accepted-not-confirmed-'+mode,await owner.locator('[aria-label="Awaiting prompt delivery"]').count()===0&&await owner.locator('[aria-label="Sent as prompt"]').count()===0);
    const during=counts(ownerSeat);assertID('held-evidence-awaiting-not-sent-'+mode,await owner.locator('[aria-label="Sent as prompt"]').count()===0&&during.attempted===before.attempted+1&&during.accepted===before.accepted+1&&during.controls===before.controls);
    await screenshot(owner,'held-evidence-accepted-'+mode+'.png');
    chatReleased=true;releaseChat();heldReleases.delete(releaseChat);await owner.unrouteAll({behavior:'wait'});
    await source.locator('[aria-label="Sent as prompt"]').waitFor({timeout:30000});
    assertID('held-evidence-confirmed-after-release-'+mode,(await owner.locator('#prompt').innerText())==='New people while awaiting '+mode&&JSON.stringify(counts(ownerSeat))===JSON.stringify(during));
    await screenshot(owner,'held-evidence-confirmed-'+mode+'.png');manifest['held-evidence-'+mode]={before,during,after:counts(ownerSeat),postCount,postStatus};
  } finally {releaseDispatch();heldReleases.delete(releaseDispatch);postReleased=true;releasePost();heldReleases.delete(releasePost);chatReleased=true;releaseChat();heldReleases.delete(releaseChat);await owner.unrouteAll({behavior:'wait'});}
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
  const comment='Synthetic retained human '+mode;let release,ready,released=false;
  const held=new Promise(resolve=>release=resolve),requested=new Promise(resolve=>ready=resolve);heldReleases.add(release);
  await page.route('**/api/team/subscriptions/'+key+'/comments',async route=>{try{ready();await held;if(!page.isClosed())await route.continue();}catch(e){if(!released&&!shutting&&!page.isClosed())throw e;}});
  try {
    await page.locator('#prompt').fill(comment);await page.locator('#sendbtn').click();
    await Promise.race([requested,delay(20000).then(()=>{throw Error('publication response not held');})]);
    assertID('permission-pending-input-is-not-publication-'+mode,(await page.locator('#prompt').innerText())===comment&&await page.locator('#msgs .bubble').getByText(comment,{exact:true}).count()===0);
    released=true;release();heldReleases.delete(release);await page.unrouteAll({behavior:'wait'});
    await page.locator('#msgs .bubble').getByText(comment,{exact:true}).waitFor({timeout:30000});
    await until(async()=>(await readerSeat.api('/api/team/subscriptions/'+key+'/comments?after=0')).entries.some(entry=>entry.text===comment),30000,'published comment retained in recipient copy');
  } finally {released=true;release();heldReleases.delete(release);await page.unrouteAll({behavior:'wait'});}
  const before=counts(ownerSeat),draft='Preserve draft on read '+mode;await page.locator('#prompt').fill(draft);
  await ownerSeat.api(sharePath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'read'}]});
  await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.some(x=>x.chat===share.uid&&x.access==='read'),30000,'read permission propagated');
  await page.waitForFunction(()=>document.querySelector('#prompt').getAttribute('contenteditable')!=='true',null,{timeout:30000});
  const denied=await readerSeat.status('/api/team/subscriptions/'+key+'/comments',{text:'Synthetic denied',anchor:0,key:'abcdefaa00000001'});
  assertID('permission-read-draft-and-denial-'+mode,denied===403&&(await page.locator('#prompt').innerText())===draft);
  await ownerSeat.api(sharePath,'DELETE');
  await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.some(x=>x.chat===share.uid&&x.state==='ended'),30000,'ended copy propagated');
  await page.locator('#msgs .bubble').getByText(comment,{exact:true}).waitFor({timeout:30000});
  manifest['permission-ended-debug-'+mode]={count:await page.locator('#msgs .bubble').getByText(comment,{exact:true}).count(),before,after:counts(ownerSeat)};
  assertID('permission-ended-copy-retained-'+mode,await page.locator('#msgs .bubble').getByText(comment,{exact:true}).count()===1&&JSON.stringify(counts(ownerSeat))===JSON.stringify(before));
  const source=owner.locator('#msgs .bubble').filter({hasText:comment}).first();await source.waitFor({timeout:30000});await source.click();
  await owner.locator('[data-gact=prompt]').click();
  assertID('permission-ended-owner-prompt-no-dispatch-'+mode,(await owner.locator('#prompt').innerText()).includes(comment)&&JSON.stringify(counts(ownerSeat))===JSON.stringify(before));
  await ownerSeat.api('/api/nodes/'+chat.id+'/exit','POST',{});
  await until(async()=>(await ownerSeat.api('/api/state')).nodes.find(x=>x.id===chat.id)?.ended_at,30000,'owner node closed');
  await owner.reload({waitUntil:'networkidle'});await owner.locator('#msgs').getByText(comment,{exact:true}).waitFor({timeout:30000});
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
  const before=counts(ownerSeat);await owner.locator('#msgs').getByText('Synthetic response 1',{exact:true}).first().click();await owner.locator('[data-bact=prompt]').click();
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
  await reader.locator('#msgs').getByText('Synthetic response 1',{exact:true}).waitFor({timeout:30000});
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
  await owner.locator('#msgs').getByText(discussion,{exact:true}).waitFor({timeout:40000});
  assertID('trace-original-once-'+mode,(await owner.locator('#msgs').getByText(discussion,{exact:true}).count())===1&&(await owner.locator('#chattitle').innerText())==='trace '+mode);
  assertID('trace-agent-turn-alongside-'+mode,await owner.locator('#msgs').getByText('Synthetic response 1',{exact:true}).count()===1);
  const log=fs.readFileSync(path.join(ownerSeat.data,'sessions',chat.id+'.jsonl'),'utf8');
  assertID('trace-human-not-agent-log-'+mode,!log.includes(discussion));
  for(const s of seats)assert.deepEqual(counts(s),before[s.name]);
  const readerSource=reader.locator('#msgs .bubble').filter({hasText:discussion}).first();await readerSource.click();await reader.locator('[data-gact=bookmark]').click();
  const bookmark=await until(async()=>{const u=await readerSeat.api('/api/ui');return (u.bookmarks||[]).find(x=>x.text===discussion&&x.team?.chat===sub.chat&&x.team?.generation===sub.generation);},20000,'received bookmark saved');
  assertID('trace-bookmark-full-address-'+mode,bookmark.team.server===sub.server&&bookmark.team.owner===sub.owner&&bookmark.team.log==='comments'&&bookmark.team.seq>0&&bookmark.role==='user');
  const ownerComment='Owner human for rename '+mode;await owner.locator('#prompt').fill(ownerComment);await owner.locator('#sendbtn').click();
  const ownerHuman=owner.locator('#msgs .turn.gmsg .bubble').filter({hasText:ownerComment});await ownerHuman.locator('.gwho').getByText('You',{exact:true}).waitFor({timeout:40000});assertID('trace-owner-people-provenance-'+mode,true);
  const otherPerson=reader.locator('#msgs .bubble').filter({hasText:ownerComment}).first();await otherPerson.waitFor({timeout:40000});await otherPerson.click();await reader.locator('[data-gact=more]').click();await reader.locator('#sendto_list [data-choice="0"]').click();await reader.locator('[data-grename]').fill('Synthetic renamed person');await reader.locator('[data-gact=name-save]').click();
  assertID('trace-rename-person-'+mode,await reader.getByText('Synthetic renamed person',{exact:false}).count()>0);
  await readerSource.click();await reader.locator('[data-gact=sendto]').click();await reader.locator('#sendto_list [data-fwd="'+readerSeat.node.id+'"]').click();
  assertID('trace-sendto-own-agent-draft-'+mode,(await reader.locator('#chattitle').innerText())===readerSeat.name+' chat'&&(await reader.locator('#prompt').innerText()).includes(discussion));
  for(const s of seats)assert.deepEqual(counts(s),before[s.name]);
  await reader.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});await reader.locator('#prompt').fill('');if(mode==='phone')await reader.locator('#chatback').click();await open(reader,received);
  const peopleDraft='Editing people during new data '+mode;await owner.locator('#prompt').fill(peopleDraft);
  await owner.locator('#prompt').evaluate(el=>{window.__tracePrompt=el;el.focus();const range=document.createRange();range.setStart(el.firstChild,5);range.collapse(true);const selection=window.getSelection();selection.removeAllRanges();selection.addRange(range);});
  const incoming='Incoming while editing '+mode;await reader.locator('#prompt').fill(incoming);await reader.locator('#sendbtn').click();
  await owner.locator('#msgs').getByText(incoming,{exact:true}).waitFor({timeout:40000});
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
  assertID('stage1-owner-send-disarms-'+mode,(await owner.locator('#destinationpeople').getAttribute('aria-pressed'))==='true'&&(await owner.locator('#prompt').innerText())==='Independent people draft '+mode);
  await screenshot(owner,'stage1-owner-disarmed-'+mode+'.png');
  await owner.locator('[aria-label="Sent as prompt"]').waitFor({timeout:30000});
  const afterSend=counts(ownerSeat);const submittedBubble=owner.locator('#msgs .turn[data-bk] .bubble').filter({hasText:edited});assertID('trace-owner-agent-provenance-'+mode,await submittedBubble.locator('.gwho').getByText('You',{exact:true}).count()>0);
  assertID('trace-explicit-one-dispatch-'+mode,afterSend.attempted===beforeSend.attempted+1&&afterSend.accepted===beforeSend.accepted+1&&afterSend.controls===beforeSend.controls);
  const link=await until(async()=>{const ui=await ownerSeat.api('/api/ui');return (ui.forward_links||[]).find(x=>x.kind==='prompt'&&x.destination?.node===chat.id);},20000,'confirmed source link persisted');
  const local=await ownerSeat.api('/api/nodes/'+chat.id+'/chat');
  assert.ok(link,'confirmed source link');
  const turn=local.turns.find(x=>x.uid===link.destination.uid&&(x.segment??0)===(link.destination.segment??0)&&x.record===link.destination.record);
  assertID('trace-confirmed-exact-address-'+mode,!!turn&&turn.text===edited&&local.turns.some((x,i)=>i>local.turns.indexOf(turn)&&x.role==='assistant'));
  manifest['trace-address-'+mode]={source:link.source,destination:link.destination,preview:link.preview,beforeSend,afterSend};
  await owner.locator('[aria-label="Sent as prompt"]').focus();
  const later='Incoming after confirmed marker '+mode;await reader.locator('#prompt').fill(later);await reader.locator('#sendbtn').click();
  await owner.locator('#msgs .gmsg').filter({hasText:later}).waitFor({timeout:30000});
  assertID('trace-confirmed-marker-keeps-focus-'+mode,await owner.evaluate(()=>document.activeElement?.getAttribute('aria-label')==='Sent as prompt')&&JSON.stringify(counts(ownerSeat))===JSON.stringify(afterSend));
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
  await owner.locator('#msgs .gmsg .bubble').last().click();await owner.locator('#msgs [data-gact=more]').click();await owner.locator('#sendto_list [data-choice="0"]').click();
  const editor=owner.locator('#msgs [data-grename]');await editor.fill('In progress person edit');await editor.evaluate(el=>{el.setSelectionRange(4,9);window.__traceEditor=el;});
  const editorAnchor=await editor.evaluate(el=>el.getBoundingClientRect().top);
  await readerSeat.api('/api/team/subscriptions/'+received.slice(5)+'/comments','POST',{text:'Poll while editing '+mode,anchor:mirror.next,key:(mode==='desktop'?'fa110000':'fa220000')+'00000001'});
  await owner.locator('#msgs').getByText('Poll while editing '+mode,{exact:true}).waitFor({timeout:30000});
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
  await owner.route('**/api/nodes/'+chat.id+'/attachments?*',async route=>{try{const response=await route.fetch();uploadReady();await uploadHeld;if(!owner.isClosed())await route.fulfill({response});}catch(e){if(!releasedUpload&&!shutting&&!owner.isClosed())throw e;}});
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
  await owner.route('**/api/nodes/'+chat.id+'/attachments?*',route=>route.abort('failed'));
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
  await owner.locator('#msgs').getByText(discussion,{exact:true}).waitFor({timeout:30000});
  const history=await ownerSeat.api('/api/nodes/'+chat.id+'/chat?history=1');
  assertID('trace-clear-history-seam-'+mode,(history.segments.length+(history.empty_segments||[]).length)>=2&&(await owner.locator('#msgs').getByText(discussion,{exact:true}).count())===1);
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
  await reader.locator('#msgs').getByText('Synthetic response 1',{exact:true}).waitFor({timeout:30000});
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
  let releaseSend;const heldSend=new Promise(resolve=>releaseSend=resolve);heldReleases.add(releaseSend);
  await owner.route('**/api/nodes/'+chat.id+'/send?*',async route=>{await heldSend;if(!owner.isClosed())await route.continue();});
  const before=counts(ownerSeat);
  try {
    await owner.locator('#sendbtn').click();await owner.locator('[aria-label="Awaiting prompt delivery"]').waitFor({timeout:10000});
    assertID('refusal-awaiting-not-sent-'+mode+'-'+newer,await owner.locator('[aria-label="Sent as prompt"]').count()===0);
  } finally {releaseSend();heldReleases.delete(releaseSend);await owner.unrouteAll({behavior:'wait'});}
  await owner.locator('#destinationpeople').click();const people='People editing '+mode+' '+newer;await owner.locator('#prompt').fill(people);
  if(newer){await owner.locator('#destinationagent').click();await owner.locator('#prompt').fill('Newer agent draft '+mode);await owner.locator('#destinationpeople').click();}
  const restoredResponse=await restored;const restoredBody=await restoredResponse.json();await delay(200);assertID('refusal-people-preserved-'+mode+'-'+newer,(await owner.locator('#prompt').innerText())===people);
  assertID('refusal-never-sent-'+mode+'-'+newer,await owner.locator('[aria-label="Sent as prompt"]').count()===0);
  await owner.locator('#destinationagent').click();assertID('refusal-agent-recovery-'+mode+'-'+newer,(await owner.locator('#prompt').innerText()).includes(newer?'Newer agent draft '+mode:rejected));
  const after=counts(ownerSeat);assertID('refusal-no-retry-'+mode+'-'+newer,after.attempted===before.attempted+1&&after.accepted===before.accepted&&after.controls===before.controls);
  manifest['refusal-'+mode+'-'+newer]={before,after,errorGET:restoredBody.error,restoreDraftGET:restoredBody.restore_draft||null};await screenshot(owner,'refusal-'+mode+'-'+newer+'.png');
}
const capturePage=createPageCapture(async(page,label)=>{
  if(captureProbeGate){captureProbeStarted=true;await captureProbeGate;}
  const sourceDir=path.resolve(__dirname,'../js');
  for(const entry of await page.coverage.stopJSCoverage()) {
      const url=new URL(entry.url);if(!/^\/js\/[a-z0-9-]+\.js$/.test(url.pathname))continue;
      const source=fs.readFileSync(path.join(sourceDir,path.basename(url.pathname)),'utf8');
      assert.equal(entry.source,source,'browser source matches checkout: '+url.pathname);
      coverageArchive.push({page:label,path:url.pathname,source,functions:entry.functions});
  }
});
async function captureCoverage(selectedPages=pages) {
  for (const {page,label} of selectedPages) {
    try {await capturePage(page,label);} catch(e){manifest.coverageError=String(e);throw e;}
  }
  fs.writeFileSync(artifacts('browser-coverage.json'),JSON.stringify(coverageArchive));
  if(!manifest.artifacts.includes('browser-coverage.json'))manifest.artifacts.push('browser-coverage.json');
}
async function runCompletedCase(fn) {
  const pageStart=pages.length,contextStart=contexts.length;
  await fn();
  const completedPages=pages.slice(pageStart),completedContexts=contexts.slice(contextStart);
  await retireCompletedContexts(completedPages,completedContexts,()=>captureCoverage(completedPages));
  pages.splice(pageStart);contexts.splice(contextStart);
}
async function terminate(c) {
  if(!c || c.exitCode!==null || c.signalCode!==null)return;
  c.kill('SIGTERM');await Promise.race([new Promise(r=>c.once('exit',r)),delay(4000)]);
  if(c.exitCode===null && c.signalCode===null){c.kill('SIGKILL');await Promise.race([new Promise(r=>c.once('exit',r)),delay(2000)]);}
}
async function performCleanup() {
  shutting=true;
  if(cleanupProbeGate)await cleanupProbeGate;
  for(const release of heldReleases)release();
  if(browser){try{await retireCompletedContexts(pages,contexts,captureCoverage);}catch(e){manifest.cleanup.push({coverage:'failed',error:String(e)});manifest.result='fail';process.exitCode=1;}try{await browser.close();}catch(e){manifest.cleanup.push({browserCloseError:String(e)});manifest.result='fail';process.exitCode=1;}}
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
const cleanup=createCleanupOnce(async()=>{try{await performCleanup();}catch(e){manifest.result='fail';manifest.cleanup.push({fatal:String(e)});process.exitCode=1;record();throw e;}});
for(const [sig,code] of [['SIGINT',130],['SIGTERM',143]])process.once(sig,()=>{signalCode=code;if(manifest.result!=='fail')manifest.result='interrupted';manifest.signal=sig;const finished=cleanup();cleanupProbeRelease?.();captureProbeRelease?.();finished.then(()=>{record();process.stdout.write('Artifacts: '+root+'\n',()=>process.exit(code));}).catch(e=>{process.stderr.write(String(e)+'\n',()=>process.exit(code));});});

async function run() {
 const selected=process.env.SCIMUX_E2E_SCENARIO||'default';
 if(!['default','stale-membership','stale-removed-membership','conversation-trace','conversation-native','conversation-refusal','conversation-core','early-native','native-start-failure','recipient-boundary','ai-source','ui-join','recipient-leave','permission-lifecycle','leave-empty','leave-stale','held-evidence','viewport-keyboard','dropped-send','reshare-stale','media-contrast','unshared','held-chat-loading','received-offline','stage1-scanner','stage1-unread','stage1-owner-files','stage2-shared-actions','stage2-bookmark-pages','recreated-chat-isolation','bubble-selection','draft-recovery','header-focus','description-save-isolation','description-recreated-choice','description-choice-order','no-selection','delete-recreation-isolation','legacy-closed-recreation','browser-lifecycle'].includes(selected))throw Error('unknown SCIMUX_E2E_SCENARIO');
 if(selected!=='default')manifest.scope=selected+' scenario only';
 relay=await launchRelay();
 for(const name of ['owner-a','owner-b','recipient','observer']) await makeSeat(name);
 const [a,b,r,observer]=seats;
 for(const s of seats)s.node=await node(s,'codex',s.name+' chat','Synthetic initial request');
 if(selected==='native-start-failure'){
  browser=await launchBrowser();
  for(const mode of ['desktop','phone']){const before=counts(a);await nativeStartupFailure(mode,a);assert.deepEqual(counts(a),before);assertID('native-start-failure-zero-dispatch-'+mode,true);}return;
 }
 if(selected==='no-selection'){browser=await launchBrowser();const empty=await makeSeat('no-selection');for(const mode of ['desktop','phone'])await noSelection(mode,empty);return;}
 if(selected==='legacy-closed-recreation'){browser=await launchBrowser();for(const mode of ['desktop','phone'])await legacyClosedRecreation(mode);return;}
 if(process.env.SCIMUX_E2E_FAIL_AFTER_START==='1')throw Error('controlled post-start failure');
 if(['failure','signal','overlap','capture-overlap'].includes(process.env.SCIMUX_E2E_CLEANUP_PROBE)) {
  manifest.scope='cleanup probe only';
  const native=await node(a,'claude','native cleanup probe','Synthetic native initial');
  if(process.env.SCIMUX_E2E_CLEANUP_PROBE==='failure')throw Error('controlled native cleanup failure');
  if(['overlap','capture-overlap'].includes(process.env.SCIMUX_E2E_CLEANUP_PROBE)){
   browser=await launchBrowser();const page=await pageFor(a,'desktop','cleanup-overlap');await open(page,native.id);await page.locator('#msgs').getByText('Synthetic native initial',{exact:true}).waitFor({timeout:30000});
   if(process.env.SCIMUX_E2E_CLEANUP_PROBE==='overlap'){
    cleanupProbeGate=new Promise(resolve=>cleanupProbeRelease=resolve);cleanup().catch(()=>{});await Promise.resolve();assert.ok(shutting,'cleanup has started');
   } else {
    captureProbeGate=new Promise(resolve=>captureProbeRelease=resolve);captureCoverage().catch(()=>{});await until(()=>captureProbeStarted,10000,'capture has started');
   }
   console.log('Cleanup overlap probe ready');await new Promise(()=>{});
  }
  console.log('Native cleanup probe ready');
  await new Promise(()=>{});
 }
 if(shutting)throw Error('runner shutting down');
 browser=await launchBrowser();
 if(selected==='browser-lifecycle'){
  for(const mode of ['desktop','phone']){
   const before=counts(a),label='lifecycle-'+mode;let page;
   await runCompletedCase(async()=>{page=await pageFor(a,mode,label);await open(page,a.node.id);
    await page.locator('#msgs').getByText('Synthetic response 1',{exact:true}).waitFor({timeout:30000});await screenshot(page,label+'.png');
    const pageSet=pages.slice(),contextSet=contexts.slice();await assert.rejects(retireCompletedContexts(pageSet,contextSet,async()=>{await captureCoverage(pageSet);throw Error('synthetic post-capture failure');}),/synthetic post-capture failure/);
    assertID('browser-lifecycle-failure-keeps-page-'+mode,!page.isClosed()&&pageSet.length===1&&contextSet.length===1);
   });
   assertID('browser-lifecycle-retains-coverage-'+mode,coverageArchive.some(item=>item.page===label&&item.path==='/js/chat.js')&&page.isClosed()&&pages.length===0&&contexts.length===0&&JSON.stringify(before)===JSON.stringify(counts(a)));
  }
  return;
 }
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
 if(selected==='stage1-scanner'){for(const mode of ['desktop','phone'])await stage1Scanner(mode,a,r);return;}
 if(selected==='stage1-unread'){for(const mode of ['desktop','phone'])await stage1Unread(mode,a,r);return;}
 if(selected==='stage1-owner-files'){for(const mode of ['desktop','phone'])await stage1OwnerFiles(mode,a,r);return;}
 if(selected==='stage2-shared-actions'){await r.api('/api/nodes/'+r.node.id,'PATCH',{title:'Synthetic renamed recipient'});for(const mode of ['desktop','phone'])await stage2SharedActions(mode,a,b,r);return;}
 if(selected==='stage2-bookmark-pages'){for(const mode of ['desktop','phone'])await stage2BookmarkPages(mode,a,r);return;}
 if(selected==='recreated-chat-isolation'){for(const mode of ['desktop','phone'])await recreatedChatIsolation(mode,a,r);return;}
 if(selected==='bubble-selection'){const before=counts(a),sh=await a.api('/api/team/shares/'+a.node.id);const sub=await until(async()=>(await a.api('/api/team/subscriptions')).subscriptions.find(x=>x.chat===sh.uid&&x.access==='owner'),30000,'selection seed');const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/');for(let i=0;i<40;i++)await a.api('/api/team/subscriptions/'+key+'/comments','POST',{key:i.toString(16).padStart(16,'0'),anchor:0,text:'Synthetic selection history '+i});assertID('bubble-selection-seed-zero-dispatch',JSON.stringify(before)===JSON.stringify(counts(a)));for(const mode of ['desktop','phone'])await bubbleSelection(mode,a,r);return;}
 if(selected==='draft-recovery'){for(const mode of ['desktop','phone'])await draftRecovery(mode,a);return;}
 if(selected==='header-focus'){const prior=await pageFor(a,'desktop','prior-owner-active');await open(prior,a.node.id);for(const mode of ['desktop','phone'])await headerFocus(mode,a);return;}
 if(selected==='description-save-isolation'){for(const mode of ['desktop','phone'])await descriptionSaveIsolation(mode,a);return;}
 if(selected==='description-recreated-choice'){for(const mode of ['desktop','phone'])await recreatedDescriptionChoice(mode,a,r);return;}
 if(selected==='description-choice-order'){for(const mode of ['desktop','phone'])await overlappingDescriptionChoices(mode,a,r);return;}
 if(selected==='delete-recreation-isolation'){for(const mode of ['desktop','phone'])await deleteRecreationIsolation(mode,a);return;}
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
  await owner.locator('#msgs').getByText(human,{exact:true}).waitFor({timeout:40000});
  assertID('new-writer-human-comment-'+mode,(await owner.locator('#msgs').getByText(human,{exact:true}).count())===1);
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
  await open(p,received);await p.locator('#msgs').getByText('Unread folded '+mode,{exact:true}).waitFor({timeout:30000});await delay(2500);await goBack();
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
  await op.locator('#msgs').getByText(offlineComment,{exact:true}).waitFor({timeout:40000});
  assertID('offline-new-content-once-'+mode,(await op.locator('#msgs').getByText(offlineComment,{exact:true}).count())===1);
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
   await rp.locator('#msgs').getByText(recoveryComment,{exact:true}).waitFor({timeout:30000});
   assertID('recovery-once-selection-draft-'+mode,(await rp.locator('#msgs').getByText(recoveryComment,{exact:true}).count())===1 && (await rp.locator('#chattitle').innerText())==='owner-a chat' && (await rp.locator('#prompt').innerText())===recoveryDraft && await rp.evaluate(id=>localStorage.getItem('scimux-sel')===id,received));
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
 await retireCompletedContexts(pages,contexts,captureCoverage);
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>earlyNativeComment(mode,a,r));
 for(const mode of ['desktop','phone']){await runCompletedCase(()=>conversationTrace(mode,a,r));await runCompletedCase(()=>nativeTrace(mode,a,r));await runCompletedCase(()=>refusalTrace(mode,a,r,false));await runCompletedCase(()=>refusalTrace(mode,a,r,true));}
 for(const mode of ['desktop','phone'])await runCompletedCase(async()=>{const before=counts(a);await nativeStartupFailure(mode,a);assert.deepEqual(counts(a),before);assertID('native-start-failure-zero-dispatch-'+mode,true);});
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>recipientBoundary(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>aiSourcePrompt(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>recipientLeave(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>permissionLifecycle(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>recipientLeaveEmpty(mode,a));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>recipientLeaveStale(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>heldEvidence(mode,a,r));
 for(const mode of ['w320','w390','w768'])await runCompletedCase(()=>viewportKeyboard(mode,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>droppedSend(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>reshareStale(mode,a,r));
 for(const mode of ['desktop','phone'])for(const scheme of ['light','dark'])await runCompletedCase(()=>mediaContrast(mode,scheme,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>unsharedChat(mode,a));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>headerFocus(mode,a));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>descriptionSaveIsolation(mode,a));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>recreatedDescriptionChoice(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>overlappingDescriptionChoices(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>heldChatLoading(mode,a));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>receivedOfflineRetry(mode,a,r));
 for(const mode of ['desktop','phone']){await runCompletedCase(()=>stage1Scanner(mode,a,r));await runCompletedCase(()=>stage1Unread(mode,a,r));await runCompletedCase(()=>stage1OwnerFiles(mode,a,r));}
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>stage2SharedActions(mode,a,b,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>stage2BookmarkPages(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>recreatedChatIsolation(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>bubbleSelection(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>draftRecovery(mode,a));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>deleteRecreationIsolation(mode,a));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>legacyClosedRecreation(mode));
 const empty=await makeSeat('no-selection');for(const mode of ['desktop','phone'])await runCompletedCase(()=>noSelection(mode,empty));
 manifest.counters.afterNative=Object.fromEntries(seats.map(s=>[s.name,counts(s)]));
 assertID('native-authoritative-response',true);
 if(process.env.SCIMUX_E2E_FAIL_AFTER_NATIVE==='1')throw Error('controlled failure with native node');
}
(async()=>{try{await run();manifest.result='pass';}catch(e){if(!signalCode)manifest.result='fail';manifest.error=String(e);console.error(e.stack||e);if(!signalCode)process.exitCode=1;for(const {page,label} of [...pages].reverse()){try{if(!page.isClosed()){const name='failure-'+label+'.png';await page.screenshot({path:artifacts(name),fullPage:true,timeout:5000});manifest.artifacts.push(name);}}catch{}}}finally{await cleanup();console.log('Artifacts: '+root);if(signalCode)process.exitCode=signalCode;}})();
