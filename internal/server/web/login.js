// login.js — 可视化登录：PKCE 两步（生成授权链接 → 回填授权码 → 落盘）
const tokenParam = new URLSearchParams(location.search).get('token') || '';
const qs = tokenParam ? `?token=${encodeURIComponent(tokenParam)}` : '';
const $ = (id) => document.getElementById(id);

let loginId = '';

function showErr(msg) {
  const el = $('err');
  el.textContent = msg;
  el.style.display = 'block';
}
function clearErr() { $('err').style.display = 'none'; }

async function post(path, body) {
  const r = await fetch(path + qs, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {}),
  });
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error((data.error && (data.error.message || data.error.code)) || `HTTP ${r.status}`);
  return data;
}

$('btnStart').addEventListener('click', async () => {
  clearErr();
  const btn = $('btnStart');
  btn.disabled = true;
  btn.textContent = '生成中…';
  try {
    const d = await post('/api/ui/login/start');
    loginId = d.id;
    $('authUrl').textContent = d.url;
    $('btnCopy').disabled = false;
    $('step2').classList.add('on');
    window.open(d.url, '_blank', 'noopener');
    btn.textContent = '重新生成';
  } catch (e) {
    showErr('生成授权链接失败：' + e.message);
    btn.textContent = '生成并打开授权页';
  } finally {
    btn.disabled = false;
  }
});

$('btnCopy').addEventListener('click', async () => {
  const text = $('authUrl').textContent;
  try {
    await navigator.clipboard.writeText(text);
    $('btnCopy').textContent = '已复制';
    setTimeout(() => ($('btnCopy').textContent = '复制链接'), 1600);
  } catch {
    showErr('浏览器不让你写剪贴板，手动选中复制吧');
  }
});

$('btnExchange').addEventListener('click', async () => {
  clearErr();
  const code = $('code').value.trim();
  if (!loginId) return showErr('先点上面的「生成并打开授权页」，否则服务端没有这次登录的 PKCE verifier');
  if (!code) return showErr('把授权码粘进来');

  const btn = $('btnExchange');
  btn.disabled = true;
  btn.textContent = '交换 token…';
  try {
    const d = await post('/api/ui/login/exchange', { id: loginId, code });
    $('okBox').style.display = 'block';
    $('okBox').innerHTML = `登录成功：<b>${d.label || d.uid}</b>` +
      (d.credits !== undefined ? ` · 积分 <b>${Number(d.credits).toLocaleString('zh-CN')}</b>` : '') +
      (d.onboarding ? ` · onboarding ${d.onboarding.done}/${d.onboarding.total}` : '') +
      `<br><span class="muted">凭证已写入 ${d.file}${d.merged > 0 ? `（合并了 ${d.merged} 个同号重复凭证，旧文件在 auths/.trash/）` : ''}</span>`;
    setTimeout(() => { location.href = './index.html' + qs; }, 1800);
  } catch (e) {
    showErr('登录失败：' + e.message + (String(e.message).includes('invalid_grant') ? '（授权码只能用一次，且几分钟内有效，重新生成链接再走一遍）' : ''));
    btn.disabled = false;
    btn.textContent = '完成登录';
  }
});
