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
    if (clean === "/api/remote/status") return jsonResponse({ hosted: true });
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
