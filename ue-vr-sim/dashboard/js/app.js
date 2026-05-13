// ── 5G VR Network Monitor — Dashboard App ──────────────────────────────
const API = '';           // same origin (served by ue-vr-sim on :3000)
const NWDAF = '';         // proxied through same origin at /nwdaf/*
const POLL_MS = 500;

// ── Chart.js Setup ─────────────────────────────────────────────────────
const SESSION_COLORS = [
    'rgba(99,102,241,.9)',  // blue
    'rgba(236,72,153,.9)',  // magenta
    'rgba(20,184,166,.9)',  // cyan
    'rgba(245,158,11,.9)', // amber
    'rgba(139,92,246,.9)', // purple
    'rgba(34,197,94,.9)',  // green
];

let tpChart, heatmapChart, sparkChart;
const tpHistory = {};     // sessionId -> [{t, y}]
const MAX_POINTS = 120;   // 60s at 500ms poll

function initCharts() {
    // Throughput line chart
    const tpCtx = document.getElementById('throughputChart').getContext('2d');
    tpChart = new Chart(tpCtx, {
        type: 'line',
        data: { datasets: [] },
        options: {
            responsive: true, maintainAspectRatio: false,
            animation: { duration: 0 },
            scales: {
                x: { type: 'linear', display: true, title: { display: true, text: 'Time (s)', color: '#9892a6' },
                     ticks: { color: '#9892a6', maxTicksLimit: 10 }, grid: { color: 'rgba(255,255,255,.04)' } },
                y: { title: { display: true, text: 'Mbps', color: '#9892a6' }, min: 0,
                     ticks: { color: '#9892a6' }, grid: { color: 'rgba(255,255,255,.04)' } },
            },
            plugins: { legend: { labels: { color: '#f0eef6', usePointStyle: true, pointStyle: 'rect', padding: 14 } } },
            elements: { point: { radius: 0 }, line: { tension: 0.2, borderWidth: 2 } },
            interaction: { intersect: false, mode: 'index' },
        }
    });

    // Latency heatmap (bar chart rotated)
    const hmCtx = document.getElementById('latencyHeatmap').getContext('2d');
    heatmapChart = new Chart(hmCtx, {
        type: 'bar',
        data: { labels: [], datasets: [] },
        options: {
            indexAxis: 'y', responsive: true, maintainAspectRatio: false,
            animation: { duration: 200 },
            scales: {
                x: { stacked: true, display: true, title: { display: true, text: 'Samples', color: '#9892a6' },
                     ticks: { color: '#9892a6' }, grid: { color: 'rgba(255,255,255,.04)' } },
                y: { stacked: true, ticks: { color: '#f0eef6', font: { size: 11 } }, grid: { display: false } },
            },
            plugins: { legend: { labels: { color: '#f0eef6', usePointStyle: true, padding: 10, font: { size: 11 } } } },
        }
    });

    // Sparkline
    const spCtx = document.getElementById('sparkTP').getContext('2d');
    sparkChart = new Chart(spCtx, {
        type: 'line',
        data: { labels: [], datasets: [{ data: [], borderColor: 'rgba(99,102,241,.7)', borderWidth: 1.5, fill: { target: 'origin', above: 'rgba(99,102,241,.1)' }, pointRadius: 0, tension: .3 }] },
        options: { responsive: true, maintainAspectRatio: false, animation: { duration: 0 },
            scales: { x: { display: false }, y: { display: false } }, plugins: { legend: { display: false } } }
    });
}

// ── Data Fetching ──────────────────────────────────────────────────────
let tickCount = 0;

async function fetchSessions() {
    try {
        const res = await fetch(API + '/api/sessions');
        return await res.json();
    } catch { return []; }
}

async function fetchAggregated() {
    try {
        const res = await fetch(API + '/api/aggregated');
        return await res.json();
    } catch { return null; }
}

async function fetchNWDAFAnalytics() {
    try {
        const res = await fetch(NWDAF + '/nnwdaf-analyticsinfo/v1/analytics?event-type=SERVICE_EXPERIENCE');
        return await res.json();
    } catch { return null; }
}

async function checkNWDAF() {
    try {
        const res = await fetch(NWDAF + '/nnwdaf-oam/v1/');
        const data = await res.json();
        document.getElementById('nwdafDot').className = 'dot green';
        document.getElementById('nwdafText').textContent = 'NWDAF Connected';
        return true;
    } catch {
        document.getElementById('nwdafDot').className = 'dot red';
        document.getElementById('nwdafText').textContent = 'NWDAF Offline';
        return false;
    }
}

// ── UI Updates ─────────────────────────────────────────────────────────
function updateClock() {
    document.getElementById('clock').textContent = new Date().toLocaleTimeString();
}

function updateStatCards(agg) {
    if (!agg) return;
    document.getElementById('totalThroughput').textContent = agg.totalThroughput?.toFixed(1) || '0';
    document.getElementById('avgLatency').textContent = agg.avgLatency?.toFixed(1) || '0';
    document.getElementById('sessionCountText').textContent = `${agg.sessionCount || 0} Active Session${agg.sessionCount !== 1 ? 's' : ''}`;

    // Latency indicator
    const li = document.getElementById('latencyIndicator');
    const lat = agg.avgLatency || 0;
    if (lat < 10) { li.textContent = '● Good'; li.className = 'stat-indicator good'; }
    else if (lat < 20) { li.textContent = '● Warning'; li.className = 'stat-indicator warn'; }
    else { li.textContent = '● Critical'; li.className = 'stat-indicator bad'; }

    // Sparkline
    sparkChart.data.labels.push('');
    sparkChart.data.datasets[0].data.push(agg.totalThroughput || 0);
    if (sparkChart.data.labels.length > 30) {
        sparkChart.data.labels.shift();
        sparkChart.data.datasets[0].data.shift();
    }
    sparkChart.update();
}

function updateMOS(analytics) {
    const mos = analytics?.overallMOS || 0;
    const pct = Math.round((mos / 5.0) * 100);
    document.getElementById('mosValue').textContent = mos.toFixed(1);
    document.getElementById('mosPercent').textContent = pct + '%';
    const circ = 2 * Math.PI * 34; // circumference
    document.getElementById('mosArc').setAttribute('stroke-dasharray', `${circ * pct / 100} ${circ}`);

    const label = document.getElementById('mosLabel');
    if (mos >= 4.0) { label.textContent = 'Excellent'; label.className = 'stat-indicator mos-label good'; }
    else if (mos >= 3.0) { label.textContent = 'Good'; label.className = 'stat-indicator mos-label warn'; }
    else if (mos > 0) { label.textContent = 'Poor'; label.className = 'stat-indicator mos-label bad'; }
    else { label.textContent = 'N/A'; label.className = 'stat-indicator mos-label'; }
}

function updateAnalyticsPanel(analytics) {
    if (!analytics) return;
    const qos = analytics.qosSustainability || 0;
    document.getElementById('qosBar').style.width = qos + '%';
    document.getElementById('qosVal').textContent = qos.toFixed(0) + '%';
    const cong = analytics.congestionLevel || 0;
    document.getElementById('congestionBar').style.width = Math.min(cong, 100) + '%';
    document.getElementById('congestionVal').textContent = cong.toFixed(0) + '%';
    document.getElementById('pktLossVal').textContent = (analytics.avgPacketLossPercent || 0).toFixed(3) + '%';

    const anomaly = document.getElementById('anomalyStatus');
    if (analytics.anomalyDetected) {
        anomaly.textContent = analytics.anomalyDetails || 'Detected!';
        anomaly.className = 'analytics-status bad';
    } else {
        anomaly.textContent = 'No anomalies';
        anomaly.className = 'analytics-status green';
    }
}

function updateThroughputChart(sessions) {
    if (!sessions) return;
    tickCount++;
    const timeS = (tickCount * POLL_MS / 1000).toFixed(1);

    sessions.forEach((s, i) => {
        if (!tpHistory[s.id]) tpHistory[s.id] = [];
        tpHistory[s.id].push({ x: parseFloat(timeS), y: s.current?.throughputMbps || 0 });
        if (tpHistory[s.id].length > MAX_POINTS) tpHistory[s.id].shift();
    });

    // Remove stale sessions
    const activeIds = new Set(sessions.map(s => s.id));
    Object.keys(tpHistory).forEach(id => { if (!activeIds.has(id)) delete tpHistory[id]; });

    // Rebuild datasets
    tpChart.data.datasets = Object.entries(tpHistory).map(([id, data], i) => ({
        label: id.substring(0, 12),
        data: [...data],
        borderColor: SESSION_COLORS[i % SESSION_COLORS.length],
        backgroundColor: SESSION_COLORS[i % SESSION_COLORS.length].replace('.9', '.1'),
        fill: false,
    }));
    tpChart.update();
}

function updateLatencyHeatmap(sessions) {
    if (!sessions || sessions.length === 0) return;
    const labels = sessions.map(s => s.id.substring(0, 12));
    const buckets = ['<5ms', '5-10ms', '10-20ms', '>20ms'];
    const colors = ['rgba(34,197,94,.7)', 'rgba(245,158,11,.7)', 'rgba(249,115,22,.7)', 'rgba(239,68,68,.7)'];

    // Simple: classify current latency into bucket (show as stacked bars from history)
    const data = [[], [], [], []];
    sessions.forEach(s => {
        const lat = s.current?.latencyMs || 0;
        data[0].push(lat < 5 ? 1 : 0);
        data[1].push(lat >= 5 && lat < 10 ? 1 : 0);
        data[2].push(lat >= 10 && lat < 20 ? 1 : 0);
        data[3].push(lat >= 20 ? 1 : 0);
    });

    // Accumulate over time
    if (!window._heatData) window._heatData = {};
    sessions.forEach((s, si) => {
        if (!window._heatData[s.id]) window._heatData[s.id] = [0, 0, 0, 0];
        const lat = s.current?.latencyMs || 0;
        if (lat < 5) window._heatData[s.id][0]++;
        else if (lat < 10) window._heatData[s.id][1]++;
        else if (lat < 20) window._heatData[s.id][2]++;
        else window._heatData[s.id][3]++;
    });

    // Clean stale
    const activeIds = new Set(sessions.map(s => s.id));
    Object.keys(window._heatData).forEach(id => { if (!activeIds.has(id)) delete window._heatData[id]; });

    const hLabels = Object.keys(window._heatData).map(id => id.substring(0, 12));
    const datasets = buckets.map((b, bi) => ({
        label: b,
        data: Object.values(window._heatData).map(d => d[bi]),
        backgroundColor: colors[bi],
        borderWidth: 0,
        borderRadius: 3,
    }));

    heatmapChart.data.labels = hLabels;
    heatmapChart.data.datasets = datasets;
    heatmapChart.update();
}

function updateSessionsTable(sessions) {
    const tbody = document.getElementById('sessionsBody');
    if (!sessions || sessions.length === 0) {
        tbody.innerHTML = '<tr><td colspan="6" style="text-align:center;color:var(--text-secondary);padding:20px">No active sessions</td></tr>';
        return;
    }
    tbody.innerHTML = sessions.map(s => `
        <tr>
            <td style="font-weight:600;color:var(--accent-blue)">${s.id.substring(0, 12)}</td>
            <td>${s.profileName || s.profileKey}</td>
            <td style="font-variant-numeric:tabular-nums">${s.ueIp}</td>
            <td style="font-weight:600;font-variant-numeric:tabular-nums">${(s.current?.throughputMbps || 0).toFixed(1)} Mbps</td>
            <td style="font-variant-numeric:tabular-nums">${(s.current?.latencyMs || 0).toFixed(1)} ms</td>
            <td><span class="status-dot active"></span></td>
        </tr>
    `).join('');
}

// ── Controls ───────────────────────────────────────────────────────────
function initControls() {
    document.getElementById('ueCountSlider').addEventListener('input', e => {
        document.getElementById('ueCountVal').textContent = e.target.value;
    });

    document.getElementById('btnStartSession').addEventListener('click', async () => {
        const profile = document.getElementById('profileSelect').value;
        const count = parseInt(document.getElementById('ueCountSlider').value);
        await fetch(API + '/api/sessions', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ profile, count })
        });
    });

    document.getElementById('btnStopAll').addEventListener('click', async () => {
        await fetch(API + '/api/sessions?id=all', { method: 'DELETE' });
        window._heatData = {};
        Object.keys(tpHistory).forEach(k => delete tpHistory[k]);
    });
}

// ── Main Loop ──────────────────────────────────────────────────────────
async function poll() {
    const [sessions, agg] = await Promise.all([fetchSessions(), fetchAggregated()]);
    updateStatCards(agg);
    updateThroughputChart(sessions || []);
    updateLatencyHeatmap(sessions || []);
    updateSessionsTable(sessions || []);

    // Fetch NWDAF analytics every 2s
    if (tickCount % 4 === 0) {
        const analytics = await fetchNWDAFAnalytics();
        updateMOS(analytics);
        updateAnalyticsPanel(analytics);
    }
}

async function init() {
    initCharts();
    initControls();
    updateClock();
    setInterval(updateClock, 1000);
    checkNWDAF();
    setInterval(checkNWDAF, 5000);
    setInterval(poll, POLL_MS);
    poll();
}

document.addEventListener('DOMContentLoaded', init);
