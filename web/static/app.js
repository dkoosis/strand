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

// ---- repo selector ----
// The header button loads the menu fragment into #repoMenu over htmx; show it
// once the fragment lands and dismiss it on an outside click. Picking a repo
// responds with HX-Refresh, so the whole page reloads and the menu goes with it.
const repoMenu = document.getElementById("repoMenu");
document.body.addEventListener("htmx:afterSwap", (e) => {
  if (e.detail.target.id === "repoMenu") repoMenu?.classList.add("show");
});
document.addEventListener("click", (e) => {
  if (!repoMenu?.classList.contains("show")) return;
  if (e.target.closest(".repo-wrap")) return; // clicks inside keep it open
  repoMenu.classList.remove("show");
});

// ---- kanban board (drag-to-mutate) ----
// SortableJS owns the drag; on a cross-column drop we POST the pivot field's new
// value through the same update path the drawer uses, then htmx swaps the moved
// card with bd's truth. A bd error returns non-2xx (no swap), so we revert the
// card to its old column and surface the message. Intra-column reorders are
// ignored — there's no rank store (spec R0 V2).
function initBoard() {
  const board = document.querySelector(".board");
  if (!board || !window.Sortable) return;
  board.querySelectorAll(".bcol-body").forEach((col) => {
    col._sortable = new Sortable(col, {
      group: "board",
      animation: 120,
      ghostClass: "card-ghost",
      onEnd: (evt) => {
        if (evt.to === evt.from) return; // reorder within a column: no-op
        const card = evt.item;
        const from = evt.from;
        const oldIndex = evt.oldIndex;
        card._revert = () => from.insertBefore(card, from.children[oldIndex] || null);
        // Freeze both columns this drop touched until bd answers. A second drag
        // before the POST returns would stomp card._revert and revert to the
        // wrong spot on error (strand-vd2). afterRequest re-enables them.
        freezeSortables(card, [from, evt.to]);
        htmx.ajax("POST", "/bead/" + card.dataset.id + "/move", {
          source: card,
          target: card,
          swap: "outerHTML",
          values: { field: board.dataset.pivot, value: evt.to.dataset.value },
        });
      },
    });
  });
}
// The V1 list is drag-to-reorder within one epic (no cross-epic group, so drops
// stay inside their tbody). On drop we POST only the post-drop id order; the server
// re-reads ranks from bd and writes the minimal change (spec R6 manual rank). A
// success is 204 — htmx swaps nothing, the optimistic DOM already matches bd. A bd
// error returns non-2xx, caught by the shared revert handler below.
function initList() {
  const pane = document.getElementById("listPane");
  if (!pane || !window.Sortable) return;
  pane.querySelectorAll(".bead-rows").forEach((tbody) => {
    tbody._sortable = new Sortable(tbody, {
      animation: 120,
      ghostClass: "card-ghost",
      // Capture the row's original following sibling before the drag moves it.
      // Reverting by saved index breaks on upward moves: the dragged row still
      // occupies a slot, shifting from.children[oldIndex] off by one. The
      // original next sibling pins the exact spot for moves in either direction
      // (null when it was last → append).
      onStart: (evt) => {
        evt.item._revertSibling = evt.item.nextSibling;
      },
      onEnd: (evt) => {
        if (evt.oldIndex === evt.newIndex) return; // no positional change
        const row = evt.item;
        const from = evt.from;
        const sibling = row._revertSibling;
        row._revert = () => from.insertBefore(row, sibling);
        // Freeze this tbody until bd answers. A second drag before the rank POST
        // returns would stomp row._revert and revert to the wrong spot on error
        // (strand-vd2). afterRequest re-enables it.
        freezeSortables(row, [from]);
        const order = Array.from(from.children)
          .map((tr) => tr.dataset.id)
          .join(",");
        htmx.ajax("POST", "/bead/" + row.dataset.id + "/rank", {
          source: row,
          values: { order },
        });
      },
    });
  });
}
// Drag-revert race guard (strand-vd2). Both initBoard and initList stash their
// optimistic-revert closure in a single slot on the dragged element
// (card._revert / row._revert). A second drag before the first POST returns
// would overwrite that slot, so a later error reverts to the wrong position.
// Fix: freeze the Sortable instance(s) the drop touched until bd answers, so no
// second drag can start mid-flight. The element carries the frozen instances so
// htmx:afterRequest (fires on success AND error) can thaw them.
function freezeSortables(elt, containers) {
  const instances = containers.map((c) => c && c._sortable).filter(Boolean);
  elt._frozen = instances;
  instances.forEach((s) => s.option("disabled", true));
}
function thawSortables(elt) {
  if (!elt || !elt._frozen) return;
  elt._frozen.forEach((s) => s.option("disabled", false));
  elt._frozen = null;
}
function showBoardError(msg) {
  const el = document.getElementById("boardErr");
  if (!el) return;
  el.textContent = msg;
  el.hidden = false;
}
// A bead move that bd rejected: revert the optimistic drop and show the reason.
// Gate on POST: the dragged card/row also carries hx-get (opens the drawer), so a
// drawer GET failing mid-drag must not fire the revert against a live _revert slot.
document.body.addEventListener("htmx:responseError", (e) => {
  if (e.detail.requestConfig?.verb !== "post") return;
  const card = e.detail.elt;
  if (card && card._revert) {
    card._revert();
    card._revert = null;
    showBoardError(e.detail.xhr.responseText.replace(/<[^>]*>/g, "").trim());
  }
});
// Every drag-POST settles here (success or error). Thaw the columns/tbody we
// froze for the duration of the request and drop the now-consumed revert slot,
// so the next drag starts from a clean state (strand-vd2).
document.body.addEventListener("htmx:afterRequest", (e) => {
  // Only the drag POSTs froze anything. The same card/row also has hx-get for the
  // drawer; a GET completing mid-drag would otherwise thaw and clear _revert early,
  // defeating the guard if the POST then errors (strand-vd2).
  if (e.detail.requestConfig?.verb !== "post") return;
  const elt = e.detail.elt;
  if (!elt) return;
  thawSortables(elt);
  if (elt._revert) elt._revert = null;
});
// The board arrives over htmx; (re)bind Sortable once its fragment lands.
document.body.addEventListener("htmx:afterSwap", (e) => {
  if (e.detail.target.id === "listPane") {
    initBoard();
    initList();
  }
});
// The V1 list renders inline on first paint (no htmx swap), so bind it once at load.
initList();

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
