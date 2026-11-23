// The user menu is a native <details>, so it works without this script.
// This only closes it on Escape (returning focus to the name) or on a click
// outside it, like other drop-down menus.
document.addEventListener("keydown", (e) => {
  const menu = document.querySelector(".user-menu[open]");
  if (e.key === "Escape" && menu) {
    menu.open = false;
    menu.querySelector("summary").focus();
  }
});
document.addEventListener("click", (e) => {
  const menu = document.querySelector(".user-menu[open]");
  if (menu && !menu.contains(e.target)) menu.open = false;
});
