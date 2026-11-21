/* Common attachment tile markup. All scoped paths pass through assetURL. */
export function attachmentTileHTML({name, url, image = false, previewURL = url, key = name, style = "", icon = "", escape, assetURL, fallbackURL = "", note = ""}) {
  if (typeof assetURL !== "function") throw new TypeError("attachment tile requires the asset transport");
  const display = escape(name);
  const resolved = assetURL(image ? previewURL : url);
  if (!resolved) return `<span class="attfile missing" role="status">${display} (unavailable)</span>`;
  const href = escape(resolved);
  return image
    ? `<a class="attthumb" data-asset-preview data-name="${display}" data-tkey="${escape(key)}"${fallbackURL ? ` data-fallback-url="${escape(fallbackURL)}"` : ""}${style} href="${href}" target="_blank" rel="noopener" title="${display}">` +
      `<img src="${href}" alt="${display}" loading="lazy"></a>`
    : `<a class="attfile" href="${href}" target="_blank" rel="noopener" download="${display}" title="${display}">` +
      `${icon}<span>${display}${note ? ` (${escape(note)})` : ""}</span></a>`;
}

export function attachmentUnavailableHTML({name, download, escape, retryPath = "", status = "not retained"}) {
  const display = escape(name);
  const message = download || retryPath ? status : status + "; download unavailable";
  return `<span class="attfile missing" role="status">${display} (${escape(message)})</span>` +
    (download ? ` <a class="attfile" href="${escape(download)}" download="${display}">Retry download</a>` : retryPath ? ` <button type="button" class="attfile" data-attachment-retry="${escape(retryPath)}" data-name="${display}">Retry download</button>` : "");
}

export function replaceFailedAttachmentPreview(anchor, document) {
  const url = anchor?.dataset?.fallbackUrl, name = anchor?.dataset?.name;
  if (url && name && document?.createElement && anchor.replaceWith) {
    const link = document.createElement("a");
    link.className = "attfile"; link.href = url; link.download = name;
    link.textContent = `${name} (preview unavailable)`;
    anchor.replaceWith(link);
    return true;
  }
  anchor?.remove();
  return false;
}

export async function retryAttachmentDownload({button, document, current, retryAsset}) {
  if (!button?.dataset?.attachmentRetry || !button.dataset.name || !document?.createElement || typeof retryAsset !== "function") return false;
  button.disabled = true; button.textContent = "Retrying download…";
  let url = "";
  try { url = await retryAsset(button.dataset.attachmentRetry); } catch { /* show a retryable failure */ }
  if (!current() || button.isConnected === false) return false;
  if (!url) { button.disabled = false; button.textContent = "Retry download (unavailable)"; return false; }
  const link = document.createElement("a");
  link.className = "attfile"; link.href = url; link.download = button.dataset.name;
  link.textContent = button.dataset.name; button.replaceWith(link); link.click();
  return true;
}
