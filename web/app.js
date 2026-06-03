const state = {
  services: [],
  checks: [],
  stats: {},
};

const els = {
  total: document.querySelector("#totalMetric"),
  healthy: document.querySelector("#healthyMetric"),
  down: document.querySelector("#downMetric"),
  latency: document.querySelector("#latencyMetric"),
  serviceList: document.querySelector("#serviceList"),
  historyBody: document.querySelector("#historyBody"),
  updatedAt: document.querySelector("#updatedAt"),
  form: document.querySelector("#serviceForm"),
  name: document.querySelector("#nameInput"),
  url: document.querySelector("#urlInput"),
  status: document.querySelector("#statusInput"),
  message: document.querySelector("#formMessage"),
  refresh: document.querySelector("#refreshBtn"),
};

async function api(path, options = {}) {
  const response = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...options,
  });
  const data = await response.json();
  if (!response.ok) {
    throw new Error(data.error || "API error");
  }
  return data;
}

async function loadDashboard() {
  const [services, checks, stats] = await Promise.all([
    api("/api/services"),
    api("/api/checks?limit=50"),
    api("/api/stats"),
  ]);
  state.services = services;
  state.checks = checks;
  state.stats = stats;
  render();
}

function render() {
  els.total.textContent = state.stats.total ?? 0;
  els.healthy.textContent = state.stats.healthy ?? 0;
  els.down.textContent = state.stats.down ?? 0;
  els.latency.textContent = state.stats.averageLatency ?? 0;
  els.updatedAt.textContent = new Intl.DateTimeFormat("ja-JP", {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  }).format(new Date());

  renderServices();
  renderHistory();
}

function renderServices() {
  if (state.services.length === 0) {
    els.serviceList.innerHTML = `<p class="message">監視対象はまだ登録されていません。</p>`;
    return;
  }
  els.serviceList.innerHTML = state.services.map(serviceCard).join("");
  document.querySelectorAll("[data-check]").forEach((button) => {
    button.addEventListener("click", () => checkNow(button.dataset.check));
  });
  document.querySelectorAll("[data-delete]").forEach((button) => {
    button.addEventListener("click", () => deleteService(button.dataset.delete));
  });
}

function serviceCard(service) {
  const status = service.last_ok === null ? "pending" : service.last_ok ? "good" : "bad";
  const label = service.last_ok === null ? "未監視" : service.last_ok ? "正常" : "障害";
  const checked = service.last_checked_at ? formatDate(service.last_checked_at) : "未実行";
  const code = service.last_status ?? "-";
  const latency = service.last_latency_ms === null ? "-" : `${service.last_latency_ms}ms`;

  return `
    <article class="service-row">
      <div class="service-main">
        <div class="service-title">
          <span class="badge ${status}">${label}</span>
          <span>${escapeHTML(service.name)}</span>
        </div>
        <div class="url">${escapeHTML(service.url)}</div>
        <div class="service-meta">
          <span>HTTP ${code}</span>
          <span>${latency}</span>
          <span>${checked}</span>
          <span>期待値 ${service.expected_status}</span>
        </div>
      </div>
      <div class="service-actions">
        <button class="small-button" data-check="${service.id}" title="今すぐチェック">✓</button>
        <button class="small-button" data-delete="${service.id}" title="削除">×</button>
      </div>
    </article>
  `;
}

function renderHistory() {
  if (state.checks.length === 0) {
    els.historyBody.innerHTML = `<tr><td colspan="6">履歴はまだありません。</td></tr>`;
    return;
  }
  els.historyBody.innerHTML = state.checks.map((check) => {
    const status = check.ok ? "good" : "bad";
    const label = check.ok ? "正常" : "障害";
    return `
      <tr>
        <td>${formatDate(check.checked_at)}</td>
        <td>${escapeHTML(check.service_name)}</td>
        <td><span class="badge ${status}">${label}</span></td>
        <td>${check.status_code ?? "-"}</td>
        <td>${check.latency_ms}ms</td>
        <td class="error-cell">${escapeHTML(check.error || "")}</td>
      </tr>
    `;
  }).join("");
}

async function checkNow(id) {
  els.message.textContent = "チェック中...";
  try {
    await api(`/api/services/${id}/check`, { method: "POST" });
    els.message.textContent = "即時チェックを実行しました。";
    await loadDashboard();
  } catch (error) {
    els.message.textContent = error.message;
  }
}

async function deleteService(id) {
  if (!confirm("このURLを削除しますか？")) {
    return;
  }
  try {
    await api(`/api/services/${id}`, { method: "DELETE" });
    await loadDashboard();
  } catch (error) {
    els.message.textContent = error.message;
  }
}

els.form.addEventListener("submit", async (event) => {
  event.preventDefault();
  els.message.textContent = "登録中...";
  try {
    await api("/api/services", {
      method: "POST",
      body: JSON.stringify({
        name: els.name.value,
        url: els.url.value,
        expected_status: Number(els.status.value),
      }),
    });
    els.form.reset();
    els.status.value = 200;
    els.message.textContent = "登録しました。";
    await loadDashboard();
  } catch (error) {
    els.message.textContent = error.message;
  }
});

els.refresh.addEventListener("click", loadDashboard);

function formatDate(value) {
  return new Intl.DateTimeFormat("ja-JP", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  }).format(new Date(value));
}

function escapeHTML(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;");
}

loadDashboard().catch((error) => {
  els.message.textContent = error.message;
});
