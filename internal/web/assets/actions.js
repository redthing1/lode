"use strict";

const storageRequests = new Map();
for (const element of document.querySelectorAll("[data-storage]")) {
  const url = element.dataset.storage;
  if (!storageRequests.has(url)) {
    storageRequests.set(url, fetch(url, {credentials: "same-origin"}).then(response => {
      if (!response.ok || response.redirected) throw new Error("Storage unavailable");
      return response.text();
    }));
  }
  storageRequests.get(url).then(html => { element.innerHTML = html; }).catch(() => {
    element.title = "Storage unavailable; reload to retry.";
    for (const value of element.querySelectorAll(".storage-value")) {
      if (value.textContent === "…") value.textContent = "Unavailable";
    }
  });
}

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
