'use strict';
const fs = require('node:fs');
const path = require('node:path');
const {execFileSync} = require('node:child_process');

// Resolve once from the caller's preflight PATH, then execute by absolute path
// while the child keeps the fixture-only agent PATH.
function prepareTunnelGo({selected, inheritedPath, env}) {
  let candidate = selected;
  if (candidate && !path.isAbsolute(candidate)) throw Error('SCIMUX_E2E_GO must be an absolute path');
  if (!candidate) {
    for (const directory of (inheritedPath || '').split(path.delimiter)) {
      if (!path.isAbsolute(directory)) continue;
      const possible = path.join(directory, 'go');
      try { fs.accessSync(possible, fs.constants.X_OK); candidate = possible; break; } catch {}
    }
  }
  if (!candidate) throw Error('Go 1.26 or newer is required for the tunnel fixture');
  let executable, version;
  try {
    executable = fs.realpathSync(candidate);
    fs.accessSync(executable, fs.constants.X_OK);
    version = execFileSync(executable, ['version'], {env:{...env,GOTOOLCHAIN:'local'}, encoding:'utf8', timeout:10000}).trim();
  } catch { throw Error('Unable to verify the selected Go toolchain'); }
  const match = /^go version go(\d+)\.(\d+)(?:\.\d+)?(?:\s|$)/.exec(version);
  if (!match || Number(match[1]) < 1 || (Number(match[1]) === 1 && Number(match[2]) < 26)) throw Error('Go 1.26 or newer is required for the tunnel fixture');
  return {executable, version};
}
module.exports = {prepareTunnelGo};
