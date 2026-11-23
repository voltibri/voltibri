// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only
//
// Shared by the status page and the configuration page: helpers, icons and
// the device setup dialog (form generated from the driver schema).

const $ = id => document.getElementById(id);
const esc = s => String(s).replace(/[&<>"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));
const ICONS = {
  solar:   '<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M2 12h2M20 12h2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>',
  balcony: '<path d="M4 14h16M6 14v6M18 14v6M4 20h16"/><path d="M7 4h10l2 7H5z"/><path d="M12 4v7M6 7.5h12"/>',
  car:     '<path d="M2.5 16.5v-3.8l2-.7 2.3-3.8h5.8l3 3.8 1.4.5v4"/><path d="M8 16.5h4.2"/><circle cx="6" cy="16.5" r="1.8"/><circle cx="14.2" cy="16.5" r="1.8"/><path d="M17 14c2.4 0 3.5-1.2 3.5-3.4V8.5"/><path d="M18.8 5.5h3.4v3h-3.4z"/><path d="M19.7 5.5V3.5M21.3 5.5V3.5"/>',
  heatpump:'<rect x="3" y="5" width="18" height="14" rx="2"/><circle cx="10" cy="12" r="4"/><path d="M10 8v8M6 12h8"/><path d="M17 9v6"/>',
  plug:    '<path d="M9 3v5M15 3v5"/><path d="M7 8h10v3a5 5 0 0 1-10 0z"/><path d="M12 16v5"/>',
  water:   '<path d="M12 3s6 6.5 6 11a6 6 0 0 1-12 0c0-4.5 6-11 6-11z"/>',
  house:   '<path d="M3 11 12 4l9 7"/><path d="M5 9.5V20h14V9.5"/><path d="M10 20v-5h4v5"/>',
  grid:    '<path d="M12 2 7 22M12 2l5 20M8.5 9h7M7 14h10M4 22h16M5 6h14"/>',
  battery: '<rect x="3" y="7" width="16" height="10" rx="2"/><path d="M21 10.5v3"/>',
};
const ICON_LABEL = {solar: 'PV', balcony: 'Balkonkraftwerk', car: 'Auto / Wallbox', heatpump: 'Wärmepumpe / Klima', plug: 'Steckdose',
  water: 'Warmwasser', house: 'Haus', grid: 'Netz', battery: 'Batterie'};
const iconSVG = (name, size = 20) => `<svg width="${size}" height="${size}" viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">${ICONS[name] || ICONS.plug}</svg>`;
const MODE_TEXT = {owned: 'von voltibri gesteuert', delegated: 'fremdgesteuert', observed: 'nur beobachtet'};
const CONTROL_CAPS = ['battery-control', 'charger-control', 'switch'];
const POWER_CAPS = ['grid', 'pv', 'charger', 'consumer'];
const shortName = d => d.name.replace(/ \(.*\)$/, '');
// [capability, roles key, label]
const ROLE_DEF = [['grid', 'grid', 'Netzzähler (Bezug und Einspeisung)'], ['pv', 'pv', 'PV-Erzeugung'], ['battery', 'battery', 'Hausbatterie'],
  ['pv-forecast', 'pvForecast', 'PV-Prognose'], ['tariff', 'tariff', 'Strompreis']];

const voltibri = (() => {
  let drivers = [];
  const ready = fetch('api/drivers').then(r => r.json()).then(d => { drivers = d; return d; }).catch(() => []);
  let dlg, st = {};

  function ensureDialog() {
    if (dlg) return;
    document.body.insertAdjacentHTML('beforeend', `<dialog class="setup" id="setup" aria-labelledby="setupTitle">
      <form class="inner" id="setupForm" novalidate>
        <h2 id="setupTitle"></h2><div id="setupBody"></div>
        <div class="err" id="setupErr" role="alert"></div><div class="actions" id="setupActions"></div>
      </form></dialog>`);
    dlg = $('setup');
    $('setupForm').addEventListener('submit', e => { e.preventDefault(); save(); });
    dlg.addEventListener('click', e => {
      if (e.target === dlg || e.target.closest('[data-close]')) dlg.close();
      const c = e.target.closest('[data-copy]');
      if (c) navigator.clipboard.writeText($('setupRedirect').textContent).then(() => { c.textContent = 'Kopiert'; }, () => {});
      const ch = e.target.closest('[data-choose]');
      if (ch) form(ch.dataset.choose);
      if (e.target.closest('[data-remove]')) remove();
    });
    dlg.addEventListener('close', () => { if (st.changed && st.onDone) st.onDone(); });
  }
  const buttons = html => { $('setupActions').innerHTML = html; };
  const body = html => { $('setupBody').innerHTML = html; };
  const err = msg => { $('setupErr').textContent = msg || ''; };

  function field(p, val, isNew) {
    const id = 'sp_' + p.name;
    const desc = p.description ? `<span class="why">${esc(p.description)}</span>` : '';
    const label = esc(p.label || p.name) + (p.unit ? ` (${esc(p.unit)})` : '');
    if (p.type === 'bool') {
      const on = val != null ? val === true : p.default === true;
      return `<label class="f cb"><input type="checkbox" id="${id}" data-param="${esc(p.name)}"${on ? ' checked' : ''}> ${label}</label>${desc}`;
    }
    const kept = p.type === 'secret' && !isNew && val;
    let input;
    if (p.type === 'enum') {
      input = `<select id="${id}" data-param="${esc(p.name)}"${p.required ? ' required' : ''}>${p.required ? '' : '<option value="">–</option>'}${(p.enum || []).map(o =>
        `<option${o === (val ?? p.default) ? ' selected' : ''}>${esc(o)}</option>`).join('')}</select>`;
    } else {
      const type = p.type === 'secret' ? 'password' : p.type === 'int' || p.type === 'float' ? 'number' : 'text';
      const step = p.type === 'float' ? ' step="any"' : p.type === 'int' ? ' step="1"' : '';
      const v = p.type === 'secret' ? '' : val ?? '';
      const ph = kept ? 'gespeichert – leer lassen, um es zu behalten' : p.default != null ? 'Standard: ' + p.default : '';
      const ac = p.type === 'secret' ? ' autocomplete="new-password"' : ' autocomplete="off"';
      input = `<input type="${type}" id="${id}" data-param="${esc(p.name)}" value="${esc(String(v))}" placeholder="${esc(ph)}"${step}${ac}${p.required && !kept ? ' required' : ''}>`;
    }
    return `<label class="f" for="${id}">${label}${p.required ? ' *' : ''}${input}${desc}</label>`;
  }

  // open: {type, id} edits or adds one device; {choose: [types], title}
  // first lets the user pick a driver. defaults: {icon, mode, name}.
  async function open(opts) {
    await ready;
    ensureDialog();
    st = {...opts, changed: false};
    err('');
    const cfg = await fetch('api/config').then(r => r.json()).catch(() => null);
    st.cfg = (cfg && cfg.config) || {devices: [], roles: {}};
    st.ids = new Set((st.cfg.devices || []).map(d => d.id));
    st.readOnly = cfg && cfg.readOnly;
    if (opts.type || opts.id) return form(opts.type, opts.id);
    $('setupTitle').textContent = opts.title || 'Gerät hinzufügen';
    const list = drivers.filter(d => opts.choose.includes(d.type));
    body(`<div class="choose">${list.map(d => `<button type="button" class="pick" data-choose="${esc(d.type)}">
      <b>${esc(d.name)}</b><span class="why">${esc(d.description || '')}</span></button>`).join('')}</div>`);
    buttons('<button type="button" class="btn" data-close>Abbrechen</button>');
    if (!dlg.open) dlg.showModal();
  }

  async function form(type, id) {
    st.isNew = !id;
    if (id) {
      const r = await fetch('api/devices/' + encodeURIComponent(id));
      if (!r.ok) { err('Gerät nicht gefunden'); return; }
      st.dev = await r.json();
      type = st.dev.type;
    }
    st.drv = drivers.find(d => d.type === type);
    if (!st.drv) { err('Unbekannter Gerätetyp: ' + type); return; }
    const d = st.drv, df = st.defaults || {};
    if (!id) {
      st.dev = {id: type, type, name: df.name || shortName(d), params: {}};
      if (df.icon) st.dev.icon = df.icon;
      let n = 2;
      while (st.ids.has(st.dev.id)) st.dev.id = type + '-' + n++;
    }
    const pv = st.dev.params || {}, caps = d.capabilities || [];
    const params = (d.params || []).filter(p => p.type !== 'list');
    const visible = params.filter(p => !p.advanced && p.name !== 'redirectUri');
    const adv = params.filter(p => p.advanced || p.name === 'redirectUri');
    const lists = (d.params || []).filter(p => p.type === 'list').map(p => p.label || p.name);
    const oauth = caps.includes('oauth');
    const redirect = pv.redirectUri || location.origin + '/api/oauth/callback';
    const steps = [];
    if (d.setupUrl) steps.push(`Zugang anlegen: <a href="${esc(d.setupUrl)}" target="_blank" rel="noopener">${esc(d.setupUrl.replace(/^https:\/\//, ''))}</a>`);
    if (oauth) steps.push(`Dort als Redirect-URI eintragen:<div class="copy"><code id="setupRedirect">${esc(redirect)}</code><button type="button" class="btn" data-copy>Kopieren</button></div>`);
    if (steps.length) steps.push('Zugangsdaten unten eintragen und speichern. Geheimnisse werden verschlüsselt gespeichert und nie wieder angezeigt.');
    if (oauth) steps.push('Danach einmal anmelden.');
    const mode = st.dev.mode || 'observed';
    const ctl = caps.some(c => CONTROL_CAPS.includes(c));
    const pow = caps.some(c => POWER_CAPS.includes(c));
    const cor = st.dev.correction || {};
    const icon = st.dev.icon || '';
    const roles = st.cfg.roles || {};
    const devName = id => ((st.cfg.devices || []).find(x => x.id === id) || {}).name || id;
    const inRole = key => key === 'pv' ? (roles.pv || []).includes(st.dev.id) : roles[key] === st.dev.id;
    const roleOpts = ROLE_DEF.filter(([cap]) => caps.includes(cap)).map(([, key, label]) => {
      // new devices: PV always, single roles only while nobody has them
      const on = st.isNew ? key === 'pv' || !roles[key] : inRole(key);
      const other = key !== 'pv' && roles[key] && roles[key] !== st.dev.id ? ` <span class="why">(bisher: ${esc(devName(roles[key]))})</span>` : '';
      return `<label class="f cb"><input type="checkbox" data-role="${key}"${on ? ' checked' : ''}> ${label}${other}</label>`;
    }).join('');
    $('setupTitle').textContent = st.isNew ? shortName(d) + (steps.length ? ' verbinden' : ' hinzufügen') : (st.dev.name || st.dev.id) + ' bearbeiten';
    body(`<div class="stack">
      ${st.isNew && d.description ? `<div class="why">${esc(d.description)}</div>` : ''}
      ${steps.length ? `<ol class="steps">${steps.map(s => `<li>${s}</li>`).join('')}</ol>` : ''}
      <label class="f" for="sp__name">Name<input type="text" id="sp__name" value="${esc(st.dev.name || '')}" placeholder="${esc(st.dev.id)}" autocomplete="off"></label>
      ${visible.map(p => field(p, pv[p.name], st.isNew)).join('')}
      ${roleOpts ? `<fieldset class="roles"><legend>Verwenden als</legend>${roleOpts}</fieldset>` : ''}
      ${ctl ? `<label class="f" for="sp__mode">Steuerung<select id="sp__mode">${Object.keys(MODE_TEXT).map(m => `<option value="${m}"${m === mode ? ' selected' : ''}>${MODE_TEXT[m]}</option>`).join('')}</select>
        <span class="why">„Fremdgesteuert“: ein anderes System (z. B. evcc, Home Assistant) steuert, voltibri liest nur mit.</span></label>` : ''}
      ${lists.length ? `<div class="note">${esc(lists.join(', '))}: nur in der YAML-Datei bearbeitbar, bleibt beim Speichern erhalten.</div>` : ''}
      <details><summary>Erweiterte Einstellungen</summary><div>
        <label class="f" for="sp__icon">Symbol<select id="sp__icon"><option value="">automatisch</option>${Object.keys(ICON_LABEL).map(k => `<option value="${k}"${k === icon ? ' selected' : ''}>${ICON_LABEL[k]}</option>`).join('')}</select></label>
        ${pow ? `<fieldset class="cor"><legend>Messkorrektur</legend>
          <label class="f" for="sp__factor">Faktor<input type="number" step="any" id="sp__factor" value="${cor.factor || ''}" placeholder="1 = unverändert, 1,03 = +3 %"></label>
          <label class="f" for="sp__offset">Aufschlag (W)<input type="number" step="any" id="sp__offset" value="${cor.offsetW || ''}" placeholder="0"></label>
          <label class="f" for="sp__above">Aufschlag nur ab (W)<input type="number" step="any" id="sp__above" value="${cor.activeAboveW || ''}" placeholder="0"></label>
        </fieldset>` : ''}
        ${adv.map(p => field(p, pv[p.name], st.isNew)).join('')}
        <span class="why">Geräte-ID: ${esc(st.dev.id)} · Typ: ${esc(d.type)}</span>
      </div></details></div>`);
    err(st.readOnly ? 'Die Konfiguration wird über eine YAML-Datei verwaltet – hier nur ansehen.' : '');
    buttons(`${st.isNew || st.readOnly ? '' : '<button type="button" class="btn danger" data-remove>Entfernen</button><span class="spacer"></span>'}
      <button type="button" class="btn" data-close>${st.readOnly ? 'Schließen' : 'Abbrechen'}</button>${st.readOnly ? '' : '<button type="submit" class="btn primary">Speichern</button>'}`);
    if (!dlg.open) dlg.showModal();
    const first = dlg.querySelector('[data-param]:not([type=checkbox])') || $('sp__name');
    if (first) first.focus();
  }

  function collect() {
    const out = {...(st.dev.params || {})};
    for (const el of dlg.querySelectorAll('[data-param]')) {
      const p = st.drv.params.find(x => x.name === el.dataset.param);
      if (p.type === 'bool') { out[p.name] = el.checked; continue; }
      const v = el.value.trim();
      if (v === '') { delete out[p.name]; continue; } // default; an empty secret keeps the stored one (server side)
      out[p.name] = p.type === 'int' || p.type === 'float' ? Number(v) : v;
    }
    const dev = {...st.dev, params: out};
    const name = $('sp__name').value.trim();
    if (name) dev.name = name; else delete dev.name;
    if ($('sp__mode')) dev.mode = $('sp__mode').value;
    const icon = $('sp__icon').value;
    if (icon) dev.icon = icon; else delete dev.icon;
    if ($('sp__factor')) {
      const num = id => { const v = $(id).value.trim(); return v === '' ? 0 : Number(v); };
      dev.correction = {factor: num('sp__factor'), offsetW: num('sp__offset'), activeAboveW: num('sp__above')};
      for (const k in dev.correction) if (!dev.correction[k]) delete dev.correction[k];
    }
    return dev;
  }

  async function applied(version) { // the runtime applies a new version asynchronously
    for (let i = 0; i < 20; i++) {
      await new Promise(r => setTimeout(r, 1000));
      const s = await fetch('api/status').then(r => r.json()).catch(() => null);
      if (!s) continue;
      if (s.state === 'error' && s.message && s.message.includes('Version ' + version)) return {error: s.message};
      if (s.version >= version && s.state !== 'error') return {state: s};
    }
    return {error: 'voltibri hat die Änderung noch nicht übernommen. Bitte die Seite gleich neu laden.'};
  }

  async function finish(v, id) {
    st.changed = true;
    body(`<div class="why">Gespeichert als Version ${v.id}. voltibri übernimmt die Änderung …</div>`);
    buttons('');
    const res = await applied(v.id);
    if (res.error) { err(res.error); buttons('<button type="button" class="btn" data-close>Schließen</button>'); return; }
    const setupHint = res.state.state === 'setup' ? `<div class="note">${esc(res.state.message || 'voltibri ist noch nicht vollständig eingerichtet.')}</div>` : '';
    if (id && (st.drv.capabilities || []).includes('oauth')) {
      const ch = await fetch('api/charging').then(r => r.json()).catch(() => ({}));
      const p = (ch.providers || []).find(x => x.id === id);
      if (p && p.needsLogin && !p.authorized) {
        body(`<div class="why">Gespeichert. Jetzt einmal bei ${esc(p.name)} anmelden – danach kommst du hierher zurück.</div>${setupHint}`);
        buttons(`<button type="button" class="btn" data-close>Später</button><a class="btn primary" href="api/oauth/${encodeURIComponent(p.id)}/login?back=${encodeURIComponent(location.pathname)}">Bei ${esc(p.name)} anmelden</a>`);
        return;
      }
    }
    body(`<div class="why">Übernommen (Version ${v.id}).</div>${setupHint}`);
    buttons('<button type="button" class="btn primary" data-close>Fertig</button>');
  }

  function roleBody(dev) {
    const boxes = [...dlg.querySelectorAll('[data-role]')];
    if (!boxes.length) return {device: dev};
    const keys = new Set(ROLE_DEF.map(r => r[1]));
    const roles = st.cfg.roles || {};
    // keep roles this form does not show (capability not offered by the driver)
    const keep = [...keys].filter(k => !boxes.some(b => b.dataset.role === k) && (k === 'pv' ? (roles.pv || []).includes(dev.id) : roles[k] === dev.id));
    return {device: dev, roles: keep.concat(boxes.filter(b => b.checked).map(b => b.dataset.role))};
  }

  async function save() {
    const missing = [...dlg.querySelectorAll('[data-param][required]')].filter(el => !el.value.trim())
      .map(el => st.drv.params.find(x => x.name === el.dataset.param).label);
    if (missing.length) { err('Bitte ausfüllen: ' + missing.join(', ')); return; }
    const dev = collect();
    const prev = $('setupActions').innerHTML;
    buttons('<button type="button" class="btn" disabled>Speichern …</button>');
    const r = await fetch('api/devices/' + encodeURIComponent(dev.id), {method: 'PUT', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(roleBody(dev))});
    if (!r.ok) {
      const e = await r.json().catch(() => ({}));
      err([e.error || 'Fehler ' + r.status].concat(e.details || []).join(' · '));
      buttons(prev);
      return;
    }
    finish(await r.json(), dev.id);
  }

  async function remove() {
    if (!confirm(`„${st.dev.name || st.dev.id}“ wirklich entfernen? Die vorige Version bleibt in der Versionsliste und kann zurückgeholt werden.`)) return;
    const r = await fetch('api/devices/' + encodeURIComponent(st.dev.id), {method: 'DELETE'});
    if (!r.ok) {
      const e = await r.json().catch(() => ({}));
      err([e.error || 'Fehler ' + r.status].concat(e.details || []).join(' · '));
      return;
    }
    finish(await r.json(), null);
  }

  return {ready, drivers: () => drivers, driver: t => drivers.find(d => d.type === t), open};
})();
