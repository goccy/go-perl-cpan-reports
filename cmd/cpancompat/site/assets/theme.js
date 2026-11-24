// The theme button switches the page between light and dark and remembers
// the choice; until one is made the page follows the system's setting. The
// head of every page applies a remembered choice before the page is drawn.
(function () {
  "use strict";
  var root = document.documentElement;
  var system = window.matchMedia("(prefers-color-scheme: dark)");
  var button = document.getElementById("theme");

  function current() {
    return root.dataset.theme || (system.matches ? "dark" : "light");
  }
  function label() {
    var next = current() === "dark" ? "light" : "dark";
    button.textContent = next === "dark" ? "Dark mode" : "Light mode";
    button.setAttribute("aria-label", "Switch to " + next + " mode");
  }

  button.addEventListener("click", function () {
    var next = current() === "dark" ? "light" : "dark";
    root.dataset.theme = next;
    try {
      localStorage.setItem("theme", next);
    } catch (e) {
      // Storage is unavailable (private browsing, blocked site data): the
      // choice lasts while this page is open.
    }
    label();
  });
  system.addEventListener("change", label);
  label();
  button.hidden = false;
})();
