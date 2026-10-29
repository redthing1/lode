(() => {
  let theme = "dark";
  try { if (localStorage.getItem("lode-theme") === "light") theme = "light"; } catch {}
  document.documentElement.dataset.theme = theme;
  document.addEventListener("DOMContentLoaded", () => {
    for (const input of document.querySelectorAll('input[name="theme"]')) {
      input.checked = input.value === theme;
      input.addEventListener("change", () => {
        document.documentElement.dataset.theme = input.value;
        try { localStorage.setItem("lode-theme", input.value); } catch {}
      });
    }
  });
})();
