"use strict";
(() => {
  const system = matchMedia("(prefers-color-scheme: dark)");
  let choice = "auto";
  try {
    choice = localStorage.getItem("lode-theme") || "auto";
  } catch {}
  if (!["auto", "light", "dark"].includes(choice)) choice = "auto";
  const apply = () =>
    (document.documentElement.dataset.theme =
      choice === "auto" ? (system.matches ? "dark" : "light") : choice);
  apply();
  system.addEventListener("change", apply);
  document.addEventListener("DOMContentLoaded", () => {
    const select = document.querySelector("#theme");
    select.value = choice;
    select.addEventListener("change", () => {
      choice = select.value;
      try {
        localStorage.setItem("lode-theme", choice);
      } catch {}
      apply();
    });
  });
})();
