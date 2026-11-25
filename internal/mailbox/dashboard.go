package mailbox

import "net/http"

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Wormhole Mailbox Dashboard</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body { font-family: system-ui, -apple-system, sans-serif; background: #f5f5f5; color: #333; line-height: 1.5; }
    header { background: #1a1a2e; color: #fff; padding: 1rem 2rem; display: flex; justify-content: space-between; align-items: center; }
    h1 { font-size: 1.5rem; font-weight: 600; }
    .status { display: flex; align-items: center; gap: 0.5rem; }
    .status-dot { width: 10px; height: 10px; border-radius: 50%; background: #4ade80; }
    .status-dot.warning { background: #fbbf24; }
    .status-dot.error { background: #f87171; }
    main { padding: 1.5rem 2rem; max-width: 1400px; margin: 0 auto; }
    .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(280px, 1fr)); gap: 1rem; margin-bottom: 1.5rem; }
    .card { background: #fff; border-radius: 8px; padding: 1rem; box-shadow: 0 1px 3px rgba(0,0,0,0.08); }
    .card h2 { font-size: 0.875rem; font-weight: 600; color: #666; margin-bottom: 0.5rem; text-transform: uppercase; letter-spacing: 0.05em; }
    .stat-value { font-size: 2rem; font-weight: 700; color: #1a1a2e; }
    .stat-sub { font-size: 0.75rem; color: #999; }
    .charts-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(400px, 1fr)); gap: 1rem; }
    .chart-card { background: #fff; border-radius: 8px; padding: 1rem; box-shadow: 0 1px 3px rgba(0,0,0,0.08); }
    .chart-card h2 { font-size: 0.875rem; font-weight: 600; color: #666; margin-bottom: 0.75rem; text-transform: uppercase; letter-spacing: 0.05em; }
    .chart-card canvas { max-height: 300px; }
    .table-card { background: #fff; border-radius: 8px; padding: 1rem; box-shadow: 0 1px 3px rgba(0,0,0,0.08); }
    table { width: 100%; border-collapse: collapse; font-size: 0.875rem; }
    th, td { padding: 0.5rem; text-align: left; border-bottom: 1px solid #eee; }
    th { font-weight: 600; color: #666; font-size: 0.75rem; text-transform: uppercase; }
    tr:last-child td { border-bottom: none; }
    .badge { display: inline-block; padding: 0.15rem 0.5rem; border-radius: 9999px; font-size: 0.7rem; font-weight: 600; }
    .badge-happy { background: #dcfce7; color: #166534; }
    .badge-lonely { background: #fef3c7; color: #92400e; }
    .badge-pruney { background: #e0e7ff; color: #3730a3; }
    .badge-crowded { background: #fee2e2; color: #991b1b; }
    .badge-scary { background: #fce7f3; color: #9d174d; }
    .badge-errory { background: #f3f4f6; color: #374151; }
    .loading { display: flex; justify-content: center; padding: 3rem; color: #999; }
    .error { background: #fee2e2; color: #991b1b; padding: 1rem; border-radius: 8px; margin: 1rem 0; display: none; }
    .timestamp { font-size: 0.75rem; color: #999; text-align: right; margin-bottom: 0.5rem; }
  </style>
</head>
<body>
  <header>
    <h1>Wormhole Mailbox Dashboard</h1>
    <div class="status">
      <span class="status-dot" id="statusDot"></span>
      <span id="statusText">Loading...</span>
    </div>
  </header>

  <main>
    <div class="error" id="errorBanner"></div>

    <div class="grid" id="overviewGrid">
      <div class="card"><h2>Active Connections</h2><div class="stat-value" id="activeConn">—</div></div>
      <div class="card"><h2>Active Mailboxes</h2><div class="stat-value" id="activeMail">—</div></div>
      <div class="card"><h2>Active Nameplates</h2><div class="stat-value" id="activeName">—</div></div>
      <div class="card"><h2>Messages In Flight</h2><div class="stat-value" id="activeMsg">—</div></div>
      <div class="card"><h2>Retired Nameplates</h2><div class="stat-value" id="retiredName">—</div></div>
      <div class="card"><h2>Retired Mailboxes</h2><div class="stat-value" id="retiredMail">—</div></div>
      <div class="card"><h2>Server Rebooted</h2><div class="stat-value" id="rebooted">—</div></div>
      <div class="card"><h2>Current Snapshot Updated</h2><div class="stat-value" id="currentUpdated">—</div></div>
    </div>

    <div class="charts-grid">
      <div class="chart-card">
        <h2>Nameplate Outcomes</h2>
        <canvas id="nameplateChart"></canvas>
      </div>
      <div class="chart-card">
        <h2>Mailbox Outcomes</h2>
        <canvas id="mailboxChart"></canvas>
      </div>
      <div class="chart-card">
        <h2>Client Implementations</h2>
        <canvas id="clientImplChart"></canvas>
      </div>
      <div class="chart-card">
        <h2>Top Client Versions</h2>
        <canvas id="clientVerChart"></canvas>
      </div>
      <div class="chart-card">
        <h2>Activity (Last 48h)</h2>
        <canvas id="activityChart"></canvas>
      </div>
    </div>

    <div class="charts-grid" style="margin-top: 1rem;">
      <div class="table-card">
        <h2>All Nameplate Results</h2>
        <table id="nameplateTable">
          <thead><tr><th>Result</th><th>Count</th></tr></thead>
          <tbody></tbody>
        </table>
      </div>
      <div class="table-card">
        <h2>All Mailbox Results</h2>
        <table id="mailboxTable">
          <thead><tr><th>Result</th><th>Count</th></tr></thead>
          <tbody></tbody>
        </table>
      </div>
    </div>

    <div class="timestamp">Last updated: <span id="lastUpdated">—</span></div>
  </main>

  <script src="/static/chart.umd.min.js" integrity="sha384-dsN6yzfVr55Hd2g1XIwnt1QJUXEdDvoVVKY3bzYrn0xAvcIYVogKEEYd43E9rKTf" crossorigin="anonymous"></script>
  <script>
    var colors = {
      happy: '#22c55e', lonely: '#f59e0b', pruney: '#6366f1',
      crowded: '#ef4444', scary: '#ec4899', errory: '#9ca3af'
    };
    var charts = {};

    function initCharts() {
      if (typeof Chart === 'undefined') {
        console.warn('Chart.js not loaded, falling back to tables');
        return;
      }
      charts.nameplate = new Chart(document.getElementById('nameplateChart'), { type: 'doughnut', data: { labels: [], datasets: [{ data: [], backgroundColor: [] }] }, options: { responsive: true, maintainAspectRatio: true, plugins: { legend: { position: 'right', labels: { font: { size: 11 } } } } } });
      charts.mailbox = new Chart(document.getElementById('mailboxChart'), { type: 'doughnut', data: { labels: [], datasets: [{ data: [], backgroundColor: [] }] }, options: { responsive: true, maintainAspectRatio: true, plugins: { legend: { position: 'right', labels: { font: { size: 11 } } } } } });
      charts.clientImpl = new Chart(document.getElementById('clientImplChart'), { type: 'bar', data: { labels: [], datasets: [{ data: [], backgroundColor: '#3b82f6' }] }, options: { indexAxis: 'y', responsive: true, maintainAspectRatio: true, plugins: { legend: { display: false } }, scales: { x: { beginAtZero: true } } } } });
      charts.clientVer = new Chart(document.getElementById('clientVerChart'), { type: 'bar', data: { labels: [], datasets: [{ data: [], backgroundColor: '#3b82f6' }] }, options: { indexAxis: 'y', responsive: true, maintainAspectRatio: true, plugins: { legend: { display: false } }, scales: { x: { beginAtZero: true } } } } });
      charts.activity = new Chart(document.getElementById('activityChart'), { type: 'line', data: { labels: [], datasets: [
        { label: 'Nameplates', data: [], borderColor: '#22c55e', backgroundColor: 'rgba(34,197,94,0.1)', fill: true, tension: 0.2 },
        { label: 'Mailboxes', data: [], borderColor: '#3b82f6', backgroundColor: 'rgba(59,130,246,0.1)', fill: true, tension: 0.2 }
      ]}, options: { responsive: true, maintainAspectRatio: true, plugins: { legend: { labels: { font: { size: 11 } } } }, scales: { x: { ticks: { maxTicksLimit: 12, font: { size: 9 } } }, y: { beginAtZero: true } } } });
    }

    function badgeClass(result) {
      return 'badge-' + result.toLowerCase().replace(/[^a-z]/g, '');
    }

    function formatTime(ts) {
      if (!ts) return '\u2014';
      var d = new Date(ts * 1000);
      return d.toLocaleString();
    }

    function updateOverview(snap) {
      document.getElementById('activeConn').textContent = snap.active_connections;
      document.getElementById('activeMail').textContent = snap.active_mailboxes;
      document.getElementById('activeName').textContent = snap.active_nameplates;
      document.getElementById('activeMsg').textContent = snap.active_messages;
      document.getElementById('retiredName').textContent = snap.retired_nameplates;
      document.getElementById('retiredMail').textContent = snap.retired_mailboxes;
      document.getElementById('rebooted').textContent = formatTime(snap.current?.rebooted);
      document.getElementById('currentUpdated').textContent = formatTime(snap.current?.updated);
      document.getElementById('lastUpdated').textContent = new Date(snap.timestamp * 1000).toLocaleTimeString();
    }

    function updateCharts(snap) {
      if (!charts.nameplate) return;

      // Nameplate results
      var npLabels = Object.keys(snap.nameplate_results);
      var npData = npLabels.map(function(k) { return snap.nameplate_results[k]; });
      charts.nameplate.data.labels = npLabels.map(function(l) { return l.charAt(0).toUpperCase() + l.slice(1); });
      charts.nameplate.data.datasets[0].data = npData;
      charts.nameplate.data.datasets[0].backgroundColor = npLabels.map(function(l) { return colors[l] || '#888'; });
      charts.nameplate.update();

      // Mailbox results
      var mbLabels = Object.keys(snap.mailbox_results);
      var mbData = mbLabels.map(function(k) { return snap.mailbox_results[k]; });
      charts.mailbox.data.labels = mbLabels.map(function(l) { return l.charAt(0).toUpperCase() + l.slice(1); });
      charts.mailbox.data.datasets[0].data = mbData;
      charts.mailbox.data.datasets[0].backgroundColor = mbLabels.map(function(l) { return colors[l] || '#888'; });
      charts.mailbox.update();

      // Client implementations
      charts.clientImpl.data.labels = snap.client_implementations.map(function(c) { return c.name; });
      charts.clientImpl.data.datasets[0].data = snap.client_implementations.map(function(c) { return c.count; });
      charts.clientImpl.update();

      // Client versions
      charts.clientVer.data.labels = snap.client_versions.map(function(c) { return c.name; });
      charts.clientVer.data.datasets[0].data = snap.client_versions.map(function(c) { return c.count; });
      charts.clientVer.update();

      // Activity
      charts.activity.data.labels = snap.activity.map(function(b) { return b.bucket.replace('T', ' ') + ':00'; });
      charts.activity.data.datasets[0].data = snap.activity.map(function(b) { return b.nameplates; });
      charts.activity.data.datasets[1].data = snap.activity.map(function(b) { return b.mailboxes; });
      charts.activity.update();
    }

    function updateTables(snap) {
      var npTable = document.querySelector('#nameplateTable tbody');
      npTable.innerHTML = Object.entries(snap.nameplate_results).map(function(e) {
        return '<tr><td><span class="badge ' + badgeClass(e[0]) + '">' + e[0] + '</span></td><td>' + e[1] + '</td></tr>';
      }).join('');

      var mbTable = document.querySelector('#mailboxTable tbody');
      mbTable.innerHTML = Object.entries(snap.mailbox_results).map(function(e) {
        return '<tr><td><span class="badge ' + badgeClass(e[0]) + '">' + e[0] + '</span></td><td>' + e[1] + '</td></tr>';
      }).join('');
    }

    function setStatus(ok, msg) {
      var dot = document.getElementById('statusDot');
      var text = document.getElementById('statusText');
      dot.className = 'status-dot' + (ok ? '' : ' error');
      text.textContent = msg;
    }

    function showError(msg) {
      var banner = document.getElementById('errorBanner');
      banner.textContent = msg;
      banner.style.display = 'block';
    }

    async function fetchStats() {
      try {
        var res = await fetch('/api/stats');
        var json = await res.json();
        if (!json.success) throw new Error(json.error || 'Unknown error');
        var snap = json.data;
        updateOverview(snap);
        updateCharts(snap);
        updateTables(snap);
        setStatus(true, 'Live');
      } catch (e) {
        setStatus(false, 'Error: ' + e.message);
        showError('Failed to load stats: ' + e.message);
      }
    }

    // Start
    initCharts();
    fetchStats();
    setInterval(fetchStats, 10000); // refresh every 10s
  </script>
</body>
</html>
`

// getDashboardHandler returns an http.Handler that serves the dashboard HTML.
func getDashboardHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(dashboardHTML))
	}
}