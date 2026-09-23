import test from "node:test";
import assert from "node:assert/strict";
import { importTileHTML, splitImportRefs } from "../js/chat.js";

test("blocked attachment references explain policy and expose explicit actions", () => {
  const off = importTileHTML("n 1", 7, 2, "outside_workspace", "chart", true, {
    externalAllowed: false,
  });
  assert.match(off, /Attachment outside workspace/);
  assert.match(off, /External attachments are disabled/);
  assert.match(off, /data-asset-settings/);
  assert.doesNotMatch(off, /data-asset-retry/);

  const on = importTileHTML("n 1", 7, 2, "outside_workspace", "chart", true, {
    externalAllowed: true,
  });
  assert.match(on, /data-asset-retry/);
  assert.match(on, /data-turn="7"/);
  assert.match(on, /data-occurrence="2"/);

  const split = splitImportRefs(
    "before ![chart](scimux-import:7:2:outside_workspace) after",
    "n 1",
    { externalAllowed: true },
  );
  assert.equal(split.clean, "before  after");
  assert.match(split.html, /Retry import/);
});

test("blocked reasons are distinct and escaped", () => {
  const cases = new Map([
    ["not_found", "File no longer exists"],
    ["unreadable", "File cannot be read"],
    ["not_regular", "Unsupported filesystem object"],
    ["too_large", "File exceeds the import limit"],
    ["storage", "Storage budget or free-space limit"],
    ["chat_unavailable", "Chat is unavailable"],
  ]);
  for (const [reason, message] of cases) {
    const html = importTileHTML("n", 1, 0, reason, `<${reason}>`, false, { externalAllowed: true });
    assert.match(html, new RegExp(message));
    assert.doesNotMatch(html, /<not_found>|<unreadable>|<not_regular>/);
  }
});
