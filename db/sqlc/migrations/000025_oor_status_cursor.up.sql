-- Match newest-first keyset pagination, including timestamp ties.
CREATE INDEX idx_oor_packages_status_cursor
    ON oor_packages (created_at DESC, session_id DESC);
CREATE INDEX idx_oor_session_registry_status_cursor
    ON oor_session_registry (created_at DESC, session_id DESC);

-- Registry creation time belongs to the session, not its later artifacts.
-- Only package-only history uses the package's creation time. Keeping these
-- sources disjoint avoids duplicate sessions with different timestamps.
CREATE VIEW oor_package_status AS
SELECT p.session_id,
    CASE p.direction WHEN 1 THEN 1 ELSE 2 END AS direction,
    1 AS status, 'completed' AS phase, '' AS last_error,
    p.created_at, p.updated_at, 1 AS has_package
FROM oor_packages p
WHERE NOT EXISTS (
    SELECT 1 FROM oor_session_registry r WHERE r.session_id = p.session_id
);

-- Package metadata still wins for completion and direction, including an
-- outgoing session whose change is later observed by an incoming actor.
CREATE VIEW oor_registry_status AS
SELECT r.session_id,
    CASE WHEN p.session_id IS NULL THEN r.direction
        WHEN p.direction = 1 THEN 1 ELSE 2 END AS direction,
    CASE WHEN p.session_id IS NULL THEN r.status ELSE 1 END AS status,
    CASE WHEN p.session_id IS NULL THEN r.phase ELSE 'completed' END AS phase,
    CASE WHEN p.session_id IS NULL THEN COALESCE(r.last_error, '')
        ELSE '' END AS last_error,
    r.created_at,
    COALESCE(p.updated_at, r.updated_at) AS updated_at,
    CASE WHEN p.session_id IS NULL THEN 0 ELSE 1 END AS has_package
FROM oor_session_registry r
LEFT JOIN oor_packages p ON p.session_id = r.session_id;

CREATE VIEW oor_status AS
SELECT * FROM oor_package_status
UNION ALL
SELECT * FROM oor_registry_status;
