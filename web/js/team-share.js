/* Share button, share sheet, and status line.
 *
 * api, doc, openSheet, closeSheets, and now are injected. This file does not
 * touch document, window, or fetch. Server and share reads run on one queue.
 * A newer select drops an older answer.
 */
export function shareStatusText(view, serverName) {
  const count = (view.members || []).length;
  const name = serverName;
  if (view.state === "stopping") return "Stopping sharing…";
  if (view.state === "lost") return "Sharing ended on " + name;
  if (view.state === "full") return "Sharing paused · " + name + " is full";
  if (view.delivery === "offline") {
    return "Shared with " + count + " · " + name + " · offline, will retry";
  }
  if (view.delivery === "refused") {
    return "Sharing stopped · " + name + " ended this computer's access";
  }
  if (view.delivery === "changed") {
    return "Sharing paused · " + name + " is not the same server";
  }
  return "Shared with " + count + " · " + name;
}

export function shareErrorText(code) {
  const ERROR_TEXT = {
    unreachable: "The server could not be reached.",
    bad_server: "That address no longer answers as this server.",
    refused: "The server ended this computer's access.",
    unknown_member: "Someone in the list has left the server. Open the sheet again.",
    server_full: "The server has no room for another shared chat.",
    too_many: "This computer shares too many chats. Stop one first.",
    no_history: "This chat has nothing to share yet.",
    not_active: "Sharing of this chat has ended. Stop it, then share again.",
    share_fixed: "Stop sharing first to change the server or the scope.",
    store_unavailable: "This computer could not save the change. Try again.",
  };
  return ERROR_TEXT[code] || "That did not work. Try again.";
}

const SERVERS = "/api/team/servers";
const INTERVAL = 10000;
const LEFT = "a server you left";
const SHARE_ICON = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="6" cy="12" r="2"></circle><circle cx="18" cy="6" r="2"></circle><circle cx="18" cy="18" r="2"></circle><path d="M8 11.2 15.2 7.2"></path><path d="M8 12.8 15.2 16.8"></path></svg>';

export function createShareFeature({ api, doc, openSheet, closeSheets, now }) {
  const $ = (sel) => doc.querySelector(sel);

  let off = false;
  let joined = [];
  let selected = "";
  let view = null;
  let generation = 0;
  let pendingLoads = 0;
  let lastLoadAt = 0;
  let chain = Promise.resolve();

  function enqueue(fn) {
    chain = chain.then(fn);
    return chain;
  }

  function stale(gen) {
    return gen !== generation;
  }

  function serverList(body) {
    if (body && Array.isArray(body.servers)) return body.servers;
    return [];
  }

  function nameOf(id) {
    for (const server of joined) {
      if (server.id === id) return server.name;
    }
    return LEFT;
  }

  function hideControls() {
    $("#sharebtn").hidden = true;
    $("#sharestatus").hidden = true;
  }

  function paintHeader() {
    const btn = $("#sharebtn");
    const status = $("#sharestatus");
    if (joined.length === 0) {
      btn.hidden = true;
      status.hidden = true;
      return;
    }
    btn.hidden = false;
    if (!view) {
      status.hidden = true;
      return;
    }
    status.hidden = false;
    status.textContent = shareStatusText(view, nameOf(view.server));
  }

  function beginLoad(node) {
    const gen = ++generation;
    pendingLoads += 1;
    lastLoadAt = now();
    const listed = api(SERVERS);
    return enqueue(async () => {
      try {
        await runLoad(gen, node, listed);
      } finally {
        pendingLoads -= 1;
      }
    });
  }

  async function runLoad(gen, node, listedPromise) {
    let listed = null;
    let serverErr = null;
    try {
      listed = await listedPromise;
    } catch (err) {
      serverErr = err;
    }
    if (stale(gen)) return;
    if (serverErr) {
      if (serverErr.status === 404) off = true;
      return;
    }
    let shareBody = null;
    let shareErr = null;
    try {
      shareBody = await api("/api/team/shares/" + node);
    } catch (err) {
      shareErr = err;
    }
    if (stale(gen)) return;
    joined = serverList(listed);
    if (shareErr) {
      if (shareErr.status === 404) {
        view = null;
        paintHeader();
      }
      return;
    }
    view = shareBody && typeof shareBody === "object" ? shareBody : null;
    paintHeader();
  }

  function select(node) {
    selected = node;
    hideControls();
    if (off) return;
    return beginLoad(node);
  }

  function tick() {
    if (off) return;
    if (!selected) return;
    if (pendingLoads > 0) return;
    if (now() - lastLoadAt < INTERVAL) return;
    return beginLoad(selected);
  }

  function bind() {
    $("#sharebtn").innerHTML = SHARE_ICON;
  }

  function destroy() {}

  async function settled() {
    let prev;
    do {
      prev = chain;
      await prev;
    } while (chain !== prev);
  }

  return { select, tick, bind, destroy, settled };
}
