import test from "node:test";
import assert from "node:assert/strict";
import { createAssetPreview, bindAssetPreviewLinks } from "../js/asset-preview.js";

function target(){
  const listeners = new Map();
  return {
    listeners,
    addEventListener(k, fn){ listeners.set(k, fn); },
    removeEventListener(k, fn){ if (listeners.get(k) === fn) listeners.delete(k); },
    dispatch(k, ev = {}){ listeners.get(k)?.(ev); },
  };
}

test("preview owns dialog focus, escape, transforms, and teardown", async () => {
  const doc = target();
  const previous = { focused: 0, focus(){ this.focused++; } };
  const overlay = target();
  overlay.hidden = true;
  overlay.style = {};
  overlay.querySelector = (sel) => ({
    focus(){}, style: {}, setAttribute(){}, removeAttribute(){},
  });
  const preview = createAssetPreview({ document: doc, overlay, activeElement: () => previous });
  preview.open({ url: "blob:asset", name: "image one.png" });
  assert.equal(overlay.hidden, false);
  assert.equal(preview.state().open, true);
  preview.zoomBy(2);
  preview.panBy(20, -10);
  assert.deepEqual(preview.state().transform, { scale: 2, x: 20, y: -10 });
  preview.reset();
  assert.deepEqual(preview.state().transform, { scale: 1, x: 0, y: 0 });
  doc.dispatch("keydown", { key: "Escape", preventDefault(){}, stopPropagation(){} });
  assert.equal(overlay.hidden, true);
  assert.equal(previous.focused, 1);
  preview.destroy();
  assert.equal(doc.listeners.size, 0);
});

test("rapid replacement ignores late image completion and revokes only owned URLs", () => {
  const doc = target();
  const overlay = target();
  overlay.hidden = true;
  overlay.style = {};
  const img = target();
  img.style = {};
  img.setAttribute = () => {};
  img.removeAttribute = () => {};
  overlay.querySelector = () => img;
  const revoked = [];
  const preview = createAssetPreview({ document: doc, overlay, revokeObjectURL: u => revoked.push(u) });
  preview.open({ url: "blob:shared-one", name: "one.png" });
  const late = img.listeners.get("load");
  preview.open({ url: "blob:shared-two", name: "two.png" });
  late();
  assert.equal(preview.state().name, "two.png");
  preview.close();
  assert.deepEqual(revoked, [], "shared transport cache URLs are not preview-owned");
});

test("preview reports load failure, supports pinch/pan, traps focus, and cleans owned resources", () => {
  const doc = target();
  let active = null;
  const makeEl = () => Object.assign(target(), {
    hidden: false, disabled: false, style: {},
    focus(){ active = this; }, setAttribute(){}, removeAttribute(){},
  });
  const image = makeEl(), stage = makeEl(), title = makeEl(), status = makeEl();
  const download = makeEl(), external = makeEl(), close = makeEl(), reset = makeEl();
  const bySelector = new Map([
    ["[data-preview-image]", image], ["[data-preview-stage]", stage],
    ["[data-preview-title]", title], ["[data-preview-status]", status],
    ["[data-preview-download]", download], ["[data-preview-external]", external],
    ["[data-preview-close]", close], ["[data-preview-reset]", reset],
  ]);
  const overlay = makeEl();
  overlay.hidden = true;
  overlay.querySelector = sel => bySelector.get(sel) || null;
  overlay.querySelectorAll = () => [close, reset, download, external];
  const revoked = [];
  const preview = createAssetPreview({
    document: doc, overlay, activeElement: () => active,
    revokeObjectURL: url => revoked.push(url),
  });
  active = external;
  preview.open({ url: "blob:owned", name: "diagram.png", ownedURL: true });
  assert.equal(active, close);
  assert.equal(status.textContent, "Loading image…");
  image.dispatch("error");
  assert.equal(status.textContent, "Image could not be loaded.");
  image.dispatch("load");
  assert.equal(status.hidden, true);

  stage.dispatch("pointerdown", { pointerId: 1, clientX: 0, clientY: 0 });
  stage.dispatch("pointerdown", { pointerId: 2, clientX: 10, clientY: 0 });
  stage.dispatch("pointermove", { pointerId: 2, clientX: 30, clientY: 0, preventDefault(){} });
  assert.equal(preview.state().transform.scale, 3);
  stage.dispatch("pointerup", { pointerId: 2 });
  stage.dispatch("pointermove", { pointerId: 1, clientX: 5, clientY: 4, preventDefault(){} });
  assert.deepEqual(preview.state().transform, { scale: 3, x: 5, y: 4 });

  active = external;
  doc.dispatch("keydown", { key: "Tab", preventDefault(){} });
  assert.equal(active, close, "Tab wraps inside dialog");
  doc.dispatch("keydown", { key: "Tab", shiftKey: true, preventDefault(){} });
  assert.equal(active, external, "Shift-Tab wraps inside dialog");
  reset.dispatch("click");
  assert.deepEqual(preview.state().transform, { scale: 1, x: 0, y: 0 });
  close.dispatch("click");
  assert.deepEqual(revoked, ["blob:owned"]);
  preview.destroy();
  assert.equal(doc.listeners.size + stage.listeners.size + close.listeners.size + reset.listeners.size, 0);
});

test("preview link delegation is removable and uses the supplied transport URL", () => {
  const root = target();
  const opens = [];
  const destroy = bindAssetPreviewLinks(root, { open: value => opens.push(value) });
  let prevented = 0;
  const link = { href: "blob:tunnel", title: "fallback", dataset: { name: "remote.png" } };
  root.dispatch("click", { target: { closest: () => link }, preventDefault(){ prevented++; } });
  root.dispatch("click", { target: { closest: () => null }, preventDefault(){ prevented++; } });
  assert.deepEqual(opens, [{ url: "blob:tunnel", name: "remote.png" }]);
  assert.equal(prevented, 1);
  destroy();
  assert.equal(root.listeners.size, 0);
  const noop = bindAssetPreviewLinks(null, null);
  noop();
});

test("preview link delegation can stop bubbling in capture mode", () => {
  const root = target();
  let stopped = 0;
  const destroy = bindAssetPreviewLinks(root, {open(){}}, {capture:true, stop:true});
  const link = {href:"/image",dataset:{name:"image.png"}};
  root.dispatch("click", {target:{closest:()=>link},preventDefault(){},stopPropagation(){stopped++;}});
  assert.equal(stopped, 1);
  destroy();
});

test("preview default no-op revoker safely handles an owned URL", () => {
  const preview = createAssetPreview({ document: target(), overlay: null });
  preview.open({ url: "blob:private", ownedURL: true });
  preview.close();
  preview.destroy();
});
