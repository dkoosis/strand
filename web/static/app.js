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
// The map itself carries no text — area is open work, color is the epic. The
// readout under it does the naming: at rest a color legend (each epic), and on
// hover the hovered story's epic with its stories listed, the one under the cursor
// lit. All read from the rendered DOM, no server round-trip. The epic is the
// hover unit, so you never have to land on a sliver of a story to read its name.
const mmReadout = document.getElementById("mmReadout");
const mmTitle = document.getElementById("mmTitle");
// Escapes for both text and double-quoted-attribute contexts: the active-cut
// readout drops a (user-controlled) bead name into an aria-label, so " and > count.
const mmEsc = (s) => (s || "").replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
function mmLegend() {
  if (!mmReadout) return;
  mmReadout.classList.add("idle");
  if (mmTitle) mmTitle.textContent = "";
  const rows = [...document.querySelectorAll(".map .epic")].map((r) => {
    const rc = r.style.getPropertyValue("--rc").trim();
    return `<div class="mm-row"><span class="mm-sw" style="background:${rc}"></span>` +
      `<span class="mm-rn">${mmEsc(r.dataset.name)}</span>` +
      `<span class="mm-rc tnum">${r.dataset.open || 0}</span></div>`;
  }).join("");
  mmReadout.innerHTML = `<div class="mm-cap">epics</div>${rows}`;
}
function mmEpic(epic, hoverStory) {
  if (!mmReadout || !epic) return;
  mmReadout.classList.remove("idle");
  if (mmTitle) mmTitle.textContent = epic.dataset.name || "";
  const rc = epic.style.getPropertyValue("--rc").trim();
  const stories = [...epic.querySelectorAll(".story")];
  let flagged = 0;
  const eps = stories.map((t) => {
    if (t.dataset.flag) flagged++;
    const on = hoverStory && t.dataset.story === hoverStory ? " on" : "";
    return `<div class="mm-ep${on}"><span class="mm-en">${mmEsc(t.dataset.name)}</span>` +
      (t.dataset.flag ? `<span class="mm-fl"></span>` : ``) +
      `<span class="mm-eo tnum">${t.dataset.open || 0}</span></div>`;
  }).join("");
  mmReadout.innerHTML =
    `<div class="mm-mod"><span class="mm-sw" style="background:${rc}"></span>` +
    `<span class="mm-mn">${mmEsc(epic.dataset.name)}</span></div>` +
    `<div class="mm-meta">${epic.dataset.open || 0} open · ${stories.length} stories` +
    `${flagged ? ` · ${flagged} flagged` : ``}</div>` +
    `<div class="mm-eps">${eps}</div>`;
}
// Delegate so stories swapped in by htmx keep working.
document.addEventListener("mouseover", (e) => {
  const story = e.target.closest(".story");
  if (story) { mmEpic(story.closest(".epic"), story.dataset.story); return; }
  const head = e.target.closest(".epic-head");
  if (head) mmEpic(head.closest(".epic"), "");
});
document.querySelector(".minimap")?.addEventListener("mouseleave", mmLegend);
mmLegend();

// ---- view-centric chrome: tab strip + minimap filter ----
// The active VIEW (Table/Board/Insights) is the centerpiece; the tab strip up top
// is the primary control, and the map is a demoted ambient minimap. Two pieces
// of state drive it: which view is active and which story (if any) it's scoped to.
// Both live on #viewport's data-attrs and are re-synced from each swapped fragment's
// pane-head, so a minimap click filters whatever view is active — not just the list.
const viewport = document.getElementById("viewport");
const VIEW_PATHS = { list: "/list", board: "/board", insights: "/insights" };

// activeView/activeStory read the single source of truth on #viewport.
function activeView() {
  return viewport?.dataset.view || "list";
}
function activeStory() {
  return viewport?.dataset.story || "";
}
// activeEpic reads the epic scope (an epic Key), set by an epic-head click.
function activeEpic() {
  return viewport?.dataset.epic || "";
}
// activeFilter reads the cross-strand scope filter ("" = everything, "bugs", or a
// masthead-pulse status cut). It is orthogonal to the story scope and overrides
// it: a filter is a whole-strand cut.
function activeFilter() {
  return viewport?.dataset.filter || "";
}
// PULSE_FILTERS are the masthead status cuts. They live only in the Table, so a
// switch to Board/Insights drops them (those views render the whole strand).
const PULSE_FILTERS = new Set(["waiting", "open", "in_progress", "blocked", "closed", "deferred"]);
// viewURL builds the fragment endpoint for a view at the current scope. Precedence
// matches the server's listViewFor: a filter (bugs) is a whole-strand cut and wins,
// then one story, then one epic; with none, the bare view is the whole strand
// ("everything"). One place owns the query shape so the tabs, scope chips, and
// minimap clicks can't drift.
function viewURL(view, story, epic, filter) {
  const path = VIEW_PATHS[view] || VIEW_PATHS.list;
  if (filter) return `${path}?filter=${encodeURIComponent(filter)}`;
  if (story) return `${path}?story=${encodeURIComponent(story)}`;
  if (epic) return `${path}?epic=${encodeURIComponent(epic)}`;
  return path;
}
// pulseChip reads a live pulse cell so the active-cut readout reuses the server's
// glyph, label, and color class for a status cut instead of duplicating them.
function pulseChip(filter) {
  const cell = document.querySelector(`.pcell[data-filter="${CSS.escape(filter)}"]`);
  const glyph = cell?.querySelector(".pg")?.textContent || "";
  const label = (cell?.getAttribute("title") || filter).split(":")[0].trim();
  const cls = [...(cell?.classList || [])].find((c) => c.startsWith("pc-")) || "";
  return { glyph, label, cls };
}
// renderActiveCuts is the ONE place the live narrowing shows: a pulse status cut,
// the bugs type scope, or a minimap epic/story scope, each a chip you can clear
// right here — replacing the old scope-hint span and the minimap-foot clear button.
// The three cuts are mutually exclusive (each control clears the others), so at most
// one chip renders today; the loop is written for the general case so a future
// composed cut lights up without a rewrite.
function renderActiveCuts(story, epic, filter) {
  const el = document.getElementById("activeCuts");
  if (!el) return;
  const chips = [];
  if (filter === "bugs") {
    chips.push({ label: "bugs" });
  } else if (PULSE_FILTERS.has(filter)) {
    chips.push(pulseChip(filter));
  }
  if (story) {
    const name = document.querySelector(`.story[data-story="${CSS.escape(story)}"]`)?.dataset.name || story;
    chips.push({ label: name });
  } else if (epic) {
    const name = document.querySelector(`.epic[data-epic="${CSS.escape(epic)}"]`)?.dataset.name || epic;
    chips.push({ label: name });
  }
  el.innerHTML = chips
    .map((c) => {
      const g = c.glyph ? `<span class="cut-g" aria-hidden="true">${mmEsc(c.glyph)}</span>` : "";
      const cls = c.cls ? ` ${c.cls}` : "";
      return `<span class="cut-chip${cls}">${g}<span class="cut-l">${mmEsc(c.label)}</span>` +
        `<button class="cut-x" type="button" data-clear-cut aria-label="Clear ${mmEsc(c.label)} filter">×</button></span>`;
    })
    .join("");
}
// clearCuts drops every narrowing scope and reloads the active view whole — the
// single clear path the readout chips and any programmatic reset route through.
function clearCuts() {
  if (!viewport) return;
  viewport.dataset.story = "";
  viewport.dataset.epic = "";
  viewport.dataset.filter = "";
  htmx.ajax("GET", viewURL(activeView(), "", "", ""), { target: "#listPane", swap: "innerHTML" });
}
document.addEventListener("click", (e) => {
  if (e.target.closest("[data-clear-cut]")) clearCuts();
});

// syncChrome reflects the active view+story into the chrome: the tab strip's pressed
// state, the active-cut readout, and the pulse/scope highlights.
function syncChrome() {
  const view = activeView();
  const story = activeStory();
  const filter = activeFilter();
  document.querySelectorAll(".viewtab").forEach((tab) => {
    const on = tab.dataset.view === view;
    tab.classList.toggle("active", on);
    tab.setAttribute("aria-pressed", on ? "true" : "false");
  });
  const epic = activeEpic();
  // Scope chips: "everything" is on when nothing narrows the strand; "bugs" when
  // its filter is live. A story or epic scope (from the minimap) leaves both off.
  document.querySelectorAll(".scopetab").forEach((tab) => {
    const want = tab.dataset.scope === "bugs" ? "bugs" : "";
    const on = !story && !epic && filter === want;
    tab.classList.toggle("active", on);
    tab.setAttribute("aria-pressed", on ? "true" : "false");
  });
  renderActiveCuts(story, epic, filter);
  // A pulse cell lights only while its cut is the live Table filter.
  const pulseOn = view === "list" && PULSE_FILTERS.has(filter);
  document.querySelectorAll(".pcell").forEach((c) => {
    c.classList.toggle("on", pulseOn && c.dataset.filter === filter);
  });
  // The active story's cell lights up; an epic scope (or the epic owning the
  // active story) lights the whole epic's border in its legend color (--rc) so
  // the swatch and the map agree.
  document.querySelectorAll(".story.mm-filter").forEach((t) => {
    t.classList.toggle("on", !!story && t.dataset.story === story);
  });
  document.querySelectorAll(".map .epic").forEach((rg) => {
    const ownsStory = !!story && rg.querySelector(`.story[data-story="${CSS.escape(story)}"]`);
    const isScoped = !!epic && rg.dataset.epic === epic;
    rg.classList.toggle("on", !!ownsStory || isScoped);
  });
}
// A tab click loads its view at the CURRENT scope; a scope chip switches the scope
// in the ACTIVE view. Both buttons carry a static bare-view hx-get, so rewrite the
// path just before htmx fires. A chip click also commits the new scope to #viewport
// up front (afterSwap can't recover a filter from the server-rendered pane-head).
document.body.addEventListener("htmx:configRequest", (e) => {
  // A refreshList (after a create/edit/delete) reloads #listPane off its static
  // hx-get="/list" — reload the ACTIVE view at the live scope instead, so an edit
  // doesn't snap the pane back to an unscoped Table.
  if (e.detail.elt.id === "listPane") {
    e.detail.path = viewURL(activeView(), activeStory(), activeEpic(), activeFilter());
    return;
  }
  // A masthead pulse cell loads its status cut into the Table: commit the filter
  // (and force the list view) up front, since afterSwap can't recover it from the
  // server-rendered pane-head, then rewrite the path onto the cut.
  const pcell = e.detail.elt.closest?.(".pcell");
  if (pcell && pcell.dataset.filter) {
    viewport.dataset.story = "";
    viewport.dataset.epic = "";
    viewport.dataset.filter = pcell.dataset.filter;
    viewport.dataset.view = "list";
    e.detail.path = viewURL("list", "", "", pcell.dataset.filter);
    return;
  }
  const tab = e.detail.elt.closest?.(".viewtab");
  if (tab) {
    // A pulse cut is Table-only; switching views drops it back to the whole strand.
    const filter = PULSE_FILTERS.has(activeFilter()) ? "" : activeFilter();
    if (!filter) viewport.dataset.filter = "";
    e.detail.path = viewURL(tab.dataset.view, activeStory(), activeEpic(), filter);
    return;
  }
  const chip = e.detail.elt.closest?.(".scopetab");
  if (chip) {
    const filter = chip.dataset.scope === "bugs" ? "bugs" : "";
    viewport.dataset.story = "";
    viewport.dataset.epic = "";
    viewport.dataset.filter = filter;
    e.detail.path = viewURL(activeView(), "", "", filter);
    return;
  }
  // A board pivot carries a static /board?pivot= URL that omits the live filter/
  // epic scope — rewrite it onto the active scope so changing Status/Priority
  // doesn't silently widen the board back to Everything.
  const pivot = e.detail.elt.closest?.(".bb-pivot");
  if (pivot) {
    const base = viewURL("board", activeStory(), activeEpic(), activeFilter());
    e.detail.path = `${base}${base.includes("?") ? "&" : "?"}pivot=${encodeURIComponent(pivot.dataset.pivot)}`;
    return;
  }
});
// Any non-search navigation that reloads #listPane (a view/scope switch, a minimap
// or pulse cut, a refreshList) leaves the search box showing a query the pane no
// longer reflects — clear it so the input never claims a filter the list isn't.
document.body.addEventListener("htmx:beforeRequest", (e) => {
  if (e.detail.elt.id === "beadSearch") return;
  if (e.detail.target?.id !== "listPane") return;
  const box = document.getElementById("beadSearch");
  if (box) box.value = "";
});
// A minimap epic/story click filters the ACTIVE view to that scope (spec §2). A
// story cell scopes to its id; an epic head clears the story scope (the whole epic
// is the strand's first epic). Routes through htmx.ajax so the active view's
// endpoint renders, not a hardcoded /list.
function minimapFilter(el) {
  const story = el.dataset.story || "";
  const epic = el.dataset.epic || ""; // set on an epic head, absent on a story
  viewport.dataset.story = story;
  viewport.dataset.epic = epic;
  viewport.dataset.filter = ""; // a map scope replaces any whole-strand filter
  htmx.ajax("GET", viewURL(activeView(), story, epic, ""), { target: "#listPane", swap: "innerHTML" });
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

// ---- Suggest (title st-suggest.1, sections st-suggest.2) ----
// A Suggest button loads a preview into its own .dr-suggest-preview slot — title
// into the drawer head, sections under Description. Apply copies the proposed text
// into the editor input named by data-target (.dr-title or .dr-desc) and dispatches
// its change event, so the normal change→PATCH path commits it via internal/bd —
// Suggest itself writes nothing. Dismiss just clears the slot. Delegated so both
// keep working after htmx swaps the drawer or a preview fragment in.
const SUGGEST_TARGETS = { title: ".dr-title", description: ".dr-desc" };
function clearSuggestPreview(el) {
  const slot = el.closest(".dr-suggest-preview");
  if (slot) slot.innerHTML = "";
}
document.addEventListener("click", (e) => {
  const apply = e.target.closest(".dr-suggest-apply");
  if (apply) {
    const sel = SUGGEST_TARGETS[apply.getAttribute("data-target")] || ".dr-title";
    const input = document.querySelector(sel);
    if (input) {
      input.value = apply.getAttribute("data-value") || "";
      input.dispatchEvent(new Event("change", { bubbles: true }));
    }
    clearSuggestPreview(apply);
    return;
  }
  const dismiss = e.target.closest(".dr-suggest-dismiss");
  if (dismiss) clearSuggestPreview(dismiss);
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
// data-view/data-story/data-epic) into #viewport, so the chrome and the minimap
// highlight follow the truth the server just rendered — including a tab click that
// changed the view, or the refreshList re-render. The epic scope must be re-read
// too: drilling into a story of a DIFFERENT epic otherwise leaves a stale epic
// scope lit alongside the drilled story's epic (two borders at once). The pane-head
// emits its epic key only for a genuine epic scope (HasEpic) — empty for a story,
// "everything", or bugs — so this clears the scope when the new fragment isn't one.
document.body.addEventListener("htmx:afterSwap", (e) => {
  // A refreshed pulse bar carries new buttons — re-apply the active-cut highlight.
  if (e.detail.target.id === "pulseBar") {
    syncChrome();
    return;
  }
  if (e.detail.target.id !== "listPane") return;
  const head = e.detail.target.querySelector(".pane-head[data-view]");
  if (head) {
    viewport.dataset.view = head.dataset.view;
    viewport.dataset.story = head.dataset.story || "";
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
// The V1 list is drag-to-reorder within one story (no cross-story group, so drops
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
