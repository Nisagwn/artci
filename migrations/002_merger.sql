-- 002_merger: events.updated yayınının "en az bir kez" garantisi için küçük bir outbox.
--
-- Neden? merger önce DB'ye commit eder, sonra Redis'e yayınlar. Arada çökerse
-- mesaj yeniden işlenir ama rapor artık "değişmemiş" (noop) görünür ve yayın
-- sonsuza dek kaybolurdu. Hangi sürümün yayınlandığını olay satırında tutarak
-- eksik yayınları her mesajdan sonra (ve açılışta) tamamlarız.
ALTER TABLE events
    ADD COLUMN IF NOT EXISTS published_version   INT NOT NULL DEFAULT 0, -- 0 = hiç yayınlanmadı
    ADD COLUMN IF NOT EXISTS published_magnitude NUMERIC(3,1);           -- tüketicilerin son gördüğü büyüklük

-- Yayın bekleyenler küçük bir küme; kısmi indeks taramayı ucuz tutar.
CREATE INDEX IF NOT EXISTS events_unpublished_idx
    ON events (updated_at) WHERE published_version < version;
