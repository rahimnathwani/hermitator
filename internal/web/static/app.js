// hermitator's client side: live views over server-sent events, the
// terminal, and a few conveniences. Pages work without it; this makes
// them live.
"use strict";

const mounted = new Set();

const mounts = {
  log: mountLog,
  pair: mountPair,
  codex: mountCodex,
  terminal: mountTerminal,
};

function mountAll(root) {
  for (const el of root.querySelectorAll("[data-mount]")) {
    if (el._unmount) continue;
    const fn = mounts[el.dataset.mount];
    if (!fn) continue;
    el._unmount = () => {};
    Promise.resolve(fn(el)).then((unmount) => {
      if (unmount) el._unmount = unmount;
    });
    mounted.add(el);
  }
}

// htmx swaps pages in place, so anything holding a connection has to be
// shut down when its element leaves the document.
function unmountGone() {
  for (const el of mounted) {
    if (!document.contains(el)) {
      el._unmount();
      mounted.delete(el);
    }
  }
}

document.addEventListener("DOMContentLoaded", () => mountAll(document));
document.addEventListener("htmx:load", (e) => mountAll(e.detail.elt));
document.addEventListener("htmx:afterSwap", unmountGone);
window.addEventListener("pagehide", () => mounted.forEach((el) => el._unmount()));

// Forms marked data-reset clear themselves after a successful submit.
document.addEventListener("htmx:afterRequest", (e) => {
  const form = e.detail.elt;
  if (form.matches?.("form[data-reset]") && e.detail.successful) form.reset();
});

// A form with model choices shows the set for the selected harness and
// provider.
function showModels(form) {
  const harness = form.querySelector('input[name="harness"]:checked')?.value;
  const provider = form.querySelector('input[name="provider"]:checked')?.value;
  for (const set of form.querySelectorAll(".model-set")) {
    set.hidden = set.dataset.harness !== harness || (harness === "pi" && set.dataset.provider !== provider);
  }
}

// Forms marked data-track show unsaved changes: each changed field is
// marked, and Save only works once there is something to save. The
// browser keeps the values the page was served with as the defaults.
function changed(el) {
  if (el.type === "radio" || el.type === "checkbox") return el.checked !== el.defaultChecked;
  if (el.type === "hidden" || el.type === "submit" || el.type === "reset" || el.tagName === "BUTTON") return false;
  return el.value !== el.defaultValue;
}

function track(form) {
  for (const field of form.querySelectorAll(".field")) {
    field.classList.toggle("changed", [...field.querySelectorAll("input, textarea")].some(changed));
  }
  const dirty = [...form.elements].some(changed);
  form.classList.toggle("dirty", dirty);
  for (const b of form.querySelectorAll("[data-save]")) b.disabled = !dirty;
}

function trackAll(root) {
  const forms = [...root.querySelectorAll("form[data-track]")];
  if (root.matches?.("form[data-track]")) forms.push(root);
  forms.forEach(track);
}

document.addEventListener("DOMContentLoaded", () => trackAll(document));
document.addEventListener("htmx:load", (e) => trackAll(e.detail.elt));

document.addEventListener("change", (e) => {
  const form = e.target.form;
  if (!form) return;
  if (e.target.matches('input[name="harness"], input[name="provider"]')) showModels(form);
  if (e.target.value === "+other") e.target.closest(".model-set")?.querySelector(".other")?.focus();
  if (form.matches("[data-track]")) track(form);
});

document.addEventListener("input", (e) => {
  if (e.target.form?.matches("[data-track]")) track(e.target.form);
});

// "reset" fires before the fields go back, so look once they have.
document.addEventListener("reset", (e) => {
  const form = e.target;
  setTimeout(() => {
    for (const t of form.querySelectorAll("textarea")) t.style.height = "";
    showModels(form);
    if (form.matches("[data-track]")) track(form);
  });
});

// Cmd/Ctrl+Enter sends from a textarea.
document.addEventListener("keydown", (e) => {
  if (e.key === "Enter" && (e.metaKey || e.ctrlKey) && e.target.matches("textarea")) {
    e.preventDefault();
    e.target.form?.requestSubmit();
  }
});

// Click to copy: a .copy element copies its text, a [data-copy] button
// copies the attribute.
document.addEventListener("click", async (e) => {
  const el = e.target.closest(".copy, [data-copy]");
  if (!el) return;
  try {
    await navigator.clipboard.writeText(el.dataset.copy ?? el.textContent.trim());
    el.classList.add("copied");
    setTimeout(() => el.classList.remove("copied"), 1200);
  } catch {}
});

// Destructive actions ask first. A form marked data-confirm opens a dialog
// with its title, its <template data-confirm-body>, and the confirm
// button's label. With data-confirm-type, the button stays off until that
// text is typed, and the form sends it as data-confirm-field for the
// server to check too. data-confirm-when names a selector that must match
// for the form to ask at all, such as a changed harness.
document.addEventListener(
  "submit",
  (e) => {
    const form = e.target;
    if (!(form instanceof HTMLFormElement) || form.dataset.confirm === undefined) return;
    if (form._confirmed) {
      form._confirmed = false;
      return;
    }
    if (form.dataset.confirmWhen && !form.querySelector(form.dataset.confirmWhen)) return;
    const dialog = document.querySelector("[data-confirm-dialog]");
    if (!dialog) return;
    // Capturing at the document, this runs before htmx sees the submit.
    e.preventDefault();
    e.stopImmediatePropagation();
    ask(dialog, form, e.submitter);
  },
  true,
);

function ask(dialog, form, submitter) {
  const $ = (sel) => dialog.querySelector(sel);
  const want = form.dataset.confirmType;
  const ok = $("[data-ok]");
  const input = $("[data-type-input]");
  $("[data-title]").textContent = form.dataset.confirmTitle || "Are you sure?";
  const body = form.querySelector("template[data-confirm-body]");
  $("[data-body]").replaceChildren(body ? body.content.cloneNode(true) : "");
  ok.textContent = form.dataset.confirm || "Confirm";
  $("[data-type]").hidden = !want;
  $("[data-type-label]").textContent = form.dataset.confirmTypeLabel || `Type ${want} to confirm.`;
  input.value = "";
  const check = () => (ok.disabled = Boolean(want) && input.value.trim() !== want);
  input.oninput = check;
  check();
  dialog.returnValue = "";
  dialog.onclose = () => {
    if (dialog.returnValue !== "ok") return;
    const name = form.dataset.confirmField;
    if (name) {
      let proof = form.querySelector(`input[type="hidden"][name="${name}"]`);
      if (!proof) form.append((proof = h("input", { type: "hidden", name })));
      proof.value = input.value.trim();
    }
    form._confirmed = true;
    form.requestSubmit(submitter ?? undefined);
  };
  dialog.showModal();
  // Cancel has the focus, so Enter on its own never destroys anything.
  (want ? input : $("[data-cancel]")).focus();
}

// A click on the backdrop cancels.
document.addEventListener("click", (e) => {
  if (e.target.matches?.("dialog.modal[open]")) e.target.close();
});

// The flash message is a toast that leaves on its own, or when dismissed.
function toast(root) {
  for (const t of root.querySelectorAll?.("[data-toast]") ?? []) {
    if (t._timer) continue;
    const leave = () => {
      t.classList.add("leaving");
      setTimeout(() => t.remove(), 300);
    };
    t._timer = setTimeout(leave, 5000);
    t.querySelector("[data-dismiss]")?.addEventListener("click", leave);
  }
}
document.addEventListener("DOMContentLoaded", () => toast(document));
document.addEventListener("htmx:load", (e) => toast(e.detail.elt.ownerDocument ?? document));

// The message box grows with what you type.
document.addEventListener("input", (e) => {
  if (!e.target.matches(".composer textarea")) return;
  e.target.style.height = "auto";
  e.target.style.height = `${e.target.scrollHeight}px`;
});

function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") el.className = v;
    else el.setAttribute(k, v);
  }
  el.append(...children.filter((c) => c != null));
  return el;
}

function data(e) {
  try {
    return JSON.parse(e.data);
  } catch {
    return null;
  }
}

const stateLabels = { running: "awake", checkpointed: "checkpointed", stopped: "stopped" };

function setState(label) {
  for (const el of document.querySelectorAll("[data-agent-state]")) {
    el.replaceChildren(h("span", { class: `state state-${label}` }, label[0].toUpperCase() + label.slice(1)));
  }
  // Stop turn only makes sense while one is running.
  for (const el of document.querySelectorAll("[data-busy-only]")) el.hidden = label !== "working";
}

function mountLog(el) {
  const src = new EventSource(el.dataset.src);
  let state = "stopped";
  let busy = false;

  const add = (kind, ...body) => {
    el.querySelector(".empty-log")?.remove();
    const when = new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23" });
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
    el.append(h("div", { class: `entry ${kind}` }, h("span", { class: "when" }, when), h("div", { class: "body" }, ...body)));
    while (el.children.length > 300) el.firstElementChild.remove();
    if (atBottom) el.scrollTop = el.scrollHeight;
  };
  const show = () => setState(busy ? "working" : stateLabels[state] || state);

  src.addEventListener("state", (e) => {
    const ev = data(e);
    if (!ev) return;
    state = ev.state;
    show();
  });
  src.addEventListener("turn_start", (e) => {
    const ev = data(e);
    busy = true;
    show();
    add("turn", ev.input || "(attachment)");
  });
  src.addEventListener("event", (e) => {
    const ev = data(e)?.event;
    if (!ev) return;
    switch (ev.type) {
      case "started":
        if (ev.model) add("meta muted", `model ${ev.model}`);
        break;
      case "tool":
        add("tool", h("span", { class: "name" }, ev.tool), ev.detail ? " " + ev.detail : "");
        break;
      case "text":
        add("text", ev.text);
        break;
      case "sent":
        add("sent", "📎 sent " + ev.text.replace(/^\d+-/, ""));
        break;
    }
  });
  src.addEventListener("turn_end", (e) => {
    const ev = data(e);
    busy = false;
    show();
    add(`end end-${ev.status}`, ev.status === "ok" ? ev.output || "(no reply)" : `${ev.status}: ${ev.output || ""}`);
  });
  return () => src.close();
}

function mountPair(el) {
  const src = new EventSource(el.dataset.src);
  const qr = el.querySelector("[data-qr]");
  const code = el.querySelector("[data-code]");
  const status = document.querySelector("[data-status]");
  const say = (text) => status && (status.textContent = text);

  src.addEventListener("qr", (e) => {
    const ev = data(e);
    qr.replaceChildren(h("img", { src: ev.qr, alt: "WhatsApp pairing code", width: 320, height: 320 }));
  });
  const showCode = (text) => {
    code.textContent = text;
    code.closest("[data-code-box]").hidden = false;
  };
  src.addEventListener("code", (e) => showCode(data(e).code));
  src.addEventListener("success", () => {
    say("Linked. Taking you back…");
    src.close();
    setTimeout(() => (location.href = "/accounts"), 700);
  });
  // EventSource also fires "error" for dropped connections; only ours carry data.
  src.addEventListener("error", (e) => {
    if (!e.data) return;
    say(data(e).message);
    src.close();
  });

  const form = el.querySelector("[data-pair-phone]");
  form?.addEventListener("submit", async (e) => {
    e.preventDefault();
    say("Asking WhatsApp for a code…");
    const res = await fetch(form.action, { method: "POST", body: new FormData(form) });
    const text = await res.text();
    if (res.ok) {
      showCode(text);
      say("Type this code on your phone.");
    } else {
      say(text);
    }
  });
  return () => src.close();
}

function mountCodex(el) {
  const start = el.querySelector("[data-start]");
  const device = el.querySelector("[data-device]");
  const message = el.querySelector("[data-message]");
  let src;
  start.addEventListener("click", () => {
    start.disabled = true;
    message.textContent = "Starting the login inside your sandbox…";
    src = new EventSource(el.dataset.src);
    src.addEventListener("device_code", (e) => {
      const ev = data(e);
      const link = device.querySelector("[data-url]");
      link.href = ev.verification_uri;
      link.textContent = ev.verification_uri;
      device.querySelector("[data-user-code]").textContent = ev.user_code;
      device.hidden = false;
      message.textContent = "";
    });
    src.addEventListener("info", (e) => (message.textContent = data(e).message));
    src.addEventListener("done", () => {
      src.close();
      message.textContent = "ChatGPT is connected.";
      setTimeout(() => location.reload(), 800);
    });
    src.addEventListener("error", (e) => {
      if (!e.data) return;
      src.close();
      device.hidden = true;
      start.disabled = false;
      message.textContent = data(e).message;
    });
  });
  return () => src?.close();
}

function loadScript(url) {
  return new Promise((resolve, reject) => {
    if (document.querySelector(`script[src="${url}"]`)) return resolve();
    const s = h("script", { src: url });
    s.onload = resolve;
    s.onerror = reject;
    document.head.append(s);
  });
}

async function mountTerminal(el) {
  await loadScript(el.dataset.xterm);
  await loadScript(el.dataset.fit);
  const term = new Terminal({
    cursorBlink: true,
    fontFamily: getComputedStyle(document.documentElement).getPropertyValue("--mono"),
    fontSize: 14,
    theme: {
      background: "#0b0d0e",
      scrollbarSliderBackground: "rgba(255, 255, 255, 0.12)",
      scrollbarSliderHoverBackground: "rgba(255, 255, 255, 0.22)",
      scrollbarSliderActiveBackground: "rgba(255, 255, 255, 0.3)",
    },
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(el);
  fit.fit();

  const scheme = location.protocol === "https:" ? "wss:" : "ws:";
  const ws = new WebSocket(`${scheme}//${location.host}${el.dataset.src}`);
  ws.binaryType = "arraybuffer";
  const encoder = new TextEncoder();
  const resize = () => {
    fit.fit();
    if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
  };
  term.write("\x1b[2mWaking the sandbox…\x1b[0m\r\n");
  ws.onopen = () => {
    resize();
    term.focus();
  };
  ws.onmessage = (e) => term.write(new Uint8Array(e.data));
  ws.onclose = () => term.write("\r\n\x1b[2m[disconnected]\x1b[0m\r\n");
  term.onData((d) => ws.readyState === WebSocket.OPEN && ws.send(encoder.encode(d)));
  window.addEventListener("resize", resize);
  return () => {
    window.removeEventListener("resize", resize);
    ws.close();
    term.dispose();
  };
}
