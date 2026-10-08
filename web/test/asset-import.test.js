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
  assert.doesNotMatch(split.html, /Retry import|data-asset-retry|<button/);
  assert.match(split.html, /File exceeds the import limit/);
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

test("missing filenames preserve dollar replacement patterns literally", () => {
  for (const name of ["a$'b.md", "q$`.md", "price$&.md"]) {
    const ref = `[${encodeURIComponent(name)}](scimux-import:3:0:not_found)`;
    const split = splitImportRefs(`before ${ref} after`, "n1");
    const rendered = chatmod.renderMissingImportText(md(split.clean), split.missing);
    assert.equal(rendered, `<p>before <span class="missingfile">${name.replaceAll("&", "&amp;")}</span> after</p>`);
    assert.equal(split.html, "");
  }
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

test("oversized attachments explain the limit without retry actions", () => {
  for (const externalAllowed of [false, true]) {
    const html = importTileHTML("n", 1, 0, "too_large", '<big&file>.md', false, { externalAllowed });
    assert.match(html, /File exceeds the import limit/);
    assert.match(html, /&lt;big&amp;file&gt;\.md/);
    assert.doesNotMatch(html, /data-asset-retry|<button|<big/);
    const split = splitImportRefs("[big](scimux-import:1:0:too_large)", "n", { externalAllowed });
    assert.match(split.html, /File exceeds the import limit/);
    assert.doesNotMatch(split.html, /data-asset-retry|<button/);
  }
});


const fencedImportsLines = "[notes.md](scimux-import:1:0:not_found)\n[x](scimux-import:2:0:retry_ready)";
const fencedImportsText = `\`\`\`text\n${fencedImportsLines}\n\`\`\``;

function fencedImportBody(result){
  return chatmod.renderMissingImportText(md(result.clean), result.missing) + result.html;
}

test("splitImportRefs fenced: missing and blocked markers remain verbatim", () => {
  const r = splitImportRefs(fencedImportsText, "n1");
  assert.equal(r.clean, fencedImportsText);
  assert.equal(r.html, "");
  assert.deepEqual(r.missing, []);
  assert.doesNotMatch(fencedImportBody(r), /assetblocked|missingfile|attrow/);
  assert.equal(fencedImportBody(r), md(fencedImportsText));
});

test("splitImportRefs fenced: outside occurrences keep blocked and missing rendering", () => {
  const outside = "See [x](scimux-import:2:0:retry_ready) and [notes.md](scimux-import:1:0:not_found).";
  const r = splitImportRefs(`${fencedImportsText}\n${outside}`, "n1");
  const expected = splitImportRefs(outside, "n1");
  assert.equal(r.html, expected.html);
  assert.deepEqual(r.missing, expected.missing);
  assert.equal(fencedImportBody(r), md(fencedImportsText) + fencedImportBody(expected));
  assert.match(fencedImportBody(r), /and <span class="missingfile">notes\.md<\/span>\./);
});

test("splitImportRefs fenced: adjacency separator stops at fence boundaries", () => {
  const a = "[a.md](scimux-import:1:0:not_found)";
  const b = "[b.md](scimux-import:1:1:not_found)";
  const text = `${a}${b}\n\`\`\`text ${a}\n${b}\n\`\`\`\n${a}${b}`;
  const r = splitImportRefs(text, "n1");
  assert.equal(r.missing.length, 4);
  assert.equal(fencedImportBody(r),
    '<p><span class="missingfile">a.md</span> <span class="missingfile">b.md</span></p>' +
    md(`\`\`\`text ${a}\n${b}\n\`\`\``) +
    '<p><span class="missingfile">a.md</span> <span class="missingfile">b.md</span></p>');
  assert.ok(r.clean.includes(`\n\`\`\`text ${a}\n${b}\n\`\`\`\n`));
});

test("splitImportRefs fenced: blank lines remain inside and collapse outside", () => {
  const lines = `${fencedImportsLines}\n\n\n\nend`;
  const fence = `\`\`\`text\n${lines}\n\`\`\``;
  const r = splitImportRefs(`before\n\n\n\n${fence}\n\n\n\nafter`, "n1");
  assert.equal(r.clean, `before\n\n${fence}\n\nafter`);
  assert.equal(fencedImportBody(r), md(`before\n\n${fence}\n\nafter`));
});

test("splitImportRefs fenced: unclosed fence preserves its trailing bytes", () => {
  const text = `  \`\`\`text\n${fencedImportsLines}\n\n`;
  const r = splitImportRefs(text, "n1");
  assert.equal(r.clean, text);
  assert.equal(r.html, "");
  assert.deepEqual(r.missing, []);
  assert.equal(fencedImportBody(r), md(text));
});

test("splitImportRefs fenced: raw CR line breaks work without asset normalization", () => {
  for (const newline of ["\r", "\r\n"]) {
    const text = fencedImportsText.replaceAll("\n", newline);
    const r = splitImportRefs(text, "n1");
    assert.equal(r.clean, text);
    assert.equal(r.html, "");
    assert.deepEqual(r.missing, []);
    assert.equal(fencedImportBody(r), md(fencedImportsText));
  }
});

test("splitImportRefs fenced: combined asset and import render order keeps quoted syntax", () => {
  const fence = `\`\`\`diff\nSee [quoted](scimux-asset:a)\n${fencedImportsLines}\n\`\`\``;
  const text = `${fence}\nSee [report](scimux-asset:a) and [notes.md](scimux-import:1:0:not_found), [x](scimux-import:2:0:retry_ready).`;
  const a = chatmod.splitAssetRefs(text, "n1", { a: { name: "report.md" } });
  const r = splitImportRefs(a.clean, "n1");
  const body = chatmod.renderMissingImportText(
    chatmod.renderMissingImportText(md(r.clean), r.missing), a.labels);
  assert.equal(body, md(fence) + '<p>See report and <span class="missingfile">notes.md</span>, .</p>');
  assert.equal((a.html.match(/<a /g) || []).length, 1);
  assert.equal((r.html.match(/class="assetblocked /g) || []).length, 1);
  assert.equal(a.labels.length, 1);
  assert.equal(r.missing.length, 1);
  assert.notEqual(a.labels[0].token, r.missing[0].token);
});
