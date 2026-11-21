'use strict';
const assert = require('node:assert/strict');
const path = require('node:path');
const { chromium, devices } = require(process.env.SCIMUX_E2E_PLAYWRIGHT);
const browserTask=chromium.launch({handleSIGINT:false,handleSIGTERM:false,handleSIGHUP:false});
let closing;
function closeBrowser(){return closing ||= browserTask.then(browser=>browser.close());}
for(const [signal,code] of [['SIGINT',130],['SIGTERM',143],['SIGHUP',129]]) process.once(signal,()=>{
 Promise.race([closeBrowser(),new Promise((_,reject)=>setTimeout(()=>reject(Error('browser cleanup timed out')),4000))])
  .then(()=>process.exit(code),err=>{console.error('browser cleanup failed:',err);process.exit(1)});
});

(async()=>{
 const mode=process.env.SCIMUX_TUNNEL_MODE,base=process.env.SCIMUX_TUNNEL_URL;
 const browser=await browserTask;
 try {
  const context=await browser.newContext(mode==='phone'?devices['iPhone 13']:{viewport:{width:1280,height:900},acceptDownloads:true});
  try {
   const page=await context.newPage();await page.goto(base,{waitUntil:'networkidle'});
   await page.locator('#ready').waitFor();await page.locator('#load').click();
   await page.waitForFunction(async()=>await (await fetch('/status')).text()==='held',null,{timeout:10000});
   await page.locator('#poll').click();await page.locator('#polled').waitFor();await page.locator('#release').click();
   const link=page.locator('#file');await link.waitFor({timeout:10000});
   if (!/^blob:/.test(await link.getAttribute('href') || '')) {
    await page.screenshot({path:path.join(process.env.SCIMUX_TUNNEL_ARTIFACTS,'remote-people-tunnel-'+mode+'-failure.png'),fullPage:true});
    throw Error('held file was not available through the remote asset transport');
   }
   const downloadEvent=page.waitForEvent('download');await link.click();const download=await downloadEvent;
   const bytes=require('node:fs').readFileSync(await download.path(),'utf8');assert.equal(bytes,'x');
   assert.equal(await (await page.request.get(base+'/dispatch-count')).text(),'0');
   await page.screenshot({path:path.join(process.env.SCIMUX_TUNNEL_ARTIFACTS,'remote-people-tunnel-'+mode+'.png'),fullPage:true});
   console.log('PASS '+mode+' held download survived unchanged capability poll; zero dispatches');
  } finally { await context.close(); }
 } finally { await closeBrowser(); }
})().catch(err=>{console.error(err);process.exitCode=1});
