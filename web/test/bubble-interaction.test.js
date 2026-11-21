import test from 'node:test';
test('shared file actions never silently claim attachments follow text-only operations', async()=>{
 const {dispatchBubbleAction}=await import('../js/menu.js');const events=[];
 const adapter={copy:text=>events.push(['copy',text]),fork:text=>events.push(['fork',text]),sendTo:value=>events.push(['sendto',value.text]),attachmentNotice:()=>events.push(['notice']),unsupported:()=>events.push(['unsupported'])};
 const capture={text:'caption',attachments:[{id:'synthetic'}],shared:true};
 dispatchBubbleAction('copy',capture,adapter);assert.deepEqual(events,[['notice'],['copy','caption']]);events.length=0;
 dispatchBubbleAction('sendto',{...capture,text:''},adapter);assert.deepEqual(events,[['unsupported']]);
});
import assert from 'node:assert/strict';
import { activateBubble } from '../js/menu.js';
import * as commonBubble from '../js/menu.js';

test('one renderer combines a turn, source markers, speaker and content', () => {
 assert.equal(commonBubble.renderBubbleTurn({turnClass:'turn user',turnAttributes:'data-id="1"',bubbleAttributes:'title="t"',markers:'<i></i>',speakerName:'You',text:'hello',formatText:text=>'<p>'+text+'</p>'}),'<div class="turn user" data-id="1"><div class="bubble" title="t"><i></i><div class="gwho">You</div><p>hello</p></div></div>');
});

test('common renderer formats and escapes a provider-supplied speaker and body', () => {
 const html=commonBubble.renderBubbleTurn({turnClass:'turn',turnAttributes:'data-id="1"',bubbleAttributes:'tabindex="0"',markers:'',speakerName:'A < B',speakerAttributes:'data-seat="s"',text:'synthetic body',formatText:text=>'<p>'+text+'</p>'});
 assert.equal(html,'<div class="turn" data-id="1"><div class="bubble" tabindex="0"><div class="gwho" data-seat="s">A &lt; B</div><p>synthetic body</p></div></div>');
});

test('one action controller owns selection across ordinary and shared turns', () => {
 const events=[],controller=commonBubble.createBubbleActionController();
 const local={key:'local',close:()=>events.push('close local')},shared={key:'shared',close:()=>events.push('close shared')};
 assert.equal(controller.toggle(local),true);assert.equal(controller.current(),local);
 assert.equal(controller.toggle(shared),true);assert.deepEqual(events,['close local']);assert.equal(controller.current(),shared);
 assert.equal(controller.toggle(shared),false);assert.deepEqual(events,['close local','close shared']);assert.equal(controller.current(),null);
 controller.toggle(local);controller.clear();assert.equal(controller.current(),null);assert.deepEqual(events,['close local','close shared','close local']);
});

test('a rebuilt bubble with the same durable key can open its new action row', () => {
 let closed=0;const controller=commonBubble.createBubbleActionController(),old={key:'same',target:{},close:()=>closed++},fresh={key:'same',target:{},close:()=>closed++};
 assert.equal(controller.toggle(old),true);assert.equal(controller.toggle(fresh),true);assert.equal(closed,1);assert.equal(controller.current(),fresh);
});

test('one conversation controller owns click and key listeners for both providers', () => {
 const handlers=new Map(), events=[];
 const root={addEventListener:(kind,fn)=>{assert.equal(handlers.has(kind),false);handlers.set(kind,fn)},removeEventListener:(kind,fn)=>{assert.equal(handlers.get(kind),fn);handlers.delete(kind)}};
 const controller=commonBubble.createBubbleActionController();
 const removeLocal=controller.listen(root,{click:()=>events.push('local click'),keydown:()=>events.push('local key')});
 const removeShared=controller.listen(root,{click:()=>events.push('shared click'),keydown:()=>events.push('shared key')});
 assert.equal(handlers.size,2);handlers.get('click')({});handlers.get('keydown')({});
 assert.deepEqual(events,['local click','shared click','local key','shared key']);
 removeLocal();handlers.get('click')({});assert.equal(events.at(-1),'shared click');
 removeShared();assert.equal(handlers.size,0);
});

test('conversation viewport preserves an editor anchor or follows the tail, never both', () => {
 const controller=commonBubble.createBubbleActionController();let top=100;
 const body={scrollTop:20,scrollHeight:200,clientHeight:100},anchor={getBoundingClientRect:()=>({top})};
 const held=controller.captureViewport(body,{anchor});top=140;body.scrollHeight=240;
 controller.restoreViewport(held,{anchor,follow:true});assert.equal(body.scrollTop,60);
 body.scrollTop=130;const bottom=controller.captureViewport(body,{slack:10});body.scrollHeight=260;
 controller.restoreViewport(bottom,{follow:true});assert.equal(body.scrollTop,260);
 body.scrollTop=20;const reader=controller.captureViewport(body,{slack:10});body.scrollHeight=300;
 controller.restoreViewport(reader,{follow:true});assert.equal(body.scrollTop,20);
});

test('conversation visit ownership rejects a response from an earlier visit to the same chat', () => {
 const controller=commonBubble.createBubbleActionController();
 controller.selectVisit('a');const first=controller.captureVisit();
 assert.equal(controller.isCurrentVisit(first),true);
 controller.selectVisit('b');assert.equal(controller.isCurrentVisit(first),false);
 controller.selectVisit('a');assert.equal(controller.isCurrentVisit(first),false);
});

test('one action-row renderer inserts ordinary and shared actions in the same place', () => {
 const rows=[],doc={createElement:()=>({className:'',innerHTML:'',dataset:{}})};
 const turn={after:row=>rows.push(row)};
 const local=commonBubble.renderActionRow({doc,turn,html:'local'});
 const shared=commonBubble.renderActionRow({doc,turn,html:'shared',shared:true});
 assert.equal(rows[0],local);assert.equal(rows[1],shared);
 assert.equal(local.className,'bubactions');assert.equal(shared.className,'bubactions');
 assert.equal(shared.dataset.gactions,'');assert.equal(local.innerHTML,'local');assert.equal(shared.innerHTML,'shared');
 const fallback={nextSibling:{},parentNode:{insertBefore:(row,next)=>rows.push([row,next])}};
 const inserted=commonBubble.renderActionRow({doc,turn:fallback,html:'fallback'});
 assert.deepEqual(rows[2],[inserted,fallback.nextSibling]);
 assert.equal(commonBubble.renderActionRow({doc:null,turn:{},html:''}),null);
 assert.ok(commonBubble.renderActionRow({doc,turn:{},html:''}));
 const native={nextElementSibling:{},insertAdjacentHTML:(where,html)=>rows.push([where,html])};
 assert.equal(commonBubble.renderActionRow({doc,turn:native,html:'native',shared:true}),native.nextElementSibling);
 assert.deepEqual(rows.at(-1),['afterend','<div class="bubactions" data-gactions>native</div>']);
 commonBubble.renderActionRow({doc,turn:native,html:'local'});
 assert.deepEqual(rows.at(-1),['afterend','<div class="bubactions">local</div>']);
});

test('ordinary and shared Send to use one source-preserving action policy', () => {
 const sent=[];const adapter={sendTo:opts=>sent.push(opts)};
 commonBubble.dispatchBubbleAction('sendto',{text:'hello',role:'person',who:'Ada',source:{server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:2},shared:true},adapter);
 assert.equal(sent[0].text,'hello');assert.equal(sent[0].allowNew,false);assert.equal(sent[0].source.seq,2);assert.equal(sent[0].who,'Ada');
 commonBubble.dispatchBubbleAction('sendto',{text:'normal',role:'assistant',source:{uid:'u',record:1},exceptId:'n1'},adapter);
 assert.equal(sent[1].exceptId,'n1');assert.equal(sent[1].source.uid,'u');assert.equal('allowNew' in sent[1],false);
});

test('common bubble commands honor capability and never dispatch an agent send', () => {
 const calls=[],capture={text:'quoted ![x](scimux-asset:abc)',role:'person',source:{uid:'u',record:1}};
 const adapter={copy:text=>calls.push(['copy',text]),fork:text=>calls.push(['fork',text]),canPrepare:()=>false,prepare:()=>calls.push(['prepare'])};
 assert.equal(commonBubble.dispatchBubbleAction('copy',capture,adapter),true);
 assert.equal(commonBubble.dispatchBubbleAction('fork',capture,adapter),true);
 assert.equal(commonBubble.dispatchBubbleAction('prompt',capture,adapter),false);
 assert.deepEqual(calls,[['copy',capture.text],['fork','quoted x']]);
 adapter.canPrepare=()=>true;adapter.prepare=(text,source)=>calls.push(['prepare',text,source]);
 assert.equal(commonBubble.dispatchBubbleAction('prompt',capture,adapter),true);assert.deepEqual(calls[2],['prepare','quoted x',capture.source]);
});

test('common actions tolerate empty text while preserving an explicit source', () => {
 const calls=[],source={uid:'u',record:2};
 const adapter={copy:value=>calls.push(['copy',value]),fork:value=>calls.push(['fork',value]),prepare:(value,s)=>calls.push(['prompt',value,s]),canPrepare:()=>true,sendTo:value=>calls.push(['sendto',value])};
 assert.equal(commonBubble.dispatchBubbleAction('copy',null,adapter),false);
 for(const action of ['copy','fork','prompt','sendto']) assert.equal(commonBubble.dispatchBubbleAction(action,{source},adapter),true);
 assert.deepEqual(calls.slice(0,3),[['copy',''],['fork',''],['prompt','',source]]);
 assert.equal(calls[3][1].text,'');assert.equal(calls[3][1].role,'');assert.equal(calls[3][1].exceptId,'');assert.equal(calls[3][1].source,source);
});

test('description action rechecks an explicit local destination before applying', () => {
 let dialog,current=true,nodes=[{id:'n1',title:'Local',instance_id:'one'},{id:'team/s/o/c/g',title:'Shared'},{id:'ended',ended_at:'T'}],applied=[];
 const capture={text:'quoted'},adapter={choose:opts=>{dialog=opts},nodes:()=>nodes,isCurrent:()=>current,describe:(id,value)=>applied.push([id,value])};
 assert.equal(commonBubble.dispatchBubbleAction('desc',capture,adapter),true);assert.deepEqual(dialog.choices,[{id:'n1',label:'Local'}]);
 dialog.onChoose({id:'team/s/o/c/g'});current=false;dialog.onChoose({id:'n1'});current=true;nodes=[];dialog.onChoose({id:'n1'});assert.deepEqual(applied,[]);
 nodes=[{id:'n1',instance_id:'one'}];dialog.onChoose({id:'n1'});assert.deepEqual(applied,[['n1',capture]]);
});

test('bookmark action suppresses stale async capture and duplicate publication', async () => {
 let finish,current=true,exists=false;const saved=[],events=[];
 const adapter={exists:()=>exists,isCurrent:()=>current,capture:value=>new Promise(resolve=>{finish=()=>resolve(value)}),commit:value=>saved.push(value),pending:()=>events.push('pending'),duplicate:()=>events.push('duplicate'),error:()=>events.push('error')};
 commonBubble.dispatchBubbleAction('bookmark',{text:'first'},adapter);assert.deepEqual(events,['pending']);current=false;finish();await Promise.resolve();assert.deepEqual(saved,[]);
 current=true;commonBubble.dispatchBubbleAction('bookmark',{text:'second'},adapter);exists=true;finish();await Promise.resolve();assert.deepEqual(saved,[]);
 commonBubble.dispatchBubbleAction('bookmark',{text:'third'},adapter);assert.equal(events.at(-1),'duplicate');
});

test('bubble activation respects selection, interactive descendants, and keyboard use', () => {
 let calls=0;const activate=()=>calls++,target={closest:()=>null};
 assert.equal(activateBubble({target:null,activate}),false);
 assert.equal(activateBubble({target:{},activate}),false);
 for(const selector of ['a, button','a','button']) {
  const interactive={closest:s=>s===selector?{}:null};assert.equal(activateBubble({target:interactive,activate}),false);
 }
 assert.equal(activateBubble({target,doc:{getSelection:()=>({isCollapsed:false,toString:()=> ' selected '})},activate}),false);
 assert.equal(activateBubble({target,win:{getSelection:()=>({isCollapsed:false,toString:()=> 'selected'})},activate}),false);
 assert.equal(activateBubble({target,doc:{getSelection:()=>({isCollapsed:false,toString:()=> '  '})},activate}),true);
 assert.equal(activateBubble({target,doc:{getSelection:()=>({isCollapsed:true})},activate}),true);
 assert.equal(activateBubble({target,doc:{getSelection:()=>({isCollapsed:false,toString:null})},activate}),true);
 assert.equal(activateBubble({target,keyboard:true,doc:{getSelection:()=>({isCollapsed:false,toString:()=> 'selected'})},activate}),true);
 assert.equal(activateBubble({target,activate}),true);assert.equal(calls,5);
});

test('one bubble interaction routes actions and selection for either source', () => {
 const seen=[],turn={},action={};
 const target={closest:s=>s==='[data-act]'?action:s==='.turn'?turn:null};
 assert.equal(commonBubble.routeBubbleClick({target,actionSelector:'[data-act]',findTurn:()=>turn,onAction:item=>seen.push(item),onTurn:item=>seen.push(item)}),true);
 assert.deepEqual(seen,[action]);
 const plain={closest:()=>null};
 assert.equal(commonBubble.routeBubbleClick({target:plain,actionSelector:'[data-act]',findTurn:()=>turn,onAction:()=>{},onTurn:item=>seen.push(item)}),true);
 assert.equal(seen[1],turn);
 assert.equal(commonBubble.routeBubbleClick({target:plain,actionSelector:'[data-act]',findTurn:()=>null,onAction:()=>{},onTurn:()=>{}}),false);
 assert.equal(commonBubble.routeBubbleClick({target:null,actionSelector:'[data-act]',findTurn:()=>turn,onAction:()=>{},onTurn:()=>{}}),false);
});


test('description chooser rejects a recreated destination with the same id', () => {
 let dialog, nodes=[{id:'n1',title:'Original',instance_id:'old'}]; const applied=[];
 commonBubble.dispatchBubbleAction('desc',{text:'quoted'},{nodes:()=>nodes,choose:value=>{dialog=value},isCurrent:()=>true,describe:id=>applied.push(id)});
 const choice=dialog.choices[0];nodes=[{id:'n1',title:'Replacement',instance_id:'new'}];dialog.onChoose(choice);
 assert.deepEqual(applied,[]);
});

test('description chooser does not offer a local chat without durable identity', () => {
 let dialog; commonBubble.dispatchBubbleAction('desc',{text:'quoted'},{nodes:()=>[{id:'n1'}],choose:value=>{dialog=value},isCurrent:()=>true,describe:()=>assert.fail('unverified destination')});
 assert.deepEqual(dialog.choices,[]);
});

test('description choice waits for authoritative instance verification and rejects a replacement', async () => {
 let dialog,finish; const applied=[],nodes=[{id:'n1',title:'Original',instance_id:'old'}];
 commonBubble.dispatchBubbleAction('desc',{text:'quoted'},{nodes:()=>nodes,choose:value=>{dialog=value},isCurrent:()=>true,verify:()=>new Promise(resolve=>{finish=resolve}),describe:id=>applied.push(id)});
 dialog.onChoose(dialog.choices[0]);assert.deepEqual(applied,[]);await Promise.resolve();finish(false);await new Promise(resolve=>setImmediate(resolve));assert.deepEqual(applied,[]);
});

test('description verification applies only to the same visit and instance', async () => {
 let dialog,finish,active=true;const nodes=[{id:'n1',instance_id:'old'}],applied=[],unavailable=[];
 const adapter={nodes:()=>nodes,choose:value=>dialog=value,isCurrent:()=>active,verify:()=>new Promise(resolve=>finish=resolve),describe:id=>applied.push(id),unavailable:()=>unavailable.push(true)};
 commonBubble.dispatchBubbleAction('desc',{text:'quoted'},adapter);
 dialog.onChoose(dialog.choices[0]);await Promise.resolve();finish(true);await new Promise(resolve=>setImmediate(resolve));assert.deepEqual(applied,['n1']);
 dialog.onChoose(dialog.choices[0]);await Promise.resolve();active=false;finish(true);await new Promise(resolve=>setImmediate(resolve));assert.deepEqual(applied,['n1']);
 active=true;dialog.onChoose(dialog.choices[0]);await Promise.resolve();nodes[0]={id:'n1',instance_id:'new'};finish(true);await new Promise(resolve=>setImmediate(resolve));assert.deepEqual(applied,['n1']);
 nodes[0]={id:'n1',instance_id:'old'};dialog.onChoose(dialog.choices[0]);await Promise.resolve();finish(false);await new Promise(resolve=>setImmediate(resolve));assert.equal(unavailable.length,1);
 adapter.verify=()=>Promise.reject(Error('offline'));dialog.onChoose(dialog.choices[0]);await new Promise(resolve=>setImmediate(resolve));assert.equal(unavailable.length,2);
});

test('newer description choice retires an older verification across fresh adapters', async () => {
 const controller=commonBubble.createBubbleActionController(),nodes=[{id:'a',instance_id:'one',title:'A'},{id:'b',instance_id:'two',title:'B'}],menus=[],pending=new Map(),applied=[];let selected='source';
 const adapter=()=>({controller,nodes:()=>nodes,isCurrent:()=>selected==='source',choose:options=>menus.push(options),verify:id=>new Promise(resolve=>pending.set(id,resolve)),describe:id=>{applied.push(id);selected=id}});
 commonBubble.dispatchBubbleAction('desc',{text:'synthetic'},adapter());menus[0].onChoose(menus[0].choices[0]);await new Promise(resolve=>setImmediate(resolve));
 commonBubble.dispatchBubbleAction('desc',{text:'synthetic'},adapter());menus[1].onChoose(menus[1].choices[1]);await new Promise(resolve=>setImmediate(resolve));
 pending.get('a')(true);await new Promise(resolve=>setImmediate(resolve));assert.deepEqual(applied,[]);
 pending.get('b')(true);await new Promise(resolve=>setImmediate(resolve));assert.deepEqual(applied,['b']);
});

test('retiring an action row or opening another chooser cancels pending verification', async () => {
 for(const retire of [c=>c.clear(),c=>c.selectVisit('other'),c=>c.cancelAction(),c=>c.chooserOpened()]){
  const controller=commonBubble.createBubbleActionController(),nodes=[{id:'a',instance_id:'one'}],applied=[],notices=[];let menu,finish;
  commonBubble.dispatchBubbleAction('desc',{text:'synthetic'},{controller,nodes:()=>nodes,isCurrent:()=>true,choose:value=>menu=value,verify:()=>new Promise((resolve,reject)=>finish={resolve,reject}),describe:id=>applied.push(id),unavailable:()=>notices.push('stale error')});
  menu.onChoose(menu.choices[0]);await Promise.resolve();retire(controller);finish.reject(Error('late'));await new Promise(resolve=>setImmediate(resolve));assert.deepEqual(applied,[]);assert.deepEqual(notices,[]);
 }
});

test('overlapping description verifications honor the latest outcome in both orders', async () => {
 for(const [first,latestOK,oldSucceeds] of [['old',true,false],['new',true,false],['old',false,false],['new',false,false],['old',true,true],['new',true,true],['old',false,true],['new',false,true]]) {
  const controller=commonBubble.createBubbleActionController(),nodes=[{id:'a',instance_id:'one'},{id:'b',instance_id:'two'}],menus=[],pending={},applied=[],notices=[];let selected='source';
  const adapter=()=>({controller,nodes:()=>nodes,isCurrent:()=>selected==='source',choose:value=>menus.push(value),verify:id=>new Promise((resolve,reject)=>pending[id]={resolve,reject}),describe:id=>{applied.push(id);selected=id},unavailable:()=>notices.push('unavailable')});
  commonBubble.dispatchBubbleAction('desc',{text:'x'},adapter());menus[0].onChoose(menus[0].choices[0]);await Promise.resolve();
  commonBubble.dispatchBubbleAction('desc',{text:'x'},adapter());menus[1].onChoose(menus[1].choices[1]);await Promise.resolve();
  const settleOld=()=>oldSucceeds?pending.a.resolve(true):pending.a.reject(Error('late old error')),settleNew=()=>pending.b.resolve(latestOK);
  if(first==='old'){settleOld();await new Promise(resolve=>setImmediate(resolve));assert.deepEqual(applied,[]);settleNew()}else{settleNew();await new Promise(resolve=>setImmediate(resolve));settleOld()}
  await new Promise(resolve=>setImmediate(resolve));assert.deepEqual(applied,latestOK?['b']:[]);assert.deepEqual(notices,latestOK?[]:['unavailable']);
 }
});

test('a superseded chooser cannot submit a retained stale callback', async () => {
 const controller=commonBubble.createBubbleActionController(),menus=[],applied=[];
 const adapter=()=>({controller,nodes:()=>[{id:'a',instance_id:'one'}],isCurrent:()=>true,choose:value=>menus.push(value),describe:id=>applied.push(id)});
 commonBubble.dispatchBubbleAction('desc',{text:'one'},adapter());commonBubble.dispatchBubbleAction('desc',{text:'two'},adapter());
 menus[0].onChoose(menus[0].choices[0]);assert.deepEqual(applied,[]);menus[1].onChoose(menus[1].choices[0]);assert.deepEqual(applied,['a']);
});


test('the owning chooser preserves its action while another chooser retires it', () => {
 const controller=commonBubble.createBubbleActionController(),ticket=controller.beginAction();controller.chooserOpened(controller);assert.equal(controller.isCurrentAction(ticket),true);controller.chooserOpened({});assert.equal(controller.isCurrentAction(ticket),false);
});

test('closing a chooser retires its outstanding action unless the choice owns the close', () => {
 const c=commonBubble.createBubbleActionController();let token=c.beginAction();c.sheetClosed(c);assert.equal(c.isCurrentAction(token),true);
 c.sheetClosed();assert.equal(c.isCurrentAction(token),false);token=c.beginAction();c.sheetClosed({});assert.equal(c.isCurrentAction(token),false);
});
