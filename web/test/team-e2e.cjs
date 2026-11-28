#!/usr/bin/env node
// Opt-in synthetic real-browser acceptance. No test dependency ships with scimux.
'use strict';
const {validMidbodyProbe}=require('./testdata/team-e2e/runner-bootstrap.cjs');
if(!validMidbodyProbe(process.env.SCIMUX_E2E_MIDBODY_PROBE,process.env.SCIMUX_E2E_SCENARIO)) {
  console.error('midbody cleanup probe requires its selected scenario');
  process.exitCode=2;
  return;
}
if(process.env.SCIMUX_E2E_INNER!=='1') {
  const {configureRunner,runnerExitCode,formatRunnerSummary}=require('./testdata/team-e2e/runner-bootstrap.cjs');
  configureRunner({env:process.env,runnerPath:__filename}).then(result=>{
    process.exitCode=runnerExitCode(result);
    if(process.exitCode)console.error(formatRunnerSummary(result));
  }).catch(()=>{console.error('E2E infrastructure failed');process.exitCode=1;});
  return;
}
const fs = require('node:fs');
const path = require('node:path');
const net = require('node:net');
const http = require('node:http');
const crypto = require('node:crypto');
const assert = require('node:assert/strict');
const { spawn, execFileSync } = require('node:child_process');
const { retire } = require('./testdata/team-e2e/tmux-cleanup.cjs');
const { selectSharedSubscription } = require('./testdata/team-e2e/share-select.cjs');
const { retireCompletedContexts } = require('./testdata/team-e2e/browser-lifecycle.cjs');
const { contrastSample } = require('./testdata/team-e2e/contrast.cjs');
const { createCleanupOnce } = require('./testdata/team-e2e/cleanup-once.cjs');
const { installRunnerSignals,finishScenario,controlledFailureCode } = require('./testdata/team-e2e/runner-signals.cjs');
const { summarizeNative } = require('./testdata/team-e2e/native-process-state.cjs');
const { createPageCapture } = require('./testdata/team-e2e/capture-once.cjs');
const { captureEvidencePage,scenarioNames } = require('./testdata/team-e2e/evidence-project.cjs');
const { createCaptureRegistry } = require('./testdata/team-e2e/capture-identity.cjs');
const { createDiagnosticBudget } = require('./testdata/team-e2e/diagnostic-log.cjs');
const { verifiedCoverageEntry } = require('./testdata/team-e2e/coverage-source.cjs');
const { verifyMidbodyWitness } = require('./testdata/team-e2e/verify-midbody-witness.cjs');
const { verifyLocalBudget } = require('./testdata/team-e2e/verify-local-budget.cjs');
const { prepareTunnelGo } = require('./testdata/team-e2e/tunnel-toolchain.cjs');
const { validateInnerRuntime } = require('./testdata/team-e2e/runtime-io.cjs');

const root=validateInnerRuntime(process.env);
if(!root){console.error('E2E runtime invalid');process.exit(2);}

function prerequisite(name) {
  const value = process.env[name];
  if (!value || !path.isAbsolute(value)) throw Error(`${name} must name an absolute path`);
  return fs.realpathSync(value);
}
let scimux, relayBin, playwright, legacyScimux, legacyRelayBin, legacySource;
try {
  scimux = prerequisite('SCIMUX_E2E_BINARY'); relayBin = prerequisite('SCIMUX_TEAM_E2E_BINARY');
  if (scimux === relayBin) throw Error('scimux and relay must be distinct binaries');
  for (const p of [scimux, relayBin]) fs.accessSync(p, fs.constants.X_OK);
  playwright = require(prerequisite('SCIMUX_E2E_PLAYWRIGHT'));
  if (!process.env.SCIMUX_E2E_SCENARIO || process.env.SCIMUX_E2E_SCENARIO === 'legacy-reader' || process.env.SCIMUX_E2E_LEGACY_BINARY) {
    legacyScimux = prerequisite('SCIMUX_E2E_LEGACY_BINARY');
    legacySource = prerequisite('SCIMUX_E2E_LEGACY_SOURCE');
    fs.accessSync(path.join(legacySource,'web','js','app.js'), fs.constants.R_OK);
    fs.accessSync(legacyScimux, fs.constants.X_OK);
    if (legacyScimux === scimux) throw Error('legacy scimux must be a distinct binary');
  }
  if (!process.env.SCIMUX_E2E_SCENARIO || process.env.SCIMUX_E2E_SCENARIO === 'old-relay') {
    legacyRelayBin = prerequisite('SCIMUX_TEAM_LEGACY_E2E_BINARY');
    fs.accessSync(legacyRelayBin, fs.constants.X_OK);
    if (legacyRelayBin === relayBin) throw Error('legacy relay must be a distinct binary');
  }
  if (!playwright.chromium || !playwright.devices) throw Error('Playwright Chromium is required');
  execFileSync('tmux', ['-V'], {stdio:'ignore'});
} catch (e) { console.error('E2E prerequisite failed'); process.exit(2); }

const fixtureDir = path.join(__dirname, 'testdata', 'team-e2e');
const manifest = {version:1, scope:'implemented synthetic browser acceptance scenarios', assertions:[], artifacts:[], counters:{}, cleanup:[]};
const seats = [], children = [], pages = [], contexts = [];
const coverageArchive = [];
const pageSource = new WeakMap();
const pending = new Set();
const heldReleases = new Set();
let browser, relay, shutting = false, signalCode = 0;
let cleanupProbeGate, cleanupProbeRelease;
const controlledNativeCleanupFailure=Error('controlled native cleanup failure');
let captureProbeGate, captureProbeRelease, captureProbeStarted=false;
const launchBrowser=()=>playwright.chromium.launch({handleSIGINT:false,handleSIGTERM:false,handleSIGHUP:false});
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
const assertID = (id, condition) => { assert.ok(condition, id); manifest.assertions.push({id, result:'pass'}); };
const record = () => fs.writeFileSync(path.join(root, 'manifest.json'), JSON.stringify(manifest, null, 2));
const artifacts = name => path.join(root, name);
const captureRegistry=createCaptureRegistry({write:value=>fs.promises.writeFile(path.join(root,'capture-identities.private.json'),value,{mode:0o600})});
const jsonLines = p => fs.existsSync(p) ? fs.readFileSync(p, 'utf8').trim().split('\n').filter(Boolean).map(JSON.parse) : [];
const diagnosticBudget = createDiagnosticBudget(root);
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
  const c = spawn(binary,args,{env,stdio:['ignore',stdout,'pipe']});
  children.push(c);diagnosticBudget.attach(label,c.stderr);return c;
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
async function launchRelay(binary=relayBin,label='relay') {
  const dir=artifacts(label); fs.mkdirSync(dir,{mode:0o700});
  const c=start(binary,['-state-dir',dir,'-addr','127.0.0.1:0','-name','Synthetic relay'],baseEnv,label,'pipe');
  let out=''; c.stdout.on('data',x=>{out+=x;});
  const url=await until(()=>{if(c.exitCode!==null)throw Error('relay exited');return out.match(/scimux-team listening on (http:\/\/127\.0\.0\.1:\d+)/)?.[1];},20000,'relay startup');
  return {dir,url,child:c};
}
async function restartRelay() {
  const c=start(relayBin,['-state-dir',relay.dir,'-addr',new URL(relay.url).host,'-name','Synthetic relay'],baseEnv,'relay-restart','pipe');
  let out='';c.stdout.on('data',x=>{out+=x;});relay.child=c;
  await until(()=>{if(c.exitCode!==null)throw Error('restarted relay exited');return out.includes('scimux-team listening on '+relay.url);},20000,'relay restart');
}
async function makeSeat(name, legacyClosed=false, binary=scimux) {
  const data=artifacts(name); fs.mkdirSync(data,{mode:0o700});
  if(legacyClosed)fs.writeFileSync(path.join(data,'nodes.jsonl'),JSON.stringify({type:'node',node:{id:'Synthetic-legacy-closed',title:'Synthetic legacy closed',agent:'codex',dir:data,created_at:'1999-01-01T00:00:00Z',ended_at:'2000-01-01T00:00:00Z'}})+'\n',{mode:0o600});
  const tmuxDir=path.join(data,'tmux');fs.mkdirSync(tmuxDir,{mode:0o700});
  const socket='e2e-'+name+'-'+process.pid;
  const env={...baseEnv, HOME:data, TMUX_TMPDIR:tmuxDir, SCIMUX_SYNTHETIC_COUNTER:path.join(data,'counter')};
  const s={name,data,env,binary,socket,socketPath:path.join(tmuxDir,'tmux-'+process.getuid(),socket), nodes:[]}; seats.push(s);
  const port=await freePort(); s.base='http://127.0.0.1:'+port;
  s.child=start(binary,['-data',data,'-addr','127.0.0.1:'+port,'-socket',socket],env,name);
  const html=await until(async()=>{if(s.child.exitCode!==null)throw Error(name+' exited');try{const r=await fetch(s.base);return r.ok?await r.text():null;}catch{return null;}},20000,name+' startup');
  const match=html.match(/name="scimux-csrf" content="([^"]+)"/);assert.ok(match,'csrf meta');const token=match[1];
  s.csrf=token;
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
  pageSource.set(page, s.binary === legacyScimux ? {dir:path.join(legacySource,'web','js'),version:'legacy'} : {dir:path.resolve(__dirname,'../js'),version:'current'});
  await page.coverage.startJSCoverage({resetOnNavigation:false});
  await page.goto(s.base,{waitUntil:'networkidle'});return page;
}
async function screenshot(p,name) { assert.ok((await captureEvidencePage({page:p,destination:artifacts(name),register:captureRegistry.register})).ok,'screenshot capture failed');manifest.artifacts.push(name); }
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
  await writer.route(url=>url.pathname==='/api/team/subscriptions/'+watched.key+'/seen',route=>route.abort());
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
async function stage2SharedActions(mode,ownerSeat,otherSeat,readerSeat,observerSeat) {
  await otherSeat.api('/api/team/shares/'+otherSeat.node.id,'PUT',{server:otherSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  await ownerSeat.api('/api/team/shares/'+ownerSeat.node.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'},{seat:observerSeat.self,access:'write'},{seat:otherSeat.self,access:'read'}]});
  const share=await ownerSeat.api('/api/team/shares/'+ownerSeat.node.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid&&x.generation===share.generation),30000,'stage2 shared subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),received='team/'+key;
  for(let i=0;i<12;i++)await readerSeat.api('/api/team/subscriptions/'+key+'/comments','POST',{text:'Synthetic reaction scroll filler '+mode+' '+i,anchor:0,key:(mode==='desktop'?'f11e0000':'f11e1000')+i.toString(16).padStart(8,'0')});
  const observerSub=await until(async()=>(await observerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid&&x.generation===share.generation),30000,'reaction observer subscription');
  const readOnlySub=await until(async()=>(await otherSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid&&x.generation===share.generation),30000,'read-only reaction subscription');
  const reader=await pageFor(readerSeat,mode,'stage2-reader-'+mode),owner=await pageFor(ownerSeat,mode,'stage2-owner-'+mode),observer=await pageFor(observerSeat,mode,'stage2-reaction-observer-'+mode);
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
  const reactionButton=duplicate.locator('[data-reaction-open]');
  assertID('stage2-reaction-no-self-control-'+mode,await reactionButton.count()===0);
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
  const shortComment=await readerSeat.api('/api/team/subscriptions/'+key+'/comments','POST',{text:'OK',anchor:0,key:(mode==='desktop'?'f11e2000':'f11e3000')+'00000001'});
  await open(owner,ownerSeat.node.id);const source=owner.locator('#msgs .gmsg[data-cseq="'+seq+'"] .bubble');await source.waitFor({timeout:40000});
  const shortMessage=owner.locator('#msgs .gmsg[data-cseq="'+shortComment.seq+'"]'),shortBubble=shortMessage.locator('.bubble');await shortBubble.waitFor({timeout:40000});
  const shortTrigger=shortMessage.locator('[data-reaction-open]');await shortTrigger.waitFor({timeout:30000});
  const compactBubble=await shortBubble.evaluate(bubble=>{
    const turn=bubble.parentElement,entry=bubble.querySelector('[data-reaction-entry]'),chips=bubble.querySelector('[data-reaction-chips]');
    const before=bubble.getBoundingClientRect();entry.remove();if(chips&&!chips.childElementCount)chips.remove();
    const plain=bubble.getBoundingClientRect();if(chips)bubble.append(chips);bubble.append(entry);
    return {sameHeight:Math.abs(before.height-plain.height)<1,sameWidth:Math.abs(before.width-plain.width)<1,separator:!!turn.querySelector('.reaction-divider')};
  });
  assertID('stage2-reaction-overlay-preserves-bubble-size-'+mode,compactBubble.sameHeight&&compactBubble.sameWidth&&!compactBubble.separator);
  await shortBubble.click();const shortPoint=await shortTrigger.boundingBox();
  if(mode==='phone')await owner.touchscreen.tap(shortPoint.x+shortPoint.width/2,shortPoint.y+shortPoint.height/2);else await shortTrigger.click();
  await shortMessage.locator('[data-reaction-choice="thumbs_up"]').waitFor({state:'visible',timeout:10000});
  const shortGeometry=await shortMessage.evaluate(message=>{const viewport=document.querySelector('#msgs').getBoundingClientRect(),buttons=[...message.querySelectorAll('[data-reaction-choice]')],rects=buttons.map(button=>{const r=button.getBoundingClientRect();return {left:r.left,right:r.right,top:r.top,bottom:r.bottom};});return {count:rects.length,inside:rects.every(r=>r.left>=viewport.left&&r.right<=viewport.right&&r.top>=viewport.top&&r.bottom<=viewport.bottom),rects,viewport:{left:viewport.left,right:viewport.right,top:viewport.top,bottom:viewport.bottom}};});
  assertID('stage2-reaction-short-message-all-choices-visible-'+mode,shortGeometry.count===6&&shortGeometry.inside);
  await screenshot(owner,'stage2-reaction-short-message-'+mode+'.png');
  if(mode==='desktop'){
    const medium=await readerSeat.api('/api/team/subscriptions/'+key+'/comments','POST',{text:'This is a medium length note',anchor:0,key:'f11e400000000001'});
    await owner.locator('#msgs').evaluate(el=>{el.style.setProperty('width','500px','important');el.style.setProperty('max-width','500px','important');el.style.setProperty('flex','0 0 500px','important');});
    const paneWidth=await owner.locator('#msgs').evaluate(el=>el.getBoundingClientRect().width);
    const mediumMessage=owner.locator('#msgs .gmsg[data-cseq="'+medium.seq+'"]'),mediumTrigger=mediumMessage.locator('[data-reaction-open]');
    await mediumTrigger.waitFor({timeout:30000});await mediumMessage.evaluate(el=>el.scrollIntoView({block:'center'}));await mediumMessage.hover();await mediumTrigger.click();
    await mediumMessage.locator('[data-reaction-choice="thumbs_up"]').waitFor({state:'visible',timeout:10000});
    const mediumGeometry=await mediumMessage.evaluate(message=>{const viewport=document.querySelector('#msgs').getBoundingClientRect(),entry=message.querySelector('.reaction-entry'),chooser=entry.querySelector('.reaction-chooser'),buttons=[...chooser.querySelectorAll('[data-reaction-choice]')],rects=buttons.map(button=>button.getBoundingClientRect());return {pane:viewport.width,count:buttons.length,compact:entry.classList.contains('compact'),order:buttons.map(button=>button.dataset.reactionChoice),inside:rects.every(r=>r.left>=viewport.left&&r.right<=viewport.right&&r.top>=viewport.top&&r.bottom<=viewport.bottom),right:Math.max(...rects.map(r=>r.right)),viewportRight:viewport.right};});
    assertID('stage2-reaction-medium-message-500px-pane-all-choices-visible-'+mode,paneWidth===500&&mediumGeometry.pane===500&&mediumGeometry.count===6&&mediumGeometry.compact&&mediumGeometry.inside&&mediumGeometry.order.join(',')==='celebration,thinking,laughing,heart,thumbs_down,thumbs_up');
    await screenshot(owner,'stage2-reaction-medium-message-500px-pane-'+mode+'.png');
    await owner.locator('#chattitle').click();
    await owner.locator('#msgs').evaluate(el=>{el.style.setProperty('width','440px','important');el.style.setProperty('max-width','440px','important');el.style.setProperty('flex','0 0 440px','important');});
    const medium440Message=mediumMessage,medium440Trigger=mediumTrigger;
    await medium440Message.evaluate(el=>el.scrollIntoView({block:'center'}));await medium440Message.hover();await medium440Trigger.click();
    await medium440Message.locator('[data-reaction-choice="thumbs_up"]').waitFor({state:'visible',timeout:10000});
    const medium440Geometry=await medium440Message.evaluate(message=>{const viewport=document.querySelector('#msgs').getBoundingClientRect(),entry=message.querySelector('.reaction-entry'),buttons=[...entry.querySelector('.reaction-chooser').querySelectorAll('[data-reaction-choice]')],rects=buttons.map(button=>button.getBoundingClientRect());return {pane:viewport.width,count:buttons.length,compact:entry.classList.contains('compact'),inside:rects.every(r=>r.left>=viewport.left&&r.right<=viewport.right&&r.top>=viewport.top&&r.bottom<=viewport.bottom)};});
    assertID('stage2-reaction-medium-message-440px-pane-all-choices-visible-'+mode,medium440Geometry.pane===440&&medium440Geometry.count===6&&medium440Geometry.compact&&medium440Geometry.inside);
    await screenshot(owner,'stage2-reaction-medium-message-440px-pane-'+mode+'.png');
    await owner.locator('#chattitle').click();
    await owner.locator('#msgs').evaluate(el=>{el.style.removeProperty('width');el.style.removeProperty('max-width');el.style.removeProperty('flex');});
    await owner.locator('#msgs').evaluate(el=>{el.style.setProperty('width','320px','important');el.style.setProperty('max-width','320px','important');el.style.setProperty('flex','0 0 320px','important');el.style.setProperty('height','230px','important');el.style.setProperty('min-height','230px','important');el.style.setProperty('max-height','230px','important');});
    await shortMessage.evaluate(el=>el.scrollIntoView({block:'center'}));await shortMessage.hover();await shortTrigger.click();
    await shortMessage.locator('.reaction-chooser').waitFor({state:'visible',timeout:10000});
    const tinyGeometry=await shortMessage.evaluate(message=>{const viewport=document.querySelector('#msgs').getBoundingClientRect(),entry=message.querySelector('.reaction-entry'),trigger=entry.querySelector('[data-reaction-open]').getBoundingClientRect(),chooser=entry.querySelector('.reaction-chooser').getBoundingClientRect(),buttons=[...entry.querySelectorAll('[data-reaction-choice]')],rects=buttons.map(button=>button.getBoundingClientRect()),first=rects[0],overlap=!(chooser.right<=trigger.left||chooser.left>=trigger.right||chooser.bottom<=trigger.top||chooser.top>=trigger.bottom),firstChoiceOverlapsTrigger=first&&!(first.right<=trigger.left||first.left>=trigger.right||first.bottom<=trigger.top||first.top>=trigger.bottom);return {pane:{width:viewport.width,height:viewport.height},rightward:entry.classList.contains('rightward'),count:buttons.length,overlap,firstChoiceOverlapsTrigger,clear:chooser.left>=trigger.right||chooser.right<=trigger.left,inside:rects.every(r=>r.left>=viewport.left&&r.right<=viewport.right&&r.top>=viewport.top&&r.bottom<=viewport.bottom),trigger:{left:trigger.left,right:trigger.right,top:trigger.top,bottom:trigger.bottom},firstChoice:first,chooser:{left:chooser.left,right:chooser.right,top:chooser.top,bottom:chooser.bottom,width:chooser.width,height:chooser.height},viewport:{left:viewport.left,right:viewport.right,top:viewport.top,bottom:viewport.bottom}};});
    const beforeTiny=await ownerSeat.api('/api/team/subscriptions/'+key+'/reactions?after=0');
    const tinyHit=await shortMessage.evaluate(message=>{const entry=message.querySelector('.reaction-entry'),trigger=entry.querySelector('[data-reaction-open]').getBoundingClientRect(),choice=entry.querySelector('[data-reaction-choice]').getBoundingClientRect(),left=Math.max(trigger.left,choice.left),right=Math.min(trigger.right,choice.right),top=Math.max(trigger.top,choice.top),bottom=Math.min(trigger.bottom,choice.bottom);return left<right&&top<bottom?{x:(left+right)/2,y:(top+bottom)/2}:{x:trigger.right-2,y:(trigger.top+trigger.bottom)/2};});
    const overlapWrite=tinyGeometry.firstChoiceOverlapsTrigger?owner.waitForResponse(response=>response.request().method()==='POST'&&response.url().includes('/reactions?'),{timeout:10000}):null;await owner.mouse.click(tinyHit.x,tinyHit.y);if(overlapWrite)await overlapWrite;
    await shortMessage.locator('.reaction-chooser').waitFor({state:'hidden',timeout:10000});
    const afterTiny=await ownerSeat.api('/api/team/subscriptions/'+key+'/reactions?after=0');
    const note=await readerSeat.api('/api/team/subscriptions/'+key+'/comments','POST',{text:'A short note',anchor:0,key:'f11e500000000001'}),noteMessage=owner.locator('#msgs .gmsg[data-cseq="'+note.seq+'"]'),noteTrigger=noteMessage.locator('[data-reaction-open]');
    await noteTrigger.waitFor({timeout:30000});await noteMessage.evaluate(el=>el.scrollIntoView({block:'center'}));await noteMessage.locator('.bubble').click({position:{x:12,y:12}});
    const selectedNote=await noteTrigger.evaluate(button=>{const turn=button.closest('.gmsg'),bubble=button.closest('.bubble'),entry=button.closest('.reaction-entry'),b=bubble.getBoundingClientRect(),r=entry.getBoundingClientRect();return {selected:turn.classList.contains('bubble-selected'),bottomGap:b.bottom-r.bottom,topGap:r.top-b.top};});
    assertID('stage2-reaction-short-note-selected-bottom-right-'+mode,selectedNote.selected&&selectedNote.bottomGap<0);
    await noteTrigger.click();
    await noteMessage.locator('.reaction-chooser').waitFor({state:'visible',timeout:10000});
    const noteBefore=await ownerSeat.api('/api/team/subscriptions/'+key+'/reactions?after=0');
    const noteGeometry=await noteMessage.evaluate(message=>{const viewport=document.querySelector('#msgs').getBoundingClientRect(),entry=message.querySelector('.reaction-entry'),trigger=entry.querySelector('[data-reaction-open]').getBoundingClientRect(),chooser=entry.querySelector('.reaction-chooser').getBoundingClientRect(),bubble=message.querySelector('.bubble').getBoundingClientRect(),buttons=[...entry.querySelectorAll('[data-reaction-choice]')],rects=buttons.map(button=>button.getBoundingClientRect()),overlap=!(chooser.right<=trigger.left||chooser.left>=trigger.right||chooser.bottom<=trigger.top||chooser.top>=trigger.bottom),horizontalGap=Math.max(0,trigger.left-chooser.right,chooser.left-trigger.right),verticalGap=Math.max(0,trigger.top-chooser.bottom,chooser.top-trigger.bottom);return {pane:{width:viewport.width,height:viewport.height},selected:message.classList.contains('bubble-selected'),triggerBottomGap:bubble.bottom-trigger.bottom,count:buttons.length,inside:rects.every(r=>r.left>=viewport.left&&r.right<=viewport.right&&r.top>=viewport.top&&r.bottom<=viewport.bottom),overlap,clear:Math.max(horizontalGap,verticalGap)>=8,trigger:{left:trigger.left,right:trigger.right,top:trigger.top,bottom:trigger.bottom},chooser:{left:chooser.left,right:chooser.right,top:chooser.top,bottom:chooser.bottom,width:chooser.width,height:chooser.height},viewport:{left:viewport.left,right:viewport.right,top:viewport.top,bottom:viewport.bottom}};});
    await screenshot(owner,'stage2-reaction-320x230-'+mode+'.png');
    const noteOverlapWrite=noteGeometry.overlap?owner.waitForResponse(response=>response.request().method()==='POST'&&response.url().includes('/reactions?'),{timeout:10000}):null,noteTriggerBox=await noteTrigger.boundingBox();
    await owner.mouse.click(noteTriggerBox.x+noteTriggerBox.width/2,noteTriggerBox.y+noteTriggerBox.height/2);if(noteOverlapWrite)await noteOverlapWrite;
    await noteMessage.locator('.reaction-chooser').waitFor({state:'hidden',timeout:10000});
    const noteAfter=await ownerSeat.api('/api/team/subscriptions/'+key+'/reactions?after=0');
    assertID('stage2-reaction-rightward-picker-clears-trigger-'+mode,tinyGeometry.pane.width===320&&tinyGeometry.rightward&&tinyGeometry.clear&&!tinyGeometry.overlap&&afterTiny.last===beforeTiny.last&&await shortTrigger.getAttribute('aria-expanded')==='false');
    assertID('stage2-reaction-short-note-fits-320x230-and-trigger-dismisses-without-selection-'+mode,noteGeometry.pane.width===320&&noteGeometry.pane.height===230&&noteGeometry.selected&&noteGeometry.triggerBottomGap<0&&noteGeometry.count===6&&noteGeometry.inside&&noteGeometry.clear&&!noteGeometry.overlap&&noteAfter.last===noteBefore.last&&await noteTrigger.getAttribute('aria-expanded')==='false');
    await owner.locator('#chattitle').click();
    await owner.locator('#msgs').evaluate(el=>{for(const property of ['width','max-width','flex','height','min-height','max-height'])el.style.removeProperty(property);});
  }
  await owner.locator('#chattitle').click();
  const ownerReaction=owner.locator('#msgs .gmsg[data-cseq="'+seq+'"] [data-reaction-open]');await ownerReaction.waitFor({timeout:30000});
  if(mode==='desktop'){
    await source.hover();
    const hoverGeometry=await ownerReaction.evaluate(button=>{const bubble=button.closest('.bubble'),entry=button.closest('.reaction-entry'),b=bubble.getBoundingClientRect(),r=entry.getBoundingClientRect();return {selected:button.closest('.gmsg').classList.contains('bubble-selected'),top:r.top-b.top,bottom:b.bottom-r.bottom};});
    assertID('stage2-reaction-hover-top-right-'+mode,!hoverGeometry.selected&&hoverGeometry.top<0);
    await ownerReaction.click();const hoverChoice=owner.locator('#msgs .gmsg[data-cseq="'+seq+'"] [data-reaction-choice="thumbs_up"]');await hoverChoice.waitFor({state:'visible',timeout:10000});
    await owner.locator('#chattitle').hover();await hoverChoice.waitFor({state:'visible',timeout:10000});
    const noWriteBefore=await ownerSeat.api('/api/team/subscriptions/'+key+'/reactions?after=0');
    await owner.locator('#chattitle').click();await hoverChoice.waitFor({state:'hidden',timeout:10000});
    const noWriteAfter=await ownerSeat.api('/api/team/subscriptions/'+key+'/reactions?after=0');
    assertID('stage2-reaction-outside-dismiss-no-write-'+mode,noWriteAfter.last===noWriteBefore.last&&await ownerReaction.getAttribute('aria-expanded')==='false');
  }
  await source.click();await owner.locator('#msgs .gmsg[data-cseq="'+seq+'"]').waitFor();
  const triggerBox=await ownerReaction.boundingBox();assertID('stage2-reaction-trigger-hit-target-'+mode,triggerBox.width>=44&&triggerBox.height>=44);
  const selectedGeometry=await ownerReaction.evaluate(button=>{const bubble=button.closest('.bubble'),entry=button.closest('.reaction-entry'),b=bubble.getBoundingClientRect(),r=entry.getBoundingClientRect();return {selected:button.closest('.gmsg').classList.contains('bubble-selected'),top:r.top-b.top,bottom:b.bottom-r.bottom};});
  assertID('stage2-reaction-selected-bottom-right-'+mode,selectedGeometry.selected&&selectedGeometry.bottom<0);
  const scrollGeometry=await owner.evaluate(seq=>{const msgs=document.querySelector('#msgs'),turn=document.querySelector('#msgs .gmsg[data-cseq="'+seq+'"]');msgs.scrollTop=0;const start=msgs.scrollTop;msgs.scrollTop=msgs.scrollHeight;const button=turn.querySelector('[data-reaction-open]'),bubble=turn.querySelector('.bubble'),r=button.closest('.reaction-entry').getBoundingClientRect(),b=bubble.getBoundingClientRect();return {start,top:msgs.scrollTop,max:msgs.scrollHeight-msgs.clientHeight,gap:b.bottom-r.bottom};},seq);
  assertID('stage2-reaction-scroll-attached-'+mode,scrollGeometry.max>0&&scrollGeometry.top===scrollGeometry.max&&scrollGeometry.gap<0);
  const chooserPoint=await ownerReaction.boundingBox();
  if(mode==='phone')await owner.touchscreen.tap(chooserPoint.x+chooserPoint.width/2,chooserPoint.y+chooserPoint.height/2);else await ownerReaction.click();
  const ownerChooser=owner.locator('#msgs .gmsg[data-cseq="'+seq+'"] [data-reaction-choice="thumbs_up"]');await ownerChooser.waitFor({state:'visible',timeout:10000});
  const chooserGeometry=await ownerChooser.evaluate(choice=>{const entry=choice.closest('.reaction-entry'),trigger=entry.querySelector('[data-reaction-open]'),chooser=entry.querySelector('.reaction-chooser'),buttons=[...chooser.querySelectorAll('[data-reaction-choice]')],rects=buttons.map(button=>{const r=button.getBoundingClientRect();return {id:button.dataset.reactionChoice,left:r.left,top:r.top,right:r.right,bottom:r.bottom};}),tr=trigger.getBoundingClientRect(),cr=chooser.getBoundingClientRect(),style=getComputedStyle(chooser);return {order:buttons.map(button=>button.dataset.reactionChoice),display:style.display,rows:style.gridTemplateRows.split(' ').length,columns:style.gridTemplateColumns.split(' ').length,leftward:cr.right<=tr.left,overlap:!(cr.right<=tr.left||cr.left>=tr.right||cr.bottom<=tr.top||cr.top>=tr.bottom),within:cr.left>=0&&cr.right<=innerWidth&&cr.top>=0&&cr.bottom<=innerHeight,rects};});
  const expectedRows=[['celebration','laughing','thumbs_down'],['thinking','heart','thumbs_up']];
  const actualRows=[...new Set(chooserGeometry.rects.map(r=>r.top))].sort((a,b)=>a-b).map(top=>chooserGeometry.rects.filter(r=>r.top===top).sort((a,b)=>a.left-b.left).map(r=>r.id));
  const chooserBox=await ownerChooser.locator('..').boundingBox(),paneBox=await owner.locator('#msgs').boundingBox();
  const prefersLeft=chooserPoint.x-paneBox.x-4>=chooserBox.width;
  const fixedGrid=chooserGeometry.display==='grid'&&chooserGeometry.rows===2&&chooserGeometry.columns===3&&JSON.stringify(actualRows)===JSON.stringify(expectedRows);
  const fitsPane=chooserBox.x>=paneBox.x&&chooserBox.x+chooserBox.width<=paneBox.x+paneBox.width&&chooserBox.y>=paneBox.y&&chooserBox.y+chooserBox.height<=paneBox.y+paneBox.height;
  assertID('stage2-reaction-chooser-geometry-'+mode,chooserGeometry.order.join(',')==='celebration,thinking,laughing,heart,thumbs_down,thumbs_up'&&(!prefersLeft||chooserGeometry.leftward)&&!chooserGeometry.overlap&&chooserGeometry.within&&fitsPane&&(mode==='phone'?fixedGrid:chooserGeometry.display==='flex'||fixedGrid));
  assertID('stage2-reaction-stops-bubble-action-'+mode,await owner.locator('#msgs .gmsg[data-cseq="'+seq+'"]').evaluate(el=>el.classList.contains('bubble-selected')));
  if(mode==='phone'){
    const bubbleBox=await source.boundingBox();await owner.mouse.move(bubbleBox.x+bubbleBox.width/2,bubbleBox.y+bubbleBox.height/2);
    await ownerChooser.waitFor({state:'visible'});const choiceBox=await ownerChooser.boundingBox();
    await owner.touchscreen.tap(choiceBox.x+choiceBox.width/2,choiceBox.y+choiceBox.height/2);
  } else await ownerChooser.click();
  await owner.locator('#msgs .gmsg[data-cseq="'+seq+'"] [data-reaction="thumbs_up"]').waitFor({timeout:40000});
  if(mode==='phone')assertID('stage2-reaction-input-switch-mouse-to-touch-'+mode,!await owner.evaluate(()=>document.body.classList.contains('reaction-input-mouse')));
  if(mode==='phone'&&await reader.locator('#bookmarkspane').isVisible())await reader.locator('#bookmarksback').click();
  if(mode==='phone')await reader.locator('#chatback').click();
  await open(reader,received);await reader.locator('#msgs .gmsg[data-cseq="'+seq+'"] [data-reaction="thumbs_up"]').waitFor({timeout:40000});
  await open(observer,'team/'+[observerSub.server,observerSub.owner,observerSub.chat,observerSub.generation].join('/'));
  const ownerMessage=owner.locator(`#msgs .gmsg[data-cseq="${seq}"]`),readerMessage=reader.locator(`#msgs .gmsg[data-cseq="${seq}"]`);
  const observerMessage=observer.locator(`#msgs .gmsg[data-cseq="${seq}"]`);await observerMessage.locator('[data-reaction="thumbs_up"]').waitFor({timeout:40000});
  const readOnly=await pageFor(otherSeat,mode,'stage2-reaction-readonly-'+mode);await open(readOnly,'team/'+[readOnlySub.server,readOnlySub.owner,readOnlySub.chat,readOnlySub.generation].join('/'));
  const readOnlySeatIsLegacy=Boolean(legacyScimux&&otherSeat.binary===legacyScimux);
  const readOnlyMessage=readOnly.locator(`#msgs .gmsg[data-cseq="${seq}"]`);
  if(readOnlySeatIsLegacy) await readOnlyMessage.getByText(text,{exact:true}).waitFor({timeout:40000});
  else await readOnlyMessage.locator('[data-reaction="thumbs_up"]').waitFor({timeout:40000});
  assertID('stage2-reaction-readonly-observes-'+mode,await readOnlyMessage.locator('[data-reaction-open]').count()===0&&await readOnlyMessage.locator('[data-reaction="thumbs_up"].mine').count()===0);
  if(readOnlySeatIsLegacy) assertID('stage2-legacy-client-skips-reaction-record-'+mode,await readOnlyMessage.getByText(text,{exact:true}).count()===1&&await readOnlyMessage.locator('[data-reaction-chip], [data-reaction-entry]').count()===0);
  assertID('stage2-reaction-three-view-sync-'+mode,await ownerMessage.locator('[data-reaction="thumbs_up"] span').last().innerText()==='1'&&await readerMessage.locator('[data-reaction="thumbs_up"] span').last().innerText()==='1'&&await observerMessage.locator('[data-reaction="thumbs_up"] span').last().innerText()==='1');
  assertID('stage2-reaction-own-message-readonly-'+mode,await readerMessage.locator('[data-reaction-open]').count()===0&&await readerMessage.locator('[data-reaction="thumbs_up"].mine').count()===0&&await ownerMessage.locator('[data-reaction="thumbs_up"].mine').count()===1&&await observerMessage.locator('[data-reaction="thumbs_up"].mine').count()===0);
  if(mode==='desktop'){
    await terminate(relay.child);assertID('stage2-reaction-relay-stopped-'+mode,relay.child.exitCode!==null||relay.child.signalCode!==null);
    await restartRelay();
    await until(async()=>{const state=await observerSeat.api('/api/team/subscriptions'),server=state.servers?.find(item=>item.id===observerSub.server);return server&&!['offline','refused','changed'].includes(server.delivery);},30000,'reaction relay reconnect');
  }
  const observerTrigger=observerMessage.locator('[data-reaction-open]');
  if(mode==='phone')await observerMessage.locator('.bubble').click();else await observerMessage.locator('.bubble').hover();
  await observerTrigger.waitFor({state:'visible'});
  const observerTriggerBox=await observerTrigger.boundingBox();
  if(mode==='phone')await observer.touchscreen.tap(observerTriggerBox.x+observerTriggerBox.width/2,observerTriggerBox.y+observerTriggerBox.height/2);else await observerTrigger.click();
  const observerChoice=observerMessage.locator('[data-reaction-choice="thumbs_up"]');await observerChoice.waitFor({state:'visible',timeout:10000});
  if(mode==='phone')await observer.touchscreen.tap((await observerChoice.boundingBox()).x+22,(await observerChoice.boundingBox()).y+22);else await observerChoice.click();
  await ownerMessage.locator('[data-reaction="thumbs_up"] span').last().filter({hasText:'2'}).waitFor({timeout:40000});
  await readerMessage.locator('[data-reaction="thumbs_up"] span').last().filter({hasText:'2'}).waitFor({timeout:40000});
  await observerMessage.locator('[data-reaction="thumbs_up"] span').last().filter({hasText:'2'}).waitFor({timeout:40000});
  if(readOnlySeatIsLegacy) assertID('stage2-legacy-client-keeps-original-comment-'+mode,await readOnlyMessage.getByText(text,{exact:true}).count()===1&&await readOnly.locator(`#msgs .gmsg[data-cseq="${seq}"]`).count()===1);
  else await readOnlyMessage.locator('[data-reaction="thumbs_up"] span').last().filter({hasText:'2'}).waitFor({timeout:40000});
  assertID('stage2-reaction-participant-highlights-'+mode,await ownerMessage.locator('[data-reaction="thumbs_up"].mine').count()===1&&await readerMessage.locator('[data-reaction="thumbs_up"].mine').count()===0&&await observerMessage.locator('[data-reaction="thumbs_up"].mine').count()===1);
  await ownerReaction.click();await ownerMessage.locator('[data-reaction-choice="thumbs_up"]').click();
  await ownerMessage.locator('[data-reaction="thumbs_up"] span').last().filter({hasText:'1'}).waitFor({timeout:40000});
  assertID('stage2-reaction-independent-remove-'+mode,await ownerMessage.locator('[data-reaction="thumbs_up"].mine').count()===0&&await readerMessage.locator('[data-reaction="thumbs_up"].mine').count()===0&&await observerMessage.locator('[data-reaction="thumbs_up"].mine').count()===1);
  await owner.reload({waitUntil:'networkidle'});await owner.locator('#msgs .gmsg[data-cseq="'+seq+'"] [data-reaction="thumbs_up"]').waitFor({timeout:40000});
  await reader.reload({waitUntil:'networkidle'});await reader.locator('#msgs .gmsg[data-cseq="'+seq+'"] [data-reaction="thumbs_up"]').waitFor({timeout:40000});
  await observer.reload({waitUntil:'networkidle'});await observer.locator('#msgs .gmsg[data-cseq="'+seq+'"] [data-reaction="thumbs_up"]').waitFor({timeout:40000});
  assertID('stage2-reaction-reload-retained-'+mode,await owner.locator('#msgs .gmsg[data-cseq="'+seq+'"] [data-reaction="thumbs_up"] span').last().innerText()==='1'&&await reader.locator('#msgs .gmsg[data-cseq="'+seq+'"] [data-reaction-open]').count()===0&&await observer.locator('#msgs .gmsg[data-cseq="'+seq+'"] [data-reaction="thumbs_up"].mine').count()===1);
  const membershipPath='/api/team/shares/'+ownerSeat.node.id;
  await ownerSeat.api(membershipPath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'},{seat:observerSeat.self,access:'read'},{seat:otherSeat.self,access:'read'}]});
  const observerRead=await until(async()=>{const list=await observerSeat.api('/api/team/subscriptions');return list.subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='read');},30000,'reaction observer read-only update');
  await observer.locator('#msgs .gmsg[data-cseq="'+seq+'"] [data-reaction-open]').waitFor({state:'detached',timeout:30000});
  const observerCopy=[observerRead.server,observerRead.owner,observerRead.chat,observerRead.generation].join('/');
  const denied=await observerSeat.status('/api/team/subscriptions/'+observerCopy+'/reactions?copy_instance='+encodeURIComponent(observerRead.people_copy_instance),{target:seq,emoji:'thumbs_up',active:false,key:'dededede76543210'});
  assertID('stage2-reaction-permission-change-'+mode,denied===403&&await observer.locator('#msgs .gmsg[data-cseq="'+seq+'"] [data-reaction="thumbs_up"].mine').count()===1);
  await screenshot(owner,'stage2-reactions-'+mode+'.png');
  if(mode==='phone')await owner.locator('#chatback').click();
  await open(owner,ownerSeat.node.id);await source.waitFor({timeout:30000});await source.click();
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
  await stage2InstantReactions(mode,{ownerSeat,observerSeat,readerSeat,otherSeat,share,key,owner,observer,reader});
  await stage2ParticipantDisclosure(mode,{ownerSeat,observerSeat,readerSeat,key,owner,observer,reader});
}

/* Route gate for People reaction writes on one page. Reads pass through; a POST
   either continues to the real local server, is held until the test decides, or
   is answered synthetically (403 read_only or an aborted connection). A synthetic
   answer replaces only the browser request, never the relay record. */
async function gateReactionWrites(page) {
  const gate={plan:{hold:false,outcome:'continue'},bodies:[],held:null,arrived:null};
  await page.route('**/reactions?*',async route=>{
    try {
      if(route.request().method()!=='POST')return await route.fallback();
      gate.bodies.push(route.request().postData());
      const plan=gate.plan;gate.plan={hold:false,outcome:'continue'};
      let outcome=plan.outcome;
      if(plan.hold) {
        const decided=new Promise(resolve=>{gate.held=resolve;});
        heldReleases.add(gate.held);
        if(gate.arrived)gate.arrived();gate.arrived=null;
        outcome=await decided;
      }
      if(outcome==='reject')return await route.fulfill({status:403,contentType:'application/json',body:'{"error":"read_only"}'});
      if(outcome==='abort')return await route.abort('connectionreset');
      return await route.continue();
    } catch {}
  });
  gate.hold=()=>{gate.plan={hold:true,outcome:'continue'};return new Promise(resolve=>{gate.arrived=resolve;});};
  gate.next=outcome=>{gate.plan={hold:false,outcome};};
  gate.release=outcome=>{const resolve=gate.held;gate.held=null;if(!resolve)return;heldReleases.delete(resolve);resolve(outcome);};
  return gate;
}
/* Null when no toast element exists; a stale response must not create one. */
async function toastText(page) {
  return page.evaluate(()=>document.querySelector('#toast')?.textContent ?? null);
}
/* Computed box and type of one reaction chip, so a pending preview can be compared with its confirmed form. */
async function reactionChipGeometry(message,emoji) {
  return message.locator('[data-reaction-chips] [data-reaction="'+emoji+'"]').evaluate(el=>{const cs=getComputedStyle(el),box=el.getBoundingClientRect();return {height:box.height,padding:cs.padding,borderWidth:cs.borderTopWidth,borderStyle:cs.borderTopStyle,fontSize:cs.fontSize,lineHeight:cs.lineHeight,fontStyle:cs.fontStyle,count:el.querySelectorAll('span')[1]?.textContent.trim()};});
}
async function reactionChipsOf(message) {
  return message.locator('[data-reaction-chips] [data-reaction]').evaluateAll(els=>els.map(el=>({emoji:el.dataset.reaction,label:el.getAttribute('aria-label'),pending:el.classList.contains('reaction-pending'),mine:el.classList.contains('mine')})));
}
/* A safe touch point inside the bubble: not on text links, chips, or the reaction control. */
async function safeBubblePoint(message) {
  const point=await message.locator('.bubble').evaluate(bubble=>{
    const box=bubble.getBoundingClientRect(),unsafe='a,button,input,textarea,[contenteditable],[data-reaction],[data-reaction-chips],.reaction-entry';
    for(const fy of [0.5,0.35,0.65,0.2,0.8])for(const fx of [0.5,0.3,0.7,0.15,0.85]){
      const x=box.left+box.width*fx,y=box.top+box.height*fy,el=document.elementFromPoint(x,y);
      if(el&&bubble.contains(el)&&!el.closest(unsafe))return {x,y};
    }
    return null;
  });
  if(!point)throw Error('no safe touch point inside the bubble');
  return point;
}
/* Proves that a tap at the center of the locator reaches that control inside the chat timeline. Playwright's
   touchscreen.tap does not reveal coordinates, and a bubble clipped by the timeline still reports as visible.
   Throws with the measured geometry rather than masking an unexpected overlay. */
async function tapProbe(locator,selector,label,options={}) {
  const probe=await locator.evaluate((el,args)=>{
    const {selector,viewport}=args,box=el.getBoundingClientRect(),msgs=viewport?{left:0,top:0,right:innerWidth,bottom:innerHeight}:document.querySelector('#msgs').getBoundingClientRect();
    const x=box.left+box.width/2,y=box.top+box.height/2,hit=document.elementFromPoint(x,y);
    return {x,y,inTimeline:x>=msgs.left&&x<=msgs.right&&y>=msgs.top&&y<=msgs.bottom,receives:!!hit&&hit.closest(selector)===el,
      target:{top:box.top,bottom:box.bottom,left:box.left,right:box.right},timeline:{top:msgs.top,bottom:msgs.bottom,left:msgs.left,right:msgs.right},
      hit:hit?hit.tagName+'.'+String(hit.className).slice(0,120):null};
  },{selector,viewport:options.viewport===true});
  if(!probe.inTimeline||!probe.receives)throw Error(label+' is not tappable: '+JSON.stringify(probe));
  return probe;
}
async function chooseReactionFor(page,message,mode,emoji) {
  const trigger=message.locator('[data-reaction-open]');
  if(mode==='phone'){
    // Tapping a bubble toggles its selection, and the trigger is visible only while it is selected.
    // Select only when it is not selected yet, so a repeat operation never deselects it.
    if(!(await message.evaluate(el=>el.classList.contains('bubble-selected')))){
      await message.locator('.bubble').evaluate(el=>el.scrollIntoView({block:'center',inline:'nearest'}));
      const point=await safeBubblePoint(message);await page.touchscreen.tap(point.x,point.y);
    }
  } else await message.locator('.bubble').hover();
  await trigger.waitFor({state:'visible',timeout:30000});
  // Reveal after any selection, whether it was just made or already present, and before the tap.
  await trigger.evaluate(el=>el.scrollIntoView({block:'center',inline:'nearest'}));
  const triggerProbe=await tapProbe(trigger,'[data-reaction-open]','reaction trigger');
  if(mode==='phone')await page.touchscreen.tap(triggerProbe.x,triggerProbe.y);else await trigger.click();
  const choice=message.locator('[data-reaction-choice="'+emoji+'"]');await choice.waitFor({state:'visible',timeout:10000});
  // The choice keeps its existing reveal. The baseline comes after all intentional reveal scrolling and
  // immediately before the emoji gesture, so later comparisons measure only what the reaction itself did.
  await choice.scrollIntoViewIfNeeded();
  const choiceProbe=await tapProbe(choice,'[data-reaction-choice]','reaction choice');
  const baseline=await page.locator('#msgs').evaluate(el=>({top:el.scrollTop,fromEnd:el.scrollHeight-el.scrollTop-el.clientHeight}));
  if(mode==='phone')await page.touchscreen.tap(choiceProbe.x,choiceProbe.y);else await choice.click();
  return {scrollBefore:baseline.top,fromEnd:baseline.fromEnd};
}
async function openTimeline(page,id,mode) {
  if(mode==='phone'&&await page.locator('#chatback').isVisible())await page.locator('#chatback').click();
  await open(page,id);await page.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
}
/* Participant disclosure acceptance (inside the protected stage2 journey). Real pointer and touch input,
   three synthetic views, a local override with markup-like text, a held write, and read-only inspection. */
async function stage2ParticipantDisclosure(mode,s) {
  const {ownerSeat,observerSeat,readerSeat,key,owner,observer,reader}=s;
  const received='team/'+key,ownNode=ownerSeat.node.id;
  const gates={owner:await gateReactionWrites(owner),reader:await gateReactionWrites(reader)};
  const reactionsOf=async()=>(await readerSeat.api('/api/team/subscriptions/'+key+'/reactions?after=0')).events.length;
  const serverNames=async()=>(await readerSeat.api('/api/team/subscriptions/'+key+'/comments?after=0')).names||{};
  const nameOf=(names,seat)=>names[seat]||'Unknown person';
  const LOCAL='<img src=x onerror="window.__disclosureXSS=1">Local name';assert.ok(LOCAL.length<=64,'local name fits the rename limit');
  const targetText='Disclosure target '+mode, ownerText='Disclosure owner comment '+mode;
  // The observer's comment is the target; the owner's comment receives one reaction from the observer.
  await openTimeline(observer,received,mode);
  await observer.locator('#prompt').fill(targetText);await observer.locator('#sendbtn').click();
  const seq=Number(await observer.locator('#msgs .gmsg').filter({hasText:targetText}).last().getAttribute('data-cseq'));
  await openTimeline(owner,ownNode,mode);await owner.locator('#prompt').fill(ownerText);await owner.locator('#sendbtn').click();
  const ownerSeq=Number(await owner.locator('#msgs .gmsg').filter({hasText:ownerText}).last().getAttribute('data-cseq'));
  await openTimeline(reader,received,mode);
  const msg=(page,n)=>page.locator('#msgs .gmsg[data-cseq="'+n+'"]');
  for(const [page,n] of [[owner,seq],[owner,ownerSeq],[reader,seq],[reader,ownerSeq],[observer,seq],[observer,ownerSeq]]) await msg(page,n).waitFor({timeout:40000});
  // Real writes through the existing picker: owner and reader react to the target; observer reacts to the owner's comment.
  await chooseReactionFor(owner,msg(owner,seq),mode,'thumbs_up');
  await msg(reader,seq).locator('[data-reaction="thumbs_up"]').waitFor({timeout:40000});
  await chooseReactionFor(reader,msg(reader,seq),mode,'heart');
  await msg(owner,seq).locator('[data-reaction="heart"]').waitFor({timeout:40000});
  await chooseReactionFor(observer,msg(observer,ownerSeq),mode,'celebration');
  await msg(reader,ownerSeq).locator('[data-reaction="celebration"]').waitFor({timeout:40000});
  // Local override on the observer, saved from the reader's view. The text is markup-like and long.
  // Select the renamed bubble only when it is not selected yet: a repeated click would deselect it and hide its actions.
  const renamed=msg(reader,seq);
  if(!(await renamed.evaluate(el=>el.classList.contains('bubble-selected')))){const p=await safeBubblePoint(renamed);if(mode==='phone')await reader.touchscreen.tap(p.x,p.y);else await reader.mouse.click(p.x,p.y);}
  await renamed.evaluate(el=>el.classList.contains('bubble-selected')||(()=>{throw Error('renamed bubble is not selected');})());
  await reader.locator('[data-gact=more]').click();await reader.locator('#sendto_list [data-choice="0"]').click();
  await reader.locator('[data-grename]').fill(LOCAL);await reader.locator('[data-gact=name-save]').click();
  await until(async()=>(await msg(reader,seq).locator('.gwho').innerText()).includes('Local name'),20000,'local rename reflected in the reader view');
  assertID('disclosure-local-override-escaped-'+mode,await reader.evaluate(()=>window.__disclosureXSS===undefined)&&await reader.locator('#msgs img[src="x"]').count()===0);
  const names=await serverNames();
  const views={owner:owner,reader:reader,observer:observer};
  const rosterOf=async(page,n,emoji)=>{
    const chip=msg(page,n).locator('[data-reaction="'+emoji+'"]');await chip.scrollIntoViewIfNeeded();
    if(mode==='phone'){const probe=await tapProbe(chip,'.reaction-chip','reaction chip');await page.touchscreen.tap(probe.x,probe.y);}else await chip.click();
    const panel=msg(page,n).locator('.reaction-disclosure:not(.reaction-hover)');await panel.waitFor({state:'visible',timeout:10000});
    const items=await panel.locator('li').allInnerTexts();
    await page.keyboard.press('Escape');await panel.waitFor({state:'detached',timeout:10000});
    return items;
  };
  // Three views, names accurate: the reader sees the local override for the observer and their own participation as You.
  assertID('disclosure-owner-view-names-'+mode,JSON.stringify(await rosterOf(owner,seq,'thumbs_up'))===JSON.stringify(['You'])&&JSON.stringify(await rosterOf(owner,seq,'heart'))===JSON.stringify([nameOf(names,readerSeat.self)]));
  assertID('disclosure-reader-view-names-'+mode,JSON.stringify(await rosterOf(reader,seq,'thumbs_up'))===JSON.stringify([nameOf(names,ownerSeat.self)])&&JSON.stringify(await rosterOf(reader,seq,'heart'))===JSON.stringify(['You']));
  assertID('disclosure-reader-local-override-'+mode,JSON.stringify(await rosterOf(reader,ownerSeq,'celebration'))===JSON.stringify([LOCAL]));
  assertID('disclosure-observer-own-message-names-'+mode,JSON.stringify(await rosterOf(observer,seq,'thumbs_up'))===JSON.stringify([nameOf(names,ownerSeat.self)])&&JSON.stringify(await rosterOf(observer,seq,'heart'))===JSON.stringify([nameOf(names,readerSeat.self)]));
  assertID('disclosure-observer-own-reaction-you-'+mode,JSON.stringify(await rosterOf(observer,ownerSeq,'celebration'))===JSON.stringify(['You']));
  assertID('disclosure-owner-own-message-names-'+mode,JSON.stringify(await rosterOf(owner,ownerSeq,'celebration'))===JSON.stringify([nameOf(names,observerSeat.self)]));
  // Inspection performs no write, leaves the selection, draft and scroll alone.
  await reader.locator('#prompt').fill('Reader draft kept '+mode);
  // Setup scrolling and selection happen before the baseline: the chip is revealed, and the renamed bubble is deselected.
  const isSelected=()=>msg(reader,seq).evaluate(el=>el.classList.contains('bubble-selected'));
  await msg(reader,seq).locator('[data-reaction="thumbs_up"]').scrollIntoViewIfNeeded();
  if(await isSelected()){const p=await safeBubblePoint(msg(reader,seq));if(mode==='phone')await reader.touchscreen.tap(p.x,p.y);else await reader.mouse.click(p.x,p.y);await until(async()=>!(await isSelected()),10000,'renamed bubble deselected before the baseline');}
  const before={writes:await reactionsOf(),scroll:await reader.locator('#msgs').evaluate(el=>el.scrollTop),selected:await msg(reader,seq).evaluate(el=>el.classList.contains('bubble-selected'))};
  const posted=gates.reader.bodies.length+gates.owner.bodies.length;
  // Real pointer: hover preview is revealed, the pointer moves in steps onto it, and it stays until the pointer leaves both.
  if(mode==='desktop'){
    const chip=msg(reader,seq).locator('[data-reaction="thumbs_up"]');await chip.scrollIntoViewIfNeeded();
    const box=await chip.boundingBox();const cx=box.x+box.width/2,cy=box.y+box.height/2;
    await reader.mouse.move(cx,cy,{steps:4});
    const preview=msg(reader,seq).locator('.reaction-disclosure.reaction-hover');await preview.waitFor({state:'visible',timeout:10000});
    const pb=await preview.boundingBox();
    await reader.mouse.move(pb.x+pb.width/2,pb.y+pb.height/2,{steps:12});
    assertID('disclosure-hover-reached-panel-'+mode,await preview.isVisible());
    const cw=await reader.locator('#msgs').evaluate(el=>el.getBoundingClientRect().toJSON());
    assertID('disclosure-hover-inside-pane-'+mode,pb.x>=cw.left-1&&pb.x+pb.width<=cw.right+1&&pb.y>=cw.top-1&&pb.y+pb.height<=cw.bottom+1&&pb.x>=0&&pb.x+pb.width<=(await reader.evaluate(()=>innerWidth))+1);
    await screenshot(reader,'disclosure-hover-'+mode+'.png');
    await reader.mouse.move(5,5,{steps:12});
    await preview.waitFor({state:'detached',timeout:10000});
    assertID('disclosure-hover-leaves-dismiss-'+mode,true);
  }
  // Pinned panel: activation pins it; the pointer leaving does not dismiss it; outside activation does.
  const chip=msg(reader,seq).locator('[data-reaction="thumbs_up"]');await chip.scrollIntoViewIfNeeded();
  if(mode==='phone'){const probe=await tapProbe(chip,'.reaction-chip','reaction chip');await reader.touchscreen.tap(probe.x,probe.y);}else await chip.click();
  const pinned=msg(reader,seq).locator('.reaction-disclosure:not(.reaction-hover)');await pinned.waitFor({state:'visible',timeout:10000});
  assertID('disclosure-pinned-expanded-'+mode,await chip.getAttribute('aria-expanded')==='true');
  if(mode==='phone'){
    assertID('disclosure-compact-tap-uses-sheet-'+mode,await pinned.first().evaluate(el=>el.classList.contains('reaction-sheet')));
    // The sheet keeps its declared 8px viewport margins; an anchored width must not survive into it.
    const sheetBox=await pinned.first().boundingBox(),viewportWidth=await reader.evaluate(()=>innerWidth);
    assertID('disclosure-compact-sheet-declared-margins-'+mode,Math.abs(sheetBox.x-8)<=1&&Math.abs(sheetBox.x+sheetBox.width-(viewportWidth-8))<=1);
  }
  if(mode==='desktop'){await reader.mouse.move(5,5,{steps:6});assertID('disclosure-pinned-survives-pointer-departure-'+mode,await pinned.isVisible());}
  const draftBefore=await reader.locator('#prompt').innerText();
  await reader.locator('#chattitle').click();
  await pinned.waitFor({state:'detached',timeout:10000});
  assertID('disclosure-outside-dismiss-'+mode,true);
  // Escape closes a pinned panel and returns focus to its chip.
  await chip.click();await pinned.waitFor({state:'visible',timeout:10000});
  await reader.keyboard.press('Escape');await pinned.waitFor({state:'detached',timeout:10000});
  assertID('disclosure-escape-focus-return-'+mode,await chip.evaluate(el=>document.activeElement===el));
  const after={writes:await reactionsOf(),scroll:await reader.locator('#msgs').evaluate(el=>el.scrollTop),selected:await msg(reader,seq).evaluate(el=>el.classList.contains('bubble-selected'))};
  assertID('disclosure-no-write-selection-scroll-draft-'+mode,after.writes===before.writes&&after.scroll===before.scroll&&after.selected===before.selected&&!after.selected&&(gates.reader.bodies.length+gates.owner.bodies.length)===posted&&(await reader.locator('#prompt').innerText())===draftBefore&&draftBefore==='Reader draft kept '+mode);
  // Pending own add: the write is held, the chip and its roster say pending, then the retained readback settles it.
  const arrival=gates.reader.hold();
  await chooseReactionFor(reader,msg(reader,ownerSeq),mode,'laughing');
  await arrival;
  const pendingChip=msg(reader,ownerSeq).locator('[data-reaction="laughing"].reaction-pending');await pendingChip.waitFor({timeout:10000});
  const pendingItems=await rosterOf(reader,ownerSeq,'laughing');
  assertID('disclosure-pending-own-label-'+mode,JSON.stringify(pendingItems)===JSON.stringify(['You — pending confirmation']));
  gates.reader.release('continue');
  await msg(reader,ownerSeq).locator('[data-reaction="laughing"]:not(.reaction-pending)').waitFor({timeout:40000});
  assertID('disclosure-pending-settles-confirmed-'+mode,JSON.stringify(await rosterOf(reader,ownerSeq,'laughing'))===JSON.stringify(['You']));
  if(mode==='phone'){
    // Narrow viewport: the disclosure stays inside the viewport, as a panel or as the compact sheet with its own close control.
    const original=reader.viewportSize();await reader.setViewportSize({width:320,height:640});
    await msg(reader,seq).locator('[data-reaction="thumbs_up"]').scrollIntoViewIfNeeded();
    const narrow=msg(reader,seq).locator('[data-reaction="thumbs_up"]');const probe=await tapProbe(narrow,'.reaction-chip','narrow reaction chip');
    await reader.touchscreen.tap(probe.x,probe.y);
    const shown=msg(reader,seq).locator('.reaction-disclosure:not(.reaction-hover)');await shown.first().waitFor({state:'visible',timeout:10000});
    const box=await shown.first().boundingBox(),sheet=await shown.first().evaluate(el=>el.classList.contains('reaction-sheet'));
    assertID('disclosure-narrow-inside-viewport-'+mode,box.x>=0&&box.x+box.width<=320+1&&box.y>=0&&box.y+box.height<=640+1);
    await screenshot(reader,'disclosure-narrow-320-'+mode+'.png');
    assertID('disclosure-narrow-320-sheet-'+mode,sheet);
    const close=msg(reader,seq).locator('[data-reaction-close]');const cp=await tapProbe(close,'[data-reaction-close]','sheet close',{viewport:true});await reader.touchscreen.tap(cp.x,cp.y);
    await shown.first().waitFor({state:'detached',timeout:10000});assertID('disclosure-sheet-close-dismisses-'+mode,true);
    // Focus on the sheet's close control survives a retained roster change made by another seat while the sheet is open.
    const focusProbe=await tapProbe(msg(reader,seq).locator('[data-reaction="thumbs_up"]'),'.reaction-chip','focus chip');await reader.touchscreen.tap(focusProbe.x,focusProbe.y);
    const openSheet=msg(reader,seq).locator('.reaction-disclosure.reaction-sheet');await openSheet.waitFor({state:'visible',timeout:10000});
    const focusClose=openSheet.locator('[data-reaction-close]');await focusClose.focus();
    const readerPage=await readerSeat.api('/api/team/subscriptions/'+key+'/reactions?after=0');
    await readerSeat.api('/api/team/subscriptions/'+key+'/reactions?copy_instance='+encodeURIComponent(readerPage.copy_instance),'POST',{target:seq,emoji:'thumbs_up',active:true,key:'f0c0500000000001'});
    await msg(reader,seq).locator('.reaction-disclosure.reaction-sheet li').nth(1).waitFor({timeout:40000});
    assertID('disclosure-focus-kept-on-refresh-'+mode,await focusClose.evaluate(el=>document.activeElement===el));
    const fcp=await tapProbe(focusClose,'[data-reaction-close]','focused sheet close',{viewport:true});await reader.touchscreen.tap(fcp.x,fcp.y);await shown.first().waitFor({state:'detached',timeout:10000});
    // The test-owned reader contribution is removed through the same API, and the reader's readback confirms it before the owner's removal below.
    await readerSeat.api('/api/team/subscriptions/'+key+'/reactions?copy_instance='+encodeURIComponent(readerPage.copy_instance),'POST',{target:seq,emoji:'thumbs_up',active:false,key:'f0c0500000000002'});
    await until(async()=>(await msg(reader,seq).locator('[data-reaction="thumbs_up"]').innerText()).includes('1')&&!(await msg(reader,seq).locator('[data-reaction="thumbs_up"]').innerText()).includes('2'),40000,'test-owned reader reaction removed and read back');
    await reader.setViewportSize(original);
  }
  if(mode==='desktop'){
    // Short-bubble regression, in the owner's view: the observer's author label is still its short server name there,
    // because the reader's local rename is private to the reader. Two confirmed participants are waited for on the exact
    // sequence before the owner's chip is opened, and the timeline must not grow.
    await observer.locator('#prompt').fill('OK');
    const okPosted=observer.waitForResponse(r=>r.request().method()==='POST'&&new URL(r.url()).pathname.endsWith('/comments')&&(()=>{try{return JSON.parse(r.request().postData()||'{}').text==='OK';}catch{return false;}})(),{timeout:30000});
    await observer.locator('#sendbtn').click();
    const okSeq=Number((await (await okPosted).json()).seq);
    assert.ok(Number.isSafeInteger(okSeq)&&okSeq>0,'the new OK comment has its own retained sequence');
    await msg(owner,okSeq).waitFor({timeout:40000});await msg(reader,okSeq).waitFor({timeout:40000});
    await chooseReactionFor(owner,msg(owner,okSeq),mode,'thumbs_up');
    await chooseReactionFor(reader,msg(reader,okSeq),mode,'thumbs_up');
    await msg(owner,okSeq).locator('[data-reaction="thumbs_up"] span').last().filter({hasText:'2'}).waitFor({timeout:40000});
    const okChip=msg(owner,okSeq).locator('[data-reaction="thumbs_up"]');await okChip.scrollIntoViewIfNeeded();
    const scrollBefore=await owner.locator('#msgs').evaluate(el=>el.scrollHeight);
    await okChip.click();
    const okPanel=msg(owner,okSeq).locator('.reaction-disclosure:not(.reaction-hover)');await okPanel.waitFor({state:'visible',timeout:10000});
    const measured=await okPanel.evaluate(el=>{const b=el.closest('.bubble').getBoundingClientRect(),p=el.getBoundingClientRect(),m=document.querySelector('#msgs').getBoundingClientRect();return {panel:p.width,bubble:b.width,sheet:el.classList.contains('reaction-sheet'),inside:p.left>=m.left-1&&p.right<=m.right+1&&p.top>=m.top-1&&p.bottom<=m.bottom+1,items:[...el.querySelectorAll('li')].map(li=>li.textContent)};});
    const author=await msg(owner,okSeq).locator('.gwho').innerText();
    // Private diagnostic capture only: never copied into the public manifest.
    console.error('SHORT_BUBBLE_PRIVATE '+JSON.stringify({mode,measured,author}));
    assertID('disclosure-short-bubble-wide-panel-'+mode,!measured.sheet&&measured.panel>measured.bubble&&measured.items.length===2&&measured.inside);
    assertID('disclosure-short-bubble-no-timeline-growth-'+mode,await owner.locator('#msgs').evaluate(el=>el.scrollHeight)===scrollBefore);
    await screenshot(owner,'disclosure-short-bubble-'+mode+'.png');
    await owner.locator('#chattitle').click();await okPanel.waitFor({state:'detached',timeout:10000});
  }
  // Removal: the owner removes its reaction; the target's thumbs_up chip leaves the reader's view.
  await chooseReactionFor(owner,msg(owner,seq),mode,'thumbs_up');
  await msg(reader,seq).locator('[data-reaction="thumbs_up"]').waitFor({state:'detached',timeout:40000});
  assertID('disclosure-removal-reflected-'+mode,await msg(reader,seq).locator('[data-reaction="heart"]').count()===1);
  await screenshot(owner,'disclosure-owner-'+mode+'.png');
  manifest['disclosure-'+mode]={seq:'synthetic-target',ownerSeq:'synthetic-owner-comment',participants:Object.keys(names).length};
}

async function stage2InstantReactions(mode,s) {
  const {ownerSeat,observerSeat,readerSeat,otherSeat,share,key,owner,observer,reader}=s;
  await ownerSeat.api('/api/team/shares/'+ownerSeat.node.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'},{seat:observerSeat.self,access:'write'},{seat:otherSeat.self,access:'read'}]});
  await until(async()=>{const list=await observerSeat.api('/api/team/subscriptions');return list.subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write');},30000,'instant reaction observer writable');
  const received='team/'+key,ownNode=ownerSeat.node.id;
  const gates={observer:await gateReactionWrites(observer),owner:await gateReactionWrites(owner)};
  const reactionsOf=async seat=>(await seat.api('/api/team/subscriptions/'+key+'/reactions?after=0')).events.length;
  const chipLabel=async(page,message,emoji)=>(await reactionChipsOf(message)).find(c=>c.emoji===emoji);
  try {
    await openTimeline(reader,received,mode);
    const text='Instant reaction target '+mode;
    await reader.locator('#prompt').fill(text);await reader.locator('#sendbtn').click();
    const readerTarget=reader.locator('#msgs .gmsg').filter({hasText:text}).last();await readerTarget.waitFor({timeout:30000});
    const seq=Number(await readerTarget.getAttribute('data-cseq'));
    // Filler after the target keeps the reacted message away from the timeline end, outside the end-follow zone.
    // The UI sends comments at the frontier anchor and the timeline sorts by anchor, then seq. The filler reuses the
    // target's retained anchor, so each filler comment sorts after the target by its later seq.
    const targetEntry=await until(async()=>(await readerSeat.api('/api/team/subscriptions/'+key+'/comments?after=0')).entries.find(x=>x.text===text&&Number.isSafeInteger(x.anchor)),20000,'instant target retained');
    for(let i=0;i<14;i++)await readerSeat.api('/api/team/subscriptions/'+key+'/comments','POST',{text:'Synthetic non-bottom filler '+mode+' '+i,anchor:targetEntry.anchor,key:(mode==='desktop'?'f11e4000':'f11e5000')+i.toString(16).padStart(8,'0')});
    await openTimeline(observer,received,mode);await openTimeline(owner,ownNode,mode);
    const observerMessage=observer.locator('#msgs .gmsg[data-cseq="'+seq+'"]'),ownerMessage=owner.locator('#msgs .gmsg[data-cseq="'+seq+'"]'),readerMessage=reader.locator('#msgs .gmsg[data-cseq="'+seq+'"]');
    await observerMessage.waitFor({timeout:40000});await ownerMessage.waitFor({timeout:40000});await readerMessage.waitFor({timeout:40000});
    // The baseline is taken only after the final filler is loaded in both observer and owner timelines.
    const finalFiller='Synthetic non-bottom filler '+mode+' 13';
    await observer.locator('#msgs').getByText(finalFiller,{exact:true}).waitFor({state:'attached',timeout:40000});
    await owner.locator('#msgs').getByText(finalFiller,{exact:true}).waitFor({state:'attached',timeout:40000});

    // Recipient reacts to another participant's comment; the write is held.
    const observerDraft='Draft kept during reaction feedback '+mode;
    await observer.locator('#prompt').fill(observerDraft);
    const readerBefore=await reactionsOf(readerSeat),ownerBefore=await reactionsOf(ownerSeat);
    const arrival=gates.observer.hold();
    const first=await chooseReactionFor(observer,observerMessage,mode,'heart');
    const scrollBefore=first.scrollBefore;
    assertID('instant-recipient-baseline-outside-end-follow-'+mode,first.fromEnd>80);
    await arrival;
    // First choice only: measured right after its POST arrives and before any second picker interaction.
    const scrollHeld=await observer.locator('#msgs').evaluate(el=>el.scrollTop);
    assertID('instant-recipient-scroll-kept-while-pending-'+mode,Math.abs(scrollHeld-scrollBefore)<2);
    assertID('instant-recipient-preview-before-post-'+mode,JSON.stringify(await reactionChipsOf(observerMessage))===JSON.stringify([{emoji:'heart',label:'❤️ 1, you reacted, pending confirmation',pending:true,mine:true}]));
    assertID('instant-recipient-one-write-in-flight-'+mode,gates.observer.bodies.length===1);
    // Repeat selection on the same pending heart: its own pre-choice baseline, no write.
    const repeat=await chooseReactionFor(observer,observerMessage,mode,'heart');
    const repeatBefore=repeat.scrollBefore;
    assertID('instant-recipient-repeat-baseline-outside-end-follow-'+mode,repeat.fromEnd>80);
    assertID('instant-recipient-repeat-selection-sends-nothing-'+mode,gates.observer.bodies.length===1);
    assertID('instant-recipient-repeat-selection-scroll-kept-'+mode,Math.abs((await observer.locator('#msgs').evaluate(el=>el.scrollTop))-repeatBefore)<2);
    assertID('instant-recipient-no-fabricated-state-'+mode,await reactionsOf(readerSeat)===readerBefore&&await reactionsOf(ownerSeat)===ownerBefore&&(await reactionChipsOf(readerMessage)).length===0&&(await reactionChipsOf(ownerMessage)).length===0);
    assertID('instant-recipient-composer-kept-while-pending-'+mode,(await observer.locator('#prompt').innerText())===observerDraft);
    await screenshot(observer,'instant-reaction-recipient-pending-'+mode+'.png');
    // Final held position, immediately before the POST is released.
    const scrollReleased=await observer.locator('#msgs').evaluate(el=>el.scrollTop);
    const pendingChip=await reactionChipGeometry(observerMessage,'heart');
    gates.observer.release('continue');
    await until(async()=>{const c=await chipLabel(observer,observerMessage,'heart');return c&&!c.pending&&c.label==='❤️ 1, you reacted';},40000,'recipient confirmation');
    assertID('instant-recipient-confirmed-once-'+mode,JSON.stringify(await reactionChipsOf(observerMessage))===JSON.stringify([{emoji:'heart',label:'❤️ 1, you reacted',pending:false,mine:true}]));
    await until(async()=>(await chipLabel(owner,ownerMessage,'heart'))?.label==='❤️ 1',40000,'owner sees recipient reaction');
    await until(async()=>(await chipLabel(reader,readerMessage,'heart'))?.label==='❤️ 1',40000,'reader sees recipient reaction');
    const confirmedChip=await reactionChipGeometry(observerMessage,'heart');
    assertID('instant-chip-geometry-same-pending-and-confirmed-'+mode,pendingChip.count==='1'&&confirmedChip.count==='1'&&Math.abs(pendingChip.height-confirmedChip.height)<0.5&&pendingChip.padding===confirmedChip.padding&&pendingChip.fontSize===confirmedChip.fontSize&&pendingChip.lineHeight===confirmedChip.lineHeight&&pendingChip.fontStyle===confirmedChip.fontStyle&&pendingChip.borderWidth===confirmedChip.borderWidth);
    assertID('instant-chip-pending-dashed-confirmed-solid-'+mode,pendingChip.borderStyle==='dashed'&&confirmedChip.borderStyle==='solid');
    assertID('instant-recipient-composer-and-scroll-kept-after-confirm-'+mode,(await observer.locator('#prompt').innerText())===observerDraft&&Math.abs((await observer.locator('#msgs').evaluate(el=>el.scrollTop))-scrollReleased)<2);
    await observer.locator('#prompt').fill('');

    // Owner reacts in the normal timeline: add, then remove with an explicit rejection.
    const readerBeforeOwner=await reactionsOf(readerSeat),observerBeforeOwner=await reactionsOf(observerSeat);
    const arrivalOwner=gates.owner.hold();
    await chooseReactionFor(owner,ownerMessage,mode,'thumbs_up');
    await arrivalOwner;
    assertID('instant-owner-add-preview-before-post-'+mode,JSON.stringify(await reactionChipsOf(ownerMessage))===JSON.stringify([{emoji:'heart',label:'❤️ 1',pending:false,mine:false},{emoji:'thumbs_up',label:'👍 1, you reacted, pending confirmation',pending:true,mine:true}]));
    assertID('instant-owner-add-no-fabricated-state-'+mode,await reactionsOf(readerSeat)===readerBeforeOwner&&await reactionsOf(observerSeat)===observerBeforeOwner&&(await reactionChipsOf(readerMessage)).every(c=>c.emoji!=='thumbs_up'));
    await screenshot(owner,'instant-reaction-owner-pending-'+mode+'.png');
    gates.owner.release('continue');
    await until(async()=>{const c=await chipLabel(owner,ownerMessage,'thumbs_up');return c&&!c.pending&&c.label==='👍 1, you reacted';},40000,'owner confirmation');
    assertID('instant-owner-add-confirmed-once-'+mode,JSON.stringify(await reactionChipsOf(ownerMessage))===JSON.stringify([{emoji:'heart',label:'❤️ 1',pending:false,mine:false},{emoji:'thumbs_up',label:'👍 1, you reacted',pending:false,mine:true}]));
    await until(async()=>(await chipLabel(reader,readerMessage,'thumbs_up'))?.label==='👍 1',40000,'reader sees owner reaction');
    await until(async()=>(await chipLabel(observer,observerMessage,'thumbs_up'))?.label==='👍 1',40000,'recipient sees owner reaction');

    const heldRemoval=gates.owner.hold();
    await chooseReactionFor(owner,ownerMessage,mode,'thumbs_up');
    await heldRemoval;
    assertID('instant-owner-removal-hides-sole-chip-while-pending-'+mode,JSON.stringify(await reactionChipsOf(ownerMessage))===JSON.stringify([{emoji:'heart',label:'❤️ 1',pending:false,mine:false}]));
    assertID('instant-owner-removal-others-unchanged-'+mode,(await chipLabel(reader,readerMessage,'thumbs_up'))?.label==='👍 1'&&(await chipLabel(observer,observerMessage,'thumbs_up'))?.label==='👍 1');
    const responded=owner.waitForResponse(r=>r.request().method()==='POST'&&r.url().includes('/reactions?'),{timeout:30000});
    gates.owner.release('reject');
    await responded;
    await until(async()=>(await owner.locator('#toast').textContent())==='Reaction was not saved.',10000,'explicit rejection feedback');
    assertID('instant-rejection-restores-confirmed-display-'+mode,JSON.stringify(await reactionChipsOf(ownerMessage))===JSON.stringify([{emoji:'heart',label:'❤️ 1',pending:false,mine:false},{emoji:'thumbs_up',label:'👍 1, you reacted',pending:false,mine:true}]));
    assertID('instant-rejection-is-not-a-stored-removal-'+mode,(await chipLabel(reader,readerMessage,'thumbs_up'))?.label==='👍 1');

    // Uncertain delivery: the browser aborts the first write; the explicit retry reuses its body.
    await owner.evaluate(()=>{const toast=document.querySelector('#toast');if(toast)toast.textContent='sentinel';});
    const beforeUncertain=gates.owner.bodies.length;
    gates.owner.next('abort');
    await chooseReactionFor(owner,ownerMessage,mode,'laughing');
    await until(async()=>{const c=await chipLabel(owner,ownerMessage,'laughing');return c&&c.pending&&c.label==='😂 1, you reacted, not confirmed';},40000,'uncertain laughing');
    assertID('instant-uncertain-not-reported-as-failure-'+mode,(await owner.locator('#toast').textContent())==='sentinel');
    gates.owner.next('continue');
    await chooseReactionFor(owner,ownerMessage,mode,'laughing');
    await until(async()=>{const c=await chipLabel(owner,ownerMessage,'laughing');return c&&!c.pending&&c.label==='😂 1, you reacted';},40000,'retained laughing');
    assertID('instant-retry-reuses-exact-body-'+mode,gates.owner.bodies.length===beforeUncertain+2&&gates.owner.bodies[beforeUncertain]===gates.owner.bodies[beforeUncertain+1]);
    await until(async()=>(await chipLabel(observer,observerMessage,'laughing'))?.label==='😂 1',40000,'recipient sees retained laughing');
    assertID('instant-retry-confirmed-once-'+mode,(await reactionChipsOf(ownerMessage)).filter(c=>c.emoji==='laughing').length===1);

    // Stale response after navigation: the write is held while the owner leaves the chat.
    const arrivalNav=gates.owner.hold();
    await chooseReactionFor(owner,ownerMessage,mode,'celebration');
    await arrivalNav;
    const sideNode=await node(ownerSeat,'codex','Owner side chat '+mode,'Synthetic side chat for navigation');
    await openTimeline(owner,sideNode.id,mode);
    const toastBeforeStale=await toastText(owner);
    const staleResponse=owner.waitForResponse(r=>r.request().method()==='POST'&&r.url().includes('/reactions?'),{timeout:30000});
    gates.owner.release('continue');
    await staleResponse;
    await openTimeline(owner,ownNode,mode);
    await until(async()=>{const c=await chipLabel(owner,ownerMessage,'celebration');return c&&!c.pending&&c.label==='🎉 1, you reacted';},40000,'stale celebration confirmed on return');
    assertID('instant-stale-response-no-toast-'+mode,(await toastText(owner))===toastBeforeStale);
    assertID('instant-stale-response-leaves-no-pending-'+mode,!(await reactionChipsOf(ownerMessage)).some(c=>c.pending));
    manifest['instant-reactions-'+mode]={seq,recipient:await reactionChipsOf(observerMessage),owner:await reactionChipsOf(ownerMessage)};
  } finally {
    gates.observer.release();gates.owner.release();
    await observer.unrouteAll({behavior:'wait'});await owner.unrouteAll({behavior:'wait'});
  }
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
  await page.locator('#destinationpeople').click();
  if(await page.locator('#attadd').isVisible()) {
    await page.locator('#prompt').evaluate(el=>{const data=new DataTransfer();data.items.add(new File(['ignore'],'people.txt',{type:'text/plain'}));el.dispatchEvent(new ClipboardEvent('paste',{bubbles:true,cancelable:true,clipboardData:data}));});
    await page.locator('#attstage .stagechip.done[title="people.txt"]').waitFor({timeout:30000});
    assertID('stage1-people-file-enabled-'+mode,(await page.locator('#destinationpeople').getAttribute('aria-pressed'))==='true');
    await page.locator('#promptbar').evaluate(el=>{const data=new DataTransfer();data.items.add(new File(['ignore'],'people-drop.txt',{type:'text/plain'}));el.dispatchEvent(new DragEvent('drop',{bubbles:true,cancelable:true,dataTransfer:data}));});
    await page.locator('#attstage .stagechip.done[title="people-drop.txt"]').waitFor({timeout:30000});
    assertID('stage1-people-drop-enabled-'+mode,(await page.locator('#destinationpeople').getAttribute('aria-pressed'))==='true');
    await page.locator('#attstage .stagechip[title="people.txt"] .sx').click();await page.locator('#attstage .stagechip[title="people-drop.txt"] .sx').click();
  } else {
    let dialog=page.waitForEvent('dialog');
    const paste=page.locator('#prompt').evaluate(el=>{const data=new DataTransfer();data.items.add(new File(['ignore'],'people.txt',{type:'text/plain'}));el.dispatchEvent(new ClipboardEvent('paste',{bubbles:true,cancelable:true,clipboardData:data}));});
    const warning=await dialog;assertID('stage1-people-file-feedback-'+mode,/text only/i.test(warning.message()));await warning.accept();await paste;
    dialog=page.waitForEvent('dialog');const drop=page.locator('#promptbar').evaluate(el=>{const data=new DataTransfer();data.items.add(new File(['ignore'],'people-drop.txt',{type:'text/plain'}));el.dispatchEvent(new DragEvent('drop',{bubbles:true,cancelable:true,dataTransfer:data}));});
    const dropWarning=await dialog;assertID('stage1-people-drop-feedback-'+mode,/text only/i.test(dropWarning.message()));await dropWarning.accept();await drop;
  }
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
  // What leaves the owner's computer names the file, never the owner's local path.
  const shared=await ownerSeat.api('/api/team/shares/'+chat.id),mirrorLog=path.join(relay.dir,'channels',ownerSeat.self,shared.uid,'mirror.jsonl');
  const relayed=await until(()=>{try{const text=fs.readFileSync(mirrorLog,'utf8');return text.includes('Owner file prompt '+mode)?text:null;}catch{return null;}},40000,'owner file turn on relay');
  assertID('stage1-owner-file-name-only-on-relay-'+mode,relayed.includes('[attached file: owner-file.txt]')&&!relayed.includes(ownerSeat.data)&&!relayed.includes(path.dirname(body.attachments[0].path)));
}

async function peopleCopyReplacement(mode,ownerSeat,readerSeat){
 const chat=await node(ownerSeat,'codex','People copy replacement '+mode,'Synthetic copy replacement setup');
 const sharePath='/api/team/shares/'+chat.id;const grant={server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]};
 await ownerSeat.api(sharePath,'PUT',grant);const share=await ownerSeat.api(sharePath);
 const getSub=async()=> (await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.owner===ownerSeat.self&&x.chat===share.uid&&x.generation===share.generation&&x.access==='write');
 const old=await until(getSub,40000,'original copy'),key=[old.server,old.owner,old.chat,old.generation].join('/'),base='/api/team/subscriptions/'+key;
 assert.ok(/^[0-9a-f]{32}$/.test(old.people_copy_instance));
 const page=await pageFor(readerSeat,mode,'people-copy-replacement-'+mode);await open(page,'team/'+key);await page.locator('#attadd').waitFor({state:'visible',timeout:30000});
 const before={owner:counts(ownerSeat),recipient:counts(readerSeat)};let entered,release,heldURL,first=true;
 const reached=new Promise(r=>entered=r),hold=new Promise(r=>release=r);heldReleases.add(release);
 await page.route('**'+base+'/attachments?*',async route=>{if(!first){await route.continue();return;}first=false;heldURL=route.request().url();entered();await hold;if(!page.isClosed())await route.continue();});
 try{
  await page.locator('#prompt').fill('Synthetic earlier People draft '+mode);
  await page.locator('#attfile').setInputFiles({name:'old-copy.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic old copy bytes')});
  await Promise.race([reached,delay(15000).then(()=>{throw Error('old upload not held')})]);
  const stageID=new URL(heldURL).searchParams.get('stage_id');assert.equal(new URL(heldURL).searchParams.get('copy_instance'),old.people_copy_instance);
  await readerSeat.api(base,'DELETE');await ownerSeat.api(sharePath,'PUT',grant);
  const fresh=await until(async()=>{const sub=await getSub();return sub?.people_copy_instance&&sub.people_copy_instance!==old.people_copy_instance?sub:null;},45000,'replacement copy identity');
  assertID('people-copy-replacement-distinct-identity-'+mode,fresh.generation===old.generation&&fresh.people_copy_instance!==old.people_copy_instance);
  await page.waitForResponse(async response=>{if(new URL(response.url()).pathname!=='/api/team/subscriptions'||response.status()!==200)return false;try{return (await response.json()).subscriptions?.some(item=>item.chat===fresh.chat&&item.generation===fresh.generation&&item.people_copy_instance===fresh.people_copy_instance)}catch{return false}},{timeout:40000});
  await page.locator('#draftrecovery a[download="old-copy.txt"]').waitFor({timeout:40000});
  const reviewTarget=await page.locator('#draftrecovery a[download="old-copy.txt"]').boundingBox();
  assertID('people-retired-download-touch-target-'+mode,reviewTarget?.height>=44);
  if(mode==='phone'&&await page.locator('#chatback').isVisible())await page.locator('#chatback').click();
  await page.locator('[data-open="team/'+key+'"]').waitFor({state:'visible',timeout:30000});await open(page,'team/'+key);await page.locator('#attadd').waitFor({state:'visible',timeout:30000});
  assertID('people-replacement-does-not-inherit-old-draft-'+mode,(await page.locator('#prompt').innerText())===''&&await page.locator('#attstage .stagechip[title="old-copy.txt"]').count()===0);
  await page.locator('#prompt').fill('Synthetic replacement People draft '+mode);
  await page.locator('#attfile').setInputFiles({name:'new-copy.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic replacement copy bytes')});await page.locator('#attstage .stagechip.done[title="new-copy.txt"]').waitFor({timeout:30000});
  const response=page.waitForResponse(r=>r.request().method()==='POST'&&r.url()===heldURL,{timeout:30000});release();heldReleases.delete(release);
  const stale=await response;assertID('people-old-upload-rejected-after-copy-recreation-'+mode,stale.status()===409&&(await stale.json()).error==='copy_changed');
  const missing=await fetch(readerSeat.base+base+'/attachments/stage/'+stageID+'?copy_instance='+fresh.people_copy_instance,{headers:{Origin:readerSeat.base,'X-Scimux-CSRF':readerSeat.csrf}});
  assertID('people-old-stage-absent-from-replacement-'+mode,missing.status===404);
  assertID('people-new-draft-and-file-preserved-'+mode,(await page.locator('#prompt').innerText())==='Synthetic replacement People draft '+mode&&await page.locator('#attstage .stagechip[title="old-copy.txt"]').count()===0&&await page.locator('#attstage .stagechip.done[title="new-copy.txt"]').count()===1);
  await page.locator('#prompt').click();await page.locator('#attadd').click();const picker=page.waitForEvent('filechooser');await page.locator('#attmenu [data-src=file]').click();
  await (await picker).setFiles({name:'remove-copy.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic removable copy bytes')});
  await page.locator('#attstage .stagechip.done[title="remove-copy.txt"]').waitFor({timeout:30000});await page.locator('#attstage .stagechip[title="remove-copy.txt"] .sx').click();
  await page.locator('#attstage .stagechip[title="remove-copy.txt"]').waitFor({state:'detached',timeout:30000});
  assertID('people-replacement-controls-actionable-'+mode,await page.locator('#attstage .stagechip[title="remove-copy.txt"]').count()===0&&await page.locator('#attstage .stagechip.done[title="new-copy.txt"]').count()===1);
  await screenshot(page,'people-copy-replacement-'+mode+'.png');
  await page.locator('#sendbtn').click();await page.locator('#msgs .gmsg').filter({hasText:'Synthetic replacement People draft '+mode}).waitFor({timeout:40000});
  assertID('people-replacement-send-actionable-'+mode,(await page.locator('#prompt').innerText())==='');
  assertID('people-copy-replacement-zero-agent-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
  manifest['people-copy-replacement-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};
 }finally{release();heldReleases.delete(release);await page.unrouteAll({behavior:'wait'});}
}


async function peopleFiles(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','People files '+mode,'Synthetic People file setup');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+chat.id);
  const subscription=await until(async()=>{const list=await readerSeat.api('/api/team/subscriptions');return list.subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write');},40000,'People file subscription');
  const key=[subscription.server,subscription.owner,subscription.chat,subscription.generation].join('/');
  const owner=await pageFor(ownerSeat,mode,'people-files-owner-'+mode),reader=await pageFor(readerSeat,mode,'people-files-recipient-'+mode);
  await open(owner,chat.id);await open(reader,'team/'+key);
  await owner.locator('#msgs').getByText('Synthetic response 1',{exact:true}).waitFor({timeout:30000});
  const before={owner:counts(ownerSeat),recipient:counts(readerSeat)};
  await reader.locator('#attadd').waitFor({state:'visible',timeout:12000});
  await reader.locator('#attadd').click();const chooser=reader.waitForEvent('filechooser');await reader.locator('#attmenu [data-src=file]').click();
  await (await chooser).setFiles({name:'recipient-synthetic.txt',mimeType:'text/plain',buffer:Buffer.from('recipient synthetic file')});
  await reader.locator('#attstage .stagechip.done[title="recipient-synthetic.txt"]').waitFor({timeout:30000});
  assertID('people-recipient-stage-'+mode,(await reader.locator('#prompt').innerText())==='');
  await reader.locator('#sendbtn').click();
  const incoming=owner.locator('#msgs .gmsg').filter({hasText:'recipient-synthetic.txt'}).last();await incoming.waitFor({timeout:40000});
  const sourceSeq=Number(await incoming.getAttribute('data-cseq'));
  assertID('people-recipient-file-only-'+mode,await incoming.locator('.attfile').count()===1);
  const ownerCaption='Owner <synthetic> & caption '+mode,ownerName='owner-<synthetic>&.txt';
  await owner.locator('#prompt').fill(ownerCaption);
  await owner.locator('#attfile').setInputFiles({name:ownerName,mimeType:'text/plain',buffer:Buffer.from('owner synthetic file')});
  await owner.locator('#attstage .stagechip.done[title="owner-<synthetic>&.txt"]').waitFor({timeout:30000});await owner.locator('#sendbtn').click();
  const returned=reader.locator('#msgs .gmsg').filter({hasText:ownerCaption}).last();await returned.waitFor({timeout:40000});
  assertID('people-owner-text-and-file-'+mode,await returned.getByText(ownerName,{exact:true}).count()===1);
  const downloadPromise=reader.waitForEvent('download');await returned.getByText(ownerName,{exact:true}).click();const download=await downloadPromise;
  assertID('people-recipient-download-'+mode,(await fs.promises.readFile(await download.path(),'utf8'))==='owner synthetic file');
  assertID('people-files-no-implicit-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
  await incoming.locator('.bubble').click({position:{x:5,y:5}});await owner.locator('[data-gact=prompt]').click();
  await owner.locator('#destinationagent[aria-pressed=true]').waitFor({timeout:30000});
  await owner.locator('#attstage .stagechip.done[title="recipient-synthetic.txt"]').waitFor({timeout:30000});
  await incoming.locator('[aria-label="Prepared as prompt"]').waitFor({timeout:30000});
  assertID('people-owner-prepare-no-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner));
  const request=owner.waitForRequest(req=>req.method()==='POST'&&new URL(req.url()).pathname==='/api/nodes/'+chat.id+'/send');
  await owner.locator('#sendbtn').click();const sent=(await request).postDataJSON();
  await until(()=>counts(ownerSeat).accepted===before.owner.accepted+1,30000,'explicit prepared file delivery');
  const delivered=jsonLines(path.join(ownerSeat.data,'counter')).at(-1)?.text||'';
  assertID('people-owner-explicit-send-'+mode,sent.attachments?.length===1&&delivered.includes(sent.attachments[0].path)&&fs.readFileSync(sent.attachments[0].path,'utf8')==='recipient synthetic file'&&counts(ownerSeat).attempted===before.owner.attempted+1&&counts(ownerSeat).controls===before.owner.controls&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
  const agentFile=owner.locator('#msgs .turn.user.media .attfile').filter({hasText:'recipient-synthetic.txt'}).last();await agentFile.waitFor({timeout:40000});
  for(const scheme of ['light','dark']) {
    await owner.emulateMedia({colorScheme:scheme});
    let sample,colors;try {colors=await until(async()=>{sample=await agentFile.evaluate(el=>{const style=getComputedStyle(el),box=el.getBoundingClientRect();return {connected:el.isConnected,visible:box.width>0&&box.height>0&&style.visibility==='visible',text:style.color,background:style.backgroundColor}});return contrastSample(sample)},10000,'connected file contrast');}catch(err){throw Error(`${err.message}; sample=${JSON.stringify(sample)}`)}
    manifest['people-agent-file-contrast-'+mode+'-'+scheme]=colors;
    assertID('people-agent-file-contrast-'+mode+'-'+scheme,colors.contrast>=4.5);
    await screenshot(owner,'people-agent-file-contrast-'+mode+'-'+scheme+'.png');
  }
  await owner.emulateMedia({colorScheme:'light'});
  const link=await until(async()=>{const ui=await ownerSeat.api('/api/ui');return (ui.forward_links||[]).find(item=>item.kind==='prompt'&&item.source?.server===subscription.server&&item.source?.owner===subscription.owner&&item.source?.chat===subscription.chat&&item.source?.generation===subscription.generation&&item.source?.log==='comments'&&item.source?.seq===sourceSeq&&item.destination?.node===chat.id);},40000,'confirmed file source link');
  assertID('people-owner-confirmed-source-'+mode,!!link);
  await reader.locator('#prompt').evaluate(el=>{const bytes=Uint8Array.from(atob('iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAEUlEQVR4nGP4z8DwH4QZYAwAR8oH+WdZbrcAAAAASUVORK5CYII='),c=>c.charCodeAt(0));const data=new DataTransfer();data.items.add(new File([bytes],'synthetic-screen.png',{type:'image/png'}));el.dispatchEvent(new ClipboardEvent('paste',{bubbles:true,cancelable:true,clipboardData:data}));});
  await reader.locator('#attstage .stagechip.done[title="synthetic-screen.png"]').waitFor({timeout:30000});
  await reader.locator('#sendbtn').click();
  const imageBubble=owner.locator('#msgs .gmsg').filter({has:owner.locator('img[alt="synthetic-screen.png"]')}).last();
  await imageBubble.waitFor({timeout:40000});
  await until(async()=>imageBubble.locator('img').evaluate(img=>img.complete&&img.naturalWidth===2&&img.naturalHeight===2),30000,'actual received raster preview');
  assertID('people-pasted-image-preview-'+mode,await imageBubble.locator('img').count()===1);
  const receivedImage=reader.locator('#msgs .gmsg').filter({has:reader.locator('img[alt="synthetic-screen.png"]')}).last();await receivedImage.waitFor({timeout:40000});
  for(const [seatPage,bubble,side] of [[reader,receivedImage,'received'],[owner,imageBubble,'owner']]) {
    const tile=bubble.locator('[data-asset-preview]'),pagesBefore=seatPage.context().pages().length;await tile.waitFor({timeout:30000});
    if(side==='received'&&mode==='phone'){await tile.focus();await seatPage.keyboard.press('Enter');}else await tile.click();
    const dialog=seatPage.locator('#assetpreview');await dialog.waitFor({state:'visible',timeout:10000});
    await until(async()=>dialog.locator('[data-preview-image]').evaluate(img=>img.complete&&img.naturalWidth===2),10000,'common image preview loaded');
    assertID('people-common-preview-no-popup-'+side+'-'+mode,seatPage.context().pages().length===pagesBefore);
    const file=seatPage.waitForEvent('download');await dialog.locator('[data-preview-download]').click();const saved=await file;
    assertID('people-common-preview-download-'+side+'-'+mode,(await fs.promises.readFile(await saved.path())).equals(Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAEUlEQVR4nGP4z8DwH4QZYAwAR8oH+WdZbrcAAAAASUVORK5CYII=','base64')));
    await screenshot(seatPage,'people-common-preview-'+side+'-'+mode+'.png');
    await dialog.locator('[data-preview-close]').click();await dialog.waitFor({state:'hidden',timeout:10000});
    assertID('people-common-preview-closed-'+side+'-'+mode,await tile.evaluate(el=>document.activeElement===el));
  }
  await owner.locator('#attfile').setInputFiles({name:'owner-image.png',mimeType:'image/png',buffer:Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAEUlEQVR4nGP4z8DwH4QZYAwAR8oH+WdZbrcAAAAASUVORK5CYII=','base64')});
  await owner.locator('#attstage .stagechip.done[title="owner-image.png"]').waitFor({timeout:30000});await owner.locator('#sendbtn').click();
  const ownerImage=reader.locator('#msgs .gmsg').filter({has:reader.locator('img[alt="owner-image.png"]')}).last();await ownerImage.waitFor({timeout:40000});
  await ownerImage.locator('[data-asset-preview]').click();await reader.locator('#assetpreview').waitFor({state:'visible',timeout:10000});
  await until(async()=>reader.locator('#assetpreview [data-preview-image]').evaluate(img=>img.complete&&img.naturalWidth===2),10000,'owner image received preview');
  assertID('people-owner-image-received-preview-'+mode,await reader.locator('#assetpreview [data-preview-title]').innerText()==='owner-image.png');
  await reader.locator('#assetpreview [data-preview-close]').click();
  assertID('people-image-preparation-not-implicit-'+mode,counts(ownerSeat).attempted===before.owner.attempted+1&&counts(ownerSeat).accepted===before.owner.accepted+1&&counts(ownerSeat).controls===before.owner.controls);
  await imageBubble.locator('.bubble').click({position:{x:5,y:5}});await owner.locator('[data-gact=prompt]').click();
  await owner.locator('#destinationagent[aria-pressed=true]').waitFor({timeout:30000});await owner.locator('#attstage .stagechip.done[title="synthetic-screen.png"]').waitFor({timeout:30000});
  assertID('people-image-prepare-no-dispatch-'+mode,counts(ownerSeat).attempted===before.owner.attempted+1);
  const imageRequest=owner.waitForRequest(req=>req.method()==='POST'&&new URL(req.url()).pathname==='/api/nodes/'+chat.id+'/send');await owner.locator('#sendbtn').click();const imageSent=(await imageRequest).postDataJSON();
  await until(()=>counts(ownerSeat).accepted===before.owner.accepted+2,30000,'explicit prepared image delivery');
  const imageDelivered=jsonLines(path.join(ownerSeat.data,'counter')).at(-1)?.text||'';
  assertID('people-image-explicit-transport-delivery-'+mode,imageSent.attachments?.length===1&&imageDelivered.includes(imageSent.attachments[0].path)&&fs.readFileSync(imageSent.attachments[0].path).equals(Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAEUlEQVR4nGP4z8DwH4QZYAwAR8oH+WdZbrcAAAAASUVORK5CYII=','base64'))&&counts(ownerSeat).attempted===before.owner.attempted+2);
  const imageURL=await imageBubble.locator('img').getAttribute('src');
  const imageSeq=Number(await imageBubble.getAttribute('data-cseq'));
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'read'}]});
  await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.some(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='read'),30000,'People reader permission');
  const denied=await reader.evaluate(async key=>{const token=document.querySelector('meta[name="scimux-csrf"]').content,form=new FormData();form.append('files',new File(['denied'],'denied.txt',{type:'text/plain'}));return (await fetch('/api/team/subscriptions/'+key+'/attachments',{method:'POST',headers:{'X-Scimux-CSRF':token},body:form})).status},key);
  assertID('people-read-only-upload-denied-'+mode,denied===403);
  await reader.locator('#gueststate').getByText(/Can view/).waitFor({timeout:30000});
  await receivedImage.locator('[data-asset-preview]').click();await reader.locator('#assetpreview').waitFor({state:'visible',timeout:10000});
  await until(async()=>reader.locator('#assetpreview [data-preview-image]').evaluate(img=>img.complete&&img.naturalWidth===2),10000,'read-only preview loaded');
  assertID('people-read-only-common-preview-'+mode,await reader.locator('#assetpreview [data-preview-image]').evaluate(img=>img.complete&&img.naturalWidth===2));await reader.locator('#assetpreview [data-preview-close]').click();
  const stillReadable=reader.waitForEvent('download');await returned.getByText(ownerName,{exact:true}).click();const readDownload=await stillReadable;
  assertID('people-read-only-retained-download-'+mode,(await fs.promises.readFile(await readDownload.path(),'utf8'))==='owner synthetic file');
  await ownerSeat.api('/api/team/shares/'+chat.id,'DELETE');
  await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.some(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.state==='ended'),30000,'People image share ended');
  await reader.locator('#gueststate').getByText(/Sharing ended/).waitFor({timeout:30000});
  await receivedImage.locator('[data-asset-preview]').click();await reader.locator('#assetpreview').waitFor({state:'visible',timeout:10000});
  await until(async()=>reader.locator('#assetpreview [data-preview-image]').evaluate(img=>img.complete&&img.naturalWidth===2),10000,'ended preview loaded');
  assertID('people-ended-common-preview-'+mode,await reader.locator('#assetpreview [data-preview-image]').evaluate(img=>img.complete&&img.naturalWidth===2));await reader.locator('#assetpreview [data-preview-close]').click();
  const retained=await ownerSeat.api('/api/team/subscriptions/'+key+'/comments?after='+(imageSeq-1));
  assertID('people-image-retained-after-end-'+mode,retained.entries?.some(item=>item.seq===imageSeq));
  const savedImage=await owner.evaluate(async url=>{const response=await fetch(url);return {status:response.status,bytes:Array.from(new Uint8Array(await response.arrayBuffer()))};},imageURL);
  assertID('people-image-bytes-retained-after-end-'+mode,savedImage.status===200&&Buffer.from(savedImage.bytes).equals(Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAEUlEQVR4nGP4z8DwH4QZYAwAR8oH+WdZbrcAAAAASUVORK5CYII=','base64')));
  assertID('people-image-no-implicit-dispatch-'+mode,counts(ownerSeat).attempted===before.owner.attempted+2&&counts(ownerSeat).accepted===before.owner.accepted+2&&counts(ownerSeat).controls===before.owner.controls&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
  await until(async()=>(await ownerSeat.api('/api/team/subscriptions')).subscriptions.some(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.state==='ended'),30000,'owner retained copy after share end');
  await terminate(relay.child);assert.ok(relay.child.exitCode!==null||relay.child.signalCode!==null,'owned relay stopped');
  try {
  const offlineImage=await owner.evaluate(async url=>{const response=await fetch(url);return {status:response.status,bytes:Array.from(new Uint8Array(await response.arrayBuffer()))};},imageURL);
  assertID('people-image-retained-offline-'+mode,offlineImage.status===200&&Buffer.from(offlineImage.bytes).equals(Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAEUlEQVR4nGP4z8DwH4QZYAwAR8oH+WdZbrcAAAAASUVORK5CYII=','base64')));
  await receivedImage.locator('[data-asset-preview]').click();await reader.locator('#assetpreview').waitFor({state:'visible',timeout:10000});
  await until(async()=>reader.locator('#assetpreview [data-preview-image]').evaluate(img=>img.complete&&img.naturalWidth===2),10000,'offline retained preview loaded');
  assertID('people-offline-common-preview-'+mode,await reader.locator('#assetpreview [data-preview-image]').evaluate(img=>img.complete&&img.naturalWidth===2));await reader.locator('#assetpreview [data-preview-close]').click();
  await incoming.locator('.bubble').click({position:{x:5,y:5}});await owner.locator('[data-gact=prompt]').click();
  await owner.locator('#attstage .stagechip.done[title="recipient-synthetic.txt"]').waitFor({timeout:30000});
  assertID('people-retained-prepare-no-dispatch-'+mode,counts(ownerSeat).attempted===before.owner.attempted+2&&counts(ownerSeat).accepted===before.owner.accepted+2);
  const retainedRequest=owner.waitForRequest(req=>req.method()==='POST'&&new URL(req.url()).pathname==='/api/nodes/'+chat.id+'/send');await owner.locator('#sendbtn').click();const retainedSent=(await retainedRequest).postDataJSON();
  await until(()=>counts(ownerSeat).accepted===before.owner.accepted+3,30000,'explicit retained file delivery');
  const retainedDelivered=jsonLines(path.join(ownerSeat.data,'counter')).at(-1)?.text||'';
  assertID('people-retained-file-explicit-delivery-'+mode,retainedSent.attachments?.length===1&&retainedDelivered.includes(retainedSent.attachments[0].path)&&fs.readFileSync(retainedSent.attachments[0].path,'utf8')==='recipient synthetic file'&&counts(ownerSeat).attempted===before.owner.attempted+3&&counts(ownerSeat).controls===before.owner.controls&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
  } finally {await restartRelay();}
  await screenshot(reader,'people-files-recipient-'+mode+'.png');await screenshot(owner,'people-files-owner-'+mode+'.png');
  manifest['people-files-'+mode]={key,before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};
}

async function peopleStalePreparation(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Stale preparation '+mode,'Synthetic stale preparation setup');const sharePath='/api/team/shares/'+chat.id;
  await ownerSeat.api(sharePath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});const share=await ownerSeat.api(sharePath);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),40000,'stale preparation subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),owner=await pageFor(ownerSeat,mode,'stale-preparation-owner-'+mode),reader=await pageFor(readerSeat,mode,'stale-preparation-reader-'+mode);
  await open(owner,chat.id);await open(reader,'team/'+key);await reader.locator('#attadd').waitFor({state:'visible',timeout:30000});
  const before={owner:counts(ownerSeat),recipient:counts(readerSeat)},upload=reader.waitForRequest(req=>req.method()==='POST'&&new URL(req.url()).pathname==='/api/team/subscriptions/'+key+'/attachments');
  await reader.locator('#attfile').setInputFiles({name:'stale-source.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic stale source')});const id=new URL((await upload).url()).searchParams.get('stage_id');
  await reader.locator('#attstage .stagechip.done[title="stale-source.txt"]').waitFor({timeout:30000});await reader.locator('#sendbtn').click();
  const incoming=owner.locator('#msgs .gmsg').filter({hasText:'stale-source.txt'}).last();await incoming.waitFor({timeout:40000});const seq=Number(await incoming.getAttribute('data-cseq'));
  let release,ready,done,released=false;const held=new Promise(resolve=>release=resolve),requested=new Promise(resolve=>ready=resolve),settled=new Promise(resolve=>done=resolve);heldReleases.add(release);
  await owner.route(url=>url.pathname==='/api/team/subscriptions/'+key+'/attachments/'+seq+'/'+id,async route=>{try{const response=await route.fetch();ready();await held;if(!owner.isClosed())await route.fulfill({response})}catch(err){if(!released&&!shutting&&!owner.isClosed())throw err}finally{done()}});
  try {
    await incoming.locator('.bubble').click({position:{x:5,y:5}});await owner.locator('[data-gact=prompt]').click();await Promise.race([requested,delay(30000).then(()=>{throw Error('source file preparation not held')})]);
    await ownerSeat.api(sharePath,'DELETE');await until(async()=>(await ownerSeat.api('/api/team/subscriptions')).subscriptions.some(item=>item.chat===share.uid&&item.generation===share.generation&&item.state==='ended'),30000,'stale source ended');
    if(mode==='phone')await owner.locator('#chatback').click();await open(owner,ownerSeat.node.id);if(mode==='phone')await owner.locator('#chatback').click();await open(owner,chat.id);
    await owner.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});await owner.locator('#prompt').fill('Synthetic newer draft '+mode);
    released=true;release();heldReleases.delete(release);await settled;await owner.unrouteAll({behavior:'wait'});
    await owner.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));
    assertID('people-stale-source-keeps-new-draft-'+mode,(await owner.locator('#prompt').innerText())==='Synthetic newer draft '+mode&&await owner.locator('#attstage .stagechip[title="stale-source.txt"]').count()===0);
    assertID('people-stale-source-no-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
    await screenshot(owner,'people-stale-preparation-'+mode+'.png');manifest['people-stale-preparation-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};
  } finally {released=true;release();heldReleases.delete(release);await owner.unrouteAll({behavior:'wait'});}
}

async function peopleRetainedUnavailable(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Unavailable retained '+mode,'Synthetic unavailable setup');const sharePath='/api/team/shares/'+chat.id;
  await ownerSeat.api(sharePath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});const share=await ownerSeat.api(sharePath);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),40000,'unavailable subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),owner=await pageFor(ownerSeat,mode,'unavailable-owner-'+mode),reader=await pageFor(readerSeat,mode,'unavailable-reader-'+mode);
  await open(owner,chat.id);await open(reader,'team/'+key);await reader.locator('#attadd').waitFor({state:'visible',timeout:30000});
  const before={owner:counts(ownerSeat),recipient:counts(readerSeat)};
  const request=reader.waitForRequest(req=>req.method()==='POST'&&new URL(req.url()).pathname==='/api/team/subscriptions/'+key+'/attachments');
  await reader.locator('#attfile').setInputFiles({name:'never-retained.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic unavailable bytes')});const staged=await request,id=new URL(staged.url()).searchParams.get('stage_id');
  await reader.locator('#attstage .stagechip.done[title="never-retained.txt"]').waitFor({timeout:30000});let blocked=0;
  await owner.route(url=>url.pathname.startsWith('/api/team/subscriptions/'+key+'/attachments/')&&url.pathname.endsWith('/'+id),async route=>{blocked++;await route.fulfill({status:503,contentType:'application/json',body:'{"error":"synthetic_unavailable"}'})});
  try {
    await reader.locator('#sendbtn').click();const incoming=owner.locator('#msgs .gmsg').filter({hasText:'never-retained.txt'}).last();await incoming.getByText(/not retained/).waitFor({timeout:40000});assertID('people-unavailable-not-retained-'+mode,blocked>0);
    await ownerSeat.api(sharePath,'DELETE');await until(async()=>(await ownerSeat.api('/api/team/subscriptions')).subscriptions.some(item=>item.chat===share.uid&&item.generation===share.generation&&item.state==='ended'),30000,'unavailable share ended');await owner.unrouteAll({behavior:'wait'});
    await incoming.locator('.bubble').click({position:{x:5,y:5}});const warning=owner.waitForEvent('dialog'),trigger=owner.locator('[data-gact=prompt]').click();const dialog=await warning;
    assertID('people-retained-missing-feedback-'+mode,/source file is unavailable/i.test(dialog.message()));await dialog.accept();await trigger;
    assertID('people-retained-missing-no-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
    const recipient=reader.locator('#msgs .gmsg').filter({hasText:'never-retained.txt'}).last();await recipient.locator('.bubble').click({position:{x:5,y:5}});
    assertID('people-recipient-no-agent-action-'+mode,await reader.locator('[data-gact=prompt]').count()===0);await screenshot(owner,'people-unavailable-'+mode+'.png');
    manifest['people-unavailable-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};
  } finally {await owner.unrouteAll({behavior:'wait'});}
}

async function peopleRetiredDrafts(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Retired People draft '+mode,'Synthetic retired draft setup');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+chat.id);
  const state=await until(async()=>{const list=await ownerSeat.api('/api/team/subscriptions'),sub=list.subscriptions.find(item=>item.chat===share.uid&&item.generation===share.generation&&item.local_node===chat.id);const server=list.servers.find(item=>item.id===ownerSeat.serverID);return sub?.people_copy_instance&&server?.people_file_binding?{sub,server}:null},40000,'retired draft copy identity');
  const native=(await ownerSeat.api('/api/state')).nodes.find(item=>item.id===chat.id);assert.ok(native?.instance_id,'synthetic native identity');
  const key=[state.sub.server,state.sub.owner,state.sub.chat,state.sub.generation].join('/'),scope='people\0'+key+'\0binding:'+state.server.people_file_binding+'\0'+chat.id+'\0'+native.instance_id;
  const pendingKey='scimux-people-file-pending:'+key+':'+native.instance_id+':'+state.server.people_file_binding;
  const page=await pageFor(ownerSeat,mode,'people-retired-draft-'+mode);await open(page,chat.id);const before={owner:counts(ownerSeat),recipient:counts(readerSeat)};
  await page.evaluate(async({scope,pendingKey,instance})=>{
    const {createBrowserPeopleFileStore}=await import('/js/people-file-store.js');const store=createBrowserPeopleFileStore(indexedDB);
    try { await store.save(scope,'0123456789abcdef',new File(['synthetic retired bytes'],'retired-synthetic.txt',{type:'text/plain'})); } finally { store.destroy(); }
    localStorage.setItem(pendingKey,JSON.stringify({instance,request:{text:'Synthetic unconfirmed older message',anchor:0,key:'fedcba9876543210',attachments:['0123456789abcdef']}}));
  },{scope,pendingKey,instance:native.instance_id});
  await page.reload({waitUntil:'networkidle'});await page.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
  const review=page.locator('#draftrecovery a[download="retired-synthetic.txt"]');await review.waitFor({timeout:40000});
  assertID('people-retired-no-drafts-panel-'+mode,await page.locator('#draftrecovery details').count()===0);
  const downloadEvent=page.waitForEvent('download');await review.click();const download=await downloadEvent;
  assertID('people-retired-file-review-'+mode,(await fs.promises.readFile(await download.path(),'utf8'))==='synthetic retired bytes');
  assertID('people-retired-review-no-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
  manifest['people-retired-draft-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};await screenshot(page,'people-retired-draft-'+mode+'.png');
}

async function peopleAttachmentRecovery(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','People recovery '+mode,'Synthetic recovery setup');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});const share=await ownerSeat.api('/api/team/shares/'+chat.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),40000,'recovery subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),owner=await pageFor(ownerSeat,mode,'people-recovery-owner-'+mode),reader=await pageFor(readerSeat,mode,'people-recovery-reader-'+mode);
  await open(owner,chat.id);await owner.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});await owner.locator('#attadd').waitFor({state:'visible',timeout:30000});
  const before={owner:counts(ownerSeat),recipient:counts(readerSeat)};
  await owner.locator('#prompt').fill('Synthetic retained retry '+mode);await owner.locator('#attfile').setInputFiles({name:'retry-synthetic.txt',mimeType:'text/plain',buffer:Buffer.from('retry synthetic bytes')});
  await owner.locator('#attstage .stagechip.done[title="retry-synthetic.txt"]').waitFor({timeout:30000});await owner.locator('#sendbtn').click();await owner.locator('#msgs .gmsg').filter({hasText:'Synthetic retained retry '+mode}).waitFor({timeout:40000});
  const invalid=Buffer.from('89504e470d0a1a0a','hex');await owner.locator('#attfile').setInputFiles({name:'invalid-synthetic.png',mimeType:'image/png',buffer:invalid});
  await owner.locator('#attstage .stagechip.done[title="invalid-synthetic.png"]').waitFor({timeout:30000});await owner.locator('#sendbtn').click();
  const comments=await until(async()=>{const list=await ownerSeat.api('/api/team/subscriptions/'+key+'/comments?after=0');return list.entries.length>=2?list.entries:null},40000,'recovery publications');
  const generic=comments.find(item=>item.text==='Synthetic retained retry '+mode),bad=comments.find(item=>item.attachments?.some(file=>file.name==='invalid-synthetic.png'));
  assert.ok(generic?.attachments?.length===1&&bad?.attachments?.length===1,'synthetic associations exist');
  const expected='/api/team/subscriptions/'+key+'/attachments/'+generic.seq+'/'+generic.attachments[0].id;let blocked=0;
  await reader.route(url=>url.pathname===expected,async route=>{if(route.request().method()==='GET'&&blocked++===0)await route.fulfill({status:503,contentType:'application/json',body:'{"error":"synthetic_unavailable"}'});else await route.continue()});
  try {
    await open(reader,'team/'+key);const bubble=reader.locator('#msgs .gmsg').filter({hasText:'Synthetic retained retry '+mode}).last();
    const retry=bubble.getByText('Retry download',{exact:true});await retry.waitFor({timeout:40000});assertID('people-missing-retry-visible-'+mode,blocked===1);
    await reader.unrouteAll({behavior:'wait'});const downloadEvent=reader.waitForEvent('download');await retry.click();const download=await downloadEvent;
    assertID('people-missing-retry-bytes-'+mode,(await fs.promises.readFile(await download.path(),'utf8'))==='retry synthetic bytes');
    const badBubble=reader.locator('#msgs .gmsg').filter({hasText:'invalid-synthetic.png'}).last(),fallback=badBubble.locator('a[download]').filter({hasText:'invalid-synthetic.png'});
    await fallback.waitFor({timeout:40000});assertID('people-preview-refused-visible-'+mode,(await fallback.innerText()).includes('preview unavailable'));
    const badDownloadEvent=reader.waitForEvent('download');await fallback.click();const badDownload=await badDownloadEvent;
    assertID('people-preview-refused-download-'+mode,(await fs.promises.readFile(await badDownload.path())).equals(invalid));
    assertID('people-recovery-zero-agent-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
    await screenshot(reader,'people-recovery-'+mode+'.png');manifest['people-recovery-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};
  } finally {await reader.unrouteAll({behavior:'wait'});}
}

async function peopleRelayBlobRecovery(mode,kind,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','People relay '+kind+' '+mode,'Synthetic relay integrity setup');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+chat.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),40000,'relay integrity subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),owner=await pageFor(ownerSeat,mode,'relay-integrity-owner-'+kind+'-'+mode),reader=await pageFor(readerSeat,mode,'relay-integrity-reader-'+kind+'-'+mode);
  await open(owner,chat.id);await owner.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});await owner.locator('#attadd').waitFor({state:'visible',timeout:30000});
  const before={owner:counts(ownerSeat),recipient:counts(readerSeat)},caption='Synthetic relay '+kind+' '+mode,bytes=Buffer.from('synthetic relay recovery '+kind+' '+mode);
  await owner.locator('#prompt').fill(caption);await owner.locator('#attfile').setInputFiles({name:'relay-recovery.txt',mimeType:'text/plain',buffer:bytes});
  await owner.locator('#attstage .stagechip.done[title="relay-recovery.txt"]').waitFor({timeout:30000});await owner.locator('#sendbtn').click();await owner.locator('#msgs .gmsg').filter({hasText:caption}).waitFor({timeout:40000});
  const entries=await until(async()=>{const result=await ownerSeat.api('/api/team/subscriptions/'+key+'/comments?after=0');return result.entries.find(item=>item.text===caption&&item.attachments?.length===1)},40000,'relay integrity publication');
  const file=entries.attachments[0],digest=crypto.createHash('sha256').update(bytes).digest('hex');
  assert.ok(/^[0-9a-f]{32}$/.test(ownerSeat.self)&&/^[0-9a-f]{16}$/.test(share.uid)&&file.sha256===digest,'synthetic relay path identity');
  const blob=path.join(relay.dir,'channels',ownerSeat.self,share.uid,'blobs',digest);
  assert.ok(fs.readFileSync(blob).equals(bytes),'published synthetic relay bytes');
  if(kind==='missing')fs.unlinkSync(blob);else {const corrupted=Buffer.from(bytes);corrupted[0]^=1;assert.equal(corrupted.length,bytes.length);assert.notEqual(crypto.createHash('sha256').update(corrupted).digest('hex'),digest);fs.writeFileSync(blob,corrupted,{mode:0o600});}
  try {
    let failures=0;reader.on('response',response=>{const u=new URL(response.url());if(u.pathname==='/api/team/subscriptions/'+key+'/attachments/'+entries.seq+'/'+file.id&&response.status()>=400)failures++;});
    await open(reader,'team/'+key);const bubble=reader.locator('#msgs .gmsg').filter({hasText:caption}).last(),retry=bubble.getByText('Retry download',{exact:true});
    await retry.waitFor({timeout:40000});assertID('people-relay-'+kind+'-unavailable-'+mode,failures>0&&(await bubble.innerText()).includes('not retained'));
    fs.writeFileSync(blob,bytes,{mode:0o600});
    const downloadEvent=reader.waitForEvent('download');await retry.click();const download=await downloadEvent;
    assertID('people-relay-'+kind+'-recovered-bytes-'+mode,(await fs.promises.readFile(await download.path())).equals(bytes));
    fs.unlinkSync(blob);await reader.reload({waitUntil:'networkidle'});
    const retained=reader.locator('#msgs .gmsg').filter({hasText:caption}).last().locator('a[download]').filter({hasText:'relay-recovery.txt'});
    await retained.waitFor({timeout:40000});const retainedEvent=reader.waitForEvent('download');await retained.click();const retainedDownload=await retainedEvent;
    assertID('people-relay-'+kind+'-retained-after-reload-'+mode,(await fs.promises.readFile(await retainedDownload.path())).equals(bytes));
    assertID('people-relay-'+kind+'-zero-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
    await screenshot(reader,'people-relay-'+kind+'-'+mode+'.png');manifest['people-relay-'+kind+'-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)},failures};
  } finally {if(!fs.existsSync(blob)||!fs.readFileSync(blob).equals(bytes))fs.writeFileSync(blob,bytes,{mode:0o600});}
}

async function peopleCrossChannelForgery(mode,firstOwner,secondOwner,readerSeat) {
  const first=await node(firstOwner,'codex','People first channel '+mode,'Synthetic first setup'),second=await node(secondOwner,'codex','People second channel '+mode,'Synthetic second setup');
  for(const [seat,chat] of [[firstOwner,first],[secondOwner,second]])await seat.api('/api/team/shares/'+chat.id,'PUT',{server:seat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const shares=await Promise.all([[firstOwner,first],[secondOwner,second]].map(([seat,chat])=>seat.api('/api/team/shares/'+chat.id)));
  const subs=await until(async()=>{const list=(await readerSeat.api('/api/team/subscriptions')).subscriptions;const found=shares.map((share,i)=>list.find(item=>item.owner===[firstOwner,secondOwner][i].self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'));return found.every(Boolean)?found:null},40000,'cross-channel subscriptions');
  const keys=subs.map(sub=>[sub.server,sub.owner,sub.chat,sub.generation].join('/'));
  const owner=await pageFor(firstOwner,mode,'cross-channel-owner-'+mode),reader=await pageFor(readerSeat,mode,'cross-channel-reader-'+mode);
  await open(owner,first.id);await owner.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});await owner.locator('#destinationagent').click();await owner.locator('#destinationagent[aria-pressed=true]').waitFor({timeout:30000});
  await open(reader,'team/'+keys[0]);await reader.locator('#attadd').waitFor({state:'visible',timeout:30000});
  for(const [seat,chat] of [[firstOwner,first],[secondOwner,second]])assert.equal((await seat.api('/api/nodes/'+chat.id+'/auto-approve','POST',{enabled:true})).enabled,true);
  const before={first:counts(firstOwner),second:counts(secondOwner),recipient:counts(readerSeat)},path='/api/team/subscriptions/'+keys[0]+'/attachments';
  const request=reader.waitForRequest(req=>req.method()==='POST'&&new URL(req.url()).pathname===path);
  await reader.locator('#attfile').setInputFiles({name:'foreign-synthetic.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic foreign channel bytes')});
  const stageID=new URL((await request).url()).searchParams.get('stage_id');await reader.locator('#attstage .stagechip.done[title="foreign-synthetic.txt"]').waitFor({timeout:30000});
  const prior=await readerSeat.api('/api/team/subscriptions/'+keys[1]+'/comments?after=0');
  const result=await reader.evaluate(async({key,instance,stage})=>{
    const csrf=document.querySelector('meta[name="scimux-csrf"]').content,base='/api/team/subscriptions/'+key,query='?copy_instance='+encodeURIComponent(instance);
    const get=await fetch(base+'/attachments/1/'+stage+query,{headers:{'X-Scimux-CSRF':csrf}});
    const post=await fetch(base+'/comments'+query,{method:'POST',headers:{'Content-Type':'application/json','X-Scimux-CSRF':csrf},body:JSON.stringify({text:'Synthetic forged reference',anchor:0,key:'0123456789abcdef',attachments:[stage]})});
    const manifest=await fetch(base+'/comments'+query,{method:'POST',headers:{'Content-Type':'application/json','X-Scimux-CSRF':csrf},body:JSON.stringify({text:'Synthetic forged manifest',anchor:0,key:'fedcba9876543210',people_attachments:[{id:stage,sha256:'a'.repeat(64),name:'synthetic.txt',mime:'text/plain',size:1}]})});
    return {get:get.status,getLeaked:(await get.text()).includes('synthetic foreign channel bytes'),post:post.status,error:(await post.json()).error,manifest:manifest.status,manifestError:(await manifest.json()).error};
  },{key:keys[1],instance:subs[1].people_copy_instance,stage:stageID});
  const after=await readerSeat.api('/api/team/subscriptions/'+keys[1]+'/comments?after=0');
  assertID('people-cross-channel-denied-'+mode,result.get===404&&!result.getLeaked&&result.post===409&&result.error==='stage_unavailable'&&result.manifest===400&&result.manifestError==='bad_request'&&after.entries.length===prior.entries.length);
  assertID('people-cross-channel-zero-controls-'+mode,JSON.stringify(counts(firstOwner))===JSON.stringify(before.first)&&JSON.stringify(counts(secondOwner))===JSON.stringify(before.second)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient)&&await owner.locator('#destinationagent[aria-pressed=true]').count()===1);
  // Independent review adds real associated A/B publications before forged durable GET.
  await reader.locator('#sendbtn').click();
  const publishedA=await until(async()=>{const value=await readerSeat.api('/api/team/subscriptions/'+keys[0]+'/comments?after=0');return value.entries.find(item=>item.attachments?.some(file=>file.id===stageID))},30000,'published source A');
  if(mode==='phone')await reader.locator('#chatback').click();
  await open(reader,'team/'+keys[1]);await reader.locator('#attadd').waitFor({state:'visible',timeout:30000});
  await reader.locator('#attfile').setInputFiles({name:'second-synthetic.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic second channel bytes')});
  await reader.locator('#attstage .stagechip.done[title="second-synthetic.txt"]').waitFor({timeout:30000});await reader.locator('#sendbtn').click();
  const publishedB=await until(async()=>{const value=await readerSeat.api('/api/team/subscriptions/'+keys[1]+'/comments?after=0');return value.entries.find(item=>item.attachments?.some(file=>file.name==='second-synthetic.txt'))},30000,'published source B');
  const associations=await reader.evaluate(async({keys,instances,a,b})=>{
    const get=async(index,seq,id)=>{const response=await fetch('/api/team/subscriptions/'+keys[index]+'/attachments/'+seq+'/'+id+'?copy_instance='+encodeURIComponent(instances[index]));return {status:response.status,type:response.headers.get('content-type'),text:await response.text()}};
    return {a:await get(0,a.seq,a.id),b:await get(1,b.seq,b.id),forged:await get(1,b.seq,a.id)};
  },{keys,instances:subs.map(sub=>sub.people_copy_instance),a:{seq:publishedA.seq,id:stageID},b:{seq:publishedB.seq,id:publishedB.attachments[0].id}});
  assertID('people-cross-channel-published-address-'+mode,associations.a.status===200&&associations.a.text==='synthetic foreign channel bytes'&&associations.b.status===200&&associations.b.text==='synthetic second channel bytes'&&associations.forged.status===404&&associations.forged.type?.startsWith('application/json')&&!associations.forged.text.includes('synthetic foreign channel bytes'));
  const leases=await Promise.all([[firstOwner,first],[secondOwner,second]].map(([seat,chat])=>seat.api('/api/nodes/'+chat.id+'/chat')));
  assertID('people-cross-channel-armed-owner-'+mode,leases.every(chat=>chat.auto_approve?.enabled===true)&&JSON.stringify(counts(firstOwner))===JSON.stringify(before.first)&&JSON.stringify(counts(secondOwner))===JSON.stringify(before.second)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
  await screenshot(reader,'people-cross-channel-'+mode+'.png');manifest['people-cross-channel-'+mode]={before,after:{first:counts(firstOwner),second:counts(secondOwner),recipient:counts(readerSeat)},result};
}

async function peopleRealTunnel(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','People tunnel '+mode,'Synthetic tunnel setup');const sharePath='/api/team/shares/'+chat.id;
  await ownerSeat.api(sharePath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});const share=await ownerSeat.api(sharePath);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),40000,'tunnel subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),page=await pageFor(readerSeat,mode,'people-real-tunnel-'+mode);await open(page,'team/'+key);
  await page.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});const before={owner:counts(ownerSeat),recipient:counts(readerSeat)};
  const goEnv={...baseEnv,GOTOOLCHAIN:'local',GOPROXY:'off'};
  const go=prepareTunnelGo({selected:process.env.SCIMUX_E2E_GO,inheritedPath:process.env.PATH,env:goEnv});
  const goDigest=crypto.createHash('sha256').update(fs.readFileSync(go.executable)).digest('hex');
  manifest['people-tunnel-toolchain-'+mode]={version:go.version,sha256:goDigest};
  async function throughTunnel(readonly) {
    const env={...goEnv,GOCACHE:process.env.GOCACHE||execFileSync(go.executable,['env','GOCACHE'],{env:goEnv,encoding:'utf8'}).trim(),GOMODCACHE:process.env.GOMODCACHE||execFileSync(go.executable,['env','GOMODCACHE'],{env:goEnv,encoding:'utf8'}).trim(),SCIMUX_TUNNEL_APP_TARGET:readerSeat.base,SCIMUX_TUNNEL_APP_KEY:key,SCIMUX_TUNNEL_APP_COPY_INSTANCE:sub.people_copy_instance,SCIMUX_TUNNEL_APP_CSRF:readerSeat.csrf,SCIMUX_TUNNEL_APP_READONLY:readonly?'1':'0'};
    const child=start(go.executable,['test','./internal/remote','-run','^TestPeopleRealAppThroughCodecPipeHTTPBridge$','-count=1','-v'],env,'people-tunnel-'+mode+'-'+readonly,'pipe');let output='';child.stdout.on('data',chunk=>{if(output.length<65536)output+=chunk});
    const status=await new Promise((resolve,reject)=>{child.once('error',reject);child.once('close',(code,signal)=>resolve({code,signal}))});
    assert.equal(status.code,0,'real app tunnel integration failed: '+output);assert.equal(status.signal,null);
    assert.match(output,/--- PASS: TestPeopleRealAppThroughCodecPipeHTTPBridge(?:\s|$)/,'codec pipe HTTP bridge test must run without skipping');
    manifest['people-tunnel-child-'+mode+'-'+readonly]={test:'TestPeopleRealAppThroughCodecPipeHTTPBridge',passed:true,toolchainSha256:goDigest};
  }
  await throughTunnel(false);
  const bubble=page.locator('#msgs .gmsg').filter({hasText:'synthetic-tunnel.txt'}).last();await bubble.waitFor({timeout:40000});
  const downloadEvent=page.waitForEvent('download');await bubble.getByText('synthetic-tunnel.txt',{exact:true}).click();const download=await downloadEvent;
  assertID('people-real-tunnel-upload-download-'+mode,(await fs.promises.readFile(await download.path(),'utf8'))==='synthetic tunnel bytes');
  await ownerSeat.api(sharePath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'read'}]});
  await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.some(item=>item.chat===share.uid&&item.generation===share.generation&&item.access==='read'),30000,'tunnel read-only permission');
  await throughTunnel(true);assertID('people-real-tunnel-read-only-'+mode,true);
  assertID('people-real-tunnel-no-agent-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
  manifest['people-real-tunnel-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};await screenshot(page,'people-real-tunnel-'+mode+'.png');
}

async function peopleFileUnread(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','File unread '+mode,'Synthetic unread setup');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+chat.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),40000,'file unread subscription');
  await until(async()=>(await ownerSeat.api('/api/team/subscriptions')).subscriptions.find(item=>item.chat===share.uid&&item.generation===share.generation&&item.local_node===chat.id),40000,'file unread owner identity');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/');
  const owner=await pageFor(ownerSeat,mode,'people-unread-owner-'+mode),reader=await pageFor(readerSeat,mode,'people-unread-reader-'+mode);
  await open(owner,ownerSeat.node.id);if(mode==='phone')await owner.locator('#chatback').click();
  await open(reader,'team/'+key);await reader.locator('#attadd').waitFor({state:'visible',timeout:30000});
  const before={owner:counts(ownerSeat),recipient:counts(readerSeat)};
  await reader.locator('#attfile').setInputFiles({name:'unread-synthetic.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic unread file')});
  await reader.locator('#attstage .stagechip.done[title="unread-synthetic.txt"]').waitFor({timeout:30000});await reader.locator('#sendbtn').click();
  await reader.locator('#msgs .gmsg').filter({hasText:'unread-synthetic.txt'}).waitFor({timeout:40000});
  const card=owner.locator('#card-'+chat.id);await card.locator('.activity-unread').waitFor({timeout:40000});
  assertID('people-file-only-unread-card-'+mode,await card.locator('.activity-unread').evaluate(el=>el.nextElementSibling?.matches('button.chev')));
  await owner.locator('[data-activity-group="mine"]').click();
  assertID('people-file-only-unread-group-'+mode,await owner.locator('[data-activity-group="mine"] .activity-unread').count()===1);
  await screenshot(owner,'people-file-unread-'+mode+'.png');await owner.locator('[data-activity-group="mine"]').click();
  await card.scrollIntoViewIfNeeded();
  if(mode==='desktop')await card.hover();else {const box=await card.boundingBox(),cdp=await owner.context().newCDPSession(owner);await cdp.send('Input.dispatchTouchEvent',{type:'touchStart',touchPoints:[{x:box.x+box.width-20,y:box.y+box.height/2}]});await cdp.send('Input.dispatchTouchEvent',{type:'touchMove',touchPoints:[{x:box.x+box.width-120,y:box.y+box.height/2}]});await cdp.send('Input.dispatchTouchEvent',{type:'touchEnd',touchPoints:[]});await cdp.detach();}
  await owner.locator('[data-pin-action="'+chat.id+'"]').click();await owner.locator('[data-tab=pinned]').click();
  assertID('people-file-only-unread-owner-pinned-'+mode,await owner.locator('#card-'+chat.id+' .activity-unread').count()===1);await owner.locator('[data-tab=current]').click();
  await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.some(item=>item.chat===share.uid&&item.generation===share.generation&&item.seen_comments>=1),40000,'recipient file read position');
  if(mode==='phone')await reader.locator('#chatback').click();await open(reader,readerSeat.node.id);if(mode==='phone')await reader.locator('#chatback').click();
  await open(owner,chat.id);await owner.locator('#msgs .gmsg').filter({hasText:'unread-synthetic.txt'}).waitFor({timeout:30000});
  await until(async()=>(await ownerSeat.api('/api/team/subscriptions')).subscriptions.some(item=>item.chat===share.uid&&item.generation===share.generation&&item.seen_comments>=1),40000,'file unread read position');
  await owner.locator('#attfile').setInputFiles({name:'own-unread-synthetic.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic own file')});
  await owner.locator('#attstage .stagechip.done[title="own-unread-synthetic.txt"]').waitFor({timeout:30000});
  let release,ready,released=false;const held=new Promise(resolve=>release=resolve),requested=new Promise(resolve=>ready=resolve);heldReleases.add(release);
  await owner.route(url=>url.pathname==='/api/team/subscriptions/'+key+'/seen',route=>route.abort());
  await owner.route(url=>url.pathname==='/api/team/subscriptions/'+key+'/comments',async route=>{if(route.request().method()!=='POST'){await route.continue();return}try{const response=await route.fetch();ready();await held;if(!owner.isClosed())await route.fulfill({response})}catch(err){if(!released&&!shutting&&!owner.isClosed())throw err}});
  try {
    await owner.locator('#sendbtn').click();await Promise.race([requested,delay(30000).then(()=>{throw Error('own file publication not held')})]);
    if(mode==='phone')await owner.locator('#chatback').click();await open(owner,ownerSeat.node.id);if(mode==='phone')await owner.locator('#chatback').click();
    released=true;release();heldReleases.delete(release);await owner.unrouteAll({behavior:'wait'});
    await until(async()=>{const state=(await ownerSeat.api('/api/team/subscriptions')).subscriptions.find(item=>item.chat===share.uid&&item.generation===share.generation);const comments=await ownerSeat.api('/api/team/subscriptions/'+key+'/comments?after=1');return state?.comments_last>=2&&comments.entries.some(item=>item.seq===2&&item.mine===true)},40000,'own file classification');
    await until(async()=>await card.locator('[aria-label="Unread status unavailable"]').count()===0&&await card.locator('.activity-unread').count()===0,40000,'own file not unread');
    assertID('people-own-file-no-unread-'+mode,true);
  } finally {released=true;release();heldReleases.delete(release);await owner.unrouteAll({behavior:'wait'});}
  const received='team/'+key,group='owner/'+sub.server+'/'+sub.owner;
  await reader.locator('[data-tab=shared] [aria-label$="unread"]').waitFor({timeout:40000});await reader.locator('[data-tab=shared]').click();
  const receivedCard=reader.locator('.card:has([data-open="'+received+'"])');await receivedCard.locator('.activity-unread').waitFor({timeout:40000});
  assertID('people-file-unread-recipient-tab-group-card-'+mode,await reader.locator('[data-activity-group="'+group+'"] .activity-unread').count()===1);
  await reader.locator('[data-activity-group="'+group+'"]').click();await reader.reload({waitUntil:'networkidle'});
  if(mode==='phone'&&await reader.locator('#cards').evaluate(el=>el.getBoundingClientRect().right<=1))await reader.locator('#chatback').click();
  await reader.locator('[data-tab=shared]').click();await reader.locator('[data-activity-group="'+group+'"] .activity-unread').waitFor({timeout:40000});
  assertID('people-file-unread-recipient-fold-reload-'+mode,(await reader.locator('[data-activity-group="'+group+'"]').getAttribute('aria-expanded'))==='false');
  await reader.context().setOffline(true);assertID('people-file-unread-recipient-offline-'+mode,await reader.locator('[data-activity-group="'+group+'"] .activity-unread').count()===1);
  const reconnected=reader.waitForResponse(response=>new URL(response.url()).pathname==='/api/team/subscriptions'&&response.status()===200,{timeout:40000});await reader.context().setOffline(false);await reconnected;
  await reader.locator('[data-activity-group="'+group+'"] .activity-unread').waitFor({timeout:40000});assertID('people-file-unread-recipient-reconnect-'+mode,true);
  await reader.locator('[data-activity-group="'+group+'"]').click();await receivedCard.scrollIntoViewIfNeeded();
  if(mode==='desktop')await receivedCard.hover();else {const box=await receivedCard.boundingBox(),cdp=await reader.context().newCDPSession(reader);await cdp.send('Input.dispatchTouchEvent',{type:'touchStart',touchPoints:[{x:box.x+box.width-20,y:box.y+box.height/2}]});await cdp.send('Input.dispatchTouchEvent',{type:'touchMove',touchPoints:[{x:box.x+box.width-120,y:box.y+box.height/2}]});await cdp.send('Input.dispatchTouchEvent',{type:'touchEnd',touchPoints:[]});await cdp.detach();}
  await reader.locator('[data-pin-action="'+received+'"]').click();await reader.locator('[data-tab=pinned]').click();
  assertID('people-file-unread-recipient-pinned-'+mode,await receivedCard.locator('.activity-unread').count()===1);
  assertID('people-file-unread-no-agent-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
  manifest['people-file-unread-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};
}

async function peopleLocalBudget(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','People local budget '+mode,'Synthetic local budget setup');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+chat.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),40000,'local budget subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),base='/api/team/subscriptions/'+key;
  const owner=await pageFor(ownerSeat,mode,'local-budget-owner-'+mode),reader=await pageFor(readerSeat,mode,'local-budget-reader-'+mode);
  await open(owner,chat.id);await open(reader,'team/'+key);
  await reader.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
  const source='Synthetic local budget source '+mode;
  await reader.locator('#prompt').fill(source);await reader.locator('#sendbtn').click();
  const bubble=owner.locator('#msgs .gmsg').filter({hasText:source}).last();await bubble.waitFor({timeout:40000});
  const before={owner:counts(ownerSeat),recipient:counts(readerSeat)};
  await bubble.locator('.bubble').click({position:{x:5,y:5}});await owner.locator('[data-gact=prompt]').click();
  await owner.locator('#destinationagent[aria-pressed=true]').waitFor({timeout:30000});
  await bubble.locator('[aria-label="Prepared as prompt"]').waitFor({timeout:30000});
  const agentDraft=await owner.locator('#prompt').innerText();
  await owner.locator('#destinationpeople').click();await owner.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});
  const caption='Synthetic local budget draft '+mode,bytes=Buffer.from('synthetic local budget bytes '+mode);
  await owner.locator('#prompt').fill(caption);
  await owner.locator('#attfile').setInputFiles({name:'prior-budget.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic prior stage')});
  await owner.locator('#attstage .stagechip.done[title="prior-budget.txt"]').waitFor({timeout:30000});
  const settings=await ownerSeat.api('/api/settings'),used=(await ownerSeat.api('/api/state')).storage.used_bytes;
  const assets=path.join(ownerSeat.data,'assets'),filler=path.join(assets,'synthetic-budget-'+mode+'-'+process.pid);
  assert.equal(path.dirname(filler),assets);fs.mkdirSync(assets,{recursive:true,mode:0o700});
  assert.ok(fs.lstatSync(assets).isDirectory()&&!fs.lstatSync(assets).isSymbolicLink()&&fs.realpathSync(assets)===path.join(fs.realpathSync(ownerSeat.data),'assets'),'filler stays in test-owned managed assets');
  const fd=fs.openSync(filler,'wx',0o600);fs.ftruncateSync(fd,1024*1024);fs.closeSync(fd);
  let removed=false;
  try {
    await ownerSeat.api('/api/settings','PUT',{storage_global_limit_bytes:used+512*1024,storage_min_free_bytes:0});
    const state=await ownerSeat.api('/api/state');assert.ok(state.storage.used_bytes>used+512*1024&&!state.storage.writable,'synthetic managed filler reaches local budget');
    const uploadPath='/api/team/subscriptions/'+key+'/attachments';
    const uploads=[];owner.on('request',req=>{if(req.method()==='POST'&&new URL(req.url()).pathname===uploadPath)uploads.push(new URL(req.url()).searchParams.get('stage_id'));});
    const requestPromise=owner.waitForRequest(req=>req.method()==='POST'&&new URL(req.url()).pathname===uploadPath);
    const responsePromise=owner.waitForResponse(resp=>resp.request().method()==='POST'&&new URL(resp.url()).pathname===uploadPath);
    await owner.locator('#attfile').setInputFiles({name:'budget-retry.txt',mimeType:'text/plain',buffer:bytes});
    const request=await requestPromise,response=await responsePromise,stageID=new URL(request.url()).searchParams.get('stage_id'),payload=await response.json();
    const blob=path.join(relay.dir,'channels',ownerSeat.self,share.uid,'blobs',crypto.createHash('sha256').update(bytes).digest('hex'));
    await owner.locator('#attstage .stagechip.err[title="budget-retry.txt"] .stageerror').waitFor({timeout:30000});
    assertID('people-local-budget-refusal-'+mode,/^[0-9a-f]{16}$/.test(stageID)&&response.status()===507&&payload.error==='storage_full'&&!fs.existsSync(blob));
    assertID('people-local-budget-preserved-'+mode,/storage is full.*free space/i.test(await owner.locator('#attstage .stagechip.err[title="budget-retry.txt"] .stageerror').innerText())&&(await owner.locator('#prompt').innerText())===caption&&await owner.locator('#attstage .stagechip.done[title="prior-budget.txt"]').count()===1&&await bubble.locator('[aria-label="Prepared as prompt"]').count()===1);
    await screenshot(owner,'people-local-budget-refused-'+mode+'.png');
    await owner.locator('#destinationagent').click();assert.equal(await owner.locator('#prompt').innerText(),agentDraft);await owner.locator('#destinationpeople').click();
    fs.unlinkSync(filler);removed=true;
    const retryRequest=owner.waitForRequest(req=>req.method()==='POST'&&new URL(req.url()).pathname===uploadPath);
    const retryResponse=owner.waitForResponse(resp=>resp.request().method()==='POST'&&new URL(resp.url()).pathname===uploadPath);
    await owner.locator('#sendbtn').click();const retried=await retryRequest,recovered=await retryResponse;
    assert.equal(new URL(retried.url()).searchParams.get('stage_id'),stageID);assert.equal(recovered.status(),200);
    const entry=await until(async()=>{const value=await readerSeat.api(base+'/comments?after=0');return value.entries.find(item=>item.text===caption)},40000,'local budget explicit publication');
    const comments=await readerSeat.api(base+'/comments?after=0'),matching=comments.entries.filter(item=>item.text===caption);
    const file=entry.attachments.find(item=>item.name==='budget-retry.txt');assert.ok(file);
    const download=await fetch(readerSeat.base+base+'/attachments/'+entry.seq+'/'+file.id+'?copy_instance='+encodeURIComponent(sub.people_copy_instance),{headers:{Origin:readerSeat.base,'X-Scimux-CSRF':readerSeat.csrf}});
    assertID('people-local-budget-explicit-retry-'+mode,matching.length===1&&entry.attachments.length===2&&entry.attachments.some(item=>item.name==='prior-budget.txt')&&uploads.length===2&&uploads.every(id=>id===stageID)&&fs.readFileSync(blob).equals(bytes)&&download.ok&&Buffer.from(await download.arrayBuffer()).equals(bytes));
    await owner.locator('#destinationagent').click();assert.equal(await owner.locator('#prompt').innerText(),agentDraft);assert.equal(await bubble.locator('[aria-label="Prepared as prompt"]').count(),1);await owner.locator('#destinationpeople').click();
    assertID('people-local-budget-zero-agent-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
    await reader.locator('#msgs .gmsg').filter({hasText:caption}).last().waitFor({timeout:40000});
    await screenshot(reader,'people-local-budget-recovered-'+mode+'.png');manifest['people-local-budget-'+mode]={status:response.status(),code:payload.error,commentCount:matching.length,before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};
  } finally {
    if(!removed&&fs.existsSync(filler))fs.unlinkSync(filler);
    await ownerSeat.api('/api/settings','PUT',{storage_global_limit_bytes:settings.storage_global_limit_bytes,storage_min_free_bytes:settings.storage_min_free_bytes});
  }
}

// Empty files fill the kept-file count while the byte budget stays open.
function fillKeptRecords(seat, sub) {
  const copy = path.join(seat.data, 'team-copies', sub.server, sub.owner, sub.chat, sub.generation);
  const records = path.join(copy, 'people-records');
  const copyStat = fs.lstatSync(copy);
  assert.ok(copyStat.isDirectory() && !copyStat.isSymbolicLink() && (copyStat.mode & 0o777) === 0o700, 'owner copy directory');
  let existing = false;
  try {
    const stat = fs.lstatSync(records);
    assert.ok(stat.isDirectory() && !stat.isSymbolicLink(), 'people-records directory');
    existing = true;
  } catch (err) { if (err.code !== 'ENOENT') throw err; }
  if (!existing) fs.mkdirSync(records, {mode: 0o700});
  fs.chmodSync(records, 0o700);
  const expected = path.join(fs.realpathSync(seat.data), 'team-copies', sub.server, sub.owner, sub.chat, sub.generation, 'people-records');
  assert.equal(fs.realpathSync(records), expected, 'filler stays in the owner kept-file directory');
  const files = [];
  try {
    for (let i = 0; i < 2048; i++) {
      const file = path.join(records, 'synthetic-kept-' + String(i).padStart(4, '0'));
      const fd = fs.openSync(file, 'wx', 0o600);
      fs.closeSync(fd);
      files.push(file);
    }
  } catch (err) { removeKeptFiller(files); throw err; }
  return {records, files};
}
function removeKeptFiller(files) {
  for (const file of files) { try { fs.unlinkSync(file); } catch (err) { if (err.code !== 'ENOENT') throw err; } }
}
function listKeptFiles(dir) {
  const out = [];
  const walk = current => {
    let entries;
    try { entries = fs.readdirSync(current, {withFileTypes: true}); } catch (err) { if (err.code === 'ENOENT') return; throw err; }
    for (const entry of entries) {
      const child = path.join(current, entry.name);
      if (entry.isDirectory() && !entry.isSymbolicLink()) walk(child);
      else out.push(child);
    }
  };
  walk(dir);
  return out;
}
async function ownerCopyReady(ownerSeat, sub) {
  const copy = path.join(ownerSeat.data, 'team-copies', sub.server, sub.owner, sub.chat, sub.generation);
  await until(async () => {
    await ownerSeat.api('/api/team/subscriptions');
    try { const stat = fs.lstatSync(copy); return stat.isDirectory() && !stat.isSymbolicLink(); } catch { return false; }
  }, 40000, 'owner copy directory');
}

async function peopleKeptFull(mode, ownerSeat, readerSeat) {
  const chat = await node(ownerSeat, 'codex', 'People kept full ' + mode, 'Synthetic kept cap setup');
  await ownerSeat.api('/api/team/shares/' + chat.id, 'PUT', {server: ownerSeat.serverID, scope: 'all', members: [{seat: readerSeat.self, access: 'write'}]});
  const share = await ownerSeat.api('/api/team/shares/' + chat.id);
  const sub = await until(async () => (await readerSeat.api('/api/team/subscriptions')).subscriptions.find(item => item.owner === ownerSeat.self && item.chat === share.uid && item.generation === share.generation && item.access === 'write' && item.people_copy_instance), 40000, 'kept cap subscription');
  const key = [sub.server, sub.owner, sub.chat, sub.generation].join('/'), base = '/api/team/subscriptions/' + key;
  await ownerCopyReady(ownerSeat, sub);
  const kept = fillKeptRecords(ownerSeat, sub);
  const owner = await pageFor(ownerSeat, mode, 'kept-full-owner-' + mode), reader = await pageFor(readerSeat, mode, 'kept-full-reader-' + mode);
  try {
    await open(owner, chat.id); await open(reader, 'team/' + key);
    await reader.locator('#prompt[contenteditable=true]').waitFor({timeout: 30000});
    await owner.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout: 30000});
    const before = {owner: counts(ownerSeat), recipient: counts(readerSeat)};
    const guestName = 'guest-over-cap.txt', guestBytes = Buffer.from('synthetic guest over cap ' + mode);
    await reader.locator('#attadd').waitFor({state: 'visible', timeout: 30000});
    await reader.locator('#attadd').click();
    const chooser = reader.waitForEvent('filechooser');
    await reader.locator('#attmenu [data-src=file]').click();
    await (await chooser).setFiles({name: guestName, mimeType: 'text/plain', buffer: guestBytes});
    await reader.locator('#attstage .stagechip.done[title="' + guestName + '"]').waitFor({timeout: 30000});
    await reader.locator('#sendbtn').click();
    const incoming = owner.locator('#msgs .gmsg').filter({hasText: guestName}).last();
    await incoming.waitFor({timeout: 40000});
    assertID('people-kept-full-guest-posted-' + mode, await incoming.locator('.attfile').count() === 1);
    const caption = 'Synthetic kept cap draft ' + mode, ownerName = 'owner-outgoing.txt', bytes = Buffer.from('synthetic owner outgoing ' + mode);
    await owner.locator('#prompt').fill(caption);
    await owner.locator('#attfile').setInputFiles({name: ownerName, mimeType: 'text/plain', buffer: bytes});
    await owner.locator('#attstage .stagechip.done[title="' + ownerName + '"]').waitFor({timeout: 30000});
    const published = owner.waitForResponse(resp => resp.request().method() === 'POST' && new URL(resp.url()).pathname === base + '/comments', {timeout: 40000});
    await owner.locator('#sendbtn').click();
    const response = await published;
    const returned = reader.locator('#msgs .gmsg').filter({hasText: caption}).last();
    await returned.waitFor({timeout: 40000});
    const saved = reader.waitForEvent('download');
    await returned.getByText(ownerName, {exact: true}).click();
    const got = await fs.promises.readFile(await (await saved).path());
    assertID('people-kept-full-published-' + mode, response.status() === 200 && got.equals(bytes) && await returned.getByText(ownerName, {exact: true}).count() === 1);
    const historyBefore = await ownerSeat.api('/api/nodes/' + chat.id + '/chat');
    const prompt = 'Synthetic kept cap agent ' + mode;
    await owner.locator('#destinationagent').click();
    await owner.locator('#destinationagent[aria-pressed=true]').waitFor({timeout: 30000});
    await owner.locator('#prompt').fill(prompt);
    const request = owner.waitForRequest(req => req.method() === 'POST' && new URL(req.url()).pathname === '/api/nodes/' + chat.id + '/send');
    await owner.locator('#sendbtn').click();
    const sent = await request;
    await until(() => counts(ownerSeat).accepted === before.owner.accepted + 1, 30000, 'kept cap explicit agent send');
    const history = await until(async () => {
      const body = await ownerSeat.api('/api/nodes/' + chat.id + '/chat');
      return (body.turns || []).some(turn => turn.role === 'user' && turn.text === prompt) ? body : null;
    }, 30000, 'kept cap session history');
    const prior = (historyBefore.turns || []).map(turn => turn.uid), now = history.turns || [];
    const after = {owner: counts(ownerSeat), recipient: counts(readerSeat)};
    assertID('people-kept-full-history-' + mode, now.length > prior.length && now.slice(0, prior.length).every((turn, index) => turn.uid === prior[index]) && now.some(turn => turn.role === 'user' && turn.text === prompt));
    assertID('people-kept-full-one-dispatch-' + mode, sent.postDataJSON().text === prompt && after.owner.attempted === before.owner.attempted + 1 && after.owner.accepted === before.owner.accepted + 1 && after.owner.controls === before.owner.controls && JSON.stringify(after.recipient) === JSON.stringify(before.recipient));
    await screenshot(owner, 'people-kept-full-owner-' + mode + '.png');
    await screenshot(reader, 'people-kept-full-reader-' + mode + '.png');
    manifest['people-kept-full-' + mode] = {before, after};
  } finally { removeKeptFiller(kept.files); }
}

async function peopleUnkeptOpen(mode, ownerSeat, readerSeat) {
  const chat = await node(ownerSeat, 'codex', 'People unkept open ' + mode, 'Synthetic unkept open setup');
  await ownerSeat.api('/api/team/shares/' + chat.id, 'PUT', {server: ownerSeat.serverID, scope: 'all', members: [{seat: readerSeat.self, access: 'write'}]});
  const share = await ownerSeat.api('/api/team/shares/' + chat.id);
  const sub = await until(async () => (await readerSeat.api('/api/team/subscriptions')).subscriptions.find(item => item.owner === ownerSeat.self && item.chat === share.uid && item.generation === share.generation && item.access === 'write' && item.people_copy_instance), 40000, 'unkept open subscription');
  await ownerCopyReady(ownerSeat, sub);
  const kept = fillKeptRecords(ownerSeat, sub);
  const owner = await pageFor(ownerSeat, mode, 'unkept-open-owner-' + mode), reader = await pageFor(readerSeat, mode, 'unkept-open-reader-' + mode);
  try {
    await open(owner, chat.id); await open(reader, 'team/' + keyOf(sub));
    await reader.locator('#prompt[contenteditable=true]').waitFor({timeout: 30000});
    const before = {owner: counts(ownerSeat), recipient: counts(readerSeat)};
    const name = 'unkept-open.txt', bytes = Buffer.from('synthetic unkept bytes ' + mode);
    await reader.locator('#attadd').waitFor({state: 'visible', timeout: 30000});
    await reader.locator('#attadd').click();
    const chooser = reader.waitForEvent('filechooser');
    await reader.locator('#attmenu [data-src=file]').click();
    await (await chooser).setFiles({name, mimeType: 'text/plain', buffer: bytes});
    await reader.locator('#attstage .stagechip.done[title="' + name + '"]').waitFor({timeout: 30000});
    await reader.locator('#sendbtn').click();
    const incoming = owner.locator('#msgs .gmsg').filter({hasText: name}).last();
    const tile = incoming.locator('a.attfile', {hasText: 'Not kept locally'});
    await tile.waitFor({timeout: 40000});
    const saved = owner.waitForEvent('download');
    await tile.click();
    const got = await fs.promises.readFile(await (await saved).path());
    const extra = listKeptFiles(kept.records).filter(file => !kept.files.includes(file));
    const after = {owner: counts(ownerSeat), recipient: counts(readerSeat)};
    assertID('people-unkept-open-bytes-' + mode, got.equals(bytes));
    assertID('people-unkept-open-note-' + mode, (await incoming.innerText()).includes('Not kept locally'));
    assertID('people-unkept-open-absent-' + mode, extra.length === 0);
    assertID('people-unkept-open-zero-agent-' + mode, JSON.stringify(after.owner) === JSON.stringify(before.owner) && JSON.stringify(after.recipient) === JSON.stringify(before.recipient));
    await screenshot(owner, 'people-unkept-open-' + mode + '.png');
    manifest['people-unkept-open-' + mode] = {before, after};
  } finally { removeKeptFiller(kept.files); }
}
function keyOf(sub) { return [sub.server, sub.owner, sub.chat, sub.generation].join('/'); }

async function peopleQuota(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','People quota '+mode,'Synthetic quota setup');
  const sharePath='/api/team/shares/'+chat.id;
  await ownerSeat.api(sharePath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  try {
  const share=await ownerSeat.api(sharePath);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),40000,'quota subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/');
  const entry=JSON.parse(fs.readFileSync(path.join(ownerSeat.data,'team.json'),'utf8')).servers.find(item=>item.id===ownerSeat.serverID);
  assert.ok(entry?.token,'synthetic relay authorization');
  const bytes=Buffer.alloc(4*1024*1024);
  for(let index=0;index<64;index++) {
    bytes.writeUInt32BE(index,bytes.length-4);
    const hash=crypto.createHash('sha256').update(bytes).digest('hex');
    const response=await fetch(relay.url+'/v1/channels/'+ownerSeat.self+'/'+share.uid+'/blobs/'+hash+'?generation='+share.generation,{method:'PUT',headers:{Authorization:'Bearer '+entry.token},body:bytes,signal:AbortSignal.timeout(30000)});
    assert.equal(response.status,201,'synthetic relay quota setup');
  }
  const page=await pageFor(readerSeat,mode,'people-quota-'+mode);await open(page,'team/'+key);
  await page.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});await page.locator('#attadd').waitFor({state:'visible',timeout:30000});
  const before={owner:counts(ownerSeat),recipient:counts(readerSeat)};
  await page.locator('#prompt').fill('Synthetic quota draft '+mode);
  const warning=page.waitForEvent('dialog');const oversized=page.locator('#attfile').setInputFiles({name:'too-large.txt',mimeType:'text/plain',buffer:Buffer.alloc(4*1024*1024+1)});
  const dialog=await warning;assertID('people-oversize-feedback-'+mode,/4 MiB|4MB/i.test(dialog.message()));await dialog.accept();await oversized;
  assertID('people-oversize-keeps-draft-'+mode,(await page.locator('#prompt').innerText())==='Synthetic quota draft '+mode);
  const upload=page.waitForRequest(req=>req.method()==='POST'&&new URL(req.url()).pathname==='/api/team/subscriptions/'+key+'/attachments');
  await page.locator('#attfile').setInputFiles({name:'quota-synthetic.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic quota file')});
  const request=await upload,stageID=new URL(request.url()).searchParams.get('stage_id');
  await page.locator('#attstage .stagechip.err .stageerror').waitFor({timeout:30000});
  assertID('people-quota-actionable-'+mode,/relay.*quota.*full/i.test(await page.locator('#attstage .stageerror').innerText()));
  const stage=await readerSeat.api('/api/team/subscriptions/'+key+'/attachments/stage/'+stageID+'?copy_instance='+encodeURIComponent(sub.people_copy_instance));
  assertID('people-quota-stage-retained-'+mode,stage?.id===stageID&&(await page.locator('#prompt').innerText())==='Synthetic quota draft '+mode);
  assertID('people-quota-no-implicit-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
  await screenshot(page,'people-quota-'+mode+'.png');manifest['people-quota-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};
  } finally {
    try { await ownerSeat.api(sharePath,'DELETE',undefined,true); }
    catch (err) { manifest.cleanup.push({quotaShareCleanupError:String(err)});throw err; }
  }
}

async function peopleStageCompetition(mode, ownerSeat, readerSeat) {
  const testRelay=await launchRelay(relayBin,'competition-relay-'+mode),target=new URL(testRelay.url);let appends=0;
  const proxy=http.createServer((req,res)=>{
    if(req.method==='POST'&&new URL(req.url,'http://synthetic.invalid').pathname.endsWith('/comments'))appends++;
    const upstream=http.request({hostname:target.hostname,port:target.port,path:req.url,method:req.method,headers:{...req.headers,host:target.host}},response=>{res.writeHead(response.statusCode,response.headers);response.pipe(res)});
    upstream.on('error',()=>{if(!res.headersSent)res.writeHead(502);res.end();});req.pipe(upstream);
  });
  await new Promise((resolve,reject)=>proxy.once('error',reject).listen(0,'127.0.0.1',resolve));
  const stopProxy=()=>proxy.close();heldReleases.add(stopProxy);
  try {
  const relayURL='http://127.0.0.1:'+proxy.address().port,identities=[];
  for(const seat of [ownerSeat,readerSeat]) {
    const issue=JSON.parse(execFileSync(relayBin,['-state-dir',testRelay.dir,'-url',relayURL,'-json','issue'],{env:baseEnv,encoding:'utf8'}));
    const joined=await seat.api('/api/team/servers','POST',{link:issue.link,name:'Synthetic competition'});
    identities.push({server:joined.id,self:(await seat.api('/api/team/servers/'+joined.id+'/seats')).self});
  }
  const [ownerIdentity,readerIdentity]=identities;
  const chat=await node(ownerSeat,'codex','Synthetic stage competition '+mode,'Synthetic setup');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerIdentity.server,scope:'all',members:[{seat:readerIdentity.self,access:'write'}]});
  const share=await ownerSeat.api('/api/team/shares/'+chat.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(x=>x.server===ownerIdentity.server&&x.owner===ownerIdentity.self&&x.chat===share.uid&&x.generation===share.generation&&x.access==='write'),40000,'competition subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),base='/api/team/subscriptions/'+key;
  const page=await pageFor(readerSeat,mode,'people-stage-competition-'+mode);await open(page,'team/'+key);
  await page.locator('#attadd').waitFor({state:'visible',timeout:30000});
  const before={owner:counts(ownerSeat),recipient:counts(readerSeat)};
  await page.locator('#prompt').fill('Newer synthetic draft '+mode);
  const upload=page.waitForRequest(req=>req.method()==='POST'&&new URL(req.url()).pathname===base+'/attachments');
  await page.locator('#attfile').setInputFiles({name:'first.txt',mimeType:'text/plain',buffer:Buffer.from('first synthetic bytes')});
  const stageID=new URL((await upload).url()).searchParams.get('stage_id');
  await page.locator('#attstage .stagechip.done[title="first.txt"]').waitFor({timeout:30000});
  assert.match(stageID,/^[0-9a-f]{16}$/);assert.match(sub.people_copy_instance,/^[0-9a-f]{32}$/);
  const original=await readerSeat.api(base+'/attachments/stage/'+stageID+'?copy_instance='+encodeURIComponent(sub.people_copy_instance));
  const firstBytes=Buffer.from('first synthetic bytes');
  assert.equal(original.id,stageID);assert.equal(original.name,'first.txt');assert.equal(original.mime,'text/plain; charset=utf-8');
  assert.equal(original.size,firstBytes.length);assert.equal(original.sha256,crypto.createHash('sha256').update(firstBytes).digest('hex'));assert.match(original.revision,/^[0-9a-f]{64}$/);
  const originalRevision=original.revision;
  assertID('people-stage-competition-no-empty-review-'+mode,(await page.locator('#attstage .stagechip.done[title="first.txt"]').count())===1&&(await page.locator('#prompt').innerText())==='Newer synthetic draft '+mode&&!(await page.locator('#attstage').innerText()).includes('No saved local People publications or files.'));
  assert.equal(appends,0,'no implicit append before publication');
  const result=await page.evaluate(async ({base,instance,stageID,originalRevision,mode})=>{
    const token=document.querySelector('meta[name="scimux-csrf"]').content, q='?copy_instance='+encodeURIComponent(instance), removalQuery=q+'&revision='+encodeURIComponent(originalRevision), headers={'X-Scimux-CSRF':token};
    const body={text:'Synthetic competing publication '+mode,anchor:0,key:'0123456789abcdef',attachments:[stageID]};
    const send=()=>fetch(base+'/comments'+q,{method:'POST',headers:{...headers,'Content-Type':'application/json'},body:JSON.stringify(body)});
    const [publication,removal]=await Promise.all([send(),fetch(base+'/attachments/stage/'+stageID+removalQuery,{method:'DELETE',headers})]);
    const replace=async()=>{const form=new FormData();form.append('files',new File(['replacement synthetic bytes'],'replacement.txt',{type:'text/plain'}));return (await fetch(base+'/attachments'+q+'&stage_id='+stageID,{method:'POST',headers,body:form})).status;};
    let replacement=0;
    if(removal.ok)replacement=await replace();
    const retry=await send();
    if(!removal.ok){const later=await fetch(base+'/attachments/stage/'+stageID+removalQuery,{method:'DELETE',headers});if(later.ok)replacement=await replace();}
    const finalRetry=await send();
    return {publication:publication.status,removal:removal.status,replacement,retry:retry.status,finalRetry:finalRetry.status};
  },{base,instance:sub.people_copy_instance,stageID,originalRevision,mode});
  assert.ok(result.retry===200,'explicit retry must confirm');
  assert.ok(result.publication===200||result.publication===409,'publication race outcome');
  assert.ok(result.removal===200||result.removal===204||result.removal===409,'removal race outcome');
  assert.ok(result.replacement>=200&&result.replacement<300&&result.finalRetry===200,'replacement created and completion retry preserved');
  const entries=(await readerSeat.api(base+'/comments?after=0')).entries.filter(x=>x.text==='Synthetic competing publication '+mode);
  assert.equal(entries.length,1);const entry=entries[0],file=entry.attachments.find(x=>x.id===stageID);assert.ok(file);
  const response=await fetch(readerSeat.base+base+'/attachments/'+entry.seq+'/'+stageID+'?copy_instance='+encodeURIComponent(sub.people_copy_instance),{headers:{Origin:readerSeat.base,'X-Scimux-CSRF':readerSeat.csrf}});
  const bytes=Buffer.from(await response.arrayBuffer()),hash=crypto.createHash('sha256').update(bytes).digest('hex');
  assertID('people-stage-competition-consistent-'+mode,response.ok&&file.sha256===hash&&file.size===bytes.length&&file.mime==='text/plain; charset=utf-8'&&((file.name==='first.txt'&&bytes.toString()==='first synthetic bytes')||(file.name==='replacement.txt'&&bytes.toString()==='replacement synthetic bytes')));
  const current=await readerSeat.api(base+'/attachments/stage/'+stageID+'?copy_instance='+encodeURIComponent(sub.people_copy_instance));assert.equal(current.name,'replacement.txt');
  assertID('people-stage-competition-one-append-'+mode,appends===1);
  assertID('people-stage-competition-draft-'+mode,(await page.locator('#prompt').innerText())==='Newer synthetic draft '+mode);
  assertID('people-stage-competition-zero-agent-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
  await page.locator('#msgs .gmsg').filter({hasText:'Synthetic competing publication '+mode}).last().waitFor({timeout:40000});
  await screenshot(page,'people-stage-competition-'+mode+'.png');manifest['people-stage-competition-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};
  } finally {heldReleases.delete(stopProxy);await new Promise(resolve=>proxy.close(resolve));}
}

async function peoplePendingPhoneReachability(mode, ownerSeat, readerSeat, ending=false) {
  const family='people-pending-layout'+(ending?'-ended':''),label=family+'-'+mode,testRelay=await launchRelay(relayBin,'pending-layout-relay-'+(ending?'ended-':'')+mode),target=new URL(testRelay.url);let releaseRelay,releaseReply,entered,replyReady,attempts=0;
  const relayGate=new Promise(resolve=>{releaseRelay=resolve;}),relayEntered=new Promise(resolve=>{entered=resolve;}),replyGate=new Promise(resolve=>{releaseReply=resolve;}),replyFetched=new Promise(resolve=>{replyReady=resolve;});
  const proxy=http.createServer(async(req,res)=>{
    if(req.method==='POST'&&new URL(req.url,'http://synthetic.invalid').pathname.endsWith('/comments')){attempts++;entered();await relayGate;}
    if(req.destroyed||res.destroyed){res.destroy();return;}
    const upstream=http.request({hostname:target.hostname,port:target.port,path:req.url,method:req.method,headers:{...req.headers,host:target.host}},response=>{res.writeHead(response.statusCode,response.headers);response.pipe(res);});upstream.on('error',()=>{if(!res.headersSent)res.writeHead(502);res.end();});req.pipe(upstream);
  });
  await new Promise((resolve,reject)=>proxy.once('error',reject).listen(0,'127.0.0.1',resolve));
  const cleanup=()=>{releaseRelay();releaseReply();proxy.closeAllConnections();proxy.close();};heldReleases.add(cleanup);
  try {
    const relayURL='http://127.0.0.1:'+proxy.address().port,identities=[];
    for(const seat of [ownerSeat,readerSeat]){const issue=JSON.parse(execFileSync(relayBin,['-state-dir',testRelay.dir,'-url',relayURL,'-json','issue'],{env:baseEnv,encoding:'utf8'}));const joined=await seat.api('/api/team/servers','POST',{link:issue.link,name:'Synthetic layout relay'});identities.push({server:joined.id,self:(await seat.api('/api/team/servers/'+joined.id+'/seats')).self});}
    const [owner,reader]=identities,chat=await node(ownerSeat,'codex','Synthetic pending layout '+mode,'Synthetic setup'),sharePath='/api/team/shares/'+chat.id,grant=access=>({server:owner.server,scope:'all',members:[{seat:reader.self,access}]});
    await ownerSeat.api(sharePath,'PUT',grant('write'));const share=await ownerSeat.api(sharePath),sub=await until(async()=>{const data=await readerSeat.api('/api/team/subscriptions');const row=data.subscriptions.find(item=>item.server===owner.server&&item.owner===owner.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),server=data.servers.find(item=>item.id===reader.server);return row?.people_copy_instance&&server?.people_file_binding?{row,server}:null;},40000,'layout subscription');
    const key=[sub.row.server,sub.row.owner,sub.row.chat,sub.row.generation].join('/'),base='/api/team/subscriptions/'+key,scope=`people\0${key}\0binding:${sub.server.people_file_binding}\0copy:${sub.row.people_copy_instance}\0`,first=await pageFor(readerSeat,mode,'people-pending-layout-origin-'+mode),before={owner:counts(ownerSeat),recipient:counts(readerSeat)};
    await open(first,'team/'+key);await first.locator('#attadd').waitFor({state:'visible',timeout:30000});await first.locator('#prompt').fill('Original synthetic pending '+mode);
    const name='long-synthetic-original-'.repeat(5)+'file.txt';await first.locator('#attfile').setInputFiles([{name,mimeType:'text/plain',buffer:Buffer.from('original one')},{name:'another-original.txt',mimeType:'text/plain',buffer:Buffer.from('original two')}]);await until(async()=>await first.locator('#attstage .stagechip.done').count()===2,30000,'two staged files');
    let replySettled=false;const originalReply=first.waitForResponse(response=>response.request().method()==='POST'&&new URL(response.url()).pathname===base+'/comments'&&new URL(response.url()).searchParams.get('copy_instance')===sub.row.people_copy_instance,{timeout:90000}).then(response=>{replySettled=true;return response;});originalReply.catch(()=>{});
    await first.route('**/api/team/subscriptions/**/comments?*',async route=>{const request=route.request(),url=new URL(request.url());if(request.method()!=='POST'||url.pathname!==base+'/comments'||url.searchParams.get('copy_instance')!==sub.row.people_copy_instance){await route.continue();return;}const response=await route.fetch({maxRetries:0});replyReady(response.status());await replyGate;if(!first.isClosed())await route.fulfill({response});});
    await first.locator('#sendbtn').click();let relayTimeout;try{await Promise.race([relayEntered,new Promise((_,reject)=>{relayTimeout=setTimeout(()=>reject(Error('relay append not reached')),15000);})]);}finally{clearTimeout(relayTimeout);}
    const state=page=>page.evaluate(async scope=>{const {createBrowserPeopleFileStore}=await import('/js/people-file-store.js'),store=createBrowserPeopleFileStore(indexedDB);try{const recovery=await store.listPeopleRecovery(scope),files=await store.list(scope);return {recovery,files:await Promise.all(files.map(async row=>({id:row.id,token:row.fileToken,name:row.file.name,text:await row.file.text()})))};}finally{store.destroy();}},scope);
    const initial=await until(async()=>{const value=await state(first);return value.recovery.pending.length===1?value:null;},30000,'persisted pending'),publication=initial.recovery.pending[0],query='?copy_instance='+encodeURIComponent(sub.row.people_copy_instance),serverPending=await readerSeat.api(base+'/pending/'+publication.request.key+query),stageID=publication.request.attachments[0],stage=await readerSeat.api(base+'/attachments/stage/'+stageID+query);assert.equal(serverPending.state,'pending');assert.match(serverPending.revision,/^[0-9a-f]{64}$/);assert.deepEqual(serverPending.stage_ids,publication.request.attachments);const stageRemoval=base+'/attachments/stage/'+stageID+query+'&revision='+encodeURIComponent(stage.revision),headers={Origin:readerSeat.base,'X-Scimux-CSRF':readerSeat.csrf},pinned=await fetch(readerSeat.base+stageRemoval,{method:'DELETE',headers});assert.equal(pinned.status,409);
    const second=await first.context().newPage();pages.push({page:second,label:'people-pending-layout-review-'+mode});pageSource.set(second,{dir:path.resolve(__dirname,'../js'),version:'current'});await second.coverage.startJSCoverage({resetOnNavigation:false});await second.goto(readerSeat.base,{waitUntil:'networkidle'});await second.locator('#prompt').waitFor({state:'visible',timeout:30000});
    await second.locator('#prompt').fill('Newer synthetic pending draft '+mode);await second.locator('#attfile').setInputFiles({name:'newer-kept.txt',mimeType:'text/plain',buffer:Buffer.from('newer exact bytes')});await second.locator('#attstage .stagechip.done[title="newer-kept.txt"]').waitFor({timeout:30000});
    const newer=await until(async()=>{const value=await state(second);return value.files.some(row=>row.name==='newer-kept.txt')&&value.recovery.heads.some(row=>row.text==='Newer synthetic pending draft '+mode)?value:null;},30000,'newer draft and file');
    if(ending)await ownerSeat.api(sharePath,'DELETE');else await ownerSeat.api(sharePath,'PUT',grant('read'));await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.some(row=>row.chat===share.uid&&row.generation===share.generation&&(ending?row.state==='ended':row.access==='read')),40000,'permission loss');
    if(ending)await second.locator('#gueststate').getByText(/Sharing ended/).waitFor({timeout:30000});else await until(async()=>!(await second.locator('#attadd').isVisible()),30000,'read-only recipient controls');
    const summary=second.locator('#attstage details summary').getByText('Review unconfirmed People message').first();await summary.waitFor({timeout:40000});await summary.click();const button=second.locator('#attstage .people-local-discard').first();await button.waitFor({state:'visible',timeout:30000});assert.match(await summary.locator('..').innerText(),/Local publication pending/);await button.scrollIntoViewIfNeeded();
    const geometry=await button.evaluate(el=>{const rect=el.getBoundingClientRect(),x=rect.left+rect.width/2,y=rect.top+rect.height/2,hit=document.elementFromPoint(x,y);return {x,y,width:rect.width,height:rect.height,viewport:innerHeight,hit:hit===el||el.contains(hit)};});assert.ok(geometry.height>=44&&geometry.y>=0&&geometry.y<geometry.viewport&&geometry.hit,`discard control unreachable: ${JSON.stringify(geometry)}`);
    const deletion=second.waitForResponse(response=>response.request().method()==='DELETE'&&new URL(response.url()).pathname===base+'/pending/'+publication.request.key);if(mode==='desktop'){await button.focus();await second.keyboard.press('Enter');}else await button.tap();const removed=await deletion,params=new URL(removed.url()).searchParams;assert.ok(removed.ok());assert.equal(params.get('copy_instance'),sub.row.people_copy_instance);assert.equal(params.get('revision'),serverPending.revision);
    const retired=await until(async()=>{const value=await state(second);return value.recovery.pending.length===0?value:null;},30000,'durable local retirement');assert.ok(retired.files.some(row=>row.name==='newer-kept.txt'&&row.text==='newer exact bytes'));assert.ok(retired.recovery.heads.some(row=>row.text==='Newer synthetic pending draft '+mode));assert.equal(await second.locator('#prompt').innerText(),'Newer synthetic pending draft '+mode);assert.deepEqual(retired.files.find(row=>row.name==='newer-kept.txt'),newer.files.find(row=>row.name==='newer-kept.txt'));
    const retiredBeforeUnpin=await readerSeat.api(base+'/pending/'+publication.request.key+query);assert.equal(retiredBeforeUnpin.state,'retired');const unpinned=await fetch(readerSeat.base+stageRemoval,{method:'DELETE',headers});assert.ok(unpinned.ok);assert.equal(replySettled,false,'original response must remain held through retirement');releaseRelay();let timer;const fetchedStatus=await Promise.race([replyFetched,new Promise((_,reject)=>{timer=setTimeout(()=>reject(Error('original response not fetched')),15000);})]).finally(()=>clearTimeout(timer));assert.ok(fetchedStatus>=400);assert.equal(replySettled,false);releaseReply();const delivered=await originalReply;await delivered.finished();await first.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));const serverRetired=await until(async()=>{const pending=await readerSeat.api(base+'/pending/'+publication.request.key+query);return pending.state==='retired'?pending:null;},30000,'server pending retired');const final=await state(second);assert.equal(final.recovery.pending.length,0);assert.deepEqual(final.files,newer.files);assert.ok(final.files.some(row=>row.name==='newer-kept.txt'&&row.text==='newer exact bytes'));assert.deepEqual(counts(ownerSeat),before.owner);assert.deepEqual(counts(readerSeat),before.recipient);
    assertID(family+'-reachable-'+mode,geometry.height>=44&&geometry.y>=0&&geometry.y<geometry.viewport&&geometry.hit);
    assertID(family+'-exact-discard-'+mode,removed.ok()&&params.get('copy_instance')===sub.row.people_copy_instance&&params.get('revision')===serverPending.revision&&pinned.status===409&&retiredBeforeUnpin.state==='retired'&&unpinned.ok&&serverRetired.state==='retired'&&attempts===1);
    assertID(family+'-retained-'+mode,final.recovery.pending.length===0&&final.recovery.heads.some(head=>head.text==='Newer synthetic pending draft '+mode)&&final.files.some(row=>row.name==='newer-kept.txt'&&row.text==='newer exact bytes'));
    assertID(family+'-zero-agent-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
    manifest[label]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};await screenshot(second,label+'.png');
  } catch(error) { console.error('Synthetic pending layout setup:',error.message); throw error; }
  finally {heldReleases.delete(cleanup);releaseRelay();releaseReply();proxy.closeAllConnections();await new Promise(resolve=>proxy.close(resolve));}
}

async function peopleLocalStageReview(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Synthetic saved file review '+mode,'Synthetic setup');
  const sharePath='/api/team/shares/'+chat.id;
  await ownerSeat.api(sharePath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api(sharePath);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),40000,'saved file subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),base='/api/team/subscriptions/'+key,page=await pageFor(readerSeat,mode,'people-local-stage-review-'+mode);
  const before={owner:counts(ownerSeat),recipient:counts(readerSeat)};
  await open(page,'team/'+key);await page.locator('#attadd').waitFor({state:'visible',timeout:30000});
  await page.locator('#prompt').fill('Synthetic saved draft '+mode);
  const upload=page.waitForRequest(req=>req.method()==='POST'&&new URL(req.url()).pathname===base+'/attachments');
  await page.locator('#attfile').setInputFiles({name:'review-synthetic.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic review bytes')});
  const stageID=new URL((await upload).url()).searchParams.get('stage_id');
  await page.locator('#attstage .stagechip.done[title="review-synthetic.txt"]').waitFor({timeout:30000});
  const stage=await readerSeat.api(base+'/attachments/stage/'+stageID+'?copy_instance='+encodeURIComponent(sub.people_copy_instance));
  assert.match(stage.revision,/^[0-9a-f]{64}$/);
  await page.reload({waitUntil:'networkidle'});
  const summary=page.locator('#attstage details summary').getByText('Review current staged files (inventory snapshot)');await summary.waitFor({timeout:40000});await summary.click();
  const button=page.locator('#attstage .people-local-stage-remove');await button.waitFor({state:'visible',timeout:30000});
  const box=await button.boundingBox();assertID('people-local-stage-review-target-'+mode,!!box&&box.height>=44);
  const geometry=await page.evaluate(()=>({viewport:innerWidth,body:document.body.scrollWidth,stage:document.querySelector('#attstage').scrollWidth,stageWidth:document.querySelector('#attstage').clientWidth}));
  assertID('people-local-stage-review-body-contained-'+mode,geometry.body<=geometry.viewport);
  assertID('people-local-stage-review-composer-contained-'+mode,geometry.stage<=geometry.stageWidth);
  const beforeRow=await page.evaluate(async id=>{const db=await new Promise((resolve,reject)=>{const req=indexedDB.open('scimux-people-file-drafts');req.onsuccess=()=>resolve(req.result);req.onerror=()=>reject(req.error);});const rows=await new Promise((resolve,reject)=>{const req=db.transaction('files').objectStore('files').getAll();req.onsuccess=()=>resolve(req.result);req.onerror=()=>reject(req.error);});db.close();const row=rows.find(value=>value.id===id);return row?{fileToken:row.fileToken,size:row.file.size}:null;},stageID);
  assert.ok(beforeRow?.fileToken,'browser file token');
  const response=page.waitForResponse(resp=>resp.request().method()==='DELETE'&&new URL(resp.url()).pathname===base+'/attachments/stage/'+stageID);
  if(mode==='desktop'){await button.focus();await page.keyboard.press('Enter');}else await button.click();
  const removed=await response,params=new URL(removed.url()).searchParams;
  assertID('people-local-stage-review-exact-'+mode,removed.ok()&&params.get('copy_instance')===sub.people_copy_instance&&params.get('revision')===stage.revision);
  await button.waitFor({state:'detached',timeout:30000});
  const inventory=await readerSeat.api(base+'/local?copy_instance='+encodeURIComponent(sub.people_copy_instance));
  assertID('people-local-stage-review-server-'+mode,!inventory.entries.some(entry=>entry.kind==='stage'&&entry.id===stageID));
  const afterRow=await page.evaluate(async id=>{const db=await new Promise((resolve,reject)=>{const req=indexedDB.open('scimux-people-file-drafts');req.onsuccess=()=>resolve(req.result);req.onerror=()=>reject(req.error);});const rows=await new Promise((resolve,reject)=>{const req=db.transaction('files').objectStore('files').getAll();req.onsuccess=()=>resolve(req.result);req.onerror=()=>reject(req.error);});db.close();const row=rows.find(value=>value.id===id);return row?{fileToken:row.fileToken,size:row.file.size}:null;},stageID);
  assertID('people-local-stage-review-preserves-'+mode,JSON.stringify(afterRow)===JSON.stringify(beforeRow)&&(await page.locator('#prompt').innerText())==='Synthetic saved draft '+mode);
  assertID('people-local-stage-review-zero-agent-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
  manifest['people-local-stage-review-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};await screenshot(page,'people-local-stage-review-'+mode+'.png');
}

async function peopleTransactionalRetirement(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Synthetic retry retirement '+mode,'Synthetic setup');
  const sharePath='/api/team/shares/'+chat.id;await ownerSeat.api(sharePath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api(sharePath),sub=await until(async()=>{const data=await readerSeat.api('/api/team/subscriptions');const row=data.subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),server=data.servers.find(item=>item.id===readerSeat.serverID);return row?.people_copy_instance&&server?.people_file_binding?{row,server}:null;},40000,'transactional retirement subscription');
  const key=[sub.row.server,sub.row.owner,sub.row.chat,sub.row.generation].join('/'),base='/api/team/subscriptions/'+key,scope=`people\0${key}\0binding:${sub.server.people_file_binding}\0copy:${sub.row.people_copy_instance}\0`,page=await pageFor(readerSeat,mode,'people-transactional-retirement-'+mode),before={owner:counts(ownerSeat),recipient:counts(readerSeat)};let comments=0;
  await page.route('**/api/team/subscriptions/**/comments?*',async route=>{const request=route.request();if(request.method()==='POST'&&new URL(request.url()).pathname===base+'/comments'){comments++;await route.abort('failed');}else await route.continue();});
  try {
    await open(page,'team/'+key);await page.locator('#attadd').waitFor({state:'visible',timeout:30000});await page.locator('#prompt').fill('Synthetic retry caption '+mode);await page.locator('#attfile').setInputFiles({name:'retire-synthetic.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic retained bytes')});await page.locator('#attstage .stagechip.done[title="retire-synthetic.txt"]').waitFor({timeout:30000});
    const dialog=page.waitForEvent('dialog'),send=page.locator('#sendbtn').click();await (await dialog).accept();await send;
    const state=()=>page.evaluate(async scope=>{const {createBrowserPeopleFileStore}=await import('/js/people-file-store.js');const store=createBrowserPeopleFileStore(indexedDB);try{const recovery=await store.listPeopleRecovery(scope),files=await store.list(scope);return {recovery,files:await Promise.all(files.map(async row=>({id:row.id,fileToken:row.fileToken,size:row.file.size,text:await row.file.text()})))};}finally{store.destroy();}},scope);
    const captured=await until(async()=>{const value=await state();return value.recovery.pending.length===1?value:null;},30000,'saved unconfirmed publication'),publication=captured.recovery.pending[0],stage=publication.request.attachments[0];
    const missing=await readerSeat.api(base+'/pending/'+publication.request.key+'?copy_instance='+encodeURIComponent(sub.row.people_copy_instance));
    assertID('people-transactional-retirement-missing-'+mode,missing.state==='missing'&&comments===1);
    const summary=page.locator('#attstage details summary').getByText('Review unconfirmed People message');await summary.waitFor({timeout:30000});await summary.click();const button=page.locator('#attstage .people-browser-retire');await button.waitFor({state:'visible',timeout:30000});
    const response=page.waitForResponse(r=>r.request().method()==='POST'&&new URL(r.url()).pathname===base+'/pending/'+publication.request.key+'/retire-absent');if(mode==='desktop'){await button.focus();await page.keyboard.press('Enter');}else await button.click();const retired=await response,body=await retired.json();
    assertID('people-transactional-retirement-bodyless-'+mode,retired.ok()&&body.state==='locally_discarded'&&retired.request().postData()===null);
    const after=await until(async()=>{const value=await state();return value.recovery.pending.length===0?value:null;},30000,'local retirement decision'),server=await readerSeat.api(base+'/pending/'+publication.request.key+'?copy_instance='+encodeURIComponent(sub.row.people_copy_instance));
    assertID('people-transactional-retirement-marker-'+mode,server.state==='absent_retired'&&after.recovery.pending.length===0);
    assertID('people-transactional-retirement-preserves-'+mode,JSON.stringify(after.files)===JSON.stringify(captured.files)&&after.files[0].id===stage&&after.files[0].text==='synthetic retained bytes'&&after.recovery.heads.some(head=>head.text==='Synthetic retry caption '+mode)&&(await page.locator('#prompt').innerText())==='Synthetic retry caption '+mode);
    assertID('people-transactional-retirement-zero-agent-'+mode,comments===1&&JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
    manifest['people-transactional-retirement-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};await screenshot(page,'people-transactional-retirement-'+mode+'.png');
  } finally {await page.unroute('**/api/team/subscriptions/**/comments?*');}
}

async function peopleTransactionalDrafts(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Synthetic concurrent drafts '+mode,'Synthetic setup');
  const sharePath='/api/team/shares/'+chat.id;
  await ownerSeat.api(sharePath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api(sharePath),sub=await until(async()=>{const data=await readerSeat.api('/api/team/subscriptions');const row=data.subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write');const server=data.servers.find(item=>item.id===readerSeat.serverID);return row?.people_copy_instance&&server?.people_file_binding?{row,server}:null;},40000,'transactional draft subscription');
  const key=[sub.row.server,sub.row.owner,sub.row.chat,sub.row.generation].join('/'),scope=`people\0${key}\0binding:${sub.server.people_file_binding}\0copy:${sub.row.people_copy_instance}\0`,first=await pageFor(readerSeat,mode,'people-transactional-drafts-'+mode),before={owner:counts(ownerSeat),recipient:counts(readerSeat)};let posts=0;
  first.on('request',request=>{if(request.method()==='POST'&&new URL(request.url()).pathname==='/api/team/subscriptions/'+key+'/comments')posts++;});
  await open(first,'team/'+key);await first.locator('#prompt').waitFor({state:'visible',timeout:30000});
  const legacyKey='scimux-people-draft:team/'+key;await first.evaluate(({legacyKey})=>localStorage.setItem(legacyKey,'synthetic preserved legacy'),{legacyKey});
  const state=page=>page.evaluate(async scope=>{const {createBrowserPeopleFileStore}=await import('/js/people-file-store.js');const store=createBrowserPeopleFileStore(indexedDB);try{return await store.listPeopleRecovery(scope);}finally{store.destroy();}},scope);
  await first.locator('#prompt').fill('same synthetic text');const initial=await until(async()=>{const snapshot=await state(first);return snapshot.heads.length===1&&snapshot.heads[0].text==='same synthetic text'?snapshot:null;},30000,'first durable draft');
  const second=await first.context().newPage();pages.push({page:second,label:'people-transactional-drafts-second-'+mode});pageSource.set(second,{dir:path.resolve(__dirname,'../js'),version:'current'});await second.coverage.startJSCoverage({resetOnNavigation:false});second.on('request',request=>{if(request.method()==='POST'&&new URL(request.url()).pathname==='/api/team/subscriptions/'+key+'/comments')posts++;});await second.goto(readerSeat.base,{waitUntil:'networkidle'});
  await until(async()=>await second.locator('#prompt').innerText()==='same synthetic text',40000,'restored transactional draft');
  await second.locator('#prompt').fill('same synthetic text');const next=await until(async()=>{const snapshot=await state(second);return snapshot.heads.length===1&&snapshot.heads[0].version!==initial.heads[0].version?snapshot:null;},30000,'newer same-text draft');
  await first.locator('#prompt').fill('same synthetic text');const fork=await until(async()=>{const snapshot=await state(first);return snapshot.heads.length===2?snapshot:null;},30000,'separate concurrent drafts');
  assertID('people-transactional-drafts-separate-'+mode,fork.heads.every(head=>head.text==='same synthetic text')&&new Set(fork.heads.map(head=>head.version)).size===2&&fork.heads.some(head=>head.version===next.heads[0].version));
  const stale=await first.evaluate(async({scope,head,version})=>{const {createBrowserPeopleFileStore}=await import('/js/people-file-store.js');const store=createBrowserPeopleFileStore(indexedDB);try{return await store.clearPeopleDraft({scope,head,version});}finally{store.destroy();}},{scope,head:initial.heads[0].head,version:initial.heads[0].version});
  const after=await state(second);assertID('people-transactional-drafts-stale-clear-'+mode,stale===false&&after.heads.length===2&&after.heads.some(head=>head.version===next.heads[0].version));
  assertID('people-transactional-drafts-legacy-'+mode,(await first.evaluate(key=>localStorage.getItem(key),legacyKey))==='synthetic preserved legacy');
  assertID('people-transactional-drafts-zero-agent-'+mode,posts===0&&JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
  manifest['people-transactional-drafts-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};await screenshot(first,'people-transactional-drafts-'+mode+'.png');
}

async function peopleStaleRetry(mode,ownerSeat,otherOwner,readerSeat,observer) {
  const label='people-stale-retry-'+mode,chat=await node(ownerSeat,'codex','Synthetic stale retry '+mode,'Synthetic setup'),sharePath='/api/team/shares/'+chat.id;
  await ownerSeat.api(sharePath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api(sharePath),sub=await until(async()=>{const data=await readerSeat.api('/api/team/subscriptions'),row=data.subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),server=data.servers.find(item=>item.id===readerSeat.serverID);return row?.people_copy_instance&&server?.people_file_binding?{row,server}:null;},40000,'stale retry subscription');
  const key=[sub.row.server,sub.row.owner,sub.row.chat,sub.row.generation].join('/'),base='/api/team/subscriptions/'+key,scope=`people\0${key}\0binding:${sub.server.people_file_binding}\0copy:${sub.row.people_copy_instance}\0`,first=await pageFor(readerSeat,mode,label+'-first');
  const seats={'owner-a':ownerSeat,'owner-b':otherOwner,recipient:readerSeat,observer},countsAll=()=>Object.fromEntries(Object.entries(seats).map(([name,seat])=>[name,counts(seat)]));
  const state=page=>page.evaluate(async scope=>{const db=await new Promise((resolve,reject)=>{const req=indexedDB.open('scimux-people-file-drafts',2);req.onsuccess=()=>resolve(req.result);req.onerror=()=>reject(req.error);});try{const tx=db.transaction(['files','recovery'],'readonly'),get=name=>new Promise((resolve,reject)=>{const req=tx.objectStore(name).getAll();req.onsuccess=()=>resolve(req.result);req.onerror=()=>reject(req.error);});const [files,recovery]=await Promise.all([get('files'),get('recovery')]);return {recovery:recovery.filter(row=>row.scope===scope),files:await Promise.all(files.filter(row=>row.scope===scope).map(async row=>({id:row.id,fileToken:row.fileToken,stageRevision:row.stageRevision,name:row.file.name,bytes:Array.from(new Uint8Array(await row.file.arrayBuffer()))})))};}finally{db.close();}},scope);
  let firstPosts=0,secondPosts=0,unexpected=0,unexpectedDialogs=0,release,ready,rejectReady,released=false;const gate=new Promise(resolve=>{release=resolve;}),acceptedPromise=new Promise((resolve,reject)=>{ready=resolve;rejectReady=reject;});acceptedPromise.catch(()=>{});const unblock=()=>{released=true;release();};heldReleases.add(unblock);
  await first.evaluate(()=>{const add=EventTarget.prototype.addEventListener;window.__syntheticRetryHandlers=[];EventTarget.prototype.addEventListener=function(type,listener,options){if(type==='click'&&this instanceof HTMLElement&&this.classList.contains('people-retry')&&typeof listener==='function')window.__syntheticRetryHandlers.push({element:this,listener});return add.call(this,type,listener,options);};});
  await first.route('**/api/team/subscriptions/**/comments**',async route=>{const request=route.request();if(request.method()==='POST'&&new URL(request.url()).pathname===base+'/comments'){firstPosts++;if(firstPosts>1)unexpected++;await route.abort('failed');}else await route.continue();});
  let second;
  try {
    await open(first,'team/'+key);await first.locator('#attadd').waitFor({timeout:30000});await first.locator('#prompt').fill('Synthetic old retry '+mode);await first.locator('#attfile').setInputFiles({name:'old-kept.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic old retained bytes')});await first.locator('#attstage .stagechip.done[title="old-kept.txt"]').waitFor({timeout:30000});
    const dialog=first.waitForEvent('dialog'),send=first.locator('#sendbtn').click();await (await dialog).accept();await send;
    const oldState=await until(async()=>{const value=await state(first),row=value.recovery.find(r=>r.kind==='publication'&&r.state==='pending');return row?{value,row}:null;},30000,'old pending publication'),old=oldState.row;
    await first.locator('#attstage details summary').getByText('Review unconfirmed People message').waitFor({timeout:30000});await first.locator('#attstage details summary').getByText('Review unconfirmed People message').click();await first.locator('.people-retry').waitFor({timeout:30000});
    first.on('dialog',dialog=>{unexpectedDialogs++;dialog.dismiss().catch(()=>{});});const captured=await first.evaluate(()=>{const entry=window.__syntheticRetryHandlers.at(-1);if(!entry)return false;window.__syntheticOldRetry=entry;return true;});assert.ok(captured);const missing=await readerSeat.api(base+'/pending/'+old.request.key+'?copy_instance='+encodeURIComponent(sub.row.people_copy_instance));assertID('people-stale-retry-prepared-'+mode,firstPosts===1&&missing.state==='missing'&&old.request.attachments.length===1);
    second=await first.context().newPage();pages.push({page:second,label:label+'-second'});pageSource.set(second,{dir:path.resolve(__dirname,'../js'),version:'current'});await second.coverage.startJSCoverage({resetOnNavigation:false});await second.goto(readerSeat.base,{waitUntil:'networkidle'});if(mode==='phone'&&await second.locator('#chatback').isVisible())await second.locator('#chatback').click();await open(second,'team/'+key);
    const summary=second.locator('#attstage details summary').getByText('Review unconfirmed People message');await summary.waitFor({timeout:30000});await summary.click();const retire=second.locator('#attstage .people-browser-retire');await retire.waitFor({timeout:30000});const retireResponse=second.waitForResponse(r=>r.request().method()==='POST'&&new URL(r.url()).pathname===base+'/pending/'+old.request.key+'/retire-absent');await retire.click();assert.ok((await retireResponse).ok());
    const retired=await until(async()=>{const value=await state(second),decision=value.recovery.find(r=>r.kind==='decision'&&r.publication===old.publication);return decision?.state==='retired'?{value,decision}:null;},30000,'old local decision');assert.deepEqual(retired.value.files,oldState.value.files);assertID('people-stale-retry-retired-'+mode,retired.decision.state==='retired'&&retired.value.files.some(file=>file.id===old.request.attachments[0]));
    await second.route('**/api/team/subscriptions/**/comments**',async route=>{const request=route.request();if(request.method()!=='POST'||new URL(request.url()).pathname!==base+'/comments'){await route.continue();return;}secondPosts++;if(secondPosts>1){unexpected++;await route.abort('failed');return;}try{const response=await route.fetch({maxRetries:0}),body=await response.json();ready({status:response.status(),body,request:request.postDataJSON()});await gate;if(!second.isClosed())await route.fulfill({response});}catch(error){rejectReady(error);if(!released&&!shutting&&!second.isClosed())throw error;}});
    await second.locator('#prompt').fill('Synthetic fresh retry '+mode);const delivered=second.waitForResponse(response=>response.request().method()==='POST'&&new URL(response.url()).pathname===base+'/comments',{timeout:90000});delivered.catch(()=>{});await second.locator('#sendbtn').click();let timer;const accepted=await Promise.race([acceptedPromise,new Promise((_,reject)=>{timer=setTimeout(()=>reject(Error('fresh response not fetched')),30000);})]).finally(()=>clearTimeout(timer));assert.equal(accepted.status,200);assert.notEqual(accepted.request.key,old.request.key);assert.deepEqual(accepted.request.attachments,old.request.attachments,'restored upload must retain its original stage ID');
    const fresh=await until(async()=>{const value=await state(second),row=value.recovery.find(r=>r.kind==='publication'&&r.state==='pending'&&r.request.key===accepted.request.key);return row?{value,row}:null;},30000,'fresh pending publication');assertID('people-stale-retry-fresh-'+mode,fresh.row.publication!==old.publication&&fresh.row.request.text==='Synthetic fresh retry '+mode&&JSON.stringify(fresh.row.request.attachments)===JSON.stringify(old.request.attachments));
    await second.locator('#prompt').fill('Synthetic newer draft '+mode);await second.locator('#attfile').setInputFiles({name:'new-kept.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic newer retained bytes')});await second.locator('#attstage .stagechip.done[title="new-kept.txt"]').waitFor({timeout:30000});
    if(mode==='phone')await first.locator('#chatback').click();await open(first,readerSeat.node.id);if(mode==='phone')await first.locator('#chatback').click();await open(first,'team/'+key);await first.locator('#prompt').waitFor({timeout:30000});await until(async()=>{const v=await state(first);return v.recovery.some(row=>row.kind==='publication'&&row.publication===fresh.row.publication)&&v.files.some(file=>file.name==='new-kept.txt');},30000,'fresh state after navigation');await until(async()=>await first.locator('#attstage textarea').evaluateAll((nodes,text)=>nodes.some(node=>node.value===text),'Synthetic fresh retry '+mode),30000,'fresh retry cache rendered');assert.ok(await first.locator('#attstage .people-retry').count());
    const saved=await state(first),before=countsAll(),beforeComments={attempted:unexpected,accepted:0,controls:0};await first.evaluate(async()=>{const {element,listener}=window.__syntheticOldRetry;await listener.call(element,new MouseEvent('click',{bubbles:true}));});await first.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));const after=countsAll(),afterComments={attempted:unexpected,accepted:0,controls:0},afterStale=await state(first);
    assert.deepEqual(afterStale,saved);assert.deepEqual(after,before);assertID('people-stale-retry-stale-safe-'+mode,unexpected===0&&unexpectedDialogs===0&&JSON.stringify(afterStale)===JSON.stringify(saved));assertID('people-stale-retry-zero-agent-'+mode,JSON.stringify(before)===JSON.stringify(after));
    release();const delivery=await delivered;await delivery.finished();const entries=await until(async()=>{const rows=(await readerSeat.api(base+'/comments?after=0')).entries.filter(row=>row.seq===accepted.body.seq);return rows.length===1?rows:null;},30000,'fresh accepted comment');assert.equal(entries[0].text,'Synthetic fresh retry '+mode);assert.equal(secondPosts,1);assert.equal(unexpected,0);const late=await state(second),newFile=saved.files.find(file=>file.name==='new-kept.txt');assert.deepEqual(late.files.find(file=>file.name==='new-kept.txt'),newFile);assert.ok(late.recovery.some(row=>row.kind==='draft'&&row.text==='Synthetic newer draft '+mode));assert.deepEqual(late.recovery.find(row=>row.kind==='decision'&&row.publication===old.publication),retired.decision);assertID('people-stale-retry-late-'+mode,entries.length===1&&secondPosts===1&&unexpected===0);
    manifest[label]={before,after,beforeComments,afterComments};
    await first.waitForFunction(title=>document.querySelector('#chattitle')?.textContent.trim()===title,'Synthetic stale retry '+mode,{timeout:10000});
    if(mode==='phone'){
      let previous=null,stable=0;
      await until(async()=>{const sample=await first.evaluate(()=>{const chat=document.querySelector('#chat')?.getBoundingClientRect(),cards=document.querySelector('#cards')?.getBoundingClientRect();return chat&&cards?{left:chat.left,width:chat.width,viewport:innerWidth,cardsRight:cards.right}:null;});
        if(!sample||Math.abs(sample.left)>1||Math.abs(sample.width-sample.viewport)>1||sample.cardsRight>0){previous=null;stable=0;return false;}
        stable=previous&&Math.abs(sample.left-previous.left)<=.5&&Math.abs(sample.width-previous.width)<=.5&&Math.abs(sample.cardsRight-previous.cardsRight)<=.5?stable+1:1;previous=sample;return stable>=3;
      },10000,'settled selected chat');
    }
    await screenshot(first,label+'.png');
  } finally {unblock();heldReleases.delete(unblock);await first.unrouteAll({behavior:'wait'});if(second)await second.unrouteAll({behavior:'wait'});}
}

async function peopleCapturedDiscard(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Synthetic captured review '+mode,'Synthetic setup'),sharePath='/api/team/shares/'+chat.id;
  await ownerSeat.api(sharePath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api(sharePath),sub=await until(async()=>{const data=await readerSeat.api('/api/team/subscriptions'),row=data.subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),server=data.servers.find(item=>item.id===readerSeat.serverID);return row?.people_copy_instance&&server?.people_file_binding?{row,server}:null;},40000,'capture review subscription');
  const key=[sub.row.server,sub.row.owner,sub.row.chat,sub.row.generation].join('/'),scope=`people\0${key}\0binding:${sub.server.people_file_binding}\0copy:${sub.row.people_copy_instance}\0`,page=await pageFor(readerSeat,mode,'people-captured-discard-'+mode),before={owner:counts(ownerSeat),recipient:counts(readerSeat)};let comments=0;
  page.on('request',request=>{if(request.method()==='POST'&&new URL(request.url()).pathname==='/api/team/subscriptions/'+key+'/comments')comments++;});
  await open(page,'team/'+key);await page.locator('#prompt').waitFor({state:'visible',timeout:30000});
  const captured=await page.evaluate(async scope=>{const {createBrowserPeopleFileStore}=await import('/js/people-file-store.js'),store=createBrowserPeopleFileStore(indexedDB);try{const draft=await store.savePeopleDraft({scope,text:'synthetic editable'}),capture=await store.freezePeopleDraft({scope,text:'synthetic editable',...draft});await store.save(scope,'3333333333333333',new File(['synthetic'], 'kept.txt',{type:'text/plain'}));localStorage.setItem('synthetic-capture-legacy','unchanged');return {draft,capture};}finally{store.destroy();}},scope);
  await page.reload({waitUntil:'networkidle'});if(mode==='phone'&&await page.locator('#chatback').isVisible())await page.locator('#chatback').click();await open(page,'team/'+key);await page.locator('#prompt').waitFor({state:'visible',timeout:30000});
  const summary=page.locator('#attstage details summary').getByText('Review saved People drafts');await summary.waitFor({state:'visible',timeout:30000});await summary.click();
  const button=page.locator('#attstage .people-captured-discard');await button.waitFor({state:'visible',timeout:30000});
  const style=await button.evaluate(el=>{const reference=document.createElement('button');reference.type='button';reference.className='btn-tinted';el.parentElement.appendChild(reference);try{const actual=getComputedStyle(el), expected=getComputedStyle(reference), properties=['display','alignItems','justifyContent','gap','padding','borderRadius','fontSize','backgroundColor','color'];return {height:el.getBoundingClientRect().height,actual:Object.fromEntries(properties.map(key=>[key,actual[key]])),expected:Object.fromEntries(properties.map(key=>[key,expected[key]]))};}finally{reference.remove();}});
  assert.ok(style.height>=44, 'captured discard has a 44px target');assert.deepEqual(style.actual,style.expected,'captured discard uses the tinted button style');
  if(mode==='desktop'){await button.focus();await page.keyboard.press('Enter');}else await button.tap();
  const after=await until(async()=>{const value=await page.evaluate(async scope=>{const {createBrowserPeopleFileStore}=await import('/js/people-file-store.js'),store=createBrowserPeopleFileStore(indexedDB);try{return {recovery:await store.listPeopleRecovery(scope),files:await Promise.all((await store.list(scope)).map(async row=>({name:row.file.name,text:await row.file.text()}))),legacy:localStorage.getItem('synthetic-capture-legacy')};}finally{store.destroy();}},scope);return value.recovery.heads.some(row=>row.head===captured.capture.head)?null:value;},30000,'capture cleanup state');
  assertID('people-captured-discard-exact-'+mode,!after.recovery.heads.some(row=>row.head===captured.capture.head)&&after.recovery.heads.some(row=>row.head===captured.draft.head&&row.text==='synthetic editable'));
  assertID('people-captured-discard-preserves-'+mode,after.files.length===1&&after.files[0].name==='kept.txt'&&after.files[0].text==='synthetic'&&after.legacy==='unchanged');
  assertID('people-captured-discard-zero-agent-'+mode,comments===0&&JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
  manifest['people-captured-discard-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};await screenshot(page,'people-captured-discard-'+mode+'.png');
}

async function peopleTextRetirement(mode, ownerSeat, otherOwner, readerSeat, observer, variant) {
  const label='people-text-retirement-'+variant+'-'+mode, testRelay=await launchRelay(relayBin,'text-retirement-relay-'+variant+'-'+mode), identities=[];
  for(const seat of [ownerSeat,readerSeat]) {
    const issue=JSON.parse(execFileSync(relayBin,['-state-dir',testRelay.dir,'-url',testRelay.url,'-json','issue'],{env:baseEnv,encoding:'utf8'}));
    const joined=await seat.api('/api/team/servers','POST',{link:issue.link,name:'Synthetic text relay'});
    identities.push({server:joined.id,self:(await seat.api('/api/team/servers/'+joined.id+'/seats')).self});
  }
  const [owner,reader]=identities,chat=await node(ownerSeat,'codex','Synthetic text retirement '+variant+' '+mode,'Synthetic setup'),sharePath='/api/team/shares/'+chat.id;
  const grant=access=>({server:owner.server,scope:'all',members:[{seat:reader.self,access}]});
  await ownerSeat.api(sharePath,'PUT',grant('write'));const share=await ownerSeat.api(sharePath);
  const sendingSeat=variant==='readonly'?readerSeat:ownerSeat;
  const subscription=await until(async()=>{const data=await sendingSeat.api('/api/team/subscriptions'),row=data.subscriptions.find(item=>item.server===owner.server&&item.owner===owner.self&&item.chat===share.uid&&item.generation===share.generation&&(variant==='readonly'?item.access==='write':item.access==='owner'));return row?.people_copy_instance?row:null;},40000,'text retirement subscription');
  const key=[subscription.server,subscription.owner,subscription.chat,subscription.generation].join('/'),base='/api/team/subscriptions/'+key,original='Synthetic explicit human text '+variant+' '+mode,newer='Synthetic newer text '+variant+' '+mode;
  const page=await pageFor(sendingSeat,mode,label+'-origin'),seatMap={'owner-a':ownerSeat,'owner-b':otherOwner,recipient:readerSeat,observer},snapshotCounts=()=>Object.fromEntries(Object.entries(seatMap).map(([role,seat])=>[role,counts(seat)])),before=snapshotCounts();
  let release,ready,rejectReady,released=false,postCount=0,extraPosts=0,requestBody;const gate=new Promise(resolve=>{release=resolve;}),fetched=new Promise((resolve,reject)=>{ready=resolve;rejectReady=reject;});fetched.catch(()=>{});
  const cleanup=()=>{released=true;release();};heldReleases.add(cleanup);
  const state=page=>page.evaluate(async()=>{const db=await new Promise((resolve,reject)=>{const request=indexedDB.open('scimux-people-file-drafts',2);request.onsuccess=()=>resolve(request.result);request.onerror=()=>reject(request.error);});try{const tx=db.transaction(['files','recovery'],'readonly'),read=store=>new Promise((resolve,reject)=>{const request=tx.objectStore(store).getAll();request.onsuccess=()=>resolve(request.result);request.onerror=()=>reject(request.error);});const [files,recovery]=await Promise.all([read('files'),read('recovery')]);return {recovery,files:await Promise.all(files.map(async row=>({key:row.key,id:row.id,fileToken:row.fileToken,name:row.file.name,bytes:Array.from(new Uint8Array(await row.file.arrayBuffer()))}))),legacy:localStorage.getItem('synthetic-text-retirement-legacy')};}finally{db.close();}});
  try {
    await open(page,variant==='readonly'?'team/'+key:chat.id);if(variant!=='readonly')await page.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});
    await page.evaluate(()=>localStorage.setItem('synthetic-text-retirement-legacy','unchanged'));
    const responsePromise=page.waitForResponse(response=>response.request().method()==='POST'&&new URL(response.url()).pathname===base+'/comments',{timeout:90000});responsePromise.catch(()=>{});
    await page.route('**/api/team/subscriptions/**/comments**',async route=>{const request=route.request();if(request.method()!=='POST'||new URL(request.url()).pathname!==base+'/comments'){await route.continue();return;}try{postCount++;requestBody=request.postDataJSON();const response=await route.fetch({maxRetries:0});const body=await response.json();ready({status:response.status(),body});await gate;if(!page.isClosed())await route.fulfill({response});}catch(error){rejectReady(error);if(!released&&!shutting&&!page.isClosed())throw error;}});
    await page.locator('#prompt').fill(original);await page.locator('#sendbtn').click();let timeout;const accepted=await Promise.race([fetched,new Promise((_,reject)=>{timeout=setTimeout(()=>reject(Error('text response not fetched')),30000);})]).finally(()=>clearTimeout(timeout));assert.equal(accepted.status,200);assert.ok(Number.isSafeInteger(accepted.body.seq)&&accepted.body.seq>0);assert.equal(requestBody.text,original);assert.match(requestBody.key,/^[0-9a-f]{16}$/);
    const initial=await until(async()=>{const value=await state(page),pending=value.recovery.find(row=>row.kind==='text-publication'&&row.state==='pending'&&row.request?.key===requestBody.key);return pending?{value,pending}:null;},30000,'immutable text pending');const publication=initial.pending;assert.deepEqual(publication.request,requestBody);assert.ok(publication.head&&publication.version&&publication.publication&&publication.scope);assert.equal(publication.peopleKey,key);assert.equal(publication.copyInstance,subscription.people_copy_instance);assert.equal(publication.native,variant==='readonly'?'':chat.instance_id);assert.ok(initial.value.recovery.some(row=>row.kind==='head'&&row.head===publication.head&&row.version===publication.version&&row.sealed));assert.ok(initial.value.recovery.some(row=>row.kind==='draft'&&row.version===publication.version&&row.text===original));
    assertID('people-text-retirement-'+variant+'-prepared-'+mode,postCount===1&&accepted.status===200&&publication.request.key===requestBody.key);
    const review=await page.context().newPage();review.on('request',request=>{if(request.method()==='POST'&&new URL(request.url()).pathname===base+'/comments')extraPosts++;});pages.push({page:review,label:label+'-review'});pageSource.set(review,{dir:path.resolve(__dirname,'../js'),version:'current'});await review.coverage.startJSCoverage({resetOnNavigation:false});await review.goto(sendingSeat.base,{waitUntil:'networkidle'});if(mode==='phone'&&await review.locator('#chatback').isVisible())await review.locator('#chatback').click();await open(review,variant==='readonly'?'team/'+key:chat.id);
    await review.locator('#prompt').fill(newer);await until(async()=>(await state(review)).recovery.some(row=>row.kind==='draft'&&row.text===newer),30000,'newer text saved');
    if(await review.locator('#attadd').isVisible()) {
      await review.locator('#attfile').setInputFiles({name:'retained.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic retained bytes')});
      await review.locator('#attstage .stagechip.done[title="retained.txt"]').waitFor({timeout:30000});
    } else {
      // A local seed is only a retained-byte check; it does not prove an upload.
      await review.evaluate(async scope=>{const {createBrowserPeopleFileStore}=await import('/js/people-file-store.js'),store=createBrowserPeopleFileStore(indexedDB);try{await store.save(scope,'3333333333333333',new File(['synthetic retained bytes'],'retained.txt',{type:'text/plain'}));}finally{store.destroy();}},publication.scope);
    }
    if(variant==='readonly') {await ownerSeat.api(sharePath,'PUT',grant('read'));await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.some(row=>row.chat===share.uid&&row.generation===share.generation&&row.access==='read'),40000,'read-only transition');await review.locator('#gueststate').getByText(/Can view/i).waitFor({timeout:30000});await review.locator('#prompt[contenteditable=false]').waitFor({state:'visible',timeout:30000});assert.equal(await review.locator('#prompt').getAttribute('contenteditable'),'false');assert.equal(await review.locator('#destinationagent').isVisible(),false);assert.equal(await review.locator('#convtools').isVisible(),false);assert.equal(await review.locator('#workpulse').evaluate(el=>el.classList.contains('on')),false);}
    else {await review.locator('#sharestatus').waitFor({state:'visible',timeout:30000});await review.locator('#destinationagent').click();await review.locator('#destinationagent[aria-pressed=true]').waitFor({timeout:30000});await review.locator('#prompt').fill('Synthetic unsent Agent draft '+mode);await review.locator('#attadd').waitFor({state:'visible',timeout:30000});await review.locator('#attfile').setInputFiles({name:'agent-unsent.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic agent bytes')});await review.locator('#attstage .stagechip.done[title="agent-unsent.txt"]').waitFor({timeout:30000});await ownerSeat.api(sharePath,'DELETE');await until(async()=>(await ownerSeat.api('/api/team/subscriptions')).subscriptions.some(row=>row.chat===share.uid&&row.generation===share.generation&&row.state==='ended'),40000,'owner sharing ended');await until(async()=>!(await review.locator('#sharestatus').isVisible())||/sharing ended/i.test(await review.locator('#sharestatus').innerText()),30000,'ended sharing UI');const live=(await ownerSeat.api('/api/state')).nodes.find(row=>row.id===chat.id);assert.ok(live&&live.instance_id===chat.instance_id&&!live.ended_at);}
    const saved=await state(review);assert.ok(saved.files.some(row=>row.name==='retained.txt'&&Buffer.from(row.bytes).toString()==='synthetic retained bytes'));const summary=review.locator('#attstage details').filter({has:review.locator('summary').getByText('Review unconfirmed People text')}).filter({has:review.locator('[data-publication="'+publication.publication+'"]')}).locator('summary');await summary.waitFor({state:'visible',timeout:30000});await summary.click();const details=summary.locator('..'),button=details.locator('button[data-publication="'+publication.publication+'"]');await button.waitFor({state:'visible',timeout:30000});await button.scrollIntoViewIfNeeded();const box=await button.boundingBox();assertID('people-text-retirement-'+variant+'-reachable-'+mode,!!box&&box.height>=44);if(mode==='desktop'){await button.focus();await review.keyboard.press('Enter');}else await button.tap();
    const retired=await until(async()=>{const value=await state(review),decision=value.recovery.find(row=>row.kind==='text-decision'&&row.publication===publication.publication);return decision?.state==='retired'?{value,decision}:null;},30000,'local text decision');assert.deepEqual(retired.decision,{...publication,key:'text-decision:'+publication.publication,kind:'text-decision',state:'retired'});assert.ok(!retired.value.recovery.some(row=>row.kind==='text-publication'&&row.publication===publication.publication));assertID('people-text-retirement-'+variant+'-local-'+mode,true);assert.match(await review.locator('#attstage').innerText(),/local|retir/i);assert.match(await review.locator('#attstage').innerText(),/not retract|does not retract/i);
    assert.deepEqual(retired.value.recovery.filter(row=>row.publication!==publication.publication),saved.recovery.filter(row=>row.publication!==publication.publication));assert.deepEqual(retired.value.files,saved.files);assert.equal(retired.value.legacy,'unchanged');assert.equal(await review.locator('#prompt').innerText(),variant==='readonly'?newer:'Synthetic unsent Agent draft '+mode);if(variant!=='readonly'){assert.match(await review.locator('#attstage').innerText(),/agent-unsent\.txt/);assert.equal(await review.locator('#destinationagent').getAttribute('aria-pressed'),'true');}assertID('people-text-retirement-'+variant+'-preserves-'+mode,true);
    cleanup();const delivered=await responsePromise;assert.ok(delivered.ok());await delivered.finished();await page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));const final=await state(review);assert.deepEqual(final,retired.value);assert.equal(await review.locator('#prompt').innerText(),variant==='readonly'?newer:'Synthetic unsent Agent draft '+mode);if(variant!=='readonly'){assert.match(await review.locator('#attstage').innerText(),/agent-unsent\.txt/);assert.equal(await review.locator('#destinationagent').getAttribute('aria-pressed'),'true');}else{assert.equal(await review.locator('#convtools').isVisible(),false);assert.equal(await review.locator('#workpulse').evaluate(el=>el.classList.contains('on')),false);}
    const entries=(await sendingSeat.api(base+'/comments?after=0')).entries.filter(row=>row.seq===accepted.body.seq);assert.equal(entries.length,1);assert.equal(entries[0].text,original);assert.equal(postCount,1);assert.equal(extraPosts,0);assertID('people-text-retirement-'+variant+'-late-'+mode,true);
    const after=snapshotCounts();assert.deepEqual(after,before);assertID('people-text-retirement-'+variant+'-zero-agent-'+mode,true);manifest[label]={before,after};await screenshot(review,label+'.png');
  }finally{heldReleases.delete(cleanup);cleanup();await page.unrouteAll({behavior:'wait'});}
}

async function peopleNoStorageText(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','Synthetic page-only retry '+mode,'Synthetic setup'),sharePath='/api/team/shares/'+chat.id;
  await ownerSeat.api(sharePath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});const share=await ownerSeat.api(sharePath),sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(row=>row.owner===ownerSeat.self&&row.chat===share.uid&&row.generation===share.generation&&row.access==='write'),40000,'page-only subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),base='/api/team/subscriptions/'+key,page=await pageFor(readerSeat,mode,'people-no-storage-text-'+mode),before={owner:counts(ownerSeat),recipient:counts(readerSeat)},posts=[];
  await page.addInitScript(()=>Object.defineProperty(globalThis,'indexedDB',{value:undefined,configurable:true}));await page.reload({waitUntil:'networkidle'});await open(page,'team/'+key);await page.locator('#prompt').waitFor({state:'visible',timeout:30000});assert.equal(await page.evaluate(()=>typeof indexedDB),'undefined');
  page.on('request',request=>{if(request.method()==='POST'&&new URL(request.url()).pathname===base+'/comments')posts.push(request.postDataJSON());});
  await page.locator('#prompt').fill('Synthetic page-only message '+mode);const response=page.waitForResponse(result=>result.request().method()==='POST'&&new URL(result.url()).pathname===base+'/comments');await page.locator('#sendbtn').click();assert.ok((await response).ok());assert.match(await page.locator('#toast').innerText(),/retry is available only while this page remains open/i);await until(async()=>(await page.locator('#prompt').innerText())==='',30000,'confirmed page-only input cleared');
  const entries=(await readerSeat.api(base+'/comments?after=0')).entries.filter(row=>row.text==='Synthetic page-only message '+mode);assert.equal(entries.length,1);assert.equal(posts.length,1);assertID('people-no-storage-text-send-'+mode,(await page.locator('#prompt').innerText())==='');
  for(const edit of ['same','different']){
    const original='Synthetic page-only uncertain '+edit+' '+mode,newer=edit==='same'?original:'Synthetic newer unsaved '+mode;let ready,release,first=true;const reached=new Promise(resolve=>{ready=resolve;}),gate=new Promise(resolve=>{release=resolve;});heldReleases.add(release);const start=posts.length;
    await page.route('**/api/team/subscriptions/**/comments**',async route=>{const request=route.request();if(request.method()!=='POST'||new URL(request.url()).pathname!==base+'/comments'||!first){await route.continue();return;}first=false;const actual=await route.fetch({maxRetries:0});ready(actual.status());await gate;await route.abort('failed');});
    try{await page.locator('#prompt').fill(original);const dialog=page.waitForEvent('dialog',{timeout:30000});dialog.catch(()=>{});await page.locator('#sendbtn').click();assert.equal(await reached,200);await page.locator('#prompt').fill(newer);release();await (await dialog).accept();assert.equal(await page.locator('#prompt').innerText(),newer);await page.locator('#prompt').fill(original);const retry=page.waitForResponse(result=>result.request().method()==='POST'&&new URL(result.url()).pathname===base+'/comments');await page.locator('#sendbtn').click();assert.ok((await retry).ok());await until(async()=>posts.length===start+2,30000,'explicit page-only retry');assert.deepEqual(posts[start+1],posts[start]);const matching=(await readerSeat.api(base+'/comments?after=0')).entries.filter(row=>row.text===original);assert.equal(matching.length,1);assertID('people-no-storage-text-uncertain-'+edit+'-'+mode,posts.length===start+2&&matching.length===1);}finally{release();heldReleases.delete(release);await page.unrouteAll({behavior:'wait'});}
  }
  assert.deepEqual(counts(ownerSeat),before.owner);assert.deepEqual(counts(readerSeat),before.recipient);assertID('people-no-storage-text-zero-agent-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));manifest['people-no-storage-text-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};await screenshot(page,'people-no-storage-text-'+mode+'.png');
}

async function peopleUncertainRetry(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','People uncertain '+mode,'Synthetic uncertain setup');
  const sharePath='/api/team/shares/'+chat.id;
  await ownerSeat.api(sharePath,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});
  const share=await ownerSeat.api(sharePath);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),40000,'uncertain subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),path='/api/team/subscriptions/'+key+'/comments';
  const page=await pageFor(readerSeat,mode,'people-uncertain-'+mode);await open(page,'team/'+key);await page.locator('#attadd').waitFor({state:'visible',timeout:30000});
  const before={owner:counts(ownerSeat),recipient:counts(readerSeat)},bodies=[];let forwarded=0,confirmedStatus=0;
  await page.route('**/api/team/subscriptions/**/comments?*',async route=>{
    const req=route.request();if(req.method()!=='POST'||new URL(req.url()).pathname!==path){await route.continue();return;}
    bodies.push(req.postDataJSON());if(forwarded++===0){const response=await route.fetch();confirmedStatus=response.status();await route.abort('failed');}else await route.continue();
  });
  try {
    await page.locator('#prompt').fill('Synthetic uncertain caption '+mode);
    await page.locator('#attfile').setInputFiles({name:'uncertain.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic uncertain bytes')});
    await page.locator('#attstage .stagechip.done[title="uncertain.txt"]').waitFor({timeout:30000});
    const dialog=page.waitForEvent('dialog');const send=page.locator('#sendbtn').click();await (await dialog).accept();await send;
    await page.locator('#attstage details summary').getByText('Review unconfirmed People message').waitFor({timeout:30000});
    assertID('people-uncertain-accepted-server-'+mode,confirmedStatus===200&&bodies.length===1);
    await page.locator('#prompt').fill('Newer People draft '+mode);
    await until(async()=>page.evaluate(async text=>{const db=await new Promise((resolve,reject)=>{const request=indexedDB.open('scimux-people-file-drafts',2);request.onsuccess=()=>resolve(request.result);request.onerror=()=>reject(request.error);});try{const rows=await new Promise((resolve,reject)=>{const request=db.transaction('recovery','readonly').objectStore('recovery').getAll();request.onsuccess=()=>resolve(request.result);request.onerror=()=>reject(request.error);});return rows.some(head=>head.kind==='head'&&!head.sealed&&rows.some(row=>row.kind==='draft'&&row.version===head.version&&row.text===text));}finally{db.close();}},'Newer People draft '+mode),30000,'durable newer People draft');
    await page.reload({waitUntil:'networkidle'});await page.locator('#attstage details summary').getByText('Review unconfirmed People message').waitFor({timeout:40000});
    await until(async()=>(await page.locator('#prompt').innerText())==='Newer People draft '+mode,30000,'reloaded People draft');
    assertID('people-uncertain-reload-keeps-draft-'+mode,(await page.locator('#prompt').innerText())==='Newer People draft '+mode);
    await page.locator('#attstage details summary').getByText('Review unconfirmed People message').click();await page.locator('.people-retry').waitFor({timeout:30000});
    await page.locator('.people-retry').click();
    await until(()=>bodies.length===2,30000,'explicit pending retry');
    await page.locator('.people-retry').waitFor({state:'detached',timeout:40000});
    const bubble=page.locator('#msgs .gmsg').filter({hasText:'Synthetic uncertain caption '+mode});await bubble.first().waitFor({timeout:40000});
    assertID('people-uncertain-exact-retry-'+mode,JSON.stringify(bodies[0])===JSON.stringify(bodies[1])&&await bubble.count()===1&&(await page.locator('#prompt').innerText())==='Newer People draft '+mode);
    assertID('people-uncertain-no-agent-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
    await screenshot(page,'people-uncertain-retry-'+mode+'.png');manifest['people-uncertain-retry-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)},forwarded,confirmedStatus};
  } finally {await page.unrouteAll({behavior:'wait'});}
}

async function peoplePermissionDuringUpload(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','People upload permission '+mode,'Synthetic permission setup');
  const sharePath='/api/team/shares/'+chat.id,grant=access=>({server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access}]});
  await ownerSeat.api(sharePath,'PUT',grant('write'));const share=await ownerSeat.api(sharePath);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),40000,'upload permission subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),base='/api/team/subscriptions/'+key,page=await pageFor(readerSeat,mode,'people-upload-permission-'+mode);
  await open(page,'team/'+key);await page.locator('#attadd').waitFor({state:'visible',timeout:30000});
  const before={owner:counts(ownerSeat),recipient:counts(readerSeat)};
  await page.locator('#prompt').fill('Synthetic partial upload draft '+mode);
  await page.locator('#attfile').setInputFiles({name:'first.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic first')});
  await page.locator('#attstage .stagechip.done[title="first.txt"]').waitFor({timeout:30000});
  let invalidRequests=0;const countInvalid=req=>{if(req.method()==='POST'&&new URL(req.url()).pathname===base+'/attachments')invalidRequests++;};page.on('request',countInvalid);
  const emptyDialog=page.waitForEvent('dialog');const emptyInput=page.locator('#attfile').setInputFiles({name:'empty.txt',mimeType:'text/plain',buffer:Buffer.alloc(0)});const emptyNotice=await emptyDialog;
  assertID('people-empty-file-feedback-'+mode,/non-empty/i.test(emptyNotice.message()));await emptyNotice.accept();await emptyInput;page.off('request',countInvalid);
  assertID('people-empty-file-keeps-draft-'+mode,invalidRequests===0&&(await page.locator('#prompt').innerText())==='Synthetic partial upload draft '+mode&&await page.locator('#attstage .stagechip.done[title="first.txt"]').count()===1);
  let release,entered;const reached=new Promise(resolve=>{entered=resolve}),hold=new Promise(resolve=>{release=resolve});heldReleases.add(release);
  await page.route('**'+base+'/attachments?*',async route=>{entered();await hold;if(!page.isClosed())await route.continue();});
  try {
    await page.locator('#attfile').setInputFiles({name:'second.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic second')});await Promise.race([reached,delay(15000).then(()=>{throw Error('second upload not held')})]);
    await ownerSeat.api(sharePath,'PUT',grant('read'));
    await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.some(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='read'),30000,'permission changed during upload');
    const failed=page.waitForResponse(response=>response.request().method()==='POST'&&new URL(response.url()).pathname===base+'/attachments',{timeout:30000});release();heldReleases.delete(release);const response=await failed;
    assertID('people-held-upload-permission-denied-'+mode,response.status()===403);
    await page.locator('#gueststate').getByText(/Can view/).waitFor({timeout:30000});
    assertID('people-partial-upload-keeps-draft-'+mode,(await page.locator('#prompt').innerText())==='Synthetic partial upload draft '+mode&&await page.locator('#attstage .stagechip[title="first.txt"]').count()===1);
    assertID('people-held-upload-no-agent-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
    await screenshot(page,'people-upload-permission-'+mode+'.png');manifest['people-upload-permission-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};
  } finally {release();heldReleases.delete(release);await page.unrouteAll({behavior:'wait'});}
}

async function peoplePermissionMidBody(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','People midbody permission '+mode,'Synthetic permission setup');
  const sharePath='/api/team/shares/'+chat.id,grant=access=>({server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access}]});
  await ownerSeat.api(sharePath,'PUT',grant('write'));const share=await ownerSeat.api(sharePath);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),40000,'midbody permission subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),base='/api/team/subscriptions/'+key,page=await pageFor(readerSeat,mode,'people-midbody-permission-'+mode);
  await open(page,'team/'+key);await page.locator('#attadd').waitFor({state:'visible',timeout:30000});
  const before={owner:counts(ownerSeat),recipient:counts(readerSeat)};
  await page.locator('#prompt').fill('Synthetic midbody draft '+mode);
  const earlierRequest=page.waitForRequest(req=>req.method()==='POST'&&new URL(req.url()).pathname===base+'/attachments');
  await page.locator('#attfile').setInputFiles({name:'earlier.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic earlier')});
  const earlierID=new URL((await earlierRequest).url()).searchParams.get('stage_id');
  await page.locator('#attstage .stagechip.done[title="earlier.txt"]').waitFor({timeout:30000});
  let release,entered;const reached=new Promise(resolve=>{entered=resolve}),hold=new Promise(resolve=>{release=resolve});heldReleases.add(release);
  let forwarded=0,continued=0,firstBytes=0,upstream,negativeRequest,stageID,forwardFailure,retiring=false,heldBody,heldHeaders,heldPath;
  // This locator belongs to the isolated seat. Never record its capability.
  const locatorPath=path.join(readerSeat.data,'muxer.json'),locatorInfo=fs.lstatSync(locatorPath);
  assert.ok(locatorInfo.isFile()&&(locatorInfo.mode&0o077)===0,'private test-owned muxer locator');
  const link=JSON.parse(fs.readFileSync(locatorPath,'utf8'));
  assert.ok(path.isAbsolute(link.socket)&&typeof link.token==='string'&&link.token.length>0,'test-owned muxer capability');
  await page.route('**'+base+'/attachments?*',async route=>{
    try {
    const request=route.request(),body=request.postDataBuffer(),headers=await request.allHeaders();
    stageID=new URL(request.url()).searchParams.get('stage_id');
    delete headers.host;delete headers['transfer-encoding'];headers['content-length']=String(body.length);headers.expect='100-continue';
    headers['x-scimux-backend-token']=link.token;headers['x-scimux-backend-major']='1';headers['x-scimux-backend-minor']='0';
    heldBody=body;heldHeaders=headers;heldPath=new URL(request.url()).pathname+new URL(request.url()).search;
    await new Promise((resolve,reject)=>{
      upstream=http.request({socketPath:link.socket,path:heldPath,method:'POST',headers},response=>{
        const chunks=[];response.on('data',chunk=>chunks.push(chunk));response.on('end',()=>{
          route.fulfill({status:response.statusCode,headers:response.headers,body:Buffer.concat(chunks)}).then(resolve,reject);
        });response.on('error',reject);
      });
      upstream.setTimeout(30000,()=>upstream.destroy(Error('midbody forward timeout')));
      upstream.on('error',reject);upstream.on('continue',async()=>{
        if(process.env.SCIMUX_E2E_MIDBODY_PROBE==='forward-error'){upstream.destroy(Error('synthetic forward failure'));return;}
        continued++;const split=Math.min(1024,body.length-1);firstBytes=split;
        upstream.write(body.subarray(0,split),async err=>{if(err){reject(err);return;}entered();await hold;if(!upstream.destroyed)upstream.end(body.subarray(split));});
      });
      forwarded++;upstream.flushHeaders();
    });} catch(err) {
      if(!retiring&&!shutting){forwardFailure=err;entered();}
      await route.abort().catch(()=>{});
    }
  });
  try {
    await page.locator('#attfile').setInputFiles({name:'midbody.txt',mimeType:'text/plain',buffer:Buffer.alloc(128*1024,65)});
    await Promise.race([reached,delay(15000).then(()=>{throw Error('upload body read not reached')})]);
    if(forwardFailure)throw forwardFailure;
    assertID('people-midbody-admitted-'+mode,forwarded===1&&continued===1&&firstBytes>0);
    // A direct muxer 100 is emitted by Go's server on the first Body.Read,
    // which the attachment handler reaches only after admission.
    assertID('people-midbody-muxer-body-read-'+mode,continued===1&&firstBytes>0&&firstBytes<heldBody.length);
    if(process.env.SCIMUX_E2E_MIDBODY_PROBE==='failure')throw Error('synthetic midbody cleanup failure');
    if(process.env.SCIMUX_E2E_MIDBODY_PROBE==='signal') {
      process.kill(process.pid,'SIGTERM');
      await delay(30000);
      throw Error('midbody signal cleanup did not terminate');
    }
    await ownerSeat.api(sharePath,'PUT',grant('read'));
    const downgraded=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='read'),30000,'permission changed during body');
    assertID('people-midbody-same-live-copy-'+mode,downgraded.state==='live'&&downgraded.people_copy_instance===sub.people_copy_instance);
    let refusedContinue=0;
    const refusedStatus=await new Promise((resolve,reject)=>{
      const request=http.request({socketPath:link.socket,path:heldPath,method:'POST',headers:{...heldHeaders,connection:'close'}},response=>{
        response.resume();response.on('end',()=>resolve(response.statusCode));response.on('error',reject);
      });
      negativeRequest=request;
      request.setTimeout(15000,()=>request.destroy(Error('refused admission timeout')));
      request.on('continue',()=>{refusedContinue++;});request.on('error',reject);
      request.flushHeaders();request.write(heldBody.subarray(0,firstBytes));
    });
    assertID('people-midbody-refused-before-body-'+mode,refusedStatus===403&&refusedContinue===0);
    const frontHeaders={...heldHeaders,connection:'close'};
    delete frontHeaders['x-scimux-backend-token'];delete frontHeaders['x-scimux-backend-major'];delete frontHeaders['x-scimux-backend-minor'];
    let frontContinue=0,frontBytes=0;
    const frontStatus=await new Promise((resolve,reject)=>{
      negativeRequest=http.request(readerSeat.base+heldPath,{method:'POST',headers:frontHeaders},response=>{
        response.resume();response.on('end',()=>resolve(response.statusCode));response.on('error',reject);
      });
      negativeRequest.setTimeout(15000,()=>negativeRequest.destroy(Error('frontend refused admission timeout')));
      negativeRequest.on('continue',()=>{frontContinue++;frontBytes=firstBytes;negativeRequest.write(heldBody.subarray(0,firstBytes),err=>{if(err)reject(err);else negativeRequest.end(heldBody.subarray(firstBytes));});});
      negativeRequest.on('error',reject);negativeRequest.flushHeaders();
    });
    manifest['people-midbody-negative-'+mode]={frontContinue,frontBytes,frontStatus,refusedContinue,refusedStatus};
    assertID('people-midbody-frontend-refused-before-body-'+mode,frontContinue===0&&frontBytes===0&&frontStatus===403&&refusedContinue===0);
    const failed=page.waitForResponse(response=>response.request().method()==='POST'&&new URL(response.url()).pathname===base+'/attachments',{timeout:30000});release();heldReleases.delete(release);const response=await failed;
    assertID('people-midbody-rejected-'+mode,response.status()===409);
    const refusal=await response.json();assertID('people-midbody-not-active-'+mode,refusal?.error==='not_active');
    await page.locator('#gueststate').getByText(/Can view/).waitFor({timeout:30000});
    assertID('people-midbody-keeps-draft-'+mode,(await page.locator('#prompt').innerText())==='Synthetic midbody draft '+mode&&await page.locator('#attstage .stagechip[title="earlier.txt"]').count()===1&&await page.locator('#attstage .stagechip.done[title="midbody.txt"]').count()===0);
    await page.unrouteAll({behavior:'wait'});
    await ownerSeat.api(sharePath,'PUT',grant('write'));
    await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.some(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),30000,'write access restored after midbody');
    const missing=await fetch(readerSeat.base+base+'/attachments/stage/'+stageID+'?copy_instance='+encodeURIComponent(sub.people_copy_instance),{headers:{Origin:readerSeat.base,'X-Scimux-CSRF':readerSeat.csrf}});
    const earlier=await readerSeat.api(base+'/attachments/stage/'+earlierID+'?copy_instance='+encodeURIComponent(sub.people_copy_instance));
    const comments=await readerSeat.api(base+'/comments?after=0');
    assertID('people-midbody-no-stage-'+mode,/^[0-9a-f]{16}$/.test(stageID)&&missing.status===404&&earlier.id===earlierID);
    assertID('people-midbody-no-publication-'+mode,comments.entries.length===0);
    await page.locator('#attadd').waitFor({state:'visible',timeout:30000});
    await page.locator('#attstage .stagechip[title="midbody.txt"] .sx').click();
    await page.locator('#attfile').setInputFiles({name:'midbody.txt',mimeType:'text/plain',buffer:Buffer.alloc(128*1024,65)});
    await page.locator('#attstage .stagechip.done[title="midbody.txt"]').waitFor({timeout:30000});
    assertID('people-midbody-explicit-recovery-'+mode,(await page.locator('#prompt').innerText())==='Synthetic midbody draft '+mode&&await page.locator('#attstage .stagechip[title="earlier.txt"]').count()===1);
    assertID('people-midbody-no-agent-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
    await screenshot(page,'people-midbody-permission-'+mode+'.png');manifest['people-midbody-permission-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)},forwarded,continued,firstBytes,muxerContinue:continued,refusedContinue,refusedStatus,frontContinue,frontBytes,frontStatus,continueAloneProvesMuxerAdmission:false};
  } finally {retiring=true;release();heldReleases.delete(release);if(upstream&&!upstream.destroyed)upstream.destroy();if(negativeRequest&&!negativeRequest.destroyed)negativeRequest.destroy();await page.unrouteAll({behavior:'wait'});}
}

async function peopleImageBookmark(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','People image bookmark '+mode,'Synthetic bookmark setup');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});const share=await ownerSeat.api('/api/team/shares/'+chat.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),40000,'image bookmark subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),page=await pageFor(readerSeat,mode,'people-image-bookmark-'+mode),caption='Synthetic bookmarked image '+mode;
  await open(page,'team/'+key);await page.locator('#attadd').waitFor({state:'visible',timeout:30000});const before={owner:counts(ownerSeat),recipient:counts(readerSeat)};
  const png=Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAEUlEQVR4nGP4z8DwH4QZYAwAR8oH+WdZbrcAAAAASUVORK5CYII=','base64');
  await page.locator('#prompt').fill(caption);await page.locator('#attfile').setInputFiles({name:'bookmarked.png',mimeType:'image/png',buffer:png});await page.locator('#attstage .stagechip.done[title="bookmarked.png"]').waitFor({timeout:30000});await page.locator('#sendbtn').click();
  const bubble=page.locator('#msgs .gmsg').filter({hasText:caption}).last();await bubble.waitFor({timeout:40000});const seq=Number(await bubble.getAttribute('data-cseq'));
  await bubble.locator('.bubble').click({position:{x:5,y:5}});await page.locator('[data-gact=bookmark]').click();
  const saved=await until(async()=>{const ui=await readerSeat.api('/api/ui');return (ui.bookmarks||[]).find(item=>item.team?.server===sub.server&&item.team?.owner===sub.owner&&item.team?.chat===sub.chat&&item.team?.generation===sub.generation&&item.team?.log==='comments'&&item.team?.seq===seq&&item.media?.items?.length);},40000,'image bookmark capture');
  assertID('people-image-bookmark-exact-source-'+mode,saved.media.items.some(item=>item.state==='ready'));
  await page.locator('#bookmarksbtn').click();const bookmark=page.locator('#bookmarklist .bookmark').filter({hasText:caption}).last();await bookmark.locator('.nbubble').click();await bookmark.locator('[data-bmact=jump]').click();await page.locator('#msgs [data-cseq="'+seq+'"].jumpflash').waitFor({timeout:30000});
  assertID('people-image-bookmark-jump-'+mode,await page.locator('#msgs [data-cseq="'+seq+'"].jumpflash').count()===1);
  if((await page.locator('#bookmarksbtn').getAttribute('aria-label'))==='open bookmarks')await page.locator('#bookmarksbtn').click();
  if(!await bookmark.locator('[data-bmact=comment]').isVisible())await bookmark.locator('.nbubble').click();await bookmark.locator('[data-bmact=comment]').click();await page.locator('#bookmarkprompt').fill('Synthetic local image annotation '+mode);await page.locator('#bookmarksend').click();
  const local=await until(async()=>(await readerSeat.api('/api/ui')).bookmarks?.find(item=>item.anchor===saved.t&&item.text==='Synthetic local image annotation '+mode),30000,'image local annotation');
  const countBefore=(await readerSeat.api('/api/team/subscriptions/'+key+'/comments?after=0')).entries.length;assertID('people-image-bookmark-local-comment-'+mode,!!local&&countBefore===1);
  await readerSeat.api('/api/team/subscriptions/'+key,'DELETE');
  const item=saved.media.items.find(value=>value.state==='ready'),url='/api/reference-media/'+saved.media.capture_id+'/assets/'+item.id;
  const retained=await page.evaluate(async url=>{const response=await fetch(url);return {status:response.status,bytes:Array.from(new Uint8Array(await response.arrayBuffer()))}},url);
  assertID('people-image-bookmark-survives-copy-removal-'+mode,retained.status===200&&Buffer.from(retained.bytes).equals(png));
  await page.reload({waitUntil:'networkidle'});if((await page.locator('#bookmarksbtn').getAttribute('aria-label'))==='open bookmarks')await page.locator('#bookmarksbtn').click();
  const retainedBookmark=page.locator('#bookmarklist .bookmark').filter({hasText:caption}).last();await retainedBookmark.waitFor({timeout:30000});
  await until(async()=>retainedBookmark.locator('img').first().evaluate(img=>img.complete&&img.naturalWidth===2),10000,'retained bookmark image after reload');
  assertID('people-image-bookmark-reloads-after-copy-removal-'+mode,await retainedBookmark.locator('img').first().evaluate(img=>img.complete&&img.naturalWidth===2));
  assertID('people-image-bookmark-no-agent-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
  await screenshot(page,'people-image-bookmark-'+mode+'.png');manifest['people-image-bookmark-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};
}

async function peopleSourceCancellation(mode,ownerSeat,readerSeat) {
  const chat=await node(ownerSeat,'codex','People source lifetime '+mode,'Synthetic source lifetime setup');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:readerSeat.self,access:'write'}]});const share=await ownerSeat.api('/api/team/shares/'+chat.id);
  const sub=await until(async()=>(await readerSeat.api('/api/team/subscriptions')).subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation&&item.access==='write'),40000,'source lifetime subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),owner=await pageFor(ownerSeat,mode,'people-source-owner-'+mode),reader=await pageFor(readerSeat,mode,'people-source-reader-'+mode);
  await open(owner,chat.id);await open(reader,'team/'+key);await reader.locator('#attadd').waitFor({state:'visible',timeout:30000});const before={owner:counts(ownerSeat),recipient:counts(readerSeat)};
  await reader.locator('#prompt').fill('Synthetic source text '+mode);await reader.locator('#attfile').setInputFiles({name:'source.txt',mimeType:'text/plain',buffer:Buffer.from('identical synthetic bytes')});await reader.locator('#attstage .stagechip.done[title="source.txt"]').waitFor({timeout:30000});await reader.locator('#sendbtn').click();
  const source=owner.locator('#msgs .gmsg').filter({hasText:'Synthetic source text '+mode}).last();await source.waitFor({timeout:40000});const seq=Number(await source.getAttribute('data-cseq'));
  await source.locator('.bubble').click({position:{x:5,y:5}});await owner.locator('[data-gact=prompt]').click();await owner.locator('#destinationagent[aria-pressed=true]').waitFor({timeout:30000});await owner.locator('#attstage .stagechip.done[title="source.txt"]').waitFor({timeout:30000});
  await owner.locator('#prompt').fill('');await owner.locator('#attfile').setInputFiles({name:'unrelated.txt',mimeType:'text/plain',buffer:Buffer.from('identical synthetic bytes')});await owner.locator('#attstage .stagechip.done[title="unrelated.txt"]').waitFor({timeout:30000});
  assertID('people-source-files-never-auto-send-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
  await owner.locator('#attstage .stagechip[title="source.txt"] .sx').click();await owner.locator('#attstage .stagechip[title="source.txt"]').waitFor({state:'detached',timeout:30000});await owner.locator('#prompt').fill('Unrelated owner prompt '+mode);
  assertID('people-source-cancel-clears-prepared-association-'+mode,await owner.evaluate(id=>{const raw=localStorage.getItem('scimux-prompt-sources:'+id);return !raw||JSON.parse(raw).length===0},chat.id));
  const posted=owner.waitForRequest(req=>req.method()==='POST'&&new URL(req.url()).pathname==='/api/nodes/'+chat.id+'/send');await owner.locator('#sendbtn').click();const sent=(await posted).postDataJSON();await until(()=>counts(ownerSeat).accepted===before.owner.accepted+1,30000,'explicit unrelated owner send');
  await owner.locator('#msgs .turn.user .bubble').filter({hasText:'Unrelated owner prompt '+mode}).first().waitFor({timeout:40000});
  const delivered=jsonLines(path.join(ownerSeat.data,'counter')).at(-1)?.text||'';const links=(await ownerSeat.api('/api/ui')).forward_links||[];
  assertID('people-source-cancel-keeps-unrelated-file-'+mode,sent.attachments?.length===1&&delivered.includes(sent.attachments[0].path)&&fs.readFileSync(sent.attachments[0].path,'utf8')==='identical synthetic bytes'&&counts(ownerSeat).attempted===before.owner.attempted+1&&counts(ownerSeat).controls===before.owner.controls);
  assertID('people-source-cancel-no-false-provenance-'+mode,!links.some(item=>item.kind==='prompt'&&item.source?.server===sub.server&&item.source?.owner===sub.owner&&item.source?.chat===sub.chat&&item.source?.generation===sub.generation&&item.source?.seq===seq)&&JSON.stringify(counts(readerSeat))===JSON.stringify(before.recipient));
  await screenshot(owner,'people-source-cancel-'+mode+'.png');manifest['people-source-cancel-'+mode]={before,after:{owner:counts(ownerSeat),recipient:counts(readerSeat)}};
}

async function legacyReaderFiles(mode,ownerSeat) {
  const legacy=await makeSeat('legacy-reader-'+mode,false,legacyScimux);legacy.node=await node(legacy,'codex','Legacy reader '+mode,'Synthetic local legacy setup');
  await legacy.api('/api/settings','PUT',{experimental_feature_team:true});
  const issue=JSON.parse(execFileSync(relayBin,['-state-dir',relay.dir,'-url',relay.url,'-json','issue'],{env:baseEnv,encoding:'utf8'}));
  await legacy.api('/api/team/servers','POST',{link:issue.link,name:'Synthetic relay'});legacy.serverID=(await legacy.api('/api/team/servers')).servers[0].id;legacy.self=(await legacy.api('/api/team/servers/'+legacy.serverID+'/seats')).self;
  const chat=await node(ownerSeat,'codex','Legacy reader share '+mode,'Synthetic legacy share setup');
  await ownerSeat.api('/api/team/shares/'+chat.id,'PUT',{server:ownerSeat.serverID,scope:'all',members:[{seat:legacy.self,access:'write'}]});const share=await ownerSeat.api('/api/team/shares/'+chat.id);
  const sub=await until(async()=>(await legacy.api('/api/team/subscriptions')).subscriptions.find(item=>item.owner===ownerSeat.self&&item.chat===share.uid&&item.generation===share.generation),40000,'legacy reader subscription');
  const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),owner=await pageFor(ownerSeat,mode,'legacy-writer-'+mode),reader=await pageFor(legacy,mode,'legacy-reader-'+mode);
  await open(owner,chat.id);await open(reader,'team/'+key);await owner.locator('#msgs').getByText('Synthetic response 1',{exact:true}).waitFor({timeout:30000});
  await owner.locator('#destinationpeople[aria-pressed=true]').waitFor({timeout:30000});await owner.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
  const before={owner:counts(ownerSeat),legacy:counts(legacy)};
  await owner.locator('#attadd').waitFor({state:'visible',timeout:12000});
  for(const [caption,name] of [['Synthetic <caption> & file '+mode,'legacy-text-file.txt'],['','legacy-file-only.txt']]) {
    if(caption)await owner.locator('#prompt').fill(caption);
    await owner.locator('#attfile').setInputFiles({name,mimeType:'text/plain',buffer:Buffer.from('synthetic legacy file')});await owner.locator('#attstage .stagechip.done[title="'+name+'"]').waitFor({timeout:30000});
    await owner.locator('#sendbtn').click();
    const matching=reader.locator('#msgs .gmsg').filter({hasText:'[Attachments unavailable in this version of scimux.]'});await until(async()=>await matching.count()===(caption?1:2),40000,'legacy fallback message');
    const bubble=matching.last();if(caption)assertID('legacy-reader-original-text-'+mode,(await bubble.innerText()).includes(caption));
    else assertID('legacy-reader-file-only-visible-'+mode,(await bubble.innerText()).includes('[Attachments unavailable in this version of scimux.]'));
    assertID('legacy-reader-no-false-file-'+(caption?'text-':'only-')+mode,!(await bubble.innerText()).includes(name));
  }
  assertID('legacy-reader-zero-implicit-dispatch-'+mode,JSON.stringify(counts(ownerSeat))===JSON.stringify(before.owner)&&JSON.stringify(counts(legacy))===JSON.stringify(before.legacy));
  await screenshot(reader,'legacy-reader-'+mode+'.png');manifest['legacy-reader-'+mode]={before,after:{owner:counts(ownerSeat),legacy:counts(legacy)}};
}

async function oldRelayFiles(mode) {
  const old=await launchRelay(legacyRelayBin,'legacy-relay-'+mode);let blobPuts=0;
  const target=new URL(old.url);
  const proxy=http.createServer((req,res)=>{
    if(req.method==='PUT' && req.url.includes('/blobs/'))blobPuts++;
    const upstream=http.request({hostname:target.hostname,port:target.port,path:req.url,method:req.method,headers:{...req.headers,host:target.host}},response=>{res.writeHead(response.statusCode,response.headers);response.pipe(res)});
    upstream.on('error',()=>{if(!res.headersSent)res.writeHead(502);res.end()});req.pipe(upstream);
  });
  await new Promise((resolve,reject)=>proxy.once('error',reject).listen(0,'127.0.0.1',resolve));
  const stopProxy=()=>proxy.close();heldReleases.add(stopProxy);
  try {
    const relayURL='http://127.0.0.1:'+proxy.address().port,owner=await makeSeat('old-owner-'+mode),reader=await makeSeat('old-reader-'+mode);
    owner.node=await node(owner,'codex','Old relay owner '+mode,'Synthetic old relay owner');reader.node=await node(reader,'codex','Old relay reader '+mode,'Synthetic old relay reader');
    for(const seat of [owner,reader]) {
      const issue=JSON.parse(execFileSync(legacyRelayBin,['-state-dir',old.dir,'-url',relayURL,'-json','issue'],{env:baseEnv,encoding:'utf8'}));
      await seat.api('/api/team/servers','POST',{link:issue.link,name:'Synthetic old relay'});seat.serverID=(await seat.api('/api/team/servers')).servers[0].id;seat.self=(await seat.api('/api/team/servers/'+seat.serverID+'/seats')).self;
    }
    await owner.api('/api/team/shares/'+owner.node.id,'PUT',{server:owner.serverID,scope:'all',members:[{seat:reader.self,access:'write'}]});const share=await owner.api('/api/team/shares/'+owner.node.id);
    const sub=await until(async()=>(await reader.api('/api/team/subscriptions')).subscriptions.find(item=>item.owner===owner.self&&item.chat===share.uid&&item.generation===share.generation),40000,'old relay membership');
    const key=[sub.server,sub.owner,sub.chat,sub.generation].join('/'),page=await pageFor(reader,mode,'old-relay-reader-'+mode);await open(page,'team/'+key);
    await page.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});const before=counts(reader);
    const dialog=page.waitForEvent('dialog');const pasted=page.locator('#prompt').evaluate(el=>{const data=new DataTransfer();data.items.add(new File(['synthetic'],'unsupported.txt',{type:'text/plain'}));el.dispatchEvent(new ClipboardEvent('paste',{bubbles:true,cancelable:true,clipboardData:data}));});
    const warning=await dialog;assertID('old-relay-feedback-'+mode,/text only/i.test(warning.message()));await warning.accept();await pasted;
    const status=await page.evaluate(async key=>{const token=document.querySelector('meta[name="scimux-csrf"]').content,form=new FormData();form.append('files',new File(['synthetic'],'unsupported.txt',{type:'text/plain'}));return (await fetch('/api/team/subscriptions/'+key+'/attachments',{method:'POST',headers:{'X-Scimux-CSRF':token},body:form})).status},key);
    assertID('old-relay-no-blob-put-'+mode,status===409&&blobPuts===0&&JSON.stringify(counts(reader))===JSON.stringify(before));
    await screenshot(page,'old-relay-'+mode+'.png');manifest['old-relay-'+mode]={status,blobPuts,before,after:counts(reader)};
  } finally {heldReleases.delete(stopProxy);await new Promise(resolve=>proxy.close(resolve));}
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
 await page.locator('#prompt[contenteditable=true]').waitFor({timeout:30000});
 assertID('recovery-adopted-'+mode,(await page.locator('#prompt').innerText())===draft);
 assertID('recovery-no-panel-'+mode,await page.locator('#draftrecovery details').count()===0);
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
  await page.route(url=>url.pathname==='/api/team/subscriptions/'+key,async route=>{if(route.request().method()!=='DELETE'){await route.continue();return;}try{const response=await route.fetch();ready();await held;if(!page.isClosed())await route.fulfill({response});}catch(e){if(!released&&!shutting&&!page.isClosed())throw e;}});
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
async function lanesShare(owner, reader) {
  const minted = '#007AFF';
  const lane = 'protein';
  const kept = await node(owner, 'codex', 'Lane kept', 'Synthetic lane kept');
  const archived = await node(owner, 'codex', 'Lane archived', 'Synthetic lane archived');
  await owner.api('/api/nodes/' + kept.id, 'PATCH', {lane_id: lane});
  await owner.api('/api/nodes/' + archived.id, 'PATCH', {lane_id: lane});
  const ui = await owner.api('/api/ui');
  await owner.uiSetup({...ui, lanes: [...(ui.lanes || []), {id: lane, name: 'Protein', color: minted}], archived: [...(ui.archived || []), archived.id]});
  const share = await owner.api('/api/team/lane-shares/' + lane, 'PUT', {server: owner.serverID, scope: 'all', name: 'Protein', color: minted.toLowerCase(), excluded: [archived.id], members: [{seat: reader.self, access: 'read'}]});
  const listed = () => ownerShares(reader, owner.self);
  const index = await until(async () => (await listed()).find(view => view.lane && view.lane.name === 'Protein'), 30000, 'lane index');
  const keptSub = await until(async () => (await listed()).find(view => view.title === 'Lane kept'), 30000, 'kept lane chat');
  assertID('lanes-share-excludes-archived', index.chat === share.id && share.color === minted.toLowerCase() && share.excluded === 1 && !(await listed()).some(view => view.title === 'Lane archived'));
  const shown = {};
  for (const mode of ['desktop', 'phone']) {
    const page = await pageFor(reader, mode, 'lanes-' + mode);
    shown[mode] = page;
    const ownerKey = 'owner/' + keptSub.server + '/' + keptSub.owner + '/lane/' + index.chat;
    const view = await until(async () => page.evaluate(key => {
      const group = document.querySelector('#cardlist .lane-group');
      if (!group || group.getAttribute('data-activity-group') !== key) return null;
      const cards = [...document.querySelectorAll('#cardlist .card.lane-indent')];
      return {label: group.querySelector('.foldlabel')?.textContent, swatch: group.querySelector('.laneswatch')?.getAttribute('style') || '', titles: cards.map(card => card.querySelector('.title')?.textContent), stripes: cards.map(card => card.querySelector('.stripe')?.getAttribute('style') || ''), archived: [...document.querySelectorAll('#cardlist .title')].some(title => title.textContent === 'Lane archived')};
    }, ownerKey), 30000, 'lane sub-group ' + mode);
    assertID('lanes-subgroup-' + mode, view.label === 'Protein' && view.swatch === 'background:#007aff' && view.titles.length === 1 && view.titles[0] === 'Lane kept' && view.stripes[0] === 'background:#007aff' && !view.archived);
    await screenshot(page, 'lanes-' + mode + '.png');
  }
  const added = await node(owner, 'codex', 'Lane added', 'Synthetic lane added');
  await owner.api('/api/nodes/' + added.id, 'PATCH', {lane_id: lane});
  await until(async () => (await listed()).some(view => view.title === 'Lane added'), 30000, 'added lane chat');
  await owner.api('/api/team/lane-shares/' + lane, 'PUT', {server: owner.serverID, scope: 'all', name: 'Folded', color: minted.toLowerCase(), excluded: [archived.id], members: [{seat: reader.self, access: 'read'}]});
  for (const mode of ['desktop', 'phone']) {
    const titles = await until(async () => shown[mode].evaluate(() => {
      const group = document.querySelector('#cardlist .lane-group');
      const cards = [...document.querySelectorAll('#cardlist .card.lane-indent .title')].map(title => title.textContent);
      if (!group || group.querySelector('.foldlabel')?.textContent !== 'Folded' || cards.length !== 2) return null;
      return cards;
    }), 30000, 'renamed lane ' + mode);
    assertID('lanes-renamed-added-' + mode, titles.includes('Lane kept') && titles.includes('Lane added'));
  }
  const page = shown.desktop;
  await open(page, 'team/' + [keptSub.server, keptSub.owner, keptSub.chat, keptSub.generation].join('/'));
  const button = page.locator('#guestremove');
  await button.waitFor({timeout: 30000});
  assertID('lanes-leave-label', (await button.getAttribute('aria-label')) === 'Leave lane');
  const relayBefore = await indexSeats(owner, share.id);
  assertID('lanes-relay-lists-recipient', relayBefore.includes(reader.self));
  page.once('dialog', dialog => dialog.accept());
  await button.click();
  await until(async () => (await listed()).length === 0, 30000, 'lane copies removed');
  await until(async () => (await page.locator('#cardlist .lane-group').count()) === 0, 30000, 'desktop lane cleared');
  await until(async () => (await shown.phone.locator('#cardlist .lane-group').count()) === 0, 30000, 'phone lane cleared');
  assertID('lanes-left', (await page.locator('#cardlist .lane-group').count()) === 0 && (await shown.phone.locator('#cardlist .lane-group').count()) === 0);
  // The owner rechecks lane membership a minute after checked_at, and no hook shortens that wait.
  const relayAfter = await indexSeats(owner, share.id);
  assertID('lanes-relay-index-without-recipient', !relayAfter.includes(reader.self));
  manifest['lanes-desktop'] = {before: counts(owner), after: counts(owner)};
  manifest['lanes-phone'] = {before: counts(reader), after: counts(reader)};
}
function ownerShares(reader, ownerID) {
  return reader.api('/api/team/subscriptions').then(body => body.subscriptions.filter(view => view.owner === ownerID));
}
async function indexSeats(ownerSeat, indexID) {
  const doc = JSON.parse(fs.readFileSync(path.join(ownerSeat.data, 'team.json'), 'utf8'));
  const token = doc.servers.find(item => item.id === ownerSeat.serverID).token;
  const response = await fetch(relay.url + '/v1/channels', {headers: {Accept: 'application/json', Authorization: 'Bearer ' + token}});
  if (!response.ok) throw Error('relay channels ' + response.status);
  const body = await response.json();
  const channel = (body.channels || []).find(item => item.chat === indexID && item.owner === ownerSeat.self);
  if (!channel) throw Error('relay index missing');
  return (channel.members || []).map(item => item.seat);
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
  let failedDelete=0;
  const deleteRoute=url=>url.pathname==='/api/team/subscriptions/'+key;
  await page.route(deleteRoute,route=>{if(route.request().method()==='DELETE'){failedDelete++;return route.abort('failed');}return route.continue();});
  page.once('dialog',dialog=>dialog.accept());await button.click();await page.locator('#guestnote').getByText('That did not work. Try again.').waitFor({timeout:30000});
  assertID('leave-error-retains-copy-'+mode,failedDelete===1&&await button.isEnabled()&&await button.evaluate(el=>document.activeElement===el)&&(await readerSeat.api('/api/team/subscriptions')).subscriptions.some(x=>x.chat===share.uid));
  await page.unroute(deleteRoute);
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
  await owner.locator('#attstage').waitFor({state:'hidden',timeout:30000});
  assertID('trace-agent-attachment-hidden-people-'+mode,!(await owner.locator('#attstage').isVisible())&&!(await owner.locator('#attstage').innerText()).includes('synthetic-note.txt'));await owner.locator('#destinationagent').click();
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
  const snapshot=pageSource.get(page);
  assert.ok(snapshot,'browser page has a source snapshot');
  for(const entry of await page.coverage.stopJSCoverage()) {
      const verified=verifiedCoverageEntry(entry,label,snapshot.dir,snapshot.version);
      if(verified)coverageArchive.push(verified);
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
  const diagnostics=await diagnosticBudget.finish();
  if(diagnostics.failed){manifest.cleanup.push({fatal:'diagnostic capture failed'});manifest.result='fail';process.exitCode=1;}
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
    let native=summarizeNative(pids);
    for(let i=0;i<50 && native.active;i++){await delay(100);native=summarizeNative(pids);}
    const remainingNativeHelpers=native.active;
    manifest.cleanup.push({seat:s.name,remainingNativeHelpers,remainingNativeZombies:native.zombies});
    if(remainingNativeHelpers){manifest.result='fail';process.exitCode=1;}
  }
  record();
}
const cleanup=createCleanupOnce(async()=>{try{await performCleanup();}catch(e){manifest.result='fail';manifest.cleanup.push({fatal:String(e)});process.exitCode=1;record();throw e;}});
// Opt-in probe only: keep the existing overlap gate blocked until the test releases it.
const holdCleanupProbe = process.env.SCIMUX_E2E_CLEANUP_PROBE === 'overlap' && process.env.SCIMUX_E2E_HOLD_CLEANUP === '1';
if (holdCleanupProbe) process.stdin.once('data',()=>cleanupProbeRelease?.());
installRunnerSignals({events:process,manifest,cleanup,record,exit:code=>process.exit(code),notify:message=>{if(holdCleanupProbe&&message==='received')console.log('Cleanup signal received');},release:()=>{signalCode=manifest.signal==='SIGINT'?130:143;if(!holdCleanupProbe)cleanupProbeRelease?.();captureProbeRelease?.();}});

async function run() {
 const selected=process.env.SCIMUX_E2E_SCENARIO||'default';
 if(!scenarioNames.includes(selected))throw Error('unknown SCIMUX_E2E_SCENARIO');
 if(selected!=='default')manifest.scope=selected+' scenario only';
 relay=await launchRelay();
 for(const name of ['owner-a','owner-b','recipient','observer']) await makeSeat(name,false,name==='owner-b'&&legacyScimux?legacyScimux:scimux);
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
  if(process.env.SCIMUX_E2E_CLEANUP_PROBE==='failure')throw controlledNativeCleanupFailure;
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
  assertID('no-server-icon-hidden-'+mode,!(await p.locator('#sharebtn').isVisible()));
  const issue=JSON.parse(execFileSync(relayBin,['-state-dir',relay.dir,'-url',relay.url,'-json','issue'],{env:baseEnv,encoding:'utf8'}));
  await p.locator('#burger').click();await p.locator('#m_sharing').waitFor({state:'visible',timeout:15000});assertID('sharing-menu-before-join-'+mode,await p.locator('#m_sharing').isVisible());await p.locator('#m_sharing summary').click();await p.locator('#m_team_add').click();
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
 for(const s of selected==='default'?[r,observer]:seats){const issue=JSON.parse(execFileSync(relayBin,['-state-dir',relay.dir,'-url',relay.url,'-json','issue'],{env:baseEnv,encoding:'utf8'}));await s.api('/api/team/servers','POST',{link:issue.link,name:selected==='received-offline'&&s===a?'Synthetic sharer':s.name});}
 for(const s of seats){s.serverID=(await s.api('/api/team/servers')).servers[0].id;s.self=(await s.api('/api/team/servers/'+s.serverID+'/seats')).self;}
 if(selected==='lanes'){await lanesShare(a,r);return;}
 for(const s of [a,b])await s.api('/api/team/shares/'+s.node.id,'PUT',{server:s.serverID,scope:'all',members:[{seat:r.self,access:'write'},...(s===b?[{seat:observer.self,access:'write'}]:[])]});
 if(selected==='people-source-cancel'){for(const mode of ['desktop','phone'])await peopleSourceCancellation(mode,a,r);return;}
 if(selected==='people-image-bookmark'){for(const mode of ['desktop','phone'])await peopleImageBookmark(mode,a,r);return;}
 if(selected==='people-upload-permission'){for(const mode of ['desktop','phone'])await peoplePermissionDuringUpload(mode,a,r);return;}
 if(selected==='people-midbody-permission'){for(const mode of ['desktop','phone'])await peoplePermissionMidBody(mode,a,r);return;}
 if(selected==='people-stage-competition'){for(const mode of ['desktop','phone'])await peopleStageCompetition(mode,a,r);return;}
 if(selected==='people-pending-layout'){for(const ending of [false,true])for(const mode of ['phone','desktop'])await peoplePendingPhoneReachability(mode,a,r,ending);return;}
 if(selected==='people-local-stage-review'){for(const mode of ['desktop','phone'])await peopleLocalStageReview(mode,a,r);return;}
 if(selected==='people-stale-retry'){for(const mode of ['desktop','phone'])await peopleStaleRetry(mode,a,b,r,observer);return;}
 if(selected==='people-captured-discard'){for(const mode of ['desktop','phone'])await peopleCapturedDiscard(mode,a,r);return;}
 if(selected==='people-text-retirement'){for(const variant of ['readonly','owner-ended-agent'])for(const mode of ['desktop','phone'])await peopleTextRetirement(mode,a,b,r,observer,variant);return;}
 if(selected==='people-transactional-drafts'){for(const mode of ['desktop','phone'])await peopleTransactionalDrafts(mode,a,r);return;}
 if(selected==='people-no-storage-text'){for(const mode of ['desktop','phone'])await peopleNoStorageText(mode,a,r);return;}
 if(selected==='people-transactional-retirement'){for(const mode of ['desktop','phone'])await peopleTransactionalRetirement(mode,a,r);return;}
 if(selected==='people-uncertain-retry'){for(const mode of ['desktop','phone'])await peopleUncertainRetry(mode,a,r);return;}
 if(selected==='people-files'){for(const mode of ['desktop','phone'])await peopleFiles(mode,a,r);return;}
 if(selected==='people-quota'){for(const mode of ['desktop','phone'])await peopleQuota(mode,a,r);return;}
 if(selected==='people-local-budget'){for(const mode of ['desktop','phone'])await peopleLocalBudget(mode,a,r);return;}
 if(selected==='people-kept-full'){for(const mode of ['desktop','phone'])await peopleKeptFull(mode,a,r);return;}
 if(selected==='people-unkept-open'){for(const mode of ['desktop','phone'])await peopleUnkeptOpen(mode,a,r);return;}
 if(selected==='people-file-unread'){for(const mode of ['desktop','phone'])await peopleFileUnread(mode,a,r);return;}
 if(selected==='people-attachment-recovery'){for(const mode of ['desktop','phone'])await peopleAttachmentRecovery(mode,a,r);return;}
 if(selected==='people-relay-blob-recovery'){for(const mode of ['desktop','phone'])for(const kind of ['missing','corrupt'])await peopleRelayBlobRecovery(mode,kind,a,r);return;}
 if(selected==='people-cross-channel-forgery'){for(const mode of ['desktop','phone'])await peopleCrossChannelForgery(mode,a,b,r);return;}
 if(selected==='people-retained-unavailable'){for(const mode of ['desktop','phone'])await peopleRetainedUnavailable(mode,a,r);return;}
 if(selected==='people-retired-drafts'){for(const mode of ['desktop','phone'])await peopleRetiredDrafts(mode,a,r);return;}
 if(selected==='people-copy-replacement'){for(const mode of ['desktop','phone'])await peopleCopyReplacement(mode,a,r);return;}
 if(selected==='people-stale-preparation'){for(const mode of ['desktop','phone'])await peopleStalePreparation(mode,a,r);return;}
 if(selected==='people-real-tunnel'){for(const mode of ['desktop','phone'])await peopleRealTunnel(mode,a,r);return;}
 if(selected==='legacy-reader'){for(const mode of ['desktop','phone'])await legacyReaderFiles(mode,a);return;}
 if(selected==='old-relay'){for(const mode of ['desktop','phone'])await oldRelayFiles(mode);return;}
 if(selected==='stage1-scanner'){for(const mode of ['desktop','phone'])await stage1Scanner(mode,a,r);return;}
 if(selected==='stage1-unread'){for(const mode of ['desktop','phone'])await stage1Unread(mode,a,r);return;}
 if(selected==='stage1-owner-files'){for(const mode of ['desktop','phone'])await stage1OwnerFiles(mode,a,r);return;}
 if(selected==='stage2-shared-actions'){await r.api('/api/nodes/'+r.node.id,'PATCH',{title:'Synthetic renamed recipient'});for(const mode of ['desktop','phone'])await stage2SharedActions(mode,a,b,r,observer);return;}
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
   await rp.route('**/api/team/**', route=>route.fulfill({status:404,contentType:'application/json',body:'{\"error\":\"not_found\"}'}));await off;
   await rp.waitForFunction(()=>document.querySelector('#prompt').getAttribute('contenteditable')!=='true');
   assertID('recovery-disabled-draft-'+mode,(await rp.locator('#prompt').innerText())===recoveryDraft);
   const on=rp.waitForResponse(x=>x.url().endsWith('/api/team/subscriptions')&&x.status()===200,{timeout:20000});
   await rp.unroute('**/api/team/**');await on;
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
  } finally {releaseOld();releaseNew();heldReleases.delete(releaseOld);heldReleases.delete(releaseNew);await rp.unroute('**/api/team/**');await rp.unroute('**/api/team/subscriptions/*/*/*/*?after=*');}
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
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleFiles(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleUncertainRetry(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleStageCompetition(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleLocalStageReview(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleTransactionalDrafts(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleCapturedDiscard(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleStaleRetry(mode,a,b,r,observer));
 for(const variant of ['readonly','owner-ended-agent'])for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleTextRetirement(mode,a,b,r,observer,variant));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleTransactionalRetirement(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleNoStorageText(mode,a,r));
 for(const ending of [false,true])for(const mode of ['desktop','phone'])await runCompletedCase(()=>peoplePendingPhoneReachability(mode,a,r,ending));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peoplePermissionDuringUpload(mode,a,r));
  for(const mode of ['desktop','phone'])await runCompletedCase(()=>peoplePermissionMidBody(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleImageBookmark(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleSourceCancellation(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleQuota(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleLocalBudget(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleKeptFull(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleUnkeptOpen(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleFileUnread(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleAttachmentRecovery(mode,a,r));
 for(const mode of ['desktop','phone'])for(const kind of ['missing','corrupt'])await runCompletedCase(()=>peopleRelayBlobRecovery(mode,kind,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleCrossChannelForgery(mode,a,b,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleRetainedUnavailable(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleRetiredDrafts(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleCopyReplacement(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleStalePreparation(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>peopleRealTunnel(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>legacyReaderFiles(mode,a));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>oldRelayFiles(mode));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>stage2BookmarkPages(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>recreatedChatIsolation(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>bubbleSelection(mode,a,r));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>draftRecovery(mode,a));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>deleteRecreationIsolation(mode,a));
 for(const mode of ['desktop','phone'])await runCompletedCase(()=>legacyClosedRecreation(mode));
 const empty=await makeSeat('no-selection');for(const mode of ['desktop','phone'])await runCompletedCase(()=>noSelection(mode,empty));
 verifyMidbodyWitness(manifest);verifyLocalBudget(manifest);
 manifest.counters.afterNative=Object.fromEntries(seats.map(s=>[s.name,counts(s)]));
 assertID('native-authoritative-response',true);
 if(process.env.SCIMUX_E2E_FAIL_AFTER_NATIVE==='1')throw Error('controlled failure with native node');
}
(async()=>{try{await run();finishScenario(manifest,signalCode);}catch(e){
  /* Private lane log only: constant basename and line:column of the first failing frames, never messages, URLs or raw stack text. */
  console.error('E2E failure locations '+[...new Set(String(e&&e.stack||'').match(/team-e2e\.cjs:\d+:\d+/g)||[])].slice(0,4).join(' '));if(!signalCode)manifest.result='fail';const failureCode=controlledFailureCode(e,controlledNativeCleanupFailure);if(failureCode&&!signalCode)manifest.failureCode=failureCode;manifest.error='scenario-failure';console.error('E2E scenario failed');if(!signalCode)process.exitCode=1;for(const {page,label} of [...pages].reverse()){try{if(!page.isClosed()){const name='failure-'+label+'.png';if((await captureEvidencePage({page,destination:artifacts(name),timeout:5000,register:captureRegistry.register})).ok)manifest.artifacts.push(name);}}catch{}}}finally{await cleanup();if(signalCode)process.exitCode=signalCode;}})();
