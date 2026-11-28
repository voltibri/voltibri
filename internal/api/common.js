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
  // interface icons
  overview:'<rect x="3" y="3" width="7" height="8" rx="1.5"/><rect x="14" y="3" width="7" height="5" rx="1.5"/><rect x="14" y="12" width="7" height="9" rx="1.5"/><rect x="3" y="15" width="7" height="6" rx="1.5"/>',
  chart:   '<path d="M4 20V10M10 20V4M16 20v-7M22 20H2"/>',
  gear:    '<circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z"/>',
  calendar:'<rect x="3" y="5" width="18" height="16" rx="2"/><path d="M3 10h18M8 3v4M16 3v4"/>',
  chevL:   '<path d="m15 18-6-6 6-6"/>',
  chevR:   '<path d="m9 18 6-6-6-6"/>',
  chevD:   '<path d="m6 9 6 6 6-6"/>',
  flame:   '<path d="M12 3c1 3.5 5 5.5 5 10a5 5 0 0 1-10 0c0-2.5 1.5-4 2.5-5 .3 1.6 1 2.6 2 3 0-3-1-5.5.5-8z"/>',
  leaf:    '<path d="M5 19c0-9 6-14 15-14 0 9-5 15-14 15"/><path d="M5 19c3-4 6-6 9-7"/>',
  euro:    '<path d="M17 6.5A6 6 0 1 0 17 17.5"/><path d="M4 10h9M4 14h9"/>',
  bolt:    '<path d="M13 2 5 13h6l-1 9 8-11h-6z"/>',
  power:   '<path d="M12 3v8"/><path d="M6.4 6.6a8 8 0 1 0 11.2 0"/>',
  clock:   '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
  target:  '<circle cx="12" cy="12" r="9"/><circle cx="12" cy="12" r="5"/><circle cx="12" cy="12" r="1"/>',
  plus:    '<path d="M12 5v14M5 12h14"/>',
  list:    '<path d="M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01"/>',
  device:  '<rect x="5" y="2" width="14" height="20" rx="2"/><path d="M12 18h.01"/>',
  user:    '<circle cx="12" cy="8" r="4"/><path d="M4 21c0-4.4 3.6-7 8-7s8 2.6 8 7"/>',
  users:   '<circle cx="9" cy="8" r="3.5"/><path d="M2.5 20c0-3.8 3-6 6.5-6s6.5 2.2 6.5 6"/><path d="M16 4.6a3.5 3.5 0 0 1 0 6.8M18 14.3c2.2.7 3.5 2.6 3.5 5.7"/>',
  logout:  '<path d="M15 4h3a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2h-3"/><path d="M10 17l-5-5 5-5M5 12h11"/>',
  key:     '<circle cx="8" cy="15" r="4"/><path d="m10.8 12.2 9.2-9.2M17 6l3 3M14.5 8.5l2 2"/>',
  lock:    '<rect x="4" y="10" width="16" height="11" rx="2"/><path d="M8 10V7a4 4 0 0 1 8 0v3"/>',
};
const ICON_LABEL = {solar: 'PV', balcony: 'Balkonkraftwerk', car: 'Auto / Wallbox', heatpump: 'Wärmepumpe / Klima', plug: 'Steckdose',
  water: 'Warmwasser', house: 'Haus', grid: 'Netz', battery: 'Batterie'};
const iconSVG = (name, size = 20) => `<svg width="${size}" height="${size}" viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">${ICONS[name] || ICONS.plug}</svg>`;
const KIND_LABEL = {pv: 'PV-Anlage', battery: 'Hausbatterie', inverter: 'Wechselrichter', 'grid-meter': 'Netzzähler', 'ev-charger': 'Wallbox', vehicle: 'Fahrzeug',
  'vehicle-account': 'Fahrzeugkonto', 'heat-pump': 'Wärmepumpe', 'heating-rod': 'Heizstab', climate: 'Klima / Entfeuchter', appliance: 'Haushaltsgerät',
  forecast: 'Prognose', tariff: 'Strompreis'};
const slugify = s => s.toLowerCase().replace(/ä/g, 'ae').replace(/ö/g, 'oe').replace(/ü/g, 'ue').replace(/ß/g, 'ss').replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '');
// vehicles: in which charge modes the home battery must not supply the car
const BAT_OPTS = {allow: {label: 'darf liefern', modes: []}, fast: {label: 'nicht bei Schnell', modes: ['fast']},
  fastmin: {label: 'nicht bei Schnell und Min+PV', modes: ['fast', 'minpv']}, never: {label: 'nie', modes: ['external', 'pv', 'pv1', 'minpv', 'boost', 'fast']}};
// (boost is new: older settings without it still match "nie")
// vehicles: the charge modes offered at the wallbox ("Aus" is always possible)
const VEHICLE_MODES = [['pv', 'Überschuss'], ['minpv', 'Min+PV'], ['pv1', '1-phasig + PV'], ['boost', 'Boost'], ['fast', 'Schnell'], ['external', 'Extern']];
const modesText = ms => VEHICLE_MODES.filter(([k]) => ms.includes(k)).map(([, l]) => l).join(', ');
const batKey = ms => { const n = l => [...(l || [])].filter(m => m !== 'boost').sort().join(), s = n(ms); return Object.keys(BAT_OPTS).find(k => n(BAT_OPTS[k].modes) === s) || 'allow'; };

// vehicles: price rule per energy share (see internal/pricing)
const PRICE_SHARES = [['pv', 'PV', 'feedin'], ['battery', 'Hausbatterie', 'feedin'], ['grid', 'Netz', 'grid']];
const PRICE_OPTS = [['inherit', 'wie Standard'], ['fixed', 'Festpreis'], ['adjust', 'Standard mit Aufschlag'], ['feedin', 'Einspeisevergütung'],
  ['grid', 'Netztarif'], ['free', 'kostenlos']];
const ctTxt = v => (Math.round(v * 100) / 100).toLocaleString('de-DE') + ' ct';
// the rule of one share as UI choice (all = rule for every share without its own)
const priceChoice = (rule, key, def) => {
  const s = (rule && rule[key] && rule[key].mode) ? rule[key] : (rule && rule.all) || {};
  let sel = !s.mode || s.mode === 'inherit' ? 'inherit' : s.mode === 'adjust' ? 'adjust' : s.base;
  if (sel === def) sel = "adjust"; // replace by the default base = adjust the inherited price
  return {sel, ct: s.ct || '', percent: s.percent || '', addCt: s.addCt || ''};
};
function priceRows(rule) {
  return PRICE_SHARES.map(([key, label, def]) => {
    const c = priceChoice(rule, key, def);
    const opts = PRICE_OPTS.filter(([v]) => v !== def).map(([v, l]) =>
      `<option value="${v}"${v === c.sel ? ' selected' : ''}>${v === 'inherit' ? l + (def === 'grid' ? ' (Netztarif)' : ' (Einspeisevergütung)') : l}</option>`).join('');
    return `<div class="prow" data-pshare="${key}"><span>${label}</span>
      <select data-pmode aria-label="Preis ${label}">${opts}</select>
      <div class="pargs"><input type="number" step="any" min="0" data-pct value="${c.ct}" placeholder="Preis ct/kWh" aria-label="Festpreis ${label} (ct/kWh)">
      <input type="number" step="any" data-ppct value="${c.percent}" placeholder="Aufschlag %" aria-label="Aufschlag ${label} (%)">
      <input type="number" step="any" data-padd value="${c.addCt}" placeholder="Aufschlag ct" aria-label="Aufschlag ${label} (ct/kWh)"></div></div>`;
  }).join('');
}
// reads the rule from the rows (undefined: default)
function priceRule(root) {
  const rule = {};
  root.querySelectorAll('[data-pshare]').forEach(row => {
    const sel = row.querySelector('[data-pmode]').value, num = a => Number(row.querySelector(a).value) || 0;
    if (sel === 'inherit') return;
    const s = sel === 'adjust' ? {mode: 'adjust'} : {mode: 'replace', base: sel};
    if (sel === 'fixed') s.ct = num('[data-pct]');
    if (sel !== 'free') {
      if (num('[data-ppct]')) s.percent = num('[data-ppct]');
      if (num('[data-padd]')) s.addCt = num('[data-padd]');
    }
    if (sel === 'adjust' && !s.percent && !s.addCt) return; // nothing to adjust: default
    rule[row.dataset.pshare] = s;
  });
  return Object.keys(rule).length ? rule : undefined;
}
// shows the fields a choice needs and the resulting prices at today's tariffs
function priceUpdate(root, feedinCt, gridCt) {
  const parts = [];
  root.querySelectorAll('[data-pshare]').forEach(row => {
    const sel = row.querySelector('[data-pmode]').value, num = a => Number(row.querySelector(a).value) || 0;
    row.querySelector('[data-pct]').hidden = sel !== 'fixed';
    row.querySelector('[data-ppct]').hidden = row.querySelector('[data-padd]').hidden = sel === 'inherit' || sel === 'free';
    row.querySelector('.pargs').hidden = sel === 'inherit' || sel === 'free';
    const defGrid = PRICE_SHARES.find(x => x[0] === row.dataset.pshare)[2] === 'grid';
    const def = defGrid ? gridCt : feedinCt;
    const base = {inherit: def, adjust: def, fixed: num('[data-pct]'), feedin: feedinCt, grid: gridCt, free: 0}[sel];
    const pct = num('[data-ppct]'), add = num('[data-padd]');
    const p = sel === 'inherit' || sel === 'free' ? base : base * (1 + pct / 100) + add;
    let txt;
    if (p != null && !isNaN(p)) txt = ctTxt(p);
    else { // tariff unknown (e.g. no tariff configured): describe the rule
      txt = sel === 'grid' || (defGrid && (sel === 'inherit' || sel === 'adjust')) ? 'Tarif' : 'Einspeisevergütung';
      if (sel !== 'inherit') txt += (pct ? ` ${pct > 0 ? '+' : '−'} ${Math.abs(pct).toLocaleString('de-DE')} %` : '') + (add ? ` ${add > 0 ? '+' : '−'} ${ctTxt(Math.abs(add))}` : '');
    }
    parts.push(row.querySelector('span').textContent + ' ' + txt);
  });
  const el = root.querySelector('#sp__pprev');
  if (el) el.textContent = (feedinCt == null && gridCt == null ? 'Je kWh: ' : 'Heute je kWh: ') + parts.join(' · ');
}

const FLEX_TEXT = {none: ['keine', 'Nur messen – voltibri schaltet nichts.'],
  shiftable: ['Start verschiebbar', 'Ein Programm, das einmal gestartet durchlaufen muss (Waschmaschine, Spülmaschine, Trockner). voltibri darf den Start verschieben – die Steuerung folgt in einem späteren Schritt.'],
  interruptible: ['unterbrechbar', 'Braucht Energie bis zu einer Frist, Pausen schaden nicht (E-Bike-Ladegerät, Poolpumpe). voltibri darf ein- und ausschalten – die Steuerung folgt in einem späteren Schritt.'],
  surplus: ['nur Überschuss', 'Kein Ziel, läuft nur bei PV-Überschuss (Heizstab als Zusatz, Entfeuchter, Poolpumpe). Die Steuerung folgt in einem späteren Schritt.']};
// what voltibri can technically do with a device (from the driver)
const controlOf = drv => {
  const c = (drv && drv.capabilities) || [];
  if (c.includes('charger-control') || c.includes('battery-control')) return ['regelbar', 'Leistung stufenlos einstellbar'];
  if (c.includes('stages')) return ['in Stufen', 'jede Phase einzeln schaltbar'];
  if (c.includes('switch')) return ['schaltbar', 'an/aus'];
  return ['keine', 'nur messen'];
};
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
      const rc = e.target.closest('[data-recalc]');
      if (rc) recalc(rc.dataset.recalc === 'run');
    });
    dlg.addEventListener('change', e => {
      if (e.target.id === 'sp_source') {
        $('sp_sourceVehicle').innerHTML = sourceOpts(e.target.value, '');
        $('sp_sourceVehicle').closest('label').hidden = !e.target.value;
      }
      if (e.target.id === 'sp_sourceVehicle' && !$('sp__name').value.trim()) {
        const o = e.target.selectedOptions[0];
        if (o && o.value) $('sp__name').value = o.textContent.replace(/ · .*$/, '');
      }
      if (e.target.id === 'sp__flex') {
        $('sp__flexwhy').textContent = FLEX_TEXT[e.target.value][1];
        $('sp__flexwarn').hidden = e.target.value === 'none' || controlOf(st.drv)[0] !== 'keine';
      }
    });
    dlg.addEventListener('input', e => {
      if (e.target.closest('[data-pshare]')) priceUpdate(dlg, (st.ch || {}).feedinCt, (st.ch || {}).gridCt);
      if (e.target.id === 'sp__rfrom' || e.target.id === 'sp__rto') { $('sp__rres').textContent = ''; dlg.querySelector('[data-recalc=run]').hidden = true; }
    });
    dlg.addEventListener('close', () => { if (st.changed && st.onDone) st.onDone(); });
  }
  const buttons = html => { $('setupActions').innerHTML = html; };
  const body = html => { $('setupBody').innerHTML = html; };
  const err = msg => { $('setupErr').textContent = msg || ''; };

  // vehicle assets: data source (vehicle account) and its vehicle
  function sourceOpts(src, cur) {
    const vs = ((st.ch && st.ch.vehicles) || []).filter(v => v.provider === src && v.sourceId && (!v.asset || v.key === st.dev.id));
    if (cur && !vs.some(v => v.sourceId === cur)) vs.push({sourceId: cur, name: cur + ' (zurzeit nicht gemeldet)', info: {}});
    return '<option value="">bitte wählen</option>' + vs.map(v => `<option value="${esc(v.sourceId)}"${v.sourceId === cur ? ' selected' : ''}>${esc(v.info.name || v.name)}${v.info.vin ? ' · ' + esc(v.info.vin) : ''}</option>`).join('');
  }
  function vehicleSource(pv) {
    const accts = (st.cfg.devices || []).filter(x => x.type !== 'vehicle' && ((drivers.find(d => d.type === x.type) || {}).capabilities || []).includes('vehicles'));
    const src = pv.source || '';
    return `<label class="f" for="sp_source">Datenquelle<select id="sp_source" data-param="source"><option value="">keine – manuell (Ladestand unbekannt)</option>${accts.map(a =>
        `<option value="${esc(a.id)}"${a.id === src ? ' selected' : ''}>${esc(a.name || a.id)}</option>`).join('')}</select>
        <span class="why">Woher Ladestand und Ladeziel kommen. Lässt sich jederzeit wechseln – Ladevorgänge, Einstellungen und Ladekarten bleiben beim Fahrzeug.</span></label>
      <label class="f" for="sp_sourceVehicle"${src ? '' : ' hidden'}>Fahrzeug im Konto<select id="sp_sourceVehicle" data-param="sourceVehicle">${sourceOpts(src, pv.sourceVehicle || '')}</select></label>`;
  }

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
    st.ch = type === 'vehicle' ? await fetch('api/charging').then(r => r.json()).catch(() => ({})) : null;
    if (!st.drv) { err('Unbekannter Gerätetyp: ' + type); return; }
    const d = st.drv, df = st.defaults || {};
    if (!id) {
      st.dev = {id: type, type, name: df.name || (type === 'none' ? '' : shortName(d)), params: {}};
      if (df.icon) st.dev.icon = df.icon;
      if (df.kind && type === 'none') st.dev.kind = df.kind;
      if (df.params) st.dev.params = {...df.params};
      let n = 2;
      while (st.ids.has(st.dev.id)) st.dev.id = type + '-' + n++;
    }
    const pv = st.dev.params || {}, caps = d.capabilities || [];
    const params = (d.params || []).filter(p => p.type !== 'list');
    const visible = params.filter(p => !p.advanced && p.name !== 'redirectUri' && !(d.type === 'vehicle' && ['source', 'sourceVehicle'].includes(p.name)));
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
    // asset kind and life cycle: prominent for devices without a connection
    const bare = d.type === 'none';
    const kindSel = `<label class="f" for="sp__kind">Art<select id="sp__kind"><option value="">${bare ? 'bitte wählen' : 'automatisch'}</option>${Object.keys(KIND_LABEL).map(k =>
      `<option value="${k}"${k === st.dev.kind ? ' selected' : ''}>${KIND_LABEL[k]}</option>`).join('')}</select>
      <span class="why">Bestimmt den Bereich und ob Läufe Ladevorgänge oder Heizläufe sind.</span></label>`;
    const life = `<div class="row2"><label class="f" for="sp__from">In Betrieb seit<input type="date" id="sp__from" value="${esc(st.dev.commissioned || '')}"></label>
      <label class="f" for="sp__to">Stillgelegt seit<input type="date" id="sp__to" value="${esc(st.dev.decommissioned || '')}"></label></div>
      <span class="why">Stillgelegte Geräte werden nicht mehr abgefragt, behalten aber ihre Läufe und Auswertungen.</span>`;
    // consumers (not wallboxes, producers, meters): flexibility and runs
    const consumer = (caps.includes('consumer') || caps.includes('switch') || bare) && !caps.includes('charger');
    const flex = st.dev.flexibility || 'none';
    // home battery: may it supply this device? (consumers and wallboxes)
    const batteryOpt = consumer || caps.includes('charger') ? `<label class="f" for="sp__bat">Hausbatterie<select id="sp__bat">
        <option value=""${st.dev.batteryUse ? '' : ' selected'}>darf dieses Gerät versorgen</option>
        <option value="never"${st.dev.batteryUse === 'never' ? ' selected' : ''}>nie – nur PV und Netz</option></select>
        <span class="why">Mit „nie“ begrenzt voltibri die Entladung der Hausbatterie auf den übrigen Verbrauch, solange das Gerät läuft. Für Fahrzeuge lässt sich das je Lademodus beim Fahrzeug einstellen (Konfiguration → Fahrzeuge).</span></label>` : '';
    const consumerOpts = consumer ? `<label class="f" for="sp__flex">Flexibilität<select id="sp__flex">${Object.entries(FLEX_TEXT).map(([k, [l]]) =>
        `<option value="${k}"${k === flex ? ' selected' : ''}>${l}</option>`).join('')}</select>
        <span class="why" id="sp__flexwhy">${esc(FLEX_TEXT[flex][1])}</span>
        <span class="why">Steuerung: <b>${controlOf(d)[0]}</b> (${controlOf(d)[1]})</span>
        <span class="note" id="sp__flexwarn"${flex !== 'none' && controlOf(d)[0] === 'keine' ? '' : ' hidden'}>voltibri kann dieses Gerät nicht schalten – die Flexibilität wirkt erst mit einem schaltbaren Anschluss, z. B. einem Zwischenstecker.</span></label>
      <label class="f cb"><input type="checkbox" id="sp__runs"${st.dev.runs === false ? '' : ' checked'}> Läufe erfassen</label>
      <span class="why">Jeder Betrieb (z. B. ein Waschgang) wird als Lauf mit Energie, PV-Anteil und Kosten gespeichert. Für Geräte, die ständig takten wie ein Kühlschrank, ausschalten – der Verbrauch wird trotzdem im Lastgang erfasst.</span>` : '';
    const thresholdOpt = consumer ? `<label class="f" for="sp__thr">Lauf beginnt ab (W)<input type="number" id="sp__thr" min="1" step="1" value="${st.dev.runThresholdW || ''}" placeholder="Standard: 25"></label>` : '';
    $('setupTitle').textContent = st.isNew && bare ? 'Gerät ohne Verbindung hinzufügen' : st.isNew ? shortName(d) + (steps.length ? ' verbinden' : ' hinzufügen') : (st.dev.name || st.dev.id) + ' bearbeiten';
    body(`<div class="stack">
      ${st.isNew && d.description ? `<div class="why">${esc(d.description)}</div>` : ''}
      ${steps.length ? `<ol class="steps">${steps.map(s => `<li>${s}</li>`).join('')}</ol>` : ''}
      <label class="f" for="sp__name">Name<input type="text" id="sp__name" value="${esc(st.dev.name || '')}" placeholder="${esc(st.dev.id)}" autocomplete="off"></label>
      ${d.type === 'vehicle' ? vehicleSource(pv) : ''}
      ${visible.map(p => field(p, pv[p.name], st.isNew)).join('')}
      ${bare ? kindSel + life : ''}
      ${roleOpts ? `<fieldset class="roles"><legend>Verwenden als</legend>${roleOpts}</fieldset>` : ''}
      ${ctl ? `<label class="f" for="sp__mode">Steuerung<select id="sp__mode">${Object.keys(MODE_TEXT).map(m => `<option value="${m}"${m === mode ? ' selected' : ''}>${MODE_TEXT[m]}</option>`).join('')}</select>
        <span class="why">„Fremdgesteuert“: ein anderes System (z. B. evcc, Home Assistant) steuert, voltibri liest nur mit.</span></label>` : ''}
      ${consumerOpts}
      ${(st.dev.kind || '') === 'heating-rod' || st.dev.icon === 'water' ? `<label class="f" for="sp__heatuse">Heizstab für<select id="sp__heatuse">
        <option value=""${st.dev.heatUse ? '' : ' selected'}>–</option>
        <option value="hotwater"${st.dev.heatUse === 'hotwater' ? ' selected' : ''}>Warmwasser (oberer Pufferbereich)</option>
        <option value="heating"${st.dev.heatUse === 'heating' ? ' selected' : ''}>Heizungsunterstützung (am Vorlauf der Heizkreise)</option></select>
        <span class="why">Wärmepumpe und Heizstäbe heizen denselben Puffer und erscheinen im Energiefluss gemeinsam als „Heizung“.</span></label>` : ''}
      ${batteryOpt}
      ${d.type === 'vehicle' ? `<label class="f" for="sp__vbat">Hausbatterie beim Laden<select id="sp__vbat">${Object.entries(BAT_OPTS).map(([k, o]) =>
        `<option value="${k}"${k === batKey(st.dev.noBatteryModes) ? ' selected' : ''}>${o.label}</option>`).join('')}</select>
        <span class="why">In den gewählten Lademodi lädt das Auto nur aus PV und Netz; voltibri begrenzt dann die Entladung der Hausbatterie auf den übrigen Verbrauch.</span></label>` : ''}
      ${d.type === 'vehicle' ? `<fieldset class="roles"><legend>Lademodi für dieses Fahrzeug</legend>${VEHICLE_MODES.map(([k, l]) =>
        `<label class="f cb"><input type="checkbox" data-cmode="${k}"${!(st.dev.chargeModes || []).length || st.dev.chargeModes.includes(k) ? ' checked' : ''}> ${l}</label>`).join('')}
        <span class="why">Nur diese Modi stehen an der Wallbox zur Wahl, sobald das Fahrzeug erkannt oder gewählt ist; „Aus“ geht immer. Alle oder keiner angehakt: alle Modi. Gäste sind nicht betroffen.</span></fieldset>` : ''}
      ${d.type === 'vehicle' ? `<fieldset class="price"><legend>Preis für dieses Fahrzeug</legend>${priceRows(st.dev.price)}
        <span class="why" id="sp__pprev"></span>
        <span class="why">Gilt für die Ladevorgänge dieses Fahrzeugs an jeder Wallbox, z. B. für die Abrechnung. Standard: PV und Hausbatterie zur entgangenen Einspeisevergütung, Netz zum Tarif. Die eigene Auswertung (Haus, Wallboxen) rechnet weiter mit dem Standard.</span>
        ${st.isNew ? '' : `<details class="recalc"><summary>Bisherige Ladevorgänge neu berechnen</summary><div class="stack">
          <div class="row2"><label class="f" for="sp__rfrom">Von<input type="date" id="sp__rfrom"></label><label class="f" for="sp__rto">Bis<input type="date" id="sp__rto"></label></div>
          <span class="why">Wendet die <b>gespeicherte</b> Regel auf die Ladevorgänge des Zeitraums an (leer: alle). Importierte Vorgänge ohne genaue Aufteilung werden mit dem Durchschnittspreis ihres Zeitraums bewertet; ihr ursprünglicher Preis bleibt im Lauf vermerkt.</span>
          <div class="actions"><button type="button" class="btn" data-recalc="preview">Vorschau</button><button type="button" class="btn primary" data-recalc="run" hidden>Neu berechnen</button></div>
          <span class="why" id="sp__rres" role="status"></span></div></details>`}</fieldset>` : ''}
      ${lists.length ? `<div class="note">${esc(lists.join(', '))}: nur in der YAML-Datei bearbeitbar, bleibt beim Speichern erhalten.</div>` : ''}
      <details><summary>Erweiterte Einstellungen</summary><div>
        ${bare ? '' : kindSel + life}
        ${thresholdOpt}
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
    if (d.type === 'vehicle') { // the rule as rendered = saved (recalculation needs it saved)
      priceUpdate(dlg, (st.ch || {}).feedinCt, (st.ch || {}).gridCt);
      st.priceSaved = JSON.stringify(priceRule(dlg) || null);
    }
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
    for (const [el, key] of [['sp__kind', 'kind'], ['sp__from', 'commissioned'], ['sp__to', 'decommissioned']]) {
      const v = $(el).value;
      if (v) dev[key] = v; else delete dev[key];
    }
    if (dlg.querySelector('[data-pshare]')) { const p = priceRule(dlg); if (p) dev.price = p; else delete dev.price; }
    if (dlg.querySelector('[data-cmode]')) { // all or none = all modes (also ones added later)
      const all = [...dlg.querySelectorAll('[data-cmode]')], on = all.filter(el => el.checked).map(el => el.dataset.cmode);
      if (on.length && on.length < all.length) dev.chargeModes = on; else delete dev.chargeModes;
    }
    if ($('sp__vbat')) { const m = BAT_OPTS[$('sp__vbat').value].modes; if (m.length) dev.noBatteryModes = m; else delete dev.noBatteryModes; }
    if ($('sp__heatuse')) { const v = $('sp__heatuse').value; if (v) dev.heatUse = v; else delete dev.heatUse; }
    if ($('sp__bat')) { const b = $('sp__bat').value; if (b) dev.batteryUse = b; else delete dev.batteryUse; }
    if ($('sp__flex')) {
      const f = $('sp__flex').value;
      if (f && f !== 'none') dev.flexibility = f; else delete dev.flexibility;
      if ($('sp__runs').checked) delete dev.runs; else dev.runs = false;
      const thr = Number($('sp__thr').value);
      if (thr > 0) dev.runThresholdW = thr; else delete dev.runThresholdW;
    }
    if (st.isNew && (dev.type === 'none' || dev.type === 'vehicle') && name) { // readable id for devices without a connection
      const base = slugify(name) || 'geraet';
      let n = 2;
      dev.id = base;
      while (st.ids.has(dev.id)) dev.id = base + '-' + n++;
    }
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

  // applies the saved price rule to past charging sessions (preview first)
  async function recalc(run) {
    const out = $('sp__rres'), btn = dlg.querySelector('[data-recalc=run]');
    if (JSON.stringify(priceRule(dlg) || null) !== st.priceSaved) {
      out.textContent = 'Bitte zuerst speichern – neu berechnet wird mit der gespeicherten Regel.';
      return;
    }
    const body = {vehicle: st.dev.id, from: $('sp__rfrom').value || undefined, to: $('sp__rto').value || undefined, dryRun: !run};
    out.textContent = run ? 'Berechne …' : 'Vorschau …';
    const r = await fetch('api/runs/recalc', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body)});
    const res = await r.json().catch(() => ({}));
    if (!r.ok) { out.textContent = res.error || 'Fehler ' + r.status; return; }
    const eur = ct => (ct / 100).toLocaleString('de-DE', {style: 'currency', currency: 'EUR'});
    const skipped = res.skipped ? ` ${res.skipped} ohne bekannten Preis übersprungen.` : '';
    if (!res.runs) {
      out.textContent = res.skipped ? `${res.skipped} Ladevorgänge übersprungen: Für ihren Zeitraum ist kein Strompreis bekannt (Tarif prüfen).` : 'Keine Ladevorgänge im Zeitraum.';
      btn.hidden = true;
      return;
    }
    out.textContent = `${res.runs} Ladevorgänge: ${eur(res.beforeCt)} → ${eur(res.afterCt)}${run ? ' – gespeichert.' : '.'}${skipped}`;
    btn.hidden = run;
    if (run) st.changed = true;
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

// Top bar shared by all pages: brand, navigation and a slot for status chips.
function topbar(active) {
  const el = document.querySelector('header.top');
  if (!el) return;
  const me = window.ME;
  const tabs = [['overview', './', 'Übersicht', 'overview'], ['reports', 'berichte', 'Berichte', 'chart'], ['config', 'konfiguration', 'Konfiguration', 'gear'], ['account', 'konto', 'Konto', 'user']]
    .filter(([k]) => !me || k === 'overview' || k === 'account' || (k === 'reports' ? me.reports : me.admin));
  el.innerHTML = `<a class="brand" href="./" aria-label="voltibri Übersicht">
      <span class="mark"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M13.2 2.5 5.5 13h5.6l-1.3 8.5L18.5 10h-5.8z" fill="#fff"/></svg></span>
      <span><b>voltibri</b><small>smart self-consumption</small></span></a>
    <div class="hstatus" id="hstatus"></div>
    <nav class="tabs" aria-label="Bereiche">${tabs.map(([k, href, label, ic]) =>
      `<a href="${href}"${k === active ? ' aria-current="page"' : ''}>${iconSVG(ic, 18)}<span>${label}</span></a>`).join('')}</nav>`;
}

// installable as an app (PWA); service workers need https or localhost
if ('serviceWorker' in navigator && (location.protocol === 'https:' || ['localhost', '127.0.0.1'].includes(location.hostname))) {
  navigator.serviceWorker.register('sw.js').catch(() => {});
}

// --- sign-in ---
// window.ME: the signed-in user and permissions (api/me), loaded once per
// page; pages wait for voltibri.me before rendering permission dependent parts.
voltibri.me = fetch('api/me').then(r => r.ok ? r.json() : null).catch(() => null).then(me => {
  window.ME = me;
  const active = document.querySelector('header.top nav [aria-current]');
  if (me && document.querySelector('header.top')) topbar(document.body.dataset.page || (active ? {'./': 'overview', berichte: 'reports', konfiguration: 'config', konto: 'account'}[active.getAttribute('href')] : ''));
  return me;
});

// 401 answers: "login" -> to the login page, "reauth" -> confirm the
// password and repeat the request once.
(() => {
  const plain = window.fetch.bind(window);
  let pending = null;
  function confirmLogin() {
    if (pending) return pending;
    pending = new Promise(async resolve => {
      // linked login services can confirm too (leaves the page and comes back)
      const me = window.ME || {}, st = await plain('api/auth/state').then(r => r.json()).catch(() => ({}));
      const linked = (st.providers || []).filter(p => (me.identities || []).some(i => i.provider === p.id));
      const back = encodeURIComponent(location.pathname + location.search + location.hash);
      const pw = !me.user || me.user.hasPassword;
      document.body.insertAdjacentHTML('beforeend', `<dialog class="setup" id="reauth" aria-labelledby="reauthTitle"><form class="inner" method="dialog" id="reauthForm">
        <h2 id="reauthTitle">Anmeldung bestätigen</h2>
        <p class="why">Für die Konfiguration bitte kurz bestätigen, dass du es bist. Danach bleibt sie eine Weile freigeschaltet.</p>
        ${pw ? '<label class="f" for="reauthPw">Passwort<input type="password" id="reauthPw" autocomplete="current-password" required></label>' : ''}
        ${linked.map(p => `<a class="btn" href="api/auth/oidc/${encodeURIComponent(p.id)}/start?purpose=reauth&next=${back}">Mit ${esc(p.name)} bestätigen</a>`).join('')}
        <div class="err" id="reauthErr" role="alert"></div>
        <div class="actions"><button type="button" class="btn" data-cancel>Abbrechen</button>${pw ? '<button type="submit" class="btn primary">Bestätigen</button>' : ''}</div>
      </form></dialog>`);
      const d = document.getElementById('reauth');
      const done = ok => { d.close(); d.remove(); pending = null; resolve(ok); };
      d.querySelector('[data-cancel]').onclick = () => done(false);
      d.addEventListener('cancel', () => done(false));
      d.querySelector('form').addEventListener('submit', async e => {
        e.preventDefault();
        const r = await plain('api/auth/reauth', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({password: document.getElementById('reauthPw').value})});
        if (r.ok) return done(true);
        const j = await r.json().catch(() => ({}));
        document.getElementById('reauthErr').textContent = j.error || 'Fehler ' + r.status;
      });
      d.showModal();
    });
    return pending;
  }
  window.fetch = async (input, init) => {
    const r = await plain(input, init);
    if (r.status !== 401 || !/^\/?api\//.test(String(input)) || /api\/auth\//.test(String(input)) || /\/login$/.test(location.pathname)) return r;
    const j = await r.clone().json().catch(() => ({}));
    if (j.code === 'reauth' && await confirmLogin()) return plain(input, init);
    if (j.code === 'login') location.href = 'login?next=' + encodeURIComponent(location.pathname + location.search + location.hash);
    return r;
  };
})();
