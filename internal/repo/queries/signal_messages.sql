-- name: Insert :one
INSERT INTO signalium.signal_messages (
    external_id,
    idempotency_key,
    recipient,
    group_id,
    sender_phone_number,
    content,
    attachments,
    max_attempts,
    timeout_at,
    correlation_id
) VALUES (
    sqlc.arg(external_id),
    sqlc.narg(idempotency_key),
    sqlc.narg(recipient),
    sqlc.narg(group_id),
    sqlc.arg(sender_phone_number),
    sqlc.arg(content),
    sqlc.arg(attachments),
    sqlc.arg(max_attempts),
    sqlc.narg(timeout_at),
    sqlc.arg(correlation_id)
)
RETURNING *;

-- name: GetByID :one
SELECT * FROM signalium.signal_messages
 WHERE signal_message_id = $1
   AND deleted_at IS NULL;

-- name: GetByExternalID :one
SELECT * FROM signalium.signal_messages
 WHERE external_id = $1
   AND deleted_at IS NULL;

-- name: GetByIdempotencyKey :one
SELECT * FROM signalium.signal_messages
 WHERE idempotency_key = $1;

-- name: ClaimPending :one
WITH claimed AS (
  SELECT signal_message_id
    FROM signalium.signal_messages
   WHERE deleted_at IS NULL
     AND (status = 'PENDING' OR (status = 'SENDING' AND next_attempt_at < now()))
     AND next_attempt_at <= now()
   ORDER BY next_attempt_at
   FOR UPDATE SKIP LOCKED
   LIMIT 1
)
UPDATE signalium.signal_messages m
   SET status         = 'SENDING',
       attempts       = attempts + 1,
       next_attempt_at = now() + sqlc.arg(lease_duration)::interval,
       modified_at    = now()
  FROM claimed
 WHERE m.signal_message_id = claimed.signal_message_id
RETURNING m.*;

-- name: MarkSent :exec
UPDATE signalium.signal_messages
   SET status      = 'SENT',
       result_id   = sqlc.arg(result_id),
       modified_at = now()
 WHERE signal_message_id = sqlc.arg(signal_message_id);

-- name: MarkFailed :exec
UPDATE signalium.signal_messages
   SET status = (CASE
                   WHEN attempts >= max_attempts THEN 'PERMANENT_FAILED'::signalium.signal_message_status
                   ELSE 'FAILED'::signalium.signal_message_status
                 END),
       last_error      = sqlc.arg(last_error),
       next_attempt_at = sqlc.arg(next_attempt_at),
       modified_at     = now()
 WHERE signal_message_id = sqlc.arg(signal_message_id);

-- name: Resend :exec
UPDATE signalium.signal_messages
   SET status          = 'PENDING',
       attempts        = 0,
       next_attempt_at = now(),
       last_error      = NULL,
       modified_at     = now()
 WHERE signal_message_id = $1
   AND status IN ('FAILED', 'TIMED_OUT', 'PERMANENT_FAILED');

-- name: UpdateStatus :one
UPDATE signalium.signal_messages
   SET status      = sqlc.arg(status),
       modified_at = now()
 WHERE signal_message_id = sqlc.arg(signal_message_id)
   AND deleted_at IS NULL
RETURNING *;

-- name: List :many
SELECT *
  FROM signalium.signal_messages
 WHERE deleted_at IS NULL
   AND (sqlc.narg(q)::text          IS NULL OR content    ILIKE '%' || sqlc.narg(q)::text || '%')
   AND (sqlc.narg(status_filter)::signalium.signal_message_status IS NULL OR status = sqlc.narg(status_filter)::signalium.signal_message_status)
   AND (sqlc.narg(attempts)::int    IS NULL OR attempts   = sqlc.narg(attempts)::int)
   AND (sqlc.narg(created_at_from)::timestamptz IS NULL OR created_at >= sqlc.narg(created_at_from)::timestamptz)
   AND (sqlc.narg(created_at_to)::timestamptz   IS NULL OR created_at <  sqlc.narg(created_at_to)::timestamptz)
 ORDER BY created_at DESC, signal_message_id ASC
 LIMIT  sqlc.arg(limit_count)::int
 OFFSET sqlc.arg(offset_count)::int;

-- name: Count :one
SELECT COUNT(*)::int AS counted
  FROM signalium.signal_messages
 WHERE deleted_at IS NULL
   AND (sqlc.narg(q)::text          IS NULL OR content    ILIKE '%' || sqlc.narg(q)::text || '%')
   AND (sqlc.narg(status_filter)::signalium.signal_message_status IS NULL OR status = sqlc.narg(status_filter)::signalium.signal_message_status)
   AND (sqlc.narg(attempts)::int    IS NULL OR attempts   = sqlc.narg(attempts)::int)
   AND (sqlc.narg(created_at_from)::timestamptz IS NULL OR created_at >= sqlc.narg(created_at_from)::timestamptz)
   AND (sqlc.narg(created_at_to)::timestamptz   IS NULL OR created_at <  sqlc.narg(created_at_to)::timestamptz);

-- name: StatsCounts :many
SELECT status,
       COUNT(*)::int AS counted
  FROM signalium.signal_messages
 WHERE deleted_at IS NULL
 GROUP BY status;

-- name: StatsPerDay :many
SELECT to_char(date_trunc('day', created_at), 'YYYY-MM-DD')                                     AS day,
       COUNT(*) FILTER (WHERE status = 'SENT')::int                                              AS sent,
       COUNT(*) FILTER (WHERE status IN ('FAILED', 'PERMANENT_FAILED', 'TIMED_OUT'))::int        AS failed
  FROM signalium.signal_messages
 WHERE deleted_at IS NULL
   AND created_at >= now() - interval '30 days'
 GROUP BY day
 ORDER BY day;
