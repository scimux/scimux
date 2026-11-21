'use strict';
const {configureRunner,runnerExitCode}=require('./runner-bootstrap.cjs');
const [runtime,evidence,signal]=process.argv.slice(2);
process.once(signal,()=>process.stdout.write('observed\n'));
const env={SCIMUX_E2E_RUNTIME_PARENT:runtime,SCIMUX_E2E_EVIDENCE_PARENT:evidence,SCIMUX_E2E_SUPERVISOR:process.execPath};
configureRunner({env,runnerPath:__filename,verifySupervisor:async()=>{
  process.stdout.write('ready\n');
  await new Promise(resolve=>{process.stdin.resume();process.stdin.once('end',resolve);});
}}).then(result=>{process.stdout.write(JSON.stringify(result)+'\n');process.exitCode=runnerExitCode(result);});
