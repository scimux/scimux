import test from "node:test";
import assert from "node:assert/strict";

import {
  channelTransport,
  driveAppJsSites,
  jsonResponse,
  routeBody,
} from "./s2-helpers.js";

async function settle() {
  for (let i = 0; i < 20; i++) await Promise.resolve();
  await new Promise(resolve => setTimeout(resolve, 0));
}

test("merged shell wires remote management controls to their features", async () => {
  const transport = channelTransport();
  const calls = [];
  const fetchImpl = async (path, opts = {}) => {
    calls.push({ path: String(path), method: opts.method || "GET" });
    const clean = String(path).split("?")[0];
    if (clean === "/api/remote/status") return jsonResponse({ hosted: "enrolled", can_pair: true });
    if (clean === "/api/remote/devices") {
      return jsonResponse({ devices: [{ id: "phone-1", label: "Hotel phone" }] });
    }
    if (clean === "/api/remote/unenroll") return jsonResponse({ released: true });
    return jsonResponse(routeBody(path));
  };
  fetchImpl.calls = calls;
  transport.fetchImpl = fetchImpl;

  const app = await driveAppJsSites(transport);
  assert.equal(app.missingSeam, false);
  const byId = app.host.byId;

  const beforeDevices = calls.filter(c => c.path === "/api/remote/devices").length;
  byId.get("burger").click();
  await settle();
  assert.match(byId.get("m_devices").innerHTML, /Hotel phone/);
  assert.ok(calls.some(c => c.path === "/api/remote/status"));

  /* The pair button is hidden markup until the status read reveals it.
     The DOM stub starts everything visible, so the load-bearing half of
     this pair of assertions is the one in the next test, where a refusing
     status must put the button away again. */
  assert.equal(byId.get("m_pair").hidden, false);
  assert.equal(byId.get("m_pair_note").hidden, true);

  assert.equal(byId.get("pairsheet").hidden, true);
  byId.get("m_pair").click();
  assert.equal(byId.get("pairsheet").hidden, false);
  byId.get("pairscrim").click();
  assert.equal(byId.get("pairsheet").hidden, true);

  byId.get("m_unlink").click();
  byId.get("m_unlink").click();
  await settle();
  assert.match(byId.get("m_unlink_note").innerHTML, /no longer enrolled/);
  assert.equal(
    calls.filter(c => c.path === "/api/remote/devices").length,
    beforeDevices + 2,
    "opening the menu and successful unlink each refresh the device list",
  );
});

/* The other half of the same wiring: opening the menu on a computer that
   cannot pair must take the button away and say why. An unwired pairControl
   fails here, because nothing else in the shell ever touches #m_pair's
   visibility. */
test("the merged shell hides Pair a device when the computer cannot pair", async () => {
  const transport = channelTransport();
  const calls = [];
  const fetchImpl = async (path, opts = {}) => {
    calls.push({ path: String(path), method: opts.method || "GET" });
    const clean = String(path).split("?")[0];
    if (clean === "/api/remote/status") {
      return jsonResponse({
        hosted: "",
        can_pair: false,
        pair_refusal: "This installation is not enrolled with a rendezvous, so there is nowhere for a device to meet it.",
      });
    }
    if (clean === "/api/remote/devices") return jsonResponse({ devices: [] });
    return jsonResponse(routeBody(path));
  };
  fetchImpl.calls = calls;
  transport.fetchImpl = fetchImpl;

  const app = await driveAppJsSites(transport);
  assert.equal(app.missingSeam, false);
  const byId = app.host.byId;

  byId.get("burger").click();
  await settle();

  assert.equal(byId.get("m_pair").hidden, true, "an unenrolled computer must not offer a code nothing can meet");
  assert.equal(byId.get("m_pair_note").hidden, false);
  assert.match(byId.get("m_pair_note").innerHTML, /not enrolled with a rendezvous/);
});
