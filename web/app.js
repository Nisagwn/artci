// Artçı canlı harita. Framework yok; Leaflet + WebSocket.
// Kaynaktan gelen tüm metinler (yer adı, özet) DOM'a yalnızca textContent ile
// yazılır: veri bize ait değil, HTML olarak yorumlanmamalı (XSS).
'use strict';

(() => {
  const WINDOW_LABELS = { 1: 'son 1 saatte', 24: 'son 24 saatte', 168: 'son 7 günde' };
  const AFAD_URL = 'https://deprem.afad.gov.tr/';
  const SOURCE_NAMES = { afad: 'AFAD', kandilli: 'Kandilli', emsc: 'EMSC' };
  const TZ = 'Europe/Istanbul';
  // summarizer bu büyüklüğün altına özet yazmaz (ARTCI_SUMMARY_MIN_MAG varsayılanı).
  const SUMMARY_MIN_MAG = 3.5;

  const fmtFull = new Intl.DateTimeFormat('tr-TR', {
    timeZone: TZ, day: 'numeric', month: 'long', hour: '2-digit', minute: '2-digit', second: '2-digit',
  });
  const fmtShort = new Intl.DateTimeFormat('tr-TR', {
    timeZone: TZ, day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit',
  });
  const num = (n, d = 1) => Number(n).toLocaleString('tr-TR', { minimumFractionDigits: d, maximumFractionDigits: d });
  const sourceName = (s) => SOURCE_NAMES[s] || s;

  // "3 dk önce" gibi göreli zaman: listeyi taramayı kolaylaştırır.
  function ago(iso) {
    const min = Math.floor((Date.now() - Date.parse(iso)) / 60000);
    if (min < 1) return 'az önce';
    if (min < 60) return min + ' dk önce';
    const h = Math.floor(min / 60);
    if (h < 24) return h + ' sa ' + (min % 60 ? (min % 60) + ' dk ' : '') + 'önce';
    return Math.floor(h / 24) + ' gün önce';
  }

  // Arama Türkçe karakterden bağımsızdır: "sindirgi" Sındırgı'yı, "sarigol"
  // Sarıgöl'ü bulur. Karakter karakter katlanır ki eşleşmenin orijinal metindeki
  // yeri (vurgulamak için) bilinsin.
  function foldChar(c) {
    return c.toLocaleLowerCase('tr-TR').normalize('NFD').replace(/\p{M}/gu, '').replace(/ı/g, 'i');
  }
  function fold(str) {
    let out = '';
    for (const c of str) out += foldChar(c);
    return out;
  }
  // matchRange, sorgunun metinde geçtiği [başlangıç, bitiş) aralığını döner.
  function matchRange(text, q) {
    let folded = '';
    const idx = [];
    let i = 0;
    for (const c of text) {
      const f = foldChar(c);
      for (let k = 0; k < f.length; k++) idx.push(i);
      folded += f;
      i += c.length;
    }
    idx.push(i);
    const at = folded.indexOf(q);
    if (at < 0) return null;
    return [idx[at], idx[at + q.length]];
  }

  // Yer adlarını okunur yapar: Kandilli'nin TAMAMI BÜYÜK harfli adları
  // "Kirkagac (Manisa)" biçimine çevrilir; AFAD'ın "[35.43 km]" uzaklık eki atılır.
  function place(name) {
    if (!name) return 'Yer bilgisi yok';
    let s = name.replace(/\s*\[[^\]]*\]\s*/g, ' ').replace(/\s+/g, ' ').trim();
    if (s === s.toLocaleUpperCase('tr-TR')) {
      // Kandilli adları ASCII'dir ("MANISA"): orada I, İ'nin de yerine geçer ve
      // Türkçe küçültme "Manısa" yazardı. Türkçe büyük harf yoksa I -> i.
      const ascii = !/[İŞĞÜÖÇ]/.test(s);
      s = (ascii ? s.toLowerCase() : s.toLocaleLowerCase('tr-TR'))
        .replace(/(^|[\s(\-\/])(\p{L})/gu, (_, p, c) => p + c.toLocaleUpperCase('tr-TR'));
    }
    return s;
  }

  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  const $ = (id) => document.getElementById(id);
  const listEl = $('list');
  const emptyEl = $('empty');
  const countEl = $('count');
  const biggestEl = $('biggest');
  const windowLabel = $('window-label');
  const searchEl = $('search');
  const statusEl = $('status');
  const statusText = $('status-text');

  // id -> { ev, marker, li, detail, key }
  const entries = new Map();
  let minMag = 0;
  let windowHours = 24;
  let sortBy = 'time';
  let query = '';
  const windowMs = () => windowHours * 3600 * 1000;

  // ---- Harita ----
  const map = L.map('map', { center: [39.0, 35.2], zoom: 6, minZoom: 4, worldCopyJump: true });
  // Altlık OSM; CSS ile soluk ve gri tonlu gösterilir ki depremler öne çıksın.
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
  const radius = (m) => Math.max(4, 2 + m * 3);
  // Küçük depremler soluk kalır, büyükler belirginleşir: göz önce önemliyi görür.
  function markerStyle(ev) {
    const c = color(ev.magnitude);
    const big = ev.magnitude >= 3;
    return { radius: radius(ev.magnitude), color: c, weight: big ? 2 : 1, fillColor: c, fillOpacity: big ? 0.6 : 0.3 };
  }

  // Lejant haritanın köşesinde: daire rengi ve boyutu ne demek.
  const legend = L.control({ position: 'bottomleft' });
  legend.onAdd = () => {
    const root = el('div', 'legend');
    root.append(el('strong', null, 'Büyüklük'));
    const ul = el('ul');
    for (const [m, label] of [[2, '3’ten küçük'], [3.5, '3–4'], [4.5, '4–5'], [5.5, '5+']]) {
      const dot = el('i');
      const d = Math.round(radius(m) * 1.4);
      dot.style.width = dot.style.height = d + 'px';
      dot.style.setProperty('--c', 'var(--' + magClass(m) + ')');
      const li = el('li');
      li.append(dot, label);
      ul.append(li);
    }
    root.append(ul);
    return root;
  };
  legend.addTo(map);

  function badge(m, extra) {
    const b = el('span', 'badge ' + magClass(m) + (extra ? ' ' + extra : ''));
    b.append(el('span', 'sr-only', 'Büyüklük '), num(m));
    return b;
  }

  // ---- Popup ----

  function buildPopup(entry) {
    const ev = entry.ev;
    const root = el('div', 'pop');
    const head = el('div', 'head');
    head.append(badge(ev.magnitude, 'small'), el('h3', null, place(ev.place_name)));
    root.append(head);
    root.append(el('p', 'when', ago(ev.origin_time) + ' · ' + fmtFull.format(new Date(ev.origin_time))));

    const dl = el('dl');
    const row = (k, v) => dl.append(el('dt', null, k), el('dd', null, v));
    row('Büyüklük', num(ev.magnitude) + (ev.mag_type ? ' (' + ev.mag_type + ')' : ''));
    row('Derinlik', ev.depth_km != null ? num(ev.depth_km) + ' km' : 'bilinmiyor');
    root.append(dl);

    if (ev.summary) {
      root.append(el('p', 'summary', ev.summary));
    } else if (ev.magnitude >= SUMMARY_MIN_MAG) {
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
    const depth = ev.depth_km != null ? ' · ' + num(ev.depth_km, 0) + ' km derinlik' : '';
    const meta = el('span', 'meta', ago(ev.origin_time) + depth);
    meta.title = fmtShort.format(new Date(ev.origin_time));
    btn.append(badge(ev.magnitude), placeEl(ev), meta);
    li.dataset.t = String(Date.parse(ev.origin_time));
    li.dataset.m = String(ev.magnitude);
  }

  // Yer adı; arama varsa eşleşen kısım <mark> ile vurgulanır (textContent ile).
  function placeEl(ev) {
    const name = place(ev.place_name);
    const span = el('span', 'place');
    const r = query ? matchRange(name, query) : null;
    if (!r) {
      span.textContent = name;
      return span;
    }
    span.append(name.slice(0, r[0]), el('mark', null, name.slice(r[0], r[1])), name.slice(r[1]));
    return span;
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

  // Liste seçilen sıraya göre (en yeni ya da en büyük) tutulur; yeni öğe doğru
  // yere eklenir ki ekran okuyucu yalnızca gerçekten yeni olanı duyursun.
  function comesBefore(a, b) {
    if (sortBy === 'mag') {
      const d = Number(a.dataset.m) - Number(b.dataset.m);
      if (d !== 0) return d > 0;
    }
    return Number(a.dataset.t) > Number(b.dataset.t);
  }
  function placeItem(li) {
    let before = null;
    for (const other of listEl.children) {
      if (other !== li && comesBefore(li, other)) { before = other; break; }
    }
    if (li.nextSibling !== before || li.parentNode !== listEl) listEl.insertBefore(li, before);
  }
  function sortAll() {
    const items = [...listEl.children];
    items.sort((a, b) => (comesBefore(a, b) ? -1 : comesBefore(b, a) ? 1 : 0));
    listEl.append(...items);
  }

  function focusEntry(entry) {
    map.flyTo([entry.ev.lat, entry.ev.lon], Math.max(map.getZoom(), 8), { duration: 0.6 });
    entry.marker.openPopup();
  }

  // ---- Durum ----
  function matches(entry) {
    if (!query) return true;
    if (entry.key == null) entry.key = fold(place(entry.ev.place_name) + ' ' + (entry.ev.place_name || ''));
    return entry.key.includes(query);
  }
  function visibleEntry(entry) { return entry.ev.magnitude >= minMag && matches(entry); }

  function applyVisibility(entry) {
    const show = visibleEntry(entry);
    entry.li.hidden = !show;
    if (show && !layer.hasLayer(entry.marker)) layer.addLayer(entry.marker);
    if (!show && layer.hasLayer(entry.marker)) layer.removeLayer(entry.marker);
  }

  let biggest = null;
  function updateCount() {
    let n = 0;
    biggest = null;
    for (const e of entries.values()) {
      if (!visibleEntry(e)) continue;
      n++;
      if (!biggest || e.ev.magnitude > biggest.ev.magnitude) biggest = e;
    }
    countEl.textContent = String(n);
    windowLabel.textContent = WINDOW_LABELS[windowHours] + (query ? ' · “' + searchEl.value.trim() + '”' : '');
    emptyEl.hidden = n > 0;
    emptyEl.textContent = query
      ? 'Aramanızla eşleşen deprem yok. Zaman aralığını genişletmeyi deneyin.'
      : 'Bu filtrelerle ' + WINDOW_LABELS[windowHours] + ' deprem yok.';
    biggestEl.hidden = !biggest;
    if (biggest) {
      const text = el('span');
      text.append(el('span', 'label', 'En büyüğü · ' + ago(biggest.ev.origin_time)),
        el('span', 'what', place(biggest.ev.place_name)));
      biggestEl.replaceChildren(badge(biggest.ev.magnitude, 'small'), text);
    }
  }
  biggestEl.addEventListener('click', () => { if (biggest) focusEntry(biggest); });

  // Göreli zamanlar ("5 dk önce") dakikada bir tazelenir.
  function refreshTimes() {
    for (const e of entries.values()) fillItem(e.li, e.ev);
    updateCount();
  }

  function highlight(entry, isNew) {
    entry.li.classList.add('fresh');
    setTimeout(() => entry.li.classList.remove('fresh'), 4000);
    if (!isNew || !visibleEntry(entry)) return;
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
    if (Date.now() - Date.parse(ev.origin_time) > windowMs()) {
      removeEntry(ev.id);
      return;
    }
    const entry = entries.get(ev.id);
    if (entry) {
      if (ev.version < entry.ev.version) return;
      const changed = ev.version > entry.ev.version;
      // WS mesajında özet yok; eldekini koru.
      entry.ev = { ...ev, summary: ev.summary ?? entry.ev.summary };
      entry.key = null;
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
    // Küçük depremlerin altında büyükler kaybolmasın.
    e.marker.on('add', () => { if (e.ev.magnitude >= 3) e.marker.bringToFront(); });
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
    const cutoff = Date.now() - windowMs();
    quietly(() => {
      for (const [id, e] of entries) if (Date.parse(e.ev.origin_time) < cutoff) removeEntry(id);
    });
  }
  setInterval(() => { prune(); refreshTimes(); }, 60 * 1000);

  // Düğme grubu: tıklanan düğme seçili olur, değeri onChange'e verilir.
  function chips(id, onChange) {
    const buttons = document.querySelectorAll('#' + id + ' button');
    for (const b of buttons) {
      b.addEventListener('click', () => {
        for (const o of buttons) o.setAttribute('aria-pressed', String(o === b));
        onChange(b.dataset.v);
      });
    }
  }
  const refilter = () => quietly(() => { for (const e of entries.values()) applyVisibility(e); });

  chips('f-mag', (v) => { minMag = Number(v) || 0; refilter(); });
  chips('f-sort', (v) => { sortBy = v; sortAll(); listEl.scrollTop = 0; });
  chips('f-hours', (v) => {
    windowHours = Number(v) || 24;
    prune();
    loadRecent().catch(() => setStatus('degraded', 'Liste yüklenemedi'));
  });

  // Arama: yazarken süzülür; eşleşme varsa harita sonuçlara yaklaşır.
  let searchTimer = null;
  function runSearch() {
    query = fold(searchEl.value.trim());
    quietly(() => {
      for (const e of entries.values()) {
        applyVisibility(e);
        fillItem(e.li, e.ev);
      }
    });
    if (!query) return;
    const pts = [];
    for (const e of entries.values()) if (visibleEntry(e)) pts.push([e.ev.lat, e.ev.lon]);
    if (pts.length) map.flyToBounds(pts, { maxZoom: 9, padding: [40, 40], duration: 0.6 });
  }
  searchEl.addEventListener('input', () => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(runSearch, 200);
  });
  searchEl.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') { searchEl.value = ''; runSearch(); }
    if (e.key === 'Enter') {
      clearTimeout(searchTimer);
      runSearch();
      const first = [...listEl.children].find((li) => !li.hidden);
      if (first) first.firstChild.focus();
    }
  });
  // "/" ile aramaya geç (başka bir alana yazılmıyorsa).
  document.addEventListener('keydown', (e) => {
    if (e.key === '/' && document.activeElement !== searchEl &&
        !/^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName)) {
      e.preventDefault();
      searchEl.focus();
    }
  });

  window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
    for (const e of entries.values()) e.marker.setStyle(markerStyle(e.ev));
  });

  async function loadRecent() {
    const hours = windowHours;
    const res = await fetch('/api/events?limit=2000&hours=' + hours);
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const body = await res.json();
    // Bu arada başka bir zaman aralığı seçildiyse eski yanıtı atla.
    if (hours !== windowHours) return;
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
