import test from "node:test";
import assert from "node:assert/strict";
import { REACTIONS, reactionEligible, replayReactionEvents, reactionChipsHTML, reactionControlHTML, createReactionInteraction } from "../js/reactions.js";

test("reaction identifiers and glyphs are frozen in presentation order", () => {
  assert.deepEqual(REACTIONS, [
    {id:"celebration", emoji:"🎉"}, {id:"thinking", emoji:"🤔"},
    {id:"laughing", emoji:"😂"}, {id:"heart", emoji:"❤️"},
    {id:"thumbs_down", emoji:"👎"}, {id:"thumbs_up", emoji:"👍"},
  ]);
});

test("eligibility uses authenticated authorship, message type and share state", () => {
  const base = {shared:true, type:"people-comment", author:"alice", viewer:"bob", access:"write", state:"live", online:true, text:"hello"};
  assert.equal(reactionEligible(base), true);
  for (const change of [
    {author:"bob"}, {shared:false}, {type:"agent"}, {type:"mirrored-prompt"},
    {text:"", attachments:[]}, {access:"read"}, {state:"ended"}, {online:false},
  ]) assert.equal(reactionEligible({...base, ...change}), false, JSON.stringify(change));
  assert.equal(reactionEligible({...base, text:"", attachments:[{id:"file"}]}), true);
  assert.equal(reactionEligible(), false);
  assert.equal(reactionEligible({...base, mine:true}), false);
  assert.equal(reactionEligible({...base, mine:false}), true);
  assert.equal(reactionEligible({...base, author:"", viewer:"bob"}), false);
  assert.equal(reactionEligible({...base, viewer:""}), false);
  assert.equal(reactionEligible({...base, text:"", attachments:null}), false);
});

test("replay applies latest event per authenticated participant and target", () => {
  const comments = new Map([[10, {author:"alice", valid:true}]]);
  const events = [
    {v:1,t:"reaction",seq:11,key:"r/000000000000000b", target:10, target_author:"alice", emoji:"thumbs_up", active:true, author:"bob"},
    {v:1,t:"reaction",seq:12,key:"r/000000000000000c", target:10, target_author:"alice", emoji:"thumbs_up", active:true, author:"carol"},
    {v:1,t:"reaction",seq:13,key:"r/000000000000000d", target:10, target_author:"alice", emoji:"thumbs_up", active:false, author:"bob"},
    {v:1,t:"reaction",seq:14,key:"r/000000000000000e", target:10, target_author:"alice", emoji:"heart", active:true, author:"bob"},
  ];
  assert.deepEqual(replayReactionEvents(events, comments, "carol"), [
    {target:10, emoji:"heart", glyph:"❤️", count:1, mine:false},
    {target:10, emoji:"thumbs_up", glyph:"👍", count:1, mine:true},
  ]);
});

test("replay ignores self, unknown, malformed, forward-target and forged actor events", () => {
  const comments = new Map([[3, {author:"alice", valid:true}], [4, {author:"bob", valid:false}]]);
  const events = [
    {v:1,t:"reaction",seq:5,key:"r/0000000000000005", target:3, target_author:"alice", emoji:"thumbs_up", active:true, author:"alice"},
    {v:1,t:"reaction",seq:6,key:"r/0000000000000006", target:3, target_author:"alice", emoji:"thumbsup", active:true, author:"carol"},
    {v:1,t:"reaction",seq:7,key:"r/0000000000000007", target:4, target_author:"bob", emoji:"heart", active:true, author:"carol"},
    {v:1,t:"reaction",seq:8,key:"r/0000000000000008", target:9, target_author:"alice", emoji:"heart", active:true, author:"carol"},
    {v:1,t:"reaction",seq:9,key:"r/0000000000000009", target:3, target_author:"alice", emoji:"heart", active:true, author:"carol", actor:"alice"},
    {v:1,t:"reaction",seq:10,key:"r/000000000000000a", target:3, target_author:"alice", emoji:"heart", active:"yes", author:"carol"},
    null,
    {v:1,t:"reaction",seq:11,key:"r/000000000000000b", target:3, target_author:"alice", emoji:"heart", active:true, author:"carol", extra:true},
    {v:1,t:"reaction",seq:12,key:"r/000000000000000c", target:3, target_author:"wrong", emoji:"heart", active:true, author:"carol"},
    {v:1,t:"reaction",seq:12,key:"r/000000000000000d", target:3, target_author:"alice", emoji:"heart", active:true, author:"carol"},
    {v:1,t:"reaction",seq:13,key:"r/000000000000000e", target:0, target_author:"alice", emoji:"heart", active:true, author:"carol"},
    {v:1,t:"reaction",seq:14,key:"r/000000000000000f", target:3, target_author:"", emoji:"heart", active:true, author:"carol"},
    {v:1,t:"reaction",seq:15,key:"r/0000000000000010", target:3, target_author:"alice", emoji:"heart", active:true, author:""},
    {v:1,t:"reaction",seq:16,key:"bad", target:3, target_author:"alice", emoji:"heart", active:true, author:"carol"},
    {v:1,t:"reaction",seq:18,key:"r/0000000000000012", target:17, target_author:"alice", emoji:"heart", active:true, author:"carol"},
    {v:1,t:"reaction",seq:19,key:"r/0000000000000013", target:3, target_author:"mallory", emoji:"heart", active:true, author:"carol"},
    {v:1,t:"reaction",seq:20,key:undefined, target:3, target_author:"alice", emoji:"heart", active:true, author:"carol"},
  ];
  assert.deepEqual(replayReactionEvents(events, comments, "carol"), []);
  assert.deepEqual(replayReactionEvents(null, comments), []);
  assert.deepEqual(replayReactionEvents([], {}), []);
});

test("replay orders targets and emoji and handles malformed event sequences", () => {
  const comments=new Map([[2,{author:"alice",valid:true}],[4,{author:"carol",valid:true}]]);
  const events=[
    undefined,
    {seq:0},
    {v:1,t:"reaction",seq:3,key:"r/0000000000000003",target:2,target_author:"alice",emoji:"thumbs_up",active:true,author:"bob",mine:true},
    {v:1,t:"reaction",seq:5,key:"r/0000000000000005",target:4,target_author:"carol",emoji:"celebration",active:true,author:"bob",mine:false},
    {v:1,t:"reaction",seq:6,key:"r/0000000000000006",target:2,target_author:"alice",emoji:"laughing",active:true,author:"dan",mine:false},
    {v:1,t:"reaction",seq:7,key:"r/0000000000000007",target:2,target_author:"alice",emoji:"heart",active:true,author:"eve",mine:false},
  ];
  assert.deepEqual(replayReactionEvents(events,comments,"bob"),[
    {target:2,emoji:"laughing",glyph:"😂",count:1,mine:false},
    {target:2,emoji:"heart",glyph:"❤️",count:1,mine:false},
    {target:2,emoji:"thumbs_up",glyph:"👍",count:1,mine:true},
    {target:4,emoji:"celebration",glyph:"🎉",count:1,mine:true},
  ]);
});

test("chips expose counts and personal participation; entry UI uses fixed trigger semantics", () => {
  const chips = reactionChipsHTML([{target:10, emoji:"thumbs_up", glyph:"👍", count:2, mine:true}]);
  assert.match(chips, /👍/);
  assert.match(chips, />2</);
  assert.match(chips, /aria-label="👍 2, you reacted"/);
  const controls = reactionControlHTML();
  assert.match(controls, /type="button"/);
  assert.match(controls, /aria-label="React with emoji\."/);
  assert.match(controls, /title="React with emoji"/);
  assert.match(controls, /aria-hidden="true"/);
  assert.doesNotMatch(controls, /reaction-divider|│/, "a hidden reaction entry leaves no separator in the bubble");
  assert.ok(controls.indexOf("🎉") < controls.indexOf("🤔") && controls.indexOf("🤔") < controls.indexOf("😂"));
  assert.match(controls, /thumbs_up/);
  assert.equal(reactionChipsHTML(null), "");
  assert.equal(reactionChipsHTML([{target:10,emoji:"thumbs_up",glyph:"👍",count:1}],11), "");
  assert.equal(reactionChipsHTML([
    null,
    {target:10,emoji:"thumbs_up",glyph:"👍",count:0},
    {target:10,emoji:"thumbs_up",glyph:"bad",count:1},
    {target:10,emoji:"thumbs_up",glyph:"👍",count:1,mine:false},
  ],10).match(/reaction-chip/g)?.length,1);
  assert.match(reactionControlHTML({target:7,key:"1".repeat(32)+"/"+"2".repeat(32)+"/"+"3".repeat(16)+"/"+"4".repeat(16),copyInstance:"5".repeat(32)}),/data-rtarget="7" data-copy-instance="5{32}"/);
  assert.match(reactionControlHTML({target:"<img>",key:"bad",copyInstance:"bad"}),/data-rkey="" data-rtarget="0" data-copy-instance=""/);
});

test("chooser stays attached, chooses once, closes outside, and opens below when space above is short", () => {
  const attrs = new Map();
  const classes = new Set();
  const trigger = {setAttribute:(key, value) => attrs.set(key, value), getBoundingClientRect:() => ({top:4,bottom:48})};
  const chooser = {hidden:true, getBoundingClientRect:() => ({height:96})};
  const entry = {
    dataset:{rkey:"1".repeat(32)+"/"+"2".repeat(32)+"/"+"3".repeat(16)+"/"+"4".repeat(16),rtarget:"7",copyInstance:"5".repeat(32)},
    classList:{add:value=>classes.add(value),remove:value=>classes.delete(value)},
    contains:target=>target === trigger,
    querySelector:selector=>selector === "[data-reaction-open]" ? trigger : selector === ".reaction-chooser" ? chooser : null,
  };
  const button = {
    dataset:{reactionChoice:"thumbs_up"},
    closest:selector=>selector === "[data-reaction-open], [data-reaction-choice]" ? button : selector === "[data-reaction-entry]" ? entry : null,
    matches:selector=>selector === "[data-reaction-choice]",
  };
  const opener = {closest:selector=>selector === "[data-reaction-open], [data-reaction-choice]" ? opener : selector === "[data-reaction-entry]" ? entry : null,
    matches:()=>false};
  const doc = {defaultView:{innerHeight:180}};
  const picked = [];
  const controller = createReactionInteraction({doc,onChoose:value=>picked.push(value)});
  const click = target => {
    const event = {target, prevented:false, stopped:false, preventDefault(){this.prevented=true;}, stopPropagation(){this.stopped=true;}};
    return {handled:controller.handle(event),event};
  };
  const opened = click(opener);
  assert.equal(opened.handled,true); assert.equal(opened.event.prevented,true); assert.equal(opened.event.stopped,true);
  assert.equal(chooser.hidden,false); assert.equal(attrs.get("aria-expanded"),"true"); assert.ok(classes.has("below"));
  const outside = click({closest:()=>null});
  assert.equal(outside.handled,false); assert.equal(chooser.hidden,true); assert.deepEqual(picked,[]);
  click(opener);
  click(button);
  assert.equal(chooser.hidden,true); assert.equal(attrs.get("aria-expanded"),"false");
  assert.deepEqual(picked,[{key:entry.dataset.rkey,target:7,copyInstance:entry.dataset.copyInstance,emoji:"thumbs_up"}]);
});

function reactionDOM(id,{rect={top:120,bottom:164},height=96,withTrigger=true,withChooser=true}={}) {
  const attrs=new Map(),classes=new Set();
  const trigger=withTrigger?{setAttribute:(key,value)=>attrs.set(key,value),getBoundingClientRect:()=>rect}:null;
  const chooser=withChooser?{hidden:true,getBoundingClientRect:()=>({height})}:null;
  const entry={dataset:{rkey:"1".repeat(32)+"/"+"2".repeat(32)+"/"+"3".repeat(16)+"/"+"4".repeat(16),rtarget:String(id),copyInstance:"5".repeat(32)},classList:{add:value=>classes.add(value),remove:value=>classes.delete(value)},contains:target=>target?.entry===entry,
    querySelector:selector=>selector==="[data-reaction-open]"?trigger:selector===".reaction-chooser"?chooser:null};
  if(trigger){trigger.entry=entry;trigger.closest=selector=>selector==="[data-reaction-open], [data-reaction-choice]"?trigger:selector==="[data-reaction-entry]"?entry:null;trigger.matches=()=>false;}
  const opener={entry,closest:selector=>selector==="[data-reaction-open], [data-reaction-choice]"?opener:selector==="[data-reaction-entry]"?entry:null,matches:()=>false};
  const choice={entry,dataset:{reactionChoice:"thumbs_up"},closest:selector=>selector==="[data-reaction-open], [data-reaction-choice]"?choice:selector==="[data-reaction-entry]"?entry:null,matches:selector=>selector==="[data-reaction-choice]"};
  return {entry,trigger,chooser,opener,choice,attrs,classes};
}

test("chooser toggles, remains open on its own pointer path, switches entries, and handles missing controls", () => {
  const first=reactionDOM(1),second=reactionDOM(2),missing=reactionDOM(3,{withTrigger:false,withChooser:false});
  const picked=[];const doc={defaultView:{innerHeight:240}};const controller=createReactionInteraction({doc,onChoose:value=>picked.push(value)});
  const click=target=>controller.handle({target,preventDefault(){},stopPropagation(){}});
  controller.close();assert.equal(controller.current(),null);
  assert.equal(click({}),false);
  assert.equal(click(first.opener),true);assert.equal(controller.current(),first.entry);assert.equal(first.classes.has("below"),false);
  assert.equal(click({entry:first.entry,closest:()=>null}),false);assert.equal(controller.current(),first.entry);
  assert.equal(click(first.opener),true);assert.equal(controller.current(),null);assert.equal(first.chooser.hidden,true);
  click(first.opener);click(second.opener);
  assert.equal(controller.current(),second.entry);assert.equal(first.chooser.hidden,true);assert.equal(second.chooser.hidden,false);
  click(missing.opener);assert.equal(controller.current(),null);
  assert.equal(picked.length,0);
});

test("chooser honors viewport limits, tolerates absent geometry, and closes unknown choices", () => {
  const belowNoRoom=reactionDOM(1,{rect:{top:2,bottom:46},height:90});
  const noRect=reactionDOM(2,{rect:null});
  const unknown=reactionDOM(3);unknown.choice.dataset.reactionChoice="not-a-reaction";
  const controller=createReactionInteraction({doc:{defaultView:{innerHeight:70}}});
  const click=target=>controller.handle({target,preventDefault(){},stopPropagation(){}});
  click(belowNoRoom.opener);assert.equal(belowNoRoom.classes.has("below"),false);
  click(noRect.opener);assert.equal(controller.current(),noRect.entry);
  click(unknown.opener);click(unknown.choice);assert.equal(controller.current(),null);
  const zeroHeight=reactionDOM(4,{height:0});
  const measured=createReactionInteraction({doc:{defaultView:{innerHeight:200}}});
  measured.handle({target:zeroHeight.opener,preventDefault(){},stopPropagation(){}});
  assert.equal(measured.current(),zeroHeight.entry);
  const zeroViewport=reactionDOM(5);
  const viewportFallback=createReactionInteraction({doc:{defaultView:{innerHeight:0}}});
  viewportFallback.handle({target:zeroViewport.opener,preventDefault(){},stopPropagation(){}});
  assert.equal(viewportFallback.current(),zeroViewport.entry);
});

test("chooser uses the chat viewport when choosing above or below", () => {
  const nearTop=reactionDOM(8,{rect:{left:180,right:224,top:112,bottom:156},height:96});
  const chat={getBoundingClientRect:()=>({left:0,right:400,top:100,bottom:500})};
  const controller=createReactionInteraction({doc:{defaultView:{innerHeight:800},querySelector:()=>chat}});
  controller.handle({target:nearTop.opener,preventDefault(){},stopPropagation(){}});
  assert.equal(nearTop.classes.has("below"),true);
});

test("chooser switches sides only when its left edge would leave the chat viewport", () => {
  const make = ({triggerRect,chatRect,defaultLeft}) => {
    const classes=new Set(),attrs=new Map();
    const trigger={setAttribute:(key,value)=>attrs.set(key,value),getBoundingClientRect:()=>triggerRect};
    const chooser={hidden:true,getBoundingClientRect:()=>{const left=classes.has("rightward")?triggerRect.right+48:defaultLeft;return {left,right:left+280,width:280,height:96};}};
    const entry={classList:{add:value=>classes.add(value),remove:value=>classes.delete(value)},contains:target=>target===trigger,
      querySelector:selector=>selector==="[data-reaction-open]"?trigger:chooser};
    const opener={closest:selector=>selector==="[data-reaction-open], [data-reaction-choice]"?opener:entry,matches:()=>false};
    return {entry,opener,chooser,classes,attrs,chatRect};
  };
  const rightFits=make({triggerRect:{left:100,right:144,top:100,bottom:144},chatRect:{left:0,right:500},defaultLeft:-20});
  const leftFits=make({triggerRect:{left:350,right:394,top:100,bottom:144},chatRect:{left:0,right:500},defaultLeft:22});
  const doc={defaultView:{innerHeight:600},querySelector:()=>({getBoundingClientRect:()=>activeChatRect})};
  let activeChatRect=rightFits.chatRect;
  const controller=createReactionInteraction({doc}),click=target=>controller.handle({target,preventDefault(){},stopPropagation(){}});
  click(rightFits.opener);assert.equal(rightFits.classes.has("rightward"),true);controller.close();assert.equal(rightFits.classes.has("rightward"),false);
  activeChatRect=leftFits.chatRect;click(leftFits.opener);assert.equal(leftFits.classes.has("rightward"),false);
});

test("chooser compresses before switching sides when neither full row fits the chat", () => {
  const classes=new Set(),attrs=new Map();
  const trigger={setAttribute:(key,value)=>attrs.set(key,value),getBoundingClientRect:()=>({left:350,right:394,top:100,bottom:144})};
  const chooser={hidden:true,getBoundingClientRect:()=>{
    const width=classes.has("compact")?140:280;
    const left=classes.has("rightward")?trigger.getBoundingClientRect().right+48:trigger.getBoundingClientRect().left-48-width;
    return {left,right:left+width,top:100,bottom:196,width,height:96};
  }};
  const entry={classList:{add:value=>classes.add(value),remove:value=>classes.delete(value)},contains:target=>target===trigger,
    querySelector:selector=>selector==="[data-reaction-open]"?trigger:chooser};
  const opener={closest:selector=>selector==="[data-reaction-open], [data-reaction-choice]"?opener:entry,matches:()=>false};
  const doc={defaultView:{innerHeight:600},querySelector:()=>({getBoundingClientRect:()=>({left:100,right:600})})};
  const controller=createReactionInteraction({doc}),click=target=>controller.handle({target,preventDefault(){},stopPropagation(){}});
  click(opener);
  const rect=chooser.getBoundingClientRect();
  assert.equal(classes.has("compact"),true);
  assert.equal(classes.has("rightward"),false);
  assert.ok(rect.left>=100&&rect.right<=600);
});

test("chooser stacks and centers in the chat when the narrow chat cannot fit either horizontal layout", () => {
  const classes=new Set(),attrs=new Map(),chatRect={left:100,right:190},styleValues=new Map();
  const trigger={setAttribute:(key,value)=>attrs.set(key,value),getBoundingClientRect:()=>({left:100,right:144,top:100,bottom:144})};
  const entry={getBoundingClientRect:()=>({left:100}),style:{setProperty:(key,value)=>styleValues.set(key,value)},classList:{add:value=>classes.add(value),remove:value=>classes.delete(value)},contains:target=>target===trigger,querySelector:selector=>selector==="[data-reaction-open]"?trigger:chooser};
  const chooser={hidden:true,getBoundingClientRect:()=>{const width=classes.has("stacked")?52:classes.has("compact")?140:280;const left=classes.has("detached")?entry.getBoundingClientRect().left+Number.parseFloat(styleValues.get("--reaction-chat-center"))-width/2:classes.has("rightward")?trigger.getBoundingClientRect().right+48:trigger.getBoundingClientRect().left-48-width;return {left,right:left+width,width,height:classes.has("stacked")?272:96};}};
  const opener={closest:selector=>selector==="[data-reaction-open], [data-reaction-choice]"?opener:entry,matches:()=>false};
  const doc={defaultView:{innerHeight:600},querySelector:()=>({getBoundingClientRect:()=>chatRect})};
  const controller=createReactionInteraction({doc}),click=target=>controller.handle({target,preventDefault(){},stopPropagation(){}});
  click(opener);const rect=chooser.getBoundingClientRect();
  assert.equal(classes.has("stacked"),true);assert.equal(classes.has("detached"),true);
  assert.equal(styleValues.get("--reaction-chat-center"),"45px");
  assert.ok(rect.left>=chatRect.left&&rect.right<=chatRect.right);
});

test("chooser safely handles an event without an entry and optional callback", () => {
  const target={closest:selector=>selector==="[data-reaction-open], [data-reaction-choice]"?target:null,matches:()=>false};
  const controller=createReactionInteraction({doc:{defaultView:{innerHeight:0}}});
  assert.equal(controller.handle({target,preventDefault(){},stopPropagation(){}}),true);
  assert.equal(controller.handle({target:{closest:()=>null}}),false);
});

/* Instant local feedback. The namespace import keeps the file loadable while
   the projection helper is still absent, so each new test fails on its own. */
import * as reactionModule from "../js/reactions.js";

const confirmedChip = (target, emoji, count, mine = false) => ({target, emoji, glyph: reactionModule.REACTIONS.find(item => item.id === emoji).emoji, count, mine});
const deepFreezeAll = value => {
  if (value && typeof value === "object") { for (const item of Object.values(value)) deepFreezeAll(item); Object.freeze(value); }
  return value;
};

test("pending projection adds the sender's pending reaction once and keeps others' contributions", () => {
  const project = reactionModule.projectReactionIntent;
  const confirmed = deepFreezeAll([confirmedChip(4, "thumbs_up", 2), confirmedChip(4, "heart", 1, true)]);
  const before = JSON.stringify(confirmed);
  const added = project(confirmed, [{target:4, emoji:"celebration", active:true, state:"sending"}]);
  assert.deepEqual(added.map(chip => [chip.emoji, chip.count, chip.mine, chip.unconfirmed]), [
    ["celebration", 1, true, "pending"], ["heart", 1, true, undefined], ["thumbs_up", 2, false, undefined],
  ]);
  assert.equal(JSON.stringify(confirmed), before, "projection never mutates confirmed input");
  assert.equal(project(confirmed, []).length, 2, "no operations means the confirmed summary");
  assert.equal(project(confirmed).length, 2, "an omitted operation list defaults to none");
  assert.deepEqual(project(confirmed, []).map(chip => chip.unconfirmed), [undefined, undefined]);
});

test("pending add counts an already confirmed personal reaction once and never doubles it", () => {
  const confirmed = deepFreezeAll([confirmedChip(4, "heart", 3, true)]);
  const again = reactionModule.projectReactionIntent(confirmed, [{target:4, emoji:"heart", active:true, state:"acknowledged"}]);
  assert.deepEqual(again.map(chip => [chip.count, chip.mine, chip.unconfirmed]), [[3, true, "pending"]]);
});

test("pending removal subtracts only the sender's confirmed contribution and hides a sole chip", () => {
  const project = reactionModule.projectReactionIntent;
  const withOthers = deepFreezeAll([confirmedChip(4, "laughing", 3, true)]);
  const removedWithOthers = project(withOthers, [{target:4, emoji:"laughing", active:false, state:"sending"}]);
  assert.deepEqual(removedWithOthers.map(chip => [chip.count, chip.mine, chip.unconfirmed]), [[2, false, "pending"]]);
  const sole = deepFreezeAll([confirmedChip(4, "thinking", 1, true)]);
  assert.deepEqual(project(sole, [{target:4, emoji:"thinking", active:false, state:"uncertain"}]), [], "zero-count chips are hidden");
  assert.deepEqual(project(sole, [{target:4, emoji:"thinking", active:false, state:"sending"}]), [], "a removal cannot drive a count below zero");
  const absent = deepFreezeAll([confirmedChip(4, "heart", 1)]);
  assert.deepEqual(project(absent, [{target:4, emoji:"thinking", active:false, state:"sending"}]).map(chip => chip.emoji), ["heart"], "removing an absent reaction changes nothing for other emoji");
});

test("pending projection preserves fixed ordering and concurrent confirmed updates across targets", () => {
  const project = reactionModule.projectReactionIntent;
  const confirmed = deepFreezeAll([confirmedChip(9, "thumbs_up", 1), confirmedChip(2, "thumbs_up", 1), confirmedChip(2, "celebration", 4)]);
  const projected = project(confirmed, [
    {target:2, emoji:"thumbs_up", active:true, state:"sending"},
    {target:9, emoji:"heart", active:true, state:"uncertain"},
    {target:2, emoji:"heart", active:true, state:"acknowledged"},
  ]);
  assert.deepEqual(projected.map(chip => `${chip.target}:${chip.emoji}:${chip.count}:${chip.unconfirmed}`), [
    "2:celebration:4:undefined", "2:heart:1:pending", "2:thumbs_up:2:pending", "9:heart:1:uncertain", "9:thumbs_up:1:undefined",
  ]);
  assert.equal(projected[0].glyph, "🎉", "projected chips keep their glyph");
  assert.deepEqual(project(null, null), [], "missing inputs produce an empty projection");
  assert.deepEqual(project(undefined, [{target:1, emoji:"thumbs_up", active:true, state:"sending"}]).map(chip => chip.count), [1]);
});

test("pending chips expose a quiet state and an accessible confirmation label", () => {
  const pendingAdd = reactionModule.reactionChipsHTML([{target:5, emoji:"thumbs_up", glyph:"👍", count:1, mine:true, unconfirmed:"pending"}], 5);
  assert.match(pendingAdd, /class="reaction-chip mine reaction-pending"/);
  assert.match(pendingAdd, /aria-label="👍 1, you reacted, pending confirmation" aria-pressed="true" title="Reaction pending confirmation"/);
  const uncertain = reactionModule.reactionChipsHTML([{target:5, emoji:"heart", glyph:"❤️", count:2, mine:false, unconfirmed:"uncertain"}], 5);
  assert.match(uncertain, /class="reaction-chip reaction-pending"/);
  assert.match(uncertain, /aria-label="❤️ 2, not confirmed" aria-pressed="false" title="Reaction not confirmed; select to retry"/);
  const settled = reactionModule.reactionChipsHTML([{target:5, emoji:"thumbs_up", glyph:"👍", count:1, mine:true}], 5);
  assert.doesNotMatch(settled, /pending|not confirmed|title=/);
});

test("acceptedReactionEvents keeps exactly the records replay accepts, in sequence order", () => {
  const comments = new Map([[1, { author: "alice", valid: true }]]);
  const record = (seq, fields = {}) => ({ v: 1, t: "reaction", seq, key: "r/0101010101010101", target: 1, target_author: "alice", emoji: "thumbs_up", active: true, author: "me", mine: true, ...fields });
  const events = [record(2), record(3, { target_author: "mallory" }), record(2, { active: false }), record(4, { active: false })];
  const accepted = reactionModule.acceptedReactionEvents(events, comments);
  assert.deepEqual(accepted, [record(2), record(4, { active: false })]);
  assert.deepEqual(reactionModule.acceptedReactionEvents(null, comments), []);
  assert.deepEqual(reactionModule.acceptedReactionEvents(events, {}), []);
  assert.deepEqual(reactionModule.replayReactionEvents(events, comments, "me"), [], "the removal leaves no aggregate chip");
});
