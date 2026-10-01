import test from "node:test";
import assert from "node:assert/strict";
import { importTileHTML, splitImportRefs } from "../js/chat.js";
import * as chatmod from "../js/chat.js";
import { md } from "../js/format.js";

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
  const missing = chatmod.renderMissingImportText(md(split.clean), split.missing);
  assert.match(missing, /before <span class="missingfile">&lt;img src=&quot;x&quot;&gt;<\/span> middle/);
  for (const markup of [
    "<a ", "attthumb", "attfile", "data-asset-preview", "data-asset-retry",
    "data-asset-settings", "download=", "<button", "<img", "Retry import",
    "Open attachment settings",
  ]) {
    assert.equal(missing.includes(markup), false, markup);
  }
  assert.match(split.html, /Retry import/);
  assert.doesNotMatch(split.html, /&lt;img src=&quot;x&quot;&gt;/);

  const empty = splitImportRefs("[](scimux-import:1:0:not_found)", "n1");
  assert.match(chatmod.renderMissingImportText(md(empty.clean), empty.missing), />file</);
  assert.equal(empty.html, "");
  assert.doesNotMatch(empty.html, /attrow|Retry import|data-asset|<a |<button/);
});

test("encoded missing filenames with brackets and percent signs remain inert text", () => {
  const split = splitImportRefs(
    "see [report%5D100%25.md](scimux-import:3:0:not_found)", "n1",
  );
  const rendered = chatmod.renderMissingImportText(md(split.clean), split.missing);
  assert.match(rendered, /see <span class="missingfile">report\]100%\.md<\/span>/);
  assert.doesNotMatch(split.html, /attrow|data-asset|<a |<button|Retry import/);
  assert.doesNotMatch(rendered + split.html, /scimux-import|%5D|%25/);

  const escaped = splitImportRefs("[x%3Cimg%3E](scimux-import:3:1:not_found)", "n1");
  const escapedHTML = chatmod.renderMissingImportText(md(escaped.clean), escaped.missing);
  assert.match(escapedHTML, />x&lt;img&gt;</);
  assert.doesNotMatch(escapedHTML, /<img/);
  const legacy = splitImportRefs("[100%broken](scimux-import:3:2:not_found)", "n1");
  assert.match(chatmod.renderMissingImportText(md(legacy.clean), legacy.missing), />100%broken</);
});

test("a previously missing file can be retried after it appears", () => {
  const split = splitImportRefs("[report](scimux-import:3:0:retry_ready)", "n1");
  assert.match(split.html, /File is now available/);
  assert.match(split.html, /data-asset-retry/);
  assert.equal(split.clean, "");
});

test("missing filenames stay in their sentence and never make a media bubble", () => {
  const split = splitImportRefs(
    "see [a%2Ab.md](scimux-import:1:0:not_found) and [b.md](scimux-import:1:1:not_found) for details",
    "n1",
  );
  const body = chatmod.renderMissingImportText(md(split.clean), split.missing);
  assert.match(body, /see <span class="missingfile">a\*b\.md<\/span> and <span class="missingfile">b\.md<\/span> for details/);
  assert.equal(split.html, "");

  const adjacent = splitImportRefs(
    "[a.md](scimux-import:1:0:not_found)[b.md](scimux-import:1:1:not_found)", "n1",
  );
  assert.match(chatmod.renderMissingImportText(md(adjacent.clean), adjacent.missing),
    /a\.md<\/span> <span class="missingfile">b\.md/);
  assert.equal(adjacent.html, "");

  const collision = splitImportRefs("\uE0000\uE001 [a.md](scimux-import:1:0:not_found)", "n1");
  const collisionHTML = chatmod.renderMissingImportText(md(collision.clean), collision.missing);
  assert.match(collisionHTML, /\uE0000\uE001 <span class="missingfile">a\.md<\/span>/);
  assert.equal(chatmod.renderMissingImportText("unchanged"), "unchanged");
});
