'use strict';
function installRunnerSignals({events,manifest,cleanup,record,exit,notify,release=()=>{}}) {
  let first=null;
  const handlers=new Map();
  for(const [signal,code] of [['SIGINT',130],['SIGTERM',143]]) {
    const handler=()=>{
      notify('received');
      if(first!==null)return;
      first=signal;manifest.signal=signal;
      if(manifest.result!=='fail')manifest.result='interrupted';
      release();
      Promise.resolve().then(cleanup).then(()=>{
        record();exit(manifest.result==='fail'?1:code);
      }).catch(()=>{
        manifest.result='fail';
        try {record();}catch {}
        notify('cleanup-failed');exit(1);
      });
    };
    handlers.set(signal,handler);events.on(signal,handler);
  }
  return {current:()=>first,dispose(){for(const [signal,handler] of handlers)events.off(signal,handler);}};
}
module.exports={installRunnerSignals};
function finishScenario(manifest,signal) { if(!signal&&manifest.result!=='fail')manifest.result='pass'; }
module.exports.finishScenario=finishScenario;

function controlledFailureCode(error,deliberate) { return deliberate!==null&&typeof deliberate==='object'&&error===deliberate?'controlled-native-cleanup':undefined; }
module.exports.controlledFailureCode=controlledFailureCode;
