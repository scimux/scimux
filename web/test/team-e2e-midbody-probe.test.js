import test from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const runner=fileURLToPath(new URL('./team-e2e.cjs',import.meta.url));

test('midbody fault probes cannot be enabled in the default suite or with an unknown mode',()=>{
  for(const [scenario,probe] of [['default','signal'],['people-midbody-permission','unknown']]) {
    const result=spawnSync(process.execPath,[runner],{env:{...process.env,SCIMUX_E2E_SCENARIO:scenario,SCIMUX_E2E_MIDBODY_PROBE:probe},encoding:'utf8',timeout:10000});
    assert.equal(result.status,2);assert.match(result.stderr,/midbody cleanup probe requires its selected scenario/);
    assert.doesNotMatch(result.stdout,/Artifacts:/);
  }
});
