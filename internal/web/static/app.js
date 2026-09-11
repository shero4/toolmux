document.documentElement.classList.add("js");

function showMatchingFields(select, attribute) {
  if (!select) return;

  const update = () => {
    document.querySelectorAll(`[${attribute}]`).forEach((element) => {
      const values = element.getAttribute(attribute).split(/\s+/);
      const visible = values.includes(select.value);
      element.hidden = !visible;
      if (visible && element.tagName === "DETAILS") element.open = true;
    });
  };

  select.addEventListener("change", update);
  update();
}

showMatchingFields(document.querySelector("[data-auth-method]"), "data-auth-fields");
showMatchingFields(document.querySelector("[data-connection-kind]"), "data-connection-fields");
showMatchingFields(document.querySelector("[data-tool-kind]"), "data-tool-fields");

document.querySelectorAll("[data-grant-toggle]").forEach((toggle) => {
  toggle.addEventListener("change", async () => {
    const form = toggle.closest("form");
    const row = toggle.closest("[data-grant-row]");
    const state = row.querySelector(".grant-state");
    const previous = !toggle.checked;
    const data = new FormData();

    data.set("visible_tool_id", toggle.value);
    if (toggle.checked) data.set("tool_id", toggle.value);
    data.set("return_url", window.location.pathname + window.location.search);

    toggle.disabled = true;
    row.classList.add("is-saving");
    state.classList.remove("is-error");
    state.textContent = "Saving…";

    try {
      const response = await fetch(form.action, {
        method: "POST",
        body: data,
        headers: { Accept: "application/json", "X-Toolmux-Async": "true" },
      });
      if (!response.ok) throw new Error("grant update failed");
      row.classList.toggle("is-granted", toggle.checked);
      state.textContent = toggle.checked ? "Granted" : "Not granted";
    } catch (_) {
      toggle.checked = previous;
      row.classList.toggle("is-granted", previous);
      state.textContent = "Could not save";
      state.classList.add("is-error");
    } finally {
      row.classList.remove("is-saving");
      toggle.disabled = false;
    }
  });
});
