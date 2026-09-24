-- Preserve the local accounting meaning of outputs across the signature
-- checkpoint. Legacy requests remain unknown: guessing an origin would turn
-- a refresh or change output into a receipt from another user.
ALTER TABLE round_vtxo_requests ADD COLUMN origin INTEGER NOT NULL DEFAULT 0
    CHECK (origin BETWEEN 0 AND 4);

ALTER TABLE round_vtxo_requests ADD COLUMN refresh_source_hash BLOB
    CHECK (refresh_source_hash IS NULL OR length(refresh_source_hash) = 32);
ALTER TABLE round_vtxo_requests ADD COLUMN refresh_source_index BIGINT
    CHECK (
        (refresh_source_hash IS NULL AND refresh_source_index IS NULL)
        OR
        (refresh_source_hash IS NOT NULL AND refresh_source_index IS NOT NULL
         AND refresh_source_index BETWEEN 0 AND 4294967295)
    );
