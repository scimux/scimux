import assert from 'node:assert/strict';

/* Map complete unchanged V8 ranges through the diff's retained lines. Offsets
   are UTF-16 string offsets, matching V8, and range ends are exclusive. An
   edit inside a range makes that range changed; unchanged children of that
   edited function are still mapped and audited independently. */
export function retainedExecutableCoverage(before, after, oldText, newText, mapping, extractions = []) {
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
  // A function is identified by both mapped root boundaries and its name.
  // Names alone (particularly empty names) do not identify nested functions.
  function boundary(offset, end) {
    const span = spans.find(s => end ? s.start < offset && s.end >= offset : s.start <= offset && s.end > offset);
    return span ? span.target + offset - span.start : null;
  }
  function matchingFunctions(original, range, mapped) {
    const root = original.ranges[0];
    const isScript = original.functionName === '' && root.startOffset === 0 && root.endOffset === oldText.length;
    const compatible = f => !isScript || (f.ranges[0].startOffset === 0 && f.ranges[0].endOffset === newText.length);
    const start = boundary(root.startOffset, false), end = boundary(root.endOffset, true);
    if (start !== null && end !== null) {
      const exact = after.filter(f => f.functionName === original.functionName && compatible(f) && f.ranges[0].startOffset === start && f.ranges[0].endOffset === end);
      if (exact.length) return exact;
    }
    const shift = mapped.startOffset - range.startOffset;
    const movedStart = root.startOffset + shift, movedEnd = root.endOffset + shift;
    if (oldText.slice(root.startOffset, root.endOffset) === newText.slice(movedStart, movedEnd))
      return after.filter(f => f.functionName === original.functionName && compatible(f) && f.ranges[0].startOffset === movedStart && f.ranges[0].endOffset === movedEnd);
    return [];
  }
  function innermost(ranges, start, end) {
    const enclosing = ranges.filter(r => r.startOffset <= start && r.endOffset >= end)
      .sort((a, b) => (a.endOffset - a.startOffset) - (b.endOffset - b.startOffset));
    return enclosing[0];
  }
  function retainsBranch(original, candidate, range, mapped) {
    if (!candidate.isBlockCoverage) return false;
    // A parent function's block counter says nothing about a nested function.
    // Treat overlap as ambiguous until a separate function/extraction proof
    // identifies that nested owner, even if the enclosing block is positive.
    if (range !== original.ranges[0] && after.some(f => {
      const child = f.ranges[0], parent = candidate.ranges[0];
      if (!(child.startOffset > parent.startOffset && child.endOffset < parent.endOffset &&
        child.startOffset < mapped.endOffset && child.endOffset > mapped.startOffset)) return false;
      return !before.some(oldChild => {
        const root = oldChild.ranges[0], oldParent = original.ranges[0];
        if (oldChild.functionName !== f.functionName || root.startOffset <= oldParent.startOffset || root.endOffset >= oldParent.endOffset) return false;
        const translated = translate(root);
        if (translated && translated.startOffset === child.startOffset && translated.endOffset === child.endOffset) return true;
        // For an edited nested function, exact lexical boundaries alone do
        // not establish coverage. Require every counter in that function and
        // every nested function to be positive; ambiguous or zero counters
        // remain failures instead of inheriting the parent's execution.
        if (boundary(root.startOffset, false) !== child.startOffset || boundary(root.endOffset, true) !== child.endOffset) return false;
        return f.ranges.every(r => r.count > 0) && after.filter(nested => {
          const nestedRoot = nested.ranges[0];
          return nestedRoot.startOffset > child.startOffset && nestedRoot.endOffset < child.endOffset;
        }).every(nested => nested.ranges.every(r => r.count > 0)) && before.filter(nested => {
          const nestedRoot = nested.ranges[0];
          return nestedRoot.startOffset > root.startOffset && nestedRoot.endOffset < root.endOffset && nestedRoot.count > 0;
        }).every(nested => {
          const nestedRoot = nested.ranges[0];
          const start = boundary(nestedRoot.startOffset, false), end = boundary(nestedRoot.endOffset, true);
          return after.some(match => match.functionName === nested.functionName && match.ranges[0].startOffset === start &&
            match.ranges[0].endOffset === end && match.ranges.every(r => r.count > 0));
        });
      });
    })) return false;
    const cuts = new Set([range.startOffset, range.endOffset]);
    for (const r of original.ranges) {
      if (r.startOffset > range.startOffset && r.startOffset < range.endOffset) cuts.add(r.startOffset);
      if (r.endOffset > range.startOffset && r.endOffset < range.endOffset) cuts.add(r.endOffset);
    }
    const shift = mapped.startOffset - range.startOffset;
    for (const r of candidate.ranges) {
      const start = r.startOffset - shift, end = r.endOffset - shift;
      if (start > range.startOffset && start < range.endOffset) cuts.add(start);
      if (end > range.startOffset && end < range.endOffset) cuts.add(end);
    }
    const points = [...cuts].sort((a, b) => a - b);
    return points.slice(0, -1).every((start, i) => {
      const end = points[i + 1];
      const prior = innermost(original.ranges, start, end);
      const current = innermost(candidate.ranges, start + shift, end + shift);
      return prior !== range || (current && current.count > 0 &&
        (range === original.ranges[0] || current !== candidate.ranges[0]));
    });
  }
  const failures = [], proof = [], seen = new Set();
  function retain(range, kind, original) {
    if (range.count <= 0) return;
    const mapped = translate(range);
    if (!mapped) return;
    const root = original.ranges[0];
    const key = `${kind}:${root.startOffset}:${root.endOffset}:${range.startOffset}:${range.endOffset}:${JSON.stringify(original.ranges)}`;
    if (seen.has(key)) return;
    seen.add(key);
    const matches = matchingFunctions(original, range, mapped);
    let extraction = null;
    const scriptRoot = original.functionName === '' && root.startOffset === 0 && root.endOffset === oldText.length;
    const scriptCandidates = after.filter(f => f.functionName === '' && f.ranges[0].startOffset === 0 && f.ranges[0].endOffset === newText.length);
    // V8 can give a top-level anonymous function the same boundaries as its
    // script. A zero counter at those identical boundaries is ambiguous and
    // must never inherit the positive script counter.
    if (scriptRoot && scriptCandidates.some(f => f.ranges[0].count <= 0)) matches.length = 0;
    if (!matches.length && extractions.some(move => move.kind === 'script-root') &&
        scriptRoot &&
        oldText === newText.slice(mapped.startOffset, mapped.endOffset)) {
      const scripts = scriptCandidates;
      const nested = after.filter(f => !(f.functionName === '' && f.ranges[0].startOffset === 0 && f.ranges[0].endOffset === newText.length) &&
        f.ranges[0].startOffset < mapped.endOffset && f.ranges[0].endOffset > mapped.startOffset);
      const preserved = nested.every(f => before.some(prior => {
        const priorRoot = prior.ranges[0], moved = translate(priorRoot), nextRoot = f.ranges[0];
        return !(prior.functionName === '' && priorRoot.startOffset === 0 && priorRoot.endOffset === oldText.length) &&
          prior.functionName === f.functionName && moved && moved.startOffset === nextRoot.startOffset && moved.endOffset === nextRoot.endOffset;
      }));
      if (scripts.length && scripts.every(f => f.ranges[0].count > 0) && preserved) { matches.push(...scripts); extraction = {kind:'script-root'}; }
    }
    if (kind === 'branch' && !matches.some(f => f.ranges[0].startOffset <= mapped.startOffset && f.ranges[0].endOffset >= mapped.endOffset)) {
      for (const move of extractions.filter(m => m.from === original.functionName && m.from && m.to)) {
        const candidates = after.filter(f => f.functionName === move.to && f.ranges[0].startOffset <= mapped.startOffset && f.ranges[0].endOffset >= mapped.endOffset);
        const identities = new Set(candidates.map(f => `${f.ranges[0].startOffset}:${f.ranges[0].endOffset}`));
        if (identities.size === 1 && oldText.slice(range.startOffset, range.endOffset) === newText.slice(mapped.startOffset, mapped.endOffset)) { matches.push(...candidates); extraction = move; }
      }
    }
    const covered = kind === 'function'
      ? matches.some(f => f.ranges[0].count > 0)
      : matches.some(f => retainsBranch(original, f, range, mapped));
    proof.push({ kind, before: [range.startOffset, range.endOffset], after: [mapped.startOffset, mapped.endOffset], covered, extraction });
    if (!covered) failures.push(`lost unchanged covered ${kind} ${range.startOffset}-${range.endOffset} -> ${mapped.startOffset}-${mapped.endOffset}`);
  }
  for (const f of before) {
    retain(f.ranges[0], 'function', f);
    if (f.isBlockCoverage) for (const range of f.ranges) retain(range, 'branch', f);
  }
  return { failures, proof };
}
