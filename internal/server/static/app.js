let STATE = {
  viewer: null,
  events: [],
  past_events: [],
  teachers: [],
  tokens_used: [],
  config: {},
  transport_mode: "manual",
  digest: "",
  outbox: [],
  selectedMonth: "all",
  historyRange: "90d",
  openEditEventId: null,
  urlInvite: new URLSearchParams(location.search).get("invite") || "",
  invite: null,
};

function setInviteUI(inviteObj, fallbackToken) {
  const el = document.getElementById("adm-invite-url");
  const statusEl = document.getElementById("adm-invite-status");
  if (!el) return;
  const active = inviteObj ? inviteObj.active : Boolean(fallbackToken);
  const token = inviteObj ? inviteObj.token : fallbackToken;
  const exp = inviteObj && inviteObj.expires_at ? new Date(inviteObj.expires_at) : null;
  if (active && token) {
    el.value = `${location.origin}/?invite=${token}`;
    if (statusEl) {
      statusEl.textContent = exp
        ? `Active — expires ${exp.toLocaleString()}`
        : "Active";
    }
  } else {
    el.value = "";
    if (statusEl) {
      statusEl.textContent = exp
        ? `Expired (${exp.toLocaleDateString()}) — click New Link to open self-signup.`
        : "Self-signup closed (revoked) — click New Link to mint an expiring link.";
    }
  }
}

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
    const err = new Error(data.error || `HTTP ${res.status}`);
    err.status = res.status;
    throw err;
  }
  return data;
}

async function loadState() {
  try {
    const data = await api("/api/state");
    Object.assign(STATE, data);
    renderAll();
  } catch (err) {
    if (err.status === 401) {
      STATE.viewer = null;
      renderAuthBar();
      return;
    }
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
  renderHistory();
  renderDigests();
  renderAdmin();
}

function renderAuthBar() {
  const bar = document.getElementById("auth-bar");
  const loginCard = document.getElementById("login-card");
  const navTabs = document.getElementById("nav-tabs");
  const adminTabBtn = document.getElementById("admin-tab-btn");

  if (!STATE.viewer) {
    bar.innerHTML = `<span class="muted">Sign in to view &amp; claim Thursdays</span>`;
    loginCard.classList.remove("hidden");
    navTabs.classList.add("hidden");
    ["schedule", "fairness", "history", "digests", "admin"].forEach((t) => {
      document.getElementById(`tab-${t}`).classList.add("hidden");
    });
    adminTabBtn.classList.add("hidden");
    return;
  }

  loginCard.classList.add("hidden");
  navTabs.classList.remove("hidden");
  const activeBtn = document.querySelector("#nav-tabs .tab.active");
  const activeTab = activeBtn ? activeBtn.dataset.tab : "schedule";
  ["schedule", "fairness", "history", "digests", "admin"].forEach((t) => {
    document.getElementById(`tab-${t}`).classList.toggle("hidden", t !== activeTab);
  });
  if (STATE.viewer.is_admin) {
    adminTabBtn.classList.remove("hidden");
  } else {
    adminTabBtn.classList.add("hidden");
  }

  bar.innerHTML = `
    <span class="pill">👤 <strong>${escapeHtml(STATE.viewer.name)}</strong>${STATE.viewer.is_admin ? " (Admin)" : ""}</span>
    <span class="pill" title="Confirmed teaching nights in rolling fairness window">⚖️ ${STATE.viewer.fairness_score} nights (${STATE.config.fairness_window_days || 90}d)</span>
    <span class="pill" title="Remaining annual Dibs">⭐ ${STATE.viewer.tokens_available}/${STATE.viewer.tokens_total} Dibs</span>
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
      const isSkipped = ev.status === "skipped";
      let statusLabel = "Open to Teach";
      if (ev.status === "tentative") statusLabel = `Requested · Resolves ${ev.resolve_deadline}`;
      if (ev.status === "recruiting") statusLabel = `Recruiting · Needs Instructor!`;
      if (isResolved) statusLabel = `Confirmed: ${escapeHtml(ev.winner_name)}`;
      if (isSkipped) statusLabel = ev.notes ? `Skipped: ${escapeHtml(ev.notes)}` : "Skipped (No Dancing)";

      const myBidBadge = ev.my_bid_kind
        ? `<span class="badge status-tentative">${ev.my_bid_kind === "token" ? "⭐ You Called Dibs" : "You Asked to Teach"}</span>`
        : "";

      const vols = (ev.volunteers || [])
        .map((v) => {
          const canRemove =
            STATE.viewer &&
            (v.teacher_id === STATE.viewer.id || ev.can_edit_details || STATE.viewer.is_admin);
          const rmBtn = canRemove
            ? ` <button class="pill-remove" title="Remove from role" onclick="unvolunteer(${ev.id}, ${v.teacher_id}, '${escapeAttr(v.role)}')">✕</button>`
            : "";
          return `<span class="pill">${escapeHtml(v.name)} (${escapeHtml(v.role)})${rmBtn}</span>`;
        })
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
              ev.can_volunteer
                ? `<div class="action-row">
                    ${ev.need_lead ? `<button class="btn small" onclick="volunteer(${ev.id}, 'lead')">I'll Lead</button>` : ""}
                    ${ev.need_follow ? `<button class="btn small" onclick="volunteer(${ev.id}, 'follow')">I'll Follow</button>` : ""}
                    ${ev.need_co ? `<button class="btn small" onclick="volunteer(${ev.id}, 'co')">I'll Co-Teach</button>` : ""}
                   </div>`
                : `<div class="event-meta">Waiting for another instructor to volunteer.</div>`
            }
          </div>
        `;
      }

      let bidActions = "";
      if (STATE.viewer && !isResolved && !isSkipped) {
        if (!ev.my_bid_kind) {
          bidActions = `
            <button class="btn primary small" onclick="placeBid(${ev.id}, 'normal')">Ask to Teach</button>
            <button class="btn gold small" onclick="placeBid(${ev.id}, 'token')">Call Dibs ⭐</button>
          `;
        } else if (ev.my_bid_kind === "normal") {
          bidActions = `
            <button class="btn gold small" onclick="placeBid(${ev.id}, 'token')">Call Dibs Instead ⭐</button>
            <button class="btn small" onclick="withdrawBid(${ev.id})">Cancel Request</button>
          `;
        } else {
          bidActions = `
            <button class="btn small" onclick="withdrawBid(${ev.id})">Cancel Dibs</button>
          `;
        }
      }

      const editBtn =
        STATE.viewer && ev.can_edit_details && !isSkipped
          ? `<button class="btn small" onclick="toggleEditDetails(${ev.id})">✏️ Night Details / Ask Help</button>`
          : "";

      const adminForceBtn =
        STATE.viewer && STATE.viewer.is_admin && !isResolved && !isSkipped
          ? `<button class="btn small" onclick="forceResolve(${ev.id})">⚡ Force Resolve</button>`
          : "";

      const adminSkipBtn =
        STATE.viewer && STATE.viewer.is_admin
          ? isSkipped
            ? `<button class="btn small" onclick="toggleSkipDate(${ev.id}, false)">↩️ Re-Open Date</button>`
            : `<button class="btn small" onclick="toggleSkipDate(${ev.id}, true)">🚫 Skip Date</button>`
          : "";

      let adminBidsHtml = "";
      if (STATE.viewer && STATE.viewer.is_admin && ev.admin_bids && ev.admin_bids.length > 0) {
        const items = ev.admin_bids
          .map(
            (b, i) =>
              `${i + 1}. ${escapeHtml(b.teacher_name)} (${b.kind === "token" ? "dibs" : "request"}, ${b.fairness}d)`
          )
          .join(" · ");
        adminBidsHtml = `<div class="event-meta">🔒 Admin Request Queue: ${items}</div>`;
      }

      const drawerOpen = STATE.openEditEventId === ev.id && ev.can_edit_details && !isSkipped;
      const drawerHtml = drawerOpen
        ? `
        <div class="details-drawer">
          <div class="field-group">
            <label>Dance Style for the Lesson</label>
            <select id="edit-type-${ev.id}">
              <option value="salsa" ${ev.lesson_type === "salsa" ? "selected" : ""}>Salsa</option>
              <option value="bachata" ${ev.lesson_type === "bachata" ? "selected" : ""}>Bachata</option>
              <option value="mixed" ${ev.lesson_type === "mixed" ? "selected" : ""}>Salsa &amp; Bachata</option>
              <option value="cumbia" ${ev.lesson_type === "cumbia" ? "selected" : ""}>Cumbia</option>
              <option value="other" ${ev.lesson_type === "other" ? "selected" : ""}>Other</option>
            </select>
          </div>
          <div class="field-group">
            <label>Lesson Focus / Level (optional)</label>
            <input type="text" id="edit-detail-${ev.id}" value="${escapeAttr(ev.lesson_detail)}" placeholder="e.g. Beginner Turn Patterns, Rueda, Sensual" />
          </div>
          <div class="field-group">
            <label>Who is DJing? (leave blank if not decided yet)</label>
            <input type="text" id="edit-dj-${ev.id}" value="${escapeAttr(ev.dj)}" placeholder="e.g. DJ Sebas" />
          </div>
          <div class="field-group">
            <label>Extra Notes for the Night (optional)</label>
            <input type="text" id="edit-notes-${ev.id}" value="${escapeAttr(ev.notes)}" placeholder="e.g. Birthday dance at 9:30pm" />
          </div>
          <div class="field-group">
            <label>Ask Other Instructors for Help</label>
            <div class="form-row">
              <label><input type="checkbox" id="edit-lead-${ev.id}" ${ev.need_lead ? "checked" : ""}/> Need a Lead</label>
              <label><input type="checkbox" id="edit-follow-${ev.id}" ${ev.need_follow ? "checked" : ""}/> Need a Follow</label>
              <label><input type="checkbox" id="edit-co-${ev.id}" ${ev.need_co ? "checked" : ""}/> Need a Co-Instructor</label>
            </div>
          </div>
          <div class="action-row">
            <button class="btn primary small" onclick="saveEventDetails(${ev.id})">Save Night Details</button>
          </div>
        </div>`
        : "";

      const djDisplay = ev.dj ? escapeHtml(ev.dj) : "TBD";
      const sublineHtml = isSkipped
        ? `<div class="event-meta">${ev.notes ? `🚫 No Dancing — <strong>${escapeHtml(ev.notes)}</strong>` : "🚫 No Dancing (Holiday / Skipped)"}</div>`
        : `<div class="event-meta">Lesson: <strong>${escapeHtml(ev.lesson_type.toUpperCase())}</strong>${ev.lesson_detail ? " (" + escapeHtml(ev.lesson_detail) + ")" : ""} · DJ: <strong>${djDisplay}</strong></div>`;

      return `
        <div class="event-card">
          <div class="event-head">
            <div>
              <div class="event-date">${formatDateNice(ev.date)}</div>
              ${sublineHtml}
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
            ${adminSkipBtn}
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
    `Fewer confirmed teaching nights in the rolling ${days}-day window = higher priority when multiple instructors ask to teach the same Thursday. Equal start (0) at launch.`;
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
          <span class="pill">⭐ ${t.tokens_available}/${t.tokens_total} dibs</span>
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
          <div>⭐ <strong>${escapeHtml(u.teacher)}</strong> called dibs on <strong>${u.event_date}</strong></div>
          <span class="muted">${u.year} (${escapeHtml(u.reason)})</span>
        </div>
      `
        )
        .join("")
    : `<p class="muted">Nobody has called dibs yet this year.</p>`;

  const publicRules = [
    { label: "Fairness Window", val: `${STATE.config.fairness_window_days || "90"} days rolling` },
    { label: "Requests Lock & Resolve", val: `T-${STATE.config.resolve_days_before || "7"} days before` },
    { label: "Open-Date Recruit Nudge", val: `T-${STATE.config.recruit_days_before || "14"} days before` },
    { label: "Dibs Allowed", val: `${STATE.config.tokens_per_year || "2"} / calendar year` },
    { label: "Schedule Horizon", val: `${STATE.config.months_open_ahead || "6"} months ahead` },
  ];
  const pubEl = document.getElementById("public-config-list");
  if (pubEl) {
    pubEl.innerHTML = publicRules
      .map(
        (r) => `
        <div class="config-stat">
          <span class="config-stat-label">${escapeHtml(r.label)}</span>
          <strong class="config-stat-val">${escapeHtml(r.val)}</strong>
        </div>
      `
      )
      .join("");
  }
}

function renderHistory() {
  const filterEl = document.getElementById("history-filters");
  if (!filterEl) return;

  const currentYear = String(new Date().getFullYear());
  const ranges = [
    { key: "30d", label: "Last 30 Days" },
    { key: "90d", label: "Last 90 Days" },
    { key: "180d", label: "Last 6 Months" },
    { key: "365d", label: "Last Year" },
    { key: "ytd", label: `This Year (${currentYear})` },
    { key: "all", label: "All Time" },
  ];

  filterEl.innerHTML = ranges
    .map(
      (r) =>
        `<button class="btn small ${STATE.historyRange === r.key ? "primary" : ""}" data-range="${r.key}">${escapeHtml(r.label)}</button>`
    )
    .join("");
  filterEl.querySelectorAll("button").forEach((btn) => {
    btn.onclick = () => {
      STATE.historyRange = btn.dataset.range;
      renderHistory();
    };
  });

  const now = new Date();
  let cutoffYMD = "0000-01-01";
  let rangeLabel = "All Time";
  const foundRange = ranges.find((r) => r.key === STATE.historyRange);
  if (foundRange) rangeLabel = foundRange.label;

  if (STATE.historyRange === "30d") {
    cutoffYMD = new Date(now.getTime() - 30 * 86400000).toISOString().slice(0, 10);
  } else if (STATE.historyRange === "90d") {
    cutoffYMD = new Date(now.getTime() - 90 * 86400000).toISOString().slice(0, 10);
  } else if (STATE.historyRange === "180d") {
    cutoffYMD = new Date(now.getTime() - 180 * 86400000).toISOString().slice(0, 10);
  } else if (STATE.historyRange === "365d") {
    cutoffYMD = new Date(now.getTime() - 365 * 86400000).toISOString().slice(0, 10);
  } else if (STATE.historyRange === "ytd") {
    cutoffYMD = `${currentYear}-01-01`;
  }

  const pastInWindow = (STATE.past_events || []).filter((e) => e.date >= cutoffYMD);

  // Aggregate stats per teacher
  const statsMap = new Map();
  for (const t of STATE.teachers || []) {
    statsMap.set(t.id, {
      id: t.id,
      name: t.name,
      taught: 0,
      dibsUsed: 0,
      volunteered: 0,
      lastTaught: "",
      styles: {},
    });
  }

  for (const ev of pastInWindow) {
    if ((ev.status === "resolved" || ev.status === "done") && ev.winner_teacher_id) {
      if (!statsMap.has(ev.winner_teacher_id)) {
        statsMap.set(ev.winner_teacher_id, {
          id: ev.winner_teacher_id,
          name: ev.winner_name || "Instructor",
          taught: 0,
          dibsUsed: 0,
          volunteered: 0,
          lastTaught: "",
          styles: {},
        });
      }
      const st = statsMap.get(ev.winner_teacher_id);
      st.taught += 1;
      if (ev.resolved_via === "token") st.dibsUsed += 1;
      if (!st.lastTaught || ev.date > st.lastTaught) st.lastTaught = ev.date;
      const styleKey = (ev.lesson_type || "salsa").toUpperCase();
      st.styles[styleKey] = (st.styles[styleKey] || 0) + 1;
    }
    for (const v of ev.volunteers || []) {
      if (!statsMap.has(v.teacher_id)) {
        statsMap.set(v.teacher_id, {
          id: v.teacher_id,
          name: v.name || "Instructor",
          taught: 0,
          dibsUsed: 0,
          volunteered: 0,
          lastTaught: "",
          styles: {},
        });
      }
      statsMap.get(v.teacher_id).volunteered += 1;
    }
  }

  const rows = Array.from(statsMap.values()).sort(
    (a, b) => b.taught - a.taught || b.volunteered - a.volunteered || a.name.localeCompare(b.name)
  );

  const totalTaughtNights = pastInWindow.filter(
    (e) => e.status === "resolved" || e.status === "done"
  ).length;
  const totalSkippedNights = pastInWindow.filter((e) => e.status === "skipped").length;

  document.getElementById("history-stats-subtext").textContent =
    `${rangeLabel}: ${totalTaughtNights} taught Thursday${totalTaughtNights === 1 ? "" : "s"}${totalSkippedNights ? ` · ${totalSkippedNights} skipped` : ""}`;

  document.getElementById("history-teacher-stats").innerHTML = rows.length
    ? rows
        .map((st) => {
          const styleSummary = Object.entries(st.styles)
            .map(([k, v]) => `${k}: ${v}`)
            .join(", ");
          return `
          <div class="roster-row">
            <div>
              <strong>${escapeHtml(st.name)}</strong>
              <div class="event-meta">
                ${st.lastTaught ? `Last taught: ${st.lastTaught}` : "No past nights in range"}
                ${styleSummary ? ` · ${escapeHtml(styleSummary)}` : ""}
              </div>
            </div>
            <div class="action-row" style="margin-top:0;padding-top:0;">
              <span class="pill">🕺 <strong>${st.taught}</strong> taught</span>
              ${st.volunteered ? `<span class="pill">🙌 ${st.volunteered} assisted</span>` : ""}
              ${st.dibsUsed ? `<span class="pill">⭐ ${st.dibsUsed} dibs</span>` : ""}
            </div>
          </div>
        `;
        })
        .join("")
    : `<p class="muted">No instructors on roster yet.</p>`;

  document.getElementById("history-log-subtext").textContent =
    pastInWindow.length
      ? `Showing ${pastInWindow.length} past Thursday${pastInWindow.length === 1 ? "" : "s"} (${rangeLabel}, newest first).`
      : `No past Thursdays recorded in ${rangeLabel.toLowerCase()}.`;

  document.getElementById("history-events-list").innerHTML = pastInWindow.length
    ? pastInWindow
        .map((ev) => {
          if (ev.status === "skipped") {
            return `
              <div class="roster-row">
                <div>
                  <strong>${formatDateNice(ev.date)}</strong>
                  <div class="event-meta">🚫 Skipped${ev.notes ? ` — ${escapeHtml(ev.notes)}` : ""}</div>
                </div>
                <span class="badge status-skipped">Skipped</span>
              </div>
            `;
          }
          const vols = (ev.volunteers || [])
            .map((v) => `${escapeHtml(v.name)} (${escapeHtml(v.role)})`)
            .join(", ");
          const teacherLabel = ev.winner_name ? escapeHtml(ev.winner_name) : "Unassigned";
          const djLabel = ev.dj ? escapeHtml(ev.dj) : "TBD";
          return `
            <div class="roster-row">
              <div>
                <strong>${formatDateNice(ev.date)}</strong> — <strong>${teacherLabel}</strong>
                <div class="event-meta">
                  Lesson: ${escapeHtml((ev.lesson_type || "salsa").toUpperCase())}${ev.lesson_detail ? ` (${escapeHtml(ev.lesson_detail)})` : ""} · DJ: ${djLabel}
                  ${vols ? ` · Assisting: ${vols}` : ""}
                </div>
              </div>
              <span class="badge status-${ev.status}">${ev.resolved_via === "token" ? "⭐ Dibs" : "Taught"}</span>
            </div>
          `;
        })
        .join("")
    : `<p class="muted">Past Thursdays will appear here once their date has passed.</p>`;
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
  setInviteUI(STATE.invite, STATE.invite_token);

  document.getElementById("admin-roster-list").innerHTML = STATE.teachers
    .map(
      (t) => `
      <div class="roster-row">
        <div>
          <strong>${escapeHtml(t.name)}</strong>
          ${t.email ? ` · <span class="muted">${escapeHtml(t.email)}</span>` : ""}
          ${t.phone ? ` · <span class="muted">${escapeHtml(t.phone)}</span>` : ""}
          ${t.is_admin ? `<span class="badge status-resolved">Admin</span>` : ""}
        </div>
        <div class="action-row">
          <button class="btn small gold" onclick="grantBonusToken(${t.id})">+1 Dibs ⭐</button>
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
    tokens_per_year: "Dibs / Year",
    months_open_ahead: "Months Open Ahead",
    invite_ttl_days: "Invite Link Lifetime (days)",
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
    showToast(kind === "token" ? "⭐ Called dibs on this Thursday!" : "Asked to teach this Thursday!");
    await loadState();
  } catch (err) {
    showToast(err.message);
  }
};

window.withdrawBid = async (eventId) => {
  try {
    await api("/api/bids/withdraw", { event_id: eventId });
    if (STATE.openEditEventId === eventId) STATE.openEditEventId = null;
    showToast("Request canceled.");
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

window.unvolunteer = async (eventId, teacherId, role) => {
  try {
    await api("/api/events/unvolunteer", {
      event_id: eventId,
      teacher_id: teacherId,
      role,
    });
    showToast(`Removed from ${role} role.`);
    await loadState();
  } catch (err) {
    showToast(err.message);
  }
};

window.forceResolve = async (eventId) => {
  try {
    await api("/api/admin/resolve", { event_id: eventId });
    if (STATE.openEditEventId === eventId) STATE.openEditEventId = null;
    showToast("Date resolved!");
    await loadState();
  } catch (err) {
    showToast(err.message);
  }
};

window.toggleSkipDate = async (eventId, skip) => {
  try {
    let reason = "";
    if (skip) {
      const input = window.prompt("Optional reason for skipping this Thursday (e.g. Thanksgiving, Christmas):", "Holiday");
      if (input === null) return;
      reason = input.trim();
    }
    await api("/api/admin/events/skip", { event_id: eventId, skip, reason });
    if (STATE.openEditEventId === eventId) STATE.openEditEventId = null;
    showToast(skip ? "Thursday marked as skipped." : "Thursday re-opened for teaching requests!");
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
    showToast("Granted +1 Dibs!");
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
      ["schedule", "fairness", "history", "digests", "admin"].forEach((t) => {
        document.getElementById(`tab-${t}`).classList.toggle("hidden", t !== tab);
      });
    };
  });

  const applySignupMode = (open) => {
    const fields = document.getElementById("signup-fields");
    const heading = document.getElementById("login-heading");
    const subtext = document.getElementById("login-subtext");
    const toggle = document.getElementById("signup-toggle");
    fields.classList.toggle("hidden", !open);
    if (open) {
      heading.textContent = "Join Scrumpy's Instructor Roster";
      subtext.textContent =
        "Enter your name and email below — we'll email a 6-digit verification code before adding you to the roster.";
      toggle.textContent = "Already on the roster? Sign in instead";
    } else {
      heading.textContent = "Instructor Sign-In";
      subtext.textContent = "Enter your email address to receive a 6-digit sign-in code.";
      toggle.textContent = "Use group invite link";
    }
  };

  if (STATE.urlInvite) {
    document.getElementById("signup-invite").value = STATE.urlInvite;
    applySignupMode(true);
  }

  document.getElementById("btn-request-otp").onclick = async () => {
    const contact = document.getElementById("login-email").value.trim();
    if (!contact) return showToast("Enter your email address");
    const isEmail = contact.includes("@");
    try {
      const res = await api("/api/auth/request-otp", {
        email: isEmail ? contact : "",
        phone: isEmail ? document.getElementById("signup-phone").value.trim() : contact,
        name: document.getElementById("signup-name").value.trim(),
        invite: document.getElementById("signup-invite").value.trim() || STATE.urlInvite,
      });
      document.getElementById("otp-step-email").classList.add("hidden");
      document.getElementById("signup-fields").classList.add("hidden");
      document.getElementById("signup-toggle-row").classList.add("hidden");
      document.getElementById("otp-step-code").classList.remove("hidden");
      const hint = document.getElementById("otp-hint");
      if (res.manual_mode_code) {
        hint.textContent = `Local Mode — your 6-digit code is: ${res.manual_mode_code}`;
        document.getElementById("login-code").value = res.manual_mode_code;
      } else {
        hint.textContent = `Sent a 6-digit code to ${contact} — check your inbox (and spam folder).`;
      }
      hint.classList.remove("hidden");
      document.getElementById("login-code").focus();
    } catch (err) {
      showToast(err.message);
    }
  };

  document.getElementById("btn-verify-otp").onclick = async () => {
    const contact = document.getElementById("login-email").value.trim();
    const isEmail = contact.includes("@");
    const code = document.getElementById("login-code").value.trim();
    try {
      await api("/api/auth/verify-otp", {
        email: isEmail ? contact : "",
        phone: isEmail ? "" : contact,
        code,
      });
      document.getElementById("otp-hint").classList.add("hidden");
      if (STATE.urlInvite) {
        history.replaceState({}, "", location.pathname);
        STATE.urlInvite = "";
      }
      showToast("Signed in!");
      await loadState();
    } catch (err) {
      showToast(err.message);
    }
  };

  document.getElementById("btn-reset-otp").onclick = () => {
    document.getElementById("otp-step-code").classList.add("hidden");
    document.getElementById("otp-step-email").classList.remove("hidden");
    document.getElementById("signup-toggle-row").classList.remove("hidden");
    if (STATE.urlInvite || document.getElementById("signup-name").value.trim()) {
      applySignupMode(true);
    }
  };

  document.getElementById("signup-toggle").onclick = (e) => {
    e.preventDefault();
    const currentlyHidden = document.getElementById("signup-fields").classList.contains("hidden");
    applySignupMode(currentlyHidden);
  };

  document.getElementById("btn-copy-invite").onclick = () => {
    const val = document.getElementById("adm-invite-url").value;
    if (!val) return showToast("No active invite link — click New Link first.");
    copyText(val);
  };

  document.getElementById("btn-rotate-invite").onclick = async () => {
    try {
      const res = await api("/api/admin/invite/rotate", {});
      setInviteUI({ token: res.invite_token, expires_at: res.expires_at, active: res.active });
      showToast("Minted new expiring invite link — previous link is dead.");
    } catch (err) {
      showToast(err.message);
    }
  };

  document.getElementById("btn-revoke-invite").onclick = async () => {
    try {
      await api("/api/admin/invite/revoke", {});
      setInviteUI({ token: "", expires_at: "", active: false });
      showToast("Invite link revoked — self-signup is now closed.");
    } catch (err) {
      showToast(err.message);
    }
  };

  document.getElementById("btn-copy-digest").onclick = () => {
    window.copyText(STATE.digest || "");
  };

  document.getElementById("form-add-teacher").onsubmit = async (e) => {
    e.preventDefault();
    try {
      await api("/api/admin/teachers", {
        name: document.getElementById("adm-name").value,
        email: document.getElementById("adm-email").value,
        phone: document.getElementById("adm-phone").value,
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
