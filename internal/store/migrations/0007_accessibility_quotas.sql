CREATE TABLE accessibility_quotas (
    site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    source_mac BLOB NOT NULL CHECK (length(source_mac) = 32),
    expires_at TEXT NOT NULL,
    PRIMARY KEY (site_id, source_mac)
) WITHOUT ROWID;
CREATE INDEX accessibility_quotas_expiry ON accessibility_quotas(expires_at);
