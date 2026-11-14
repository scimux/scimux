/* Share button, share sheet, and status line.
 * Stub: the functions exist so tests can import them. Nothing is loaded.
 * api, doc, openSheet, closeSheets, and now are injected later. This file
 * does not touch document, window, or fetch.
 */
export function shareStatusText() {
  return "";
}

export function shareErrorText() {
  return "";
}

export function createShareFeature() {
  return {
    select() {},
    tick() {},
    bind() {},
    destroy() {},
    settled() {
      return Promise.resolve();
    },
  };
}
