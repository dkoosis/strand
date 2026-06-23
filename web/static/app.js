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
    initGraph();
    initList();
  }
});
// The V1 list renders inline on first paint (no htmx swap), so bind it once at load.
initList();

// ---- dependency graph (V3) ----
// The server computes the DAG and its metrics and serializes them into #cy's
// data-graph attribute (spec D8: server renders, client only draws). Cytoscape
// lays the nodes out top-down with dagre so blocks→blocked-by reads as depth;
// node size is normalized PageRank so foundational beads loom; cycles and the
// critical path are outlined. A node tap loads the bead into the same #drawer a
// card click does, so there's no second open path.
if (window.cytoscape && window.cytoscapeDagre) cytoscape.use(window.cytoscapeDagre);
let cy;
function initGraph() {
  const el = document.getElementById("cy");
  if (!el || !window.cytoscape) return;
  let model;
  try {
    model = JSON.parse(el.dataset.graph);
  } catch {
    return; // a malformed model shows the empty container, not a thrown page
  }
  if (!model || !Array.isArray(model.nodes) || !Array.isArray(model.edges)) {
    return; // a null/shapeless model shows the empty container, not a TypeError
  }

  // Normalize PageRank to a node-size range so the largest visibly dominates; raw
  // scores (~sub-1, near-equal) would all render as same-size dots.
  let lo = Infinity, hi = -Infinity;
  for (const n of model.nodes) {
    if (n.score < lo) lo = n.score;
    if (n.score > hi) hi = n.score;
  }
  const sizeOf = (s) => (hi > lo ? 22 + ((s - lo) / (hi - lo)) * 40 : 30);

  const elements = [
    ...model.nodes.map((n) => ({
      data: {
        id: n.id,
        label: n.label,
        size: sizeOf(n.score),
        status: n.status,
        cycle: n.inCycle ? 1 : 0,
        path: n.onPath ? 1 : 0,
      },
    })),
    ...model.edges.map((e) => ({ data: { source: e.source, target: e.target } })),
  ];

  // Dagre ranks nodes by edges; with no edges every node lands at rank 0 and the
  // graph collapses to one horizontal line (the edgeless-scope case). Fall back to
  // a grid so isolated beads tile legibly instead of stringing out.
  const layout = elements.some((e) => e.data.source)
    ? { name: window.cytoscapeDagre ? "dagre" : "breadthfirst", rankDir: "TB", padding: 18 }
    : { name: "grid", padding: 18 };

  cy?.destroy();
  cy = cytoscape({
    container: el,
    elements,
    layout,
    style: [
      {
        selector: "node",
        style: {
          width: "data(size)",
          height: "data(size)",
          label: "data(label)",
          "font-size": 10,
          "text-wrap": "ellipsis",
          "text-max-width": 96,
          "text-valign": "bottom",
          "text-margin-y": 4,
          "background-color": "#7a7f87",
          color: "#aab",
        },
      },
      { selector: 'node[status = "in_progress"]', style: { "background-color": "#3b82f6" } },
      { selector: 'node[status = "closed"]', style: { "background-color": "#4b5563" } },
      // path first, cycle last: equal-specificity selectors resolve last-wins, so a
      // node that is both on the critical path and in a cycle keeps the red cycle
      // border — the cycle is the warning users must not miss.
      { selector: "node[path = 1]", style: { "border-width": 3, "border-color": "#eab308" } },
      { selector: "node[cycle = 1]", style: { "border-width": 3, "border-color": "#ef4444" } },
      {
        selector: "edge",
        style: {
          width: 1.5,
          "line-color": "#5b6068",
          "target-arrow-color": "#5b6068",
          "target-arrow-shape": "triangle",
          "arrow-scale": 0.9,
          "curve-style": "bezier",
        },
      },
    ],
  });
  cy.on("tap", "node", (evt) => {
    htmx.ajax("GET", "/bead/" + evt.target.id(), { target: "#drawer" });
  });
}

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
