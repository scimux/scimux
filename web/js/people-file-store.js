import { parsePeoplePendingSnapshot } from "./people-local-review.js";

/* Durable People attachment drafts. The driver serializes read/write updates;
   stored bytes never enter agent context without an explicit owner action. */
const FILE_MAX = 4 * 1024 * 1024;
const SCOPE_MAX = 16 * 1024 * 1024;
const GLOBAL_MAX = 256 * 1024 * 1024;
const GLOBAL_COUNT = 4096;

function validScope(scope) { return typeof scope === "string" && scope.length > 0 && scope.length <= 512; }
function validID(id) { return typeof id === "string" && /^[0-9a-f]{16}$/.test(id); }
function validRevision(revision) { return typeof revision === "string" && /^[0-9a-f]{64}$/.test(revision); }
function validFileToken(token) { return typeof token === "string" && /^[0-9a-f-]{36}$/.test(token); }
function newFileToken() {
  try {
    const bytes = new Uint8Array(16);
    globalThis.crypto.getRandomValues(bytes);
    const hex = Array.from(bytes, value => value.toString(16).padStart(2, "0")).join("");
    return `${hex.slice(0,8)}-${hex.slice(8,12)}-${hex.slice(12,16)}-${hex.slice(16,20)}-${hex.slice(20)}`;
  } catch { throw new Error("People file identity is unavailable."); }
}
function validFile(file) {
  return !!file && typeof file.name === "string" && !!file.name && file.name !== "." && file.name !== ".." &&
    !/[\\/\x00-\x1f\x7f]/.test(file.name) && new TextEncoder().encode(file.name).length <= 255 &&
    typeof file.type === "string" && file.type.length <= 255 && Number.isSafeInteger(file.size) && file.size >= 1 && file.size <= FILE_MAX;
}
function validMetadata(metadata) {
  if (!metadata || typeof metadata !== "object" || !validID(metadata.sourceFileID)) return false;
  const source = metadata.source;
  if (!source || !/^[0-9a-f]{32}$/.test(source.server || "") || !/^[0-9a-f]{32}$/.test(source.owner || "") || !/^[0-9a-f]{16}$/.test(source.chat || "") || !/^[0-9a-f]{16}$/.test(source.generation || "") || source.log !== "comments" || !Number.isSafeInteger(source.seq) || source.seq < 1) return false;
  try { return JSON.stringify(metadata).length <= 4096; } catch { return false; }
}
function validRow(row) {
  return !!row && validScope(row.scope) && validID(row.id) && row.key === row.scope + "\0" + row.id && validFile(row.file) && (row.metadata === undefined || validMetadata(row.metadata)) && (row.fileToken === undefined || validFileToken(row.fileToken)) && (row.stageRevision === undefined || validRevision(row.stageRevision)) && (row.published === undefined || (Number.isSafeInteger(row.published) && row.published > 0));
}
function checkedRows(rows) {
  if (!Array.isArray(rows) || rows.some(row => !validRow(row))) throw new Error("People file recovery is unavailable.");
  return rows;
}
function peopleStoredBytes(files, recovery) {
  const encoder = new TextEncoder();
  let metadata = 0, fileBytes = 0;
  for (const row of recovery) metadata += encoder.encode(row.key).length + encoder.encode(JSON.stringify(row)).length + ((row.kind === "publication" || row.kind === "text-publication") && row.state === "pending" ? 65536 : 0);
  for (const row of checkedRows(files)) {
    fileBytes += row.file.size;
    metadata += encoder.encode(row.key).length + encoder.encode(JSON.stringify({...row, file:{name:row.file.name, type:row.file.type, size:row.file.size}})).length;
  }
  return {metadata, total:metadata + fileBytes};
}
function completedPeopleDrafts(recovery, publication, enabled) {
  const owned = recovery.find(item => item.key === "head:" + publication.head && item.scope === publication.scope && item.version === publication.version);
  const pendingReference = version => recovery.some(item => {
    if (item.key === publication.key || (item.kind !== "publication" && item.kind !== "text-publication") || item.state !== "pending") return false;
    if (item.version === version) return true;
    const head = recovery.find(candidate => candidate.key === "head:" + item.head && candidate.scope === item.scope && candidate.version === item.version);
    return head?.sourceVersion === version && head.scope === publication.scope;
  });
  const removable = head => !!head && !pendingReference(head.version) && !recovery.some(item => item.kind === "head" && item.key !== head.key && item.version === head.version);
  const clearOwned = enabled && removable(owned);
  const source = owned && recovery.find(item => item.key === "head:" + owned.sourceHead && item.scope === publication.scope && item.version === owned.sourceVersion);
  const clearSource = clearOwned && !source?.sealed && removable(source);
  const keys = new Set();
  if (clearOwned) { keys.add(owned.key); keys.add("draft:" + owned.version); }
  if (clearSource) { keys.add(source.key); keys.add("draft:" + source.version); }
  return {keys, clearOwned};
}
function peoplePublicationFileWrite(driver, scope, publication, selected, update, removal = false) {
  if (publication === undefined) return driver.readWrite(update);
  if (typeof publication !== "string" || !/^[0-9a-f]{32}:[1-9][0-9]*$/.test(publication) || typeof driver.writePeople !== "function") throw new Error("People publication identity is unavailable.");
  return driver.writePeople(({files, recovery}) => {
    const pending = recovery.find(row => row.key === "publication:" + publication) || (removal ? recovery.find(row => row.key === "decision:" + publication) : null);
    if (!pending || pending.scope !== scope || (pending.state !== "pending" && !(removal && pending.state === "completed"))) throw new Error("People publication is retired or unavailable.");
    for (const item of selected) {
      const captured = pending.files.find(file => file.id === item.id && file.fileToken === item.fileToken);
      const stored = files.find(file => file.scope === scope && file.id === item.id && file.fileToken === item.fileToken);
      if (!captured || !stored || stored.stageRevision !== captured.stageRevision) throw new Error("People file identity conflict.");
      if (removal && (stored.published === undefined || (pending.state === "completed" && stored.published !== pending.seq) || recovery.some(row => row.key !== pending.key && row.kind === "publication" && row.state === "pending" && row.scope === scope && row.files.some(file => file.id === item.id && file.fileToken === item.fileToken)))) throw new Error("People file identity conflict.");
    }
    const next = update(files);
    return {files:next.rows, recovery, value:next.value};
  });
}

export function createPeopleFileStore(driver) {
  if (!driver || typeof driver.readWrite !== "function" || typeof driver.read !== "function") throw new Error("People file storage unavailable.");
  return {
    destroy() { driver.destroy?.(); },
    async savePeopleDraft({scope, text, head, expected, current}) {
      if (!validScope(scope) || typeof text !== "string" || new TextEncoder().encode(text).length > 16384 ||
          (head !== undefined && (typeof head !== "string" || typeof expected !== "string")) || (current !== undefined && typeof current !== "function")) throw new Error("People draft identity is unavailable.");
      if (typeof driver.writePeople !== "function") throw new Error("People recovery is unavailable.");
      return driver.writePeople(({files, recovery}) => {
        if (current !== undefined && current() !== true) throw new Error("People draft operation cancelled.");
        checkedRows(files);
        const allocator = recovery.find(row => row.key === "allocator");
        if (!allocator || !/^[0-9a-f]{32}$/.test(allocator.epoch) || !Number.isSafeInteger(allocator.next) || allocator.next < 1 || allocator.next >= Number.MAX_SAFE_INTEGER) throw new Error("People recovery identity is unavailable.");
        const version = allocator.epoch + ":" + allocator.next, observed = head ? recovery.find(row => row.key === "head:" + head) : null;
        if (observed && observed.scope !== scope) throw new Error("People draft changed; review it again.");
        const previous = observed && observed.version === expected && !observed.sealed ? observed : null;
        const headID = previous ? head : version, headKey = "head:" + headID, draftKey = "draft:" + version;
        if (recovery.some(row => row.key === draftKey) || (!previous && recovery.some(row => row.key === headKey))) throw new Error("People recovery identity conflict.");
        const next = recovery.filter(row => row.key !== "allocator" && row.key !== headKey &&
          !(previous && row.key === "draft:" + previous.version && !recovery.some(item => (item.kind === "publication" || item.kind === "text-publication") && item.state === "pending" && item.version === previous.version)));
        next.push({...allocator, next:allocator.next + 1}, {key:draftKey, kind:"draft", scope, version, text}, {key:headKey, kind:"head", scope, head:headID, version});
        const encoder = new TextEncoder();
        for (const row of next) {
          const size = encoder.encode(row.key).length + encoder.encode(JSON.stringify(row)).length;
          if (size > 65536) throw new Error("People recovery storage limit reached.");
        }
        const bytes = peopleStoredBytes(files, next);
        if (next.length + next.filter(row => (row.kind === "publication" || row.kind === "text-publication") && row.state === "pending").length > 2048 || bytes.metadata > 8 * 1024 * 1024 || bytes.total > GLOBAL_MAX || next.filter(row => row.kind === "head" && row.scope === scope).length > 32) throw new Error("People recovery storage limit reached.");
        return {files, recovery:next, value:{head:headID, version}};
      });
    },
    async freezePeopleDraft({scope, text, head, version, current}) {
      if (!validScope(scope) || typeof text !== "string" || new TextEncoder().encode(text).length > 16384 || typeof head !== "string" || head.length > 512 || typeof version !== "string" || version.length > 512 || (current !== undefined && typeof current !== "function") || typeof driver.writePeople !== "function") throw new Error("People draft identity is unavailable.");
      return driver.writePeople(({files, recovery}) => {
        if (current !== undefined && current() !== true) throw new Error("People draft operation cancelled.");
        const allocator = recovery.find(row => row.key === "allocator");
        if (!allocator || !/^[0-9a-f]{32}$/.test(allocator.epoch) || !Number.isSafeInteger(allocator.next) || allocator.next < 1 || allocator.next >= Number.MAX_SAFE_INTEGER) throw new Error("People recovery identity is unavailable.");
        const id = allocator.epoch + ":" + allocator.next;
        if (recovery.some(row => row.key === "head:" + id || row.key === "draft:" + id)) throw new Error("People recovery identity conflict.");
        const source = recovery.find(row => row.key === "head:" + head && row.scope === scope && row.version === version);
        const sourceDraft = recovery.find(row => row.key === "draft:" + version && row.scope === scope && row.text === text);
        const next = recovery.filter(row => row.key !== "allocator");
        next.push({...allocator,next:allocator.next+1}, {key:"draft:"+id,kind:"draft",scope,version:id,text}, {key:"head:"+id,kind:"head",scope,head:id,version:id,sealed:true,...(source && sourceDraft ? {sourceHead:head,sourceVersion:version} : {})});
        const encoder = new TextEncoder();
        for (const row of next) if (encoder.encode(row.key).length + encoder.encode(JSON.stringify(row)).length > 65536) throw new Error("People recovery storage limit reached.");
        const bytes = peopleStoredBytes(files, next), pending = next.filter(row => (row.kind === "publication" || row.kind === "text-publication") && row.state === "pending");
        if (next.length + pending.length > 2048 || bytes.metadata > 8 * 1024 * 1024 || bytes.total > GLOBAL_MAX || next.filter(row => row.kind === "head" && row.scope === scope).length > 32) throw new Error("People recovery storage limit reached.");
        return {files,recovery:next,value:{head:id,version:id}};
      });
    },
    async beginPeoplePublication({scope, peopleKey, head, version, copyInstance, binding, native, request, files:captured, current}) {
      if (!validScope(scope) || typeof peopleKey !== "string" || !/^[0-9a-f]{32}\/[0-9a-f]{32}\/[0-9a-f]{16}\/[0-9a-f]{16}$/.test(peopleKey) || typeof head !== "string" || typeof version !== "string" || !/^[0-9a-f]{32}$/.test(copyInstance) || typeof binding !== "string" || binding.length > 256 || typeof native !== "string" || native.length > 256 || !request || !validID(request.key) || typeof request.text !== "string" || new TextEncoder().encode(request.text).length > 16384 || !Number.isSafeInteger(request.anchor) || request.anchor < 0 || !Array.isArray(request.attachments) || request.attachments.length < 1 || request.attachments.length > 16 || request.attachments.some(id => !validID(id)) || new Set(request.attachments).size !== request.attachments.length || !Array.isArray(captured) || captured.length !== request.attachments.length || captured.some((file, i) => !file || file.id !== request.attachments[i] || !validFileToken(file.fileToken) || !validRevision(file.stageRevision)) || (current !== undefined && typeof current !== "function")) throw new Error("People publication identity is unavailable.");
      if (typeof driver.writePeople !== "function") throw new Error("People recovery is unavailable.");
      return driver.writePeople(({files, recovery}) => {
        if (current !== undefined && current() !== true) throw new Error("People publication operation cancelled.");
        checkedRows(files);
        const allocator = recovery.find(row => row.key === "allocator"), currentHead = recovery.find(row => row.key === "head:" + head), draft = recovery.find(row => row.key === "draft:" + version);
        if (!allocator || !/^[0-9a-f]{32}$/.test(allocator.epoch) || !Number.isSafeInteger(allocator.next) || allocator.next < 1 || allocator.next >= Number.MAX_SAFE_INTEGER) throw new Error("People recovery identity is unavailable.");
        if (!currentHead || currentHead.scope !== scope || currentHead.version !== version || !draft || draft.scope !== scope || draft.text !== request.text) throw new Error("People draft changed; review it again.");
        if (captured.some(file => !files.some(row => row.scope === scope && row.id === file.id && row.fileToken === file.fileToken && row.stageRevision === file.stageRevision && row.published === undefined))) throw new Error("People file identity conflict.");
        if (recovery.some(row => (row.kind === "publication" || row.kind === "decision") && row.scope === scope && row.copyInstance === copyInstance && row.request.key === request.key)) throw new Error("People publication identity conflict.");
        const publication = allocator.epoch + ":" + allocator.next, key = "publication:" + publication;
        if (recovery.some(row => row.key === key)) throw new Error("People publication identity conflict.");
        const row = {key, kind:"publication", state:"pending", scope, peopleKey, publication, head, version, copyInstance, binding, native, request:{key:request.key,text:request.text,anchor:request.anchor,attachments:[...request.attachments]}, files:captured.map(file => ({id:file.id,fileToken:file.fileToken,stageRevision:file.stageRevision}))};
        const next = recovery.filter(item => item.key !== "allocator");next.push({...allocator, next:allocator.next + 1}, row);
        const bytes = peopleStoredBytes(files, next), pending = next.filter(item => (item.kind === "publication" || item.kind === "text-publication") && item.state === "pending");
        if (pending.filter(item => item.scope === scope).length > 16 || next.length + pending.length > 2048 || bytes.metadata > 8 * 1024 * 1024 || bytes.total > GLOBAL_MAX || new TextEncoder().encode(JSON.stringify(row)).length + new TextEncoder().encode(row.key).length > 65536) throw new Error("People recovery storage limit reached.");
        return {files, recovery:next, value:{publication}};
      });
    },
    async beginPeopleTextPublication({scope, peopleKey, copyInstance, binding, native, head, version, request, current}) {
      if (!validScope(scope) || typeof peopleKey !== "string" || !/^[0-9a-f]{32}\/[0-9a-f]{32}\/[0-9a-f]{16}\/[0-9a-f]{16}$/.test(peopleKey) || typeof copyInstance !== "string" || !/^(?:[0-9a-f]{32})?$/.test(copyInstance) || typeof binding !== "string" || typeof native !== "string" || typeof head !== "string" || typeof version !== "string" || !request || !validID(request.key) || typeof request.text !== "string" || !request.text.trim() || new TextEncoder().encode(request.text).length > 16384 || !Number.isSafeInteger(request.anchor) || request.anchor < 0 || (current !== undefined && typeof current !== "function") || typeof driver.writePeople !== "function") throw new Error("People text recovery is unavailable.");
      return driver.writePeople(({files, recovery}) => {
        if (current !== undefined && current() !== true) throw new Error("People publication operation cancelled.");
        const allocator = recovery.find(row => row.key === "allocator"), currentHead = recovery.find(row => row.key === "head:" + head), draft = recovery.find(row => row.key === "draft:" + version);
        if (!allocator || !/^[0-9a-f]{32}$/.test(allocator.epoch) || !Number.isSafeInteger(allocator.next) || allocator.next < 1 || allocator.next >= Number.MAX_SAFE_INTEGER) throw new Error("People recovery identity is unavailable.");
        if (!currentHead || currentHead.scope !== scope || currentHead.version !== version || !draft || draft.scope !== scope || draft.text !== request.text) throw new Error("People draft changed; review it again.");
        if (recovery.some(row => row.kind === "text-decision" && row.scope === scope && row.head === head && row.version === version)) throw new Error("People publication was already decided; review it again.");
        if (recovery.some(row => row.kind === "text-publication" && row.state === "pending" && row.scope === scope)) throw new Error("Previous People message is unconfirmed. Review it before sending another.");
        const publication = allocator.epoch + ":" + allocator.next, key = "text-publication:" + publication;
        if (recovery.some(row => row.key === key)) throw new Error("People publication identity conflict.");
        const row = {key,kind:"text-publication",state:"pending",scope,peopleKey,copyInstance,binding,native,head,version,publication,request:{text:request.text,anchor:request.anchor,key:request.key}};
        const next = recovery.filter(item => item.key !== "allocator");next.push({...allocator,next:allocator.next+1},row);
        const bytes = peopleStoredBytes(files,next), pending = next.filter(item => (item.kind === "publication" || item.kind === "text-publication") && item.state === "pending");
        if (pending.filter(item => item.scope === scope).length > 16 || next.length + pending.length > 2048 || bytes.metadata > 8*1024*1024 || bytes.total > GLOBAL_MAX || new TextEncoder().encode(row.key+JSON.stringify(row)).length > 65536) throw new Error("People recovery storage limit reached.");
        return {files,recovery:next,value:{publication}};
      });
    },
    async retirePeopleTextPublication({scope, publication, expected, current}) {
      if (!validScope(scope) || typeof publication !== "string" || !/^[0-9a-f]{32}:[1-9][0-9]*$/.test(publication) ||
          !expected || typeof expected.peopleKey !== "string" || typeof expected.copyInstance !== "string" ||
          typeof expected.binding !== "string" || typeof expected.native !== "string" || typeof expected.head !== "string" ||
          typeof expected.version !== "string" || !expected.request || !validID(expected.request.key) ||
          typeof expected.request.text !== "string" || !Number.isSafeInteger(expected.request.anchor) ||
          typeof current !== "function" || typeof driver.writePeople !== "function") throw new Error("People text recovery identity is unavailable.");
      if (current() !== true) throw new Error("People publication operation cancelled.");
      return driver.writePeople(({files, recovery}) => {
        if (current() !== true) throw new Error("People publication operation cancelled.");
        const pending = recovery.find(row => row.key === "text-publication:" + publication);
        const decision = recovery.find(row => row.key === "text-decision:" + publication);
        if ((pending && decision) || (!pending && !decision)) throw new Error("People publication identity conflict.");
        const row = pending || decision;
        if (row.kind !== (pending ? "text-publication" : "text-decision") || row.publication !== publication || row.scope !== scope ||
            row.peopleKey !== expected.peopleKey || row.copyInstance !== expected.copyInstance || row.binding !== expected.binding ||
            row.native !== expected.native || row.head !== expected.head || row.version !== expected.version || !row.request ||
            row.request.key !== expected.request.key || row.request.text !== expected.request.text || row.request.anchor !== expected.request.anchor ||
            (pending ? row.state !== "pending" : !["retired", "completed"].includes(row.state)) ||
            (decision && row.state === "completed" && (!Number.isSafeInteger(row.seq) || row.seq < 1))) throw new Error("People publication identity conflict.");
        if (decision) return {files, recovery, value:row.state === "completed" ? {state:"completed", seq:row.seq} : {state:"retired"}};
        const next = recovery.filter(item => item !== row);
        next.push({...row, key:"text-decision:" + publication, kind:"text-decision", state:"retired"});
        return {files, recovery:next, value:{state:"retired"}};
      });
    },
    async finishPeopleTextPublication({scope, publication, expected, seq, current}) {
      if (!validScope(scope) || typeof publication !== "string" || !/^[0-9a-f]{32}:[1-9][0-9]*$/.test(publication) || !expected || !expected.request || !Number.isSafeInteger(seq) || seq < 1 || (current !== undefined && typeof current !== "function") || typeof driver.writePeople !== "function") throw new Error("People text recovery is unavailable.");
      return driver.writePeople(({files,recovery}) => {
        if (current !== undefined && current() !== true) throw new Error("People publication operation cancelled.");
        const row = recovery.find(item => item.key === "text-publication:" + publication);
        if (!row || row.scope !== scope || row.state !== "pending" || row.peopleKey !== expected.peopleKey || row.copyInstance !== expected.copyInstance || row.binding !== expected.binding || row.native !== expected.native || row.head !== expected.head || row.version !== expected.version || row.request.key !== expected.request.key || row.request.text !== expected.request.text || row.request.anchor !== expected.request.anchor) throw new Error("People publication identity conflict.");
        const cleanup = completedPeopleDrafts(recovery, row, true), clearHead = cleanup.clearOwned;
        const next = recovery.filter(item => item.key !== row.key && !cleanup.keys.has(item.key));
        const decisionKey = "text-decision:" + publication;
        if (recovery.some(item => item.key === decisionKey)) throw new Error("People publication identity conflict.");
        next.push({...row,key:decisionKey,kind:"text-decision",state:"completed",seq});
        const frozen = recovery.find(item => item.key === "head:" + row.head && item.scope === scope && item.version === row.version && item.sealed);
        return {files,recovery:next,value:{state:"completed",seq,draftCleared:!!clearHead,...(frozen ? {identity:{scope,publication,head:row.head,version:row.version,requestKey:row.request.key}} : {})}};
      });
    },
    async decidePeoplePublication({scope, publication, expected, proof, current}) {
      if (!validScope(scope) || typeof publication !== "string" || !/^[0-9a-f]{32}:[1-9][0-9]*$/.test(publication) || !expected || !proof || !["completed", "locally_discarded"].includes(proof.state) || (proof.state === "completed" && (!Number.isSafeInteger(proof.seq) || proof.seq < 1 || typeof proof.text !== "string" || !Array.isArray(proof.stage_ids))) || (current !== undefined && typeof current !== "function")) throw new Error("People publication decision is unavailable.");
      if (typeof driver.writePeople !== "function") throw new Error("People recovery is unavailable.");
      return driver.writePeople(({files, recovery}) => {
        if (current !== undefined && current() !== true) throw new Error("People publication operation cancelled.");
        const row = recovery.find(item => item.key === "publication:" + publication);
        if (!row || row.scope !== scope || row.state !== "pending") throw new Error("People publication identity conflict.");
        const parsed = parsePeoplePendingSnapshot(expected.value, {key:expected.key, copyInstance:expected.copyInstance, binding:expected.binding, instance:expected.instance});
        const request = JSON.parse(expected.value).request;
        if (expected.key !== row.peopleKey || expected.copyInstance !== row.copyInstance || expected.binding !== row.binding || expected.instance !== row.native || expected.head !== row.head || expected.version !== row.version || expected.publicationKey !== row.request.key || parsed.publicationKey !== row.request.key || parsed.text !== row.request.text || request.anchor !== row.request.anchor || JSON.stringify(parsed.attachments) !== JSON.stringify(row.request.attachments)) throw new Error("People publication identity conflict.");
        if (proof.state === "completed" && (proof.text !== row.request.text || JSON.stringify(proof.stage_ids) !== JSON.stringify(row.request.attachments))) throw new Error("People publication changed; review it again.");
        const completed = proof.state === "completed", nextFiles = files.slice();
        const shared = recovery.some(item => item.kind === "publication" && item.state === "pending" && item.key !== row.key && item.scope === scope && item.files.some(file => row.files.some(original => original.id === file.id && original.fileToken === file.fileToken)));
        const matched = completed && !shared && row.files.every(file => files.some(item => item.scope === scope && item.id === file.id && item.fileToken === file.fileToken && item.stageRevision === file.stageRevision));
        if (matched) for (const file of row.files) {
          const index = files.findIndex(item => item.scope === scope && item.id === file.id && item.fileToken === file.fileToken);
          if (files[index].published !== undefined && files[index].published !== proof.seq) throw new Error("People file identity conflict.");
          nextFiles[index] = {...files[index], published:proof.seq};
        }
        const frozen = recovery.some(item => item.key === "head:" + row.head && item.scope === scope && item.version === row.version && item.sealed);
        const cleanup = completedPeopleDrafts(recovery, row, matched || (completed && frozen)), clearHead = cleanup.clearOwned;
        const next = recovery.filter(item => item.key !== row.key && !cleanup.keys.has(item.key));
        const decisionKey = "decision:" + publication;
        if (recovery.some(item => item.key === decisionKey)) throw new Error("People publication identity conflict.");
        next.push({...row, key:decisionKey, kind:"decision", state:completed ? "completed" : "retired", ...(completed ? {seq:proof.seq, filesConfirmed:matched} : {})});
        return {files:nextFiles, recovery:next, value:completed ? {state:"completed", seq:proof.seq, filesConfirmed:matched, draftCleared:!!clearHead} : {state:"retired"}};
      });
    },
    async listPeopleRecovery(scope) {
      if (!validScope(scope) || typeof driver.writePeople !== "function") throw new Error("People recovery is unavailable.");
      return driver.writePeople(({files, recovery}) => {
        const allocator = recovery.find(row => row.key === "allocator");
        if (!allocator || !/^[0-9a-f]{32}$/.test(allocator.epoch) || !Number.isSafeInteger(allocator.next) || allocator.next < 1) throw new Error("People recovery identity is unavailable.");
        const own = recovery.filter(row => row.scope === scope), heads = own.filter(row => row.kind === "head").map(head => {
          const draft = own.find(row => row.key === "draft:" + head.version);
          if (!draft || typeof draft.text !== "string") throw new Error("People draft recovery is unavailable.");
          return {head:head.head, version:head.version, text:draft.text, ...(head.sealed ? {sealed:true} : {})};
        });
        const pending = own.filter(row => row.kind === "publication" && row.state === "pending").map(row => JSON.parse(JSON.stringify(row)));
        const textPending = own.filter(row => row.kind === "text-publication" && row.state === "pending").map(row => JSON.parse(JSON.stringify(row)));
        const completed = own.filter(row => row.kind === "decision" && row.state === "completed" && row.filesConfirmed === false).map(row => JSON.parse(JSON.stringify(row)));
        const legacy = own.filter(row => row.kind === "legacy-decision").map(row => JSON.parse(JSON.stringify(row)));
        return {files, recovery, value:{heads, pending, textPending, completed, legacy}};
      });
    },
    async listRetainedPeopleRecovery({peopleKey, copyInstance, native, kind = "publication"}) {
      if (typeof peopleKey !== "string" || !/^[0-9a-f]{32}\/[0-9a-f]{32}\/[0-9a-f]{16}\/[0-9a-f]{16}$/.test(peopleKey) || !["publication", "text-publication"].includes(kind) || typeof copyInstance !== "string" || !(kind === "text-publication" ? /^(?:[0-9a-f]{32})?$/.test(copyInstance) : /^[0-9a-f]{32}$/.test(copyInstance)) || typeof native !== "string" || native.length > 256 || typeof driver.writePeople !== "function") throw new Error("People recovery is unavailable.");
      return driver.writePeople(({files, recovery}) => ({files, recovery, value:recovery.filter(row => row.kind === kind && row.state === "pending" && row.peopleKey === peopleKey && row.copyInstance === copyInstance && row.native === native).map(row => JSON.parse(JSON.stringify(row)))}));
    },
    async recordLegacyPeopleDecision({scope, snapshot, proof, current}) {
      if (!validScope(scope) || !snapshot || typeof snapshot.key !== "string" || !/^[0-9a-f]{32}\/[0-9a-f]{32}\/[0-9a-f]{16}\/[0-9a-f]{16}$/.test(snapshot.key) || !/^[0-9a-f]{32}$/.test(snapshot.copyInstance || "") || typeof snapshot.binding !== "string" || snapshot.binding.includes(":") || typeof snapshot.instance !== "string" || snapshot.instance.includes(":") || typeof snapshot.value !== "string" || !proof || !["completed", "locally_discarded"].includes(proof.state) || (current !== undefined && typeof current !== "function")) throw new Error("People legacy recovery is unavailable.");
      const identity = {key:snapshot.key, copyInstance:snapshot.copyInstance, binding:snapshot.binding, instance:snapshot.instance};
      const parsed = parsePeoplePendingSnapshot(snapshot.value, identity);
      if (parsed.storageKey !== snapshot.storageKey || parsed.publicationKey !== snapshot.publicationKey || parsed.text !== snapshot.text || JSON.stringify(parsed.attachments) !== JSON.stringify(snapshot.attachments) || (proof.state === "completed" && (!Number.isSafeInteger(proof.seq) || proof.seq < 1 || proof.text !== parsed.text || JSON.stringify(proof.stage_ids) !== JSON.stringify(parsed.attachments)))) throw new Error("People legacy review changed.");
      if (typeof driver.writePeople !== "function") throw new Error("People recovery is unavailable.");
      return driver.writePeople(({files, recovery}) => {
        if (current !== undefined && current() !== true) throw new Error("People legacy operation cancelled.");
        const prior = recovery.find(row => row.kind === "legacy-decision" && row.scope === scope && row.snapshot.storageKey === parsed.storageKey && row.snapshot.value === parsed.value);
        const state = proof.state === "completed" ? "completed" : "retired";
        if (prior) {
          if (prior.state !== state || (state === "completed" && prior.seq !== proof.seq)) throw new Error("People legacy decision changed; review it again.");
          return {files, recovery, value:{state}};
        }
        const allocator = recovery.find(row => row.key === "allocator");
        if (!allocator || !/^[0-9a-f]{32}$/.test(allocator.epoch) || !Number.isSafeInteger(allocator.next) || allocator.next < 1 || allocator.next >= Number.MAX_SAFE_INTEGER) throw new Error("People recovery identity is unavailable.");
        const key = "legacy:" + allocator.epoch + ":" + allocator.next;
        if (recovery.some(row => row.key === key)) throw new Error("People legacy identity conflict.");
        const row = {key, kind:"legacy-decision", scope, state, snapshot:parsed, ...(state === "completed" ? {seq:proof.seq} : {})};
        const next = recovery.filter(item => item.key !== "allocator");next.push({...allocator, next:allocator.next + 1}, row);
        const bytes = peopleStoredBytes(files, next), pending = next.filter(item => (item.kind === "publication" || item.kind === "text-publication") && item.state === "pending");
        if (next.length + pending.length > 2048 || bytes.metadata > 8 * 1024 * 1024 || bytes.total > GLOBAL_MAX || new TextEncoder().encode(row.key + JSON.stringify(row)).length > 65536) throw new Error("People recovery storage limit reached.");
        return {files, recovery:next, value:{state}};
      });
    },
    async discardPeopleCapture({scope, head, version, text, current}) {
      if (!validScope(scope) || typeof head !== "string" || typeof version !== "string" || typeof text !== "string" || typeof current !== "function" || typeof driver.writePeople !== "function") throw new Error("People draft identity is unavailable.");
      return driver.writePeople(({files, recovery}) => {
        if (current() !== true) return {files, recovery, value:false};
        const saved = recovery.find(row => row.key === "head:" + head), draft = recovery.find(row => row.key === "draft:" + version);
        if (!saved || saved.kind !== "head" || saved.scope !== scope || saved.head !== head || saved.version !== version || !saved.sealed || !draft || draft.kind !== "draft" || draft.scope !== scope || draft.version !== version || draft.text !== text) return {files, recovery, value:false};
        const referenced = recovery.some(row => {
          if (row === saved || row === draft) return false;
          if (row.head !== head && row.version !== version && row.sourceHead !== head && row.sourceVersion !== version) return false;
          const retired = (row.kind === "decision" || row.kind === "text-decision") && row.state === "retired" &&
            row.scope === scope && row.head === head && row.version === version && row.sourceHead !== head && row.sourceVersion !== version &&
            typeof row.publication === "string" && /^[0-9a-f]{32}:[1-9][0-9]*$/.test(row.publication) && row.key === row.kind + ":" + row.publication &&
            typeof row.peopleKey === "string" && /^[0-9a-f]{32}\/[0-9a-f]{32}\/[0-9a-f]{16}\/[0-9a-f]{16}$/.test(row.peopleKey) &&
            typeof row.copyInstance === "string" && (row.kind === "text-decision" ? /^(?:[0-9a-f]{32})?$/.test(row.copyInstance) : /^[0-9a-f]{32}$/.test(row.copyInstance)) &&
            typeof row.binding === "string" && typeof row.native === "string" &&
            (row.kind === "text-decision" || (row.binding.length <= 256 && row.native.length <= 256)) &&
            row.request && validID(row.request.key) && row.request.text === text && Number.isSafeInteger(row.request.anchor) && row.request.anchor >= 0 &&
            (row.kind === "text-decision" || (Array.isArray(row.request.attachments) && row.request.attachments.length >= 1 && row.request.attachments.length <= 16 &&
              row.request.attachments.every(validID) && new Set(row.request.attachments).size === row.request.attachments.length &&
              Array.isArray(row.files) && row.files.length === row.request.attachments.length &&
              row.files.every((file, i) => file && file.id === row.request.attachments[i] && validFileToken(file.fileToken) && validRevision(file.stageRevision))));
          return !retired;
        });
        if (referenced) return {files, recovery, value:false};
        return {files, recovery:recovery.filter(row => row !== saved && row !== draft), value:true};
      });
    },
    async clearPeopleDraft({scope, head, version, current}) {
      if (!validScope(scope) || typeof head !== "string" || typeof version !== "string" || (current !== undefined && typeof current !== "function") || typeof driver.writePeople !== "function") throw new Error("People draft recovery is unavailable.");
      return driver.writePeople(({files, recovery}) => {
        if (current !== undefined && current() !== true) throw new Error("People draft operation cancelled.");
        const saved = recovery.find(row => row.key === "head:" + head);
        const pending = recovery.some(row => (row.kind === "publication" || row.kind === "text-publication") && row.state === "pending" && row.version === version);
        if (!saved || saved.scope !== scope || saved.version !== version || saved.sealed || pending) return {files, recovery, value:false};
        const shared = recovery.some(row => row.kind === "head" && row.key !== saved.key && row.version === version);
        const next = recovery.filter(row => row.key !== saved.key && (shared || row.key !== "draft:" + version));
        return {files, recovery:next, value:true};
      });
    },
    async save(scope, id, file, metadata) {
      if (!validScope(scope) || !validID(id)) throw new Error("People file identity is unavailable.");
      if (!validFile(file)) throw new Error("People files must be non-empty and at most 4 MiB with a safe name.");
      if (metadata !== undefined && !validMetadata(metadata)) throw new Error("People file identity is unavailable.");
      const frozenMetadata = metadata === undefined ? undefined : JSON.parse(JSON.stringify(metadata));
      const fileToken = newFileToken();
      return driver.readWrite(input => {
        const rows = checkedRows(input); const own = rows.filter(row => row.scope === scope);
        if (rows.some(row => row.key === scope + "\0" + id)) throw new Error("People file identity conflict.");
        if (own.length >= 16 || own.reduce((sum, row) => sum + row.file.size, 0) + file.size > SCOPE_MAX || rows.length >= GLOBAL_COUNT || rows.reduce((sum, row) => sum + row.file.size, 0) + file.size > GLOBAL_MAX) throw new Error("People file storage limit reached.");
        return {rows:[...rows, {key:scope + "\0" + id, scope, id, file, fileToken, ...(frozenMetadata === undefined ? {} : {metadata:frozenMetadata})}], value:fileToken};
      });
    },
    async list(scope) {
      if (!validScope(scope)) throw new Error("People file identity is unavailable.");
      return checkedRows(await driver.read()).filter(row => row.scope === scope).map(row => ({id:row.id, file:row.file, ...(row.metadata === undefined ? {} : {metadata:row.metadata}), ...(row.fileToken === undefined ? {} : {fileToken:row.fileToken}), ...(row.stageRevision === undefined ? {} : {stageRevision:row.stageRevision}), ...(row.published === undefined ? {} : {published:row.published})}));
    },
    async setStageRevision(scope, id, revision, fileToken) {
      if (!validScope(scope) || !validID(id) || !validRevision(revision) || !validFileToken(fileToken)) throw new Error("People file identity is unavailable.");
      return driver.readWrite(input => {
        const rows = checkedRows(input), index = rows.findIndex(row => row.key === scope + "\0" + id);
        if (index < 0) throw new Error("People file recovery is unavailable.");
        if (rows[index].fileToken !== fileToken) throw new Error("People file identity conflict.");
        if (rows[index].stageRevision !== undefined && rows[index].stageRevision !== revision) throw new Error("People file identity conflict.");
        const next = rows.slice(); next[index] = {...rows[index], stageRevision:revision};
        return {rows:next, value:true};
      });
    },
    async removeStageFile(scope, id, fileToken, current, publication) {
      if (!validScope(scope) || !validID(id) || (fileToken !== undefined && !validFileToken(fileToken))) throw new Error("People file identity is unavailable.");
      if (current !== undefined && typeof current !== "function") throw new Error("People file ownership is unavailable.");
      return peoplePublicationFileWrite(driver, scope, publication, [{id,fileToken}], input => {
        if (current !== undefined && current() !== true) throw new Error("People file operation cancelled.");
        const rows = checkedRows(input), row = rows.find(item => item.key === scope + "\0" + id);
        if (row && row.fileToken !== fileToken) throw new Error("People file identity conflict.");
        return {rows:rows.filter(item => item.key !== scope + "\0" + id), value:true};
      }, true);
    },
    async listRetired(prefix, ownerIdentity, currentScope) {
      if (!validScope(prefix) || typeof ownerIdentity !== "string" || !validScope(currentScope)) throw new Error("People file identity is unavailable.");
      const legacy = prefix + "\0" + ownerIdentity, marker = prefix + "\0copy:";
      return checkedRows(await driver.read()).filter(row => {
        if (row.scope === currentScope) return false;
        if (row.scope === legacy) return true;
        if (!row.scope.startsWith(marker)) return false;
        const tail = row.scope.slice(marker.length);
        return /^[0-9a-f]{32}$/.test(tail.slice(0, 32)) && tail.slice(32) === "\0" + ownerIdentity;
      }).map(row => ({scope:row.scope, id:row.id, file:row.file, ...(row.published === undefined ? {} : {published:row.published})}));
    },
    async markPublished(scope, id, seq) {
      return this.markPublishedBatch(scope, [id], seq);
    },
    async markPublishedBatch(scope, ids, seq, fileTokens, current, publication) {
      if (!validScope(scope) || !Array.isArray(ids) || !ids.length || ids.length > 16 || ids.some(id => !validID(id)) || new Set(ids).size !== ids.length || !Number.isSafeInteger(seq) || seq < 1) throw new Error("People file identity is unavailable.");
      if (fileTokens !== undefined && (!Array.isArray(fileTokens) || fileTokens.length !== ids.length)) throw new Error("People file identity is unavailable.");
      if (fileTokens !== undefined && fileTokens.some(token => !validFileToken(token))) throw new Error("Review saved files before recording delivery.");
      if (current !== undefined && typeof current !== "function") throw new Error("People file ownership is unavailable.");
      return peoplePublicationFileWrite(driver, scope, publication, ids.map((id, index) => ({id,fileToken:fileTokens?.[index]})), input => {
        if (current !== undefined && current() !== true) throw new Error("People file operation cancelled.");
        const rows = checkedRows(input); const next = rows.slice();
        for (const [position, id] of ids.entries()) {
          const index = rows.findIndex(row => row.key === scope + "\0" + id);
          if (index < 0) throw new Error("People file recovery is unavailable.");
          if (fileTokens !== undefined && rows[index].fileToken !== fileTokens[position]) throw new Error("People file identity conflict.");
          if (rows[index].published !== undefined && rows[index].published !== seq) throw new Error("People file identity conflict.");
          next[index] = {...next[index], published:seq};
        }
        return {rows:next, value:true};
      });
    },
    async remove(scope, id) {
      if (!validScope(scope) || !validID(id)) throw new Error("People file identity is unavailable.");
      return driver.readWrite(input => ({rows:checkedRows(input).filter(row => row.key !== scope + "\0" + id), value:true}));
    },
  };
}

export function createIndexedDBPeopleDriver(indexedDB) {
  if (!indexedDB || typeof indexedDB.open !== "function") throw new Error("People file storage unavailable.");
  let opened = null, connection = null, retired = false, rejectOpen = null;
  const unavailable = message => Object.assign(new Error(message), {code:"PEOPLE_STORAGE_UNAVAILABLE"});
  function database() {
    if (retired) return Promise.reject(unavailable("People file storage is unavailable."));
    if (opened) return opened;
    opened = new Promise((resolve, reject) => {
      let settled = false;
      const fail = () => { if (!settled) { settled = true; rejectOpen = null; reject(unavailable("People file storage unavailable.")); } };
      rejectOpen = fail;
      let request;
      try { request = indexedDB.open("scimux-people-file-drafts", 2); } catch { fail(); return; }
      request.onupgradeneeded = () => {
        try {
          if (!request.result.objectStoreNames.contains("files")) request.result.createObjectStore("files", {keyPath:"key"});
          if (!request.result.objectStoreNames.contains("recovery")) {
            const recovery = request.result.createObjectStore("recovery", {keyPath:"key"});
            recovery.add({key:"allocator", epoch:newFileToken().replaceAll("-", ""), next:1});
          }
        }
        catch {
          try { request.transaction?.abort(); } catch { /* the upgrade may already have aborted */ }
          fail();
        }
      };
      request.onerror = fail;
      request.onblocked = fail;
      request.onsuccess = () => {
        const db = request.result;
        if (settled || retired) { db.close(); fail(); return; }
        settled = true;
        rejectOpen = null;
        connection = db;
        db.onversionchange = () => { if (connection === db) { connection = null; db.close(); opened = null; } };
        resolve(db);
      };
    });
    const attempt = opened;
    attempt.catch(() => { if (opened === attempt) opened = null; });
    return opened;
  }
  // Every write uses both stores; this separate transaction only reads files.
  async function transact() {
    const db = await database();
    if (retired) throw unavailable("People file storage is unavailable.");
    return new Promise((resolve, reject) => {
      let tx;
      try { tx = db.transaction("files", "readonly"); }
      catch { reject(new Error("People file storage unavailable.")); return; }
      let result; let failure = null;
      tx.oncomplete = () => resolve(result);
      tx.onabort = tx.onerror = () => reject(failure || new Error("People file storage unavailable."));
      try {
        const store = tx.objectStore("files");
        const request = store.getAll();
        request.onsuccess = () => { result = request.result; };
        request.onerror = () => { failure = new Error("People file storage unavailable."); tx.abort(); };
      } catch { failure = new Error("People file storage unavailable."); tx.abort(); }
    });
  }
  async function writePeople(update) {
    const db = await database();
    if (retired) throw unavailable("People file storage is unavailable.");
    return new Promise((resolve, reject) => {
      let tx;
      try { tx = db.transaction(["files", "recovery"], "readwrite"); }
      catch { reject(unavailable("People recovery is unavailable.")); return; }
      let result, failure = null;
      tx.oncomplete = () => resolve(result);
      tx.onabort = () => reject(failure || unavailable("People recovery is unavailable."));
      try {
        const files = tx.objectStore("files"), recovery = tx.objectStore("recovery");
        const filesRequest = files.getAll(), recoveryRequest = recovery.getAll();
        let fileRows, recoveryRows;
        const failed = () => { failure = unavailable("People recovery is unavailable."); tx.abort(); };
        filesRequest.onerror = failed; recoveryRequest.onerror = failed;
        const ready = () => {
          if (!fileRows || !recoveryRows) return;
          try {
            const next = update({files:fileRows, recovery:recoveryRows});
            const beforeSize = peopleStoredBytes(fileRows, recoveryRows), afterSize = peopleStoredBytes(next.files, next.recovery);
            if ((afterSize.total > GLOBAL_MAX && afterSize.total > beforeSize.total) || (afterSize.metadata > 8 * 1024 * 1024 && afterSize.metadata > beforeSize.metadata)) throw new Error("People recovery storage limit reached.");
            for (const [store, before, after] of [[files,fileRows,next.files], [recovery,recoveryRows,next.recovery]]) {
              const old = new Map(before.map(row => [row.key, row])), keys = new Set(after.map(row => row.key));
              for (const key of old.keys()) if (!keys.has(key)) store.delete(key);
              for (const row of after) if (old.get(row.key) !== row) old.has(row.key) ? store.put(row) : store.add(row);
            }
            result = next.value;
          } catch (error) { failure = error; tx.abort(); }
        };
        filesRequest.onsuccess = () => { fileRows = filesRequest.result; ready(); };
        recoveryRequest.onsuccess = () => { recoveryRows = recoveryRequest.result; ready(); };
      } catch { failure = unavailable("People recovery is unavailable."); tx.abort(); }
    });
  }
  return {read:() => transact(), readWrite:update => writePeople(({files, recovery}) => { const next = update(files); return {files:next.rows, recovery, value:next.value}; }), writePeople, destroy() { retired = true; rejectOpen?.(); if (connection) { connection.close(); connection = null; } }};
}

export function createBrowserPeopleFileStore(indexedDB) {
  if (!indexedDB || typeof indexedDB.open !== "function") return null;
  return createPeopleFileStore(createIndexedDBPeopleDriver(indexedDB));
}
