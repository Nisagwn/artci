-- 001_init: Artçı temel şeması (Plan §7).
CREATE EXTENSION IF NOT EXISTS postgis;

-- Birleştirilmiş olay (kullanıcının gördüğü deprem)
CREATE TABLE IF NOT EXISTS events (
    id                UUID PRIMARY KEY,
    origin_time       TIMESTAMPTZ NOT NULL,
    location          GEOGRAPHY(POINT, 4326) NOT NULL,
    depth_km          NUMERIC(5,1),
    magnitude         NUMERIC(3,1) NOT NULL,
    mag_type          TEXT,
    place_name        TEXT,
    version           INT NOT NULL DEFAULT 1,           -- her değişiklikte artar
    first_reported_at TIMESTAMPTZ NOT NULL DEFAULT now(), -- uçtan uca gecikme ölçümü için
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS events_location_idx ON events USING GIST (location);
CREATE INDEX IF NOT EXISTS events_origin_time_idx ON events (origin_time DESC);

-- Her kaynaktan gelen her rapor (ham gerçek)
CREATE TABLE IF NOT EXISTS reports (
    id              BIGSERIAL PRIMARY KEY,
    source          TEXT        NOT NULL,           -- 'afad' | 'kandilli' | 'emsc'
    source_event_id TEXT        NOT NULL,           -- kaynağın kendi kimliği
    origin_time     TIMESTAMPTZ NOT NULL,
    location        GEOGRAPHY(POINT, 4326) NOT NULL,
    depth_km        NUMERIC(5,1),
    magnitude       NUMERIC(3,1) NOT NULL,
    mag_type        TEXT,                           -- Mw, ML, Md...
    place_name      TEXT,
    raw             JSONB       NOT NULL,           -- kaynağın orijinal kaydı
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    event_id        UUID REFERENCES events(id),
    UNIQUE (source, source_event_id)                -- idempotency'nin kalbi
);
CREATE INDEX IF NOT EXISTS reports_event_id_idx ON reports (event_id);

-- Abonelikler
CREATE TABLE IF NOT EXISTS subscriptions (
    id          BIGSERIAL PRIMARY KEY,
    chat_id     BIGINT NOT NULL,                    -- Telegram sohbet kimliği
    city        TEXT NOT NULL,
    center      GEOGRAPHY(POINT, 4326) NOT NULL,    -- şehir merkezi (kişisel konum değil)
    radius_km   INT NOT NULL CHECK (radius_km BETWEEN 10 AND 500),
    min_mag     NUMERIC(3,1) NOT NULL DEFAULT 3.5,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS subscriptions_center_idx ON subscriptions USING GIST (center);
CREATE INDEX IF NOT EXISTS subscriptions_chat_idx ON subscriptions (chat_id);

-- Gönderilen bildirimler: aynı bildirimin iki kez gitmesini engeller
CREATE TABLE IF NOT EXISTS notifications (
    event_id        UUID   NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    subscription_id BIGINT NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
    kind            TEXT   NOT NULL DEFAULT 'initial', -- 'initial' | 'update'
    magnitude       NUMERIC(3,1),                      -- bildirilen büyüklük
    sent_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, subscription_id, kind)
);

-- LLM özetleri
CREATE TABLE IF NOT EXISTS summaries (
    event_id      UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    event_version INT  NOT NULL,
    text          TEXT NOT NULL,
    generator     TEXT NOT NULL,                    -- 'llm:<model>' | 'template'
    validated     BOOLEAN NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, event_version)
);

-- Günlük LLM harcaması (maliyet tavanı için)
CREATE TABLE IF NOT EXISTS llm_spend (
    day       DATE PRIMARY KEY,
    usd       NUMERIC(10,6) NOT NULL DEFAULT 0,
    calls     INT NOT NULL DEFAULT 0
);
