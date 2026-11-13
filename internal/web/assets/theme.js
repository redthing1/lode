(() => {
  const themes = ["dark", "light", "charcoal", "rose"];
  let theme = "dark";
  const applyTheme = value => {
    theme = themes.includes(value) ? value : "dark";
    document.documentElement.dataset.theme = theme;
    for (const control of document.querySelectorAll('[name="theme"]')) {
      if (control.tagName === "SELECT") control.value = theme;
      else control.checked = control.value === theme;
    }
  };
  try { applyTheme(localStorage.getItem("lode-theme")); } catch { applyTheme(theme); }
  document.addEventListener("DOMContentLoaded", () => {
    applyTheme(theme);
    for (const control of document.querySelectorAll('[name="theme"]')) {
      control.addEventListener("change", () => {
        applyTheme(control.value);
        try { localStorage.setItem("lode-theme", theme); } catch {}
      });
    }
  });
  window.addEventListener("storage", event => {
    if (event.key === "lode-theme" || event.key === null) applyTheme(event.newValue);
  });
})();
