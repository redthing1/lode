"use strict";

const menus = document.querySelectorAll("details[data-menu]");
document.addEventListener("click", event => {
  for (const menu of menus) if (!menu.contains(event.target)) menu.open = false;
});
document.addEventListener("keydown", event => {
  if (event.key !== "Escape") return;
  for (const menu of menus) {
    if (!menu.open) continue;
    menu.open = false;
    menu.querySelector("summary").focus();
  }
});

for (const button of document.querySelectorAll("button[data-copy]")) {
  let timer;
  button.addEventListener("click", async () => {
    const input = document.getElementById(button.dataset.copy);
    const label = button.querySelector("span");
    clearTimeout(timer);
    try {
      await navigator.clipboard.writeText(input.value);
      label.textContent = "Copied";
    } catch {
      input.focus();
      input.select();
      label.textContent = "Select text";
    }
    timer = setTimeout(() => { label.textContent = "Copy"; }, 2500);
  });
}
