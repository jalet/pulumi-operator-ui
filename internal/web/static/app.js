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
