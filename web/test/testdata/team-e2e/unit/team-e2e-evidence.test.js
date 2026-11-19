import test from 'node:test';
import assert from 'node:assert/strict';
import {projectEvidence,captureEvidencePage} from '../evidence-project.cjs';
import {audit,auditDiagnostic} from '../audit-evidence.cjs';
import {synthetic} from '../synthetic-audit-manifest.cjs';

const zero=()=>({attempted:0,accepted:0,controls:0});

async function scaledCoverageProjection(run) {
  const {limits}=await import('../runner-bootstrap.cjs');
  const bytes=Buffer.from('[]'+' '.repeat(298));
  const manifest={version:1,scope:'synthetic',result:'pass',assertions:[],artifacts:['browser-coverage.json'],counters:{}};
  const writes=new Map();let published=false;
  const result=await projectEvidence({source:{manifest,files:{'browser-coverage.json':{size:bytes.length,identity:'synthetic'}},rawMetadataBytes:Buffer.byteLength(JSON.stringify(manifest))},
    io:{read:async()=>({kind:'file',bytes,identity:'synthetic'}),write:async(name,value)=>writes.set(name,Buffer.from(value)),snapshot:async()=>[],publish:async()=>{published=true;return {published:true};}},
    proof:{waited:true,clean:true},limits:{run,aggregate:limits.total/1048576,diagnostics:limits.diagnostics/1048576,registry:2},
    usage:{staging:0,registryCopies:0,diagnostics:0,otherRuns:0},verifyCoverage:async({bytes:value})=>assert.ok(Array.isArray(JSON.parse(value))),auditor:async()=>({ok:true,acceptance:true})});
  return {result,writes,published};
}

test('scaled default budget accepts source and staged full-shape synthetic coverage',async()=>{
  const {limits}=await import('../runner-bootstrap.cjs');
  const {result,writes,published}=await scaledCoverageProjection(limits.reservation/1048576);
  assert.equal(result.ok,true);assert.equal(published,true);assert.deepEqual([...writes.keys()].sort(),['browser-coverage.json','manifest.json']);
});

test('explicit scaled 512 budget refuses the same over-cap projection before writing',async()=>{
  const {result,writes,published}=await scaledCoverageProjection(512);
  assert.equal(result.ok,false);assert.equal(result.category,'capacity');assert.equal(writes.size,0);assert.equal(published,false);
});

function fixture() {
  const writes=new Map(),calls=[],publications=[];
  const screenshot=Buffer.from('synthetic image bytes');
  const source={manifest:{version:1,scope:'synthetic',result:'pass',assertions:[{id:'synthetic-check',result:'pass'}],artifacts:['screen.png'],counters:{before:zero(),after:zero()}},
    files:{'screen.png':{kind:'file',identity:'screen-v1',captureIdentity:'screen-v1',bytes:screenshot},'browser-coverage.json':{kind:'file',identity:'coverage-v1',bytes:Buffer.from('synthetic coverage')}},
    privateEntries:{home:'secret-home',bin:'secret-binary',socket:'secret-socket',credentials:'secret-credential',log:'secret-log'}};
  const io={read:async name=>{calls.push(['read',name]);return source.files[name];},write:async(name,bytes)=>{calls.push(['write',name]);writes.set(name,Buffer.from(bytes));},snapshot:async()=>[],publish:async data=>{calls.push(['publish']);publications.push(data);return {published:true};}};
  const auditor=async files=>{calls.push(['audit',files]);return {ok:true,assertions:files.manifest.assertions.length,screenshots:files.manifest.artifacts.filter(x=>x.endsWith('.png')).length,seats:Object.keys(files.manifest.counters).length,comparisons:1};};
  const args={source,io,auditor,verifyCoverage:()=>{},proof:{waited:true,clean:true},limits:{run:1024,aggregate:2048,diagnostics:16,registry:32},usage:{staging:0,registryCopies:0,diagnostics:0,otherRuns:0}};
  return {args,source,writes,calls,publications};
}

for(const kind of ['full','focused','failure']) test(`${kind} projection preserves source-shaped keyboard assertion IDs`,async()=>{
  const f=fixture();
  const assertions=['viewport-keyboard-desktop-current-Enter','viewport-keyboard-phone-shared-Space','tab-keyboard-desktop-pinned-Space','tab-keyboard-phone-archived-Enter'].map(id=>({id,result:'pass'}));
  if(kind==='full')f.source.manifest=synthetic('owner-agent-attachments');
  else {f.source.manifest.scope='cleanup probe only';f.source.manifest.result=kind==='failure'?'fail':'pass';}
  f.source.manifest.assertions.push(...assertions);
  f.args.kind=kind;
  f.args.limits={run:1048576,aggregate:2097152,diagnostics:1024,registry:32};
  f.args.auditor=async({manifest})=>kind==='full'?{ok:!!audit(manifest,'owner-agent-attachments')} : auditDiagnostic(manifest);
  assert.equal((await projectEvidence(f.args)).ok,true);
  const projected=JSON.parse(f.writes.get('manifest.json'));
  assert.deepEqual(projected.assertions.slice(-assertions.length),assertions);
  if(kind!=='full')assert.equal(auditDiagnostic(projected).ok,true);
});

for(const id of [null, '', 'bad id', 'bad\nkey', '../path', 'a'.repeat(161)]) test('unsafe assertion ID is refused without echo',async()=>{
  const f=fixture();f.source.manifest.assertions=[{id,result:'pass'}];
  const result=await projectEvidence(f.args);
  assert.deepEqual(result,{ok:false,category:'invalid-evidence'});assert.equal(f.writes.size,0);
  const diagnostic={version:1,scope:'focused',result:'pass',assertions:[{id,result:'pass'}],artifacts:[],counters:{}};
  assert.deepEqual(auditDiagnostic(diagnostic),{ok:false});
});

test('assertion IDs accept the exact length limit',async()=>{
  const f=fixture(),id='A'.repeat(160);f.source.manifest.assertions=[{id,result:'pass'}];
  assert.equal((await projectEvidence(f.args)).ok,true);
  assert.equal(JSON.parse(f.writes.get('manifest.json')).assertions[0].id,id);
  assert.equal(auditDiagnostic({version:1,scope:'focused',result:'pass',assertions:[{id,result:'pass'}],artifacts:[],counters:{}}).ok,true);
});

test('duplicate keyboard IDs cannot pass either auditor or publish',async()=>{
  const ids=[{id:'synthetic-key-Enter',result:'pass'},{id:'synthetic-key-Enter',result:'pass'}];
  const full=synthetic('owner-agent-attachments');full.assertions.push(...ids);
  assert.throws(()=>audit(full,'owner-agent-attachments'));
  assert.deepEqual(auditDiagnostic({version:1,scope:'focused',result:'pass',assertions:ids,artifacts:[],counters:{}}),{ok:false});
  for(const kind of ['full','focused']) {
    const f=fixture();f.args.kind=kind;
    if(kind==='full')f.source.manifest=full;
    else {f.source.manifest.scope='cleanup probe only';f.source.manifest.assertions=ids;}
    f.args.limits={run:1048576,aggregate:2097152,diagnostics:1024,registry:32};
    f.args.auditor=async({manifest})=>kind==='full'?{ok:!!audit(manifest,'owner-agent-attachments')}:auditDiagnostic(manifest);
    assert.equal((await projectEvidence(f.args)).category,'audit-failure');assert.equal(f.publications.length,0);
  }
});

test('projection retains only approved evidence after waited clean proof',async()=>{
  const f=fixture();const result=await projectEvidence(f.args);
  assert.equal(result.ok,true);
  assert.deepEqual([...f.writes.keys()].sort(),['browser-coverage.json','manifest.json','screen.png']);
  const manifest=JSON.parse(f.writes.get('manifest.json'));
  assert.deepEqual(manifest.assertions,f.source.manifest.assertions);
  assert.deepEqual(manifest.counters,f.source.manifest.counters);
  const all=Buffer.concat([...f.writes.values()]).toString();
  for(const privateValue of Object.values(f.source.privateEntries))assert.equal(all.includes(privateValue),false);
  assert.ok(f.calls.find(x=>x[0]==='audit'));
});

for(const proof of [null,{waited:false,clean:true},{waited:true,clean:false}]) test('projection does not read or publish before authoritative process quiescence',async()=>{
  const f=fixture();f.args.proof=proof;
  const result=await projectEvidence(f.args);
  assert.equal(result.ok,false);assert.equal(result.category,'unproven');
  assert.deepEqual(f.calls,[]);assert.equal(f.writes.size,0);
});

for(const field of ['path','host','credential','url','home']) test(`private manifest ${field} cannot enter retained output`,async()=>{
  const f=fixture();f.source.manifest[field]='synthetic-private-marker';
  const result=await projectEvidence(f.args);
  assert.equal(result.ok,false);assert.equal(result.category,'invalid-evidence');
  assert.equal(Buffer.concat([...f.writes.values()]).toString().includes('synthetic-private-marker'),false);
});

for(const [label,change] of [
  ['runtime-home',f=>{f.source.manifest.artifacts.push('home');f.source.files.home={kind:'directory',identity:'home',bytes:Buffer.from('secret-home')};}],
  ['binary',f=>{f.source.manifest.artifacts.push('bin');f.source.files.bin={kind:'file',identity:'bin',bytes:Buffer.from('secret-binary')};}],
  ['socket',f=>{f.source.manifest.artifacts.push('socket');f.source.files.socket={kind:'socket',identity:'socket',bytes:Buffer.alloc(0)};}],
  ['credential',f=>{f.source.manifest.artifacts.push('credentials');f.source.files.credentials={kind:'file',identity:'credential',bytes:Buffer.from('secret-credential')};}],
]) test(`projection rejects ${label} even if requested as an artifact`,async()=>{
  const f=fixture();change(f);const result=await projectEvidence(f.args);
  assert.equal(result.ok,false);assert.equal(result.category,'invalid-evidence');assert.equal(f.writes.size,0);
});

for(const [label,change] of [
  ['run staging',f=>{f.args.usage.staging=1024;}],
  ['registry copies',f=>{f.args.usage.registryCopies=2048;}],
  ['failure diagnostics',f=>{f.args.usage.diagnostics=17;}],
  ['aggregate',f=>{f.args.usage.otherRuns=2048;}],
]) test(`${label} bytes count toward projection admission`,async()=>{
  const f=fixture();change(f);const result=await projectEvidence(f.args);
  assert.equal(result.ok,false);assert.equal(result.category,'capacity');assert.equal(f.writes.size,0);
});

test('input and filesystem errors produce a fixed diagnostic with no input echo',async()=>{
  const f=fixture();f.args.io.read=async()=>{throw Error('synthetic-private-marker /private/input');};
  const result=await projectEvidence(f.args);
  assert.deepEqual(result,{ok:false,category:'io-failure'});
  assert.equal(JSON.stringify(result).includes('synthetic-private-marker'),false);
  assert.equal(JSON.stringify(result).includes('/private/input'),false);
});
for(const name of ['screen.png','browser-coverage.json']) test(`changed ${name} identity refuses projection`,async()=>{
  const f=fixture();const read=f.args.io.read;
  f.args.io.read=async key=>{const value=await read(key);return key===name?{...value,identity:'changed'}:value;};
  f.args.expectedIdentities={[name]:f.source.files[name].identity};
  const result=await projectEvidence(f.args);
  assert.equal(result.ok,false);assert.equal(result.category,'identity-changed');assert.equal(f.writes.size,0);
});

test('shared capture boundary masks actual host, directory and invite UI selectors',async()=>{
  const seen=[],locators=new Map();
  const page={locator:selector=>{if(!locators.has(selector))locators.set(selector,{selector});return locators.get(selector);},
    screenshot:async options=>{seen.push(options);return Buffer.from('synthetic image bytes');}};
  const result=await captureEvidencePage({page,label:'synthetic',destination:'screen.png'});
  assert.equal(result.ok,true);assert.equal(seen.length,1);
  const masked=new Set(seen[0].mask);
  for(const selector of ['#host','#sys','.teammeta','#nc_dir','#m_team_link']) {
    assert.ok(locators.has(selector),`missing locator for ${selector}`);
    assert.ok(masked.has(locators.get(selector)),`missing mask for ${selector}`);
  }
  assert.equal(masked.has(locators.get('#workpulse')),false);
  assert.equal(seen[0].path,'screen.png');
});

test('distinct screenshots on one page are captured afresh',async()=>{
  const seen=[];
  const page={locator:selector=>({selector}),screenshot:async options=>{seen.push(options);return Buffer.from('synthetic image bytes');}};
  assert.equal((await captureEvidencePage({page,label:'first',destination:'first.png'})).ok,true);
  assert.equal((await captureEvidencePage({page,label:'second',destination:'second.png'})).ok,true);
  assert.deepEqual(seen.map(options=>options.path),['first.png','second.png']);
});

test('capture refuses when a required mask cannot be obtained',async()=>{
  let captures=0;
  const page={locator:()=>{throw Error('synthetic selector failure');},screenshot:async()=>{captures++;}};
  const result=await captureEvidencePage({page,label:'synthetic',destination:'screen.png'});
  assert.deepEqual(result,{ok:false,category:'capture-failure'});assert.equal(captures,0);
});

test('public auditor is invoked and rejects incomplete projected evidence',async()=>{
  const f=fixture();let invoked=false;
  f.args.auditor=async files=>{invoked=true;try{audit(files.manifest,'owner-agent-attachments');return {ok:true};}catch{return {ok:false};}};
  const result=await projectEvidence(f.args);
  assert.equal(invoked,true);assert.equal(result.ok,false);assert.equal(result.category,'audit-failure');
});

test('auditor-derived figures come from projected artifacts and counters',async()=>{
  const f=fixture();const result=await projectEvidence(f.args);
  assert.equal(result.ok,true);
  assert.deepEqual(result.figures,{assertions:1,screenshots:1,seats:2,comparisons:1});
  assert.ok(f.calls.find(x=>x[0]==='audit'));
});

test('auditor rejection of an unintended send blocks publication',async()=>{
  const f=fixture();f.source.manifest.counters.after={attempted:1,accepted:1,controls:0};
  let invoked=false;f.args.auditor=async files=>{invoked=true;return {ok:files.manifest.counters.after.attempted===0};};
  const result=await projectEvidence(f.args);
  assert.equal(invoked,true);assert.equal(result.ok,false);assert.equal(result.category,'audit-failure');assert.equal(f.publications.length,0);
});

test('projected on-disk evidence is checked by the public auditor and corruption is refused',async t=>{
  const fs=await import('node:fs');const os=await import('node:os');const path=await import('node:path');
  const {runCLI}=await import('../audit-evidence.cjs');
  const {synthetic}=await import('../synthetic-audit-manifest.cjs');
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'projection-synthetic-'));
  t.after(()=>fs.rmSync(root,{recursive:true,force:true}));
  const f=fixture();f.source.manifest=synthetic('owner-agent-attachments');
  const png=Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAEUlEQVR4nGP4z8DwH4QZYAwAR8oH+WdZbrcAAAAASUVORK5CYII=','base64');
  f.source.files={'screen.png':{kind:'file',identity:'screen-v1',captureIdentity:'screen-v1',bytes:png}};
  f.args.limits={run:1024*1024,aggregate:2*1024*1024,diagnostics:1024,registry:1024};
  f.args.io.write=async(name,bytes)=>fs.writeFileSync(path.join(root,name),bytes);
  let output='',called=0;
  f.args.auditor=async()=>{called++;const cwd=process.cwd();try{
    const code=runCLI(['--profile','owner-agent-attachments',root],s=>{output+=s;});
    if(code!==0)return {ok:false};
    const lines=output.trim().split('\n');
    return {ok:code===0,figures:{assertions:Number(lines[0].split(' ').at(-1)),screenshots:Number(lines[1].split(' ').at(-1)),historical:Number(lines[2].split(' ').at(-1)),additional:Number(lines[3].split(' ').at(-1)),seats:Number(lines[4].split(' ').at(-1))}};
  }finally{process.chdir(cwd);}};
  const result=await projectEvidence(f.args);
  assert.equal(result.ok,true);assert.equal(called,1);
  const actual=fs.readdirSync(root).sort();assert.deepEqual(actual,['manifest.json','screen.png']);
  const manifest=JSON.parse(fs.readFileSync(path.join(root,'manifest.json'),'utf8'));
  const derived=audit(manifest,'owner-agent-attachments');
  assert.deepEqual(output.split('\n').slice(0,2),[`assertions ${derived.assertions}`,`screenshots ${derived.screenshots}`]);
  assert.deepEqual(result.figures,{assertions:derived.assertions,screenshots:derived.screenshots,seats:derived.seats,historical:derived.historical,additional:derived.additional});
  fs.writeFileSync(path.join(root,'screen.png'),Buffer.from('corrupt'));
  const cwd=process.cwd();let after='';try{assert.equal(runCLI(['--profile','owner-agent-attachments',root],s=>{after+=s;}),1);}finally{process.chdir(cwd);}
  assert.equal(after,'FAIL\n');
});

test('checked-in source mismatch in coverage refuses publication',async()=>{
  const f=fixture();let checked=false;
  f.args.verifyCoverage=()=>{checked=true;throw Error('synthetic source mismatch');};
  const result=await projectEvidence(f.args);
  assert.equal(checked,true);assert.equal(result.ok,false);assert.equal(result.category,'identity-changed');
});

test('synthetic audit fixture checks file integrity and exact dispatch rules',async t=>{
  const fs=await import('node:fs');const os=await import('node:os');const path=await import('node:path');
  const {runCLI}=await import('../audit-evidence.cjs');
  const {synthetic}=await import('../synthetic-audit-manifest.cjs');
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'audit-fixture-synthetic-'));
  t.after(()=>fs.rmSync(root,{recursive:true,force:true}));
  const manifest=synthetic('owner-agent-attachments');
  const png=Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAEUlEQVR4nGP4z8DwH4QZYAwAR8oH+WdZbrcAAAAASUVORK5CYII=','base64');
  fs.writeFileSync(path.join(root,'manifest.json'),JSON.stringify(manifest));fs.writeFileSync(path.join(root,'screen.png'),png);
  const invoke=()=>{const cwd=process.cwd();let out='';try{return {code:runCLI(['--profile','owner-agent-attachments',root],s=>{out+=s;}),out};}finally{process.chdir(cwd);}};
  const expected=audit(manifest,'owner-agent-attachments');const first=invoke();
  assert.equal(first.code,0);assert.deepEqual(first.out.split('\n').slice(0,2),[`assertions ${expected.assertions}`,`screenshots ${expected.screenshots}`]);
  const unexpected=structuredClone(manifest);unexpected['boundary-desktop'].after.attempted=1;unexpected['boundary-desktop'].after.accepted=1;
  fs.writeFileSync(path.join(root,'manifest.json'),JSON.stringify(unexpected));assert.deepEqual(invoke(),{code:1,out:'FAIL\n'});
  const missingSend=structuredClone(manifest);missingSend['trace-address-desktop'].afterSend.accepted=0;
  fs.writeFileSync(path.join(root,'manifest.json'),JSON.stringify(missingSend));assert.deepEqual(invoke(),{code:1,out:'FAIL\n'});
  fs.writeFileSync(path.join(root,'manifest.json'),JSON.stringify(manifest));fs.writeFileSync(path.join(root,'screen.png'),'corrupt');
  assert.deepEqual(invoke(),{code:1,out:'FAIL\n'});
  fs.unlinkSync(path.join(root,'screen.png'));assert.deepEqual(invoke(),{code:1,out:'FAIL\n'});
});

test('audit reads bounded staging and success publishes exactly once afterward',async()=>{
  const f=fixture();let observed;
  f.args.auditor=async()=>{
    observed=[...f.writes.keys()].sort();f.calls.push(['audited']);
    return {ok:true,figures:{assertions:1,screenshots:1,seats:2,comparisons:1}};
  };
  const result=await projectEvidence(f.args);
  assert.equal(result.ok,true);
  assert.deepEqual(observed,['browser-coverage.json','manifest.json','screen.png']);
  assert.equal(f.publications.length,1);
  assert.ok(f.calls.findIndex(x=>x[0]==='audited')<f.calls.findIndex(x=>x[0]==='publish'));
  assert.ok(result.stagedBytes>0&&result.stagedBytes<=f.args.limits.run);
});

for(const mode of ['reject','throw']) test(`audit ${mode} retains accounted staging without publishing acceptance`,async()=>{
  const f=fixture();let observed;
  f.args.auditor=async()=>{observed=[...f.writes.keys()].sort();if(mode==='throw')throw Error('synthetic audit error');return {ok:false};};
  const result=await projectEvidence(f.args);
  assert.equal(result.ok,false);assert.equal(result.category,'audit-failure');
  assert.deepEqual(observed,['browser-coverage.json','manifest.json','screen.png']);
  assert.equal(f.publications.length,0);
  const bytes=[...f.writes.values()].reduce((n,b)=>n+b.length,0);
  assert.equal(result.stagedBytes,bytes);assert.ok(bytes>0&&bytes<=f.args.limits.run);
  assert.deepEqual([...f.writes.keys()].sort(),observed);
});

test('coverage without source verification is never staged or published',async()=>{
  const f=fixture();delete f.args.verifyCoverage;
  const result=await projectEvidence(f.args);
  assert.equal(result.ok,false);assert.equal(result.category,'identity-changed');
  assert.equal(f.writes.size,0);assert.equal(f.publications.length,0);
});

test('a screenshot without registered synthetic capture provenance is not staged',async()=>{
  const f=fixture();delete f.source.files['screen.png'].captureIdentity;
  const result=await projectEvidence(f.args);
  assert.equal(result.ok,false);assert.equal(result.category,'identity-changed');
  assert.equal(f.writes.size,0);assert.equal(f.publications.length,0);
});

test('failure screenshot retains its bounded capture timeout',async()=>{
  let options;
  const page={locator:selector=>({selector}),screenshot:async value=>{options=value;}};
  assert.equal((await captureEvidencePage({page,destination:'failure.png',timeout:5000})).ok,true);
  assert.equal(options.timeout,5000);
});

test('a successful screenshot records capture identity only after the write completes',async()=>{
  const events=[];
  const page={locator:selector=>({selector}),screenshot:async()=>{events.push('write');}};
  const result=await captureEvidencePage({page,destination:'synthetic.png',register:async destination=>{events.push(['register',destination]);return {identity:'synthetic'};}});
  assert.equal(result.ok,true);assert.deepEqual(result.identity,{identity:'synthetic'});
  assert.deepEqual(events,['write',['register','synthetic.png']]);
});

test('a failed screenshot is never registered as synthetic capture',async()=>{
  let registered=false;
  const page={locator:selector=>({selector}),screenshot:async()=>{throw Error('synthetic screenshot failure');}};
  const result=await captureEvidencePage({page,destination:'synthetic.png',register:async()=>{registered=true;}});
  assert.equal(result.ok,false);assert.equal(registered,false);
});

test('capture identity binds the exact regular-file bytes and rejects symlinks',async t=>{
  const fs=await import('node:fs/promises');const os=await import('node:os');const path=await import('node:path');
  const {captureIdentity}=await import('../capture-identity.cjs');
  const root=await fs.mkdtemp(path.join(os.tmpdir(),'capture-identity-synthetic-'));t.after(()=>fs.rm(root,{recursive:true,force:true}));
  const file=path.join(root,'screen.png'),link=path.join(root,'link');
  await fs.writeFile(file,'synthetic first');const first=await captureIdentity(file);
  assert.equal(typeof first,'string');assert.ok(first.length>0);
  await fs.writeFile(file,'synthetic other');assert.notEqual(await captureIdentity(file),first);
  await fs.symlink(file,link);assert.equal(await captureIdentity(link),null);
});

test('capture cannot succeed when its registered identity is unavailable',async()=>{
  const page={locator:selector=>({selector}),screenshot:async()=>{}};
  const result=await captureEvidencePage({page,destination:'synthetic.png',register:async()=>null});
  assert.deepEqual(result,{ok:false,category:'capture-failure'});
});

test('capture registry persists only the basename and verified identity',async()=>{
  const {createCaptureRegistry}=await import('../capture-identity.cjs');
  const writes=[];const registry=createCaptureRegistry({identity:async()=> 'synthetic-identity',write:async value=>writes.push(value)});
  assert.equal(await registry.register('/synthetic/screen.png'),'synthetic-identity');
  assert.deepEqual(JSON.parse(writes[0]),{'screen.png':'synthetic-identity'});
});

test('capture registry refuses a duplicate screenshot name without replacing its identity',async()=>{
  const {createCaptureRegistry}=await import('../capture-identity.cjs');
  const writes=[];let sequence=0;
  const registry=createCaptureRegistry({identity:async()=>String(++sequence),write:async value=>writes.push(value)});
  assert.equal(await registry.register('/synthetic/screen.png'),'1');
  assert.equal(await registry.register('/synthetic/screen.png'),null);
  assert.equal(writes.length,1);assert.deepEqual(JSON.parse(writes[0]),{'screen.png':'1'});
});

for(const [kind,result,signal] of [['focused','pass',null],['failure','fail',null],['interrupted','interrupted','SIGINT']]) test(`projected ${kind} evidence remains diagnostic and truthful`,async()=>{
  const f=fixture();f.source.manifest.scope='browser-lifecycle scenario only';f.source.manifest.result=result;
  if(signal)f.source.manifest.signal=signal;
  let observed;f.args.kind=kind;
  f.args.auditor=async files=>{observed=files.manifest;return {ok:true,acceptance:false,figures:{assertions:1,screenshots:1,seats:0,comparisons:0}};};
  const projected=await projectEvidence(f.args);
  assert.equal(projected.ok,true);assert.equal(projected.acceptance,false);
  assert.equal(observed.scope,kind);assert.equal(observed.result,result);
  if(signal)assert.equal(observed.signal,signal);
  assert.equal(f.publications.length,1);assert.equal(f.publications[0].acceptance,false);
});

test('a failed full audit cannot be replaced by diagnostic acceptance',async()=>{
  const f=fixture();f.args.kind='full';
  f.args.auditor=async()=>({ok:false,acceptance:false});
  const result=await projectEvidence(f.args);
  assert.equal(result.ok,false);assert.equal(result.category,'audit-failure');assert.equal(f.publications.length,0);
});

test('capture identity enforces the specific artifact byte limit',async t=>{
  const fs=await import('node:fs/promises');const os=await import('node:os');const path=await import('node:path');
  const {captureIdentity}=await import('../capture-identity.cjs');
  const root=await fs.mkdtemp(path.join(os.tmpdir(),'identity-limit-synthetic-'));t.after(()=>fs.rm(root,{recursive:true,force:true}));
  const file=path.join(root,'artifact');await fs.writeFile(file,'synthetic payload');
  assert.equal(await captureIdentity(file,{limit:4}),null);
  assert.equal(typeof await captureIdentity(file,{limit:64}),'string');
});

test('publication receives the artifact identities captured before audit',async()=>{
  const f=fixture(),expected=[{name:'screen.png',dev:1,ino:2,size:3,sha256:'synthetic'}];let snapshotBeforeAudit=false;
  f.args.io.snapshot=async()=>{snapshotBeforeAudit=true;return expected;};
  f.args.auditor=async()=>{assert.equal(snapshotBeforeAudit,true);return {ok:true,figures:{}};};
  const result=await projectEvidence(f.args);
  assert.equal(result.ok,true);assert.deepEqual(f.publications[0].expected,expected);
});

test('source evidence and its staged copy both count toward the per-run peak',async()=>{
  const f=fixture();
  const fileBytes=Object.values(f.source.files).reduce((sum,file)=>sum+file.bytes.length,0);
  const manifestBytes=Buffer.byteLength(JSON.stringify(f.source.manifest));
  f.args.limits.run=fileBytes+manifestBytes+f.args.limits.registry+1;
  const result=await projectEvidence(f.args);
  assert.equal(result.ok,false);assert.equal(result.category,'capacity');
  assert.equal(f.writes.size,0);assert.equal(f.publications.length,0);
});

test('over-budget declared source evidence is refused before artifact reads',async()=>{
  const f=fixture();f.source.files['screen.png'].bytes=Buffer.alloc(600);f.source.files['screen.png'].size=600;f.args.limits.run=500;
  const result=await projectEvidence(f.args);
  assert.equal(result.ok,false);assert.equal(result.category,'capacity');
  assert.equal(f.calls.some(call=>call[0]==='read'),false);assert.equal(f.writes.size,0);
});

test('missing source inventory fails with a fixed diagnostic',async()=>{
  const f=fixture();delete f.source.files;
  await assert.doesNotReject(async()=>{const result=await projectEvidence(f.args);assert.equal(result.ok,false);assert.equal(result.category,'invalid-evidence');});
  assert.equal(f.publications.length,0);assert.equal(f.writes.size,0);
});

for(const defect of ['null-manifest','invalid-counters','invalid-scope','hidden-counter','metadata-size','artifact-size','missing-artifact','invalid-assertion']) test(`projection refuses ${defect} without publication`,async()=>{
  const f=fixture();
  if(defect==='null-manifest')f.source.manifest=null;
  if(defect==='invalid-counters')f.source.manifest.counters=[];
  if(defect==='invalid-scope')f.source.manifest.scope='untrusted';
  if(defect==='hidden-counter')f.source.manifest.unexpected={nested:{attempted:1}};
  if(defect==='metadata-size')f.source.rawMetadataBytes=-1;
  if(defect==='artifact-size')f.source.files['screen.png'].size=-1;
  if(defect==='missing-artifact')delete f.source.files['screen.png'];
  if(defect==='invalid-assertion')f.source.manifest.assertions=[{id:'synthetic',result:'fail'}];
  const result=await projectEvidence(f.args);assert.equal(result.ok,false);assert.equal(f.publications.length,0);assert.equal(f.writes.size,0);
});

for(const fault of ['first-write','later-write','snapshot-throw','snapshot-invalid','publish-refuse','publish-throw']) test(`projection reports ${fault} and retains accounted staging`,async()=>{
  const f=fixture();let writes=0;
  if(fault.endsWith('write'))f.args.io.write=async()=>{if(++writes===(fault==='first-write'?1:2))throw Error('synthetic write failure');};
  if(fault==='snapshot-throw')f.args.io.snapshot=async()=>{throw Error('synthetic snapshot failure');};
  if(fault==='snapshot-invalid')f.args.io.snapshot=async()=>null;
  if(fault==='publish-refuse')f.args.io.publish=async()=>({published:false});
  if(fault==='publish-throw')f.args.io.publish=async()=>{throw Error('synthetic publication failure');};
  const result=await projectEvidence(f.args);assert.equal(result.ok,false);
  assert.equal(result.category,fault.startsWith('publish')?'publication-failure':'io-failure');
  if(fault!=='first-write')assert.ok(result.stagedBytes>0);
});

test('projection preserves allowed counters and sanitizes native cleanup outcome',async()=>{
  const f=fixture();f.source.manifest['boundary-desktop']={before:{attempted:0,accepted:0,controls:0}};
  f.source.manifest.cleanup=[{remainingNativeHelpers:1}];let manifest;
  f.args.auditor=async value=>{manifest=value.manifest;return {ok:true};};
  assert.equal((await projectEvidence(f.args)).ok,true);assert.deepEqual(manifest['boundary-desktop'],{before:zero()});assert.deepEqual(manifest.cleanup.failures,['native']);
});

test('capture masks runtime-generated chat seam timestamps',async()=>{
  let options;const locators=new Map();
  const page={locator:selector=>{if(!locators.has(selector))locators.set(selector,{selector});return locators.get(selector);},screenshot:async value=>{options=value;}};
  assert.equal((await captureEvidencePage({page,destination:'synthetic.png'})).ok,true);
  assert.ok(locators.has('.chatseam span'));assert.ok(options.mask.includes(locators.get('.chatseam span')));
});

test('failed diagnostic preserves the first signal without claiming interruption success',async()=>{
  const f=fixture();f.args.kind='failure';f.source.manifest.scope='cleanup probe only';f.source.manifest.result='fail';f.source.manifest.signal='SIGINT';
  let observed;f.args.auditor=async files=>{observed=files.manifest;return {ok:true,acceptance:false,figures:{}};};
  const result=await projectEvidence(f.args);
  assert.equal(result.ok,true);assert.equal(result.acceptance,false);
  assert.equal(observed.result,'fail');assert.equal(observed.signal,'SIGINT');assert.equal(f.publications[0].acceptance,false);
});

test('diagnostic cleanup preserves fixed failure categories without raw details',async()=>{
  const f=fixture();f.args.kind='failure';f.source.manifest.scope='cleanup probe only';f.source.manifest.result='fail';
  f.source.manifest.cleanup=[{coverage:'failed',error:'synthetic private detail'},{childrenStopped:false},{socketRemoved:false},{remainingWorkerLocators:1}];
  let observed;f.args.auditor=async files=>{observed=files.manifest;return {ok:true,acceptance:false,figures:{}};};
  const result=await projectEvidence(f.args);
  assert.equal(result.ok,true);assert.deepEqual(observed.cleanup,{failures:['coverage','children','socket','worker']});
  assert.equal(JSON.stringify(observed).includes('synthetic private detail'),false);
});

test('diagnostic projection retains positive synthetic cleanup observations',async()=>{
  const f=fixture();f.args.kind='interrupted';f.source.manifest.scope='cleanup probe only';f.source.manifest.result='interrupted';f.source.manifest.signal='SIGINT';
  f.source.manifest.cleanup=[{childrenStopped:true},{socketRemoved:true},{remainingWorkerLocators:0},{remainingNativeHelpers:0}];
  let observed;f.args.auditor=async files=>{observed=files.manifest;return {ok:true,acceptance:false};};
  assert.equal((await projectEvidence(f.args)).ok,true);
  assert.deepEqual(observed.cleanup,{failures:[],childrenStopped:true,socketRemoved:true});
});

test('diagnostic projection retains only the fixed synthetic controlled failure identity',async()=>{
  const f=fixture();f.args.kind='failure';f.source.manifest.scope='cleanup probe only';f.source.manifest.result='fail';f.source.manifest.failureCode='controlled-native-cleanup';
  let observed;f.args.auditor=async files=>{observed=files.manifest;return {ok:true,acceptance:false};};
  assert.equal((await projectEvidence(f.args)).ok,true);
  assert.equal(observed.failureCode,'controlled-native-cleanup');
});

test('diagnostic projection reports synthetic quota cleanup errors without their details',async()=>{
  const f=fixture();f.args.kind='failure';f.source.manifest.scope='cleanup probe only';f.source.manifest.result='fail';
  f.source.manifest.cleanup=[{quotaShareCleanupError:'synthetic private detail'}];
  let observed;f.args.auditor=async files=>{observed=files.manifest;return {ok:true,acceptance:false};};
  assert.equal((await projectEvidence(f.args)).ok,true);
  assert.deepEqual(observed.cleanup.failures,['seat']);
  assert.equal(JSON.stringify(observed).includes('synthetic private detail'),false);
});

test('diagnostic projection does not invent cleanup positives or controlled failure identity',async()=>{
  const f=fixture();f.args.kind='failure';f.source.manifest.scope='cleanup probe only';f.source.manifest.result='fail';f.source.manifest.error='unexpected synthetic error';f.source.manifest.cleanup=[];
  let observed;f.args.auditor=async files=>{observed=files.manifest;return {ok:true,acceptance:false};};
  assert.equal((await projectEvidence(f.args)).ok,true);
  assert.notEqual(observed.cleanup.childrenStopped,true);assert.notEqual(observed.cleanup.socketRemoved,true);assert.equal(observed.failureCode,undefined);
});

for(const [kind,result,scope,signal] of [
  ['failure','fail','implemented synthetic browser acceptance scenarios'],
  ['interrupted','interrupted','implemented synthetic browser acceptance scenarios','SIGINT'],
  ['focused','pass','people-files scenario only'],
]) test(`diagnostic projection accepts known ${scope} for ${kind}`,async()=>{
  const f=fixture();f.args.kind=kind;Object.assign(f.source.manifest,{scope,result});if(signal)f.source.manifest.signal=signal;
  f.args.auditor=async()=>({ok:true,acceptance:false});
  const resultValue=await projectEvidence(f.args);assert.equal(resultValue.ok,true);assert.equal(resultValue.acceptance,false);
});

test('diagnostic projection rejects unknown scopes',async()=>{
  const f=fixture();f.args.kind='failure';Object.assign(f.source.manifest,{scope:'unknown scenario only',result:'fail'});
  assert.equal((await projectEvidence(f.args)).ok,false);
});

test('diagnostic positive observations preserve simultaneous failure flags',async()=>{
  const f=fixture();f.args.kind='failure';Object.assign(f.source.manifest,{scope:'cleanup probe only',result:'fail',cleanup:[{childrenStopped:true},{childrenStopped:false},{socketRemoved:true},{socketRemoved:false}]});
  let observed;f.args.auditor=async value=>{observed=value.manifest;return {ok:true,acceptance:false};};
  assert.equal((await projectEvidence(f.args)).ok,true);assert.deepEqual(observed.cleanup,{failures:['children','socket'],childrenStopped:true,socketRemoved:true});
});

for(const value of [false,'true',1,null]) test(`diagnostic projection does not claim invalid positive ${String(value)}`,async()=>{
  const f=fixture();f.args.kind='failure';Object.assign(f.source.manifest,{scope:'cleanup probe only',result:'fail',cleanup:[{childrenStopped:value},{socketRemoved:value}]});
  let observed;f.args.auditor=async value=>{observed=value.manifest;return {ok:true,acceptance:false};};
  const result=await projectEvidence(f.args);if(value===false){assert.equal(result.ok,true);assert.deepEqual(observed.cleanup.failures,['children','socket']);assert.equal(observed.cleanup.childrenStopped,undefined);assert.equal(observed.cleanup.socketRemoved,undefined);}else assert.equal(result.ok,false);
});

test('diagnostic projection rejects unsupported failure code',async()=>{
  const f=fixture();f.args.kind='failure';Object.assign(f.source.manifest,{scope:'cleanup probe only',result:'fail',failureCode:'synthetic-unknown'});
  f.args.auditor=async()=>({ok:true,acceptance:false});
  assert.equal((await projectEvidence(f.args)).ok,false);
});

test('partial full diagnostic preserves observed intended sends without claiming acceptance',async()=>{
  const f=fixture();f.args.kind='failure';Object.assign(f.source.manifest,{scope:'implemented synthetic browser acceptance scenarios',result:'fail',counters:{before:{'owner-a':zero()},after:{'owner-a':{attempted:1,accepted:1,controls:0}}},'people-files-desktop':{beforeSend:zero(),afterSend:{attempted:1,accepted:1,controls:0}}});
  f.args.auditor=async({manifest})=>{const {auditDiagnostic}=await import('../audit-evidence.cjs');return auditDiagnostic(manifest);};
  const result=await projectEvidence(f.args);assert.equal(result.ok,true,result.category);assert.equal(result.acceptance,false);
  const saved=JSON.parse(f.writes.get('manifest.json'));assert.equal(saved.counters.after['owner-a'].accepted,1);assert.equal(saved['people-files-desktop'].afterSend.accepted,1);
});

test('partial focused diagnostic preserves an already observed intended send',async()=>{
 const f=fixture();f.args.kind='failure';Object.assign(f.source.manifest,{scope:'people-files scenario only',result:'fail','people-files-desktop':{beforeSend:zero(),afterSend:{attempted:1,accepted:1,controls:0}}});
 f.args.auditor=async({manifest})=>{const {auditDiagnostic}=await import('../audit-evidence.cjs');return auditDiagnostic(manifest);};
 const result=await projectEvidence(f.args);assert.equal(result.ok,true,result.category);assert.equal(result.acceptance,false);
 assert.equal(JSON.parse(f.writes.get('manifest.json'))['people-files-desktop'].afterSend.accepted,1);
});

test('full projection rejects diagnostic failure codes',async()=>{
 const f=fixture();f.source.manifest.failureCode='controlled-native-cleanup';assert.equal((await projectEvidence(f.args)).ok,false);
});

test('projection validates code scope and interrupted signal',async()=>{
 for(const [kind,result,signal,code] of [['focused','pass',null,'controlled-native-cleanup'],['interrupted','interrupted','SIGINT','controlled-native-cleanup'],['interrupted','interrupted','BAD',null]]){
  const f=fixture();f.args.kind=kind;Object.assign(f.source.manifest,{scope:'synthetic',result});if(signal)f.source.manifest.signal=signal;if(code)f.source.manifest.failureCode=code;
  f.args.auditor=async()=>({ok:true,acceptance:false});assert.equal((await projectEvidence(f.args)).ok,false);
 }
});

test('projection rejects malformed cleanup and unsafe synthetic counter shapes',async()=>{
 for(const change of [
  m=>{m.cleanup=[null];},m=>{m.counters={unknown:{attempted:1}};},m=>{m.counters={before:{attempted:-1}};},
 ]){const f=fixture();f.args.kind='failure';Object.assign(f.source.manifest,{scope:'synthetic',result:'fail'});change(f.source.manifest);f.args.auditor=async()=>({ok:true,acceptance:false});assert.equal((await projectEvidence(f.args)).ok,false);}
});

test('projection discards irrelevant synthetic counter metadata',async()=>{
 const f=fixture();f.source.manifest.counters={before:zero(),irrelevant:'synthetic'};assert.equal((await projectEvidence(f.args)).ok,true);assert.equal(JSON.parse(f.writes.get('manifest.json')).counters.irrelevant,undefined);
});

for(const shape of ['native-trace-debug','no-selection']) test(`full projection preserves source-defined ${shape} observations`,async()=>{
 const {synthetic}=await import('../synthetic-audit-manifest.cjs');
 const f=fixture();f.source.manifest=synthetic('people-attachments');delete f.source.files['browser-coverage.json'];
 f.args.limits={run:1048576,aggregate:2097152,diagnostics:1024,registry:32};
 if(shape==='native-trace-debug')Object.assign(f.source.manifest['native-trace-debug-desktop'],{edited:'synthetic private text',submitted:'synthetic private text',attempts:[{text:'synthetic'}],accepted:[{text:'synthetic'}]});
 else f.source.manifest['no-selection-desktop'].initial={attempted:23,accepted:19,controls:0};
 assert.doesNotThrow(()=>audit(f.source.manifest,'people-attachments'));
 f.args.auditor=async({manifest})=>({ok:true,acceptance:true,figures:audit(manifest,'people-attachments')});
 const result=await projectEvidence(f.args);assert.equal(result.ok,true,result.category);
 const saved=JSON.parse(f.writes.get('manifest.json'));
 if(shape==='native-trace-debug'){
  assert.deepEqual(saved['native-trace-debug-desktop'].before,f.source.manifest['native-trace-debug-desktop'].before);
  assert.deepEqual(saved['native-trace-debug-desktop'].after,f.source.manifest['native-trace-debug-desktop'].after);
  assert.equal(Object.hasOwn(saved['native-trace-debug-desktop'],'accepted'),false);assert.equal(JSON.stringify(saved).includes('synthetic private text'),false);
 } else assert.deepEqual(saved['no-selection-desktop'].initial,{attempted:23,accepted:19,controls:0});
 assert.doesNotThrow(()=>audit(saved,'people-attachments'));
});

test('projection rejects accepted arrays in actual numeric counter objects',async()=>{
 const f=fixture();f.source.manifest.counters.before.accepted=['synthetic'];assert.equal((await projectEvidence(f.args)).category,'invalid-evidence');
});

test('diagnostic projection rejects unsafe known counter shapes',async()=>{
 for(const value of ['synthetic private text',[],null]) {
  const f=fixture();f.args.kind='failure';Object.assign(f.source.manifest,{scope:'synthetic',result:'fail',counters:{before:value}});f.args.auditor=async()=>({ok:true,acceptance:false});
  assert.equal((await projectEvidence(f.args)).category,'invalid-evidence');
 }
});

test('diagnostic projection omits non-counter synthetic observation details while retaining counters',async()=>{
 const f=fixture();f.args.kind='failure';Object.assign(f.source.manifest,{scope:'synthetic',result:'fail','people-files-desktop':{beforeSend:zero(),afterSend:{attempted:1,accepted:1,controls:0},note:'synthetic private detail'}});
 f.args.auditor=async()=>({ok:true,acceptance:false});assert.equal((await projectEvidence(f.args)).ok,true);
 const saved=JSON.parse(f.writes.get('manifest.json'));assert.equal(saved['people-files-desktop'].afterSend.accepted,1);assert.equal(JSON.stringify(saved).includes('synthetic private detail'),false);
});

test('native trace debug projection rejects malformed evidence containers',async()=>{
 for(const observation of [null,[],{attempts:'synthetic'}, {accepted:'synthetic'}, {attempts:['synthetic'],accepted:{attempted:1}}]){
  const f=fixture();f.source.manifest['native-trace-debug-desktop']=observation;
  assert.equal((await projectEvidence(f.args)).category,'invalid-evidence');
 }
});
