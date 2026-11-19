/* Durable People attachment drafts. The driver serializes read/write updates;
   stored bytes never enter agent context without an explicit owner action. */
const FILE_MAX = 4 * 1024 * 1024;
const SCOPE_MAX = 16 * 1024 * 1024;
const GLOBAL_MAX = 256 * 1024 * 1024;
const GLOBAL_COUNT = 4096;

function validScope(scope) { return typeof scope === "string" && scope.length > 0 && scope.length <= 512; }
function validID(id) { return typeof id === "string" && /^[0-9a-f]{16}$/.test(id); }
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
  return !!row && validScope(row.scope) && validID(row.id) && row.key === row.scope + "\0" + row.id && validFile(row.file) && (row.metadata === undefined || validMetadata(row.metadata)) && (row.published === undefined || (Number.isSafeInteger(row.published) && row.published > 0));
}
function checkedRows(rows) {
  if (!Array.isArray(rows) || rows.some(row => !validRow(row))) throw new Error("People file recovery is unavailable.");
  return rows;
}

export function createPeopleFileStore(driver) {
  if (!driver || typeof driver.readWrite !== "function" || typeof driver.read !== "function") throw new Error("People file storage unavailable.");
  return {
    async save(scope, id, file, metadata) {
      if (!validScope(scope) || !validID(id)) throw new Error("People file identity is unavailable.");
      if (!validFile(file)) throw new Error("People files must be non-empty and at most 4 MiB with a safe name.");
      if (metadata !== undefined && !validMetadata(metadata)) throw new Error("People file identity is unavailable.");
      const frozenMetadata = metadata === undefined ? undefined : JSON.parse(JSON.stringify(metadata));
      return driver.readWrite(input => {
        const rows = checkedRows(input); const own = rows.filter(row => row.scope === scope);
        if (rows.some(row => row.key === scope + "\0" + id)) throw new Error("People file identity conflict.");
        if (own.length >= 16 || own.reduce((sum, row) => sum + row.file.size, 0) + file.size > SCOPE_MAX || rows.length >= GLOBAL_COUNT || rows.reduce((sum, row) => sum + row.file.size, 0) + file.size > GLOBAL_MAX) throw new Error("People file storage limit reached.");
        return {rows:[...rows, {key:scope + "\0" + id, scope, id, file, ...(frozenMetadata === undefined ? {} : {metadata:frozenMetadata})}], value:true};
      });
    },
    async list(scope) {
      if (!validScope(scope)) throw new Error("People file identity is unavailable.");
      return checkedRows(await driver.read()).filter(row => row.scope === scope).map(row => ({id:row.id, file:row.file, ...(row.metadata === undefined ? {} : {metadata:row.metadata}), ...(row.published === undefined ? {} : {published:row.published})}));
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
    async markPublishedBatch(scope, ids, seq) {
      if (!validScope(scope) || !Array.isArray(ids) || !ids.length || ids.length > 16 || ids.some(id => !validID(id)) || new Set(ids).size !== ids.length || !Number.isSafeInteger(seq) || seq < 1) throw new Error("People file identity is unavailable.");
      return driver.readWrite(input => {
        const rows = checkedRows(input); const next = rows.slice();
        for (const id of ids) {
          const index = rows.findIndex(row => row.key === scope + "\0" + id);
          if (index < 0) throw new Error("People file recovery is unavailable.");
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
  let opened = null;
  function database() {
    if (opened) return opened;
    opened = new Promise((resolve, reject) => {
      let settled = false;
      const fail = () => { if (!settled) { settled = true; reject(new Error("People file storage unavailable.")); } };
      let request;
      try { request = indexedDB.open("scimux-people-file-drafts", 1); } catch { fail(); return; }
      request.onupgradeneeded = () => {
        try { if (!request.result.objectStoreNames.contains("files")) request.result.createObjectStore("files", {keyPath:"key"}); }
        catch { fail(); }
      };
      request.onerror = fail;
      request.onblocked = fail;
      request.onsuccess = () => {
        const db = request.result;
        if (settled) { db.close(); return; }
        settled = true;
        db.onversionchange = () => { db.close(); opened = null; };
        resolve(db);
      };
    });
    const attempt = opened;
    attempt.catch(() => { if (opened === attempt) opened = null; });
    return opened;
  }
  async function transact(write, update) {
    const db = await database();
    return new Promise((resolve, reject) => {
      let tx;
      try { tx = db.transaction("files", write ? "readwrite" : "readonly"); }
      catch { reject(new Error("People file storage unavailable.")); return; }
      let result; let failure = null;
      tx.oncomplete = () => resolve(result);
      tx.onabort = tx.onerror = () => reject(failure || new Error("People file storage unavailable."));
      try {
        const store = tx.objectStore("files");
        const request = store.getAll();
        request.onsuccess = () => {
          try {
            const before = request.result;
            if (!write) { result = before; return; }
            const next = update(before);
            const previous = new Map(before.map(row => [row.key, row]));
            const keys = new Set(next.rows.map(row => row.key));
            for (const key of previous.keys()) if (!keys.has(key)) store.delete(key);
            for (const row of next.rows) if (previous.get(row.key) !== row) store.put(row);
            result = next.value;
          } catch (err) { failure = err; tx.abort(); }
        };
        request.onerror = () => { failure = new Error("People file storage unavailable."); tx.abort(); };
      } catch { failure = new Error("People file storage unavailable."); tx.abort(); }
    });
  }
  return {read:() => transact(false), readWrite:update => transact(true, update)};
}

export function createBrowserPeopleFileStore(indexedDB) {
  if (!indexedDB || typeof indexedDB.open !== "function") return null;
  return createPeopleFileStore(createIndexedDBPeopleDriver(indexedDB));
}
