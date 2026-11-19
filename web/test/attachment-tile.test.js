import test from "node:test";
import assert from "node:assert/strict";
import {attachmentTileHTML} from "../js/attachment-tile.js";

test("common attachment tile requires the asset transport for preview and download",()=>{
 const escape=String,paths=[];const assetURL=path=>{paths.push(path);return "blob:resolved"};
 assert.throws(()=>attachmentTileHTML({name:"x",url:"/api/nodes/n/attachments/x",escape}),/asset transport/i);
 const html=attachmentTileHTML({name:"x",url:"/download",previewURL:"/preview",image:true,escape,assetURL});
 assert.deepEqual(paths,["/preview"]);assert.match(html,/href="blob:resolved"/);assert.match(html,/src="blob:resolved"/);assert.doesNotMatch(html,/href="\/preview"/);
});

test('generic downloads and unavailable files use the common resolved transport',()=>{
 const escape=String,paths=[];const html=attachmentTileHTML({name:'note.txt',url:'/associated',escape,assetURL:p=>{paths.push(p);return 'blob:managed'}});
 assert.deepEqual(paths,['/associated']);assert.match(html,/download="note.txt"/);assert.match(html,/href="blob:managed"/);
 assert.match(attachmentTileHTML({name:'lost',url:'/missing',escape,assetURL:()=>''}),/unavailable/);
});

test('refused preview retains a named download without changing ordinary broken thumbnails',async()=>{
 const {replaceFailedAttachmentPreview}=await import('../js/attachment-tile.js');let replacement,removed=0;
 const link={dataset:{name:'synthetic.png',fallbackUrl:'blob:safe'},replaceWith:value=>{replacement=value},remove:()=>removed++};
 const document={createElement:()=>({})};assert.equal(replaceFailedAttachmentPreview(link,document),true);
 assert.equal(replacement.href,'blob:safe');assert.equal(replacement.download,'synthetic.png');assert.match(replacement.textContent,/synthetic\.png.*preview unavailable/i);assert.equal(replacement.className,'attfile');assert.equal(removed,0);
 const ordinary={dataset:{name:'ordinary'},remove:()=>removed++};assert.equal(replaceFailedAttachmentPreview(ordinary,document),false);assert.equal(removed,1);
});

test('shared image tile carries only an explicitly resolved fallback download',()=>{
 const escape=String,assetURL=path=>'blob:'+path;
 const html=attachmentTileHTML({name:'image.png',url:'/file',previewURL:'/preview',image:true,escape,assetURL,fallbackURL:'blob:download'});
 assert.match(html,/data-fallback-url="blob:download"/);
});

test('unavailable attachment keeps a separately actionable retry and an inert status',async()=>{
 const {attachmentUnavailableHTML}=await import('../js/attachment-tile.js');const html=attachmentUnavailableHTML({name:'synthetic.txt',download:'blob:retry',escape:String});
 assert.match(html,/<span class="attfile missing"[^>]*>synthetic\.txt \(not retained\)<\/span>/);assert.match(html,/<a class="attfile" href="blob:retry"[^>]*>Retry download<\/a>/);
 assert.doesNotMatch(attachmentUnavailableHTML({name:'synthetic.txt',download:'',escape:String}),/<a /);
});

test('remote retry uses the scoped asset adapter and never updates a retired view',async()=>{
 const {retryAttachmentDownload}=await import('../js/attachment-tile.js');let replacement,clicked=0;
 const button={dataset:{attachmentRetry:'/api/team/subscriptions/s/o/c/g/attachments/1/id',name:'saved.txt'},isConnected:true,replaceWith:value=>{replacement=value}};
 const document={createElement:()=>({click(){clicked++}})};
 assert.equal(await retryAttachmentDownload({button,document,current:()=>true,retryAsset:async()=> 'blob:verified'}),true);
 assert.equal(replacement.href,'blob:verified');assert.equal(replacement.download,'saved.txt');assert.equal(clicked,1);
 let release;const stale={...button,replaceWith:()=>{throw Error('retired view changed')}};const pending=retryAttachmentDownload({button:stale,document,current:()=>false,retryAsset:()=>new Promise(resolve=>{release=resolve})});release('blob:old');assert.equal(await pending,false);
});

test('retry leaves unavailable files retryable and avoids stale or invalid controls',async()=>{
 const {retryAttachmentDownload}=await import('../js/attachment-tile.js');const document={createElement:()=>({click(){}})};
 for(const button of [null,{dataset:{}},{dataset:{attachmentRetry:'/x'}},{dataset:{name:'x'}}]) assert.equal(await retryAttachmentDownload({button,document,current:()=>true,retryAsset:async()=>''}),false);
 const make=()=>({dataset:{attachmentRetry:'/x',name:'x'},isConnected:true,replaceWith(){throw Error('no replacement')}});
 for(const retryAsset of [async()=>'',async()=>{throw Error('offline')}]) {const button=make();assert.equal(await retryAttachmentDownload({button,document,current:()=>true,retryAsset}),false);assert.equal(button.disabled,false);assert.match(button.textContent,/unavailable/i)}
 const detached=make();detached.isConnected=false;assert.equal(await retryAttachmentDownload({button:detached,document,current:()=>true,retryAsset:async()=> 'blob:x'}),false);
 assert.equal(await retryAttachmentDownload({button:make(),document:null,current:()=>true,retryAsset:async()=>''}),false);
 assert.equal(await retryAttachmentDownload({button:make(),document,current:()=>true,retryAsset:null}),false);
});

test('file-only generic tiles identify an unavailable preview and escape its text',()=>{
 const html=attachmentTileHTML({name:'x',url:'/x',escape:s=>s.replace(/</g,'&lt;'),assetURL:()=> 'blob:x',note:'<unavailable>'});assert.match(html,/&lt;unavailable>/);
});

test('remote uncached file exposes a separate keyboard-operable retry button',async()=>{
 const {attachmentUnavailableHTML}=await import('../js/attachment-tile.js');const html=attachmentUnavailableHTML({name:'x',download:'',retryPath:'/scoped/file',escape:String});
 assert.match(html,/<button type="button" class="attfile" data-attachment-retry="\/scoped\/file" data-name="x">Retry download<\/button>/);
});
