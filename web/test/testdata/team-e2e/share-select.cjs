'use strict';
function selectSharedSubscription(views, target) {
  return views.find(view => view.server === target.server && view.owner === target.owner &&
    view.chat === target.chat && view.generation === target.generation);
}
module.exports = {selectSharedSubscription};
