function localCopyPath(api, key, copyInstance, current) {
  if (typeof api !== 'function' || typeof current !== 'function' ||
      typeof key !== 'string' || !/^[0-9a-f]{32}\/[0-9a-f]{32}\/[0-9a-f]{16}\/[0-9a-f]{16}$/.test(key) ||
      typeof copyInstance !== 'string' || !/^[0-9a-f]{32}$/.test(copyInstance)) {
    throw new Error('Local review identity is unavailable');
  }
  return `/api/team/subscriptions/${key}`;
}

/* Read the exact local copy for review; this never changes publication state. */
export async function loadPeopleLocalInventory({api, key, copyInstance, signal, current}) {
  const path = localCopyPath(api, key, copyInstance, current) + `/local?copy_instance=${copyInstance}`;
  const entries = [], identities = new Set();
  let cursor = '';
  const check = () => {
    if (signal?.aborted || !current()) throw new Error('Local review was cancelled');
  };
  for (let page = 0; page < 1024; page++) {
    check();
    let result;
    try {
      result = await api(path + (cursor ? `&cursor=${cursor}` : ''), {method:'GET', signal});
    } catch (error) {
      check();
      throw error;
    }
    check();
    if (!result || !Array.isArray(result.entries) || result.entries.length > 8) throw new Error('Invalid local review page');
    for (const entry of result.entries) {
      if (!entry || (entry.kind !== 'pending' && entry.kind !== 'stage') ||
          typeof entry.id !== 'string' || !/^[0-9a-f]{16}$/.test(entry.id) ||
          typeof entry.revision !== 'string' || !/^[0-9a-f]{64}$/.test(entry.revision)) throw new Error('Invalid local review record');
      const identity = entry.kind + ':' + entry.id;
      if (identities.has(identity)) throw new Error('Duplicate local review record');
      identities.add(identity);
      entries.push(entry);
    }
    if (result.next_cursor === undefined || result.next_cursor === '') return entries;
    if (typeof result.next_cursor !== 'string' || !/^[ps][0-9a-f]{16}$/.test(result.next_cursor) || result.next_cursor <= cursor) throw new Error('Invalid local review cursor');
    cursor = result.next_cursor;
  }
  throw new Error('Local review exceeds the inventory limit');
}

/* Retire only the immutable pending record shown in the current local review. */
export async function discardPeopleLocalPending({api, key, copyInstance, record, current}) {
  const path = localCopyPath(api, key, copyInstance, current);
  const kind = record?.kind, id = record?.id, revision = record?.revision;
  if (kind !== 'pending' || typeof id !== 'string' || !/^[0-9a-f]{16}$/.test(id) ||
      typeof revision !== 'string' || !/^[0-9a-f]{64}$/.test(revision)) throw new Error('Local pending review is unavailable');
  const check = () => { if (!current()) throw new Error('Local review was cancelled'); };
  check();
  let result;
  try {
    result = await api(`${path}/pending/${id}?copy_instance=${copyInstance}&revision=${revision}`, {method:'DELETE'});
  } catch (error) {
    check();
    throw error;
  }
  check();
  if (result?.state === 'locally_discarded' && result.seq === undefined) return {state:'locally_discarded'};
  if (result?.state === 'already_completed' && Number.isSafeInteger(result.seq) && result.seq > 0) return {state:'already_completed', seq:result.seq};
  throw new Error('Local pending result is unavailable');
}

// Capture both the channel tuple and the distinct publication key before dispatch.
async function requestLocalPublication({api, key, copyInstance, publicationKey, signal, current}, method, suffix) {
  const path = localCopyPath(api, key, copyInstance, current);
  if (typeof publicationKey !== 'string' || !/^[0-9a-f]{16}$/.test(publicationKey)) throw new Error('Local publication identity is unavailable');
  const check = () => { if (signal?.aborted || !current()) throw new Error('Local review was cancelled'); };
  check();
  let result;
  try {
    result = await api(`${path}/pending/${publicationKey}${suffix}?copy_instance=${copyInstance}`, {method, signal});
  } catch (error) {
    check();
    throw error;
  }
  check();
  return result;
}

function hasOnlyFields(result, fields) {
  return result !== null && typeof result === 'object' && !Array.isArray(result) &&
    Object.keys(result).every(key => fields.includes(key));
}

/* Read an exact local publication without changing it or inferring authority. */
export async function readPeopleLocalPublication(input) {
  const result = await requestLocalPublication(input, 'GET', '');
  const invalid = () => { throw new Error('Local publication result is unavailable'); };
  if (!hasOnlyFields(result, ['state', 'revision', 'text', 'stage_ids', 'seq', 'pending_pinned'])) return invalid();
  if (result.state === 'missing' || result.state === 'absent_retired') {
    if (Object.keys(result).length !== 1) return invalid();
    return {state:result.state};
  }
  if (result.state !== 'pending' && result.state !== 'retired' && result.state !== 'completed') return invalid();
  if (typeof result.revision !== 'string' || !/^[0-9a-f]{64}$/.test(result.revision) ||
      (result.text !== undefined && (typeof result.text !== 'string' || new TextEncoder().encode(result.text).length > 16384)) ||
      !Array.isArray(result.stage_ids) || result.stage_ids.length < 1 || result.stage_ids.length > 16 ||
      result.stage_ids.some(id => typeof id !== 'string' || !/^[0-9a-f]{16}$/.test(id)) ||
      new Set(result.stage_ids).size !== result.stage_ids.length) return invalid();
  if (result.state === 'completed') {
    if (!Number.isSafeInteger(result.seq) || result.seq <= 0 ||
        (result.pending_pinned !== undefined && typeof result.pending_pinned !== 'boolean')) return invalid();
  } else if (result.seq !== undefined || result.pending_pinned !== undefined) return invalid();
  const projection = {state:result.state, revision:result.revision, text:result.text ?? '', stage_ids:[...result.stage_ids]};
  if (result.state === 'completed') {
    projection.seq = result.seq;
    if (result.pending_pinned === true) projection.pending_pinned = true;
  }
  return projection;
}

/* Only an explicit caller action may retire an absent publication key. */
export async function retireAbsentPeopleLocalPublication(input) {
  const result = await requestLocalPublication(input, 'POST', '/retire-absent');
  if (!hasOnlyFields(result, ['state']) || result.state !== 'locally_discarded') throw new Error('Local publication retirement is unavailable');
  return {state:'locally_discarded'};
}

/* Browser retry snapshots remain tied to their exact copy and binding. */
export function peoplePendingStorageKey({key, instance, binding, copyInstance}) {
  return 'scimux-people-file-pending:' + key + ':' + instance + (binding ? ':' + binding : '') + ':copy:' + copyInstance;
}

export function parsePeoplePendingSnapshot(value, identity) {
  let saved;
  try { saved = JSON.parse(value); } catch { throw new Error('Pending message recovery is unavailable.'); }
  const request = saved?.request;
  if (saved?.instance !== identity.instance || saved.copyInstance !== identity.copyInstance || !request ||
      typeof request.text !== 'string' || new TextEncoder().encode(request.text).length > 16384 ||
      !Number.isSafeInteger(request.anchor) || request.anchor < 0 || typeof request.key !== 'string' ||
      !/^[0-9a-f]{16}$/.test(request.key) || !Array.isArray(request.attachments) ||
      request.attachments.length < 1 || request.attachments.length > 16 ||
      request.attachments.some(id => typeof id !== 'string' || !/^[0-9a-f]{16}$/.test(id)) ||
      new Set(request.attachments).size !== request.attachments.length) throw new Error('Pending message recovery is unavailable.');
  return {...identity, storageKey:peoplePendingStorageKey(identity), value, publicationKey:request.key,
    text:request.text, attachments:[...request.attachments]};
}
