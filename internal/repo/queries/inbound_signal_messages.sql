-- name: InsertInbound :execrows
INSERT INTO signalium.inbound_signal_messages (
    source,
    source_uuid,
    source_timestamp,
    group_id,
    content,
    attachments,
    raw
) VALUES (
    sqlc.arg(source),
    sqlc.narg(source_uuid),
    sqlc.arg(source_timestamp),
    sqlc.narg(group_id),
    sqlc.narg(content),
    sqlc.arg(attachments),
    sqlc.arg(raw)
)
ON CONFLICT (source, source_timestamp) DO NOTHING;
