// Artçı canlı harita. Framework yok; Leaflet + WebSocket.
// Kaynaktan gelen tüm metinler (yer adı, özet) DOM'a yalnızca textContent ile
// yazılır: veri bize ait değil, HTML olarak yorumlanmamalı (XSS).
'use strict';

(() => {
  const WINDOW_MS = 24 * 3600 * 1000;
  const AFAD_URL = 'https://deprem.afad.gov.tr/';
  const SOURCE_NAMES = { afad: 'AFAD', kandilli: 'Kandilli', emsc: 'EMSC' };
  const TZ = 'Europe/Istanbul';

  const fmtFull = new Intl.DateTimeFormat('tr-TR', {
    timeZone: TZ, day: 'numeric', month: 'long', hour: '2-digit', minute: '2-digit', second: '2-digit',
  });
  const fmtShort = new Intl.DateTimeFormat('tr-TR', {
    timeZone: TZ, day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit',
  });
  const num = (n, d = 1) => Number(n).toLocaleString('tr-TR', { minimumFractionDigits: d, maximumFractionDigits: d });
  const sourceName = (s) => SOURCE_NAMES[s] || s;

  const $ = (id) => document.getElementById(id);
  const listEl = $('list');
  const emptyEl = $('empty');
  const countEl = $('count');
  const statusEl = $('status');
  const statusText = $('status-text');
  const filterEl = $('min-mag');

  // id -> { ev, marker, li, detail }
  const entries = new Map();
  let minMag = 0;

  // ---- Harita ----
  const map = L.map('map', { center: [39.0, 35.2], zoom: 6, minZoom: 4, worldCopyJump: true });
  L.tileLayer('https://tile.openstreetmap.org/{z}/{x}/{y}.png', {
    maxZoom: 18,
    attribution: '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> katkıcıları',
  }).addTo(map);
  const layer = L.layerGroup().addTo(map);

  function magClass(m) {
    if (m >= 5) return 'm5';
    if (m >= 4) return 'm4';
    if (m >= 3) return 'm3';
    return 'm0';
  }
  // Renkler CSS değişkenlerinden okunur; tema değişince yeniden uygulanır.
  function color(m) {
    return getComputedStyle(document.documentElement).getPropertyValue('--' + magClass(m)).trim();
  }
  // Büyüklük logaritmik olduğu için doğrusal yarıçap görsel olarak yeterli ayrım verir.
  const radius = (m) => Math.max(4, 3 + m * 2.6);
  function markerStyle(ev) {
    const c = color(ev.magnitude);
    return { radius: radius(ev.magnitude), color: c, weight: 1.5, fillColor: c, fillOpacity: 0.45 };
  }

  // ---- Popup ----
  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  function buildPopup(entry) {
    const ev = entry.ev;
    const root = el('div', 'pop');
    root.append(el('h3', null, ev.place_name || 'Yer bilgisi yok'));

    const dl = el('dl');
    const row = (k, v) => dl.append(el('dt', null, k), el('dd', null, v));
    row('Büyüklük', num(ev.magnitude) + (ev.mag_type ? ' (' + ev.mag_type + ')' : ''));
    row('Zaman', fmtFull.format(new Date(ev.origin_time)) + ' (TSİ)');
    row('Derinlik', ev.depth_km != null ? num(ev.depth_km) + ' km' : 'bilinmiyor');
    root.append(dl);

    if (ev.summary) {
      root.append(el('p', 'summary', ev.summary));
    } else {
      root.append(el('p', 'summary pending', 'Özet hazırlanıyor.'));
    }

    // Ayrıntı yüklendiyse her kaynağın kendi değerini göster: farklılık bilgidir.
    let srcText;
    if (entry.detail && entry.detail.reports.length) {
      srcText = entry.detail.reports
        .map((r) => sourceName(r.source) + ' (' + (r.mag_type ? r.mag_type + ' ' : '') + num(r.magnitude) + ')')
        .join(', ');
    } else {
      srcText = (ev.sources || []).map(sourceName).join(', ') || 'bilinmiyor';
    }
    root.append(el('p', 'src', 'Kaynaklar: ' + srcText));

    const official = el('p', 'src');
    official.append('Resmî bilgi: ');
    const a = el('a', null, 'AFAD');
    a.href = AFAD_URL;
    a.target = '_blank';
    a.rel = 'noopener';
    official.append(a);
    root.append(official);
    return root;
  }

  async function loadDetail(entry) {
    try {
      const res = await fetch('/api/events/' + encodeURIComponent(entry.ev.id));
      if (!res.ok) return;
      const d = await res.json();
      entry.detail = d;
      // Ayrıntı daha yeni olabilir (özet sonradan yazılmış olabilir).
      if (d.version >= entry.ev.version) Object.assign(entry.ev, d);
      if (entry.marker.isPopupOpen()) entry.marker.setPopupContent(buildPopup(entry));
    } catch (_) { /* ağ hatası: popup eldeki veriyle kalır */ }
  }

  // ---- Liste ----
  function fillItem(li, ev) {
    const btn = li.firstChild;
    btn.replaceChildren();
    const mag = el('span', 'mag ' + magClass(ev.magnitude));
    mag.append(el('span', 'sr-only', 'Büyüklük '), num(ev.magnitude));
    const meta = [fmtShort.format(new Date(ev.origin_time))];
    if (ev.depth_km != null) meta.push(num(ev.depth_km) + ' km');
    if (ev.sources && ev.sources.length) meta.push(ev.sources.map(sourceName).join(', '));
    btn.append(mag, el('span', 'place', ev.place_name || 'Yer bilgisi yok'), el('span', 'meta', meta.join(' · ')));
    li.dataset.t = String(Date.parse(ev.origin_time));
  }

  function createItem(entry) {
    const li = el('li');
    const btn = el('button');
    btn.type = 'button';
    btn.addEventListener('click', () => focusEntry(entry));
    li.append(btn);
    fillItem(li, entry.ev);
    return li;
  }

  // Liste en yeniden eskiye sıralı tutulur; yeni öğe doğru yere eklenir ki
  // ekran okuyucu yalnızca gerçekten yeni olanı duyursun.
  function placeItem(li) {
    const t = Number(li.dataset.t);
    let before = null;
    for (const other of listEl.children) {
      if (other !== li && Number(other.dataset.t) < t) { before = other; break; }
    }
    if (li.nextSibling !== before || li.parentNode !== listEl) listEl.insertBefore(li, before);
  }

  function focusEntry(entry) {
    map.flyTo([entry.ev.lat, entry.ev.lon], Math.max(map.getZoom(), 8), { duration: 0.6 });
    entry.marker.openPopup();
  }

  // ---- Durum ----
  function visible(ev) { return ev.magnitude >= minMag; }

  function applyVisibility(entry) {
    const show = visible(entry.ev);
    entry.li.hidden = !show;
    if (show && !layer.hasLayer(entry.marker)) layer.addLayer(entry.marker);
    if (!show && layer.hasLayer(entry.marker)) layer.removeLayer(entry.marker);
  }

  function updateCount() {
    let n = 0;
    for (const e of entries.values()) if (visible(e.ev)) n++;
    countEl.textContent = '(' + n + ')';
    emptyEl.hidden = n > 0;
  }

  function highlight(entry, isNew) {
    entry.li.classList.add('fresh');
    setTimeout(() => entry.li.classList.remove('fresh'), 4000);
    if (!isNew || !visible(entry.ev)) return;
    // Yeni olay: tek seferlik, sönerek genişleyen halka (yanıp sönme değil).
    const ring = L.circleMarker([entry.ev.lat, entry.ev.lon], {
      ...markerStyle(entry.ev), fillOpacity: 0, weight: 2, className: 'ring', interactive: false,
    }).addTo(map);
    setTimeout(() => ring.remove(), 2600);
  }

  function removeEntry(id) {
    const e = entries.get(id);
    if (!e) return;
    layer.removeLayer(e.marker);
    e.li.remove();
    entries.delete(id);
  }

  // upsert, olayı ekler ya da yerinde günceller. Sürüm karşılaştırması şart:
  // REST yanıtı ile WebSocket mesajları farklı sırada gelebilir.
  function upsert(ev, live) {
    if (Date.now() - Date.parse(ev.origin_time) > WINDOW_MS) {
      removeEntry(ev.id);
      return;
    }
    const entry = entries.get(ev.id);
    if (entry) {
      if (ev.version < entry.ev.version) return;
      const changed = ev.version > entry.ev.version;
      // WS mesajında özet yok; eldekini koru.
      entry.ev = { ...ev, summary: ev.summary ?? entry.ev.summary };
      if (changed) entry.detail = null;
      entry.marker.setLatLng([ev.lat, ev.lon]);
      entry.marker.setStyle(markerStyle(ev));
      entry.marker.setRadius(radius(ev.magnitude));
      fillItem(entry.li, entry.ev);
      placeItem(entry.li);
      applyVisibility(entry);
      if (entry.marker.isPopupOpen()) {
        entry.marker.setPopupContent(buildPopup(entry));
        if (changed) loadDetail(entry);
      }
      if (live && changed) highlight(entry, false);
      return;
    }
    const e = { ev: { ...ev }, detail: null };
    e.marker = L.circleMarker([ev.lat, ev.lon], markerStyle(ev));
    e.marker.bindPopup(() => buildPopup(e), { maxWidth: 300 });
    e.marker.on('popupopen', () => loadDetail(e));
    e.li = createItem(e);
    entries.set(ev.id, e);
    applyVisibility(e);
    placeItem(e.li);
    if (live) highlight(e, true);
  }

  // Toplu değişikliklerde (ilk yükleme, filtre) canlı bölge susturulur;
  // aksi halde ekran okuyucu tüm listeyi okur.
  function quietly(fn) {
    listEl.setAttribute('aria-live', 'off');
    try { fn(); } finally {
      updateCount();
      setTimeout(() => listEl.setAttribute('aria-live', 'polite'), 0);
    }
  }

  function prune() {
    const cutoff = Date.now() - WINDOW_MS;
    quietly(() => {
      for (const [id, e] of entries) if (Date.parse(e.ev.origin_time) < cutoff) removeEntry(id);
    });
  }
  setInterval(prune, 60 * 1000);

  filterEl.addEventListener('change', () => {
    minMag = Number(filterEl.value) || 0;
    quietly(() => { for (const e of entries.values()) applyVisibility(e); });
  });

  window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
    for (const e of entries.values()) e.marker.setStyle(markerStyle(e.ev));
  });

  async function loadRecent() {
    const res = await fetch('/api/events?hours=24');
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const body = await res.json();
    quietly(() => { for (const ev of body.events) upsert(ev, false); });
  }

  // ---- WebSocket ----
  function setStatus(state, text) {
    statusEl.dataset.state = state;
    statusText.textContent = text;
  }

  let attempt = 0;
  let retryTimer = null;
  let ws = null;

  function connect() {
    clearTimeout(retryTimer);
    setStatus('connecting', 'Bağlanıyor…');
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    ws = new WebSocket(proto + '//' + location.host + '/ws');

    ws.addEventListener('open', () => {
      attempt = 0;
      setStatus('live', 'Canlı');
      // Liste bağlantı açıldıktan sonra çekilir: kopukluk sırasında kaçan
      // olaylar da gelir, ikisi arasındaki yarış sürüm karşılaştırmasıyla çözülür.
      loadRecent().catch(() => setStatus('degraded', 'Canlı · liste yüklenemedi'));
    });

    ws.addEventListener('message', (msg) => {
      let data;
      try { data = JSON.parse(msg.data); } catch (_) { return; }
      if (data.type !== 'batch' || !Array.isArray(data.events)) return;
      for (const ev of data.events) upsert(ev, true);
      updateCount();
    });

    ws.addEventListener('close', () => {
      // Üstel geri çekilme + rastgelelik: sunucu yeniden açıldığında tüm
      // ziyaretçiler aynı anda bağlanmaya çalışmasın.
      const base = Math.min(30000, 1000 * 2 ** attempt);
      const delay = Math.round(base / 2 + Math.random() * base / 2);
      attempt++;
      setStatus('retry', 'Bağlantı yok · ' + Math.ceil(delay / 1000) + ' sn içinde yeniden denenecek');
      retryTimer = setTimeout(connect, delay);
    });
  }

  // Sekme uykudan dönünce beklemeden dene.
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible' && ws && ws.readyState === WebSocket.CLOSED) {
      connect();
    }
  });

  updateCount();
  connect();
})();
