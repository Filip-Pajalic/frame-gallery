// Frame Gallery: every button with data-api posts once and says what happened.
// Nothing here runs on a timer; the TV is only contacted when a button is pressed.
(function () {
  "use strict";
  const status = () => document.getElementById("status");

  function say(kind, text, list) {
    const el = status();
    if (!el) return;
    el.hidden = false;
    el.className = "status is-" + kind;
    el.textContent = "";
    const dot = document.createElement("span");
    dot.className = "dot" + (kind === "busy" ? " is-off" : "");
    el.appendChild(dot);
    const body = document.createElement("div");
    body.textContent = text;
    if (list && list.length) {
      const ul = document.createElement("ul");
      for (const li of list) {
        const x = document.createElement("li");
        x.textContent = li;
        ul.appendChild(x);
      }
      body.appendChild(ul);
    }
    el.appendChild(body);
  }

  async function post(url, body) {
    let res, data;
    try {
      res = await fetch(url, { method: "POST", body: body || new FormData() });
      data = await res.json();
    } catch (e) {
      return { ok: false, message: "No answer from Frame Gallery: " + e.message };
    }
    return data;
  }

  function after(data) {
    if (data.redirect) location.href = data.redirect;
    else if (data.reload) setTimeout(() => location.reload(), 900);
  }

  async function run(el, body) {
    if (el.dataset.confirm && !confirm(el.dataset.confirm)) return;
    const buttons = el.tagName === "FORM" ? el.querySelectorAll("button") : [el];
    buttons.forEach((b) => (b.disabled = true));
    const label = el.tagName === "FORM" ? "Saving…" : (el.textContent.trim() + "…");
    say("busy", label);
    const data = await post(el.dataset.api, body);
    buttons.forEach((b) => (b.disabled = false));
    say(data.ok ? "ok" : "bad", data.message);
    if (data.ok) {
      after(data);
      if (!data.redirect && !data.reload && el.dataset.api.indexOf("/api/item/") === 0 && !el.closest("form")) {
        setTimeout(() => location.reload(), 900); // TV state of this picture changed
      }
    }
  }

  document.addEventListener("click", (ev) => {
    const el = ev.target.closest("button[data-api]");
    if (!el || el.closest("form[data-api]")) return;
    ev.preventDefault();
    const body = new FormData();
    if (el.dataset.on !== undefined) body.set("on", el.dataset.on);
    run(el, body);
  });

  document.addEventListener("submit", (ev) => {
    const form = ev.target.closest("form[data-api]");
    if (!form) return;
    ev.preventDefault();
    run(form, new FormData(form));
  });

  document.addEventListener("change", async (ev) => {
    const box = ev.target.closest("input[data-coll]");
    if (!box) return;
    const body = new FormData();
    body.set("collection", box.dataset.coll);
    body.set("in", box.checked ? "1" : "0");
    const data = await post("/api/item/" + box.dataset.item + "/collection", body);
    if (!data.ok) { box.checked = !box.checked; say("bad", data.message); }
  });

  // ---- Reframed Gallery: import one picture ----
  document.addEventListener("click", async (ev) => {
    const b = ev.target.closest("button[data-import]");
    if (!b) return;
    ev.preventDefault();
    b.disabled = true;
    b.textContent = "Importing…";
    const body = new FormData();
    body.set("path", b.dataset.import);
    const data = await post("/api/browse/import", body);
    if (data.ok && data.id) {
      const a = document.createElement("a");
      a.className = "ontv is-ok";
      a.href = "/item/" + data.id;
      a.innerHTML = '<span class="dot"></span>in library';
      b.replaceWith(a);
    } else {
      b.disabled = false;
      b.textContent = "Import";
    }
    say(data.ok ? "ok" : "bad", data.message);
  });

  // ---- import: file picker and drag-and-drop ----
  const drop = document.getElementById("drop");
  if (!drop) return;
  const input = document.getElementById("files");

  async function importFiles(files) {
    if (!files.length) return;
    const body = new FormData();
    for (const f of files) body.append("files", f);
    if (drop.dataset.collection && drop.dataset.collection !== "0") body.set("collection", drop.dataset.collection);
    say("busy", "Importing " + files.length + (files.length === 1 ? " picture…" : " pictures…"));
    const data = await post("/api/import", body);
    const lines = (data.results || []).map((r) => r.ok ? r.name + ": imported, " + r.note : r.name + ": refused. " + r.reason);
    say(data.ok ? "ok" : "bad", data.message, lines);
    if ((data.results || []).some((r) => r.ok)) setTimeout(() => location.reload(), 2500);
  }

  input.addEventListener("change", () => { importFiles(input.files); input.value = ""; });
  ["dragenter", "dragover"].forEach((t) => document.addEventListener(t, (e) => {
    if (!e.dataTransfer || !Array.from(e.dataTransfer.types).includes("Files")) return;
    e.preventDefault();
    drop.classList.add("is-over");
  }));
  ["dragleave", "drop"].forEach((t) => document.addEventListener(t, (e) => {
    if (t === "dragleave" && e.relatedTarget) return;
    drop.classList.remove("is-over");
  }));
  document.addEventListener("drop", (e) => {
    if (!e.dataTransfer || !e.dataTransfer.files.length) return;
    e.preventDefault();
    importFiles(e.dataTransfer.files);
  });
})();
