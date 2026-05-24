package handler

import (
	"context"
	"errors" //nolint:depguard // stdlib errors.Is matches the domain sentinels at the handler edge

	"github.com/palantir/pkg/datetime"
	palantiruuid "github.com/palantir/pkg/uuid"
	werror "github.com/palantir/witchcraft-go-error"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
	"go.uber.org/fx"

	"github.com/olehmushka/go-signalium/internal/config"
	"github.com/olehmushka/go-signalium/internal/domain"
	signalapi "github.com/olehmushka/go-signalium/internal/generated/signalium/api"
	"github.com/olehmushka/go-signalium/internal/repo"
	"github.com/olehmushka/go-signalium/internal/signal"
)

// Module wires the handler layer: a concrete SignalMessagesService that
// witchcraft can register routes against, plus the MultipartHandler that
// covers POST /api/v1/signal-messages (registered outside Conjure — see
// docs/decisions/0008). M5 fills in the operational endpoints.
var Module = fx.Module(
	"handler",
	fx.Provide(
		fx.Annotate(NewSignalMessagesService, fx.As(new(signalapi.SignalMessagesService))),
		NewMultipartHandler,
	),
)

// listDefaultLimit caps a single page when the caller does not provide one.
const listDefaultLimit = 50

// SignalMessagesHandler is the M5 implementation of the conjure-generated
// SignalMessagesService interface. It wraps the repo for CRUD, the signal-cli
// HTTP client for the groups proxy, and the install config for InstanceInfo.
type SignalMessagesHandler struct {
	repo       *repo.Messages
	signalHTTP *signal.HTTPClient
	install    config.Install
	logger     svc1log.Logger
}

// NewSignalMessagesService is the fx provider.
func NewSignalMessagesService(
	r *repo.Messages,
	httpClient *signal.HTTPClient,
	install config.Install,
	logger svc1log.Logger,
) *SignalMessagesHandler {
	return &SignalMessagesHandler{
		repo:       r,
		signalHTTP: httpClient,
		install:    install,
		logger:     logger,
	}
}

// GetInstanceInfo handles GET /api/v1/info. Returns product name + configured
// sender phone number — both static for the lifetime of the process so no
// repo or signal-cli round trip is needed.
func (h *SignalMessagesHandler) GetInstanceInfo(_ context.Context) (signalapi.InstanceInfo, error) {
	return signalapi.InstanceInfo{
		Name:              h.install.ProductName,
		SenderPhoneNumber: h.install.SignalCli.SenderPhoneNumber,
	}, nil
}

// ListGroups handles GET /api/v1/groups. Proxies to signal-cli's HTTP
// daemon via JSON-RPC `listGroups`; daemon-unreachable maps to 503
// SignalCliUnavailable in the HTTP client.
func (h *SignalMessagesHandler) ListGroups(ctx context.Context) ([]signalapi.SignalGroupInfo, error) {
	groups, err := h.signalHTTP.ListGroups(ctx)
	if err != nil {
		return nil, mapToConjureError(err)
	}
	return groups, nil
}

// ListSignalMessages handles GET /api/v1/signal-messages.
func (h *SignalMessagesHandler) ListSignalMessages(
	ctx context.Context,
	q *string, limit *int, offset *int,
	createdAtFrom *datetime.DateTime, createdAtTo *datetime.DateTime,
	status *signalapi.SignalMessageStatus,
	attempts *int,
) (signalapi.SignalMessageList, error) {
	filters := repo.ListFilters{
		Q:             q,
		Status:        conjureToDomainStatus(status),
		Attempts:      intPtrToInt32(attempts),
		CreatedAtFrom: dateTimeToTime(createdAtFrom),
		CreatedAtTo:   dateTimeToTime(createdAtTo),
		Limit:         coalescePositive(limit, listDefaultLimit),
		Offset:        coalesceNonNegative(offset),
	}

	rows, err := h.repo.List(ctx, filters)
	if err != nil {
		return signalapi.SignalMessageList{}, mapToConjureError(err)
	}
	total, err := h.repo.Count(ctx, filters)
	if err != nil {
		return signalapi.SignalMessageList{}, mapToConjureError(err)
	}

	items := make([]signalapi.SignalMessageInfo, 0, len(rows))
	for _, row := range rows {
		items = append(items, domainToConjure(row))
	}
	return signalapi.SignalMessageList{Items: items, Total: total}, nil
}

// GetSignalMessage handles GET /api/v1/signal-messages/{id}.
func (h *SignalMessagesHandler) GetSignalMessage(ctx context.Context, id palantiruuid.UUID) (signalapi.SignalMessageInfo, error) {
	row, err := h.repo.GetByID(ctx, googleUUID(id))
	if err != nil {
		return signalapi.SignalMessageInfo{}, notFoundOrInternal(ctx, id, err)
	}
	return domainToConjure(row), nil
}

// UpdateSignalMessage handles PUT /api/v1/signal-messages/{id}. Currently
// the only mutable field is the status — operators use this to force a
// terminal state (e.g. PERMANENT_FAILED override). Validation rejects
// transitions to SENDING (worker-owned) so callers cannot fake an in-flight
// row.
func (h *SignalMessagesHandler) UpdateSignalMessage(
	ctx context.Context,
	id palantiruuid.UUID,
	body signalapi.UpdateSignalMessageRequest,
) (signalapi.SignalMessageInfo, error) {
	target := domain.SignalMessageStatus(body.Status.Value())
	if !isOperatorAssignableStatus(target) {
		return signalapi.SignalMessageInfo{}, signalapi.WrapWithInvalidSignalMessage(
			werror.ErrorWithContextParams(ctx, "status not operator-assignable",
				werror.SafeParam("status", string(target))),
			"status", "must be one of PENDING, SENT, FAILED, PERMANENT_FAILED, TIMED_OUT",
		)
	}
	row, err := h.repo.UpdateStatus(ctx, googleUUID(id), target)
	if err != nil {
		return signalapi.SignalMessageInfo{}, notFoundOrInternal(ctx, id, err)
	}
	return domainToConjure(row), nil
}

// ResendSignalMessage handles POST /api/v1/signal-messages/{id}/resend.
// No-op for non-terminal rows; the response always echoes the current row
// state so the caller sees the post-call status.
func (h *SignalMessagesHandler) ResendSignalMessage(ctx context.Context, id palantiruuid.UUID) (signalapi.SignalMessageInfo, error) {
	if _, err := h.repo.GetByID(ctx, googleUUID(id)); err != nil {
		return signalapi.SignalMessageInfo{}, notFoundOrInternal(ctx, id, err)
	}
	if err := h.repo.Resend(ctx, googleUUID(id)); err != nil {
		return signalapi.SignalMessageInfo{}, mapToConjureError(err)
	}
	row, err := h.repo.GetByID(ctx, googleUUID(id))
	if err != nil {
		return signalapi.SignalMessageInfo{}, notFoundOrInternal(ctx, id, err)
	}
	return domainToConjure(row), nil
}

// GetSignalMessagesStats handles GET /api/v1/signal-messages-stats.
func (h *SignalMessagesHandler) GetSignalMessagesStats(ctx context.Context) (signalapi.SignalMessageStats, error) {
	counts, err := h.repo.StatsCounts(ctx)
	if err != nil {
		return signalapi.SignalMessageStats{}, mapToConjureError(err)
	}
	perDay, err := h.repo.StatsPerDay(ctx)
	if err != nil {
		return signalapi.SignalMessageStats{}, mapToConjureError(err)
	}

	stats := signalapi.SignalMessageStats{
		Counts: make(map[signalapi.SignalMessageStatus]int, len(counts)),
		PerDay: make([]signalapi.DayStat, 0, len(perDay)),
	}
	for _, c := range counts {
		key := signalapi.New_SignalMessageStatus(signalapi.SignalMessageStatus_Value(c.Status))
		stats.Counts[key] = c.Count
	}
	for _, d := range perDay {
		stats.PerDay = append(stats.PerDay, signalapi.DayStat{
			Date:   d.Date,
			Sent:   d.Sent,
			Failed: d.Failed,
		})
	}
	return stats, nil
}

// notFoundOrInternal wraps domain.ErrNotFound as SignalMessageNotFound with
// the offending id; any other error funnels through the generic mapper.
func notFoundOrInternal(ctx context.Context, id palantiruuid.UUID, err error) error {
	if errors.Is(err, domain.ErrNotFound) {
		return signalapi.WrapWithSignalMessageNotFound(
			werror.WrapWithContextParams(ctx, err, "signal message not found",
				werror.SafeParam("id", id.String())),
			id.String(),
		)
	}
	return mapToConjureError(err)
}

// isOperatorAssignableStatus rejects transitions to SENDING — only the
// outbox worker may claim a row into SENDING; an operator forcing it would
// race the lease and risk double-sends.
func isOperatorAssignableStatus(s domain.SignalMessageStatus) bool {
	switch s {
	case domain.StatusPending, domain.StatusSent, domain.StatusFailed,
		domain.StatusPermanentFailed, domain.StatusTimedOut:
		return true
	case domain.StatusSending:
		return false
	default:
		return false
	}
}
