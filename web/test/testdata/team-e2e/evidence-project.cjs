'use strict';

const scenarioNames=['default','people-files','people-stage-competition','people-pending-layout','people-no-storage-text','people-local-stage-review','people-transactional-drafts','people-captured-discard','people-stale-retry','people-text-retirement','people-transactional-retirement','people-uncertain-retry','people-upload-permission','people-midbody-permission','people-image-bookmark','people-source-cancel','people-quota','people-local-budget','people-kept-full','people-unkept-open','people-file-unread','people-attachment-recovery','people-relay-blob-recovery','people-cross-channel-forgery','people-retained-unavailable','people-retired-drafts','people-copy-replacement','people-stale-preparation','people-real-tunnel','legacy-reader','old-relay','stale-membership','stale-removed-membership','conversation-trace','conversation-native','conversation-refusal','conversation-core','early-native','native-start-failure','recipient-boundary','ai-source','ui-join','recipient-leave','permission-lifecycle','leave-empty','leave-stale','held-evidence','viewport-keyboard','dropped-send','reshare-stale','media-contrast','unshared','held-chat-loading','received-offline','stage1-scanner','stage1-unread','stage1-owner-files','stage2-shared-actions','stage2-bookmark-pages','recreated-chat-isolation','bubble-selection','draft-recovery','header-focus','description-save-isolation','description-recreated-choice','description-choice-order','no-selection','delete-recreation-isolation','legacy-closed-recreation','browser-lifecycle','lanes'];

// Only fields used by the public auditor can carry source-run observations.
const families=['stale-membership','stale-removed-membership','early-native','native-trace-debug','native-trace','boundary','ai-source','leave','permission-ended-debug','permission','held-evidence','dropped-send','reshare','coordinator-header-focus','coordinator-description-save','description-recreated-choice','description-choice-order','received-offline','stage1-scanner','stage1-owner-files','stage2-pages','draft-recovery','delete-recreation','legacy-closed','no-selection','people-files','people-stage-competition','people-pending-layout','people-pending-layout-ended','people-no-storage-text','people-local-stage-review','people-transactional-drafts','people-captured-discard','people-stale-retry','people-text-retirement-readonly','people-text-retirement-owner-ended-agent','people-transactional-retirement','people-uncertain-retry','people-upload-permission','people-midbody-permission','people-image-bookmark','people-source-cancel','people-quota','people-local-budget','people-kept-full','people-unkept-open','people-file-unread','people-recovery','people-relay-missing','people-relay-corrupt','people-unavailable','people-retired-draft','people-copy-replacement','people-stale-preparation','people-real-tunnel','people-cross-channel','legacy-reader','old-relay','stage1-scanner-owner','stage1-unread','stage2-shared','trace-address','trace-choices','native-start-failure','refusal','recreated','lanes'];
const fields=new Set(['attempted','accepted','controls','before','after','beforeOwner','afterOwner','beforeReader','afterReader','beforeOther','afterOther','beforeSend','afterSend','beforeSecond','secondCounts','during','final','initial','afterCreate','counters','beforeComments','afterComments','afterExtraSetup','afterNative','owner','owner-a','owner-b','recipient','observer','first','second','legacy','empty-desktop','empty-phone','legacy-closed-desktop','legacy-closed-phone','no-selection','legacy-reader-desktop','legacy-reader-phone','old-owner-desktop','old-reader-desktop','old-owner-phone','old-reader-phone','membership-desktop','membership-phone','activities-desktop','activities-phone','recovery-desktop','recovery-phone']);
const hasCounter=value=>value&&typeof value==='object'&&Object.entries(value).some(([key,child])=>['attempted','accepted','controls'].includes(key)||hasCounter(child));
function counters(value,strict=false,validateKnown=strict) {
  if(!value||typeof value!=='object'||Array.isArray(value))throw Error('invalid');
  if(validateKnown&&['attempted','accepted','controls'].some(key=>Object.hasOwn(value,key))&&
    (Object.keys(value).length!==3||!['attempted','accepted','controls'].every(key=>Object.hasOwn(value,key))))throw Error('invalid');
  const out={};
  for(const [key,child] of Object.entries(value)) {
    if(!fields.has(key)) {if(strict||hasCounter(child))throw Error('invalid');continue;}
    if(['attempted','accepted','controls'].includes(key)) {
      if(!Number.isSafeInteger(child)||child<0)throw Error('invalid');out[key]=child;
    } else if(child&&typeof child==='object')out[key]=counters(child,strict,validateKnown);
    else if(validateKnown)throw Error('invalid');
  }
  return out;
}
function observationKey(key) {return families.some(family=>key===family+'-desktop'||key===family+'-phone'||key===family+'-desktop-false'||key===family+'-desktop-true'||key===family+'-phone-false'||key===family+'-phone-true');}
function assertionID(id) {return typeof id==='string'&&id.length<=160&&/^[a-zA-Z0-9-]+$/.test(id);}
function safeManifest(input,kind) {
  if(!input||input.version!==1||!['pass','fail','interrupted'].includes(input.result)||!Array.isArray(input.assertions)||!Array.isArray(input.artifacts))throw Error('invalid');
  for(const key of ['path','host','credential','url','home'])if(Object.hasOwn(input,key))throw Error('invalid');
  const out={version:1,result:input.result,assertions:[],artifacts:[],counters:counters(input.counters,false,kind!=='full')};
  if(kind==='full'&&Object.hasOwn(input,'failureCode'))throw Error('invalid');
  if(Object.hasOwn(input,'cleanup')) {
    if(!Array.isArray(input.cleanup)||input.cleanup.some(item=>!item||typeof item!=='object'||Array.isArray(item)))throw Error('invalid');
    if(kind!=='full'&&input.cleanup.some(item=>['childrenStopped','socketRemoved'].some(key=>Object.hasOwn(item,key)&&typeof item[key]!=='boolean')))throw Error('invalid');
    const failures=[];
    for(const [name,failed] of [
      ['coverage',item=>item.coverage==='failed'],['browser',item=>Object.hasOwn(item,'browserCloseError')],
      ['seat',item=>Object.hasOwn(item,'deleteError')||Object.hasOwn(item,'quotaShareCleanupError')],['children',item=>item.childrenStopped===false],
      ['socket',item=>item.socketRemoved===false||Object.hasOwn(item,'socketError')||Object.hasOwn(item,'killError')],
      ['worker',item=>Number.isSafeInteger(item.remainingWorkerLocators)&&item.remainingWorkerLocators>0],
      ['native',item=>Number.isSafeInteger(item.remainingNativeHelpers)&&item.remainingNativeHelpers>0],
      ['fatal',item=>Object.hasOwn(item,'fatal')],
    ])if(input.cleanup.some(failed))failures.push(name);
    out.cleanup={failures};
    if(kind!=='full') {
      if(input.cleanup.some(item=>item.childrenStopped===true))out.cleanup.childrenStopped=true;
      if(input.cleanup.some(item=>item.socketRemoved===true))out.cleanup.socketRemoved=true;
    }
  }
  if(kind!=='full') {
    const outcomes={focused:'pass',failure:'fail',interrupted:'interrupted'};
    if(!Object.hasOwn(outcomes,kind)||input.result!==outcomes[kind]||!(['synthetic','cleanup probe only','implemented synthetic browser acceptance scenarios'].includes(input.scope)||scenarioNames.slice(1).some(name=>input.scope===name+' scenario only')))throw Error('invalid');
    if(Object.hasOwn(input,'failureCode')) {
      if(kind!=='failure'||input.failureCode!=='controlled-native-cleanup')throw Error('invalid');
      out.failureCode=input.failureCode;
    }
    out.scope=kind;
    if(kind==='interrupted'||(kind==='failure'&&Object.hasOwn(input,'signal'))) {
      if(!['SIGINT','SIGTERM'].includes(input.signal))throw Error('invalid');
      out.signal=input.signal;
    }
  } else if(Object.hasOwn(input,'scope')) {
    if(!['synthetic','implemented synthetic browser acceptance scenarios'].includes(input.scope))throw Error('invalid');
    out.scope=input.scope;
  }
  for(const item of input.assertions) {
    if(!item||Object.keys(item).length!==2||!assertionID(item.id)||item.result!=='pass')throw Error('invalid');
    out.assertions.push({id:item.id,result:item.result});
  }
  for(const name of input.artifacts) {
    if(typeof name!=='string'||!(/^[a-z0-9][a-z0-9-]*\.png$/.test(name)||name==='browser-coverage.json')||out.artifacts.includes(name))throw Error('invalid');
    out.artifacts.push(name);
  }
  for(const [key,value] of Object.entries(input)) {
    if(['version','scope','result','signal','assertions','artifacts','counters','cleanup','failureCode'].includes(key))continue;
    const allowed=observationKey(key);
    if(allowed) {
      let observation=value;
      if(key==='native-trace-debug-desktop'||key==='native-trace-debug-phone') {
        if(!value||typeof value!=='object'||Array.isArray(value))throw Error('invalid');
        observation={...value};
        for(const field of ['edited','submitted','attempts','accepted']) {
          if((field==='accepted'||field==='attempts')&&Object.hasOwn(observation,field)&&!Array.isArray(observation[field]))throw Error('invalid');
          delete observation[field];
        }
      }
      out[key]=counters(observation,false,kind!=='full');
    }
    else if(hasCounter(value))throw Error('invalid');
  }
  return out;
}
const amount=n=>Number.isSafeInteger(n)&&n>=0;
async function projectEvidence({source,io,auditor,proof,limits,usage,expectedIdentities={},verifyCoverage,kind='full'}) {
  if(!proof||proof.waited!==true||proof.clean!==true)return {ok:false,category:'unproven'};
  if(!source||!source.files||typeof source.files!=='object'||Array.isArray(source.files))return {ok:false,category:'invalid-evidence'};
  let manifest;
  try {manifest=safeManifest(source.manifest,kind);}catch{return {ok:false,category:'invalid-evidence'};}
  const names=[...manifest.artifacts];
  if(Object.hasOwn(source.files,'browser-coverage.json')&&!names.includes('browser-coverage.json'))names.push('browser-coverage.json');
  const files=new Map();
  let sourceBytes=source.rawMetadataBytes??Buffer.byteLength(JSON.stringify(source.manifest));
  if(!amount(sourceBytes))return {ok:false,category:'capacity'};
  let declaredBytes=sourceBytes;
  for(const name of names) {
    const meta=source.files[name],size=meta?.size??(Buffer.isBuffer(meta?.bytes)?meta.bytes.length:null);
    if(!amount(size))return {ok:false,category:'identity-changed'};
    declaredBytes+=size;
    if(!amount(declaredBytes)||!amount(limits.run)||declaredBytes>limits.run)return {ok:false,category:'capacity'};
  }
  try {
    for(const name of names) {
      const file=await io.read(name),expected=expectedIdentities[name]??source.files[name]?.identity;
      const declared=source.files[name].size??source.files[name].bytes.length;
      if(!file||file.kind!=='file'||!Buffer.isBuffer(file.bytes)||file.bytes.length!==declared||file.identity!==expected)return {ok:false,category:'identity-changed'};
      if(name.endsWith('.png')&&file.captureIdentity!==file.identity)return {ok:false,category:'identity-changed'};
      if(name==='browser-coverage.json') {
        if(typeof verifyCoverage!=='function')return {ok:false,category:'identity-changed'};
        try {await verifyCoverage(file);}catch{return {ok:false,category:'identity-changed'};}
      }
      files.set(name,file.bytes);
      sourceBytes+=file.bytes.length;
    }
  }catch{return {ok:false,category:'io-failure'};}
  files.set('manifest.json',Buffer.from(JSON.stringify(manifest)));
  const stagedBytes=[...files.values()].reduce((n,b)=>n+b.length,0);
  if(!amount(stagedBytes)||!amount(sourceBytes)||!['run','aggregate','diagnostics','registry'].every(k=>amount(limits[k]))||!['staging','registryCopies','diagnostics','otherRuns'].every(k=>amount(usage[k]))||usage.diagnostics>limits.diagnostics||stagedBytes+sourceBytes+usage.staging+usage.registryCopies+usage.diagnostics+limits.registry>limits.run||stagedBytes+sourceBytes+usage.staging+usage.registryCopies+usage.diagnostics+usage.otherRuns+limits.registry>limits.aggregate)return {ok:false,category:'capacity'};
  let written=0;
  try {for(const [name,bytes] of files){await io.write(name,bytes);written+=bytes.length;}}catch{return written?{ok:false,category:'io-failure',stagedBytes:written}:{ok:false,category:'io-failure'};}
  let expected;
  try {expected=await io.snapshot();if(!Array.isArray(expected))throw Error('invalid snapshot');}
  catch{return {ok:false,category:'io-failure',stagedBytes:written};}
  let audited;
  try {audited=await auditor({manifest,files});}catch{return {ok:false,category:'audit-failure',stagedBytes:written};}
  if(!audited?.ok||(kind!=='full'&&audited.acceptance!==false))return {ok:false,category:'audit-failure',stagedBytes:written};
  try {const result=await io.publish({stagedBytes:written,expected,acceptance:kind==='full'});if(!result?.published)return {ok:false,category:'publication-failure',stagedBytes:written};}
  catch{return {ok:false,category:'publication-failure',stagedBytes:written};}
  const figures=audited.figures||{assertions:audited.assertions,screenshots:audited.screenshots,seats:audited.seats,comparisons:audited.comparisons};
  return {ok:true,figures,stagedBytes:written,...(kind==='full'?{}:{acceptance:false})};
}
async function captureEvidencePage({page,destination,timeout,register}) {
  try {
    const mask=['#host','#sys','.teammeta','#nc_dir','#m_team_link','.chatseam span'].map(selector=>page.locator(selector));
    await page.screenshot({path:destination,fullPage:true,mask,...(timeout===undefined?{}:{timeout})});
    if(register) {
      const identity=await register(destination);
      if(!identity)return {ok:false,category:'capture-failure'};
      return {ok:true,identity};
    }
    return {ok:true};
  } catch { return {ok:false,category:'capture-failure'}; }
}
module.exports={projectEvidence,captureEvidencePage,scenarioNames,counters,observationKey,assertionID};
