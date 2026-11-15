export function guestKey() { return ""; }
export function guestGroups() { return []; }
export function createGuestFeature() {
  return { tick() {}, bind() {}, destroy() {}, async settled() {}, tab: { tabHTML() { return ""; }, render() { return false; } } };
}
