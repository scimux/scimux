import test from "node:test";
import assert from "node:assert/strict";
import { harnessRowsHTML, updateVibeInspectControl } from "../js/harness.js";
import { mountHarnessRows } from "../js/app.js";

function makeHost(row){
  let vibe = null;
  return {
    html: "",
    set innerHTML(html){
      if (vibe) assert.notEqual(row.parentNode, vibe, "detach the live checkbox before replacing its row");
      this.html = html;
      vibe = html.includes('data-agent="vibe"') ? {
        appendChild(child){ child.parentNode = this; this.child = child; },
      } : null;
    },
    querySelector(selector){
      assert.equal(selector, '[data-agent="vibe"]');
      return vibe;
    },
  };
}

test("the same one-shot checkbox lives under the Mistral Vibe row across renders", () => {
  const box = { checked: true, disabled: false };
  const row = {
    hidden: true,
    parentNode: { staticHost: true },
    remove(){ this.parentNode = null; },
  };
  const host = makeHost(row);
  const rows = [{ agent: "vibe", present: true, launchable: true, installed: "2.25.0", has_source: true }];
  updateVibeInspectControl(rows, row, box);
  mountHarnessRows(host, harnessRowsHTML(rows, null), row);
  assert.equal(row.hidden, false);
  assert.match(host.html, /Mistral Vibe/);
  assert.equal(host.querySelector('[data-agent="vibe"]').child, row);

  box.disabled = true;
  mountHarnessRows(host, harnessRowsHTML(rows, null), row);
  assert.equal(host.querySelector('[data-agent="vibe"]').child, row);
  assert.equal(box.checked, true, "a settings rerender does not consume the choice");
  assert.equal(box.disabled, true, "a rerender cannot re-enable an in-flight choice");

  updateVibeInspectControl([{ agent: "vibe", present: false }], row, box);
  mountHarnessRows(host, harnessRowsHTML([{ agent: "vibe", present: false }], null), row);
  assert.equal(row.hidden, true);
  assert.equal(box.checked, false, "an unavailable Vibe discards the choice");
  assert.equal(row.parentNode, null);
});

test("a freshly loaded app graph can remount the Vibe choice after a handover", async () => {
  const { mountHarnessRows: remount } = await import("../js/app.js?graph=successor");
  const row = { hidden: false, parentNode: null, remove(){ this.parentNode = null; } };
  const host = makeHost(row);
  remount(host, harnessRowsHTML([{ agent: "vibe", present: true, launchable: true }], null), row);
  assert.equal(host.querySelector('[data-agent="vibe"]').child, row);
});
