-- name: GetSetting :one
SELECT value FROM settings WHERE key = ?;

-- name: ListSettings :many
SELECT key, value FROM settings ORDER BY key;

-- name: SetSetting :exec
INSERT INTO settings (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value;

-- name: CreateMeasurement :exec
INSERT INTO measurements (
    created_at, kind, coma_x, coma_y, coma_err, axis_x_mm, axis_y_mm, decentre_mm,
    seeing_px, obstruction, sa_px, step_um, paraxial_focus, alt, az, foc_temp, amb_temp,
    stars, frames, files, ref_focpos, positions, after_slew, note,
    tilt_x, tilt_y, tilt_err, shadow_x_mm, shadow_y_mm, hub_x_mm, hub_y_mm, pupil_err_mm, intra_high,
    tilt_rough, pupil_rough
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetLastMeasurementID :one
SELECT CAST(COALESCE(MAX(id), 0) AS INTEGER) AS id FROM measurements;

-- name: GetMeasurement :one
SELECT * FROM measurements WHERE id = ?;

-- name: ListRecentMeasurements :many
SELECT * FROM measurements ORDER BY id DESC LIMIT ?;

-- name: ListMeasurementsSince :many
SELECT * FROM measurements
WHERE created_at >= ? AND kind IN ('measure', 'calibrate')
ORDER BY id;

-- name: CreateCalibration :exec
INSERT INTO calibrations (
    created_at, source, a_cx, a_cy, b_cx, b_cy, c_cx, c_cy,
    a_sx, a_sy, b_sx, b_sy, c_sx, c_sy, c_measured
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetCurrentCalibration :one
SELECT * FROM calibrations ORDER BY id DESC LIMIT 1;

-- name: CreateAdjustment :exec
INSERT INTO adjustments (created_at, before_id, turns_a, turns_b, turns_c, pred_dcx, pred_dcy)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetPendingAdjustment :one
SELECT * FROM adjustments WHERE status = 'pending' ORDER BY id DESC LIMIT 1;

-- name: CompleteAdjustment :exec
UPDATE adjustments
SET after_id = ?, obs_dcx = ?, obs_dcy = ?, applied = ?, status = 'done'
WHERE id = ?;

-- name: SkipPendingAdjustments :exec
UPDATE adjustments SET status = 'skipped' WHERE status = 'pending';

-- name: ListRecentAdjustments :many
SELECT * FROM adjustments ORDER BY id DESC LIMIT ?;
