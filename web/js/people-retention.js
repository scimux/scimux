/* Retain associated discussion files through the normal scoped download API.
   Requests are bounded and never enter an agent conversation. */
export async function retainPeopleEntries({key, entries, copyInstance, fetchImpl, signal, timeoutMs = 10000}) {
  const result = {available:0, unavailable:0, files:Object.create(null)};
  if (typeof key !== "string" || !/^[0-9a-f]{32}\/[0-9a-f]{32}\/[0-9a-f]{16}\/[0-9a-f]{16}$/.test(key) || !Array.isArray(entries) || typeof fetchImpl !== "function") return result;
  const tasks = [], seen = new Set();
  for (const entry of entries) {
    if (!Number.isSafeInteger(entry?.seq) || entry.seq < 1 || !Array.isArray(entry.attachments) || entry.attachments.length > 16) continue;
    for (const file of entry.attachments) {
      if (!file || typeof file.id !== "string" || !/^[0-9a-f]{16}$/.test(file.id) || !Number.isSafeInteger(file.size) || file.size < 1 || file.size > 4194304) continue;
      const path = `/api/team/subscriptions/${key}/attachments/${entry.seq}/${file.id}${copyInstance ? "?copy_instance=" + encodeURIComponent(copyInstance) : ""}`;
      const association = `${entry.seq}/${file.id}`;
      if (!seen.has(path) && tasks.length < 4096) { seen.add(path); tasks.push({path, association}); result.files[association] = false; }
    }
  }
  if (!tasks.length) return result;
  const controller = new AbortController();
  const cancel = () => controller.abort();
  if (signal?.aborted) cancel();
  else signal?.addEventListener("abort", cancel, {once:true});
  const timer = setTimeout(cancel, timeoutMs);
  const aborted = new Promise(resolve => {
    if (controller.signal.aborted) resolve(null);
    else controller.signal.addEventListener("abort", () => resolve(null), {once:true});
  });
  let next = 0;
  async function worker() {
    while (next < tasks.length) {
      const {path, association} = tasks[next++];
      if (controller.signal.aborted) { result.unavailable++; continue; }
      try {
        const response = await Promise.race([fetchImpl(path, {method:"GET", signal:controller.signal}), aborted]);
        if (!response?.ok) { result.unavailable++; continue; }
        await Promise.race([Promise.resolve(response.body?.cancel()), aborted]);
        if (controller.signal.aborted) { result.unavailable++; continue; }
        result.available++;
        result.files[association] = true;
      } catch { result.unavailable++; }
    }
  }
  try { await Promise.all(Array.from({length:Math.min(4, tasks.length)}, worker)); }
  finally { clearTimeout(timer); signal?.removeEventListener("abort", cancel); }
  return result;
}
