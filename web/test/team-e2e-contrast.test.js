import test from 'node:test';
import assert from 'node:assert/strict';
import {createRequire} from 'node:module';
const {contrastSample}=createRequire(import.meta.url)('./testdata/team-e2e/contrast.cjs');

test('contrast uses connected visible actual RGB samples',()=>{
 const result=contrastSample({connected:true,visible:true,text:'rgb(108, 108, 112)',background:'rgb(245, 244, 241)',caption:'You'});
 assert.ok(result.contrast>=4.5);assert.equal(result.caption,'You');
});
test('contrast rejects detached, hidden, transparent and malformed samples',()=>{
 for(const sample of [null,{connected:false,visible:true,text:'rgb(0,0,0)',background:'rgb(255,255,255)'},{connected:true,visible:false,text:'rgb(0,0,0)',background:'rgb(255,255,255)'},{connected:true,visible:true,text:null,background:'rgb(255,255,255)'},{connected:true,visible:true,text:'transparent',background:'rgb(255,255,255)'},{connected:true,visible:true,text:'rgb(999,0,0)',background:'rgb(255,255,255)'},{connected:true,visible:true,text:'rgb(0,0,0)',background:'rgba(0,0,0,0)'}])assert.equal(contrastSample(sample),null);
});
