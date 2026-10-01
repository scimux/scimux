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
    ["unreadable", "File cannot be read"],
    ["not_regular", "Unsupported filesystem object"],
    ["too_large", "File exceeds the import limit"],
    ["storage", "Storage budget or free-space limit"],
    ["chat_unavailable", "Chat is unavailable"],
  ]);
  for (const [reason, message] of cases) {
    const html = importTileHTML("n", 1, 0, reason, `<${reason}>`, false, { externalAllowed: true });
    assert.match(html, new RegExp(message));
    assert.match(html, /Retry import/);
    assert.doesNotMatch(html, /<unreadable>|<not_regular>/);
  }
});

test("a missing unimported file is escaped inert text without attachment actions", () => {
  const split = splitImportRefs(
    'before [<img src="x">](scimux-import:3:1:not_found) middle [big](scimux-import:3:2:too_large)',
    "n 1",
    { externalAllowed: true },
  );
  assert.equal(split.clean, "before  middle");
  assert.match(split.html, /&lt;img src=&quot;x&quot;&gt;/);
  const rowAt = split.html.indexOf('<div class="attrow');
  const nameAt = split.html.indexOf("&lt;img src=&quot;x&quot;&gt;");
  assert.ok(nameAt >= 0 && rowAt > nameAt, split.html);
  assert.equal(split.html.slice(rowAt).includes("&lt;img src=&quot;x&quot;&gt;"), false);
  const missing = split.html.slice(0, rowAt);
  for (const markup of [
    "<a ", "attthumb", "attfile", "data-asset-preview", "data-asset-retry",
    "data-asset-settings", "download=", "<button", "<img", "Retry import",
    "Open attachment settings",
  ]) {
    assert.equal(missing.includes(markup), false, markup);
  }
  assert.match(split.html.slice(rowAt), /Retry import/);

  const empty = splitImportRefs("[](scimux-import:1:0:not_found)", "n1");
  assert.match(empty.html, />file</);
  assert.doesNotMatch(empty.html, /attrow|Retry import|data-asset|<a |<button/);
});

test("encoded missing filenames with brackets and percent signs remain inert text", () => {
  const split = splitImportRefs(
    "see [report%5D100%25.md](scimux-import:3:0:not_found)", "n1",
  );
  assert.equal(split.clean, "see");
  assert.match(split.html, />report\]100%\.md</);
  assert.doesNotMatch(split.html, /attrow|data-asset|<a |<button|Retry import/);
  assert.doesNotMatch(split.clean + split.html, /scimux-import|%5D|%25/);

  const escaped = splitImportRefs("[x%3Cimg%3E](scimux-import:3:1:not_found)", "n1");
  assert.match(escaped.html, />x&lt;img&gt;</);
  assert.doesNotMatch(escaped.html, /<img/);
  const legacy = splitImportRefs("[100%broken](scimux-import:3:2:not_found)", "n1");
  assert.match(legacy.html, />100%broken</);
});
