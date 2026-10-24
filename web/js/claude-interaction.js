/* Structured Claude question and plan card.
   The browser posts typed ids only. It does not send hook JSON, paths, or
   permission updates. A 202 is recorded, not delivered to Claude.
   Feedback and free-text limits match the server: 4096 UTF-8 bytes. */

const FEEDBACK_MAX = 4096;
const FREE_TEXT_MAX = 4096;
const PLAN_MODES = new Set(["restore", "default", "acceptEdits"]);

function escText(value){
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

export function interactionSignature(view){
  if (!view || !view.request_id) return "";
  const questions = Array.isArray(view.questions) ? view.questions : [];
  return JSON.stringify({
    request_id: view.request_id,
    phase: view.phase || "",
    tool: view.tool || "",
    native_keys: view.native_keys === true,
    answer_until: view.answer_until || "",
    digest: view.digest || "",
    questions: questions.map(q => ({
      id: q.id,
      text: q.text,
      header: q.header || "",
      multi: q.multi === true,
      preview: q.preview || "",
      options: (q.options || []).map(o => ({
        id: o.id,
        label: o.label,
        description: o.description || "",
      })),
    })),
    plan: view.plan || "",
    modes: (Array.isArray(view.modes) ? view.modes : []).map(m => ({ id: m.id, label: m.label })),
    delivery: view.delivery || "",
  });
}

export function interactionBlocksNativeKeys(view){
  return !!(view && view.request_id && view.native_keys === false);
}

// Human intent and correlated application outcomes retain distinct authorship.
export function manualDecisionRowHTML(surface, options){
  const s = surface || {};
  const d = s.decision || {};
  const application = d.source === "application";
  const manual = application ? d.outcome || {} : d.manual || {};
  const fmtTime = options && typeof options.fmtTime === "function" ? options.fmtTime : null;
  const when = s.time && fmtTime ? escText(fmtTime(s.time)) : "";
  const action = manual.action ? escText(manual.action) : "";
  const feedback = manual.feedback ? escText(manual.feedback) : "";
  const context = manual.app_context ? escText(manual.app_context) : "";
  const reviewed = manual.reviewed ? escText(manual.reviewed) : "";
  const modeLabel = manual.mode_label ? escText(manual.mode_label) : "";
  const delivery = manual.delivery ? escText(String(manual.delivery).replaceAll("_", " ")) : "";
  const selections = Array.isArray(manual.selections) ? manual.selections : [];
  const selectionHTML = selections.map(item => {
    const labels = Array.isArray(item.labels) ? item.labels.map(escText).join(", ") : "";
    const free = item.free_text ? escText(item.free_text) : "";
    const question = item.question ? escText(item.question) : "";
    return `<li>${question}${labels ? ` ${labels}` : ""}${free ? ` ${free}` : ""}</li>`;
  }).join("");
  return `<div class="decision" data-record="${escText(s.record ?? "")}" data-request="${escText(d.request_id || "")}">` +
    `<div class="decision-head"><span class="decision-state">${application ? "Application" : "Manual"}</span>` +
    (action ? ` <span class="decision-action">${action}</span>` : "") +
    `</div>` +
    (reviewed ? `<div class="decision-reviewed">${reviewed}</div>` : "") +
    (feedback ? `<div class="decision-row"><span class="k">Feedback</span> <div class="decision-feedback">${feedback}</div></div>` : "") +
    (context ? `<div class="decision-row"><span class="k">Application context</span> <div class="decision-context">${context}</div></div>` : "") +
    (selectionHTML ? `<ul class="decision-manual">${selectionHTML}</ul>` : "") +
    (modeLabel ? `<div class="decision-row"><span class="k">Mode</span> ${modeLabel}</div>` : "") +
    (delivery ? `<div class="decision-row"><span class="k">Delivery</span> ${delivery}</div>` : "") +
    (when ? `<div class="decision-row"><span class="k">Time</span> ${when}</div>` : "") +
    `</div>`;
}

function make(doc, tag, className){
  const node = doc.createElement(tag);
  if (className) node.className = className;
  return node;
}

function addText(doc, parent, className, text){
  const node = make(doc, "div", className);
  node.textContent = text || "";
  parent.appendChild(node);
  return node;
}

function actionable(view){
  return !!view && (view.phase === "pending" || view.phase === "claimed");
}

export function createClaudeInteractionCard(deps){
  const d = deps || {};
  const doc = d.document;
  let host = null;
  let current = null;
  let viewRef = null;
  let draft = null;
  let inflight = false;
  let holdId = "";
  let statusText = "";
  let skipCapture = false;
  let flight = 0;
  const listeners = [];

  function listen(node, type, fn){
    node.addEventListener(type, fn);
    listeners.push({ node, type, fn });
  }

  function clearListeners(){
    while (listeners.length){
      const item = listeners.pop();
      item.node.removeEventListener(item.type, item.fn);
    }
  }

  function unmount(){
    if (current) invalidateFlight();
    clearListeners();
    if (host) host.remove();
    host = null;
  }

  function blankAnswers(){
    return {};
  }

  function nodeFocused(node){
    return !!(node && doc && doc.activeElement === node);
  }

  function readSelection(node){
    const start = Number(node.selectionStart) || 0;
    const end = node.selectionEnd == null ? start : (Number(node.selectionEnd) || 0);
    return {
      start,
      end,
      direction: node.selectionDirection == null ? "none" : node.selectionDirection,
      focused: nodeFocused(node),
    };
  }

  function writeSelection(node, start, end, direction){
    const dir = direction || "none";
    node.selectionStart = start;
    node.selectionEnd = end;
    node.selectionDirection = dir;
    if (typeof node.setSelectionRange === "function") node.setSelectionRange(start, end, dir);
  }

  function blankAnswer(){
    return { optionIds: [], freeText: "", selectionStart: 0, selectionEnd: 0, selectionDirection: "none", focused: false };
  }

  function readDraft(){
    const answers = blankAnswers();
    for (const input of host.querySelectorAll(".ci-opt")){
      const id = input.dataset.questionId;
      if (!answers[id]) answers[id] = blankAnswer();
      if (input.checked && input.dataset.optionId) answers[id].optionIds.push(input.dataset.optionId);
    }
    for (const input of host.querySelectorAll(".ci-free")){
      const id = input.dataset.questionId;
      if (!answers[id]) answers[id] = blankAnswer();
      const sel = readSelection(input);
      answers[id].freeText = input.value || "";
      answers[id].selectionStart = sel.start;
      answers[id].selectionEnd = sel.end;
      answers[id].selectionDirection = sel.direction;
      if (sel.focused) answers[id].focused = true;
    }
    const feedback = host.querySelector(".ci-feedback");
    const feedbackSel = feedback ? readSelection(feedback) : null;
    let mode = "";
    for (const input of host.querySelectorAll(".ci-mode")){
      if (input.checked) mode = input.dataset.mode || "";
    }
    return {
      answers,
      feedback: feedback ? feedback.value || "" : "",
      feedbackStart: feedbackSel ? feedbackSel.start : 0,
      feedbackEnd: feedbackSel ? feedbackSel.end : 0,
      feedbackDirection: feedbackSel ? feedbackSel.direction : "none",
      feedbackFocused: !!(feedbackSel && feedbackSel.focused),
      mode,
    };
  }

  function capture(){
    if (skipCapture){
      skipCapture = false;
      return;
    }
    if (!host || !current) return;
    draft = readDraft();
  }

  function applyDraft(){
    for (const input of host.querySelectorAll(".ci-opt")){
      const state = draft.answers[input.dataset.questionId];
      input.checked = !!(state && state.optionIds.indexOf(input.dataset.optionId) >= 0);
    }
    for (const input of host.querySelectorAll(".ci-free")){
      const state = draft.answers[input.dataset.questionId];
      if (!state) continue;
      input.value = state.freeText || "";
      writeSelection(input, state.selectionStart || 0, state.selectionEnd || 0, state.selectionDirection);
      if (state.focused) input.focus();
    }
    const feedback = host.querySelector(".ci-feedback");
    if (feedback){
      feedback.value = draft.feedback || "";
      writeSelection(feedback, draft.feedbackStart || 0, draft.feedbackEnd || 0, draft.feedbackDirection);
      if (draft.feedbackFocused) feedback.focus();
    }
    if (draft.mode){
      for (const input of host.querySelectorAll(".ci-mode")){
        input.checked = input.dataset.mode === draft.mode;
      }
    }
  }

  function setStatus(text){
    statusText = text || "";
    if (!host) return;
    const status = host.querySelector(".ci-status");
    if (status) status.textContent = statusText;
  }

  function setBusy(busy){
    const locked = !!busy || holdId === viewRef.request_id || !actionable(viewRef);
    for (const button of host.querySelectorAll(".ci-action")) button.disabled = locked;
  }

  function selectedMode(){
    for (const input of host.querySelectorAll(".ci-mode")){
      if (input.checked) return input.dataset.mode || "";
    }
    return "";
  }

  function answerBody(view){
    const live = readDraft();
    const questions = Array.isArray(view.questions) ? view.questions : [];
    if (questions.length === 0) return { error: "Answer every question before sending." };
    const answers = [];
    for (const question of questions){
      const state = live.answers[question.id] || { optionIds: [], freeText: "" };
      const text = String(state.freeText || "");
      if (!question.multi && text.trim() !== ""){
        if (new TextEncoder().encode(text).length > FREE_TEXT_MAX) return { error: "Free text is too long (maximum 4096 UTF-8 bytes)." };
        answers.push({ question_id: question.id, free_text: text });
        continue;
      }
      const chosen = new Set(state.optionIds);
      const ids = [];
      const options = Array.isArray(question.options) ? question.options : [];
      for (const option of options){
        if (chosen.has(option.id)) ids.push(option.id);
      }
      if (ids.length === 0) return { error: "Answer every question before sending." };
      answers.push({ question_id: question.id, option_ids: ids });
    }
    return { body: { request_id: view.request_id, action: "answer", answers } };
  }

  function feedbackBody(view, action){
    const live = readDraft();
    const text = String(live.feedback || "");
    if (text.trim() === "") return { error: "Feedback is required." };
    if (new TextEncoder().encode(text).length > FEEDBACK_MAX) return { error: "Feedback is too long (maximum 4096 UTF-8 bytes)." };
    return { body: { request_id: view.request_id, action, feedback: text } };
  }

  function build(kind, view){
    if (kind === "answer") return answerBody(view);
    if (kind === "discuss") return feedbackBody(view, "discuss");
    if (kind === "request_changes") return feedbackBody(view, "request_changes");
    if (kind === "approve"){
      const mode = selectedMode();
      if (!PLAN_MODES.has(mode)) return { error: "Choose a mode before approving." };
      return { body: { request_id: view.request_id, action: "approve", mode } };
    }
    if (kind === "cancel") return { body: { request_id: view.request_id, action: "cancel" } };
    return { body: { request_id: view.request_id, action: "terminal" } };
  }

  function sameFlight(token, view, nodeId){
    return token === flight && !!host && !!view && !!current && !!viewRef &&
      current.nodeId === nodeId &&
      current.requestId === view.request_id &&
      viewRef.request_id === view.request_id;
  }

  function invalidateFlight(){
    flight += 1;
    inflight = false;
  }

  async function submit(kind){
    const view = viewRef;
    const nodeId = current && current.nodeId;
    if (inflight || !view || !current) return;
    if (holdId === view.request_id) return;
    if (kind !== "terminal" && !actionable(view)) return;
    const built = build(kind, view);
    if (!built.body){
      setStatus(built.error);
      return;
    }
    const token = flight + 1;
    flight = token;
    inflight = true;
    setBusy(true);
    try {
      await d.api(`/api/nodes/${encodeURIComponent(nodeId)}/claude-interaction`, {
        method: "POST",
        body: JSON.stringify(built.body),
      });
      if (!sameFlight(token, view, nodeId)) return;
      holdId = view.request_id;
      setStatus("Recorded. Not confirmed by Claude.");
    } catch (err) {
      if (!sameFlight(token, view, nodeId)) return;
      if (err && err.status === 409) holdId = view.request_id;
      setStatus(err && err.message ? err.message : "The interaction could not be recorded.");
    } finally {
      if (token !== flight) return;
      inflight = false;
      if (sameFlight(token, view, nodeId)) setBusy(false);
    }
  }

  function bindChoice(input){
    listen(input, "change", () => {
      if (input.type !== "radio") return;
      for (const other of host.querySelectorAll(".ci-opt")){
        if (other.dataset.questionId === input.dataset.questionId && other !== input) other.checked = false;
      }
      for (const free of host.querySelectorAll(".ci-free")){
        if (free.dataset.questionId === input.dataset.questionId) free.value = "";
      }
    });
  }

  function bindFree(input){
    listen(input, "input", () => {
      if (String(input.value || "").trim() === "") return;
      for (const other of host.querySelectorAll(".ci-opt")){
        if (other.dataset.questionId === input.dataset.questionId) other.checked = false;
      }
    });
  }

  function bindMode(input){
    listen(input, "change", () => {
      for (const other of host.querySelectorAll(".ci-mode")){
        if (other !== input) other.checked = false;
      }
    });
  }

  function actionButton(className, label, kind){
    const button = make(doc, "button", `ci-action ${className}`);
    button.type = "button";
    button.textContent = label;
    listen(button, "click", () => { submit(kind); });
    return button;
  }

  function paintQuestion(question){
    const block = make(doc, "fieldset", "ci-q");
    if (question.header) addText(doc, block, "ci-header", question.header);
    addText(doc, block, "ci-text", question.text || "");
    if (question.preview) addText(doc, block, "ci-preview", question.preview);
    const options = Array.isArray(question.options) ? question.options : [];
    for (const option of options){
      const label = make(doc, "label", "ci-option");
      const input = make(doc, "input", "ci-opt");
      input.type = question.multi ? "checkbox" : "radio";
      input.name = `ci-${question.id}`;
      input.dataset.questionId = question.id;
      input.dataset.optionId = option.id;
      label.appendChild(input);
      const name = make(doc, "span", "ci-label");
      name.textContent = option.label || "";
      label.appendChild(name);
      if (option.description){
        const desc = make(doc, "span", "ci-desc");
        desc.textContent = option.description;
        label.appendChild(desc);
      }
      bindChoice(input);
      block.appendChild(label);
    }
    if (!question.multi){
      const free = make(doc, "textarea", "ci-free");
      free.dataset.questionId = question.id;
      bindFree(free);
      block.appendChild(free);
    }
    host.appendChild(block);
  }

  function paint(parent, view){
    clearListeners();
    if (host) host.remove();
    host = make(doc, "div", "ci-card");
    parent.appendChild(host);
    const questions = Array.isArray(view.questions) ? view.questions : [];
    if (view.tool === "ExitPlanMode"){
      addText(doc, host, "ci-plan", view.plan || "");
      const modes = Array.isArray(view.modes) ? view.modes : [];
      for (const mode of modes){
        const label = make(doc, "label", "ci-modelabel");
        const input = make(doc, "input", "ci-mode");
        input.type = "radio";
        input.name = "ci-plan-mode";
        input.dataset.mode = mode.id;
        label.appendChild(input);
        const name = make(doc, "span", "ci-label");
        name.textContent = mode.label || "";
        label.appendChild(name);
        bindMode(input);
        host.appendChild(label);
      }
      const feedback = make(doc, "textarea", "ci-feedback");
      host.appendChild(feedback);
      const actions = make(doc, "div", "ci-actions");
      actions.appendChild(actionButton("ci-approve", "Approve plan", "approve"));
      actions.appendChild(actionButton("ci-changes", "Request changes", "request_changes"));
      actions.appendChild(actionButton("ci-cancel", "Cancel", "cancel"));
      actions.appendChild(actionButton("ci-terminal", "Use terminal", "terminal"));
      host.appendChild(actions);
    } else if (questions.length > 0){
      for (const question of questions) paintQuestion(question);
      const feedback = make(doc, "textarea", "ci-feedback");
      host.appendChild(feedback);
      const actions = make(doc, "div", "ci-actions");
      actions.appendChild(actionButton("ci-answer", "Answer", "answer"));
      actions.appendChild(actionButton("ci-discuss", "Discuss", "discuss"));
      actions.appendChild(actionButton("ci-cancel", "Cancel", "cancel"));
      actions.appendChild(actionButton("ci-terminal", "Use terminal", "terminal"));
      host.appendChild(actions);
    } else {
      addText(doc, host, "ci-text", "Structured choices are not available. Use the terminal.");
      const actions = make(doc, "div", "ci-actions");
      actions.appendChild(actionButton("ci-terminal", "Use terminal", "terminal"));
      host.appendChild(actions);
    }
    const status = make(doc, "div", "ci-status");
    status.textContent = statusText;
    host.appendChild(status);
    setBusy(inflight);
  }

  function update(parent, view, meta){
    const nodeId = meta && meta.nodeId ? meta.nodeId : "";
    if (!parent || !doc || !view || !view.request_id || view.native_keys !== false){
      if (current) invalidateFlight();
      unmount();
      viewRef = null;
      current = null;
      return;
    }
    const same = !!(draft && current && current.nodeId === nodeId && current.requestId === view.request_id);
    if (!same){
      if (current && (current.nodeId !== nodeId || current.requestId !== view.request_id)) invalidateFlight();
      draft = null;
      if (holdId !== view.request_id) setStatus("");
    }
    current = { nodeId, requestId: view.request_id };
    viewRef = view;
    paint(parent, view);
    if (draft) applyDraft();
  }

  function retire(){
    invalidateFlight();
    draft = null;
    current = null;
    viewRef = null;
    holdId = "";
    statusText = "";
    skipCapture = true;
    unmount();
  }

  function destroy(){
    retire();
    skipCapture = false;
  }

  return { capture, update, unmount, destroy, retire };
}
