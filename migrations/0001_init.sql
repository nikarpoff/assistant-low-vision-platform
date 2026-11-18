-- Journal of the gateway (PLT-11): devices, sessions, requests with their
-- stage timings, metrics of the application and configuration snapshots.
--
-- The journal is written asynchronously and may lose records (full buffer,
-- unavailable database), so foreign keys link only rows written by one
-- record: a session and its device, a request and its stage timings. A lost
-- session does not cost its requests, a lost request leaves its client
-- metrics unmatched. Images and audio are never stored (NFR-8): a request
-- keeps only the size of the received image.

CREATE TABLE device (
    device_id     TEXT PRIMARY KEY,           -- pseudonym: random UUID of the application
    first_seen_at TIMESTAMPTZ NOT NULL,
    last_seen_at  TIMESTAMPTZ NOT NULL
);

CREATE TABLE session (
    session_id  UUID PRIMARY KEY,
    device_id   TEXT NOT NULL REFERENCES device (device_id),
    app_version TEXT NOT NULL,
    network     TEXT NOT NULL,                -- wifi, cellular, other
    label       TEXT NOT NULL DEFAULT '',     -- participant code (EXP-6)
    started_at  TIMESTAMPTZ NOT NULL,
    ended_at    TIMESTAMPTZ
);

CREATE TABLE request (
    request_id             UUID PRIMARY KEY,
    session_id             UUID NOT NULL,
    kind                   TEXT NOT NULL CHECK (kind IN ('describe', 'ask', 'passive')),
    describer_mode         TEXT NOT NULL,     -- template, llm, vlm
    intent                 TEXT NOT NULL DEFAULT '',
    status                 TEXT NOT NULL CHECK (status IN ('ok', 'degraded', 'error', 'cancelled', 'skipped', 'stale', 'clear')),
    error_code             TEXT NOT NULL DEFAULT '',
    degraded_reason        TEXT NOT NULL DEFAULT '' CHECK (degraded_reason IN ('', 'llm_timeout', 'llm_unavailable', 'tts_unavailable')),
    image_bytes            INTEGER NOT NULL DEFAULT 0,  -- size of the received image, the image itself is not stored
    transcript             TEXT NOT NULL DEFAULT '',
    answer_text            TEXT NOT NULL DEFAULT '',
    hazard                 BOOLEAN NOT NULL DEFAULT FALSE,
    objects                JSONB NOT NULL DEFAULT '[]',
    config_id              TEXT NOT NULL,
    llm_model              TEXT NOT NULL DEFAULT '',     -- model from the provider response
    llm_system_fingerprint TEXT NOT NULL DEFAULT '',
    llm_error              TEXT NOT NULL DEFAULT '',     -- reason of PLT-15
    created_at             TIMESTAMPTZ NOT NULL
);

CREATE INDEX request_created_at_idx ON request (created_at);
CREATE INDEX request_config_id_created_at_idx ON request (config_id, created_at);
CREATE INDEX request_session_id_idx ON request (session_id);

CREATE TABLE stage_timing (
    request_id  UUID NOT NULL REFERENCES request (request_id) ON DELETE CASCADE,
    stage       TEXT NOT NULL,                -- name of PLT-14, e.g. detect_ms
    duration_ms DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (request_id, stage)
);

CREATE TABLE client_metric (
    request_id UUID NOT NULL,
    name       TEXT NOT NULL,                 -- metric of client.metrics, e.g. trigger_to_first_audio_ms
    value      DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (request_id, name)
);

CREATE TABLE config_snapshot (
    config_id    TEXT PRIMARY KEY,
    config       JSONB NOT NULL,              -- snapshot of PLT-12, without secrets
    collected_at TIMESTAMPTZ NOT NULL         -- first time the configuration was collected
);
