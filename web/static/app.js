// strand front-end: the server renders every view and htmx swaps the fragments.
// This file is only the progressive enhancement htmx can't express declaratively:
// theme toggle, the hover hint bar, and opening/closing the detail drawer.

// ---- theme toggle ----
document.getElementById("theme")?.addEventListener("click", () => {
  const r = document.documentElement;
  r.dataset.theme = r.dataset.theme === "dark" ? "light" : "dark";
  try { localStorage.setItem("theme", r.dataset.theme); } catch (e) {}
});

// ---- minimap readout ----
// The map itself carries no text — area is open work, color is the module. The
// readout under it does the naming: at rest a color legend (each module), and on
// hover the hovered tile's module with its epics listed, the one under the cursor
// lit. All read from the rendered DOM, no server round-trip. The region is the
// hover unit, so you never have to land on a sliver of a tile to read its name.
const mmReadout = document.getElementById("mmReadout");
const mmTitle = document.getElementById("mmTitle");
const mmEsc = (s) => (s || "").replace(/&/g, "&amp;").replace(/</g, "&lt;");
function mmLegend() {
  if (!mmReadout) return;
  mmReadout.classList.add("idle");
  if (mmTitle) mmTitle.textContent = "";
  const rows = [...document.querySelectorAll(".treemap .region")].map((r) => {
    const rc = r.style.getPropertyValue("--rc").trim();
    return `<div class="mm-row"><span class="mm-sw" style="background:${rc}"></span>` +
      `<span class="mm-rn">${mmEsc(r.dataset.name)}</span>` +
      `<span class="mm-rc tnum">${r.dataset.open || 0}</span></div>`;
  }).join("");
  mmReadout.innerHTML = `<div class="mm-cap">modules</div>${rows}`;
}
function mmModule(region, hoverEpic) {
  if (!mmReadout || !region) return;
  mmReadout.classList.remove("idle");
  if (mmTitle) mmTitle.textContent = region.dataset.name || "";
  const rc = region.style.getPropertyValue("--rc").trim();
  const tiles = [...region.querySelectorAll(".tile")];
  let flagged = 0;
  const eps = tiles.map((t) => {
    if (t.dataset.flag) flagged++;
    const on = hoverEpic && t.dataset.epic === hoverEpic ? " on" : "";
    return `<div class="mm-ep${on}"><span class="mm-en">${mmEsc(t.dataset.name)}</span>` +
      (t.dataset.flag ? `<span class="mm-fl"></span>` : ``) +
      `<span class="mm-eo tnum">${t.dataset.open || 0}</span></div>`;
  }).join("");
  mmReadout.innerHTML =
    `<div class="mm-mod"><span class="mm-sw" style="background:${rc}"></span>` +
    `<span class="mm-mn">${mmEsc(region.dataset.name)}</span></div>` +
    `<div class="mm-meta">${region.dataset.open || 0} open · ${tiles.length} epics` +
    `${flagged ? ` · ${flagged} flagged` : ``}</div>` +
    `<div class="mm-eps">${eps}</div>`;
}
// Delegate so tiles swapped in by htmx keep working.
document.addEventListener("mouseover", (e) => {
  const tile = e.target.closest(".tile");
  if (tile) { mmModule(tile.closest(".region"), tile.dataset.epic); return; }
  const head = e.target.closest(".rg-head");
  if (head) mmModule(head.closest(".region"), "");
});
document.querySelector(".minimap")?.addEventListener("mouseleave", mmLegend);
mmLegend();

// ---- view-centric chrome: tab strip + minimap filter ----
// The active VIEW (Table/Board/Insights) is the centerpiece; the tab strip up top
// is the primary control, and the treemap is a demoted ambient minimap. Two pieces
// of state drive it: which view is active and which epic (if any) it's scoped to.
// Both live on #viewport's data-attrs and are re-synced from each swapped fragment's
// pane-head, so a minimap click filters whatever view is active — not just the list.
const viewport = document.getElementById("viewport");
const VIEW_PATHS = { list: "/list", board: "/board", insights: "/insights" };

// activeView/activeEpic read the single source of truth on #viewport.
function activeView() {
  return viewport?.dataset.view || "list";
}
function activeEpic() {
  return viewport?.dataset.epic || "";
}
// viewURL builds the fragment endpoint for a view scoped to an epic (or the whole
// strand when epic is empty). One place owns the ?epic= shape so tabs and minimap
// clicks can't drift.
function viewURL(view, epic) {
  const path = VIEW_PATHS[view] || VIEW_PATHS.list;
  return epic ? `${path}?epic=${encodeURIComponent(epic)}` : path;
}
// syncChrome reflects the active view+epic into the chrome: the tab strip's pressed
// state, the scope hint, and the minimap's "all" clear affordance.
function syncChrome() {
  const view = activeView();
  const epic = activeEpic();
  document.querySelectorAll(".viewtab").forEach((tab) => {
    const on = tab.dataset.view === view;
    tab.classList.toggle("active", on);
    tab.setAttribute("aria-pressed", on ? "true" : "false");
  });
  const hint = document.getElementById("scopeHint");
  if (hint) {
    const name = epic
      ? document.querySelector(`.tile[data-epic="${CSS.escape(epic)}"]`)?.dataset.name
      : "";
    hint.textContent = epic ? `Filtered · ${name || epic}` : "Everything";
  }
  const clear = document.getElementById("mmClear");
  if (clear) clear.hidden = !epic;
  // The active epic's tile lights up, and its parent module's border turns to the
  // module's legend color (--rc) so the swatch and the map agree.
  document.querySelectorAll(".tile.mm-filter").forEach((t) => {
    t.classList.toggle("on", !!epic && t.dataset.epic === epic);
  });
  document.querySelectorAll(".treemap .region").forEach((region) => {
    const owns = !!epic && region.querySelector(`.tile[data-epic="${CSS.escape(epic)}"]`);
    region.classList.toggle("on", !!owns);
  });
}
// A tab click loads its view at the CURRENT scope. The button's hx-get is static
// (the bare view), so rewrite it just before htmx fires to carry the active epic.
document.body.addEventListener("htmx:configRequest", (e) => {
  const tab = e.detail.elt.closest?.(".viewtab");
  if (tab) e.detail.path = viewURL(tab.dataset.view, activeEpic());
});
// A minimap region/epic click filters the ACTIVE view to that scope (spec §2). An
// epic tile scopes to its id; a region head clears the scope (the whole region is
// the strand's first region). Routes through htmx.ajax so the active view's
// endpoint renders, not a hardcoded /list.
function minimapFilter(el) {
  const epic = el.dataset.epic || "";
  viewport.dataset.epic = epic;
  htmx.ajax("GET", viewURL(activeView(), epic), { target: "#listPane", swap: "innerHTML" });
}
document.addEventListener("click", (e) => {
  const f = e.target.closest(".mm-filter");
  if (f) minimapFilter(f);
});

// Insights "frees N" — toggle the list of beads this one would unblock. Delegated so
// it keeps working after htmx swaps the insights fragment in.
document.addEventListener("click", (e) => {
  const btn = e.target.closest(".dn-frees");
  if (!btn) return;
  const t = document.getElementById(btn.getAttribute("aria-controls"));
  if (!t) return;
  const opening = t.hidden;
  t.hidden = !opening;
  btn.setAttribute("aria-expanded", String(opening));
});
document.addEventListener("keydown", (e) => {
  if (e.key !== "Enter" && e.key !== " ") return;
  const f = e.target.closest?.(".mm-filter");
  if (f) {
    e.preventDefault();
    minimapFilter(f);
  }
});
// Keyboard-activated [role=button] tiles/rows/heads fire htmx on keyup (Enter/Space),
// but Space's default page-scroll happens on keydown — before the keyup trigger. htmx
// won't preventDefault a keydown on a non-form element, so cancel it here; activation
// still lands on keyup. mm-filter elements handle their own keys above (this only
// preventDefaults, so the double-cancel on any overlap is harmless).
document.addEventListener("keydown", (e) => {
  if (e.key === " " && e.target.closest?.("[role=button]")) e.preventDefault();
});
// After any centerpiece swap, re-read the fragment's own scope (pane-head carries
// data-view/data-epic) into #viewport, so the chrome and the minimap highlight
// follow the truth the server just rendered — including a tab click that changed
// the view, or the refreshList re-render.
document.body.addEventListener("htmx:afterSwap", (e) => {
  if (e.detail.target.id !== "listPane") return;
  const head = e.detail.target.querySelector(".pane-head[data-view]");
  if (head) {
    viewport.dataset.view = head.dataset.view;
    viewport.dataset.epic = head.dataset.epic || "";
  }
  syncChrome();
});
syncChrome();

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
