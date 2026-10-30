// app.js — 看板前端（无依赖，纯 DOM + SVG）
// 数据全部来自 /api/ui/*（后端再去问上游），前端不做任何积分推算。
const tokenParam = new URLSearchParams(location.search).get('token') || '';
const qs = tokenParam ? `?token=${encodeURIComponent(tokenParam)}` : '';

const $ = (id) => document.getElementById(id);
const fmt = (n) => (n === null || n === undefined || Number.isNaN(n)) ? '—' : Number(n).toLocaleString('zh-CN');
const days = (iso) => {
  if (!iso) return null;
  const ms = new Date(iso).getTime() - Date.now();
  return Math.round(ms / 3600000); // 小时
};

async function api(path, opts) {
  const url = path.includes('?') ? path + '&' + qs.slice(1) : path + qs;
  const o = Object.assign({ headers: { 'Content-Type': 'application/json' } }, opts || {});
  if (o.body && typeof o.body !== 'string') o.body = JSON.stringify(o.body);
  const r = await fetch(url, o);
  if (!r.ok) throw new Error((await r.text()).slice(0, 200) || r.status);
  return r.json();
}

function badge(text, kind) {
  return `<span class="badge ${kind || 'mute'}">${text}</span>`;
}

// ---------- 总览 ----------
async function loadOverview() {
  const ov = await api('/api/ui/overview');
  const t = ov.totals;

  $('kpiCredits').textContent = fmt(t.credits);
  $('kpiCreditsFoot').textContent = t.accounts ? `平均每号 ${fmt(Math.round(t.credits / t.accounts))}` : '还没有账号';

  $('kpiAccounts').textContent = fmt(t.accounts);
  $('kpiAccountsFoot').textContent = `${t.referral_auto_tiers} 档邀请奖励已到账`;

  $('kpiClaimed').textContent = `${t.claimed_today}/${t.accounts}`;
  $('kpiClaimedFoot').textContent = t.claimed_today === t.accounts && t.accounts ? '全部签完了' : '还有号没签';

  if (t.nearest_plan_expiry) {
    const h = t.nearest_plan_hours;
    $('kpiExpiry').textContent = h > 24 ? `${Math.floor(h / 24)} 天` : `${Math.round(h)} 小时`;
    $('kpiExpiry').innerHTML += `<small>${new Date(t.nearest_plan_expiry).toLocaleDateString('zh-CN')}</small>`;
    $('kpiExpiryFoot').textContent = h < 72 ? '⚠ 快到期，优先消耗这个号' : '整包积分随套餐到期作废';
  }

  renderJobs(ov.jobs || []);
  renderInvite(ov.invite);

  const up = ov.server.uptime_seconds;
  $('subline').textContent = `上游 ${ov.server.base} · ${ov.server.accounts} 个号 · 粘性会话 ${ov.server.sticky_sessions} 条 · 已运行 ${up < 60 ? up + ' 秒' : Math.floor(up / 60) + ' 分钟'}`;

  const rows = ov.accounts.map((a) => {
    const ob = a.onboarding;
    const obPct = ob.total ? Math.round((ob.done / ob.total) * 100) : 0;
    const claimKind = a.daily.status === 'granted_today' || a.daily.status === 'already_granted' ? 'ok' : 'warn';
    const claimText = a.daily.status === 'granted_today' ? `已签 +${a.daily.points}`
      : a.daily.status === 'already_granted' ? '已签' : (a.daily.status || '未知');
    const left = a.token_left_seconds;
    const tokKind = left > 600 ? 'ok' : left > 0 ? 'warn' : 'bad';
    const tokText = left > 0 ? `${Math.floor(left / 60)} 分钟` : '已过期';
    const planLeft = a.plan_left_hours;
    const planKind = planLeft == null ? 'mute' : planLeft < 72 ? 'bad' : planLeft < 24 * 14 ? 'warn' : 'ok';
    return `<tr>
      <td><span class="who">${a.label_full || a.uid}</span><div class="mono uid">${a.uid}</div></td>
      <td class="num"><b>${fmt(a.credits)}</b></td>
      <td>${badge(a.plan_name || '—', planKind)}<div class="muted">${a.plan_expires_at ? new Date(a.plan_expires_at).toLocaleDateString('zh-CN') : ''}</div></td>
      <td>${badge(claimText, claimKind)}<div class="muted">连签 ${a.daily.streak_day || 0} 天</div></td>
      <td><span class="progress"><i style="width:${obPct}%"></i></span><span class="mono">${ob.done}/${ob.total}</span><div class="muted">+${fmt(ob.earned)} / ${fmt(ob.max)} 分</div></td>
      <td>${badge(a.referral.auto_tiers_done + '/2 档', a.referral.auto_tiers_done >= 2 ? 'mute' : 'info')}<div class="muted">下一档 +${fmt(a.referral.next_reward_points)}</div></td>
      <td class="num mono">${a.weight.toFixed(1)}</td>
      <td class="num">${badge(tokText, tokKind)}</td>
      <td>${a.cooling_seconds ? badge(`冷却 ${a.cooling_seconds}s`, 'warn') : (a.error ? (String(a.error).includes('desktop_installation_required') ? badge('待登记装机', 'warn') : badge('摘要失败', 'bad')) : badge('可用', 'ok'))}
        ${a.last_used_ago_seconds ? `<div class="muted">${Math.floor(a.last_used_ago_seconds / 60)} 分钟前用过</div>` : '<div class="muted">从未使用</div>'}</td>
    </tr>`;
  }).join('');
  $('acctBody').innerHTML = rows || '<tr><td colspan="9" class="muted">auths/ 下没有账号，点右上角「登录新账号」</td></tr>';

  const claimBtn = $('btnClaim');
  if (t.accounts > 0 && t.claimed_today === t.accounts) {
    claimBtn.disabled = true;
    claimBtn.textContent = '今日已签到';
  } else {
    claimBtn.disabled = false;
    claimBtn.textContent = '立即签到';
  }

  $('connDot').className = 'dot';
  $('connText').textContent = '在线';
}

// ---------- 用量 ----------
let chartDays = 14;

function niceMax(v) {
  if (!(v > 0)) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  const n = v / p;
  const step = n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10;
  return step * p;
}

function drawChart(series) {
  const svg = $('chart');
  const W = 1000, H = 220, pad = { l: 46, r: 46, t: 14, b: 26 };
  const innerW = W - pad.l - pad.r, innerH = H - pad.t - pad.b;
  if (!series.length) {
    svg.innerHTML = `<text x="500" y="110" text-anchor="middle" fill="#8a9099" font-size="14">暂无用量</text>`;
    return;
  }
  // 刻度取整：0/1/2/5×10^n，柱子不会顶到边框
  const maxP = niceMax(Math.max(1, ...series.map((d) => d.points)) * 1.05);
  const maxR = niceMax(Math.max(1, ...series.map((d) => d.requests)) * 1.05);
  const n = series.length;
  const step = innerW / n;
  const bw = Math.min(44, step * 0.5);

  let grid = '';
  for (let i = 0; i <= 4; i++) {
    const y = pad.t + (innerH * i) / 4;
    const pv = Math.round((maxP * (4 - i)) / 4);
    const rv = Math.round((maxR * (4 - i)) / 4);
    grid += `<line x1="${pad.l}" y1="${y.toFixed(1)}" x2="${W - pad.r}" y2="${y.toFixed(1)}" stroke="${i === 4 ? '#e1e5ea' : '#eef1f4'}" stroke-width="1"/>`;
    grid += `<text x="${pad.l - 6}" y="${(y + 3.5).toFixed(1)}" text-anchor="end" fill="#a3a9b1" font-size="10">${pv}</text>`;
    grid += `<text x="${W - pad.r + 6}" y="${(y + 3.5).toFixed(1)}" fill="#a3a9b1" font-size="10">${rv}</text>`;
  }
  grid += `<text x="${pad.l - 6}" y="10" text-anchor="end" fill="#8a9099" font-size="9">积分</text>`;
  grid += `<text x="${W - pad.r + 6}" y="10" fill="#8a9099" font-size="9">次</text>`;

  let bars = '', dots = '', labels = '';
  let prev = null;
  series.forEach((d, i) => {
    const x = pad.l + step * i + step / 2;
    const h = (d.points / maxP) * innerH;
    if (d.points > 0) {
      bars += `<rect x="${x - bw / 2}" y="${pad.t + innerH - h}" width="${bw}" height="${Math.max(h, 2)}" rx="5" fill="url(#g)"><title>${d.date}｜${d.points} 分｜${d.requests} 次｜${d.tokens} tokens</title></rect>`;
    } else {
      bars += `<rect x="${x - bw / 2}" y="${pad.t + innerH - 3}" width="${bw}" height="3" rx="1.5" fill="#e8ecf0"><title>${d.date}｜无用量</title></rect>`;
    }
    const y = pad.t + innerH - (d.requests / maxR) * innerH;
    // 只在有请求的日子画点：全为 0 的日子连线会在视觉上造出假的"趋势"
    if (d.requests > 0) {
      dots += `<circle cx="${x.toFixed(1)}" cy="${y.toFixed(1)}" r="3" fill="#27bfae"/>`;
      prev = { x, y };
    }
    labels += `<text x="${x}" y="${H - 8}" text-anchor="middle" fill="#8a9099" font-size="10">${d.date.slice(5)}</text>`;
  });

  svg.innerHTML = `<defs><linearGradient id="g" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0%" stop-color="#2f7bff"/><stop offset="100%" stop-color="#8fb9ff"/></linearGradient></defs>
    ${grid}
    ${bars}
    ${dots}`;
}

function agoText(ts) {
  if (!ts) return '从未';
  const d = new Date(ts);
  const sec = Math.max(0, (Date.now() - d.getTime()) / 1000);
  if (sec < 60) return `${Math.round(sec)} 秒前`;
  if (sec < 3600) return `${Math.floor(sec / 60)} 分钟前`;
  if (sec < 86400) return `${Math.floor(sec / 3600)} 小时前`;
  return `${Math.floor(sec / 86400)} 天前`;
}

function clockText(ts) {
  if (!ts) return '—';
  const d = new Date(ts);
  const today = new Date();
  const sameDay = d.toDateString() === today.toDateString();
  const hm = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
  return sameDay ? hm : `${d.getMonth() + 1}/${d.getDate()} ${hm}`;
}

let jobsCache = [];

// ---------- 邀请入口 ----------
// 上游只在「新手机号注册表单提交」那一刻建立邀请关系（/r/<code> 会预填 inviteCode），
// 账号建好之后 POST /referrals/bind 永远 409 existing_user_ineligible。
// 所以这里给的是注册链接 + 复制，而不是「补填」按钮。
function renderInvite(inv) {
  const box = $('inviteBody'), foot = $('inviteFoot');
  if (!box) return;
  if (!inv || !inv.share_url) {
    box.innerHTML = '<div class="muted">还没有可用邀请人：前两档已用满，或所有号都不可邀请。点下面按钮重挑一次。</div>';
    foot.textContent = '';
    return;
  }
  box.innerHTML = `
    <div class="invite-grid">
      <div><div class="k">邀请人</div><div class="v">${inv.phone || inv.inviter}</div>
        <div class="mono uid">${inv.inviter}</div></div>
      <div><div class="k">已用档位</div><div class="v">${inv.tier}/2</div>
        <div class="muted small">下一档 +${fmt(inv.next_reward)}</div></div>
      <div><div class="k">邀请码</div><div class="v mono">${inv.invite_code || '—'}</div>
        <div class="muted small">注册页「邀请码」一栏</div></div>
    </div>
    <div class="linkbox">
      <input id="inviteUrl" readonly value="${inv.share_url}" spellcheck="false">
      <button class="btn primary" id="btnCopyInvite">复制链接</button>
    </div>
    <div class="muted small" style="margin-top:10px">${inv.note}</div>`;
  foot.textContent = inv.at ? `链接生成于 ${new Date(inv.at).toLocaleString('zh-CN')}` : '';
  const btn = $('btnCopyInvite');
  if (btn) btn.onclick = async () => {
    const t = $('inviteUrl');
    t.select();
    try { await navigator.clipboard.writeText(t.value); btn.textContent = '已复制'; }
    catch (e) { btn.textContent = '请手动复制'; }
    setTimeout(() => { btn.textContent = '复制链接'; }, 1600);
  };
}

function renderJobs(jobs) {
  jobsCache = jobs;
  const body = $('jobBody');
  if (!jobs.length) {
    body.innerHTML = '<tr><td colspan="7" class="muted">config.schedule.jobs 为空，网关没有定时任务</td></tr>';
    return;
  }
  const names = { claim: '签到', verify: '状态回读', sweep: '补做（邀请 → onboarding）', bind: '邀请补填', onboard: 'onboarding 补做' };
  body.innerHTML = jobs.map((j) => {
    const kind = j.running ? 'run' : (j.last_rc === 0 ? 'ok' : j.last_rc > 0 ? 'fail' : 'idle');
    const result = j.running ? '运行中…'
      : j.last_rc === 0 ? '成功'
        : j.last_rc > 0 ? `失败 rc=${j.last_rc}`
          : (j.enabled ? '待首次运行' : '已禁用');
    return `<tr>
      <td><span class="job-dot ${kind}"></span><b>${names[j.name] || j.name}</b>
        <div class="mono uid">${j.cmd}</div></td>
      <td class="mono">${j.schedule.join(' · ')}</td>
      <td class="mono">${j.enabled ? clockText(j.next_run) : '—'}</td>
      <td>${agoText(j.last_run)}<div class="muted mono">${j.last_run ? clockText(j.last_run) : ''}</div></td>
      <td class="num mono">${j.last_duration_seconds ? j.last_duration_seconds + 's' : '—'}</td>
      <td>${badge(result, j.running ? 'info' : j.last_rc === 0 ? 'ok' : j.last_rc > 0 ? 'bad' : 'mute')}</td>
      <td class="num"><button class="btn ghost mini" data-job="${j.name}" ${j.running ? 'disabled' : ''}>立即运行</button></td>
    </tr>`;
  }).join('');

  body.querySelectorAll('button[data-job]').forEach((b) => {
    b.addEventListener('click', async () => {
      const name = b.dataset.job;
      b.disabled = true;
      b.textContent = '已提交';
      try {
        await api('/api/ui/run', { method: 'POST', body: { job: name } });
        setTimeout(() => loadOverview().catch(() => {}), 1200);
      } catch (e) {
        b.textContent = '失败';
        showJobTail('触发失败：' + e.message);
      }
    });
  });

  // 点任务名看上次输出尾巴
  body.querySelectorAll('tr').forEach((tr, i) => {
    tr.style.cursor = jobs[i] && jobs[i].last_tail ? 'pointer' : 'default';
    tr.addEventListener('click', (ev) => {
      if (ev.target.closest('button')) return;
      const j = jobs[i];
      if (j && j.last_tail) showJobTail(j.last_tail);
    });
  });
}

function showJobTail(text) {
  const el = $('jobTail');
  el.style.display = 'block';
  el.textContent = text;
}

async function loadUsage() {
  const u = await api(`/api/ui/usage?days=${chartDays}`);
  drawChart(u.series);
  $('usageTotals').textContent = `${chartDays} 天合计：${fmt(u.totals.points)} 分 · ${fmt(u.totals.requests)} 次请求 · ${fmt(u.totals.tokens)} tokens`;
  const max = Math.max(1, ...u.by_model.map((m) => m.points));
  $('bars').innerHTML = u.by_model.length ? u.by_model.map((m) => `
    <div class="bar-row">
      <div class="name" title="${m.model}">${m.model}</div>
      <div class="bar-track"><div class="bar-fill" style="width:${(m.points / max * 100).toFixed(1)}%"></div></div>
      <div class="num">${fmt(m.points)} 分 · ${(m.share * 100).toFixed(1)}%</div>
    </div>`).join('') : '<div class="muted">这段时间没有调用记录</div>';
}

// ---------- 动作 ----------
async function doClaim() {
  const btn = $('btnClaim');
  btn.disabled = true; btn.textContent = '签到中…';
  try {
    const r = await api('/api/ui/claim', { method: 'POST' });
    const ok = r.rows.filter((x) => !x.error && x.points > 0);
    const sum = ok.reduce((s, x) => s + x.points, 0);
    btn.textContent = sum ? `+${sum} 分` : '今日已领';
    await Promise.all([loadOverview(), loadUsage()]);
  } catch (e) {
    btn.textContent = '签到失败';
    console.error(e);
  } finally {
    setTimeout(() => { btn.disabled = false; btn.textContent = '立即签到'; }, 2500);
  }
}

async function doKeepalive() {
  const btn = $('btnKeepalive');
  btn.disabled = true; btn.textContent = '续期中…';
  try {
    const r = await api('/api/ui/refresh', { method: 'POST' });
    const n = r.rows.filter((x) => x.refreshed).length;
    btn.textContent = `续了 ${n} 个号`;
    await loadOverview();
  } catch (e) {
    btn.textContent = '续期失败';
  } finally {
    setTimeout(() => { btn.disabled = false; btn.textContent = '续期全部 token'; }, 2500);
  }
}

// ---------- 初始化 ----------
$('segDays').addEventListener('click', (e) => {
  const b = e.target.closest('button');
  if (!b) return;
  document.querySelectorAll('#segDays button').forEach((x) => x.classList.toggle('on', x === b));
  chartDays = Number(b.dataset.days);
  loadUsage().catch(console.error);
});
$('btnRefresh').addEventListener('click', () => {
  loadOverview().catch(console.error);
  loadUsage().catch(console.error);
});
$('btnClaim').addEventListener('click', doClaim);
const btnInviteNew = $('btnInviteNew');
if (btnInviteNew) btnInviteNew.addEventListener('click', async () => {
  const b = btnInviteNew;
  b.disabled = true; b.textContent = '正在挑选…';
  try {
    await api('/api/ui/run', { method: 'POST', body: { job: 'invite' } });
    // invite 要打上游 referrals/code，给它几秒
    await new Promise((r) => setTimeout(r, 4000));
    await loadOverview();
  } catch (e) {
    $('inviteFoot').textContent = String(e.message || e);
  } finally {
    b.disabled = false; b.textContent = '重新挑邀请人';
  }
});
$('btnKeepalive').addEventListener('click', doKeepalive);

loadOverview().catch((e) => {
  $('connDot').className = 'dot bad';
  $('connText').textContent = '连接失败';
  $('subline').textContent = String(e.message || e);
});
loadUsage().catch(console.error);
setInterval(() => loadOverview().catch(() => {}), 60000);
