/* P0 — scimux's wrapper over the vendored Nayuki encoder.
 *
 * The vendor boundary is deliberate and this module is the whole of it:
 * nothing else in the tree imports qrcodegen.js, so a re-vendor has one
 * caller to satisfy. Everything here is scimux's own decision — error
 * correction level, quiet zone, colours — and everything in qrcodegen.js
 * is upstream's.
 *
 * Why a QR at all: the pairing link carries the laptop's uncompressed
 * P-256 point as 130 hex characters (internal/remote/invite_link.go), so
 * it cannot be typed, and rendezvous-v1 §11.2 records that V1 has no
 * typed pairing path. Scanning is the only way a device learns X, and X
 * arriving out-of-band is what the SAS comparison then binds.
 */
import { qrcodegen } from "./qrcodegen.js";

/* Error correction level. LOW maximises payload per module, which keeps a
 * 266-character link at version 10 (57 modules) instead of spilling into a
 * denser symbol. A phone camera at arm's length reads 57 modules
 * comfortably; the redundancy that matters here is the human's ability to
 * simply re-scan, not the code's tolerance of damage. Nothing is printed. */
const ECC = qrcodegen.QrCode.Ecc.LOW;

/* The spec's mandatory light margin. Below 4 modules, scanners lose the
 * symbol against page furniture. */
const QUIET_ZONE = 4;

/* Explicit, not themed. A scanner reads reflectance, so these must never
 * become currentColor or a CSS variable — that is how a QR silently stops
 * scanning in dark mode. */
const LIGHT = "#ffffff";
const DARK = "#000000";

/* qrMatrix encodes text and returns the module grid.
 *
 * The returned shape is intentionally narrow — size plus a getter — so the
 * upstream QrCode object does not leak into callers and become an implicit
 * part of our API. */
export function qrMatrix(text) {
  const qr = qrcodegen.QrCode.encodeText(String(text ?? ""), ECC);
  return {
    size: qr.size,
    get(x, y) {
      return qr.getModule(x, y);
    },
  };
}

/* qrSVG renders a matrix as a standalone SVG string.
 *
 * One path of one-module squares rather than one rect per module: a version
 * 10 symbol is 3249 modules, and 3249 elements is a page the browser
 * struggles to lay out, especially while the pairing sheet is also
 * re-rendering on a timer. */
export function qrSVG(matrix) {
  const size = matrix.size;
  const span = size + QUIET_ZONE * 2;
  const parts = [];
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      if (matrix.get(x, y)) {
        parts.push(`M${x + QUIET_ZONE},${y + QUIET_ZONE}h1v1h-1z`);
      }
    }
  }
  return (
    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${span} ${span}" ` +
    `shape-rendering="crispEdges" role="img">` +
    `<rect width="100%" height="100%" fill="${LIGHT}"/>` +
    `<path d="${parts.join(" ")}" fill="${DARK}"/>` +
    `</svg>`
  );
}
