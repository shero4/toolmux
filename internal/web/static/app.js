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
