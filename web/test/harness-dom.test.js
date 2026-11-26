import test from "node:test";
import assert from "node:assert/strict";
import { harnessRowsHTML, updateVibeInspectControl, updateCopilotInspectControl, placeCopilotRow } from "../js/harness.js";
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
  // Both outcomes of the Vibe mount condition run in this graph: a visible row
  // under a present Vibe row, a hidden row that is not mounted, and a visible
  // row with no Vibe row to sit under.
  const row = { hidden: false, parentNode: null, remove(){ this.parentNode = null; } };
  const host = makeHost(row);
  remount(host, harnessRowsHTML([{ agent: "vibe", present: true, launchable: true }], null), row);
  assert.equal(host.querySelector('[data-agent="vibe"]').child, row);

  const hiddenRow = { hidden: true, parentNode: { staticHost: true }, remove(){ this.parentNode = null; } };
  const hiddenHost = makeHost(hiddenRow);
  remount(hiddenHost, harnessRowsHTML([{ agent: "vibe", present: true, launchable: true }], null), hiddenRow);
  assert.equal(hiddenRow.parentNode, null, "a hidden Vibe choice was left mounted");
  assert.ok(!hiddenHost.querySelector('[data-agent="vibe"]').child, "a hidden row was mounted");

  const orphanRow = { hidden: false, parentNode: null, remove(){ this.parentNode = null; } };
  const orphanHost = makeHost(orphanRow);
  remount(orphanHost, harnessRowsHTML([{ agent: "claude", present: true, launchable: true }], null), orphanRow);
  assert.equal(orphanRow.parentNode, null, "a visible Vibe choice with no Vibe row was mounted");
});

/* A host that holds the Vibe and Copilot rows' inspection controls. Each
 * selector is answered only for the agent it names, so a mount that asks for any
 * other row fails the test. */
function makeMultiHost(){
  const children = {};
  const host = {
    html: "",
    set innerHTML(html){
      for (const k of Object.keys(children)) delete children[k];
      this.html = html;
      for (const agent of ["vibe", "copilot"]){
        if (html.includes(`data-agent="${agent}"`)){
          children[agent] = { agent, child: null, appendChild(c){ c.parentNode = this; this.child = c; } };
        }
      }
    },
    querySelector(selector){
      const m = /^\[data-agent="(vibe|copilot)"\]$/.exec(selector);
      assert.ok(m, "unexpected selector " + selector);
      return children[m[1]] || null;
    },
    children,
  };
  return host;
}

test("a visible Copilot inspection row mounts under the present Copilot row", () => {
  const host = makeMultiHost();
  const copilotRow = { hidden: false, parentNode: null, remove(){ this.parentNode = null; } };
  const vibeRow = { hidden: true, parentNode: null, remove(){ this.parentNode = null; } };
  const rows = [{ agent: "copilot", present: true, launchable: true, installed: "1.0.115", has_source: true }];
  mountHarnessRows(host, harnessRowsHTML(rows, null), vibeRow);
  placeCopilotRow(host, copilotRow);
  assert.equal(host.children.copilot.child, copilotRow);
  assert.equal(host.children.vibe, undefined, "no Vibe row was published");
});

test("a hidden Copilot inspection row is detached and never mounted", () => {
  const host = makeMultiHost();
  const copilotRow = { hidden: true, parentNode: { staticHost: true }, remove(){ this.parentNode = null; } };
  const vibeRow = { hidden: true, parentNode: null, remove(){ this.parentNode = null; } };
  const rows = [{ agent: "copilot", present: true, launchable: true, installed: "1.0.115" }];
  mountHarnessRows(host, harnessRowsHTML(rows, null), vibeRow);
  placeCopilotRow(host, copilotRow);
  assert.equal(copilotRow.parentNode, null, "the stale mount was detached");
  assert.equal(host.children.copilot.child, null, "a hidden row was mounted");
});

test("a visible Copilot row with no Copilot harness in the list is not mounted", () => {
  const host = makeMultiHost();
  const copilotRow = { hidden: false, parentNode: null, remove(){ this.parentNode = null; } };
  const vibeRow = { hidden: true, parentNode: null, remove(){ this.parentNode = null; } };
  const rows = [{ agent: "vibe", present: true, launchable: true, installed: "2.25.0" }];
  mountHarnessRows(host, harnessRowsHTML(rows, null), vibeRow);
  placeCopilotRow(host, copilotRow);
  assert.equal(copilotRow.parentNode, null, "a control with no row to sit under was mounted");
  assert.equal(host.children.copilot, undefined);
});

test("omitting the Copilot row leaves the Vibe-only mount behavior unchanged", () => {
  const host = makeMultiHost();
  const vibeRow = { hidden: false, parentNode: null, remove(){ this.parentNode = null; } };
  const rows = [{ agent: "vibe", present: true, launchable: true, installed: "2.25.0" }];
  mountHarnessRows(host, harnessRowsHTML(rows, null), vibeRow);
  assert.equal(host.children.vibe.child, vibeRow);
  assert.equal(host.children.copilot, undefined);
});

test("rerendering moves the Copilot row without duplicating it", () => {
  const host = makeMultiHost();
  const copilotRow = { hidden: false, parentNode: null, remove(){ this.parentNode = null; } };
  const vibeRow = { hidden: true, parentNode: null, remove(){ this.parentNode = null; } };
  const rows = [{ agent: "copilot", present: true, launchable: true, installed: "1.0.115" }];
  mountHarnessRows(host, harnessRowsHTML(rows, null), vibeRow);
  placeCopilotRow(host, copilotRow);
  mountHarnessRows(host, harnessRowsHTML(rows, null), vibeRow);
  placeCopilotRow(host, copilotRow);
  assert.equal(host.children.copilot.child, copilotRow);
  assert.equal(copilotRow.parentNode, host.children.copilot);
});

test("the Copilot control is shown and unchecked only for a present, launchable copilot", () => {
  const row = { hidden: true };
  const box = { checked: true, disabled: false };
  assert.equal(updateCopilotInspectControl([{ agent: "copilot", present: true, launchable: true }], row, box), true);
  assert.equal(row.hidden, false);
  assert.equal(box.checked, true, "an available control keeps the user's choice");
  assert.equal(updateCopilotInspectControl([{ agent: "copilot", present: false }], row, box), false);
  assert.equal(row.hidden, true);
  assert.equal(box.checked, false, "an absent Copilot discards the choice");
  assert.equal(updateCopilotInspectControl(null, null, null), false, "a missing row and box are not an error");
});

test("placing a missing Copilot row is a no-op for the host", () => {
  const host = makeMultiHost();
  mountHarnessRows(host, harnessRowsHTML([{ agent: "copilot", present: true, launchable: true }], null), { hidden: true, remove(){} });
  placeCopilotRow(host, null);
  placeCopilotRow(host, undefined);
  assert.ok(!host.children.copilot.child, "a missing row was mounted");
});
