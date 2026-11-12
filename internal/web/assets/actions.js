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
for (const picker of document.querySelectorAll("[data-picker]")) {
  const summary = picker.querySelector("summary");
  const close = () => { picker.open = false; summary.focus(); };
  picker.addEventListener("toggle", () => {
    if (picker.open) picker.querySelector("input:checked").focus();
  });
  picker.addEventListener("change", event => {
    summary.firstElementChild.textContent = event.target.nextElementSibling.textContent;
  });
  picker.addEventListener("click", event => {
    if (event.target.matches("input") && event.detail > 0) close();
  });
  picker.addEventListener("keydown", event => {
    if (event.target === summary && ["ArrowDown", "ArrowUp"].includes(event.key)) {
      event.preventDefault();
      picker.open = true;
    } else if (event.target.matches("input") && ["Enter", " "].includes(event.key)) {
      event.preventDefault();
      close();
    }
  });
}
document.addEventListener("focusin", event => {
  for (const menu of menus) if (!menu.contains(event.target)) menu.open = false;
});
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

for (const button of document.querySelectorAll("button[data-copy], button[data-copy-text]")) {
  const input = button.dataset.copy ? document.getElementById(button.dataset.copy) : null;
  const label = button.querySelector("[aria-live]");
  const originalLabel = label.textContent;
  const originalTitle = button.getAttribute("title");
  let timer;
  button.addEventListener("click", async () => {
    clearTimeout(timer);
    delete button.dataset.copied;
    try {
      await navigator.clipboard.writeText(input ? input.value : button.dataset.copyText);
      button.dataset.copied = "true";
      label.textContent = "Copied";
      button.title = "Copied";
    } catch {
      if (input) {
        input.focus();
        input.select();
      }
      label.textContent = input ? "Select text" : "Unable to copy";
      button.title = label.textContent;
    }
    timer = setTimeout(() => {
      delete button.dataset.copied;
      label.textContent = originalLabel;
      if (originalTitle === null) button.removeAttribute("title");
      else button.title = originalTitle;
    }, 2500);
  });
}

// Recent activity uses relative wording; older activity keeps a calendar date.
const relativeTimes = document.querySelectorAll("time[data-relative-time]");
const relativeFormat = new Intl.RelativeTimeFormat("en", {numeric: "auto"});
const calendarFormat = new Intl.DateTimeFormat("en", {month: "short", day: "numeric", year: "numeric", timeZone: "UTC"});
function updateRelativeTimes() {
  const now = Date.now();
  for (const element of relativeTimes) {
    const date = new Date(element.dateTime);
    if (!Number.isFinite(date.getTime())) continue;
    const seconds = (date.getTime() - now) / 1000;
    const age = Math.abs(seconds);
    let text;
    if (age >= 30 * 86400) {
      text = calendarFormat.format(date);
    } else if (age < 60) {
      text = "just now";
    } else {
      const [unit, length] = age < 3600 ? ["minute", 60]
        : age < 86400 ? ["hour", 3600]
        : age < 7 * 86400 ? ["day", 86400] : ["week", 7 * 86400];
      text = relativeFormat.format(Math.trunc(seconds / length), unit);
    }
    element.textContent = text;
  }
}
updateRelativeTimes();
if (relativeTimes.length) setInterval(updateRelativeTimes, 30000);

const projectFilter = document.querySelector("[data-project-filter]");
if (projectFilter) {
  const input = projectFilter.querySelector('input[name="q"]');
  const rows = [...document.querySelectorAll("[data-project-name]")];
  const count = document.querySelector(".project-count");
  const empty = document.querySelector("[data-project-empty]");
  const clear = empty.querySelector("[data-clear-filter]");
  const filterProjects = () => {
    const value = input.value.trim();
    const query = value.toLowerCase();
    let matches = 0;
    for (const row of rows) {
      row.hidden = !row.dataset.projectName.toLowerCase().includes(query);
      if (!row.hidden) matches++;
    }
    count.textContent = matches;
    empty.hidden = matches !== 0;
    empty.querySelector("span").textContent = query ? "No matching projects." : "No projects yet.";
    clear.hidden = !query;
    const url = new URL(location.href);
    if (value) url.searchParams.set("q", value);
    else url.searchParams.delete("q");
    if (url.href !== location.href) history.replaceState(history.state, "", url);
    for (const link of document.querySelectorAll(".project-sort a")) {
      const target = new URL(link.href);
      if (value) target.searchParams.set("q", value);
      else target.searchParams.delete("q");
      link.href = target.pathname + target.search;
    }
  };
  input.addEventListener("input", filterProjects);
  projectFilter.addEventListener("submit", event => { event.preventDefault(); filterProjects(); });
  clear.addEventListener("click", event => {
    event.preventDefault();
    input.value = "";
    filterProjects();
    input.focus();
  });
  window.addEventListener("popstate", () => {
    input.value = new URL(location.href).searchParams.get("q") || "";
    filterProjects();
  });
}
