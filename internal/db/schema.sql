-- Collimation station schema. Applied with CREATE ... IF NOT EXISTS at start-up.

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- One row per measurement (a both-sides capture, a live reading, a
-- calibration step or an analysis of saved frames).
CREATE TABLE IF NOT EXISTS measurements (
    id             INTEGER PRIMARY KEY,
    created_at     TEXT    NOT NULL,
    kind           TEXT    NOT NULL,           -- measure | live | calibrate | analyse
    coma_x         REAL    NOT NULL,           -- px, + flares toward +x
    coma_y         REAL    NOT NULL,
    coma_err       REAL    NOT NULL,
    axis_x_mm      REAL    NOT NULL,           -- where the primary's axis lands
    axis_y_mm      REAL    NOT NULL,
    decentre_mm    REAL    NOT NULL,
    seeing_px      REAL    NOT NULL,
    obstruction    REAL    NOT NULL,
    sa_px          REAL    NOT NULL,
    step_um        REAL    NOT NULL,
    paraxial_focus REAL    NOT NULL,
    alt            REAL    NOT NULL,
    az             REAL    NOT NULL,
    foc_temp       REAL    NOT NULL,
    amb_temp       REAL    NOT NULL,
    stars          INTEGER NOT NULL,
    frames         INTEGER NOT NULL,
    files          TEXT    NOT NULL,           -- newline-separated paths
    ref_focpos     INTEGER NOT NULL DEFAULT 0, -- FOCPOS of the frame in positions
    positions      TEXT    NOT NULL DEFAULT '',-- JSON [[x,y],...] star centroids
    after_slew     INTEGER NOT NULL DEFAULT 0, -- 1 if the mount moved since the last one
    note           TEXT    NOT NULL DEFAULT ''
);

-- Screw sensitivities: coma change and star shift per +1/8 turn
-- (clockwise seen from behind the cell). The newest row is in use.
CREATE TABLE IF NOT EXISTS calibrations (
    id         INTEGER PRIMARY KEY,
    created_at TEXT    NOT NULL,
    source     TEXT    NOT NULL,               -- wizard | update
    a_cx REAL NOT NULL, a_cy REAL NOT NULL,
    b_cx REAL NOT NULL, b_cy REAL NOT NULL,
    c_cx REAL NOT NULL, c_cy REAL NOT NULL,
    a_sx REAL NOT NULL, a_sy REAL NOT NULL,
    b_sx REAL NOT NULL, b_sy REAL NOT NULL,
    c_sx REAL NOT NULL, c_sy REAL NOT NULL,
    c_measured INTEGER NOT NULL DEFAULT 0
);

-- Suggested adjustments and what happened when they were made.
CREATE TABLE IF NOT EXISTS adjustments (
    id         INTEGER PRIMARY KEY,
    created_at TEXT    NOT NULL,
    before_id  INTEGER NOT NULL REFERENCES measurements(id),
    after_id   INTEGER NOT NULL DEFAULT 0,
    turns_a    REAL    NOT NULL,               -- eighths, + = clockwise from behind
    turns_b    REAL    NOT NULL,
    turns_c    REAL    NOT NULL,
    pred_dcx   REAL    NOT NULL,
    pred_dcy   REAL    NOT NULL,
    obs_dcx    REAL    NOT NULL DEFAULT 0,
    obs_dcy    REAL    NOT NULL DEFAULT 0,
    applied    REAL    NOT NULL DEFAULT 0,     -- fraction of the asked move achieved
    status     TEXT    NOT NULL DEFAULT 'pending' -- pending | done | skipped
);

CREATE INDEX IF NOT EXISTS measurements_created ON measurements(created_at);
