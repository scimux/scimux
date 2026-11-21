/* Explicit owner preparation reads only retained, source-associated files.
   The caller owns visit/identity checks and the later explicit Agent send. */
export async function sha256Hex(bytes, subtle) {
  return Array.from(new Uint8Array(await subtle.digest("SHA-256", bytes)), byte => byte.toString(16).padStart(2, "0")).join("");
}

export function peopleFileFailureText(message) {
  let code = "";
  try { code = JSON.parse(message)?.error || ""; } catch { /* unknown outcome */ }
  switch (code) {
    case "storage_full": return "This computer's storage is full. Free space before retrying.";
    case "relay_quota": return "The relay's storage quota is full. Free space or contact the relay owner before retrying.";
    case "too_large": return "The file is too large. Choose a smaller file before retrying.";
    case "file_too_large": return "The relay refused this file because it is too large.";
    case "read_only":
    case "relay_forbidden": return "Permission to upload this file was refused.";
    case "relay_unauthorized": return "Relay access was refused. Check the server connection before retrying.";
    case "not_active":
    case "stale_generation": return "The shared chat changed or ended. Review the saved file before retrying.";
    case "channel_unavailable": return "The shared chat is unavailable. Review the saved file before retrying.";
    case "attachments_unsupported": return "This relay does not support safe file transfers.";
    case "copy_instance_required":
    case "copy_identity_unavailable": return "The local copy identity is unavailable. Reconnect and review this file.";
    case "copy_changed": return "This local copy changed. Refresh and review your saved file before retrying.";
    default: return "File delivery is unconfirmed. Review the saved file before retrying.";
  }
}

export async function preparePeopleFiles({source, attachments, copyInstance, fetchImpl, digest, makeFile, signal, current = () => true}) {
  const hex = (value, n) => typeof value === "string" && new RegExp(`^[0-9a-f]{${n}}$`).test(value);
  if (!source || !hex(source.server,32) || !hex(source.owner,32) || !hex(source.chat,16) || !hex(source.generation,16) || source.log !== "comments" || !Number.isSafeInteger(source.seq) || source.seq < 1 || !Array.isArray(attachments) || attachments.length < 1 || attachments.length > 16 || typeof fetchImpl !== "function" || typeof digest !== "function" || typeof makeFile !== "function") throw new Error("Source files are unavailable.");
  const seen = new Set(); let total = 0;
  for (const file of attachments) {
    if (!file || !hex(file.id,16) || !hex(file.sha256,64) || seen.has(file.id) || !Number.isSafeInteger(file.size) || file.size < 1 || file.size > 4194304 || typeof file.name !== "string" || !file.name || file.name === "." || file.name === ".." || /[\\/\x00-\x1f\x7f]/.test(file.name) || new TextEncoder().encode(file.name).length > 255 || typeof file.mime !== "string" || file.mime.length > 255) throw new Error("Source files are unavailable.");
    seen.add(file.id); total += file.size;
    if (total > 16777216) throw new Error("Source files are unavailable.");
  }
  const key = [source.server,source.owner,source.chat,source.generation].join("/");
  const files = [];
  for (const file of attachments) {
    if (signal?.aborted || !current()) throw new Error("Source changed before preparation completed.");
    const path = `/api/team/subscriptions/${key}/attachments/${source.seq}/${file.id}${copyInstance ? "?copy_instance=" + encodeURIComponent(copyInstance) : ""}`;
    const response = await fetchImpl(path, {method:"GET", signal});
    if (!response.ok) throw new Error("Source file is unavailable.");
    const bytes = await response.arrayBuffer();
    if (signal?.aborted || !current()) throw new Error("Source changed before preparation completed.");
    if (bytes.byteLength !== file.size || await digest(bytes) !== file.sha256) throw new Error("Source file is corrupt.");
    if (signal?.aborted || !current()) throw new Error("Source changed before preparation completed.");
    files.push(makeFile(bytes, file.name, {type:file.mime}));
  }
  return files;
}
