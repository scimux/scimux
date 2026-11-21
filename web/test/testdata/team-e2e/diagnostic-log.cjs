'use strict';
const fs=require('node:fs');
const path=require('node:path');
function createDiagnosticBudget(root) {
  const pending=[],files=new Map();let bytes=0,failed=false;
  let rootIdentity;
  try {const st=fs.lstatSync(root);if(!st.isDirectory()||st.isSymbolicLink()||st.uid!==process.getuid()||(st.mode&0o777)!==0o700)throw Error('root');rootIdentity={dev:st.dev,ino:st.ino};}catch{failed=true;}
  function verifyRoot() {const st=fs.lstatSync(root);if(!rootIdentity||!st.isDirectory()||st.isSymbolicLink()||st.uid!==process.getuid()||(st.mode&0o777)!==0o700||st.dev!==rootIdentity.dev||st.ino!==rootIdentity.ino)throw Error('root');}
  return {
    attach(name,stream) {
      let fd,file;
      try {
        if(!/^[a-z0-9][a-z0-9-]*$/.test(name))throw Error('invalid');
        verifyRoot();file=path.join(root,name+'.log');
        fd=fs.openSync(file,fs.constants.O_WRONLY|fs.constants.O_APPEND|fs.constants.O_CREAT|fs.constants.O_NOFOLLOW|fs.constants.O_NONBLOCK,0o600);
        const opened=fs.fstatSync(fd),last=fs.lstatSync(file),prior=files.get(name);
        if(!opened.isFile()||last.isSymbolicLink()||opened.uid!==process.getuid()||opened.dev!==rootIdentity.dev||(opened.mode&0o777)!==0o600||opened.dev!==last.dev||opened.ino!==last.ino||!Number.isSafeInteger(opened.size)||(prior&&(opened.dev!==prior.dev||opened.ino!==prior.ino||opened.size!==prior.size)))throw Error('identity');
        if(!prior){bytes+=opened.size;if(bytes>1048576)throw Error('capacity');files.set(name,{dev:opened.dev,ino:opened.ino,size:opened.size});}
      } catch {failed=true;if(fd!==undefined){try{fs.closeSync(fd);}catch{}fd=undefined;}}
      pending.push(new Promise(resolve=>{
        stream.on('data',chunk=>{
          const value=Buffer.from(chunk),allowed=Math.min(value.length,Math.max(0,1048576-bytes));
          if(allowed<value.length)failed=true;
          if(fd===undefined)return;
          try {
            let offset=0;while(offset<allowed){const written=fs.writeSync(fd,value,offset,allowed-offset);if(!written)throw Error('write');offset+=written;bytes+=written;files.get(name).size+=written;}
          }catch {failed=true;}
        });
        stream.once('error',()=>{failed=true;});
        stream.once('close',()=>{
          if(fd!==undefined){
            try {verifyRoot();const opened=fs.fstatSync(fd),last=fs.lstatSync(file),expected=files.get(name);if(!opened.isFile()||last.isSymbolicLink()||opened.dev!==expected.dev||opened.ino!==expected.ino||opened.size!==expected.size||last.dev!==opened.dev||last.ino!==opened.ino||last.size!==opened.size)throw Error('identity');}catch{failed=true;}
            try{fs.closeSync(fd);}catch{failed=true;}
          }
          resolve();
        });
      }));
    },
    async finish(){await Promise.all(pending);return {bytes,failed};},
  };
}
module.exports={createDiagnosticBudget};
