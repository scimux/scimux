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
  assert.match(pendingAdd, /aria-label="👍 1, you reacted, pending confirmation" title="Reaction pending confirmation"/);
  const uncertain = reactionModule.reactionChipsHTML([{target:5, emoji:"heart", glyph:"❤️", count:2, mine:false, unconfirmed:"uncertain"}], 5);
  assert.match(uncertain, /class="reaction-chip reaction-pending"/);
  assert.match(uncertain, /aria-label="❤️ 2, not confirmed" title="Reaction not confirmed; choose it again in the reaction menu to retry"/);
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

/* Participant disclosure. Pure roster, preview, markup and controller behavior. */
const need = name => {
  const value = reactionModule[name];
  if (typeof value !== "function") throw new Error(`${name} is not implemented`);
  return value;
};
const participantEvent = (seq, author, emoji, active, fields = {}) => ({v:1, t:"reaction", seq, key:"r/" + seq.toString(16).padStart(16, "0"), target:10, target_author:"alice", emoji, active, author, ...fields});
const participantComments = () => new Map([[10, {author:"alice", valid:true}], [20, {author:"bob", valid:false}]]);

test("participants keep each author's latest accepted state in activation order and skip invalid records", () => {
  const participants = need("reactionParticipants");
  const events = [
    participantEvent(11, "bob", "thumbs_up", true),
    participantEvent(12, "carol", "thumbs_up", true),
    participantEvent(13, "bob", "thumbs_up", false),
    participantEvent(14, "bob", "thumbs_up", true),
    participantEvent(15, "dave", "heart", true),
    participantEvent(16, "erin", "thumbs_up", true, {target_author:"wrong"}),
    participantEvent(17, "alice", "thumbs_up", true),
    participantEvent(18, "frank", "thumbs_up", true, {target:20, target_author:"bob"}),
    participantEvent(19, "gina", "thumbs_up", true, {extra:true}),
  ];
  assert.deepEqual(participants(events, participantComments(), {target:10, emoji:"thumbs_up"}), [
    {author:"carol", mine:false, state:"confirmed"}, {author:"bob", mine:false, state:"confirmed"},
  ], "bob's removal then re-add moves him to his latest activation; invalid, self, forged and extra-field records never count");
  assert.deepEqual(participants(events, participantComments(), {target:10, emoji:"heart"}), [{author:"dave", mine:false, state:"confirmed"}]);
  assert.deepEqual(participants(events, participantComments(), {target:20, emoji:"thumbs_up"}), [], "an invalid target comment has no participants");
});

test("own retained participation is marked mine, and distinct identities with one display name stay separate", () => {
  const participants = need("reactionParticipants");
  const events = [
    participantEvent(11, "me", "laughing", true, {mine:true}),
    participantEvent(12, "bob-seat-1", "laughing", true),
    participantEvent(13, "bob-seat-2", "laughing", true, {mine:false}),
    participantEvent(14, "carol", "laughing", true, {mine:"yes"}),
  ];
  assert.deepEqual(participants(events, participantComments(), {target:10, emoji:"laughing"}), [
    {author:"me", mine:true, state:"confirmed"},
    {author:"bob-seat-1", mine:false, state:"confirmed"},
    {author:"bob-seat-2", mine:false, state:"confirmed"},
    {author:"carol", mine:false, state:"confirmed"},
  ]);
});

test("empty and malformed roster inputs yield no participants", () => {
  const participants = need("reactionParticipants");
  assert.deepEqual(participants([], participantComments(), {target:10, emoji:"heart"}), []);
  assert.deepEqual(participants(null, participantComments(), {target:10, emoji:"heart"}), []);
  assert.deepEqual(participants([participantEvent(11, "bob", "heart", true)], {}, {target:10, emoji:"heart"}), [], "a non-map comment index accepts nothing");
  assert.deepEqual(participants([participantEvent(11, "bob", "heart", true)], participantComments()), [], "a call without a target and emoji has no participants");
});

test("pending and uncertain own operations label or hide only the sender's contribution, without double counting", () => {
  const participants = need("reactionParticipants");
  const comments = participantComments();
  const bob = participantEvent(11, "bob", "thumbs_up", true);
  const mine = participantEvent(12, "me", "thumbs_up", true, {mine:true});
  const cases = [
    ["pending own add", [bob], [{target:10, emoji:"thumbs_up", active:true, state:"pending"}], [
      {author:"bob", mine:false, state:"confirmed"}, {author:"", mine:true, state:"pending"},
    ]],
    ["uncertain own add", [], [{target:10, emoji:"thumbs_up", active:true, state:"uncertain"}], [
      {author:"", mine:true, state:"uncertain"},
    ]],
    ["pending add over a confirmed own reaction", [mine], [{target:10, emoji:"thumbs_up", active:true, state:"pending"}], [
      {author:"me", mine:true, state:"pending"},
    ]],
    ["pending removal of own confirmed contribution", [bob, mine], [{target:10, emoji:"thumbs_up", active:false, state:"pending"}], [
      {author:"bob", mine:false, state:"confirmed"},
    ]],
    ["pending removal without an own contribution is a no-op", [bob], [{target:10, emoji:"thumbs_up", active:false, state:"pending"}], [
      {author:"bob", mine:false, state:"confirmed"},
    ]],
    ["add then remove cancels out", [], [{target:10, emoji:"thumbs_up", active:true, state:"pending"}, {target:10, emoji:"thumbs_up", active:false, state:"uncertain"}], []],
    ["operations for other targets or emoji are ignored", [bob], [{target:20, emoji:"thumbs_up", active:true, state:"pending"}, {target:10, emoji:"heart", active:true, state:"pending"}], [
      {author:"bob", mine:false, state:"confirmed"},
    ]],
    ["a concurrent external add during a pending add stays accurate", [bob, participantEvent(13, "carol", "thumbs_up", true)], [{target:10, emoji:"thumbs_up", active:true, state:"pending"}], [
      {author:"bob", mine:false, state:"confirmed"}, {author:"carol", mine:false, state:"confirmed"}, {author:"", mine:true, state:"pending"},
    ]],
  ];
  for (const [name, events, operations, expected] of cases) {
    assert.deepEqual(participants(events, comments, {target:10, emoji:"thumbs_up", operations}), expected, name);
  }
  assert.deepEqual(participants([], comments, {target:10, emoji:"thumbs_up", operations:[{target:10, emoji:"thumbs_up", active:true, state:"pending"}], writable:false}), [], "read-only, offline and ended copies show confirmed state only");
  assert.deepEqual(participants([bob], comments, {target:10, emoji:"thumbs_up", operations:[{target:10, emoji:"thumbs_up", active:true, state:"pending"}], writable:false}), [{author:"bob", mine:false, state:"confirmed"}]);
});

test("the roster agrees with the aggregate chip count and own flag for every bounded sequence of events and operations", () => {
  const participants = need("reactionParticipants");
  const comments = participantComments();
  const events = [
    participantEvent(11, "bob", "thumbs_up", true),
    participantEvent(12, "me", "thumbs_up", true, {mine:true}),
    participantEvent(13, "bob", "thumbs_up", false),
    participantEvent(14, "carol", "thumbs_up", true),
    participantEvent(15, "me", "thumbs_up", false, {mine:true}),
  ];
  const options = [
    {target:10, emoji:"thumbs_up", active:true, state:"pending"},
    {target:10, emoji:"thumbs_up", active:false, state:"sending"},
    {target:10, emoji:"thumbs_up", active:true, state:"uncertain"},
    {target:10, emoji:"thumbs_up", active:false, state:"acknowledged"},
  ];
  const sequences = [[]];
  for (const first of options) {
    sequences.push([first]);
    for (const second of options) sequences.push([first, second]);
  }
  let checked = 0;
  for (let mask = 0; mask < 1 << events.length; mask++) {
    const subset = events.filter((_, index) => mask & (1 << index));
    const confirmed = reactionModule.replayReactionEvents(subset, comments, "");
    for (const operations of sequences) {
      const roster = participants(subset, comments, {target:10, emoji:"thumbs_up", operations});
      const chip = reactionModule.projectReactionIntent(confirmed, operations).find(item => item.target === 10 && item.emoji === "thumbs_up");
      assert.equal(roster.length, chip?.count ?? 0, JSON.stringify({mask, operations}));
      assert.equal(roster.some(entry => entry.mine), chip?.mine === true, JSON.stringify({mask, operations}));
      checked++;
    }
  }
  assert.equal(checked, 32 * sequences.length);
});

test("roster reads never mutate events, comments, or operations", () => {
  const participants = need("reactionParticipants");
  const events = deepFreezeAll([participantEvent(11, "bob", "heart", true), participantEvent(12, "me", "heart", true, {mine:true})]);
  const operations = deepFreezeAll([{target:10, emoji:"heart", active:false, state:"pending"}]);
  const comments = participantComments();
  const before = JSON.stringify([events, operations, [...comments]]);
  participants(events, comments, {target:10, emoji:"heart", operations});
  assert.equal(JSON.stringify([events, operations, [...comments]]), before);
});

test("preview wording names everyone up to four, abbreviates larger rosters, and states own pending or uncertain status", () => {
  const preview = need("reactionPreviewText");
  assert.equal(preview([{name:"Anna", mine:false, state:"confirmed"}], "❤️"), "Anna reacted with ❤️");
  assert.equal(preview([{name:"Anna", mine:false, state:"confirmed"}, {name:"Ben", mine:false, state:"confirmed"}, {mine:true, state:"confirmed"}], "❤️"), "Anna, Ben and you reacted with ❤️");
  assert.equal(preview([{mine:true, state:"confirmed"}], "👍"), "You reacted with 👍");
  assert.equal(preview([{mine:true, state:"pending"}], "👍"), "You reacted with 👍. Your reaction is pending confirmation.");
  assert.equal(preview([{mine:true, state:"uncertain"}], "👍"), "You reacted with 👍. Your reaction is not confirmed.");
  assert.equal(preview([{mine:true, state:"confirmed"}, {name:"Ben", mine:false, state:"confirmed"}], "🎉"), "You and Ben reacted with 🎉");
  const four = ["A", "B", "C", "D"].map(name => ({name, mine:false, state:"confirmed"}));
  assert.equal(preview(four, "👍"), "A, B, C and D reacted with 👍");
  const six = ["A", "B", "C", "D", "E", "F"].map(name => ({name, mine:false, state:"confirmed"}));
  assert.equal(preview(six, "👍"), "A, B, C and 3 others reacted with 👍");
  assert.equal(preview(six.slice(0, 5), "👍"), "A, B, C and 2 others reacted with 👍");
  assert.equal(preview([], "👍"), "");
});

test("roster markup escapes names and content, labels own status, and differs by presentation mode", () => {
  const html = need("reactionRosterHTML");
  const hostile = [{name:'<img src=x onerror="alert(1)">&', mine:false, state:"confirmed"}, {mine:true, state:"pending"}, {mine:true, state:"uncertain"}];
  const wide = html(hostile, "👍", "wide");
  assert.doesNotMatch(wide, /<img/);
  assert.match(wide, /&lt;img src=x onerror=&quot;alert\(1\)&quot;&gt;&amp;/);
  assert.match(wide, /<ul class="reaction-roster">/);
  assert.match(wide, /<li>You — pending confirmation<\/li>/);
  assert.match(wide, /<li>You — not confirmed<\/li>/);
  assert.doesNotMatch(wide, /data-reaction-close/);
  const sheet = html([{name:"Anna", mine:false, state:"confirmed"}], "👍", "sheet");
  assert.match(sheet, /<button type="button" class="reaction-disclosure-close" data-reaction-close>Close<\/button>/);
  assert.match(sheet, /<li>Anna<\/li>/);
  const hover = html(hostile, "👍", "hover");
  assert.match(hover, /<p class="reaction-disclosure-summary">/);
  assert.doesNotMatch(hover, /<ul|<li/);
  assert.doesNotMatch(hover, /<img/);
  assert.match(html([{mine:true, state:"confirmed"}], "👍", "wide"), /<li>You<\/li>/);
});

test("chips are whole-chip inspection buttons that keep their existing labels and expose their target", () => {
  const chips = reactionChipsHTML([{target:7, emoji:"thumbs_up", glyph:"👍", count:1, mine:true}], 7);
  assert.match(chips, /^<button class="reaction-chip mine" data-reaction="thumbs_up" aria-label="👍 1, you reacted" aria-description="Show who reacted" type="button" aria-haspopup="dialog" aria-expanded="false" data-rtarget="7"><span aria-hidden="true">👍<\/span><span>1<\/span><\/button>$/);
  const pending = reactionChipsHTML([{target:7, emoji:"heart", glyph:"❤️", count:2, mine:true, unconfirmed:"uncertain"}], 7);
  assert.match(pending, /class="reaction-chip mine reaction-pending" data-reaction="heart" aria-label="❤️ 2, you reacted, not confirmed" title="Reaction not confirmed; choose it again in the reaction menu to retry" aria-description="Show who reacted" type="button" aria-haspopup="dialog" aria-expanded="false" data-rtarget="7">/);
});

test("glyph lookup returns the fixed glyph for known reactions and nothing for unknown ones", () => {
  const glyph = need("reactionGlyph");
  assert.equal(glyph("heart"), "❤️");
  assert.equal(glyph("thumbs_up"), "👍");
  assert.equal(glyph("unknown"), "");
});

/* A minimal element tree for the controller: only the selectors and DOM methods it uses. */
function fakeRect(left, top, right, bottom) { return {left, top, right, bottom, width:right - left, height:bottom - top}; }
function fakeElement(tag, doc, options = {}) {
  const element = {
    tagName: tag, parent: null, children: [], isConnected: !!options.connected, focused: 0, writes: 0, attrs: new Map(),
    classes: new Set(options.classes || []), dataset: {...(options.dataset || {})}, style: {setProperty(name, value) { this[name] = value; }}, rect: options.rect || null, html: "",
    classList: {
      add(...names) { names.forEach(name => element.classes.add(name)); },
      remove(...names) { names.forEach(name => element.classes.delete(name)); },
      contains(name) { return element.classes.has(name); },
    },
    get innerHTML() { return element.html; },
    /* Like a real parser: replacing content destroys focused descendants, and creates a close control when the markup has one. */
    set innerHTML(value) {
      const focused = doc.activeElement;
      for (const child of element.children) if (focused && (child === focused || descendants(child).includes(focused))) doc.activeElement = null;
      for (const child of [...element.children]) child.remove();
      element.html = value; element.writes++;
      if (String(value).includes("data-reaction-close")) element.appendChild(fakeElement("button", doc, {dataset:{reactionClose:""}}));
    },
    setAttribute(name, value) { element.attrs.set(name, String(value)); },
    getAttribute(name) { return element.attrs.has(name) ? element.attrs.get(name) : null; },
    appendChild(child) { child.parent = element; element.children.push(child); setConnected(child, element.isConnected); return child; },
    remove() { if (element.parent) element.parent.children = element.parent.children.filter(child => child !== element); element.parent = null; setConnected(element, false); },
    closest(selector) { for (let node = element; node; node = node.parent) if (matchesSelector(node, selector)) return node; return null; },
    matches(selector) { return matchesSelector(element, selector); },
    contains(other) { for (let node = other; node; node = node.parent) if (node === element) return true; return false; },
    querySelector(selector) { return descendants(element).find(node => matchesSelector(node, selector)) || null; },
    querySelectorAll(selector) { return descendants(element).filter(node => matchesSelector(node, selector)); },
    focus() { doc.activeElement = element; element.focused++; },
    getBoundingClientRect() {
      if (element.classes.has("reaction-disclosure") || element.classes.has("reaction-sheet")) return {left:0, top:0, width:doc.panelSize.width, height:doc.panelSize.height, right:doc.panelSize.width, bottom:doc.panelSize.height};
      return element.rect || fakeRect(0, 0, 0, 0);
    },
  };
  return element;
}
function setConnected(node, value) { node.isConnected = value; for (const child of node.children) setConnected(child, value); }
function descendants(root) { const out = []; for (const child of root.children) { out.push(child, ...descendants(child)); } return out; }
function matchesSelector(node, selector) {
  return selector.split(",").some(part => {
    const simple = part.trim();
    if (simple.startsWith(".")) return node.classes.has(simple.slice(1));
    const match = /^\[data-([a-z-]+)\]$/.exec(simple);
    if (match) return Object.hasOwn(node.dataset, match[1].replace(/-([a-z])/g, (_, letter) => letter.toUpperCase()));
    return node.tagName === simple;
  });
}
function fakeDocument({msgs = fakeRect(0, 0, 600, 600), viewport = 800, panel = {width:160, height:90}} = {}) {
  const listeners = new Map();
  const msgsNode = fakeElement("div", null, {connected:true, rect:msgs});
  const doc = {
    defaultView: {innerHeight:viewport}, activeElement: null, panelSize: panel, msgsNode,
    body: null,
    listenerCount: type => listeners.get(type)?.size || 0,
    listener: type => [...(listeners.get(type) || [])][0],
    addEventListener(type, fn) { if (!listeners.has(type)) listeners.set(type, new Set()); listeners.get(type).add(fn); },
    removeEventListener(type, fn) { listeners.get(type)?.delete(fn); },
    querySelector(selector) { return selector === "#msgs" ? msgsNode : msgsNode.querySelector(selector); },
    querySelectorAll(selector) { return msgsNode.querySelectorAll(selector); },
    createElement(tag) { const element = fakeElement(tag, doc); return element; },
  };
  msgsNode.doc = doc;
  return doc;
}
/* A message turn with one bubble and one chip, attached to the chat list. */
function disclosureScene({roster = () => [{name:"Anna", mine:false, state:"confirmed"}], msgs, panel, bubbleRect = fakeRect(100, 200, 300, 300), chipRect = fakeRect(110, 270, 150, 290), viewport} = {}) {
  const doc = fakeDocument({msgs, panel, viewport});
  const turn = fakeElement("div", doc, {dataset:{cseq:"7", gkey:"KEY"}});
  const bubble = fakeElement("div", doc, {classes:["bubble"], rect:bubbleRect});
  const chip = fakeElement("button", doc, {classes:["reaction-chip"], dataset:{reaction:"thumbs_up", rtarget:"7"}, rect:chipRect});
  doc.msgsNode.appendChild(turn); turn.appendChild(bubble); bubble.appendChild(chip);
  const picks = [];
  const calls = [];
  const ui = createReactionInteraction({doc, onChoose:destination => picks.push(destination), roster:(key, target, emoji) => { calls.push([key, target, emoji]); return roster(key, target, emoji); }});
  const click = (target, extra = {}) => { const event = {type:"click", target, defaultPrevented:false, stopped:false, preventDefault() { this.defaultPrevented = true; }, stopPropagation() { this.stopped = true; }, ...extra}; return {event, handled:ui.handle(event)}; };
  const hover = (target, relatedTarget = null, type = "pointerover", pointerType = "mouse") => ui.hover({type, pointerType, target, relatedTarget});
  const panelNode = () => bubble.children.find(child => child.classes.has("reaction-disclosure")) || null;
  return {doc, turn, bubble, chip, picks, calls, ui, click, hover, panelNode, msgs:doc.msgsNode};
}
test("hover previews appear only for mouse pointers, never take focus, and do not change the pinned state", () => {
  const s = disclosureScene();
  s.hover(s.chip, null, "pointerover", "touch");
  assert.equal(s.ui.disclosure(), null, "a touch pointer does not reveal the hover preview");
  s.hover(s.chip, null, "pointerover", "pen");
  assert.equal(s.ui.disclosure(), null, "a pen does not reveal the hover preview");
  s.hover(s.chip);
  assert.deepEqual([s.ui.disclosure().pinned, s.ui.disclosure().sheet], [false, false]);
  assert.ok(s.panelNode().classes.has("reaction-hover"));
  assert.equal(s.doc.activeElement, null, "hover never moves focus");
  assert.equal(s.chip.getAttribute("aria-expanded"), null, "hover is not an expanded disclosure");
  s.hover(s.chip);
  assert.equal(s.bubble.children.filter(child => child.classes.has("reaction-disclosure")).length, 1, "a repeated enter keeps one preview");
  s.hover(s.doc.msgsNode, null, "pointerover");
  assert.ok(s.ui.disclosure(), "pointers over the message list leave the preview alone");
  s.click(s.chip);
  s.hover(s.chip, null, "pointerout");
});
test("hover transitions keep the preview while the pointer moves onto the panel or inside the chip, and hide it when it leaves both", () => {
  const s = disclosureScene();
  s.hover(s.chip);
  const panel = s.panelNode();
  s.hover(s.chip, panel, "pointerout");
  assert.ok(s.ui.disclosure(), "moving from chip onto its panel keeps the preview");
  s.hover(panel, s.chip, "pointerout");
  assert.ok(s.ui.disclosure(), "moving from the panel back onto its chip keeps it");
  const inner = fakeElement("span", s.doc, {});
  s.chip.appendChild(inner);
  s.hover(inner, s.chip, "pointerout");
  assert.ok(s.ui.disclosure(), "a pointer moving between a chip's own parts keeps the preview");
  s.hover(s.chip, null, "pointerout");
  assert.equal(s.ui.disclosure(), null, "leaving the chip into nothing hides the preview");
  assert.equal(s.panelNode(), null);
  s.hover(s.chip);
  s.hover(s.panelNode(), null, "pointerout");
  assert.equal(s.ui.disclosure(), null, "leaving the panel into nothing hides the preview");
  s.hover(s.chip);
  s.click(s.chip);
  s.hover(s.chip, null, "pointerout");
  assert.equal(s.ui.disclosure().pinned, true, "a pinned disclosure survives hover departure");
});
test("hover over a different chip is ignored while a pinned disclosure is open", () => {
  const s = disclosureScene();
  s.click(s.chip);
  const other = fakeElement("button", s.doc, {classes:["reaction-chip"], dataset:{reaction:"heart", rtarget:"7"}, rect:fakeRect(160, 270, 190, 290)});
  s.bubble.appendChild(other);
  s.hover(other);
  assert.equal(s.ui.disclosure().chip, s.chip, "the pinned panel is not replaced by a preview");
});
test("a pinned click on a chip opens a wide panel focused for the keyboard and closes on the same chip", () => {
  const s = disclosureScene();
  const { event, handled } = s.click(s.chip);
  assert.equal(handled, true);
  assert.ok(event.stopped && event.defaultPrevented, "the chip click never reaches the bubble or its actions");
  assert.deepEqual([s.ui.disclosure().pinned, s.ui.disclosure().sheet], [true, false]);
  assert.equal(s.chip.getAttribute("aria-expanded"), "true");
  assert.equal(s.doc.activeElement, s.panelNode(), "the pinned panel takes focus so screen readers announce it");
  assert.equal(s.doc.listenerCount("keydown"), 1);
  assert.ok(s.calls.some(call => call[0] === "KEY" && call[1] === 7 && call[2] === "thumbs_up"), "the roster is read for this chip only");
  s.click(s.chip);
  assert.equal(s.ui.disclosure(), null, "the same chip closes its own disclosure");
  assert.equal(s.chip.getAttribute("aria-expanded"), "false");
  assert.equal(s.doc.listenerCount("keydown"), 0, "the keydown listener is removed with the disclosure");
  assert.equal(s.doc.activeElement, s.chip, "focus returns to the chip when the focused panel closes");
  assert.deepEqual(s.picks, [], "inspection never chooses a reaction");
});
test("a same-chip hover preview upgrades to a pinned disclosure on click, and a different chip switches it", () => {
  const s = disclosureScene();
  s.hover(s.chip);
  const preview = s.panelNode();
  s.click(s.chip);
  assert.equal(s.panelNode(), preview, "the preview is the same element that becomes pinned");
  assert.equal(s.ui.disclosure().pinned, true);
  const other = fakeElement("button", s.doc, {classes:["reaction-chip"], dataset:{reaction:"heart", rtarget:"7"}, rect:fakeRect(160, 270, 190, 290)});
  s.bubble.appendChild(other);
  s.click(other);
  assert.equal(s.ui.disclosure().chip, other, "a different chip takes over the single disclosure");
  assert.equal(s.chip.getAttribute("aria-expanded"), "false");
  assert.equal(s.bubble.children.filter(child => child.classes.has("reaction-disclosure")).length, 1, "only one disclosure exists at a time");
});
test("a chip whose roster is unavailable opens nothing", () => {
  const s = disclosureScene({roster:() => null});
  const { event, handled } = s.click(s.chip);
  assert.equal(handled, true, "the click is still consumed");
  assert.equal(event.stopped, true);
  assert.equal(s.ui.disclosure(), null);
  assert.equal(s.chip.getAttribute("aria-expanded"), null);
});
test("panels that cannot fit the chat width or height open as a compact sheet with an explicit close control", () => {
  const wide = disclosureScene({panel:{width:600, height:90}});
  wide.click(wide.chip);
  assert.equal(wide.ui.disclosure().sheet, true);
  assert.ok(wide.panelNode().classes.has("reaction-sheet"));
  const close = fakeElement("button", wide.doc, {dataset:{reactionClose:""}});
  wide.panelNode().appendChild(close);
  const { event } = wide.click(close);
  assert.equal(event.stopped, true, "the close control is consumed");
  assert.equal(wide.ui.disclosure(), null, "the sheet's close control dismisses it");
  assert.equal(wide.doc.activeElement, wide.chip, "focus returns to the chip after an explicit close");
  const tall = disclosureScene({msgs:fakeRect(0, 0, 600, 100), bubbleRect:fakeRect(100, 30, 300, 70), chipRect:fakeRect(110, 40, 150, 60)});
  tall.click(tall.chip);
  assert.equal(tall.ui.disclosure().sheet, true, "a panel with no vertical room anywhere opens as a sheet");
});
test("a wide panel is placed inside the chat above or below its chip and clamped horizontally", () => {
  const above = disclosureScene();
  above.click(above.chip);
  const panel = above.panelNode();
  assert.ok(panel.classes.has("above"));
  assert.equal(panel.style.left, "10px", "the panel's left edge follows the chip, relative to the bubble");
  assert.equal(panel.style.top, "70px", "the panel's bottom edge meets the chip's top");
  const right = disclosureScene({chipRect:fakeRect(540, 270, 548, 290)});
  right.click(right.chip);
  assert.equal(right.panelNode().style.left, "332px", "a chip near the right edge clamps the panel inside the chat");
  const below = disclosureScene({bubbleRect:fakeRect(100, 10, 300, 110), chipRect:fakeRect(110, 20, 150, 40)});
  below.click(below.chip);
  assert.ok(!below.panelNode().classes.has("above"), "a chip near the top opens its panel below");
  assert.equal(below.panelNode().style.top, "30px", "the panel's top edge meets the chip's bottom");
  const hoverWide = disclosureScene({panel:{width:500, height:90}});
  hoverWide.hover(hoverWide.chip);
  assert.equal(hoverWide.panelNode().style.left, "-8px", "a preview wider than the chat clamps to the chat's left edge and never becomes a sheet");
  assert.ok(!hoverWide.panelNode().classes.has("reaction-sheet"));
  const hoverBelow = disclosureScene({msgs:fakeRect(0, 0, 400, 100), bubbleRect:fakeRect(100, 5, 300, 35), chipRect:fakeRect(110, 10, 150, 30)});
  hoverBelow.hover(hoverBelow.chip);
  assert.ok(!hoverBelow.panelNode().classes.has("above"), "with no room above, a preview takes the larger side");
  const hoverAbove = disclosureScene({msgs:fakeRect(0, 0, 400, 100), bubbleRect:fakeRect(100, 60, 300, 100), chipRect:fakeRect(110, 70, 150, 90)});
  hoverAbove.hover(hoverAbove.chip);
  assert.ok(hoverAbove.panelNode().classes.has("above"), "with more room above than below, a preview takes the side above");
});
test("clicks inside a disclosure are consumed, outside clicks dismiss without being consumed, and both leave selection alone", () => {
  const s = disclosureScene();
  s.click(s.chip);
  const inside = fakeElement("li", s.doc, {});
  s.panelNode().appendChild(inside);
  const { event, handled } = s.click(inside);
  assert.equal(handled, true);
  assert.equal(event.stopped, true, "a click on the panel never reaches the bubble");
  assert.ok(s.ui.disclosure(), "a click on the panel keeps it open");
  const outside = fakeElement("div", s.doc, {});
  s.msgs.appendChild(outside);
  const dismiss = s.click(outside);
  assert.equal(dismiss.handled, false, "an outside click is not consumed, like the existing picker");
  assert.equal(dismiss.event.stopped, false);
  assert.equal(s.ui.disclosure(), null, "an outside click dismisses the disclosure");
  assert.equal(s.doc.listenerCount("keydown"), 0);
});
test("Escape dismisses a pinned disclosure and is not consumed when nothing is open", () => {
  const s = disclosureScene();
  assert.equal(s.doc.listenerCount("keydown"), 0, "no keyboard listener exists without a disclosure, so Escape passes through");
  const escape = {key:"Escape", target:s.chip, preventDefault() { this.defaultPrevented = true; }, stopPropagation() { this.stopped = true; }};
  s.click(s.chip);
  const other = {key:"a", target:s.chip, preventDefault() {}, stopPropagation() { this.stopped = true; }};
  s.doc.listener("keydown")(other);
  assert.ok(s.ui.disclosure(), "other keys leave the disclosure open");
  assert.equal(other.stopped, undefined);
  s.doc.listener("keydown")(escape);
  assert.equal(escape.stopped, true, "Escape is consumed by the open disclosure");
  assert.equal(escape.defaultPrevented, true);
  assert.equal(s.ui.disclosure(), null);
  assert.equal(s.doc.listenerCount("keydown"), 0);
  s.hover(s.chip);
  assert.equal(s.doc.listenerCount("keydown"), 0, "hover previews install no keyboard listener");
});
test("a pinned disclosure and the reaction picker never stack, in either order", () => {
  const s = disclosureScene();
  const entry = fakeElement("span", s.doc, {dataset:{reactionEntry:""}});
  const trigger = fakeElement("button", s.doc, {dataset:{reactionOpen:""}});
  const chooser = fakeElement("span", s.doc, {classes:["reaction-chooser"]});
  s.bubble.appendChild(entry); entry.appendChild(trigger); entry.appendChild(chooser);
  s.click(s.chip);
  s.click(trigger);
  assert.equal(s.ui.disclosure(), null, "opening the picker closes the disclosure");
  assert.equal(s.ui.current(), entry);
  s.click(s.chip);
  assert.equal(s.ui.current(), null, "opening a disclosure closes the picker");
  assert.equal(s.ui.disclosure().chip, s.chip);
  assert.equal(chooser.hidden, true);
});
test("sync re-anchors a pinned disclosure to a replaced chip, refreshes names, and closes when its data is gone", () => {
  let names = "Anna";
  const s = disclosureScene({roster:() => names ? [{name:names, mine:false, state:"confirmed"}] : null});
  s.click(s.chip);
  const panel = s.panelNode();
  assert.match(panel.innerHTML, /Anna/);
  const writesBefore = panel.writes;
  s.ui.sync();
  assert.equal(panel.writes, writesBefore, "unchanged data does not rewrite the panel");
  const replacement = fakeElement("button", s.doc, {classes:["reaction-chip"], dataset:{reaction:"thumbs_up", rtarget:"7"}, rect:fakeRect(110, 270, 150, 290)});
  s.chip.remove();
  s.bubble.appendChild(replacement);
  s.ui.sync();
  assert.equal(s.ui.disclosure().chip, replacement, "a poll that replaces the chip re-anchors the same panel");
  assert.equal(s.panelNode(), panel);
  names = "Ben";
  s.ui.sync();
  assert.match(panel.innerHTML, /Ben/, "a name update refreshes the open roster");
  replacement.remove();
  s.ui.sync();
  assert.equal(s.ui.disclosure(), null, "a chip that disappears closes its disclosure");
  assert.equal(s.panelNode(), null);
  assert.equal(s.doc.listenerCount("keydown"), 0);
  s.ui.sync();
  assert.equal(s.ui.disclosure(), null, "sync without a disclosure does nothing");
});
test("sync closes a disclosure whose copy has retired, and a stale chip closes it on the next interaction", () => {
  let available = true;
  const s = disclosureScene({roster:() => available ? [{name:"Anna", mine:false, state:"confirmed"}] : null});
  s.click(s.chip);
  available = false;
  s.ui.sync();
  assert.equal(s.ui.disclosure(), null, "a retired or inaccessible copy closes the disclosure");
  s.click(s.chip);
  assert.equal(s.ui.disclosure(), null, "with no roster, a click opens nothing");
  available = true;
  s.hover(s.chip);
  s.chip.remove();
  s.hover(s.chip, null, "pointerout");
  assert.equal(s.ui.disclosure(), null, "a removed chip's preview closes on the next pointer event");
  assert.equal(s.doc.listenerCount("keydown"), 0);
});
test("close releases the disclosure, its panel, the keydown listener, and the picker", () => {
  const s = disclosureScene();
  s.click(s.chip);
  s.ui.close();
  assert.equal(s.ui.disclosure(), null);
  assert.equal(s.panelNode(), null);
  assert.equal(s.chip.getAttribute("aria-expanded"), "false");
  assert.equal(s.doc.listenerCount("keydown"), 0);
  s.ui.close();
  assert.equal(s.ui.disclosure(), null, "closing twice is harmless");
});
test("a disclosure whose panel node is absent still closes its state", () => {
  const s = disclosureScene();
  s.click(s.chip);
  s.panelNode().remove();
  s.ui.close();
  assert.equal(s.ui.disclosure(), null);
});

test("the fallbacks are decided on purpose: null operation and entry lists, a missing key, bubble, message list or query, and no roster", () => {
  const participants = need("reactionParticipants");
  const comments = participantComments();
  const events = [participantEvent(11, "bob", "heart", true)];
  assert.deepEqual(participants(events, comments, {target:10, emoji:"heart", operations:null}), [{author:"bob", mine:false, state:"confirmed"}], "null operations are treated as none");
  assert.equal(need("reactionRosterHTML")(null, "👍", "wide"), '<p class="reaction-disclosure-summary"></p><ul class="reaction-roster"></ul>', "a null roster renders no names");
  assert.equal(need("reactionPreviewText")(null, "👍"), "", "a null roster has no preview sentence");

  const lone = disclosureScene();
  const detachedOpener = createReactionInteraction({doc:lone.doc, onChoose() {}});
  assert.equal(detachedOpener.handle({type:"click", target:lone.chip, preventDefault() {}, stopPropagation() {}}), true, "a controller without a roster source still consumes the chip click");
  assert.equal(detachedOpener.disclosure(), null, "with no roster, nothing opens");

  const keyless = disclosureScene();
  const bare = fakeElement("div", keyless.doc, {dataset:{cseq:"9"}});
  keyless.msgs.appendChild(bare);
  const bareBubble = fakeElement("div", keyless.doc, {classes:["bubble"], rect:fakeRect(100, 200, 300, 300)});
  const bareChip = fakeElement("button", keyless.doc, {classes:["reaction-chip"], dataset:{reaction:"thumbs_up", rtarget:"9"}, rect:fakeRect(110, 270, 150, 290)});
  bare.appendChild(bareBubble); bareBubble.appendChild(bareChip);
  keyless.click(bareChip);
  assert.equal(keyless.calls.at(-1)[0], "", "a chip outside any keyed turn reads its roster with an empty key");
  assert.ok(keyless.ui.disclosure(), "the roster decides, not the key's presence");

  const floating = disclosureScene();
  const loose = fakeElement("button", floating.doc, {classes:["reaction-chip"], dataset:{reaction:"thumbs_up", rtarget:"7"}, rect:fakeRect(110, 270, 150, 290)});
  floating.turn.appendChild(loose);
  floating.click(loose);
  assert.equal(floating.ui.disclosure(), null, "a chip with no bubble to hold its panel opens nothing");
  assert.equal(floating.doc.listenerCount("keydown"), 0);

  const bare2 = disclosureScene();
  bare2.doc.querySelector = () => null;
  bare2.hover(bare2.chip);
  assert.ok(bare2.ui.disclosure(), "without a message list, a preview still opens with zero bounds");
  assert.equal(bare2.ui.disclosure().sheet, false);

  const unqueryable = disclosureScene();
  unqueryable.click(unqueryable.chip);
  delete unqueryable.doc.querySelectorAll;
  unqueryable.chip.remove();
  unqueryable.ui.sync();
  assert.equal(unqueryable.ui.disclosure(), null, "a document that cannot query chips cannot re-anchor, so the disclosure closes");

  const unavailable = disclosureScene({roster:() => null});
  unavailable.hover(unavailable.chip);
  assert.equal(unavailable.ui.disclosure(), null, "a preview whose roster is unavailable shows nothing");
});

/* Review regressions: stacking, expanded state on replacement, empty rosters, orphan picker choices. */
test("a hover over a chip while the picker is open shows no preview, and a preview upgrades to pinned after the picker closes", () => {
  const s = disclosureScene();
  const entry = fakeElement("span", s.doc, {dataset:{reactionEntry:""}});
  const trigger = fakeElement("button", s.doc, {dataset:{reactionOpen:""}});
  const chooser = fakeElement("span", s.doc, {classes:["reaction-chooser"]});
  s.bubble.appendChild(entry); entry.appendChild(trigger); entry.appendChild(chooser);
  s.click(trigger);
  assert.equal(s.ui.current(), entry);
  s.hover(s.chip);
  assert.equal(s.ui.disclosure(), null, "the picker and a preview must not stack");
  assert.equal(s.ui.current(), entry, "the picker stays as it was");
  assert.equal(s.panelNode(), null);
  s.click(trigger);
  assert.equal(s.ui.current(), null, "the same trigger closes the picker");
  s.hover(s.chip);
  assert.equal(s.ui.disclosure().pinned, false, "once the picker is closed, the preview shows again");
  s.click(s.chip);
  assert.equal(s.ui.disclosure().pinned, true, "the preview upgrades to pinned in place");
  assert.equal(s.ui.current(), null);
});

test("a replacement chip keeps the expanded state of its disclosure, and clears it when closed", () => {
  const s = disclosureScene();
  s.click(s.chip);
  const replacement = fakeElement("button", s.doc, {classes:["reaction-chip"], dataset:{reaction:"thumbs_up", rtarget:"7"}, rect:fakeRect(110, 270, 150, 290)});
  s.chip.remove();
  s.bubble.appendChild(replacement);
  s.ui.sync();
  assert.equal(s.ui.disclosure().chip, replacement);
  assert.equal(replacement.getAttribute("aria-expanded"), "true", "the replacement is expanded while its disclosure is pinned");
  s.ui.close();
  assert.equal(replacement.getAttribute("aria-expanded"), "false", "closing clears the expanded state on the chip that is shown");
});

test("a preview that re-anchors to a replacement chip is not expanded", () => {
  const s = disclosureScene();
  s.hover(s.chip);
  const replacement = fakeElement("button", s.doc, {classes:["reaction-chip"], dataset:{reaction:"thumbs_up", rtarget:"7"}, rect:fakeRect(110, 270, 150, 290)});
  s.chip.remove();
  s.bubble.appendChild(replacement);
  s.ui.sync();
  assert.equal(s.ui.disclosure().chip, replacement);
  assert.equal(replacement.getAttribute("aria-expanded"), "false");
});

test("an empty participant roster opens nothing, and an open disclosure closes when its roster empties", () => {
  const none = disclosureScene({roster:() => []});
  none.click(none.chip);
  assert.equal(none.ui.disclosure(), null, "a click on a chip with no participants opens no blank disclosure");
  none.hover(none.chip);
  assert.equal(none.ui.disclosure(), null, "a preview with no participants shows nothing");
  let entries = [{name:"Anna", mine:false, state:"confirmed"}];
  const s = disclosureScene({roster:() => entries});
  s.click(s.chip);
  assert.ok(s.ui.disclosure());
  entries = [];
  s.ui.sync();
  assert.equal(s.ui.disclosure(), null, "an update that empties the roster closes the disclosure");
  assert.equal(s.panelNode(), null);
  assert.equal(s.doc.listenerCount("keydown"), 0);
});

test("a picker choice outside any reaction entry is a harmless no-op, as it was before", () => {
  const s = disclosureScene();
  const button = fakeElement("button", s.doc, {dataset:{reactionChoice:"heart"}});
  const result = s.click(button);
  assert.equal(result.handled, true);
  assert.equal(result.event.stopped, true, "the orphan choice is still consumed");
  assert.deepEqual(s.picks, [], "no reaction is chosen without an entry");
});

test("a pinned panel is bounded by the chat width, not by a short bubble, and a short bubble never widens", () => {
  const s = disclosureScene({bubbleRect:fakeRect(100, 200, 160, 230), chipRect:fakeRect(110, 210, 150, 226)});
  s.click(s.chip);
  assert.equal(s.panelNode().style.maxWidth, "min(18rem, 584px)", "the roster may extend beyond a 60px bubble, up to 18rem and the chat width less margins");
  assert.equal(s.ui.disclosure().sheet, false, "a readable roster beside a short bubble stays a wide panel");
  assert.equal(s.ui.disclosure().chip, s.chip);
});

test("a compact chat pane opens pinned disclosures as sheets even when a small roster would fit beside the chip", () => {
  const compact = disclosureScene({msgs:fakeRect(0, 0, 390, 600), bubbleRect:fakeRect(60, 200, 350, 300), chipRect:fakeRect(70, 270, 110, 290)});
  compact.click(compact.chip);
  assert.equal(compact.ui.disclosure().sheet, true, "at the compact breakpoint a tap opens the sheet");
  assert.ok(compact.panelNode().classes.has("reaction-sheet"));
  const boundary = disclosureScene({msgs:fakeRect(0, 0, 480, 600), bubbleRect:fakeRect(60, 200, 350, 300), chipRect:fakeRect(70, 270, 110, 290)});
  boundary.click(boundary.chip);
  assert.equal(boundary.ui.disclosure().sheet, true, "the breakpoint itself is compact");
  const wider = disclosureScene({msgs:fakeRect(0, 0, 481, 600), bubbleRect:fakeRect(60, 200, 350, 300), chipRect:fakeRect(70, 270, 110, 290)});
  wider.click(wider.chip);
  assert.equal(wider.ui.disclosure().sheet, false, "above the breakpoint, a fitting roster is a wide panel");
});

test("hover previews stay previews at compact widths, and a compact pinned sheet keeps its own state across updates", () => {
  const compact = disclosureScene({msgs:fakeRect(0, 0, 390, 600), bubbleRect:fakeRect(60, 200, 350, 300), chipRect:fakeRect(70, 270, 110, 290)});
  compact.hover(compact.chip);
  assert.equal(compact.ui.disclosure().pinned, false);
  assert.equal(compact.ui.disclosure().sheet, false, "a preview is never a sheet");
  assert.ok(compact.panelNode().classes.has("reaction-hover"));
  compact.click(compact.chip);
  assert.equal(compact.ui.disclosure().sheet, true, "the same chip upgraded to pinned becomes the compact sheet");
  compact.ui.sync();
  assert.equal(compact.ui.disclosure().sheet, true, "a sync keeps the sheet while the chat stays compact");
});

test("names keep their own case, and only the own entry is capitalised at the start of a sentence", () => {
  const preview = need("reactionPreviewText");
  assert.equal(preview([{name:"anna", mine:false, state:"confirmed"}], "❤️"), "anna reacted with ❤️", "a lowercase display name is shown as chosen");
  assert.equal(preview([{name:"ßtraße", mine:false, state:"confirmed"}], "👍"), "ßtraße reacted with 👍", "no Unicode case expansion reaches another person's name");
  assert.equal(preview([{name:"anna", mine:false, state:"confirmed"}, {mine:true, state:"confirmed"}], "👍"), "anna and you reacted with 👍");
  assert.equal(preview([{mine:true, state:"confirmed"}, {name:"anna", mine:false, state:"confirmed"}], "👍"), "You and anna reacted with 👍", "the own word is capitalised when it starts the sentence");
  assert.equal(preview([{name:"zoë", mine:false, state:"confirmed"}, {name:"bob", mine:false, state:"confirmed"}, {name:"cy", mine:false, state:"confirmed"}, {name:"dee", mine:false, state:"confirmed"}], "🎉"), "zoë, bob, cy and dee reacted with 🎉");
});

test("a preview survives a diagonal pointer path across its bubble, but not leaving the bubble for the message", () => {
  const s = disclosureScene();
  const text = fakeElement("p", s.doc, {});
  s.bubble.appendChild(text);
  s.hover(s.chip);
  const panel = s.panelNode();
  s.hover(s.chip, text, "pointerout");
  assert.ok(s.ui.disclosure(), "moving from the chip sideways onto the bubble's own content keeps the preview");
  s.hover(text, panel, "pointerout");
  assert.ok(s.ui.disclosure(), "a bubble content to panel move keeps it");
  s.hover(text, s.msgs, "pointerout");
  assert.equal(s.ui.disclosure(), null, "leaving the bubble for the message closes the preview");
  s.hover(s.chip);
  s.click(s.chip);
  s.hover(s.chip, text, "pointerout");
  assert.equal(s.ui.disclosure().pinned, true, "a pinned disclosure ignores hover departure");
  s.ui.close();
});

test("a focused sheet close control keeps focus when its disclosure refreshes in place", () => {
  let names = "Anna";
  const s = disclosureScene({roster:() => [{name:names, mine:false, state:"confirmed"}], panel:{width:600, height:90}});
  s.click(s.chip);
  assert.equal(s.ui.disclosure().sheet, true);
  const close = s.panelNode().children.find(child => child.dataset.reactionClose !== undefined);
  assert.ok(close, "the sheet's close control exists");
  close.focus();
  assert.equal(s.doc.activeElement, close);
  names = "Ben";
  s.ui.sync();
  assert.match(s.panelNode().innerHTML, /Ben/, "the roster refreshed");
  assert.equal(s.ui.disclosure().sheet, true, "still a sheet");
  const rebuilt = s.panelNode().children.find(child => child.dataset.reactionClose !== undefined);
  assert.ok(rebuilt && rebuilt !== close, "the parser rebuilt the close control");
  assert.equal(s.doc.activeElement, rebuilt, "focus moves to the rebuilt close control, not lost");
});

test("focus on the panel itself survives a refresh, and focus outside the panel is never taken", () => {
  let names = "Anna";
  const s = disclosureScene({roster:() => [{name:names, mine:false, state:"confirmed"}]});
  s.click(s.chip);
  const panel = s.panelNode();
  panel.focus();
  names = "Ben";
  s.ui.sync();
  assert.equal(s.doc.activeElement, panel, "focus on the panel stays there");
  const outside = fakeElement("button", s.doc, {});
  s.msgs.appendChild(outside);
  outside.focus();
  names = "Cy";
  s.ui.sync();
  assert.equal(s.doc.activeElement, outside, "a refresh never steals focus from outside the disclosure");
  assert.match(panel.innerHTML, /Cy/);
});

test("a focused child of a wide panel that is rebuilt falls back to the panel for focus", () => {
  let names = "Anna";
  const s = disclosureScene({roster:() => [{name:names, mine:false, state:"confirmed"}]});
  s.click(s.chip);
  const panel = s.panelNode();
  const row = fakeElement("li", s.doc, {});
  panel.appendChild(row);
  row.focus();
  names = "Ben";
  s.ui.sync();
  assert.equal(s.doc.activeElement, panel, "focus that the rebuilt roster destroyed falls back to the panel");
});

test("a hover or wide panel that becomes a sheet clears its anchored inline width and offsets", () => {
  const s = disclosureScene({panel:{width:600, height:90}});
  s.hover(s.chip);
  const panel = s.panelNode();
  assert.equal(panel.style.maxWidth, "min(18rem, 584px)", "a preview is bounded by the chat width");
  assert.notEqual(panel.style.left, "", "a preview is anchored");
  s.click(s.chip);
  assert.equal(panel.classes.has("reaction-sheet"), true, "a pinned panel too wide for the chat is a sheet");
  assert.equal(panel.style.maxWidth, "", "the sheet clears the anchored width, so its declared margins apply");
  assert.equal(panel.style.left, "");
  assert.equal(panel.style.top, "");
});
