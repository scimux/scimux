/* Boot-twice retirement for the composition root (web/js/app.js).
 *
 * A phone that sleeps loses the peer connection; rv re-dials on wake and runs
 * the FR-40 handover again, which re-mints every module as a fresh blob: URL,
 * installs a new head/body and calls createApp() a second time. The document's
 * *elements* are discarded with their listeners, but the document and window
 * objects are the same, so everything registered on them — and every timer —
 * outlives the swap. Nothing retired the instance that came before, so both
 * polled, both wrote the status bar, and because the retired one holds the dead
 * transport (FR-41: once `cause` is set, every request is refused forever) it
 * painted "server unreachable" every 2s over a connection the live instance was
 * demonstrably using. Each sleep/wake added another.
 *
 * These are behavioural tests, not source-text ratchets: they run the real
 * createApp against the fake host and read the listener/timer ledger.
 */
import test from "node:test";
import assert from "node:assert/strict";
import { installHost, identityAssetURL, jsonResponse } from "./s2-helpers.js";
import { APP_SLOT } from "../js/lifecycle.js";

const flush = async (n = 30) => { for (let i = 0; i < n; i++) await Promise.resolve(); };

/* Records listeners on the targets that survive a document swap, and every
   timer, without ever firing one. Element listeners are deliberately not
   tracked: those nodes are thrown away by the handover. */
function instrument(host) {
  const listeners = [];
  const timers = [];

  const track = (where, obj) => {
    obj.addEventListener = (type, fn, opts) => {
      listeners.push({ where, type, fn, opts, live: true });
    };
    obj.removeEventListener = (type, fn) => {
      for (const l of listeners) {
        if (l.live && l.where === where && l.type === type && l.fn === fn) {
          l.live = false;
          return;
        }
      }
    };
  };
  track("document", host.document);
  track("window", host.window);
  if (host.window.visualViewport) track("visualViewport", host.window.visualViewport);

  /* app.js reads matchMedia off the global; insets.js off the injected window. */
  const matchMedia = q => {
    const mql = {
      matches: /min-width:\s*900/.test(q),
      media: q,
      addListener() {},
      removeListener() {},
    };
    track("matchMedia " + q, mql);
    return mql;
  };
  host.window.matchMedia = matchMedia;
  globalThis.matchMedia = matchMedia;

  let nextId = 0;
  const clock = (kind) => (fn, ms) => {
    const id = ++nextId;
    timers.push({ kind, id, ms, fn, live: true });
    return id;
  };
  const stop = id => {
    const t = timers.find(t => t.id === id);
    if (t) t.live = false;
  };
  host.window.setTimeout = clock("timeout");
  host.window.setInterval = clock("interval");
  host.window.clearTimeout = stop;
  host.window.clearInterval = stop;

  return {
    listeners,
    timers,
    liveListeners: () => listeners.filter(l => l.live),
    liveTimers: () => timers.filter(t => t.live),
  };
}

async function bootTwice(t, { secondGraph = false } = {}) {
  const host = installHost({});
  const rec = instrument(host);
  const deps = () => ({
    fetchImpl: host.trapFetch,
    assetURL: identityAssetURL,
    document: host.document,
    window: host.window,
  });

  const first = await (await import("../js/app.js")).createApp(deps());
  await flush();
  const firstListeners = rec.liveListeners();
  const firstTimers = rec.liveTimers();

  const successor = secondGraph
    ? await import("../js/app.js?graph=successor")
    : await import("../js/app.js");
  const second = await successor.createApp(deps());
  await flush();

  t.after(() => {
    try { host.window[APP_SLOT]?.retire?.(); } catch { /* best effort */ }
  });
  return { host, rec, first, second, firstListeners, firstTimers };
}

test('app graphs reject missing transport and host dependencies before boot', async () => {
  for (const path of ['../js/app.js','../js/app.js?graph=successor']) {
    const {createApp}=await import(path);
    const valid={fetchImpl:async()=>{},assetURL:x=>x,document:{},window:{}};
    for (const key of ['fetchImpl','assetURL','document','window']) {
      const input={...valid,[key]:undefined};
      await assert.rejects(createApp(input),new RegExp(key+' is required'));
    }
  }
});

test("app: booting twice in one window leaves no listener from the first instance", async (t) => {
  const { rec, firstListeners } = await bootTwice(t);
  assert.ok(firstListeners.length > 0,
    "the harness must actually observe the shell's document/window listeners");
  assert.deepEqual(
    firstListeners.filter(l => l.live).map(l => `${l.where} ${l.type}`),
    [],
    "a surviving visibilitychange listener is what restarted the retired instance's poll loop");
  assert.ok(rec.liveListeners().length > 0, "the second instance must still be wired");
});

test("app: booting twice in one window stops every timer the first instance started", async (t) => {
  const { firstTimers } = await bootTwice(t);
  assert.ok(firstTimers.length > 0, "the harness must actually observe the shell's cadences");
  assert.deepEqual(firstTimers.filter(t => t.live).map(t => `${t.kind} ${t.ms}`), [],
    "the 8s status phase and 30s usage cadences would otherwise double on every wake");
});

test("app: the second boot's listener count matches the first, not double it", async (t) => {
  const { rec, firstListeners } = await bootTwice(t);
  assert.equal(rec.liveListeners().length, firstListeners.length,
    "two boots must leave exactly one instance's worth of surviving listeners");
});

test("app: a handover's fresh module graph still retires the predecessor", async (t) => {
  /* The production condition: the successor's app.js is a different module
     object, so nothing but the window connects the two instances. */
  const { rec, firstListeners, firstTimers } = await bootTwice(t, { secondGraph: true });
  assert.deepEqual(firstListeners.filter(l => l.live).map(l => `${l.where} ${l.type}`), []);
  assert.deepEqual(firstTimers.filter(t => t.live).map(t => `${t.kind} ${t.ms}`), []);
  assert.equal(rec.liveListeners().length, firstListeners.length);
});

test("app: createApp claims the window slot and returns a retire handle", async (t) => {
  const { host, second } = await bootTwice(t);
  assert.equal(typeof second?.retire, "function",
    "the handle is how a successor — or a caller that owns the page — retires an instance");
  assert.equal(host.window[APP_SLOT], second,
    "the slot lives on the window because that is the only identity a handover preserves");
});

test("app: retire() leaves nothing registered behind", async (t) => {
  const host = installHost({});
  const rec = instrument(host);
  const { createApp } = await import("../js/app.js");
  const app = await createApp({
    fetchImpl: host.trapFetch,
    assetURL: identityAssetURL,
    document: host.document,
    window: host.window,
  });
  await flush();
  assert.ok(rec.liveListeners().length > 0);
  app.retire();
  await flush();
  assert.deepEqual(rec.liveListeners().map(l => `${l.where} ${l.type}`), []);
  assert.deepEqual(rec.liveTimers().map(x => `${x.kind} ${x.ms}`), []);
  app.retire();  /* idempotent */
});

test("app: legacy media resolution runs the composed bookmark and Notes refresh callback", async () => {
  const host = installHost({});
  const calls = [];
  let failCapture = false;
  let circularProv = false;
  const fetchImpl = async (path, opts = {}) => {
    calls.push(String(path));
    if (String(path).startsWith("/api/ui")) return jsonResponse({lanes:[],groups:[],archived:[],pinned:[],bookmarks:[{
      t:"T",uid:"u",segment:0,record:0,text:"![one](scimux-asset:a_1)",
    }]});
    if (String(path).startsWith("/api/preview")) return jsonResponse({uid:"u",node:"n1",anchor:0,turns:[{uid:"u",segment:0,record:0}],assets:{a_1:{name:"one.png"}}});
    if (String(path).startsWith("/api/state")) return jsonResponse({nodes:[{id:"n1",title:"One",agent:"codex",created_at:"T0"}],unadopted:[],hostname:"computer",version:"test",sys:{}});
    if (String(path).startsWith("/api/nodes/n1/chat")) {
      const prov = circularProv ? {} : {source:"synthetic"}; if (circularProv) prov.self = prov;
      return jsonResponse({turns:[{role:"assistant",agent:"codex",prov,text:"![one](scimux-asset:a_1)",time:"T",uid:"u",segment:0,record:0}],segments:[],assets:{a_1:{name:"one.png"}}});
    }
    if (String(path) === "/api/reference-media") {
      if (failCapture) throw new Error("budget");
      return jsonResponse({text:"![one](scimux-asset:a_1)",media:{version:1,capture_id:"a".repeat(64),items:[{id:"0",key:"asset:a_1",name:"one.png",state:"ready"}]}});
    }
    return host.trapFetch(path, opts);
  };
  const { createApp } = await import("../js/app.js");
  const app = await createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
  await flush();
  host.byId.get("bookmarksbtn").dispatchEvent({type:"click",preventDefault(){},stopPropagation(){}});
  await new Promise(resolve => setTimeout(resolve, 5));
  assert.ok(calls.some(x => x.startsWith("/api/preview")), calls.join(","));
  const station = {dataset:{forkbookmark:"n1"},closest:sel=>sel === "[data-forkbookmark]" ? station : null};
  host.byId.get("maptoolbar").dispatchEvent({type:"click",target:station,preventDefault(){},stopPropagation(){}});
  await flush();
  assert.ok(calls.includes("/api/reference-media"), calls.join(","));
  failCapture = true; circularProv = true;
  host.byId.get("maptoolbar").dispatchEvent({type:"click",target:station,preventDefault(){},stopPropagation(){}});
  await flush();
  app.retire();
});

test("app restores the selected chat's sharing header without switching chats", async () => {
  const host = installHost({});
  host.window.localStorage.setItem("scimux-sel", "n1");
  const calls = [];
  const fetchImpl = async (path, opts) => {
    calls.push(String(path));
    if (String(path).startsWith("/api/state")) return jsonResponse({nodes:[{id:"n1",title:"One",agent:"codex",created_at:"T0"}],unadopted:[],hostname:"computer",version:"test",sys:{}});
    if (String(path) === "/api/team/servers") return jsonResponse({servers:[{id:"server",name:"Team"}]});
    if (String(path) === "/api/team/shares/n1") return jsonResponse({state:"active",server:"server",members:[],delivery:"ok"});
    return host.trapFetch(path, opts);
  };
  const { createApp } = await import("../js/app.js?graph=successor");
  const app = await createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
  await flush(100); await new Promise(resolve => setTimeout(resolve, 10));
  assert.ok(calls.includes("/api/team/shares/n1"), calls.join(","));
  assert.equal(host.byId.get("sharebtn").hidden, false);
  app.retire();
});

test("app refreshes the current share and subscriptions after join", async () => {
  const host = installHost({}); host.window.localStorage.setItem("scimux-sel", "n1");
  const calls = [];
  const fetchImpl = async (path, opts = {}) => {
    path = String(path); calls.push({path, method:opts.method || "GET"});
    if (path.startsWith("/api/state")) return jsonResponse({nodes:[{id:"n1",title:"One",agent:"codex",created_at:"T0"}],unadopted:[],hostname:"computer",version:"test",sys:{}});
    if (path === "/api/settings") return jsonResponse({});
    if (path === "/api/team/servers" && opts.method === "POST") return jsonResponse({id:"server",name:"Team"});
    if (path === "/api/team/servers") return jsonResponse({servers:[{id:"server",name:"Team"}]});
    if (path === "/api/team/shares/n1") return jsonResponse({state:"active",server:"server",members:[],delivery:"ok"});
    if (path === "/api/team/subscriptions") return jsonResponse({subscriptions:[],servers:[],truncated:false});
    return host.trapFetch(path,opts);
  };
  const app = await (await import("../js/app.js?graph=successor")).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
  await flush(100); await new Promise(resolve=>setTimeout(resolve,10));
  let before = calls.filter(c=>c.path === "/api/team/shares/n1").length;
  let subs = calls.filter(c=>c.path === "/api/team/subscriptions").length;
  host.byId.get("m_team_link").value = "http://127.0.0.1/join#synthetic";
  host.byId.get("m_team_name").value = "Synthetic";
  await host.byId.get("m_team_join").click(); await flush(100); await new Promise(resolve=>setTimeout(resolve,10));
  assert.ok(calls.filter(c=>c.path === "/api/team/shares/n1").length > before);
  assert.ok(calls.filter(c=>c.path === "/api/team/subscriptions").length > subs);
  app.retire();
});

test("settings team key does not trigger sharing refresh", async () => {
  const host = installHost({}); host.window.localStorage.setItem("scimux-sel", "n1");
  let enabled = false; const calls = [];
  const fetchImpl = async (path, opts = {}) => {
    path = String(path); calls.push(path);
    if (path.startsWith("/api/state")) return jsonResponse({nodes:[{id:"n1",title:"One",agent:"codex",created_at:"T0"}],unadopted:[],hostname:"computer",version:"test",sys:{}});
    if (path === "/api/settings") return jsonResponse({experimental_feature_team:enabled});
    if (path === "/api/team/servers") return jsonResponse({servers:[{id:"server",name:"Team"}]});
    if (path === "/api/team/shares/n1") return jsonResponse({state:"active",server:"server",members:[],delivery:"ok"});
    if (path === "/api/team/subscriptions") return jsonResponse({subscriptions:[],servers:[],truncated:false});
    return host.trapFetch(path, opts);
  };
  const app = await (await import("../js/app.js?graph=successor")).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
  await flush(100); await new Promise(resolve=>setTimeout(resolve,10));
  const before = calls.filter(c=>c === "/api/team/shares/n1" || c === "/api/team/subscriptions").length;
  enabled = true; await host.byId.get("burger").click(); await flush(100); await new Promise(resolve=>setTimeout(resolve,10));
  assert.equal(calls.filter(c=>c === "/api/team/shares/n1" || c === "/api/team/subscriptions").length, before);
  app.retire();
});

test("app restores a received selection without substituting an owned chat", async () => {
  const host = installHost({});
  for (const id of ["sharedview", "sv_text", "sv_send"]) assert.equal(host.byId.has(id), false, `obsolete ${id} must be absent`);
  const received = "team/s/o/c/g";
  host.window.localStorage.setItem("scimux-sel", received);
  const calls = []; const posts = [];
  const msgs = host.byId.get("msgs");
  msgs.insertAdjacentHTML = (_where, html) => { msgs.innerHTML += html; };
  const fetchImpl = async (path, opts) => {
    calls.push(String(path));
    if (opts?.method === "POST") posts.push({path:String(path),body:opts.body});
    if (String(path).endsWith("/comments") && opts?.method === "POST") return jsonResponse({seq:1});
    if (String(path).startsWith("/api/state")) return jsonResponse({nodes:[{id:"n1",title:"Local",agent:"codex",created_at:"T0"}],unadopted:[],hostname:"computer",version:"test",sys:{}});
    if (String(path) === "/api/team/subscriptions") return jsonResponse({subscriptions:[{server:"s",owner:"o",chat:"c",generation:"g",access:"write",state:"live",title:"Received",server_name:"Team",owner_name:"Owner"}],servers:[],truncated:false});
    if (String(path).includes("/comments?")) return jsonResponse({entries:[{seq:1,anchor:1,text:"A reply",author:"p",at:"T"}],names:{p:"Person"},next:1,more:false});
    if (String(path).includes("/c/g?")) return jsonResponse({view:{server:"s",owner:"o",chat:"c",generation:"g",access:"write",state:"live",title:"Received",server_name:"Team",owner_name:"Owner"},entries:[{t:"assistant",seq:1,text:"An answer"}],next:1,more:false});
    return host.trapFetch(path, opts);
  };
  const app = await (await import("../js/app.js?graph=successor")).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
  try {
    await flush(100); await new Promise(resolve => setTimeout(resolve, 10));
    assert.equal(host.window.localStorage.getItem("scimux-sel"), received);
    assert.ok(calls.some(path => path.startsWith("/api/team/subscriptions/s/o/c/g?")), calls.join(","));
    assert.equal(host.byId.has("sharedview"), false);
    assert.match(msgs.innerHTML, /An answer/);
    assert.match(msgs.innerHTML, /A reply/);
    assert.equal(host.byId.get("chattitle").textContent, "Received");
    assert.equal(host.byId.get("prompt").getAttribute("contenteditable"), "true");
    const prompt = host.byId.get("prompt"); prompt.innerText="Hello people"; prompt.dispatchEvent({type:"input",target:prompt});
    await host.byId.get("sendbtn").click(); await flush(30);
    assert.equal(posts.filter(p => p.path.endsWith("/comments")).length,1);
    assert.equal(posts.filter(p => p.path.includes("/api/nodes/")).length,0);
    assert.ok(!calls.some(path => path.startsWith("/api/nodes/team")));
  } finally { app.retire(); }
});

test("received card opens the normal panel and survives a fresh app graph", async () => {
  const host = installHost({});
  assert.equal(host.byId.has("sharedview"), false);
  const calls = [];
  const view = {server:"s",owner:"o",chat:"c",generation:"g",access:"read",state:"live",title:"Received",server_name:"Team",owner_name:"Owner",first_seen:"1",seen_mirror:0,seen_comments:0};
  const fetchImpl = async (path, opts) => {
    calls.push({path:String(path),method:opts?.method || "GET"});
    if (String(path).startsWith("/api/state")) return jsonResponse({nodes:[{id:"n1",title:"Local",agent:"codex",created_at:"T0"}],unadopted:[],hostname:"computer",version:"test",sys:{}});
    if (String(path) === "/api/team/subscriptions") return jsonResponse({subscriptions:[view],servers:[],truncated:false});
    if (String(path).includes("/comments?")) return jsonResponse({entries:[],names:{},next:0,more:false});
    if (String(path).includes("/c/g?")) return jsonResponse({view,entries:[],next:0,more:false});
    return host.trapFetch(path,opts);
  };
  let app = await (await import("../js/app.js")).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
  try {
    await flush(100); await new Promise(resolve => setTimeout(resolve, 10));
    const card = {dataset:{open:"team/s/o/c/g"},closest:selector => selector === "[data-open]" ? card : null};
    host.byId.get("cardlist").dispatchEvent({type:"click",target:card,preventDefault(){},stopPropagation(){}});
    await flush(100);
    assert.equal(host.window.localStorage.getItem("scimux-sel"), "team/s/o/c/g");
    assert.equal(host.byId.get("chattitle").textContent,"Received");
    assert.equal(host.byId.has("sharedview"),false);
    const key = {dataset:{key:"y"},closest:selector => selector === "[data-key]" ? key : null};
    host.byId.get("keyrow").dispatchEvent({type:"click",target:key,preventDefault(){},stopPropagation(){}});
    await host.byId.get("autoapprove").click();
    await host.byId.get("infobtn").click();
    await host.byId.get("termtoggle").click();
    await host.byId.get("sendbtn").click();
    assert.equal(host.byId.get("chattitle").textContent,"Received");
    assert.equal(host.byId.get("prompt").getAttribute("contenteditable"),"false");
    await flush(20);
    assert.equal(calls.filter(c => c.path.startsWith("/api/nodes/team") && c.method !== "GET").length,0);
    app.retire();
    app = await (await import("../js/app.js?graph=successor")).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
    await flush(100); await new Promise(resolve => setTimeout(resolve, 10));
    assert.equal(host.window.localStorage.getItem("scimux-sel"), "team/s/o/c/g");
    assert.equal(host.byId.get("chattitle").textContent,"Received");
    assert.equal(host.byId.has("sharedview"),false);
  } finally { app.retire(); }
});

test("late subscription metadata cannot replace a newer local selection", async () => {
  const host = installHost({});
  host.window.localStorage.setItem("scimux-sel", "team/s/o/c/g");
  let resolveList;
  const fetchImpl = async (path, opts) => {
    if (String(path).startsWith("/api/state")) return jsonResponse({nodes:[{id:"n1",title:"Local",agent:"codex",created_at:"T0"}],unadopted:[],hostname:"computer",version:"test",sys:{}});
    if (String(path) === "/api/team/subscriptions") return new Promise(resolve => { resolveList = resolve; });
    return host.trapFetch(path,opts);
  };
  const app = await (await import("../js/app.js")).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
  try {
    await flush(100);
    assert.equal(typeof resolveList,"function");
    const card = {dataset:{open:"n1"},closest:selector => selector === "[data-open]" ? card : null};
    host.byId.get("cardlist").dispatchEvent({type:"click",target:card,preventDefault(){},stopPropagation(){}});
    resolveList(jsonResponse({subscriptions:[{server:"s",owner:"o",chat:"c",generation:"g",access:"read",state:"live",title:"Received",server_name:"Team",owner_name:"Owner",first_seen:"1"}],servers:[],truncated:false}));
    await flush(100);
    assert.equal(host.window.localStorage.getItem("scimux-sel"), "n1");
    assert.equal(host.byId.get("chattitle").textContent,"Local");
  } finally { app.retire(); }
});

test("owner local chat retains its authoritative turn and shows an anchored human reply once", async () => {
  const host=installHost({}); host.window.localStorage.setItem("scimux-sel","n1"); host.window.localStorage.setItem("scimux-draft:n1","earlier agent draft");
  const promptHost=host.byId.get("prompt"); Object.defineProperty(promptHost,"innerText",{get(){return this.textContent},set(value){this.textContent=value},configurable:true});
  const view={server:"s",owner:"o",chat:"c",generation:"g",access:"owner",state:"live",title:"Chat",server_name:"Team",owner_name:"Owner",first_seen:"1",seen_comments:0,seen_mirror:0};
  const calls=[]; const posts=[];
  const fetchImpl=async(path,opts)=>{
    path=String(path); calls.push(path); if(opts?.method === "POST") posts.push({path, body:opts.body});
    if(path.endsWith("/comments") && opts?.method === "POST") return jsonResponse({seq:1});
    if(path.startsWith('/api/state')) return jsonResponse({nodes:[{id:'n1',title:'Local',agent:'codex',created_at:'T0'}],unadopted:[],hostname:'computer',version:'test',sys:{}});
    if(path==='/api/team/servers') return jsonResponse({servers:[{id:'s',name:'Team'}]});
    if(path==='/api/team/shares/n1') return jsonResponse({node:'n1',uid:'c',server:'s',generation:'g',state:'active',members:[]});
    if(path==='/api/team/subscriptions') return jsonResponse({subscriptions:[view],servers:[],truncated:false});
    if(path.includes('/comments?')) return jsonResponse({entries:[{seq:1,anchor:1,text:'Human reply',author:'p',at:'T'}],names:{p:'Person'},next:1,more:false});
    if(path.includes('/subscriptions/s/o/c/g?')) return jsonResponse({view,entries:[{seq:1,t:'assistant',text:'Local answer',local_segment:0,local_record:4}],next:1,more:false});
    if(path.startsWith('/api/nodes/n1/chat')) return jsonResponse({turns:[{role:'assistant',text:'Local answer',uid:'c',record:4}],decisions:[],source:'codex',live:'working',turn_in_flight:true});
    return host.trapFetch(path,opts);
  };
  const app=await (await import('../js/app.js?graph=successor')).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
  try { await flush(200); await new Promise(r=>setTimeout(r,20));
    const body=host.byId.get('msgs').innerHTML;
    assert.equal((body.match(/Local answer/g)||[]).length,1);
    assert.equal((body.match(/Human reply/g)||[]).length,1);
    assert.match(body,/Person/);
    assert.equal(host.byId.get('destination').hidden,false);
    assert.equal(host.byId.get('destinationpeople').getAttribute('aria-pressed'),'true');
    const prompt=host.byId.get('prompt'); prompt.innerText='Human message'; prompt.dispatchEvent({type:'input',target:prompt});
    await host.byId.get('sendbtn').click(); await flush(100);
    assert.equal(posts.filter(p=>p.path.endsWith('/comments')).length,1);
    assert.equal(posts.filter(p=>p.path.includes('/api/nodes/')).length,0);
    const turn={dataset:{cseq:'1',gkey:'s/o/c/g'},nextElementSibling:null,insertAdjacentHTML(_where,html){this.rowHTML=html;this.nextElementSibling={remove(){}}}};
    const bubble={closest:s=>s==='.bubble'?bubble:(s==='[data-cseq]'||s==='[data-seq], [data-cseq]')?turn:null};
    host.byId.get('msgs').dispatchEvent({type:'click',target:bubble});
    const action={dataset:{gact:'prompt'},closest:s=>s==='[data-gact]'?action:null};
    host.byId.get('msgs').dispatchEvent({type:'click',target:action});
    await flush(50); assert.match(prompt.textContent,/Human reply/); assert.match(prompt.textContent,/earlier agent draft/);
    assert.equal(posts.filter(p=>p.path.includes('/api/nodes/')).length,0);
    await host.byId.get('destinationagent').click();
    assert.match(prompt.textContent,/earlier agent draft/);
    assert.equal(host.byId.get('destinationagent').getAttribute('aria-pressed'),'true');
    await host.byId.get('destinationpeople').click();
    assert.equal(host.byId.get('destinationpeople').getAttribute('aria-pressed'),'true');
  } finally { app.retire(); }
});

for (const modulePath of ['../js/app.js','../js/app.js?graph=successor']) test('the app refuses a send when an older server cannot identify the chat instance: '+modulePath, async () => {
 const host=installHost({});host.window.localStorage.setItem('scimux-sel','n1');const posts=[];
 const fetchImpl=async(path,opts)=>{path=String(path);if(opts?.method==='POST')posts.push(path);
  if(path.startsWith('/api/state'))return jsonResponse({nodes:[{id:'n1',title:'Local',agent:'codex'}],unadopted:[],hostname:'synthetic',version:'old',sys:{}});
  if(path.startsWith('/api/nodes/n1/chat'))return jsonResponse({turns:[],live:'quiet',source:'codex'});
  return host.trapFetch(path,opts);
 };
 const app=await (await import(modulePath)).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
 try {await flush(200);const prompt=host.byId.get('prompt');prompt.innerText='synthetic';prompt.dispatchEvent({type:'input',target:prompt});await host.byId.get('sendbtn').click();await flush(30);assert.ok(!posts.some(path=>path.split('?')[0]==='/api/nodes/n1/send'));} finally {app.retire()}
});

for (const modulePath of ['../js/app.js','../js/app.js?graph=successor']) test('a same-slug replacement retires an in-flight chat response: '+modulePath, async () => {
 const host=installHost({});host.window.localStorage.setItem('scimux-sel','n1');const clock=instrument(host);let generation='old',release,reads=0;
 const held=new Promise(resolve=>{release=resolve});
 const fetchImpl=async(path,opts)=>{path=String(path);
  if(path.startsWith('/api/state'))return jsonResponse({nodes:[{id:'n1',instance_id:generation,title:'Local',agent:'codex'}],unadopted:[],hostname:'synthetic',version:'test',sys:{}});
  if(path.startsWith('/api/nodes/n1/chat')){if(++reads===1)return held;return jsonResponse({turns:[{role:'assistant',text:'replacement'}],live:'quiet',source:'codex'});}
  return host.trapFetch(path,opts);
 };
 const app=await (await import(modulePath)).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
 try {await flush(200);generation='new';const tick=clock.liveTimers().find(t=>t.kind==='timeout'&&t.ms===2000);assert.ok(tick);tick.fn();await flush(200);
  release(jsonResponse({turns:[{role:'assistant',text:'retired response'}],live:'working',source:'codex'}));await flush(200);
  assert.doesNotMatch(host.byId.get('msgs').innerHTML,/retired response/);
 } finally {app.retire()}
});

for (const modulePath of ['../js/app.js','../js/app.js?graph=successor']) test('a missing selected node followed by slug reuse does not revive its old draft: '+modulePath, async () => {
 const host=installHost({});host.window.localStorage.setItem('scimux-sel','n1');const clock=instrument(host);let generation='old';
 const prompt=host.byId.get('prompt');Object.defineProperty(prompt,'innerText',{get(){return this.textContent},set(value){this.textContent=value},configurable:true});
 const fetchImpl=async(path,opts)=>{path=String(path);
  if(path.startsWith('/api/state'))return jsonResponse({nodes:generation==='missing'?[]:[{id:'n1',instance_id:generation,title:'Local',agent:'codex'}],unadopted:[],hostname:'synthetic',version:'test',sys:{}});
  if(path.startsWith('/api/nodes/n1/chat'))return jsonResponse({turns:[],live:'quiet',source:'codex'});
  return host.trapFetch(path,opts);
 };
 const app=await (await import(modulePath)).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
 try {await flush(200);prompt.innerText='retired text';prompt.dispatchEvent({type:'input',target:prompt});generation='missing';
  clock.liveTimers().filter(t=>t.kind==='timeout'&&t.ms===2000).at(-1).fn();await flush(200);
  generation='new';clock.liveTimers().filter(t=>t.kind==='timeout'&&t.ms===2000).at(-1).fn();await flush(200);
  assert.doesNotMatch(prompt.innerText,/retired text/);
 } finally {app.retire()}
});

for (const modulePath of ['../js/app.js','../js/app.js?graph=successor']) test('the app wires shared fork to an explicit fresh-chat send: '+modulePath, async () => {
 const host=installHost({});host.window.localStorage.setItem('scimux-sel','n1');const copied=[];
 host.window.isSecureContext=true;host.window.navigator.clipboard={writeText:text=>{copied.push(text);return Promise.resolve()}};
 const view={server:'s',owner:'o',chat:'c',generation:'g',access:'owner',state:'live',local_node:'n1',title:'Chat',server_name:'Team',owner_name:'Owner',first_seen:'1',seen_comments:0,seen_mirror:0};
 const calls=[];const fetchImpl=async(path,opts)=>{path=String(path);calls.push(path);
  if(path==='/api/ui')return jsonResponse({lanes:[],groups:[],archived:[],pinned:[],bookmarks:[{t:'missing',text:'Saved',team:{server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:9}}]});
  if(path.startsWith('/api/state'))return jsonResponse({nodes:[{id:'n1',instance_id:'generation-1',title:'Local',agent:'codex',created_at:'T0'}],unadopted:[],hostname:'computer',version:'test',sys:{}});
  if(path==='/api/team/servers')return jsonResponse({servers:[{id:'s',name:'Team'}]});
  if(path==='/api/team/shares/n1')return jsonResponse({node:'n1',uid:'c',server:'s',generation:'g',state:'active',members:[]});
  if(path==='/api/team/subscriptions')return jsonResponse({subscriptions:[view],servers:[],truncated:false});
  if(path.includes('/comments?'))return jsonResponse({entries:[{seq:1,anchor:1,text:'Quoted reply',author:'p',at:'T'}],names:{p:'Person'},next:1,more:false});
  if(path.includes('/subscriptions/s/o/c/g?'))return jsonResponse({view,entries:[{seq:1,t:'assistant',text:'answer',local_segment:0,local_record:4}],next:1,more:false});
  if(path.startsWith('/api/nodes/n1/chat'))return jsonResponse({turns:[{role:'assistant',text:'answer',uid:'c',record:4}],decisions:[],source:'codex',live:'quiet'});
  return host.trapFetch(path,opts);
 };
 const app=await (await import(modulePath)).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
 try {await flush(200);await new Promise(r=>setTimeout(r,20));
  const turn={dataset:{cseq:'1',gkey:'s/o/c/g'},nextElementSibling:null,insertAdjacentHTML(_where,html){this.rowHTML=html;this.nextElementSibling={remove(){}}}};
  const bubble={closest:s=>s==='.bubble'?bubble:(s==='[data-cseq]'||s==='[data-seq], [data-cseq]')?turn:null};host.byId.get('msgs').dispatchEvent({type:'click',target:bubble});
  const fork={dataset:{gact:'fork'},closest:s=>s==='[data-gact]'?fork:null};host.byId.get('msgs').dispatchEvent({type:'click',target:fork});
  assert.match(host.byId.get('nc_prompt').value,/Quoted reply/);assert.equal(host.byId.get('nc_start').getAttribute('aria-label'),'Start and send');
  const copy={dataset:{gact:'copy'},closest:s=>s==='[data-gact]'?copy:null};host.byId.get('msgs').dispatchEvent({type:'click',target:copy});assert.deepEqual(copied,['Quoted reply']);
  const legacyCopies=[];host.window.isSecureContext=false;host.document.execCommand=command=>{legacyCopies.push(command);return true};
  host.byId.get('msgs').dispatchEvent({type:'click',target:copy});assert.deepEqual(legacyCopies,['copy']);
  const bookmark={dataset:{gact:'bookmark'},closest:s=>s==='[data-gact]'?bookmark:null};host.byId.get('msgs').dispatchEvent({type:'click',target:bookmark});await flush(50);
  host.byId.get('bookmarksbtn').dispatchEvent({type:'click',preventDefault(){},stopPropagation(){}});assert.match(host.byId.get('bookmarklist').innerHTML,/Quoted reply/);
  const sendto={dataset:{gact:'sendto'},closest:s=>s==='[data-gact]'?sendto:null};host.byId.get('msgs').dispatchEvent({type:'click',target:sendto});assert.match(host.byId.get('sendto_list').innerHTML,/Local/);
  const more={dataset:{gact:'more'},closest:s=>s==='[data-gact]'?more:null};host.byId.get('msgs').dispatchEvent({type:'click',target:more});assert.match(host.byId.get('sendto_list').innerHTML,/Rename person/);
  const desc={dataset:{gact:'desc'},closest:s=>s==='[data-gact]'?desc:null};host.byId.get('msgs').dispatchEvent({type:'click',target:desc});
  const choice={dataset:{choice:'0'},closest:s=>s==='[data-choice]'?choice:null};host.byId.get('sendto_list').onclick({target:choice});await flush(50);
  assert.match(host.byId.get('chatdescinput').value,/Quoted reply/);
  const row={dataset:{t:'missing'}},jump={dataset:{bmact:'jump'},closest:s=>s==='[data-bmact]'?jump:s==='.bookmark'?row:null};
  host.byId.get('bookmarkspane').dispatchEvent({type:'click',target:jump});await flush(200);await new Promise(r=>setTimeout(r,20));
  const toast=host.document.body.children.find(x=>x.id==='toast');assert.match(toast?.textContent||'',/unavailable/i);
 } finally {app.retire()}
});

for (const modulePath of ['../js/app.js','../js/app.js?graph=successor']) for (const local of [true,false]) test('the app wires received source markers to the shared bookmark: '+modulePath+' local='+local, async () => {
 const host=installHost({});host.window.localStorage.setItem('scimux-sel',local?'n1':'');
 const msgs=host.byId.get('msgs');msgs.insertAdjacentHTML=(_where,html)=>{msgs.innerHTML+=html};
 const source={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:1};
 const view={...source,access:'write',state:'live',title:'Chat',server_name:'Team',owner_name:'Owner',first_seen:'1',seen_comments:0,seen_mirror:0};
 const calls=[];const fetchImpl=async(path,opts)=>{path=String(path);calls.push(path);
  if(path==='/api/ui')return jsonResponse({lanes:[],groups:[],archived:[],pinned:[],bookmarks:[{t:'saved',text:'Discussion',team:source},{t:'stale',text:'Retained stale',team:{...source,generation:'stale'}}]});
  if(path==='/api/team/subscriptions/s/o/c/g' && opts?.method==='DELETE')return jsonResponse({});
  if(path.startsWith('/api/state'))return jsonResponse({nodes:local?[{id:'n1',title:'Local',agent:'codex',created_at:'T0'}]:[],unadopted:[],hostname:'computer',version:'test',sys:{}});
  if(path==='/api/team/servers')return jsonResponse({servers:[{id:'s',name:'Team'}]});
  if(path==='/api/team/subscriptions')return jsonResponse({subscriptions:[view],servers:[],truncated:false});
  if(path.includes('/comments?'))return jsonResponse({entries:[{seq:1,anchor:0,text:'Discussion',author:'p',at:'T'}],names:{p:'Person'},next:1,more:false});
  if(path.includes('/subscriptions/s/o/c/g?'))return jsonResponse({view,entries:[],next:0,more:false});
  if(path.startsWith('/api/nodes/n1/chat'))return jsonResponse({turns:[],decisions:[],source:'codex',live:'quiet'});
  return host.trapFetch(path,opts);
 };
 const app=await (await import(modulePath)).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
 try {await flush(200);await new Promise(r=>setTimeout(r,20));
  const card={dataset:{open:'team/s/o/c/g'},closest:s=>s==='[data-open]'?card:null};host.byId.get('cardlist').dispatchEvent({type:'click',target:card,preventDefault(){},stopPropagation(){}});
  await flush(200);await new Promise(r=>setTimeout(r,20));assert.equal(host.window.localStorage.getItem('scimux-sel'),'team/s/o/c/g');assert.match(host.byId.get('msgs').innerHTML,/data-bmarker="bookmark"/,calls.join(',')+' note='+host.byId.get('guestnote').textContent);
  const turn={dataset:{cseq:'1'}},marker={dataset:{bmarker:'bookmark'},closest:s=>s==='[data-bmarker]'?marker:s==='[data-seq], [data-cseq]'?turn:null};
  host.byId.get('msgs').dispatchEvent({type:'click',target:marker});assert.match(host.byId.get('bookmarklist').innerHTML,/Discussion/);
  let scrolled=0;const original=msgs.querySelector.bind(msgs);msgs.querySelector=s=>s==='[data-cseq="1"]'?{scrollIntoView(){scrolled++},classList:{add(){},remove(){}}}:original(s);
  const row={dataset:{t:'saved'}},jump={dataset:{bmact:'jump'},closest:s=>s==='[data-bmact]'?jump:s==='.bookmark'?row:null};
  host.byId.get('bookmarkspane').dispatchEvent({type:'click',target:jump});assert.equal(scrolled,1);
  host.byId.get('notesbtn').dispatchEvent({type:'click',preventDefault(){},stopPropagation(){}});await flush(50);
  const inboxRow={dataset:{t:'saved'}},inboxJump={dataset:{bmact:'jump'},closest:s=>s==='[data-bmact]'?inboxJump:s==='.wsibookmark'?inboxRow:null};
  host.byId.get('wsinboxlist').dispatchEvent({type:'click',target:inboxJump});assert.equal(scrolled,2);
  host.byId.get('notesbtn').dispatchEvent({type:'click',preventDefault(){},stopPropagation(){}});await flush(50);
  inboxRow.dataset.t='stale';host.byId.get('wsinboxlist').dispatchEvent({type:'click',target:inboxJump});
  const toast=host.document.body.children.find(x=>x.id==='toast');assert.match(toast?.textContent||'',/unavailable/i);assert.equal(scrolled,2);
  host.byId.get('guestremove').dispatchEvent({type:'click'});await flush(100);assert.equal(host.window.localStorage.getItem('scimux-sel'),local?'n1':'');
 } finally {app.retire()}
});

test('owner from-now frontier places anchor zero and start comments before a new mirrored turn', async () => {
  const host=installHost({}); host.window.localStorage.setItem('scimux-sel','n1');
  const view={server:'s',owner:'o',chat:'c',generation:'g',access:'owner',state:'live',title:'Chat',server_name:'Team',owner_name:'Owner',first_seen:'1',seen_comments:0,seen_mirror:0};
  const fetchImpl=async(path,opts)=>{
    path=String(path);
    if(path.startsWith('/api/state')) return jsonResponse({nodes:[{id:'n1',instance_id:'generation-1',title:'Local',agent:'codex',created_at:'T0'}],unadopted:[],hostname:'computer',version:'test',sys:{}});
    if(path==='/api/team/servers') return jsonResponse({servers:[{id:'s',name:'Team'}]});
    if(path==='/api/team/shares/n1') return jsonResponse({node:'n1',uid:'c',server:'s',generation:'g',state:'active',members:[]});
    if(path==='/api/team/subscriptions') return jsonResponse({subscriptions:[view],servers:[],truncated:false});
    if(path.includes('/comments?')) return jsonResponse({entries:[{seq:1,anchor:0,text:'zero reply',author:'p',at:'T'},{seq:2,anchor:1,text:'start reply',author:'p',at:'T'}],names:{p:'Person'},next:2,more:false});
    if(path.includes('/subscriptions/s/o/c/g?')) return jsonResponse({view,entries:[{seq:1,t:'start',local_segment:0,local_record:1},{seq:2,t:'assistant',local_segment:0,local_record:2,text:'new turn'}],next:2,more:false});
    if(path.startsWith('/api/nodes/n1/chat')) return jsonResponse({turns:[{role:'assistant',text:'earlier turn',uid:'c',record:1},{role:'assistant',text:'new turn',uid:'c',record:2}],decisions:[],source:'codex',live:'quiet'});
    return host.trapFetch(path,opts);
  };
  const app=await (await import('../js/app.js?graph=successor')).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
  try { await flush(200); await new Promise(r=>setTimeout(r,20)); const body=host.byId.get('msgs').innerHTML;
    assert.ok(body.indexOf('earlier turn') < body.indexOf('zero reply')); assert.ok(body.indexOf('zero reply') < body.indexOf('start reply')); assert.ok(body.indexOf('start reply') < body.indexOf('new turn'));
  } finally { app.retire(); }
});

test('owner timeline includes an ended retained generation beside the current generation', async () => {
  const host=installHost({}); host.window.localStorage.setItem('scimux-sel','n1');
  const base={server:'s',owner:'o',chat:'c',access:'owner',title:'Chat',server_name:'Team',owner_name:'Owner',first_seen:'1',seen_comments:0,seen_mirror:0,local_node:'n1'};
  const old={...base,generation:'old',state:'ended'}, live={...base,generation:'new',state:'live'};
  const fetchImpl=async(path,opts)=>{path=String(path);
    if(path.startsWith('/api/state')) return jsonResponse({nodes:[{id:'n1',instance_id:'generation-1',title:'Local',agent:'codex',created_at:'T0'}],unadopted:[],hostname:'computer',version:'test',sys:{}});
    if(path==='/api/team/servers') return jsonResponse({servers:[{id:'s',name:'Team'}]});
    if(path==='/api/team/shares/n1') return jsonResponse({node:'n1',uid:'c',server:'s',generation:'new',state:'active',members:[]});
    if(path==='/api/team/subscriptions') return jsonResponse({subscriptions:[old,live],servers:[],truncated:false});
    if(path.includes('/comments?')) return jsonResponse({entries:[{seq:1,anchor:1,text:path.includes('/old/')?'Earlier discussion':'Current discussion',author:'p',at:'T'}],names:{p:'Person'},next:1,more:false});
    if(path.includes('/subscriptions/s/o/c/')) return jsonResponse({view:path.includes('/old?')?old:live,entries:[{seq:1,t:'assistant',text:'answer',local_segment:0,local_record:4}],next:1,more:false});
    if(path.startsWith('/api/nodes/n1/chat')) return jsonResponse({turns:[{role:'assistant',text:'answer',uid:'c',record:4}],decisions:[],source:'codex',live:'quiet'});
    return host.trapFetch(path,opts);
  };
  const app=await (await import('../js/app.js?graph=successor')).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
  try { await flush(200); await new Promise(r=>setTimeout(r,20)); const body=host.byId.get('msgs').innerHTML;
    assert.equal((body.match(/Earlier discussion/g)||[]).length,1); assert.equal((body.match(/Current discussion/g)||[]).length,1); assert.equal((body.match(/>answer</g)||[]).length,1);
  } finally { app.retire(); }
});

test('ended retained owner discussion appears after recreation without an active share', async () => {
 const host=installHost({}); host.window.localStorage.setItem('scimux-sel','n1');
 const view={server:'s',owner:'o',chat:'c',generation:'old',access:'owner',state:'ended',local_node:'n1',title:'Chat',server_name:'Team',owner_name:'Owner',first_seen:'1',seen_comments:0,seen_mirror:0};
 const fetchImpl=async(path,opts)=>{path=String(path);
  if(path.startsWith('/api/state')) return jsonResponse({nodes:[{id:'n1',instance_id:'generation-1',title:'Local',agent:'codex',created_at:'T0'}],unadopted:[],hostname:'computer',version:'test',sys:{}});
  if(path==='/api/team/servers') return jsonResponse({servers:[{id:'s',name:'Team'}]});
  if(path==='/api/team/shares/n1') return new Response('{"error":"not_found"}',{status:404});
  if(path==='/api/team/subscriptions') return jsonResponse({subscriptions:[view],servers:[],truncated:false});
  if(path.includes('/comments?')) return jsonResponse({entries:[{seq:1,anchor:1,text:'retained reply',author:'p',at:'T'}],names:{p:'Person'},next:1,more:false});
  if(path.includes('/subscriptions/s/o/c/old?')) return jsonResponse({view,entries:[{seq:1,t:'start',local_segment:0,local_record:1}],next:1,more:false});
  if(path.startsWith('/api/nodes/n1/chat')) return jsonResponse({turns:[{role:'assistant',text:'prior answer',uid:'c',record:1}],decisions:[],source:'codex',live:'quiet'});
  return host.trapFetch(path,opts);
 };
 const app=await (await import('../js/app.js?graph=successor')).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
 try { await flush(200); await new Promise(r=>setTimeout(r,20)); const body=host.byId.get('msgs').innerHTML;
  assert.equal((body.match(/retained reply/g)||[]).length,1); assert.ok(body.indexOf('prior answer')<body.indexOf('retained reply'));
  assert.equal(host.byId.get('destination').hidden,true);
 } finally {app.retire()}
});

test('owner reply anchored after clear appears in an empty current segment', async () => {
 const host=installHost({}); host.window.localStorage.setItem('scimux-sel','n1');
 const view={server:'s',owner:'o',chat:'c',generation:'g',access:'owner',state:'live',title:'Chat',server_name:'Team',owner_name:'Owner',first_seen:'1',seen_comments:0,seen_mirror:0};
 const fetchImpl=async(path,opts)=>{path=String(path);
  if(path.startsWith('/api/state')) return jsonResponse({nodes:[{id:'n1',instance_id:'generation-1',title:'Local',agent:'codex',created_at:'T0'}],unadopted:[],hostname:'computer',version:'test',sys:{}});
  if(path==='/api/team/servers') return jsonResponse({servers:[{id:'s',name:'Team'}]});
  if(path==='/api/team/shares/n1') return jsonResponse({node:'n1',uid:'c',server:'s',generation:'g',state:'active',members:[]});
  if(path==='/api/team/subscriptions') return jsonResponse({subscriptions:[view],servers:[],truncated:false});
  if(path.includes('/comments?')) return jsonResponse({entries:[{seq:1,anchor:1,text:'after clear',author:'p',at:'T'}],names:{p:'Person'},next:1,more:false});
  if(path.includes('/subscriptions/s/o/c/g?')) return jsonResponse({view,entries:[{seq:1,t:'source',local_segment:0,local_record:6}],next:1,more:false});
  if(path.startsWith('/api/nodes/n1/chat')) return jsonResponse({turns:[],log_uid:'c',segment_index:1,prior_turns:1,chat_started:'T',decisions:[],source:'codex',live:'quiet'});
  return host.trapFetch(path,opts);
 };
 const app=await (await import('../js/app.js?graph=successor')).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
 try { await flush(200); await new Promise(r=>setTimeout(r,20)); assert.match(host.byId.get('msgs').innerHTML,/after clear/); } finally {app.retire()}
});

test('local node named team can read and send through the real app routing', async () => {
 const host=installHost({}); host.window.localStorage.setItem('scimux-sel','team'); const calls=[];
 const fetchImpl=async(path,opts)=>{path=String(path);calls.push({path,method:opts?.method||'GET'});
  if(path.startsWith('/api/state')) return jsonResponse({nodes:[{id:'team',instance_id:'team-generation',title:'Team',agent:'codex',created_at:'T0'}],unadopted:[],hostname:'computer',version:'test',sys:{}});
  if(path==='/api/team/servers') return jsonResponse({servers:[]});
  if(path==='/api/team/shares/team') return new Response('{"error":"not_found"}',{status:404});
  if(path.startsWith('/api/nodes/team/chat')) return jsonResponse({turns:[{role:'assistant',text:'local'}],live:'quiet',source:'codex'});
  return host.trapFetch(path,opts);
 };
 const app=await (await import('../js/app.js?graph=successor')).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
 try {await flush(200); await new Promise(r=>setTimeout(r,10));
  const prompt=host.byId.get('prompt'); prompt.innerText='hello'; prompt.dispatchEvent({type:'input',target:prompt}); await host.byId.get('sendbtn').click(); await flush(30);
  assert.ok(calls.some(c=>c.path==='/api/nodes/team/chat?tail=50')); assert.ok(calls.some(c=>c.path==='/api/nodes/team/send?chat_instance=team-generation' && c.method==='POST'));
 } finally {app.retire()}
});

test('owner anchor zero is visible at the saved frontier before any mirror arrives', async () => {
 const host=installHost({}); host.window.localStorage.setItem('scimux-sel','n1');
 const view={server:'s',owner:'o',chat:'c',generation:'g',access:'owner',state:'live',local_node:'n1',local_frontier:{node:'n1',segment:0,record:1},title:'Chat',server_name:'Team',owner_name:'Owner',first_seen:'1',seen_comments:0,seen_mirror:0};
 const fetchImpl=async(path,opts)=>{path=String(path);
  if(path.startsWith('/api/state')) return jsonResponse({nodes:[{id:'n1',instance_id:'generation-1',title:'Local',agent:'codex',created_at:'T0'}],unadopted:[],hostname:'computer',version:'test',sys:{}});
  if(path==='/api/team/servers') return jsonResponse({servers:[{id:'s',name:'Team'}]});
  if(path==='/api/team/shares/n1') return jsonResponse({node:'n1',uid:'c',server:'s',generation:'g',state:'active',members:[]});
  if(path==='/api/team/subscriptions') return jsonResponse({subscriptions:[view],servers:[],truncated:false});
  if(path.includes('/comments?')) return jsonResponse({entries:[{seq:1,anchor:0,text:'before mirror',author:'p',at:'T'}],names:{p:'Person'},next:1,more:false});
  if(path.includes('/subscriptions/s/o/c/g?')) return jsonResponse({view,entries:[],next:0,more:false});
  if(path.startsWith('/api/nodes/n1/chat')) return jsonResponse({turns:[{role:'assistant',text:'private earlier',uid:'c',record:1},{role:'assistant',text:'local later',uid:'c',record:2}],log_uid:'c',segment_index:0,decisions:[],source:'codex',live:'quiet'});
  return host.trapFetch(path,opts);
 };
 const app=await (await import('../js/app.js?graph=successor')).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
 try {await flush(200);await new Promise(r=>setTimeout(r,20));const body=host.byId.get('msgs').innerHTML;assert.ok(body.indexOf('private earlier')<body.indexOf('before mirror'));assert.ok(body.indexOf('before mirror')<body.indexOf('local later'));} finally {app.retire()}
});

test('stale share for a reused node slug cannot become the new chat People context', async () => {
 const host=installHost({}); host.window.localStorage.setItem('scimux-sel','n1'); const calls=[];
 const old={server:'s',owner:'o',chat:'old',generation:'g',access:'owner',state:'live',title:'Old',server_name:'Team',owner_name:'Owner',first_seen:'1',seen_comments:0,seen_mirror:0};
 const fetchImpl=async(path,opts)=>{path=String(path);calls.push({path,method:opts?.method||'GET'});
  if(path.startsWith('/api/state')) return jsonResponse({nodes:[{id:'n1',title:'New',agent:'codex',created_at:'T0'}],unadopted:[],hostname:'computer',version:'test',sys:{}});
  if(path==='/api/team/servers') return jsonResponse({servers:[{id:'s',name:'Team'}]});
  if(path==='/api/team/shares/n1') return jsonResponse({node:'n1',uid:'old',server:'s',generation:'g',state:'active',members:[]});
  if(path==='/api/team/subscriptions') return jsonResponse({subscriptions:[old],servers:[],truncated:false});
  if(path.includes('/comments?')) return jsonResponse({entries:[],names:{},next:0,more:false});
  if(path.includes('/subscriptions/s/o/old/g?')) return jsonResponse({view:old,entries:[],next:0,more:false});
  if(path.startsWith('/api/nodes/n1/chat')) return jsonResponse({log_uid:'new',segment_index:0,turns:[{role:'assistant',uid:'new',record:1,text:'new chat'}],decisions:[],source:'codex',live:'quiet'});
  return host.trapFetch(path,opts);
 };
 const app=await (await import('../js/app.js?graph=successor')).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
 try { await flush(200); await new Promise(r=>setTimeout(r,20)); assert.equal(host.byId.get('destination').hidden,true); assert.ok(!calls.some(c=>c.path.includes('/subscriptions/s/o/old/g?'))); } finally {app.retire()}
});

test('received metadata that arrives before a delayed state response still restores received selection', async () => {
 const host=installHost({}); host.window.localStorage.setItem('scimux-sel','team/s/o/c/g'); let resolveState;
 const view={server:'s',owner:'o',chat:'c',generation:'g',access:'read',state:'live',title:'Received',server_name:'Team',owner_name:'Owner',first_seen:'1'};
 const fetchImpl=async(path,opts)=>{path=String(path);
  if(path.startsWith('/api/state')) return new Promise(resolve=>{resolveState=resolve});
  if(path==='/api/settings') return jsonResponse({});
  if(path==='/api/team/subscriptions') return jsonResponse({subscriptions:[view],servers:[],truncated:false});
  if(path.includes('/comments?')) return jsonResponse({entries:[],names:{},next:0,more:false});
  if(path.includes('/subscriptions/s/o/c/g?')) return jsonResponse({view,entries:[],next:0,more:false});
  return host.trapFetch(path,opts);
 };
 const app=await (await import('../js/app.js?graph=successor')).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
 try {await flush(150); assert.equal(typeof resolveState,'function');
  resolveState(jsonResponse({nodes:[{id:'n1',instance_id:'generation-1',title:'Local',agent:'codex',created_at:'T0'}],unadopted:[],hostname:'computer',version:'test',sys:{}}));
  await flush(150); assert.equal(host.window.localStorage.getItem('scimux-sel'),'team/s/o/c/g'); assert.equal(host.byId.get('chattitle').textContent,'Received');
 } finally {app.retire()}
});

test('phone received card opens chat and back returns to Activities', async () => {
 const host=installHost({}); const phone=()=>({matches:false,addEventListener(){},removeEventListener(){},addListener(){},removeListener(){}}); globalThis.matchMedia=phone; host.window.matchMedia=phone;
 const view={server:'s',owner:'o',chat:'c',generation:'g',access:'read',state:'live',title:'Received',server_name:'Team',owner_name:'Owner',first_seen:'1'};
 const fetchImpl=async(path,opts)=>{path=String(path);
  if(path.startsWith('/api/state')) return jsonResponse({nodes:[{id:'n1',instance_id:'generation-1',title:'Local',agent:'codex',created_at:'T0'}],unadopted:[],hostname:'computer',version:'test',sys:{}});
  if(path==='/api/team/subscriptions') return jsonResponse({subscriptions:[view],servers:[],truncated:false});
  if(path.includes('/comments?')) return jsonResponse({entries:[],names:{},next:0,more:false});
  if(path.includes('/subscriptions/s/o/c/g?')) return jsonResponse({view,entries:[],next:0,more:false});
  return host.trapFetch(path,opts);
 };
 const app=await (await import('../js/app.js?graph=successor')).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
 try {await flush(200); await new Promise(r=>setTimeout(r,10)); const card={dataset:{open:'team/s/o/c/g'},closest:s=>s==='[data-open]'?card:null};
  host.byId.get('cardlist').dispatchEvent({type:'click',target:card,preventDefault(){},stopPropagation(){}}); await flush(50);
  assert.equal(host.document.body.classList.contains('cards-open'),false); assert.equal(host.byId.get('scrim').classList.contains('on'),false);
  await host.byId.get('chatback').click(); assert.equal(host.document.body.classList.contains('cards-open'),true);
 } finally {app.retire()}
});

test('late local UID mismatching an already loaded share releases the new chat without adopting it', async () => {
 const host=installHost({}); host.window.localStorage.setItem('scimux-sel','n1'); let resolveChat, requested=false;
 const old={server:'s',owner:'o',chat:'old',generation:'g',access:'owner',state:'live',title:'Old',server_name:'Team',owner_name:'Owner',first_seen:'1'};
 const fetchImpl=async(path,opts)=>{path=String(path);
  if(path.startsWith('/api/state')) return jsonResponse({nodes:[{id:'n1',title:'New',agent:'codex',created_at:'T0'}],unadopted:[],hostname:'computer',version:'test',sys:{}});
  if(path==='/api/team/servers') return jsonResponse({servers:[{id:'s',name:'Team'}]});
  if(path==='/api/team/shares/n1'){requested=true;return jsonResponse({node:'n1',uid:'old',server:'s',generation:'g',state:'active',members:[]})}
  if(path==='/api/team/subscriptions') return jsonResponse({subscriptions:[old],servers:[],truncated:false});
  if(path.startsWith('/api/nodes/n1/chat')) return new Promise(resolve=>{resolveChat=resolve});
  return host.trapFetch(path,opts);
 };
 const app=await (await import('../js/app.js?graph=successor')).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
 try {await flush(200); assert.ok(requested);assert.equal(typeof resolveChat,'function');
  resolveChat(jsonResponse({log_uid:'new',segment_index:0,turns:[{role:'assistant',uid:'new',record:1,text:'new chat'}],decisions:[],source:'codex',live:'quiet'}));
  await flush(200); assert.equal(host.byId.get('destination').hidden,true); assert.equal(host.byId.get('prompt').getAttribute('contenteditable'),'true');
 } finally {app.retire()}
});

test('app restores a rejected in-flight Agent prompt without dispatching a retry', async () => {
 const host=installHost({});host.window.localStorage.setItem('scimux-sel','n1');
 const source={uid:'u',segment:0,record:1};
 host.window.localStorage.setItem('scimux-prompt-flights:n1',JSON.stringify([{id:1,accepted:true,text:'rejected draft',address:{uid:'u',segment:0,record:2},boundary:source,sources:[source]}]));
 const prompt=host.byId.get('prompt');Object.defineProperty(prompt,'innerText',{get(){return this.textContent},set(value){this.textContent=value},configurable:true});
 const posts=[];const fetchImpl=async(path,opts)=>{
  path=String(path);if(opts?.method==='POST')posts.push(path);
  if(path.startsWith('/api/state'))return jsonResponse({nodes:[{id:'n1',title:'Local',agent:'codex'}],unadopted:[],hostname:'computer',version:'test',sys:{}});
  if(path.startsWith('/api/nodes/n1/chat'))return jsonResponse({turns:[{uid:'u',record:1,role:'assistant',text:'old'},{uid:'u',record:2,role:'user',text:'rejected draft'}],log_uid:'u',segment_index:0,trace_outcome:'rejected',error:'dispatch rejected',turn_in_flight:false,source:'acp',live:'quiet'});
  return host.trapFetch(path,opts);
 };
 const app=await (await import('../js/app.js?graph=successor')).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
 try{await flush(200);assert.match(prompt.textContent,/rejected draft/);assert.ok(host.window.localStorage.getItem('scimux-prompt-sources:n1'));assert.equal(posts.filter(p=>p.includes('/api/nodes/')).length,0);}finally{app.retire();}
});

test('late send completion cannot repaint another selected chat', async () => {
 const host=installHost({});host.window.localStorage.setItem('scimux-sel','n1');let finishSend;const calls=[];
 const prompt=host.byId.get('prompt');Object.defineProperty(prompt,'innerText',{get(){return this.textContent},set(value){this.textContent=value},configurable:true});
 const fetchImpl=async(path,opts)=>{path=String(path);calls.push({path,method:opts?.method||'GET'});
  if(path==='/api/nodes/n1/send?chat_instance=generation-1'&&opts?.method==='POST')return new Promise(resolve=>{finishSend=resolve});
  if(path.startsWith('/api/state'))return jsonResponse({nodes:[{id:'n1',instance_id:'generation-1',title:'First',agent:'codex'},{id:'n2',instance_id:'generation-2',title:'Second',agent:'codex'}],unadopted:[],hostname:'computer',version:'test',sys:{}});
  if(path.startsWith('/api/nodes/')&&path.includes('/chat'))return jsonResponse({turns:[],source:'acp',live:'quiet'});
  return host.trapFetch(path,opts);
 };
 const app=await (await import('../js/app.js?graph=successor')).createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
 try{await flush(200);prompt.innerText='first draft';prompt.dispatchEvent({type:'input',target:prompt});host.window.localStorage.setItem('scimux-prompt-sources:n1',JSON.stringify([{uid:'u',segment:0,record:1}]));host.byId.get('sendbtn').click();await flush(100);assert.equal(typeof finishSend,'function');
  const card={dataset:{open:'n2'},closest:s=>s==='[data-open]'?card:null};host.byId.get('cardlist').dispatchEvent({type:'click',target:card,preventDefault(){},stopPropagation(){}});await flush(100);
  assert.equal(host.window.localStorage.getItem('scimux-sel'),'n2');
  prompt.innerText='second draft';prompt.dispatchEvent({type:'input',target:prompt});const before=calls.filter(c=>c.path.startsWith('/api/nodes/n2/chat')).length;
  finishSend(jsonResponse({ok:true}));await flush(100);assert.equal(prompt.innerText,'second draft');assert.equal(calls.filter(c=>c.path.startsWith('/api/nodes/n2/chat')).length,before);
 }finally{app.retire()}
});

test('startup reports when drafts are limited to this tab', async () => {
 const host=installHost({});host.window.localStorage=null;
 const app=await (await import('../js/app.js?graph=successor')).createApp({fetchImpl:host.trapFetch,assetURL:identityAssetURL,document:host.document,window:host.window});
 try {await flush(30);const toast=host.document.body.children.find(x=>x.id==='toast');assert.match(toast?.textContent||'',/kept in this tab/);} finally {app.retire()}
});
