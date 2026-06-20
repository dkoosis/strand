// strand front-end: fetches the JSON API and renders the bead list + detail.
// No framework — just enough DOM to be pleasant.

const listEl = document.getElementById("list");
const detailEl = document.getElementById("detail");
const statusEl = document.getElementById("status");
const nav = document.querySelector("nav");

// view -> API endpoint
const ENDPOINTS = {
  ready: "/api/ready",
  all: "/api/issues",
  open: "/api/issues?status=open",
};

let current = "ready";

nav.addEventListener("click", (e) => {
  const btn = e.target.closest("button[data-view]");
  if (!btn) return;
  current = btn.dataset.view;
  for (const b of nav.querySelectorAll("button")) {
    b.classList.toggle("active", b === btn);
  }
  load();
});

listEl.addEventListener("click", (e) => {
  const li = e.target.closest("li.issue");
  if (li) showDetail(li.dataset.id);
});

async function load() {
  detailEl.hidden = true;
  statusEl.textContent = "loading…";
  try {
    const issues = await fetchJSON(ENDPOINTS[current]);
    render(issues || []);
    statusEl.textContent = `${(issues || []).length} bead(s)`;
  } catch (err) {
    listEl.innerHTML = `<li class="error">${escape(err.message)}</li>`;
    statusEl.textContent = "error";
  }
}

function render(issues) {
  if (!issues.length) {
    listEl.innerHTML = `<li class="empty">Nothing here.</li>`;
    return;
  }
  listEl.innerHTML = issues.map(rowHTML).join("");
}

function rowHTML(it) {
  const p = it.priority ?? 3;
  return `<li class="issue" data-id="${escape(it.id)}">
    <span class="pri pri-${p}">P${p}</span>
    <span class="id">${escape(it.id)}</span>
    <span class="title">${escape(it.title)}</span>
    <span class="badge">${escape(it.status)}</span>
  </li>`;
}

async function showDetail(id) {
  detailEl.hidden = false;
  detailEl.innerHTML = `<p class="empty">loading ${escape(id)}…</p>`;
  try {
    const it = await fetchJSON(`/api/issues/${encodeURIComponent(id)}`);
    detailEl.innerHTML = `
      <button class="close" aria-label="close">×</button>
      <h2>${escape(it.title)}</h2>
      <p class="id">${escape(it.id)} · P${it.priority} · ${escape(it.status)} · ${escape(it.issue_type || "")}</p>
      ${it.description ? `<h3>Description</h3><pre>${escape(it.description)}</pre>` : ""}
      ${it.design ? `<h3>Design</h3><pre>${escape(it.design)}</pre>` : ""}
      <p class="badge">deps ${it.dependency_count} · blocks ${it.dependent_count} · comments ${it.comment_count}</p>`;
    detailEl.querySelector(".close").addEventListener("click", () => {
      detailEl.hidden = true;
    });
  } catch (err) {
    detailEl.innerHTML = `<p class="error">${escape(err.message)}</p>`;
  }
}

async function fetchJSON(url) {
  const res = await fetch(url);
  const data = await res.json();
  if (!res.ok || (data && data.error)) {
    throw new Error((data && data.error) || `HTTP ${res.status}`);
  }
  return data;
}

function escape(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  })[c]);
}

load();
