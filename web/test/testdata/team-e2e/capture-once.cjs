'use strict';
function createPageCapture(capture) {
  const tasks=new WeakMap();
  return (page,label)=>{
    let task=tasks.get(page);
    if(!task){task=Promise.resolve().then(()=>capture(page,label));tasks.set(page,task);}
    return task;
  };
}
module.exports={createPageCapture};
