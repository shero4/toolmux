/* Progressive enhancement for the Toolmux interface. Every page works
   without this script; it adds conditional form fields, confirmations,
   clipboard copy, and immediate saving of tool grants. */
(function () {
  "use strict";

  /* Conditional fields: <select data-controls="x"> shows each element with
     data-when="x:a,b" only while the selected value is one of a or b. */
  document.querySelectorAll("[data-controls]").forEach(function (select) {
    var key = select.getAttribute("data-controls");
    var targets = document.querySelectorAll('[data-when^="' + key + ':"]');
    var update = function () {
      targets.forEach(function (element) {
        var values = element.getAttribute("data-when").slice(key.length + 1).split(",");
        element.hidden = values.indexOf(select.value) === -1;
      });
    };
    select.addEventListener("change", update);
    update();
  });

  /* Option filtering: <select data-filter-by="x"> keeps only the options whose
     data-kind matches the controlling select's value. */
  document.querySelectorAll("[data-filter-by]").forEach(function (select) {
    var controller = document.querySelector('[data-controls="' + select.getAttribute("data-filter-by") + '"]');
    if (!controller) return;
    var update = function () {
      Array.prototype.forEach.call(select.options, function (option) {
        var kind = option.getAttribute("data-kind");
        var visible = !kind || kind === controller.value;
        option.hidden = !visible;
        option.disabled = !visible;
      });
      var selected = select.options[select.selectedIndex];
      if (selected && selected.disabled) select.value = "";
    };
    controller.addEventListener("change", update);
    update();
  });

  /* Confirmations for destructive forms. */
  document.querySelectorAll("form[data-confirm]").forEach(function (form) {
    form.addEventListener("submit", function (event) {
      if (!window.confirm(form.getAttribute("data-confirm"))) event.preventDefault();
    });
  });

  /* Copy buttons: <button data-copy="#selector">. */
  document.querySelectorAll("[data-copy]").forEach(function (button) {
    button.addEventListener("click", function () {
      var target = document.querySelector(button.getAttribute("data-copy"));
      if (!target || !navigator.clipboard) return;
      navigator.clipboard.writeText(target.textContent).then(function () {
        var label = button.textContent;
        button.textContent = "Copied";
        window.setTimeout(function () { button.textContent = label; }, 1500);
      });
    });
  });

  /* Tool grants: each checkbox saves on change; the header checkbox applies
     to every row on the page in one request. */
  var grants = document.querySelector("form[data-grants]");
  if (!grants) return;
  var fallback = grants.querySelector("[data-grants-fallback]");
  if (fallback) fallback.hidden = true;
  var rows = Array.prototype.slice.call(grants.querySelectorAll("[data-grant-row]"));
  var all = grants.querySelector("[data-grant-all]");
  var box = function (row) { return row.querySelector("[data-grant]"); };

  var syncHeader = function () {
    if (!all) return;
    var checked = rows.filter(function (row) { return box(row).checked; }).length;
    all.checked = rows.length > 0 && checked === rows.length;
    all.indeterminate = checked > 0 && checked < rows.length;
  };

  var save = function (changed, previous) {
    var data = new FormData();
    changed.forEach(function (row, index) {
      data.append("visible_tool_id", box(row).value);
      if (box(row).checked) data.append("tool_id", box(row).value);
      row.classList.add("is-saving");
      row.classList.remove("is-error");
      box(row).disabled = true;
      void index;
    });
    fetch(grants.action, { method: "POST", body: data, credentials: "same-origin", headers: { "X-Toolmux-Async": "true" } })
      .then(function (response) {
        if (!response.ok) throw new Error(response.statusText);
        changed.forEach(function (row) { row.classList.toggle("is-granted", box(row).checked); });
      })
      .catch(function () {
        changed.forEach(function (row, index) {
          box(row).checked = previous[index];
          row.classList.toggle("is-granted", previous[index]);
          row.classList.add("is-error");
        });
      })
      .then(function () {
        changed.forEach(function (row) {
          row.classList.remove("is-saving");
          box(row).disabled = false;
        });
        syncHeader();
      });
  };

  rows.forEach(function (row) {
    box(row).addEventListener("change", function () {
      save([row], [!box(row).checked]);
    });
  });
  if (all) {
    all.addEventListener("change", function () {
      var previous = rows.map(function (row) { return box(row).checked; });
      rows.forEach(function (row) { box(row).checked = all.checked; });
      save(rows, previous);
    });
  }
  syncHeader();
})();

/* Rotation is a normal server-rendered page without JS, and a review dialog
   with JS. Credentials are never part of the preview. */
(function () {
  document.querySelectorAll('[data-rotate-dialog]').forEach(function (link) {
    link.addEventListener('click', async function (event) {
      if (!window.HTMLDialogElement) return;
      event.preventDefault();
      try {
        var response = await fetch(link.href, {credentials: 'same-origin'});
        if (!response.ok || response.redirected) throw new Error('Preview unavailable');
        var page = new DOMParser().parseFromString(await response.text(), 'text/html');
        var content = page.querySelector('[data-rotation-content]');
        if (!content) throw new Error('Preview unavailable');
        var dialog = document.createElement('dialog');
        dialog.className = 'rotation-dialog';
        dialog.appendChild(content);
        document.body.appendChild(dialog);
        dialog.querySelector('[data-dialog-cancel]').addEventListener('click', function (e) {e.preventDefault(); dialog.close();});
        dialog.addEventListener('close', function () {dialog.remove();});
        dialog.showModal();
      } catch (_) {window.location.assign(link.href);}
    });
  });
})();

(function () {
  var panel = document.querySelector('[data-codex-status]');
  var summary = document.querySelector('[data-codex-summary]');
  if (!panel && !summary) return;
  var labels = {connected:'Connected',reauthorization_required:'Sign-in required',awaiting_signin:'Waiting for your sign-in',starting:'Starting sign-in…',unavailable:'Local bridge unavailable',unknown:'Checking saved authorization…'};
  async function update() {
    try {
      var response = await fetch('/auth/codex/status', {credentials:'same-origin'});
      if (!response.ok || response.redirected) return;
      var state = await response.json();
      if (summary) summary.textContent = labels[state.status] || 'Unknown';
      if (!panel) return;
      panel.querySelector('[data-codex-label]').textContent = labels[state.status] || 'Unknown';
      panel.querySelector('[data-codex-checked]').textContent = state.checked_at && !state.checked_at.startsWith('0001') ? 'Last checked: ' + new Date(state.checked_at).toLocaleString() : 'Not checked yet';
      var instructions = panel.querySelector('[data-codex-instructions]');
      if (instructions) {
        instructions.hidden = state.status !== 'awaiting_signin' || !state.code;
        panel.querySelector('[data-codex-code]').textContent = state.code || '';
      }
      var button = panel.querySelector('[data-codex-start]');
      if (button) button.disabled = state.running;
    } catch (_) {if (panel) panel.querySelector('[data-codex-label]').textContent = 'Status temporarily unavailable'; if (summary) summary.textContent = 'Status temporarily unavailable';}
    finally {setTimeout(update, 3000);}
  }
  update();
})();
(function () {
  var links = document.querySelectorAll('[data-update-link]');
  if (!links.length) return;
  var wasRunning = false;
  async function refresh() {
    try {
      var response = await fetch('/settings/updates/status', {credentials:'same-origin'});
      if (!response.ok || response.redirected) return;
      var state = await response.json();
      links.forEach(function(link) { link.hidden = !state.available && state.phase !== 'running'; link.textContent = state.phase === 'running' ? 'Updating…' : 'Update available'; });
      var message = document.querySelector('[data-update-message]');
      if (message && state.message) message.textContent = state.message;
      if (message && wasRunning && state.phase !== 'running') location.reload();
      wasRunning = state.phase === 'running';
    } catch (_) { /* The server may be restarting; retry without losing the page. */ }
    finally { setTimeout(refresh, 10000); }
  }
  refresh();
})();
