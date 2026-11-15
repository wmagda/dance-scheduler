let STATE = {
  viewer: null,
  events: [],
  teachers: [],
  tokens_used: [],
  config: {},
  transport_mode: "manual",
  digest: "",
  outbox: [],
  selectedMonth: "all",
  openEditEventId: null,
};

function showToast(msg) {
  const el = document.getElementById("toast");
  el.textContent = msg;
  el.classList.remove("hidden");
  setTimeout(() => el.classList.add("hidden"), 4500);
}

async function api(path, body) {
  const opts = { headers: { "Content-Type": "application/json" } };
  if (body !== undefined) {
    opts.method = "POST";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    throw new Error(data.error || `HTTP ${res.status}`);
  }
  return data;
}

async function loadState() {
  try {
    const data = await api("/api/state");
    Object.assign(STATE, data);
    renderAll();
  } catch (err) {
    showToast("Error loading state: " + err.message);
  }
}

function formatDateNice(ymd) {
  const [y, m, d] = ymd.split("-").map(Number);
  const dt = new Date(Date.UTC(y, m - 1, d));
  return dt.toLocaleDateString("en-US", {
    timeZone: "UTC",
    weekday: "short",
    month: "short",
    day: "numeric",
    year: "numeric",
  });
}

function renderAll() {
  renderAuthBar();
  renderMonthFilters();
  renderEvents();
  renderFairness();
  renderDigests();
  renderAdmin();
}

function renderAuthBar() {
  const bar = document.getElementById("auth-bar");
  const loginCard = document.getElementById("login-card");
  const adminTabBtn = document.getElementById("admin-tab-btn");

  if (!STATE.viewer) {
    bar.innerHTML = `<span class="muted">Viewing as Guest — Sign in below to bid or volunteer</span>`;
    loginCard.classList.remove("hidden");
    adminTabBtn.classList.add("hidden");
    return;
  }

  loginCard.classList.add("hidden");
  if (STATE.viewer.is_admin) {
    adminTabBtn.classList.remove("hidden");
  } else {
    adminTabBtn.classList.add("hidden");
  }

  bar.innerHTML = `
    <span class="pill">👤 <strong>${escapeHtml(STATE.viewer.name)}</strong>${STATE.viewer.is_admin ? " (Admin)" : ""}</span>
    <span class="pill" title="Confirmed teaching nights in rolling fairness window">⚖️ ${STATE.viewer.fairness_score} nights (${STATE.config.fairness_window_days || 90}d)</span>
    <span class="pill" title="Remaining annual Veto Tokens">⭐ ${STATE.viewer.tokens_available}/${STATE.viewer.tokens_total} Tokens</span>
    <button class="btn small" id="btn-logout">Sign Out</button>
  `;
  document.getElementById("btn-logout").onclick = async () => {
    await api("/api/auth/logout", {});
    await loadState();
  };
}

function renderMonthFilters() {
  const container = document.getElementById("month-filters");
  const months = Array.from(new Set(STATE.events.map((e) => e.date.slice(0, 7))));
  let html = `<button class="btn small ${STATE.selectedMonth === "all" ? "primary" : ""}" data-m="all">All (${STATE.events.length})</button>`;
  for (const m of months) {
    html += `<button class="btn small ${STATE.selectedMonth === m ? "primary" : ""}" data-m="${m}">${m}</button>`;
  }
  container.innerHTML = html;
  container.querySelectorAll("button").forEach((btn) => {
    btn.onclick = () => {
      STATE.selectedMonth = btn.dataset.m;
      renderMonthFilters();
      renderEvents();
    };
  });
}

function renderEvents() {
  const grid = document.getElementById("events-grid");
  const list = STATE.events.filter(
    (e) => STATE.selectedMonth === "all" || e.date.startsWith(STATE.selectedMonth)
  );

  grid.innerHTML = list
    .map((ev) => {
      const isResolved = ev.status === "resolved" || ev.status === "done";
      let statusLabel = "Open for Bids";
      if (ev.status === "tentative") statusLabel = `Bid Placed · Resolves ${ev.resolve_deadline}`;
      if (ev.status === "recruiting") statusLabel = `Recruiting · Needs Instructor!`;
      if (isResolved) statusLabel = `Confirmed: ${escapeHtml(ev.winner_name)}`;

      const myBidBadge = ev.my_bid_kind
        ? `<span class="badge status-tentative">Your Bid: ${ev.my_bid_kind === "token" ? "⭐ Veto Token" : "Normal"}</span>`
        : "";

      const vols = (ev.volunteers || [])
        .map((v) => `<span class="pill">${escapeHtml(v.name)} (${escapeHtml(v.role)})</span>`)
        .join(" ");

      let helpHtml = "";
      if (ev.need_lead || ev.need_follow || ev.need_co) {
        const who = ev.help_requester ? escapeHtml(ev.help_requester) : "Instructor";
        const needs = [];
        if (ev.need_lead) needs.push("Lead");
        if (ev.need_follow) needs.push("Follow");
        if (ev.need_co) needs.push("Co-Instructor");
        helpHtml = `
          <div class="help-box">
            🤝 <strong>${who}</strong> requested help: <strong>${needs.join(", ")}</strong>
            ${
              STATE.viewer
                ? `<div class="action-row">
                    ${ev.need_lead ? `<button class="btn small" onclick="volunteer(${ev.id}, 'lead')">I'll Lead</button>` : ""}
                    ${ev.need_follow ? `<button class="btn small" onclick="volunteer(${ev.id}, 'follow')">I'll Follow</button>` : ""}
                    ${ev.need_co ? `<button class="btn small" onclick="volunteer(${ev.id}, 'co')">I'll Co-Teach</button>` : ""}
                   </div>`
                : ""
            }
          </div>
        `;
      }

      let bidActions = "";
      if (STATE.viewer && !isResolved) {
        if (!ev.my_bid_kind) {
          bidActions = `
            <button class="btn primary small" onclick="placeBid(${ev.id}, 'normal')">Bid on Date</button>
            <button class="btn gold small" onclick="placeBid(${ev.id}, 'token')">Use Veto Token ⭐</button>
          `;
        } else if (ev.my_bid_kind === "normal") {
          bidActions = `
            <button class="btn gold small" onclick="placeBid(${ev.id}, 'token')">Upgrade to Token ⭐</button>
            <button class="btn small" onclick="withdrawBid(${ev.id})">Withdraw Bid</button>
          `;
        } else {
          bidActions = `
            <button class="btn small" onclick="withdrawBid(${ev.id})">Withdraw Token Bid</button>
          `;
        }
      }

      const editBtn =
        STATE.viewer && ev.can_edit_details
          ? `<button class="btn small" onclick="toggleEditDetails(${ev.id})">✏️ Night Details / Ask Help</button>`
          : "";

      const adminForceBtn =
        STATE.viewer && STATE.viewer.is_admin && !isResolved
          ? `<button class="btn small" onclick="forceResolve(${ev.id})">⚡ Force Resolve</button>`
          : "";

      let adminBidsHtml = "";
      if (STATE.viewer && STATE.viewer.is_admin && ev.admin_bids && ev.admin_bids.length > 0) {
        const items = ev.admin_bids
          .map(
            (b, i) =>
              `${i + 1}. ${escapeHtml(b.teacher_name)} (${b.kind}, ${b.fairness}d)`
          )
          .join(" · ");
        adminBidsHtml = `<div class="event-meta">🔒 Admin Bid Queue: ${items}</div>`;
      }

      const drawerOpen = STATE.openEditEventId === ev.id;
      const drawerHtml = drawerOpen
        ? `
        <div class="details-drawer">
          <div class="form-row">
            <select id="edit-type-${ev.id}">
              <option value="salsa" ${ev.lesson_type === "salsa" ? "selected" : ""}>Salsa</option>
              <option value="bachata" ${ev.lesson_type === "bachata" ? "selected" : ""}>Bachata</option>
              <option value="mixed" ${ev.lesson_type === "mixed" ? "selected" : ""}>Salsa &amp; Bachata</option>
              <option value="other" ${ev.lesson_type === "other" ? "selected" : ""}>Other</option>
            </select>
            <input type="text" id="edit-detail-${ev.id}" value="${escapeAttr(ev.lesson_detail)}" placeholder="Topic (e.g. Rueda, Sensual)" />
          </div>
          <div class="form-row">
            <input type="text" id="edit-dj-${ev.id}" value="${escapeAttr(ev.dj)}" placeholder="DJ Name" />
            <input type="text" id="edit-notes-${ev.id}" value="${escapeAttr(ev.notes)}" placeholder="Extra notes" />
          </div>
          <div class="form-row">
            <label><input type="checkbox" id="edit-lead-${ev.id}" ${ev.need_lead ? "checked" : ""}/> Need Lead</label>
            <label><input type="checkbox" id="edit-follow-${ev.id}" ${ev.need_follow ? "checked" : ""}/> Need Follow</label>
            <label><input type="checkbox" id="edit-co-${ev.id}" ${ev.need_co ? "checked" : ""}/> Need Co-Teacher</label>
          </div>
          <div class="action-row">
            <button class="btn primary small" onclick="saveEventDetails(${ev.id})">Save Details</button>
          </div>
        </div>`
        : "";

      return `
        <div class="event-card">
          <div class="event-head">
            <div>
              <div class="event-date">${formatDateNice(ev.date)}</div>
              <div class="event-meta">Lesson: <strong>${escapeHtml(ev.lesson_type.toUpperCase())}</strong>${ev.lesson_detail ? " (" + escapeHtml(ev.lesson_detail) + ")" : ""} · DJ: <strong>${escapeHtml(ev.dj)}</strong></div>
            </div>
            <span class="badge status-${ev.status}">${statusLabel}</span>
          </div>
          ${myBidBadge ? `<div>${myBidBadge}</div>` : ""}
          ${vols ? `<div class="event-meta">Assisting: ${vols}</div>` : ""}
          ${helpHtml}
          ${adminBidsHtml}
          <div class="action-row">
            ${bidActions}
            ${editBtn}
            ${adminForceBtn}
          </div>
          ${drawerHtml}
        </div>
      `;
    })
    .join("");
}

function renderFairness() {
  const days = STATE.config.fairness_window_days || "90";
  document.getElementById("fairness-explainer").textContent =
    `Fewer confirmed teaching nights in the rolling ${days}-day window = higher priority when multiple instructors bid on the same Thursday. Equal start (0) at launch.`;
  document.getElementById("tokens-per-year-label").textContent =
    STATE.config.tokens_per_year || "2";

  const sorted = [...STATE.teachers].sort((a, b) => a.fairness_score - b.fairness_score);
  document.getElementById("fairness-list").innerHTML = sorted
    .map(
      (t, idx) => `
      <div class="roster-row">
        <div><strong>#${idx + 1} ${escapeHtml(t.name)}</strong></div>
        <div>
          <span class="pill">${t.fairness_score} nights (${days}d)</span>
          <span class="pill">⭐ ${t.tokens_available}/${t.tokens_total} tokens</span>
        </div>
      </div>
    `
    )
    .join("");

  const hist = STATE.tokens_used || [];
  document.getElementById("token-history-list").innerHTML = hist.length
    ? hist
        .map(
          (u) => `
        <div class="roster-row">
          <div>⭐ <strong>${escapeHtml(u.teacher)}</strong> reserved <strong>${u.event_date}</strong></div>
          <span class="muted">${u.year} (${escapeHtml(u.reason)})</span>
        </div>
      `
        )
        .join("")
    : `<p class="muted">No Veto Tokens used yet this year.</p>`;
}

function renderDigests() {
  document.getElementById("digest-preview").textContent = STATE.digest || "";
  const resolved = STATE.events.filter((e) => e.status === "resolved" || e.status === "done");
  const container = document.getElementById("fb-events-list");
  if (!resolved.length) {
    container.innerHTML = `<p class="muted">No resolved Thursdays yet. Once a date resolves, its pre-filled Facebook Event description appears here.</p>`;
    return;
  }
  container.innerHTML = resolved
    .map(
      (ev) => `
      <div class="card">
        <div class="card-head">
          <strong>${formatDateNice(ev.date)} — ${escapeHtml(ev.winner_name)}</strong>
          <button class="btn small" onclick="copyText(${JSON.stringify(ev.fb_event_copy)})">Copy FB Event</button>
        </div>
        <pre class="code-box">${escapeHtml(ev.fb_event_copy)}</pre>
      </div>
    `
    )
    .join("");
}

function renderAdmin() {
  if (!STATE.viewer || !STATE.viewer.is_admin) return;
  document.getElementById("transport-mode-label").textContent = STATE.transport_mode;

  document.getElementById("admin-roster-list").innerHTML = STATE.teachers
    .map(
      (t) => `
      <div class="roster-row">
        <div>
          <strong>${escapeHtml(t.name)}</strong> · <span class="muted">${escapeHtml(t.phone)}</span>
          ${t.is_admin ? `<span class="badge status-resolved">Admin</span>` : ""}
        </div>
        <div class="action-row">
          <button class="btn small gold" onclick="grantBonusToken(${t.id})">+1 Token ⭐</button>
          <button class="btn small" onclick="removeTeacher(${t.id})">Remove</button>
        </div>
      </div>
    `
    )
    .join("");

  const cfgLabels = {
    fairness_window_days: "Fairness Window (days)",
    resolve_days_before: "Resolve Deadline (days before)",
    recruit_days_before: "Recruit Alert (days before)",
    tokens_per_year: "Veto Tokens / Year",
    months_open_ahead: "Months Open Ahead",
  };
  document.getElementById("admin-config-fields").innerHTML = Object.entries(cfgLabels)
    .map(
      ([k, label]) => `
      <div class="config-item">
        <label>${label}</label>
        <input type="number" name="${k}" value="${escapeAttr(STATE.config[k] || "")}" />
      </div>
    `
    )
    .join("");

  const outbox = STATE.outbox || [];
  document.getElementById("admin-outbox-list").innerHTML = outbox.length
    ? outbox
        .map(
          (o) => `
        <div class="outbox-row">
          <div>
            <span class="badge status-open">${escapeHtml(o.channel)}</span>
            <strong>${escapeHtml(o.recipient)}</strong>: ${escapeHtml(o.body)}
          </div>
          <span class="muted">${escapeHtml(o.created_at)}</span>
        </div>
      `
        )
        .join("")
    : `<p class="muted">No messages in outbox yet.</p>`;
}

// Actions
window.placeBid = async (eventId, kind) => {
  try {
    await api("/api/bids", { event_id: eventId, kind });
    showToast(kind === "token" ? "⭐ Veto Token bid placed!" : "Bid placed!");
    await loadState();
  } catch (err) {
    showToast(err.message);
  }
};

window.withdrawBid = async (eventId) => {
  try {
    await api("/api/bids/withdraw", { event_id: eventId });
    showToast("Bid withdrawn.");
    await loadState();
  } catch (err) {
    showToast(err.message);
  }
};

window.toggleEditDetails = (eventId) => {
  STATE.openEditEventId = STATE.openEditEventId === eventId ? null : eventId;
  renderEvents();
};

window.saveEventDetails = async (id) => {
  try {
    await api("/api/events/details", {
      event_id: id,
      lesson_type: document.getElementById(`edit-type-${id}`).value,
      lesson_detail: document.getElementById(`edit-detail-${id}`).value,
      dj: document.getElementById(`edit-dj-${id}`).value,
      notes: document.getElementById(`edit-notes-${id}`).value,
      need_lead: document.getElementById(`edit-lead-${id}`).checked,
      need_follow: document.getElementById(`edit-follow-${id}`).checked,
      need_co: document.getElementById(`edit-co-${id}`).checked,
    });
    STATE.openEditEventId = null;
    showToast("Event details updated!");
    await loadState();
  } catch (err) {
    showToast(err.message);
  }
};

window.volunteer = async (eventId, role) => {
  try {
    await api("/api/events/volunteer", { event_id: eventId, role });
    showToast(`Volunteered as ${role}!`);
    await loadState();
  } catch (err) {
    showToast(err.message);
  }
};

window.forceResolve = async (eventId) => {
  try {
    await api("/api/admin/resolve", { event_id: eventId });
    showToast("Date resolved!");
    await loadState();
  } catch (err) {
    showToast(err.message);
  }
};

window.grantBonusToken = async (teacherId) => {
  try {
    await api("/api/admin/tokens/grant", {
      teacher_id: teacherId,
      year: new Date().getUTCFullYear(),
      reason: "special occasion bonus",
    });
    showToast("Granted +1 Veto Token!");
    await loadState();
  } catch (err) {
    showToast(err.message);
  }
};

window.removeTeacher = async (teacherId) => {
  try {
    await api("/api/admin/teachers/remove", { teacher_id: teacherId });
    showToast("Instructor removed from active roster.");
    await loadState();
  } catch (err) {
    showToast(err.message);
  }
};

window.copyText = (txt) => {
  navigator.clipboard.writeText(txt);
  showToast("Copied to clipboard!");
};

function escapeHtml(s) {
  return String(s || "")
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}

function escapeAttr(s) {
  return escapeHtml(s).replace(/"/g, "&quot;");
}

// Wire up static controls
document.addEventListener("DOMContentLoaded", () => {
  document.querySelectorAll("#nav-tabs .tab").forEach((btn) => {
    btn.onclick = () => {
      document.querySelectorAll("#nav-tabs .tab").forEach((b) => b.classList.remove("active"));
      btn.classList.add("active");
      const tab = btn.dataset.tab;
      ["schedule", "fairness", "digests", "admin"].forEach((t) => {
        document.getElementById(`tab-${t}`).classList.toggle("hidden", t !== tab);
      });
    };
  });

  document.getElementById("btn-request-otp").onclick = async () => {
    const phone = document.getElementById("login-phone").value.trim();
    if (!phone) return showToast("Enter your phone number");
    try {
      const res = await api("/api/auth/request-otp", { phone });
      document.getElementById("otp-step-phone").classList.add("hidden");
      document.getElementById("otp-step-code").classList.remove("hidden");
      const hint = document.getElementById("otp-hint");
      if (res.manual_mode_code) {
        hint.textContent = `Manual/Local Mode (WhatsApp bot not linked yet) — your code is: ${res.manual_mode_code}`;
        document.getElementById("login-code").value = res.manual_mode_code;
      } else {
        hint.textContent = "Check your WhatsApp DMs for the 6-digit login code.";
      }
      hint.classList.remove("hidden");
    } catch (err) {
      showToast(err.message);
    }
  };

  document.getElementById("btn-verify-otp").onclick = async () => {
    const phone = document.getElementById("login-phone").value.trim();
    const code = document.getElementById("login-code").value.trim();
    try {
      await api("/api/auth/verify-otp", { phone, code });
      document.getElementById("otp-hint").classList.add("hidden");
      showToast("Signed in!");
      await loadState();
    } catch (err) {
      showToast(err.message);
    }
  };

  document.getElementById("btn-reset-otp").onclick = () => {
    document.getElementById("otp-step-code").classList.add("hidden");
    document.getElementById("otp-step-phone").classList.remove("hidden");
  };

  document.getElementById("btn-copy-digest").onclick = () => {
    window.copyText(STATE.digest || "");
  };

  document.getElementById("form-add-teacher").onsubmit = async (e) => {
    e.preventDefault();
    try {
      await api("/api/admin/teachers", {
        name: document.getElementById("adm-name").value,
        phone: document.getElementById("adm-phone").value,
        handle: document.getElementById("adm-handle").value,
        is_admin: document.getElementById("adm-is-admin").checked,
      });
      e.target.reset();
      showToast("Instructor saved!");
      await loadState();
    } catch (err) {
      showToast(err.message);
    }
  };

  document.getElementById("form-config").onsubmit = async (e) => {
    e.preventDefault();
    const updates = {};
    e.target.querySelectorAll("input[name]").forEach((inp) => {
      updates[inp.name] = inp.value;
    });
    try {
      await api("/api/admin/config", updates);
      showToast("Configuration updated!");
      await loadState();
    } catch (err) {
      showToast(err.message);
    }
  };

  document.getElementById("btn-run-resolver").onclick = async () => {
    try {
      const r = await api("/api/admin/resolve", { event_id: 0 });
      showToast(`Sweep complete: ${r.resolved} resolved, ${r.recruited} recruited, ${r.escalated} escalated.`);
      await loadState();
    } catch (err) {
      showToast(err.message);
    }
  };

  loadState();
});
