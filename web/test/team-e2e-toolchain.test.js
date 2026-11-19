import test from 'node:test';
import assert from 'node:assert/strict';
import {createRequire} from 'node:module';
import {spawnSync} from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
const require=createRequire(import.meta.url);
const {prepareTunnelGo}=require('./testdata/team-e2e/tunnel-toolchain.cjs');

test('tunnel child uses the approved Go executable even with a legacy go in its restricted PATH',()=>{
  const dir=fs.mkdtempSync(path.join(os.tmpdir(),'scimux-toolchain-test-'));
  try {
    const approvedDir=path.join(dir,'approved'),restricted=path.join(dir,'restricted');
    fs.mkdirSync(approvedDir);fs.mkdirSync(restricted);
    const approved=path.join(approvedDir,'go'),legacy=path.join(restricted,'go');
    fs.writeFileSync(approved,'#!/bin/sh\nif [ "$1" = version ]; then [ "$GOTOOLCHAIN" = local ] || exit 9; echo "go version go1.26.11 linux/amd64"; else echo approved-toolchain; fi\n',{mode:0o755});
    fs.writeFileSync(legacy,'#!/bin/sh\nif [ "$1" = version ]; then echo "go version go1.22.0 linux/amd64"; else echo legacy-toolchain; fi\n',{mode:0o755});
    const env={PATH:restricted,HOME:dir,GOTOOLCHAIN:'local'};
    const tool=prepareTunnelGo({selected:approved,inheritedPath:restricted,env});
    const child=spawnSync(tool.executable,['test'],{env,encoding:'utf8'});
    assert.equal(child.status,0);assert.equal(child.stdout.trim(),'approved-toolchain');
    assert.equal(tool.version,'go version go1.26.11 linux/amd64');
    assert.throws(()=>prepareTunnelGo({selected:legacy,inheritedPath:restricted,env}),/Go 1\.26/);
    assert.throws(()=>prepareTunnelGo({selected:'relative/go',inheritedPath:restricted,env}),/absolute/);
    assert.throws(()=>prepareTunnelGo({inheritedPath:restricted,env}),/Go 1\.26/);
    assert.throws(()=>prepareTunnelGo({selected:path.join(dir,'missing'),inheritedPath:restricted,env}),/verify/);
    assert.throws(()=>prepareTunnelGo({inheritedPath:'relative',env}),/Go 1\.26/);
    assert.throws(()=>prepareTunnelGo({env}),/Go 1\.26/);
    const found=prepareTunnelGo({inheritedPath:approvedDir,env});assert.equal(found.executable,fs.realpathSync(approved));
    const afterMissing=prepareTunnelGo({inheritedPath:path.join(dir,'missing')+path.delimiter+approvedDir,env});
    assert.equal(afterMissing.executable,fs.realpathSync(approved));
  } finally {fs.rmSync(dir,{recursive:true,force:true});}
});
