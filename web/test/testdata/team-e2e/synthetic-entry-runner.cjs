'use strict';
const fs=require('node:fs');
let input='';
process.stdin.setEncoding('utf8');
process.stdin.on('data',chunk=>{input+=chunk;});
process.stdin.on('end',()=>{
  let inherited=false;
  try { fs.writeSync(3,'unexpected');inherited=true; } catch {}
  process.stdout.write('status clean\nrunner_exit 0\nreaped 0\ninput:'+input);
  process.stderr.write(inherited?'unexpected descriptor':'synthetic stderr');
  process.exitCode=7;
});
