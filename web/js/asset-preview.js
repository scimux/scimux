"use strict";

export function createAssetPreview(deps = {}){
  const doc = deps.document;
  const overlay = deps.overlay;
  const revoke = typeof deps.revokeObjectURL === "function" ? deps.revokeObjectURL : () => {};
  const activeElement = typeof deps.activeElement === "function"
    ? deps.activeElement : () => doc && doc.activeElement;
  const image = overlay?.querySelector?.("[data-preview-image]") || null;
  const stage = overlay?.querySelector?.("[data-preview-stage]") || overlay;
  const title = overlay?.querySelector?.("[data-preview-title]") || null;
  const status = overlay?.querySelector?.("[data-preview-status]") || null;
  const download = overlay?.querySelector?.("[data-preview-download]") || null;
  const external = overlay?.querySelector?.("[data-preview-external]") || null;
  const closeButton = overlay?.querySelector?.("[data-preview-close]") || null;
  const resetButton = overlay?.querySelector?.("[data-preview-reset]") || null;
  const listeners = [];
  const pointers = new Map();
  let opened = false, name = "", generation = 0, returnFocus = null;
  let ownedURL = "", transform = { scale: 1, x: 0, y: 0 }, pinch = null;
  let imageHandlers = null;

  function on(target, kind, fn, opts){
    if (!target?.addEventListener) return;
    target.addEventListener(kind, fn, opts);
    listeners.push(() => target.removeEventListener(kind, fn, opts));
  }
  function paint(){
    if (image?.style)
      image.style.transform = `translate(${transform.x}px, ${transform.y}px) scale(${transform.scale})`;
  }
  function reset(){ transform = { scale: 1, x: 0, y: 0 }; pinch = null; paint(); }
  function zoomBy(factor){
    const next = Math.max(1, Math.min(8, transform.scale * Number(factor || 1)));
    transform = { ...transform, scale: next };
    paint();
  }
  function panBy(dx, dy){
    transform = { ...transform, x: transform.x + Number(dx || 0), y: transform.y + Number(dy || 0) };
    paint();
  }
  function clearImageHandlers(){
    if (!imageHandlers || !image?.removeEventListener) return;
    image.removeEventListener("load", imageHandlers.load);
    image.removeEventListener("error", imageHandlers.error);
    imageHandlers = null;
  }
  function releaseOwned(){ if (ownedURL){ revoke(ownedURL); ownedURL = ""; } }

  function close(){
    if (!opened) return;
    opened = false;
    generation++;
    pointers.clear();
    clearImageHandlers();
    if (image?.removeAttribute) image.removeAttribute("src");
    if (overlay) overlay.hidden = true;
    releaseOwned();
    const target = returnFocus;
    returnFocus = null;
    target?.focus?.();
  }

  function open(opts = {}){
    const token = ++generation;
    pointers.clear();
    clearImageHandlers();
    releaseOwned();
    opened = true;
    name = String(opts.name || "image");
    returnFocus = activeElement() || null;
    ownedURL = opts.ownedURL ? String(opts.url || "") : "";
    reset();
    if (overlay) overlay.hidden = false;
    if (title) title.textContent = name;
    if (status){ status.textContent = "Loading image…"; status.hidden = false; }
    const url = String(opts.url || "");
    if (download){ download.href = url; download.download = name; }
    if (external){ external.href = url; }
    if (image){
      const load = () => {
        if (token !== generation || !opened) return;
        if (status) status.hidden = true;
      };
      const error = () => {
        if (token !== generation || !opened) return;
        if (status){ status.textContent = "Image could not be loaded."; status.hidden = false; }
      };
      imageHandlers = { load, error };
      image.addEventListener?.("load", load);
      image.addEventListener?.("error", error);
      image.src = url;
      image.alt = name;
    }
    closeButton?.focus?.();
  }

  function pointerDown(ev){
    pointers.set(ev.pointerId, { x: ev.clientX, y: ev.clientY });
    stage?.setPointerCapture?.(ev.pointerId);
    if (pointers.size === 2){
      const [a, b] = [...pointers.values()];
      pinch = { distance: Math.hypot(a.x - b.x, a.y - b.y), scale: transform.scale };
    }
  }
  function pointerMove(ev){
    const prev = pointers.get(ev.pointerId);
    if (!prev) return;
    pointers.set(ev.pointerId, { x: ev.clientX, y: ev.clientY });
    if (pointers.size === 1 && transform.scale > 1) panBy(ev.clientX - prev.x, ev.clientY - prev.y);
    if (pointers.size === 2 && pinch){
      const [a, b] = [...pointers.values()];
      const distance = Math.hypot(a.x - b.x, a.y - b.y);
      transform = { ...transform, scale: Math.max(1, Math.min(8, pinch.scale * distance / Math.max(1, pinch.distance))) };
      paint();
    }
    ev.preventDefault?.();
  }
  function pointerUp(ev){ pointers.delete(ev.pointerId); if (pointers.size < 2) pinch = null; }
  function keydown(ev){
    if (!opened) return;
    if (ev.key === "Escape" || ev.key === "Esc"){
      ev.preventDefault?.(); ev.stopPropagation?.(); close();
      return;
    }
    if (ev.key !== "Tab") return;
    const focusable = [...(overlay?.querySelectorAll?.("button, a[href]") || [])]
      .filter(el => !el.disabled && !el.hidden);
    if (!focusable.length) return;
    const current = activeElement();
    let index = focusable.indexOf(current);
    index = ev.shiftKey
      ? (index <= 0 ? focusable.length - 1 : index - 1)
      : (index < 0 || index === focusable.length - 1 ? 0 : index + 1);
    ev.preventDefault?.();
    focusable[index].focus?.();
  }
  function destroy(){ close(); while (listeners.length) listeners.pop()(); clearImageHandlers(); releaseOwned(); }
  function state(){ return { open: opened, name, transform: { ...transform } }; }

  on(doc, "keydown", keydown, true);
  on(closeButton, "click", close);
  on(resetButton, "click", reset);
  on(stage, "pointerdown", pointerDown);
  on(stage, "pointermove", pointerMove);
  on(stage, "pointerup", pointerUp);
  on(stage, "pointercancel", pointerUp);
  return { open, close, reset, zoomBy, panBy, destroy, state };
}

export function bindAssetPreviewLinks(root, preview){
  if (!root?.addEventListener || !preview?.open) return () => {};
  const click = ev => {
    const link = ev.target?.closest?.("[data-asset-preview]");
    if (!link) return;
    ev.preventDefault?.();
    preview.open({ url: link.href, name: link.dataset?.name || link.title || "image" });
  };
  root.addEventListener("click", click);
  return () => root.removeEventListener("click", click);
}
