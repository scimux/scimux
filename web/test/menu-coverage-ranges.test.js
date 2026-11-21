import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { retainedExecutableCoverage } from './menu-coverage-ranges.mjs';
import { execFileSync } from 'node:child_process';

const range = (startOffset, endOffset, count = 1) => ({ startOffset, endOffset, count });
const fn = (start, end, branches = [], count = 1, isBlockCoverage = true) => ({ functionName: 'synthetic', isBlockCoverage, ranges: [range(start, end, count), ...branches] });
const oldText = 'function a(){\n  if (x) yes();\n  else no();\n}\nfunction b(){ return 1; }\n';
const newText = '// added\n' + oldText;
const mapping = new Map([[1, 2], [2, 3], [3, 4], [4, 5], [5, 6], [6, 7]]);
const endA = oldText.indexOf('function b');
const shift = r => range(r.startOffset + 9, r.endOffset + 9, r.count);
const b = fn(endA, oldText.length - 1);
const a = fn(0, endA - 1, [range(oldText.indexOf('else'), oldText.indexOf('no();') + 5)]);
const shifted = f => ({ ...f, ranges: f.ranges.map(shift) });

test('same-name parent cannot credit an uncovered nested function branch', () => {
 const old='function x() {\n branch();\n}\n', next='function x() {\n if (flag) {\nfunction x() {\n changed();\n branch();\n}\n }\n}\n';
 const before=[{functionName:'x',isBlockCoverage:true,ranges:[range(0,old.lastIndexOf('}')+1),range(old.indexOf('branch'),old.indexOf('branch')+9)]}];
 const child=next.indexOf('function x',1),branch=next.indexOf('branch');
 const after=[{functionName:'x',isBlockCoverage:true,ranges:[range(0,next.lastIndexOf('}')+1),range(next.indexOf('if'),next.lastIndexOf('}')-1)]},{functionName:'x',isBlockCoverage:true,ranges:[range(child,next.indexOf('\n}',child)+2),range(branch,branch+9,0)]}];
 assert.ok(retainedExecutableCoverage(before,after,old,next,[[1,1],[2,5],[3,6]]).failures.length);
});

test('even matching parent boundaries cannot hide a nested zero-count function', () => {
 const text='0123456789\n',map=new Map([[1,1]]);
 const before=[{functionName:'x',isBlockCoverage:true,ranges:[range(0,10),range(2,8)]}];
 const after=[{functionName:'x',isBlockCoverage:true,ranges:[range(0,10),range(2,8)]},{functionName:'child',isBlockCoverage:true,ranges:[range(3,7,0)]}];
 assert.ok(retainedExecutableCoverage(before,after,text,text,map).failures.length);
});

test('a separately mapped nested function retains its own coverage', () => {
 const text='0123456789\n',map=new Map([[1,1]]);
 const parent={functionName:'x',isBlockCoverage:true,ranges:[range(0,10),range(2,8)]};
 const child={functionName:'child',isBlockCoverage:true,ranges:[range(3,7),range(4,6)]};
 assert.deepEqual(retainedExecutableCoverage([parent,child],[parent,child],text,text,map).failures,[]);
});

test('an edited nested function with both boundaries mapped retains only its own covered branch', () => {
 const old='function parent(){\n function child(){\n  old();\n  kept();\n }\n}\n';
 const next='function parent(){\n function child(){\n  changed();\n  kept();\n }\n}\n';
 const map=new Map([[1,1],[2,2],[4,4],[5,5],[6,6]]);
 const make=(text,count)=>{const child=text.indexOf('function child'),end=text.indexOf('\n }')+3,start=text.indexOf('kept()');return [
  {functionName:'parent',isBlockCoverage:true,ranges:[range(0,text.lastIndexOf('}')+1),range(start,start+6)]},
  {functionName:'child',isBlockCoverage:true,ranges:[range(child,end),range(start,start+6,count)]}];};
 assert.deepEqual(retainedExecutableCoverage(make(old,1),make(next,1),old,next,map).failures,[]);
 assert.ok(retainedExecutableCoverage(make(old,1),make(next,0),old,next,map).failures.length);
});

test('an edited nested non-block function cannot inherit a positive parent counter', () => {
 const old='function parent(){\n function child(){\n  old();\n  kept();\n }\n}\n';
 const next='function parent(){\n function child(){\n  changed();\n  kept();\n }\n}\n';
 const map=new Map([[1,1],[2,2],[4,4],[5,5],[6,6]]);
 const make=(text,count)=>{const child=text.indexOf('function child'),end=text.indexOf('\n }')+3,start=text.indexOf('kept()');return [
  {functionName:'parent',isBlockCoverage:true,ranges:[range(0,text.lastIndexOf('}')+1),range(start,start+6)]},
  {functionName:'child',isBlockCoverage:false,ranges:[range(child,end,count)]}];};
 assert.ok(retainedExecutableCoverage(make(old,1),make(next,0),old,next,map).failures.length);
});

test('an edited mapped nested function cannot hide its own zero branch', () => {
 const old='function parent(){\n function child(){\n  old();\n  kept();\n }\n}\n';
 const next='function parent(){\n function child(){\n  changed();\n  kept();\n }\n}\n';
 const map=new Map([[1,1],[2,2],[4,4],[5,5],[6,6]]);
 const make=(text,zero)=>{const child=text.indexOf('function child'),end=text.indexOf('\n }')+3,start=text.indexOf('kept()');return [
  {functionName:'parent',isBlockCoverage:true,ranges:[range(0,text.lastIndexOf('}')+1),range(start,start+6)]},
  {functionName:'child',isBlockCoverage:true,ranges:[range(child,end),range(start,start+6),range(start+1,start+2,zero?0:1)]}];};
 assert.ok(retainedExecutableCoverage(make(old,false),make(next,true),old,next,map).failures.length);
});

test('an edited nested function cannot hide a missing previously covered descendant', () => {
 const old='function parent(){\n function child(){\n  old();\n  function grand(){\n   oldg();\n   kept();\n  }\n }\n}\n';
 const next='function parent(){\n function child(){\n  changed();\n  function grand(){\n   changedg();\n   kept();\n  }\n }\n}\n';
 const map=new Map([[1,1],[2,2],[4,4],[6,6],[7,7],[8,8],[9,9]]);
 const make=(text,grand)=>{const child=text.indexOf('function child'),end=text.indexOf('\n }')+3,start=text.indexOf('kept()'),g=text.indexOf('function grand'),ge=text.indexOf('\n  }',g)+4;return [
  {functionName:'parent',isBlockCoverage:true,ranges:[range(0,text.lastIndexOf('}')+1),range(start,start+6)]},
  {functionName:'child',isBlockCoverage:true,ranges:[range(child,end)]},
  ...(grand?[{functionName:'grand',isBlockCoverage:false,ranges:[range(g,ge)]}]:[])];};
 assert.ok(retainedExecutableCoverage(make(old,true),make(next,false),old,next,map).failures.length);
 assert.deepEqual(retainedExecutableCoverage(make(old,true),make(next,true),old,next,map).failures,[]);
 const zero=make(next,true);zero[2].ranges[0].count=0;
 assert.ok(retainedExecutableCoverage(make(old,true),zero,old,next,map).failures.length);
 const renamed=make(next,true);renamed[2].functionName='other';
 assert.ok(retainedExecutableCoverage(make(old,true),renamed,old,next,map).failures.length);
 const displaced=make(next,true);displaced[2].ranges[0].startOffset++;
 assert.ok(retainedExecutableCoverage(make(old,true),displaced,old,next,map).failures.length);
});

test('a different mapped nested boundary cannot identify an edited function', () => {
 const text='0123456789\n',map=new Map([[1,1]]);
 const parent={functionName:'p',isBlockCoverage:true,ranges:[range(0,10),range(4,6)]};
 const oldChild={functionName:'c',isBlockCoverage:true,ranges:[range(3,7)]};
 const newChild={functionName:'c',isBlockCoverage:true,ranges:[range(3,8)]};
 assert.ok(retainedExecutableCoverage([parent,oldChild],[parent,newChild],text,text,map).failures.length);
});

test('an explicitly identified whole script can grow only when its retained source still executes', () => {
 const old='branch();\n', extended=old+'// appended\n', wrapped='function wrapper(){\n'+old+'}\n';
 const root=(text,count=1)=>({functionName:'',isBlockCoverage:true,ranges:[range(0,text.length,count)]});
 const proof=[{kind:'script-root'}];
 assert.deepEqual(retainedExecutableCoverage([root(old)],[root(extended)],old,extended,new Map([[1,1]]),proof).failures,[]);
 const wrapper={functionName:'wrapper',isBlockCoverage:false,ranges:[range(0,wrapped.length-1,0)]};
 assert.ok(retainedExecutableCoverage([root(old)],[root(wrapped),wrapper],old,wrapped,new Map([[1,2]]),proof).failures.length);
});

test('whole-script mapping checks nested ownership and effective counters', () => {
 const old='function child(){ branch(); }\n',next=old+'// appended\n',map=new Map([[1,1]]),proof=[{kind:'script-root'}];
 const root=(text)=>({functionName:'',isBlockCoverage:true,ranges:[range(0,text.length)]});
 const child={functionName:'child',isBlockCoverage:true,ranges:[range(0,old.length-1)]};
 assert.deepEqual(retainedExecutableCoverage([root(old),child],[root(next),child],old,next,map,proof).failures,[]);
 const lost={...child,ranges:[range(0,old.length-1,0)]};
 assert.ok(retainedExecutableCoverage([root(old),child],[root(next),lost],old,next,map,proof).failures.length);
 assert.ok(retainedExecutableCoverage([root(old)],[child],old,next,map,proof).failures.length);
});

test('a script root cannot identify a distinct anonymous function with the same mapped range', () => {
 const old='function(){}\n',next='// before\n'+old+'// after\n',map=new Map([[1,2]]),proof=[{kind:'script-root'}];
 const root=text=>({functionName:'',isBlockCoverage:true,ranges:[range(0,text.length)]});
 const nested={functionName:'',isBlockCoverage:false,ranges:[range(10,10+old.length,0)]};
 assert.ok(retainedExecutableCoverage([root(old)],[root(next),nested],old,next,map,proof).failures.length);
 nested.ranges[0].count=1;
 nested.isBlockCoverage=true;
 assert.ok(retainedExecutableCoverage([root(old)],[root(next),nested],old,next,map,proof).failures.length);
});

test('a zero-count anonymous function sharing the whole-script range cannot inherit the root counter', () => {
 const text='x=>x',map=new Map([[1,1]]),proof=[{kind:'script-root'}];
 const f=count=>({functionName:'',isBlockCoverage:true,ranges:[range(0,text.length,count)]});
 assert.ok(retainedExecutableCoverage([f(1),f(1)],[f(1),f(0)],text,text,map,proof).failures.length);
});

test('actual V8 rejects a lost branch inside a nested same-name function', () => {
 const helper = new URL('./menu-coverage-ranges.mjs', import.meta.url).href;
 const script = `import assert from 'node:assert/strict';
 import inspector from 'node:inspector'; import vm from 'node:vm'; import {promisify} from 'node:util';
 import {retainedExecutableCoverage} from ${JSON.stringify(helper)};
 const session=new inspector.Session();session.connect();const post=promisify(session.post.bind(session));
 const old='function x(flag) {\\n if (flag) { branch(); }\\n}\\nx(true);x(false);\\n';
 const next='function x(flag) {\\n if (flag) {\\nfunction x(flag) {\\n changed();\\n if (flag) { branch(); }\\n}\\nx(false);\\n }\\n}\\nx(true);x(false);\\n';
 await post('Profiler.enable');await post('Profiler.startPreciseCoverage',{callCount:true,detailed:true});
 vm.runInNewContext(old,{branch(){},changed(){}},{filename:'synthetic-before.js'});
 vm.runInNewContext(next,{branch(){},changed(){}},{filename:'synthetic-after.js'});
 const data=await post('Profiler.takePreciseCoverage');await post('Profiler.stopPreciseCoverage');session.disconnect();
 const before=data.result.find(f=>f.url==='synthetic-before.js').functions.filter(f=>f.functionName==='x');
 const after=data.result.find(f=>f.url==='synthetic-after.js').functions.filter(f=>f.functionName==='x');
 assert.ok(retainedExecutableCoverage(before,after,old,next,[[1,1],[2,5],[3,6]]).failures.length);`;
 execFileSync(process.execPath,['--input-type=module','--eval',script],{env:{...process.env,NODE_V8_COVERAGE:''}});
});

test('retains shifted functions and branches inside changed files', () => {
  const result = retainedExecutableCoverage([a, b], [shifted(a), shifted(b)], oldText, newText, mapping);
  assert.deepEqual(result.failures, []);
  assert.equal(result.proof.filter(r => r.kind === 'function').length, 2);
  assert.equal(result.proof.filter(r => r.kind === 'branch').length, 3);
});

test('rejects an unchanged branch losing coverage even while its line remains covered', () => {
  const lost = shifted(a); lost.ranges[1].count = 0;
  const result = retainedExecutableCoverage([a, b], [lost, shifted(b)], oldText, newText, mapping);
  assert.equal(result.failures.length, 1);
  assert.match(result.failures[0], /branch/);
});

test('accepts a covered V8 branch split only when every subrange is covered', () => {
  const oldBranch=range(oldText.indexOf('else'),oldText.indexOf('no();')+5);
  const before=fn(0,endA-1,[oldBranch]);
  const moved=shift(oldBranch), mid=moved.startOffset+3;
  const split=fn(9,endA+8,[range(moved.startOffset,mid),range(mid,moved.endOffset)]);
  assert.deepEqual(retainedExecutableCoverage([before],[split],oldText,newText,mapping).failures,[]);
  split.ranges[2].count=0;
  assert.ok(retainedExecutableCoverage([before],[split],oldText,newText,mapping).failures.some(x=>x.includes('branch')));
});

test('a split with an uncovered nested child cannot preserve a covered branch', () => {
  const text='0123456789\n', map=new Map([[1,1]]);
  const before=fn(0,10,[range(2,8)]);
  const after=fn(0,10,[range(2,5),range(5,8),range(6,7,0)]);
  assert.ok(retainedExecutableCoverage([before],[after],text,text,map).failures.some(x=>x.includes('branch')));
});

test('gapped or missing split ranges cannot be filled by a positive function root', () => {
  const text='0123456789\n', map=new Map([[1,1]]), before=fn(0,10,[range(2,8)]);
  for (const after of [fn(0,10,[range(2,5),range(6,8)]),fn(0,10)])
    assert.ok(retainedExecutableCoverage([before],[after],text,text,map).failures.some(x=>x.includes('branch')));
});

test('an anonymous parent cannot supply coverage for a distinct child function', () => {
  const text='0123456789\n', map=new Map([[1,1]]);
  const before={...fn(2,8,[range(4,6)]),functionName:''};
  const parent={...fn(0,10),functionName:''};
  const child={...fn(2,8,[range(4,6,0)]),functionName:''};
  assert.ok(retainedExecutableCoverage([before],[parent,child],text,text,map).failures.some(x=>x.includes('branch')));
});

test('separate baseline graphs cannot hide a covered nested branch from each other', () => {
  const text='0123456789\n', map=new Map([[1,1]]);
  const first=fn(0,10,[range(2,8),range(4,6,0)]), second=fn(0,10,[range(2,8),range(4,6)]);
  assert.ok(retainedExecutableCoverage([first,second],[first],text,text,map).failures.some(x=>x.includes('branch')));
});

test('an explicitly mapped extraction must retain its own effective branch coverage', () => {
  const old='function original(){\n branch();\n}\n', next='function extracted(){\n branch();\n}\n';
  const prior=old.indexOf('branch'), moved=next.indexOf('branch');
  const before={...fn(0,old.length-1,[range(prior,prior+9)]),functionName:'original'};
  const after={...fn(0,next.length-1,[range(moved,moved+9)]),functionName:'extracted'};
  const map=new Map([[2,2],[3,3]]), extraction=[{from:'original',to:'extracted'}];
  assert.ok(retainedExecutableCoverage([before],[after],old,next,map).failures.some(x=>x.includes('branch')));
  const retained=retainedExecutableCoverage([before],[after],old,next,map,extraction);
  assert.deepEqual(retained.failures,[]);assert.deepEqual(retained.proof.find(p=>p.kind==='branch').extraction,extraction[0]);
  assert.ok(retainedExecutableCoverage([before],[after],old,next,map,[{from:'other',to:'extracted'}]).failures.some(x=>x.includes('branch')));
  assert.ok(retainedExecutableCoverage([before],[after],old,next,map,[{from:'original',to:'other'}]).failures.some(x=>x.includes('branch')));
  after.ranges.push(range(moved+1,moved+2,0));
  assert.ok(retainedExecutableCoverage([before],[after],old,next,map,extraction).failures.some(x=>x.includes('branch')));
});

test('accepts a merged covered V8 branch but rejects an uncovered nested segment', () => {
  const oldBranch=range(oldText.indexOf('else'),oldText.indexOf('no();')+5);
  const before=fn(0,endA-1,[oldBranch]);
  const moved=shift(oldBranch), merged=fn(9,endA+8,[range(moved.startOffset-2,moved.endOffset+1)]);
  assert.deepEqual(retainedExecutableCoverage([before],[merged],oldText,newText,mapping).failures,[]);
  merged.ranges.push(range(moved.startOffset+1,moved.endOffset-1,0));
  assert.ok(retainedExecutableCoverage([before],[merged],oldText,newText,mapping).failures.some(x=>x.includes('branch')));
});

test('rejects an unchanged function losing coverage or disappearing', () => {
  const lost = shifted(b); lost.ranges[0].count = 0;
  const result = retainedExecutableCoverage([b], [lost], oldText, newText, mapping);
  assert.ok(result.failures.some(f => /function/.test(f)));
  assert.ok(retainedExecutableCoverage([b], [], oldText, newText, mapping).failures.some(f => /function/.test(f)));
});

test('checks surviving branches inside edited functions and exempts only edited text', () => {
  const edited = oldText.replace('yes()', 'different()');
  const oldStart = oldText.indexOf('else'), oldEnd = oldText.indexOf('no();') + 5;
  const newStart = edited.indexOf('else'), newEnd = edited.indexOf('no();') + 5;
  const partial = new Map([[1, 1], [3, 3], [4, 4], [5, 5], [6, 6]]);
  const before = fn(0, endA - 1, [range(oldStart, oldEnd)]);
  const after = fn(0, edited.indexOf('function b') - 1, [range(newStart, newEnd, 0)]);
  const result = retainedExecutableCoverage([before], [after], oldText, edited, partial);
  assert.equal(result.failures.length, 1);
  assert.match(result.failures[0], /branch/);
  assert.equal(result.proof.filter(r => r.kind === 'function').length, 0, 'edited body is checked by the changed-range gate');
});

test('requires a mapped function boundary and block-level evidence for retained branches', () => {
  const old='function a(){\n  branch();\n}\n', changed='function altered(){\n  branch();\n}\n';
  const start=old.indexOf('branch'), end=start+'branch();'.length;
  const before=fn(0,old.length-1,[range(start,end)]);
  const after=fn(0,changed.length-1,[range(changed.indexOf('branch'),changed.indexOf('branch')+end-start)]);
  assert.ok(retainedExecutableCoverage([before],[after],old,changed,new Map([[2,2],[3,3]])).failures.some(x=>x.includes('branch')));
  const unsupported=fn(0,old.length-1,[],1,false);
  assert.ok(retainedExecutableCoverage([before],[unsupported],old,old,new Map([[1,1],[2,2],[3,3]])).failures.some(x=>x.includes('branch')));
});

test('retains an identical function whose surrounding line changed', () => {
  const old='const x = () => {\n branch();\n};\n', next='const y = () => {\n branch();\n};\n';
  const root=old.indexOf('() =>'), end=old.indexOf('};')+1, branch=old.indexOf('branch');
  const before={...fn(root,end,[range(branch,branch+9)]),functionName:''};
  const after={...fn(root,end,[range(branch,branch+9)]),functionName:''};
  assert.deepEqual(retainedExecutableCoverage([before],[after],old,next,new Map([[2,2],[3,3]])).failures,[]);
});

test('an unchanged function remains identifiable when a diff aligns its closing brace with a later function', () => {
  const old = 'function x(){\n branch();\n}\n';
  const next = 'function x(){\n branch();\n}\nfunction y(){\n other();\n}\n';
  const branch = old.indexOf('branch'), end = old.indexOf('}') + 1;
  const before = {...fn(0, end, [range(branch, branch + 9)]), functionName:'x'};
  const after = {...fn(0, end, [range(branch, branch + 9)]), functionName:'x'};
  const mapping = new Map([[1,1],[2,2],[3,6]]);
  assert.deepEqual(retainedExecutableCoverage([before],[after],old,next,mapping).failures,[]);
  after.ranges.push(range(branch + 1, branch + 2, 0));
  assert.ok(retainedExecutableCoverage([before],[after],old,next,mapping).failures.some(x => x.includes('branch')));
});

test('an edited named function requires an explicit extraction when its end is misaligned', () => {
  const old='function x(){\n branch();\n}\n';
  const next='function x(){\n branch();\n extra();\n}\nfunction y(){\n other();\n}\n';
  const branch=old.indexOf('branch'), end=old.indexOf('}')+1;
  const before={...fn(0,end,[range(branch,branch+9)]),functionName:'x'};
  const after={...fn(0,next.indexOf('}')+1,[range(branch,branch+9)]),functionName:'x'};
  const map=new Map([[1,1],[2,2],[3,7]]);
  assert.ok(retainedExecutableCoverage([before],[after],old,next,map).failures.length);
  const extraction=[{from:'x',to:'x'}];
  assert.deepEqual(retainedExecutableCoverage([before],[after],old,next,map,extraction).failures,[]);
  after.ranges.push(range(branch+1,branch+2,0));
  assert.ok(retainedExecutableCoverage([before],[after],old,next,map,extraction).failures.some(x=>x.includes('branch')));
  assert.ok(retainedExecutableCoverage([before],[{...after,functionName:'other'}],old,next,map).failures.some(x=>x.includes('branch')));
});

test('a named function with an appended parameter requires explicit extraction', () => {
  const old='function x(a){\n branch();\n}\n';
  const next='function x(a, b){\n branch();\n}\n';
  const before={...fn(0,old.indexOf('}')+1,[range(old.indexOf('branch'),old.indexOf('branch')+9)]),functionName:'x'};
  const after={...fn(0,next.indexOf('}')+1,[range(next.indexOf('branch'),next.indexOf('branch')+9)]),functionName:'x'};
  const map=new Map([[2,2],[3,3]]);
  assert.ok(retainedExecutableCoverage([before],[after],old,next,map).failures.length);
  const extraction=[{from:'x',to:'x'}];
  assert.deepEqual(retainedExecutableCoverage([before],[after],old,next,map,extraction).failures,[]);
  after.ranges.push(range(next.indexOf('branch')+1,next.indexOf('branch')+2,0));
  assert.ok(retainedExecutableCoverage([before],[after],old,next,map,extraction).failures.some(x=>x.includes('branch')));
  const unrelated='function x(q, b){\n branch();\n}\n';
  assert.ok(retainedExecutableCoverage([before],[after],old,unrelated,map).failures.some(x=>x.includes('branch')));
});

test('a named edited function requires an explicit extraction', () => {
  const old='export function x(){\n branch();\n}\n', next='function x(){\n branch();\n extra();\n}\n';
  const start=old.indexOf('function'), branch=old.indexOf('branch'), moved=next.indexOf('branch');
  const before={...fn(start,old.length-1,[range(branch,branch+9)]),functionName:'x'};
  const after={...fn(0,next.length-1,[range(moved,moved+9)]),functionName:'x'};
  const mapping=new Map([[2,2],[3,4]]);
  assert.ok(retainedExecutableCoverage([before],[after],old,next,mapping).failures.length);
  assert.deepEqual(retainedExecutableCoverage([before],[after],old,next,mapping,[{from:'x',to:'x'}]).failures,[]);
  const wrong={...after,functionName:'other'};
  assert.ok(retainedExecutableCoverage([before],[wrong],old,next,mapping).failures.some(x=>x.includes('branch')));
  const anonymous={...before,functionName:''};
  assert.ok(retainedExecutableCoverage([anonymous],[{...after,functionName:''}],old,next,mapping).failures.some(x=>x.includes('branch')));
});

test('uncovered baseline paths need no retention, function-only records remain functions, and graphs combine', () => {
  const uncovered = fn(0, endA - 1, [], 0);
  const functionOnly = fn(endA, oldText.length - 1, [], 1, false);
  const after = shifted(functionOnly);
  const absent = { ...after, ranges: after.ranges.map(r => ({ ...r, count: 0 })) };
  const result = retainedExecutableCoverage([uncovered, functionOnly, functionOnly], [absent, after], oldText, newText, mapping);
  assert.deepEqual(result.failures, []);
  assert.deepEqual(result.proof.map(r => r.kind), ['function']);
});

test('does not map across insertions within ranges and rejects inaccurate source mappings', () => {
  const inserted = oldText.replace('  else', '// inserted\n  else');
  const insertedMap = new Map([[1, 1], [2, 2], [3, 4], [4, 5], [5, 6], [6, 7]]);
  const result = retainedExecutableCoverage([fn(0, endA - 1, [], 1, false)], [], oldText, inserted, insertedMap);
  assert.deepEqual(result.failures, []);
  assert.deepEqual(result.proof, []);
  assert.throws(() => retainedExecutableCoverage([], [], oldText, newText, new Map([[1, 1]])), /unchanged source/);
});

test('maps UTF-16 offsets and end-exclusive boundaries, including a range at EOF', () => {
  const old = 'const symbol = "🛰";\nfunction retained() {}';
  const next = '// prefix\n' + old;
  const start = old.indexOf('function'), before = fn(start, old.length, [], 1, false);
  const after = fn(start + 10, next.length, [], 1, false);
  const result = retainedExecutableCoverage([before], [after], old, next, new Map([[1, 2], [2, 3]]));
  assert.deepEqual(result.failures, []);
  assert.equal(result.proof.length, 1);
});

test('the verifier applies executable retention to every production file before checking changed ranges', () => {
  const src = readFileSync(new URL('./menu-coverage.mjs', import.meta.url), 'utf8');
  const call = src.indexOf('retainedExecutableCoverage(\n');
  assert.ok(call > src.indexOf('for (const path of new Set('));
  assert.ok(call < src.indexOf('if (!changed.size)'));
});
