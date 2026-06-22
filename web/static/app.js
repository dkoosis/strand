// strand front-end: the server renders every view and htmx swaps the fragments.
// This file is only the progressive enhancement htmx can't express declaratively:
// theme toggle, the hover hint bar, and opening/closing the detail drawer.

// ---- theme toggle ----
document.getElementById("theme")?.addEventListener("click", () => {
  const r = document.documentElement;
  r.dataset.theme = r.dataset.theme === "dark" ? "light" : "dark";
});

// ---- hover hint bar ----
// Each tile carries its identity in data-attrs; sweeping the treemap updates the
// fixed hint bar with no server round-trip.
const hintbar = document.getElementById("hintbar");
function showHint(tile) {
  if (!hintbar) return;
  hintbar.classList.remove("idle");
  const open = tile.dataset.open || "0";
  const esc = tile.dataset.flag ? ` · <span class="esc">escalated</span>` : "";
  hintbar.querySelector(".hint-name").textContent = tile.dataset.name || "";
  hintbar.querySelector(".hint-meta").innerHTML = `<b style="color:var(--ink-2)">${open}</b> open${esc}`;
}
function idleHint() {
  if (!hintbar) return;
  hintbar.classList.add("idle");
  hintbar.querySelector(".hint-name").textContent = "Hover a tile";
  hintbar.querySelector(".hint-meta").innerHTML = "";
}
// Delegate so tiles swapped in by htmx keep working.
document.addEventListener("mouseover", (e) => {
  const tile = e.target.closest(".tile");
  if (tile) showHint(tile);
});
document.querySelector(".treemap")?.addEventListener("mouseleave", idleHint);

// ---- detail drawer ----
const scrim = document.getElementById("scrim");
const drawer = document.getElementById("drawer");
function openDrawer() {
  scrim?.classList.add("show");
  drawer?.classList.add("show");
}
function closeDrawer() {
  scrim?.classList.remove("show");
  drawer?.classList.remove("show");
}
// htmx swaps the drawer's contents; open it once the fragment lands.
document.body.addEventListener("htmx:afterSwap", (e) => {
  if (e.detail.target.id === "drawer") openDrawer();
});
scrim?.addEventListener("click", closeDrawer);
drawer?.addEventListener("click", (e) => {
  if (e.target.closest("#drClose")) closeDrawer();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") closeDrawer();
});
