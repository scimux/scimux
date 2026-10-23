import test from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  GO_COVERAGE_KIND,
  blockKey,
  compareEvidence,
  crossRatioFalls,
  evaluateCoverage,
  goTotals,
  loadGoProfile,
  normalizeGoBlocks,
  parseGoCover,
  aggregateGoCopies,
  splitCoverpkgStanzas,
  analyzeRawV8,
  auditChangedRanges,
  executableSpans,
  lineSpans,
  graphsFromRawV8,
  mergeV8Observations,
  rangesOnLines,
  readV8Directory,
  v8FunctionRanges,
} from "./claude-interaction-coverage.mjs";

const profile = (rows) => `mode: atomic\n${rows.join("\n")}${rows.length ? "\n" : ""}`;

test("go ratios use cross multiplication and reject negatives and empty denominators", () => {
  assert.equal(crossRatioFalls(1, 2, 2, 4), false);
  assert.equal(crossRatioFalls(1, 2, 1, 3), true);
  assert.equal(crossRatioFalls(-1, 2, 1, 2), true);
  assert.equal(crossRatioFalls(0, 0, 0, 0), false);
  assert.equal(crossRatioFalls(0, 0, 0, 1), true);
  assert.equal(crossRatioFalls(1, 0, 0, 0), true);
  assert.equal(GO_COVERAGE_KIND, "go-statement-blocks");
});

test("go profiles parse statement blocks and refuse a damaged profile", () => {
  const blocks = parseGoCover(profile([
    "github.com/scimux/scimux/internal/app/a.go:1.1,2.2 3 1",
    "github.com/scimux/scimux/internal/app/a.go:3.1,4.2 1 0",
    "",
  ]));
  assert.equal(blocks.length, 2);
  assert.equal(blocks[0].count, 1);
  assert.equal(profile([]), "mode: atomic\n");
  assert.throws(() => parseGoCover(), /mode line/);
  assert.throws(() => parseGoCover("not a profile\n"), /mode line/);
  assert.throws(() => parseGoCover("mode: set\nbad line\n"), /unreadable/);
  const totals = goTotals(blocks.concat([{
    file: "solo.go", startLine: 1, startCol: 1, endLine: 1, endCol: 2,
    statements: 2, count: 2,
  }]));
  assert.equal(totals.kind, GO_COVERAGE_KIND);
  assert.equal(totals.num, 5);
  assert.equal(totals.den, 6);
  assert.equal(totals.byPkg.get(".").num, 2);
  assert.equal(normalizeGoBlocks(blocks)[0].file, blocks[0].file);
  const normalized = normalizeGoBlocks(blocks, ["", "/tmp/checkout/", "github.com/scimux/scimux/"]);
  assert.equal(normalized[0].file, "internal/app/a.go");
  assert.equal(blockKey(normalized[0]), "internal/app/a.go:1.1,2.2");
});

test("each coverage gate fails on its own synthetic evidence", () => {
  const covered = {
    file: "internal/app/old.go", startLine: 1, startCol: 1, endLine: 2, endCol: 2,
    statements: 1, count: 1,
  };
  const uncoveredNew = {
    file: "internal/app/new.go", startLine: 1, startCol: 1, endLine: 1, endCol: 5,
    statements: 1, count: 0,
  };
  const base = { go: [covered] };
  const after = { go: [{ ...covered, count: 0 }, uncoveredNew] };
  const gates = [
    [{}, /missing baseline or after evidence/],
    ["nope", /missing coverage model/],
    [null, /missing coverage model/],
    [{ goKind: "branch", baseline: base, after }, /labeled as branch coverage/],
    [{ baseline: base }, /missing baseline or after evidence/],
    [{ baseline: base, after, discoveredCases: 0 }, /zero discovered test cases/],
    [{ baseline: base, after, missingPackages: ["internal/missing"], missingScripts: ["web/js/gone.js"] }, /absent package internal\/missing/],
    [{ baseline: {}, after: { go: [covered] } }, /missing baseline or after go profile/],
    [{
      baseline: { go: [covered, { ...covered, file: "internal/app/gone.go", statements: 1, count: 1 }] },
      after: { go: [covered] },
    }, /missing go source graph internal\/app\/gone.go/],
    [{
      baseline: { go: [covered] },
      after: { go: [covered, { ...covered, startLine: 9, statements: 1, count: 0 }] },
    }, /go file ratio fell/],
    [{
      baseline: { go: [covered] },
      after: { go: [{ ...covered, file: "other/old.go" }] },
    }, /missing go package internal\/app/],
    [{
      baseline: { go: [covered, { ...covered, startLine: 8, statements: 4, count: 4 }] },
      after: { go: [covered, { ...covered, startLine: 8, statements: 4, count: 0 }] },
    }, /go package ratio fell/],
    [{
      baseline: { go: [covered] },
      after: { go: [{ ...covered, count: 0 }] },
    }, /lost unchanged covered go block/],
    [{
      baseline: { go: [covered] },
      after: { go: [covered, uncoveredNew] },
      newOrChangedGo: new Set([blockKey(uncoveredNew)]),
    }, /uncovered new or changed go block/],
    [{
      baseline: { go: [covered] },
      after: { go: [{ ...covered, count: 0 }, { ...uncoveredNew, count: 1 }] },
      newGoFiles: new Set(["internal/app/new.go", "internal/app/keep.go"]),
      changedGo: new Set([blockKey(covered)]),
      baseline: {
        go: [covered, {
          file: "internal/app/keep.go", startLine: 1, startCol: 1, endLine: 1, endCol: 2,
          statements: 1, count: 1,
        }],
      },
      after: {
        go: [
          { ...covered, count: 0 },
          { ...uncoveredNew, count: 1 },
          {
            file: "internal/app/keep.go", startLine: 1, startCol: 1, endLine: 1, endCol: 2,
            statements: 1, count: 1,
          },
        ],
      },
    }, /new covered go code masks a loss on old code/],
    [{
      baseline: base, after, requireJS: true,
    }, /missing baseline or after javascript coverage/],
    [{
      baseline: { go: [covered], js: [{ id: "file:///a.js", num: 1, den: 1 }] },
      after: { go: [covered] },
      requireJS: true,
    }, /missing baseline or after javascript coverage/],
    [{
      baseline: { go: [covered] },
      after: {},
    }, /missing baseline or after go profile/],
    [{
      baseline: { go: [covered], js: [{ id: "file:///app/old.js", num: 1, den: 1 }] },
      after: { go: [covered], js: [{ id: "file:///other/old.js", num: 1, den: 1 }] },
    }, /renamed javascript graph/],
    [{
      baseline: { go: [covered], js: [{ id: "file:///old.js", num: 1, den: 1 }] },
      after: { go: [covered], js: [] },
    }, /missing javascript source graph/],
    [{
      baseline: { go: [covered], js: [{ id: "file:///a.js", num: 2, den: 2 }] },
      after: { go: [covered], js: [{ id: "file:///a.js", num: 1, den: 2 }] },
    }, /javascript graph ratio fell/],
    [{
      baseline: {
        go: [covered],
        js: [{
          id: "file:///a.js", num: 1, den: 1, text: "function a(){ return 1; }\n",
          mapping: [[1, 1]],
          functions: [{ ranges: [{ startOffset: 0, endOffset: 26, count: 1 }], isBlockCoverage: true }],
        }],
      },
      after: {
        go: [covered],
        js: [{
          id: "file:///a.js", num: 1, den: 1, text: "function a(){ return 1; }\n",
          functions: [{ ranges: [{ startOffset: 0, endOffset: 26, count: 0 }], isBlockCoverage: true }],
        }],
      },
    }, /lost unchanged covered function/],
    [{
      baseline: { go: [covered], js: [] },
      after: {
        go: [covered],
        js: [{ id: "file:///new.js", num: 1, den: 1, uncovered: [{ kind: "branch", start: 1, end: 4 }, { start: 8, end: 9 }], uncoveredBranchOnCoveredLine: true }],
      },
      newOrChangedJS: new Set(["file:///new.js"]),
    }, /uncovered javascript branch on a covered line/],
    [{
      baseline: { go: [covered], js: [{ id: "file:///old.js", num: 2, den: 2 }, { id: "file:///new.js", num: 1, den: 1 }] },
      after: { go: [covered], js: [{ id: "file:///old.js", num: 1, den: 2 }, { id: "file:///new.js", num: 5, den: 5 }] },
      newOrChangedJS: new Set(["file:///new.js"]),
    }, /new covered javascript masks a loss on old code/],
    [{
      baseline: base, after: { go: [covered] }, unexplainedBoundaries: 1,
    }, /unexplained instrumentation-block boundary/],
  ];
  for (let i = 0; i < gates.length; i++) {
    const [model, pattern] = gates[i];
    const result = evaluateCoverage(model);
    assert.equal(result.ok, false, `gate ${i}: ${result.failures.join(" | ")}`);
    assert.ok(result.failures.some(item => pattern.test(item)), `gate ${i}: ${pattern} not in ${result.failures.join(" | ")}`);
    assert.equal(result.kind, GO_COVERAGE_KIND);
  }
  const idle = {
    file: "internal/app/old.go", startLine: 20, startCol: 1, endLine: 20, endCol: 2,
    statements: 1, count: 0,
  };
  const fresh = {
    file: "internal/app/fresh.go", startLine: 1, startCol: 1, endLine: 1, endCol: 2,
    statements: 1, count: 1,
  };
  const allowedRename = evaluateCoverage({
    goKind: GO_COVERAGE_KIND,
    discoveredCases: 4,
    baseline: {
      go: [covered, idle],
      js: [{ id: "file:///old.js", num: 1, den: 1 }, { id: "file:///kept.js", num: 1, den: 1 }],
    },
    after: {
      go: [covered, idle, fresh],
      js: [
        { id: "file:///old.js?v=2", num: 1, den: 1 },
        { id: "file:///kept.js", num: 1, den: 1 },
      ],
    },
    renamedGraphs: { "file:///old.js": "file:///old.js?v=2" },
    newGoFiles: new Set(["internal/app/fresh.go"]),
    newOrChangedGo: new Set([blockKey(fresh)]),
    newOrChangedJS: new Set(["file:///kept.js"]),
    boundaryNotes: ["split block documented"],
  });
  assert.equal(allowedRename.ok, true, allowedRename.failures.join("\n"));
  assert.deepEqual(allowedRename.notes, ["split block documented"]);
});

test("evidence loader fails closed and accepts a normalized pair", () => {
  assert.throws(() => compareEvidence({}), /missing baseline or after/);
  const dir = mkdtempSync(join(tmpdir(), "scimux-cov-"));
  const base = join(dir, "base.cover");
  const after = join(dir, "after.cover");
  writeFileSync(base, "mode: atomic\n");
  writeFileSync(after, "mode: atomic\n");
  const empty = compareEvidence({ baselineGo: base, afterGo: after });
  assert.equal(empty.ok, false);
  assert.ok(empty.failures.some(item => /zero discovered/.test(item)));
  const root = "/tmp/scimux-src/";
  const row = `${root}internal/app/old.go:1.1,2.2 1 1`;
  writeFileSync(base, profile([row]));
  writeFileSync(after, profile([row]));
  const loaded = loadGoProfile(base, [root]);
  assert.equal(loaded[0].file, "internal/app/old.go");
  const ok = compareEvidence({
    baselineGo: base,
    afterGo: after,
    roots: [root],
    discoveredCases: 3,
    baselineJS: [{ id: "file:///a.js", num: 1, den: 1 }],
    afterJS: [{ id: "file:///a.js", num: 1, den: 1 }],
    requireJS: true,
  });
  assert.equal(ok.ok, true, ok.failures.join("\n"));
  const plain = compareEvidence({
    baselineGo: base,
    afterGo: after,
    discoveredCases: 3,
    missingPackages: [],
    missingScripts: [],
    newOrChangedGo: [],
    newGoFiles: [],
    changedGo: [],
    newOrChangedJS: [],
    renamedGraphs: {},
    boundaryNotes: ["kept"],
    unexplainedBoundaries: 1,
  });
  assert.equal(plain.ok, true, plain.failures.join("\n"));
  assert.deepEqual(plain.notes, ["kept"]);
  writeFileSync(base, "mode: set\nbad\n");
  assert.throws(() => compareEvidence({ baselineGo: base, afterGo: after, discoveredCases: 1 }));
  assert.throws(() => loadGoProfile(join(dir, "missing.cover"), []));
});

test("v8 directory reads array and result documents and skips non-files", () => {
  const dir = mkdtempSync(join(tmpdir(), "scimux-v8-"));
  writeFileSync(join(dir, "a.json"), JSON.stringify([{ url: "file:///a.js", functions: [{ ranges: [{ startOffset: 0, endOffset: 2, count: 1 }], isBlockCoverage: false }] }]));
  writeFileSync(join(dir, "b.json"), JSON.stringify({ result: [{ url: "file:///b.js", functions: [] }] }));
  mkdirSync(join(dir, "skip.json"));
  const merged = readV8Directory(dir);
  assert.equal(merged.length, 2);
  const ranges = v8FunctionRanges(merged);
  assert.equal(ranges[0].functions[0].ranges[0].count, 1);
  assert.equal(ranges[0].functions[0].isBlockCoverage, false);
  assert.equal(ranges[1].id, "file:///b.js");
  assert.deepEqual(v8FunctionRanges([{ url: "file:///c.js" }])[0].functions, []);
  assert.deepEqual(v8FunctionRanges(null), []);
  const bare = v8FunctionRanges([
    { functions: [{}] },
    { url: "file:///t.js", functions: [{ ranges: [{ startOffset: 0, endOffset: 1, count: 1 }], isBlockCoverage: true }] },
  ]);
  assert.equal(bare[0].id, "");
  assert.deepEqual(bare[0].functions[0].ranges, []);
  assert.equal(bare[1].functions[0].isBlockCoverage, true);
  const empty = mkdtempSync(join(tmpdir(), "scimux-v8-empty-"));
  assert.throws(() => readV8Directory(empty), /no v8 coverage/);
  const blank = mkdtempSync(join(tmpdir(), "scimux-v8-blank-"));
  writeFileSync(join(blank, "c.json"), JSON.stringify({ result: [] }));
  assert.throws(() => readV8Directory(blank), /empty v8 coverage/);
  const odd = mkdtempSync(join(tmpdir(), "scimux-v8-odd-"));
  writeFileSync(join(odd, "d.json"), JSON.stringify({ nope: true }));
  assert.throws(() => readV8Directory(odd), /empty v8 coverage/);
});

test("raw V8 keeps an unexecuted nested branch uncovered", () => {
  const coldChild = {
    isBlockCoverage: true,
    ranges: [
      { startOffset: 0, endOffset: 10, count: 1 },
      { startOffset: 3, endOffset: 7, count: 0 },
    ],
  };
  const analysis = analyzeRawV8([coldChild]);
  assert.deepEqual(analysis.uncovered, [{ kind: "branch", start: 3, end: 7 }]);
  assert.equal(analysis.uncoveredBranchOnCoveredLine, true);
  assert.equal(analysis.alternateBoundaries, 0);

  const split = analyzeRawV8([{
    isBlockCoverage: true,
    ranges: [
      { startOffset: 0, endOffset: 10, count: 0 },
      { startOffset: 0, endOffset: 5, count: 1 },
      { startOffset: 5, endOffset: 10, count: 2 },
    ],
  }]);
  assert.deepEqual(split.uncovered, []);
  assert.equal(split.alternateBoundaries, 1);
  assert.equal(split.uncoveredBranchOnCoveredLine, false);

  const covered = {
    file: "internal/app/old.go", startLine: 1, startCol: 1, endLine: 2, endCol: 2,
    statements: 1, count: 1,
  };
  const graphs = graphsFromRawV8([{ url: "file:///new.js", functions: [coldChild] }]);
  const result = evaluateCoverage({
    goKind: GO_COVERAGE_KIND,
    discoveredCases: 1,
    baseline: { go: [covered], js: [] },
    after: { go: [covered], js: graphs },
    newOrChangedJS: new Set(["file:///new.js"]),
  });
  assert.equal(result.ok, false, result.failures.join(" | "));
  assert.ok(result.failures.some(item => /uncovered/.test(item)), result.failures.join(" | "));

  const explained = evaluateCoverage({
    goKind: GO_COVERAGE_KIND,
    discoveredCases: 1,
    baseline: { go: [covered], js: [] },
    after: {
      go: [covered],
      js: graphsFromRawV8([{
        url: "file:///split.js",
        functions: [{
          ranges: [
            { startOffset: 0, endOffset: 10, count: 0 },
            { startOffset: 0, endOffset: 5, count: 1 },
            { startOffset: 1, endOffset: 2, count: 1 },
            { startOffset: 5, endOffset: 10, count: 2 },
          ],
        }],
      }]),
    },
    newOrChangedJS: new Set(["file:///split.js"]),
  });
  assert.equal(explained.ok, true, explained.failures.join(" | "));

  const twin = analyzeRawV8([{
    ranges: [
      { startOffset: 0, endOffset: 10, count: 0 },
      { startOffset: 0, endOffset: 10, count: 4 },
    ],
  }]);
  assert.deepEqual(twin.uncovered, []);
  assert.equal(twin.alternateBoundaries, 1);
  assert.equal(twin.num, 2);
  assert.equal(twin.den, 2);

  const root = analyzeRawV8([{ ranges: [{ startOffset: 0, endOffset: 4, count: 0 }] }]);
  assert.deepEqual(root.uncovered, [{ kind: "function", start: 0, end: 4 }]);
  assert.equal(root.uncoveredBranchOnCoveredLine, false);

  const gap = analyzeRawV8([{
    ranges: [
      { startOffset: 0, endOffset: 10, count: 0 },
      { startOffset: 0, endOffset: 4, count: 1 },
      { startOffset: 5, endOffset: 10, count: 1 },
    ],
  }]);
  assert.deepEqual(gap.uncovered, [{ kind: "function", start: 0, end: 10 }]);

  const short = analyzeRawV8([{
    ranges: [
      { startOffset: 0, endOffset: 10, count: 0 },
      { startOffset: 1, endOffset: 5, count: 1 },
      { startOffset: 5, endOffset: 10, count: 1 },
    ],
  }]);
  assert.equal(short.uncovered[0].kind, "function");
  const early = analyzeRawV8([{
    ranges: [
      { startOffset: 0, endOffset: 10, count: 0 },
      { startOffset: 0, endOffset: 4, count: 1 },
      { startOffset: 4, endOffset: 8, count: 1 },
    ],
  }]);
  assert.equal(early.uncovered.length, 1);
  const oneArm = analyzeRawV8([{
    ranges: [
      { startOffset: 0, endOffset: 10, count: 0 },
      { startOffset: 0, endOffset: 10, count: 0 },
      { startOffset: 0, endOffset: 5, count: 1 },
    ],
  }]);
  assert.ok(oneArm.uncovered.some(item => item.kind === "function"));
  const coldArm = analyzeRawV8([{
    ranges: [
      { startOffset: 0, endOffset: 10, count: 0 },
      { startOffset: 0, endOffset: 5, count: 1 },
      { startOffset: 5, endOffset: 10, count: 1 },
      { startOffset: 6, endOffset: 8, count: 0 },
    ],
  }]);
  assert.ok(coldArm.uncovered.some(item => item.start === 6 && item.end === 8 && item.kind === "branch"));
  assert.equal(coldArm.uncoveredBranchOnCoveredLine, true);
  const bareChild = analyzeRawV8([{
    ranges: [
      { startOffset: 0, endOffset: 10, count: 0 },
      { startOffset: 3, endOffset: 7, count: 0 },
    ],
  }]);
  assert.equal(bareChild.uncoveredBranchOnCoveredLine, false);
  assert.deepEqual(analyzeRawV8(null), {
    num: 0, den: 0, uncovered: [], uncoveredBranchOnCoveredLine: false, alternateBoundaries: 0,
  });
  assert.equal(analyzeRawV8([{}, { ranges: "nope" }, { ranges: [null, { count: "2", startOffset: "0", endOffset: "1" }] }]).den, 2);
  assert.deepEqual(graphsFromRawV8(null), []);
  const emptyGraph = graphsFromRawV8([{ url: "file:///empty.js" }]);
  assert.equal(emptyGraph[0].den, 1);
  assert.equal(emptyGraph[0].id, "file:///empty.js");
  const tiedArms = analyzeRawV8([{
    ranges: [
      { startOffset: 0, endOffset: 10, count: 0 },
      { startOffset: 0, endOffset: 5, count: 1 },
      { startOffset: 0, endOffset: 5, count: 1 },
    ],
  }]);
  assert.equal(tiedArms.alternateBoundaries, 0);
  assert.ok(tiedArms.uncovered.some(item => item.kind === "function" && item.start === 0 && item.end === 10));
  const nameless = graphsFromRawV8([{ functions: [] }]);
  assert.equal(nameless[0].id, "");
  assert.equal(nameless[0].den, 1);
});

test("v8 observations merge by url and do not cover a cold child", () => {
  const merged = mergeV8Observations([
    {
      url: "file:///chat.js",
      functions: [{
        functionName: "render",
        isBlockCoverage: false,
        ranges: [
          { startOffset: 0, endOffset: 10, count: 1 },
          { startOffset: 3, endOffset: 7, count: 0 },
        ],
      }],
    },
    {
      url: "file:///chat.js",
      functions: [{
        functionName: "render",
        isBlockCoverage: true,
        ranges: [
          { startOffset: 0, endOffset: 10, count: 4 },
          { startOffset: 3, endOffset: 7, count: 0 },
        ],
      }],
    },
    {
      url: "file:///other.js",
      functions: [{
        functionName: "other",
        ranges: [{ startOffset: 0, endOffset: 2, count: 0 }],
      }],
    },
  ]);
  assert.equal(merged.length, 2);
  const chat = merged.find(item => item.url === "file:///chat.js");
  assert.equal(chat.functions.length, 1);
  assert.equal(chat.functions[0].isBlockCoverage, true);
  assert.equal(chat.functions[0].ranges[0].count, 4);
  assert.equal(chat.functions[0].ranges[1].count, 0);
  const analysis = analyzeRawV8(chat.functions);
  assert.equal(analysis.uncoveredBranchOnCoveredLine, true);
  assert.ok(analysis.uncovered.some(item => item.kind === "branch" && item.start === 3 && item.end === 7));

  const lower = mergeV8Observations([
    { url: "file:///a.js", functions: [{ functionName: "f", isBlockCoverage: true, ranges: [{ startOffset: 1, endOffset: 2, count: 9 }] }] },
    { url: "file:///a.js", functions: [{ functionName: "f", isBlockCoverage: false, ranges: [{ startOffset: 1, endOffset: 2, count: 1 }] }] },
  ]);
  assert.equal(lower[0].functions[0].ranges[0].count, 9);
  assert.equal(lower[0].functions[0].isBlockCoverage, true);

  const split = mergeV8Observations([
    { url: "file:///a.js", functions: [{ functionName: "f", ranges: [{ startOffset: 0, endOffset: 5, count: 1 }] }] },
    { url: "file:///a.js", functions: [{ functionName: "g", ranges: [{ startOffset: 0, endOffset: 5, count: 1 }] }] },
    { url: "file:///a.js", functions: [{ functionName: "f", ranges: [{ startOffset: 0, endOffset: 8, count: 1 }] }] },
  ]);
  assert.equal(split[0].functions.length, 3);

  assert.deepEqual(mergeV8Observations(null), []);
  assert.deepEqual(mergeV8Observations("nope"), []);
  const loose = mergeV8Observations([
    null,
    { functions: "nope" },
    { url: "file:///b.js" },
    { url: "file:///b.js", functions: [{ ranges: [null, { count: "3", startOffset: "1", endOffset: "4" }] }] },
    { url: "file:///b.js", functions: [{ ranges: [null, { count: "1", startOffset: "1", endOffset: "4" }] }] },
  ]);
  assert.equal(loose.length, 2);
  const blank = loose.find(item => item.url === "");
  assert.equal(blank.functions.length, 0);
  const named = loose.find(item => item.url === "file:///b.js");
  assert.equal(named.functions.length, 1);
  assert.equal(named.functions[0].functionName, "");
  assert.equal(named.functions[0].isBlockCoverage, false);
  assert.equal(named.functions[0].ranges[0].count, 0);
  assert.equal(named.functions[0].ranges[1].count, 3);
  const absent = mergeV8Observations([{ url: "file:///n.js", functions: [null] }]);
  assert.equal(absent[0].functions[0].functionName, "");
  assert.equal(absent[0].functions[0].isBlockCoverage, false);
  assert.deepEqual(absent[0].functions[0].ranges, []);

  const text = "alpha\nbeta\ngamma\n";
  assert.deepEqual(rangesOnLines(text, null, [1]), []);
  assert.deepEqual(rangesOnLines(text, [{ start: 0, end: 5 }], null), []);
  assert.deepEqual(rangesOnLines("", [{ start: 0, end: 1 }], [1]), []);
  assert.deepEqual(rangesOnLines(null, [{ start: 0, end: 1 }], [1]), []);
  const hit = rangesOnLines(text, [
    null,
    { start: 0, end: 5 },
    { start: 6, end: 10 },
    { start: 5, end: 6 },
  ], [2]);
  assert.deepEqual(hit, [{ start: 6, end: 10 }]);
});

test("changed-range audit rejects a positive range that covers only part of a line", () => {
  const text = "unchanged\nlive(); dead();\nunchanged\n";
  const liveAt = text.indexOf("live();");
  const deadAt = text.indexOf("dead();");
  const coarse = { start: 0, end: text.length, count: 0, kind: "branch" };
  const partial = auditChangedRanges(text, [2], [coarse], [
    { start: liveAt, end: liveAt + "live();".length, count: 1 },
  ]);
  assert.equal(partial.explained.length, 0);
  assert.equal(partial.holes.length, 1);
  assert.ok(partial.holes[0].changedLines.includes(2));

  const both = auditChangedRanges(text, [2], [coarse], [
    { start: liveAt, end: liveAt + "live();".length, count: 1 },
    { start: deadAt, end: deadAt + "dead();".length, count: 1 },
  ]);
  assert.equal(both.holes.length, 0);
  assert.equal(both.explained.length, 1);

  const split = auditChangedRanges(text, [2], [coarse], [
    { start: liveAt, end: liveAt + 3, count: 1 },
    { start: liveAt + 3, end: liveAt + "live();".length, count: 1 },
    { start: deadAt, end: deadAt + 2, count: 1 },
    { start: deadAt + 2, end: deadAt + "dead();".length, count: 1 },
  ]);
  assert.equal(split.holes.length, 0);
  assert.equal(split.explained.length, 1);

  const gap = "unchanged\nlive(); /* gap */ dead();\nunchanged\n";
  const gapLive = gap.indexOf("live();");
  const gapDead = gap.indexOf("dead();");
  const gapAudit = auditChangedRanges(gap, [2], [{ start: 0, end: gap.length, count: 0 }], [
    { start: gapLive, end: gapLive + "live();".length, count: 1 },
    { start: gapDead, end: gapDead + "dead();".length, count: 1 },
  ]);
  assert.equal(gapAudit.holes.length, 0);

  const branch = "unchanged\nflag ? left() : right();\nunchanged\n";
  const leftAt = branch.indexOf("left()");
  const rightAt = branch.indexOf("right()");
  const branchPartial = auditChangedRanges(branch, [2], [{ start: 0, end: branch.length, count: 0 }], [
    { start: leftAt, end: leftAt + "left()".length, count: 1 },
  ]);
  assert.equal(branchPartial.holes.length, 1);
  const branchBoth = auditChangedRanges(branch, [2], [{ start: 0, end: branch.length, count: 0 }], [
    { start: branch.indexOf("flag"), end: rightAt + "right();".length, count: 1 },
  ]);
  assert.equal(branchBoth.holes.length, 0);

  const fns = "unchanged\nfunction a(){return 1} function b(){return 2}\nunchanged\n";
  const fnA = fns.indexOf("function a");
  const fnB = fns.indexOf("function b");
  const oneFn = auditChangedRanges(fns, [2], [{ start: 0, end: fns.length, count: 0 }], [
    { start: fnA, end: fnB - 1, count: 1 },
  ]);
  assert.equal(oneFn.holes.length, 1);
  const twoFn = auditChangedRanges(fns, [2], [{ start: 0, end: fns.length, count: 0 }], [
    { start: fnA, end: fnB - 1, count: 1 },
    { start: fnB, end: fns.indexOf("\nunchanged"), count: 1 },
  ]);
  assert.equal(twoFn.holes.length, 0);

  const commentLine = "code();\n  // only a comment\ncode();\n";
  const commentAudit = auditChangedRanges(commentLine, [2], [{ start: 0, end: commentLine.length, count: 0 }], []);
  assert.equal(commentAudit.holes.length, 0);
  assert.equal(commentAudit.explained.length, 1);

  const blank = "code();\n   \ncode();\n";
  const blankAudit = auditChangedRanges(blank, [2], [{ start: 0, end: blank.length, count: 0 }], []);
  assert.equal(blankAudit.explained.length, 1);

  const quoted = "unchanged\nconst s = \"live(); // stay\"; dead();\nunchanged\n";
  const deadQuoted = quoted.indexOf("dead();");
  const quotedPartial = auditChangedRanges(quoted, [2], [{ start: 0, end: quoted.length, count: 0 }], [
    { start: deadQuoted, end: deadQuoted + "dead();".length, count: 1 },
  ]);
  assert.equal(quotedPartial.holes.length, 1);
  const quotedFull = auditChangedRanges(quoted, [2], [{ start: 0, end: quoted.length, count: 0 }], [
    { start: quoted.indexOf("const"), end: deadQuoted, count: 1 },
    { start: deadQuoted, end: deadQuoted + "dead();".length, count: 1 },
  ]);
  assert.equal(quotedFull.holes.length, 0);

  const crossed = "live();\n/* comment\nstill */\ndead();\n";
  const crossedAudit = auditChangedRanges(crossed, [2, 3], [{ start: 0, end: crossed.length, count: 0 }], []);
  assert.equal(crossedAudit.holes.length, 0);

  const templated = "unchanged\nconst s = `a ${function () { return 1; }} b`; // note\nunchanged\n";
  const spans = executableSpans(templated);
  const noteAt = templated.indexOf("// note");
  assert.equal(spans.some(span => noteAt >= span.start && noteAt < span.end), false);
  const spaceAt = templated.indexOf("`a ") + 2;
  assert.equal(spans.some(span => spaceAt >= span.start && spaceAt < span.end), true);
  const whole = auditChangedRanges(templated, [2], [{ start: 0, end: templated.length, count: 0 }], [
    { start: templated.indexOf("const"), end: templated.indexOf("// note"), count: 1 },
  ]);
  assert.equal(whole.holes.length, 0);
});

test("regexes after control conditions cannot hide executable source", () => {
  for (const statement of [
    'if (ok) /[//]/.test("x"); dead();',
    'while (ok) /[/*]/.test("x"); dead();',
    'for (;ok;) /[//]/.test("x"); dead();',
    'if (ok) /[//]/.test("x");\n/* real comment */\ndead();',
    'value++ / /[//]/.test("x"); dead();',
    'const value = {} / /[//]/.test("x"); dead();',
    'if (ok) /[//]/.test("x"); const value = `\n${dead()}\n`;',
    'if (ok) /[//]/.test("x"); const value = `\n// literal ${dead()}\n`;',
  ]) {
    const text = `unchanged\n${statement}\nunchanged\n`;
    const deadAt = text.indexOf("dead()");
    assert.ok(deadAt >= 0);
    assert.equal(executableSpans(text).some(span => deadAt >= span.start && deadAt < span.end), true, statement);
    const coarse = { start: 0, end: text.length, count: 0 };
    const changed = statement.split("\n").map((_, index) => index + 2);
    const audit = auditChangedRanges(text, changed, [coarse], [
      { start: text.indexOf(statement), end: text.indexOf("/["), count: 1 },
    ]);
    assert.equal(audit.explained.length, 0, statement);
    assert.equal(audit.holes.length, 1, statement);
  }
});

test("a regex literal does not hide the executable code after it", () => {
  const text = "unchanged\nconst r = /[/*]/; dead();\nunchanged\n";
  const deadAt = text.indexOf("dead();");
  const slashStar = text.indexOf("/*");
  assert.ok(deadAt > slashStar);
  assert.equal(executableSpans(text).some(span => deadAt >= span.start && deadAt < span.end), true);
  const coarse = { start: 0, end: text.length, count: 0 };
  const prefix = auditChangedRanges(text, [2], [coarse], [
    { start: text.indexOf("const"), end: slashStar, count: 1 },
  ]);
  assert.equal(prefix.explained.length, 0);
  assert.equal(prefix.holes.length, 1);

  const escaped = "unchanged\nconst r = /\\/\\*/; dead();\nunchanged\n";
  const escapedDead = escaped.indexOf("dead();");
  assert.equal(executableSpans(escaped).some(span => escapedDead >= span.start && escapedDead < span.end), true);
  const classed = "const r = /[\\/\\*]/; dead();";
  assert.equal(executableSpans(classed).some(span => classed.indexOf("dead();") >= span.start && classed.indexOf("dead();") < span.end), true);
  const divided = "const n = a / b; live();";
  assert.equal(executableSpans(divided).some(span => divided.indexOf("/") >= span.start && divided.indexOf("/") < span.end), true);
  assert.equal(executableSpans(divided).some(span => divided.indexOf("live();") >= span.start && divided.indexOf("live();") < span.end), true);

  const comment = "live();\n/* comment */\ndead();\n";
  const commentAt = comment.indexOf("comment");
  assert.equal(executableSpans(comment).some(span => commentAt >= span.start && commentAt < span.end), false);
  assert.equal(executableSpans(comment).some(span => comment.indexOf("dead();") >= span.start && comment.indexOf("dead();") < span.end), true);
  const line = "live(); // gone\nstay();";
  assert.equal(executableSpans(line).some(span => line.indexOf("gone") >= span.start && line.indexOf("gone") < span.end), false);
  assert.equal(executableSpans(line).some(span => line.indexOf("stay();") >= span.start && line.indexOf("stay();") < span.end), true);

  const open = "unchanged\nconst r = /[/*]; dead();\nunchanged\n";
  const openDead = open.indexOf("dead();");
  assert.equal(executableSpans(open).some(span => openDead >= span.start && openDead < span.end), true);
  const openAudit = auditChangedRanges(open, [2], [{ start: 0, end: open.length, count: 0 }], [
    { start: open.indexOf("const"), end: open.indexOf("/*"), count: 1 },
  ]);
  assert.equal(openAudit.explained.length, 0);
  assert.equal(openAudit.holes.length, 1);

  const shows = (source, marker) => {
    const at = source.indexOf(marker);
    assert.ok(at >= 0, source);
    return executableSpans(source).some(span => at >= span.start && at < span.end);
  };
  for (const source of [
    "return /[/*]/; dead();",
    "return /* keep */ /[/*]/; dead();",
    "} /[/*]/; dead();",
    "{ /[/*]/; dead();",
    "( /[/*]/ ); dead();",
    "x in /[/*]/; dead();",
    "void /[/*]/; dead();",
    "typeof /[/*]/; dead();",
    "12/3; dead();",
    "1e2; dead();",
    "1.2; dead();",
    "1_2; dead();",
    "OK; dead();",
    "a.b; dead();",
    "const r = /a/g; dead();",
    "const r = /a/G; dead();",
    "const r = /a/; dead();",
    "const r = /[]]/; dead();",
    "const r = /[^]]/; dead();",
    "const r = /[^a]/; dead();",
    "const r = /\\n/; dead();",
    "const t = `x ${/[/*]/} y`; dead();",
    "const t = `x ${ { /[/*]/ } } y`; dead();",
    "#/[/*]/; dead();",
    "_x; dead();",
    "$x; dead();",
    "a1; dead();",
  ]) {
    assert.equal(shows(source, "dead();"), true, source);
  }
  assert.equal(shows("const tail = /a", "/a"), true);
  assert.equal(shows("(a)/* hidden */dead();", "hidden"), false);
  assert.equal(shows("(a)/* hidden */dead();", "dead();"), true);
  assert.equal(shows("a[0]/* hidden */dead();", "hidden"), false);
  assert.equal(shows("const t = `a` /* hidden */ dead();", "hidden"), false);
  assert.equal(shows("const t = `a` /* hidden */ dead();", "dead();"), true);
  assert.equal(shows("const s = \"a\" /* hidden */ dead();", "hidden"), false);
  assert.equal(shows("const end = /", "/"), true);
  assert.equal(shows("const nl = /\n dead();", "dead();"), true);
  assert.equal(shows("const mid = /a\n dead();", "dead();"), true);
  assert.equal(shows("const esc = /\\\n dead();", "dead();"), true);
  assert.equal(shows("const bare = /\\", "/\\"), true);
  assert.equal(shows("const cut = /[", "/["), true);
  assert.equal(shows("const neg = /[^", "/[^"), true);
  const spaced = "const r = /a b/; dead();";
  const innerSpace = spaced.indexOf("a b") + 1;
  assert.equal(executableSpans(spaced).some(span => innerSpace >= span.start && innerSpace < span.end), true);
});

test("executable span scanner keeps comments and string boundaries apart", () => {
  assert.deepEqual(executableSpans(""), []);
  assert.deepEqual(executableSpans(null), []);
  const plain = executableSpans("a/b");
  assert.deepEqual(plain, [{ start: 0, end: 3 }]);
  const line = executableSpans("keep // gone\nstay");
  assert.equal(line.some(span => span.start === 0 && span.end === 4), true);
  assert.equal(line.some(span => span.start === "keep // gone\n".length), true);
  const trapped = executableSpans("\"unterminated");
  assert.deepEqual(trapped, [{ start: 0, end: "\"unterminated".length }]);
  const escaped = executableSpans("'a\\\\'b'");
  assert.equal(escaped[0].start, 0);
  assert.equal(escaped[0].end, "'a\\\\'b'".length);
  const dangling = executableSpans("\"\\");
  assert.deepEqual(dangling, [{ start: 0, end: 2 }]);
  const openBlock = executableSpans("code /* still");
  assert.deepEqual(openBlock, [{ start: 0, end: 4 }]);
});

test("changed-range audit explains a coarse zero only from strict inner coverage", () => {
  const text = "unchanged\nchanged-a\nchanged-b\n";
  const changed = [2, "3", 0, 1.5, -1];
  const coarse = { start: 0, end: 30, count: 0 };
  const explained = auditChangedRanges(text, changed, [null, coarse, { start: 0, end: 10, count: 0 }], [
    null,
    { start: 10, end: 20, count: 4 },
    { start: 20, end: 30, count: 1 },
  ]);
  assert.equal(explained.holes.length, 0);
  assert.deepEqual(explained.explained, [{ start: 0, end: 30, changedLines: [2, 3] }]);

  const onlyChanged = auditChangedRanges(text, [2, 3], [{ start: 10, end: 30, count: 0 }], [
    { start: 10, end: 20, count: 4 },
    { start: 20, end: 30, count: 2 },
  ]);
  assert.equal(onlyChanged.explained.length, 0);
  assert.equal(onlyChanged.holes[0].reason, "uncovered range is only changed lines");

  const coldInner = auditChangedRanges(text, [2, 3], [coarse], [
    { start: 10, end: 20, count: 0 },
    { start: 20, end: 30, count: 2 },
  ]);
  assert.equal(coldInner.explained.length, 0);
  assert.equal(coldInner.holes[0].reason, "changed line lacks its own positive range");

  const parentOnly = auditChangedRanges(text, [2], [coarse], [
    { start: 0, end: 30, count: 9 },
  ]);
  assert.equal(parentOnly.explained.length, 0);
  assert.equal(parentOnly.holes[0].reason, "changed line lacks its own positive range");

  const insetParent = auditChangedRanges(text, [2, 3], [coarse], [
    { start: 1, end: 29, count: 5 },
    { start: 10, end: 20, count: 0 },
    { start: 20, end: 30, count: 2 },
  ]);
  assert.equal(insetParent.explained.length, 0);
  assert.equal(insetParent.holes[0].reason, "changed line lacks its own positive range");
  assert.ok(insetParent.holes[0].changedLines.includes(2));

  const elsewhere = auditChangedRanges(text, [2], [{ start: 0, end: 10, count: 0 }], []);
  assert.deepEqual(elsewhere, { explained: [], holes: [] });

  for (const bad of [null, ["nope"], [text, null, [], []], [text, [], null, []], [text, [], [], null]]) {
    assert.throws(() => auditChangedRanges(...(bad || [])), /auditChangedRanges/);
  }
  assert.throws(() => auditChangedRanges(), /auditChangedRanges/);
});

test("coverpkg copies aggregate by sum and max and reject a mismatched block", () => {
  const one = { file: "a.go", startLine: 1, startCol: 2, endLine: 3, endCol: 4, statements: 2, count: 0 };
  const two = { ...one, count: 5 };
  const other = { file: "a.go", startLine: 8, startCol: 1, endLine: 9, endCol: 2, statements: 1, count: 1 };
  const aggregated = aggregateGoCopies([one, two, other, { ...other, count: 1 }]);
  assert.equal(aggregated.length, 2);
  assert.deepEqual(aggregated[0], { ...one, copies: 2, sum: 5, max: 5 });
  assert.equal(aggregated[1].copies, 2);
  assert.equal(aggregated[1].sum, 2);
  assert.equal(aggregated[1].max, 1);
  for (const bad of [null, [], [null], [{ ...one, count: -1 }], [{ ...one, statements: 1 }, { ...one, statements: 2 }]]) {
    assert.throws(() => aggregateGoCopies(bad), /aggregateGoCopies/);
  }
});

test("missing text and a zero-length range do not cover a changed line", () => {
  assert.deepEqual(lineSpans(undefined), lineSpans(""));
  assert.deepEqual(lineSpans(null), lineSpans(""));
  assert.equal(lineSpans("").length, 1);
  assert.equal(lineSpans("")[0].start, 0);
  assert.equal(lineSpans("")[0].end, 0);
  const text = "unchanged\nchanged();\nunchanged\n";
  const missing = auditChangedRanges(text, [2], [{ start: "nope", end: "nope", count: 0 }], []);
  assert.deepEqual(missing, { explained: [], holes: [] });
  const zeroEnd = auditChangedRanges(text, [2], [{ start: 0, end: 0, count: 0 }], []);
  assert.deepEqual(zeroEnd, { explained: [], holes: [] });
  const blankEnd = auditChangedRanges(text, [2], [{ start: 0, count: 0 }], []);
  assert.deepEqual(blankEnd, { explained: [], holes: [] });
});

test("coverpkg stanzas split on a repeated block and reject empty input", () => {
  const first = { file: "a.go", startLine: 1, startCol: 1, endLine: 2, endCol: 1, statements: 1, count: 0 };
  const second = { file: "b.go", startLine: 3, startCol: 1, endLine: 4, endCol: 1, statements: 1, count: 4 };
  const stanzas = splitCoverpkgStanzas([first, second, { ...first, count: 2 }, { ...second, count: 0 }]);
  assert.equal(stanzas.length, 2);
  assert.equal(stanzas[0][0].count, 0);
  assert.equal(stanzas[1][0].count, 2);
  assert.equal(stanzas[1][1].count, 0);
  assert.equal(splitCoverpkgStanzas([first]).length, 1);
  for (const bad of [null, [], [null]]) {
    assert.throws(() => splitCoverpkgStanzas(bad), /splitCoverpkgStanzas/);
  }
});
