import test from 'node:test';
import assert from 'node:assert/strict';
import {createRequire} from 'node:module';
import {spawnSync} from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
const require=createRequire(import.meta.url);
const {verifyMidbodyWitness}=require('./testdata/team-e2e/verify-midbody-witness.cjs');
const {verifyLocalBudget}=require('./testdata/team-e2e/verify-local-budget.cjs');
function evidence() {
  const manifest={assertions:[]};
  for (const mode of ['desktop','phone']) {
    for (const prefix of ['people-midbody-muxer-body-read-','people-midbody-refused-before-body-','people-midbody-frontend-continue-not-admission-','people-local-budget-refusal-','people-local-budget-preserved-','people-local-budget-explicit-retry-','people-local-budget-zero-agent-']) manifest.assertions.push({id:prefix+mode,result:'pass'});
    manifest['people-midbody-permission-'+mode]={muxerContinue:1,refusedContinue:0,refusedStatus:403,frontContinue:1,frontBytes:1024,frontStatus:403};
    manifest['people-local-budget-'+mode]={status:507,code:'storage_full',commentCount:1};
  }
  return manifest;
}
test('midbody witness requires backend admission and refused frontend negative evidence',()=>{
  const manifest=evidence();verifyMidbodyWitness(manifest);
  for(const mutate of [m=>delete m.assertions,m=>{m.assertions=m.assertions.filter(x=>x.id!=='people-midbody-muxer-body-read-desktop')},m=>{m['people-midbody-permission-desktop'].muxerContinue=0},m=>{m['people-midbody-permission-phone'].refusedContinue=1},m=>{m['people-midbody-permission-phone'].frontBytes=0},m=>{m['people-midbody-permission-phone'].frontStatus=200}]) {
    const copy=structuredClone(manifest);mutate(copy);
    assert.throws(()=>verifyMidbodyWitness(copy));
  }
});
test('local budget proof requires real storage refusal and exactly one publication',()=>{
  const manifest=evidence();verifyLocalBudget(manifest);
  for(const mutate of [m=>delete m.assertions,m=>{m.assertions=m.assertions.filter(x=>x.id!=='people-local-budget-refusal-phone')},m=>{m['people-local-budget-desktop'].status=503},m=>{m['people-local-budget-phone'].code='quota'},m=>{m['people-local-budget-phone'].commentCount=2}]) {
    const copy=structuredClone(manifest);mutate(copy);assert.throws(()=>verifyLocalBudget(copy));
  }
});
test('manifest verifier commands accept complete evidence and reject absent evidence',()=>{
  const dir=fs.mkdtempSync(path.join(os.tmpdir(),'scimux-witness-test-'));
  try {
    const manifest=path.join(dir,'manifest.json');
    for(const helper of ['verify-midbody-witness.cjs','verify-local-budget.cjs']) {
      const script=fileURLToPath(new URL('./testdata/team-e2e/'+helper,import.meta.url));
      fs.writeFileSync(manifest,JSON.stringify(evidence()));
      assert.equal(spawnSync(process.execPath,[script,manifest],{encoding:'utf8'}).status,0);
      fs.writeFileSync(manifest,JSON.stringify({assertions:[]}));
      assert.notEqual(spawnSync(process.execPath,[script,manifest],{encoding:'utf8'}).status,0);
    }
  } finally {fs.rmSync(dir,{recursive:true,force:true});}
});
