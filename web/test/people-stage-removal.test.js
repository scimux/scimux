import test from 'node:test';
import assert from 'node:assert/strict';
import {removePeopleStage} from '../js/people-stage-removal.js';

for(const [name,change] of [['tuple',{peopleKey:'../bad'}],['stage',{ref:{id:'../bad'}}],['copy',{copyInstance:'bad'}]]) test('refuses malformed '+name+' before dispatch',async()=>{
 const item={peopleKey:['1'.repeat(32),'2'.repeat(32),'3'.repeat(16),'4'.repeat(16)].join('/'),copyInstance:'5'.repeat(32),stageRevision:'6'.repeat(64),ref:{id:'0123456789abcdef'},...change};let calls=0;
 await assert.rejects(removePeopleStage(async()=>{calls++},item),/review/i);assert.equal(calls,0);
});

test('malformed local addresses are refused before any deletion request',async()=>{
 const base={peopleKey:['1'.repeat(32),'2'.repeat(32),'3'.repeat(16),'4'.repeat(16)].join('/'),copyInstance:'5'.repeat(32),stageRevision:'6'.repeat(64),ref:{id:'0123456789abcdef'}};
 const invalid=[null,{}, {...base,peopleKey:'../bad'}, {...base,peopleKey:42}, {...base,peopleKey:base.peopleKey+'/extra'},...base.peopleKey.split('/').map((_,i)=>({...base,peopleKey:base.peopleKey.split('/').map((part,j)=>j===i?'bad':part).join('/')})),{...base,ref:null},{...base,ref:{id:42}},{...base,ref:{id:'../bad'}},{...base,copyInstance:42},{...base,copyInstance:'bad'},{...base,stageRevision:42},{...base,stageRevision:'bad'}];
 for(const item of invalid){let calls=0;await assert.rejects(removePeopleStage(async()=>{calls++},item),/review/i);assert.equal(calls,0);}
});

test('removal uses the reviewed revision and refuses missing revision', async()=>{
 const calls=[], item={peopleKey:['1'.repeat(32),'2'.repeat(32),'3'.repeat(16),'4'.repeat(16)].join('/'),copyInstance:'a'.repeat(32),instance:'owner',ref:{id:'0123456789abcdef'},stageRevision:'b'.repeat(64)};
 await removePeopleStage(async(...args)=>calls.push(args),item);
 assert.match(calls[0][0],/revision=b{64}/); assert.equal(calls[0][1].method,'DELETE');
 await assert.rejects(removePeopleStage(async()=>assert.fail('must not delete'),{...item,stageRevision:undefined}),/review/i);
 await assert.rejects(removePeopleStage(null,item),/unavailable/i);
 await assert.rejects(removePeopleStage(async()=>{throw Error('replacement')},{...item,instance:''}),/replacement/);
 await removePeopleStage(async(...args)=>calls.push(args),{...item,instance:''});
});
