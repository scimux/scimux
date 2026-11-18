'use strict';
function createCleanupOnce(cleanup) {
  let task;
  return () => {
    if (!task) task = Promise.resolve().then(cleanup);
    return task;
  };
}
module.exports = {createCleanupOnce};
