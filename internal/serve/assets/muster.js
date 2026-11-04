// muster.js — the agents page: one row per agent on the bus with its inbox
// counts, reloaded when serve says the rows changed.
import { bar, createApi, h, initTheme, list, live } from '/_kit/kit.js';

const app = document.getElementById('app');
const top = bar({ brand: { name: 'muster' }, staleText: 'restarted · reload' });
initTheme('muster', top.themeControl);
const api = createApi({ onStale: () => top.setLive('stale') });

const ago = (ms) => {
  if (!ms) return '';
  const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86400) return `${Math.round(s / 3600)}h ago`;
  return `${Math.round(s / 86400)}d ago`;
};

const rows = list({
  label: 'Agents',
  row: (a) => ({
    id: a.alias,
    key: a.project || '',
    title: a.alias + (a.label ? ` · ${a.label}` : ''),
    meta: [
      a.unread ? `${a.unread} unread` : '',
      a.action_required ? `${a.action_required} action required` : '',
    ].filter(Boolean).join(' · '),
    sub: [
      a.departed ? 'departed' : a.activity || '',
      // last_seen is unix ms.
      a.last_seen ? `seen ${ago(a.last_seen)}` : '',
    ].filter(Boolean).join(' · '),
  }),
});

app.append(top.el, h('main', { class: 'kit-read' }, rows.el));

async function load() {
  try {
    const { agents } = await api.get('/api/agents');
    rows.setItems(agents);
    const unread = agents.reduce((n, a) => n + (a.unread || 0), 0);
    top.setStatus(`${agents.length} agents · ${unread} unread`);
  } catch (err) {
    top.setStatus(String(err.message || err), { tone: 'danger' });
  }
}

live({
  events: '/api/events',
  poll: '/api/poll',
  onEvent: (e) => { if (e.type === 'agents') load(); },
  onStatus: (s) => top.setLive(s),
  onNewBuild: api.newBuild,
});
load();
