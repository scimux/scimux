import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { sharedActivityId, sharedActivityGroups, sharedPinnedOrder, readActivityFolds, toggleActivityFold } from '../js/team-guest.js';

const sub = (server, owner, chat, extra={}) => ({server, owner, chat, generation:'g', owner_name:'Alex', server_name:'Lab', title:chat, first_seen:'1', state:'live', access:'write', unread:false, ...extra});
test('activity section headers are flat warm labels with a perforated separator',()=>{
 const css=readFileSync(new URL('../css/cards.css',import.meta.url),'utf8');
 const rule=css.match(/#cardlist \.activity-group\s*\{([^}]*)\}/)?.[1] || '';
 assert.match(rule,/background:\s*var\(--bg\)/);assert.match(rule,/border:\s*0/);
 assert.match(rule,/border-bottom:\s*1px dashed var\(--perf\)/);assert.match(rule,/min-height:\s*44px/);
});
test('shared identities are namespaced and group by server and owner, never by name', () => {
  const a=sub('s1','o1','a'), b=sub('s2','o1','b'), c=sub('s1','o2','c'), mine=sub('s1','self','m',{access:'owner'});
  assert.notEqual(sharedActivityId(a),sharedActivityId(b));
  assert.match(sharedActivityId(a),/^team\//);
  const groups=sharedActivityGroups([a,b,c,mine],[],{});
  assert.equal(groups.length,3); assert.deepEqual(groups.flatMap(g=>g.items.map(x=>x.chat)).sort(),['a','b','c']);
  assert.equal(new Set(groups.map(g=>g.key)).size,3);
});
test('mixed pins preserve stored order with hard attention local first', () => {
  const shared=sub('s','o','c'), id=sharedActivityId(shared), local={id:'local'}, urgent={id:'urgent',attention:'approval'};
  assert.deepEqual(sharedPinnedOrder([local,urgent],[shared],[id,'local','urgent']).map(x=>x.id),['urgent',id,'local']);
});
test('fold storage tolerates malformed and refused storage and persists changes', () => {
  const data=new Map(); const storage={getItem:k=>data.get(k) || null,setItem:(k,v)=>data.set(k,v)};
  assert.deepEqual([...readActivityFolds(storage)],[]);
  let folds=toggleActivityFold(storage,readActivityFolds(storage),'mine'); assert.equal(folds.has('mine'),true);
  assert.equal(readActivityFolds(storage).has('mine'),true);
  folds=toggleActivityFold(storage,folds,'mine'); assert.equal(folds.has('mine'),false);
  data.set('scimux-activity-folds','{'); assert.equal(readActivityFolds(storage).size,0);
  assert.equal(toggleActivityFold({setItem(){throw Error('denied')}},new Set(),'x').has('x'),true);
});

import { createCardsFeature } from '../js/cards.js';
function cardsFixture({nodes=[],views=[],pinned=[],archived=[],tab='current',storage: suppliedStorage, selected='', actionCard='', editingTitle='', icons={}}={}) {
 const handlers={}; let sig=''; let foldStore=new Map(); const changes=[]; const calls=[];
 const list={innerHTML:'',querySelectorAll:()=>[],addEventListener:(n,f)=>{handlers[n]=f},removeEventListener(){}};
 const tabs={innerHTML:'',querySelectorAll:()=>[],addEventListener(){},removeEventListener(){}};
 const storage=suppliedStorage || {getItem:k=>foldStore.get(k)||null,setItem:(k,v)=>foldStore.set(k,v)};
 const sharedTab={tabHTML:()=>'<button data-tab="shared">Shared with me</button>', activities:()=>views, groups:()=>sharedActivityGroups(views,[],{}), mineUnread:()=>views.some(v=>v.access==='owner' && v.unread), mineUnknown:()=>views.some(v=>v.access==='owner' && v.unreadStatus==='unknown'), ownerUnread:id=>views.some(v=>v.access==='owner' && v.local_node===id && v.unread), ownerUnreadStatus:id=>views.find(v=>v.access==='owner' && v.local_node===id)?.unreadStatus || 'read', selected:()=>selected, open:id=>calls.push(['open',id])};
 const feature=createCardsFeature({roots:{tabs,list},document:{title:'',querySelector:()=>null},CSS:{escape:s=>s},sharedTab,storage,icons,actionCard:()=>actionCard,editingTitle:()=>editingTitle,editingTitleScope:()=>"cards",nodes:()=>nodes,cardTab:()=>tab,setCardTab:v=>{tab=v},cardsSig:()=>sig,setCardsSig:v=>{sig=v},pinned:()=>pinned,archived:()=>archived,expanded:()=>new Set(),uiMutate:x=>changes.push(x),api:(...x)=>{calls.push(['api',...x]);},exitThread:id=>calls.push(['exit',id]),selectNode:id=>calls.push(['local',id]),setInterval:()=>0,clearInterval:()=>{}});
 feature.bind(); return {feature,list,tabs,handlers,changes,calls,storage,setTab:v=>{tab=v},tab:()=>tab};
}
test('owner unread is shown on the matching local card even when Mine is already unread',()=>{
 const views=[sub('s','self','a',{access:'owner',local_node:'a',unread:true}),sub('s','self','b',{access:'owner',local_node:'b',unread:false})];
 const f=cardsFixture({nodes:[{id:'a',title:'A'},{id:'b',title:'B'}],views}); f.feature.render();
 const card=id=>f.list.innerHTML.split('id="card-'+id+'"')[1].split('</div>')[0];
 assert.match(card('a'),/activity-unread[^]*<button class="chev"/);
 assert.doesNotMatch(card('b'),/activity-unread/);
 views[1].unread=true; f.feature.render(); assert.match(card('b'),/activity-unread[^]*<button class="chev"/);
});
test('owner card announces unavailable unread status without showing an unread dot',()=>{
 const f=cardsFixture({nodes:[{id:'a',title:'A'}],views:[sub('s','self','a',{access:'owner',local_node:'a',unreadStatus:'unknown'})]}); f.feature.render();
 const card=f.list.innerHTML.split('id="card-a"')[1].split('</div>')[0];
 assert.match(card,/Unread status unavailable/); assert.doesNotMatch(card,/activity-unread/);
});
test('unknown shared and owner statuses remain visible through folds and local card controls',async()=>{
 const other=sub('s','o','c',{unreadUnknown:true}), owner=sub('s','self','a',{access:'owner',local_node:'a',unreadStatus:'unknown'});
 const f=cardsFixture({nodes:[{id:'a',title:'Edit me'},{id:'b',title:'Ended',ended_at:'t'}],views:[other,owner],editingTitle:'a'});f.feature.render();
 assert.match(f.list.innerHTML,/data-title-input="a"/);assert.match(f.list.innerHTML,/Unread status unavailable/);
 await f.handlers.click({target:target('[data-activity-group]',{activityGroup:'owner/s/o'})});assert.match(f.list.innerHTML,/Unread status unavailable/);
 const archived=cardsFixture({nodes:[{id:'b',title:'Ended',ended_at:'t'}],archived:['b'],tab:'archived'});archived.feature.render();
 assert.match(archived.list.innerHTML,/aria-label="restore"/);assert.doesNotMatch(archived.list.innerHTML,/aria-label="close thread/);
});
test('All uses local cards and grouped received cards while Shared omits owner copies',()=>{
 const mine=sub('s','self','copy',{access:'owner'}), other=sub('s','o','received',{unread:true});
 const f=cardsFixture({nodes:[{id:'local',title:'Local',live:'quiet'}],views:[mine,other]}); f.feature.render();
 assert.match(f.list.innerHTML,/Mine/); assert.match(f.list.innerHTML,/id="card-local"/);
 assert.match(f.list.innerHTML,/class="card[^\"]*"[^>]*id="card-team\//); assert.doesNotMatch(f.list.innerHTML,/card-team\/s\/self/);
 f.setTab('shared'); f.feature.invalidate(); f.feature.render(); assert.doesNotMatch(f.list.innerHTML,/card-local|card-team\/s\/self/); assert.match(f.list.innerHTML,/card-team\/s\/o/);
});
test('forged recipient control actions never invoke local agent routes',async()=>{
 const other=sub('s','o','received'), id=sharedActivityId(other); const f=cardsFixture({views:[other]});
 for(const [selector,key] of [['[data-exit]','exit'],['[data-save-desc]','saveDesc'],['[data-trash]','trash'],['[data-arch-action]','archAction']]) {
  const el={dataset:{[key]:id},closest:s=>s===selector?el:null}; await f.handlers.click({target:el});
 }
 assert.deepEqual(f.calls,[]); assert.deepEqual(f.changes,[]);
});

function target(selector, dataset) { const node={dataset,closest:s=>s===selector?node:null}; return node; }
test('fold headings retain unread and persistence without hiding urgent local cards', async () => {
 const views=[sub('s','o','c',{unread:true})];
 const f=cardsFixture({nodes:[{id:'normal',title:'Normal',live:'quiet'},{id:'urgent',title:'Urgent',attention:'approval'}],views});
 f.feature.render();
 await f.handlers.click({target:target('[data-activity-group]',{activityGroup:'owner/s/o'})});
 assert.match(f.list.innerHTML,/aria-expanded="false"[^>]*>[^]*aria-label="unread"/);
 assert.doesNotMatch(f.list.innerHTML,/card-team\/s\/o/);
 assert.equal(readActivityFolds(f.storage).has('owner/s/o'),true);
 await f.handlers.click({target:target('[data-activity-group]',{activityGroup:'mine'})});
 assert.doesNotMatch(f.list.innerHTML,/card-normal/); assert.match(f.list.innerHTML,/card-urgent/);
 assert.equal(readActivityFolds(f.storage).has('mine'),true);
 await f.handlers.click({target:target('[data-activity-group]',{activityGroup:'mine'})});
 assert.match(f.list.innerHTML,/card-normal/);
});
test('shared pin and open route through the subscription adapter only', async()=>{
 const view=sub('s','o','c'); const id=sharedActivityId(view); const f=cardsFixture({views:[view]}); f.feature.render();
 await f.handlers.click({target:target('[data-pin-action]',{pinAction:id})});
 assert.deepEqual(f.changes,[{k:'pin',id}]);
 await f.handlers.click({target:target('[data-open]',{open:id})});
 assert.deepEqual(f.calls,[['open',id]]);
 await f.handlers.click({target:target('[data-pin-action]',{pinAction:'team/unknown'})});
 assert.equal(f.changes.length,1);
});
test('pinned tab mixes shared and local in manual order and falls back only after last visible pin',()=>{
 const view=sub('s','o','c'), id=sharedActivityId(view), f=cardsFixture({nodes:[{id:'local',title:'Local'}],views:[view],pinned:[id,'local','unknown'],tab:'pinned'});
 f.feature.render(); assert.ok(f.list.innerHTML.indexOf('card-'+id) < f.list.innerHTML.indexOf('card-local'));
 assert.doesNotMatch(f.list.innerHTML,/activity-group/);
 const empty=cardsFixture({views:[],pinned:['unknown'],tab:'pinned'}); empty.feature.render(); assert.equal(empty.tab(),'current');
});
test('fold readers filter wrong types and tolerate storage failures',()=>{
 assert.deepEqual([...readActivityFolds({getItem:()=>JSON.stringify(['mine',12,null,'owner/s/o'])})],['mine','owner/s/o']);
 assert.equal(readActivityFolds({getItem:()=>'{'}).size,0);
 assert.equal(readActivityFolds({getItem:()=>'{"x":1}'}).size,0);
 assert.equal(readActivityFolds({getItem(){throw Error('no')}}).size,0);
});

test('fallback values and fold state are safe for absent optional inputs',()=>{
 assert.deepEqual(sharedActivityGroups(null,null,null),[]);
 assert.deepEqual(sharedPinnedOrder(null,null,null),[]);
 assert.equal(readActivityFolds().size,0);
 assert.equal(toggleActivityFold(undefined,new Set(),'mine').has('mine'),true);
});
test('shared cards show truthful ended/read status and mixed pinned control',()=>{
 const view=sub('s','o','',{title:'',access:'read',state:'ended'}), id=sharedActivityId(view);
 const f=cardsFixture({views:[view],pinned:[id],tab:'pinned'}); f.feature.render();
 assert.match(f.list.innerHTML,/Untitled chat/); assert.match(f.list.innerHTML,/Sharing ended/); assert.match(f.list.innerHTML,/aria-label="unpin"/);
 assert.doesNotMatch(f.list.innerHTML,/working|approval|data-exit|data-trash|data-title/);
});
test('received activity uses the common vendor-logo slot and no inferred member count',()=>{
 const view=sub('s','o','c',{members:42});const f=cardsFixture({views:[view],icons:{ICON_USERS:'<svg data-icon="users"></svg>'}});
 f.feature.render();assert.match(f.list.innerHTML,/<span class="agent-logo"[^>]*><svg data-icon="users"><\/svg><\/span>/);
 assert.doesNotMatch(f.list.innerHTML,/42 (?:people|members)/);
});
test('group fold state survives re-instantiation with a shared storage adapter',async()=>{
 const f=cardsFixture({views:[sub('s','o','c',{unread:true})]}); f.feature.render();
 await f.handlers.click({target:target('[data-activity-group]',{activityGroup:'owner/s/o'})});
 const keys=readActivityFolds(f.storage); assert.equal(keys.has('owner/s/o'),true);
 assert.equal(toggleActivityFold(f.storage,keys,'owner/s/o').size,0);
 assert.equal(readActivityFolds(f.storage).size,0);
});

import { applyOp } from '../js/state.js';
import { pinOrderAfterDrag, permuteSlots, withCardEditsPreserved } from '../js/cards.js';
test('mixed pin mutations and drag preserve unknown pins',()=>{
 const id=sharedActivityId(sub('s','o','c')); const doc={pinned:['unknown','local']};
 applyOp(doc,{k:'pin',id}); assert.deepEqual(doc.pinned,[id,'unknown','local']);
 applyOp(doc,{k:'pin-order',pinned:pinOrderAfterDrag(['local',id],doc.pinned)});
 assert.deepEqual(doc.pinned,['local',id,'unknown']);
 applyOp(doc,{k:'unpin',id}); assert.deepEqual(doc.pinned,['local','unknown']);
});
test('reordering local card slots leaves shared groups in place',()=>{
 assert.deepEqual(permuteSlots(['head-mine','card-a','card-b','head-owner','card-shared'],['card-b','card-a']),['head-mine','card-b','card-a','head-owner','card-shared']);
});
test('card rebuild restores scroll, focused edit and selection',()=>{
 const field={dataset:{descInput:'local'},value:'draft',selectionStart:1,selectionEnd:3,focus(){},setSelectionRange(a,b){this.range=[a,b]}};
 const doc={activeElement:field}; const root={scrollTop:120,querySelectorAll:()=>[field]};
 withCardEditsPreserved(root,'local',()=>{root.scrollTop=0;field.value='server'}, {document:doc});
 assert.equal(root.scrollTop,120); assert.equal(field.value,'draft'); assert.deepEqual(field.range,[1,3]);
});
test('shared card selection, action state, read and write remain honest',()=>{
 const view=sub('s','o','c',{access:'read'}), id=sharedActivityId(view);
 const f=cardsFixture({views:[view],selected:id,actionCard:id,icons:{ICON_PIN:'pin',ICON_UNPIN:'unpin'}}); f.feature.render();
 assert.match(f.list.innerHTML,/shared-card selected actions-open/); assert.match(f.list.innerHTML,/Can view/); assert.doesNotMatch(f.list.innerHTML,/data-exit|data-trash|data-save-desc/);
});
test('mixed pinned view unpins shared without touching unrelated pins',async()=>{
 const view=sub('s','o','c'), id=sharedActivityId(view); const f=cardsFixture({views:[view],pinned:[id,'unknown'],tab:'pinned'}); f.feature.render();
 await f.handlers.click({target:target('[data-pin-action]',{pinAction:id})}); assert.deepEqual(f.changes,[{k:'unpin',id}]);
});

test('liveness patches preserve shared group boundaries and the focused editor',()=>{
 const view=sub('s','o','c'), id=sharedActivityId(view);
 const nodes=[{id:'local',title:'Local',live:'quiet'}]; let sig='',writes=0; const moves=[];
 const editor={dataset:{descInput:'local'},value:'draft',focus(){},setSelectionRange(){}};
 const localStatus={innerHTML:''}, sharedStatus={innerHTML:'Can comment'};
 const card=(id,status)=>({id:'card-'+id,classList:{add(){},remove(){}},querySelector:s=>s==='.status'?status:null});
 const local=card('local',localStatus), shared=card(id,sharedStatus), head={id:''}, otherHead={id:''};
 const list={children:[head,local,otherHead,shared],scrollTop:20,get innerHTML(){return ''},set innerHTML(v){writes++},querySelectorAll(s){if(s==='.card')return [local,shared];if(s==='input, textarea, select')return [editor];return []},insertBefore(...args){moves.push(args)},addEventListener(){},removeEventListener(){}};
 const doc={title:'',activeElement:editor};
 const tab={tabHTML:()=>'',activities:()=>[view],groups:()=>sharedActivityGroups([view],[],{})};
 const feature=createCardsFeature({roots:{list,tabs:{innerHTML:''}},document:doc,sharedTab:tab,nodes:()=>nodes,cardTab:()=> 'current',cardsSig:()=>sig,setCardsSig:v=>{sig=v},pinned:()=>[],archived:()=>[],expanded:()=>new Set(),editingDesc:()=> 'local',setInterval:()=>0,clearInterval(){}});
 feature.render(); assert.equal(writes,1); nodes[0].live='active'; feature.render();
 assert.equal(writes,1); assert.equal(doc.activeElement,editor); assert.equal(editor.value,'draft'); assert.equal(sharedStatus.innerHTML,'Can comment'); assert.deepEqual(moves,[]);
});

test('empty and collapsed groups render the right empty state',async()=>{
 const empty=cardsFixture(); empty.feature.render(); assert.match(empty.list.innerHTML,/start the first one/);
 empty.setTab('shared'); empty.feature.invalidate(); empty.feature.render(); assert.equal(empty.tab(),'current'); assert.match(empty.list.innerHTML,/start the first one/);
 const f=cardsFixture({nodes:[{id:'normal',title:'Normal'}]}); f.feature.render();
 await f.handlers.click({target:target('[data-activity-group]',{activityGroup:'mine'})});
 assert.doesNotMatch(f.list.innerHTML,/card-normal|start the first one/);
});
test('missing pin lists and invalid ids do not create subscriptions',()=>{
 assert.deepEqual(sharedPinnedOrder([{id:'local'}],[],undefined),[]);
 const view=sub('s','o','c'); assert.deepEqual(sharedPinnedOrder([], [view],undefined),[]);
});
test('pinned liveness patch does not fabricate shared status',()=>{
 const view=sub('s','o','c'),id=sharedActivityId(view), nodes=[{id:'local',title:'Local',live:'quiet'}]; let sig='',writes=0;
 const localStatus={innerHTML:''},sharedStatus={innerHTML:'Can comment'};
 const card=(id,status)=>({id:'card-'+id,classList:{add(){},remove(){}},querySelector:s=>s==='.status'?status:null});
 const local=card('local',localStatus),shared=card(id,sharedStatus);
 const list={children:[shared,local],set innerHTML(v){writes++},querySelectorAll:s=>s==='.card'?[shared,local]:[],addEventListener(){},removeEventListener(){},insertBefore(){}};
 const feature=createCardsFeature({roots:{list,tabs:{innerHTML:''}},document:{},sharedTab:{tabHTML:()=>'',activities:()=>[view],groups:()=>[]},nodes:()=>nodes,cardTab:()=> 'pinned',pinned:()=>[id,'local'],archived:()=>[],cardsSig:()=>sig,setCardsSig:v=>{sig=v},expanded:()=>new Set()});
 feature.render(); nodes[0].live='active'; feature.render(); assert.equal(writes,1); assert.equal(sharedStatus.innerHTML,'Can comment');
});
test('opening an unknown shared identifier never selects a local node',async()=>{
 const f=cardsFixture(); await f.handlers.click({target:target('[data-open]',{open:'team/unknown'})});
 assert.deepEqual(f.calls,[['open','team/unknown']]); assert.equal(f.calls.some(x=>x[0]==='local'),false);
});

test('owner unread comments remain visible on collapsed Mine without a duplicate card',async()=>{
 const owned=sub('s','self','mine',{access:'owner',unread:true}); const f=cardsFixture({nodes:[{id:'local',title:'Local'}],views:[owned]});
 f.feature.render(); await f.handlers.click({target:target('[data-activity-group]',{activityGroup:'mine'})});
 assert.match(f.list.innerHTML,/aria-expanded="false"[^>]*>[^]*aria-label="unread"/);
 assert.doesNotMatch(f.list.innerHTML,/card-team\/s\/self/);
});

test('mixed pinned drag reorders the same UI pin array and keeps offscreen pins',()=>{
 const view=sub('s','o','c'),id=sharedActivityId(view),pinned=[id,'local','unknown'],handlers={},effects=[];
 const cls=()=>({contains:()=>false,add(){},remove(){}});
 const shared={classList:cls(),querySelector:()=>({dataset:{open:id}})},local={classList:cls(),querySelector:()=>({dataset:{open:'local'}}),getBoundingClientRect:()=>({top:0,height:100})};
 let moved=false; local.parentNode={insertBefore(el,before){assert.equal(el,shared);moved=true}};
 const list={innerHTML:'',querySelectorAll:s=>s==='.card'?[local,shared]:[],addEventListener:(k,f)=>{handlers[k]=f},removeEventListener(){}};
 const feature=createCardsFeature({roots:{list,tabs:{innerHTML:'',addEventListener(){},removeEventListener(){},querySelectorAll:()=>[]}},document:{},sharedTab:{activities:()=>[view],groups:()=>[],tabHTML:()=>''},nodes:()=>[{id:'local',title:'Local'}],cardTab:()=> 'pinned',pinned:()=>pinned,archived:()=>[],expanded:()=>new Set(),uiMutate:op=>effects.push(op),setInterval:()=>0,clearInterval(){}});
 feature.bind(); let prevented=0; handlers.dragstart({target:{closest:()=>shared},dataTransfer:{setData(){}}});
 handlers.dragover({target:{closest:()=>local},clientY:75,preventDefault(){prevented++}}); handlers.drop({preventDefault(){prevented++}}); handlers.dragend();
 assert.equal(moved,true); assert.equal(prevented,2); assert.deepEqual(effects,[{k:'pin-order',pinned:['local',id,'unknown']}]); feature.destroy();
});

test('shared long presses never edit local titles or descriptions but reveal pin actions',()=>{
 const view=sub('s','o','c'),id=sharedActivityId(view),handlers={},effects=[]; let action='';
 const list={innerHTML:'',querySelectorAll:()=>[],addEventListener(){},removeEventListener(){}};
 const feature=createCardsFeature({roots:{list,tabs:{innerHTML:'',addEventListener(){},removeEventListener(){}}},document:{querySelector:()=>null},sharedTab:{activities:()=>[view],groups:()=>[],tabHTML:()=>''},nodes:()=>[{id:'local',title:'Local'}],cardTab:()=> 'current',pinned:()=>[],archived:()=>[],expanded:()=>new Set(),actionCard:()=>action,setActionCard:v=>{action=v},setEditingDesc:v=>effects.push(['description',v]),startTitleEdit:(...v)=>effects.push(['title',...v]),longpress:(_root,selector,fn)=>{handlers[selector]=fn;return ()=>{}},setInterval:()=>0,clearInterval(){}});
 feature.bind(); handlers['.card .title']({dataset:{title:id}}); handlers['.card .title']({dataset:{}});
 handlers['.summary']({dataset:{desc:id}}); assert.deepEqual(effects,[]);
 const card={classList:{contains:()=>false},closest:()=>null,querySelector:()=>({dataset:{open:id}})};
 const unknown={...card,querySelector:()=>({dataset:{open:'team/unknown'}})};
 handlers['.card'](unknown,{target:{closest:()=>null}}); assert.equal(action,'');
 handlers['.card'](card,{target:{closest:()=>null}}); assert.equal(action,id); assert.deepEqual(effects,[['description','']]);
 handlers['.card .title']({dataset:{title:'local'}}); assert.deepEqual(effects.at(-1),['title','local','cards']);
 feature.destroy();
});

test('toggling a group restores keyboard focus and the stored choice after reconnect',async()=>{
 const view=sub('s','o','c'), first=cardsFixture({views:[view]}); let focused=0,selector='';
 first.list.querySelector=s=>{selector=s;return {focus(){focused++}}}; first.feature.render();
 await first.handlers.click({target:target('[data-activity-group]',{activityGroup:'owner/s/o'})});
 assert.equal(focused,1); assert.match(selector,/owner\/s\/o/);
 const second=cardsFixture({views:[view],storage:first.storage}); second.feature.render();
 assert.match(second.list.innerHTML,/aria-expanded="false"/); assert.doesNotMatch(second.list.innerHTML,/card-team\/s\/o/);
});

test('pinned received card refreshes metadata and selection without manual invalidation', () => {
 const views=[sub('s','o','c',{title:'First',access:'write',unread:false})]; const id=sharedActivityId(views[0]);
 let selected=''; let sig='',writes=0,html=''; const list={get innerHTML(){return html},set innerHTML(v){html=v;writes++},querySelectorAll:()=>[]};
 const tab={activities:()=>views,groups:()=>[],selected:()=>selected,tabHTML:()=>''};
 const cards=createCardsFeature({roots:{list,tabs:{innerHTML:''}},document:{},nodes:()=>[],cardTab:()=> 'pinned',pinned:()=>[id],sharedTab:tab,cardsSig:()=>sig,setCardsSig:v=>{sig=v}});
 cards.render(); assert.match(html,/First/); assert.match(html,/Can comment/); assert.equal(writes,1);
 cards.render(); assert.equal(writes,1);
 views[0]={...views[0],title:'Second',access:'read',unread:true}; cards.render();
 assert.match(html,/Second/); assert.match(html,/Can view/); assert.match(html,/aria-label="unread"/);
 views[0]={...views[0],state:'ended'}; cards.render(); assert.match(html,/Sharing ended/);
 selected=id; cards.render(); assert.match(html,/shared-card selected/);
 selected=''; cards.render(); assert.doesNotMatch(html,/shared-card selected/);
});

test('last received removal falls back from Shared to All and retains unrelated state',()=>{
 const views=[sub('s','o','c')], id=sharedActivityId(views[0]); const pinned=[id,'unknown'];
 const f=cardsFixture({nodes:[{id:'local',title:'Local'}],views,pinned,tab:'shared'});
 toggleActivityFold(f.storage,new Set(),'owner/s/o'); f.feature.render(); assert.equal(f.tab(),'shared');
 views.splice(0,1); f.feature.render();
 assert.equal(f.tab(),'current'); assert.match(f.tabs.innerHTML,/data-tab="current" class="on"/);
 assert.match(f.list.innerHTML,/card-local/); assert.deepEqual(pinned,[id,'unknown']);
 assert.equal(readActivityFolds(f.storage).has('owner/s/o'),true);
});

test('pinned metadata rebuild preserves a local editor while liveness changes patch in place',()=>{
 const views=[sub('s','o','c',{title:'Before'})], id=sharedActivityId(views[0]); const nodes=[{id:'local',title:'Local',live:'quiet'}];
 let sig='',html='',writes=0; const editor={dataset:{descInput:'local'},value:'draft',selectionStart:1,selectionEnd:3,focus(){this.focused=true},setSelectionRange(a,b){this.range=[a,b]}};
 const localStatus={innerHTML:''},sharedStatus={innerHTML:'Can comment'};
 const card=(id,status)=>({id:'card-'+id,classList:{add(){},remove(){}},querySelector:s=>s==='.status'?status:null});
 const local=card('local',localStatus),shared=card(id,sharedStatus);
 const list={children:[shared,local],scrollTop:123,get innerHTML(){return html},set innerHTML(v){html=v;writes++;editor.value='server';this.scrollTop=0},querySelectorAll:s=>s==='.card'?[shared,local]:s==='input, textarea, select'?[editor]:[],insertBefore(){}};
 const doc={activeElement:editor}; const tab={activities:()=>views,groups:()=>[],tabHTML:()=>''};
 const cards=createCardsFeature({roots:{list,tabs:{innerHTML:''}},document:doc,sharedTab:tab,nodes:()=>nodes,cardTab:()=> 'pinned',pinned:()=>[id,'local'],archived:()=>[],cardsSig:()=>sig,setCardsSig:v=>{sig=v},expanded:()=>new Set(),editingDesc:()=> 'local'});
 cards.render(); assert.equal(writes,1); assert.equal(editor.value,'draft'); assert.equal(list.scrollTop,123);
 views[0]={...views[0],title:'After',access:'read',unread:true}; cards.render();
 assert.equal(writes,2); assert.match(html,/After/); assert.equal(editor.value,'draft'); assert.deepEqual(editor.range,[1,3]); assert.equal(list.scrollTop,123);
 nodes[0].live='active'; cards.render(); assert.equal(writes,2); assert.equal(sharedStatus.innerHTML,'Can comment'); assert.equal(editor.value,'draft');
});

test('Mine unread header refreshes when owner comments become unread without invalidation',()=>{
 const views=[sub('s','self','mine',{access:'owner',unread:false})]; const f=cardsFixture({nodes:[{id:'local',title:'Local'}],views});
 f.feature.render(); assert.doesNotMatch(f.list.innerHTML,/aria-label="unread"/);
 views[0]={...views[0],unread:true}; f.feature.render(); assert.match(f.list.innerHTML,/aria-label="unread"/);
});

test('a lane sub-group indents its header and colors only a safe stripe', () => {
  const badName = '<img src=x onerror=1>';
  const bad = sub('s','o','idx',{title:'Shared lane · bad', lane:{name:badName, color:'red;background:url(x)', chats:[{chat:'bad'}]}});
  const good = sub('s','o','gidx',{title:'Shared lane · Good', lane:{name:'Good', color:'#11aa22', chats:[{chat:'c'}]}});
  const upper = sub('s','o','uidx',{title:'Shared lane · Upper', lane:{name:'Upper', color:'#ABCDEF', chats:[{chat:'q'}]}});
  const ask = sub('s','o','aidx',{title:'Shared lane · Ask', lane:{name:'Ask', chats:[{chat:'u'}]}});
  const chat = sub('s','o','c',{title:'Chat', unread:true});
  const unknown = sub('s','o','u',{title:'Unknown', unreadUnknown:true});
  const quiet = sub('s','o','q',{title:'Quiet'});
  const poisoned = sub('s','o','bad',{title:'Poison'});
  const loose = sub('s','o','loose',{title:'Loose'});
  const f = cardsFixture({views:[bad, good, upper, ask, chat, unknown, quiet, poisoned, loose]});
  f.feature.render();
  const html = f.list.innerHTML;
  assert.match(html, /class="attnfold activity-group lane-group lane-indent open" data-activity-group="owner\/s\/o\/lane\/gidx"/);
  assert.match(html, /data-activity-group="owner\/s\/o\/lane\/gidx"[^]*<span class="laneswatch" style="background:#11aa22"><\/span><span class="foldlabel">Good<\/span><span class="activity-unread" aria-label="unread">/);
  assert.match(html, /data-activity-group="owner\/s\/o\/lane\/aidx"[^]*<span class="laneswatch"><\/span><span class="foldlabel">Ask<\/span><span title="Unread status unavailable"/);
  assert.match(html, /class="card shared-card[^"]*lane-indent[^"]*"\s+id="card-team\/s\/o\/c\/g">[\s\S]*?<span class="stripe" style="background:#11aa22"><\/span>/);
  const looseCard = html.match(/class="(card shared-card[^"]*)"\s+id="card-team\/s\/o\/loose\/g">[\s\S]*?<span class="stripe"><\/span>/);
  assert.equal(looseCard[1].includes("lane-indent"), false);
  assert.doesNotMatch(html, /card-team\/s\/o\/idx/);
  assert.doesNotMatch(html, /<img/);
  assert.match(html, /foldlabel">&lt;img src=x onerror=1<\/span>/);
  assert.doesNotMatch(html, /red;background|url\(x\)|#ABCDEF/);
  const owner = html.match(/<button class="([^"]*)" data-activity-group="owner\/s\/o"/);
  assert.equal(owner[1].includes('lane-group'), false);
  assert.equal((html.match(/card-team\/s\/o\/c\/g/g) || []).length, 1);
});

test('a lane sub-group folds on its own key', async () => {
  const good = sub('s','o','gidx',{lane:{name:'Good', color:'#11aa22', chats:[{chat:'c'}]}});
  const chat = sub('s','o','c',{unread:true});
  const f = cardsFixture({views:[good, chat]});
  f.feature.render();
  assert.match(f.list.innerHTML, /card-team\/s\/o\/c\/g/);
  await f.handlers.click({target:target('[data-activity-group]',{activityGroup:'owner/s/o/lane/gidx'})});
  assert.doesNotMatch(f.list.innerHTML, /card-team\/s\/o\/c\/g/);
  assert.match(f.list.innerHTML, /data-activity-group="owner\/s\/o\/lane\/gidx"[^>]*aria-expanded="false"/);
  assert.equal(readActivityFolds(f.storage).has('owner/s/o/lane/gidx'), true);
});

test('lane sub-groups drop the dashed rule and indent 12px', () => {
  const file = readFileSync(new URL('../css/cards.css', import.meta.url), 'utf8');
  assert.match(file, /#cardlist \.activity-group\.lane-group\s*\{[^}]*border-bottom:\s*0/);
  assert.match(file, /#cardlist \.activity-group\.lane-indent\s*\{[^}]*margin-left:\s*12px/);
  assert.match(file, /#cardlist \.card\.lane-indent\s*\{[^}]*margin-left:\s*12px/);
});

test('a temporarily empty grouping with a retained subscription shows a shared empty state',()=>{
 const views=[sub('s','o','c')], list={innerHTML:'',querySelectorAll:()=>[]}; let tab='shared';
 const cards=createCardsFeature({roots:{list,tabs:{innerHTML:''}},document:{},nodes:()=>[],cardTab:()=>tab,setCardTab:v=>{tab=v},sharedTab:{activities:()=>views,groups:()=>[],tabHTML:()=>''},pinned:()=>[],archived:()=>[],expanded:()=>new Set()});
 cards.render(); assert.equal(tab,'shared'); assert.match(list.innerHTML,/No shared activities yet/);
});
