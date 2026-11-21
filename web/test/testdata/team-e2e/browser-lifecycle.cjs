'use strict';

async function retireCompletedContexts(pages, contexts, capture) {
  await capture();
  for (const context of contexts) await context.close();
  pages.length = 0;
  contexts.length = 0;
}

module.exports = {retireCompletedContexts};
