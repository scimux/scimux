import test from 'node:test';
import assert from 'node:assert/strict';
import {createPairingFeature} from '../js/pairing-ui.js';

function fixture(api) {
 const ids=['pairsheet','pair_title','pair_body','pair_qr','pair_sas','pair_actions','pair_live','pair_x'];
 const els=new Map(ids.map(id=>{const listeners=new Map(); return [id,{hidden:false,value:'',innerHTML:'',textContent:'',addEventListener:(n,f)=>listeners.set(n,f),removeEventListener:n=>listeners.delete(n),setAttribute(){},click(action){listeners.get('click')?.({target:{closest:s=>s==='[data-pa]'?{dataset:{pa:action}}:null},preventDefault(){}})}}]}));
 let tick; const f=createPairingFeature({api,doc:{querySelector:s=>els.get(s.slice(1))||null},timers:{setInterval:fn=>(tick=fn,1),clearInterval(){}},now:()=>1000000}); f.bind();
 return {f,els,tick:()=>tick()};
}
async function open(h) {h.f.open(); h.els.get('pair_actions').click('begin'); h.els.get('pair_actions').click('ack'); await h.f.settled();}
test('pairing shows a clear fallback for a malformed hosted refusal',async()=>{
 const h=fixture(async()=>{const error=Error('not json');error.status=409;throw error}); await open(h);
 assert.equal(h.f.screen(),'failed'); assert.match(h.els.get('pair_body').innerHTML,/computer/i); h.f.destroy();
});
test('temporary pairing status read failures leave the pairing usable',async()=>{
 let calls=0; const h=fixture(async(path,opts)=>{calls++; if(opts?.method==='POST') return {code:'04106105',rid:'a'.repeat(64),link:'https://example.invalid/p',expires_at:'2030-01-01T00:00:00Z'}; throw Error('offline')});
 await open(h); h.tick(); await h.f.settled(); assert.equal(h.f.screen(),'show-code'); assert.ok(calls>=2); h.f.destroy();
});
