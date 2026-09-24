import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { md } from "../js/format.js";

// The disclosures are pre-rendered with md(PRIVACY.md) so they need no
// JavaScript or network request. Update both copies when the policy changes.
test("About and Contributor policies contain the complete offline policy", () => {
  const policy = readFileSync(new URL("../../PRIVACY.md", import.meta.url), "utf8");
  const index = readFileSync(new URL("../index.html", import.meta.url), "utf8");
  const expected = md(policy);
  for (const id of ["m_privacy", "nc_privacy"]) {
    const section = index.match(new RegExp(`<details id="${id}" class="privacy-policy">([\\s\\S]*?)</details>`))?.[1];
    assert.ok(section, `${id} must offer an independently expandable policy`);
    assert.match(section, /<summary>scimux privacy policy<\/summary>/);
    const body = section.match(/<div class="privacy-policy-body">([\s\S]*)<\/div>/)?.[1];
    assert.equal(body?.trim(), expected, `${id} must match PRIVACY.md, without a network request`);
  }
  const contributor = index.indexOf('id="nc_privacy"');
  assert.ok(contributor > index.indexOf('id="nc_muse_privacy"'));
  assert.ok(contributor < index.indexOf('id="nc_muse_ack"'), "policy is available before acknowledgment");
  const readme = readFileSync(new URL("../../README.md", import.meta.url), "utf8");
  assert.match(readme, /\[scimux privacy policy\]\(PRIVACY\.md\)/);
});
