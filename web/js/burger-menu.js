"use strict";
import { esc } from "./format.js";

/* Menu-only rendering. The update coordinator owns disabled lifetimes; this
   view reuses Start's indeterminate dots without replacing the version tag. */
export function renderMenuCheck(button, busy, label) {
  button.innerHTML = `<span class="ctalabel">${esc(label)}</span>` +
    (busy ? `<span class="ctadots" aria-hidden="true"><i></i><i></i><i></i></span>` : "");
  if (busy) {
    button.setAttribute?.("aria-busy", "true");
    button.setAttribute?.("aria-label", "Checking harness and scimux updates");
  } else {
    button.removeAttribute?.("aria-busy");
    button.removeAttribute?.("aria-label");
  }
}

/* Shared presentation for the version-row check and the combined check.
   Release failures remain beside the installed version and reveal ABOUT. */
export function createMenuReleaseView($, md) {
  return {
    checking() {
      $("#m_result").hidden = false;
      $("#m_reltext").textContent = "Checking scimux updates…";
      $("#m_relurl").hidden = true;
      $("#m_apply").hidden = true;
      $("#m_notes").hidden = true;
    },
    result(info) {
      $("#m_version").textContent = info.current;
      $("#m_result").hidden = false;
      $("#m_reltext").textContent = info.available
        ? info.latest + " available" : "up to date (" + info.latest + ")";
      $("#m_relurl").href = info.url || "https://github.com/scimux/scimux/releases";
      $("#m_relurl").hidden = false;
      $("#m_apply").hidden = !info.available;
      $("#m_notes").innerHTML = info.available ? md(info.notes || "") : "";
      $("#m_notes").hidden = !info.available || !info.notes;
      if (info.available) $("#m_about").open = true;
    },
    failed() {
      $("#m_result").hidden = false;
      $("#m_reltext").textContent = "scimux update check failed — try again";
      $("#m_relurl").hidden = true;
      $("#m_apply").hidden = true;
      $("#m_notes").hidden = true;
      $("#m_about").open = true;
    },
  };
}
