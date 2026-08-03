const keys = [
  {
    name: '小周',
    fingerprint: '••••6a93',
    allocation: '10x',
    confirmed: '4.6x / 10x',
    cycle: '27.61M',
    total: '54.20M',
    usage: '27.61M / 60M Token',
    allModels: 'codex-auto-review, gpt-5.3-codex-spark, gpt-5.4, gpt-5.4-mini, gpt-5.6-luna, gpt-5.6-terra, gpt-image-1.5, gpt-image-2',
    status: '管理中',
    percent: '46%',
    used: '27.61M',
    reserved: '8.74M',
    remaining: '23.65M / 60M',
    requests: '12,842',
    success: '98.7%',
    latency: '1.21s'
  },
  {
    name: 'VIP 专用',
    fingerprint: '••••b1e7',
    allocation: '10x',
    confirmed: '3.04x / 10x',
    cycle: '18.23M',
    total: '36.44M',
    usage: '18.23M / 60M Token',
    allModels: 'gpt-5.4-mini, gpt-5.6-luna, gpt-5.6-terra, gpt-5.6-sol, codex-auto-review',
    status: '管理中',
    percent: '31%',
    used: '18.23M',
    reserved: '4.18M',
    remaining: '37.59M / 60M',
    requests: '8,104',
    success: '99.1%',
    latency: '1.08s'
  },
  {
    name: '备用 Key',
    fingerprint: '••••a923',
    allocation: '1x',
    confirmed: '0.54x / 1x',
    cycle: '3.21M',
    total: '8.46M',
    usage: '3.21M / 6M Token',
    allModels: 'gpt-image-1.5, gpt-image-2',
    status: '空闲',
    percent: '54%',
    used: '3.21M',
    reserved: '0.28M',
    remaining: '2.51M / 6M',
    requests: '1,220',
    success: '96.4%',
    latency: '1.83s'
  }
];

keys.forEach(key => {
  const models = key.allModels.split(',').map(item => item.trim()).filter(Boolean);
  key.models = models.slice(0, 3).join(', ') + (models.length > 3 ? `，另有 ${models.length - 3} 个` : '');
});

const logs = [
  ['2026-07-28 09:10:21', '小周', '••••6a93', 'gpt-5.3-codex-spark', 'ALLOW', '4,293', '200', '实际 Token 结算 · 账号 ••••json'],
  ['2026-07-28 09:09:50', '小周', '••••6a93', 'gpt-5.4', 'ALLOW', '2,978', '200', '实际 Token 结算 · 账号 ••••json'],
  ['2026-07-28 09:09:34', 'VIP 专用', '••••b1e7', 'gpt-5.6-luna', 'ALLOW', '5,291', '200', '实际 Token 结算 · 账号 ••••json'],
  ['2026-07-28 09:09:11', '小周', '••••6a93', 'gpt-5.3-codex-spark', 'ALLOW', '2,184', '200', '实际 Token 结算 · 账号 ••••json'],
  ['2026-07-28 09:08:47', '备用 Key', '••••a923', 'gpt-image-1.5', 'ALLOW', '1,280', '200', '实际 Token 结算 · 账号 ••••json'],
  ['2026-07-28 09:08:23', 'VIP 专用', '••••b1e7', 'gpt-5.4-mini', 'THROTTLE', '7,622', '429', 'quota_allocation_exhausted'],
  ['2026-07-28 09:07:59', '小周', '••••6a93', 'gpt-5.4', 'ALLOW', '4,029', '200', '实际 Token 结算 · 账号 ••••json'],
  ['2026-07-28 09:07:17', '备用 Key', '••••a923', 'gpt-image-2', 'REJECT', '0', '403', 'model_not_allowed'],
  ['2026-07-28 09:06:47', 'VIP 专用', '••••b1e7', 'gpt-5.6-terra', 'ALLOW', '4,996', '200', '实际 Token 结算 · 账号 ••••json']
];

const trendPoints = [
  {label: '7/22', units: 3260000, requests: 1320},
  {label: '7/23', units: 5180000, requests: 1940},
  {label: '7/24', units: 6740000, requests: 2380},
  {label: '7/25', units: 6120000, requests: 2260},
  {label: '7/26', units: 4540000, requests: 1820},
  {label: '7/27', units: 3980000, requests: 1690},
  {label: '7/28', units: 7110000, requests: 2730}
];

let selected = 0;
const rows = document.querySelector('#key-rows');
const logRows = document.querySelector('#log-rows');

function renderKeys(filter = '') {
  rows.innerHTML = keys
    .filter(key => (key.name + key.fingerprint).toLowerCase().includes(filter.toLowerCase()))
    .map(key => {
      const index = keys.indexOf(key);
      return `<tr class="${index === selected ? 'selected' : ''}" data-index="${index}">
        <td><span class="radio"></span><strong>${key.name}</strong><small>${key.fingerprint}</small></td>
        <td><b>${key.allocation}</b></td>
        <td class="key-usage-cell"><strong>${key.confirmed}</strong></td>
        <td class="key-token-cell" title="当前官方周期实际 Token：${key.cycle}"><strong>${key.cycle}</strong></td>
        <td class="key-token-cell" title="累计实际 Token：${key.total}"><strong>${key.total}</strong></td>
        <td class="model-cell"><span class="model-truncate" title="${key.allModels}">${key.models}</span></td>
        <td><em class="status ${key.status === '空闲' ? 'idle' : ''}">${key.status}</em></td>
        <td><div class="row-actions">
          <button type="button" class="key-action" data-key-action="edit" title="编辑 ${key.name}"><iconify-icon icon="solar:pen-linear"></iconify-icon>编辑</button>
          <button type="button" class="key-action delete" data-key-action="delete" title="删除 ${key.name}"><iconify-icon icon="solar:trash-bin-trash-linear"></iconify-icon>删除</button>
        </div></td>
      </tr>`;
    }).join('');

  rows.querySelectorAll('tr').forEach(row => {
    row.addEventListener('click', () => selectKey(Number(row.dataset.index)));
  });
  rows.querySelectorAll('[data-key-action]').forEach(button => {
    button.addEventListener('click', event => {
      event.stopPropagation();
      button.classList.add('preview-pressed');
      setTimeout(() => button.classList.remove('preview-pressed'), 180);
    });
  });
}

function selectKey(index) {
  selected = index;
  const key = keys[index];
  document.querySelector('#analysis-name').textContent = `单 Key 用量分析 · ${key.name}`;
  document.querySelector('#usage-percent').textContent = key.percent;
  document.querySelector('#used-token').textContent = key.used;
  document.querySelector('#reserved-token').textContent = key.reserved;
  document.querySelector('#remaining-token').textContent = key.remaining;
  document.querySelector('#request-count').textContent = key.requests;
  document.querySelector('#success-rate').textContent = key.success;
  document.querySelector('#latency').textContent = key.latency;
  document.querySelector('.usage-ring').style.setProperty('--usage', key.percent);
  renderKeys(document.querySelector('#key-search').value);
}

function renderLogs() {
  logRows.innerHTML = logs.map(row => `<tr>${row.map((cell, index) => {
    if (index === 4) return `<td><span class="decision ${cell.toLowerCase()}">${cell}</span></td>`;
    if (index === 6) return `<td class="http ${cell === '200' ? 'ok' : 'bad'}">${cell}</td>`;
    return `<td title="${cell}">${cell}</td>`;
  }).join('')}</tr>`).join('');
}

function formatTokens(value) {
  if (value >= 1000000) return `${(value / 1000000).toFixed(value >= 10000000 ? 1 : 2)}M`;
  if (value >= 1000) return `${(value / 1000).toFixed(1)}K`;
  return String(value);
}

function renderLineChart() {
  const canvas = document.querySelector('#preview-line-chart');
  const tooltip = document.querySelector('#preview-chart-tooltip');
  if (!canvas || !tooltip) return;

  const parent = canvas.parentElement;
  const width = Math.max(260, parent.clientWidth);
  const height = Math.max(92, parent.clientHeight);
  const ratio = Math.min(window.devicePixelRatio || 1, 2);
  canvas.width = Math.round(width * ratio);
  canvas.height = Math.round(height * ratio);
  canvas.style.width = `${width}px`;
  canvas.style.height = `${height}px`;

  const context = canvas.getContext('2d');
  context.setTransform(ratio, 0, 0, ratio, 0, 0);
  const padding = {left: 38, right: 10, top: 10, bottom: 22};
  const plotWidth = width - padding.left - padding.right;
  const plotHeight = height - padding.top - padding.bottom;
  const max = Math.max(...trendPoints.map(point => point.units), 1) * 1.12;

  context.font = '10px Inter, "Microsoft YaHei", sans-serif';
  context.textAlign = 'right';
  context.textBaseline = 'middle';
  for (let index = 0; index < 4; index += 1) {
    const value = max * (3 - index) / 3;
    const y = padding.top + plotHeight * index / 3;
    context.strokeStyle = '#e5edf6';
    context.lineWidth = 1;
    context.beginPath();
    context.moveTo(padding.left, y);
    context.lineTo(width - padding.right, y);
    context.stroke();
    context.fillStyle = '#8a9bb1';
    context.fillText(formatTokens(value), padding.left - 7, y);
  }

  const positions = trendPoints.map((point, index) => ({
    x: padding.left + (trendPoints.length === 1 ? plotWidth / 2 : plotWidth * index / (trendPoints.length - 1)),
    y: padding.top + plotHeight - (point.units / max) * plotHeight,
    point
  }));

  const gradient = context.createLinearGradient(0, padding.top, 0, padding.top + plotHeight);
  gradient.addColorStop(0, 'rgba(22,138,103,.24)');
  gradient.addColorStop(1, 'rgba(22,138,103,0)');
  context.beginPath();
  context.moveTo(positions[0].x, padding.top + plotHeight);
  positions.forEach(position => context.lineTo(position.x, position.y));
  context.lineTo(positions[positions.length - 1].x, padding.top + plotHeight);
  context.closePath();
  context.fillStyle = gradient;
  context.fill();

  context.beginPath();
  positions.forEach((position, index) => index ? context.lineTo(position.x, position.y) : context.moveTo(position.x, position.y));
  context.strokeStyle = '#168a67';
  context.lineWidth = 2.5;
  context.lineJoin = 'round';
  context.lineCap = 'round';
  context.stroke();
  positions.forEach(position => {
    context.beginPath();
    context.arc(position.x, position.y, 3.2, 0, Math.PI * 2);
    context.fillStyle = '#fff';
    context.fill();
    context.strokeStyle = '#168a67';
    context.lineWidth = 2;
    context.stroke();
  });

  context.fillStyle = '#8495aa';
  context.textAlign = 'center';
  context.textBaseline = 'top';
  positions.forEach(position => context.fillText(position.point.label, position.x, height - padding.bottom + 7));

  canvas.onmousemove = event => {
    const rectangle = canvas.getBoundingClientRect();
    const mouseX = event.clientX - rectangle.left;
    const nearest = positions.reduce((best, item) => Math.abs(item.x - mouseX) < Math.abs(best.x - mouseX) ? item : best, positions[0]);
    tooltip.innerHTML = `<strong>${nearest.point.label}</strong><span>${formatTokens(nearest.point.units)} Token</span><span>${nearest.point.requests.toLocaleString()} 次请求</span>`;
    tooltip.hidden = false;
    tooltip.style.left = `${Math.min(Math.max(nearest.x, 68), width - 68)}px`;
    tooltip.style.top = `${Math.max(nearest.y - 8, 10)}px`;
  };
  canvas.onmouseleave = () => {
    tooltip.hidden = true;
  };
}

document.querySelector('#key-search').addEventListener('input', event => renderKeys(event.target.value));
document.querySelectorAll('.tab').forEach(tab => {
  tab.addEventListener('click', () => {
    document.querySelectorAll('.tab').forEach(item => item.classList.toggle('active', item === tab));
  });
});

let resizeTimer = 0;
window.addEventListener('resize', () => {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(renderLineChart, 120);
});

renderKeys();
renderLogs();
selectKey(0);
renderLineChart();
