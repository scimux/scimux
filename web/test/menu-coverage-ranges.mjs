import assert from 'node:assert/strict';

/* Map complete unchanged V8 ranges through the diff's retained lines. Offsets
   are UTF-16 string offsets, matching V8, and range ends are exclusive. An
   edit inside a range makes that range changed; unchanged children of that
   edited function are still mapped and audited independently. */
export function retainedExecutableCoverage(before, after, oldText, newText, mapping) {
  const starts = text => {
    const lines = text.split(/(?<=\n)/), offsets = []; let offset = 0;
    for (const line of lines) { offsets.push(offset); offset += line.length; }
    return { lines, offsets };
  };
  const old = starts(oldText), next = starts(newText), spans = [];
  for (const [oldLine, newLine] of mapping) {
    // split('\n') used by the caller has an extra empty line after final LF.
    if (oldLine > old.lines.length) continue;
    assert.equal(old.lines[oldLine - 1], next.lines[newLine - 1], 'invalid unchanged source mapping');
    const start = old.offsets[oldLine - 1], end = start + old.lines[oldLine - 1].length;
    const target = next.offsets[newLine - 1], last = spans.at(-1);
    if (last && last.end === start && last.target + last.end - last.start === target) last.end = end;
    else spans.push({ start, end, target });
  }
  const translate = range => {
    const span = spans.find(s => s.start <= range.startOffset && s.end >= range.endOffset);
    if (!span) return null;
    return { startOffset: span.target + range.startOffset - span.start, endOffset: span.target + range.endOffset - span.start };
  };
  const roots = after.map(f => f.ranges[0]);
  const branches = after.filter(f => f.isBlockCoverage).flatMap(f => f.ranges);
  const failures = [], proof = [], seen = new Set();
  function retain(range, kind, candidates) {
    if (range.count <= 0) return;
    const mapped = translate(range);
    if (!mapped) return;
    const key = `${kind}:${range.startOffset}:${range.endOffset}`;
    if (seen.has(key)) return;
    seen.add(key);
    const covered = candidates.some(r => r.startOffset === mapped.startOffset && r.endOffset === mapped.endOffset && r.count > 0);
    proof.push({ kind, before: [range.startOffset, range.endOffset], after: [mapped.startOffset, mapped.endOffset], covered });
    if (!covered) failures.push(`lost unchanged covered ${kind} ${range.startOffset}-${range.endOffset} -> ${mapped.startOffset}-${mapped.endOffset}`);
  }
  for (const f of before) {
    retain(f.ranges[0], 'function', roots);
    if (f.isBlockCoverage) for (const range of f.ranges) retain(range, 'branch', branches);
  }
  return { failures, proof };
}
