"use strict";

// Enhance single selects while keeping their values and ordinary form submission.
(() => {
  let opened;
  document.addEventListener("pointerdown", (event) => {
    if (opened && !opened.root.contains(event.target)) opened.close(true);
  });
  window.addEventListener("resize", () => opened?.close(false));

  for (const [id, select] of [...document.querySelectorAll("select")].entries()) {
    if (select.multiple || select.size > 1 || !select.options.length) continue;
    const options = [...select.options];
    const enabled = options.map((o, i) => i).filter((i) => !options[i].disabled);
    if (!enabled.length) continue;
    const root = document.createElement("span");
    root.className = "select";
    const button = document.createElement("button");
    button.type = "button";
    button.className = "select-trigger";
    button.disabled = select.disabled;
    button.setAttribute("role", "combobox");
    button.setAttribute("aria-expanded", "false");
    button.setAttribute("aria-controls", `select-${id}`);
    const label = select.labels[0];
    const caption = label?.cloneNode(true);
    caption?.querySelectorAll("select").forEach((node) => node.remove());
    button.setAttribute(
      "aria-label",
      select.getAttribute("aria-label") || caption?.textContent.trim() || select.name,
    );
    const value = document.createElement("span");
    button.append(value);
    const list = document.createElement("span");
    list.id = `select-${id}`;
    list.className = "select-options";
    list.setAttribute("role", "listbox");
    list.setAttribute("aria-label", button.getAttribute("aria-label"));
    list.hidden = true;
    let active = select.selectedIndex;
    let typed = "";
    let typedAt = 0;
    const items = options.map((option, index) => {
      const item = document.createElement("span");
      item.id = `select-${id}-${index}`;
      item.setAttribute("role", "option");
      item.textContent = option.label.trim();
      if (option.disabled) item.setAttribute("aria-disabled", "true");
      item.addEventListener("pointerdown", (event) => event.preventDefault());
      item.addEventListener("click", (event) => {
        event.preventDefault();
        if (option.disabled) return;
        active = index;
        close(true);
        button.focus();
      });
      list.append(item);
      return item;
    });
    root.append(button, list);
    select.after(root);
    select.hidden = true;
    // A wrapping label must have only one labelable control: the new button.
    if (label?.contains(select)) label.after(select);
    label?.addEventListener("click", (event) => {
      if (event.target === event.currentTarget) {
        event.preventDefault();
        button.focus();
      }
    });

    function sync() {
      value.textContent = options[select.selectedIndex]?.label.trim() || "";
      items.forEach((item, i) =>
        item.setAttribute("aria-selected", i === select.selectedIndex),
      );
    }
    function highlight(index) {
      active = index;
      items.forEach((item, i) => item.classList.toggle("active", i === active));
      button.setAttribute("aria-activedescendant", items[active].id);
      items[active].scrollIntoView({ block: "nearest" });
    }
    function close(accept) {
      if (list.hidden) return;
      list.hidden = true;
      button.setAttribute("aria-expanded", "false");
      button.removeAttribute("aria-activedescendant");
      opened = null;
      typed = "";
      if (accept && active !== select.selectedIndex) {
        select.selectedIndex = active;
        sync();
        select.dispatchEvent(new Event("input", { bubbles: true }));
        select.dispatchEvent(new Event("change", { bubbles: true }));
      }
    }
    function open() {
      if (!list.hidden) return;
      opened?.close(true);
      opened = { root, close };
      const rect = button.getBoundingClientRect();
      const below = innerHeight - rect.bottom - 8;
      const above = below < 200 && rect.top > below;
      root.classList.toggle("above", above);
      list.style.maxHeight = `${Math.max(80, Math.min(320, (above ? rect.top : below) - 8))}px`;
      list.hidden = false;
      button.setAttribute("aria-expanded", "true");
      highlight(enabled.includes(select.selectedIndex) ? select.selectedIndex : enabled[0]);
    }
    button.addEventListener("click", () => {
      list.hidden ? open() : close(false);
    });
    root.addEventListener("focusout", (event) => {
      if (!root.contains(event.relatedTarget)) close(true);
    });
    button.addEventListener("keydown", (event) => {
      const key = event.key;
      if (Date.now() - typedAt > 700) typed = "";
      if (key === "Tab") {
        close(true);
        return;
      }
      if (key === "Escape") {
        if (!list.hidden) {
          event.preventDefault();
          close(false);
        }
        return;
      }
      if (key === "Enter" || (key === " " && !typed)) {
        event.preventDefault();
        list.hidden ? open() : close(true);
      } else if (["ArrowDown", "ArrowUp", "Home", "End"].includes(key)) {
        event.preventDefault();
        const wasClosed = list.hidden;
        open();
        const index = enabled.indexOf(active);
        if (key === "Home") highlight(enabled[0]);
        else if (key === "End") highlight(enabled.at(-1));
        else if (wasClosed && key === "ArrowUp") highlight(enabled[0]);
        else if (!wasClosed) {
          const next = index + (key === "ArrowDown" ? 1 : -1);
          highlight(enabled[Math.max(0, Math.min(enabled.length - 1, next))]);
        }
      } else if (key.length === 1 && !event.ctrlKey && !event.metaKey && !event.altKey) {
        event.preventDefault();
        open();
        const now = Date.now();
        typed = (now - typedAt > 700 ? "" : typed) + key.toLowerCase();
        typedAt = now;
        const prefix = [...typed].every((c) => c === typed[0]) ? typed[0] : typed;
        const index = enabled.indexOf(active);
        for (let step = 1; step <= enabled.length; step++) {
          const next = enabled[(index + step) % enabled.length];
          if (options[next].label.trim().toLowerCase().startsWith(prefix)) {
            highlight(next);
            break;
          }
        }
      }
    });
    select.addEventListener("change", sync);
    select.form?.addEventListener("reset", () => {
      close(false);
      setTimeout(sync, 0);
    });
    sync();
  }
})();
