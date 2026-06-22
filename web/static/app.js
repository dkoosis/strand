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
    new Sortable(col, {
      group: "board",
      animation: 120,
      ghostClass: "card-ghost",
      onEnd: (evt) => {
        if (evt.to === evt.from) return; // reorder within a column: no-op
        const card = evt.item;
        const from = evt.from;
        const oldIndex = evt.oldIndex;
        card._revert = () => from.insertBefore(card, from.children[oldIndex] || null);
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
function showBoardError(msg) {
  const el = document.getElementById("boardErr");
  if (!el) return;
  el.textContent = msg;
  el.hidden = false;
}
// A bead move that bd rejected: revert the optimistic drop and show the reason.
document.body.addEventListener("htmx:responseError", (e) => {
  const card = e.detail.elt;
  if (card && card._revert) {
    card._revert();
    card._revert = null;
    showBoardError(e.detail.xhr.responseText.replace(/<[^>]*>/g, "").trim());
  }
});
// The board arrives over htmx; (re)bind Sortable once its fragment lands.
document.body.addEventListener("htmx:afterSwap", (e) => {
  if (e.detail.target.id === "listPane") {
    initBoard();
    initGraph();
  }
});

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

  cy?.destroy();
  cy = cytoscape({
    container: el,
    elements,
    layout: { name: window.cytoscapeDagre ? "dagre" : "breadthfirst", rankDir: "TB", padding: 18 },
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
