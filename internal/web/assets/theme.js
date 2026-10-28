"use strict";
(() => {
  const system = matchMedia("(prefers-color-scheme: dark)");
  let choice = "dark";
  try {
    choice = localStorage.getItem("lode-theme") || "dark";
  } catch {}
  if (!["auto", "light", "dark"].includes(choice)) choice = "dark";
  const apply = () =>
    (document.documentElement.dataset.theme =
      choice === "auto" ? (system.matches ? "dark" : "light") : choice);
  apply();
  system.addEventListener("change", apply);
  document.addEventListener("DOMContentLoaded", () => {
    for (const input of document.querySelectorAll('input[name="theme"]')) {
      input.checked = input.value === choice;
      input.addEventListener("change", () => {
        choice = input.value;
        try {
          localStorage.setItem("lode-theme", choice);
        } catch {}
        apply();
      });
    }
  });
})();
