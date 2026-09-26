"use strict";
// Copy buttons: <button type="button" data-copy="value">. One delegated listener and no
// inline handlers, so the CSP's script-src 'self' holds.
document.addEventListener("click", (event) => {
  const button = event.target.closest("[data-copy]");
  if (!button || !navigator.clipboard) {
    return;
  }
  const label = button.textContent;
  navigator.clipboard.writeText(button.dataset.copy).then(() => {
    button.textContent = "Copied";
    setTimeout(() => { button.textContent = label; }, 1500);
  });
});
// The rail is re-rendered on live updates; reopen the preview folds the viewer had open.
let openFolds = [];
document.addEventListener("htmx:beforeSwap", (event) => {
  openFolds = Array.from(event.detail.target.querySelectorAll("details.rail-fold[open]"),
    (d) => d.id);
});
document.addEventListener("htmx:afterSwap", () => {
  for (const id of openFolds) {
    document.getElementById(id)?.setAttribute("open", "");
  }
  openFolds = [];
});
